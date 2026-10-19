package kraken

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

// ---------------------------------------------------------------------------------------------
// A fake Ledgers endpoint
// ---------------------------------------------------------------------------------------------

type fakeEntry struct {
	ID, RefID, Type, Subtype, Asset, Amount, Fee string
	T                                            float64
}

type ledgerReq struct {
	ofs   int
	start int64
	hasSt bool
	body  string
}

// ledgerFake serves POST /0/private/Ledgers from an in-memory ledger (newest first), checking the
// signature like Kraken would: it must equal Sign(path, nonce, body) and the nonce must grow.
type ledgerFake struct {
	*httptest.Server
	t         *testing.T
	mu        sync.Mutex
	entries   []fakeEntry
	reqs      []ledgerReq
	lastNonce uint64
	ignoreSt  bool // behave as if `start` was not supported
	noCount   bool // send "count": null
	// respond, when set, may replace the answer to request number n (1-based): return handled=true.
	respond func(n int, w http.ResponseWriter) bool
}

func newLedgerFake(t *testing.T) *ledgerFake {
	f := &ledgerFake{t: t}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// add puts entries (newest first) above everything already in the ledger.
func (f *ledgerFake) add(es ...fakeEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(append([]fakeEntry(nil), es...), f.entries...)
}

func (f *ledgerFake) requests() []ledgerReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ledgerReq(nil), f.reqs...)
}

func (f *ledgerFake) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	f.mu.Lock()
	defer f.mu.Unlock()

	fail := func(format string, args ...any) {
		f.t.Errorf("Ledgers: "+format, args...)
		fmt.Fprint(w, `{"error":["EAPI:Invalid signature"]}`)
	}
	if r.Method != http.MethodPost || r.URL.Path != ledgersPath {
		fail("%s %s", r.Method, r.URL.Path)
		return
	}
	form, err := url.ParseQuery(body)
	if err != nil {
		fail("bad body %q", body)
		return
	}
	nonce, err := strconv.ParseUint(form.Get("nonce"), 10, 64)
	if err != nil || nonce <= f.lastNonce {
		fail("nonce %q after %d", form.Get("nonce"), f.lastNonce)
		return
	}
	f.lastNonce = nonce
	if want, _ := Sign(ledgersPath, form.Get("nonce"), body, docSecret); r.Header.Get("API-Sign") != want {
		fail("bad signature for %q", body)
		return
	}
	ofs, err := strconv.Atoi(form.Get("ofs"))
	if err != nil || ofs < 0 {
		fail("bad ofs %q", form.Get("ofs"))
		return
	}
	rec := ledgerReq{ofs: ofs, body: body}
	if v := form.Get("start"); v != "" {
		rec.start, _ = strconv.ParseInt(v, 10, 64)
		rec.hasSt = true
	}
	f.reqs = append(f.reqs, rec)

	if f.respond != nil && f.respond(len(f.reqs), w) {
		return
	}

	list := f.entries
	if rec.hasSt && !f.ignoreSt {
		var kept []fakeEntry
		for _, e := range list {
			if e.T > float64(rec.start) {
				kept = append(kept, e)
			}
		}
		list = kept
	}
	page := list[min(ofs, len(list)):min(len(list), ofs+50)]
	var sb strings.Builder
	sb.WriteString(`{"error":[],"result":{"ledger":{`)
	for i, e := range page {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `%q:{"refid":%q,"time":%.4f,"type":%q,"subtype":%q,"aclass":"currency","asset":%q,"amount":%q,"fee":%q,"balance":"0.0"}`,
			e.ID, e.RefID, e.T, e.Type, e.Subtype, e.Asset, e.Amount, e.Fee)
	}
	if f.noCount {
		sb.WriteString(`},"count":null}}`)
	} else {
		fmt.Fprintf(&sb, `},"count":%d}}`, len(list))
	}
	fmt.Fprint(w, sb.String())
}

func newActivitySource(t *testing.T, f *ledgerFake) *Source {
	t.Helper()
	return newTestSource(t, uniqueKey("activity"), f.URL)
}

const baseT = 1_750_000_000.0

// deposits builds n single-entry deposits named prefix-0001.., newest first, a minute apart.
func deposits(prefix string, n int, newest float64) []fakeEntry {
	out := make([]fakeEntry, n)
	for i := range out {
		out[i] = fakeEntry{
			ID: fmt.Sprintf("L-%s-%04d", prefix, i+1), RefID: fmt.Sprintf("R-%s-%04d", prefix, i+1),
			Type: "deposit", Asset: "XXBT", Amount: "0.01", Fee: "0", T: newest - float64(i)*60,
		}
	}
	return out
}

func refs(txs []core.Tx) []string {
	out := make([]string, len(txs))
	for i, tx := range txs {
		out[i] = tx.ExternalID
	}
	return out
}

// run calls Activity with the same budget until BackfillDone, failing on a repeated Tx.ID.
func run(t *testing.T, s *Source, since time.Time, budget, maxCalls int) (all []core.Tx, state []byte, calls int) {
	t.Helper()
	seen := map[string]bool{}
	for calls < maxCalls {
		calls++
		res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Since: since, Budget: budget})
		if err != nil {
			t.Fatalf("call %d: %v", calls, err)
		}
		for _, tx := range res.Txs {
			if seen[tx.ID] {
				t.Fatalf("call %d returned %s again", calls, tx.ID)
			}
			seen[tx.ID] = true
			all = append(all, tx)
		}
		state = res.State
		if res.BackfillDone {
			return all, state, calls
		}
	}
	t.Fatalf("not done after %d calls", maxCalls)
	return nil, nil, 0
}

func fp(f float64) *float64 { return &f }

// ---------------------------------------------------------------------------------------------
// Request format
// ---------------------------------------------------------------------------------------------

func TestSourceImplementsActivitySource(t *testing.T) {
	var _ core.ActivitySource = (*Source)(nil)
}

func TestActivityRequestIsSignedAndPaged(t *testing.T) {
	f := newLedgerFake(t) // the fake verifies signature, nonce and form
	f.add(deposits("a", 120, baseT)...)
	s := newActivitySource(t, f)

	if _, err := s.Activity(context.Background(), core.ActivityRequest{}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests for 120 entries, want 3 (50+50+20)", len(reqs))
	}
	for i, want := range []int{0, 50, 100} {
		if reqs[i].ofs != want || reqs[i].hasSt {
			t.Errorf("request %d = %+v, want ofs=%d and no start", i, reqs[i], want)
		}
	}
	form, _ := url.ParseQuery(reqs[0].body)
	if len(form) != 2 {
		t.Errorf("body %q should hold exactly nonce and ofs", reqs[0].body)
	}
}

func TestActivitySendsStartForSince(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 3, baseT)...)
	s := newActivitySource(t, f)
	since := time.Unix(int64(baseT)-3600, 0)
	if _, err := s.Activity(context.Background(), core.ActivityRequest{Since: since}); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	dayStart := since.Unix() - since.Unix()%86400
	if len(reqs) != 1 || !reqs[0].hasSt || reqs[0].start != dayStart-1 {
		t.Errorf("requests = %+v, want start = %d (the UTC day of Since, exclusive)", reqs, dayStart-1)
	}
}

// ---------------------------------------------------------------------------------------------
// Grouping and kinds
// ---------------------------------------------------------------------------------------------

func TestActivityGroupsByRefidAndClassifies(t *testing.T) {
	f := newLedgerFake(t)
	f.add(
		// A buy: spend USD (with fee) + receive BTC.
		fakeEntry{"L1a", "TRADE1", "spend", "", "ZUSD", "-100.0000", "0.2600", baseT - 10},
		fakeEntry{"L1b", "TRADE1", "receive", "", "XXBT", "0.0020000000", "0.0000000000", baseT - 10},
		// A convert shown as two trade legs, fee in the quote asset on the EUR leg.
		fakeEntry{"L2a", "TRADE2", "trade", "", "XETH", "1.0000000000", "0.0000000000", baseT - 100},
		fakeEntry{"L2b", "TRADE2", "trade", "", "ZEUR", "-2000.0000", "5.0000", baseT - 99},
		// A deposit, a withdrawal with a fee.
		fakeEntry{"L3", "DEP1", "deposit", "", "XXBT", "0.5000000000", "0.0000000000", baseT - 200},
		fakeEntry{"L4", "WD1", "withdrawal", "", "XXBT", "-0.1000000000", "0.0005000000", baseT - 300},
		// Rewards: on-chain staking, an earn reward and a dividend.
		fakeEntry{"L5", "STK1", "staking", "", "DOT.S", "0.0500000000", "0.0000000000", baseT - 400},
		fakeEntry{"L6", "EARN1", "earn", "reward", "USDC.F", "0.1200", "0.0000", baseT - 500},
		fakeEntry{"L7", "DIV1", "dividend", "", "AAPLx", "0.0100000000", "0.0000000000", baseT - 600},
		// Earn allocation: spot -> earn, both sides under one refid; and the way back.
		fakeEntry{"L8a", "ALLOC1", "earn", "allocation", "USDC", "-100.0000", "0.0000", baseT - 700},
		fakeEntry{"L8b", "ALLOC1", "earn", "allocation", "USDC.F", "100.0000", "0.0000", baseT - 700},
		fakeEntry{"L9a", "DEALLOC1", "earn", "deallocation", "USDC.F", "-100.0000", "0.0000", baseT - 800},
		fakeEntry{"L9b", "DEALLOC1", "earn", "deallocation", "USDC", "100.0000", "0.0000", baseT - 800},
		// Automatic allocation and the credited side of an older staking move, filed under earn.
		fakeEntry{"L17", "AUTO1", "earn", "autoallocation", "DOT.S", "2.0000000000", "0", baseT - 850},
		fakeEntry{"L18", "AUTO2", "earn", "stakingfromspot", "SOL.S", "3.0000000000", "0", baseT - 860},
		// Legacy staking moves and a plain transfer.
		fakeEntry{"L10a", "XFER1", "transfer", "spottostaking", "DOT", "-5.0000000000", "0", baseT - 900},
		fakeEntry{"L10b", "XFER1", "transfer", "spottostaking", "DOT.S", "5.0000000000", "0", baseT - 900},
		fakeEntry{"L11", "XFER2", "transfer", "", "XXBT", "0.0100000000", "0", baseT - 1000},
		// Unknown types and a negative staking-type entry stay "other".
		fakeEntry{"L12", "MARGIN1", "margin", "", "ZUSD", "-1.5000", "0.0100", baseT - 1100},
		fakeEntry{"L13", "ADJ1", "adjustment", "", "XXBT", "0.0001000000", "0", baseT - 1200},
		fakeEntry{"L14", "STK2", "staking", "", "ETH2.S", "-0.0010000000", "0", baseT - 1300},
		// The fee of a trade carried by a zero-amount leg.
		fakeEntry{"L15a", "TRADE3", "trade", "", "XXBT", "0.0010000000", "0", baseT - 1400},
		fakeEntry{"L15b", "TRADE3", "trade", "", "ZUSD", "-60.0000", "0", baseT - 1400},
		fakeEntry{"L15c", "TRADE3", "trade", "", "ZUSD", "0.0000", "0.3000", baseT - 1400},
		// Fees in two assets: the first one is the Fee leg, the other is mentioned.
		fakeEntry{"L16a", "TRADE4", "trade", "", "XXBT", "0.0100000000", "0.0000100000", baseT - 1500},
		fakeEntry{"L16b", "TRADE4", "trade", "", "ZUSD", "-600.0000", "1.0000", baseT - 1500},
	)
	s := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || !res.BackfillDone {
		t.Fatalf("done=%v err=%v", res.BackfillDone, err)
	}
	by := map[string]core.Tx{}
	for _, tx := range res.Txs {
		by[tx.ExternalID] = tx
	}
	if len(by) != 18 || len(res.Txs) != 18 {
		t.Fatalf("%d txs (%d distinct), want 18: %v", len(res.Txs), len(by), refs(res.Txs))
	}
	legs := func(tx core.Tx) string {
		var parts []string
		for _, l := range tx.Legs {
			parts = append(parts, fmt.Sprintf("%s %v", l.Symbol, l.Amount))
		}
		return strings.Join(parts, ", ")
	}
	for _, tc := range []struct {
		ref  string
		kind core.TxKind
		legs string
	}{
		{"TRADE1", core.TxTrade, "USD -100, BTC 0.002"},
		{"TRADE2", core.TxTrade, "EUR -2000, ETH 1"},
		{"DEP1", core.TxReceive, "BTC 0.5"},
		{"WD1", core.TxSend, "BTC -0.1"},
		{"STK1", core.TxReward, "DOT 0.05"},
		{"EARN1", core.TxReward, "USDC 0.12"},
		{"DIV1", core.TxReward, "AAPLx 0.01"},
		{"ALLOC1", core.TxTransfer, "USDC -100, USDC 100"},
		{"DEALLOC1", core.TxTransfer, "USDC -100, USDC 100"},
		{"AUTO1", core.TxTransfer, "DOT 2"},
		{"AUTO2", core.TxTransfer, "SOL 3"},
		{"XFER1", core.TxTransfer, "DOT -5, DOT 5"},
		{"XFER2", core.TxTransfer, "BTC 0.01"},
		{"MARGIN1", core.TxOther, "USD -1.5"},
		{"ADJ1", core.TxOther, "BTC 0.0001"},
		{"STK2", core.TxOther, "ETH -0.001"},
		{"TRADE3", core.TxTrade, "USD -60, BTC 0.001"},
	} {
		tx, ok := by[tc.ref]
		if !ok {
			t.Errorf("%s missing", tc.ref)
			continue
		}
		if tx.Kind != tc.kind || legs(tx) != tc.legs {
			t.Errorf("%s: kind=%s legs=[%s], want %s [%s]", tc.ref, tx.Kind, legs(tx), tc.kind, tc.legs)
		}
		if tx.Status != core.TxConfirmed || tx.ID != "kraken:"+tc.ref || tx.SourceID != "kraken" || tx.TxHash != "" || tx.ExplorerURL != "" {
			t.Errorf("%s: %+v", tc.ref, tx)
		}
	}

	// Trade: the fee is a negative USD leg; the BTC leg is worth what the USD leg moved.
	tr := by["TRADE1"]
	if tr.Fee == nil || tr.Fee.Symbol != "USD" || tr.Fee.Amount != -0.26 || tr.Fee.USD == nil || *tr.Fee.USD != -0.26 {
		t.Errorf("TRADE1 fee = %+v", tr.Fee)
	}
	if tr.Legs[0].USD == nil || *tr.Legs[0].USD != -100 || tr.Legs[1].USD == nil || *tr.Legs[1].USD != 100 {
		t.Errorf("TRADE1 USD values = %v %v", tr.Legs[0].USD, tr.Legs[1].USD)
	}
	if want := time.Unix(int64(baseT)-10, 0).UTC(); !tr.Time.Equal(want) || tr.Time.Location() != time.UTC {
		t.Errorf("TRADE1 time = %v, want %v", tr.Time, want)
	}
	// Time is the earliest entry of the group (the legs of TRADE2 are a second apart).
	if want := time.Unix(int64(baseT)-100, 0).UTC(); !by["TRADE2"].Time.Equal(want) {
		t.Errorf("TRADE2 time = %v, want %v", by["TRADE2"].Time, want)
	}
	if fee := by["TRADE2"].Fee; fee == nil || fee.Symbol != "EUR" || fee.Amount != -5 || fee.USD != nil {
		t.Errorf("TRADE2 fee = %+v", fee)
	}
	if by["TRADE2"].Legs[0].USD != nil || by["TRADE2"].Legs[1].USD != nil {
		t.Errorf("a EUR trade has no USD values: %+v", by["TRADE2"].Legs)
	}
	if fee := by["WD1"].Fee; fee == nil || fee.Symbol != "BTC" || fee.Amount != -0.0005 || fee.AssetKey != "BTC" {
		t.Errorf("WD1 fee = %+v", fee)
	}
	if by["DEP1"].Fee != nil || by["STK1"].Fee != nil {
		t.Error("zero fees must leave Fee nil")
	}
	// Zero-amount fee carrier: the leg is dropped, the fee kept.
	if fee := by["TRADE3"].Fee; fee == nil || fee.Symbol != "USD" || fee.Amount != -0.3 || len(by["TRADE3"].Legs) != 2 {
		t.Errorf("TRADE3 fee=%+v legs=%+v", by["TRADE3"].Fee, by["TRADE3"].Legs)
	}
	// Fees in two assets: first one wins, the note says what was left out.
	t4 := by["TRADE4"]
	if t4.Fee == nil || t4.Fee.Symbol != "BTC" || t4.Fee.Amount != -0.00001 || !strings.Contains(t4.Note, "1 USD") {
		t.Errorf("TRADE4 fee=%+v note=%q", t4.Fee, t4.Note)
	}
	// Notes say where reward/transfer/other come from.
	for ref, want := range map[string]string{"STK1": "kraken staking", "EARN1": "kraken earn/reward", "ALLOC1": "kraken earn/allocation",
		"XFER1": "kraken transfer/spottostaking", "MARGIN1": "kraken margin", "STK2": "kraken staking"} {
		if !strings.Contains(by[ref].Note, want) {
			t.Errorf("%s note = %q, want it to contain %q", ref, by[ref].Note, want)
		}
	}
	for i := 1; i < len(res.Txs); i++ {
		if res.Txs[i].Time.After(res.Txs[i-1].Time) {
			t.Fatal("not sorted newest first")
		}
	}
}

func TestActivityNormalizesAssets(t *testing.T) {
	f := newLedgerFake(t)
	f.add(
		fakeEntry{"L1", "R1", "deposit", "", "XXBT", "1", "0", baseT},
		fakeEntry{"L2", "R2", "staking", "", "DOT.S", "0.1", "0", baseT - 1},
		fakeEntry{"L3", "R3", "staking", "", "ETH2.S", "0.1", "0", baseT - 2},
		fakeEntry{"L4", "R4", "staking", "", "DOT28.S", "0.1", "0", baseT - 3},
		fakeEntry{"L5", "R5", "deposit", "", "XETH", "1", "0", baseT - 4},
		fakeEntry{"L6", "R6", "deposit", "", "ZUSD", "1", "0", baseT - 5},
		fakeEntry{"L7", "R7", "deposit", "", "XXDG", "1", "0", baseT - 6},
		fakeEntry{"L8", "R8", "deposit", "", "SOL", "1", "0", baseT - 7},
	)
	s := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tx := range res.Txs {
		if tx.Legs[0].AssetKey != tx.Legs[0].Symbol {
			t.Errorf("AssetKey %q != Symbol %q", tx.Legs[0].AssetKey, tx.Legs[0].Symbol)
		}
		got = append(got, tx.Legs[0].Symbol)
	}
	if want := []string{"BTC", "DOT", "ETH", "DOT", "ETH", "USD", "DOGE", "SOL"}; !slices.Equal(got, want) {
		t.Errorf("symbols = %v, want %v", got, want)
	}
}

func TestOrderedLedgerKeepsWireOrder(t *testing.T) {
	var o orderedLedger
	in := `{"Z-last-id":{"refid":"r1","time":3.5},"A-first-id":{"refid":"r2","time":2},"M":{"refid":"r3","time":"1"}}`
	if err := o.UnmarshalJSON([]byte(in)); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range o {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []string{"Z-last-id", "A-first-id", "M"}) {
		t.Errorf("order = %v", ids)
	}
	if o[2].Time != "1" || o[0].RefID != "r1" {
		t.Errorf("entries = %+v", o)
	}
	if err := (&orderedLedger{}).UnmarshalJSON([]byte(`[1]`)); err == nil {
		t.Error("an array is not a ledger")
	}
}

// ---------------------------------------------------------------------------------------------
// Paging, budget and resuming
// ---------------------------------------------------------------------------------------------

func TestActivityBackfillAndIdleCalls(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 120, baseT)...)
	s := newActivitySource(t, f)
	all, state, calls := run(t, s, time.Time{}, 0, 5)
	if len(all) != 120 || calls != 1 {
		t.Fatalf("%d txs in %d calls, want 120 in 1 (default budget covers it)", len(all), calls)
	}

	// Idle: one request (to see the newest entry), nothing returned, still done.
	before := len(f.requests())
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: state})
	if err != nil || len(res.Txs) != 0 || !res.BackfillDone {
		t.Fatalf("idle: %+v %v", res, err)
	}
	if n := len(f.requests()) - before; n != 1 {
		t.Errorf("idle call made %d requests, want 1", n)
	}

	// Three new entries: the page behind the run shows the ledger grew (and is dropped), then one
	// read from the top; only the new ones come back.
	f.add(deposits("n", 3, baseT+1000)...)
	before = len(f.requests())
	res, err = s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil || !res.BackfillDone {
		t.Fatalf("%v %v", res.BackfillDone, err)
	}
	if got := refs(res.Txs); !slices.Equal(got, []string{"R-n-0001", "R-n-0002", "R-n-0003"}) {
		t.Errorf("new = %v", got)
	}
	if n := len(f.requests()) - before; n != 2 {
		t.Errorf("new-activity call made %d requests, want 2", n)
	}
}

func TestActivityBudgetIsCappedForKrakenRateLimits(t *testing.T) {
	for _, budget := range []int{0, 30} { // default, and a syncer asking for more than Kraken tolerates
		f := newLedgerFake(t)
		f.add(deposits("a", 800, baseT)...) // 16 pages
		s := newActivitySource(t, f)
		res, err := s.Activity(context.Background(), core.ActivityRequest{Budget: budget})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(f.requests()); n != 6 {
			t.Errorf("budget %d: %d requests, want 6 (Ledgers costs 2 of a 15-point limit)", budget, n)
		}
		// 6 pages = 300 entries; the last one is held back because its refid group might go on.
		if res.BackfillDone || len(res.Txs) != 299 {
			t.Errorf("done=%v txs=%d, want not done with 299", res.BackfillDone, len(res.Txs))
		}
	}
}

func TestActivityBudgetResumeWithNewEntriesInBetween(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 230, baseT)...)
	s := newActivitySource(t, f)

	var state []byte
	seen := map[string]bool{}
	for call := 1; call <= 40; call++ {
		before := len(f.requests())
		res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 2})
		if err != nil {
			t.Fatal(err)
		}
		if used := len(f.requests()) - before; used > 2 {
			t.Fatalf("call %d made %d requests with a budget of 2", call, used)
		}
		for _, tx := range res.Txs {
			if seen[tx.ExternalID] {
				t.Fatalf("call %d returned %s again", call, tx.ExternalID)
			}
			seen[tx.ExternalID] = true
		}
		state = res.State
		switch call {
		case 1: // a few entries arrive while the backfill is half way
			f.add(deposits("b", 7, baseT+10_000)...)
		case 2: // and a lot more than a call can read: the new activity itself needs several calls
			f.add(deposits("c", 130, baseT+20_000)...)
		}
		if res.BackfillDone {
			if want := 230 + 7 + 130; len(seen) != want {
				t.Fatalf("done with %d entries, want %d", len(seen), want)
			}
			// Everything is accounted for and a further call is a plain no-op.
			res, err = s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 2})
			if err != nil || len(res.Txs) != 0 || !res.BackfillDone {
				t.Fatalf("after done: %+v %v", res, err)
			}
			return
		}
	}
	t.Fatalf("never done; saw %d entries", len(seen))
}

// New entries that appear while one call is paging must not be returned twice or shift the
// backfill (the ledger moves down under the offsets).
func TestActivityLedgerGrowsDuringACall(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 200, baseT)...)
	s := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{Budget: 2}) // 100 entries
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tx := range res.Txs {
		seen[tx.ExternalID] = true
	}

	// Second call: 5 entries arrive after the first request of the call (the lock is held by serve).
	grown := false
	f.mu.Lock()
	f.respond = func(n int, w http.ResponseWriter) bool {
		if n == 4 && !grown {
			grown = true
			f.entries = append(deposits("g", 5, baseT+5000), f.entries...)
		}
		return false
	}
	f.mu.Unlock()
	state := res.State
	for call := 0; call < 10; call++ {
		res, err = s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, tx := range res.Txs {
			if seen[tx.ExternalID] {
				t.Fatalf("%s returned twice", tx.ExternalID)
			}
			seen[tx.ExternalID] = true
		}
		state = res.State
		if res.BackfillDone {
			break
		}
	}
	// The five late entries are picked up by the following call.
	res, err = s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range res.Txs {
		if seen[tx.ExternalID] {
			t.Fatalf("%s returned twice", tx.ExternalID)
		}
		seen[tx.ExternalID] = true
	}
	if len(seen) != 205 {
		t.Errorf("saw %d entries, want 205", len(seen))
	}
}

// A trade whose two legs fall on different pages must come out once, with both legs.
func TestActivityGroupAcrossPageBoundary(t *testing.T) {
	f := newLedgerFake(t)
	es := deposits("a", 100, baseT)
	// Entries 49 and 50 (the last of page one and the first of page two) are the legs of one trade.
	es[49] = fakeEntry{"L-leg-1", "R-trade", "trade", "", "XXBT", "0.5", "0", es[49].T}
	es[50] = fakeEntry{"L-leg-2", "R-trade", "trade", "", "ZUSD", "-30000", "10", es[49].T}
	f.add(es...)
	s := newActivitySource(t, f)

	res, err := s.Activity(context.Background(), core.ActivityRequest{Budget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Txs) != 49 {
		t.Fatalf("first call returned %d txs, want 49 (the trailing group is held back)", len(res.Txs))
	}
	for _, tx := range res.Txs {
		if tx.ExternalID == "R-trade" {
			t.Fatal("half a trade was returned")
		}
	}
	var trade *core.Tx
	rest := 0
	state := res.State
	for call := 0; call < 5; call++ {
		res, err = s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 1})
		if err != nil {
			t.Fatal(err)
		}
		for i, tx := range res.Txs {
			rest++
			if tx.ExternalID == "R-trade" {
				trade = &res.Txs[i]
			}
		}
		state = res.State
		if res.BackfillDone {
			break
		}
	}
	if !res.BackfillDone {
		t.Fatal("not done")
	}
	if trade == nil || len(trade.Legs) != 2 || trade.Kind != core.TxTrade || trade.Fee == nil {
		t.Fatalf("trade = %+v", trade)
	}
	if rest != 50 { // the trade (two entries) and the 49 entries after it
		t.Errorf("later calls returned %d txs, want 50", rest)
	}
}

func TestActivitySinceCutoff(t *testing.T) {
	for _, supportsStart := range []bool{true, false} {
		t.Run(fmt.Sprintf("server honours start=%v", supportsStart), func(t *testing.T) {
			f := newLedgerFake(t)
			f.ignoreSt = !supportsStart
			f.add(deposits("a", 160, baseT)...) // entry i is i minutes old
			s := newActivitySource(t, f)

			since := time.Unix(int64(baseT)-99*60-30, 0) // keeps the first 100 entries
			res, err := s.Activity(context.Background(), core.ActivityRequest{Since: since, Budget: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Txs) != 100 || !res.BackfillDone {
				t.Fatalf("txs=%d done=%v, want 100 and done", len(res.Txs), res.BackfillDone)
			}
			for _, tx := range res.Txs {
				if tx.Time.Before(since) {
					t.Errorf("%s at %v is older than Since", tx.ID, tx.Time)
				}
			}
			// Page 3 shows the horizon was crossed (start only cuts at the day).
			if n := len(f.requests()); n != 3 {
				t.Errorf("%d requests, want 3", n)
			}

			// Same horizon again: one request, nothing new.
			before := len(f.requests())
			res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State, Since: since})
			if err != nil || len(res2.Txs) != 0 || !res2.BackfillDone || len(f.requests())-before != 1 {
				t.Fatalf("idle call: %+v %v requests=%d", res2, err, len(f.requests())-before)
			}

			// A later horizon changes nothing; an earlier one continues where the backfill stopped.
			res3, err := s.Activity(context.Background(), core.ActivityRequest{State: res2.State, Since: time.Time{}, Budget: 10})
			if err != nil || !res3.BackfillDone {
				t.Fatalf("extend: %v %v", res3.BackfillDone, err)
			}
			if len(res3.Txs) != 60 {
				t.Fatalf("extension returned %d txs, want the 60 older ones", len(res3.Txs))
			}
			got := map[string]bool{}
			for _, tx := range res3.Txs {
				got[tx.ExternalID] = true
			}
			for i := 101; i <= 160; i++ {
				if id := fmt.Sprintf("R-a-%04d", i); !got[id] {
					t.Fatalf("%s missing after extending Since", id)
				}
			}
		})
	}
}

// The syncer passes now-90d, a horizon that moves on every call. That must not make every call
// start over: the call is one request while nothing changes, also across a day boundary.
func TestActivityMovingHorizon(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 120, baseT)...)
	s := newActivitySource(t, f)
	since := time.Unix(int64(baseT)-3*86400, 0)
	res, err := s.Activity(context.Background(), core.ActivityRequest{Since: since})
	if err != nil || !res.BackfillDone || len(res.Txs) != 120 {
		t.Fatalf("%+v %v", res, err)
	}
	state := res.State
	var starts []int64
	for _, step := range []time.Duration{time.Minute, time.Hour, 30 * time.Hour, 30*time.Hour + time.Minute} {
		before := len(f.requests())
		res, err = s.Activity(context.Background(), core.ActivityRequest{State: state, Since: since.Add(step)})
		if err != nil || len(res.Txs) != 0 || !res.BackfillDone {
			t.Fatalf("idle +%v: %+v %v", step, res, err)
		}
		reqs := f.requests()[before:]
		if len(reqs) != 1 {
			t.Fatalf("idle +%v made %d requests, want 1", step, len(reqs))
		}
		starts = append(starts, reqs[0].start)
		state = res.State
	}
	if starts[0] != starts[1] || starts[2] == starts[1] {
		t.Errorf("start parameters %v: want the same within a UTC day and a new one the next day", starts)
	}

	// New entries are still found afterwards, and only they are returned.
	f.add(deposits("n", 2, baseT+1000)...)
	res, err = s.Activity(context.Background(), core.ActivityRequest{State: state, Since: since.Add(31 * time.Hour)})
	if err != nil || len(res.Txs) != 2 || !res.BackfillDone {
		t.Fatalf("new entries: %+v %v", res, err)
	}
}

func TestActivityEmptyLedger(t *testing.T) {
	f := newLedgerFake(t)
	s := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || len(res.Txs) != 0 || !res.BackfillDone || res.State == nil {
		t.Fatalf("%+v %v", res, err)
	}
	f.add(deposits("a", 2, baseT)...)
	res, err = s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil || len(res.Txs) != 2 || !res.BackfillDone {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestActivityWithoutCountField(t *testing.T) {
	f := newLedgerFake(t)
	f.noCount = true
	f.add(deposits("a", 100, baseT)...) // exactly two full pages: the end shows as an empty page
	s := newActivitySource(t, f)
	all, _, _ := run(t, s, time.Time{}, 0, 3)
	if len(all) != 100 {
		t.Fatalf("got %d", len(all))
	}
	if n := len(f.requests()); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}
}

func TestActivityBadStateStartsOver(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 3, baseT)...)
	s := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: []byte("garbage")})
	if err != nil || len(res.Txs) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
}

// A state whose anchor is no longer in the ledger (wrong account, deleted data) must heal itself by
// reading the whole ledger instead of looping.
func TestActivityStateAnchorMissing(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 70, baseT)...)
	s := newActivitySource(t, f)
	state := []byte(`{"runs":[{"id":"L-gone","last":"L-gone-2","n":30}],"done":true}`)
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: state})
	if err != nil || !res.BackfillDone || len(res.Txs) != 70 {
		t.Fatalf("txs=%d done=%v err=%v", len(res.Txs), res.BackfillDone, err)
	}
}

// ---------------------------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------------------------

func TestActivityErrorArrayIsAnError(t *testing.T) {
	f := newLedgerFake(t)
	f.respond = func(n int, w http.ResponseWriter) bool {
		fmt.Fprint(w, `{"error":["EGeneral:Permission denied"]}`)
		return true
	}
	s := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: nil})
	if err == nil || !strings.Contains(err.Error(), "EGeneral:Permission denied") || errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v", err)
	}
	if res.BackfillDone || len(res.Txs) != 0 {
		t.Errorf("result = %+v", res)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests after a hard error, want 1", n)
	}
}

func TestActivityHTTPErrorWithErrorBody(t *testing.T) {
	f := newLedgerFake(t)
	f.respond = func(n int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":["EAPI:Invalid key"]}`)
		return true
	}
	s := newActivitySource(t, f)
	_, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err == nil || !strings.Contains(err.Error(), "EAPI:Invalid key") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}

func TestActivityRateLimitStopsCleanlyAndResumes(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 400, baseT)...)
	limited := true
	f.respond = func(n int, w http.ResponseWriter) bool {
		if limited && n == 4 {
			fmt.Fprint(w, `{"error":["EAPI:Rate limit exceeded"]}`)
			return true
		}
		return false
	}
	s := newActivitySource(t, f)

	// Three pages were read before the limit: they are kept, the call ends cleanly and not done,
	// and it stops at the limit instead of retrying.
	res, err := s.Activity(context.Background(), core.ActivityRequest{Budget: 10})
	if err != nil {
		t.Fatalf("err = %v, want a clean stop after progress", err)
	}
	if n := len(f.requests()); n != 4 {
		t.Errorf("%d requests, want 4 (3 pages + the throttled one, no retry)", n)
	}
	if len(res.Txs) != 149 || res.BackfillDone { // the third page's last entry is held back
		t.Fatalf("partial result: txs=%d done=%v", len(res.Txs), res.BackfillDone)
	}

	limited = false
	seen := map[string]bool{}
	for _, tx := range res.Txs {
		seen[tx.ExternalID] = true
	}
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State, Budget: 10})
	if err != nil || !res2.BackfillDone {
		t.Fatalf("resume: done=%v err=%v", res2.BackfillDone, err)
	}
	for _, tx := range res2.Txs {
		if seen[tx.ExternalID] {
			t.Fatalf("%s returned again after resuming", tx.ExternalID)
		}
		seen[tx.ExternalID] = true
	}
	if len(seen) != 400 {
		t.Errorf("saw %d entries, want 400", len(seen))
	}
}

func TestActivityRateLimitOnFirstRequestIsAnError(t *testing.T) {
	f := newLedgerFake(t)
	f.add(deposits("a", 10, baseT)...)
	f.respond = func(n int, w http.ResponseWriter) bool {
		fmt.Fprint(w, `{"error":["EAPI:Rate limit exceeded"]}`)
		return true
	}
	s := newActivitySource(t, f)
	state := []byte(`{"runs":[{"id":"L-a-0001","last":"L-a-0010","n":10}],"count":10}`)
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 10})
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "Rate limit exceeded") {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests, want 1: a throttled key is left alone", n)
	}
	if len(res.Txs) != 0 || res.BackfillDone || string(res.State) != string(state) {
		t.Errorf("result = %+v, want no progress and the same state %s", res, state)
	}
}

func TestActivityThrottledVariantsAreRateLimits(t *testing.T) {
	for _, msg := range []string{"EAPI:Rate limit exceeded", "EService:Throttled: 1750000000", "EGeneral:Temporary lockout"} {
		if err := krakenError([]string{msg}, 0); !errors.Is(err, ErrRateLimited) {
			t.Errorf("%q is not reported as a rate limit: %v", msg, err)
		}
	}
	if err := krakenError([]string{"EAPI:Invalid nonce"}, 200); errors.Is(err, ErrRateLimited) {
		t.Error("an invalid nonce is not a rate limit")
	}
}

func TestActivityHTTP429IsRateLimitWithoutHammering(t *testing.T) {
	f := newLedgerFake(t)
	f.respond = func(n int, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}
	s := newActivitySource(t, f)
	_, err := s.Activity(context.Background(), core.ActivityRequest{Budget: 10})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v", err)
	}
	// httpx retries a 429 with backoff (fastClient: 1 + 2 retries), and Activity stops after that.
	if n := len(f.requests()); n != 3 {
		t.Errorf("%d requests, want 3 (httpx's own backoff retries only)", n)
	}
}

func TestActivityBadEntryRejectsPageButKeepsState(t *testing.T) {
	f := newLedgerFake(t)
	es := deposits("a", 60, baseT)
	es[55].Amount = "lots" // on the second page
	f.add(es...)
	s := newActivitySource(t, f)

	// Page one is kept and the call ends cleanly; the bad page is not used at all.
	res, err := s.Activity(context.Background(), core.ActivityRequest{Budget: 5})
	if err != nil || len(res.Txs) != 49 || res.BackfillDone { // page one, minus its held-back last entry
		t.Fatalf("txs=%d done=%v err=%v", len(res.Txs), res.BackfillDone, err)
	}
	// The next call fails at once, so the problem is reported.
	_, err = s.Activity(context.Background(), core.ActivityRequest{State: res.State, Budget: 5})
	if err == nil || !strings.Contains(err.Error(), "L-a-0056") {
		t.Fatalf("err = %v, want the bad entry named", err)
	}
	// Fixed data: the next call carries on with page two only.
	f.mu.Lock()
	f.entries[55].Amount = "0.01"
	f.mu.Unlock()
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State, Budget: 5})
	if err != nil || len(res2.Txs) != 11 || !res2.BackfillDone {
		t.Fatalf("resume: txs=%d done=%v err=%v", len(res2.Txs), res2.BackfillDone, err)
	}
}

func TestActivityHonoursContext(t *testing.T) {
	f := newLedgerFake(t)
	s := newActivitySource(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Activity(ctx, core.ActivityRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Randomised: whatever the budgets and arrivals, every refid comes out exactly once, complete
// ---------------------------------------------------------------------------------------------

// newArrivals builds k refids (a mix of single deposits and two-leg trades) newest first, with
// times above newest, so they sit on top of the ledger.
func newArrivals(rng *lcg, prefix string, k int, newest float64) []fakeEntry {
	var out []fakeEntry
	for i := 0; i < k; i++ {
		ref := fmt.Sprintf("R-%s-%03d", prefix, i)
		ts := newest - float64(i)*60
		if rng.next(3) == 0 {
			out = append(out,
				fakeEntry{fmt.Sprintf("L-%s-%03da", prefix, i), ref, "trade", "", "XXBT", "0.001", "0", ts},
				fakeEntry{fmt.Sprintf("L-%s-%03db", prefix, i), ref, "trade", "", "ZUSD", "-60", "0.1", ts})
		} else {
			out = append(out, fakeEntry{fmt.Sprintf("L-%s-%03d", prefix, i), ref, "deposit", "", "XXBT", "0.01", "0", ts})
		}
	}
	return out
}

type lcg struct{ s uint64 }

func (l *lcg) next(n int) int {
	l.s = l.s*6364136223846793005 + 1442695040888963407
	return int((l.s >> 33) % uint64(n))
}

func TestActivityRandomisedBudgetsAndArrivals(t *testing.T) {
	for seed := uint64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := &lcg{s: seed * 7919}
			f := newLedgerFake(t)
			clock := baseT
			batch := 0
			arrive := func(k int) {
				// Called with f.mu held (from respond) or not: callers lock as needed.
				batch++
				clock += 100_000
				f.entries = append(newArrivals(rng, fmt.Sprintf("b%d", batch), k, clock), f.entries...)
			}
			f.mu.Lock()
			arrive(40 + rng.next(120))
			f.respond = func(n int, w http.ResponseWriter) bool {
				if rng.next(12) == 0 { // new entries show up between two requests of a call
					arrive(1 + rng.next(60))
				}
				return false
			}
			f.mu.Unlock()
			s := newActivitySource(t, f)

			var state []byte
			legs := map[string]int{}
			for call := 1; call <= 160; call++ {
				if rng.next(3) == 0 { // ... or between calls
					f.mu.Lock()
					arrive(1 + rng.next(70))
					f.mu.Unlock()
				}
				budget := 1 + rng.next(4)
				if call > 120 { // drain
					f.mu.Lock()
					f.respond = nil
					f.mu.Unlock()
					budget = 20
				}
				res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: budget})
				if err != nil {
					t.Fatalf("call %d: %v", call, err)
				}
				for _, tx := range res.Txs {
					if _, dup := legs[tx.ExternalID]; dup {
						t.Fatalf("call %d (budget %d) returned %s again", call, budget, tx.ExternalID)
					}
					legs[tx.ExternalID] = len(tx.Legs)
				}
				state = res.State
				if call > 120 && res.BackfillDone {
					break
				}
			}

			f.mu.Lock()
			want := map[string]int{}
			for _, e := range f.entries {
				want[e.RefID]++
			}
			f.mu.Unlock()
			if len(legs) != len(want) {
				t.Fatalf("saw %d refids, ledger has %d", len(legs), len(want))
			}
			for ref, n := range want {
				if legs[ref] != n {
					t.Fatalf("%s has %d legs, want %d (a group was split)", ref, legs[ref], n)
				}
			}
		})
	}
}
