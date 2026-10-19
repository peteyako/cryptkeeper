package server

import (
	"testing"
	"time"

	"crypto-portfolio/internal/portfolio"
)

// TestSyncChipWarmStart: while the last session's balances stand in after a restart, the
// header says it is updating, not that it is synced or stale.
func TestSyncChipWarmStart(t *testing.T) {
	now := time.Now()
	s := &Server{Deps: Deps{Now: func() time.Time { return now }}}
	snap := portfolio.Snapshot{Warm: true, UpdatedAt: now.Add(-time.Hour),
		Wallets: []portfolio.WalletView{{ID: "cold", Status: portfolio.StatusPending, AsOf: now.Add(-time.Hour)}}}
	if got := s.syncInfo(snap, true); got.Status != portfolio.StatusPending || got.Label != "Updating…" {
		t.Fatalf("sync chip = %+v, want pending Updating…", got)
	}
	snap.Warm = false
	snap.Wallets[0].Status = portfolio.StatusSynced
	if got := s.syncInfo(snap, true); got.Status != portfolio.StatusSynced {
		t.Fatalf("live snapshot chip = %+v", got)
	}
}
