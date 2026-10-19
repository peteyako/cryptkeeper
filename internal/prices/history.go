package prices

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
)

const (
	dayFormat = "2006-01-02"

	// Coinbase Exchange returns at most 300 candles per request, and the bucket starting
	// exactly at `end` is included, so a window is 300 whole days with end = last day + 23:59:59.
	histWindowDays = 300
	// A window extends this far after the requested day (capped at yesterday) and the rest
	// before it, so callers walking activity in either direction hit the warmed cache.
	histForwardDays = 150

	histUnknownFor = 6 * time.Hour
	histMaxCovered = 64
	// histFetchTimeout bounds a shared windowed fetch, which runs detached from the request
	// of whichever caller started it.
	histFetchTimeout = 60 * time.Second
)

// Historical returns the closing USD price of symbol for the UTC day containing day.
// A "stock:<TICKER>" key is a stock's close (the previous close on a day without trading;
// see stockClose).
//
// Closes come from the price_history table, else from Coinbase Exchange daily candles; one
// request warms a window of up to 300 days around the requested day. USD/USDC/USDT are 1.
// ok is false (with a nil error) for symbols Coinbase does not list, SPL keys, days with no
// candle and future days. A day that is still in progress is answered from the live candle
// but never stored.
func (s *Service) Historical(ctx context.Context, symbol string, day time.Time) (usd float64, ok bool, err error) {
	if raw, isStock := core.IsStockKey(strings.TrimSpace(symbol)); isStock {
		if ticker, valid := core.NormalizeTicker(raw); valid {
			return s.stockClose(ctx, ticker, day, false)
		}
		return 0, false, nil
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if core.Pegged[sym] {
		return 1, true, nil
	}
	if !validSymbol(sym) { // also rejects "spl:<mint>": no free history source for SPL tokens
		return 0, false, nil
	}
	d := utcDay(day)
	now := s.now()
	today := utcDay(now)
	switch {
	case d.After(today):
		return 0, false, nil
	case d.Equal(today):
		return s.histResolveToday(ctx, sym, "", today, now, func(ctx context.Context) (float64, bool, bool, error) {
			candles, err := s.fetchCandles(ctx, sym, today, today)
			if err != nil {
				var se *httpx.StatusError
				if errors.As(err, &se) && se.Status == 404 {
					return 0, false, true, nil
				}
				return 0, false, false, err
			}
			for _, c := range candles {
				if utcDay(time.Unix(c.t, 0)).Equal(today) {
					return c.close, true, false, nil
				}
			}
			return 0, false, false, nil
		})
	}
	return s.histResolve(ctx, sym, sym, d, time.Time{}, now, func(ctx context.Context) (*histWindow, error) {
		return s.histFetch(ctx, sym, d, today, now)
	})
}

// histResolve is the lookup shared by Historical and HistoricalMint: the price_history row
// for asset, else (unless d is before oldest, when non-zero) the remembered misses under
// key, else one singleflighted windowed fetch whose stored window answers d. The shared
// fetch runs on its own context, so a caller that gives up does not fail the others.
func (s *Service) histResolve(ctx context.Context, key, asset string, d, oldest, now time.Time,
	fetch func(ctx context.Context) (*histWindow, error)) (float64, bool, error) {
	if usd, ok := s.histDB(ctx, asset, d); ok {
		return usd, true, nil
	}
	if !oldest.IsZero() && d.Before(oldest) {
		return 0, false, nil
	}
	if s.histKnownMissing(key, d, now) {
		return 0, false, nil
	}
	run := func(ctx context.Context) (*histWindow, error) {
		// Another caller may have stored the day while this one waited.
		if usd, ok := s.histDB(ctx, asset, d); ok {
			return &histWindow{first: d, last: d, closes: map[string]float64{d.Format(dayFormat): usd}}, nil
		}
		return fetch(ctx)
	}
	ch := s.sfHist.DoChan(key, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), histFetchTimeout)
		defer cancel()
		return run(fctx)
	})
	var res singleflight.Result
	select {
	case <-ctx.Done():
		return 0, false, ctx.Err()
	case res = <-ch:
	}
	if res.Err != nil {
		return 0, false, res.Err
	}
	w := res.Val.(*histWindow)
	if !w.covers(d) { // joined another caller's fetch for a distant day
		var err error
		if w, err = run(ctx); err != nil {
			return 0, false, err
		}
	}
	usd, ok := w.closes[d.Format(dayFormat)]
	return usd, ok, nil
}

// histResolveToday answers a still-forming day from fetch, which reports the price, whether
// there is one, and whether the upstream does not know the asset at all (remembered as a miss
// under key). Nothing is stored, but an answer is kept in memory for todayTTL, per key and
// variant (which of the day's prices fetch picks), so a page that prices many assets, or
// renders again, does not ask the upstream each time.
func (s *Service) histResolveToday(ctx context.Context, key, variant string, today, now time.Time,
	fetch func(ctx context.Context) (usd float64, ok, unknown bool, err error)) (float64, bool, error) {
	if s.histKnownMissing(key, today, now) {
		return 0, false, nil
	}
	ck := key + "|" + variant
	s.mu.Lock()
	e, hit := s.todayCache[ck]
	s.mu.Unlock()
	if hit && now.Before(e.until) && e.day.Equal(today) {
		return e.usd, e.ok, nil
	}
	usd, ok, unknown, err := fetch(ctx)
	if err != nil {
		return 0, false, err
	}
	if unknown {
		s.markUnknown(key, now)
	}
	s.mu.Lock()
	s.todayCache[ck] = todayEntry{usd: usd, ok: ok, day: today, until: now.Add(todayTTL)}
	s.mu.Unlock()
	return usd, ok, nil
}

// todayTTL is how long an answer for a still-forming day is reused.
const todayTTL = 5 * time.Minute

type todayEntry struct {
	usd   float64
	ok    bool
	day   time.Time
	until time.Time
}

// histWindow is the outcome of one candle fetch: closes by "YYYY-MM-DD" for the UTC days
// first..last inclusive.
type histWindow struct {
	first, last time.Time
	closes      map[string]float64
	unknown     bool // the product does not exist
}

func (w *histWindow) covers(d time.Time) bool {
	return w.unknown || (!d.Before(w.first) && !d.After(w.last))
}

func utcDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (s *Service) histFetch(ctx context.Context, sym string, d, today, now time.Time) (*histWindow, error) {
	last := d.AddDate(0, 0, histForwardDays)
	if y := today.AddDate(0, 0, -1); last.After(y) {
		last = y // today's candle is still forming
	}
	first := last.AddDate(0, 0, -(histWindowDays - 1))

	candles, err := s.fetchCandles(ctx, sym, first, last)
	if err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && se.Status == 404 {
			s.markUnknown(sym, now)
			return &histWindow{unknown: true}, nil
		}
		return nil, err
	}
	closes := make(map[string]float64, len(candles))
	for _, c := range candles {
		if cd := utcDay(time.Unix(c.t, 0)); !cd.Before(first) && !cd.After(last) {
			closes[cd.Format(dayFormat)] = c.close
		}
	}
	if s.histStore(ctx, sym, closes) == nil {
		s.markCovered(sym, first, last) // only a stored window is known to have no other days
	}
	return &histWindow{first: first, last: last, closes: closes}, nil
}

type candle struct {
	t     int64
	close float64
}

// fetchCandles: GET /products/{SYM}-USD/candles?granularity=86400&start=..&end=.. ->
// [[time, low, high, open, close, volume], ...] newest first, buckets without trades omitted.
// It requests the UTC days first..last inclusive.
func (s *Service) fetchCandles(ctx context.Context, sym string, first, last time.Time) ([]candle, error) {
	q := url.Values{
		"granularity": {"86400"},
		"start":       {first.UTC().Format(time.RFC3339)},
		"end":         {last.UTC().Add(24*time.Hour - time.Second).Format(time.RFC3339)},
	}
	var raw [][]float64
	if err := s.c.GetJSON(ctx, s.o.CoinbaseExchangeURL+"/products/"+sym+"-USD/candles?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]candle, 0, len(raw))
	for _, r := range raw {
		if len(r) >= 5 && positive(r[4]) {
			out = append(out, candle{t: int64(r[0]), close: r[4]})
		}
	}
	return out, nil
}

// --- price_history table (asset, day "YYYY-MM-DD" UTC, usd) ---

func (s *Service) histDB(ctx context.Context, sym string, d time.Time) (float64, bool) {
	var usd float64
	err := s.db.QueryRowContext(ctx, `SELECT usd FROM price_history WHERE asset = ? AND day = ?`, sym, d.Format(dayFormat)).Scan(&usd)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("prices: read price_history failed", "asset", sym, "err", err)
		}
		return 0, false
	}
	return usd, positive(usd)
}

// histStore writes closes in one transaction; a failure is logged and returned.
func (s *Service) histStore(ctx context.Context, sym string, closes map[string]float64) (err error) {
	if len(closes) == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			s.log.Warn("prices: store price_history failed", "asset", sym, "err", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO price_history (asset, day, usd) VALUES (?, ?, ?)
		ON CONFLICT(asset, day) DO UPDATE SET usd = excluded.usd`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for day, usd := range closes {
		if _, err := stmt.ExecContext(ctx, sym, day, usd); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- remembered misses, so long activity lists do not re-ask for the same gaps ---

func (s *Service) markUnknown(sym string, now time.Time) {
	s.mu.Lock()
	s.histBad[sym] = now.Add(histUnknownFor)
	s.mu.Unlock()
}

func (s *Service) markCovered(sym string, first, last time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cov := append(s.histCov[sym], [2]time.Time{first, last})
	if len(cov) > histMaxCovered {
		cov = cov[len(cov)-histMaxCovered:]
	}
	s.histCov[sym] = cov
}

// histKnownMissing reports whether a lookup that just missed the table is already known to
// have no data: the symbol is unlisted, or d lies in a window that was fetched successfully.
func (s *Service) histKnownMissing(sym string, d, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until, ok := s.histBad[sym]; ok {
		if now.Before(until) {
			return true
		}
		delete(s.histBad, sym)
	}
	for _, r := range s.histCov[sym] {
		if !d.Before(r[0]) && !d.After(r[1]) {
			return true
		}
	}
	return false
}
