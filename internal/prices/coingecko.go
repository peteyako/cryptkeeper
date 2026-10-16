package prices

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	"crypto-portfolio/internal/httpx"
)

// The CoinGecko demo plan serves only the past 365 days.
const cgMaxAgeDays = 365

// HistoricalMint returns the USD price of the SPL token mint for the UTC day containing day,
// from CoinGecko's by-contract market chart (the daily point nearest 00:00 UTC).
//
// Order: price_history ("spl:"+mint), then days older than 365 days (or in the future) are
// unknown without a request, then the 6h negative cache, then one windowed fetch
// [day-150d, min(day+150d, yesterday)] clamped to the last 365 days whose days are all
// stored. Today is answered from the newest point and never stored. ok is false (nil
// error) for unknown mints and uncovered days; 429/5xx surface as errors and cache nothing.
func (s *Service) HistoricalMint(ctx context.Context, mint string, day time.Time) (usd float64, ok bool, err error) {
	d := utcDay(day)
	now := s.now()
	today := utcDay(now)
	oldest := today.AddDate(0, 0, -cgMaxAgeDays)
	bad := "cg:" + mint
	switch {
	case d.After(today):
		return 0, false, nil
	case d.Equal(today):
		return s.histResolveToday(ctx, bad, today, now, func(ctx context.Context) (float64, bool, bool, error) {
			pts, notFound, err := s.cgRange(ctx, mint, today.AddDate(0, 0, -1), now.UTC())
			if err != nil || notFound || len(pts) == 0 {
				return 0, false, notFound, err
			}
			newest := pts[0]
			for _, p := range pts {
				if p.t.After(newest.t) {
					newest = p
				}
			}
			return newest.usd, true, false, nil
		})
	}
	return s.histResolve(ctx, bad, "spl:"+mint, d, oldest, now, func(ctx context.Context) (*histWindow, error) {
		return s.cgFetch(ctx, mint, d, today, oldest, now)
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
	if notFound || len(pts) == 0 {
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
		g := p.t.Sub(mid)
		if g < 0 {
			g = -g
		}
		if old, seen := gap[k]; !seen || g < old {
			gap[k], closes[k] = g, p.usd
		}
	}
	s.histStore(ctx, "spl:"+mint, closes)
	s.markCovered("cg:"+mint, first, last)
	return &histWindow{first: first, last: last, closes: closes}, nil
}

type cgPoint struct {
	t   time.Time
	usd float64
}

// cgRange: GET /coins/solana/contract/{mint}/market_chart/range?vs_currency=usd&from=&to= ->
// {"prices": [[ms, usd], ...]}. notFound reports a 404 (unknown contract).
func (s *Service) cgRange(ctx context.Context, mint string, from, to time.Time) ([]cgPoint, bool, error) {
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
