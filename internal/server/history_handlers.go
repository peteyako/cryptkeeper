package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/web/templates"
)

// backfillTimeout bounds one "Load last N days" run (it may fetch many daily prices).
const backfillTimeout = 60 * time.Second

// chartBackfill writes the past daily history rows for the selected range, then patches the
// value card. A failure or timeout patches the card with a note instead.
func (s *Server) chartBackfill(w http.ResponseWriter, r *http.Request) {
	v := readView(r)
	sse := datastar.NewSSE(w, r)
	var errMsg string
	if days := backfillDays[v.Range]; days > 0 && s.History != nil {
		ctx, cancel := context.WithTimeout(s.withActivity(r.Context()), backfillTimeout)
		defer cancel()
		if err := s.runBackfill(ctx, days); err != nil {
			s.Log.Warn("history backfill", "err", err)
			errMsg = err.Error()
		}
	}
	p := s.overviewPage(r, v)
	p.BackfillErr = errMsg
	_ = sse.PatchElementTempl(templates.ValueCard(p))
}

// runBackfill rolls the current holdings (all wallets) back through the activity, minus CSV rows the live API already synced.
func (s *Server) runBackfill(ctx context.Context, days int) error {
	snap, _ := s.latest()
	var txs []core.Tx
	if s.Activity != nil {
		var err error
		if txs, err = s.taxActivity(ctx); err != nil {
			return err
		}
	}
	_, err := s.History.Backfill(ctx, days, s.Now(), snap, txs, s.priceAt)
	return err
}

// priceAt adapts Deps.Historical to portfolio.PriceAt: pegged stablecoins are 1, Solana
// tokens price by mint, everything else by ticker then by mint. Without Historical it
// reports no price, so Backfill falls back to the current snapshot prices.
func (s *Server) priceAt(ctx context.Context, assetKey, mint string, day time.Time) (float64, bool) {
	if core.Pegged[strings.ToUpper(assetKey)] {
		return 1, true
	}
	if s.Historical == nil {
		return 0, false
	}
	if m, ok := core.IsSPLKey(assetKey); ok {
		v, ok, err := s.Historical.HistoricalMint(ctx, m, day)
		return v, ok && err == nil
	}
	if v, ok, err := s.Historical.Historical(ctx, assetKey, day); err == nil && ok {
		return v, true
	}
	if mint != "" {
		if v, ok, err := s.Historical.HistoricalMint(ctx, mint, day); err == nil && ok {
			return v, true
		}
	}
	return 0, false
}
