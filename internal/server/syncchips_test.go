package server

import (
	"testing"
	"time"

	"cryptkeeper/internal/config"
	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
	"cryptkeeper/web/templates"
)

// TestSyncChips: the header has one chip for prices and one for wallet balances, which
// refresh on their own schedules. While the wallets update, the prices chip still says when
// prices last came in; a stale price, or prices that stopped coming, show on the prices chip.
func TestSyncChips(t *testing.T) {
	now := time.Now()
	s := &Server{Deps: Deps{Now: func() time.Time { return now }, Cfg: config.Default()}}
	snap := portfolio.Snapshot{UpdatedAt: now, SyncedAt: now.Add(-3 * time.Minute), PricedAt: now.Add(-10 * time.Second),
		Wallets: []portfolio.WalletView{{ID: "cold", Status: portfolio.StatusSynced}}}
	check := func(what string, got, want templates.SyncInfo) {
		t.Helper()
		if got.Status != want.Status || got.Label != want.Label {
			t.Errorf("%s: %s %q, want %s %q", what, got.Status, got.Label, want.Status, want.Label)
		}
	}

	got := s.syncStatus(snap, true)
	check("prices", got.Prices, templates.SyncInfo{Status: portfolio.StatusSynced, Label: "Prices just now"})
	check("wallets", got.Wallets, templates.SyncInfo{Status: portfolio.StatusSynced, Label: "Wallets 3m ago"})
	if got.Prices.Title != "Prices refresh every minute" || got.Wallets.Title != "Balances refresh every 5 min" {
		t.Errorf("titles %q, %q", got.Prices.Title, got.Wallets.Title)
	}

	snap.Warm = true // a wallet refresh in progress
	got = s.syncStatus(snap, true)
	check("wallets updating", got.Wallets, templates.SyncInfo{Status: portfolio.StatusPending, Label: "Updating wallets…"})
	check("prices while wallets update", got.Prices, templates.SyncInfo{Status: portfolio.StatusSynced, Label: "Prices just now"})

	snap.Warm = false
	snap.Assets = []portfolio.AssetRow{{AssetKey: "BTC", Price: &core.Price{USD: 1, Stale: true}}, {AssetKey: "SOL", Price: &core.Price{USD: 1}}}
	check("a stale price", s.syncStatus(snap, true).Prices, templates.SyncInfo{Status: portfolio.StatusStale, Label: "1 price stale"})

	snap.Assets = nil
	snap.PricedAt = now.Add(-10 * time.Minute)
	check("old prices", s.syncStatus(snap, true).Prices, templates.SyncInfo{Status: portfolio.StatusStale, Label: "Prices 10m ago"})

	snap.PricedAt = time.Time{}
	check("no live prices yet", s.syncStatus(snap, true).Prices, templates.SyncInfo{Status: portfolio.StatusPending, Label: "Loading prices…"})

	got = s.syncStatus(portfolio.Snapshot{}, false)
	check("prices before any snapshot", got.Prices, templates.SyncInfo{Status: portfolio.StatusPending, Label: "Loading prices…"})
	check("wallets before any snapshot", got.Wallets, templates.SyncInfo{Status: portfolio.StatusPending, Label: "Syncing wallets…"})
}
