package prices

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
)

// Stocks, ETFs and funds ("stock:<TICKER>" keys) are priced by Yahoo Finance, which needs no
// key. Quotes come from the batch spark endpoint on YahooURL, up to yahooBatch tickers a
// request; when that fails, from each ticker's chart endpoint on YahooFallbackURL, a separate
// host. Daily closes come from the chart endpoint. Only US-dollar listings are priced.

// Defaults for the Yahoo Finance options.
const (
	DefaultYahooURL         = "https://query1.finance.yahoo.com"
	DefaultYahooFallbackURL = "https://query2.finance.yahoo.com"

	yahooBatch = 20
	// yahooPace spaces requests to Yahoo's hosts, shared by every caller in the process.
	yahooPace = 500 * time.Millisecond
	// stockInfoTTL is how long a ticker's name and type are reused before Yahoo is asked again.
	stockInfoTTL = 24 * time.Hour
	// stockUnknownFor is how long a ticker Yahoo doesn't know is left alone.
	stockUnknownFor = time.Hour
	// stockStaleMax is how old a stock's last good price may be when Yahoo can't be reached:
	// prices don't move while markets are shut, so a long weekend's worth.
	stockStaleMax = 4 * 24 * time.Hour
	// carryDays is how far back a non-trading day looks for the previous close.
	carryDays = 7
)

// Security is what Yahoo Finance says about a ticker.
type Security struct {
	Name     string // "Vanguard Total Stock Market ETF"
	Type     string // "ETF", "EQUITY", "MUTUALFUND", ...
	Currency string // "USD"; listings in other currencies are not priced
}

type stockInfoEntry struct {
	sec Security
	at  time.Time
}

// yahooMeta is the meta block of a spark or chart answer.
type yahooMeta struct {
	Symbol                     string   `json:"symbol"`
	Currency                   string   `json:"currency"`
	InstrumentType             string   `json:"instrumentType"`
	LongName                   string   `json:"longName"`
	ShortName                  string   `json:"shortName"`
	RegularMarketPrice         float64  `json:"regularMarketPrice"`
	RegularMarketTime          int64    `json:"regularMarketTime"`
	RegularMarketChangePercent *float64 `json:"regularMarketChangePercent"`
	ChartPreviousClose         float64  `json:"chartPreviousClose"`
	CurrentTradingPeriod       struct {
		Regular struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"regular"`
	} `json:"currentTradingPeriod"`
}

func (m yahooMeta) security() Security {
	name := m.LongName
	if name == "" {
		name = m.ShortName
	}
	if name == "" {
		name = m.Symbol
	}
	return Security{Name: name, Type: m.InstrumentType, Currency: m.Currency}
}

// quote is the price of a US-dollar listing; ok is false for other currencies and missing
// prices.
func (m yahooMeta) quote(now time.Time) (quote, bool) {
	if m.Currency != "USD" || !positive(m.RegularMarketPrice) {
		return quote{}, false
	}
	q := quote{usd: m.RegularMarketPrice, change: m.RegularMarketChangePercent}
	if q.change == nil {
		q.change = pctChange(m.RegularMarketPrice, m.ChartPreviousClose)
	}
	if m.RegularMarketTime > 0 {
		q.marketTime = time.Unix(m.RegularMarketTime, 0).UTC()
	}
	reg := m.CurrentTradingPeriod.Regular
	q.open = now.Unix() >= reg.Start && now.Unix() < reg.End
	return q, true
}

// Securities looks tickers up on Yahoo Finance: name, type and currency, reused for
// stockInfoTTL. A ticker Yahoo doesn't know is absent; err is set only when Yahoo couldn't be
// asked at all. The prices that come with the answer are cached, so pricing the same tickers
// right after costs no request.
func (s *Service) Securities(ctx context.Context, tickers []string) (map[string]Security, error) {
	now := s.now()
	out := map[string]Security{}
	var ask []string
	s.mu.Lock()
	for _, t := range tickers {
		if _, done := out[t]; done || slices.Contains(ask, t) {
			continue
		}
		if e, ok := s.stockInfo[t]; ok && now.Sub(e.at) < stockInfoTTL {
			out[t] = e.sec
			continue
		}
		if until, ok := s.stockUnknown[t]; ok && now.Before(until) {
			continue
		}
		ask = append(ask, t)
	}
	s.mu.Unlock()
	if len(ask) == 0 {
		return out, nil
	}
	metas, err := s.fetchStocks(ctx, ask)
	fresh := map[string]core.Price{}
	asOf := s.now()
	for t, m := range metas {
		out[t] = m.security()
		if q, ok := m.quote(asOf); ok {
			fresh[core.StockKey(t)] = q.price(asOf)
		}
	}
	s.cachePut(fresh, asOf)
	s.persist(ctx, fresh)
	return out, err
}

// stockQuotes prices tickers (for fetchLive), keyed by ticker.
func (s *Service) stockQuotes(ctx context.Context, tickers []string) map[string]quote {
	metas, err := s.fetchStocks(ctx, tickers)
	if err != nil {
		s.log.Debug("prices: yahoo quotes failed", "err", err)
	}
	now := s.now()
	out := make(map[string]quote, len(metas))
	for t, m := range metas {
		if q, ok := m.quote(now); ok {
			out[t] = q
		}
	}
	return out
}

// fetchStocks asks Yahoo about tickers, batched; a batch that fails is asked again ticker by
// ticker on the fallback host. Names are remembered, and tickers Yahoo answered without are
// remembered as unknown. err is the last failure when no ticker could be asked at all.
func (s *Service) fetchStocks(ctx context.Context, tickers []string) (map[string]yahooMeta, error) {
	out := map[string]yahooMeta{}
	var asked []string
	var lastErr error
	for batch := range slices.Chunk(tickers, yahooBatch) {
		metas, err := s.yahooSpark(ctx, batch)
		if err == nil {
			asked = append(asked, batch...)
			for t, m := range metas {
				out[t] = m
			}
			continue
		}
		s.log.Debug("prices: yahoo spark failed, asking per ticker", "err", err)
		for _, t := range batch {
			m, found, err := s.yahooChartMeta(ctx, t)
			if err != nil {
				lastErr = err
				continue
			}
			asked = append(asked, t)
			if found {
				out[t] = m
			}
		}
	}
	now := s.now()
	s.mu.Lock()
	for _, t := range asked {
		if m, ok := out[t]; ok {
			s.stockInfo[t] = stockInfoEntry{sec: m.security(), at: now}
			delete(s.stockUnknown, t)
		} else {
			s.stockUnknown[t] = now.Add(stockUnknownFor)
		}
	}
	s.mu.Unlock()
	if len(asked) == 0 {
		return out, lastErr
	}
	return out, nil
}

// yahooSpark: GET /v7/finance/spark?symbols=A,B&range=1d&interval=1d ->
// {"spark":{"result":[{"symbol":"A","response":[{"meta":{...}}]}]}}. Unknown tickers are
// left out of the result.
func (s *Service) yahooSpark(ctx context.Context, tickers []string) (map[string]yahooMeta, error) {
	q := url.Values{"symbols": {strings.Join(tickers, ",")}, "range": {"1d"}, "interval": {"1d"}}
	var raw struct {
		Spark struct {
			Result []struct {
				Symbol   string `json:"symbol"`
				Response []struct {
					Meta yahooMeta `json:"meta"`
				} `json:"response"`
			} `json:"result"`
		} `json:"spark"`
	}
	if err := s.c.GetJSON(ctx, s.o.YahooURL+"/v7/finance/spark?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := map[string]yahooMeta{}
	for _, r := range raw.Spark.Result {
		if len(r.Response) == 0 || !slices.Contains(tickers, r.Symbol) {
			continue
		}
		out[r.Symbol] = r.Response[0].Meta
	}
	return out, nil
}

// yahooChartMeta is one ticker's quote from the chart endpoint on the fallback host; found is
// false when Yahoo doesn't know the ticker.
func (s *Service) yahooChartMeta(ctx context.Context, ticker string) (yahooMeta, bool, error) {
	res, found, err := s.yahooChart(ctx, s.o.YahooFallbackURL, ticker, url.Values{"range": {"1d"}, "interval": {"1d"}})
	return res.Meta, found, err
}

type yahooChartResult struct {
	Meta       yahooMeta `json:"meta"`
	Timestamp  []int64   `json:"timestamp"`
	Indicators struct {
		Quote []struct {
			Close []*float64 `json:"close"`
		} `json:"quote"`
	} `json:"indicators"`
}

// yahooChart: GET /v8/finance/chart/{TICKER}?... -> {"chart":{"result":[{...}]}}; a 404 means
// Yahoo doesn't know the ticker (found false, no error).
func (s *Service) yahooChart(ctx context.Context, base, ticker string, q url.Values) (yahooChartResult, bool, error) {
	var raw struct {
		Chart struct {
			Result []yahooChartResult `json:"result"`
		} `json:"chart"`
	}
	err := s.c.GetJSON(ctx, base+"/v8/finance/chart/"+url.PathEscape(ticker)+"?"+q.Encode(), nil, &raw)
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && se.Status == 404 {
			return yahooChartResult{}, false, nil
		}
		return yahooChartResult{}, false, err
	}
	if len(raw.Chart.Result) == 0 {
		return yahooChartResult{}, false, nil
	}
	return raw.Chart.Result[0], true, nil
}

// --- daily closes ---

// stockClose is Historical for a stock: the close of the trading day d (UTC), from
// price_history or one windowed chart fetch. A day without trading (a weekend, a holiday)
// takes the previous close unless exact is set. Today is the live price, which with exact
// counts only when the market traded today.
func (s *Service) stockClose(ctx context.Context, ticker string, day time.Time, exact bool) (float64, bool, error) {
	key := core.StockKey(ticker)
	d := utcDay(day)
	now := s.now()
	today := utcDay(now)
	switch {
	case d.After(today):
		return 0, false, nil
	case d.Equal(today):
		p, ok := s.Get(ctx, []Want{{Key: key}})[key]
		if !ok || (exact && (p.MarketTime.IsZero() || !utcDay(p.MarketTime).Equal(today))) {
			return 0, false, nil
		}
		return p.USD, true, nil
	}
	usd, ok, err := s.histResolve(ctx, key, key, d, time.Time{}, now, func(ctx context.Context) (*histWindow, error) {
		return s.stockHistFetch(ctx, ticker, d, today, now)
	})
	if err != nil || ok || exact {
		return usd, ok, err
	}
	return s.histBefore(ctx, key, d)
}

// stockHistFetch fetches and stores the daily closes of a window around d, as histFetch does
// for coins: from YahooURL, else from the fallback host.
func (s *Service) stockHistFetch(ctx context.Context, ticker string, d, today, now time.Time) (*histWindow, error) {
	key := core.StockKey(ticker)
	last := d.AddDate(0, 0, histForwardDays)
	if y := today.AddDate(0, 0, -1); last.After(y) {
		last = y
	}
	first := last.AddDate(0, 0, -(histWindowDays - 1))
	q := url.Values{
		"period1":  {strconv.FormatInt(first.Unix(), 10)},
		"period2":  {strconv.FormatInt(last.Add(24*time.Hour).Unix(), 10)},
		"interval": {"1d"},
	}
	res, found, err := s.yahooChart(ctx, s.o.YahooURL, ticker, q)
	if err != nil {
		res, found, err = s.yahooChart(ctx, s.o.YahooFallbackURL, ticker, q)
	}
	if err != nil {
		return nil, err
	}
	if !found || res.Meta.Currency != "USD" {
		s.markUnknown(key, now)
		return &histWindow{unknown: true}, nil
	}
	closes := map[string]float64{}
	if len(res.Indicators.Quote) > 0 {
		c := res.Indicators.Quote[0].Close
		for i, ts := range res.Timestamp {
			if i >= len(c) || c[i] == nil || !positive(*c[i]) {
				continue
			}
			if cd := utcDay(time.Unix(ts, 0)); !cd.Before(first) && !cd.After(last) {
				closes[cd.Format(dayFormat)] = *c[i]
			}
		}
	}
	if s.histStore(ctx, key, closes) == nil {
		s.markCovered(key, first, last)
	}
	return &histWindow{first: first, last: last, closes: closes}, nil
}

// histBefore is the latest stored close of asset in the carryDays before d.
func (s *Service) histBefore(ctx context.Context, asset string, d time.Time) (float64, bool, error) {
	var usd float64
	err := s.db.QueryRowContext(ctx, `SELECT usd FROM price_history WHERE asset = ? AND day < ? AND day >= ? ORDER BY day DESC LIMIT 1`,
		asset, d.Format(dayFormat), d.AddDate(0, 0, -carryDays).Format(dayFormat)).Scan(&usd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return usd, positive(usd), nil
}

// isYahooHost reports whether u is one of Yahoo Finance's public hosts, which are paced.
func isYahooHost(u string) (string, bool) {
	p, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	return p.Host, strings.HasSuffix(p.Hostname(), ".finance.yahoo.com")
}
