package prices

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	"cryptkeeper/internal/httpx"
	"cryptkeeper/internal/store"
)

func init() {
	// price_unknown remembers that an upstream does not know an asset (CoinGecko's "coin not
	// found" for a mint, key "cg:<mint>") until a unix time, so restarts do not ask again.
	store.AddMigration("prices-unknown-v1", `CREATE TABLE price_unknown (key TEXT PRIMARY KEY, until INTEGER NOT NULL);`)
}

const (
	// The CoinGecko demo plan serves only the past 365 days.
	cgMaxAgeDays = 365
	// cgNotFoundFor is how long CoinGecko's "coin not found" for a mint is believed: spam tokens
	// never get listed, and a real one listed meanwhile is priced within a week.
	cgNotFoundFor = 7 * 24 * time.Hour
	// cgPauseFor is how long every CoinGecko lookup fails fast after a 429, unless its
	// Retry-After asks for longer.
	cgPauseFor = 5 * time.Minute
)

// errCGPaused is the fail-fast answer while CoinGecko lookups are paused after a 429.
var errCGPaused = errors.New("coingecko: rate limited, lookups paused for a few minutes")

// HistoricalMint returns the USD price of the SPL token mint at the start of the UTC day
// containing day, from CoinGecko's by-contract market chart (the daily point nearest 00:00
// UTC). HistoricalMintClose gives the day's close.
//
// Order: price_history ("spl:"+mint), then days older than 365 days (or in the future) are
// unknown without a request, then the remembered misses (a 404 for a week, across restarts;
// an empty answer for 6h), then one windowed fetch [day-150d, min(day+150d, yesterday)]
// clamped to the last 365 days whose days are all stored. Today is answered from the newest
// point and never stored. ok is false (nil error) for unknown mints and uncovered days;
// 429/5xx surface as errors and cache nothing, and a 429 pauses every CoinGecko lookup.
func (s *Service) HistoricalMint(ctx context.Context, mint string, day time.Time) (usd float64, ok bool, err error) {
	s.cgLoadNotFound(ctx)
	d := utcDay(day)
	now := s.now()
	today := utcDay(now)
	oldest := today.AddDate(0, 0, -cgMaxAgeDays)
	switch {
	case d.After(today):
		return 0, false, nil
	case d.Equal(today):
		return s.cgToday(ctx, mint, "newest", now, func(pts []cgPoint) cgPoint {
			newest := pts[0]
			for _, p := range pts {
				if p.t.After(newest.t) {
					newest = p
				}
			}
			return newest
		})
	}
	return s.histResolve(ctx, "cg:"+mint, "spl:"+mint, d, oldest, now, func(ctx context.Context) (*histWindow, error) {
		return s.cgFetch(ctx, mint, d, today, oldest, now)
	})
}

// HistoricalMintClose returns the closing USD price of the SPL token mint for the UTC day
// containing day, as Historical's Coinbase close and the history rows stamped 23:59:59 mean
// it: the CoinGecko point nearest the following 00:00 UTC. That is HistoricalMint of the next
// day, except that yesterday's close is read from today's points (never stored) and today's
// is the newest price so far.
func (s *Service) HistoricalMintClose(ctx context.Context, mint string, day time.Time) (usd float64, ok bool, err error) {
	d := utcDay(day)
	now := s.now()
	today := utcDay(now)
	switch next := d.AddDate(0, 0, 1); {
	case !d.Before(today):
		return s.HistoricalMint(ctx, mint, d)
	case next.Before(today):
		return s.HistoricalMint(ctx, mint, next)
	}
	s.cgLoadNotFound(ctx)
	return s.cgToday(ctx, mint, "close", now, func(pts []cgPoint) cgPoint {
		best := pts[0]
		for _, p := range pts {
			if absDur(p.t.Sub(today)) < absDur(best.t.Sub(today)) {
				best = p
			}
		}
		return best
	})
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// cgToday answers from the points since yesterday 00:00 UTC, the one pick chooses. Nothing is
// stored; a 404 is remembered like any other.
func (s *Service) cgToday(ctx context.Context, mint, variant string, now time.Time, pick func([]cgPoint) cgPoint) (float64, bool, error) {
	today := utcDay(now)
	return s.histResolveToday(ctx, "cg:"+mint, variant, today, now, func(ctx context.Context) (float64, bool, bool, error) {
		pts, notFound, err := s.cgRange(ctx, mint, today.AddDate(0, 0, -1), now.UTC())
		if notFound {
			s.cgMarkNotFound(ctx, mint, now)
		}
		if err != nil || notFound || len(pts) == 0 {
			return 0, false, false, err
		}
		return pick(pts).usd, true, false, nil
	})
}

func (s *Service) cgFetch(ctx context.Context, mint string, d, today, oldest, now time.Time) (*histWindow, error) {
	last := d.AddDate(0, 0, histForwardDays)
	if y := today.AddDate(0, 0, -1); last.After(y) {
		last = y
	}
	first := d.AddDate(0, 0, -histForwardDays)
	if first.Before(oldest) {
		first = oldest
	}
	pts, notFound, err := s.cgRange(ctx, mint, first, last.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	if notFound {
		s.cgMarkNotFound(ctx, mint, now)
		return &histWindow{unknown: true}, nil
	}
	if len(pts) == 0 {
		s.markUnknown("cg:"+mint, now)
		return &histWindow{unknown: true}, nil
	}
	// Keep, per UTC day, the point nearest its 00:00 UTC (hourly data for ranges <= 90 days).
	closes := map[string]float64{}
	gap := map[string]time.Duration{}
	for _, p := range pts {
		mid := utcDay(p.t.Add(12 * time.Hour))
		if mid.Before(first) || mid.After(last) {
			continue
		}
		k := mid.Format(dayFormat)
		g := absDur(p.t.Sub(mid))
		if old, seen := gap[k]; !seen || g < old {
			gap[k], closes[k] = g, p.usd
		}
	}
	if s.histStore(ctx, "spl:"+mint, closes) == nil {
		s.markCovered("cg:"+mint, first, last)
	}
	return &histWindow{first: first, last: last, closes: closes}, nil
}

type cgPoint struct {
	t   time.Time
	usd float64
}

// cgRange: GET /coins/solana/contract/{mint}/market_chart/range?vs_currency=usd&from=&to= ->
// {"prices": [[ms, usd], ...]}. notFound reports a 404 (unknown contract). After a 429 every
// call fails fast with errCGPaused for cgPauseFor, or the Retry-After if longer.
func (s *Service) cgRange(ctx context.Context, mint string, from, to time.Time) ([]cgPoint, bool, error) {
	s.mu.Lock()
	paused := s.now().Before(s.cgPause)
	s.mu.Unlock()
	if paused {
		return nil, false, errCGPaused
	}
	q := url.Values{
		"vs_currency": {"usd"},
		"from":        {strconv.FormatInt(from.Unix(), 10)},
		"to":          {strconv.FormatInt(to.Unix(), 10)},
	}
	var hdr map[string]string
	if s.o.CoinGeckoKey != nil {
		if k := s.o.CoinGeckoKey(); k != "" {
			hdr = map[string]string{"x-cg-demo-api-key": k}
		}
	}
	var resp struct {
		Prices [][]float64 `json:"prices"`
	}
	u := s.o.CoinGeckoURL + "/coins/solana/contract/" + url.PathEscape(mint) + "/market_chart/range?" + q.Encode()
	if err := s.c.GetJSON(ctx, u, hdr, &resp); err != nil {
		var se *httpx.StatusError
		if errors.As(err, &se) && se.Status == 404 {
			return nil, true, nil
		}
		if errors.As(err, &se) && se.Status == 429 {
			until := s.now().Add(max(cgPauseFor, se.RetryAfter))
			s.mu.Lock()
			if until.After(s.cgPause) {
				s.cgPause = until
			}
			s.mu.Unlock()
			s.log.Warn("prices: CoinGecko rate limit; pausing lookups", "until", until.Format(time.RFC3339))
		}
		return nil, false, err
	}
	out := make([]cgPoint, 0, len(resp.Prices))
	for _, r := range resp.Prices {
		if len(r) >= 2 && positive(r[1]) {
			out = append(out, cgPoint{t: time.UnixMilli(int64(r[0])).UTC(), usd: r[1]})
		}
	}
	return out, false, nil
}

// --- CoinGecko "coin not found", remembered for a week in memory and in price_unknown ---

// cgMarkNotFound remembers that CoinGecko does not know mint.
func (s *Service) cgMarkNotFound(ctx context.Context, mint string, now time.Time) {
	key, until := "cg:"+mint, now.Add(cgNotFoundFor)
	s.mu.Lock()
	s.histBad[key] = until
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO price_unknown (key, until) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET until = excluded.until`, key, until.Unix()); err != nil {
		s.log.Warn("prices: store price_unknown failed", "key", key, "err", err)
	}
}

// cgLoadNotFound loads the remembered answers into the in-memory misses, dropping the
// expired ones. It is done once it has succeeded; a failed load is tried again by the next
// lookup.
func (s *Service) cgLoadNotFound(ctx context.Context) {
	s.cgLoadMu.Lock()
	defer s.cgLoadMu.Unlock()
	if s.cgLoaded {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := s.now().Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM price_unknown WHERE until <= ?`, now); err != nil {
		s.log.Warn("prices: read price_unknown failed", "err", err)
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key, until FROM price_unknown`)
	if err != nil {
		s.log.Warn("prices: read price_unknown failed", "err", err)
		return
	}
	got := map[string]time.Time{}
	for rows.Next() {
		var key string
		var until int64
		if rows.Scan(&key, &until) == nil {
			got[key] = time.Unix(until, 0)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.log.Warn("prices: read price_unknown failed", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, until := range got {
		if until.After(s.histBad[key]) {
			s.histBad[key] = until
		}
	}
	s.cgLoaded = true
}
