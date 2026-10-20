package portfolio

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/store"
)

type stubSource struct {
	id   string
	res  core.WalletResult
	err  error
	hits atomic.Int32
}

func (s *stubSource) ID() string { return s.id }
func (s *stubSource) Fetch(context.Context) (core.WalletResult, error) {
	s.hits.Add(1)
	if s.err != nil {
		return core.WalletResult{}, s.err
	}
	return s.res, nil
}

func h(wallet, key string, amt float64) core.Holding {
	return core.Holding{WalletID: wallet, AssetKey: key, Symbol: key, Amount: amt}
}

func fixedPrices(m map[string]float64) PriceFunc {
	return func(_ context.Context, reqs []PriceReq) map[string]core.Price {
		out := map[string]core.Price{}
		for _, r := range reqs {
			if v, ok := m[r.Key]; ok {
				out[r.Key] = core.Price{USD: v, AsOf: time.Now()}
			}
		}
		return out
	}
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "cryptkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestBuildMergesAssetsAndSorts(t *testing.T) {
	cold := &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 0.3)}}}
	cb := &stubSource{id: "coinbase", res: core.WalletResult{Holdings: []core.Holding{h("coinbase", "BTC", 0.1), h("coinbase", "USDC", 500)}}}
	sol := &stubSource{id: "phantom", res: core.WalletResult{Holdings: []core.Holding{
		h("phantom", "SOL", 10),
		{WalletID: "phantom", AssetKey: "spl:SCAM", Symbol: "USDC", Mint: "SCAM", Amount: 1e6}, // unpriced → hidden
		{WalletID: "phantom", AssetKey: "spl:DUST", Symbol: "DUST", Mint: "DUST", Amount: 1},   // $0.10 → hidden
		{WalletID: "phantom", AssetKey: "USDC", Symbol: "USDC", Mint: "EPjF", Amount: 100, Verified: true},
		{WalletID: "phantom", AssetKey: "spl:FAKE", Symbol: "BINBULL", Mint: "FAKE", Amount: 5e11}, // priced but unverified → hidden
	}}}
	a := &Aggregator{
		Wallets: func() []Wallet {
			return []Wallet{
				{ID: "cold", Label: "Cold storage", Type: "bitcoin", Kind: core.KindHardware, Source: cold},
				{ID: "coinbase", Label: "Coinbase", Type: "coinbase", Kind: core.KindExchange, Source: cb},
				{ID: "phantom", Label: "Phantom", Type: "solana", Kind: core.KindHot, Source: sol},
				{ID: "broken", Label: "Broken", Type: "kraken", Kind: core.KindExchange, BuildErr: errors.New("bad key")},
			}
		},
		Prices:       fixedPrices(map[string]float64{"BTC": 60000, "USDC": 1, "SOL": 150, "spl:DUST": 0.1, "spl:FAKE": 0.0000001}),
		HideUnpriced: true, MinUSD: 1,
	}
	s := a.Build(context.Background())
	if !near(s.TotalUSD, 0.4*60000+600+1500) {
		t.Fatalf("total %v", s.TotalUSD)
	}
	if s.Assets[0].AssetKey != "BTC" || !near(s.Assets[0].Amount, 0.4) || len(s.Assets[0].HeldIn) != 2 {
		t.Fatalf("btc row %+v", s.Assets[0])
	}
	var usdc AssetRow
	for _, ar := range s.Assets {
		if ar.AssetKey == "USDC" {
			usdc = ar
		}
		if ar.AssetKey == "spl:SCAM" {
			t.Fatal("scam token must not merge into assets")
		}
	}
	if !near(usdc.Amount, 600) {
		t.Fatalf("USDC across Coinbase+Solana should merge: %+v", usdc)
	}
	var shares float64
	for _, ar := range s.Assets {
		shares += ar.Share
	}
	if !near(shares, 1) {
		t.Fatalf("shares sum %v", shares)
	}
	for _, w := range s.Wallets {
		switch w.ID {
		case "phantom":
			if len(w.HiddenRows) != 3 || len(w.Rows) != 2 {
				t.Fatalf("phantom rows %d hidden %d", len(w.Rows), len(w.HiddenRows))
			}
		case "broken":
			if w.Status != StatusError || w.Err != "bad key" {
				t.Fatalf("broken %+v", w)
			}
		}
	}
	if s.Wallets[0].ID != "cold" {
		t.Fatalf("wallets sorted by value: first %q", s.Wallets[0].ID)
	}
}

func TestStaleFallbackFromLastGood(t *testing.T) {
	db := openDB(t)
	src := &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 1)}}}
	now := time.Now()
	a := &Aggregator{
		Wallets:  func() []Wallet { return []Wallet{{ID: "cold", Label: "Cold", Type: "bitcoin", Source: src}} },
		Prices:   fixedPrices(map[string]float64{"BTC": 100}),
		DB:       db,
		StaleMax: 24 * time.Hour,
		Now:      func() time.Time { return now },
	}
	if s := a.Build(context.Background()); s.Wallets[0].Status != StatusSynced {
		t.Fatal("first build should sync")
	}
	src.err = errors.New("esplora down")
	s := a.Build(context.Background())
	w := s.Wallets[0]
	if w.Status != StatusStale || !s.HasStale || !near(s.TotalUSD, 100) || w.Err != "esplora down" {
		t.Fatalf("want stale last-good, got %+v total %v", w, s.TotalUSD)
	}
	now = now.Add(25 * time.Hour)
	s = a.Build(context.Background())
	if s.Wallets[0].Status != StatusError || s.TotalUSD != 0 {
		t.Fatalf("past StaleMax should be error: %+v", s.Wallets[0])
	}
}

func TestPanickingSourceIsAnError(t *testing.T) {
	a := &Aggregator{Wallets: func() []Wallet { return []Wallet{{ID: "p", Label: "P", Source: panicSource{}}} }}
	s := a.Build(context.Background())
	if s.Wallets[0].Status != StatusError {
		t.Fatalf("%+v", s.Wallets[0])
	}
}

type panicSource struct{}

func (panicSource) ID() string { return "p" }
func (panicSource) Fetch(context.Context) (core.WalletResult, error) {
	panic("boom")
}

func TestBrokerLatestOnlyAndUnsubscribe(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	b.Publish(Snapshot{TotalUSD: 1})
	b.Publish(Snapshot{TotalUSD: 2})
	if got := <-ch; got.TotalUSD != 2 {
		t.Fatalf("want latest snapshot, got %v", got.TotalUSD)
	}
	cancel()
	cancel() // idempotent
	if b.Subscribers() != 0 {
		t.Fatal("subscriber leaked")
	}
	b.Publish(Snapshot{TotalUSD: 3}) // must not block or panic
}

func TestHistorySeriesAndCompaction(t *testing.T) {
	db := openDB(t)
	hist := &History{DB: db, Interval: 30 * time.Minute}
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(ts time.Time, btc float64) Snapshot {
		return Snapshot{UpdatedAt: ts, Prices: map[string]core.Price{"BTC": {USD: btc}, "SOL": {USD: 100}},
			Wallets: []WalletView{
				{ID: "cold", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("cold", "BTC", 1)}}},
				{ID: "phantom", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("phantom", "SOL", 2)}}},
				{ID: "dead", Status: StatusError},
			}}
	}
	for i := range 4 {
		if err := hist.Record(ctx, mk(base.Add(time.Duration(i)*20*time.Minute), float64(1000+i))); err != nil {
			t.Fatal(err)
		}
	}
	// 20-minute spacing with a 30-minute interval: rows at 0 and 40 min only.
	pts, err := hist.Series(ctx, time.Time{}, nil, nil)
	if err != nil || len(pts) != 2 || pts[1].V != 1002+200 {
		t.Fatalf("pts %+v %v", pts, err)
	}
	onlyBTC, _ := hist.Series(ctx, time.Time{}, func(w, k string) bool { return k == "BTC" }, nil)
	if onlyBTC[0].V != 1000 {
		t.Fatalf("filtered %+v", onlyBTC)
	}
	// Compaction: 3 rows on day 1, one on day 2, all old.
	db.Exec(`DELETE FROM history`)
	hist.last = time.Time{}
	for _, ts := range []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour), base.Add(26 * time.Hour)} {
		hist.last = time.Time{}
		hist.Record(ctx, mk(ts, 1))
	}
	if err := hist.Compact(ctx, base.Add(90*24*time.Hour), 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n)
	if n != 2 {
		t.Fatalf("want 1 row per day after compaction, got %d", n)
	}
}

func TestChangeFor(t *testing.T) {
	now := time.Now()
	pts := []Point{{T: now.Add(-7 * 24 * time.Hour), V: 100}, {T: now, V: 110}}
	c := ChangeFor("1W", pts, now)
	if !c.Valid || c.Abs != 10 || !near(c.Pct, 10) || c.Label != "past week" {
		t.Fatalf("%+v", c)
	}
	short := []Point{{T: now.Add(-time.Hour), V: 100}, {T: now, V: 110}}
	if ChangeFor("1W", short, now).Valid {
		t.Fatal("history shorter than the window must be invalid")
	}
	if !ChangeFor("All", short, now).Valid {
		t.Fatal("All is valid with 2 points")
	}
}

func TestRefresherForceThrottleAndPublish(t *testing.T) {
	src := &stubSource{id: "a", res: core.WalletResult{Holdings: []core.Holding{h("a", "BTC", 1)}}}
	b := NewBroker()
	ch, cancel := b.Subscribe()
	defer cancel()
	r := &Refresher{
		Agg:              &Aggregator{Wallets: func() []Wallet { return []Wallet{{ID: "a", Label: "A", Source: src}} }, Prices: fixedPrices(map[string]float64{"BTC": 5})},
		Broker:           b,
		Interval:         time.Hour,
		MinForceInterval: time.Minute,
	}
	if _, err := r.Force(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := <-ch; s.TotalUSD != 5 {
		t.Fatalf("published %v", s.TotalUSD)
	}
	if _, err := r.Force(context.Background()); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("want ErrTooSoon, got %v", err)
	}
	if s, ok := r.Latest(); !ok || s.TotalUSD != 5 || r.LastSuccess().IsZero() {
		t.Fatal("latest not stored")
	}
}

func TestDownsample(t *testing.T) {
	var pts []Point
	for i := range 1000 {
		pts = append(pts, Point{V: float64(i)})
	}
	d := Downsample(pts, 500)
	if len(d) != 500 || d[0].V != 0 || d[499].V != 999 {
		t.Fatalf("len %d first %v last %v", len(d), d[0].V, d[len(d)-1].V)
	}
}

func TestSeriesStartsWhenEveryLiveWalletHasHistory(t *testing.T) {
	db := openDB(t)
	hist := &History{DB: db, Interval: time.Minute}
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	price := map[string]core.Price{"BTC": {USD: 100}, "SOL": {USD: 10}}
	cold := WalletView{ID: "cold", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("cold", "BTC", 1), ValueUSD: 100}}}
	sol := WalletView{ID: "sol", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("sol", "SOL", 50), ValueUSD: 500}}}
	gone := WalletView{ID: "gone", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("gone", "BTC", 9)}}}
	// t0: cold + a wallet later removed; t1: sol added; t2: steady.
	hist.Record(ctx, Snapshot{UpdatedAt: base, Prices: price, Wallets: []WalletView{cold, gone}})
	hist.Record(ctx, Snapshot{UpdatedAt: base.Add(time.Hour), Prices: price, Wallets: []WalletView{cold, sol}})
	hist.Record(ctx, Snapshot{UpdatedAt: base.Add(2 * time.Hour), Prices: price, Wallets: []WalletView{cold, sol}})
	cur := Snapshot{UpdatedAt: base.Add(3 * time.Hour), Wallets: []WalletView{cold, sol}}
	pts, err := hist.Series(ctx, time.Time{}, nil, &cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 || !pts[0].T.Equal(base.Add(time.Hour)) {
		t.Fatalf("series must start when sol first appears: %+v", pts)
	}
	for _, p := range pts {
		if p.V != 600 {
			t.Fatalf("flat portfolio must chart flat (no add/remove cliffs): %+v", pts)
		}
	}
}

func TestPriceChanges(t *testing.T) {
	db := openDB(t)
	hist := &History{DB: db, Interval: time.Minute}
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	row := func(ts time.Time, btc float64) {
		hist.last = time.Time{}
		hist.Record(ctx, Snapshot{UpdatedAt: ts, Prices: map[string]core.Price{"BTC": {USD: btc}},
			Wallets: []WalletView{{ID: "w", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("w", "BTC", 1)}}}}})
	}
	row(now.Add(-week-30*time.Hour), 50) // outside tolerance (±16.8h)
	row(now.Add(-week+2*time.Hour), 100) // nearest within tolerance
	got, err := hist.PriceChanges(ctx, map[string]float64{"BTC": 110, "ETH": 3000}, week, now)
	if err != nil || !near(got["BTC"], 10) {
		t.Fatalf("got %v %v", got, err)
	}
	if _, ok := got["ETH"]; ok {
		t.Fatal("asset unpriced in history must be absent")
	}
	none, _ := hist.PriceChanges(ctx, map[string]float64{"BTC": 110}, 365*24*time.Hour, now)
	if len(none) != 0 {
		t.Fatalf("no history a year back: %v", none)
	}
}

// TestPriceSeries: each asset's recorded prices since a time, oldest first; a row an asset
// wasn't priced in gives it no point.
func TestPriceSeries(t *testing.T) {
	db := openDB(t)
	hist := &History{DB: db, Interval: time.Minute}
	ctx := context.Background()
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	row := func(ts time.Time, prices map[string]core.Price) {
		hist.last = time.Time{}
		if err := hist.Record(ctx, Snapshot{UpdatedAt: ts, Prices: prices,
			Wallets: []WalletView{{ID: "w", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("w", "BTC", 1)}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	row(now.Add(-9*24*time.Hour), map[string]core.Price{"BTC": {USD: 50}}) // before the window
	row(now.Add(-6*24*time.Hour), map[string]core.Price{"BTC": {USD: 100}, "SOL": {USD: 20}})
	row(now.Add(-time.Hour), map[string]core.Price{"BTC": {USD: 110}})
	got, err := hist.PriceSeries(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	btc := got["BTC"]
	if len(btc) != 2 || btc[0].V != 100 || btc[1].V != 110 || !btc[1].T.Equal(now.Add(-time.Hour)) {
		t.Errorf("BTC = %+v, want 100 six days ago then 110 an hour ago", btc)
	}
	if sol := got["SOL"]; len(sol) != 1 || sol[0].V != 20 {
		t.Errorf("SOL = %+v, want the one row it was priced in", sol)
	}
}

func TestValueAt(t *testing.T) {
	db := openDB(t)
	hist := &History{DB: db, Interval: time.Minute}
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hist.Record(ctx, Snapshot{UpdatedAt: base, Prices: map[string]core.Price{"BTC": {USD: 100}, "SOL": {USD: 10}},
		Wallets: []WalletView{{ID: "w", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("w", "BTC", 2)}, {Holding: h("w", "SOL", 5)}}}}})
	v, ok, err := hist.ValueAt(ctx, base.Add(20*time.Minute), time.Hour, nil)
	if err != nil || !ok || v != 250 {
		t.Fatalf("%v %v %v", v, ok, err)
	}
	v, _, _ = hist.ValueAt(ctx, base, time.Hour, func(_, k string) bool { return k == "SOL" })
	if v != 50 {
		t.Fatalf("filtered %v", v)
	}
	if _, ok, _ := hist.ValueAt(ctx, base.Add(5*time.Hour), time.Hour, nil); ok {
		t.Fatal("no row within tolerance")
	}
}

func TestSeriesSkipsPartialRows(t *testing.T) {
	db := openDB(t)
	hist := &History{DB: db, Interval: time.Minute}
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	price := map[string]core.Price{"BTC": {USD: 100}, "SOL": {USD: 10}}
	cold := WalletView{ID: "cold", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("cold", "BTC", 1), ValueUSD: 100}}}
	sol := WalletView{ID: "sol", Status: StatusSynced, Rows: []HoldingRow{{Holding: h("sol", "SOL", 50), ValueUSD: 500}}}
	solDown := WalletView{ID: "sol", Status: StatusError}
	// t1: sol's fetch failed, so the row holds cold only.
	hist.Record(ctx, Snapshot{UpdatedAt: base, Prices: price, Wallets: []WalletView{cold, sol}})
	hist.Record(ctx, Snapshot{UpdatedAt: base.Add(time.Hour), Prices: price, Wallets: []WalletView{cold, solDown}})
	hist.Record(ctx, Snapshot{UpdatedAt: base.Add(2 * time.Hour), Prices: price, Wallets: []WalletView{cold, sol}})
	cur := Snapshot{UpdatedAt: base.Add(3 * time.Hour), Wallets: []WalletView{cold, sol}}
	pts, err := hist.Series(ctx, time.Time{}, nil, &cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 || !pts[1].T.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("the partial row must be left out, not charted as a dip: %+v", pts)
	}
	for _, p := range pts {
		if p.V != 600 {
			t.Fatalf("dip: %+v", pts)
		}
	}
}

func TestStableShare(t *testing.T) {
	s := Snapshot{Assets: []AssetRow{
		{AssetKey: "BTC", Symbol: "BTC", ValueUSD: 600},
		{AssetKey: "USDC", Symbol: "USDC", ValueUSD: 300},
		{AssetKey: "spl:dai", Symbol: "DAI", ValueUSD: 100},
		{AssetKey: "spl:junk", Symbol: "JUNK", ValueUSD: 0},
	}}
	share, ok := s.StableShare()
	if !ok || share < 0.3999 || share > 0.4001 {
		t.Fatalf("share=%v ok=%v", share, ok)
	}
	if _, ok := (Snapshot{}).StableShare(); ok {
		t.Fatal("nothing valued must not report a share")
	}
}

func TestYearToDateRange(t *testing.T) {
	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	if got := RangeStart("YTD", now); !got.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("RangeStart(YTD) = %v", got)
	}
	if !RangeStart("All", now).IsZero() || !RangeStart("1W", now).Equal(now.AddDate(0, 0, -7)) {
		t.Fatal("other ranges changed")
	}
	// Daily rows from Jan 1 cover the window; rows starting Jan 3 do not.
	var pts []Point
	for d := RangeStart("YTD", now); d.Before(now); d = d.AddDate(0, 0, 1) {
		pts = append(pts, Point{T: d.Add(23 * time.Hour), V: 100 + float64(len(pts))})
	}
	if c := ChangeFor("YTD", pts, now); !c.Valid || c.Label != "this year" {
		t.Fatalf("%+v", c)
	}
	// The window may start up to a tenth late (about a week here), not later.
	if c := ChangeFor("YTD", pts[10:], now); c.Valid {
		t.Fatalf("history from Jan 11 must not count as the year: %+v", c)
	}
}

func TestWalletStableShare(t *testing.T) {
	w := WalletView{Rows: []HoldingRow{
		{Holding: core.Holding{AssetKey: "SOL", Symbol: "SOL"}, ValueUSD: 300},
		{Holding: core.Holding{AssetKey: "USDC", Symbol: "USDC"}, ValueUSD: 100},
	}}
	if share, ok := w.StableShare(); !ok || share < 0.2499 || share > 0.2501 {
		t.Fatalf("share=%v ok=%v", share, ok)
	}
	if _, ok := (WalletView{}).StableShare(); ok {
		t.Fatal("an empty wallet has no share")
	}
}
