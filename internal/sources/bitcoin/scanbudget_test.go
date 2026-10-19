package bitcoin

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// addressHits counts the /address/{a} lookups the fake served.
func (f *fakeEsplora) addressHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for path, c := range f.hits {
		if strings.HasPrefix(path, "/api/address/") && !strings.HasSuffix(path, "/utxo") && !strings.HasSuffix(path, "/txs") {
			n += c
		}
	}
	return n
}

// busyWallet is a wallet shaped like a typical hardware-wallet account: 20 used receive
// addresses (the first three still funded) and 8 used change addresses (the last funded).
func busyWallet(t *testing.T) (*Source, *fakeEsplora) {
	t.Helper()
	f := newFake()
	s, _ := startSource(t, f, Config{ID: "ledger", Key: bip84Zpub})
	seedBusyWallet(t, f)
	return s, f
}

// seedBusyWallet gives f the bip84Zpub account's history described at busyWallet.
func seedBusyWallet(t *testing.T, f *fakeEsplora) {
	t.Helper()
	s := newTestSource(t, Config{Key: bip84Zpub})
	for i := range 20 {
		st := &fakeStats{chainFunded: 10_000, chainSpent: 10_000, chainTx: 2}
		if i < 3 {
			st = &fakeStats{chainFunded: 10_000, chainTx: 1}
		}
		f.stats[mustAddr(t, s, 0, i)] = st
	}
	for i := range 8 {
		st := &fakeStats{chainFunded: 5_000, chainSpent: 5_000, chainTx: 2}
		if i == 7 {
			st = &fakeStats{chainFunded: 5_000, chainTx: 1}
		}
		f.stats[mustAddr(t, s, 1, i)] = st
	}
}

// newBlock moves the fake chain tip, so the next refresh reads addresses again.
func (f *fakeEsplora) newBlock() {
	f.mu.Lock()
	f.tip++
	f.mu.Unlock()
}

func fetchAt(t *testing.T, s *Source, at time.Time) string {
	t.Helper()
	s.now = func() time.Time { return at }
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch at %v: %v", at, err)
	}
	return res.Holdings[0].AmountStr
}

// TestRefreshChecksOnlyChangeableAddresses: after a full scan, a refresh reads only the
// funded addresses and the first few unused ones on each chain, not the whole gap window.
func TestRefreshChecksOnlyChangeableAddresses(t *testing.T) {
	s, f := busyWallet(t)
	if got := fetchAt(t, s, fixedNow); got != "0.00035000" {
		t.Fatalf("balance = %s", got)
	}
	full := f.addressHits()
	if full < 60 {
		t.Fatalf("first scan read %d addresses, want the full gap window", full)
	}
	f.newBlock()
	if got := fetchAt(t, s, fixedNow.Add(5*time.Minute)); got != "0.00035000" {
		t.Fatalf("balance after refresh = %s", got)
	}
	funded, unused := 4, 2*lookahead
	if n := f.addressHits() - full; n > funded+unused {
		t.Errorf("refresh read %d addresses, want at most %d", n, funded+unused)
	}
}

// TestRefreshSeesNewPayment: a payment to the next receive address is seen at the next
// refresh, and the scan extends so the gap limit holds past it.
func TestRefreshSeesNewPayment(t *testing.T) {
	s, f := busyWallet(t)
	fetchAt(t, s, fixedNow)
	f.mu.Lock()
	f.stats[mustAddr(t, s, 0, 21)] = &fakeStats{memFunded: 7_000, memTx: 1}
	f.mu.Unlock()
	f.newBlock()
	if got := fetchAt(t, s, fixedNow.Add(5*time.Minute)); got != "0.00042000" {
		t.Fatalf("balance = %s, want the new payment included", got)
	}
	next, err := s.NextReceiveAddress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := mustAddr(t, s, 0, 22); next != want {
		t.Errorf("next receive address = %s, want index 22", next)
	}
	addrs, _ := s.Addresses(context.Background())
	receive := 0
	for _, a := range addrs {
		if !a.Change {
			receive++
		}
	}
	if receive != 22+DefaultGapLimit {
		t.Errorf("%d receive addresses listed, want the gap limit past index 21 (%d)", receive, 22+DefaultGapLimit)
	}
}

// TestRefreshSeesSpend: spending from a funded address shows at the next refresh.
func TestRefreshSeesSpend(t *testing.T) {
	s, f := busyWallet(t)
	fetchAt(t, s, fixedNow)
	f.mu.Lock()
	f.stats[mustAddr(t, s, 0, 0)] = &fakeStats{chainFunded: 10_000, chainTx: 1, memSpent: 10_000, memTx: 1}
	f.mu.Unlock()
	f.newBlock()
	if got := fetchAt(t, s, fixedNow.Add(5*time.Minute)); got != "0.00025000" {
		t.Fatalf("balance = %s, want the spend reflected", got)
	}
}

// TestFullScanCatchesAddressReuse: a payment to an old, emptied address is missed by the
// light refresh and caught by the next full scan.
func TestFullScanCatchesAddressReuse(t *testing.T) {
	s, f := busyWallet(t)
	fetchAt(t, s, fixedNow)
	f.mu.Lock()
	f.stats[mustAddr(t, s, 0, 10)] = &fakeStats{chainFunded: 13_000, chainSpent: 10_000, chainTx: 3}
	f.mu.Unlock()
	f.newBlock()
	if got := fetchAt(t, s, fixedNow.Add(5*time.Minute)); got != "0.00035000" {
		t.Fatalf("light refresh balance = %s; it isn't expected to re-read emptied addresses", got)
	}
	if got := fetchAt(t, s, fixedNow.Add(fullScanEvery)); got != "0.00038000" {
		t.Fatalf("full scan balance = %s, want the reused address counted", got)
	}
}

// TestActivityReusesTheRefreshScan: activity sync between refreshes reads no addresses
// again; the refresh keeps the scan current.
func TestActivityReusesTheRefreshScan(t *testing.T) {
	s, f := busyWallet(t)
	fetchAt(t, s, fixedNow)
	before := f.addressHits()
	s.now = func() time.Time { return fixedNow.Add(5 * time.Minute) }
	if _, err := s.Addresses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.addressHits() - before; n != 0 {
		t.Errorf("Addresses re-read %d addresses five minutes after a refresh", n)
	}
}

// TestRateLimitPausesServer: after a 429 the server isn't asked again until the pause ends.
func TestRateLimitPausesServer(t *testing.T) {
	limited, hits := downServer(t, http.StatusTooManyRequests)
	s := sourceWithFallback(t, Config{Key: bip84Zpub}, limited.URL, "")
	if _, err := s.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Fetch error = %v, want the 429", err)
	}
	// While paused, every request fails at once without reaching the server. (Counting the
	// server's hits here would race with lookups the first 429 cancelled in flight.)
	_, err := s.Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused Fetch error = %v, want the pause's own 429", err)
	}
	before := hits.Load()
	s.now = func() time.Time { return fixedNow.Add(rateLimitPause + time.Second) }
	s.Fetch(context.Background()) // still limited: asked once more, then paused again
	if hits.Load() == before {
		t.Error("server not asked again after the pause")
	}
}

// TestNoRescanWithoutNewBlock: while the chain tip stays put, a refresh within quietRescan of
// the last scan reads only the tip; a new block, or quietRescan passing, reads addresses.
func TestNoRescanWithoutNewBlock(t *testing.T) {
	s, f := busyWallet(t)
	fetchAt(t, s, fixedNow)
	before := f.addressHits()
	fetchAt(t, s, fixedNow.Add(5*time.Minute))
	if n := f.addressHits() - before; n != 0 {
		t.Fatalf("refresh with no new block read %d addresses", n)
	}
	f.newBlock()
	fetchAt(t, s, fixedNow.Add(10*time.Minute))
	if f.addressHits() == before {
		t.Fatal("a new block didn't trigger a refresh")
	}
}

// TestUnconfirmedPaymentSeenWithinQuietRescan: a payment still in the mempool shows within
// quietRescan even without a new block.
func TestUnconfirmedPaymentSeenWithinQuietRescan(t *testing.T) {
	s, f := busyWallet(t)
	fetchAt(t, s, fixedNow)
	f.mu.Lock()
	f.stats[mustAddr(t, s, 0, 20)] = &fakeStats{memFunded: 7_000, memTx: 1}
	f.mu.Unlock()
	if got := fetchAt(t, s, fixedNow.Add(5*time.Minute)); got != "0.00035000" {
		t.Fatalf("balance %s before quietRescan; the refresh isn't expected to look yet", got)
	}
	if got := fetchAt(t, s, fixedNow.Add(quietRescan+time.Second)); got != "0.00042000" {
		t.Fatalf("balance %s after quietRescan, want the unconfirmed payment", got)
	}
}
