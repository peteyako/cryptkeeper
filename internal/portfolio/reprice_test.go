package portfolio

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

// livePricer is a PriceFunc whose prices a test can change; hold makes its next call wait
// for release.
type livePricer struct {
	mu      sync.Mutex
	usd     map[string]float64
	hold    bool
	entered chan struct{}
	release chan struct{}
}

func (p *livePricer) set(key string, usd float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.usd[key] = usd
}

func (p *livePricer) get(_ context.Context, reqs []PriceReq) map[string]core.Price {
	p.mu.Lock()
	hold := p.hold
	p.hold = false
	p.mu.Unlock()
	if hold {
		p.entered <- struct{}{}
		<-p.release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]core.Price{}
	for _, r := range reqs {
		if v, ok := p.usd[r.Key]; ok {
			out[r.Key] = core.Price{USD: v, AsOf: time.Now()}
		}
	}
	return out
}

// TestRepriceKeepsTheBalances: Reprice prices the last finished build's balances again
// without fetching any wallet, and keeps the time those balances were fetched.
func TestRepriceKeepsTheBalances(t *testing.T) {
	src := &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 2)}}}
	p := &livePricer{usd: map[string]float64{"BTC": 100}}
	a := &Aggregator{
		Wallets: func() []Wallet { return []Wallet{{ID: "cold", Label: "Cold", Source: src}} },
		Prices:  p.get, StaleMax: time.Hour,
	}
	if _, ok := a.Reprice(context.Background()); ok {
		t.Fatal("repriced before any build finished")
	}
	first := a.Build(context.Background())
	if first.SyncedAt.IsZero() || !first.PricedAt.Equal(first.SyncedAt) {
		t.Fatalf("a finished build: synced %v, priced %v; want both its time", first.SyncedAt, first.PricedAt)
	}

	p.set("BTC", 150)
	src.res = core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 9)}} // not fetched again
	time.Sleep(2 * time.Millisecond)
	s, ok := a.Reprice(context.Background())
	if !ok || !near(s.TotalUSD, 300) {
		t.Fatalf("reprice ok=%v total=%v, want 300 (the same 2 BTC at 150)", ok, s.TotalUSD)
	}
	if n := src.hits.Load(); n != 1 {
		t.Errorf("the wallet was fetched %d times, want once (by the build)", n)
	}
	if !s.SyncedAt.Equal(first.SyncedAt) || !s.PricedAt.After(first.PricedAt) || s.Warm {
		t.Errorf("reprice: synced %v (want %v), priced %v (want later), warm %v", s.SyncedAt, first.SyncedAt, s.PricedAt, s.Warm)
	}
}

// TestRepriceGivesWayToABuild: a build that finishes while a reprice is pricing has the
// newer balances, so the reprice is dropped.
func TestRepriceGivesWayToABuild(t *testing.T) {
	src := &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 2)}}}
	p := &livePricer{usd: map[string]float64{"BTC": 100}, entered: make(chan struct{}), release: make(chan struct{})}
	a := &Aggregator{
		Wallets: func() []Wallet { return []Wallet{{ID: "cold", Label: "Cold", Source: src}} },
		Prices:  p.get, StaleMax: time.Hour,
	}
	a.Build(context.Background())
	p.mu.Lock()
	p.hold = true
	p.mu.Unlock()
	type result struct {
		s  Snapshot
		ok bool
	}
	got := make(chan result, 1)
	go func() {
		s, ok := a.Reprice(context.Background())
		got <- result{s, ok}
	}()
	<-p.entered
	src.res = core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 3)}}
	a.Build(context.Background())
	close(p.release)
	if r := <-got; r.ok {
		t.Fatalf("a reprice that started before a build published its old balances: total %v", r.s.TotalUSD)
	}
}

// TestPricesRefreshBetweenWalletRefreshes: with PriceInterval shorter than Interval, the
// refresher publishes new prices on the last balances in between, without fetching the
// wallets, and runs OnSnapshot for them (price alerts) but doesn't count them as a sync.
func TestPricesRefreshBetweenWalletRefreshes(t *testing.T) {
	src := &stubSource{id: "cold", res: core.WalletResult{Holdings: []core.Holding{h("cold", "BTC", 2)}}}
	p := &livePricer{usd: map[string]float64{"BTC": 100}}
	a := &Aggregator{
		Wallets: func() []Wallet { return []Wallet{{ID: "cold", Label: "Cold", Source: src}} },
		Prices:  p.get, StaleMax: time.Hour,
	}
	var snaps atomic.Int32
	r := &Refresher{Agg: a, Broker: NewBroker(), Interval: time.Hour, PriceInterval: 20 * time.Millisecond,
		OnSnapshot: func(Snapshot) { snaps.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitFor(t, 2*time.Second, "the first refresh never finished", func() bool {
		s, ok := r.Latest()
		return ok && !s.Warm && near(s.TotalUSD, 200)
	})
	first, _ := r.Latest()
	synced := r.LastSuccess()

	p.set("BTC", 300)
	waitFor(t, 2*time.Second, "the new price never showed between wallet refreshes", func() bool {
		s, _ := r.Latest()
		return near(s.TotalUSD, 600)
	})
	s, _ := r.Latest()
	if n := src.hits.Load(); n != 1 {
		t.Errorf("the wallet was fetched %d times, want once", n)
	}
	if !s.SyncedAt.Equal(first.SyncedAt) || s.Warm {
		t.Errorf("repriced snapshot: synced %v (want %v), warm %v", s.SyncedAt, first.SyncedAt, s.Warm)
	}
	if !r.LastSuccess().Equal(synced) {
		t.Error("a reprice counted as a wallet sync")
	}
	if n := snaps.Load(); n < 2 {
		t.Errorf("OnSnapshot ran %d times, want the refresh and the reprice", n)
	}
}
