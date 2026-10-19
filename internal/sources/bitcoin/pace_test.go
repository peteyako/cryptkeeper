package bitcoin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/httpx"
)

// burstLimited answers like a mempool instance: a burst of four requests, then one more per
// refill, and 429 beyond that.
type burstLimited struct {
	h      http.Handler
	mu     sync.Mutex
	tokens float64
	last   time.Time
	refill time.Duration
	denied int
}

func (b *burstLimited) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	now := time.Now()
	if b.last.IsZero() {
		b.tokens, b.last = 4, now
	}
	b.tokens = min(4, b.tokens+float64(now.Sub(b.last))/float64(b.refill))
	b.last = now
	ok := b.tokens >= 1
	if ok {
		b.tokens--
	} else {
		b.denied++
	}
	b.mu.Unlock()
	if !ok {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	b.h.ServeHTTP(w, r)
}

// TestWalletsShareThePace: three wallets scanning at once through a rate-limited server stay
// under its limit, because requests to a paced host are spaced across all of them; none is
// ever refused.
func TestWalletsShareThePace(t *testing.T) {
	f := newFake()
	seedBusyWallet(t, f)
	limited := &burstLimited{h: f, refill: 3 * time.Millisecond}
	srv := httptest.NewServer(limited)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	httpx.SetPace(host, 4*time.Millisecond) // what New sets for a public server, scaled down
	t.Cleanup(func() { httpx.SetPace(host, 0) })
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i, key := range []string{bip84Zpub, bip49Ypub, bip44Xpub} {
		s, err := New(Config{ID: key[:4], Key: key, EsploraURL: srv.URL + "/api"},
			&httpx.Client{HTTP: srv.Client(), MaxRetries: 0, BaseDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { _, errs[i] = s.Fetch(context.Background()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("wallet %d: %v", i, err)
		}
	}
	if limited.denied != 0 {
		t.Errorf("server refused %d requests; paced wallets must stay under its limit", limited.denied)
	}
}

// TestPublicServersArePaced: a public Esplora server gets the polite pace, a node on the
// local network doesn't.
func TestPublicServersArePaced(t *testing.T) {
	for host, want := range map[string]bool{
		"mempool.space": true, "mempool.emzy.de": true, "blockstream.info": true,
		"localhost:3002": false, "127.0.0.1:3002": false, "192.168.1.20:3002": false, "10.0.0.5": false,
		"umbrel.local": false, "node.lan:3002": false, "mynode": false, "[::1]:3002": false,
		"abcdefghijklmnop.onion": false,
	} {
		if got := !privateHost(host); got != want {
			t.Errorf("%s: paced = %v, want %v", host, got, want)
		}
	}
}

// TestOneScanAtATime: the balance refresh and the activity sync asking at once share one scan.
func TestOneScanAtATime(t *testing.T) {
	f := newFake()
	seedBusyWallet(t, f)
	f.delay = 2 * time.Millisecond
	s, _ := startSource(t, f, Config{ID: "w", Key: bip84Zpub})
	var wg sync.WaitGroup
	wg.Go(func() { s.Fetch(context.Background()) })
	wg.Go(func() { s.Addresses(context.Background()) })
	wg.Wait()

	single := newFake()
	seedBusyWallet(t, single)
	s2, _ := startSource(t, single, Config{ID: "w2", Key: bip84Zpub})
	s2.Fetch(context.Background())
	if got, want := f.addressHits(), single.addressHits(); got != want {
		t.Errorf("concurrent callers read %d addresses, one scan reads %d", got, want)
	}
}

// TestScanFinishesAfterCallerGivesUp: a caller whose time runs out doesn't take the scan
// with it; the scan completes and is kept for the next call.
func TestScanFinishesAfterCallerGivesUp(t *testing.T) {
	f := newFake()
	seedBusyWallet(t, f)
	f.delay = 3 * time.Millisecond
	store := &memScans{m: map[string][]byte{}}
	s, _ := startSource(t, f, Config{ID: "w", Key: bip84Zpub, ScanStore: store})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := s.Fetch(ctx); err == nil {
		t.Fatal("Fetch finished within 5ms; the test needs a slower server")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		done := s.last != nil
		s.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scan didn't finish after its caller gave up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if raw, _ := store.LoadScan(context.Background(), "w"); len(raw) == 0 {
		t.Error("the finished scan wasn't saved")
	}
}

// TestFirst429PausesAtOnce: a server's first 429 pauses it; it isn't asked again until the
// pause ends (public mempool servers block an IP that keeps going after a 429).
func TestFirst429PausesAtOnce(t *testing.T) {
	limited, hits := downServer(t, http.StatusTooManyRequests)
	s := sourceWithFallback(t, Config{Key: bip84Zpub}, limited.URL)
	var r esploraAddress
	if err := s.get(context.Background(), addressPath(bip84Receive0), &r); err == nil {
		t.Fatal("want the 429")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("server asked %d times for one request, want once", n)
	}
	s.get(context.Background(), addressPath(bip84Receive0), &r)
	if n := hits.Load(); n != 1 {
		t.Errorf("server asked again while paused (%d requests)", n)
	}
}

// TestPauseSharedByWallets: one wallet's 429 keeps the other wallets off the server too.
func TestPauseSharedByWallets(t *testing.T) {
	limited, hits := downServer(t, http.StatusTooManyRequests)
	a := sourceWithFallback(t, Config{Key: bip84Zpub}, limited.URL)
	b := sourceWithFallback(t, Config{Key: bip49Ypub}, limited.URL)
	var r esploraAddress
	a.get(context.Background(), addressPath(bip84Receive0), &r)
	before := hits.Load()
	if err := b.get(context.Background(), addressPath(bip84Receive0), &r); err == nil {
		t.Fatal("want the pause's 429")
	}
	if hits.Load() != before {
		t.Error("another wallet asked the paused server")
	}
}
