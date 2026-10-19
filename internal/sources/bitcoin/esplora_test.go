package bitcoin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
)

// fakeStats is one address's Esplora state.
type fakeStats struct {
	chainFunded, chainSpent int64
	chainTx                 int
	memFunded, memSpent     int64
	memTx                   int
	utxos                   []map[string]any
}

// fakeEsplora serves /address/{a}, /address/{a}/utxo and /blocks/tip/height under /api.
type fakeEsplora struct {
	mu        sync.Mutex
	stats     map[string]*fakeStats
	hits      map[string]int // request path → count
	tip       int64
	tipStatus int    // non-zero: reply with this status instead of the height
	failAddr  string // reply 400 for this address
	delay     time.Duration

	inflight, maxInflight atomic.Int32
}

func newFake() *fakeEsplora {
	return &fakeEsplora{stats: map[string]*fakeStats{}, hits: map[string]int{}, tip: 867530}
}

func (f *fakeEsplora) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		m := f.maxInflight.Load()
		if n <= m || f.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(f.delay)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[r.URL.Path]++
	path, ok := strings.CutPrefix(r.URL.Path, "/api")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case path == "/blocks/tip/height":
		if f.tipStatus != 0 {
			http.Error(w, "tip unavailable", f.tipStatus)
			return
		}
		fmt.Fprint(w, f.tip)
	case strings.HasPrefix(path, "/address/"):
		rest := strings.TrimPrefix(path, "/address/")
		addr, isUTXO := strings.CutSuffix(rest, "/utxo")
		if addr == f.failAddr {
			http.Error(w, "Invalid Bitcoin address", http.StatusBadRequest)
			return
		}
		st := f.stats[addr]
		if st == nil {
			st = &fakeStats{}
		}
		if isUTXO {
			if st.utxos == nil {
				st.utxos = []map[string]any{}
			}
			json.NewEncoder(w).Encode(st.utxos)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"address": addr,
			"chain_stats": map[string]any{
				"funded_txo_count": 1, "funded_txo_sum": st.chainFunded,
				"spent_txo_count": 0, "spent_txo_sum": st.chainSpent, "tx_count": st.chainTx,
			},
			"mempool_stats": map[string]any{
				"funded_txo_count": 0, "funded_txo_sum": st.memFunded,
				"spent_txo_count": 0, "spent_txo_sum": st.memSpent, "tx_count": st.memTx,
			},
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeEsplora) hitCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeEsplora) totalHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.hits {
		n += c
	}
	return n
}

var fixedNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// startSource runs a fake Esplora and builds a source against it.
func startSource(t *testing.T, f *fakeEsplora, cfg Config) (*Source, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg.EsploraURL = srv.URL + "/api/" // trailing slash must be tolerated
	client := &httpx.Client{HTTP: srv.Client(), MaxRetries: 0, BaseDelay: time.Millisecond}
	s, err := New(cfg, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return fixedNow }
	return s, srv
}

func mustAddr(t *testing.T, s *Source, chain, i int) string {
	t.Helper()
	a, err := s.addressAt(chain, i)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// usedWallet is the BIP84 test wallet with receive 0, 1, 5 and change 0 used.
func usedWallet(t *testing.T, gap int) (*Source, *fakeEsplora, *httptest.Server) {
	t.Helper()
	f := newFake()
	s, srv := startSource(t, f, Config{ID: "cold", Key: bip84Zpub, GapLimit: gap})
	f.stats[mustAddr(t, s, 0, 0)] = &fakeStats{chainFunded: 100_000, chainTx: 1}
	f.stats[mustAddr(t, s, 0, 1)] = &fakeStats{chainFunded: 50_000, chainSpent: 50_000, chainTx: 2}
	f.stats[mustAddr(t, s, 0, 5)] = &fakeStats{memFunded: 20_000, memTx: 1}
	f.stats[mustAddr(t, s, 1, 0)] = &fakeStats{chainFunded: 30_000, chainTx: 1, memSpent: 10_000, memTx: 1}
	return s, f, srv
}

func TestScanStopsAtGapLimit(t *testing.T) {
	s, f, _ := usedWallet(t, 3)
	addrs, err := s.Addresses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, a := range addrs {
		paths = append(paths, a.Path)
	}
	want := "0/0 0/1 0/2 0/3 0/4 0/5 0/6 0/7 0/8 1/0 1/1 1/2 1/3"
	if got := strings.Join(paths, " "); got != want {
		t.Fatalf("scanned paths:\n got %s\nwant %s", got, want)
	}
	for i := 0; i <= 8; i++ {
		if n := f.hitCount("/api/address/" + mustAddr(t, s, 0, i)); n != 1 {
			t.Errorf("receive %d fetched %d times, want 1", i, n)
		}
	}
	if n := f.hitCount("/api/address/" + mustAddr(t, s, 0, 9)); n != 0 {
		t.Errorf("receive 9 fetched beyond the gap limit")
	}
	if n := f.hitCount("/api/address/" + mustAddr(t, s, 1, 6)); n != 0 {
		t.Errorf("change 6 fetched beyond the gap-limit batch")
	}
}

func TestAddressBalancesIncludeMempool(t *testing.T) {
	s, _, _ := usedWallet(t, 3)
	addrs, err := s.Addresses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]core.Address{}
	for _, a := range addrs {
		byPath[a.Path] = a
	}
	checks := []struct {
		path    string
		balance float64
		txs     int
		change  bool
	}{
		{"0/0", 0.001, 1, false},
		{"0/1", 0, 2, false},
		{"0/5", 0.0002, 1, false},
		{"0/2", 0, 0, false},
		{"1/0", 0.0002, 2, true},
	}
	for _, c := range checks {
		a := byPath[c.path]
		if a.Balance != c.balance || a.TxCount != c.txs || a.Change != c.change {
			t.Errorf("%s: balance %v txs %d change %v; want %v %d %v", c.path, a.Balance, a.TxCount, a.Change, c.balance, c.txs, c.change)
		}
	}
	if byPath["0/0"].Address != "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu" {
		t.Errorf("0/0 address = %s", byPath["0/0"].Address)
	}
}

func TestFetch(t *testing.T) {
	s, _, srv := usedWallet(t, 3)
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.WalletID != "cold" || !res.AsOf.Equal(fixedNow) || len(res.Warnings) != 0 {
		t.Fatalf("result header: %+v", res)
	}
	want := core.Holding{WalletID: "cold", AssetKey: "BTC", Symbol: "BTC", Name: "Bitcoin", Amount: 0.0014, AmountStr: "0.00140000"}
	if len(res.Holdings) != 1 || res.Holdings[0] != want {
		t.Fatalf("holdings = %+v, want [%+v]", res.Holdings, want)
	}
	u, _ := url.Parse(srv.URL)
	wantMeta := []core.KV{
		{K: "Imported via", V: "xpub / zpub"},
		{K: "Script", V: "Native SegWit (p2wpkh)"},
		{K: "Derivation", V: "m/84'/0'/0'"},
		{K: "Addresses used", V: "4 of 13"},
		{K: "Node", V: u.Host},
		{K: "Block height", V: "867530"},
	}
	if fmt.Sprint(res.Meta) != fmt.Sprint(wantMeta) {
		t.Fatalf("meta:\n got %v\nwant %v", res.Meta, wantMeta)
	}
}

func TestFetchTipFailureIsWarning(t *testing.T) {
	s, f, _ := usedWallet(t, 3)
	f.tipStatus = http.StatusServiceUnavailable
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("tip failure must not fail the fetch: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "block height") {
		t.Fatalf("warnings = %q", res.Warnings)
	}
	for _, kv := range res.Meta {
		if kv.K == "Block height" {
			t.Fatalf("block height meta present after tip failure: %v", kv)
		}
	}
	if res.Holdings[0].AmountStr != "0.00140000" {
		t.Fatalf("holding = %+v", res.Holdings[0])
	}
}

func TestFetchAddressFailureIsError(t *testing.T) {
	f := newFake()
	s, _ := startSource(t, f, Config{ID: "cold", Key: bip84Zpub, GapLimit: 3})
	f.failAddr = mustAddr(t, s, 0, 1)
	res, err := s.Fetch(context.Background())
	if err == nil {
		t.Fatal("want error when an address lookup fails")
	}
	if res.Err == "" || res.WalletID != "cold" {
		t.Fatalf("error result = %+v", res)
	}
	if strings.Contains(err.Error(), bip84Zpub) {
		t.Fatalf("error leaks xpub: %v", err)
	}
}

func TestZeroBalanceStillReturnsBTCRow(t *testing.T) {
	s, _ := startSource(t, newFake(), Config{ID: "empty", Key: bip84Zpub, GapLimit: 2})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Holdings) != 1 || res.Holdings[0].Amount != 0 || res.Holdings[0].AmountStr != "0.00000000" {
		t.Fatalf("holdings = %+v", res.Holdings)
	}
	if got := metaValue(res.Meta, "Addresses used"); got != "0 of 4" {
		t.Fatalf("Addresses used = %q", got)
	}
}

func metaValue(meta []core.KV, k string) string {
	for _, kv := range meta {
		if kv.K == k {
			return kv.V
		}
	}
	return ""
}

func TestUTXOs(t *testing.T) {
	s, f, _ := usedWallet(t, 3)
	a00, a05, a10 := mustAddr(t, s, 0, 0), mustAddr(t, s, 0, 5), mustAddr(t, s, 1, 0)
	f.mu.Lock()
	f.stats[a00].utxos = []map[string]any{
		{"txid": "aa", "vout": 1, "value": 60_000, "status": map[string]any{"confirmed": true, "block_height": 800_000, "block_hash": "00ff", "block_time": 1}},
		{"txid": "bb", "vout": 0, "value": 40_000, "status": map[string]any{"confirmed": true, "block_height": 800_001}},
	}
	f.stats[a05].utxos = []map[string]any{{"txid": "cc", "vout": 3, "value": 20_000, "status": map[string]any{"confirmed": false}}}
	f.stats[a10].utxos = []map[string]any{{"txid": "dd", "vout": 0, "value": 20_000, "status": map[string]any{"confirmed": true, "block_height": 800_100}}}
	f.mu.Unlock()

	utxos, err := s.UTXOs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []core.UTXO{
		{TxID: "aa", Vout: 1, Address: a00, Value: 0.0006, Confirmed: true, Height: 800_000},
		{TxID: "bb", Vout: 0, Address: a00, Value: 0.0004, Confirmed: true, Height: 800_001},
		{TxID: "cc", Vout: 3, Address: a05, Value: 0.0002},
		{TxID: "dd", Vout: 0, Address: a10, Value: 0.0002, Confirmed: true, Height: 800_100},
	}
	if fmt.Sprint(utxos) != fmt.Sprint(want) {
		t.Fatalf("utxos:\n got %+v\nwant %+v", utxos, want)
	}
	// Only addresses holding a balance are asked for UTXOs.
	if n := f.hitCount("/api/address/" + mustAddr(t, s, 0, 1) + "/utxo"); n != 0 {
		t.Errorf("fetched UTXOs of a zero-balance address")
	}
}

func TestNextReceiveAddress(t *testing.T) {
	s, _, _ := usedWallet(t, 3)
	got, err := s.NextReceiveAddress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := mustAddr(t, s, 0, 6); got != want {
		t.Fatalf("next receive = %s, want index 6 %s", got, want)
	}

	fresh, _ := startSource(t, newFake(), Config{Key: bip84Zpub, GapLimit: 3})
	got, err = fresh.NextReceiveAddress(context.Background())
	if err != nil || got != "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu" {
		t.Fatalf("unused wallet next receive = %s, %v; want index 0", got, err)
	}
}

func TestAddressesReuseRecentScan(t *testing.T) {
	s, f, _ := usedWallet(t, 3)
	ctx := context.Background()
	if _, err := s.Addresses(ctx); err != nil {
		t.Fatal(err)
	}
	before := f.totalHits()
	s.now = func() time.Time { return fixedNow.Add(scanTTL / 2) }
	if _, err := s.Addresses(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextReceiveAddress(ctx); err != nil {
		t.Fatal(err)
	}
	if f.totalHits() != before {
		t.Fatalf("rescanned within scanTTL: %d → %d requests", before, f.totalHits())
	}
	s.now = func() time.Time { return fixedNow.Add(2 * scanTTL) }
	if _, err := s.Addresses(ctx); err != nil {
		t.Fatal(err)
	}
	if f.totalHits() == before {
		t.Fatal("did not rescan after the cache expired")
	}
}

func TestSingleAddressWallet(t *testing.T) {
	const addr = "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"
	f := newFake()
	f.stats[addr] = &fakeStats{chainFunded: 250_000_000, chainTx: 3}
	s, _ := startSource(t, f, Config{ID: "paper", Key: addr, GapLimit: 5})
	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Holdings[0].AmountStr != "2.50000000" || res.Holdings[0].Amount != 2.5 {
		t.Fatalf("holding = %+v", res.Holdings[0])
	}
	if n := f.totalHits(); n != 2 { // one address lookup + tip height
		t.Fatalf("requests = %d, want 2", n)
	}
	if v := metaValue(res.Meta, "Imported via"); v != "Address" {
		t.Errorf("Imported via = %q", v)
	}
	if v := metaValue(res.Meta, "Script"); v != "Native SegWit (p2wpkh)" {
		t.Errorf("Script = %q", v)
	}
	if v := metaValue(res.Meta, "Addresses used"); v != "1 of 1" {
		t.Errorf("Addresses used = %q", v)
	}
	if v := metaValue(res.Meta, "Derivation"); v != "" {
		t.Errorf("Derivation = %q for a plain address", v)
	}
	next, err := s.NextReceiveAddress(context.Background())
	if err != nil || next != "" {
		t.Fatalf("NextReceiveAddress = %q, %v; want empty", next, err)
	}
	addrs, err := s.Addresses(context.Background())
	if err != nil || len(addrs) != 1 || addrs[0].Address != addr || addrs[0].Path != "" || addrs[0].TxCount != 3 {
		t.Fatalf("Addresses = %+v, %v", addrs, err)
	}
}

func TestImportedViaAndMultisigLabel(t *testing.T) {
	cases := []struct {
		cfg    Config
		via    string
		script string
	}{
		{Config{Descriptor: caravanDescriptor}, "Output descriptor", "2-of-3 multisig (p2wsh)"},
		{Config{WalletFile: readFixture(t, "caravan.json")}, "Wallet file", "2-of-3 multisig (p2wsh)"},
		{Config{Key: bip86Xpub, ScriptType: "p2tr"}, "xpub / zpub", "Taproot (p2tr)"},
	}
	for _, c := range cases {
		c.cfg.GapLimit = 1
		s, _ := startSource(t, newFake(), c.cfg)
		res, err := s.Fetch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if v := metaValue(res.Meta, "Imported via"); v != c.via {
			t.Errorf("Imported via = %q, want %q", v, c.via)
		}
		if v := metaValue(res.Meta, "Script"); v != c.script {
			t.Errorf("Script = %q, want %q", v, c.script)
		}
	}
}

func TestScanConcurrencyIsLimited(t *testing.T) {
	f := newFake()
	f.delay = 15 * time.Millisecond
	s, _ := startSource(t, f, Config{Key: bip84Zpub, GapLimit: 20})
	if _, err := s.Addresses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := f.maxInflight.Load(); m > 4 || m < 2 {
		t.Fatalf("max concurrent requests = %d, want 2..4", m)
	}
}
