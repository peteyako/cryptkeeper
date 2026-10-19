package bitcoin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"

	"golang.org/x/sync/errgroup"
)

const (
	scanTTL          = 10 * time.Minute // Addresses, UTXOs, NextReceiveAddress and Activity reuse a scan this fresh
	fetchConcurrency = 4

	// Between full scans (every fullScanEvery) a refresh reads only the addresses that can
	// change: those holding a balance and the first lookahead unused ones on each chain.
	fullScanEvery = 6 * time.Hour
	lookahead     = 5
	// rateLimitPause is the least time a server that answered 429 is left alone.
	rateLimitPause  = 5 * time.Minute
	maxScanPerChain = 10_000 // safety stop against a server that reports every address as used
	satsPerBTC      = 100_000_000

	// With a fallback server, one primary request gets primaryTimeout, and a primary that
	// doesn't answer is skipped for primaryRetry before it is tried again.
	primaryTimeout = 8 * time.Second
	primaryRetry   = 10 * time.Minute
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
	fullAt    time.Time     // when every address was last read
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

// esploraHost validates an Esplora base URL and returns its host.
func esploraHost(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("invalid Esplora URL")
	}
	return u.Host, nil
}

// failFast is c cut to one attempt of primaryTimeout, so a primary server that doesn't
// answer costs seconds before the fallback is asked, not c's full retries.
func failFast(c *httpx.Client) *httpx.Client {
	h := http.Client{}
	if c.HTTP != nil {
		h = *c.HTTP
	}
	h.Timeout = primaryTimeout
	return &httpx.Client{HTTP: &h, MaxRetries: 0, BaseDelay: c.BaseDelay}
}

func addressPath(a string) string { return "/address/" + url.PathEscape(a) }

// get reads path from the Esplora server into out. With a fallback, a primary that doesn't
// answer (a network error, a timeout, 429 or 5xx) hands the request to the fallback, which
// then serves every request for primaryRetry.
func (s *Source) get(ctx context.Context, path string, out any) error {
	if s.fallback == "" {
		return s.fetch(ctx, s.client, s.esplora, path, out)
	}
	if !s.usingFallback() {
		err := s.fetch(ctx, s.fast, s.esplora, path, out)
		if err == nil || ctx.Err() != nil || !unavailable(err) {
			return err
		}
		s.mu.Lock()
		first := !s.now().Before(s.primaryDown)
		s.primaryDown = s.now().Add(primaryRetry)
		s.mu.Unlock()
		if first {
			slog.Warn("bitcoin: Esplora server isn't answering; using the fallback", "wallet", s.id, "server", s.node, "fallback", s.fallbackNode)
		}
	}
	return s.fetch(ctx, s.client, s.fallback, path, out)
}

// fetch GETs base+path with c. A server that answers 429 is left alone for its Retry-After,
// at least rateLimitPause: until then fetch fails at once with a 429 of its own, so a scan
// doesn't keep spending the server's request allowance.
func (s *Source) fetch(ctx context.Context, c *httpx.Client, base, path string, out any) error {
	s.mu.Lock()
	until := s.paused[base]
	s.mu.Unlock()
	if s.now().Before(until) {
		return &httpx.StatusError{Status: http.StatusTooManyRequests, URL: base,
			Body: "paused until " + until.Format(time.Kitchen) + " after the server asked CryptKeep to slow down"}
	}
	err := c.GetJSON(ctx, base+path, nil, out)
	var se *httpx.StatusError
	if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
		s.mu.Lock()
		s.paused[base] = s.now().Add(max(se.RetryAfter, rateLimitPause))
		s.mu.Unlock()
	}
	return err
}

// usingFallback reports whether the primary server is being skipped.
func (s *Source) usingFallback() bool {
	if s.fallback == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Before(s.primaryDown)
}

// unavailable reports whether err means the server didn't answer, rather than answered no.
func unavailable(err error) bool {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	return true
}

// lookup fills a.sats and a.txs from GET /address/{a}.
func (s *Source) lookup(ctx context.Context, a *addrState) error {
	var r esploraAddress
	if err := s.get(ctx, addressPath(a.address), &r); err != nil {
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

// scan brings the wallet up to date and remembers the result for recentScan. A full scan
// reads every address up to the gap limit. Public Esplora servers allow a few hundred
// requests an hour, so between full scans (every fullScanEvery) a refresh reads only the
// addresses that can change: those holding a balance (they can be spent from) and the first
// lookahead unused ones on each chain (where new payments and change arrive). A payment to an
// old, emptied address shows up at the next full scan.
func (s *Source) scan(ctx context.Context) (*scanResult, error) {
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	res := &scanResult{at: s.now(), fullAt: s.now()}
	light := last != nil && s.ranged && s.now().Sub(last.fullAt) < fullScanEvery
	if light {
		res.fullAt = last.fullAt
	}
	for chain := range s.chains {
		var states []addrState
		lastUsed := -1
		if light {
			var err error
			if states, lastUsed, err = s.recheck(ctx, last.chains[chain]); err != nil {
				return nil, err
			}
		}
		states, lastUsed, truncated, err := s.scanChain(ctx, chain, states, lastUsed)
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

// recheck reads again the addresses of a previous chain scan that can change, and returns
// the updated chain with its last used index.
func (s *Source) recheck(ctx context.Context, prev []addrState) ([]addrState, int, error) {
	states := slices.Clone(prev)
	lastUsed := lastUsedIndex(states)
	var idx []int
	for i, a := range states {
		if a.sats != 0 || (i > lastUsed && i <= lastUsed+lookahead) {
			idx = append(idx, i)
		}
	}
	batch := make([]addrState, len(idx))
	for j, i := range idx {
		batch[j] = states[i]
	}
	if err := s.lookupAll(ctx, batch); err != nil {
		return nil, 0, err
	}
	for j, i := range idx {
		states[i] = batch[j]
	}
	return states, lastUsedIndex(states), nil
}

// lastUsedIndex is the index of the last address with transactions, -1 when none is used.
func lastUsedIndex(states []addrState) int {
	for i := len(states) - 1; i >= 0; i-- {
		if states[i].txs > 0 {
			return states[i].index
		}
	}
	return -1
}

// scanChain continues a chain scan from the addresses already read (none for a full scan):
// it fetches further addresses in batches of the gap limit until gap consecutive addresses
// after the last used one have no transactions. The result is trimmed to lastUsed+gap+1
// addresses (later ones in the final batch are unused by definition).
func (s *Source) scanChain(ctx context.Context, chain int, all []addrState, lastUsed int) ([]addrState, int, bool, error) {
	batchSize, limit := s.gap, maxScanPerChain
	if !s.ranged {
		batchSize, limit = 1, 1
	}
	truncated := false
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
	node := s.node
	if s.usingFallback() {
		node = s.fallbackNode
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s isn't answering; using %s for now.", s.node, s.fallbackNode))
	}
	out.Meta = append(out.Meta,
		core.KV{K: "Addresses used", V: fmt.Sprintf("%d of %d", used, scanned)},
		core.KV{K: "Node", V: node})
	if res.truncated {
		out.Warnings = append(out.Warnings, fmt.Sprintf("address scan stopped after %d addresses on one chain", maxScanPerChain))
	}
	if used == 0 && s.autoKey != nil {
		out.Warnings = append(out.Warnings, s.scriptHint(ctx))
	}
	var height int64
	if err := s.get(ctx, "/blocks/tip/height", &height); err != nil {
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
			if err := s.get(gctx, addressPath(a.address)+"/utxo", &raw); err != nil {
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

// scriptHint explains an empty wallet whose xpub was read as Legacy. It checks the first
// receive address of each other script type once and names the one with history; while
// the server can't be reached it gives the general advice and checks again next time.
func (s *Source) scriptHint(ctx context.Context) string {
	s.mu.Lock()
	hint, done := s.hint, s.hintDone
	s.mu.Unlock()
	if done {
		return hint
	}
	hint = "No transactions at this key's Legacy (1…) addresses. Ledger and some other wallets show an xpub " +
		"for SegWit and Taproot accounts too: if this account's addresses start with bc1q, 3 or bc1p, " +
		"edit the wallet and set its Script type."
	for _, st := range []scriptType{scriptP2WPKH, scriptP2SHP2WPKH, scriptP2TR} {
		a, err := descriptorFromXpub(s.autoKey, st).address(0, 0)
		if err != nil {
			continue
		}
		var r esploraAddress
		if err := s.get(ctx, addressPath(a.EncodeAddress()), &r); err != nil {
			return hint
		}
		if r.ChainStats.TxCount+r.MempoolStats.TxCount > 0 {
			name, _, _ := strings.Cut(st.label(), " (")
			hint = fmt.Sprintf("No transactions at this key's Legacy (1…) addresses, but its %s addresses have history: "+
				"edit the wallet and set Script type to %s.", st.label(), name)
			break
		}
	}
	s.mu.Lock()
	s.hint, s.hintDone = hint, true
	s.mu.Unlock()
	return hint
}
