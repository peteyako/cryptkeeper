package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/sources/solana/defi"
)

// --- fake ledger -------------------------------------------------------------------------

type ledgerEntry struct {
	sig  string
	slot int64
	at   int64
	err  any
}

// fakeLedger is a JSON-RPC endpoint that serves per-address signature lists and transactions on
// top of fakeChain (which answers getTokenAccountsByOwner). Nothing here touches the network.
type fakeLedger struct {
	t     *testing.T
	chain *fakeChain
	srv   *httptest.Server

	mu       sync.Mutex
	slot     int64
	next     int
	lists    map[string][]ledgerEntry   // address -> entries, newest first
	txs      map[string]json.RawMessage // signature -> getTransaction result; absent = not served (null)
	calls    map[string]int
	fetched  map[string]int   // getTransaction requests per signature
	sigReqs  []map[string]any // getSignaturesForAddress options, as received
	listHTTP int              // non-zero: getSignaturesForAddress answers with this HTTP status
	txHTTP   map[string]int   // signature -> HTTP status for getTransaction
	txRPCErr map[string]int   // signature -> JSON-RPC error code for getTransaction
}

// set mutates the ledger's fault-injection state while no request is in flight.
func (l *fakeLedger) set(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f()
}

func newLedger(t *testing.T, owner string) (*fakeLedger, *Source) {
	t.Helper()
	return newLedgerWith(t, owner, nil)
}

// newLedgerWith is newLedger with a hook to adjust the source's config.
func newLedgerWith(t *testing.T, owner string, mutate func(*Config)) (*fakeLedger, *Source) {
	t.Helper()
	chain := newFakeChain(t)
	chain.owner = owner
	l := &fakeLedger{
		t: t, chain: chain,
		lists: map[string][]ledgerEntry{}, txs: map[string]json.RawMessage{},
		calls: map[string]int{}, fetched: map[string]int{},
		txHTTP: map[string]int{}, txRPCErr: map[string]int{},
	}
	l.srv = httptest.NewServer(l)
	t.Cleanup(l.srv.Close)
	cfg := Config{
		ID: "phantom", Label: "Phantom", Address: owner,
		RPCURL:           l.srv.URL + "/?api-key=" + testRPCKey,
		JupiterTokensURL: l.srv.URL + "/jup",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg, fastClient(l.srv), NewMemoryMetaCache())
	if err != nil {
		t.Fatal(err)
	}
	return l, s
}

func (l *fakeLedger) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls["total"]
}

func (l *fakeLedger) callCount(method string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[method]
}

func (l *fakeLedger) fetchCount(sig string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fetched[sig]
}

// addRaw registers a transaction result: it appears (as the newest entry) in the signature list
// of every account key, like on a real node.
func (l *fakeLedger) addRaw(sig string, at int64, txErr any, keys []string, result json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.slot++
	l.txs[sig] = result
	for _, k := range slices.Compact(slices.Sorted(slices.Values(keys))) {
		l.lists[k] = append([]ledgerEntry{{sig: sig, slot: l.slot, at: at, err: txErr}}, l.lists[k]...)
	}
}

func (l *fakeLedger) add(spec txSpec) string {
	l.t.Helper()
	if spec.sig == "" {
		l.mu.Lock()
		l.next++
		spec.sig = fmt.Sprintf("sig%04d", l.next)
		l.mu.Unlock()
	}
	l.mu.Lock()
	slot := l.slot + 1
	l.mu.Unlock()
	l.addRaw(spec.sig, spec.at, spec.err, spec.finalKeys(), spec.result(slot))
	return spec.sig
}

func (l *fakeLedger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad", 400)
		return
	}
	l.mu.Lock()
	l.calls["total"]++
	l.calls[req.Method]++
	l.mu.Unlock()

	switch req.Method {
	case "getSignaturesForAddress":
		l.serveSignatures(w, req.Params)
	case "getTransaction":
		l.serveTransaction(w, req.Params)
	default:
		r.Body = io.NopCloser(bytes.NewReader(body))
		l.chain.serveRPC(w, r)
	}
}

func (l *fakeLedger) serveSignatures(w http.ResponseWriter, params []json.RawMessage) {
	var addr string
	var opts struct {
		Limit      int    `json:"limit"`
		Before     string `json:"before"`
		Until      string `json:"until"`
		Commitment string `json:"commitment"`
	}
	json.Unmarshal(params[0], &addr)
	json.Unmarshal(params[1], &opts)
	var raw map[string]any
	json.Unmarshal(params[1], &raw)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sigReqs = append(l.sigReqs, raw)
	if l.listHTTP != 0 {
		w.WriteHeader(l.listHTTP)
		fmt.Fprint(w, `{"error":"unavailable"}`)
		return
	}
	if opts.Commitment != "confirmed" {
		l.t.Errorf("getSignaturesForAddress commitment = %q", opts.Commitment)
	}
	list := l.lists[addr]
	start := 0
	if opts.Before != "" {
		i := slices.IndexFunc(list, func(e ledgerEntry) bool { return e.sig == opts.Before })
		if i < 0 {
			l.t.Errorf("before %q is not in the list of %s", opts.Before, addr)
		}
		start = i + 1
	}
	end := len(list)
	if opts.Until != "" {
		if i := slices.IndexFunc(list[start:], func(e ledgerEntry) bool { return e.sig == opts.Until }); i >= 0 {
			end = start + i
		}
	}
	if opts.Limit > 0 && end-start > opts.Limit {
		end = start + opts.Limit
	}
	out := []any{}
	for _, e := range list[start:end] {
		var bt any
		if e.at != 0 {
			bt = e.at
		}
		out = append(out, map[string]any{
			"signature": e.sig, "slot": e.slot, "err": e.err, "memo": nil,
			"blockTime": bt, "confirmationStatus": "finalized",
		})
	}
	rpcReply(w, out)
}

func (l *fakeLedger) serveTransaction(w http.ResponseWriter, params []json.RawMessage) {
	var sig string
	json.Unmarshal(params[0], &sig)
	var opts struct {
		Encoding string `json:"encoding"`
		MaxVer   *int   `json:"maxSupportedTransactionVersion"`
		Commit   string `json:"commitment"`
	}
	json.Unmarshal(params[1], &opts)
	if opts.Encoding != "jsonParsed" || opts.MaxVer == nil || *opts.MaxVer != 0 || opts.Commit != "confirmed" {
		l.t.Errorf("getTransaction options = %s", params[1])
	}
	l.mu.Lock()
	l.fetched[sig]++
	status, code := l.txHTTP[sig], l.txRPCErr[sig]
	res, ok := l.txs[sig]
	l.mu.Unlock()
	switch {
	case status != 0:
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":"unavailable"}`)
	case code != 0:
		rpcErrorReply(w, code, "Transaction not available")
	case !ok:
		rpcReply(w, nil)
	default:
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
	}
}

// --- transaction builder -----------------------------------------------------------------

const baseLamports = 10_000_000_000

type tokBal struct {
	acct, owner, mint, program string
	pre, post                  string // base units; "" = the account did not exist before / after
	dec                        int
}

// txSpec describes a transaction in terms of what moved; result renders the jsonParsed
// getTransaction result a node would return.
type txSpec struct {
	sig   string
	at    int64
	fee   uint64
	err   any
	keys  []string         // keys[0] pays the fee
	delta map[string]int64 // lamports moved per key, fee excluded
	toks  []tokBal
	ixs   []map[string]any
}

func (t txSpec) finalKeys() []string {
	keys := slices.Clone(t.keys)
	for _, b := range t.toks {
		if !slices.Contains(keys, b.acct) {
			keys = append(keys, b.acct)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(t.delta)) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

func (t txSpec) result(slot int64) json.RawMessage {
	keys := t.finalKeys()
	pre := make([]uint64, len(keys))
	post := make([]uint64, len(keys))
	akeys := make([]any, len(keys))
	for i, k := range keys {
		d := t.delta[k]
		if i == 0 {
			d -= int64(t.fee)
		}
		pre[i] = baseLamports
		post[i] = uint64(int64(baseLamports) + d)
		akeys[i] = map[string]any{"pubkey": k, "signer": i == 0, "source": "transaction", "writable": true}
	}
	tokJSON := func(raw func(tokBal) string) []any {
		out := []any{}
		for _, b := range t.toks {
			amt := raw(b)
			if amt == "" {
				continue
			}
			n, _ := new(big.Int).SetString(amt, 10)
			ui := formatAmount(n, b.dec)
			out = append(out, map[string]any{
				"accountIndex": slices.Index(keys, b.acct), "mint": b.mint, "owner": b.owner, "programId": b.program,
				"uiTokenAmount": map[string]any{"amount": amt, "decimals": b.dec, "uiAmount": 0.0, "uiAmountString": ui},
			})
		}
		return out
	}
	ixs := t.ixs
	if ixs == nil {
		ixs = []map[string]any{}
	}
	res := map[string]any{
		"slot": slot, "blockTime": t.at, "version": "legacy",
		"transaction": map[string]any{
			"signatures": []string{t.sig},
			"message":    map[string]any{"accountKeys": akeys, "instructions": ixs, "recentBlockhash": "x"},
		},
		"meta": map[string]any{
			"err": t.err, "status": map[string]any{"Ok": nil}, "fee": t.fee,
			"preBalances": pre, "postBalances": post,
			"preTokenBalances":  tokJSON(func(b tokBal) string { return b.pre }),
			"postTokenBalances": tokJSON(func(b tokBal) string { return b.post }),
			"innerInstructions": []any{}, "logMessages": []string{}, "rewards": []any{},
		},
	}
	raw, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}
	return raw
}

func ixSOL(src, dst string, lamports uint64) map[string]any {
	return map[string]any{
		"program": "system", "programId": systemProgramID,
		"parsed": map[string]any{"type": "transfer", "info": map[string]any{"source": src, "destination": dst, "lamports": lamports}},
	}
}

func ixSPL(program, src, dst, auth, mint, amount string, dec int) map[string]any {
	return map[string]any{
		"program": "spl-token", "programId": program,
		"parsed": map[string]any{"type": "transferChecked", "info": map[string]any{
			"source": src, "destination": dst, "authority": auth, "mint": mint,
			"tokenAmount": map[string]any{"amount": amount, "decimals": dec},
		}},
	}
}

func ixRaw(programID string) map[string]any {
	return map[string]any{"programId": programID, "accounts": []string{}, "data": "3Bxs4h24hBtQy9rw"}
}

func ixStake(typ string) map[string]any {
	return map[string]any{"program": "stake", "programId": StakeProgramID, "parsed": map[string]any{"type": typ, "info": map[string]any{}}}
}

// --- shared helpers ----------------------------------------------------------------------

func wallet(name string) string { return testMint("wallet/" + name) }

const (
	pyusdMint = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
	jupProg   = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
)

func (l *fakeLedger) addSOLSend(from, to string, lamports uint64, at int64) string {
	return l.add(txSpec{
		at: at, fee: 5000, keys: []string{from, to, systemProgramID},
		delta: map[string]int64{from: -int64(lamports), to: int64(lamports)},
		ixs:   []map[string]any{ixSOL(from, to, lamports)},
	})
}

// addUSDCReceive records an incoming USDC transfer into ownerATA. The owner wallet is not part
// of the transaction, exactly like on chain.
func (l *fakeLedger) addUSDCReceive(ownerATA, owner, sender string, amount string, at int64) string {
	senderATA := testMint("ata/" + sender)
	return l.add(txSpec{
		at: at, fee: 5000, keys: []string{sender, senderATA, ownerATA, usdcMint, TokenProgramID},
		toks: []tokBal{
			{acct: senderATA, owner: sender, mint: usdcMint, program: TokenProgramID, pre: "900000000", post: "800000000", dec: 6},
			{acct: ownerATA, owner: owner, mint: usdcMint, program: TokenProgramID, pre: "1000000", post: sumAmounts("1000000", amount), dec: 6},
		},
		ixs: []map[string]any{ixSPL(TokenProgramID, senderATA, ownerATA, sender, usdcMint, amount, 6)},
	})
}

func sumAmounts(a, b string) string {
	x, _ := new(big.Int).SetString(a, 10)
	y, _ := new(big.Int).SetString(b, 10)
	return x.Add(x, y).String()
}

func mustOne(t *testing.T, txs []core.Tx) core.Tx {
	t.Helper()
	if len(txs) != 1 {
		t.Fatalf("got %d txs, want 1: %+v", len(txs), txs)
	}
	return txs[0]
}

func legMap(tx core.Tx) map[string]float64 {
	m := map[string]float64{}
	for _, l := range tx.Legs {
		m[l.AssetKey] = l.Amount
	}
	return m
}

func wantLegs(t *testing.T, tx core.Tx, want map[string]float64) {
	t.Helper()
	got := legMap(tx)
	if len(got) != len(tx.Legs) {
		t.Errorf("duplicate asset keys in legs: %+v", tx.Legs)
	}
	if len(got) != len(want) {
		t.Errorf("legs = %+v, want %v", tx.Legs, want)
		return
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("leg %s = %v (present %v), want %v; legs %+v", k, g, ok, v, tx.Legs)
		}
	}
}

func wantFee(t *testing.T, tx core.Tx, want float64) {
	t.Helper()
	if want == 0 {
		if tx.Fee != nil {
			t.Errorf("Fee = %+v, want none", tx.Fee)
		}
		return
	}
	if tx.Fee == nil || tx.Fee.Amount != want || tx.Fee.AssetKey != "SOL" || tx.Fee.Symbol != "SOL" {
		t.Errorf("Fee = %+v, want %v SOL", tx.Fee, want)
	}
}

func (l *fakeLedger) run(t *testing.T, s *Source, state []byte, since time.Time, budget int) (core.ActivityResult, int) {
	t.Helper()
	before := l.total()
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Since: since, Budget: budget})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	used := l.total() - before
	limit := budget
	if limit <= 0 {
		limit = 40
	}
	if used > limit {
		t.Fatalf("used %d RPC requests with a budget of %d", used, limit)
	}
	return res, used
}

// drain calls Activity until backfill is done, returning everything it emitted.
func (l *fakeLedger) drain(t *testing.T, s *Source, state []byte, since time.Time, budget int) (txs []core.Tx, final []byte, calls int) {
	t.Helper()
	for calls < 500 {
		res, _ := l.run(t, s, state, since, budget)
		calls++
		txs = append(txs, res.Txs...)
		state = res.State
		if res.BackfillDone {
			return txs, state, calls
		}
	}
	t.Fatalf("backfill not done after %d calls", calls)
	return nil, nil, calls
}

func assertNoDupes(t *testing.T, txs []core.Tx) {
	t.Helper()
	seen := map[string]bool{}
	for _, tx := range txs {
		if seen[tx.ID] {
			t.Errorf("duplicate tx %s", tx.ID)
		}
		seen[tx.ID] = true
	}
}

func idSet(txs []core.Tx) map[string]bool {
	m := map[string]bool{}
	for _, tx := range txs {
		m[tx.ExternalID] = true
	}
	return m
}

func setPageLimit(t *testing.T, n int) {
	t.Helper()
	old := sigPageLimit
	sigPageLimit = n
	t.Cleanup(func() { sigPageLimit = old })
}

func setClock(t *testing.T, now time.Time) {
	t.Helper()
	activityNow = func() time.Time { return now }
	t.Cleanup(func() { activityNow = time.Now })
}

func decodeState(t *testing.T, raw []byte) activityState {
	t.Helper()
	var st activityState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state is not JSON: %v", err)
	}
	return st
}

// --- classification ----------------------------------------------------------------------

func TestActivitySOLSendSplitsFee(t *testing.T) {
	l, s := newLedger(t, testOwner)
	bob := wallet("bob")
	sig := l.addSOLSend(testOwner, bob, 1_500_000_000, 1_700_000_000)

	res, _ := l.run(t, s, nil, time.Time{}, 0)
	tx := mustOne(t, res.Txs)
	if tx.Kind != core.TxSend || tx.Status != core.TxConfirmed {
		t.Errorf("kind/status = %s/%s", tx.Kind, tx.Status)
	}
	wantLegs(t, tx, map[string]float64{"SOL": -1.5}) // the fee is not inside the SOL leg
	wantFee(t, tx, -0.000005)
	if tx.Counterparty != bob {
		t.Errorf("Counterparty = %q, want %q", tx.Counterparty, bob)
	}
	if tx.ID != "phantom:"+sig || tx.SourceID != "phantom" || tx.ExternalID != sig || tx.TxHash != sig {
		t.Errorf("ids = %q %q %q %q", tx.ID, tx.SourceID, tx.ExternalID, tx.TxHash)
	}
	if tx.ID != core.TxID(s.ID(), sig) {
		t.Errorf("ID is not core.TxID")
	}
	if tx.ExplorerURL != "https://solscan.io/tx/"+sig {
		t.Errorf("ExplorerURL = %q", tx.ExplorerURL)
	}
	if !tx.Time.Equal(time.Unix(1_700_000_000, 0)) {
		t.Errorf("Time = %v", tx.Time)
	}
	if tx.Legs == nil {
		t.Errorf("Legs must be non-nil")
	}
	if !res.BackfillDone {
		t.Errorf("BackfillDone = false")
	}
	if len(res.State) == 0 {
		t.Errorf("no state")
	}
}

func TestActivitySOLReceiveNotFeePayer(t *testing.T) {
	l, s := newLedger(t, testOwner)
	alice := wallet("alice")
	l.addSOLSend(alice, testOwner, 2_000_000_000, 1_700_000_000) // alice pays the fee

	res, _ := l.run(t, s, nil, time.Time{}, 0)
	tx := mustOne(t, res.Txs)
	if tx.Kind != core.TxReceive {
		t.Errorf("kind = %s", tx.Kind)
	}
	wantLegs(t, tx, map[string]float64{"SOL": 2})
	wantFee(t, tx, 0) // not the fee payer: no fee leg
	if tx.Counterparty != alice {
		t.Errorf("Counterparty = %q, want %q", tx.Counterparty, alice)
	}
}

func TestActivitySPLReceiveViaTokenAccount(t *testing.T) {
	l, s := newLedger(t, testOwner)
	ownerATA := testMint("ata/owner-usdc")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ownerATA, mint: usdcMint, amount: "61000000", decimals: 6})
	alice := wallet("alice")

	// Incoming transfer: only the token account is in the transaction, not the owner wallet.
	recv := l.addUSDCReceive(ownerATA, testOwner, alice, "60000000", 1_700_000_000)
	// The owner sends USDC out: the transaction names both the owner and its token account, so
	// it shows up on both signature lists.
	bobATA := testMint("ata/bob")
	both := l.add(txSpec{
		at: 1_700_000_100, fee: 5000, keys: []string{testOwner, ownerATA, bobATA, usdcMint, TokenProgramID},
		toks: []tokBal{
			{acct: ownerATA, owner: testOwner, mint: usdcMint, program: TokenProgramID, pre: "61000000", post: "11000000", dec: 6},
			{acct: bobATA, owner: wallet("bob"), mint: usdcMint, program: TokenProgramID, pre: "0", post: "50000000", dec: 6},
		},
		ixs: []map[string]any{ixSPL(TokenProgramID, ownerATA, bobATA, testOwner, usdcMint, "50000000", 6)},
	})

	res, _ := l.run(t, s, nil, time.Time{}, 0)
	if len(res.Txs) != 2 {
		t.Fatalf("got %d txs, want 2: %+v", len(res.Txs), res.Txs)
	}
	assertNoDupes(t, res.Txs)
	if n := l.fetchCount(both); n != 1 {
		t.Errorf("tx on both lists was fetched %d times, want 1", n)
	}
	if n := l.fetchCount(recv); n != 1 {
		t.Errorf("incoming tx fetched %d times, want 1", n)
	}
	byID := map[string]core.Tx{}
	for _, tx := range res.Txs {
		byID[tx.ExternalID] = tx
	}

	in := byID[recv]
	if in.Kind != core.TxReceive {
		t.Errorf("incoming kind = %s", in.Kind)
	}
	wantLegs(t, in, map[string]float64{"USDC": 60})
	wantFee(t, in, 0)
	if in.Legs[0].Mint != usdcMint || in.Legs[0].Symbol != "USDC" {
		t.Errorf("leg identity = %+v", in.Legs[0])
	}
	if in.Counterparty != alice {
		t.Errorf("incoming Counterparty = %q, want %q", in.Counterparty, alice)
	}

	out := byID[both]
	if out.Kind != core.TxSend {
		t.Errorf("outgoing kind = %s", out.Kind)
	}
	wantLegs(t, out, map[string]float64{"USDC": -50})
	wantFee(t, out, -0.000005)
	if out.Counterparty != wallet("bob") {
		t.Errorf("outgoing Counterparty = %q (token accounts must map to their owner wallet)", out.Counterparty)
	}

	// A second call with the returned state finds nothing new and fetches nothing.
	fetchedBefore := l.callCount("getTransaction")
	res2, _ := l.run(t, s, res.State, time.Time{}, 0)
	if len(res2.Txs) != 0 || !res2.BackfillDone {
		t.Errorf("second call: %d txs, done=%v", len(res2.Txs), res2.BackfillDone)
	}
	if n := l.callCount("getTransaction"); n != fetchedBefore {
		t.Errorf("second call fetched %d transactions", n-fetchedBefore)
	}
}

func TestActivitySwapSOLToUSDC(t *testing.T) {
	l, s := newLedger(t, testOwner)
	ownerATA, pool := testMint("ata/owner-usdc"), testMint("pool")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ownerATA, mint: usdcMint, amount: "150250000", decimals: 6})
	l.add(txSpec{
		at: 1_700_000_000, fee: 5000, keys: []string{testOwner, ownerATA, pool, usdcMint, TokenProgramID, jupProg},
		delta: map[string]int64{testOwner: -1_000_000_000, pool: 1_000_000_000},
		toks: []tokBal{
			// "pre" missing: the token account was created by this swap.
			{acct: ownerATA, owner: testOwner, mint: usdcMint, program: TokenProgramID, pre: "", post: "150250000", dec: 6},
		},
		ixs: []map[string]any{ixRaw(jupProg)},
	})

	res, _ := l.run(t, s, nil, time.Time{}, 0)
	tx := mustOne(t, res.Txs)
	if tx.Kind != core.TxSwap {
		t.Errorf("kind = %s", tx.Kind)
	}
	if len(tx.Legs) != 2 || tx.Legs[0].Amount >= 0 || tx.Legs[1].Amount <= 0 {
		t.Fatalf("want outgoing leg then incoming leg, got %+v", tx.Legs)
	}
	wantLegs(t, tx, map[string]float64{"SOL": -1, "USDC": 150.25})
	wantFee(t, tx, -0.000005)
	if tx.Counterparty != "" {
		t.Errorf("a swap has no counterparty, got %q", tx.Counterparty)
	}
}

func TestActivityFailedTransaction(t *testing.T) {
	failure := map[string]any{"InstructionError": []any{2, map[string]any{"Custom": 6001}}}

	t.Run("only the fee was charged", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		l.add(txSpec{at: 1_700_000_000, fee: 6360, err: failure, keys: []string{testOwner, jupProg}, ixs: []map[string]any{ixRaw(jupProg)}})
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		tx := mustOne(t, res.Txs)
		if tx.Status != core.TxFailed || tx.Kind != core.TxFee {
			t.Errorf("status/kind = %s/%s", tx.Status, tx.Kind)
		}
		if len(tx.Legs) != 0 || tx.Legs == nil {
			t.Errorf("Legs = %#v, want empty non-nil", tx.Legs)
		}
		wantFee(t, tx, -0.00000636)
	})

	t.Run("legs are kept for display", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		bob := wallet("bob")
		l.add(txSpec{at: 1_700_000_000, fee: 5000, err: failure, keys: []string{testOwner, bob, systemProgramID},
			delta: map[string]int64{testOwner: -1_000_000_000, bob: 1_000_000_000}, ixs: []map[string]any{ixSOL(testOwner, bob, 1_000_000_000)}})
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		tx := mustOne(t, res.Txs)
		if tx.Status != core.TxFailed || tx.Kind != core.TxSend {
			t.Errorf("status/kind = %s/%s", tx.Status, tx.Kind)
		}
		wantLegs(t, tx, map[string]float64{"SOL": -1})
	})

	t.Run("someone else's failed tx touching my account is not an entry", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		ata := testMint("ata/owner-usdc")
		l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ata, mint: usdcMint, amount: "1", decimals: 6})
		l.add(txSpec{at: 1_700_000_000, fee: 5000, err: failure, keys: []string{wallet("eve"), ata}, ixs: []map[string]any{ixRaw(jupProg)}})
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		if len(res.Txs) != 0 || !res.BackfillDone {
			t.Errorf("got %d txs done=%v", len(res.Txs), res.BackfillDone)
		}
	})
}

func TestActivityToken2022Receive(t *testing.T) {
	l, s := newLedger(t, testOwner)
	ata := testMint("ata/owner-pyusd")
	l.chain.addTokenAccount(Token2022ProgramID, fakeAcct{pubkey: ata, mint: pyusdMint, amount: "25500000", decimals: 6})
	alice := wallet("alice")
	aliceATA := testMint("ata/alice-pyusd")
	l.add(txSpec{
		at: 1_700_000_000, fee: 5000, keys: []string{alice, aliceATA, ata, pyusdMint, Token2022ProgramID},
		toks: []tokBal{
			{acct: aliceATA, owner: alice, mint: pyusdMint, program: Token2022ProgramID, pre: "100000000", post: "74500000", dec: 6},
			{acct: ata, owner: testOwner, mint: pyusdMint, program: Token2022ProgramID, pre: "0", post: "25500000", dec: 6},
		},
		ixs: []map[string]any{ixSPL(Token2022ProgramID, aliceATA, ata, alice, pyusdMint, "25500000", 6)},
	})

	res, _ := l.run(t, s, nil, time.Time{}, 0)
	tx := mustOne(t, res.Txs)
	if tx.Kind != core.TxReceive {
		t.Errorf("kind = %s", tx.Kind)
	}
	wantLegs(t, tx, map[string]float64{"PYUSD": 25.5})
	if tx.Legs[0].Mint != pyusdMint {
		t.Errorf("Mint = %q", tx.Legs[0].Mint)
	}
	if tx.Counterparty != alice {
		t.Errorf("Counterparty = %q", tx.Counterparty)
	}
	if n := l.chain.callCount("getTokenAccountsByOwner"); n != 2 {
		t.Errorf("getTokenAccountsByOwner called %d times, want once per token program", n)
	}
}

func TestActivityStake(t *testing.T) {
	stakeAcct := wallet("stake-account")

	t.Run("stake SOL", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		l.add(txSpec{
			at: 1_700_000_000, fee: 85000, keys: []string{testOwner, stakeAcct, systemProgramID, StakeProgramID},
			delta: map[string]int64{testOwner: -3_000_000_000, stakeAcct: 3_000_000_000},
			ixs: []map[string]any{
				{"program": "system", "programId": systemProgramID, "parsed": map[string]any{"type": "createAccount", "info": map[string]any{"source": testOwner, "newAccount": stakeAcct}}},
				ixStake("initialize"), ixStake("delegate"),
			},
		})
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		tx := mustOne(t, res.Txs)
		if tx.Kind != core.TxStake {
			t.Errorf("kind = %s", tx.Kind)
		}
		wantLegs(t, tx, map[string]float64{"SOL": -3})
		wantFee(t, tx, -0.000085)
	})

	t.Run("deactivate moves nothing but the fee", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		l.add(txSpec{at: 1_700_000_000, fee: 5000, keys: []string{testOwner, stakeAcct, StakeProgramID}, ixs: []map[string]any{ixStake("deactivate")}})
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		tx := mustOne(t, res.Txs)
		if tx.Kind != core.TxStake || len(tx.Legs) != 0 {
			t.Errorf("kind=%s legs=%+v", tx.Kind, tx.Legs)
		}
		wantFee(t, tx, -0.000005)
	})

	t.Run("stake program reached through a CPI", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		pool := wallet("pool")
		spec := txSpec{at: 1_700_000_000, fee: 5000, keys: []string{testOwner, pool, systemProgramID},
			delta: map[string]int64{testOwner: -1_000_000_000, pool: 1_000_000_000}}
		var res map[string]any
		json.Unmarshal(spec.result(1), &res)
		res["meta"].(map[string]any)["innerInstructions"] = []any{map[string]any{"index": 0, "instructions": []any{ixStake("delegate")}}}
		raw, _ := json.Marshal(res)
		spec.sig = "sig-cpi"
		l.addRaw(spec.sig, spec.at, nil, spec.finalKeys(), raw)
		out, _ := l.run(t, s, nil, time.Time{}, 0)
		if tx := mustOne(t, out.Txs); tx.Kind != core.TxStake {
			t.Errorf("kind = %s", tx.Kind)
		}
	})
}

func TestActivityLegIdentity(t *testing.T) {
	l, s := newLedger(t, testOwner)
	named, bare := testMint("named-token"), testMint("bare-token")
	s.res.cache.Put(named, TokenMeta{Mint: named, Symbol: "ZED", Name: "Zed", Decimals: 9, Source: sourceJupiter})
	ataN, ataB := testMint("ata/named"), testMint("ata/bare")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ataN, mint: named, amount: "1", decimals: 9})
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ataB, mint: bare, amount: "1", decimals: 0})

	l.add(txSpec{at: 1_700_000_000, fee: 5000, keys: []string{wallet("a"), ataN, ataB}, toks: []tokBal{
		{acct: ataN, owner: testOwner, mint: named, program: TokenProgramID, pre: "0", post: "1500000000", dec: 9},
		{acct: ataB, owner: testOwner, mint: bare, program: TokenProgramID, pre: "0", post: "42", dec: 0},
	}})
	res, _ := l.run(t, s, nil, time.Time{}, 0)
	tx := mustOne(t, res.Txs)
	if tx.Kind != core.TxReceive {
		t.Errorf("kind = %s", tx.Kind)
	}
	for _, leg := range tx.Legs {
		switch leg.Mint {
		case named:
			if leg.AssetKey != "spl:"+named || leg.Symbol != "ZED" || leg.Amount != 1.5 {
				t.Errorf("named leg = %+v", leg)
			}
		case bare:
			if leg.AssetKey != "spl:"+bare || leg.Symbol != shortKey(bare) || leg.Amount != 42 {
				t.Errorf("bare leg = %+v", leg)
			}
		default:
			t.Errorf("unexpected leg %+v", leg)
		}
	}
}

func TestActivityWrappedSOLIsSOL(t *testing.T) {
	t.Run("unwrapping returns rent", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		wsolATA := testMint("ata/wsol")
		l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: wsolATA, mint: WrappedSOLMint, amount: "2000000000", decimals: 9})
		// Closing a 2 SOL wSOL account: 2 SOL + 0.00203928 rent land in the wallet, the token
		// balance drops by 2. Seen as holdings (SOL + wSOL merged) only the rent is new.
		l.add(txSpec{at: 1_700_000_000, fee: 5000, keys: []string{testOwner, wsolATA, TokenProgramID},
			delta: map[string]int64{testOwner: 2_002_039_280, wsolATA: -2_002_039_280},
			toks:  []tokBal{{acct: wsolATA, owner: testOwner, mint: WrappedSOLMint, program: TokenProgramID, pre: "2000000000", post: "", dec: 9}}})
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		tx := mustOne(t, res.Txs)
		wantLegs(t, tx, map[string]float64{"SOL": 0.00203928})
		if tx.Kind != core.TxReceive {
			t.Errorf("kind = %s", tx.Kind)
		}
	})

	t.Run("wrapping is not a movement", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		wsolATA := testMint("ata/wsol")
		l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: wsolATA, mint: WrappedSOLMint, amount: "1000000000", decimals: 9})
		l.add(txSpec{at: 1_700_000_000, fee: 5000, keys: []string{testOwner, wsolATA, systemProgramID, TokenProgramID},
			delta: map[string]int64{testOwner: -1_000_000_000, wsolATA: 1_000_000_000},
			toks:  []tokBal{{acct: wsolATA, owner: testOwner, mint: WrappedSOLMint, program: TokenProgramID, pre: "0", post: "1000000000", dec: 9}}})
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		tx := mustOne(t, res.Txs)
		if tx.Kind != core.TxFee || len(tx.Legs) != 0 {
			t.Errorf("kind=%s legs=%+v", tx.Kind, tx.Legs)
		}
		wantFee(t, tx, -0.000005)
	})
}

func TestActivityNoiseIsDropped(t *testing.T) {
	l, s := newLedger(t, testOwner)
	ata := testMint("ata/owner-usdc")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ata, mint: usdcMint, amount: "5000000", decimals: 6})
	// Someone else's transaction reads my token account; my balance is unchanged.
	l.add(txSpec{at: 1_700_000_000, fee: 5000, keys: []string{wallet("eve"), ata, usdcMint},
		toks: []tokBal{{acct: ata, owner: testOwner, mint: usdcMint, program: TokenProgramID, pre: "5000000", post: "5000000", dec: 6}}})
	res, _ := l.run(t, s, nil, time.Time{}, 0)
	if len(res.Txs) != 0 {
		t.Errorf("got %+v, want no entries", res.Txs)
	}
	if !res.BackfillDone {
		t.Errorf("noise must still advance the cursors")
	}
}

func TestActivityCounterpartyAmbiguous(t *testing.T) {
	l, s := newLedger(t, testOwner)
	a, b := wallet("a"), wallet("b")
	l.add(txSpec{at: 1_700_000_000, fee: 5000, keys: []string{testOwner, a, b, systemProgramID},
		delta: map[string]int64{testOwner: -3_000_000_000, a: 1_000_000_000, b: 2_000_000_000},
		ixs:   []map[string]any{ixSOL(testOwner, a, 1_000_000_000), ixSOL(testOwner, b, 2_000_000_000)}})
	res, _ := l.run(t, s, nil, time.Time{}, 0)
	tx := mustOne(t, res.Txs)
	if tx.Kind != core.TxSend || tx.Counterparty != "" {
		t.Errorf("kind=%s counterparty=%q", tx.Kind, tx.Counterparty)
	}
}

func TestScaleDecimal(t *testing.T) {
	for _, c := range []struct {
		in   string
		dec  int
		want string
		ok   bool
	}{
		{"12.5", 6, "12500000", true},
		{"0.000001", 6, "1", true},
		{"7", 0, "7", true},
		{"-0.5", 2, "-50", true},
		{"1.234", 2, "", false},
		{"", 6, "", false},
		{"1e3", 6, "", false},
		{"1.2.3", 6, "", false},
		{"+5", 0, "", false},
	} {
		got, ok := scaleDecimal(c.in, c.dec)
		if ok != c.ok || (ok && got.String() != c.want) {
			t.Errorf("scaleDecimal(%q, %d) = %v, %v; want %q, %v", c.in, c.dec, got, ok, c.want, c.ok)
		}
	}
}

func TestTokenBalanceFallsBackToUIAmountString(t *testing.T) {
	var b tokenBalance
	if err := json.Unmarshal([]byte(`{"accountIndex":1,"mint":"m","owner":"o","uiTokenAmount":{"decimals":6,"uiAmountString":"453584854.43453"}}`), &b); err != nil {
		t.Fatal(err)
	}
	n, ok := b.raw()
	if !ok || n.String() != "453584854434530" {
		t.Errorf("raw = %v, %v", n, ok)
	}
}

func TestAccountKeyDecodesBothShapes(t *testing.T) {
	var keys []acctKey
	if err := json.Unmarshal([]byte(`[{"pubkey":"A","signer":true},"B"]`), &keys); err != nil || len(keys) != 2 || keys[0].Pubkey != "A" || keys[1].Pubkey != "B" {
		t.Errorf("keys = %+v, err = %v", keys, err)
	}
}

// --- captured mainnet transactions -------------------------------------------------------

// loadCaptured registers a testdata/*.json getTransaction response in the ledger, listed under
// every account key like on a real node, and returns its signature.
func loadCaptured(t *testing.T, l *fakeLedger, file string) (sig string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	var tx struct {
		BlockTime   int64 `json:"blockTime"`
		Transaction struct {
			Signatures []string `json:"signatures"`
			Message    struct {
				AccountKeys []acctKey `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
		Meta struct {
			Err any `json:"err"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Result, &tx); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, k := range tx.Transaction.Message.AccountKeys {
		keys = append(keys, k.Pubkey)
	}
	sig = tx.Transaction.Signatures[0]
	l.addRaw(sig, tx.BlockTime, tx.Meta.Err, keys, env.Result)
	return sig
}

// TestActivityCapturedMainnet runs transactions captured from mainnet-beta (getTransaction,
// jsonParsed, maxSupportedTransactionVersion 0, confirmed) through the full Activity path.
func TestActivityCapturedMainnet(t *testing.T) {
	const (
		cexHot   = "5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9"
		swapper  = "DTcpygzYPrD8wWNniCYdkRUY7bRPjLtrceSqamoeHQTn"
		arena    = "ARENA551cbiMFSFzXv1V1RDxufZEw9Q2Kbd5uD7FrFeR"
		usdtMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	)
	for _, c := range []struct {
		name, file, owner string
		tokenAccts        []fakeAcct
		program           string

		kind         core.TxKind
		status       core.TxStatus
		legs         map[string]float64
		fee          float64
		counterparty string
		at           int64
	}{
		{
			name: "SOL send, one destination", file: "activity_sol_send.json", owner: cexHot,
			kind: core.TxSend, status: core.TxConfirmed, legs: map[string]float64{"SOL": -0.014037}, fee: -0.000007,
			counterparty: "AbJP624QzrpgSpCBuYeg3fGiFxYVGymmdhBfP1jtfbAt", at: 1792178487,
		},
		{
			name: "SOL send, three destinations", file: "activity_sol_multisend.json", owner: cexHot,
			kind: core.TxSend, status: core.TxConfirmed, legs: map[string]float64{"SOL": -1.72101}, fee: -0.000013,
			counterparty: "", at: 1792178520,
		},
		{
			name: "SOL receive, not the fee payer", file: "activity_sol_multisend.json", owner: "D4NLYBCknwrGrq23TMt9okGnVL9Xp9DzsNrC39dw6E3T",
			kind: core.TxReceive, status: core.TxConfirmed, legs: map[string]float64{"SOL": 0.499}, fee: 0,
			counterparty: cexHot, at: 1792178520,
		},
		{
			name: "USDT send, token account to wallet owner", file: "activity_spl_send.json", owner: cexHot,
			kind: core.TxSend, status: core.TxConfirmed, legs: map[string]float64{"USDT": -50.7}, fee: -0.000015,
			counterparty: "21phnVwuqWCctA5d8QQhJoqi4LeH5tPL6V1oEktb7EVE", at: 1792178512,
		},
		{
			name: "Jupiter swap SOL to an unlisted token", file: "activity_swap.json", owner: swapper,
			kind: core.TxSwap, status: core.TxConfirmed, legs: map[string]float64{"SOL": -0.011644667, "spl:" + arena: 325.285695}, fee: -0.000005,
			counterparty: "", at: 1792178546,
		},
		{
			name: "failed Jupiter swap", file: "activity_failed.json", owner: "HHHJTk2pdPSCoAcFRxkapVdDRprR7Wo1tpXyB8LcVb4s",
			kind: core.TxFee, status: core.TxFailed, legs: map[string]float64{}, fee: -0.00000636,
			counterparty: "", at: 1792178546,
		},
		{
			name: "Token-2022 PYUSD receive via the token account", file: "activity_token2022_receive.json",
			owner:      "5nGJ1L9svaLSDS7NJ483dFrsK2ZvMonxi2ZJgcvU9sKo",
			tokenAccts: []fakeAcct{{pubkey: "FAQXQea2UJuL4AvjUVN1CategC4LTzNbhtv3fiGoz4yK", mint: pyusdMint, amount: "19977748", decimals: 6}}, program: Token2022ProgramID,
			kind: core.TxReceive, status: core.TxConfirmed, legs: map[string]float64{"PYUSD": 19.977748}, fee: 0,
			counterparty: "fr6yQkDmWy6R6pecbUsxXaw6EvRJznZ2HsK5frQgud8", at: 1792178542,
		},
		{
			// The sender also paid 0.0016002 SOL of rent to create the receiver's token account.
			name: "Token-2022 PYUSD send by the fee payer", file: "activity_token2022_receive.json", owner: "fr6yQkDmWy6R6pecbUsxXaw6EvRJznZ2HsK5frQgud8",
			kind: core.TxSend, status: core.TxConfirmed, legs: map[string]float64{"SOL": -0.0016002, "PYUSD": -19.977748}, fee: -0.000005017,
			counterparty: "5nGJ1L9svaLSDS7NJ483dFrsK2ZvMonxi2ZJgcvU9sKo", at: 1792178542,
		},
		{
			name: "stake account created and delegated", file: "activity_stake.json", owner: "DPbVCc8rfGbz5c6J3RaCy1HN3RW13tTDc7F3Tx32NNZn",
			kind: core.TxStake, status: core.TxConfirmed, legs: map[string]float64{"SOL": -9.42376624}, fee: -0.000085,
			counterparty: "", at: 1792178552,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, s := newLedger(t, c.owner)
			sig := loadCaptured(t, l, c.file)
			for _, a := range c.tokenAccts {
				l.chain.addTokenAccount(c.program, a)
			}
			res, _ := l.run(t, s, nil, time.Time{}, 0)
			tx := mustOne(t, res.Txs)
			if tx.Kind != c.kind || tx.Status != c.status {
				t.Errorf("kind/status = %s/%s, want %s/%s", tx.Kind, tx.Status, c.kind, c.status)
			}
			wantLegs(t, tx, c.legs)
			wantFee(t, tx, c.fee)
			if tx.Counterparty != c.counterparty {
				t.Errorf("Counterparty = %q, want %q", tx.Counterparty, c.counterparty)
			}
			if tx.ExternalID != sig || tx.TxHash != sig || tx.Time.Unix() != c.at {
				t.Errorf("sig/time = %s %s %d", tx.ExternalID, tx.TxHash, tx.Time.Unix())
			}
			if !res.BackfillDone {
				t.Errorf("BackfillDone = false")
			}
		})
	}
}

func TestCapturedSignatureListDecodes(t *testing.T) {
	raw, err := os.ReadFile("testdata/activity_signatures.json")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result []sigEntry `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Result) < 2 {
		t.Fatalf("decode: %v (%d entries)", err, len(env.Result))
	}
	failed, ok := 0, 0
	for _, e := range env.Result {
		if e.Signature == "" || e.BlockTime == nil || e.Slot == 0 {
			t.Errorf("incomplete entry %+v", e)
		}
		if isNullJSON(e.Err) {
			ok++
		} else {
			failed++
		}
	}
	if failed == 0 || ok == 0 {
		t.Errorf("fixture should contain both failed (%d) and successful (%d) signatures", failed, ok)
	}
}

// --- cursors, budget, resume -------------------------------------------------------------

func TestActivityBudgetExhaustionResumes(t *testing.T) {
	setPageLimit(t, 5)
	l, s := newLedger(t, testOwner)
	ata := testMint("ata/owner-usdc")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ata, mint: usdcMint, amount: "100000000", decimals: 6})
	bob := wallet("bob")

	want := map[string]bool{}
	at := int64(1_700_000_000)
	for i := 0; i < 14; i++ { // owner-only history, three pages
		at += 10
		want[l.addSOLSend(testOwner, bob, 1000+uint64(i), at)] = true
	}
	for i := 0; i < 7; i++ { // incoming USDC, only on the token account's list
		at += 10
		want[l.addUSDCReceive(ata, testOwner, wallet(fmt.Sprint("sender", i)), "1000000", at)] = true
	}
	// Transactions on both lists.
	for i := 0; i < 4; i++ {
		at += 10
		bobATA := testMint("ata/bob")
		want[l.add(txSpec{at: at, fee: 5000, keys: []string{testOwner, ata, bobATA, usdcMint, TokenProgramID}, toks: []tokBal{
			{acct: ata, owner: testOwner, mint: usdcMint, program: TokenProgramID, pre: "10000000", post: "9000000", dec: 6},
			{acct: bobATA, owner: bob, mint: usdcMint, program: TokenProgramID, pre: "0", post: "1000000", dec: 6},
		}})] = true
	}

	const budget = 6
	txs, state, calls := l.drain(t, s, nil, time.Time{}, budget)
	assertNoDupes(t, txs)
	got := idSet(txs)
	if len(got) != len(want) {
		t.Errorf("got %d distinct txs, want %d", len(got), len(want))
	}
	for sig := range want {
		if !got[sig] {
			t.Errorf("missing %s", sig)
		}
		if n := l.fetchCount(sig); n != 1 {
			t.Errorf("%s fetched %d times", sig, n)
		}
	}
	if calls < 3 {
		t.Errorf("finished in %d calls; budget %d should not suffice for %d txs", calls, budget, len(want))
	}

	// Newest first within a call.
	first, _ := l.run(t, s, nil, time.Time{}, budget)
	for i := 1; i < len(first.Txs); i++ {
		if first.Txs[i].Time.After(first.Txs[i-1].Time) {
			t.Errorf("result not newest first: %v then %v", first.Txs[i-1].Time, first.Txs[i].Time)
		}
	}

	// Nothing new: a call after completion fetches nothing.
	before := l.callCount("getTransaction")
	res, _ := l.run(t, s, state, time.Time{}, budget)
	if len(res.Txs) != 0 || !res.BackfillDone || l.callCount("getTransaction") != before {
		t.Errorf("idle call: %d txs, done=%v, %d fetches", len(res.Txs), res.BackfillDone, l.callCount("getTransaction")-before)
	}
}

func TestActivityNewSignaturesAfterBackfill(t *testing.T) {
	setPageLimit(t, 4)
	l, s := newLedger(t, testOwner)
	ata := testMint("ata/owner-usdc")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ata, mint: usdcMint, amount: "100000000", decimals: 6})
	bob := wallet("bob")
	at := int64(1_700_000_000)
	for i := 0; i < 6; i++ {
		at += 10
		l.addSOLSend(testOwner, bob, 1000+uint64(i), at)
	}
	_, state, _ := l.drain(t, s, nil, time.Time{}, 40)

	// 13 new signatures on the two accounts arrive; a budget of 5 cannot finish them in one call,
	// so the remainder must be parked as a gap and picked up later without loss.
	want := map[string]bool{}
	for i := 0; i < 9; i++ {
		at += 10
		want[l.addSOLSend(testOwner, bob, 5000+uint64(i), at)] = true
	}
	for i := 0; i < 4; i++ {
		at += 10
		want[l.addUSDCReceive(ata, testOwner, wallet(fmt.Sprint("s", i)), "2000000", at)] = true
	}

	var got []core.Tx
	sawGap := false
	for i := 0; i < 50 && len(idSet(got)) < len(want); i++ {
		res, _ := l.run(t, s, state, time.Time{}, 5)
		got = append(got, res.Txs...)
		state = res.State
		for _, a := range decodeState(t, state).Accts {
			sawGap = sawGap || len(a.Gaps) > 0
		}
	}
	assertNoDupes(t, got)
	if len(got) != len(want) {
		t.Fatalf("got %d new txs, want %d", len(got), len(want))
	}
	for _, tx := range got {
		if !want[tx.ExternalID] {
			t.Errorf("unexpected tx %s (already delivered earlier?)", tx.ExternalID)
		}
	}
	if !sawGap {
		t.Errorf("expected the limited budget to leave a gap in the state")
	}
	// Settled: no gaps left and nothing more to deliver.
	res, _ := l.run(t, s, state, time.Time{}, 40)
	if len(res.Txs) != 0 || !res.BackfillDone {
		t.Errorf("after settling: %d txs, done=%v", len(res.Txs), res.BackfillDone)
	}
	for addr, a := range decodeState(t, res.State).Accts {
		if len(a.Gaps) != 0 {
			t.Errorf("%s still has gaps %+v", addr, a.Gaps)
		}
	}
}

func TestActivitySinceStopsBackfill(t *testing.T) {
	setPageLimit(t, 4)
	l, s := newLedger(t, testOwner)
	bob := wallet("bob")
	base := int64(1_700_000_000)
	var sigs []string
	for i := 0; i < 10; i++ {
		sigs = append(sigs, l.addSOLSend(testOwner, bob, 1000+uint64(i), base+int64(i)*100))
	}

	since := time.Unix(base+450, 0) // keeps tx 5..9
	txs, state, _ := l.drain(t, s, nil, since, 0)
	assertNoDupes(t, txs)
	got := idSet(txs)
	for i, sig := range sigs {
		wantIn := i >= 5
		if got[sig] != wantIn {
			t.Errorf("tx %d delivered=%v, want %v", i, got[sig], wantIn)
		}
		if !wantIn && l.fetchCount(sig) != 0 {
			t.Errorf("tx %d is older than Since but was fetched", i)
		}
	}

	// Going further back resumes where backfill stopped, without repeating anything.
	more, _, _ := l.drain(t, s, state, time.Time{}, 0)
	assertNoDupes(t, append(slices.Clone(txs), more...))
	if len(more) != 5 {
		t.Errorf("reopened backfill delivered %d txs, want the 5 older ones", len(more))
	}
	for _, sig := range sigs {
		if n := l.fetchCount(sig); n != 1 {
			t.Errorf("%s fetched %d times", sig, n)
		}
	}
}

func TestActivityDefaultBudgetAndRequestShape(t *testing.T) {
	l, s := newLedger(t, testOwner)
	bob := wallet("bob")
	at := int64(1_700_000_000)
	for i := 0; i < 150; i++ {
		at += 10
		l.addSOLSend(testOwner, bob, 1000+uint64(i), at)
	}
	res, used := l.run(t, s, nil, time.Time{}, 0)
	if used != 40 {
		t.Errorf("default budget: used %d requests, want exactly 40", used)
	}
	if res.BackfillDone {
		t.Errorf("40 requests cannot cover 150 transactions")
	}
	if n := l.callCount("getTokenAccountsByOwner"); n != 2 {
		t.Errorf("token accounts listed %d times, want 2 (once per program)", n)
	}
	// 2 (token accounts) + 1 (page) + 37 transactions.
	if n := len(res.Txs); n != 37 {
		t.Errorf("delivered %d txs, want 37", n)
	}
	l.mu.Lock()
	first := l.sigReqs[0]
	l.mu.Unlock()
	if first["limit"] != float64(100) {
		t.Errorf("limit = %v, want 100", first["limit"])
	}
	if _, ok := first["before"]; ok {
		t.Errorf("first page must not have a before cursor: %v", first)
	}

	// A tiny explicit budget is honoured.
	res2, used2 := l.run(t, s, res.State, time.Time{}, 5)
	if used2 > 5 {
		t.Errorf("used %d of 5", used2)
	}
	_ = res2
}

func TestActivityPartialListingFailureStillDeliversOwner(t *testing.T) {
	l, s := newLedger(t, testOwner)
	l.chain.failMethods["getTokenAccountsByOwner"] = "method disabled"
	l.addSOLSend(testOwner, wallet("bob"), 1000, 1_700_000_000)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil {
		t.Fatalf("progress was made, error expected to be held back: %v", err)
	}
	if len(res.Txs) != 1 {
		t.Errorf("got %d txs, want the owner's", len(res.Txs))
	}
	if res.BackfillDone {
		t.Errorf("backfill must not be complete while token accounts are unknown")
	}

	// With nothing to do, the listing failure is what the caller must hear about.
	l2, s2 := newLedger(t, testOwner)
	l2.chain.failMethods["getTokenAccountsByOwner"] = "method disabled"
	if _, err := s2.Activity(context.Background(), core.ActivityRequest{}); err == nil || !strings.Contains(err.Error(), "method disabled") {
		t.Errorf("err = %v", err)
	}
}

func TestActivityErrors(t *testing.T) {
	t.Run("no progress returns the error, scrubbed", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		l.addSOLSend(testOwner, wallet("bob"), 1000, 1_700_000_000)
		l.set(func() { l.listHTTP = http.StatusInternalServerError })
		res, err := s.Activity(context.Background(), core.ActivityRequest{})
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), testRPCKey) {
			t.Errorf("error leaks the RPC key: %v", err)
		}
		if len(res.Txs) != 0 || res.BackfillDone {
			t.Errorf("res = %+v", res)
		}
	})

	t.Run("progress then failure ends the call quietly and resumes", func(t *testing.T) {
		setPageLimit(t, 10)
		l, s := newLedger(t, testOwner)
		var sigs []string
		for i := 0; i < 5; i++ {
			sigs = append(sigs, l.addSOLSend(testOwner, wallet("bob"), 1000+uint64(i), 1_700_000_000+int64(i)*10))
		}
		// Newest first: sigs[4], sigs[3], then sigs[2] fails.
		l.set(func() { l.txHTTP[sigs[2]] = http.StatusBadGateway })
		res, err := s.Activity(context.Background(), core.ActivityRequest{})
		if err != nil {
			t.Fatalf("progress was made, got %v", err)
		}
		if len(res.Txs) != 2 || res.BackfillDone {
			t.Fatalf("got %d txs done=%v, want 2 and not done", len(res.Txs), res.BackfillDone)
		}
		// Still failing, no further progress: now the error surfaces.
		if _, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State}); err == nil {
			t.Errorf("repeated failure without progress must be reported")
		}
		// Recovery continues exactly after the last handled transaction.
		l.set(func() { delete(l.txHTTP, sigs[2]) })
		res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
		if err != nil || len(res2.Txs) != 3 || !res2.BackfillDone {
			t.Fatalf("recovery: err=%v txs=%d done=%v", err, len(res2.Txs), res2.BackfillDone)
		}
		all := append(slices.Clone(res.Txs), res2.Txs...)
		assertNoDupes(t, all)
		if len(all) != 5 {
			t.Errorf("total %d", len(all))
		}
	})

	t.Run("cancelled context returns its error with a usable state", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		l.addSOLSend(testOwner, wallet("bob"), 1000, 1_700_000_000)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := s.Activity(ctx, core.ActivityRequest{})
		if err == nil || ctx.Err() == nil {
			t.Fatalf("err = %v", err)
		}
		// The state from the cancelled call is consistent: a fresh call from it loses nothing.
		res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
		if err != nil || len(res2.Txs) != 1 {
			t.Errorf("after cancel: err=%v txs=%d", err, len(res2.Txs))
		}
	})
}

func TestActivityTransactionNotServed(t *testing.T) {
	now := time.Unix(1_700_001_000, 0)
	setClock(t, now)

	t.Run("recent: retried, cursor not advanced", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		sig := l.addSOLSend(testOwner, wallet("bob"), 1000, now.Add(-time.Minute).Unix())
		raw := l.txs[sig]
		l.set(func() { delete(l.txs, sig) }) // the node does not have it yet: result null
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		if len(res.Txs) != 0 || res.BackfillDone {
			t.Fatalf("txs=%d done=%v", len(res.Txs), res.BackfillDone)
		}
		l.set(func() { l.txs[sig] = raw })
		res, _ = l.run(t, s, res.State, time.Time{}, 0)
		if len(res.Txs) != 1 || !res.BackfillDone {
			t.Errorf("after the node caught up: txs=%d done=%v", len(res.Txs), res.BackfillDone)
		}
	})

	t.Run("old: skipped for good", func(t *testing.T) {
		l, s := newLedger(t, testOwner)
		bob := wallet("bob")
		old := l.addSOLSend(testOwner, bob, 1000, now.Add(-time.Hour).Unix())
		pruned := l.addSOLSend(testOwner, bob, 2000, now.Add(-50*time.Minute).Unix())
		l.set(func() {
			l.txRPCErr[pruned] = -32009 // missing in long-term storage
			delete(l.txs, old)          // null
		})
		newest := l.addSOLSend(testOwner, bob, 3000, now.Add(-40*time.Minute).Unix())
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		if len(res.Txs) != 1 || res.Txs[0].ExternalID != newest || !res.BackfillDone {
			t.Errorf("txs=%+v done=%v", res.Txs, res.BackfillDone)
		}
	})
}

func TestActivityStateHandling(t *testing.T) {
	l, s := newLedger(t, testOwner)
	l.addSOLSend(testOwner, wallet("bob"), 1000, 1_700_000_000)

	t.Run("garbage state starts over", func(t *testing.T) {
		res, _ := l.run(t, s, []byte("{not json"), time.Time{}, 0)
		if len(res.Txs) != 1 {
			t.Errorf("txs = %d", len(res.Txs))
		}
	})

	t.Run("state of another wallet is ignored", func(t *testing.T) {
		res, _ := l.run(t, s, []byte(`{"v":1,"owner":"someone-else","accts":{"x":{"newest":"a","oldest":"a","done":true}}}`), time.Time{}, 0)
		if len(res.Txs) != 1 {
			t.Errorf("txs = %d", len(res.Txs))
		}
		if _, ok := decodeState(t, res.State).Accts["x"]; ok {
			t.Errorf("foreign cursor kept")
		}
	})

	t.Run("state shape", func(t *testing.T) {
		res, _ := l.run(t, s, nil, time.Time{}, 0)
		st := decodeState(t, res.State)
		a, ok := st.Accts[testOwner]
		if !ok || a.Newest == "" || a.Oldest == "" || !a.Done {
			t.Errorf("owner cursor = %+v", a)
		}
		if st.Owner != testOwner || st.V != activityStateVersion || len(st.Seen) != 1 {
			t.Errorf("state = %+v", st)
		}
	})

	t.Run("seen set is bounded", func(t *testing.T) {
		seen := make([]string, maxSeenSigs+500)
		for i := range seen {
			seen[i] = fmt.Sprintf("old%05d", i)
		}
		raw, _ := json.Marshal(activityState{V: activityStateVersion, Owner: testOwner, Accts: map[string]*acctCursor{}, Seen: seen})
		res, _ := l.run(t, s, raw, time.Time{}, 0)
		st := decodeState(t, res.State)
		if len(st.Seen) != maxSeenSigs {
			t.Errorf("len(Seen) = %d, want %d", len(st.Seen), maxSeenSigs)
		}
		if last := st.Seen[len(st.Seen)-1]; last != "sig0001" {
			t.Errorf("newest signature must be last, got %q", last)
		}
	})
}

func TestActivityTokenAccountSetChanges(t *testing.T) {
	l, s := newLedger(t, testOwner)
	ata := testMint("ata/owner-usdc")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ata, mint: usdcMint, amount: "1000000", decimals: 6})
	l.addUSDCReceive(ata, testOwner, wallet("alice"), "1000000", 1_700_000_000)
	res, _ := l.run(t, s, nil, time.Time{}, 0)
	if _, ok := decodeState(t, res.State).Accts[ata]; !ok || len(res.Txs) != 1 {
		t.Fatalf("token account not watched: %+v / %d txs", decodeState(t, res.State).Accts, len(res.Txs))
	}

	// The token account is closed: once it is fully fetched it is forgotten.
	l.chain.mu.Lock()
	l.chain.tokenAccts[TokenProgramID] = nil
	l.chain.mu.Unlock()
	res, _ = l.run(t, s, res.State, time.Time{}, 0)
	if _, ok := decodeState(t, res.State).Accts[ata]; ok {
		t.Errorf("closed, fully fetched token account still tracked")
	}

	// A new token account appears later and its history (the incoming transfer that created it
	// is the first entry) is delivered.
	ata2 := testMint("ata/owner-usdc-2")
	l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ata2, mint: usdcMint, amount: "5000000", decimals: 6})
	sig := l.addUSDCReceive(ata2, testOwner, wallet("carol"), "4000000", 1_700_000_500)
	res, _ = l.run(t, s, res.State, time.Time{}, 0)
	if len(res.Txs) != 1 || res.Txs[0].ExternalID != sig {
		t.Errorf("txs = %+v", res.Txs)
	}
}

func TestActivityCapsWatchedTokenAccounts(t *testing.T) {
	l, s := newLedger(t, testOwner)
	total := maxWatchedTokenAccts + 20
	for i := 0; i < total; i++ {
		amount := "0"
		if i >= total-3 {
			amount = "7" // the last three hold a balance and must be watched first
		}
		l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: testMint(fmt.Sprint("spam-ata", i)), mint: testMint(fmt.Sprint("spam", i)), amount: amount, decimals: 0})
	}
	res, _ := l.run(t, s, nil, time.Time{}, 0)
	accts := decodeState(t, res.State).Accts
	if len(accts) != maxWatchedTokenAccts+1 {
		t.Errorf("watching %d addresses, want owner + %d", len(accts), maxWatchedTokenAccts)
	}
	for i := total - 3; i < total; i++ {
		if _, ok := accts[testMint(fmt.Sprint("spam-ata", i))]; !ok {
			t.Errorf("token account %d holding a balance is not watched", i)
		}
	}
}

// TestActivityManyTokenAccountsDoNotStarveBackfill: more token accounts than the budget can even
// check once per call must still let backfill make progress, and every account is eventually
// checked.
func TestActivityManyTokenAccountsDoNotStarveBackfill(t *testing.T) {
	l, s := newLedger(t, testOwner)
	var atas []string
	for i := 0; i < 60; i++ {
		ata := testMint(fmt.Sprint("ata", i))
		atas = append(atas, ata)
		l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: ata, mint: testMint(fmt.Sprint("mint", i)), amount: "1", decimals: 0})
	}
	want := map[string]bool{}
	at := int64(1_700_000_000)
	for i, ata := range atas[:6] {
		at += 10
		want[l.add(txSpec{at: at, fee: 5000, keys: []string{wallet("x"), testMint(fmt.Sprint("ata-x", i)), ata}, toks: []tokBal{
			{acct: ata, owner: testOwner, mint: testMint(fmt.Sprint("mint", i)), program: TokenProgramID, pre: "1", post: "2", dec: 0},
		}})] = true
	}
	txs, state, _ := l.drain(t, s, nil, time.Time{}, 40)
	assertNoDupes(t, txs)
	if len(idSet(txs)) != len(want) {
		t.Fatalf("first sync delivered %d, want %d", len(idSet(txs)), len(want))
	}

	// Steady state: new activity on the last-checked accounts shows up within a few calls even
	// though one call cannot check all 61 accounts.
	late := atas[len(atas)-1]
	at += 10
	sig := l.add(txSpec{at: at, fee: 5000, keys: []string{wallet("x"), testMint("ata-late"), late}, toks: []tokBal{
		{acct: late, owner: testOwner, mint: testMint("mint59"), program: TokenProgramID, pre: "1", post: "5", dec: 0},
	}})
	found := false
	for i := 0; i < 5 && !found; i++ {
		res, _ := l.run(t, s, state, time.Time{}, 40)
		state = res.State
		for _, tx := range res.Txs {
			found = found || tx.ExternalID == sig
		}
	}
	if !found {
		t.Errorf("activity on a rarely-checked token account never arrived")
	}
}

// TestActivityRandomized throws random growth, overlapping signature lists, tiny budgets and
// small pages at the cursor logic and checks the invariant: every transaction is delivered,
// none twice.
func TestActivityRandomized(t *testing.T) {
	for seed := uint64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, seed*7919))
			setPageLimit(t, 2+rng.IntN(5))
			l, s := newLedger(t, testOwner)
			atas := []string{testMint("ata/r0"), testMint("ata/r1")}
			for i, a := range atas {
				l.chain.addTokenAccount(TokenProgramID, fakeAcct{pubkey: a, mint: usdcMint, amount: fmt.Sprint(1000 + i), decimals: 6})
			}
			bob := wallet("bob")
			at := int64(1_700_000_000)
			want := map[string]bool{}
			addRandom := func() {
				at += 10
				switch rng.IntN(4) {
				case 0:
					want[l.addSOLSend(testOwner, bob, 1000+uint64(at%97), at)] = true
				case 1, 2:
					ata := atas[rng.IntN(len(atas))]
					want[l.addUSDCReceive(ata, testOwner, wallet(fmt.Sprint("s", rng.IntN(5))), "1000000", at)] = true
				case 3: // swap touching the owner and both token accounts
					want[l.add(txSpec{at: at, fee: 5000, keys: []string{testOwner, atas[0], atas[1], jupProg},
						delta: map[string]int64{testOwner: -1_000_000},
						toks: []tokBal{
							{acct: atas[0], owner: testOwner, mint: usdcMint, program: TokenProgramID, pre: "0", post: "100", dec: 6},
							{acct: atas[1], owner: testOwner, mint: usdcMint, program: TokenProgramID, pre: "0", post: "100", dec: 6},
						}})] = true
				}
			}
			for i := 0; i < 5+rng.IntN(15); i++ {
				addRandom()
			}

			var got []core.Tx
			var state []byte
			for round := 0; round < 40; round++ {
				budget := 4 + rng.IntN(9)
				res, _ := l.run(t, s, state, time.Time{}, budget)
				got = append(got, res.Txs...)
				state = res.State
				for i := rng.IntN(5); i > 0; i-- {
					addRandom()
				}
			}
			for i := 0; i < 200; i++ { // let it settle
				res, _ := l.run(t, s, state, time.Time{}, 40)
				got = append(got, res.Txs...)
				state = res.State
				if res.BackfillDone && len(res.Txs) == 0 {
					break
				}
			}
			assertNoDupes(t, got)
			have := idSet(got)
			for sig := range want {
				if !have[sig] {
					t.Errorf("missing %s", sig)
				}
			}
			if len(have) != len(want) {
				t.Errorf("delivered %d, want %d", len(have), len(want))
			}
			for sig := range want {
				if n := l.fetchCount(sig); n != 1 {
					t.Errorf("%s fetched %d times", sig, n)
				}
			}
		})
	}
}

func TestActivityMergesGapsBeyondLimit(t *testing.T) {
	// More unfinished ranges than maxGaps collapse into one that still covers all of them.
	setPageLimit(t, 4)
	l, s := newLedger(t, testOwner)
	bob := wallet("bob")
	l.addSOLSend(testOwner, bob, 1000, 1_700_000_000)
	_, state, _ := l.drain(t, s, nil, time.Time{}, 40)

	st := decodeState(t, state)
	a := st.Accts[testOwner]
	for i := 0; i < maxGaps; i++ {
		a.Gaps = append(a.Gaps, gap{Before: fmt.Sprint("b", i), Until: fmt.Sprint("u", i)})
	}
	var sigs []string
	for i := 0; i < 3; i++ {
		sigs = append(sigs, l.addSOLSend(testOwner, bob, 2000+uint64(i), 1_700_000_100+int64(i)))
	}

	// 1 request lists the new signatures, 2 fetch the newest two; the third is left for later,
	// which parks the rest of the range as a 17th gap and triggers the merge.
	r := &activityRun{s: s, ctx: context.Background(), st: &st, left: 3, seen: map[string]bool{}}
	r.topScan(testOwner, a)
	if len(a.Gaps) != 1 || a.Gaps[0].Before != sigs[1] || a.Gaps[0].Until != fmt.Sprint("u", maxGaps-1) {
		t.Errorf("gaps = %+v, want one {Before: %s, Until: u%d}", a.Gaps, sigs[1], maxGaps-1)
	}
	if a.Newest != sigs[2] {
		t.Errorf("Newest = %s, want %s", a.Newest, sigs[2])
	}
}

// addProgramTx records a tx running one instruction of programID with the owner's SOL delta
// (lamports) and token balance changes (mint -> pre/post raw amounts, 6 decimals).
func (l *fakeLedger) addProgramTx(programID string, lamports int64, toks map[string][2]string) {
	keys := []string{testOwner, programID}
	var tb []tokBal
	for mint, pp := range toks {
		ata := testMint("ata/" + mint)
		keys = append(keys, ata, mint)
		tb = append(tb, tokBal{acct: ata, owner: testOwner, mint: mint, program: TokenProgramID, pre: pp[0], post: pp[1], dec: 6})
	}
	l.add(txSpec{
		at: 1_700_000_000, fee: 5000, keys: keys,
		delta: map[string]int64{testOwner: lamports},
		toks:  tb,
		ixs:   []map[string]any{ixRaw(programID)},
	})
}

func legByMint(tx core.Tx, mint string) (float64, bool) {
	for _, l := range tx.Legs {
		if l.Mint == mint {
			return l.Amount, true
		}
	}
	return 0, false
}

func classifyProgramTx(t *testing.T, programID string, lamports int64, toks map[string][2]string) core.Tx {
	t.Helper()
	l, s := newLedger(t, testOwner)
	l.addProgramTx(programID, lamports, toks)
	res, _ := l.run(t, s, nil, time.Time{}, 0)
	return mustOne(t, res.Txs)
}

func TestClassifyJupLock(t *testing.T) {
	jup := testMint("jup")
	tx := classifyProgramTx(t, defi.JupLockedVoterProgram, -2_000_000, map[string][2]string{jup: {"5000000000", "4000000000"}})
	if tx.Kind != core.TxPosition {
		t.Fatalf("kind = %s", tx.Kind)
	}
	if a, ok := legByMint(tx, jup); !ok || a != -1000 {
		t.Errorf("JUP leg = %v (%v)", a, ok)
	}
	if sol := legMap(tx)["SOL"]; sol != -0.002 {
		t.Errorf("SOL leg = %v, want -0.002; legs %+v", sol, tx.Legs)
	}
}

func TestClassifyPerpsCloseIsPosition(t *testing.T) {
	tx := classifyProgramTx(t, defi.JupPerpsProgram, 3_000_000, map[string][2]string{usdcMint: {"1000000", "251000000"}})
	if tx.Kind != core.TxPosition {
		t.Errorf("kind = %s, legs %+v", tx.Kind, tx.Legs)
	}
}

func TestClassifyPerpsJLPBuyIsSwap(t *testing.T) {
	tx := classifyProgramTx(t, defi.JupPerpsProgram, -1_000_000_000, map[string][2]string{testMint("jlp"): {"0", "40000000"}})
	if tx.Kind != core.TxSwap {
		t.Errorf("kind = %s, legs %+v", tx.Kind, tx.Legs)
	}
}

func TestClassifySwapNotPosition(t *testing.T) {
	tx := classifyProgramTx(t, defi.OrcaWhirlpoolProgram, -1_000_000_000, map[string][2]string{usdcMint: {"0", "150000000"}})
	if tx.Kind != core.TxSwap {
		t.Errorf("kind = %s, legs %+v", tx.Kind, tx.Legs)
	}
}

func TestClassifyLockerUnlock(t *testing.T) {
	jup := testMint("jup")
	tx := classifyProgramTx(t, defi.JupLockerProgram, 0, map[string][2]string{jup: {"0", "500000000"}})
	if tx.Kind != core.TxPosition {
		t.Errorf("kind = %s, legs %+v", tx.Kind, tx.Legs)
	}
}
