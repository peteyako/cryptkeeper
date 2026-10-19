package portfolio

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

// waitFor polls cond until it holds or within runs out.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("after %v: %s", within, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func walletByID(s Snapshot, id string) (WalletView, bool) {
	for _, w := range s.Wallets {
		if w.ID == id {
			return w, true
		}
	}
	return WalletView{}, false
}

// TestRefreshShowsWalletsAsTheyFinish: while one wallet is slow, the others' new balances
// show as soon as they arrive, and the slow one keeps what it showed before until it
// answers. Such a snapshot is in progress (Warm); only a finished refresh runs OnSnapshot.
func TestRefreshShowsWalletsAsTheyFinish(t *testing.T) {
	hot := &stubSource{id: "hot", res: core.WalletResult{Holdings: []core.Holding{h("hot", "BTC", 1)}}}
	var mu sync.Mutex
	var cold core.Source = &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 5)}}}
	a := &Aggregator{
		Wallets: func() []Wallet {
			mu.Lock()
			defer mu.Unlock()
			return []Wallet{{ID: "hot", Label: "Hot", Source: hot}, {ID: "cold", Label: "Cold", Source: cold}}
		},
		Prices:       fixedPrices(map[string]float64{"BTC": 100}),
		CachedPrices: fixedPrices(map[string]float64{"BTC": 100}),
		DB:           openDB(t), StaleMax: time.Hour, FetchTimeout: 10 * time.Second,
	}
	var finished atomic.Int32
	r := &Refresher{Agg: a, Broker: NewBroker(), Interval: time.Hour, OnSnapshot: func(Snapshot) { finished.Add(1) }}
	if s := r.Rebuild(context.Background()); s.Warm || !near(s.TotalUSD, 600) {
		t.Fatalf("first refresh warm=%v total=%v, want finished at 600", s.Warm, s.TotalUSD)
	}

	// Next refresh: the hot wallet now holds 3 BTC and answers at once; the cold one is slow.
	hot.res = core.WalletResult{Holdings: []core.Holding{h("hot", "BTC", 3)}}
	slow := &blockingSource{release: make(chan struct{}), res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 2)}}}
	defer func() {
		select {
		case <-slow.release:
		default:
			close(slow.release)
		}
	}()
	mu.Lock()
	cold = slow
	mu.Unlock()
	done := make(chan Snapshot, 1)
	go func() { done <- r.Rebuild(context.Background()) }()

	waitFor(t, 2*time.Second, "the hot wallet's new balance never showed while the cold one was slow", func() bool {
		s, _ := r.Latest()
		return s.Warm && near(s.TotalUSD, 800) // hot's new 300 + cold's previous 500
	})
	s, _ := r.Latest()
	if w, _ := walletByID(s, "cold"); w.Status != StatusSynced {
		t.Errorf("the slow wallet shows %q while it is fetched again, want what it showed before (synced)", w.Status)
	}
	if n := finished.Load(); n != 1 {
		t.Errorf("OnSnapshot ran %d times, want once (the in-progress snapshot doesn't count)", n)
	}

	close(slow.release)
	select {
	case s := <-done:
		if s.Warm || !near(s.TotalUSD, 500) {
			t.Fatalf("finished refresh warm=%v total=%v, want 500 (hot 300 + cold 200)", s.Warm, s.TotalUSD)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh didn't finish after the slow wallet answered")
	}
	if n := finished.Load(); n != 2 {
		t.Errorf("OnSnapshot ran %d times, want 2", n)
	}
}

// TestKickStartsOverWithTheNewWallets: a kick (a backup imported, a source added or removed)
// while a refresh waits on a slow wallet doesn't wait for it. The refresh in progress stops,
// the new wallet list shows at once, and the old refresh never overwrites it.
func TestKickStartsOverWithTheNewWallets(t *testing.T) {
	slow := &blockingSource{release: make(chan struct{})}
	defer close(slow.release)
	added := &stubSource{id: "hot", res: core.WalletResult{Holdings: []core.Holding{h("hot", "BTC", 1)}}}
	var mu sync.Mutex
	wallets := []Wallet{{ID: "cold", Label: "Cold", Source: slow}}
	a := &Aggregator{
		Wallets: func() []Wallet {
			mu.Lock()
			defer mu.Unlock()
			return append([]Wallet(nil), wallets...)
		},
		Prices:       fixedPrices(map[string]float64{"BTC": 100}),
		CachedPrices: fixedPrices(map[string]float64{"BTC": 100}),
		DB:           openDB(t), StaleMax: time.Hour, FetchTimeout: 10 * time.Second,
	}
	r := &Refresher{Agg: a, Broker: NewBroker(), Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitFor(t, 2*time.Second, "no snapshot while the first refresh waits on the slow wallet", func() bool {
		_, ok := r.Latest()
		return ok
	})

	mu.Lock()
	wallets = append(wallets, Wallet{ID: "hot", Label: "Hot", Source: added})
	mu.Unlock()
	r.Kick()
	waitFor(t, time.Second, "the added wallet didn't show within a second of the kick", func() bool {
		s, _ := r.Latest()
		w, ok := walletByID(s, "hot")
		return ok && w.Status == StatusSynced && near(s.TotalUSD, 100)
	})
	time.Sleep(100 * time.Millisecond) // the old refresh has stopped by now; it must not have published
	if s, _ := r.Latest(); func() bool { _, ok := walletByID(s, "hot"); return !ok }() {
		t.Fatal("the refresh the kick replaced published its old wallet list afterwards")
	}
}
