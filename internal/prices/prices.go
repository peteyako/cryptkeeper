// Package prices resolves USD spot prices (with 24h change) and historical daily closes.
//
// Spot chain per asset key, first success wins:
//
//   - "USD": constant 1.
//   - fiat codes (EUR, GBP, ...): Coinbase /v2/exchange-rates, price = 1/rate.
//   - crypto symbols: Coinbase spot, then Kraken Ticker, then Jupiter by mint (only when the
//     caller supplied one), then, for USDC/USDT only, the 1.0 peg.
//   - "spl:<mint>": Jupiter only. Symbol sources are never consulted because scam tokens
//     reuse real tickers.
//   - "stock:<TICKER>": Yahoo Finance (see stocks.go). Its last-good price is usable for
//     stockStaleMax, since markets close for whole weekends.
//
// Every live price is cached in memory for Options.TTL and written to the lastgood_price table.
// When the chain fails, the last-good row is served marked Stale until it is older than
// Options.StaleMax.
package prices

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
	"cryptkeeper/internal/store"
)

// Defaults for Options.
const (
	DefaultCoinbaseURL         = "https://api.coinbase.com"
	DefaultCoinbaseExchangeURL = "https://api.exchange.coinbase.com"
	DefaultKrakenURL           = "https://api.kraken.com"
	DefaultJupiterPriceURL     = "https://lite-api.jup.ag/price/v3"
	DefaultCoinGeckoURL        = "https://api.coingecko.com/api/v3"
	DefaultTTL                 = 60 * time.Second
	DefaultStaleMax            = 24 * time.Hour

	lookupConcurrency = 8
	jupiterChunk      = 50
	maxJupiterChunks  = 10 // per Get: 500 mints; a spam-heavy wallet can't exhaust the rate limit
	cacheSweepAt      = 4096
)

// Want is one asset to price.
type Want struct {
	// Key is "BTC", "EUR", "USDC" or "spl:<mint>". It is also the key of the result map.
	Key string
	// Mint is set for any SPL token, including ones mapped to a symbol (USDC, JUP), so Jupiter
	// can serve as a fallback. For "spl:" keys the mint embedded in the key is used.
	Mint string
}

// Options configures a Service. Zero values select the defaults.
type Options struct {
	CoinbaseURL         string // default https://api.coinbase.com
	CoinbaseExchangeURL string // default https://api.exchange.coinbase.com
	KrakenURL           string // default https://api.kraken.com
	JupiterPriceURL     string // default https://lite-api.jup.ag/price/v3
	CoinGeckoURL        string // default https://api.coingecko.com/api/v3
	YahooURL            string // default https://query1.finance.yahoo.com
	YahooFallbackURL    string // default https://query2.finance.yahoo.com
	// CoinGeckoKey returns the CoinGecko demo API key, or "" for none. Called per request.
	CoinGeckoKey func() string
	TTL          time.Duration // in-memory cache TTL, default 60s
	StaleMax     time.Duration // last-good usable up to this age, default 24h
	Now          func() time.Time
	// Logger receives per-source failures at debug level. Defaults to slog.Default().
	Logger *slog.Logger
}

// Service looks prices up. It is safe for concurrent use.
type Service struct {
	c   *httpx.Client
	db  *store.DB
	o   Options
	log *slog.Logger

	sfSym  singleflight.Group // per asset key: Coinbase -> Kraken chain
	sfJup  singleflight.Group // per sorted mint set
	sfFX   singleflight.Group // exchange-rates fetch
	sfHist singleflight.Group // per historical symbol

	cgLoadMu sync.Mutex // guards cgLoaded: the remembered CoinGecko "coin not found" answers are loaded
	cgLoaded bool       // once, a failed load is retried by the next lookup

	mu         sync.Mutex
	cache      map[string]cacheEntry
	fx         fxCache
	histCov    map[string][][2]time.Time // symbol -> UTC-day ranges already fetched
	histBad    map[string]time.Time      // symbol -> unknown-until
	cbStatus   map[string]statusEntry    // symbol -> Coinbase Exchange product tradable?
	noPrice    map[string]time.Time      // mint -> when Jupiter last had no price for it
	todayCache map[string]todayEntry     // "key|variant" -> a still-forming day's answer, for todayTTL
	cgPause    time.Time                 // CoinGecko lookups fail fast until then (after a 429)

	stockInfo    map[string]stockInfoEntry // ticker -> Yahoo's name and type
	stockUnknown map[string]time.Time      // ticker -> not asked again until
}

type cacheEntry struct {
	p  core.Price
	at time.Time
}

type quote struct {
	usd    float64
	change *float64
	// Stocks only: when the exchange set the price, and whether its session was open.
	marketTime time.Time
	open       bool
}

func (q quote) price(asOf time.Time) core.Price {
	return core.Price{USD: q.usd, AsOf: asOf, Change24h: q.change, MarketTime: q.marketTime, MarketOpen: q.open}
}

// New returns a Service. db must not be nil; c may be nil to use httpx.New().
func New(c *httpx.Client, db *store.DB, opts Options) *Service {
	if c == nil {
		c = httpx.New()
	}
	o := opts
	o.CoinbaseURL = urlOrDefault(o.CoinbaseURL, DefaultCoinbaseURL)
	o.CoinbaseExchangeURL = urlOrDefault(o.CoinbaseExchangeURL, DefaultCoinbaseExchangeURL)
	o.KrakenURL = urlOrDefault(o.KrakenURL, DefaultKrakenURL)
	o.JupiterPriceURL = urlOrDefault(o.JupiterPriceURL, DefaultJupiterPriceURL)
	o.CoinGeckoURL = urlOrDefault(o.CoinGeckoURL, DefaultCoinGeckoURL)
	o.YahooURL = urlOrDefault(o.YahooURL, DefaultYahooURL)
	o.YahooFallbackURL = urlOrDefault(o.YahooFallbackURL, DefaultYahooFallbackURL)
	for _, u := range []string{o.YahooURL, o.YahooFallbackURL} {
		if host, ok := isYahooHost(u); ok {
			httpx.SetPace(host, yahooPace)
		}
	}
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.StaleMax <= 0 {
		o.StaleMax = DefaultStaleMax
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Service{
		c: c, db: db, o: o, log: lg,
		cache:      map[string]cacheEntry{},
		histCov:    map[string][][2]time.Time{},
		histBad:    map[string]time.Time{},
		cbStatus:   map[string]statusEntry{},
		noPrice:    map[string]time.Time{},
		todayCache: map[string]todayEntry{},

		stockInfo:    map[string]stockInfoEntry{},
		stockUnknown: map[string]time.Time{},
	}
}

func urlOrDefault(u, def string) string {
	if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
		return u
	}
	return def
}

func (s *Service) now() time.Time { return s.o.Now() }

// Get prices wants. The result is keyed by Want.Key; an asset with no live price and no
// usable last-good value is absent. One failing asset never affects the others.
func (s *Service) Get(ctx context.Context, wants []Want) map[string]core.Price {
	out := make(map[string]core.Price, len(wants))
	now := s.now()

	var todo []Want
	for _, w := range dedupe(wants) {
		if w.Key == "USD" {
			out[w.Key] = core.Price{USD: 1, AsOf: now}
			continue
		}
		if p, ok := s.cacheGet(w.Key, now); ok {
			out[w.Key] = p
			continue
		}
		todo = append(todo, w)
	}
	if len(todo) == 0 {
		return out
	}

	live := s.fetchLive(ctx, todo)
	asOf := s.now()
	fresh := make(map[string]core.Price, len(live))
	for k, q := range live {
		fresh[k] = q.price(asOf)
		out[k] = fresh[k]
	}
	s.cachePut(fresh, asOf)
	s.persist(ctx, fresh)

	for _, w := range todo {
		if _, ok := out[w.Key]; ok {
			continue
		}
		if _, isSPL := core.IsSPLKey(w.Key); !isSPL && core.Pegged[strings.ToUpper(w.Key)] {
			// Stablecoin with every source down: assume the peg. Not stored or cached.
			out[w.Key] = core.Price{USD: 1, AsOf: asOf}
			continue
		}
		if p, ok := s.lastGood(ctx, w.Key, asOf); ok {
			out[w.Key] = p
		}
	}
	return out
}

// Cached prices wants from what is already known, without network calls: USD, the
// in-memory cache, pegged stablecoins and the stored last-good prices (marked stale). An
// asset with none of those is absent. A warm start after a restart uses it.
func (s *Service) Cached(ctx context.Context, wants []Want) map[string]core.Price {
	out := make(map[string]core.Price, len(wants))
	now := s.now()
	for _, w := range dedupe(wants) {
		if w.Key == "USD" {
			out[w.Key] = core.Price{USD: 1, AsOf: now}
			continue
		}
		if p, ok := s.cacheGet(w.Key, now); ok {
			out[w.Key] = p
			continue
		}
		if p, ok := s.lastGood(ctx, w.Key, now); ok {
			out[w.Key] = p
			continue
		}
		if _, isSPL := core.IsSPLKey(w.Key); !isSPL && core.Pegged[strings.ToUpper(w.Key)] {
			out[w.Key] = core.Price{USD: 1, AsOf: now}
		}
	}
	return out
}

// dedupe drops empty and repeated keys, keeping the first position and any non-empty Mint.
func dedupe(wants []Want) []Want {
	idx := make(map[string]int, len(wants))
	out := make([]Want, 0, len(wants))
	for _, w := range wants {
		if strings.TrimSpace(w.Key) == "" {
			continue
		}
		if i, ok := idx[w.Key]; ok {
			if out[i].Mint == "" {
				out[i].Mint = w.Mint
			}
			continue
		}
		idx[w.Key] = len(out)
		out = append(out, w)
	}
	return out
}

func mintOf(w Want) string {
	if m, ok := core.IsSPLKey(w.Key); ok {
		return m
	}
	return w.Mint
}

// fetchLive runs the live chains for todo and returns the quotes that succeeded.
//
// Anything with a Solana mint is priced by mint on Jupiter first: the mint is the token's
// real identity, and exchange tickers can be stale (Coinbase's delisted JUP product still
// quotes ~1000x too low). Mapped symbols that Jupiter can't price fall back to the symbol
// chain; "spl:" keys never do (scam tokens reuse popular tickers).
func (s *Service) fetchLive(ctx context.Context, todo []Want) map[string]quote {
	res := make(map[string]quote, len(todo))
	var bySymbol []Want
	seen := map[string]bool{}
	var mints, tickers []string
	for _, w := range todo {
		if t, ok := core.IsStockKey(w.Key); ok {
			tickers = append(tickers, t)
			continue
		}
		if m := mintOf(w); m != "" {
			if !seen[m] {
				seen[m] = true
				mints = append(mints, m)
			}
			continue
		}
		bySymbol = append(bySymbol, w)
	}
	if len(tickers) > 0 {
		for t, q := range s.stockQuotes(ctx, tickers) {
			res[core.StockKey(t)] = q
		}
	}
	if len(mints) > 0 {
		jq := s.jupiter(ctx, mints)
		for _, w := range todo {
			m := mintOf(w)
			if m == "" {
				continue
			}
			if q, ok := jq[m]; ok {
				res[w.Key] = q
			} else if _, isSPL := core.IsSPLKey(w.Key); !isSPL {
				bySymbol = append(bySymbol, w)
			}
		}
	}

	var (
		mu sync.Mutex
		g  errgroup.Group
	)
	g.SetLimit(lookupConcurrency)
	for _, w := range bySymbol {
		g.Go(func() error {
			if q, ok := s.symbolQuote(ctx, w.Key); ok {
				mu.Lock()
				res[w.Key] = q
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait() // lookups never return errors
	return res
}

// symbolQuote runs the symbol chain (fiat, or Coinbase then Kraken), deduplicated per key.
func (s *Service) symbolQuote(ctx context.Context, key string) (quote, bool) {
	type result struct {
		q  quote
		ok bool
	}
	v, _, _ := s.sfSym.Do(key, func() (any, error) {
		q, ok := s.lookupSymbol(ctx, strings.ToUpper(strings.TrimSpace(key)))
		return result{q, ok}, nil
	})
	r := v.(result)
	return r.q, r.ok
}

func (s *Service) lookupSymbol(ctx context.Context, sym string) (quote, bool) {
	if !validSymbol(sym) {
		return quote{}, false
	}
	if fiatCodes[sym] {
		rates, err := s.fxRates(ctx)
		if err != nil {
			s.log.Debug("prices: exchange rates failed", "symbol", sym, "err", err)
			return quote{}, false
		}
		if r := rates[sym]; r > 0 {
			return quote{usd: 1 / r}, true
		}
		return quote{}, false
	}
	usd, err := s.coinbaseSpot(ctx, sym)
	if err == nil && s.coinbaseTradable(ctx, sym) {
		return quote{usd: usd, change: s.coinbaseChange(ctx, sym)}, true
	}
	if err == nil {
		s.log.Debug("prices: coinbase product not online, ignoring its spot price", "symbol", sym)
	} else {
		s.log.Debug("prices: coinbase spot failed", "symbol", sym, "err", err)
	}
	q, err := s.krakenTicker(ctx, sym)
	if err == nil {
		return q, true
	}
	s.log.Debug("prices: kraken ticker failed", "symbol", sym, "err", err)
	return quote{}, false
}

// validSymbol accepts only [A-Z0-9]{1,20}; anything else could alter the upstream URL path.
func validSymbol(sym string) bool {
	if sym == "" || len(sym) > 20 {
		return false
	}
	for i := 0; i < len(sym); i++ {
		c := sym[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func positive(f float64) bool { return f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f) }

func pctChange(last, open float64) *float64 {
	if !positive(last) || !positive(open) {
		return nil
	}
	c := (last - open) / open * 100
	return &c
}

// --- in-memory cache ---

func (s *Service) cacheGet(key string, now time.Time) (core.Price, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok || now.Sub(e.at) >= s.o.TTL {
		return core.Price{}, false
	}
	return e.p, true
}

func (s *Service) cachePut(fresh map[string]core.Price, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) > cacheSweepAt {
		for k, e := range s.cache {
			if at.Sub(e.at) >= s.o.TTL {
				delete(s.cache, k)
			}
		}
	}
	for k, p := range fresh {
		s.cache[k] = cacheEntry{p: p, at: at}
	}
}

// --- last-good persistence (lastgood_price: asset, usd, change24h, as_of unix seconds) ---

func (s *Service) persist(ctx context.Context, fresh map[string]core.Price) {
	if len(fresh) == 0 {
		return
	}
	// Detach from cancellation: prices already fetched should still be saved.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.log.Warn("prices: persist last-good failed", "err", err)
		return
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO lastgood_price (asset, usd, change24h, as_of) VALUES (?, ?, ?, ?)
		ON CONFLICT(asset) DO UPDATE SET usd = excluded.usd, change24h = excluded.change24h, as_of = excluded.as_of`)
	if err != nil {
		s.log.Warn("prices: persist last-good failed", "err", err)
		return
	}
	defer stmt.Close()
	for k, p := range fresh {
		var chg any
		if p.Change24h != nil {
			chg = *p.Change24h
		}
		if _, err := stmt.ExecContext(ctx, k, p.USD, chg, p.AsOf.Unix()); err != nil {
			s.log.Warn("prices: persist last-good failed", "asset", k, "err", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.log.Warn("prices: persist last-good failed", "err", err)
	}
}

func (s *Service) lastGood(ctx context.Context, key string, now time.Time) (core.Price, bool) {
	var (
		usd  float64
		chg  sql.NullFloat64
		asOf int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT usd, change24h, as_of FROM lastgood_price WHERE asset = ?`, key).Scan(&usd, &chg, &asOf)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("prices: read last-good failed", "asset", key, "err", err)
		}
		return core.Price{}, false
	}
	at := time.Unix(asOf, 0).UTC()
	maxAge := s.o.StaleMax
	if _, ok := core.IsStockKey(key); ok {
		maxAge = max(maxAge, stockStaleMax)
	}
	if now.Sub(at) > maxAge || !positive(usd) {
		return core.Price{}, false
	}
	p := core.Price{USD: usd, AsOf: at, Stale: true}
	if chg.Valid {
		c := chg.Float64
		p.Change24h = &c
	}
	return p, true
}
