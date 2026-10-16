package bitcoin

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"crypto-portfolio/internal/core"

	"golang.org/x/sync/errgroup"
)

const (
	scanTTL          = time.Minute // Addresses/UTXOs/NextReceiveAddress reuse a scan this fresh
	fetchConcurrency = 4
	maxScanPerChain  = 10_000 // safety stop against a server that reports every address as used
	satsPerBTC       = 100_000_000
)

// addrState is one scanned address.
type addrState struct {
	chain, index int
	address      string
	sats         int64 // confirmed + mempool balance
	txs          int   // confirmed + mempool transaction count
}

type scanResult struct {
	at        time.Time
	chains    [][]addrState // per chain, by index
	lastUsed  []int         // per chain; -1 when no address is used
	truncated bool
}

type esploraStats struct {
	FundedTxoSum int64 `json:"funded_txo_sum"`
	SpentTxoSum  int64 `json:"spent_txo_sum"`
	TxCount      int   `json:"tx_count"`
}

type esploraAddress struct {
	ChainStats   esploraStats `json:"chain_stats"`
	MempoolStats esploraStats `json:"mempool_stats"`
}

type esploraUTXO struct {
	TxID   string `json:"txid"`
	Vout   uint32 `json:"vout"`
	Value  int64  `json:"value"`
	Status struct {
		Confirmed   bool  `json:"confirmed"`
		BlockHeight int64 `json:"block_height"`
	} `json:"status"`
}

func (s *Source) addressURL(a string) string { return s.esplora + "/address/" + url.PathEscape(a) }

// lookup fills a.sats and a.txs from GET /address/{a}.
func (s *Source) lookup(ctx context.Context, a *addrState) error {
	var r esploraAddress
	if err := s.client.GetJSON(ctx, s.addressURL(a.address), nil, &r); err != nil {
		return err
	}
	a.sats = (r.ChainStats.FundedTxoSum - r.ChainStats.SpentTxoSum) + (r.MempoolStats.FundedTxoSum - r.MempoolStats.SpentTxoSum)
	a.txs = r.ChainStats.TxCount + r.MempoolStats.TxCount
	return nil
}

func (s *Source) lookupAll(ctx context.Context, batch []addrState) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fetchConcurrency)
	for i := range batch {
		g.Go(func() error { return s.lookup(gctx, &batch[i]) })
	}
	return g.Wait()
}

// scan looks up every chain and remembers the result for recentScan.
func (s *Source) scan(ctx context.Context) (*scanResult, error) {
	res := &scanResult{at: s.now()}
	for chain := range s.chains {
		states, lastUsed, truncated, err := s.scanChain(ctx, chain)
		if err != nil {
			return nil, err
		}
		res.chains = append(res.chains, states)
		res.lastUsed = append(res.lastUsed, lastUsed)
		res.truncated = res.truncated || truncated
	}
	s.mu.Lock()
	s.last = res
	s.mu.Unlock()
	return res, nil
}

// scanChain fetches addresses in batches of the gap limit until gap consecutive addresses
// after the last used one have no transactions. The result is trimmed to lastUsed+gap+1
// addresses (later ones in the final batch are unused by definition).
func (s *Source) scanChain(ctx context.Context, chain int) ([]addrState, int, bool, error) {
	batchSize, limit := s.gap, maxScanPerChain
	if !s.ranged {
		batchSize, limit = 1, 1
	}
	var all []addrState
	lastUsed, truncated := -1, false
	for len(all)-1-lastUsed < s.gap {
		if len(all) >= limit {
			truncated = s.ranged
			break
		}
		start := len(all)
		batch := make([]addrState, batchSize)
		for j := range batch {
			a, err := s.addressAt(chain, start+j)
			if err != nil {
				return nil, 0, false, err
			}
			batch[j] = addrState{chain: chain, index: start + j, address: a}
		}
		if err := s.lookupAll(ctx, batch); err != nil {
			return nil, 0, false, err
		}
		for _, a := range batch {
			if a.txs > 0 {
				lastUsed = a.index
			}
		}
		all = append(all, batch...)
	}
	if keep := lastUsed + s.gap + 1; keep < len(all) {
		all = all[:keep]
	}
	return all, lastUsed, truncated, nil
}

// recentScan returns the last scan if it is younger than scanTTL, else scans again.
func (s *Source) recentScan(ctx context.Context) (*scanResult, error) {
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	if last != nil && s.now().Sub(last.at) < scanTTL {
		return last, nil
	}
	return s.scan(ctx)
}

func satsToBTC(sats int64) float64 { return float64(sats) / satsPerBTC }

// formatSats renders sats as an exact 8-decimal BTC string.
func formatSats(sats int64) string {
	sign := ""
	if sats < 0 {
		sign, sats = "-", -sats
	}
	return fmt.Sprintf("%s%d.%08d", sign, sats/satsPerBTC, sats%satsPerBTC)
}

// Fetch scans the wallet and returns its BTC balance (always one row, even when zero)
// plus Connection-panel facts. A block-height failure is a warning, not an error.
func (s *Source) Fetch(ctx context.Context) (core.WalletResult, error) {
	res, err := s.scan(ctx)
	if err != nil {
		err = fmt.Errorf("bitcoin wallet scan: %w", err)
		return core.WalletResult{WalletID: s.id, Err: err.Error(), AsOf: s.now()}, err
	}
	var sats int64
	used, scanned := 0, 0
	for _, ch := range res.chains {
		for _, a := range ch {
			sats += a.sats
			scanned++
			if a.txs > 0 {
				used++
			}
		}
	}
	out := core.WalletResult{
		WalletID: s.id,
		AsOf:     res.at,
		Holdings: []core.Holding{{
			WalletID: s.id, AssetKey: "BTC", Symbol: "BTC", Name: "Bitcoin",
			Amount: satsToBTC(sats), AmountStr: formatSats(sats),
		}},
	}
	out.Meta = append(out.Meta, core.KV{K: "Imported via", V: s.importedVia}, core.KV{K: "Script", V: s.script})
	if s.derivation != "" {
		out.Meta = append(out.Meta, core.KV{K: "Derivation", V: s.derivation})
	}
	out.Meta = append(out.Meta,
		core.KV{K: "Addresses used", V: fmt.Sprintf("%d of %d", used, scanned)},
		core.KV{K: "Node", V: s.node})
	if res.truncated {
		out.Warnings = append(out.Warnings, fmt.Sprintf("address scan stopped after %d addresses on one chain", maxScanPerChain))
	}
	var height int64
	if err := s.client.GetJSON(ctx, s.esplora+"/blocks/tip/height", nil, &height); err != nil {
		out.Warnings = append(out.Warnings, "could not read block height: "+err.Error())
	} else {
		out.Meta = append(out.Meta, core.KV{K: "Block height", V: strconv.FormatInt(height, 10)})
	}
	return out, nil
}

// Addresses lists every scanned address with its path ("0/i" receive, "1/i" change;
// empty for a single-address wallet), transaction count and balance.
func (s *Source) Addresses(ctx context.Context) ([]core.Address, error) {
	res, err := s.recentScan(ctx)
	if err != nil {
		return nil, err
	}
	out := []core.Address{}
	for _, ch := range res.chains {
		for _, a := range ch {
			path := ""
			if s.ranged {
				path = fmt.Sprintf("%d/%d", a.chain, a.index)
			}
			out = append(out, core.Address{Address: a.address, Path: path, Change: a.chain == 1, TxCount: a.txs, Balance: satsToBTC(a.sats)})
		}
	}
	return out, nil
}

// UTXOs lists unspent outputs of every address with a positive balance.
func (s *Source) UTXOs(ctx context.Context) ([]core.UTXO, error) {
	res, err := s.recentScan(ctx)
	if err != nil {
		return nil, err
	}
	var funded []addrState
	for _, ch := range res.chains {
		for _, a := range ch {
			if a.sats > 0 {
				funded = append(funded, a)
			}
		}
	}
	lists := make([][]core.UTXO, len(funded))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(fetchConcurrency)
	for i, a := range funded {
		g.Go(func() error {
			var raw []esploraUTXO
			if err := s.client.GetJSON(gctx, s.addressURL(a.address)+"/utxo", nil, &raw); err != nil {
				return err
			}
			for _, u := range raw {
				lists[i] = append(lists[i], core.UTXO{
					TxID: u.TxID, Vout: u.Vout, Address: a.address, Value: satsToBTC(u.Value),
					Confirmed: u.Status.Confirmed, Height: u.Status.BlockHeight,
				})
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := []core.UTXO{}
	for _, l := range lists {
		out = append(out, l...)
	}
	return out, nil
}

// NextReceiveAddress returns the receive address after the last used one (index 0 when
// none is used), or "" for a single-address wallet.
func (s *Source) NextReceiveAddress(ctx context.Context) (string, error) {
	if !s.ranged {
		return "", nil
	}
	res, err := s.recentScan(ctx)
	if err != nil {
		return "", err
	}
	return s.addressAt(0, res.lastUsed[0]+1)
}
