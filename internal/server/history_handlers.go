package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
	"cryptkeeper/internal/tax"
	"cryptkeeper/web/templates"
)

// backfillTimeout bounds one "Load last N days" run (it may fetch many daily prices).
const backfillTimeout = 60 * time.Second

// errNoSnapshot refuses a backfill before the first refresh: there are no holdings to roll back.
var errNoSnapshot = errors.New("no snapshot yet")

// chartBackfill writes the past daily history rows for the selected range (and adds wallets
// missing from the rows already there), then patches the value card. A failure or timeout
// patches the card with a note instead (the error itself goes to the log), and so does a
// run that could add nothing.
func (s *Server) chartBackfill(w http.ResponseWriter, r *http.Request) {
	v := readView(r)
	sse := datastar.NewSSE(w, r)
	var errMsg, note string
	if days := backfillDaysFor(v.Range, s.Now()); days > 0 && s.History != nil {
		ctx, cancel := context.WithTimeout(s.withActivity(r.Context()), backfillTimeout)
		defer cancel()
		n, err := s.runBackfill(ctx, days)
		switch {
		case err != nil:
			s.Log.Warn("history backfill", "err", err)
			errMsg = backfillErrText(err)
		case n == 0:
			note = "History already covers these days for every wallet; gaps after a wallet's first recorded day can't be backfilled."
		}
	}
	p := s.overviewPage(r, v)
	p.BackfillErr, p.BackfillNote = errMsg, note
	if note != "" {
		p.BackfillDays = 0 // loading again would add nothing either
	}
	_ = sse.PatchElementTempl(templates.ValueCard(p))
}

// backfillErrText is the card's note for a failed backfill.
func backfillErrText(err error) string {
	var notSynced *portfolio.NotSyncedError
	var price *portfolio.PriceError
	switch {
	case errors.Is(err, errNoSnapshot):
		return "Your balances haven't loaded yet — try again in a moment."
	case errors.As(err, &notSynced):
		return notSynced.Label + " isn't syncing — try again once it has synced."
	case errors.Is(err, context.DeadlineExceeded):
		return "Loading history took too long — try again in a few minutes."
	case errors.As(err, &price):
		return "Couldn't fetch past prices right now — try again in a few minutes."
	}
	return "Couldn't load history — try again later."
}

// runBackfill rolls the current holdings (all wallets) back through the activity, minus CSV
// rows the live API already synced, with tagged position moves keeping their units (as
// tax.RollBack does), and returns how many rows it wrote.
func (s *Server) runBackfill(ctx context.Context, days int) (int, error) {
	snap, ready := s.latest()
	if !ready || snap.Warm {
		return 0, errNoSnapshot
	}
	var txs []core.Tx
	if s.Activity != nil {
		var err error
		if txs, err = s.taxActivity(ctx); err != nil {
			return 0, err
		}
	}
	var settings tax.Settings
	if s.TaxSettings != nil {
		settings = s.TaxSettings.Get()
	}
	isPosition := func(id string) bool { return settings.Tags[id].Tag == tax.TagPosition }
	return s.History.Backfill(ctx, days, s.Now(), snap, txs, isPosition, s.priceAt)
}

// priceAt adapts Deps.Historical to portfolio.PriceAt, the daily close of day: pegged
// stablecoins are 1, Solana tokens price by mint, everything else by ticker then by mint. A
// failed lookup is returned, so Backfill writes nothing rather than a fallback price for good.
// Without Historical it reports no price, so Backfill falls back to the current snapshot
// prices.
func (s *Server) priceAt(ctx context.Context, assetKey, mint string, day time.Time) (float64, bool, error) {
	if core.Pegged[strings.ToUpper(assetKey)] {
		return 1, true, nil
	}
	if s.Historical == nil {
		return 0, false, nil
	}
	if m, ok := core.IsSPLKey(assetKey); ok {
		return s.mintClose(ctx, m, day)
	}
	if v, ok, err := s.Historical.Historical(ctx, assetKey, day); err != nil || ok {
		return v, ok, err
	}
	if mint != "" {
		return s.mintClose(ctx, mint, day)
	}
	return 0, false, nil
}

// mintCloser is the daily close by Solana mint (CoinGecko's price at the next 00:00 UTC, as a
// Coinbase candle closes); *prices.Service implements it next to HistoricalPrices.
type mintCloser interface {
	HistoricalMintClose(ctx context.Context, mint string, day time.Time) (float64, bool, error)
}

// mintClose is the daily close of a Solana token, or HistoricalMint's price for the day when
// Historical cannot tell the close.
func (s *Server) mintClose(ctx context.Context, mint string, day time.Time) (float64, bool, error) {
	if mc, ok := s.Historical.(mintCloser); ok {
		return mc.HistoricalMintClose(ctx, mint, day)
	}
	return s.Historical.HistoricalMint(ctx, mint, day)
}
