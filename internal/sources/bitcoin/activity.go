package bitcoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
)

var _ core.ActivitySource = (*Source)(nil)

const (
	// defaultActivityBudget bounds the Esplora requests of one Activity call when the
	// request names no budget.
	defaultActivityBudget = 30
	// activityStateVersion is bumped when the State layout changes; unknown versions
	// are discarded and the history is fetched again (Tx ids are stable, so that is safe).
	activityStateVersion = 1
)

// activityState is the opaque State handed back and forth through core.ActivityRequest.
type activityState struct {
	Version int                    `json:"v"`
	Tip     int64                  `json:"tip,omitempty"` // block height seen by the last call
	Addrs   map[string]*addrCursor `json:"addrs,omitempty"`
	// Pending holds every transaction last seen unconfirmed, with the entry that was
	// emitted for it, so a later status check can re-emit it without refetching the
	// transaction (and can still describe it after it vanished from the mempool).
	Pending []pendingTx `json:"pending,omitempty"`
}

// pendingTx is one unconfirmed transaction and the entry emitted for it.
type pendingTx struct {
	TxID  string   `json:"txid"`
	Tx    core.Tx  `json:"tx"`
	Addrs []string `json:"addrs,omitempty"` // wallet addresses the tx touches
}

// addrCursor is how far one address's confirmed history has been read. Esplora lists a
// history newest first, so the confirmed txs from Newest down to Oldest have all been
// emitted. Unconfirmed txs are never cursor anchors (they move, get replaced or vanish);
// they are tracked through activityState.Pending instead.
type addrCursor struct {
	TxCount int    `json:"txCount"`          // tx_count (chain+mempool) when the newest page was last read
	Newest  string `json:"newest,omitempty"` // newest confirmed txid covered
	Oldest  string `json:"oldest,omitempty"` // oldest confirmed txid covered
	Done    bool   `json:"done"`             // nothing older than Oldest is wanted (history ended, or reached Since)
	// DoneSince is the Since that ended the backfill; 0 when the history itself ended.
	// A later request with an earlier Since reopens the backfill.
	DoneSince int64 `json:"doneSince,omitempty"`
	// Top and Next describe a new-activity pass that ran out of budget between pages:
	// Top is the newest txid of the pass, Next the last txid read, from which paging resumes.
	Top  string `json:"top,omitempty"`
	Next string `json:"next,omitempty"`
}

// Esplora transaction, as served by /address/{a}/txs. Only the fields Activity reads.
type esploraTx struct {
	TxID   string          `json:"txid"`
	Fee    int64           `json:"fee"`
	Vin    []esploraVin    `json:"vin"`
	Vout   []esploraOut    `json:"vout"`
	Status esploraTxStatus `json:"status"`
}

type esploraVin struct {
	Prevout *esploraOut `json:"prevout"` // null for a coinbase input
}

type esploraOut struct {
	Address string `json:"scriptpubkey_address"` // absent for OP_RETURN and other unaddressable scripts
	Value   int64  `json:"value"`
}

// esploraTxStatus is the status object of a transaction and the whole /tx/{txid}/status reply.
type esploraTxStatus struct {
	Confirmed   bool  `json:"confirmed"`
	BlockHeight int64 `json:"block_height"`
	BlockTime   int64 `json:"block_time"`
}

// Activity lists the wallet's transactions, one core.Tx per txid for the whole wallet.
//
// It works from the same address scan Fetch uses (a scan younger than a minute is reused;
// the scan's own requests are not charged to req.Budget). In priority order, with every
// Esplora request charged to req.Budget (30 when zero):
//
//  1. the block height, once;
//  2. pending txs seen unconfirmed earlier are checked with /tx/{txid}/status and re-emitted
//     once confirmed. A tx the node no longer knows is marked failed, and the addresses it
//     touched are read again in step 3: an RBF replacement leaves their tx_count unchanged;
//  3. new activity: addresses whose tx_count differs from State are read from the newest
//     page down to the last confirmed tx already covered;
//  4. backfill: older history is paged address by address, one page per address per round,
//     until a tx is older than req.Since or the history ends.
//
// BackfillDone is true once every address is read down to Since. An exhausted budget stops
// the call cleanly: the returned State resumes where it stopped and no tx is emitted twice.
//
// If a request fails, the error is returned together with a result that still carries
// everything fetched before the failure; its State resumes from that point.
func (s *Source) Activity(ctx context.Context, req core.ActivityRequest) (core.ActivityResult, error) {
	scan, err := s.recentScan(ctx)
	if err != nil {
		return core.ActivityResult{}, fmt.Errorf("bitcoin wallet scan: %w", err)
	}
	r := newActivityRun(s, ctx, req, scan)
	r.fetchTip()
	r.recheckPending()
	r.newActivity()
	r.backfill()
	return r.result()
}

// addrTxs is one scanned address with its current transaction count.
type addrTxs struct {
	address string
	txs     int
}

// activityRun is the working state of one Activity call.
type activityRun struct {
	s         *Source
	ctx       context.Context
	since     time.Time
	budget    int
	err       error // first request failure; stops all further work
	exhausted bool  // the budget ran out while there was still work to do

	state   *activityState
	wallet  map[string]bool // every scanned address
	addrs   []addrTxs       // scan order: receive chain first, then change
	tip     int64
	scanTip int64 // the tip the wallet scan read moments ago; 0 when the scan is older
	pending map[string]pendingTx

	fetched map[string]*esploraTx // txs read this call, by txid
	order   []string              // txids in fetch order
	emit    map[string]core.Tx    // entries already built (pending checks)
}

func newActivityRun(s *Source, ctx context.Context, req core.ActivityRequest, scan *scanResult) *activityRun {
	r := &activityRun{
		s:       s,
		ctx:     ctx,
		since:   req.Since,
		budget:  req.Budget,
		wallet:  map[string]bool{},
		pending: map[string]pendingTx{},
		fetched: map[string]*esploraTx{},
		emit:    map[string]core.Tx{},
	}
	if r.budget <= 0 {
		r.budget = defaultActivityBudget
	}
	for _, chain := range scan.chains {
		for _, a := range chain {
			r.wallet[a.address] = true
			r.addrs = append(r.addrs, addrTxs{a.address, a.txs})
		}
	}
	if s.now().Sub(scan.at) < time.Minute {
		r.scanTip = scan.tip
	}
	r.state = decodeActivityState(req.State)
	r.tip = r.state.Tip
	for addr, c := range r.state.Addrs {
		switch {
		case !r.wallet[addr]:
			delete(r.state.Addrs, addr)
		case c.Done && c.DoneSince > 0 && (r.since.IsZero() || r.since.Unix() < c.DoneSince):
			// Since moved further back than the backfill went: read on.
			if c.Oldest == "" {
				delete(r.state.Addrs, addr) // nothing was kept, start over
			} else {
				c.Done, c.DoneSince = false, 0
			}
		}
	}
	for _, p := range r.state.Pending {
		r.pending[p.TxID] = p
	}
	return r
}

// decodeActivityState parses a previous State; nil, garbage and other versions give a fresh one.
func decodeActivityState(raw []byte) *activityState {
	st := &activityState{}
	if len(raw) > 0 && (json.Unmarshal(raw, st) != nil || st.Version != activityStateVersion) {
		st = &activityState{}
	}
	st.Version = activityStateVersion
	if st.Addrs == nil {
		st.Addrs = map[string]*addrCursor{}
	}
	for addr, c := range st.Addrs {
		if c == nil {
			delete(st.Addrs, addr)
		}
	}
	return st
}

// take charges one request to the budget and reports whether it may be made.
func (r *activityRun) take() bool {
	if r.err != nil || r.exhausted {
		return false
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return false
	}
	if r.budget <= 0 {
		r.exhausted = true
		return false
	}
	r.budget--
	return true
}

func (r *activityRun) stopped() bool { return r.err != nil || r.exhausted }

func (r *activityRun) get(path string, out any) error {
	return r.s.get(r.ctx, path, out)
}

// page reads one list of transactions; failures are recorded in r.err.
func (r *activityRun) page(path string) ([]*esploraTx, bool) {
	var txs []*esploraTx
	if err := r.get(path, &txs); err != nil {
		r.err = err
		return nil, false
	}
	return txs, true
}

func addressTxsPath(addr, last string) string {
	p := "/address/" + url.PathEscape(addr) + "/txs"
	if last != "" {
		p += "/chain/" + last
	}
	return p
}

// fetchTip reads the block height used for confirmation counts. A failure is not fatal:
// the height of the previous call is used instead (counts then run low, never high).
func (r *activityRun) fetchTip() {
	if !r.take() {
		return
	}
	if r.scanTip > 0 { // the scan just read it; still charged, so budgets mean the same
		r.tip = r.scanTip
		return
	}
	var h int64
	if err := r.get("/blocks/tip/height", &h); err == nil && h > 0 {
		r.tip = h
	}
}

// ingest remembers a transaction read from Esplora. The same tx arrives once per wallet
// address it touches; a confirmed copy wins over an unconfirmed one.
func (r *activityRun) ingest(tx *esploraTx) {
	old, seen := r.fetched[tx.TxID]
	if seen && (old.Status.Confirmed || !tx.Status.Confirmed) {
		return
	}
	if !seen {
		r.order = append(r.order, tx.TxID)
	}
	r.fetched[tx.TxID] = tx
}

// beforeSince reports whether a confirmed tx is older than the requested window.
func (r *activityRun) beforeSince(tx *esploraTx) bool {
	return !r.since.IsZero() && tx.Status.BlockTime > 0 && time.Unix(tx.Status.BlockTime, 0).Before(r.since)
}

// newActivity reads the newest page of every address whose tx_count changed (or whose
// previous pass was interrupted) and pages older until it meets the confirmed tx it
// already covers.
func (r *activityRun) newActivity() {
	for _, a := range r.addrs {
		if r.stopped() {
			return
		}
		c := r.state.Addrs[a.address]
		switch {
		case c == nil && a.txs == 0:
			continue
		case c != nil && c.Next == "" && c.TxCount == a.txs:
			continue
		case c == nil:
			c = &addrCursor{}
			r.state.Addrs[a.address] = c
		}
		r.refresh(a, c)
	}
}

// refresh runs one new-activity pass for an address, committing progress to c after every
// page so that a stop at any point resumes without gaps or repeats.
func (r *activityRun) refresh(a addrTxs, c *addrCursor) {
	anchor := c.Newest
	top, last := c.Top, c.Next // non-empty when resuming an interrupted pass
	resumed := last != ""
	for {
		if !r.take() {
			return
		}
		page, ok := r.page(addressTxsPath(a.address, last))
		if !ok {
			return
		}
		var reached, cut bool
		n := 0 // confirmed txs taken from this page
		for _, tx := range page {
			if !tx.Status.Confirmed { // mempool part, only on the first page
				r.ingest(tx)
				continue
			}
			if anchor != "" && tx.TxID == anchor {
				reached = true
				break
			}
			if r.beforeSince(tx) {
				cut = true
				break
			}
			r.ingest(tx)
			if top == "" {
				top = tx.TxID
			}
			last = tx.TxID
			n++
		}
		switch {
		case reached: // the new txs now sit directly above the old range
			if top != "" {
				c.Newest = top
			}
		case cut: // the rest is older than the window; the range is what was taken
			c.Newest, c.Oldest = top, last
			c.Done, c.DoneSince = true, r.since.Unix()
		case n == 0: // history ended without meeting the anchor: everything was read
			c.Newest, c.Oldest = top, last
			c.Done, c.DoneSince = true, 0
		case anchor == "": // first sight: the newest page will do, backfill continues from it
			c.Newest, c.Oldest = top, last
			c.Done, c.DoneSince = false, 0
		default: // more new txs below this page
			c.Top, c.Next = top, last
			continue
		}
		c.Top, c.Next, c.TxCount = "", "", a.txs
		if resumed {
			// The first page was read in an earlier call, so txs may have landed above it
			// since, and a.txs counts them. Look at the newest page once more.
			c.TxCount = -1
			r.refresh(a, c)
		}
		return
	}
}

// recheckPending asks for the status of every pending tx. Confirmed ones are re-emitted from
// the entry kept in State. One the node no longer knows (replaced or dropped) is marked
// failed, and the addresses it touched are flagged for a fresh read: a replacement keeps their
// tx_count unchanged, so the count alone would never reveal it.
func (r *activityRun) recheckPending() {
	ids := make([]string, 0, len(r.pending))
	for id := range r.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !r.take() {
			return
		}
		var st esploraTxStatus
		err := r.get("/tx/"+url.PathEscape(id)+"/status", &st)
		var se *httpx.StatusError
		switch {
		case errors.As(err, &se) && se.Status == http.StatusNotFound:
			p := r.pending[id]
			p.Tx.Status = core.TxFailed
			p.Tx.Note = "no longer in the mempool: replaced or dropped"
			r.emit[p.Tx.ID] = p.Tx
			delete(r.pending, id)
			for _, addr := range p.Addrs {
				if c := r.state.Addrs[addr]; c != nil {
					c.TxCount = -1
				}
			}
		case err != nil:
			r.err = err
			return
		case st.Confirmed:
			t := r.pending[id].Tx
			t.Status = core.TxConfirmed
			if st.BlockTime > 0 {
				t.Time = time.Unix(st.BlockTime, 0).UTC()
			}
			t.Confirmations = r.confirmations(st.BlockHeight)
			r.emit[t.ID] = t
			delete(r.pending, id)
		}
	}
}

// backfill pages older history, one page per unfinished address per round, so recent
// history of every address arrives before the deep history of any.
func (r *activityRun) backfill() {
	for {
		progressed := false
		for _, a := range r.addrs {
			c := r.state.Addrs[a.address]
			if c == nil || c.Done || c.Next != "" || c.Oldest == "" {
				continue
			}
			if !r.take() {
				return
			}
			page, ok := r.page(addressTxsPath(a.address, c.Oldest))
			if !ok {
				return
			}
			progressed = true
			n := 0
			for _, tx := range page {
				if !tx.Status.Confirmed {
					continue
				}
				if r.beforeSince(tx) {
					c.Done, c.DoneSince = true, r.since.Unix()
					break
				}
				r.ingest(tx)
				c.Oldest = tx.TxID
				n++
			}
			if n == 0 && !c.Done { // an empty page is the end of the history
				c.Done, c.DoneSince = true, 0
			}
		}
		if !progressed {
			return
		}
	}
}

// backfillDone reports whether every used address is read down to Since.
func (r *activityRun) backfillDone() bool {
	if r.err != nil {
		return false
	}
	for _, a := range r.addrs {
		c := r.state.Addrs[a.address]
		switch {
		case c == nil:
			if a.txs > 0 {
				return false
			}
		case !c.Done || c.Next != "" || c.TxCount != a.txs:
			return false
		}
	}
	return true
}

// result turns everything fetched into entries and encodes the next State.
func (r *activityRun) result() (core.ActivityResult, error) {
	for _, id := range r.order {
		t, ok := r.build(r.fetched[id])
		if !ok {
			continue
		}
		if t.Status == core.TxPending {
			if _, known := r.pending[id]; known {
				continue // still unconfirmed and already emitted: nothing changed
			}
			r.pending[id] = pendingTx{TxID: id, Tx: t, Addrs: r.walletAddrs(r.fetched[id])}
		} else {
			delete(r.pending, id)
		}
		r.emit[t.ID] = t
	}
	txs := make([]core.Tx, 0, len(r.emit))
	for _, t := range r.emit {
		txs = append(txs, t)
	}
	sort.Slice(txs, func(i, j int) bool {
		if !txs[i].Time.Equal(txs[j].Time) {
			return txs[i].Time.After(txs[j].Time)
		}
		return txs[i].ID < txs[j].ID
	})

	st := r.state
	st.Tip = r.tip
	st.Pending = st.Pending[:0]
	for _, p := range r.pending {
		st.Pending = append(st.Pending, p)
	}
	sort.Slice(st.Pending, func(i, j int) bool { return st.Pending[i].TxID < st.Pending[j].TxID })
	raw, err := json.Marshal(st)
	if err != nil {
		return core.ActivityResult{}, fmt.Errorf("bitcoin activity state: %w", err)
	}
	res := core.ActivityResult{Txs: txs, State: raw, BackfillDone: r.backfillDone()}
	if r.err != nil {
		return res, fmt.Errorf("bitcoin activity: %w", r.err)
	}
	return res, nil
}

func (r *activityRun) confirmations(height int64) int {
	switch {
	case height <= 0 || r.tip <= 0:
		return 0
	case r.tip < height:
		return 1
	}
	return int(r.tip - height + 1)
}

// build turns an Esplora transaction into the wallet's view of it. It reports false for a
// tx that moves nothing into or out of the wallet.
//
// net is what the wallet's balance changed by: outputs to wallet addresses minus the
// prevouts it spent. When the wallet funded inputs, it paid the miner fee, capped at what
// it lost (in a collaborative tx the other parties pay their share); Legs then carry the
// amount that left excluding the fee, so Legs + Fee always equals net.
func (r *activityRun) build(tx *esploraTx) (core.Tx, bool) {
	var received, spent int64
	var fromWallet bool
	var firstIn, firstOut string
	for _, in := range tx.Vin {
		p := in.Prevout
		switch {
		case p == nil:
		case r.wallet[p.Address]:
			spent += p.Value
			fromWallet = true
		case firstIn == "":
			firstIn = p.Address
		}
	}
	touched := fromWallet
	for _, o := range tx.Vout {
		switch {
		case r.wallet[o.Address]:
			received += o.Value
			touched = true
		case firstOut == "":
			firstOut = o.Address
		}
	}
	if !touched {
		return core.Tx{}, false
	}

	net := received - spent
	var fee int64
	if fromWallet && net < 0 {
		fee = min(tx.Fee, -net)
	}
	moved := net + fee

	t := core.Tx{
		ID:          core.TxID(r.s.id, tx.TxID),
		SourceID:    r.s.id,
		ExternalID:  tx.TxID,
		Legs:        []core.Leg{},
		TxHash:      tx.TxID,
		ExplorerURL: r.s.explorerTxURL(tx.TxID),
	}
	switch {
	case net > 0:
		t.Kind = core.TxReceive
		t.Counterparty = firstIn
	case moved < 0:
		t.Kind = core.TxSend
		t.Counterparty = firstOut
	case received > 0:
		t.Kind = core.TxTransfer // a self-send: only the fee left the wallet
	case fee > 0:
		t.Kind = core.TxFee // everything went to the miner or an unaddressable output
	default:
		t.Kind = core.TxOther
	}
	if moved != 0 {
		t.Legs = append(t.Legs, btcLeg(moved))
	}
	if fee > 0 {
		f := btcLeg(-fee)
		t.Fee = &f
	}

	if tx.Status.Confirmed {
		t.Status = core.TxConfirmed
		t.Time = time.Unix(tx.Status.BlockTime, 0).UTC()
		t.Confirmations = r.confirmations(tx.Status.BlockHeight)
	} else {
		t.Status = core.TxPending
		t.Time = r.s.now().UTC()
	}
	return t, true
}

// walletAddrs lists the wallet addresses a transaction spends from or pays to.
func (r *activityRun) walletAddrs(tx *esploraTx) []string {
	set := map[string]bool{}
	for _, in := range tx.Vin {
		if in.Prevout != nil && r.wallet[in.Prevout.Address] {
			set[in.Prevout.Address] = true
		}
	}
	for _, o := range tx.Vout {
		if r.wallet[o.Address] {
			set[o.Address] = true
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func btcLeg(sats int64) core.Leg {
	return core.Leg{AssetKey: "BTC", Symbol: "BTC", Amount: satsToBTC(sats)}
}

// explorerTxURL links a tx on mempool.space when that is the configured Esplora host;
// for any other node there is no known explorer, so the link is left empty.
func (s *Source) explorerTxURL(txid string) string {
	u, err := url.Parse(s.esplora)
	if err != nil || u.Hostname() != "mempool.space" {
		return ""
	}
	return "https://mempool.space/tx/" + txid
}
