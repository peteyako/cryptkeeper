package bitcoin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// ScanStore keeps a wallet's last address scan across restarts, so a restart carries on
// with light refreshes instead of reading every address again.
type ScanStore interface {
	LoadScan(ctx context.Context, walletID string) ([]byte, error) // nil when none is saved
	SaveScan(ctx context.Context, walletID string, data []byte) error
}

// savedScan is a scan as stored. Addresses aren't stored: they are derived again from the
// wallet's key, and Wallet tells whether the scan belongs to that key.
type savedScan struct {
	Wallet    string        `json:"wallet"`
	At        time.Time     `json:"at"`
	FullAt    time.Time     `json:"fullAt"`
	Tip       int64         `json:"tip,omitempty"`
	Chains    [][]savedAddr `json:"chains"`
	Truncated bool          `json:"truncated,omitempty"`
}

type savedAddr struct {
	Sats int64 `json:"s,omitempty"`
	Txs  int   `json:"t,omitempty"`
}

// walletFingerprint identifies the wallet's key and gap limit without revealing them.
func (s *Source) walletFingerprint() (string, error) {
	h := sha256.New()
	for chain := range s.chains {
		a, err := s.addressAt(chain, 0)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s|", a)
	}
	fmt.Fprintf(h, "gap=%d", s.gap)
	return hex.EncodeToString(h.Sum(nil)[:12]), nil
}

// lastScan is the previous scan: the one in memory, else, once, the one saved before a
// restart.
func (s *Source) lastScan(ctx context.Context) *scanResult {
	s.mu.Lock()
	last, tried := s.last, s.restored
	s.restored = true
	s.mu.Unlock()
	if last != nil || tried || s.store == nil || s.id == "" {
		return last
	}
	saved := s.loadScan(ctx)
	if saved == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = saved
	}
	return s.last
}

func (s *Source) loadScan(ctx context.Context) *scanResult {
	raw, err := s.store.LoadScan(ctx, s.id)
	if err != nil {
		slog.Warn("bitcoin: read saved scan", "wallet", s.id, "err", err)
		return nil
	}
	var saved savedScan
	if len(raw) == 0 || json.Unmarshal(raw, &saved) != nil || len(saved.Chains) != len(s.chains) {
		return nil
	}
	if fp, err := s.walletFingerprint(); err != nil || fp != saved.Wallet {
		return nil // the wallet's key changed since
	}
	res := &scanResult{at: saved.At, fullAt: saved.FullAt, tip: saved.Tip, truncated: saved.Truncated}
	for chain, addrs := range saved.Chains {
		states := make([]addrState, len(addrs))
		for i, a := range addrs {
			addr, err := s.addressAt(chain, i)
			if err != nil {
				return nil
			}
			states[i] = addrState{chain: chain, index: i, address: addr, sats: a.Sats, txs: a.Txs}
		}
		res.chains = append(res.chains, states)
		res.lastUsed = append(res.lastUsed, lastUsedIndex(states))
	}
	return res
}

func (s *Source) saveScan(ctx context.Context, res *scanResult) {
	if s.store == nil || s.id == "" {
		return
	}
	fp, err := s.walletFingerprint()
	if err != nil {
		return
	}
	saved := savedScan{Wallet: fp, At: res.at, FullAt: res.fullAt, Tip: res.tip, Truncated: res.truncated}
	for _, states := range res.chains {
		addrs := make([]savedAddr, len(states))
		for i, a := range states {
			addrs[i] = savedAddr{Sats: a.sats, Txs: a.txs}
		}
		saved.Chains = append(saved.Chains, addrs)
	}
	raw, err := json.Marshal(saved)
	if err == nil {
		err = s.store.SaveScan(ctx, s.id, raw)
	}
	if err != nil {
		slog.Warn("bitcoin: save scan", "wallet", s.id, "err", err)
	}
}
