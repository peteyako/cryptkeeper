package prices

import (
	"context"
	"strings"
	"time"

	"cryptkeeper/internal/core"
)

// Point is one daily USD price; Day is 00:00 UTC.
type Point struct {
	Day time.Time
	USD float64
}

// Series returns the daily closing USD prices of an asset for the UTC days today-days+1 ..
// today, ascending, built from Historical / HistoricalMintClose (so the price_history cache
// bounds the requests); today's is the latest price. assetKey is a symbol or "spl:<mint>";
// mint is the fallback for symbols Coinbase does not list. Pegged assets are 1 every day.
// A stock ("stock:<TICKER>") has a point for each trading day only.
// Days with no price are omitted and per-day errors are logged and skipped; only a context
// error is returned.
func (s *Service) Series(ctx context.Context, assetKey, mint string, days int) ([]Point, error) {
	key := strings.TrimSpace(assetKey)
	today := utcDay(s.now())
	out := make([]Point, 0, max(days, 0))
	splMint := ""
	if m, ok := strings.CutPrefix(key, "spl:"); ok {
		splMint = m
		if mint == "" {
			mint = m
		}
	}
	pegged := core.Pegged[strings.ToUpper(key)]
	ticker, isStock := core.IsStockKey(key)
	for i := days - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := today.AddDate(0, 0, -i)
		var (
			usd float64
			ok  bool
			err error
		)
		switch {
		case pegged:
			usd, ok = 1, true
		case isStock:
			usd, ok, err = s.stockClose(ctx, ticker, d, true) // trading days only
		case splMint != "":
			usd, ok, err = s.HistoricalMintClose(ctx, splMint, d)
		default:
			usd, ok, err = s.Historical(ctx, key, d)
			if err == nil && !ok && mint != "" {
				usd, ok, err = s.HistoricalMintClose(ctx, mint, d)
			}
		}
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			s.log.Warn("prices: series day failed", "asset", key, "day", d.Format(dayFormat), "err", err)
			continue
		}
		if ok {
			out = append(out, Point{Day: d, USD: usd})
		}
	}
	return out, nil
}
