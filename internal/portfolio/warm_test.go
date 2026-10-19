package portfolio

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

// noLivePrices fails the test if a warm start asks for live prices.
func noLivePrices(t *testing.T) PriceFunc {
	return func(context.Context, []PriceReq) map[string]core.Price {
		t.Error("warm start asked for live prices")
		return nil
	}
}

// lastSession records one synced build of cold (1 BTC) at at, as the previous run would have.
func lastSession(t *testing.T, a *Aggregator, at time.Time) {
	t.Helper()
	src := &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 1)}, AsOf: at}}
	prev := *a
	prev.Wallets = func() []Wallet { return []Wallet{{ID: "cold", Label: "Cold", Type: "bitcoin", Source: src}} }
	prev.Prices = fixedPrices(map[string]float64{"BTC": 100})
	prev.Build(context.Background())
}

// TestWarmFromLastSession: at startup the last session's balances and prices make a
// snapshot without fetching anything; wallets read "pending" until the live refresh.
func TestWarmFromLastSession(t *testing.T) {
	db := openDB(t)
	now := time.Now()
	was := now.Add(-10 * time.Minute)
	src := &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 2)}}}
	a := &Aggregator{
		Wallets: func() []Wallet {
			return []Wallet{
				{ID: "cold", Label: "Cold", Type: "bitcoin", Source: src},
				{ID: "new", Label: "New", Type: "solana", Source: &stubSource{id: "new"}},
			}
		},
		Prices:       noLivePrices(t),
		CachedPrices: fixedPrices(map[string]float64{"BTC": 100}),
		DB:           db, StaleMax: 24 * time.Hour,
		Now: func() time.Time { return now },
	}
	lastSession(t, a, was)

	s, ok := a.Warm(context.Background())
	if !ok || !s.Warm {
		t.Fatalf("warm = %v, snapshot warm = %v", ok, s.Warm)
	}
	if !near(s.TotalUSD, 100) || !s.UpdatedAt.Equal(was) {
		t.Errorf("total %v updated %v, want 100 as of the last session (%v)", s.TotalUSD, s.UpdatedAt, was)
	}
	for _, w := range s.Wallets {
		if w.Status != StatusPending {
			t.Errorf("%s status %q, want pending until the live refresh", w.ID, w.Status)
		}
		if w.ID == "new" && len(w.Rows) != 0 {
			t.Errorf("wallet with no last session has rows %v", w.Rows)
		}
	}
	if src.hits.Load() != 0 {
		t.Error("warm start fetched a source")
	}
}

// TestWarmNeedsALastSession: a first run, or data older than StaleMax, gives no warm snapshot.
func TestWarmNeedsALastSession(t *testing.T) {
	db := openDB(t)
	now := time.Now()
	a := &Aggregator{
		Wallets: func() []Wallet {
			return []Wallet{{ID: "cold", Label: "Cold", Type: "bitcoin", Source: &stubSource{id: "cold"}}}
		},
		Prices:       noLivePrices(t),
		CachedPrices: fixedPrices(map[string]float64{"BTC": 100}),
		DB:           db, StaleMax: 24 * time.Hour,
		Now: func() time.Time { return now },
	}
	if _, ok := a.Warm(context.Background()); ok {
		t.Fatal("warm snapshot on a first run")
	}
	lastSession(t, a, now.Add(-25*time.Hour))
	if _, ok := a.Warm(context.Background()); ok {
		t.Fatal("warm snapshot from data older than StaleMax")
	}
}

// blockingSource blocks Fetch until release is closed.
type blockingSource struct {
	release chan struct{}
	res     core.WalletResult
}

func (b *blockingSource) ID() string { return "cold" }
func (b *blockingSource) Fetch(ctx context.Context) (core.WalletResult, error) {
	select {
	case <-b.release:
		return b.res, nil
	case <-ctx.Done():
		return core.WalletResult{}, ctx.Err()
	}
}

// TestRefresherShowsLastSessionWhileFirstRefreshRuns: the warm snapshot is published at
// once, without alerts; the live one replaces it when the slow first refresh finishes.
func TestRefresherShowsLastSessionWhileFirstRefreshRuns(t *testing.T) {
	db := openDB(t)
	now := time.Now()
	slow := &blockingSource{release: make(chan struct{}), res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 3)}}}
	a := &Aggregator{
		Wallets:      func() []Wallet { return []Wallet{{ID: "cold", Label: "Cold", Type: "bitcoin", Source: slow}} },
		Prices:       fixedPrices(map[string]float64{"BTC": 100}),
		CachedPrices: fixedPrices(map[string]float64{"BTC": 100}),
		DB:           db, StaleMax: 24 * time.Hour,
		Now: func() time.Time { return now },
	}
	lastSession(t, a, now.Add(-time.Minute))
	var snapshots atomic.Int32
	r := &Refresher{Agg: a, Broker: NewBroker(), Interval: time.Hour, OnSnapshot: func(Snapshot) { snapshots.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, ok := r.Latest(); ok {
			if !s.Warm || !near(s.TotalUSD, 100) {
				t.Fatalf("first snapshot warm=%v total=%v, want the last session's", s.Warm, s.TotalUSD)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no snapshot while the first refresh is still running")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snapshots.Load() != 0 || !r.LastSuccess().IsZero() {
		t.Error("the warm snapshot ran OnSnapshot or counted as a successful sync")
	}

	close(slow.release)
	for {
		if s, _ := r.Latest(); !s.Warm {
			if !near(s.TotalUSD, 300) {
				t.Fatalf("live total %v, want 300", s.TotalUSD)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("live snapshot never replaced the warm one")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
