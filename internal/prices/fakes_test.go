package prices_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
	"crypto-portfolio/internal/prices"
	"crypto-portfolio/internal/store"
)

// fakes serves stand-ins for Coinbase, Coinbase Exchange, Kraken and Jupiter from one
// httptest server (one path prefix per upstream). Response shapes mirror the live APIs;
// see testdata/ for captured originals.
type fakes struct {
	srv *httptest.Server

	mu      sync.Mutex
	calls   map[string]int // "spot", "rates", "stats", "candles", "kraken", "jupiter"
	jupIDs  [][]string     // ids of every Jupiter request, in arrival order
	krPairs []string
	candleQ []url2 // start/end of every candles request
	inFlt   map[string]int
	maxFlt  map[string]int

	spot    map[string]string             // Coinbase spot amount by symbol; missing => 404
	rates   map[string]string             // Coinbase exchange-rates (USD base)
	stats   map[string][2]string          // Exchange stats {open,last} by symbol
	kraken  map[string]krakenFake         // by requested pair, e.g. "XBTUSD"
	jup     map[string]jupFake            // by mint
	history map[string]map[string]float64 // Exchange candles: symbol -> "YYYY-MM-DD" -> close

	cg           map[string]map[string]float64 // CoinGecko: mint -> "YYYY-MM-DD" (00:00 UTC) or "YYYY-MM-DDTHH:MM" -> USD
	cgStatus     map[string]int                // mint -> forced HTTP status (e.g. 404)
	cgRetryAfter string                        // Retry-After header sent with a forced status
	cgHeaders    []http.Header                 // headers of every CoinGecko request
	cgKey        string                        // key the test service returns

	candleIn   chan struct{} // when non-nil, receives a value as each candles request arrives
	candleHold chan struct{} // when non-nil, the candles handler blocks until closed

	delisted map[string]bool // Exchange products reporting status "delisted"; others 404 (unknown)
	bad      map[string]bool // spot symbols answering 200 with garbage
	down     atomic.Bool     // every endpoint answers 500
	hold     chan struct{}   // when non-nil, spot handler blocks until closed
	slow     time.Duration   // spot handler sleeps this long
}

type url2 struct{ Start, End time.Time }

type krakenFake struct{ Key, Last, Open string }

type jupFake struct {
	Price  float64
	Change *float64
}

func newFakes(t *testing.T) *fakes {
	t.Helper()
	f := &fakes{
		calls:    map[string]int{},
		inFlt:    map[string]int{},
		maxFlt:   map[string]int{},
		spot:     map[string]string{},
		rates:    map[string]string{},
		stats:    map[string][2]string{},
		kraken:   map[string]krakenFake{},
		jup:      map[string]jupFake{},
		history:  map[string]map[string]float64{},
		cg:       map[string]map[string]float64{},
		cgStatus: map[string]int{},
		bad:      map[string]bool{},
		delisted: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cb/v2/prices/{pair}/spot", f.handleSpot)
	mux.HandleFunc("GET /cb/v2/exchange-rates", f.handleRates)
	mux.HandleFunc("GET /ex/products/{pair}/stats", f.handleStats)
	mux.HandleFunc("GET /ex/products/{pair}", f.handleProduct)
	mux.HandleFunc("GET /ex/products/{pair}/candles", f.handleCandles)
	mux.HandleFunc("GET /kr/0/public/Ticker", f.handleKraken)
	mux.HandleFunc("GET /jup/price/v3", f.handleJupiter)
	mux.HandleFunc("GET /cg/coins/solana/contract/{mint}/market_chart/range", f.handleCoinGecko)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakes) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakes) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.calls {
		n += v
	}
	return n
}

func (f *fakes) maxInFlight(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxFlt[name]
}

// begin records a call; the returned func must be deferred. It reports false (after
// writing a 500) when the fakes are "down".
func (f *fakes) begin(w http.ResponseWriter, name string) (func(), bool) {
	f.mu.Lock()
	f.calls[name]++
	f.inFlt[name]++
	if f.inFlt[name] > f.maxFlt[name] {
		f.maxFlt[name] = f.inFlt[name]
	}
	f.mu.Unlock()
	done := func() {
		f.mu.Lock()
		f.inFlt[name]--
		f.mu.Unlock()
	}
	if f.down.Load() {
		http.Error(w, `{"message":"upstream down"}`, http.StatusInternalServerError)
		return done, false
	}
	return done, true
}

func (f *fakes) handleSpot(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "spot")
	defer done()
	if !ok {
		return
	}
	if f.hold != nil {
		<-f.hold
	}
	if f.slow > 0 {
		time.Sleep(f.slow)
	}
	sym, _, _ := strings.Cut(r.PathValue("pair"), "-")
	f.mu.Lock()
	amount, found := f.spot[sym]
	bad := f.bad[sym]
	f.mu.Unlock()
	if bad {
		fmt.Fprint(w, `<html>not json</html>`)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"rpc error: code = NotFound desc = not found","code":5,"message":"rpc error: code = NotFound desc = not found"}`)
		return
	}
	fmt.Fprintf(w, `{"data":{"amount":%q,"base":%q,"currency":"USD"}}`, amount, sym)
}

func (f *fakes) handleRates(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "rates")
	defer done()
	if !ok {
		return
	}
	if r.URL.Query().Get("currency") != "USD" {
		http.Error(w, "currency must be USD", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	rates := make(map[string]string, len(f.rates))
	for k, v := range f.rates {
		rates[k] = v
	}
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"currency": "USD", "rates": rates}})
}

func (f *fakes) handleStats(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "stats")
	defer done()
	if !ok {
		return
	}
	sym, _, _ := strings.Cut(r.PathValue("pair"), "-")
	f.mu.Lock()
	st, found := f.stats[sym]
	f.mu.Unlock()
	if !found {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"NotFound"}`)
		return
	}
	fmt.Fprintf(w, `{"open":%q,"high":"1","low":"1","last":%q,"volume":"10","volume_30day":"100"}`, st[0], st[1])
}

// handleCandles mimics Coinbase Exchange as observed live: newest first,
// [time, low, high, open, close, volume]; buckets without data are omitted; the bucket that
// starts exactly at `end` is included (so a 300-day span yields up to 301 buckets, and this
// fake rejects anything over 300 to stay on the safe side of the documented limit).
func (f *fakes) handleCandles(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "candles")
	defer done()
	if !ok {
		return
	}
	sym, _, _ := strings.Cut(r.PathValue("pair"), "-")
	q := r.URL.Query()
	if q.Get("granularity") != "86400" {
		http.Error(w, `{"message":"Unsupported granularity"}`, http.StatusBadRequest)
		return
	}
	start, err1 := time.Parse(time.RFC3339, q.Get("start"))
	end, err2 := time.Parse(time.RFC3339, q.Get("end"))
	if err1 != nil || err2 != nil || !end.After(start) {
		http.Error(w, `{"message":"bad start/end"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.candleQ = append(f.candleQ, url2{start, end})
	days, found := f.history[sym]
	in, hold := f.candleIn, f.candleHold
	f.mu.Unlock()
	if in != nil {
		in <- struct{}{}
	}
	if hold != nil {
		<-hold
	}
	buckets := 0
	for d := end.UTC().Truncate(24 * time.Hour); !d.Before(start); d = d.Add(-24 * time.Hour) {
		buckets++
	}
	if buckets > 300 {
		http.Error(w, `{"message":"granularity too small for the requested time range"}`, http.StatusBadRequest)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"NotFound"}`)
		return
	}
	out := [][]float64{}
	for d := end.UTC().Truncate(24 * time.Hour); !d.Before(start); d = d.Add(-24 * time.Hour) {
		c, ok := days[d.Format("2006-01-02")]
		if !ok {
			continue
		}
		out = append(out, []float64{float64(d.Unix()), c - 2, c + 2, c - 1, c, 100})
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakes) handleKraken(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "kraken")
	defer done()
	if !ok {
		return
	}
	pair := r.URL.Query().Get("pair")
	f.mu.Lock()
	f.krPairs = append(f.krPairs, pair)
	k, found := f.kraken[pair]
	f.mu.Unlock()
	if !found {
		fmt.Fprint(w, `{"error":["EQuery:Unknown asset pair"]}`)
		return
	}
	fmt.Fprintf(w, `{"error":[],"result":{%q:{"a":["1","1","1.000"],"b":["1","1","1.000"],"c":[%q,"0.5"],"v":["1","1"],"p":["1","1"],"t":[1,1],"l":["1","1"],"h":["1","1"],"o":%q}}}`,
		k.Key, k.Last, k.Open)
}

func (f *fakes) handleJupiter(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "jupiter")
	defer done()
	if !ok {
		return
	}
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	f.mu.Lock()
	f.jupIDs = append(f.jupIDs, ids)
	out := map[string]any{}
	for _, id := range ids {
		if j, found := f.jup[id]; found {
			e := map[string]any{"createdAt": "2024-06-05T08:55:25.527Z", "liquidity": 1e6, "usdPrice": j.Price, "blockId": 1, "decimals": 6}
			if j.Change != nil {
				e["priceChange24h"] = *j.Change
			}
			out[id] = e
		}
	}
	f.mu.Unlock()
	json.NewEncoder(w).Encode(out)
}

func (f *fakes) jupiterRequests() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.jupIDs...)
}

func (f *fakes) krakenPairs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.krPairs...)
}

func (f *fakes) candleRequests() []url2 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url2(nil), f.candleQ...)
}

// --- clock ---

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// --- environment ---

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type env struct {
	t     *testing.T
	fk    *fakes
	db    *store.DB
	clock *clock
	svc   *prices.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "cryptkeep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{t: t, fk: newFakes(t), db: db, clock: &clock{t: t0}}
	e.svc = e.newService()
	return e
}

// newService builds another Service over the same DB, fakes and clock (a "restart").
func (e *env) newService() *prices.Service {
	return prices.New(&httpx.Client{HTTP: e.fk.srv.Client(), MaxRetries: 0}, e.db, prices.Options{
		CoinbaseURL:         e.fk.srv.URL + "/cb",
		CoinbaseExchangeURL: e.fk.srv.URL + "/ex",
		KrakenURL:           e.fk.srv.URL + "/kr",
		JupiterPriceURL:     e.fk.srv.URL + "/jup/price/v3",
		CoinGeckoURL:        e.fk.srv.URL + "/cg",
		CoinGeckoKey:        func() string { return e.fk.cgKey },
		TTL:                 60 * time.Second,
		StaleMax:            24 * time.Hour,
		Now:                 e.clock.Now,
	})
}

func keys(syms ...string) []prices.Want {
	w := make([]prices.Want, len(syms))
	for i, s := range syms {
		w[i] = prices.Want{Key: s}
		if m, ok := core.IsSPLKey(s); ok {
			w[i].Mint = m
		}
	}
	return w
}

// --- assertions ---

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func wantUSD(t *testing.T, got map[string]core.Price, key string, usd float64) core.Price {
	t.Helper()
	p, ok := got[key]
	if !ok {
		t.Fatalf("no price for %q; got %v", key, sortedKeys(got))
	}
	if !near(p.USD, usd) {
		t.Fatalf("%s: USD = %v, want %v", key, p.USD, usd)
	}
	return p
}

func wantChange(t *testing.T, p core.Price, pct float64) {
	t.Helper()
	if p.Change24h == nil {
		t.Fatalf("Change24h = nil, want %v", pct)
	}
	if math.Abs(*p.Change24h-pct) > 1e-6 {
		t.Fatalf("Change24h = %v, want %v", *p.Change24h, pct)
	}
}

func sortedKeys(m map[string]core.Price) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fptr(f float64) *float64 { return &f }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// handleProduct mimics GET /products/{SYM}-USD on Coinbase Exchange: delisted products
// answer with status "delisted"; everything else is unknown (404), which must not block
// a Coinbase spot price.
func (f *fakes) handleProduct(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "product")
	defer done()
	if !ok {
		return
	}
	sym := strings.TrimSuffix(r.PathValue("pair"), "-USD")
	f.mu.Lock()
	del := f.delisted[sym]
	f.mu.Unlock()
	if !del {
		http.Error(w, `{"message":"NotFound"}`, http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"id": sym + "-USD", "status": "delisted", "trading_disabled": true})
}

// handleCoinGecko serves {"prices": [[ms, usd], ...]} with one point per stored key (a day at
// 00:00 UTC, or a day and time) inside [from, to].
func (f *fakes) handleCoinGecko(w http.ResponseWriter, r *http.Request) {
	done, ok := f.begin(w, "coingecko")
	defer done()
	if !ok {
		return
	}
	mint := r.PathValue("mint")
	f.mu.Lock()
	f.cgHeaders = append(f.cgHeaders, r.Header.Clone())
	status := f.cgStatus[mint]
	series := f.cg[mint]
	retryAfter := f.cgRetryAfter
	f.mu.Unlock()
	if status != 0 {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		http.Error(w, `{"error":"coin not found"}`, status)
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	pts := [][2]float64{}
	for day, usd := range series {
		d, err := time.Parse("2006-01-02", day)
		if err != nil {
			d, err = time.Parse("2006-01-02T15:04", day)
		}
		if err != nil || d.Unix() < from || d.Unix() > to {
			continue
		}
		pts = append(pts, [2]float64{float64(d.UnixMilli()), usd})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i][0] < pts[j][0] })
	_ = json.NewEncoder(w).Encode(map[string]any{"prices": pts})
}
