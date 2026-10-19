package demo

import (
	"math"
	"time"

	"cryptkeeper/internal/portfolio"
	"cryptkeeper/web/templates"
)

// coinDays normalises a coin-page range and gives its day count.
func coinDays(rng string) (string, int) {
	if d, ok := templates.CoinDays[rng]; ok {
		return rng, d
	}
	return "30D", 30
}

// CoinPage returns the coin page for an asset key and range on invented data; false when
// the asset is not held. With loading set the chart is left empty, as on first paint. The series is a gentle wave that ends at the current price.
func CoinPage(key, rng string, loading bool, now time.Time) (templates.CoinPage, bool) {
	snap := Snapshot(now)
	rng, days := coinDays(rng)
	for _, a := range snap.Assets {
		if a.AssetKey != key {
			continue
		}
		p := templates.CoinPage{
			Shell:    Shell(a.Symbol, "overview", "all", ""),
			AssetKey: a.AssetKey, Symbol: a.Symbol, Name: a.Name, LogoURL: a.LogoURL,
			Price: a.Price, Amount: a.Amount, ValueUSD: a.ValueUSD,
			Range: rng, Loading: loading, Sync: SyncSynced(), Now: now,
		}
		p.Wallets = templates.CoinWalletsOf(snap.Wallets, key)
		if !loading && a.Price != nil {
			p.Points = coinSeries(a.Price.USD, days, now)
			p.Change = templates.CoinChangeOf(p.Points, rng)
		}
		return p, true
	}
	return templates.CoinPage{}, false
}

func coinSeries(last float64, days int, now time.Time) []portfolio.Point {
	pts := make([]portfolio.Point, days)
	for i := range pts {
		x := float64(i) / float64(days-1)
		pts[i] = portfolio.Point{
			T: now.AddDate(0, 0, i-days+1),
			V: round2(last * (0.9 + 0.1*x + 0.04*math.Sin(x*9))),
		}
	}
	pts[days-1].V = last
	return pts
}
