package prices_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"crypto-portfolio/internal/httpx"
	"crypto-portfolio/internal/prices"
	"crypto-portfolio/internal/store"
)

const dayFmt = "2006-01-02"

var histEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// closeFor is a deterministic fake daily close, unique per day.
func closeFor(d time.Time) float64 {
	return 1000 + float64(d.UTC().Sub(histEpoch)/(24*time.Hour)) + 0.5
}

// seedHistory registers daily candles for sym from 2025-01-01 through upto (inclusive).
func seedHistory(f *fakes, sym string, upto time.Time) {
	m := map[string]float64{}
	for d := histEpoch; !d.After(upto); d = d.AddDate(0, 0, 1) {
		m[d.Format(dayFmt)] = closeFor(d)
	}
	f.mu.Lock()
	f.history[sym] = m
	f.mu.Unlock()
}

func hist(t *testing.T, svc *prices.Service, sym string, day time.Time) (float64, bool) {
	t.Helper()
	usd, ok, err := svc.Historical(ctx, sym, day)
	if err != nil {
		t.Fatalf("Historical(%s, %s): %v", sym, day.Format(dayFmt), err)
	}
	return usd, ok
}

func TestHistoricalFromCandlesThenDB(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	usd, ok := hist(t, e.svc, "BTC", day)
	if !ok || usd != closeFor(day) {
		t.Fatalf("got %v %v, want %v", usd, ok, closeFor(day))
	}
	if e.fk.count("candles") != 1 {
		t.Fatalf("candle requests = %d, want 1", e.fk.count("candles"))
	}

	// Second lookup: straight from the price_history table, no HTTP.
	usd, ok = hist(t, e.svc, "BTC", day)
	if !ok || usd != closeFor(day) || e.fk.count("candles") != 1 {
		t.Fatalf("second lookup: %v %v, candle calls=%d", usd, ok, e.fk.count("candles"))
	}

	// The row is in the table, keyed by symbol and UTC day.
	var got float64
	if err := e.db.QueryRow(`SELECT usd FROM price_history WHERE asset='BTC' AND day='2026-09-01'`).Scan(&got); err != nil || got != closeFor(day) {
		t.Fatalf("db row: %v %v", got, err)
	}
}

func TestHistoricalFetchesWarmWindow(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	hist(t, e.svc, "BTC", day)

	reqs := e.fk.candleRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	r := reqs[0]
	if !r.Start.Equal(r.Start.Truncate(24*time.Hour)) || r.Start.After(day) || !r.End.After(day) {
		t.Fatalf("window %v..%v does not cover %v on UTC day boundaries", r.Start, r.End, day)
	}
	if span := r.End.Sub(r.Start); span < 290*24*time.Hour {
		t.Fatalf("window spans only %v; should warm ~300 days", span)
	}

	// Days around the requested one are now served without HTTP.
	for _, off := range []int{-120, -10, -1, 1, 10, 120} {
		d := day.AddDate(0, 0, off)
		usd, ok := hist(t, e.svc, "BTC", d)
		if !ok || usd != closeFor(d) {
			t.Fatalf("offset %d: %v %v want %v", off, usd, ok, closeFor(d))
		}
	}
	if e.fk.count("candles") != 1 {
		t.Fatalf("neighbouring days refetched: %d requests", e.fk.count("candles"))
	}

	// A day far outside the window triggers one more request.
	far := day.AddDate(-1, 0, 0)
	if usd, ok := hist(t, e.svc, "BTC", far); !ok || usd != closeFor(far) {
		t.Fatalf("far day: %v %v", usd, ok)
	}
	if e.fk.count("candles") != 2 {
		t.Fatalf("candle requests = %d, want 2", e.fk.count("candles"))
	}
}

func TestHistoricalRecentDayWindowStaysInThePast(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	yesterday := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if usd, ok := hist(t, e.svc, "BTC", yesterday); !ok || usd != closeFor(yesterday) {
		t.Fatalf("%v %v", usd, ok)
	}
	// Today's candle is still forming, so the warm window must not have stored it.
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM price_history WHERE day >= '2026-10-16'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows for in-progress days stored", n)
	}
}

func TestHistoricalSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "ETH", t0)
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	hist(t, e.svc, "ETH", day)

	e.fk.down.Store(true)
	svc2 := e.newService()
	usd, ok := hist(t, svc2, "ETH", day)
	if !ok || usd != closeFor(day) {
		t.Fatalf("%v %v", usd, ok)
	}
}

func TestHistoricalNormalisesToUTCDay(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	// 23:30 on Sep 1 in UTC-5 is 04:30 on Sep 2 UTC.
	local := time.Date(2026, 9, 1, 23, 30, 0, 0, time.FixedZone("EST", -5*3600))
	usd, ok := hist(t, e.svc, "btc", local)
	want := closeFor(time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	if !ok || usd != want {
		t.Fatalf("%v %v want %v", usd, ok, want)
	}
}

func TestHistoricalPeggedIsOne(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{"USD", "USDC", "USDT"} {
		usd, ok := hist(t, e.svc, s, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
		if !ok || usd != 1 {
			t.Fatalf("%s: %v %v", s, usd, ok)
		}
	}
	if e.fk.total() != 0 {
		t.Fatal("pegged assets must not hit the network")
	}
}

func TestHistoricalUnknownSymbolIsNotAnError(t *testing.T) {
	e := newEnv(t)
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	usd, ok := hist(t, e.svc, "NOPE", day)
	if ok || usd != 0 {
		t.Fatalf("%v %v", usd, ok)
	}
	if e.fk.count("candles") != 1 {
		t.Fatalf("candle requests = %d", e.fk.count("candles"))
	}
	// Unknown symbols are remembered so a large activity list does not hammer the API.
	hist(t, e.svc, "NOPE", day.AddDate(0, 0, -3))
	if e.fk.count("candles") != 1 {
		t.Fatalf("unknown symbol re-requested: %d", e.fk.count("candles"))
	}
}

func TestHistoricalSPLAndInvalidSymbolsMakeNoRequest(t *testing.T) {
	e := newEnv(t)
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []string{"spl:" + mintBONK, "", "../etc", "A B", "EUR/USD"} {
		if usd, ok := hist(t, e.svc, s, day); ok || usd != 0 {
			t.Fatalf("%q: %v %v", s, usd, ok)
		}
	}
	if e.fk.total() != 0 {
		t.Fatalf("%d requests", e.fk.total())
	}
}

func TestHistoricalDayWithoutCandleIsMissingNotError(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	gap := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	e.fk.mu.Lock()
	delete(e.fk.history["BTC"], gap.Format(dayFmt)) // Coinbase omits buckets with no trades
	e.fk.mu.Unlock()

	if usd, ok := hist(t, e.svc, "BTC", gap); ok || usd != 0 {
		t.Fatalf("%v %v", usd, ok)
	}
	// Asking again must not refetch the same window.
	hist(t, e.svc, "BTC", gap)
	if e.fk.count("candles") != 1 {
		t.Fatalf("candle requests = %d, want 1", e.fk.count("candles"))
	}
	// And neighbours still work.
	d := gap.AddDate(0, 0, 1)
	if usd, ok := hist(t, e.svc, "BTC", d); !ok || usd != closeFor(d) {
		t.Fatalf("%v %v", usd, ok)
	}
}

func TestHistoricalBeforeListingIsMissing(t *testing.T) {
	e := newEnv(t)
	// Symbol exists but only has candles from mid-2026.
	e.fk.mu.Lock()
	e.fk.history["NEW"] = map[string]float64{"2026-07-01": 5, "2026-07-02": 6}
	e.fk.mu.Unlock()
	if _, ok := hist(t, e.svc, "NEW", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatal("expected missing")
	}
	if usd, ok := hist(t, e.svc, "NEW", time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)); !ok || usd != 6 {
		t.Fatalf("%v %v", usd, ok)
	}
}

func TestHistoricalTransientErrorIsReturnedAndNotRemembered(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	e.fk.down.Store(true)
	usd, ok, err := e.svc.Historical(ctx, "BTC", day)
	if err == nil || ok || usd != 0 {
		t.Fatalf("want an error, got %v %v %v", usd, ok, err)
	}

	e.fk.down.Store(false)
	if usd, ok := hist(t, e.svc, "BTC", day); !ok || usd != closeFor(day) {
		t.Fatalf("after recovery: %v %v", usd, ok)
	}
}

func TestHistoricalToday(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	today := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

	// The in-progress candle is usable but never persisted as a final close.
	usd, ok := hist(t, e.svc, "BTC", t0)
	if !ok || usd != closeFor(today) {
		t.Fatalf("%v %v", usd, ok)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM price_history WHERE asset='BTC' AND day='2026-10-16'`).Scan(&n)
	if n != 0 {
		t.Fatal("today's partial candle was stored")
	}

	// Future days have no data and cost no request.
	before := e.fk.count("candles")
	if _, ok := hist(t, e.svc, "BTC", today.AddDate(0, 0, 3)); ok {
		t.Fatal("future day should be missing")
	}
	if e.fk.count("candles") != before {
		t.Fatal("future day triggered a request")
	}
}

func TestHistoricalConcurrentSameSymbolSharesFetch(t *testing.T) {
	e := newEnv(t)
	seedHistory(e.fk, "BTC", t0)
	day := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			d := day.AddDate(0, 0, i)
			if usd, ok, err := e.svc.Historical(ctx, "BTC", d); err != nil || !ok || usd != closeFor(d) {
				t.Errorf("day +%d: %v %v %v", i, usd, ok, err)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if n := e.fk.count("candles"); n != 1 {
		t.Fatalf("%d candle requests for 8 nearby days; expected the warm window to be shared", n)
	}
}

// TestLiveCandleShape runs the real Exchange candle payload (testdata) through the parser.
func TestLiveCandleShape(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /products/BTC-USD/candles", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "exchange_candles_btc.json"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := prices.New(&httpx.Client{HTTP: srv.Client()}, db, prices.Options{
		CoinbaseExchangeURL: srv.URL, Now: func() time.Time { return t0 },
	})
	// Fixture: 1791849600 = 2026-10-13 close 83101.99; 1791590400 = 2026-10-10 close 82910.8.
	if usd, ok := hist(t, svc, "BTC", time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC)); !ok || usd != 83101.99 {
		t.Fatalf("%v %v", usd, ok)
	}
	if usd, ok := hist(t, svc, "BTC", time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)); !ok || usd != 82910.8 {
		t.Fatalf("%v %v", usd, ok)
	}
}
