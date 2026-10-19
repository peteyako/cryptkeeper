package prices_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
	"cryptkeeper/internal/prices"
	"cryptkeeper/internal/store"
)

const (
	mintSOL  = "So11111111111111111111111111111111111111112"
	mintJUP  = "JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN"
	mintUSDC = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	mintBONK = "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263"
)

var ctx = context.Background()

func TestBTCViaCoinbase(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000.5"
	e.fk.stats["BTC"] = [2]string{"64000", "65000"}

	got := e.svc.Get(ctx, keys("BTC"))

	p := wantUSD(t, got, "BTC", 65000.5)
	if p.Stale || !p.AsOf.Equal(t0) {
		t.Fatalf("want fresh price as of %v, got %+v", t0, p)
	}
	wantChange(t, p, 1.5625) // (65000-64000)/64000*100
	if n := e.fk.count("kraken") + e.fk.count("jupiter"); n != 0 {
		t.Fatalf("fallbacks hit despite Coinbase success: %d", n)
	}
}

func TestChangeLookupFailureIsNotFatal(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["ETH"] = "2400" // no stats registered -> 404
	p := wantUSD(t, e.svc.Get(ctx, keys("ETH")), "ETH", 2400)
	if p.Change24h != nil {
		t.Fatalf("Change24h = %v, want nil", *p.Change24h)
	}
}

func TestKrakenOnlySymbol(t *testing.T) {
	e := newEnv(t)
	// Coinbase answers 404 for XYZ; Kraken knows it under an altname-style result key.
	e.fk.kraken["XYZUSD"] = krakenFake{Key: "XXYZZUSD", Last: "12.5", Open: "10"}

	got := e.svc.Get(ctx, keys("XYZ"))

	p := wantUSD(t, got, "XYZ", 12.5)
	wantChange(t, p, 25)
	if e.fk.count("spot") != 1 || e.fk.count("kraken") != 1 {
		t.Fatalf("calls: spot=%d kraken=%d", e.fk.count("spot"), e.fk.count("kraken"))
	}
	if e.fk.count("jupiter") != 0 {
		t.Fatal("Jupiter must not be called without a Mint")
	}
}

func TestKrakenMapsBTCToXBT(t *testing.T) {
	e := newEnv(t)
	e.fk.kraken["XBTUSD"] = krakenFake{Key: "XXBTZUSD", Last: "81091.7", Open: "82135.4"}

	got := e.svc.Get(ctx, keys("BTC"))

	wantUSD(t, got, "BTC", 81091.7)
	if pairs := e.fk.krakenPairs(); len(pairs) != 1 || pairs[0] != "XBTUSD" {
		t.Fatalf("kraken pairs = %v, want [XBTUSD]", pairs)
	}
}

func TestKrakenErrorBodyIsFailure(t *testing.T) {
	e := newEnv(t)
	got := e.svc.Get(ctx, keys("NOPE"))
	if _, ok := got["NOPE"]; ok {
		t.Fatalf("unknown symbol should have no price: %+v", got)
	}
}

func TestJupiterBatchesMintsInOneRequest(t *testing.T) {
	e := newEnv(t)
	e.fk.jup[mintSOL] = jupFake{103.5, fptr(-2.0)}
	e.fk.jup[mintJUP] = jupFake{0.298, fptr(-3.5)}
	e.fk.jup[mintBONK] = jupFake{0.00002, nil}

	got := e.svc.Get(ctx, keys("spl:"+mintSOL, "spl:"+mintJUP, "spl:"+mintBONK, "spl:"+mintUSDC))

	if reqs := e.fk.jupiterRequests(); len(reqs) != 1 || len(reqs[0]) != 4 {
		t.Fatalf("want exactly one Jupiter request with 4 ids, got %v", reqs)
	}
	p := wantUSD(t, got, "spl:"+mintSOL, 103.5)
	wantChange(t, p, -2.0)
	wantChange(t, wantUSD(t, got, "spl:"+mintJUP, 0.298), -3.5)
	if p := wantUSD(t, got, "spl:"+mintBONK, 0.00002); p.Change24h != nil {
		t.Fatalf("missing priceChange24h should give nil, got %v", *p.Change24h)
	}
	if _, ok := got["spl:"+mintUSDC]; ok {
		t.Fatal("mint unknown to Jupiter must be omitted")
	}
}

func TestJupiterChunksAtFifty(t *testing.T) {
	e := newEnv(t)
	var wants []prices.Want
	for i := 0; i < 120; i++ {
		m := fmt.Sprintf("Mint%03d", i)
		e.fk.jup[m] = jupFake{float64(i + 1), nil}
		wants = append(wants, prices.Want{Key: core.SPLKey(m), Mint: m})
	}
	got := e.svc.Get(ctx, wants)
	if len(got) != 120 {
		t.Fatalf("got %d prices, want 120", len(got))
	}
	reqs := e.fk.jupiterRequests()
	if len(reqs) != 3 {
		t.Fatalf("want 3 Jupiter requests (50+50+20), got %d", len(reqs))
	}
	total := 0
	for _, r := range reqs {
		if len(r) > 50 {
			t.Fatalf("chunk of %d ids exceeds 50", len(r))
		}
		total += len(r)
	}
	if total != 120 {
		t.Fatalf("ids across chunks = %d, want 120", total)
	}
}

func TestMappedTokenPricedByMintFirst(t *testing.T) {
	e := newEnv(t)
	// Coinbase's delisted JUP product quotes a stale price ~1000x too low; the mint is the
	// token's real identity, so Jupiter wins and the exchanges aren't consulted.
	e.fk.spot["JUP"] = "0.000253"
	e.fk.jup[mintJUP] = jupFake{0.3, fptr(4)}

	got := e.svc.Get(ctx, []prices.Want{{Key: "JUP", Mint: mintJUP}})

	wantChange(t, wantUSD(t, got, "JUP", 0.3), 4)
	if e.fk.count("jupiter") != 1 || e.fk.count("spot") != 0 || e.fk.count("kraken") != 0 {
		t.Fatalf("chain order broken: spot=%d kraken=%d jupiter=%d", e.fk.count("spot"), e.fk.count("kraken"), e.fk.count("jupiter"))
	}
}

func TestMappedTokenFallsBackToSymbolChainWhenJupiterMisses(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["JUP"] = "0.31" // Jupiter has nothing for the mint
	got := e.svc.Get(ctx, []prices.Want{{Key: "JUP", Mint: mintJUP}})
	wantUSD(t, got, "JUP", 0.31)
	if e.fk.count("jupiter") != 1 || e.fk.count("spot") != 1 {
		t.Fatalf("jupiter=%d spot=%d", e.fk.count("jupiter"), e.fk.count("spot"))
	}
}

func TestMappedFallbacksShareOneJupiterRequest(t *testing.T) {
	e := newEnv(t)
	e.fk.jup[mintJUP] = jupFake{0.3, nil}
	e.fk.jup[mintBONK] = jupFake{0.00002, nil}
	e.svc.Get(ctx, []prices.Want{
		{Key: "JUP", Mint: mintJUP},
		{Key: "BONK", Mint: mintBONK},
		{Key: "spl:" + mintSOL, Mint: mintSOL},
	})
	if reqs := e.fk.jupiterRequests(); len(reqs) != 1 || len(reqs[0]) != 3 {
		t.Fatalf("want one request with 3 ids, got %v", reqs)
	}
}

func TestSPLKeyNeverUsesSymbolSources(t *testing.T) {
	e := newEnv(t)
	// A scam token reusing a real ticker must not be priced via Coinbase/Kraken.
	e.fk.spot[mintBONK] = "999"
	e.fk.kraken[mintBONK+"USD"] = krakenFake{Key: "X", Last: "999", Open: "999"}
	got := e.svc.Get(ctx, keys("spl:"+mintBONK))
	if len(got) != 0 {
		t.Fatalf("expected no price, got %+v", got)
	}
	for _, name := range []string{"spot", "stats", "kraken", "rates"} {
		if e.fk.count(name) != 0 {
			t.Fatalf("%s hit for an spl: key", name)
		}
	}
	if e.fk.count("jupiter") != 1 {
		t.Fatalf("jupiter calls = %d, want 1", e.fk.count("jupiter"))
	}
}

func TestFiatViaExchangeRates(t *testing.T) {
	e := newEnv(t)
	e.fk.rates["EUR"] = "0.8"
	e.fk.rates["GBP"] = "0.5"
	e.fk.rates["BTC"] = "0.00001"

	got := e.svc.Get(ctx, keys("EUR", "GBP", "CAD"))

	p := wantUSD(t, got, "EUR", 1.25)
	if p.Change24h != nil {
		t.Fatal("fiat has no 24h change")
	}
	wantUSD(t, got, "GBP", 2.0)
	if _, ok := got["CAD"]; ok {
		t.Fatal("CAD is absent from rates; must have no price")
	}
	if e.fk.count("rates") != 1 {
		t.Fatalf("exchange-rates fetched %d times, want 1 per call", e.fk.count("rates"))
	}
	if e.fk.count("spot") != 0 || e.fk.count("kraken") != 0 {
		t.Fatal("fiat must not use crypto sources")
	}
}

func TestExchangeRatesAreSharedAcrossGetsWithinTTL(t *testing.T) {
	e := newEnv(t)
	e.fk.rates["EUR"] = "0.8"
	e.fk.rates["GBP"] = "0.5"
	wantUSD(t, e.svc.Get(ctx, keys("EUR")), "EUR", 1.25)
	e.clock.Advance(30 * time.Second)
	wantUSD(t, e.svc.Get(ctx, keys("GBP")), "GBP", 2.0) // different key, same rates table
	if e.fk.count("rates") != 1 {
		t.Fatalf("rates fetched %d times within TTL, want 1", e.fk.count("rates"))
	}
	e.clock.Advance(2 * time.Minute)
	e.fk.rates["EUR"] = "0.9"
	wantUSD(t, e.svc.Get(ctx, keys("EUR")), "EUR", 1/0.9)
	if e.fk.count("rates") != 2 {
		t.Fatalf("rates fetched %d times after TTL, want 2", e.fk.count("rates"))
	}
}

func TestMNTIsCryptoNotTugrik(t *testing.T) {
	// Coinbase's rates table lists MNT as the Mongolian tugrik, but MNT is also Mantle.
	e := newEnv(t)
	e.fk.rates["MNT"] = "3599"
	e.fk.spot["MNT"] = "0.85"
	wantUSD(t, e.svc.Get(ctx, keys("MNT")), "MNT", 0.85)
}

func TestCryptoCodeInRatesIsNotTreatedAsFiat(t *testing.T) {
	e := newEnv(t)
	e.fk.rates["BTC"] = "0.0000123" // present in the rates table, but BTC is not fiat
	e.fk.spot["BTC"] = "81000"
	wantUSD(t, e.svc.Get(ctx, keys("BTC")), "BTC", 81000)
	if e.fk.count("rates") != 0 {
		t.Fatal("rates fetched for a crypto symbol")
	}
}

func TestUSDIsFixedWithoutNetwork(t *testing.T) {
	e := newEnv(t)
	got := e.svc.Get(ctx, keys("USD"))
	p := wantUSD(t, got, "USD", 1)
	if p.Stale || !p.AsOf.Equal(t0) {
		t.Fatalf("%+v", p)
	}
	if e.fk.total() != 0 {
		t.Fatalf("USD made %d HTTP calls", e.fk.total())
	}
}

func TestStablecoinUsesLivePriceWhenAvailable(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["USDC"] = "0.9998"
	e.fk.spot["USDT"] = "1.0003"
	got := e.svc.Get(ctx, []prices.Want{{Key: "USDC"}, {Key: "USDT"}})
	wantUSD(t, got, "USDC", 0.9998)
	wantUSD(t, got, "USDT", 1.0003)
	if e.fk.count("jupiter") != 0 {
		t.Fatal("Jupiter called for wants without a mint")
	}
}

func TestStablecoinPegFallbackWhenEverythingFails(t *testing.T) {
	e := newEnv(t)
	// Nothing registered anywhere: Coinbase 404, Kraken unknown, Jupiter empty.
	got := e.svc.Get(ctx, []prices.Want{{Key: "USDC", Mint: mintUSDC}, {Key: "USDT"}})
	for _, k := range []string{"USDC", "USDT"} {
		p := wantUSD(t, got, k, 1)
		if p.Stale || !p.AsOf.Equal(t0) {
			t.Fatalf("%s: peg fallback should be fresh-as-now, got %+v", k, p)
		}
	}
	if e.fk.count("jupiter") != 1 {
		t.Fatalf("jupiter should be tried before the peg: %d", e.fk.count("jupiter"))
	}

	// Same when the upstreams are hard down.
	e.fk.down.Store(true)
	e.clock.Advance(time.Hour)
	wantUSD(t, e.svc.Get(ctx, keys("USDC")), "USDC", 1)
}

func TestPegFallbackBeatsStaleCache(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["USDC"] = "0.9991"
	wantUSD(t, e.svc.Get(ctx, keys("USDC")), "USDC", 0.9991)
	e.fk.down.Store(true)
	e.clock.Advance(5 * time.Minute)
	p := wantUSD(t, e.svc.Get(ctx, keys("USDC")), "USDC", 1)
	if p.Stale {
		t.Fatal("peg fallback is not a stale price")
	}
}

func TestStaleFallbackThenExpiry(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.fk.stats["BTC"] = [2]string{"60000", "65000"}
	wantUSD(t, e.svc.Get(ctx, keys("BTC")), "BTC", 65000)

	e.fk.down.Store(true)
	e.clock.Advance(10 * time.Minute) // past TTL, well inside StaleMax
	got := e.svc.Get(ctx, keys("BTC"))
	p := wantUSD(t, got, "BTC", 65000)
	if !p.Stale || !p.AsOf.Equal(t0) {
		t.Fatalf("want stale price as of %v, got %+v", t0, p)
	}
	wantChange(t, p, 8.333333333)

	e.clock.Set(t0.Add(24 * time.Hour)) // exactly StaleMax: still usable
	if p := wantUSD(t, e.svc.Get(ctx, keys("BTC")), "BTC", 65000); !p.Stale {
		t.Fatal("expected stale")
	}

	e.clock.Set(t0.Add(24*time.Hour + time.Second)) // past StaleMax: dropped
	if got := e.svc.Get(ctx, keys("BTC")); len(got) != 0 {
		t.Fatalf("price older than StaleMax must be omitted, got %+v", got)
	}
}

func TestRecoveryReplacesStale(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.svc.Get(ctx, keys("BTC"))
	e.fk.down.Store(true)
	e.clock.Advance(2 * time.Minute)
	if p := e.svc.Get(ctx, keys("BTC"))["BTC"]; !p.Stale {
		t.Fatal("expected stale while down")
	}
	e.fk.down.Store(false)
	e.fk.spot["BTC"] = "66000"
	e.clock.Advance(2 * time.Minute)
	p := wantUSD(t, e.svc.Get(ctx, keys("BTC")), "BTC", 66000)
	if p.Stale || !p.AsOf.Equal(e.clock.Now()) {
		t.Fatalf("%+v", p)
	}
}

func TestStaleJupiterAndFiat(t *testing.T) {
	e := newEnv(t)
	e.fk.jup[mintBONK] = jupFake{0.00002, fptr(7)}
	e.fk.rates["EUR"] = "0.8"
	e.svc.Get(ctx, keys("spl:"+mintBONK, "EUR"))
	e.fk.down.Store(true)
	e.clock.Advance(time.Hour)
	got := e.svc.Get(ctx, keys("spl:"+mintBONK, "EUR"))
	if p := wantUSD(t, got, "spl:"+mintBONK, 0.00002); !p.Stale || !p.AsOf.Equal(t0) {
		t.Fatalf("%+v", p)
	}
	if p := wantUSD(t, got, "EUR", 1.25); !p.Stale {
		t.Fatalf("%+v", p)
	}
}

func TestLastGoodSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.fk.stats["BTC"] = [2]string{"50000", "65000"}
	e.svc.Get(ctx, keys("BTC"))

	// "Restart": a brand-new Service (empty in-memory cache) over the same database.
	svc2 := e.newService()
	e.fk.down.Store(true)
	e.clock.Advance(3 * time.Hour)
	p := wantUSD(t, svc2.Get(ctx, keys("BTC")), "BTC", 65000)
	if !p.Stale || !p.AsOf.Equal(t0) {
		t.Fatalf("want stale as of %v, got %+v", t0, p)
	}
	wantChange(t, p, 30)
}

func TestLastGoodRowIsWritten(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.svc.Get(ctx, keys("BTC", "USD"))
	var usd float64
	var chg sql.NullFloat64
	var asOf int64
	if err := e.db.QueryRow(`SELECT usd, change24h, as_of FROM lastgood_price WHERE asset = 'BTC'`).Scan(&usd, &chg, &asOf); err != nil {
		t.Fatal(err)
	}
	if usd != 65000 || chg.Valid || asOf != t0.Unix() {
		t.Fatalf("row = %v %v %v", usd, chg, asOf)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM lastgood_price WHERE asset = 'USD'`).Scan(&n)
	if n != 0 {
		t.Fatal("USD is a constant and should not be stored")
	}
}

func TestTTLCacheAvoidsSecondCall(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.fk.spot["ETH"] = "2400"
	e.fk.stats["BTC"] = [2]string{"64000", "65000"}

	first := e.svc.Get(ctx, keys("BTC"))
	e.clock.Advance(59 * time.Second)
	second := e.svc.Get(ctx, keys("BTC"))
	if e.fk.count("spot") != 1 || e.fk.count("stats") != 1 {
		t.Fatalf("within TTL: spot=%d stats=%d, want 1/1", e.fk.count("spot"), e.fk.count("stats"))
	}
	if second["BTC"].USD != first["BTC"].USD || !second["BTC"].AsOf.Equal(t0) {
		t.Fatalf("cached price differs: %+v vs %+v", first["BTC"], second["BTC"])
	}

	// A new key only fetches itself.
	e.svc.Get(ctx, keys("BTC", "ETH"))
	if e.fk.count("spot") != 2 {
		t.Fatalf("spot=%d, want 2 (ETH only)", e.fk.count("spot"))
	}

	e.clock.Advance(2 * time.Second) // t0+61s: BTC expired, ETH (fetched at +59s) still fresh
	e.svc.Get(ctx, keys("BTC", "ETH"))
	if e.fk.count("spot") != 3 {
		t.Fatalf("after TTL: spot=%d, want 3", e.fk.count("spot"))
	}
}

func TestStaleResultsAreNotCached(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.svc.Get(ctx, keys("BTC"))
	e.fk.down.Store(true)
	e.clock.Advance(2 * time.Minute)
	e.svc.Get(ctx, keys("BTC"))
	before := e.fk.count("spot")
	e.svc.Get(ctx, keys("BTC")) // must retry the network, not serve the stale value from memory
	if e.fk.count("spot") != before+1 {
		t.Fatal("stale price was cached in memory")
	}
}

func TestOneBadSymbolDoesNotFailTheMap(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.fk.spot["ETH"] = "garbage" // unparsable amount
	e.fk.bad["SOL"] = true       // non-JSON body
	e.fk.spot["ADA"] = "0.4"
	e.fk.spot["DOT"] = "-1" // nonsense price
	e.fk.jup[mintBONK] = jupFake{0.00002, nil}

	got := e.svc.Get(ctx, []prices.Want{
		{Key: "BTC"}, {Key: "ETH"}, {Key: "SOL"}, {Key: "ADA"}, {Key: "DOT"},
		{Key: "spl:" + mintBONK, Mint: mintBONK}, {Key: "bad key/../x"}, {Key: ""},
	})
	wantUSD(t, got, "BTC", 65000)
	wantUSD(t, got, "ADA", 0.4)
	wantUSD(t, got, "spl:"+mintBONK, 0.00002)
	for _, k := range []string{"ETH", "SOL", "DOT", "bad key/../x", ""} {
		if _, ok := got[k]; ok {
			t.Fatalf("%q should have no price: %+v", k, got[k])
		}
	}
}

func TestJupiterFailureDoesNotAffectExchangePrices(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	// Jupiter answers garbage for this ids set.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"error":"nope"}`) }))
	defer srv.Close()
	svc := prices.New(&httpx.Client{HTTP: srv.Client()}, e.db, prices.Options{
		CoinbaseURL: e.fk.srv.URL + "/cb", CoinbaseExchangeURL: e.fk.srv.URL + "/ex", KrakenURL: e.fk.srv.URL + "/kr",
		JupiterPriceURL: srv.URL, Now: e.clock.Now,
	})
	got := svc.Get(ctx, keys("BTC", "spl:"+mintBONK))
	wantUSD(t, got, "BTC", 65000)
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestDuplicateWantsAndConcurrentGets(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.fk.hold = make(chan struct{})

	var wg sync.WaitGroup
	results := make([]map[string]core.Price, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.svc.Get(ctx, keys("BTC", "BTC"))
		}()
	}
	// Wait until the first request is in flight, then let everything through.
	deadline := time.Now().Add(5 * time.Second)
	for e.fk.count("spot") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the other callers pile up on singleflight
	close(e.fk.hold)
	wg.Wait()

	for _, r := range results {
		wantUSD(t, r, "BTC", 65000)
	}
	if n := e.fk.count("spot"); n != 1 {
		t.Fatalf("concurrent Gets issued %d spot requests, want 1 (singleflight)", n)
	}
}

func TestConcurrencyLimitOfEight(t *testing.T) {
	e := newEnv(t)
	e.fk.slow = 20 * time.Millisecond
	var wants []prices.Want
	for i := 0; i < 40; i++ {
		s := fmt.Sprintf("T%02d", i)
		e.fk.spot[s] = "1.5"
		wants = append(wants, prices.Want{Key: s})
	}
	got := e.svc.Get(ctx, wants)
	if len(got) != 40 {
		t.Fatalf("got %d prices, want 40", len(got))
	}
	if m := e.fk.maxInFlight("spot"); m > 8 || m < 2 {
		t.Fatalf("max concurrent lookups = %d, want 2..8", m)
	}
}

func TestContextCancellationFallsBackToNothingGracefully(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	c, cancel := context.WithCancel(ctx)
	cancel()
	got := e.svc.Get(c, keys("BTC")) // must return promptly without panicking
	if len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// TestLiveShapes feeds responses captured from the real APIs (testdata/) through the parsers.
func TestLiveShapes(t *testing.T) {
	mux := http.NewServeMux()
	serve := func(pattern, file string) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(fixture(t, file))
		})
	}
	serve("GET /cb/v2/prices/BTC-USD/spot", "coinbase_spot_btc.json")
	serve("GET /cb/v2/exchange-rates", "coinbase_rates_sample.json")
	serve("GET /ex/products/BTC-USD/stats", "exchange_stats_btc.json")
	serve("GET /kr/0/public/Ticker", "kraken_ticker_xbtusd.json")
	serve("GET /jup/price/v3", "jupiter_price_v3.json")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	opts := prices.Options{
		CoinbaseURL: srv.URL + "/cb", CoinbaseExchangeURL: srv.URL + "/ex", KrakenURL: srv.URL + "/kr",
		JupiterPriceURL: srv.URL + "/jup/price/v3", Now: func() time.Time { return t0 },
	}
	svc := prices.New(&httpx.Client{HTTP: srv.Client()}, db, opts)

	got := svc.Get(ctx, []prices.Want{
		{Key: "BTC"}, {Key: "EUR"}, {Key: "spl:" + mintSOL, Mint: mintSOL}, {Key: "spl:" + mintJUP, Mint: mintJUP},
	})
	wantUSD(t, got, "BTC", 81100.845)
	wantChange(t, got["BTC"], (81123.7-82315.17)/82315.17*100)
	wantUSD(t, got, "EUR", 1/0.8906156666666667)
	wantUSD(t, got, "spl:"+mintSOL, 103.53533729876874)
	wantChange(t, got["spl:"+mintSOL], -1.8273832227879163)
	wantUSD(t, got, "spl:"+mintJUP, 0.2985198218127832)

	// Kraken fixture through the fallback path (Coinbase spot for XBT-based symbol is 404 here).
	mux2 := http.NewServeMux()
	mux2.HandleFunc("GET /kr/0/public/Ticker", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "kraken_ticker_xbtusd.json"))
	})
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	o2 := opts
	o2.CoinbaseURL, o2.KrakenURL = srv2.URL+"/cb", srv2.URL+"/kr"
	p := wantUSD(t, prices.New(&httpx.Client{HTTP: srv2.Client()}, db, o2).Get(ctx, keys("BTC")), "BTC", 81116.8)
	wantChange(t, p, (81116.8-82135.4)/82135.4*100)
}

func TestDelistedCoinbaseProductFallsThroughToKraken(t *testing.T) {
	e := newEnv(t)
	// Coinbase still quotes JUP's stale spot price after delisting it; Kraken has the market.
	e.fk.spot["JUP"] = "0.000253"
	e.fk.delisted["JUP"] = true
	e.fk.kraken["JUPUSD"] = krakenFake{Key: "JUPUSD", Last: "0.2972", Open: "0.30"}
	got := e.svc.Get(ctx, keys("JUP"))
	wantUSD(t, got, "JUP", 0.2972)
	// The product status is cached: a later lookup doesn't re-check it.
	e.clock.Advance(2 * time.Minute)
	e.svc.Get(ctx, keys("JUP"))
	if e.fk.count("product") != 1 {
		t.Fatalf("product status fetched %d times, want 1 (cached)", e.fk.count("product"))
	}
}

func TestJupiterSkipsMintsWithoutAPriceForAnHour(t *testing.T) {
	e := newEnv(t)
	e.fk.jup[mintBONK] = jupFake{0.00002, nil}
	spam := "Spam1111111111111111111111111111111111111111"
	e.svc.Get(ctx, keys("spl:"+mintBONK, "spl:"+spam))
	e.clock.Advance(2 * time.Minute) // past the TTL: BONK is re-fetched, spam is not
	e.svc.Get(ctx, keys("spl:"+mintBONK, "spl:"+spam))
	reqs := e.fk.jupiterRequests()
	if len(reqs) != 2 || len(reqs[1]) != 1 || reqs[1][0] != mintBONK {
		t.Fatalf("second request should only carry BONK, got %v", reqs)
	}
	e.clock.Advance(time.Hour)
	e.svc.Get(ctx, keys("spl:"+spam))
	if n := len(e.fk.jupiterRequests()); n != 3 {
		t.Fatalf("spam mint should be retried after an hour, requests=%d", n)
	}
}

func TestJupiterCapsChunksPerCall(t *testing.T) {
	e := newEnv(t)
	var ws []prices.Want
	for i := range 2000 {
		ws = append(ws, prices.Want{Key: fmt.Sprintf("spl:Mint%04dxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", i)})
	}
	e.svc.Get(ctx, ws)
	if n := len(e.fk.jupiterRequests()); n > 10 {
		t.Fatalf("made %d Jupiter requests in one call, cap is 10", n)
	}
}

// TestCachedNeedsNoNetwork: Cached answers from stored prices only, so a warm start never
// waits on a price API.
func TestCachedNeedsNoNetwork(t *testing.T) {
	e := newEnv(t)
	e.fk.spot["BTC"] = "65000"
	e.fk.stats["BTC"] = [2]string{"50000", "65000"}
	e.svc.Get(ctx, keys("BTC"))

	svc2 := e.newService() // a restart: empty in-memory cache, same database
	before := e.fk.total()
	got := svc2.Cached(ctx, keys("BTC", "USD", "USDC", "ETH"))
	if n := e.fk.total() - before; n != 0 {
		t.Fatalf("Cached made %d requests", n)
	}
	if p := wantUSD(t, got, "BTC", 65000); !p.Stale {
		t.Errorf("stored price not marked stale: %+v", p)
	}
	wantUSD(t, got, "USD", 1)
	wantUSD(t, got, "USDC", 1)
	if _, ok := got["ETH"]; ok {
		t.Error("ETH has no stored price and must be absent")
	}
}
