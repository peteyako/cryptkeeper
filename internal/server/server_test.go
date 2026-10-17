package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/activity"
	"crypto-portfolio/internal/alerts"
	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/store"
	"crypto-portfolio/internal/tax"
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
	act     *activity.Store
	rules   *alerts.RuleStore
	ps      *portfolios.Store
	reg     *sources.Registry
	ref     *portfolio.Refresher
	syncer  *activity.Syncer // tests set its Sources
	dataDir string
	db      *store.DB
	box     *secrets.Box
	// price is the fake pricer's USD price per asset key (100 when not set). Set it before
	// the first refresh.
	price    map[string]float64
	taxSet   *tax.SettingsStore
	taxYears *tax.YearStore
	deps     Deps // what the server was built from, for tests that call its methods directly
}

func newEnv(t *testing.T, password string, opts ...func(*Deps)) *env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "cryptkeep.db"))
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
				if d.APISecret == "" {
					return nil, errors.New("needs its API key and secret: edit this source to add them")
				}
				return fakeExchange{d.ID}, nil
			case sources.TypeBitcoin:
				return fakeExchange{d.ID}, nil
			}
			return fakeExchange{d.ID}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	priceOf := map[string]float64{}
	prices := func(_ context.Context, reqs []portfolio.PriceReq) map[string]core.Price {
		out := map[string]core.Price{}
		for _, r := range reqs {
			usd, ok := priceOf[r.Key]
			if !ok {
				usd = 100
			}
			out[r.Key] = core.Price{USD: usd, AsOf: time.Now()}
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
	ps, err := portfolios.NewStore(filepath.Join(dir, "portfolios.yaml"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	ref := &portfolio.Refresher{Agg: &portfolio.Aggregator{Wallets: wallets, Prices: prices, DB: db, StaleMax: time.Hour}, Broker: broker, History: hist, Interval: time.Hour, MinForceInterval: time.Millisecond}
	act := &activity.Store{DB: db}
	cb, err := costbasis.NewManualStore(filepath.Join(dir, "costbasis.yaml"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := alerts.NewRuleStore(filepath.Join(dir, "alerts.yaml"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	taxSet, err := tax.NewSettingsStore(filepath.Join(dir, "tax.yaml"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	taxYears, err := tax.NewYearStore(filepath.Join(dir, "tax"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	engine := &alerts.Engine{Rules: rules, DB: db}
	syncer := &activity.Syncer{Store: act}
	cfg := config.Default()
	cfg.Password = password
	cfg.User = "admin"
	d := Deps{
		Cfg: cfg, DataDir: dir, Version: "test", Box: box, Registry: reg, Portfolios: ps, Refresher: ref,
		Activity: act, Syncer: syncer, CostBasis: cb, AlertRules: rules, Alerts: engine, DB: db, Broker: broker, History: hist, Prices: prices,
		TaxSettings: taxSet, TaxYears: taxYears, TaxLoc: time.UTC,
		DetectBitcoin: func(s string) (string, error) {
			if strings.HasPrefix(s, "wsh(") {
				return "descriptor", nil
			}
			return "zpub", nil
		},
	}
	for _, opt := range opts {
		opt(&d)
	}
	srv := httptest.NewServer(New(d))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, act: act, rules: rules, ps: ps, reg: reg, ref: ref, syncer: syncer, dataDir: dir, db: db, box: box,
		price: priceOf, taxSet: taxSet, taxYears: taxYears, deps: d}
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

// fixedActivity is an activity source that always lists the same entries.
type fixedActivity []core.Tx

func (f fixedActivity) Activity(context.Context, core.ActivityRequest) (core.ActivityResult, error) {
	return core.ActivityResult{Txs: f, State: []byte("done"), BackfillDone: true}, nil
}

// slowActivity is an activity source whose every sync runs out of time.
type slowActivity struct{}

func (slowActivity) Activity(context.Context, core.ActivityRequest) (core.ActivityResult, error) {
	return core.ActivityResult{}, context.DeadlineExceeded
}

func TestActivitySyncStream(t *testing.T) {
	e := newEnv(t, "")
	if _, err := e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeSolana, Label: "Phantom", Kind: core.KindHot, Address: "So11111111111111111111111111111111111111112"}); err != nil {
		t.Fatal(err)
	}
	e.syncer.Sources = func() []activity.Entry { return []activity.Entry{{ID: "sol", Label: "Phantom", Src: slowActivity{}}} }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/activity/sync", nil)
	req.Header.Set("Datastar-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	// waitFor reads events until one carries every want.
	waitFor := func(wants ...string) {
		t.Helper()
		var ev strings.Builder
		for sc.Scan() {
			if sc.Text() != "" {
				ev.WriteString(sc.Text())
				continue
			}
			got, all := ev.String(), true
			for _, w := range wants {
				all = all && strings.Contains(got, w)
			}
			if all {
				return
			}
			ev.Reset()
		}
		t.Fatalf("stream ended without an event carrying %q", wants)
	}

	// The card right away, then again once a sync has run, now explaining the failure in
	// plain language with the advice for the public Solana RPC.
	waitFor("datastar-patch-elements", `id="activity-sync"`, "Phantom", "Not synced yet")
	go e.syncer.SyncOnce(context.Background())
	waitFor(`id="activity-sync"`, "Timed out: the Solana RPC was too slow to answer.", "CryptKeep tries again every 15 min.",
		"solana.rpcUrl", "<code>context deadline exceeded</code>")
}

// TestActivitySyncStreamOffersNewEntries: on the Activity page the stream shows the "new
// activity" prompt once the entry count passes what the page was rendered with.
func TestActivitySyncStreamOffersNewEntries(t *testing.T) {
	e := newEnv(t, "")
	if _, err := e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeSolana, Label: "Phantom", Kind: core.KindHot, Address: "So11111111111111111111111111111111111111112"}); err != nil {
		t.Fatal(err)
	}
	e.syncer.Sources = func() []activity.Entry {
		return []activity.Entry{{ID: "sol", Label: "Phantom", Src: fixedActivity{core.Tx{SourceID: "sol", ExternalID: "1", Time: time.Now().Add(-time.Hour), Kind: core.TxReceive,
			Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: 1}}, Status: core.TxConfirmed}}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/activity/sync?feed=0", nil)
	req.Header.Set("Datastar-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	started := false
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, `id="activity-new"`) {
			if !started || !strings.Contains(line, "New activity synced") {
				t.Fatalf("prompt before any new entry, or empty: %s", line)
			}
			return
		}
		if strings.Contains(line, `id="activity-sync"`) && !started {
			started = true // the first card: nothing new yet, so sync one entry
			go e.syncer.SyncOnce(context.Background())
		}
	}
	t.Fatal("no new-activity prompt after the sync added an entry")
}

func TestPortfolioFlow(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}})
	e.reg.Upsert(sources.Def{ID: "hot", Type: sources.TypeManual, Label: "Hot", Holdings: []manual.Holding{{Asset: "SOL", Amount: 10}, {Asset: "BTC", Amount: 1}}})
	e.ref.Force(context.Background())

	// Create "BTC only" over all wallets.
	body, hdr := form(url.Values{"name": {"BTC only"}, "walletsMode": {"all"}, "assetsMode": {"selected"}, "assets": {"BTC"}, "watch": {"eth"}})
	_, out := e.do("POST", "/portfolios", body, hdr)
	if !strings.Contains(out, "/p/btc-only") {
		t.Fatalf("save: %s", out)
	}
	p, ok := e.ps.Get("btc-only")
	if !ok || p.Assets.All || len(p.Watch) != 1 || p.Watch[0] != "ETH" {
		t.Fatalf("stored %+v", p)
	}
	// The filtered overview shows only BTC (2 × $100) and the sidebar lists the portfolio.
	resp, page := e.do("GET", "/p/btc-only", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(page, "$200.00") || !strings.Contains(page, "BTC only") {
		t.Fatalf("filtered overview %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/p/nope", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("unknown portfolio: %d", resp.StatusCode)
	}
	// Export → delete → import (merge) restores it.
	resp, exported := e.do("GET", "/portfolios/export", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(exported, "btc-only") || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export %d %s", resp.StatusCode, exported)
	}
	e.do("POST", "/portfolios/btc-only/delete", nil, ds(nil))
	if _, ok := e.ps.Get("btc-only"); ok {
		t.Fatal("not deleted")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "portfolios.yaml")
	fw.Write([]byte(exported))
	mw.WriteField("mode", "merge")
	mw.Close()
	_, out = e.do("POST", "/portfolios/import", &buf, ds(map[string]string{"Content-Type": mw.FormDataContentType()}))
	if !strings.Contains(out, "1 added") {
		t.Fatalf("import: %s", out)
	}
	// Restore the newest backup (taken before the import).
	bs, _ := e.ps.Backups()
	if len(bs) == 0 {
		t.Fatal("no backups")
	}
	_, out = e.do("POST", "/portfolios/restore/"+bs[0].Name, nil, ds(nil))
	if !strings.Contains(out, "restored") {
		t.Fatalf("restore: %s", out)
	}
	// Removing a source drops it from explicit wallet lists.
	e.ps.Upsert(portfolios.Portfolio{ID: "hot-only", Name: "Hot only", Wallets: portfolios.Selector{IDs: []string{"hot"}}, Assets: portfolios.AllSelector()})
	e.do("POST", "/sources/hot/delete", nil, ds(nil))
	if p, _ := e.ps.Get("hot-only"); len(p.Wallets.IDs) != 0 {
		t.Fatalf("deleted source still referenced: %+v", p.Wallets)
	}
	// Editor and manager pages render.
	for _, path := range []string{"/portfolios", "/portfolios/new", "/portfolios/hot-only/edit"} {
		if resp, _ := e.do("GET", path, nil, nil); resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
}

func usd(v float64) *float64 { return &v }

func TestActivityReportsAndExports(t *testing.T) {
	e := newEnv(t, "")
	// 2025 is closed with 1 BTC in Cold at $40; the activity below is from 2025, so the
	// engine starts from that lot alone.
	e.closeYear(2025, tax.Summary{}, tax.Lot{Wallet: "cold", Asset: "BTC", Amount: 1, CostUSD: 40, Acquired: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)})
	e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}})
	e.ref.Force(context.Background()) // BTC priced at $100 by the fake pricer
	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	var txs []core.Tx
	for i := range 60 {
		txs = append(txs, core.Tx{ID: core.TxID("cold", fmt.Sprint(i)), SourceID: "cold", ExternalID: fmt.Sprint(i),
			Time: base.Add(time.Duration(i) * time.Hour), Kind: core.TxReceive, Status: core.TxConfirmed,
			Legs: []core.Leg{{AssetKey: "BTC", Symbol: "BTC", Amount: 0.01}}})
	}
	if _, err := e.act.Upsert(context.Background(), txs); err != nil {
		t.Fatal(err)
	}
	// Activity page: first 50, newest first, with a "load older" cursor.
	resp, page := e.do("GET", "/activity", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(page, `id="activity-more"`) || !strings.Contains(page, "Load older") {
		t.Fatalf("activity page %d", resp.StatusCode)
	}
	// Load older appends the remaining 10 rows into #activity-rows.
	cur := cursor(txs[10].Time)
	_, out := e.do("GET", "/activity/list?before="+cur, nil, ds(nil))
	if !strings.Contains(out, "selector #activity-rows") || !strings.Contains(out, "mode append") {
		t.Fatalf("load older must append rows: %.300s", out)
	}
	// Filter change patches the whole list (signals in the query).
	sig := url.QueryEscape(`{"actKind":"send"}`)
	_, out = e.do("GET", "/activity/list?datastar="+sig, nil, ds(nil))
	if !strings.Contains(out, "activity-list") || !strings.Contains(out, "Nothing matches") {
		t.Fatalf("filtered list: %.300s", out)
	}
	// Wallet activity tab.
	if _, page := e.do("GET", "/w/cold?tab=activity", nil, nil); !strings.Contains(page, "View all activity") {
		t.Fatal("wallet activity tab should link to all activity when there are more than 25")
	}

	// Cost basis from the tax lots → P&L and wallet tiles.
	if _, page := e.do("GET", "/reports", nil, nil); !strings.Contains(page, "$40.00") || !strings.Contains(page, "$60.00") || !strings.Contains(page, "$100.00") {
		t.Fatal("reports should show basis $40, unrealized $60 and total value $100 for 1 BTC at $100")
	}
	if _, page := e.do("GET", "/w/cold", nil, nil); !strings.Contains(page, "$40.00") {
		t.Fatal("wallet cost basis tile")
	}

	// CSV import (Coinbase) is stored as activity under the CSV pseudo-source.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("source", "coinbase")
	fw, _ := mw.CreateFormFile("file", "coinbase.csv")
	fw.Write([]byte("ID,Timestamp,Transaction Type,Asset,Quantity Transacted,Price Currency,Price at Transaction,Subtotal,Total (inclusive of fees and/or spread),Fees and/or Spread,Notes\n" +
		"abc123,2025-01-02T10:00:00Z,Buy,ETH,1,USD,$3000.00,$3000.00,$3010.00,$10.00,Bought 1 ETH\n"))
	mw.Close()
	_, out = e.do("POST", "/reports/import", &buf, ds(map[string]string{"Content-Type": mw.FormDataContentType()}))
	if !strings.Contains(out, "Imported 1 transactions") {
		t.Fatalf("csv import: %.400s", out)
	}
	if got, ok, _ := e.act.Get(context.Background(), "coinbase-csv:abc123"); !ok || got.Kind != core.TxTrade {
		t.Fatalf("imported tx %+v %v", got, ok)
	}

	// Exports.
	for path, want := range map[string]string{
		"/export/holdings.csv": "wallet,source_type,asset",
		"/export/history.csv":  "timestamp",
		"/export/pnl.csv":      "asset,amount,avg_cost,basis,value,unrealized,unrealized_pct,missing_basis_units\nBTC,1,40,40,100,60,150,0\n",
		"/export/activity.csv": "time,source,kind",
	} {
		resp, body := e.do("GET", path, nil, nil)
		if resp.StatusCode != 200 || !strings.HasPrefix(body, want) || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("%s: %d %.80q", path, resp.StatusCode, body)
		}
	}
}

func TestDedupeImportedAgainstAPI(t *testing.T) {
	txs := []core.Tx{
		{ID: "cb:1", SourceID: "cb", ExternalID: "1"},
		{ID: "coinbase-csv:1", SourceID: "coinbase-csv", ExternalID: "1"}, // same trade, already synced
		{ID: "coinbase-csv:2", SourceID: "coinbase-csv", ExternalID: "2"}, // older than the API reaches
	}
	got := dedupeImported(txs, map[string]string{"cb": sources.TypeCoinbase})
	if len(got) != 2 || got[1].ID != "coinbase-csv:2" {
		t.Fatalf("%+v", got)
	}
}

func TestAlertRulesFlow(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"type": {"price"}, "asset": {"btc"}, "above": {"100,000"}, "cooldown": {"6h"}, "enabled": {"on"}})
	_, out := e.do("POST", "/alerts", body, hdr)
	if !strings.Contains(out, "/alerts") {
		t.Fatalf("save: %s", out)
	}
	rules := e.rules.List()
	if len(rules) != 1 || rules[0].Asset != "BTC" || rules[0].Above != 100000 {
		t.Fatalf("stored %+v", rules)
	}
	// Both directions in one rule is rejected with a helpful message.
	body, hdr = form(url.Values{"type": {"price"}, "asset": {"SOL"}, "above": {"300"}, "below": {"80"}})
	if _, out := e.do("POST", "/alerts", body, hdr); !strings.Contains(out, "either above or below") {
		t.Fatalf("expected direction error: %s", out)
	}
	if _, page := e.do("GET", "/alerts", nil, nil); !strings.Contains(page, "BTC above $100,000.00") {
		t.Fatal("rule summary missing")
	}
	// Test without ntfy configured explains how to set it up.
	if _, out := e.do("POST", "/alerts/test", nil, ds(nil)); !strings.Contains(out, "ntfy.url") {
		t.Fatalf("test notification: %s", out)
	}
	e.do("POST", "/alerts/"+rules[0].ID+"/delete", nil, ds(nil))
	if len(e.rules.List()) != 0 {
		t.Fatal("rule not deleted")
	}
	for _, path := range []string{"/alerts/new", "/settings", "/reports", "/activity"} {
		if resp, _ := e.do("GET", path, nil, nil); resp.StatusCode != 200 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}

func TestDeletingSourceRemovesItsActivity(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "hot", Type: sources.TypeManual, Label: "Hot", Holdings: []manual.Holding{{Asset: "SOL", Amount: 1}}})
	e.act.Upsert(context.Background(), []core.Tx{{ID: "hot:1", SourceID: "hot", ExternalID: "1", Time: time.Now(), Kind: core.TxReceive, Status: core.TxConfirmed}})
	e.do("POST", "/sources/hot/delete", nil, ds(nil))
	if _, ok, _ := e.act.Get(context.Background(), "hot:1"); ok {
		t.Fatal("activity of a removed source must be deleted")
	}
}

func multipartFile(t *testing.T, field, name string, content []byte, extra map[string]string) (io.Reader, map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range extra {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile(field, name)
	fw.Write(content)
	mw.Close()
	return &buf, ds(map[string]string{"Content-Type": mw.FormDataContentType()})
}

func TestSettingsLogosToggle(t *testing.T) {
	e := newEnv(t, "")
	if _, page := e.do("GET", "/settings", nil, nil); !strings.Contains(page, `id="f-show-logos"`) || strings.Contains(page, `id="f-show-logos" type="checkbox" name="showLogos" value="on" checked`) {
		t.Fatal("logos toggle should render unchecked by default")
	}
	body, hdr := form(url.Values{"showLogos": {"on"}})
	_, out := e.do("POST", "/settings/logos", body, hdr)
	if !strings.Contains(out, "settings-display") || !strings.Contains(out, "checked") {
		t.Fatalf("toggle patch: %.300s", out)
	}
	// Persisted: a fresh page load (and the shell of every page) sees it.
	if _, page := e.do("GET", "/settings", nil, nil); !strings.Contains(page, "checked") {
		t.Fatal("setting not persisted")
	}
	body, hdr = form(url.Values{})
	e.do("POST", "/settings/logos", body, hdr)
	if _, page := e.do("GET", "/settings", nil, nil); strings.Contains(page, `name="showLogos" value="on" checked`) {
		t.Fatal("unchecking must turn it off")
	}
}

func TestBackupExportRestore(t *testing.T) {
	e := newEnv(t, "")
	e.reg.Upsert(sources.Def{ID: "kraken", Type: sources.TypeKraken, Label: "Kraken", APIKey: "pub-key", APISecret: "very-secret"})
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	e.ps.Upsert(portfolios.Portfolio{ID: "test", Name: "Test", Wallets: portfolios.Selector{IDs: []string{"kids"}}, Assets: portfolios.AllSelector()})
	e.rules.Upsert(alerts.Rule{ID: "btc-high", Type: alerts.TypePrice, Asset: "BTC", Above: 100000})

	resp, plain := e.do("GET", "/backup/export", nil, nil)
	if resp.StatusCode != 200 || strings.Contains(plain, "very-secret") || strings.Contains(plain, "enc:v1:") {
		t.Fatalf("export without secrets: %d\n%s", resp.StatusCode, plain)
	}
	for _, want := range []string{"sources:", "portfolios:", "id: test", "alerts:", "btc-high"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("backup missing %q:\n%s", want, plain)
		}
	}
	_, sealed := e.do("GET", "/backup/export?secrets=1", nil, nil)
	if strings.Contains(sealed, "very-secret") || !strings.Contains(sealed, "enc:v1:") {
		t.Fatal("export with secrets must carry them sealed, never in plaintext")
	}

	// The reported scenario: delete the portfolio and its source, then restore the backup.
	e.ps.Delete("test")
	e.do("POST", "/sources/kids/delete", nil, ds(nil))
	body, hdr := multipartFile(t, "file", "cryptkeep-backup.yaml", []byte(sealed), map[string]string{"mode": "merge"})
	_, out := e.do("POST", "/backup/import", body, hdr)
	if !strings.Contains(out, "backup-import-result") || !strings.Contains(out, "portfolios 1 added") {
		t.Fatalf("restore: %.500s", out)
	}
	if p, ok := e.ps.Get("test"); !ok || !p.Wallets.Has("kids") {
		t.Fatalf("portfolio not restored with its wallet: %+v", p)
	}
	if _, ok := e.reg.Get("kids"); !ok {
		t.Fatal("source not restored")
	}

	// Replace with a sources-only file: kraken goes (with cleanup), portfolios are untouched.
	file := []byte("version: 1\nsources:\n  - {id: kids, type: manual, label: Kids, kind: manual, holdings: [{asset: SOL, amount: 3}]}\n  - {id: kraken2, type: kraken, label: Kraken two, kind: exchange}\n")
	body, hdr = multipartFile(t, "file", "sources.yaml", file, map[string]string{"mode": "replace"})
	_, out = e.do("POST", "/backup/import", body, hdr)
	if !strings.Contains(out, "1 removed") {
		t.Fatalf("replace: %.400s", out)
	}
	if _, ok := e.ps.Get("test"); !ok {
		t.Fatal("sections not in the file must be kept")
	}
	e.ref.Force(context.Background())
	if _, page := e.do("GET", "/w/kraken2", nil, nil); !strings.Contains(page, "Edit this source to add them.") {
		t.Fatal("a source without secrets should ask for them")
	}
	// A seed phrase anywhere in a backup is refused.
	seed := []byte("version: 1\nsources:\n  - {id: x, type: bitcoin, label: X, kind: hardware, key: \"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about\"}\n")
	body, hdr = multipartFile(t, "file", "b.yaml", seed, map[string]string{"mode": "merge"})
	if _, out := e.do("POST", "/backup/import", body, hdr); !strings.Contains(out, "seed phrase") {
		t.Fatalf("seed import: %.300s", out)
	}
}

func TestPortfolioExportCarriesAlertRules(t *testing.T) {
	e := newEnv(t, "")
	e.ps.Upsert(portfolios.Portfolio{ID: "lt", Name: "Long-term", Wallets: portfolios.AllSelector(), Assets: portfolios.AllSelector()})
	e.rules.Upsert(alerts.Rule{ID: "btc-high", Type: alerts.TypePrice, Asset: "BTC", Above: 100000})
	_, exported := e.do("GET", "/portfolios/export", nil, nil)
	if !strings.Contains(exported, "alerts:") || !strings.Contains(exported, "btc-high") {
		t.Fatalf("export all should include alert rules:\n%s", exported)
	}
	if _, one := e.do("GET", "/portfolios/lt/export", nil, nil); strings.Contains(one, "alerts:") {
		t.Fatal("a single-portfolio export carries no alert rules")
	}
	// Restore into an empty install: portfolios and alert rules both come back.
	e.ps.Delete("lt")
	e.rules.Delete("btc-high")
	body, hdr := multipartFile(t, "file", "portfolios.yaml", []byte(exported), map[string]string{"mode": "merge"})
	_, out := e.do("POST", "/portfolios/import", body, hdr)
	if !strings.Contains(out, "Alert rules: 1 added") {
		t.Fatalf("import message: %.400s", out)
	}
	if _, ok := e.rules.Get("btc-high"); !ok {
		t.Fatal("alert rule not restored")
	}
	if _, ok := e.ps.Get("lt"); !ok {
		t.Fatal("portfolio not restored")
	}
}

func TestDatastarNavigationPatchesRegionsOnly(t *testing.T) {
	e := newEnv(t, "")
	// A normal browser load is a whole document.
	if _, page := e.do("GET", "/activity", nil, nil); !strings.Contains(page, "<html") {
		t.Fatal("full load must render the document")
	}
	// A Datastar @get of the same URL gets only the regions, as one SSE patch.
	resp, out := e.do("GET", "/activity", nil, ds(nil))
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"datastar-patch-elements", `id="page-title"`, `id="sidebar"`, `id="main"`, `id="bottomnav"`, "Activity · CryptKeep"} {
		if !strings.Contains(out, want) {
			t.Errorf("nav patch missing %q", want)
		}
	}
	for _, never := range []string{"<html", "<head>", "datastar.js", "app.css"} {
		if strings.Contains(out, never) {
			t.Errorf("nav patch must not resend %q", never)
		}
	}
	// A missing page still patches (200): Datastar would retry an error status.
	resp, out = e.do("GET", "/w/nope", nil, ds(nil))
	if resp.StatusCode != 200 || !strings.Contains(out, "doesn") {
		t.Fatalf("missing wallet via nav: %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/w/nope", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("missing wallet via full load: %d", resp.StatusCode)
	}
	// The overview keeps the chart range the client had picked.
	e.reg.Upsert(sources.Def{ID: "m", Type: sources.TypeManual, Label: "M", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}})
	e.ref.Force(context.Background())
	sig := url.QueryEscape(`{"pf":"all","range":"1M"}`)
	if _, out := e.do("GET", "/?datastar="+sig, nil, ds(nil)); !strings.Contains(out, `class="seg__btn seg--on" aria-pressed="true" data-on:click="$range = &#39;1M&#39;`) {
		t.Fatal("overview nav should render the range from the client's signal")
	}
	// Pages outside the app shell reload instead of patching into it.
	if _, out := e.do("GET", "/sources/new", nil, ds(nil)); !strings.Contains(out, "location.reload()") {
		t.Fatal("add-source flow must reload on navigation")
	}
}

func TestActivityPeriodFilter(t *testing.T) {
	e := newEnv(t, "")
	now := time.Now()
	mk := func(id string, at time.Time) core.Tx {
		return core.Tx{ID: core.TxID("w", id), SourceID: "w", ExternalID: id, Time: at, Kind: core.TxReceive, Status: core.TxConfirmed,
			Legs: []core.Leg{{AssetKey: "BTC", Symbol: "BTC", Amount: 1}}, TxHash: "hash-" + id}
	}
	e.act.Upsert(context.Background(), []core.Tx{
		mk("old", time.Date(now.Year()-2, 6, 1, 12, 0, 0, 0, time.Local)),
		mk("lastyear", time.Date(now.Year()-1, 3, 15, 12, 0, 0, 0, time.Local)),
		mk("thismonth", time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, time.Local)),
	})
	list := func(sig string) string {
		_, out := e.do("GET", "/activity/list?datastar="+url.QueryEscape(sig), nil, ds(nil))
		return out
	}
	lastYear := fmt.Sprint(now.Year() - 1)
	out := list(`{"actRange":"y` + lastYear + `"}`)
	if !strings.Contains(out, "hash-l") || strings.Contains(out, "hash-o") || strings.Contains(out, "hash-t") {
		t.Fatalf("year filter: want only last year's entry")
	}
	out = list(`{"actRange":"mtd"}`)
	if !strings.Contains(out, "hash-t") || strings.Contains(out, "hash-l") {
		t.Fatal("month to date: want only this month's entry")
	}
	from := fmt.Sprintf("%d-01-01", now.Year()-2)
	to := fmt.Sprintf("%d-12-31", now.Year()-1)
	out = list(`{"actRange":"custom","actFrom":"` + from + `","actTo":"` + to + `"}`)
	if !strings.Contains(out, "hash-o") || !strings.Contains(out, "hash-l") || strings.Contains(out, "hash-t") {
		t.Fatal("custom range: want the two older entries")
	}
	// The page lists the years with activity, and the CSV export honours the period.
	if _, page := e.do("GET", "/activity?range=y"+lastYear, nil, nil); !strings.Contains(page, `value="y`+lastYear+`" selected`) {
		t.Fatal("full load should select the period from the query")
	}
	_, csv := e.do("GET", "/export/activity.csv?range=y"+lastYear, nil, nil)
	if !strings.Contains(csv, "hash-lastyear") || strings.Contains(csv, "hash-old") || strings.Contains(csv, "hash-thismonth") {
		t.Fatalf("export should honour the period:\n%s", csv)
	}
}

func (e *env) priceKey() (string, bool) {
	e.t.Helper()
	var sealed string
	ok, err := e.db.GetJSON(context.Background(), "prices.coingeckoKey", &sealed)
	if err != nil {
		e.t.Fatal(err)
	}
	return sealed, ok
}

func TestSettingsPricesSaveSealed(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"coingeckoKey": {"abc123"}})
	_, out := e.do("POST", "/settings/prices", body, hdr)
	sealed, ok := e.priceKey()
	if !ok || !secrets.IsSealed(sealed) {
		t.Fatalf("key must be stored sealed: %q ok=%v", sealed, ok)
	}
	if plain, err := e.box.Open(sealed); err != nil || plain != "abc123" {
		t.Fatalf("open: %q %v", plain, err)
	}
	if !strings.Contains(out, "settings-prices") || !strings.Contains(out, "••••c123") || strings.Contains(out, "abc123") {
		t.Fatalf("patch: %.600s", out)
	}
}

func TestSettingsPricesClear(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"coingeckoKey": {"abc123"}})
	e.do("POST", "/settings/prices", body, hdr)
	body, hdr = form(url.Values{"clear": {"1"}})
	_, out := e.do("POST", "/settings/prices", body, hdr)
	if _, ok := e.priceKey(); ok {
		t.Fatal("key must be deleted")
	}
	if !strings.Contains(out, "not set") {
		t.Fatalf("patch: %.600s", out)
	}
}

func TestSettingsPricesNotSet(t *testing.T) {
	e := newEnv(t, "")
	_, page := e.do("GET", "/settings", nil, nil)
	if !strings.Contains(page, `id="settings-prices"`) || !strings.Contains(page, "not set") {
		t.Fatal("settings should show the prices card with the key not set")
	}
}

// ---- tax lots on the dashboard, wallet tiles and Reports --------------------------------

// closeYear saves a closed tax year whose closing lots open the next one. Call it before
// the first request: the tax result is cached for a minute.
func (e *env) closeYear(year int, sum tax.Summary, lots ...tax.Lot) {
	e.t.Helper()
	y := tax.Year{Year: year, Source: "koinly", ClosedAt: time.Date(year+1, 1, 5, 0, 0, 0, 0, time.UTC), Summary: sum, ClosingLots: lots}
	if err := e.taxYears.Save(y); err != nil {
		e.t.Fatal(err)
	}
}

// within returns the n bytes of page after the first marker ("" when it is missing).
func within(page, marker string, n int) string {
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}
	return page[i:min(len(page), i+n)]
}

func solLot(wallet string, amount, cost float64) tax.Lot {
	return tax.Lot{Wallet: wallet, Asset: "SOL", Amount: amount, CostUSD: cost, Acquired: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)}
}

func TestReportsUsesTaxLots(t *testing.T) {
	e := newEnv(t, "")
	e.price["SOL"] = 150
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol", Holdings: []manual.Holding{{Asset: "SOL", Amount: 10}}})
	e.ref.Force(context.Background())

	_, page := e.do("GET", "/reports", nil, nil)
	tile := within(page, `<span class="label">Cost basis</span>`, 300)
	if !strings.Contains(tile, "$1,000.00") || !strings.Contains(page, "Basis known for all of the value") {
		t.Errorf("basis tile: %q", tile)
	}
	row := within(page, `<td class="col-basis num">`, 900)
	if !strings.Contains(row, "$1,000.00") || !strings.Contains(row, "+$500.00") || !strings.Contains(row, "+50.00%") {
		t.Errorf("P&L row: %q", row)
	}
	if !strings.Contains(within(page, `<span class="label">Unrealized P/L</span>`, 300), "+$500.00") {
		t.Error("unrealized tile")
	}
	for _, gone := range []string{`id="basis-panel"`, "/reports/basis", "Set cost", `<span class="badge">manual</span>`} {
		if strings.Contains(page, gone) {
			t.Errorf("Reports still has %q", gone)
		}
	}
	// The overview's P/L column uses the same lots.
	_, ov := e.do("GET", "/", nil, nil)
	if !strings.Contains(within(ov, `<td class="col-pnl num"`, 400), "+50.00%") {
		t.Errorf("overview P/L cell: %q", within(ov, `<td class="col-pnl num"`, 400))
	}
	// So does the P&L export.
	_, csv := e.do("GET", "/export/pnl.csv", nil, nil)
	if !strings.Contains(csv, "\nSOL,10,100,1000,1500,500,50,0\n") {
		t.Errorf("pnl.csv:\n%s", csv)
	}
}

func TestBasisRoutesRemoved(t *testing.T) {
	e := newEnv(t, "")
	for _, path := range []string{"/reports/basis", "/reports/basis/BTC/delete"} {
		body, hdr := form(url.Values{"asset": {"BTC"}, "amount": {"1"}, "cost": {"40"}})
		hdr["Origin"] = e.srv.URL
		if resp, _ := e.do("POST", path, body, hdr); resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d, want 404 or 405", path, resp.StatusCode)
		}
	}
}

func TestWalletTilesFromLots(t *testing.T) {
	e := newEnv(t, "")
	e.price["SOL"] = 150
	// The same asset in two wallets with different costs: each wallet shows its own lots.
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000), solLot("vault", 10, 3000))
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol", Holdings: []manual.Holding{{Asset: "SOL", Amount: 10}}})
	e.reg.Upsert(sources.Def{ID: "vault", Type: sources.TypeManual, Label: "Vault", Holdings: []manual.Holding{{Asset: "SOL", Amount: 10}}})
	e.ps.Upsert(portfolios.Portfolio{ID: "solo", Name: "Solo", Wallets: portfolios.Selector{IDs: []string{"sol"}}, Assets: portfolios.AllSelector()})
	e.ref.Force(context.Background())

	for id, want := range map[string][2]string{"sol": {"$1,000.00", "+$500.00"}, "vault": {"$3,000.00", "−$1,500.00"}} {
		_, page := e.do("GET", "/w/"+id, nil, nil)
		if tile := within(page, `<span class="label">Cost basis</span>`, 300); !strings.Contains(tile, want[0]) {
			t.Errorf("%s: cost basis tile %q, want %s", id, tile, want[0])
		}
		if tile := within(page, `<span class="label">Unrealized P/L</span>`, 300); !strings.Contains(tile, want[1]) {
			t.Errorf("%s: unrealized tile %q, want %s", id, tile, want[1])
		}
	}
	// All holdings: 20 SOL worth $3,000 against $4,000; the portfolio of Sol alone: +50%.
	cell := func(path string) string {
		_, page := e.do("GET", path, nil, nil)
		return within(page, `<td class="col-pnl num"`, 400)
	}
	if c := cell("/"); !strings.Contains(c, "−25.00%") {
		t.Errorf("all holdings P/L: %q", c)
	}
	if c := cell("/p/solo"); !strings.Contains(c, "+50.00%") {
		t.Errorf("portfolio P/L uses its wallets' lots: %q", c)
	}
}

func TestReportsRealizedMergesClosedYears(t *testing.T) {
	e := newEnv(t, "")
	e.price["SOL"] = 150
	e.closeYear(2025, tax.Summary{ShortGain: 100, LongGain: 23.45}, solLot("sol", 10, 1000))
	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeManual, Label: "Sol", Holdings: []manual.Holding{{Asset: "SOL", Amount: 5}}})
	tx := func(id string, at time.Time, kind core.TxKind, legs ...core.Leg) core.Tx {
		return core.Tx{ID: core.TxID("sol", id), SourceID: "sol", ExternalID: id, Time: at, Kind: kind, Status: core.TxConfirmed, Legs: legs}
	}
	usd := func(v float64) *float64 { return &v }
	txs := []core.Tx{
		// Before the open year: already in the closed 2025, never replayed.
		tx("old", time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC), core.TxTrade,
			core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -3, USD: usd(-900)}, core.Leg{AssetKey: "USDC", Symbol: "USDC", Amount: 900, USD: usd(900)}),
		// 2026: sell 4 SOL (cost $400) for $800, and give 1 SOL away (tagged gift: not a sale).
		tx("sell", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), core.TxTrade,
			core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -4, USD: usd(-800)}, core.Leg{AssetKey: "USDC", Symbol: "USDC", Amount: 800, USD: usd(800)}),
		tx("gift", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), core.TxSend,
			core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -1, USD: usd(-170)}),
	}
	if _, err := e.act.Upsert(context.Background(), txs); err != nil {
		t.Fatal(err)
	}
	if err := e.taxSet.Update(func(s *tax.Settings) {
		s.Tags = map[string]tax.TagEntry{core.TxID("sol", "gift"): {Tag: tax.TagGift}}
	}); err != nil {
		t.Fatal(err)
	}
	e.ref.Force(context.Background())

	_, page := e.do("GET", "/reports", nil, nil)
	card := within(page, `id="realized-h"`, 1500)
	for _, want := range []string{">2026<", "+$400.00", ">2025<", "+$123.45"} {
		if !strings.Contains(card, want) {
			t.Errorf("realized card lacks %q:\n%s", want, card)
		}
	}
	if strings.Index(card, ">2026<") > strings.Index(card, ">2025<") {
		t.Error("newest year first")
	}
	// 5 SOL left at $100 each: basis $500, value $750.
	if row := within(page, `<td class="col-basis num">`, 900); !strings.Contains(row, "$500.00") || !strings.Contains(row, "+$250.00") {
		t.Errorf("P&L row: %q", row)
	}
}

func TestReportsShowsTaxSettingsError(t *testing.T) {
	e := newEnv(t, "")
	if err := os.WriteFile(filepath.Join(e.dataDir, "tax.yaml"), []byte("version: 1\nbogus: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, page := e.do("GET", "/reports", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(page, "data/tax.yaml has an error") || !strings.Contains(page, `role="alert"`) {
		t.Fatalf("%d: tax.yaml error not shown", resp.StatusCode)
	}
}

func TestSpamSet(t *testing.T) {
	row := func(key string) portfolio.HoldingRow {
		return portfolio.HoldingRow{Holding: core.Holding{AssetKey: key, Symbol: key}}
	}
	snap := portfolio.Snapshot{Wallets: []portfolio.WalletView{
		{ID: "phantom", Rows: []portfolio.HoldingRow{row("SOL")}, HiddenRows: []portfolio.HoldingRow{row("spl:Airdrop1"), row("USDC")}},
		{ID: "cb", Rows: []portfolio.HoldingRow{row("USDC")}},
	}}
	snap.Assets = []portfolio.AssetRow{{AssetKey: "SOL"}, {AssetKey: "USDC"}}
	got := spamSet([]string{"Mint9"}, snap)
	want := map[string]bool{"spl:Mint9": true, "spl:Airdrop1": true}
	if len(got) != len(want) {
		t.Fatalf("spam = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("spam lacks %s: %v", k, got)
		}
	}
}
