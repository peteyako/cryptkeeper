package prices_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/httpx"
	"crypto-portfolio/internal/prices"
	"crypto-portfolio/internal/store"
)

// yahooFake serves Yahoo Finance's spark (on /y1) and chart (on /y1 and /y2) endpoints.
type yahooFake struct {
	srv *httptest.Server

	mu        sync.Mutex
	calls     map[string]int                // "spark", "chart1", "chart2"
	quotes    map[string]map[string]any     // ticker -> meta
	closes    map[string]map[string]float64 // ticker -> "YYYY-MM-DD" -> close
	sparkDown bool
	down      bool // every endpoint answers 500
}

func newYahooFake(t *testing.T) *yahooFake {
	f := &yahooFake{calls: map[string]int{}, quotes: map[string]map[string]any{}, closes: map[string]map[string]float64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /y1/v7/finance/spark", f.spark)
	mux.HandleFunc("GET /y1/v8/finance/chart/{t}", func(w http.ResponseWriter, r *http.Request) { f.chart(w, r, "chart1") })
	mux.HandleFunc("GET /y2/v8/finance/chart/{t}", func(w http.ResponseWriter, r *http.Request) { f.chart(w, r, "chart2") })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// quote sets a US-dollar quote: price, previous close, change percent, market time and the
// regular session.
func (f *yahooFake) quote(ticker, name string, price, prevClose float64, at time.Time, open, end time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotes[ticker] = map[string]any{
		"symbol": ticker, "currency": "USD", "instrumentType": "ETF", "longName": name,
		"regularMarketPrice": price, "chartPreviousClose": prevClose,
		"regularMarketChangePercent": (price - prevClose) / prevClose * 100,
		"regularMarketTime":          at.Unix(),
		"currentTradingPeriod":       map[string]any{"regular": map[string]any{"start": open.Unix(), "end": end.Unix()}},
	}
}

func (f *yahooFake) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *yahooFake) hit(w http.ResponseWriter, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
	if f.down || (name == "spark" && f.sparkDown) {
		http.Error(w, "down", http.StatusInternalServerError)
		return false
	}
	return true
}

func (f *yahooFake) spark(w http.ResponseWriter, r *http.Request) {
	if !f.hit(w, "spark") {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []any
	for _, t := range strings.Split(r.URL.Query().Get("symbols"), ",") {
		if m, ok := f.quotes[t]; ok {
			result = append(result, map[string]any{"symbol": t, "response": []any{map[string]any{"meta": m}}})
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"spark": map[string]any{"result": result, "error": nil}})
}

func (f *yahooFake) chart(w http.ResponseWriter, r *http.Request, name string) {
	if !f.hit(w, name) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t := r.PathValue("t")
	meta, ok := f.quotes[t]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`)
		return
	}
	res := map[string]any{"meta": meta}
	if p1 := r.URL.Query().Get("period1"); p1 != "" {
		from, _ := strconv.ParseInt(p1, 10, 64)
		to, _ := strconv.ParseInt(r.URL.Query().Get("period2"), 10, 64)
		var ts []int64
		var closes []any
		for day, c := range f.closes[t] {
			d, _ := time.Parse(time.DateOnly, day)
			open := d.Add(13*time.Hour + 30*time.Minute).Unix() // a US session opens 13:30 UTC
			if open >= from && open < to {
				ts = append(ts, open)
				closes = append(closes, c)
			}
		}
		ts, closes = append(ts, from), append(closes, nil) // Yahoo leaves gaps as null
		res["timestamp"] = ts
		res["indicators"] = map[string]any{"quote": []any{map[string]any{"close": closes}}}
	}
	json.NewEncoder(w).Encode(map[string]any{"chart": map[string]any{"result": []any{res}, "error": nil}})
}

type stockEnv struct {
	yf    *yahooFake
	db    *store.DB
	clock *clock
	svc   *prices.Service
}

func newStockEnv(t *testing.T) *stockEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "cryptkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &stockEnv{yf: newYahooFake(t), db: db, clock: &clock{t: t0}}
	e.svc = e.newService()
	return e
}

func (e *stockEnv) newService() *prices.Service {
	return prices.New(&httpx.Client{HTTP: e.yf.srv.Client(), MaxRetries: 0}, e.db, prices.Options{
		YahooURL:         e.yf.srv.URL + "/y1",
		YahooFallbackURL: e.yf.srv.URL + "/y2",
		TTL:              60 * time.Second,
		StaleMax:         24 * time.Hour,
		Now:              e.clock.Now,
	})
}

// session is a US regular session on the UTC day of t: 13:30–20:00 UTC.
func session(t time.Time) (open, end time.Time) {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.Add(13*time.Hour + 30*time.Minute), d.Add(20 * time.Hour)
}

// TestStockQuotes: tickers are priced together in one spark request, with the day's change,
// when the exchange set the price, and whether the session is open.
func TestStockQuotes(t *testing.T) {
	e := newStockEnv(t)
	open, end := session(t0) // t0 is 12:00 UTC, before the session
	e.yf.quote("VTI", "Vanguard Total Stock Market ETF", 287.45, 285.68, open.Add(-16*time.Hour), open, end)
	e.yf.quote("BND", "Vanguard Total Bond Market ETF", 72.81, 72.89, open.Add(-16*time.Hour), open, end)

	got := e.svc.Get(context.Background(), keys("stock:VTI", "stock:BND", "BTC"))
	vti := wantUSD(t, got, "stock:VTI", 287.45)
	wantChange(t, vti, (287.45-285.68)/285.68*100)
	if vti.MarketOpen || !vti.MarketTime.Equal(open.Add(-16*time.Hour)) {
		t.Errorf("VTI market open %v at %v, want closed at the previous close", vti.MarketOpen, vti.MarketTime)
	}
	wantUSD(t, got, "stock:BND", 72.81)
	if n := e.yf.count("spark"); n != 1 {
		t.Errorf("spark requests = %d, want 1 for both tickers", n)
	}

	e.clock.Set(open.Add(time.Hour))
	if p := e.svc.Get(context.Background(), keys("stock:VTI"))["stock:VTI"]; !p.MarketOpen {
		t.Error("VTI during the session should be open")
	}
}

// TestStockFallbackHost: when the spark request fails, each ticker is asked on the fallback
// host's chart endpoint.
func TestStockFallbackHost(t *testing.T) {
	e := newStockEnv(t)
	open, end := session(t0)
	e.yf.quote("VTI", "Vanguard Total Stock Market ETF", 287.45, 285.68, open, open, end)
	e.yf.sparkDown = true
	wantUSD(t, e.svc.Get(context.Background(), keys("stock:VTI")), "stock:VTI", 287.45)
	if e.yf.count("chart2") != 1 {
		t.Errorf("fallback chart requests = %d, want 1", e.yf.count("chart2"))
	}
}

// TestSecurities: names come with the prices, so pricing right after asks nothing more; an
// unknown ticker is absent and left alone for a while; a listing in another currency is
// described but not priced.
func TestSecurities(t *testing.T) {
	e := newStockEnv(t)
	open, end := session(t0)
	e.yf.quote("VTI", "Vanguard Total Stock Market ETF", 287.45, 285.68, open, open, end)
	e.yf.quote("VOD.L", "Vodafone Group Plc", 71.2, 70.9, open, open, end)
	e.yf.quotes["VOD.L"]["currency"] = "GBp"

	ctx := context.Background()
	secs, err := e.svc.Securities(ctx, []string{"VTI", "ZZZZQ", "VOD.L"})
	if err != nil {
		t.Fatal(err)
	}
	if secs["VTI"].Name != "Vanguard Total Stock Market ETF" || secs["VTI"].Type != "ETF" {
		t.Errorf("VTI = %+v", secs["VTI"])
	}
	if _, ok := secs["ZZZZQ"]; ok {
		t.Error("an unknown ticker was described")
	}
	if secs["VOD.L"].Currency != "GBp" {
		t.Errorf("VOD.L = %+v", secs["VOD.L"])
	}
	got := e.svc.Get(ctx, keys("stock:VTI", "stock:VOD.L"))
	wantUSD(t, got, "stock:VTI", 287.45)
	if _, ok := got["stock:VOD.L"]; ok {
		t.Error("a pence listing was priced in dollars")
	}
	before := e.yf.count("spark")
	e.svc.Securities(ctx, []string{"VTI", "ZZZZQ"})
	if e.yf.count("spark") != before {
		t.Error("known and unknown tickers were asked again within the hour")
	}
}

// TestStockLastGoodOverWeekend: Friday's price still serves on Monday when Yahoo is down,
// though coins' last-good prices expire after StaleMax (a day).
func TestStockLastGoodOverWeekend(t *testing.T) {
	e := newStockEnv(t)
	open, end := session(t0)
	e.yf.quote("VTI", "Vanguard Total Stock Market ETF", 287.45, 285.68, open, open, end)
	wantUSD(t, e.svc.Get(context.Background(), keys("stock:VTI")), "stock:VTI", 287.45)

	e.yf.down = true
	e.clock.Advance(3 * 24 * time.Hour)
	p := wantUSD(t, e.newService().Get(context.Background(), keys("stock:VTI")), "stock:VTI", 287.45)
	if !p.Stale {
		t.Error("Friday's price on Monday should be marked stale")
	}
}

// TestStockHistory: daily closes come from one chart request; a weekend takes Friday's close,
// and Series leaves non-trading days out.
func TestStockHistory(t *testing.T) {
	e := newStockEnv(t)
	open, end := session(t0)
	e.yf.quote("VTI", "Vanguard Total Stock Market ETF", 290, 288, open.Add(-16*time.Hour), open, end)
	e.yf.closes["VTI"] = map[string]float64{"2026-10-08": 281, "2026-10-09": 283.5, "2026-10-12": 284, "2026-10-15": 288}
	ctx := context.Background()
	fri := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		day  time.Time
		want float64
	}{{fri, 283.5}, {fri.AddDate(0, 0, 1), 283.5}, {fri.AddDate(0, 0, 2), 283.5}, {fri.AddDate(0, 0, 3), 284}} {
		usd, ok, err := e.svc.Historical(ctx, "stock:VTI", tc.day)
		if err != nil || !ok || !near(usd, tc.want) {
			t.Errorf("%s: %v %v %v, want %v", tc.day.Format(time.DateOnly), usd, ok, err, tc.want)
		}
	}
	if n := e.yf.count("chart1"); n != 1 {
		t.Errorf("chart requests = %d, want 1 for the window", n)
	}

	pts, err := e.svc.Series(ctx, "stock:VTI", "", 9) // Oct 8 .. Oct 16 (today, before the open)
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	for _, p := range pts {
		days = append(days, p.Day.Format("01-02"))
	}
	if got := strings.Join(days, " "); got != "10-08 10-09 10-12 10-15" {
		t.Errorf("series days %s, want the trading days only", got)
	}

	if _, ok, _ := e.svc.Historical(ctx, "stock:../../x", fri); ok {
		t.Error("an invalid ticker was looked up")
	}
}

// TestYahooCapturedAnswers parses answers captured from Yahoo Finance (testdata/): a spark
// quote of a mutual fund and an ETF, and a week of VTI's daily closes.
func TestYahooCapturedAnswers(t *testing.T) {
	spark, err := os.ReadFile("testdata/yahoo_spark.json")
	if err != nil {
		t.Fatal(err)
	}
	chart, err := os.ReadFile("testdata/yahoo_chart_vti.json")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /y1/v7/finance/spark", func(w http.ResponseWriter, r *http.Request) { w.Write(spark) })
	mux.HandleFunc("GET /y1/v8/finance/chart/VTI", func(w http.ResponseWriter, r *http.Request) { w.Write(chart) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "cryptkeeper.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	svc := prices.New(&httpx.Client{HTTP: srv.Client(), MaxRetries: 0}, db, prices.Options{
		YahooURL: srv.URL + "/y1", YahooFallbackURL: srv.URL + "/y2", Now: func() time.Time { return now },
	})
	ctx := context.Background()

	secs, err := svc.Securities(ctx, []string{"VTSAX", "BND"})
	if err != nil || secs["VTSAX"].Type != "MUTUALFUND" || secs["BND"].Name != "Vanguard Total Bond Market Index Fund ETF Shares" {
		t.Fatalf("securities %+v %v", secs, err)
	}
	got := svc.Get(ctx, keys("stock:VTSAX", "stock:BND"))
	wantChange(t, wantUSD(t, got, "stock:VTSAX", 182.09), -1.162)
	wantUSD(t, got, "stock:BND", 70.59)

	usd, ok, err := svc.Historical(ctx, "stock:VTI", time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)) // a Sunday
	if err != nil || !ok || !near(usd, 375.42999267578125) {
		t.Fatalf("Sunday close %v %v %v, want Friday's", usd, ok, err)
	}
}
