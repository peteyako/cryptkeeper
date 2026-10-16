package kraken

// Activity: the account's ledger from POST /0/private/Ledgers
// (https://docs.kraken.com/api/docs/rest-api/get-ledgers-info), 50 entries per call, newest first.
// Entries are paged with `ofs`; the ledger is append-only, so the position of an entry grows by the
// number of entries added above it.
//
// State is a list of runs. A run is a stretch of the ledger that has been processed completely:
// the id of its newest entry, the id of its oldest entry and its length. After a normal call there
// is exactly one run, starting at the newest entry known then.
//
// A call first asks for the page right behind the first run (offset = the run's length). Every
// answer carries the size of the ledger, and State remembers the size it had when the run's head
// was fixed, so that one answer tells whether anything new arrived. If not, that page is simply the
// next page of the backfill and the call carries on with older history until the Since horizon,
// the end of the ledger or the budget. If so, the page is dropped, and the call pages from ofs=0
// until it meets the run's newest entry (everything above it is new activity), jumps over the run
// and carries on as before.
//
// A call that runs out of budget while still reading new activity leaves a second, older run
// behind, with an unread gap between the two; the next call picks up behind the first run, closes
// the gap and merges the runs, so no call ever has to read the same entries twice, and even a
// budget of one request makes progress.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
)

var _ core.ActivitySource = (*Source)(nil)

const (
	ledgersPath           = "/0/private/Ledgers"
	ledgerPageSize        = 50 // entries per Ledgers call, fixed by Kraken
	defaultActivityBudget = 6
	// maxActivityBudget caps Ledgers calls per Activity call regardless of what the syncer
	// asks for: each costs 2 points against Kraken's starter-tier limit of 15.
	maxActivityBudget = 6
)

// ErrRateLimited is returned (wrapped) by Activity when Kraken throttles the key. The progress
// made before that is still returned; call again later. Ledgers calls cost 2 points on a counter
// that decays by about 0.33 per second on the starter tier, so a burst of more than about 7
// calls is throttled.
var ErrRateLimited = errors.New("kraken: rate limited")

// ---------------------------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------------------------

type ledgerRun struct {
	ID   string `json:"id"`   // newest entry of the run
	Last string `json:"last"` // oldest entry of the run
	N    int    `json:"n"`    // entries in the run
}

type activityState struct {
	Runs []ledgerRun `json:"runs,omitempty"`
	// Done means the oldest entry inside the Since horizon (or of the ledger) has been reached.
	Done bool `json:"done,omitempty"`
	// Since is the horizon (unix seconds, 0 = none) Done refers to.
	Since int64 `json:"since,omitempty"`
	// Count is the size of the ledger (as filtered by Start) when the first run's head was fixed:
	// a later answer with a different size means entries were added above the run. 0 = unknown.
	Count int64 `json:"count,omitempty"`
	// Start is the start parameter Count was taken with; a size is only comparable to one taken
	// with the same Start.
	Start int64 `json:"start,omitempty"`
	// Sweep means new entries are known to be waiting above the first run, so the next call
	// reads from the top.
	Sweep bool `json:"sweep,omitempty"`
}

func loadState(raw []byte) activityState {
	var st activityState
	// State is only a set of cursors: if it cannot be read, starting over re-fetches history and
	// the caller's store (keyed by Tx.ID) absorbs the repeats.
	if len(raw) == 0 || json.Unmarshal(raw, &st) != nil {
		return activityState{}
	}
	return st
}

// ---------------------------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------------------------

type ledgerEntry struct {
	ID      string      `json:"-"` // the key in result.ledger
	RefID   string      `json:"refid"`
	Time    json.Number `json:"time"` // unix seconds with decimals
	Type    string      `json:"type"`
	Subtype string      `json:"subtype"`
	Aclass  string      `json:"aclass"`
	Asset   string      `json:"asset"`
	Amount  amount      `json:"amount"`
	Fee     amount      `json:"fee"`
	Balance amount      `json:"balance"`
}

// orderedLedger decodes the result.ledger object keeping the order of its keys, which is the
// order of the ledger (newest first) and not recoverable from a Go map.
type orderedLedger []ledgerEntry

func (o *orderedLedger) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil {
		return err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("ledger is not an object")
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return err
		}
		var e ledgerEntry
		if err := dec.Decode(&e); err != nil {
			return err
		}
		e.ID, _ = k.(string)
		*o = append(*o, e)
	}
	_, err := dec.Token()
	return err
}

type ledgerResponse struct {
	Error  []string `json:"error"`
	Result struct {
		Ledger orderedLedger `json:"ledger"`
		Count  json.Number   `json:"count"` // null when Kraken skips the count
	} `json:"result"`
}

// ledgers performs one signed Ledgers call. Like balance(), each attempt (including httpx
// retries) gets a fresh nonce and signature. start, when not 0, asks only for entries newer than
// that unix time (exclusive); because it only cuts the old end of the ledger, offsets from the
// newest entry mean the same with or without it.
func (s *Source) ledgers(ctx context.Context, ofs int, start int64) (*ledgerResponse, error) {
	endpoint := s.baseURL + ledgersPath
	body, err := s.http.Do(ctx, func(ctx context.Context) (*http.Request, error) {
		nonce := s.nextNonce()
		form := url.Values{"nonce": {nonce}, "ofs": {strconv.Itoa(ofs)}}
		if start != 0 {
			form.Set("start", strconv.FormatInt(start, 10))
		}
		postData := form.Encode()
		sig, err := Sign(ledgersPath, nonce, postData, s.secret)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(postData))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("API-Key", s.apiKey)
		req.Header.Set("API-Sign", sig)
		return req, nil
	})

	var resp ledgerResponse
	parseErr := json.Unmarshal(body, &resp)
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) {
			if se.Status == http.StatusTooManyRequests {
				return nil, fmt.Errorf("%w (HTTP 429)", ErrRateLimited)
			}
			if parseErr == nil && len(resp.Error) > 0 {
				return nil, krakenError(resp.Error, se.Status)
			}
		}
		return nil, fmt.Errorf("kraken: %w", err)
	}
	if parseErr != nil {
		return nil, fmt.Errorf("kraken: decode Ledgers response: %w", parseErr)
	}
	// Kraken reports failures in error[] even with HTTP 200.
	if len(resp.Error) > 0 {
		return nil, krakenError(resp.Error, 0)
	}
	return &resp, nil
}

func krakenError(msgs []string, status int) error {
	text := strings.Join(msgs, "; ")
	suffix := ""
	if status != 0 {
		suffix = fmt.Sprintf(" (HTTP %d)", status)
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "throttled") || strings.Contains(lower, "temporary lockout") {
		return fmt.Errorf("%w: %s%s", ErrRateLimited, text, suffix)
	}
	return fmt.Errorf("kraken: %s%s", text, suffix)
}

// ---------------------------------------------------------------------------------------------
// Grouping entries into transactions
// ---------------------------------------------------------------------------------------------

// entry is a ledger entry with its numbers parsed.
type entry struct {
	ledgerEntry
	sym    string
	amt    float64
	fee    decimalSum
	hasFee bool
	t      float64
}

func parseEntry(e ledgerEntry) (entry, error) {
	out := entry{ledgerEntry: e}
	t, err := strconv.ParseFloat(string(e.Time), 64)
	if err != nil || t <= 0 || math.IsNaN(t) || math.IsInf(t, 0) {
		return out, fmt.Errorf("kraken: ledger entry %s: bad time %q", e.ID, e.Time)
	}
	out.t = t
	var amt decimalSum
	if err := amt.add(orZero(string(e.Amount))); err != nil {
		return out, fmt.Errorf("kraken: ledger entry %s amount: %w", e.ID, err)
	}
	out.amt = amt.float()
	if err := out.fee.add(orZero(string(e.Fee))); err != nil {
		return out, fmt.Errorf("kraken: ledger entry %s fee: %w", e.ID, err)
	}
	out.hasFee = !out.fee.isZero()
	out.sym, _ = NormalizeAsset(e.Asset)
	if out.sym == "" {
		return out, fmt.Errorf("kraken: ledger entry %s has no asset", e.ID)
	}
	return out, nil
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

func unixTime(f float64) time.Time {
	sec := math.Floor(f)
	// Kraken's times have four decimals; rounding to microseconds drops float noise.
	us := int64(math.Round((f - sec) * 1e6))
	return time.Unix(int64(sec), us*1000).UTC()
}

// kindRank orders kinds when the entries of one refid disagree: the more specific wins.
var kindRank = map[core.TxKind]int{core.TxOther: 0, core.TxTransfer: 1, core.TxReward: 2, core.TxReceive: 3, core.TxSend: 3, core.TxTrade: 4}

// isAllocation reports whether a staking/earn subtype is a move between Spot and Earn rather than
// income: allocation, deallocation, autoallocation/autoallocate, migration and the older
// spottostaking / stakingfromspot / stakingtospot / spotfromstaking.
func isAllocation(subtype string) bool {
	return strings.Contains(subtype, "allocat") || strings.Contains(subtype, "spot") || subtype == "migration"
}

// entryKind classifies one ledger entry.
func entryKind(e entry) core.TxKind {
	switch e.Type {
	case "trade", "spend", "receive", "conversion":
		return core.TxTrade
	case "deposit":
		return core.TxReceive
	case "withdrawal":
		return core.TxSend
	case "staking", "earn", "dividend", "reward":
		switch {
		case isAllocation(e.Subtype):
			return core.TxTransfer // spot <-> earn moves
		case e.amt > 0:
			return core.TxReward
		}
		return core.TxOther
	case "transfer":
		return core.TxTransfer
	}
	return core.TxOther
}

// priceAgainstUSD sets other.USD from usd when usd is a USD leg moving the opposite way.
func priceAgainstUSD(usd, other *core.Leg) {
	if usd.Symbol == "USD" && other.Symbol != "USD" && usd.Amount*other.Amount < 0 {
		v := -usd.Amount
		other.USD = &v
	}
}

// buildTxs turns committed entries (newest first) into one core.Tx per refid.
func (s *Source) buildTxs(entries []entry) []core.Tx {
	var order []string
	groups := map[string][]entry{}
	for _, e := range entries {
		key := e.RefID
		if key == "" {
			key = e.ID
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], e)
	}
	out := make([]core.Tx, 0, len(order))
	for _, key := range order {
		out = append(out, s.buildTx(key, groups[key]))
	}
	return out
}

func (s *Source) buildTx(refid string, es []entry) core.Tx {
	tx := core.Tx{
		ID:         core.TxID(s.id, refid),
		SourceID:   s.id,
		ExternalID: refid,
		Status:     core.TxConfirmed,
		Time:       unixTime(es[0].t),
		Kind:       core.TxOther,
	}
	best := -1
	var legs []core.Leg
	var feeOrder []string
	fees := map[string]*decimalSum{}
	var detail string
	for _, e := range es {
		if t := unixTime(e.t); t.Before(tx.Time) {
			tx.Time = t
		}
		if k := entryKind(e); kindRank[k] > best {
			best, tx.Kind = kindRank[k], k
			detail = e.Type
			if e.Subtype != "" {
				detail += "/" + e.Subtype
			}
		}
		legs = append(legs, core.Leg{AssetKey: e.sym, Symbol: e.sym, Amount: e.amt})
		if e.hasFee {
			if fees[e.sym] == nil {
				fees[e.sym] = &decimalSum{}
				feeOrder = append(feeOrder, e.sym)
			}
			fees[e.sym].sum.Add(&fees[e.sym].sum, &e.fee.sum)
			fees[e.sym].scale = max(fees[e.sym].scale, e.fee.scale)
		}
	}

	// A zero amount with a fee is just the fee's carrier (the fee is taken in the other asset of a
	// trade); drop such legs unless nothing else is left.
	for _, l := range legs {
		if l.Amount != 0 {
			tx.Legs = append(tx.Legs, l)
		}
	}
	if len(tx.Legs) == 0 {
		tx.Legs = legs[:1]
	}
	// Outgoing legs first (what was spent before what was received), otherwise in ledger order.
	sort.SliceStable(tx.Legs, func(i, j int) bool { return tx.Legs[i].Amount < 0 && tx.Legs[j].Amount >= 0 })
	for i := range tx.Legs {
		if tx.Legs[i].Symbol == "USD" {
			v := tx.Legs[i].Amount
			tx.Legs[i].USD = &v
		}
	}
	// In a trade against USD the other leg is worth what the USD leg moved.
	if tx.Kind == core.TxTrade && len(tx.Legs) == 2 {
		priceAgainstUSD(&tx.Legs[0], &tx.Legs[1])
		priceAgainstUSD(&tx.Legs[1], &tx.Legs[0])
	}

	// One fee leg: the first asset that paid a fee; the rest are mentioned in the note.
	var notes []string
	if len(feeOrder) > 0 {
		first := fees[feeOrder[0]]
		f := core.Leg{AssetKey: feeOrder[0], Symbol: feeOrder[0], Amount: -first.float()}
		if f.Symbol == "USD" {
			v := f.Amount
			f.USD = &v
		}
		tx.Fee = &f
		var extra []string
		for _, sym := range feeOrder[1:] {
			extra = append(extra, fees[sym].String()+" "+sym)
		}
		if len(extra) > 0 {
			notes = append(notes, "other fees not included: "+strings.Join(extra, ", "))
		}
	}
	switch tx.Kind {
	case core.TxOther, core.TxTransfer, core.TxReward:
		notes = append([]string{"kraken " + detail}, notes...)
	}
	tx.Note = strings.Join(notes, " · ")
	return tx
}

// ---------------------------------------------------------------------------------------------
// Walking the ledger
// ---------------------------------------------------------------------------------------------

type walker struct {
	s      *Source
	ctx    context.Context
	budget int
	used   int
	since  float64 // horizon in unix seconds, 0 = none
	start  int64   // value for the start parameter, 0 = none

	older   []ledgerRun // known runs not met yet, newest first
	wasDone bool        // the history below the last known run was already complete
	cur     ledgerRun   // the run being built from the head of the ledger
	pending []entry     // read but not yet part of cur
	seen    map[string]bool
	pos     int    // next offset to request
	skipEnd int    // entries before this position belong to a run already processed
	probe   string // oldest entry of the run just skipped, to spot a shift of the ledger
	done    bool
	txs     []core.Tx

	pages       int   // answers processed in this call
	frontier    bool  // the first request goes behind the first run, not to the top
	headPending bool  // the next answer is the first one of a read from the top
	sweeping    bool  // a read from the top has begun
	dirty       bool  // new entries above the first run were seen, but not read yet
	headCount   int64 // ledger size in the first answer of a read from the top
	base        int64 // ledger size in the first answer of the call, the reference for shift (0 = unknown)
	shift       int   // entries added since base; requests go to position + shift
}

// restart drops all progress of this call and reads from the top, using the runs in st.
func (w *walker) restart(runs []ledgerRun, wasDone bool) {
	w.frontier, w.headPending = false, true
	w.cur = ledgerRun{}
	w.older = append([]ledgerRun(nil), runs...)
	w.wasDone = wasDone
	w.pending = nil
	w.pos, w.skipEnd, w.probe = 0, 0, ""
	w.base, w.shift = 0, 0
}

// commit moves pending entries into cur and turns them into transactions. Unless all is set, the
// trailing refid group stays pending: its other entries may be on the next page.
func (w *walker) commit(all bool) {
	n := len(w.pending)
	if !all && n > 0 {
		ref := w.pending[n-1].RefID
		for n > 0 && w.pending[n-1].RefID == ref && ref != "" {
			n--
		}
	}
	if n == 0 {
		return
	}
	batch := w.pending[:n]
	w.txs = append(w.txs, w.s.buildTxs(batch)...)
	if w.cur.ID == "" {
		w.cur.ID = batch[0].ID
	}
	w.cur.N += n
	w.cur.Last = batch[n-1].ID
	w.pending = w.pending[n:]
}

// page processes one Ledgers response. The request was made at offset reqOff; count is the size of
// the ledger in the answer (0 if unknown). Positions are kept in the frame of the first answer of
// the call: entries that arrived since sit above position 0 and are left for the next call.
func (w *walker) page(reqOff int, count int64, resp *ledgerResponse) error {
	off := reqOff - w.shift // position of the page's first entry in that frame
	raw := resp.Result.Ledger
	entries := make([]entry, len(raw))
	for i, e := range raw {
		pe, err := parseEntry(e)
		if err != nil {
			return err
		}
		entries[i] = pe
	}
	// Kraken's order is newest first; do not trust it blindly (stable, so ties keep wire order).
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].t > entries[j].t })

	// The ledger grew between the request that ended the previous page and this one: the run we
	// jumped over now ends a few entries later than expected. Its last entry gives it away.
	if w.probe != "" {
		for i, e := range entries {
			if e.ID == w.probe {
				w.skipEnd = max(w.skipEnd, off+i+1)
				break
			}
		}
		w.probe = ""
	}

	for i, e := range entries {
		p := off + i
		switch {
		case p < 0, p < w.skipEnd, w.seen[e.ID]:
			continue
		case len(w.older) > 0 && e.ID == w.older[0].ID:
			// Reached a run processed earlier: close the gap above it and jump over it.
			w.commit(true)
			r := w.older[0]
			w.older = w.older[1:]
			if w.cur.ID == "" {
				w.cur.ID = r.ID
			}
			w.cur.N += r.N
			w.cur.Last = r.Last
			w.skipEnd = p + r.N
			w.probe = r.Last
			if len(w.older) == 0 && w.wasDone {
				w.done = true
				return nil
			}
		case w.since > 0 && e.t < w.since:
			w.done = true
			return nil
		default:
			w.pending = append(w.pending, e)
			w.seen[e.ID] = true
		}
	}

	end := off + len(raw)
	if count > 0 && int64(reqOff+len(raw)) >= count {
		w.done = true
	}
	if len(raw) < ledgerPageSize {
		w.done = true
	}
	w.pos = max(end, w.skipEnd)
	if w.skipEnd <= end {
		w.probe = ""
	}
	return nil
}

// Activity returns Kraken ledger entries as core.Tx, one per refid.
//
// Each call makes at most req.Budget Ledgers requests (default 10), newest entries first: the
// entries added since the last call, then older history down to req.Since. State carries the
// cursors, so repeated calls cover the whole ledger without returning an entry twice, also when
// new entries arrive in between.
//
// A request that fails, including a Kraken rate limit (ErrRateLimited), ends the call. If pages
// were read before it, the call ends cleanly (no error, BackfillDone false) with a State that
// matches them; if the failing request was the first, the error is returned (ErrRateLimited for a
// throttled key). Nothing is retried here beyond httpx's backoff, so a throttled key is left alone
// until the next call. Each request costs 2 points of Kraken's private-call counter (limit 15
// on the starter tier, decaying about 0.33 per second), so budgets above about 6 will be
// throttled when the counter starts out full.
func (s *Source) Activity(ctx context.Context, req core.ActivityRequest) (core.ActivityResult, error) {
	budget := req.Budget
	if budget <= 0 {
		budget = defaultActivityBudget
	}
	budget = min(budget, maxActivityBudget)
	st := loadState(req.State)

	var sinceU int64
	var since float64
	var start int64
	if !req.Since.IsZero() {
		sinceU = req.Since.Unix()
		since = float64(req.Since.UnixNano()) / 1e9
		// start is exclusive and in whole seconds. It is rounded down to the start of the UTC day
		// so that a horizon that moves with the clock (now - 90 days) changes it once a day, not
		// on every call: the size of the ledger as filtered by start is what tells new entries
		// apart. The exact cut is made on the entries below.
		start = sinceU - sinceU%86400 - 1
	}
	// An earlier horizon than the one Done refers to reopens the backfill.
	if st.Done && st.Since != 0 && (sinceU == 0 || sinceU < st.Since) {
		st.Done = false
	}

	w := &walker{
		s: s, ctx: ctx, budget: budget, since: since, start: start,
		older:   append([]ledgerRun(nil), st.Runs...),
		wasDone: st.Done,
		seen:    map[string]bool{},
	}
	if !st.Sweep && st.Count > 0 && len(st.Runs) > 0 && st.Start == start {
		r := st.Runs[0]
		w.cur, w.older = r, append([]ledgerRun(nil), st.Runs[1:]...)
		w.pos, w.skipEnd, w.probe = r.N, r.N, r.Last
		w.frontier = true
	} else {
		w.headPending = true
	}
	var walkErr error
	for !w.done && w.used < w.budget {
		w.used++
		reqOff := w.pos + w.shift
		resp, err := s.ledgers(ctx, reqOff, w.start)
		if err != nil {
			walkErr = err
			break
		}
		count, countErr := resp.Result.Count.Int64()
		if countErr != nil {
			count = 0
		}
		if w.frontier {
			w.frontier = false
			if count == 0 || count != st.Count {
				// Something new above the run, or no way to tell: this page may be shifted. Drop it
				// and read from the top.
				w.dirty = true
				w.restart(st.Runs, st.Done)
				continue
			}
			w.base = count
		}
		if w.headPending {
			w.headPending, w.sweeping = false, true
			w.headCount, w.base, w.shift = count, count, 0
		}
		if w.base > 0 && count > 0 {
			w.shift = max(0, int(count-w.base))
		}
		if err := w.page(reqOff, count, resp); err != nil {
			walkErr = err
			break
		}
		w.pages++
	}
	w.commit(w.done) // after a cut-off, the trailing group is read again next time

	// Done also stays set while only a gap of new entries above the oldest run is unread: the
	// history below that run is complete.
	next := activityState{Done: w.done || (w.wasDone && len(w.older) > 0), Since: sinceU, Count: st.Count, Start: st.Start}
	if w.sweeping {
		next.Count, next.Start = w.headCount, start
		if w.cur.N == 0 {
			next.Count = 0 // the head was not committed, the size says nothing about the first run
		}
	}
	next.Sweep = (st.Sweep || w.dirty) && !w.sweeping
	if w.cur.N > 0 {
		next.Runs = append(next.Runs, w.cur)
	}
	if !w.done {
		next.Runs = append(next.Runs, w.older...)
	}
	state, err := json.Marshal(next)
	if err != nil {
		return core.ActivityResult{}, err
	}
	sort.SliceStable(w.txs, func(i, j int) bool { return w.txs[i].Time.After(w.txs[j].Time) })
	res := core.ActivityResult{Txs: w.txs, State: state, BackfillDone: w.done}
	if walkErr != nil && w.pages > 0 && ctx.Err() == nil {
		// A failure after progress ends the call cleanly: callers that drop a result along with
		// its error would otherwise lose the pages already read, and a problem that persists
		// shows up as an error the next time, when it is the first request to fail.
		walkErr = nil
	}
	return res, walkErr
}
