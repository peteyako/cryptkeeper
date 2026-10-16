package activity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))

// fakeSource is a core.ActivitySource driven by a function of the call number.
type fakeSource struct {
	mu   sync.Mutex
	reqs []core.ActivityRequest
	fn   func(n int, req core.ActivityRequest) (core.ActivityResult, error)
}

func (f *fakeSource) Activity(_ context.Context, req core.ActivityRequest) (core.ActivityResult, error) {
	f.mu.Lock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return f.fn(n, req)
}

func (f *fakeSource) requests() []core.ActivityRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.ActivityRequest(nil), f.reqs...)
}

func (f *fakeSource) calls() int { return len(f.requests()) }

// ctxSource is a fakeSource variant whose function also sees the call's context.
type ctxSource struct {
	fn func(ctx context.Context) (core.ActivityResult, error)
}

func (c ctxSource) Activity(ctx context.Context, _ core.ActivityRequest) (core.ActivityResult, error) {
	return c.fn(ctx)
}

func fakeFn(fn func(n int, req core.ActivityRequest) (core.ActivityResult, error)) *fakeSource {
	return &fakeSource{fn: fn}
}

// doneWith is a source that always reports the given txs, backfill complete.
func doneWith(txs ...core.Tx) *fakeSource {
	return fakeFn(func(int, core.ActivityRequest) (core.ActivityResult, error) {
		return core.ActivityResult{Txs: txs, BackfillDone: true}, nil
	})
}

func entry(id string, src core.ActivitySource) Entry { return Entry{ID: id, Label: id, Src: src} }

func newSyncer(t *testing.T, entries ...Entry) *Syncer {
	t.Helper()
	return &Syncer{
		Store:   newStore(t),
		Sources: func() []Entry { return entries },
		Since:   func() time.Time { return t0.Add(-30 * 24 * time.Hour) },
		Log:     quiet,
	}
}

func syncOnce(t *testing.T, s *Syncer) {
	t.Helper()
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func statuses(t *testing.T, s *Syncer) map[string]SyncStatus {
	t.Helper()
	st, err := s.Store.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSyncStatePersistedAndPassedBack(t *testing.T) {
	src := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		return core.ActivityResult{
			Txs:          []core.Tx{mk("src", fmt.Sprint("tx", n), t0.Add(time.Duration(n)*time.Minute), "BTC", 1)},
			State:        []byte(fmt.Sprint("state", n+1)),
			BackfillDone: true,
		}, nil
	})
	s := newSyncer(t, entry("src", src))
	syncOnce(t, s)
	syncOnce(t, s)
	syncOnce(t, s)

	reqs := src.requests()
	if len(reqs) != 3 {
		t.Fatalf("calls = %d, want 1 per tick once backfill is done", len(reqs))
	}
	for i, want := range []string{"", "state1", "state2"} {
		if string(reqs[i].State) != want {
			t.Fatalf("call %d got state %q, want %q", i, reqs[i].State, want)
		}
		if reqs[i].Budget != 30 {
			t.Fatalf("default budget = %d", reqs[i].Budget)
		}
		if !reqs[i].Since.Equal(t0.Add(-30 * 24 * time.Hour)) {
			t.Fatalf("since = %v", reqs[i].Since)
		}
	}
	if reqs[0].State != nil {
		t.Fatalf("first call state = %v, want nil", reqs[0].State)
	}

	st := statuses(t, s)["src"]
	if !st.BackfillDone || st.Count != 3 || st.LastError != "" || time.Since(st.LastSync) > time.Minute {
		t.Fatalf("status: %+v", st)
	}
	state, done, _ := s.Store.loadState(context.Background(), "src")
	if string(state) != "state3" || !done {
		t.Fatalf("saved state %q done=%v", state, done)
	}

	// a custom budget is forwarded
	s.Budget = 7
	syncOnce(t, s)
	if got := src.requests()[3].Budget; got != 7 {
		t.Fatalf("budget = %d", got)
	}
}

func TestBackfillRunsSeveralCallsPerTick(t *testing.T) {
	never := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		return core.ActivityResult{
			Txs:   []core.Tx{mk("never", fmt.Sprint("tx", n), t0.Add(time.Duration(n)*time.Minute), "BTC", 1)},
			State: []byte(fmt.Sprint("c", n)),
		}, nil
	})
	// finishes on its second call
	short := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		return core.ActivityResult{State: []byte(fmt.Sprint("s", n)), BackfillDone: n >= 1}, nil
	})
	s := newSyncer(t, entry("never", never), entry("short", short))

	syncOnce(t, s)
	if n := never.calls(); n != 3 {
		t.Fatalf("unfinished backfill made %d calls in one tick, want 3", n)
	}
	var states []string
	for _, r := range never.requests() {
		states = append(states, string(r.State))
	}
	if !reflect.DeepEqual(states, []string{"", "c0", "c1"}) {
		t.Fatalf("states chained as %q", states)
	}
	if n := short.calls(); n != 2 {
		t.Fatalf("source that finished on its 2nd call made %d calls", n)
	}
	st := statuses(t, s)
	if st["never"].BackfillDone || st["never"].Count != 3 {
		t.Fatalf("never: %+v", st["never"])
	}
	if !st["short"].BackfillDone {
		t.Fatalf("short: %+v", st["short"])
	}
	if state, _, _ := s.Store.loadState(context.Background(), "never"); string(state) != "c2" {
		t.Fatalf("state = %q", state)
	}

	syncOnce(t, s)
	if n := never.calls(); n != 6 {
		t.Fatalf("second tick: %d calls total, want 6", n)
	}
	if n := short.calls(); n != 3 {
		t.Fatalf("finished source is polled once per tick, got %d calls total", n)
	}
}

func TestSourceErrorIsolationAndLastError(t *testing.T) {
	flaky := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		switch n {
		case 0:
			return core.ActivityResult{Txs: []core.Tx{mk("flaky", "1", t0, "BTC", 1)}, State: []byte("good"), BackfillDone: true}, nil
		case 1: // no State: nothing fetched, so nothing to keep
			return core.ActivityResult{}, errors.New("rate limited")
		}
		if string(req.State) != "good" {
			return core.ActivityResult{}, fmt.Errorf("state after failure = %q", req.State)
		}
		return core.ActivityResult{State: []byte("better"), BackfillDone: true}, nil
	})
	steady := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		return core.ActivityResult{Txs: []core.Tx{mk("steady", fmt.Sprint(n), t0.Add(time.Duration(n)*time.Minute), "ETH", 1)}, BackfillDone: true}, nil
	})
	s := newSyncer(t, entry("flaky", flaky), entry("steady", steady))

	syncOnce(t, s)
	if st := statuses(t, s)["flaky"]; st.LastError != "" || st.Count != 1 {
		t.Fatalf("after success: %+v", st)
	}
	firstSync := statuses(t, s)["flaky"].LastSync

	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("a failing source must not fail the pass: %v", err)
	}
	st := statuses(t, s)
	if !strings.Contains(st["flaky"].LastError, "rate limited") {
		t.Fatalf("last_error = %q", st["flaky"].LastError)
	}
	if !st["flaky"].BackfillDone || st["flaky"].Count != 1 || !st["flaky"].LastSync.Equal(firstSync) {
		t.Fatalf("failure changed progress: %+v", st["flaky"])
	}
	if st["steady"].LastError != "" || st["steady"].Count != 2 {
		t.Fatalf("steady was affected: %+v", st["steady"])
	}
	if state, _, _ := s.Store.loadState(context.Background(), "flaky"); string(state) != "good" {
		t.Fatalf("state after failure = %q, want the old one", state)
	}

	syncOnce(t, s) // recovers from the old state and clears the error
	st = statuses(t, s)
	if st["flaky"].LastError != "" {
		t.Fatalf("error not cleared: %q", st["flaky"].LastError)
	}
	if state, _, _ := s.Store.loadState(context.Background(), "flaky"); string(state) != "better" {
		t.Fatalf("state = %q", state)
	}
}

func TestSourcePanicRecovered(t *testing.T) {
	boom := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		if n == 0 {
			return core.ActivityResult{Txs: []core.Tx{mk("boom", "1", t0, "BTC", 1)}, State: []byte("s0")}, nil
		}
		panic("kaboom")
	})
	always := fakeFn(func(int, core.ActivityRequest) (core.ActivityResult, error) { panic(errors.New("nil map")) })
	ok := doneWith(mk("ok", "1", t0, "ETH", 1))
	s := newSyncer(t, entry("always", always), entry("boom", boom), entry("ok", ok))

	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatalf("panic leaked out of the pass: %v", err)
	}
	st := statuses(t, s)
	if !strings.Contains(st["always"].LastError, "panic") || !strings.Contains(st["always"].LastError, "nil map") {
		t.Fatalf("always: %q", st["always"].LastError)
	}
	// boom made progress on call 1 (kept) and panicked on call 2
	if !strings.Contains(st["boom"].LastError, "kaboom") || st["boom"].Count != 1 {
		t.Fatalf("boom: %+v", st["boom"])
	}
	if state, _, _ := s.Store.loadState(context.Background(), "boom"); string(state) != "s0" {
		t.Fatalf("boom state = %q", state)
	}
	if st["ok"].LastError != "" || st["ok"].Count != 1 {
		t.Fatalf("ok: %+v", st["ok"])
	}
}

func TestOnNewOnlyNewIDs(t *testing.T) {
	pending := mk("src", "a", t0, "BTC", 1)
	pending.Status = core.TxPending
	confirmed := pending
	confirmed.Status = core.TxConfirmed
	b := mk("src", "b", t0.Add(time.Minute), "BTC", 1)
	c := mk("src", "c", t0.Add(2*time.Minute), "BTC", 1)

	ticks := [][]core.Tx{{b, pending}, {confirmed, c}, {confirmed}, {}}
	src := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		return core.ActivityResult{Txs: ticks[n], BackfillDone: true}, nil
	})
	s := newSyncer(t, entry("src", src))
	var got [][]string
	s.OnNew = func(txs []core.Tx) { got = append(got, ids(txs)) }

	for range ticks {
		syncOnce(t, s)
	}
	want := [][]string{{"src:a", "src:b"}, {"src:c"}} // oldest first; no call when nothing is new
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OnNew calls = %v, want %v", got, want)
	}
	if tx, _, _ := s.Store.Get(context.Background(), "src:a"); tx.Status != core.TxConfirmed {
		t.Fatalf("update not stored: %+v", tx)
	}
}

func TestOnNewPanicDoesNotBreakSync(t *testing.T) {
	s := newSyncer(t, entry("src", doneWith(mk("src", "a", t0, "BTC", 1))))
	s.OnNew = func([]core.Tx) { panic("hook bug") }
	syncOnce(t, s)
	if st := statuses(t, s)["src"]; st.Count != 1 || st.LastError != "" {
		t.Fatalf("%+v", st)
	}
}

// ---- valuation ----

type fakeValuer struct {
	mu    sync.Mutex
	calls []string
	price func(symbol string, day time.Time) (float64, bool, error)
}

func (v *fakeValuer) Historical(_ context.Context, symbol string, day time.Time) (float64, bool, error) {
	v.mu.Lock()
	v.calls = append(v.calls, symbol+"@"+day.Format(time.RFC3339))
	v.mu.Unlock()
	return v.price(symbol, day)
}

func (v *fakeValuer) called() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := append([]string(nil), v.calls...)
	sort.Strings(out)
	return out
}

func usdOf(t *testing.T, s *Store, id string) (legs []*float64, fee *float64) {
	t.Helper()
	tx, ok, err := s.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get %s: %v %v", id, ok, err)
	}
	for _, l := range tx.Legs {
		legs = append(legs, l.USD)
	}
	if tx.Fee != nil {
		fee = tx.Fee.USD
	}
	return legs, fee
}

func wantUSD(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: USD not set, want %v", name, want)
	}
	if d := *got - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("%s: USD = %v, want %v", name, *got, want)
	}
}

func TestValuation(t *testing.T) {
	day1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	v := &fakeValuer{price: func(sym string, day time.Time) (float64, bool, error) {
		switch sym {
		case "BTC":
			if day.Equal(day2) {
				return 60000, true, nil
			}
			return 50000, true, nil
		case "DOGE":
			return 0, false, errors.New("upstream 500")
		case "XRP":
			return 0, false, nil
		case "PANIC":
			panic("valuer bug")
		}
		return 0, false, fmt.Errorf("unexpected symbol %s", sym)
	}}

	preset := -3000.0
	srcLegs := []core.Leg{
		{AssetKey: "BTC", Symbol: "BTC", Amount: 2},
		{AssetKey: "USDC", Symbol: "USDC", Amount: 100},
		{AssetKey: "spl:Mint1", Symbol: "FOO", Amount: 5},
		{AssetKey: "ETH", Symbol: "ETH", Amount: -1, USD: &preset},
	}
	tx1 := mk("one", "1", t0, "BTC", 0)
	tx1.Legs = srcLegs
	tx1.Fee = &core.Leg{AssetKey: "BTC", Symbol: "BTC", Amount: -0.001}
	tx3 := mk("one", "3", t0.Add(24*time.Hour), "BTC", 1) // next day
	tx4 := mk("one", "4", t0, "DOGE", 10)
	tx4.Legs = append(tx4.Legs, core.Leg{AssetKey: "XRP", Amount: 5}, core.Leg{AssetKey: "PANIC", Amount: 1})
	tx2 := mk("two", "2", t0.Add(time.Hour), "BTC", -1) // same BTC day, other source
	// 02:00 on 2 March in UTC+5 is still 1 March in UTC
	tx6 := mk("two", "6", time.Date(2026, 3, 2, 2, 0, 0, 0, time.FixedZone("X", 5*3600)), "BTC", 1)

	s := newSyncer(t, entry("one", doneWith(tx1, tx3, tx4)), entry("two", doneWith(tx2, tx6)))
	s.Valuer = v
	syncOnce(t, s)
	syncOnce(t, s) // second pass re-reports the same txs: new pass, new cache, same answers

	st := s.Store
	legs, fee := usdOf(t, st, "one:1")
	wantUSD(t, "BTC leg", legs[0], 100000)
	wantUSD(t, "USDC pegged", legs[1], 100)
	if legs[2] != nil {
		t.Fatalf("spl leg valued: %v", *legs[2])
	}
	wantUSD(t, "preset kept", legs[3], -3000)
	wantUSD(t, "fee", fee, -50)

	legs, _ = usdOf(t, st, "two:2")
	wantUSD(t, "send valued negative", legs[0], -50000)
	legs, _ = usdOf(t, st, "one:3")
	wantUSD(t, "next day price", legs[0], 60000)
	legs, _ = usdOf(t, st, "two:6")
	wantUSD(t, "UTC day, not local", legs[0], 50000)

	legs, _ = usdOf(t, st, "one:4")
	if legs[0] != nil || legs[1] != nil || legs[2] != nil {
		t.Fatalf("failed valuations must leave USD unset: %v", legs)
	}
	if got := statuses(t, s)["one"]; got.LastError != "" || got.Count != 3 {
		t.Fatalf("valuation problems leaked into the sync: %+v", got)
	}

	// once per (symbol, day) per pass: BTC@day1 (3 txs on two sources), BTC@day2, DOGE, XRP, PANIC;
	// never for USDC, spl: or a leg that already has a value
	day := func(d time.Time) string { return d.Format(time.RFC3339) }
	one := []string{"BTC@" + day(day1), "BTC@" + day(day2), "DOGE@" + day(day1), "PANIC@" + day(day1), "XRP@" + day(day1)}
	want := append(append([]string(nil), one...), one...) // two passes
	sort.Strings(want)
	if got := v.called(); !reflect.DeepEqual(got, want) {
		t.Fatalf("valuer calls\n got  %v\n want %v", got, want)
	}

	// the source's own leg slice is not written to
	if srcLegs[0].USD != nil || srcLegs[1].USD != nil {
		t.Fatal("valuation mutated the source's legs")
	}
}

func TestNoValuerLeavesLegsAlone(t *testing.T) {
	tx := mk("src", "1", t0, "USDC", 10)
	s := newSyncer(t, entry("src", doneWith(tx)))
	syncOnce(t, s)
	if legs, _ := usdOf(t, s.Store, "src:1"); legs[0] != nil {
		t.Fatalf("valued without a valuer: %v", *legs[0])
	}
}

// ---- timeouts, cancellation, concurrency ----

func TestActivityTimeout(t *testing.T) {
	hang := ctxSource{fn: func(ctx context.Context) (core.ActivityResult, error) {
		<-ctx.Done()
		return core.ActivityResult{}, ctx.Err()
	}}
	s := newSyncer(t, entry("hang", hang), entry("ok", doneWith(mk("ok", "1", t0, "BTC", 1))))
	s.timeout = 50 * time.Millisecond

	start := time.Now()
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("hanging source blocked the pass")
	}
	st := statuses(t, s)
	if !strings.Contains(st["hang"].LastError, "deadline exceeded") {
		t.Fatalf("hang: %q", st["hang"].LastError)
	}
	if st["ok"].Count != 1 {
		t.Fatalf("ok: %+v", st["ok"])
	}
}

func TestCancelledPassIsNotASourceError(t *testing.T) {
	started := make(chan struct{})
	hang := ctxSource{fn: func(ctx context.Context) (core.ActivityResult, error) {
		close(started)
		<-ctx.Done()
		return core.ActivityResult{}, ctx.Err()
	}}
	s := newSyncer(t, entry("hang", hang))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.SyncOnce(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SyncOnce ignored cancellation")
	}
	if st := statuses(t, s)["hang"]; st.LastError != "" {
		t.Fatalf("shutdown recorded as source error: %q", st.LastError)
	}
}

func TestSourcesRunConcurrentlyUpToFour(t *testing.T) {
	var inflight, peak atomic.Int32
	slow := func() core.ActivitySource {
		return fakeFn(func(int, core.ActivityRequest) (core.ActivityResult, error) {
			n := inflight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			inflight.Add(-1)
			return core.ActivityResult{BackfillDone: true}, nil
		})
	}
	var entries []Entry
	for i := 0; i < 12; i++ {
		entries = append(entries, entry(fmt.Sprint("s", i), slow()))
	}
	entries = append(entries, entries[0]) // duplicate IDs are synced once
	s := newSyncer(t, entries...)
	syncOnce(t, s)
	if p := peak.Load(); p > 4 || p < 2 {
		t.Fatalf("peak concurrency = %d, want 2..4", p)
	}
	if got := len(statuses(t, s)); got != 12 {
		t.Fatalf("statuses = %d", got)
	}
}

func TestPrepareFillsIdentityAndDropsBadEntries(t *testing.T) {
	bare := core.Tx{ExternalID: "x1", Time: t0, Kind: core.TxReceive, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "BTC", Amount: 1}}}
	noID := core.Tx{Time: t0, Legs: []core.Leg{{AssetKey: "BTC", Amount: 1}}}
	s := newSyncer(t, entry("src", doneWith(bare, noID)))
	syncOnce(t, s)
	got, ok, _ := s.Store.Get(context.Background(), "src:x1")
	if !ok || got.SourceID != "src" {
		t.Fatalf("identity not filled: %+v %v", got, ok)
	}
	if all, _ := s.Store.All(context.Background(), nil); len(all) != 1 {
		t.Fatalf("stored %d, want 1", len(all))
	}
	if st := statuses(t, s)["src"]; st.LastError != "" {
		t.Fatalf("%+v", st)
	}
}

// ---- transfer matching inside the syncer ----

func TestSyncOnceMatchesTransfers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sent := mk("cb", "w1", now.Add(-2*time.Hour), "BTC", -1)
	sent.TxHash = "deadbeef"
	sent.Status = core.TxPending

	var tick atomic.Int32
	cb := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		tx := sent
		if n >= 1 { // the exchange later reports the withdrawal as completed, knowing nothing of peers
			tx.Status = core.TxConfirmed
		}
		old := mk("cb", "old", now.AddDate(0, 0, -100), "ETH", -1) // outside the 90-day window
		return core.ActivityResult{Txs: []core.Tx{tx, old}, BackfillDone: true}, nil
	})
	wallet := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		if tick.Load() == 0 {
			return core.ActivityResult{BackfillDone: true}, nil // not seen on chain yet
		}
		rcv := mk("wallet", "in1", now.Add(-time.Hour), "BTC", 0.9999)
		rcv.TxHash = "DEADBEEF"
		oldIn := mk("wallet", "oldin", now.AddDate(0, 0, -100), "ETH", 1)
		return core.ActivityResult{Txs: []core.Tx{rcv, oldIn}, BackfillDone: true}, nil
	})
	s := newSyncer(t, entry("cb", cb), entry("wallet", wallet))
	var onNew [][]core.Tx
	s.OnNew = func(txs []core.Tx) { onNew = append(onNew, txs) }

	syncOnce(t, s) // only the send is known: no partner yet
	if tx, _, _ := s.Store.Get(context.Background(), "cb:w1"); tx.PeerID != "" || tx.Kind != core.TxSend {
		t.Fatalf("matched without a partner: %+v", tx)
	}

	tick.Store(1)
	syncOnce(t, s)
	snd, _, _ := s.Store.Get(context.Background(), "cb:w1")
	rcv, _, _ := s.Store.Get(context.Background(), "wallet:in1")
	if snd.Kind != core.TxTransfer || snd.PeerID != "wallet:in1" || snd.PeerSource != "wallet" ||
		rcv.Kind != core.TxTransfer || rcv.PeerID != "cb:w1" || rcv.PeerSource != "cb" {
		t.Fatalf("not linked:\n%+v\n%+v", snd, rcv)
	}
	if snd.Status != core.TxConfirmed {
		t.Fatalf("status update lost: %+v", snd)
	}
	// the 100-day-old pair is outside the matching window
	if old, _, _ := s.Store.Get(context.Background(), "cb:old"); old.PeerID != "" {
		t.Fatalf("old tx matched: %+v", old)
	}

	// OnNew: the receive arrives already linked
	if len(onNew) != 2 {
		t.Fatalf("OnNew calls = %d", len(onNew))
	}
	last := onNew[1]
	var inbound *core.Tx
	for i := range last {
		if last[i].ID == "wallet:in1" {
			inbound = &last[i]
		}
	}
	if inbound == nil || inbound.Kind != core.TxTransfer || inbound.PeerID != "cb:w1" {
		t.Fatalf("OnNew did not carry the link: %+v", last)
	}

	// another pass re-reports the send; the link survives and nothing is re-announced
	syncOnce(t, s)
	snd, _, _ = s.Store.Get(context.Background(), "cb:w1")
	if snd.Kind != core.TxTransfer || snd.PeerID != "wallet:in1" {
		t.Fatalf("link lost on re-report: %+v", snd)
	}
	if len(onNew) != 2 {
		t.Fatalf("OnNew fired for known txs: %d calls", len(onNew))
	}
}

// ---- Run / Kick ----

func TestRunAndKick(t *testing.T) {
	calls := make(chan struct{}, 16)
	src := fakeFn(func(int, core.ActivityRequest) (core.ActivityResult, error) {
		calls <- struct{}{}
		return core.ActivityResult{BackfillDone: true}, nil
	})
	s := newSyncer(t, entry("src", src))
	s.Interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { s.Run(ctx); close(stopped) }()

	wait := func(what string) {
		t.Helper()
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("no sync: %s", what)
		}
	}
	wait("immediate first tick")
	select {
	case <-calls:
		t.Fatal("second tick before the interval or a kick")
	case <-time.After(100 * time.Millisecond):
	}
	s.Kick()
	wait("kicked tick")
	s.Kick()
	s.Kick() // coalesced; must not block
	wait("second kick")

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestRunTicksOnInterval(t *testing.T) {
	var n atomic.Int32
	src := fakeFn(func(int, core.ActivityRequest) (core.ActivityResult, error) {
		n.Add(1)
		return core.ActivityResult{BackfillDone: true}, nil
	})
	s := newSyncer(t, entry("src", src))
	s.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n.Load() < 3 {
		t.Fatalf("only %d ticks", n.Load())
	}
}

func TestKickBeforeRunIsSafe(t *testing.T) {
	s := &Syncer{}
	s.Kick() // zero-value Syncer: must not panic or block
	s.Kick()
}

func TestSyncOnceWithoutSources(t *testing.T) {
	s := &Syncer{Store: newStore(t), Log: quiet}
	if err := s.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPartialProgressIsKeptWhenASourceFailsMidway(t *testing.T) {
	st := newStore(t)
	part := core.Tx{ID: "w:1", SourceID: "w", ExternalID: "1", Time: time.Now(), Kind: core.TxReceive, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "BTC", Symbol: "BTC", Amount: 1}}}
	src := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		if n == 0 {
			return core.ActivityResult{Txs: []core.Tx{part}, State: []byte(`{"page":2}`)}, errors.New("page 2 timed out")
		}
		if string(req.State) != `{"page":2}` {
			t.Errorf("next call must resume from the partial state, got %q", req.State)
		}
		return core.ActivityResult{State: []byte(`{"page":3}`), BackfillDone: true}, nil
	})
	sy := &Syncer{Store: st, Log: quiet, Sources: func() []Entry { return []Entry{{ID: "w", Label: "W", Src: src}} }}
	sy.SyncOnce(context.Background())
	if _, ok, _ := st.Get(context.Background(), "w:1"); !ok {
		t.Fatal("transactions fetched before the failure must be stored")
	}
	stat, _ := st.Statuses(context.Background())
	if stat["w"].LastError == "" || stat["w"].BackfillDone {
		t.Fatalf("error must be recorded and backfill not marked done: %+v", stat["w"])
	}
	sy.SyncOnce(context.Background())
	if stat, _ := st.Statuses(context.Background()); stat["w"].LastError != "" || !stat["w"].BackfillDone {
		t.Fatalf("a later successful call clears the error: %+v", stat["w"])
	}

	// An error without a State carries no trustworthy progress: nothing is stored.
	bad := fakeFn(func(int, core.ActivityRequest) (core.ActivityResult, error) {
		return core.ActivityResult{Txs: []core.Tx{{ID: "x:1", SourceID: "x", ExternalID: "1", Time: time.Now()}}}, errors.New("boom")
	})
	sy.Sources = func() []Entry { return []Entry{{ID: "x", Label: "X", Src: bad}} }
	sy.SyncOnce(context.Background())
	if _, ok, _ := st.Get(context.Background(), "x:1"); ok {
		t.Fatal("result without State must be dropped")
	}
}

func TestSubscribeSignalsStartAndFinish(t *testing.T) {
	var s *Syncer
	sawRunning := false
	src := fakeFn(func(n int, req core.ActivityRequest) (core.ActivityResult, error) {
		sawRunning = s.Running()["src"]
		return core.ActivityResult{}, errors.New("boom")
	})
	s = newSyncer(t, entry("src", src))
	ch, cancel := s.Subscribe()
	defer cancel()

	syncOnce(t, s)
	if !sawRunning {
		t.Fatal("source must be reported as running during its sync")
	}
	if len(s.Running()) != 0 {
		t.Fatalf("still running after the pass: %v", s.Running())
	}
	select {
	case <-ch:
	default:
		t.Fatal("no signal after the pass")
	}
	// The finish signal comes after the outcome is saved, so a subscriber re-reading the
	// statuses on it sees the error.
	if st := statuses(t, s)["src"]; st.LastError != "boom" {
		t.Fatalf("last error = %q", st.LastError)
	}
	cancel()
	syncOnce(t, s)
	select {
	case <-ch:
		t.Fatal("signal after cancel")
	default:
	}
}

// ---- mint valuation and revalue ----

type fakeMintValuer struct {
	fakeValuer
	mintCalls []string
	mintPrice func(mint string, day time.Time) (float64, bool, error)
}

func (v *fakeMintValuer) HistoricalMint(_ context.Context, mint string, day time.Time) (float64, bool, error) {
	v.mu.Lock()
	v.mintCalls = append(v.mintCalls, mint+"@"+day.Format(time.RFC3339))
	v.mu.Unlock()
	return v.mintPrice(mint, day)
}

func (v *fakeMintValuer) mintCalled() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.mintCalls...)
}

func TestValuerFallsBackToMint(t *testing.T) {
	day := t0.UTC().Truncate(24 * time.Hour)
	v := &fakeMintValuer{mintPrice: func(m string, d time.Time) (float64, bool, error) {
		if m == "M" && d.Equal(day) {
			return 0.5, true, nil
		}
		return 0, false, nil
	}}
	v.price = func(string, time.Time) (float64, bool, error) {
		t.Fatal("symbol valuer called for spl key")
		return 0, false, nil
	}
	tx := mk("src", "1", t0, "spl:M", -10)
	tx.Legs[0].Mint = "M"
	tx.Fee = &core.Leg{AssetKey: "spl:M", Mint: "M", Amount: -2}
	s := newSyncer(t, entry("src", doneWith(tx)))
	s.Valuer = v
	syncOnce(t, s)
	legs, fee := usdOf(t, s.Store, "src:1")
	wantUSD(t, "leg", legs[0], -5)
	wantUSD(t, "fee", fee, -1)
	if got := v.mintCalled(); len(got) != 1 {
		t.Fatalf("mint lookups should be cached per pass: %v", got)
	}
}

func TestValuerSymbolFirst(t *testing.T) {
	v := &fakeMintValuer{mintPrice: func(string, time.Time) (float64, bool, error) {
		t.Fatal("mint valuer called although the symbol priced the leg")
		return 0, false, nil
	}}
	v.price = func(sym string, _ time.Time) (float64, bool, error) { return 0.8, sym == "JUP", nil }
	tx := mk("src", "1", t0, "JUP", 10)
	tx.Legs[0].Mint = "JUPmint"
	s := newSyncer(t, entry("src", doneWith(tx)))
	s.Valuer = v
	syncOnce(t, s)
	legs, _ := usdOf(t, s.Store, "src:1")
	wantUSD(t, "leg", legs[0], 8)
}

func TestRevalueFillsOnlyMissing(t *testing.T) {
	ten := 10.0
	v := &fakeMintValuer{mintPrice: func(m string, _ time.Time) (float64, bool, error) { return 0.5, m == "M", nil }}
	v.price = func(string, time.Time) (float64, bool, error) { return 0, false, nil }
	old := mk("src", "old", t0, "A", 1)
	old.Legs = []core.Leg{
		{AssetKey: "A", Symbol: "A", Amount: 1, USD: &ten},
		{AssetKey: "spl:M", Mint: "M", Amount: 4},
		{AssetKey: "spl:X", Mint: "X", Amount: 4}, // stays unpriceable
		{AssetKey: "EUR", Symbol: "EUR", Amount: 4},
	}
	clean := mk("src", "clean", t0, "A", 1)
	clean.Legs[0].USD = &ten
	s := newSyncer(t, entry("src", doneWith()))
	s.Valuer = v
	if _, err := s.Store.Upsert(context.Background(), []core.Tx{old, clean}); err != nil {
		t.Fatal(err)
	}
	syncOnce(t, s)
	legs, _ := usdOf(t, s.Store, "src:old")
	wantUSD(t, "A kept", legs[0], 10)
	wantUSD(t, "M filled", legs[1], 2)
	if legs[2] != nil || legs[3] != nil {
		t.Fatalf("unpriceable legs got a value: %v", legs)
	}
	if n := s.revalued.Load(); n != 1 {
		t.Fatalf("revalue upserted %d txs, want 1", n)
	}
	syncOnce(t, s) // nothing more can be filled: no upserts
	if n := s.revalued.Load(); n != 0 {
		t.Fatalf("second revalue upserted %d txs, want 0", n)
	}
}
