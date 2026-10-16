package prices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/httpx"
)

// fiatCodes are the currencies priced from Coinbase's exchange-rates table. Presence in that
// table is not enough: it also lists crypto assets (BTC, ADA, ...), and a few ISO codes
// double as crypto tickers (MNT is Mantle, ALL, TOP, BOB, SOS ...), which are left out here
// so they are priced as crypto.
var fiatCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(`
		EUR GBP CAD JPY CHF AUD NZD SGD HKD KRW CNY INR BRL MXN ZAR TRY SEK NOK DKK PLN CZK HUF
		ILS AED SAR QAR THB PHP IDR MYR VND TWD RUB UAH NGN KES EGP CLP COP ARS PEN RON BGN ISK
		PKR BDT LKR`) {
		m[c] = true
	}
	return m
}()

type fxCache struct {
	m  map[string]float64 // fiat code -> units per 1 USD
	at time.Time
}

// fxRates returns fiat units per USD from Coinbase /v2/exchange-rates?currency=USD, fetched at
// most once per TTL no matter how many fiat codes are priced.
func (s *Service) fxRates(ctx context.Context) (map[string]float64, error) {
	s.mu.Lock()
	if s.fx.m != nil && s.now().Sub(s.fx.at) < s.o.TTL {
		m := s.fx.m
		s.mu.Unlock()
		return m, nil
	}
	s.mu.Unlock()

	v, err, _ := s.sfFX.Do("fx", func() (any, error) {
		var r struct {
			Data struct {
				Rates map[string]string `json:"rates"`
			} `json:"data"`
		}
		if err := s.c.GetJSON(ctx, s.o.CoinbaseURL+"/v2/exchange-rates?currency=USD", nil, &r); err != nil {
			return nil, err
		}
		m := make(map[string]float64)
		for code := range fiatCodes {
			if f, err := parsePrice(r.Data.Rates[code]); err == nil {
				m[code] = f
			}
		}
		if len(m) == 0 {
			return nil, errors.New("coinbase exchange-rates: no fiat rates in response")
		}
		s.mu.Lock()
		s.fx = fxCache{m: m, at: s.now()}
		s.mu.Unlock()
		return m, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(map[string]float64), nil
}

func parsePrice(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	if !positive(f) {
		return 0, fmt.Errorf("non-positive price %q", s)
	}
	return f, nil
}

// --- Coinbase ---

// coinbaseSpot: GET /v2/prices/{SYM}-USD/spot -> {"data":{"amount":"81099.195","base":"BTC","currency":"USD"}}.
// Unknown products answer 404.
func (s *Service) coinbaseSpot(ctx context.Context, sym string) (float64, error) {
	var r struct {
		Data struct {
			Amount string `json:"amount"`
		} `json:"data"`
	}
	if err := s.c.GetJSON(ctx, s.o.CoinbaseURL+"/v2/prices/"+sym+"-USD/spot", nil, &r); err != nil {
		return 0, err
	}
	return parsePrice(r.Data.Amount)
}

// coinbaseChange: Coinbase Exchange GET /products/{SYM}-USD/stats -> {"open":"..","last":"..",...}.
// Returns (last-open)/open*100, or nil when unavailable.
func (s *Service) coinbaseChange(ctx context.Context, sym string) *float64 {
	var r struct {
		Open string `json:"open"`
		Last string `json:"last"`
	}
	if err := s.c.GetJSON(ctx, s.o.CoinbaseExchangeURL+"/products/"+sym+"-USD/stats", nil, &r); err != nil {
		return nil
	}
	open, err1 := parsePrice(r.Open)
	last, err2 := parsePrice(r.Last)
	if err1 != nil || err2 != nil {
		return nil
	}
	return pctChange(last, open)
}

// coinbaseTradable reports whether Coinbase still trades SYM-USD. Coinbase keeps quoting a
// stale v2 spot price for delisted products (JUP sat ~1000x below market), so a spot price
// only counts when the Exchange product is "online". A product the Exchange doesn't know
// (404, retail-only assets) or a failed lookup doesn't block the spot price. Cached 24h.
func (s *Service) coinbaseTradable(ctx context.Context, sym string) bool {
	now := s.now()
	s.mu.Lock()
	if e, ok := s.cbStatus[sym]; ok && now.Sub(e.at) < 24*time.Hour {
		s.mu.Unlock()
		return e.ok
	}
	s.mu.Unlock()
	var r struct {
		Status          string `json:"status"`
		TradingDisabled bool   `json:"trading_disabled"`
	}
	err := s.c.GetJSON(ctx, s.o.CoinbaseExchangeURL+"/products/"+sym+"-USD", nil, &r)
	var se *httpx.StatusError
	switch {
	case err == nil:
		ok := strings.EqualFold(r.Status, "online") && !r.TradingDisabled
		s.mu.Lock()
		s.cbStatus[sym] = statusEntry{ok: ok, at: now}
		s.mu.Unlock()
		return ok
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		s.mu.Lock()
		s.cbStatus[sym] = statusEntry{ok: true, at: now}
		s.mu.Unlock()
		return true
	}
	return true // transient failure: don't cache, don't block
}

type statusEntry struct {
	ok bool
	at time.Time
}

// --- Kraken ---

// krakenTicker: GET /0/public/Ticker?pair={SYM}USD. Unknown pairs answer HTTP 200 with
// {"error":["EQuery:Unknown asset pair"]}. The result has one key (XXBTZUSD, XDGUSD, ...),
// whose c[0] is the last trade and o today's open.
func (s *Service) krakenTicker(ctx context.Context, sym string) (quote, error) {
	if sym == "BTC" {
		sym = "XBT"
	}
	var r struct {
		Error  []string `json:"error"`
		Result map[string]struct {
			C []string        `json:"c"`
			O json.RawMessage `json:"o"`
		} `json:"result"`
	}
	if err := s.c.GetJSON(ctx, s.o.KrakenURL+"/0/public/Ticker?pair="+sym+"USD", nil, &r); err != nil {
		return quote{}, err
	}
	if len(r.Error) > 0 {
		return quote{}, fmt.Errorf("kraken: %s", strings.Join(r.Error, "; "))
	}
	if len(r.Result) != 1 {
		return quote{}, fmt.Errorf("kraken: %d result entries for %sUSD", len(r.Result), sym)
	}
	for _, t := range r.Result {
		if len(t.C) == 0 {
			return quote{}, errors.New("kraken: missing last trade")
		}
		last, err := parsePrice(t.C[0])
		if err != nil {
			return quote{}, fmt.Errorf("kraken: last trade: %w", err)
		}
		q := quote{usd: last}
		var open string
		if json.Unmarshal(t.O, &open) == nil {
			if o, err := parsePrice(open); err == nil {
				q.change = pctChange(last, o)
			}
		}
		return q, nil
	}
	return quote{}, errors.New("kraken: empty result")
}

// --- Jupiter ---

// jupiter prices mints in as few requests as possible (chunks of 50). Mints Jupiter does not
// know, and chunks that fail, are simply absent from the result. Identical concurrent sets
// share one fetch.
func (s *Service) jupiter(ctx context.Context, mints []string) map[string]quote {
	key := append([]string(nil), mints...)
	sort.Strings(key)
	v, _, _ := s.sfJup.Do(strings.Join(key, ","), func() (any, error) {
		return s.jupiterFetch(ctx, mints), nil
	})
	return v.(map[string]quote)
}

// jupiterFetch prices mints in the order given (callers put the tokens that matter first),
// in at most maxJupiterChunks requests, stopping at the first failure (usually a 429).
// Mints Jupiter answers without a price (spam airdrops, dead tokens) are remembered for an
// hour so they aren't re-requested on every refresh.
func (s *Service) jupiterFetch(ctx context.Context, mints []string) map[string]quote {
	now := s.now()
	todo := make([]string, 0, len(mints))
	s.mu.Lock()
	for _, m := range mints {
		if t, ok := s.noPrice[m]; ok && now.Sub(t) < time.Hour {
			continue
		}
		todo = append(todo, m)
	}
	s.mu.Unlock()
	out := make(map[string]quote, len(todo))
	for i, n := 0, 0; i < len(todo) && n < maxJupiterChunks; i, n = i+jupiterChunk, n+1 {
		chunk := todo[i:min(i+jupiterChunk, len(todo))]
		var raw map[string]json.RawMessage
		if err := s.c.GetJSON(ctx, s.jupiterURL(chunk), nil, &raw); err != nil {
			s.log.Debug("prices: jupiter failed", "mints", len(chunk), "err", err)
			break
		}
		s.mu.Lock()
		for _, m := range chunk {
			var e struct {
				UsdPrice       *float64 `json:"usdPrice"`
				PriceChange24h *float64 `json:"priceChange24h"`
			}
			if json.Unmarshal(raw[m], &e) != nil || e.UsdPrice == nil || !positive(*e.UsdPrice) {
				s.noPrice[m] = now
				continue
			}
			delete(s.noPrice, m)
			q := quote{usd: *e.UsdPrice}
			q.change = e.PriceChange24h
			out[m] = q
		}
		s.mu.Unlock()
	}
	return out
}

func (s *Service) jupiterURL(mints []string) string {
	esc := make([]string, len(mints))
	for i, m := range mints {
		esc[i] = url.QueryEscape(m)
	}
	sep := "?"
	if strings.Contains(s.o.JupiterPriceURL, "?") {
		sep = "&"
	}
	return s.o.JupiterPriceURL + sep + "ids=" + strings.Join(esc, ",")
}
