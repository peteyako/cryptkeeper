package bitcoin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/httpx"
)

const bip84Receive0 = "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"

// downServer answers every request with status and counts them.
func downServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unavailable", status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// sourceWithFallback builds a source whose primary Esplora is primaryURL and whose fallbacks
// are fallbackURLs, in order ("" for none).
func sourceWithFallback(t *testing.T, cfg Config, primaryURL string, fallbackURLs ...string) *Source {
	t.Helper()
	cfg.EsploraURL, cfg.FallbackURLs = primaryURL+"/api", nil
	for _, u := range fallbackURLs {
		if u != "" {
			cfg.FallbackURLs = append(cfg.FallbackURLs, u+"/api")
		}
	}
	client := &httpx.Client{HTTP: &http.Client{}, MaxRetries: 0, BaseDelay: time.Millisecond}
	s, err := New(cfg, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return fixedNow }
	return s
}

func hasWarning(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TestFallbackWhenPrimaryDown: a primary that doesn't answer hands the scan to the fallback,
// which is then used directly for a while; the wallet says so.
func TestFallbackWhenPrimaryDown(t *testing.T) {
	primary, primaryHits := downServer(t, http.StatusServiceUnavailable)
	f := newFake()
	f.stats[bip84Receive0] = &fakeStats{chainFunded: 50_000, chainTx: 1}
	fallback := httptest.NewServer(f)
	t.Cleanup(fallback.Close)

	s := sourceWithFallback(t, Config{ID: "w", Key: bip84Zpub}, primary.URL, fallback.URL)
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Holdings[0].AmountStr != "0.00050000" {
		t.Fatalf("balance = %s, want the fallback's 0.00050000", res.Holdings[0].AmountStr)
	}
	fallbackHost := strings.TrimPrefix(fallback.URL, "http://")
	if node := metaValue(res.Meta, "Node"); !strings.Contains(node, fallbackHost) {
		t.Errorf("Node = %q, want the fallback host %s", node, fallbackHost)
	}
	if !hasWarning(res.Warnings, "isn't answering") {
		t.Errorf("warnings = %v, want one saying the primary isn't answering", res.Warnings)
	}

	before := primaryHits.Load()
	if _, err := s.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if after := primaryHits.Load(); after != before {
		t.Errorf("primary got %d more requests while marked down, want 0", after-before)
	}
}

// TestFallbackNotUsedForBadRequests: a 4xx from the primary is the answer, not an outage.
func TestFallbackNotUsedForBadRequests(t *testing.T) {
	primary, _ := downServer(t, http.StatusBadRequest)
	f := newFake()
	fallback := httptest.NewServer(f)
	t.Cleanup(fallback.Close)

	s := sourceWithFallback(t, Config{Key: bip84Zpub}, primary.URL, fallback.URL)
	if _, err := s.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded, want the primary's 400")
	}
	if n := f.totalHits(); n != 0 {
		t.Errorf("fallback got %d requests, want 0", n)
	}
}

// TestNoFallbackConfigured: without a fallback an outage is an error, as before.
func TestNoFallbackConfigured(t *testing.T) {
	primary, _ := downServer(t, http.StatusServiceUnavailable)
	s := sourceWithFallback(t, Config{Key: bip84Zpub}, primary.URL, "")
	if _, err := s.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch succeeded with the only server down")
	}
}

func TestInvalidFallbackURLRejected(t *testing.T) {
	if _, err := New(Config{Key: bip84Zpub, FallbackURLs: []string{"https://ok.example/api", "ftp://x"}}, nil); err == nil {
		t.Fatal("accepted an ftp fallback URL")
	}
}

// TestScriptHintNamesTheTypeWithHistory: an xpub read as Legacy with no history, whose Native
// SegWit addresses do have history, says which script type to pick. The check runs once.
func TestScriptHintNamesTheTypeWithHistory(t *testing.T) {
	f := newFake()
	f.stats[bip84Receive0] = &fakeStats{chainTx: 2}
	s, _ := startSource(t, f, Config{Key: asXpub(t, bip84Zpub)})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(res.Warnings, "Native SegWit") || !hasWarning(res.Warnings, "Script type") {
		t.Fatalf("warnings = %v, want a hint naming Native SegWit", res.Warnings)
	}
	res, err = s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(res.Warnings, "Native SegWit") {
		t.Errorf("second Fetch lost the hint: %v", res.Warnings)
	}
	if n := f.hitCount("/api/address/" + bip84Receive0); n != 1 {
		t.Errorf("native segwit address checked %d times, want once", n)
	}
}

// TestScriptHintGenericWhenNothingFound: no history under any script type still explains
// that the xpub was read as Legacy.
func TestScriptHintGenericWhenNothingFound(t *testing.T) {
	s, _ := startSource(t, newFake(), Config{Key: asXpub(t, bip84Zpub)})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(res.Warnings, "Legacy") || !hasWarning(res.Warnings, "Script type") {
		t.Fatalf("warnings = %v, want the generic Legacy hint", res.Warnings)
	}
}

// TestNoScriptHint: a zpub, an explicit script type, or an xpub with history needs no hint.
func TestNoScriptHint(t *testing.T) {
	legacyUsed := newFake()
	cases := map[string]struct {
		f   *fakeEsplora
		cfg Config
	}{
		"zpub":              {newFake(), Config{Key: bip84Zpub}},
		"explicit legacy":   {newFake(), Config{Key: asXpub(t, bip84Zpub), ScriptType: "p2pkh"}},
		"xpub with history": {legacyUsed, Config{Key: bip44Xpub}},
	}
	s0, _ := startSource(t, newFake(), Config{Key: bip44Xpub})
	legacyUsed.stats[mustAddr(t, s0, 0, 0)] = &fakeStats{chainTx: 1}
	for name, tc := range cases {
		s, _ := startSource(t, tc.f, tc.cfg)
		res, err := s.Fetch(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hasWarning(res.Warnings, "Script type") {
			t.Errorf("%s: unexpected hint %v", name, res.Warnings)
		}
	}
}

// TestFallbacksTriedInOrder: with several fallbacks, the first one that answers serves the
// request, and the servers that didn't are skipped next time.
func TestFallbacksTriedInOrder(t *testing.T) {
	primary := &switchable{h: newFake()}
	primary.status.Store(http.StatusServiceUnavailable)
	limited := &switchable{h: newFake()}
	limited.status.Store(http.StatusTooManyRequests)
	f := newFake()
	f.stats[bip84Receive0] = &fakeStats{chainFunded: 12_000, chainTx: 1}
	good := &switchable{h: f}
	var urls []string
	for _, h := range []http.Handler{primary, limited, good} {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
	}
	s := sourceWithFallback(t, Config{ID: "w", Key: bip84Zpub}, urls[0], urls[1], urls[2])
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Holdings[0].AmountStr != "0.00012000" {
		t.Fatalf("balance %s, want the third server's", res.Holdings[0].AmountStr)
	}
	goodHost := strings.TrimPrefix(urls[2], "http://")
	if node := metaValue(res.Meta, "Node"); node != goodHost {
		t.Errorf("Node = %q, want %s", node, goodHost)
	}
	p, l := primary.hits.Load(), limited.hits.Load()
	var r esploraAddress
	if err := s.get(context.Background(), addressPath(bip84Receive0), &r); err != nil {
		t.Fatal(err)
	}
	if primary.hits.Load() != p || limited.hits.Load() != l {
		t.Error("servers that failed were asked again before the one that answers")
	}
}
