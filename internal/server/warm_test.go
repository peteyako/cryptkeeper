package server

import (
	"context"
	"testing"
	"time"

	"cryptkeeper/internal/portfolio"
	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/sources/manual"
)

// TestSyncChipWarmStart: while the last session's balances stand in after a restart, the
// header says it is updating, not that it is synced or stale.
func TestSyncChipWarmStart(t *testing.T) {
	now := time.Now()
	s := &Server{Deps: Deps{Now: func() time.Time { return now }}}
	snap := portfolio.Snapshot{Warm: true, UpdatedAt: now.Add(-time.Hour),
		Wallets: []portfolio.WalletView{{ID: "cold", Status: portfolio.StatusPending, AsOf: now.Add(-time.Hour)}}}
	if got := s.syncStatus(snap, true).Wallets; got.Status != portfolio.StatusPending || got.Label != "Updating wallets…" {
		t.Fatalf("sync chip = %+v, want pending Updating wallets…", got)
	}
	snap.Warm = false
	snap.Wallets[0].Status = portfolio.StatusSynced
	if got := s.syncStatus(snap, true).Wallets; got.Status != portfolio.StatusSynced {
		t.Fatalf("live snapshot chip = %+v", got)
	}
}

// TestDeletingWalletDropsSavedScan: a removed wallet's saved Bitcoin scan goes with it.
func TestDeletingWalletDropsSavedScan(t *testing.T) {
	e := newEnv(t, "")
	if _, err := e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.db.PutJSON(ctx, BitcoinScanKVPrefix+"cold", map[string]int{"v": 1}); err != nil {
		t.Fatal(err)
	}
	e.do("POST", "/sources/cold/delete", nil, ds(nil))
	var v map[string]int
	if ok, err := e.db.GetJSON(ctx, BitcoinScanKVPrefix+"cold", &v); err != nil || ok {
		t.Fatalf("saved scan still there: ok=%v err=%v", ok, err)
	}
}
