package server

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"

	"cryptkeeper/internal/portfolio"
	"cryptkeeper/internal/prices"
	"cryptkeeper/web/templates"
)

// coinSeriesTimeout bounds one on-demand price series load.
const coinSeriesTimeout = 30 * time.Second

// priceSeries is the optional daily price series behind the coin chart. *prices.Service
// implements it next to HistoricalPrices, so Deps.Historical serves both; without it the
// chart is empty.
type priceSeries interface {
	Series(ctx context.Context, assetKey, mint string, days int) ([]prices.Point, error)
}

const defaultCoinRange = "30D"

// coinPage builds the coin page for an asset key (path-escaped in the URL) and range.
// ok is false when the asset is not in the current holdings.
func (s *Server) coinPage(r *http.Request, rng string, withSeries bool) (templates.CoinPage, bool) {
	key, err := url.PathUnescape(chi.URLParam(r, "asset"))
	if err != nil {
		return templates.CoinPage{}, false
	}
	if _, ok := templates.CoinDays[rng]; !ok {
		rng = defaultCoinRange
	}
	snap, ready := s.latest()
	var row *portfolio.AssetRow
	for i := range snap.Assets {
		if snap.Assets[i].AssetKey == key {
			row = &snap.Assets[i]
			break
		}
	}
	if row == nil {
		return templates.CoinPage{}, false
	}
	p := templates.CoinPage{
		Shell:    s.shell(r, row.Symbol, "overview", "all", ""),
		AssetKey: row.AssetKey, Symbol: row.Symbol, Name: row.Name, LogoURL: row.LogoURL,
		Price: row.Price, Amount: row.Amount, ValueUSD: row.ValueUSD,
		Range: rng, Sync: s.syncInfo(snap, ready), Now: s.Now(),
	}
	p.Wallets = templates.CoinWalletsOf(snap.Wallets, key)
	p.Loading = !withSeries
	if withSeries {
		ctx, cancel := context.WithTimeout(r.Context(), coinSeriesTimeout)
		defer cancel()
		p.Points = s.coinSeries(ctx, row, templates.CoinDays[rng])
		p.Change = templates.CoinChangeOf(p.Points, rng)
	}
	return p, true
}

// coinSeries loads the daily prices of an asset; a missing source or a failure gives none.
func (s *Server) coinSeries(ctx context.Context, row *portfolio.AssetRow, days int) []portfolio.Point {
	ps, ok := s.Historical.(priceSeries)
	if !ok {
		return nil
	}
	pts, err := ps.Series(ctx, row.AssetKey, row.Mint, days)
	if err != nil {
		s.Log.Warn("coin series", "asset", row.AssetKey, "err", err)
		return nil
	}
	out := make([]portfolio.Point, len(pts))
	for i, p := range pts {
		out[i] = portfolio.Point{T: p.Day, V: p.USD}
	}
	return out
}

// coin renders the coin page at once; its chart loads itself on arrival (see CoinChart).
func (s *Server) coin(w http.ResponseWriter, r *http.Request) {
	p, ok := s.coinPage(r, defaultCoinRange, false)
	if !ok {
		s.renderStatus(w, r, http.StatusNotFound, templates.ErrorFull(s.shell(r, "Not found", "overview", "all", ""), "That asset isn't in your holdings."))
		return
	}
	s.render(w, r, templates.CoinFull(p))
}

// coinChart loads one range's price series and patches #coin-chart. The range is the
// query parameter (default 30D).
func (s *Server) coinChart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.coinPage(r, r.URL.Query().Get("range"), true)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(templates.CoinChart(p))
}
