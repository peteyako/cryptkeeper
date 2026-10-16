package bitcoin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
)

// ---- fake Esplora with transaction history -------------------------------------------

// fio is one input or output of a fake transaction. An empty addr is an OP_RETURN output,
// or a coinbase input.
type fio struct {
	addr string
	sats int64
}

type actTx struct {
	raw     map[string]any
	parsed  esploraTx
	seq     int
	dropped bool
}

// actFake serves what Activity reads, shaped like mempool.space: /address/{a} stats,
// /address/{a}/txs (all mempool txs, then the first pageSize confirmed ones, newest first),
// /address/{a}/txs/chain/{last_seen}, /tx/{txid}/status and /blocks/tip/height.
type actFake struct {
	mu       sync.Mutex
	txs      map[string]*actTx
	seq      int
	tip      int64
	pageSize int      // confirmed txs per page; 25 on a real server
	failPath string   // reply 500 to this path
	tipFails bool     // reply 500 to the block height
	reqs     []string // every request path, /api stripped
}

func newActFake() *actFake {
	return &actFake{txs: map[string]*actTx{}, tip: 1000, pageSize: 25}
}

func txid(label string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(label))) }

func script(addr string) (hex, asm, typ string) {
	if addr == "" {
		return "6a0400000000", "OP_RETURN OP_PUSHBYTES_4 00000000", "op_return"
	}
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(addr)))[:40]
	return "0014" + h, "OP_0 OP_PUSHBYTES_20 " + h, "v0_p2wpkh"
}

func fakeOut(addr string, sats int64) map[string]any {
	hex, asm, typ := script(addr)
	o := map[string]any{"scriptpubkey": hex, "scriptpubkey_asm": asm, "scriptpubkey_type": typ, "value": sats}
	if addr != "" {
		o["scriptpubkey_address"] = addr
	}
	return o
}

func fakeStatus(height, ts int64) map[string]any {
	if height <= 0 {
		return map[string]any{"confirmed": false}
	}
	return map[string]any{"confirmed": true, "block_height": height, "block_hash": txid(fmt.Sprint("block", height)), "block_time": ts}
}

// add stores a transaction; height 0 means unconfirmed.
func (f *actFake) add(label string, height, ts, fee int64, ins, outs []fio) string {
	id := txid(label)
	var vin, vout []any
	for i, in := range ins {
		v := map[string]any{
			"txid": txid(fmt.Sprint(label, "-prev", i)), "vout": 0,
			"scriptsig": "", "scriptsig_asm": "", "witness": []string{}, "is_coinbase": in.addr == "", "sequence": 4294967293,
			"prevout": nil,
		}
		if in.addr != "" {
			v["prevout"] = fakeOut(in.addr, in.sats)
		}
		vin = append(vin, v)
	}
	for _, o := range outs {
		vout = append(vout, fakeOut(o.addr, o.sats))
	}
	f.put(map[string]any{
		"txid": id, "version": 2, "locktime": 0, "vin": vin, "vout": vout,
		"size": 200, "weight": 600, "sigops": 1, "fee": fee, "status": fakeStatus(height, ts),
	})
	return id
}

func (f *actFake) put(raw map[string]any) {
	b, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	var p esploraTx
	if err := json.Unmarshal(b, &p); err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.txs[p.TxID] = &actTx{raw: raw, parsed: p, seq: f.seq}
}

func (f *actFake) confirm(id string, height, ts int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.txs[id]
	t.raw["status"] = fakeStatus(height, ts)
	t.parsed.Status = esploraTxStatus{Confirmed: true, BlockHeight: height, BlockTime: ts}
}

func (f *actFake) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs[id].dropped = true
}

func (f *actFake) setFail(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPath = path
}

func (f *actFake) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

// requests returns the paths asked since the last clear, without the per-address stats
// lookups of the wallet scan.
func (f *actFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.reqs {
		if isStatsPath(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (f *actFake) statsRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.reqs {
		if isStatsPath(p) {
			n++
		}
	}
	return n
}

func isStatsPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/address/")
	return ok && !strings.Contains(rest, "/")
}

// history lists the txs touching addr: unconfirmed ones, then confirmed newest first.
func (f *actFake) history(addr string) (mempool, chain []*actTx) {
	for _, t := range f.txs {
		if t.dropped || !touches(&t.parsed, addr) {
			continue
		}
		if t.parsed.Status.Confirmed {
			chain = append(chain, t)
		} else {
			mempool = append(mempool, t)
		}
	}
	sort.Slice(mempool, func(i, j int) bool { return mempool[i].seq > mempool[j].seq })
	sort.Slice(chain, func(i, j int) bool {
		a, b := chain[i], chain[j]
		if a.parsed.Status.BlockHeight != b.parsed.Status.BlockHeight {
			return a.parsed.Status.BlockHeight > b.parsed.Status.BlockHeight
		}
		return a.seq > b.seq
	})
	return mempool, chain
}

func touches(t *esploraTx, addr string) bool {
	for _, in := range t.Vin {
		if in.Prevout != nil && in.Prevout.Address == addr {
			return true
		}
	}
	for _, o := range t.Vout {
		if o.Address == addr {
			return true
		}
	}
	return false
}

func raws(ts []*actTx) []map[string]any {
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.raw)
	}
	return out
}

func (f *actFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, ok := strings.CutPrefix(r.URL.Path, "/api")
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.reqs = append(f.reqs, path)
	if f.failPath != "" && path == f.failPath {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	enc := json.NewEncoder(w)
	switch {
	case path == "/blocks/tip/height":
		if f.tipFails {
			http.Error(w, "no tip", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, f.tip)
	case strings.HasPrefix(path, "/tx/") && strings.HasSuffix(path, "/status"):
		t := f.txs[strings.TrimSuffix(strings.TrimPrefix(path, "/tx/"), "/status")]
		if t == nil || t.dropped {
			http.Error(w, "Transaction not found", http.StatusNotFound)
			return
		}
		enc.Encode(t.raw["status"])
	case strings.HasPrefix(path, "/address/"):
		parts := strings.Split(strings.TrimPrefix(path, "/address/"), "/")
		mempool, chain := f.history(parts[0])
		switch {
		case len(parts) == 1:
			enc.Encode(map[string]any{
				"address":       parts[0],
				"chain_stats":   map[string]any{"funded_txo_sum": 0, "spent_txo_sum": 0, "tx_count": len(chain)},
				"mempool_stats": map[string]any{"funded_txo_sum": 0, "spent_txo_sum": 0, "tx_count": len(mempool)},
			})
		case len(parts) == 2 && parts[1] == "txs":
			mempool = mempool[:min(len(mempool), 50)]
			chain = chain[:min(len(chain), f.pageSize)]
			enc.Encode(append(raws(mempool), raws(chain)...))
		case len(parts) == 4 && parts[1] == "txs" && parts[2] == "chain":
			i := slices.IndexFunc(chain, func(t *actTx) bool { return t.parsed.TxID == parts[3] })
			if i < 0 {
				http.Error(w, "Transaction not found", http.StatusNotFound)
				return
			}
			rest := chain[i+1:]
			enc.Encode(raws(rest[:min(len(rest), f.pageSize)]))
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

// ---- test environment -----------------------------------------------------------------

const (
	extA = "bc1qexternalsendera0000000000000000000000"
	extB = "bc1qexternalreceiverb000000000000000000000"
)

// actEnv is the BIP84 test wallet (gap 3) on a fake node.
type actEnv struct {
	t  *testing.T
	s  *Source
	f  *actFake
	r0 string // receive 0, 1, 2 and change 0
	r1 string
	r2 string
	c0 string
}

func newActEnv(t *testing.T) *actEnv {
	t.Helper()
	f := newActFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := &httpx.Client{HTTP: srv.Client(), MaxRetries: 0, BaseDelay: time.Millisecond}
	s, err := New(Config{ID: "cold", Key: bip84Zpub, GapLimit: 3, EsploraURL: srv.URL + "/api"}, client)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixedNow }
	return &actEnv{
		t: t, s: s, f: f,
		r0: mustAddr(t, s, 0, 0), r1: mustAddr(t, s, 0, 1), r2: mustAddr(t, s, 0, 2), c0: mustAddr(t, s, 1, 0),
	}
}

// hoursAgo is a block time.
func hoursAgo(h int) int64 { return fixedNow.Add(-time.Duration(h) * time.Hour).Unix() }

func (e *actEnv) forgetScan() {
	e.s.mu.Lock()
	e.s.last = nil
	e.s.mu.Unlock()
}

// try runs one Activity call on a fresh scan, with an empty request log.
func (e *actEnv) try(budget int, since time.Time, state []byte) (core.ActivityResult, error) {
	e.t.Helper()
	e.forgetScan()
	e.f.clear()
	return e.s.Activity(context.Background(), core.ActivityRequest{State: state, Since: since, Budget: budget})
}

func (e *actEnv) run(budget int, since time.Time, state []byte) core.ActivityResult {
	e.t.Helper()
	res, err := e.try(budget, since, state)
	if err != nil {
		e.t.Fatalf("Activity: %v", err)
	}
	return res
}

func byExt(txs []core.Tx) map[string]core.Tx {
	m := map[string]core.Tx{}
	for _, t := range txs {
		m[t.ExternalID] = t
	}
	return m
}

func extIDs(txs []core.Tx) []string {
	out := make([]string, len(txs))
	for i, t := range txs {
		out[i] = t.ExternalID
	}
	return out
}

func labelIDs(labels ...string) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = txid(l)
	}
	return out
}

func sameSet(t *testing.T, got []string, want []string) {
	t.Helper()
	g, w := slices.Clone(got), slices.Clone(want)
	sort.Strings(g)
	sort.Strings(w)
	if !slices.Equal(g, w) {
		t.Fatalf("txs:\n got %v\nwant %v", g, w)
	}
}

func sameReqs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("requests:\n got %q\nwant %q", got, want)
	}
}

func btc(sats int64) float64 { return float64(sats) / 1e8 }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// wantLegs checks the BTC legs and fee of a tx.
func wantLegs(t *testing.T, tx core.Tx, legSats int64, feeSats int64) {
	t.Helper()
	if legSats == 0 {
		if tx.Legs == nil || len(tx.Legs) != 0 {
			t.Errorf("%s: legs = %#v, want empty non-nil", tx.ExternalID[:8], tx.Legs)
		}
	} else if len(tx.Legs) != 1 || tx.Legs[0].AssetKey != "BTC" || tx.Legs[0].Symbol != "BTC" || !near(tx.Legs[0].Amount, btc(legSats)) {
		t.Errorf("%s: legs = %#v, want one BTC leg of %d sats", tx.ExternalID[:8], tx.Legs, legSats)
	}
	switch {
	case feeSats == 0 && tx.Fee != nil:
		t.Errorf("%s: fee = %#v, want none", tx.ExternalID[:8], tx.Fee)
	case feeSats != 0 && (tx.Fee == nil || tx.Fee.AssetKey != "BTC" || tx.Fee.Symbol != "BTC" || !near(tx.Fee.Amount, btc(-feeSats))):
		t.Errorf("%s: fee = %#v, want %d sats", tx.ExternalID[:8], tx.Fee, -feeSats)
	}
}

// ---- classification ---------------------------------------------------------------------

func TestActivityReceive(t *testing.T) {
	e := newActEnv(t)
	ts := hoursAgo(5)
	id := e.f.add("recv", 990, ts, 200, []fio{{extA, 120_000}}, []fio{{e.r0, 100_000}, {extA, 19_800}})

	res := e.run(0, time.Time{}, nil)
	if len(res.Txs) != 1 {
		t.Fatalf("got %d txs, want 1: %v", len(res.Txs), extIDs(res.Txs))
	}
	tx := res.Txs[0]
	if tx.ID != "cold:"+id || tx.SourceID != "cold" || tx.ExternalID != id || tx.TxHash != id {
		t.Errorf("identity: %+v", tx)
	}
	if tx.Kind != core.TxReceive || tx.Status != core.TxConfirmed {
		t.Errorf("kind %q status %q", tx.Kind, tx.Status)
	}
	if tx.Confirmations != 11 { // tip 1000, block 990
		t.Errorf("confirmations = %d, want 11", tx.Confirmations)
	}
	if !tx.Time.Equal(time.Unix(ts, 0)) {
		t.Errorf("time = %v, want %v", tx.Time, time.Unix(ts, 0))
	}
	if tx.Counterparty != extA {
		t.Errorf("counterparty = %q", tx.Counterparty)
	}
	if tx.ExplorerURL != "" { // the fake node is not mempool.space
		t.Errorf("explorer url = %q", tx.ExplorerURL)
	}
	wantLegs(t, tx, 100_000, 0)
	if !res.BackfillDone {
		t.Error("backfill should be done")
	}
}

func TestActivitySendExcludesChangeToWalletChangeAddress(t *testing.T) {
	e := newActEnv(t)
	e.f.add("fund", 980, hoursAgo(10), 100, []fio{{extA, 600_000}}, []fio{{e.r0, 500_000}, {extA, 99_900}})
	send := e.f.add("send", 990, hoursAgo(5), 1000, []fio{{e.r0, 500_000}}, []fio{{extB, 300_000}, {e.c0, 199_000}})

	res := e.run(0, time.Time{}, nil)
	if len(res.Txs) != 2 {
		t.Fatalf("got %d txs, want 2 (the send touches r0 and c0 but counts once): %v", len(res.Txs), extIDs(res.Txs))
	}
	tx := byExt(res.Txs)[send]
	if tx.Kind != core.TxSend {
		t.Fatalf("kind = %q, want send", tx.Kind)
	}
	// The wallet lost 301_000 sats: 300_000 to the payee and 1_000 of fee. The 199_000 of
	// change coming back to the change address is not part of the payment.
	wantLegs(t, tx, -300_000, 1000)
	if tx.Counterparty != extB {
		t.Errorf("counterparty = %q, want the first output outside the wallet (%s)", tx.Counterparty, extB)
	}
	if got := tx.Legs[0].Amount + tx.Fee.Amount; !near(got, btc(-301_000)) {
		t.Errorf("legs + fee = %v, want the wallet's net change %v", got, btc(-301_000))
	}
}

func TestActivitySelfTransfer(t *testing.T) {
	e := newActEnv(t)
	e.f.add("fund", 980, hoursAgo(10), 100, []fio{{extA, 101_000}}, []fio{{e.r0, 100_000}})
	self := e.f.add("self", 990, hoursAgo(5), 1000, []fio{{e.r0, 100_000}}, []fio{{e.r1, 99_000}})

	res := e.run(0, time.Time{}, nil)
	tx := byExt(res.Txs)[self]
	if tx.Kind != core.TxTransfer {
		t.Fatalf("kind = %q, want transfer", tx.Kind)
	}
	wantLegs(t, tx, 0, 1000)
	if tx.Counterparty != "" {
		t.Errorf("counterparty = %q, want none", tx.Counterparty)
	}
	if len(res.Txs) != 2 {
		t.Errorf("got %d txs, want 2", len(res.Txs))
	}
}

func TestActivityFeeAttribution(t *testing.T) {
	e := newActEnv(t)
	f := e.f
	// The sender pays the fee of a payment to the wallet: no fee on the receiving side.
	recv := f.add("recv", 970, hoursAgo(50), 5000, []fio{{extA, 205_000}}, []fio{{e.r0, 150_000}, {extA, 50_000}})
	// The wallet funds every input: it pays the whole fee.
	send := f.add("send", 971, hoursAgo(49), 700, []fio{{e.r0, 150_000}}, []fio{{extB, 100_000}, {e.c0, 49_300}})
	// Everything the wallet put in went to an OP_RETURN and the miner.
	burn := f.add("burn", 972, hoursAgo(48), 1000, []fio{{e.c0, 1000}}, []fio{{"", 0}})
	// Collaborative tx: the wallet put in 546 sats and the other party funded the rest of the
	// 779 sat fee. The wallet cannot have paid more than it lost.
	collab := f.add("collab", 973, hoursAgo(47), 779, []fio{{extA, 546}, {e.r1, 546}}, []fio{{extB, 313}})
	// Payjoin receive: the wallet adds an input of 10_000 and gets 45_000 net; the sender pays the fee.
	payjoin := f.add("payjoin", 974, hoursAgo(46), 1000, []fio{{extA, 50_000}, {e.r2, 10_000}}, []fio{{e.r2, 55_000}, {extA, 4_000}})
	// A coinbase input has no prevout.
	coinbase := f.add("coinbase", 975, hoursAgo(45), 0, []fio{{"", 0}}, []fio{{e.r0, 625_000_000}})

	res := e.run(0, time.Time{}, nil)
	got := byExt(res.Txs)
	if len(got) != 6 {
		t.Fatalf("got %d txs: %v", len(got), extIDs(res.Txs))
	}
	cases := []struct {
		name         string
		id           string
		kind         core.TxKind
		legs, fee    int64
		counterparty string
	}{
		{"receive", recv, core.TxReceive, 150_000, 0, extA},
		{"send", send, core.TxSend, -100_000, 700, extB},
		{"burn", burn, core.TxFee, 0, 1000, ""},
		{"collaborative", collab, core.TxFee, 0, 546, ""},
		{"payjoin receive", payjoin, core.TxReceive, 45_000, 0, extA},
		{"coinbase", coinbase, core.TxReceive, 625_000_000, 0, ""},
	}
	for _, c := range cases {
		tx := got[c.id]
		if tx.Kind != c.kind {
			t.Errorf("%s: kind = %q, want %q", c.name, tx.Kind, c.kind)
		}
		if tx.Counterparty != c.counterparty {
			t.Errorf("%s: counterparty = %q, want %q", c.name, tx.Counterparty, c.counterparty)
		}
		wantLegs(t, tx, c.legs, c.fee)
	}
}

func TestActivityTxTouchingTwoWalletAddressesCountedOnce(t *testing.T) {
	e := newActEnv(t)
	f := e.f
	f0 := f.add("fund0", 970, hoursAgo(30), 100, []fio{{extA, 60_100}}, []fio{{e.r0, 60_000}})
	f1 := f.add("fund1", 971, hoursAgo(29), 100, []fio{{extA, 40_100}}, []fio{{e.r1, 40_000}})
	spend := f.add("spend", 980, hoursAgo(20), 5000, []fio{{e.r0, 60_000}, {e.r1, 40_000}}, []fio{{extB, 95_000}})
	split := f.add("split", 981, hoursAgo(19), 300, []fio{{extA, 30_300}}, []fio{{e.r1, 10_000}, {e.r2, 20_000}})

	res := e.run(0, time.Time{}, nil)
	sameSet(t, extIDs(res.Txs), []string{f0, f1, spend, split}) // spend is in the history of r0 and r1, split of r1 and r2
	got := byExt(res.Txs)
	if got[spend].Kind != core.TxSend {
		t.Errorf("spend kind = %q", got[spend].Kind)
	}
	wantLegs(t, got[spend], -95_000, 5000) // both wallet inputs are subtracted
	if got[split].Kind != core.TxReceive {
		t.Errorf("split kind = %q", got[split].Kind)
	}
	wantLegs(t, got[split], 30_000, 0) // both wallet outputs are added
	for _, id := range extIDs(res.Txs) {
		if n := strings.Count(strings.Join(extIDs(res.Txs), " "), id); n != 1 {
			t.Errorf("%s emitted %d times", id, n)
		}
	}
}

func TestActivityRealMempoolSpaceShapes(t *testing.T) {
	// Six transactions copied from GET /api/address/bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu/txs
	// (receive address 0 of the BIP84 test wallet), newest first.
	var rawTxs []map[string]any
	if err := json.Unmarshal(readFixture(t, "mempool_address_txs.json"), &rawTxs); err != nil {
		t.Fatal(err)
	}
	e := newActEnv(t)
	e.f.tip = 956_030
	for i := len(rawTxs) - 1; i >= 0; i-- { // oldest first, so the listing comes back as in the file
		e.f.put(rawTxs[i])
	}
	if e.r0 != "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu" {
		t.Fatalf("test wallet receive 0 = %s", e.r0)
	}

	res := e.run(0, time.Time{}, nil)
	got := byExt(res.Txs)
	if len(got) != 6 {
		t.Fatalf("got %d txs: %v", len(got), extIDs(res.Txs))
	}
	cases := []struct {
		id           string
		kind         core.TxKind
		legs, fee    int64
		counterparty string
		height, time int64
	}{
		// An OP_RETURN spend of a 1000 sat dust input: all of it went to the miner.
		{"fd4b2c20cea81c2319f460114d2303b2dc3494524cc07cde015eebb38033cdcd", core.TxFee, 0, 1000, "", 956025, 1782796117},
		{"f5a6d77a22949953b7e4b26b8b2eeec75557ef25fde52740d45f4f300051ea9c", core.TxReceive, 1000, 0, "bc1q4hl33ar4c7lpry6ynu8a7v8duu0rz6wumwny3m", 956025, 1782796117},
		// Another party's 294 sat input passes straight through; the wallet's 670 sats paid the fee.
		{"e98b09d87c06737944a6cab2574df0bf313949f3b66867f0bfb897cf01c83093", core.TxFee, 0, 670, "", 955637, 1782553528},
		{"db40e26384887ea2fa0a01fb4edff9b1fe393a9a8d3b9af99a49a223776cc31b", core.TxReceive, 670, 0, "bc1q923wsupdm5xm0ctaahcc7ljfuf2v4ashu8nsem", 955637, 1782553528},
		{"552a0b8652bffa19d943bd3a48c92358d14a5a91a30d71cd8621162fdfe487d8", core.TxSend, -312, 43166, "bc1qsajvsu4wnucuq33j0z6dgwrxgm8u5gx6ghaaaa", 913502, 1757204988},
		{"81669a6a0ae3c7de701a3ac3aafa75ab61514df2908fba72b5e3c34b4c4a75ee", core.TxReceive, 43_478, 0, "bc1qsatlphjcgvzlt9xhsgn0dnjus5jgwg83dr05c6", 913502, 1757204988},
	}
	for _, c := range cases {
		tx, ok := got[c.id]
		if !ok {
			t.Errorf("%s missing", c.id[:8])
			continue
		}
		if tx.Kind != c.kind || tx.Status != core.TxConfirmed || tx.Counterparty != c.counterparty {
			t.Errorf("%s: kind %q status %q counterparty %q", c.id[:8], tx.Kind, tx.Status, tx.Counterparty)
		}
		if want := int(956_030 - c.height + 1); tx.Confirmations != want {
			t.Errorf("%s: confirmations = %d, want %d", c.id[:8], tx.Confirmations, want)
		}
		if !tx.Time.Equal(time.Unix(c.time, 0)) {
			t.Errorf("%s: time = %v", c.id[:8], tx.Time)
		}
		wantLegs(t, tx, c.legs, c.fee)
	}
}

// ---- paging, budget, resume ---------------------------------------------------------------

// fiveReceives puts five confirmed receives on r0, n1 oldest, and returns their ids oldest first.
func fiveReceives(e *actEnv) []string {
	var ids []string
	for i := 1; i <= 5; i++ {
		ids = append(ids, e.f.add(fmt.Sprint("n", i), int64(900+i), hoursAgo(100-i), 100, []fio{{extA, 10_100}}, []fio{{e.r0, 10_000}}))
	}
	return ids
}

func TestActivityPagesThroughChain(t *testing.T) {
	e := newActEnv(t)
	e.f.pageSize = 2
	n := fiveReceives(e)

	res := e.run(0, time.Time{}, nil)
	// History newest first: n5 n4 | n3 n2 | n1. The first page comes from /txs, older ones
	// from /txs/chain/{last_seen}; the empty page after n1 ends the history.
	sameReqs(t, e.f.requests(),
		"/blocks/tip/height",
		"/address/"+e.r0+"/txs",
		"/address/"+e.r0+"/txs/chain/"+n[3],
		"/address/"+e.r0+"/txs/chain/"+n[1],
		"/address/"+e.r0+"/txs/chain/"+n[0],
	)
	if !slices.Equal(extIDs(res.Txs), []string{n[4], n[3], n[2], n[1], n[0]}) {
		t.Fatalf("txs (newest first): %v", extIDs(res.Txs))
	}
	if !res.BackfillDone {
		t.Error("backfill should be done")
	}
}

func TestActivityBudgetExhaustionThenResume(t *testing.T) {
	e := newActEnv(t)
	e.f.pageSize = 2
	n := fiveReceives(e)

	// tip + first page + one chain page = 3 requests.
	first := e.run(3, time.Time{}, nil)
	sameReqs(t, e.f.requests(), "/blocks/tip/height", "/address/"+e.r0+"/txs", "/address/"+e.r0+"/txs/chain/"+n[3])
	sameSet(t, extIDs(first.Txs), []string{n[4], n[3], n[2], n[1]})
	if first.BackfillDone {
		t.Fatal("backfill cannot be done with the budget spent")
	}

	// The state resumes below n2: no repeat of the first pages, no repeat of any tx.
	second := e.run(30, time.Time{}, first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height", "/address/"+e.r0+"/txs/chain/"+n[1], "/address/"+e.r0+"/txs/chain/"+n[0])
	sameSet(t, extIDs(second.Txs), []string{n[0]})
	if !second.BackfillDone {
		t.Error("backfill should be done after the resume")
	}

	// Nothing left: only the block height is read.
	third := e.run(30, time.Time{}, second.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height")
	if len(third.Txs) != 0 || !third.BackfillDone {
		t.Errorf("third call: %d txs, done %v", len(third.Txs), third.BackfillDone)
	}
}

func TestActivityBudgetOfOneOnlyReadsTheTip(t *testing.T) {
	e := newActEnv(t)
	fiveReceives(e)
	res := e.run(1, time.Time{}, nil)
	sameReqs(t, e.f.requests(), "/blocks/tip/height")
	if len(res.Txs) != 0 || res.BackfillDone {
		t.Fatalf("%d txs, done %v", len(res.Txs), res.BackfillDone)
	}
	// The state it returns is still a valid starting point.
	again := e.run(30, time.Time{}, res.State)
	if len(again.Txs) != 5 || !again.BackfillDone {
		t.Fatalf("resume: %d txs, done %v", len(again.Txs), again.BackfillDone)
	}
}

func TestActivityDefaultBudgetIs30Requests(t *testing.T) {
	e := newActEnv(t)
	e.f.pageSize = 1
	for i := 1; i <= 40; i++ {
		e.f.add(fmt.Sprint("m", i), int64(500+i), hoursAgo(1000-i), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	}
	res := e.run(0, time.Time{}, nil)
	if n := len(e.f.requests()); n != 30 {
		t.Errorf("%d requests, want exactly the default budget of 30", n)
	}
	if len(res.Txs) != 29 || res.BackfillDone { // 30 requests = tip + 29 pages of one tx
		t.Errorf("%d txs, done %v", len(res.Txs), res.BackfillDone)
	}
}

func TestActivityUnchangedAddressesAreNotRefetched(t *testing.T) {
	e := newActEnv(t)
	f := e.f
	f.add("a1", 900, hoursAgo(60), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	f.add("a2", 901, hoursAgo(59), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	f.add("b1", 902, hoursAgo(58), 10, []fio{{extA, 1100}}, []fio{{e.r1, 1000}})
	f.add("b2", 903, hoursAgo(57), 10, []fio{{extA, 1100}}, []fio{{e.r1, 1000}})
	first := e.run(0, time.Time{}, nil)
	if len(first.Txs) != 4 || !first.BackfillDone {
		t.Fatalf("first: %d txs, done %v", len(first.Txs), first.BackfillDone)
	}

	// Nothing happened: the scan's tx_count matches the state everywhere.
	idle := e.run(0, time.Time{}, first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height")
	if len(idle.Txs) != 0 || !idle.BackfillDone {
		t.Errorf("idle: %d txs, done %v", len(idle.Txs), idle.BackfillDone)
	}

	// One new tx on r1 only: r0 is left alone, and r1 stops at the tx it already covers.
	nb := f.add("b3", 910, hoursAgo(20), 10, []fio{{extA, 2100}}, []fio{{e.r1, 2000}})
	next := e.run(0, time.Time{}, idle.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height", "/address/"+e.r1+"/txs")
	sameSet(t, extIDs(next.Txs), []string{nb})
	if !next.BackfillDone {
		t.Error("backfill should still be done")
	}
}

func TestActivityNewActivityPagesBelowTheFirstPage(t *testing.T) {
	e := newActEnv(t)
	f := e.f
	f.pageSize = 2
	old1 := f.add("old1", 900, hoursAgo(90), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	old2 := f.add("old2", 901, hoursAgo(89), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	first := e.run(0, time.Time{}, nil)
	sameSet(t, extIDs(first.Txs), []string{old1, old2})

	var fresh []string
	for i := 1; i <= 3; i++ {
		fresh = append(fresh, f.add(fmt.Sprint("new", i), int64(910+i), hoursAgo(30-i), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}}))
	}
	// History: new3 new2 | new1 old2 | old1. The pass stops at old2, which it already has.
	res := e.run(0, time.Time{}, first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height", "/address/"+e.r0+"/txs", "/address/"+e.r0+"/txs/chain/"+fresh[1])
	sameSet(t, extIDs(res.Txs), fresh)
	if !res.BackfillDone {
		t.Error("backfill should be done")
	}
}

func TestActivityInterruptedNewActivityResumes(t *testing.T) {
	e := newActEnv(t)
	f := e.f
	f.pageSize = 2
	f.add("old1", 900, hoursAgo(90), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	f.add("old2", 901, hoursAgo(89), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	first := e.run(0, time.Time{}, nil)
	var fresh []string
	for i := 1; i <= 3; i++ {
		fresh = append(fresh, f.add(fmt.Sprint("new", i), int64(910+i), hoursAgo(30-i), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}}))
	}

	// Tip and the first page only: new3 and new2 arrive, new1 is still to come.
	part := e.run(2, time.Time{}, first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height", "/address/"+e.r0+"/txs")
	sameSet(t, extIDs(part.Txs), []string{fresh[2], fresh[1]})
	if part.BackfillDone {
		t.Fatal("an interrupted pass is not done")
	}

	// A fourth tx lands in between; the resume still continues below new2 (no gap, no repeat),
	// and the next call picks the late one up.
	late := f.add("late", 930, hoursAgo(5), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	rest := e.run(30, time.Time{}, part.State)
	sameReqs(t, e.f.requests(),
		"/blocks/tip/height",
		"/address/"+e.r0+"/txs/chain/"+fresh[1], // resumes the interrupted pass
		"/address/"+e.r0+"/txs",                 // then reads the late arrival
	)
	sameSet(t, extIDs(rest.Txs), []string{fresh[0], late})
	if !rest.BackfillDone {
		t.Error("backfill should be done")
	}
}

func TestActivityFailureKeepsEarlierProgress(t *testing.T) {
	e := newActEnv(t)
	e.f.pageSize = 2
	n := fiveReceives(e)
	e.f.setFail("/address/" + e.r0 + "/txs/chain/" + n[1])

	res, err := e.try(0, time.Time{}, nil)
	if err == nil {
		t.Fatal("want the request failure")
	}
	sameSet(t, extIDs(res.Txs), []string{n[4], n[3], n[2], n[1]})
	if res.BackfillDone {
		t.Error("backfill cannot be done after a failure")
	}

	e.f.setFail("")
	rest := e.run(0, time.Time{}, res.State)
	sameSet(t, extIDs(rest.Txs), []string{n[0]})
	if !rest.BackfillDone {
		t.Error("backfill should be done")
	}
}

func TestActivityStateGarbageStartsOver(t *testing.T) {
	e := newActEnv(t)
	fiveReceives(e)
	for _, state := range []string{"not json", `{"v":99,"addrs":{}}`, `null`, `{"v":1,"addrs":{"x":null}}`} {
		res := e.run(0, time.Time{}, []byte(state))
		if len(res.Txs) != 5 || !res.BackfillDone {
			t.Errorf("state %q: %d txs, done %v", state, len(res.Txs), res.BackfillDone)
		}
	}
}

func TestActivityReusesFreshScan(t *testing.T) {
	e := newActEnv(t)
	fiveReceives(e)
	if _, err := e.s.Fetch(context.Background()); err != nil { // the scan Fetch makes
		t.Fatal(err)
	}
	e.f.clear()
	res, err := e.s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.f.statsRequests(); n != 0 {
		t.Errorf("Activity looked up %d addresses again; the scan from Fetch is fresh", n)
	}
	if len(res.Txs) != 5 {
		t.Errorf("%d txs", len(res.Txs))
	}

	// Once the cached scan is older than scanTTL, Activity scans again.
	e.s.now = func() time.Time { return fixedNow.Add(2 * scanTTL) }
	e.f.clear()
	if _, err := e.s.Activity(context.Background(), core.ActivityRequest{State: res.State}); err != nil {
		t.Fatal(err)
	}
	if e.f.statsRequests() == 0 {
		t.Error("a stale scan should have been refreshed")
	}
}

// ---- pending ---------------------------------------------------------------------------------

func TestActivityPendingBecomesConfirmed(t *testing.T) {
	e := newActEnv(t)
	m := e.f.add("mempool", 0, 0, 200, []fio{{extA, 100_200}}, []fio{{e.r0, 100_000}})

	first := e.run(0, time.Time{}, nil)
	if len(first.Txs) != 1 {
		t.Fatalf("got %d txs", len(first.Txs))
	}
	tx := first.Txs[0]
	if tx.Status != core.TxPending || tx.Kind != core.TxReceive || tx.Confirmations != 0 || !tx.Time.Equal(fixedNow) {
		t.Fatalf("pending tx: status %q kind %q confirmations %d time %v", tx.Status, tx.Kind, tx.Confirmations, tx.Time)
	}

	// Still in the mempool: one status request, nothing to say.
	idle := e.run(0, time.Time{}, first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height", "/tx/"+m+"/status")
	if len(idle.Txs) != 0 {
		t.Fatalf("still pending, but %d txs re-emitted", len(idle.Txs))
	}

	// It confirms. The address's tx_count (1 either way) does not move, so only the status
	// check can notice.
	ts := hoursAgo(1)
	e.f.confirm(m, 995, ts)
	done := e.run(0, time.Time{}, idle.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height", "/tx/"+m+"/status")
	if len(done.Txs) != 1 {
		t.Fatalf("got %d txs, want the confirmed re-emit", len(done.Txs))
	}
	got := done.Txs[0]
	if got.ID != tx.ID || got.Status != core.TxConfirmed || got.Confirmations != 6 || !got.Time.Equal(time.Unix(ts, 0)) {
		t.Errorf("confirmed re-emit: id %s status %q confirmations %d time %v", got.ID, got.Status, got.Confirmations, got.Time)
	}
	if got.Kind != core.TxReceive || got.Counterparty != extA {
		t.Errorf("kind %q counterparty %q", got.Kind, got.Counterparty)
	}
	wantLegs(t, got, 100_000, 0)

	// Settled: the next call does not ask about it again.
	after := e.run(0, time.Time{}, done.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height")
	if len(after.Txs) != 0 {
		t.Errorf("%d txs after settling", len(after.Txs))
	}
}

func TestActivityPendingSeenAgainIsNotReEmitted(t *testing.T) {
	e := newActEnv(t)
	e.f.add("mempool", 0, 0, 200, []fio{{extA, 100_200}}, []fio{{e.r0, 100_000}})
	first := e.run(0, time.Time{}, nil)

	// A second tx arrives; the first page lists both, but the pending one has not changed.
	other := e.f.add("other", 0, 0, 200, []fio{{extA, 5_200}}, []fio{{e.r0, 5_000}})
	next := e.run(0, time.Time{}, first.State)
	sameSet(t, extIDs(next.Txs), []string{other})
}

func TestActivityDroppedPendingFailsAndReplacementIsFound(t *testing.T) {
	e := newActEnv(t)
	f := e.f
	f.add("fund", 980, hoursAgo(10), 100, []fio{{extA, 100_100}}, []fio{{e.r0, 100_000}})
	orig := f.add("orig", 0, 0, 300, []fio{{e.r0, 100_000}}, []fio{{extB, 60_000}, {e.c0, 39_700}})
	first := e.run(0, time.Time{}, nil)
	if tx := byExt(first.Txs)[orig]; tx.Status != core.TxPending || tx.Kind != core.TxSend {
		t.Fatalf("orig: %+v", tx)
	}

	// RBF: the original leaves the mempool and a bumped copy takes its place. Every
	// address keeps its tx_count, so only the failed status check reveals the swap.
	f.drop(orig)
	bumped := f.add("bumped", 0, 0, 900, []fio{{e.r0, 100_000}}, []fio{{extB, 60_000}, {e.c0, 39_100}})
	res := e.run(0, time.Time{}, first.State)

	sameReqs(t, e.f.requests(),
		"/blocks/tip/height",
		"/tx/"+orig+"/status",   // 404
		"/address/"+e.r0+"/txs", // r0 and c0 are read again
		"/address/"+e.c0+"/txs",
	)
	got := byExt(res.Txs)
	if len(got) != 2 {
		t.Fatalf("got %v", extIDs(res.Txs))
	}
	if g := got[orig]; g.Status != core.TxFailed || g.Note == "" || g.Kind != core.TxSend {
		t.Errorf("replaced tx: status %q note %q kind %q", g.Status, g.Note, g.Kind)
	}
	if g := got[bumped]; g.Status != core.TxPending || g.Kind != core.TxSend {
		t.Errorf("replacement: status %q kind %q", g.Status, g.Kind)
	} else {
		wantLegs(t, g, -60_000, 900)
	}
}

// ---- Since ---------------------------------------------------------------------------------------

func TestActivitySinceStopsBackfill(t *testing.T) {
	e := newActEnv(t)
	e.f.pageSize = 2
	day := func(d int) int64 { return fixedNow.Add(-time.Duration(d) * 24 * time.Hour).Unix() }
	var n []string // n[0] oldest
	for i, d := range []int{12, 11, 10, 3, 2, 1} {
		n = append(n, e.f.add(fmt.Sprint("d", d), int64(800+i), day(d), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}}))
	}
	since := fixedNow.Add(-5 * 24 * time.Hour)

	// n5 n4 | n3 n2: n3 (10 days old) is the first tx before Since, so the walk stops there
	// without asking for the next page.
	first := e.run(0, since, nil)
	sameReqs(t, e.f.requests(),
		"/blocks/tip/height",
		"/address/"+e.r0+"/txs",
		"/address/"+e.r0+"/txs/chain/"+n[4],
	)
	sameSet(t, extIDs(first.Txs), []string{n[5], n[4], n[3]})
	if !first.BackfillDone {
		t.Fatal("backfill down to Since is done")
	}

	// Same window again: nothing to do. A window that moved forward changes nothing either.
	e.run(0, since, first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height")
	e.run(0, fixedNow.Add(-2*24*time.Hour), first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height")

	// A window reaching further back resumes below the last tx read.
	wide := e.run(0, fixedNow.Add(-100*24*time.Hour), first.State)
	sameReqs(t, e.f.requests(),
		"/blocks/tip/height",
		"/address/"+e.r0+"/txs/chain/"+n[3],
		"/address/"+e.r0+"/txs/chain/"+n[1],
		"/address/"+e.r0+"/txs/chain/"+n[0],
	)
	sameSet(t, extIDs(wide.Txs), []string{n[2], n[1], n[0]})
	if !wide.BackfillDone {
		t.Error("backfill should be done")
	}
}

func TestActivitySinceNewerThanAllHistory(t *testing.T) {
	e := newActEnv(t)
	old := e.f.add("old", 800, hoursAgo(24*30), 10, []fio{{extA, 1100}}, []fio{{e.r0, 1000}})
	since := fixedNow.Add(-24 * time.Hour)

	first := e.run(0, since, nil)
	if len(first.Txs) != 0 || !first.BackfillDone {
		t.Fatalf("%d txs, done %v", len(first.Txs), first.BackfillDone)
	}
	idle := e.run(0, since, first.State)
	sameReqs(t, e.f.requests(), "/blocks/tip/height")

	// Widening the window starts the address over.
	wide := e.run(0, time.Time{}, idle.State)
	sameSet(t, extIDs(wide.Txs), []string{old})
	if !wide.BackfillDone {
		t.Error("backfill should be done")
	}
}

// ---- odds and ends -----------------------------------------------------------------------------

func TestActivitySingleAddressWallet(t *testing.T) {
	f := newActFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	const addr = "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"
	s, err := New(Config{ID: "watch", Key: addr, EsploraURL: srv.URL + "/api"}, &httpx.Client{HTTP: srv.Client(), BaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	id := f.add("in", 900, hoursAgo(3), 100, []fio{{extA, 5100}}, []fio{{addr, 5000}})
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Txs) != 1 || res.Txs[0].ExternalID != id || res.Txs[0].ID != "watch:"+id || !res.BackfillDone {
		t.Fatalf("%+v", res)
	}
}

func TestActivityTipFailureIsNotFatal(t *testing.T) {
	e := newActEnv(t)
	e.f.add("in", 900, hoursAgo(3), 100, []fio{{extA, 5100}}, []fio{{e.r0, 5000}})
	e.f.tipFails = true
	res := e.run(0, time.Time{}, nil)
	if len(res.Txs) != 1 || res.Txs[0].Confirmations != 0 || res.Txs[0].Status != core.TxConfirmed {
		t.Fatalf("%+v", res.Txs)
	}
}

func TestActivityCancelledContext(t *testing.T) {
	e := newActEnv(t)
	e.f.add("in", 900, hoursAgo(3), 100, []fio{{extA, 5100}}, []fio{{e.r0, 5000}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.s.Activity(ctx, core.ActivityRequest{}); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
}

func TestExplorerTxURL(t *testing.T) {
	mem, err := New(Config{Key: bip84Zpub}, nil) // default node: mempool.space
	if err != nil {
		t.Fatal(err)
	}
	if got := mem.explorerTxURL("abc"); got != "https://mempool.space/tx/abc" {
		t.Errorf("mempool.space: %q", got)
	}
	for _, node := range []string{"https://blockstream.info/api", "http://umbrel.local:3006/api", "https://mempool.space.evil.example/api"} {
		s, err := New(Config{Key: bip84Zpub, EsploraURL: node}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.explorerTxURL("abc"); got != "" {
			t.Errorf("%s: explorer url %q, want none", node, got)
		}
	}
}

func TestActivityStateIsJSON(t *testing.T) {
	e := newActEnv(t)
	m := e.f.add("mempool", 0, 0, 200, []fio{{extA, 100_200}}, []fio{{e.r0, 100_000}})
	e.f.add("conf", 900, hoursAgo(3), 100, []fio{{extA, 5100}}, []fio{{e.r1, 5000}})
	res := e.run(0, time.Time{}, nil)

	var st activityState
	if err := json.Unmarshal(res.State, &st); err != nil {
		t.Fatalf("state is not JSON: %v", err)
	}
	if st.Tip != 1000 || len(st.Pending) != 1 || st.Pending[0].TxID != m {
		t.Errorf("tip %d pending %+v", st.Tip, st.Pending)
	}
	c := st.Addrs[e.r1]
	if c == nil || c.TxCount != 1 || c.Newest != txid("conf") || c.Oldest != txid("conf") {
		t.Errorf("cursor for r1: %+v", c)
	}
}
