package alerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/store"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// ---- fakes ----

type fakeNotifier struct {
	mu       sync.Mutex
	sent     []Notification
	err      error
	panicMsg string
}

func (f *fakeNotifier) Send(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	f.sent = append(f.sent, n)
	return f.err
}

func (f *fakeNotifier) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeNotifier) take(from int) []Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Notification(nil), f.sent[from:]...)
}

func (f *fakeNotifier) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// ---- harness ----

type harness struct {
	t     *testing.T
	fx    fixture
	db    *store.DB
	rules *RuleStore
	n     *fakeNotifier
	clk   *fakeClock
	eng   *Engine

	pf        map[string]float64 // current portfolio values; missing = valuer says ok=false
	hist      map[string]float64 // value at now-window; missing = no history
	histCalls []histCall
	seen      int // notifications already returned by run
}

type histCall struct {
	id string
	at time.Time
}

func newHarness(t *testing.T, rules ...string) *harness {
	t.Helper()
	fx := newFixture(t)
	body := "version: 1\nrules:\n"
	for _, r := range rules {
		body += "  - " + r + "\n"
	}
	fx.write(t, body)
	db, err := store.Open(filepath.Join(fx.dir, "cryptkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{
		t: t, fx: fx, db: db, rules: fx.store(t),
		n: &fakeNotifier{}, clk: &fakeClock{t: t0},
		pf: map[string]float64{}, hist: map[string]float64{},
	}
	h.eng = h.newEngine()
	return h
}

// newEngine builds a fresh Engine over the same DB, rules, notifier and clock: what a
// process restart looks like.
func (h *harness) newEngine() *Engine {
	return &Engine{
		Rules:    h.rules,
		DB:       h.db,
		Notifier: h.n,
		BaseURL:  "http://cryptkeeper.test:8787",
		Portfolio: func(id string, _ portfolio.Snapshot) (float64, bool) {
			v, ok := h.pf[id]
			return v, ok
		},
		History: func(_ context.Context, id string, at time.Time) (float64, bool) {
			h.histCalls = append(h.histCalls, histCall{id, at})
			v, ok := h.hist[id]
			return v, ok
		},
		Labels: func(id string) string { return walletLabels[id] },
		PortfolioLabel: func(id string) string {
			if id == "lt" {
				return "Long-term"
			}
			return ""
		},
		Now: h.clk.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (h *harness) ctx() context.Context { return context.Background() }

// run evaluates a snapshot and returns the notifications it produced.
func (h *harness) run(snap portfolio.Snapshot) []Notification {
	h.t.Helper()
	h.eng.OnSnapshot(h.ctx(), snap)
	return h.fresh()
}

func (h *harness) fresh() []Notification {
	out := h.n.take(h.seen)
	h.seen += len(out)
	return out
}

func (h *harness) snap(ws ...portfolio.WalletView) []Notification {
	h.t.Helper()
	return h.run(snapshot(h.clk.Now(), nil, ws...))
}

func (h *harness) snapP(prices map[string]float64, ws ...portfolio.WalletView) []Notification {
	h.t.Helper()
	return h.run(snapshot(h.clk.Now(), prices, ws...))
}

func (h *harness) activity(txs []core.Tx, kinds map[string]core.Kind) []Notification {
	h.t.Helper()
	h.eng.OnActivity(h.ctx(), txs, kinds)
	return h.fresh()
}

func (h *harness) stateRows(ruleID string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM alert_state WHERE rule_id = ?`, ruleID).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// ---- snapshot builders ----

var walletLabels = map[string]string{"cold": "Cold storage", "vault": "Vault", "hot": "Hot wallet", "ex": "Exchange"}

type hold struct {
	sym    string
	amt    float64
	hidden bool
}

func wallet(id string, kind core.Kind, st portfolio.Status, hs ...hold) portfolio.WalletView {
	label := walletLabels[id]
	if label == "" {
		label = id
	}
	w := portfolio.WalletView{ID: id, Label: label, Kind: kind, Status: st}
	for _, h := range hs {
		row := portfolio.HoldingRow{Holding: core.Holding{WalletID: id, AssetKey: h.sym, Symbol: h.sym, Amount: h.amt}}
		if h.hidden {
			w.HiddenRows = append(w.HiddenRows, row)
		} else {
			w.Rows = append(w.Rows, row)
		}
	}
	return w
}

func synced(id string, kind core.Kind, hs ...hold) portfolio.WalletView {
	return wallet(id, kind, portfolio.StatusSynced, hs...)
}

func snapshot(now time.Time, prices map[string]float64, ws ...portfolio.WalletView) portfolio.Snapshot {
	s := portfolio.Snapshot{UpdatedAt: now, Wallets: ws, Prices: map[string]core.Price{}}
	for k, v := range prices {
		s.Prices[k] = core.Price{USD: v, AsOf: now}
	}
	return s
}

func one(t *testing.T, got []Notification) Notification {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("want exactly 1 notification, got %d: %+v", len(got), got)
	}
	return got[0]
}

func none(t *testing.T, got []Notification) {
	t.Helper()
	if len(got) != 0 {
		t.Fatalf("want no notifications, got %d: %+v", len(got), got)
	}
}

func hasTag(n Notification, tag string) bool {
	for _, t := range n.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("%q does not contain %q", s, sub)
		}
	}
}

// ---- balance_drop ----

const coldDrop = `{id: cold-drop, type: balance_drop, wallets: [cold]}`

func TestBalanceDropFiresOncePerDrop(t *testing.T) {
	h := newHarness(t, coldDrop)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}

	none(t, h.snap(cold(2))) // first sight only records the baseline

	n := one(t, h.snap(cold(1.5)))
	if n.RuleID != "cold-drop" || n.Priority != 5 || !hasTag(n, "rotating_light") {
		t.Fatalf("notification: %+v", n)
	}
	if n.Click != "http://cryptkeeper.test:8787/w/cold" {
		t.Fatalf("click %q", n.Click)
	}
	if !n.Time.Equal(t0) {
		t.Fatalf("time %v", n.Time)
	}
	mustContain(t, n.Title+n.Body, "Cold storage", "BTC", "25%", "2", "1.5")

	none(t, h.snap(cold(1.5))) // same drop is not announced twice
	one(t, h.snap(cold(1.2)))  // a further decrease fires again
	none(t, h.snap(cold(1.4))) // an increase just moves the baseline
	one(t, h.snap(cold(1.3)))  // and drops are measured from that new baseline
	if h.n.count() != 3 {
		t.Fatalf("%d notifications in total", h.n.count())
	}
}

func TestBalanceDropIgnoresCooldown(t *testing.T) {
	h := newHarness(t, `{id: cold-drop, type: balance_drop, wallets: [cold], cooldown: 24h}`)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(3))
	one(t, h.snap(cold(2)))
	h.clk.Advance(time.Minute)
	one(t, h.snap(cold(1)))
}

func TestBalanceDropMinPct(t *testing.T) {
	h := newHarness(t, `{id: cold-drop, type: balance_drop, wallets: [cold], minPct: 5}`)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(100))
	none(t, h.snap(cold(97)))               // 3% < 5%
	none(t, h.snap(cold(97*0.95+0.000001))) // just under 5% more
	n := one(t, h.snap(cold(80)))           // 17%+
	mustContain(t, n.Body, "BTC")
}

func TestBalanceDropMinPctZeroMeansAnyDecreaseBeyondEpsilon(t *testing.T) {
	h := newHarness(t, coldDrop)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(1))
	none(t, h.snap(cold(1-1e-10))) // below the 1e-9 noise floor
	one(t, h.snap(cold(1-1e-6)))
}

func TestBalanceDropSkipsStaleAndErrorAndKeepsBaseline(t *testing.T) {
	for _, st := range []portfolio.Status{portfolio.StatusStale, portfolio.StatusError, portfolio.StatusPending} {
		t.Run(string(st), func(t *testing.T) {
			h := newHarness(t, coldDrop)
			cold := func(btc float64) portfolio.WalletView {
				return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
			}
			h.snap(cold(10))

			none(t, h.snap(wallet("cold", core.KindHardware, st, hold{sym: "BTC", amt: 4}))) // never evaluated
			none(t, h.snap(cold(10)))                                                        // baseline was not touched: no drop, no fire

			none(t, h.snap(wallet("cold", core.KindHardware, st))) // empty rows while failing
			n := one(t, h.snap(cold(6)))                           // measured against the last synced 10
			mustContain(t, n.Body, "40%")
		})
	}
}

func TestBalanceDropWatchedSet(t *testing.T) {
	// No wallets listed: every non-exchange wallet.
	h := newHarness(t, `{id: any-drop, type: balance_drop}`)
	mk := func(scale float64) []portfolio.WalletView {
		return []portfolio.WalletView{
			synced("cold", core.KindHardware, hold{sym: "BTC", amt: 10 * scale}),
			synced("hot", core.KindHot, hold{sym: "SOL", amt: 10 * scale}),
			synced("man", core.KindManual, hold{sym: "ETH", amt: 10 * scale}),
			synced("ex", core.KindExchange, hold{sym: "BTC", amt: 10 * scale}),
		}
	}
	h.snap(mk(1)...)
	got := h.snap(mk(0.5)...)
	if len(got) != 3 {
		t.Fatalf("want 3 notifications (hardware, hot, manual), got %d: %+v", len(got), got)
	}
	for _, n := range got {
		if strings.Contains(n.Body, "Exchange") {
			t.Fatalf("exchange wallet must not be watched by default: %+v", n)
		}
	}

	// An explicit list is a whitelist, and may name an exchange.
	h2 := newHarness(t, `{id: ex-drop, type: balance_drop, wallets: [ex]}`)
	h2.snap(mk(1)...)
	n := one(t, h2.snap(mk(0.5)...))
	mustContain(t, n.Body, "Exchange")
}

func TestBalanceDropVanishedAssetAndHiddenRows(t *testing.T) {
	h := newHarness(t, coldDrop)
	h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 1}, hold{sym: "ETH", amt: 5}))
	// ETH row disappears entirely: a 100% drop.
	n := one(t, h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 1})))
	mustContain(t, n.Body, "ETH", "100%")
	if strings.Contains(n.Body, "BTC") {
		t.Fatalf("BTC did not change: %q", n.Body)
	}

	// An asset moving between the visible and hidden rows (e.g. it became unpriced) is not a drop.
	h2 := newHarness(t, coldDrop)
	h2.snap(synced("cold", core.KindHardware, hold{sym: "ETH", amt: 5}))
	none(t, h2.snap(synced("cold", core.KindHardware, hold{sym: "ETH", amt: 5, hidden: true})))
}

func TestBalanceDropSeveralAssetsOneNotification(t *testing.T) {
	h := newHarness(t, coldDrop)
	h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 2}, hold{sym: "ETH", amt: 10}, hold{sym: "SOL", amt: 5}))
	n := one(t, h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 1}, hold{sym: "ETH", amt: 10}, hold{sym: "SOL", amt: 4})))
	mustContain(t, n.Body, "BTC", "50%", "SOL", "20%")
	if strings.Contains(n.Body, "ETH") {
		t.Fatalf("ETH is unchanged: %q", n.Body)
	}
}

func TestBalanceDropSumsRowsOfOneAssetAndIgnoresDebt(t *testing.T) {
	h := newHarness(t, coldDrop)
	two := func(liquid, staked, debt float64) portfolio.WalletView {
		w := synced("cold", core.KindHardware, hold{sym: "SOL", amt: liquid}, hold{sym: "SOL", amt: staked})
		if debt != 0 {
			w.Rows = append(w.Rows, portfolio.HoldingRow{Holding: core.Holding{WalletID: "cold", AssetKey: "USDC", Symbol: "USDC", Amount: debt}})
		}
		return w
	}
	h.snap(two(5, 5, 0))
	none(t, h.snap(two(2, 8, 0))) // moved between positions: the total is unchanged
	none(t, h.snap(two(2, 8, -100)))
	none(t, h.snap(two(2, 8, -500))) // a growing debt is not a balance drop
	one(t, h.snap(two(1, 8, -500)))
}

func TestBalanceDropMissingWalletAndNewWalletBaseline(t *testing.T) {
	h := newHarness(t, `{id: any-drop, type: balance_drop}`)
	none(t, h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 1})))
	// A wallet that disappears and comes back lower has its baseline dropped with it: no alert.
	none(t, h.snap())
	none(t, h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 0.1})))
	// A brand new wallet only records a baseline.
	none(t, h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 0.1}), synced("hot", core.KindHot, hold{sym: "SOL", amt: 3})))
}

func TestDisabledRuleIsNotEvaluatedAndStartsFreshWhenEnabled(t *testing.T) {
	h := newHarness(t, coldDrop)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(10))
	off := false
	if _, err := h.rules.Upsert(Rule{ID: "cold-drop", Type: TypeBalanceDrop, Wallets: []string{"cold"}, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	none(t, h.snap(cold(5)))
	if n := h.stateRows("cold-drop"); n != 0 {
		t.Fatalf("disabled rule keeps %d state rows", n)
	}
	if _, err := h.rules.Upsert(Rule{ID: "cold-drop", Type: TypeBalanceDrop, Wallets: []string{"cold"}}); err != nil {
		t.Fatal(err)
	}
	none(t, h.snap(cold(5))) // the drop happened while the rule was off: new baseline, no alert
	one(t, h.snap(cold(4)))
}

func TestStateOfDeletedRulesIsRemoved(t *testing.T) {
	h := newHarness(t, coldDrop, `{id: src-down, type: source_down, minutes: 10}`)
	h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 1}))
	if h.stateRows("cold-drop") == 0 {
		t.Fatal("expected a baseline row")
	}
	if err := h.rules.Delete("cold-drop"); err != nil {
		t.Fatal(err)
	}
	h.snap(synced("cold", core.KindHardware, hold{sym: "BTC", amt: 1}))
	if n := h.stateRows("cold-drop"); n != 0 {
		t.Fatalf("deleted rule keeps %d state rows", n)
	}
}

// ---- price ----

func TestPriceAboveCrossingHysteresisAndCooldown(t *testing.T) {
	h := newHarness(t, `{id: btc-100k, type: price, asset: BTC, above: 100000}`)
	p := func(v float64) []Notification { return h.snapP(map[string]float64{"BTC": v}) }

	none(t, p(99000)) // below: armed
	n := one(t, p(100500))
	if n.RuleID != "btc-100k" || n.Priority != 3 || n.Click != "http://cryptkeeper.test:8787/" {
		t.Fatalf("notification: %+v", n)
	}
	mustContain(t, n.Body, "BTC", "above", "$100,000", "$100,500")

	none(t, p(101000)) // still above
	none(t, p(99500))  // back under, but within 2%: not re-armed
	none(t, p(100500)) // so this is not a new crossing
	none(t, p(97000))  // 3% back: re-armed

	h.clk.Advance(time.Hour)
	none(t, p(100200)) // a crossing inside the 6h cooldown is dropped...
	none(t, p(100400)) // ...not deferred
	h.clk.Advance(7 * time.Hour)
	none(t, p(100400)) // needs a fresh retreat and crossing
	none(t, p(97000))
	one(t, p(100300))
}

func TestPriceHysteresisWithoutCooldown(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rule         string
		start, cross float64
		shallow      float64 // retreat inside the 2% band: must not re-arm
		deep         float64 // retreat beyond it: re-arms
	}{
		{"above", `{id: a, type: price, asset: BTC, above: 100000, cooldown: 0s}`, 99000, 100500, 99000, 97999},
		{"below", `{id: b, type: price, asset: BTC, below: 80, cooldown: 0s}`, 90, 79, 81, 81.7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.rule)
			p := func(v float64) []Notification { return h.snapP(map[string]float64{"BTC": v}) }
			none(t, p(tc.start))
			one(t, p(tc.cross))
			none(t, p(tc.shallow))
			none(t, p(tc.cross)) // inside the band: not re-armed, so no new crossing
			none(t, p(tc.deep))
			one(t, p(tc.cross)) // re-armed, and no cooldown in the way
		})
	}
}

func TestPriceBelowMirror(t *testing.T) {
	h := newHarness(t, `{id: sol-low, type: price, asset: SOL, below: 80}`)
	p := func(v float64) []Notification { return h.snapP(map[string]float64{"SOL": v}) }
	none(t, p(90))
	n := one(t, p(79))
	mustContain(t, n.Body, "SOL", "below", "$80.00", "$79.00")
	none(t, p(78))
	none(t, p(81))   // within 2% of 80 (81.6)
	none(t, p(81.7)) // re-armed
	h.clk.Advance(7 * time.Hour)
	one(t, p(79.9))
}

func TestPriceFirstSightBeyondThresholdDoesNotFire(t *testing.T) {
	h := newHarness(t, `{id: btc-100k, type: price, asset: BTC, above: 100000, cooldown: 0s}`)
	p := func(v float64) []Notification { return h.snapP(map[string]float64{"BTC": v}) }
	none(t, p(105000)) // already above when the rule appeared
	none(t, p(106000))
	none(t, p(99000)) // under, but not by 2%
	none(t, p(100100))
	none(t, p(97000)) // re-armed now
	one(t, p(100100)) // zero cooldown: fires on the very next crossing
}

func TestPriceSkipsMissingAndStalePricesWithoutLosingState(t *testing.T) {
	h := newHarness(t, `{id: btc-100k, type: price, asset: BTC, above: 100000}`)
	none(t, h.snapP(map[string]float64{"BTC": 99000})) // armed
	none(t, h.snap())                                  // no price at all
	s := snapshot(h.clk.Now(), nil)
	s.Prices["BTC"] = core.Price{USD: 101000, Stale: true}
	none(t, h.run(s)) // stale price: skipped
	one(t, h.snapP(map[string]float64{"BTC": 101000}))
}

func TestPriceThresholdChangeResetsState(t *testing.T) {
	h := newHarness(t, `{id: btc, type: price, asset: BTC, above: 100000}`)
	none(t, h.snapP(map[string]float64{"BTC": 99000})) // armed for 100000
	if _, err := h.rules.Upsert(Rule{ID: "btc", Type: TypePrice, Asset: "BTC", Above: 98000}); err != nil {
		t.Fatal(err)
	}
	none(t, h.snapP(map[string]float64{"BTC": 99000})) // already beyond the new threshold: first sight, no alert
	none(t, h.snapP(map[string]float64{"BTC": 99500}))
}

// ---- portfolio_move ----

const ltMove = `{id: lt-move, type: portfolio_move, portfolio: lt, pct: 10, window: 24h}`

func TestPortfolioMoveDownAndUp(t *testing.T) {
	h := newHarness(t, ltMove)
	h.hist["lt"] = 10000
	h.pf["lt"] = 8900

	n := one(t, h.snap())
	if n.Priority != 4 || n.RuleID != "lt-move" {
		t.Fatalf("notification: %+v", n)
	}
	mustContain(t, n.Body, "Long-term", "−11%", "24h", "$10,000", "$8,900")
	mustContain(t, n.Click, "http://cryptkeeper.test:8787/p/lt")
	if len(h.histCalls) != 1 || h.histCalls[0].id != "lt" || !h.histCalls[0].at.Equal(t0.Add(-24*time.Hour)) {
		t.Fatalf("history queried as %+v", h.histCalls)
	}

	none(t, h.snap()) // cooldown
	h.clk.Advance(7 * time.Hour)
	one(t, h.snap()) // still moved after the cooldown

	h.clk.Advance(7 * time.Hour)
	h.pf["lt"] = 11200
	n = one(t, h.snap())
	if n.Priority != 3 {
		t.Fatalf("an upward move is priority 3, got %d", n.Priority)
	}
	mustContain(t, n.Body, "+12%")
}

func TestPortfolioMoveThresholdAndSkips(t *testing.T) {
	h := newHarness(t, ltMove)
	h.hist["lt"] = 10000
	h.pf["lt"] = 9100
	none(t, h.snap()) // 9% < 10%
	h.pf["lt"] = 9000
	one(t, h.snap()) // exactly 10% counts

	h = newHarness(t, ltMove)
	h.hist["lt"] = 10000
	none(t, h.snap()) // valuer: not ok (stale data)
	h.pf["lt"] = 5000
	delete(h.hist, "lt")
	none(t, h.snap()) // no history yet
	h.hist["lt"] = 0
	none(t, h.snap()) // zero past value: no percentage
}

func TestPortfolioMoveAllDefaultsAndMissingValuers(t *testing.T) {
	h := newHarness(t, `{id: all-move, type: portfolio_move, pct: 5}`)
	h.hist["all"] = 100
	h.pf["all"] = 90
	n := one(t, h.snap())
	mustContain(t, n.Body, "All holdings", "−10%", "24h")

	h.eng.Portfolio, h.eng.History = nil, nil
	h.clk.Advance(7 * time.Hour)
	none(t, h.snap()) // no valuers wired: skip quietly
}

func TestPortfolioMoveUsesRuleName(t *testing.T) {
	h := newHarness(t, `{id: lt-move, type: portfolio_move, portfolio: lt, pct: 10, name: Retirement swing}`)
	h.hist["lt"], h.pf["lt"] = 100, 80
	n := one(t, h.snap())
	if n.Title != "Retirement swing" {
		t.Fatalf("title %q", n.Title)
	}
}

// ---- source_down ----

const srcDown = `{id: src-down, type: source_down, minutes: 60}`

func TestSourceDownFiresAfterThresholdOncePerOutage(t *testing.T) {
	h := newHarness(t, srcDown)
	stale := wallet("cold", core.KindHardware, portfolio.StatusStale)
	ok := synced("cold", core.KindHardware)

	none(t, h.snap(stale))
	h.clk.Advance(59 * time.Minute)
	none(t, h.snap(stale))
	h.clk.Advance(2 * time.Minute)
	n := one(t, h.snap(stale))
	if n.RuleID != "src-down" || n.Priority != 4 {
		t.Fatalf("notification: %+v", n)
	}
	mustContain(t, n.Body, "Cold storage", "stale", "61 min")
	if n.Click != "http://cryptkeeper.test:8787/w/cold" {
		t.Fatalf("click %q", n.Click)
	}

	h.clk.Advance(5 * time.Hour)
	none(t, h.snap(stale)) // same outage: never again

	none(t, h.snap(ok)) // recovery
	none(t, h.snap(stale))
	h.clk.Advance(61 * time.Minute)
	one(t, h.snap(stale)) // a new outage, a new alert
}

func TestSourceDownErrorStatusAndRecoveryResetsClock(t *testing.T) {
	h := newHarness(t, srcDown)
	bad := wallet("hot", core.KindHot, portfolio.StatusError)
	good := synced("hot", core.KindHot)
	none(t, h.snap(bad))
	h.clk.Advance(45 * time.Minute)
	none(t, h.snap(good)) // recovered before the threshold
	none(t, h.snap(bad))
	h.clk.Advance(45 * time.Minute)
	none(t, h.snap(bad)) // 45 min into the second outage, not 90
	h.clk.Advance(16 * time.Minute)
	n := one(t, h.snap(bad))
	mustContain(t, n.Body, "Hot wallet", "failing")
}

func TestSourceDownPendingIsNotAnOutageAndGoneWalletsAreForgotten(t *testing.T) {
	h := newHarness(t, srcDown)
	pending := wallet("cold", core.KindHardware, portfolio.StatusPending)
	none(t, h.snap(pending))
	h.clk.Advance(3 * time.Hour)
	none(t, h.snap(pending))

	stale := wallet("cold", core.KindHardware, portfolio.StatusStale)
	none(t, h.snap(stale))
	h.clk.Advance(30 * time.Minute)
	none(t, h.snap()) // the source was removed
	none(t, h.snap(stale))
	h.clk.Advance(45 * time.Minute) // 75 min since the first sighting, 45 since the second
	none(t, h.snap(stale))
}

func TestSourceDownSeveralWallets(t *testing.T) {
	h := newHarness(t, `{id: src-down, type: source_down, minutes: 10}`)
	a := wallet("cold", core.KindHardware, portfolio.StatusStale)
	b := wallet("hot", core.KindHot, portfolio.StatusError)
	c := synced("vault", core.KindHardware)
	h.snap(a, b, c)
	h.clk.Advance(11 * time.Minute)
	got := h.snap(a, b, c)
	if len(got) != 2 {
		t.Fatalf("want one notification per failing wallet, got %d: %+v", len(got), got)
	}
}

// ---- large_activity ----

func usd(v float64) *float64 { return &v }

func send(id, source string, usdVal *float64) core.Tx {
	return core.Tx{
		ID: core.TxID(source, id), SourceID: source, ExternalID: id,
		Time: t0.Add(-time.Minute), Kind: core.TxSend, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "BTC", Symbol: "BTC", Amount: -0.5, USD: usdVal}},
	}
}

var kinds = map[string]core.Kind{"cold": core.KindHardware, "hot": core.KindHot, "ex": core.KindExchange, "man": core.KindManual}

func TestLargeActivityFilters(t *testing.T) {
	mod := func(f func(*core.Tx)) core.Tx {
		tx := send("t", "cold", usd(-1500))
		f(&tx)
		return tx
	}
	cases := []struct {
		name string
		rule string
		tx   core.Tx
		want bool
	}{
		{"hardware send above", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "cold", usd(-1500)), true},
		{"hot send exactly at min", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "hot", usd(-1000)), true},
		{"just under", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "hot", usd(-999.99)), false},
		{"positive usd leg uses magnitude", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "cold", usd(1500)), true},
		{"exchange wallet", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "ex", usd(-1500)), false},
		{"manual wallet", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "man", usd(-1500)), false},
		{"unknown wallet kind", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "mystery", usd(-1500)), false},
		{"transfer", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, mod(func(tx *core.Tx) { tx.Kind = core.TxTransfer }), false},
		{"receive", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, mod(func(tx *core.Tx) { tx.Kind = core.TxReceive }), false},
		{"swap", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, mod(func(tx *core.Tx) { tx.Kind = core.TxSwap }), false},
		{"failed send", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, mod(func(tx *core.Tx) { tx.Status = core.TxFailed }), false},
		{"pending send", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, mod(func(tx *core.Tx) { tx.Status = core.TxPending }), true},
		{"unknown usd skipped", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, send("t", "cold", nil), false},
		{"unknown usd with minUsd 0", `{id: any, type: large_activity, cooldown: 0s}`, send("t", "cold", nil), true},
		{"tiny send with minUsd 0", `{id: any, type: large_activity, cooldown: 0s}`, send("t", "cold", usd(-0.01)), true},
		{"failed with minUsd 0", `{id: any, type: large_activity, cooldown: 0s}`, mod(func(tx *core.Tx) { tx.Status = core.TxFailed }), false},
		{"legs add up", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, mod(func(tx *core.Tx) {
			tx.Legs = []core.Leg{{AssetKey: "BTC", Symbol: "BTC", Amount: -0.1, USD: usd(-600)}, {AssetKey: "ETH", Symbol: "ETH", Amount: -1, USD: usd(-600)}}
		}), true},
		{"an old backfilled tx is not news", `{id: big, type: large_activity, minUsd: 1000, cooldown: 0s}`, mod(func(tx *core.Tx) { tx.Time = t0.Add(-30 * 24 * time.Hour) }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.rule)
			got := h.activity([]core.Tx{tc.tx}, kinds)
			if tc.want {
				n := one(t, got)
				if n.Priority != 4 || n.RuleID == "" {
					t.Fatalf("notification: %+v", n)
				}
			} else {
				none(t, got)
			}
		})
	}
}

func TestLargeActivityContentAndLinks(t *testing.T) {
	h := newHarness(t, `{id: big-send, type: large_activity, minUsd: 1000}`)
	n := one(t, h.activity([]core.Tx{send("a", "cold", usd(-1500))}, kinds))
	mustContain(t, n.Body, "Sent", "0.5 BTC", "$1,500", "Cold storage")
	if n.Click != "http://cryptkeeper.test:8787/activity" {
		t.Fatalf("click %q", n.Click)
	}
}

func TestLargeActivityGroupsABatchIntoOneNotification(t *testing.T) {
	h := newHarness(t, `{id: big-send, type: large_activity, minUsd: 100}`)
	txs := []core.Tx{send("a", "cold", usd(-1500)), send("b", "hot", usd(-2500)), send("c", "cold", usd(-50)), send("d", "cold", usd(-900))}
	n := one(t, h.activity(txs, kinds))
	if lines := strings.Split(n.Body, "\n"); len(lines) != 3 {
		t.Fatalf("want 3 lines (the $50 send is below the minimum), got %q", n.Body)
	}
	mustContain(t, n.Body, "Cold storage", "Hot wallet")
}

func TestLargeActivityCapsLongBatches(t *testing.T) {
	h := newHarness(t, `{id: big-send, type: large_activity}`)
	var txs []core.Tx
	for i := 0; i < 9; i++ {
		txs = append(txs, send(string(rune('a'+i)), "cold", usd(-100)))
	}
	n := one(t, h.activity(txs, kinds))
	mustContain(t, n.Body, "+4 more")
}

func TestLargeActivityCooldownAndDedupe(t *testing.T) {
	h := newHarness(t, `{id: big-send, type: large_activity, minUsd: 100}`)
	a := send("a", "cold", usd(-1500))
	one(t, h.activity([]core.Tx{a}, kinds))
	none(t, h.activity([]core.Tx{a}, kinds)) // the same tx delivered twice

	b := send("b", "cold", usd(-1500))
	none(t, h.activity([]core.Tx{b}, kinds)) // inside the 6h cooldown
	h.clk.Advance(7 * time.Hour)
	c := send("c", "cold", usd(-1500))
	c.Time = h.clk.Now().Add(-time.Minute)
	one(t, h.activity([]core.Tx{c}, kinds))
}

func TestLargeActivityEmptyInputs(t *testing.T) {
	h := newHarness(t, `{id: big-send, type: large_activity}`)
	none(t, h.activity(nil, kinds))
	none(t, h.activity([]core.Tx{send("a", "cold", usd(-5))}, nil))
}

// ---- redaction ----

var allowedInRedacted = regexp.MustCompile(`[+−-]?\d+(\.\d+)?%|\b\d+[hdm]\b`)

func TestRedactedBodiesHaveNoAmounts(t *testing.T) {
	h := newHarness(t,
		`{id: cold-drop, type: balance_drop, wallets: [cold]}`,
		`{id: btc-100k, type: price, asset: BTC, above: 100000}`,
		ltMove,
		`{id: big-send, type: large_activity, minUsd: 100}`,
	)
	h.eng.Redact = true
	h.hist["lt"], h.pf["lt"] = 10000, 8900

	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snapP(map[string]float64{"BTC": 99000}, cold(7.3))
	h.clk.Advance(7 * time.Hour) // let the portfolio_move cooldown from the first run lapse
	got := h.snapP(map[string]float64{"BTC": 100250}, cold(6.424))
	got = append(got, h.activity([]core.Tx{send("a", "cold", usd(-1500))}, kinds)...)

	bodies := map[string]string{}
	for _, n := range got {
		bodies[n.RuleID] = n.Body
	}
	want := map[string]string{
		"cold-drop": "Cold storage: BTC balance dropped 12%",
		"btc-100k":  "BTC crossed above your alert",
		"lt-move":   "Long-term moved −11% in 24h",
		"big-send":  "Sent BTC from Cold storage",
	}
	for id, w := range want {
		if bodies[id] != w {
			t.Errorf("%s body = %q, want %q", id, bodies[id], w)
		}
	}
	for _, n := range got {
		if rest := allowedInRedacted.ReplaceAllString(n.Title+"\n"+n.Body, ""); regexp.MustCompile(`\d`).MatchString(rest) {
			t.Errorf("%s leaks digits beyond percentages and windows: %q", n.RuleID, n.Title+" / "+n.Body)
		}
		if strings.ContainsAny(n.Body, "$") {
			t.Errorf("%s leaks a currency amount: %q", n.RuleID, n.Body)
		}
	}
}

func TestUnredactedBodiesCarryAmounts(t *testing.T) {
	h := newHarness(t, coldDrop)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(7.3))
	n := one(t, h.snap(cold(6.424)))
	mustContain(t, n.Body, "12%", "7.3", "6.424", "BTC")
}

func TestLabelsOverride(t *testing.T) {
	h := newHarness(t, coldDrop)
	h.eng.Labels = func(id string) string {
		if id == "cold" {
			return "Safe in the closet"
		}
		return ""
	}
	h.eng.Redact = true
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(2))
	n := one(t, h.snap(cold(1)))
	mustContain(t, n.Body, "Safe in the closet: BTC balance dropped 50%")
}

func TestNoBaseURLMeansNoClick(t *testing.T) {
	h := newHarness(t, coldDrop, `{id: btc, type: price, asset: BTC, above: 10}`, srcDown)
	h.eng.BaseURL = ""
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snapP(map[string]float64{"BTC": 5}, cold(2))
	first := h.snapP(map[string]float64{"BTC": 11}, cold(1))
	if len(first) != 2 {
		t.Fatalf("want the drop and the price alert, got %+v", first)
	}
	for _, n := range first {
		if n.Click != "" {
			t.Fatalf("click %q without a base URL", n.Click)
		}
	}
	h.eng.BaseURL = "http://cryptkeeper.test:8787/" // a trailing slash must not double up
	h.clk.Advance(7 * time.Hour)
	h.snapP(map[string]float64{"BTC": 5}, cold(1))
	h.snapP(map[string]float64{"BTC": 5}, cold(1))
	got := h.snapP(map[string]float64{"BTC": 12}, cold(0.5))
	if len(got) != 2 {
		t.Fatalf("want the drop and the price alert, got %+v", got)
	}
	for _, n := range got {
		if !strings.HasPrefix(n.Click, "http://cryptkeeper.test:8787/") || strings.Contains(n.Click, "8787//") {
			t.Fatalf("click %q", n.Click)
		}
	}
}

// ---- delivery, log, persistence ----

func TestSendErrorsAreLoggedAndDoNotStopEvaluation(t *testing.T) {
	h := newHarness(t, coldDrop, `{id: btc, type: price, asset: BTC, above: 100}`)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snapP(map[string]float64{"BTC": 90}, cold(2))
	h.n.setErr(errors.New("ntfy: HTTP 503: down"))

	got := h.snapP(map[string]float64{"BTC": 110}, cold(1)) // both rules fire, both sends fail
	if len(got) != 2 {
		t.Fatalf("a failing notifier must not stop the second rule: %+v", got)
	}
	log, err := h.eng.Recent(h.ctx(), 10)
	if err != nil || len(log) != 2 {
		t.Fatalf("%v %+v", err, log)
	}
	for _, e := range log {
		if e.Err != "ntfy: HTTP 503: down" || e.Body == "" || e.Title == "" || e.RuleID == "" || !e.Time.Equal(t0) {
			t.Fatalf("log entry %+v", e)
		}
	}

	// Delivery is at-most-once: the event is consumed, not retried on every snapshot.
	h.n.setErr(nil)
	none(t, h.snapP(map[string]float64{"BTC": 110}, cold(1)))
}

func TestNotifierPanicIsContained(t *testing.T) {
	h := newHarness(t, coldDrop)
	h.n.panicMsg = "boom"
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(2))
	h.snap(cold(1)) // must not panic
	log, err := h.eng.Recent(h.ctx(), 5)
	if err != nil || len(log) != 1 || !strings.Contains(log[0].Err, "boom") {
		t.Fatalf("%v %+v", err, log)
	}
}

func TestNilNotifierIsLoggedNotFatal(t *testing.T) {
	h := newHarness(t, coldDrop)
	h.eng.Notifier = nil
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	h.snap(cold(2))
	h.snap(cold(1))
	log, _ := h.eng.Recent(h.ctx(), 5)
	if len(log) != 1 || log[0].Err == "" {
		t.Fatalf("%+v", log)
	}
	if err := h.eng.Test(h.ctx()); err == nil {
		t.Fatal("Test without a notifier must fail")
	}
}

func TestRecentIsNewestFirstAndLimited(t *testing.T) {
	h := newHarness(t, `{id: any, type: large_activity, cooldown: 0s}`)
	for i, id := range []string{"a", "b", "c", "d"} {
		h.clk.Advance(time.Minute)
		tx := send(id, "cold", usd(-100))
		tx.Time = h.clk.Now()
		h.activity([]core.Tx{tx}, kinds)
		_ = i
	}
	log, err := h.eng.Recent(h.ctx(), 3)
	if err != nil || len(log) != 3 {
		t.Fatalf("%v %+v", err, log)
	}
	for i := 1; i < len(log); i++ {
		if !log[i-1].Time.After(log[i].Time) {
			t.Fatalf("not newest first: %+v", log)
		}
	}
	if !log[0].Time.Equal(t0.Add(4 * time.Minute)) {
		t.Fatalf("newest entry at %v", log[0].Time)
	}
	if got, _ := h.eng.Recent(h.ctx(), 0); len(got) != 0 {
		t.Fatalf("n=0: %+v", got)
	}
	if got, _ := h.eng.Recent(h.ctx(), 100); len(got) != 4 {
		t.Fatalf("n=100: %d", len(got))
	}
}

func TestLogIsPruned(t *testing.T) {
	h := newHarness(t, `{id: any, type: large_activity, cooldown: 0s}`)
	for i := 0; i < logKeep+25; i++ {
		tx := send(string(rune('A'+i%26))+string(rune('a'+i/26)), "cold", usd(-100))
		h.activity([]core.Tx{tx}, kinds)
	}
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM alert_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != logKeep {
		t.Fatalf("%d log rows, want %d", n, logKeep)
	}
}

func TestTestNotification(t *testing.T) {
	h := newHarness(t)
	if err := h.eng.Test(h.ctx()); err != nil {
		t.Fatal(err)
	}
	got := h.fresh()
	n := one(t, got)
	if n.Body == "" || n.Title == "" || n.Click != "http://cryptkeeper.test:8787/alerts" {
		t.Fatalf("%+v", n)
	}
	h.n.setErr(errors.New("nope"))
	if err := h.eng.Test(h.ctx()); err == nil || err.Error() != "nope" {
		t.Fatalf("want the send error, got %v", err)
	}
	log, _ := h.eng.Recent(h.ctx(), 5)
	if len(log) != 2 || log[0].Err != "nope" || log[1].Err != "" || log[0].RuleID != "test" {
		t.Fatalf("%+v", log)
	}
}

func TestStatePersistsAcrossEngines(t *testing.T) {
	h := newHarness(t, coldDrop, `{id: btc, type: price, asset: BTC, above: 100000}`, srcDown)
	cold := func(btc float64) portfolio.WalletView {
		return synced("cold", core.KindHardware, hold{sym: "BTC", amt: btc})
	}
	stale := wallet("hot", core.KindHot, portfolio.StatusStale)

	h.snapP(map[string]float64{"BTC": 99000}, cold(2), stale) // baseline, armed, outage clock
	h.eng = h.newEngine()                                     // "restart"

	h.clk.Advance(61 * time.Minute)
	got := h.snapP(map[string]float64{"BTC": 101000}, cold(1), stale)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.RuleID] = true
	}
	if len(got) != 3 || !ids["cold-drop"] || !ids["btc"] || !ids["src-down"] {
		t.Fatalf("a restart must not lose baselines, armed flags or outage clocks: %+v", got)
	}

	// And what already fired stays fired after another restart.
	h.eng = h.newEngine()
	none(t, h.snapP(map[string]float64{"BTC": 101000}, cold(1), stale))
}

func TestCooldownPersistsAcrossEngines(t *testing.T) {
	h := newHarness(t, `{id: any, type: large_activity}`)
	one(t, h.activity([]core.Tx{send("a", "cold", usd(-100))}, kinds))
	h.eng = h.newEngine()
	none(t, h.activity([]core.Tx{send("b", "cold", usd(-100))}, kinds))
}

func TestEngineWithoutRulesOrDB(t *testing.T) {
	var e Engine
	e.OnSnapshot(context.Background(), portfolio.Snapshot{})
	e.OnActivity(context.Background(), []core.Tx{send("a", "cold", nil)}, kinds)
	if _, err := e.Recent(context.Background(), 5); err == nil {
		t.Fatal("Recent without a DB must error")
	}
}

func TestConcurrentUse(t *testing.T) {
	h := newHarness(t, coldDrop, `{id: any, type: large_activity, cooldown: 0s}`, srcDown)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				h.eng.OnSnapshot(h.ctx(), snapshot(h.clk.Now(), nil, synced("cold", core.KindHardware, hold{sym: "BTC", amt: float64(10 - i)})))
				tx := send(string(rune('a'+g))+string(rune('a'+i)), "cold", usd(-100))
				h.eng.OnActivity(h.ctx(), []core.Tx{tx}, kinds)
				_, _ = h.eng.Recent(h.ctx(), 5)
			}
		}(g)
	}
	wg.Wait()
	if h.n.count() == 0 {
		t.Fatal("expected some notifications")
	}
}
