package bitcoin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"

	"golang.org/x/sync/errgroup"
)

const (
	scanTTL          = 10 * time.Minute // Addresses, UTXOs, NextReceiveAddress and Activity reuse a scan this fresh
	fetchConcurrency = 4

	// Between full scans (every fullScanEvery) a refresh reads only the addresses that can
	// change: those holding a balance and the first lookahead unused ones on each chain.
	fullScanEvery = 6 * time.Hour
	lookahead     = 5
	// quietRescan: while the chain tip doesn't move, a scan this fresh is reused. A payment
	// still in the mempool shows at the next block or after quietRescan.
	quietRescan = 15 * time.Minute
	// rateLimitPause is the least time a server that answered 429 is left alone. Public
	// mempool servers block an IP that keeps going after a 429, so it isn't retried.
	rateLimitPause = 5 * time.Minute
	// publicPace spaces the requests to a public Esplora server, across all wallets: a
	// mempool instance allows about one a second after a short burst.
	publicPace      = time.Second
	maxScanPerChain = 10_000 // safety stop against a server that reports every address as used
	satsPerBTC      = 100_000_000

	// With fallback servers, every server but the last one asked gets one attempt of
	// quickTimeout, and a server that doesn't answer is asked last for downFor.
	quickTimeout = 8 * time.Second
	downFor      = 10 * time.Minute

	// scanTimeout bounds a scan, which runs on after its callers give up: through a
	// rate-limited server a full scan can take minutes.
	scanTimeout = 10 * time.Minute
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
	tip       int64         // chain tip height when scanned; 0 when it couldn't be read
	tipErr    string        // why the tip couldn't be read
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

// privateHost reports whether host (with any port) is on this machine or the local
// network, a node of the user's own, which needs no pacing.
func privateHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".home", ".internal", ".home.arpa", ".onion"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// esploraHost validates an Esplora base URL and returns its host.
func esploraHost(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("invalid Esplora URL")
	}
	return u.Host, nil
}

// failFast is c cut to one attempt of quickTimeout, so a server that doesn't
// answer costs seconds before the fallback is asked, not c's full retries.
func failFast(c *httpx.Client) *httpx.Client {
	h := http.Client{}
	if c.HTTP != nil {
		h = *c.HTTP
	}
	h.Timeout = quickTimeout
	return &httpx.Client{HTTP: &h, MaxRetries: 0, BaseDelay: c.BaseDelay, PaceOwn429: true}
}

// ownPacing is c returning 429s at once: fetch pauses the server instead of retrying.
func ownPacing(c *httpx.Client) *httpx.Client {
	cc := *c
	cc.PaceOwn429 = true
	return &cc
}

func addressPath(a string) string { return "/address/" + url.PathEscape(a) }

// get reads path from the Esplora server into out. With a Blockstream API key, Blockstream's
// enterprise server answers first; when it can't, the public servers do.
func (s *Source) get(ctx context.Context, path string, out any) error {
	if s.auth != nil && s.auth.Configured() {
		if err := s.getWithKey(ctx, path, out); err == nil || ctx.Err() != nil {
			return err
		}
	}
	return s.getPublic(ctx, path, out)
}

// getWithKey reads path from Blockstream's enterprise server with the key's access token. A
// token the server refuses is dropped, so the next request fetches a new one.
func (s *Source) getWithKey(ctx context.Context, path string, out any) error {
	tok, err := s.auth.token(ctx)
	keyProblem := err != nil
	if err == nil {
		err = s.fetch(ctx, s.client, s.auth.baseURL, path, out, tok)
		var se *httpx.StatusError
		if errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
			s.auth.drop(tok)
			keyProblem = true
		}
	}
	s.mu.Lock()
	s.keyUsed = err == nil
	switch {
	case err == nil:
		s.keyErr = ""
	case keyProblem:
		s.keyErr = err.Error()
	}
	s.mu.Unlock()
	return err
}

// esploraServer is one Esplora server: its API base URL and host.
type esploraServer struct{ base, host string }

// getPublic reads path from the servers in order. One that doesn't answer (a network error,
// a timeout, 429 or 5xx) is asked last for downFor, while the next one answers; when
// every other server fails too it still gets its turn, as it may be back.
func (s *Source) getPublic(ctx context.Context, path string, out any) error {
	order := s.serverOrder()
	var err error
	for i, srv := range order {
		c := s.fast
		if i == len(order)-1 {
			c = s.client // the last resort gets the full retries
		}
		if err = s.fetch(ctx, c, srv.base, path, out, ""); err == nil {
			s.mu.Lock()
			delete(s.down, srv.base)
			s.answeredBy = srv.host
			s.mu.Unlock()
			return nil
		}
		if ctx.Err() != nil || !unavailable(err) {
			return err
		}
		s.markDown(srv, err)
	}
	return err
}

// serverOrder is the servers answering lately, in configured order, then the others.
func (s *Source) serverOrder() []esploraServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var up, down []esploraServer
	for _, srv := range s.servers {
		if s.now().Before(s.down[srv.base]) {
			down = append(down, srv)
		} else {
			up = append(up, srv)
		}
	}
	return append(up, down...)
}

// markDown moves srv to the back of the order for downFor.
func (s *Source) markDown(srv esploraServer, err error) {
	s.mu.Lock()
	first := !s.now().Before(s.down[srv.base])
	s.down[srv.base] = s.now().Add(downFor)
	s.mu.Unlock()
	if first && len(s.servers) > 1 {
		slog.Warn("bitcoin: Esplora server isn't answering; trying the next one", "wallet", s.id, "server", srv.host, "reason", downReason(err))
	}
}

// downReason says why a server was passed over, without the URL (it names an address).
func downReason(err error) string {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return "HTTP " + strconv.Itoa(se.Status)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "no answer"
	}
	return "connection failed"
}

// usingFallback reports whether the last request was answered by a server other than the
// configured one.
func (s *Source) usingFallback() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answeredBy != "" && s.answeredBy != s.node
}

// serverPauses holds, by server base URL, until when a server that answered 429 is left
// alone. It is shared by every wallet, so one wallet's 429 keeps all of them off the server.
var serverPauses = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

// fetch GETs base+path with c, sending token as a bearer token when set. A server that
// answers 429 is left alone for its Retry-After, at least rateLimitPause: until then fetch
// fails at once with a 429 of its own, so no wallet keeps spending the server's request
// allowance.
func (s *Source) fetch(ctx context.Context, c *httpx.Client, base, path string, out any, token string) error {
	serverPauses.Lock()
	until := serverPauses.until[base]
	serverPauses.Unlock()
	if s.now().Before(until) {
		return &httpx.StatusError{Status: http.StatusTooManyRequests, URL: base,
			Body: "paused until " + until.Format(time.Kitchen) + " after the server asked CryptKeeper to slow down"}
	}
	var headers map[string]string
	if token != "" {
		headers = map[string]string{"Authorization": "Bearer " + token}
	}
	err := c.GetJSON(ctx, base+path, headers, out)
	var se *httpx.StatusError
	if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
		serverPauses.Lock()
		serverPauses.until[base] = s.now().Add(max(se.RetryAfter, rateLimitPause))
		serverPauses.Unlock()
	}
	return err
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
//
// Concurrent callers (the balance refresh and the activity sync) share one scan. It runs on
// its own for up to scanTimeout, so a caller that gives up on a slow, rate-limited server
// doesn't lose it: the scan finishes and is kept for the next call.
func (s *Source) scan(ctx context.Context) (*scanResult, error) {
	ch := s.scans.DoChan("scan", func() (any, error) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scanTimeout)
		defer cancel()
		return s.runScan(sctx)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*scanResult), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Source) runScan(ctx context.Context) (*scanResult, error) {
	last := s.lastScan(ctx)
	res := &scanResult{at: s.now(), fullAt: s.now()}
	if err := s.get(ctx, "/blocks/tip/height", &res.tip); err != nil {
		res.tip, res.tipErr = 0, err.Error()
	}
	light := last != nil && s.ranged && s.now().Sub(last.fullAt) < fullScanEvery
	if light && res.tip != 0 && res.tip == last.tip && s.now().Sub(last.at) < quietRescan {
		return last, nil // no new block since a recent scan: nothing confirmed has changed
	}
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
	s.saveScan(ctx, res)
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
	s.mu.Lock()
	keyUsed, keyErr := s.keyUsed, s.keyErr
	s.mu.Unlock()
	switch {
	case keyUsed:
		host, _ := esploraHost(s.auth.baseURL)
		node = host + " (API key)"
	case s.usingFallback():
		s.mu.Lock()
		node = s.answeredBy
		s.mu.Unlock()
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s isn't answering; using %s for now.", s.node, node))
	}
	if keyErr != "" {
		out.Warnings = append(out.Warnings, "Blockstream API key isn't working ("+keyErr+"); using the public servers.")
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
	if res.tip == 0 {
		out.Warnings = append(out.Warnings, "could not read block height: "+res.tipErr)
	} else {
		out.Meta = append(out.Meta, core.KV{K: "Block height", V: strconv.FormatInt(res.tip, 10)})
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
