package bitcoin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/httpx"
)

// memScans is a ScanStore in memory, shared by "restarts" of a source.
type memScans struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (m *memScans) LoadScan(_ context.Context, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.m[id], nil
}

func (m *memScans) SaveScan(_ context.Context, id string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[id] = data
	return nil
}

// restart builds a new source over the same fake server and scan store, as a server restart
// would.
func restart(t *testing.T, f *fakeEsplora, srv *httptest.Server, cfg Config, at time.Time) *Source {
	t.Helper()
	cfg.EsploraURL = srv.URL + "/api"
	s, err := New(cfg, &httpx.Client{HTTP: srv.Client(), MaxRetries: 0, BaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return at }
	return s
}

// TestScanSurvivesRestart: after a restart the wallet carries on from its saved scan: no
// full rescan, same balance.
func TestScanSurvivesRestart(t *testing.T) {
	store := &memScans{m: map[string][]byte{}}
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := Config{ID: "ledger", Key: bip84Zpub, ScanStore: store}
	seedBusyWallet(t, f)

	if got := fetchAt(t, restart(t, f, srv, cfg, fixedNow), fixedNow); got != "0.00035000" {
		t.Fatalf("first balance %s", got)
	}
	before := f.addressHits()
	f.mu.Lock()
	f.tip++ // a new block, so the refresh after the restart does read addresses
	f.mu.Unlock()
	s := restart(t, f, srv, cfg, fixedNow.Add(5*time.Minute))
	if got := fetchAt(t, s, fixedNow.Add(5*time.Minute)); got != "0.00035000" {
		t.Fatalf("balance after restart %s", got)
	}
	if n := f.addressHits() - before; n > 4+2*lookahead {
		t.Errorf("restart read %d addresses, want a light refresh, not a full scan", n)
	}
	if next, _ := s.NextReceiveAddress(context.Background()); next != mustAddr(t, s, 0, 20) {
		t.Errorf("next receive address %s, want index 20 from the saved scan", next)
	}
}

// TestSavedScanForAnotherKeyIgnored: a saved scan only applies to the same wallet key.
func TestSavedScanForAnotherKeyIgnored(t *testing.T) {
	store := &memScans{m: map[string][]byte{}}
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	seedBusyWallet(t, f)
	fetchAt(t, restart(t, f, srv, Config{ID: "w", Key: bip84Zpub, ScanStore: store}, fixedNow), fixedNow)
	before := f.addressHits()
	s := restart(t, f, srv, Config{ID: "w", Key: bip49Ypub, ScanStore: store}, fixedNow.Add(time.Minute))
	fetchAt(t, s, fixedNow.Add(time.Minute))
	if n := f.addressHits() - before; n < 2*DefaultGapLimit {
		t.Errorf("edited wallet read %d addresses, want a full scan", n)
	}
}

// TestSavedScanStillFullScansOnSchedule: the 6-hour full scan carries over a restart.
func TestSavedScanStillFullScansOnSchedule(t *testing.T) {
	store := &memScans{m: map[string][]byte{}}
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	seedBusyWallet(t, f)
	cfg := Config{ID: "w", Key: bip84Zpub, ScanStore: store}
	fetchAt(t, restart(t, f, srv, cfg, fixedNow), fixedNow)
	before := f.addressHits()
	later := fixedNow.Add(fullScanEvery)
	fetchAt(t, restart(t, f, srv, cfg, later), later)
	if n := f.addressHits() - before; n < 60 {
		t.Errorf("read %d addresses %v after the last full scan, want a full scan", n, fullScanEvery)
	}
}

// switchable serves h, or answers status when status is non-zero.
type switchable struct {
	h      http.Handler
	status atomic.Int32
	hits   atomic.Int32
}

func (s *switchable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.hits.Add(1)
	if st := s.status.Load(); st != 0 {
		http.Error(w, "unavailable", int(st))
		return
	}
	s.h.ServeHTTP(w, r)
}

// TestPrimaryRetriedWhenFallbackFails: while the fallback stands in, a fallback that starts
// refusing sends the request back to the primary, which is used again once it answers.
func TestPrimaryRetriedWhenFallbackFails(t *testing.T) {
	primary := &switchable{h: newFake()}
	primary.status.Store(http.StatusServiceUnavailable)
	fallback := &switchable{h: newFake()}
	pSrv, fSrv := httptest.NewServer(primary), httptest.NewServer(fallback)
	t.Cleanup(pSrv.Close)
	t.Cleanup(fSrv.Close)
	s := sourceWithFallback(t, Config{Key: bip84Zpub}, pSrv.URL, fSrv.URL)
	var r esploraAddress
	if err := s.get(context.Background(), addressPath(bip84Receive0), &r); err != nil || !s.usingFallback() {
		t.Fatalf("err %v, using fallback %v", err, s.usingFallback())
	}

	primary.status.Store(0)
	fallback.status.Store(http.StatusTooManyRequests)
	if err := s.get(context.Background(), addressPath(bip84Receive0), &r); err != nil {
		t.Fatalf("fallback refusing and primary back: %v", err)
	}
	if s.usingFallback() {
		t.Error("still on the fallback after the primary answered")
	}
	before := fallback.hits.Load()
	s.get(context.Background(), addressPath(bip84Receive0), &r)
	if fallback.hits.Load() != before {
		t.Error("fallback asked again after the primary recovered")
	}
}
