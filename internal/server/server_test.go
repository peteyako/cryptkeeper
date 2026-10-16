package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/store"
)

// fakeExchange stands in for Kraken/Coinbase without network access.
type fakeExchange struct{ id string }

func (f fakeExchange) ID() string { return f.id }
func (f fakeExchange) Fetch(context.Context) (core.WalletResult, error) {
	return core.WalletResult{WalletID: f.id, Holdings: []core.Holding{{WalletID: f.id, AssetKey: "BTC", Symbol: "BTC", Amount: 0.5}}}, nil
}

type env struct {
	t       *testing.T
	srv     *httptest.Server
	reg     *sources.Registry
	ref     *portfolio.Refresher
	dataDir string
}

func newEnv(t *testing.T, password string) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, _ := secrets.NewBox(bytes.Repeat([]byte{3}, 32))
	reg, err := sources.NewRegistry(filepath.Join(dir, "sources.yaml"), filepath.Join(dir, "backups"), box,
		func(d sources.Def) (core.Source, error) {
			switch d.Type {
			case sources.TypeManual:
				return manual.New(d.ID, d.Holdings), nil
			case sources.TypeKraken:
				return fakeExchange{d.ID}, nil
			case sources.TypeBitcoin:
				return fakeExchange{d.ID}, nil
			}
			return fakeExchange{d.ID}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	prices := func(_ context.Context, reqs []portfolio.PriceReq) map[string]core.Price {
		out := map[string]core.Price{}
		for _, r := range reqs {
			out[r.Key] = core.Price{USD: 100, AsOf: time.Now()}
		}
		return out
	}
	wallets := func() []portfolio.Wallet {
		var ws []portfolio.Wallet
		for _, e := range reg.Entries() {
			ws = append(ws, portfolio.Wallet{ID: e.Def.ID, Label: e.Def.Label, Type: e.Def.Type, Kind: e.Def.Kind, Source: e.Source, BuildErr: e.BuildErr})
		}
		return ws
	}
	broker := portfolio.NewBroker()
	hist := &portfolio.History{DB: db}
	ref := &portfolio.Refresher{Agg: &portfolio.Aggregator{Wallets: wallets, Prices: prices, DB: db, StaleMax: time.Hour}, Broker: broker, History: hist, Interval: time.Hour, MinForceInterval: time.Millisecond}
	cfg := config.Default()
	cfg.Password = password
	cfg.User = "admin"
	h := New(Deps{
		Cfg: cfg, DataDir: dir, Version: "test", Registry: reg, Refresher: ref, Broker: broker, History: hist, Prices: prices,
		DetectBitcoin: func(s string) (string, error) {
			if strings.HasPrefix(s, "wsh(") {
				return "descriptor", nil
			}
			return "zpub", nil
		},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, reg: reg, ref: ref, dataDir: dir}
}

func (e *env) do(method, path string, body io.Reader, hdr map[string]string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func ds(extra map[string]string) map[string]string {
	h := map[string]string{"Datastar-Request": "true", "Accept": "text/event-stream"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func form(vals url.Values) (io.Reader, map[string]string) {
	return strings.NewReader(vals.Encode()), ds(map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

func TestOverviewRendersBeforeFirstRefresh(t *testing.T) {
	e := newEnv(t, "")
	resp, body := e.do("GET", "/", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `id="dashboard"`) || !strings.Contains(body, "Syncing") {
		t.Fatalf("status %d, body has dashboard=%v", resp.StatusCode, strings.Contains(body, `id="dashboard"`))
	}
}

func TestGuardBlocksForeignPosts(t *testing.T) {
	e := newEnv(t, "")
	if resp, _ := e.do("POST", "/refresh", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("no Datastar header: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/refresh", nil, ds(map[string]string{"Origin": "http://evil.example"})); resp.StatusCode != 403 {
		t.Fatalf("cross-origin: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/refresh", nil, ds(map[string]string{"Sec-Fetch-Site": "cross-site"})); resp.StatusCode != 403 {
		t.Fatalf("cross-site: %d", resp.StatusCode)
	}
	resp, body := e.do("POST", "/refresh", nil, ds(map[string]string{"Origin": e.srv.URL}))
	if resp.StatusCode != 200 || !strings.Contains(body, "datastar-patch-elements") {
		t.Fatalf("same-origin refresh: %d %q", resp.StatusCode, body[:min(200, len(body))])
	}
}

func TestBasicAuthAndHealth(t *testing.T) {
	e := newEnv(t, "hunter2")
	if resp, _ := e.do("GET", "/", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("no creds: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/", nil)
	req.SetBasicAuth("admin", "wrong")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("wrong creds: %d", resp.StatusCode)
	}
	req.SetBasicAuth("admin", "hunter2")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Fatalf("good creds: %d", resp.StatusCode)
	}
	// Health is exempt from auth: 503 before any refresh, 200 after.
	if resp, _ := e.do("GET", "/healthz", nil, nil); resp.StatusCode != 503 {
		t.Fatalf("health before refresh: %d", resp.StatusCode)
	}
	e.ref.Force(context.Background())
	if resp, body := e.do("GET", "/healthz", nil, nil); resp.StatusCode != 200 || !strings.Contains(body, `"ok"`) {
		t.Fatalf("health after refresh: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do("GET", "/manifest.webmanifest", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("manifest must be reachable without auth: %d", resp.StatusCode)
	}
}

func TestAddManualSourceAndDelete(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"type": {"manual"}, "label": {"Mom's safe"}, "holdings": {"BTC 0.25 paper\nETH 2"}})
	resp, out := e.do("POST", "/sources", body, hdr)
	if resp.StatusCode != 200 || !strings.Contains(out, "/w/mom-s-safe") {
		t.Fatalf("save: %d %s", resp.StatusCode, out)
	}
	d, ok := e.reg.Get("mom-s-safe")
	if !ok || len(d.Holdings) != 2 || d.Kind != core.KindManual {
		t.Fatalf("stored %+v", d)
	}
	e.ref.Force(context.Background())
	resp, page := e.do("GET", "/w/mom-s-safe", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(page, "Mom&#39;s safe") {
		t.Fatalf("wallet page %d", resp.StatusCode)
	}
	resp, _ = e.do("POST", "/sources/mom-s-safe/delete", nil, ds(nil))
	if _, ok := e.reg.Get("mom-s-safe"); ok || resp.StatusCode != 200 {
		t.Fatal("not deleted")
	}
}

func TestSecretsSealedAndNeverRendered(t *testing.T) {
	e := newEnv(t, "")
	const secret = "kraken-PLAINTEXT-secret-9f3a"
	body, hdr := form(url.Values{"type": {"kraken"}, "label": {"Kraken"}, "apiKey": {"public-key-1234"}, "apiSecret": {secret}})
	if resp, out := e.do("POST", "/sources", body, hdr); resp.StatusCode != 200 || strings.Contains(out, "form-errors\">") && strings.Contains(out, "notice") {
		t.Fatalf("save: %d %s", resp.StatusCode, out)
	}
	raw, _ := os.ReadFile(filepath.Join(e.dataDir, "sources.yaml"))
	if strings.Contains(string(raw), secret) {
		t.Fatal("plaintext secret written to sources.yaml")
	}
	_, page := e.do("GET", "/sources/kraken/edit", nil, nil)
	if strings.Contains(page, secret) || strings.Contains(page, "public-key-1234") {
		t.Fatal("secret rendered into the edit form")
	}
	if !strings.Contains(page, "••••9f3a") {
		t.Fatal("expected the masked last-4 hint")
	}
	// Editing the label with blank secret fields keeps the stored secret.
	body, hdr = form(url.Values{"type": {"kraken"}, "label": {"Kraken main"}, "editId": {"kraken"}})
	e.do("POST", "/sources/kraken", body, hdr)
	d, _ := e.reg.Get("kraken")
	opened, err := e.reg.Open(d)
	if err != nil || opened.APISecret != secret || d.Label != "Kraken main" {
		t.Fatalf("edit lost the secret: %+v %v", d.Label, err)
	}
}

func TestSeedPhraseRejected(t *testing.T) {
	e := newEnv(t, "")
	seed := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	body, hdr := form(url.Values{"type": {"bitcoin"}, "label": {"Oops"}, "key": {seed}})
	_, out := e.do("POST", "/sources", body, hdr)
	if !strings.Contains(out, "seed phrase") {
		t.Fatalf("expected seed-phrase refusal, got %s", out)
	}
	if len(e.reg.Defs()) != 0 {
		t.Fatal("source saved despite seed phrase")
	}
}

func TestTestConnection(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"type": {"manual"}, "label": {"X"}, "holdings": {"SOL 3"}})
	_, out := e.do("POST", "/sources/test", body, hdr)
	if !strings.Contains(out, "test-result") || !strings.Contains(out, "Connected") {
		t.Fatalf("test result: %s", out)
	}
	body, hdr = form(url.Values{"type": {"manual"}, "label": {"X"}, "holdings": {"SOL banana"}})
	_, out = e.do("POST", "/sources/test", body, hdr)
	if !strings.Contains(out, "not a valid amount") {
		t.Fatalf("expected field error: %s", out)
	}
}

func TestStreamSendsInitialPatch(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "m", Type: sources.TypeManual, Label: "M", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}})
	e.ref.Force(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/stream", nil)
	req.Header.Set("Datastar-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "datastar-patch-elements") {
			return
		}
	}
	t.Fatal("no patch event on the stream")
}
