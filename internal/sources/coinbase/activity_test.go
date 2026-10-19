package coinbase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

// ---------------------------------------------------------------------------------------------
// A fake v2 API with real cursor pagination
// ---------------------------------------------------------------------------------------------

type fakeWallet struct {
	ID, Code, Name string
}

// v2Fake serves /v2/accounts and the per-account transactions the way the v2 API pages them:
// newest first, limit/starting_after/ending_before cursors and a next_uri on every page that has
// older entries behind it. ending_before=X returns the page immediately before X, which is the
// meaning Activity relies on.
type v2Fake struct {
	mu          sync.Mutex
	wallets     []fakeWallet
	txs         map[string][]v2Tx // per account, newest first
	acctPage    int               // accounts per page, 0 = all
	failTxs     map[string]int    // account id -> status returned by its transactions endpoint
	showStatus  map[string]string // tx id -> status served by the show endpoint
	listCalls   int
	showCalls   int
	txListCalls map[string]int
	// beforeTxs, when set, runs (with the lock held) before each transactions page is served.
	beforeTxs func(account string)
}

func newV2Fake(wallets ...fakeWallet) *v2Fake {
	return &v2Fake{wallets: wallets, txs: map[string][]v2Tx{}, failTxs: map[string]int{}, showStatus: map[string]string{}, txListCalls: map[string]int{}}
}

// add puts txs at the head of the account's history (they must be newer than what is there and
// given newest first).
func (f *v2Fake) add(account string, txs ...v2Tx) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs[account] = append(append([]v2Tx(nil), txs...), f.txs[account]...)
}

func (f *v2Fake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		path := strings.TrimPrefix(r.URL.Path, "/v2/accounts")
		switch {
		case path == "":
			f.listCalls++
			f.serveAccounts(w, q)
		case strings.HasSuffix(path, "/transactions"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/transactions")
			f.serveTxs(w, id, q)
		case strings.Contains(path, "/transactions/"):
			parts := strings.Split(strings.TrimPrefix(path, "/"), "/transactions/")
			f.showCalls++
			for _, tx := range f.txs[parts[0]] {
				if tx.ID == parts[1] {
					if st, ok := f.showStatus[tx.ID]; ok {
						tx.Status = st
					}
					json.NewEncoder(w).Encode(map[string]any{"data": tx})
					return
				}
			}
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func (f *v2Fake) serveAccounts(w http.ResponseWriter, q url.Values) {
	start := 0
	if after := q.Get("starting_after"); after != "" {
		for i, a := range f.wallets {
			if a.ID == after {
				start = i + 1
			}
		}
	}
	end := len(f.wallets)
	if f.acctPage > 0 && start+f.acctPage < end {
		end = start + f.acctPage
	}
	var data []map[string]any
	for _, a := range f.wallets[start:end] {
		data = append(data, map[string]any{
			"id": a.ID, "name": a.Code + " Wallet", "type": "wallet",
			"currency": map[string]any{"code": a.Code, "name": a.Name},
			"balance":  map[string]any{"amount": "1", "currency": a.Code},
		})
	}
	var next any
	if end < len(f.wallets) {
		next = fmt.Sprintf("/v2/accounts?limit=100&starting_after=%s", f.wallets[end-1].ID)
	}
	json.NewEncoder(w).Encode(map[string]any{"pagination": map[string]any{"next_uri": next}, "data": data})
}

func (f *v2Fake) serveTxs(w http.ResponseWriter, id string, q url.Values) {
	f.txListCalls[id]++
	if f.beforeTxs != nil {
		f.beforeTxs(id)
	}
	if code := f.failTxs[id]; code != 0 {
		w.WriteHeader(code)
		fmt.Fprint(w, `{"errors":[{"id":"forbidden","message":"nope"}]}`)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 25
	}
	list := f.txs[id]
	indexOf := func(txid string) int {
		for i, tx := range list {
			if tx.ID == txid {
				return i
			}
		}
		return -1
	}
	var page []v2Tx
	older := false
	switch {
	case q.Get("ending_before") != "":
		i := indexOf(q.Get("ending_before"))
		if i < 0 {
			http.Error(w, `{"errors":[{"id":"not_found"}]}`, http.StatusNotFound)
			return
		}
		page = list[max(0, i-limit):i]
		older = true // the cursor itself and everything after it
	case q.Get("starting_after") != "":
		i := indexOf(q.Get("starting_after"))
		if i < 0 {
			http.Error(w, `{"errors":[{"id":"not_found"}]}`, http.StatusNotFound)
			return
		}
		page = list[i+1 : min(len(list), i+1+limit)]
		older = i+1+limit < len(list)
	default:
		page = list[:min(len(list), limit)]
		older = limit < len(list)
	}
	var next any
	if older && len(page) > 0 {
		next = fmt.Sprintf("/v2/accounts/%s/transactions?&limit=%d&order=desc&starting_after=%s", id, limit, page[len(page)-1].ID)
	}
	if page == nil {
		page = []v2Tx{}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"pagination": map[string]any{"limit": limit, "order": "desc", "next_uri": next},
		"data":       page,
	})
}

func newActivitySource(t *testing.T, f *v2Fake) (*Source, *fakeCB) {
	t.Helper()
	return newES256Source(t, f.handler(t))
}

// txReqs returns the transaction requests (list and show) seen by the server.
func txReqs(f *fakeCB) []seenReq {
	var out []seenReq
	for _, r := range f.requests() {
		if strings.Contains(r.path, "/transactions") {
			out = append(out, r)
		}
	}
	return out
}

var t0 = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// mkTx builds a completed BTC-style transaction.
func mkTx(id, typ, amount, cur string, at time.Time) v2Tx {
	return v2Tx{
		ID: id, Type: typ, Status: "completed",
		Amount:       v2Money{Amount: decStr(amount), Currency: cur},
		NativeAmount: v2Money{Amount: "10.00", Currency: "USD"},
		CreatedAt:    at.Format(time.RFC3339),
	}
}

// history builds n transactions named prefix-0001... , newest first, one hour apart, starting at newest.
func history(prefix string, n int, newest time.Time) []v2Tx {
	out := make([]v2Tx, n)
	for i := range out {
		out[i] = mkTx(fmt.Sprintf("%s-%04d", prefix, i+1), "buy", "0.01", "BTC", newest.Add(-time.Duration(i)*time.Hour))
	}
	return out
}

func ids(txs []core.Tx) []string {
	out := make([]string, len(txs))
	for i, tx := range txs {
		out[i] = tx.ExternalID
	}
	return out
}

// collect runs Activity until BackfillDone (or maxCalls), returning every Tx seen and failing on
// any repeated id. The budget stays the same on every call.
func collect(t *testing.T, s *Source, since time.Time, budget, maxCalls int) (all []core.Tx, state []byte, calls int) {
	t.Helper()
	seen := map[string]bool{}
	for calls < maxCalls {
		calls++
		res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Since: since, Budget: budget})
		if err != nil {
			t.Fatalf("call %d: %v", calls, err)
		}
		for _, tx := range res.Txs {
			if seen[tx.ID] {
				t.Fatalf("call %d returned %s again", calls, tx.ID)
			}
			seen[tx.ID] = true
			all = append(all, tx)
		}
		state = res.State
		if res.BackfillDone {
			return all, state, calls
		}
	}
	t.Fatalf("backfill not done after %d calls", maxCalls)
	return nil, nil, 0
}

// ---------------------------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------------------------

func TestSourceImplementsActivitySource(t *testing.T) {
	var _ core.ActivitySource = (*Source)(nil)
}

func TestMapTxKinds(t *testing.T) {
	s, _ := newES256Source(t, func(http.ResponseWriter, *http.Request) {})
	w := walletMeta{ID: "a", Code: "BTC", Name: "Bitcoin"}
	cases := []struct {
		typ, amount string
		want        core.TxKind
	}{
		{"buy", "1", core.TxTrade},
		{"sell", "-1", core.TxTrade},
		{"trade", "1", core.TxTrade},
		{"advanced_trade_fill", "-1", core.TxTrade},
		{"retail_simple_dust", "-0.0001", core.TxTrade},
		{"send", "-1", core.TxSend},
		{"send", "1", core.TxReceive},
		{"receive", "1", core.TxReceive},
		{"staking_reward", "0.1", core.TxReward},
		{"inflation_reward", "0.1", core.TxReward},
		{"interest", "0.1", core.TxReward},
		{"earn_payout", "0.1", core.TxReward},
		{"reward", "0.1", core.TxReward},
		{"fiat_deposit", "1", core.TxTransfer},
		{"fiat_withdrawal", "-1", core.TxTransfer},
		{"exchange_deposit", "1", core.TxTransfer},
		{"exchange_withdrawal", "-1", core.TxTransfer},
		{"pro_deposit", "1", core.TxTransfer},
		{"pro_withdrawal", "-1", core.TxTransfer},
		{"transfer", "-1", core.TxTransfer},
		// Types that are in Coinbase's docs but not in the brief.
		{"staking_transfer", "-1", core.TxTransfer},
		{"unstaking_transfer", "1", core.TxTransfer},
		{"vault_withdrawal", "-1", core.TxTransfer},
		{"incentives_rewards_payout", "1", core.TxReward},
		{"subscription_rebate", "1", core.TxOther},
		{"tx", "1", core.TxOther},
		{"something_new", "1", core.TxOther},
	}
	for _, tc := range cases {
		t.Run(tc.typ+"/"+tc.amount, func(t *testing.T) {
			tx, err := s.mapTx(w, mkTx("t1", tc.typ, tc.amount, "BTC", t0))
			if err != nil {
				t.Fatal(err)
			}
			if tx.Kind != tc.want {
				t.Errorf("kind = %s, want %s", tx.Kind, tc.want)
			}
			if len(tx.Legs) != 1 || tx.Legs[0].Symbol != "BTC" || tx.Legs[0].AssetKey != "BTC" {
				t.Fatalf("legs = %+v", tx.Legs)
			}
			wantAmt, _ := strconv.ParseFloat(tc.amount, 64)
			if tx.Legs[0].Amount != wantAmt {
				t.Errorf("amount = %v, want the signed amount %v", tx.Legs[0].Amount, wantAmt)
			}
			if tc.want == core.TxOther && !strings.Contains(tx.Note, "coinbase type: "+tc.typ) {
				t.Errorf("note %q must keep the type", tx.Note)
			}
			if tx.ID != "coinbase:t1" || tx.SourceID != "coinbase" || tx.ExternalID != "t1" {
				t.Errorf("ids = %q %q %q", tx.ID, tx.SourceID, tx.ExternalID)
			}
		})
	}
}

func TestMapTxStatus(t *testing.T) {
	s, _ := newES256Source(t, func(http.ResponseWriter, *http.Request) {})
	w := walletMeta{ID: "a", Code: "BTC"}
	for status, want := range map[string]core.TxStatus{
		"completed":             core.TxConfirmed,
		"pending":               core.TxPending,
		"failed":                core.TxFailed,
		"canceled":              core.TxFailed,
		"expired":               core.TxFailed,
		"waiting_for_clearing":  core.TxPending,
		"waiting_for_signature": core.TxPending,
		"":                      core.TxPending,
		"something_new":         core.TxPending,
	} {
		tx := mkTx("t", "send", "-1", "BTC", t0)
		tx.Status = status
		got, err := s.mapTx(w, tx)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Errorf("status %q -> %s, want %s", status, got.Status, want)
		}
	}
}

func TestMapTxUSDFromNativeAmount(t *testing.T) {
	s, _ := newES256Source(t, func(http.ResponseWriter, *http.Request) {})
	w := walletMeta{ID: "a", Code: "BTC"}

	tx := mkTx("t", "sell", "-0.1", "BTC", t0)
	tx.NativeAmount = v2Money{Amount: "-4100.50", Currency: "USD"}
	got, _ := s.mapTx(w, tx)
	if got.Legs[0].USD == nil || *got.Legs[0].USD != -4100.50 {
		t.Errorf("USD = %v, want -4100.50 (signed like the amount)", got.Legs[0].USD)
	}

	tx.NativeAmount = v2Money{Amount: "3500.00", Currency: "EUR"}
	if got, _ = s.mapTx(w, tx); got.Legs[0].USD != nil {
		t.Errorf("a EUR native amount must not become USD, got %v", *got.Legs[0].USD)
	}
	tx.NativeAmount = v2Money{}
	if got, _ = s.mapTx(w, tx); got.Legs[0].USD != nil {
		t.Errorf("a missing native amount must stay unknown, got %v", *got.Legs[0].USD)
	}

	// A USD wallet is worth its own amount even without native_amount.
	usd := mkTx("u", "fiat_deposit", "250.00", "USD", t0)
	usd.NativeAmount = v2Money{}
	got, _ = s.mapTx(walletMeta{ID: "u", Code: "USD"}, usd)
	if got.Legs[0].USD == nil || *got.Legs[0].USD != 250 {
		t.Errorf("USD wallet leg USD = %v", got.Legs[0].USD)
	}
}

func TestMapTxStakedWalletsUseBaseAsset(t *testing.T) {
	s, _ := newES256Source(t, func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		w    walletMeta
		cur  string
		want string
	}{
		{walletMeta{ID: "1", Code: "ETH2", Name: "Ethereum 2"}, "ETH2", "ETH"},
		{walletMeta{ID: "2", Code: "ATOM", Name: "Staked Cosmos"}, "ATOM", "ATOM"},
		{walletMeta{ID: "3", Code: "DOT-STAKED", Name: "Polkadot"}, "DOT-STAKED", "DOT"},
		{walletMeta{ID: "4", Code: "CBETH", Name: "Coinbase Wrapped Staked ETH"}, "CBETH", "CBETH"},
		{walletMeta{ID: "5", Code: "SOL", Name: "Solana"}, "", "SOL"}, // amount without currency falls back to the wallet's
	}
	for _, tc := range cases {
		tx, err := s.mapTx(tc.w, mkTx("r", "staking_reward", "0.02", tc.cur, t0))
		if err != nil {
			t.Fatal(err)
		}
		if tx.Kind != core.TxReward || tx.Legs[0].AssetKey != tc.want || tx.Legs[0].Symbol != tc.want {
			t.Errorf("%s: kind=%s leg=%+v, want reward in %s", tc.w.Code, tx.Kind, tx.Legs[0], tc.want)
		}
	}
}

func TestMapTxSendHashFeeCounterparty(t *testing.T) {
	s, _ := newES256Source(t, func(http.ResponseWriter, *http.Request) {})
	w := walletMeta{ID: "a", Code: "BTC"}

	send := mkTx("s", "send", "-0.00133", "BTC", t0)
	send.Network = &v2Network{
		Status: "confirmed", Hash: "abcd", TransactionURL: "https://explorer/tx/abcd",
		TransactionFee: &v2Money{Amount: "0.00033", Currency: "BTC"},
	}
	send.To = &v2Party{Resource: "bitcoin_address", Address: "bc1qdest"}
	got, err := s.mapTx(w, send)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != core.TxSend || got.TxHash != "abcd" || got.ExplorerURL != "https://explorer/tx/abcd" || got.Counterparty != "bc1qdest" {
		t.Errorf("send = %+v", got)
	}
	if got.Fee == nil || got.Fee.Amount != -0.00033 || got.Fee.Symbol != "BTC" || got.Fee.AssetKey != "BTC" {
		t.Errorf("fee = %+v, want a negative BTC leg", got.Fee)
	}
	// Without network.transaction_amount there is no proof that amount includes the fee, so the
	// leg stays as Coinbase gives it.
	if got.Legs[0].Amount != -0.00133 {
		t.Errorf("leg = %v", got.Legs[0].Amount)
	}

	// With transaction_amount + fee == amount, the fee is cut out of the leg (core.Tx.Fee is
	// "already excluded from Legs"), and the leg's USD value is scaled to match.
	send.Network.TransactionAmount = &v2Money{Amount: "0.001", Currency: "BTC"}
	send.NativeAmount = v2Money{Amount: "-133.00", Currency: "USD"}
	got, _ = s.mapTx(w, send)
	if got.Legs[0].Amount != -0.001 || got.Fee == nil || got.Fee.Amount != -0.00033 {
		t.Errorf("fee-inclusive send: leg=%v fee=%+v", got.Legs[0].Amount, got.Fee)
	}
	if u := got.Legs[0].USD; u == nil || *u < -100.0000001 || *u > -99.9999999 {
		t.Errorf("scaled USD = %v, want -100", u)
	}

	// transaction_amount equal to amount: the fee is on top, the leg stays.
	send.Network.TransactionAmount = &v2Money{Amount: "0.00133", Currency: "BTC"}
	got, _ = s.mapTx(w, send)
	if got.Legs[0].Amount != -0.00133 || got.Fee == nil {
		t.Errorf("fee-exclusive send: leg=%v fee=%+v", got.Legs[0].Amount, got.Fee)
	}

	// A fee on an incoming transaction is the sender's, not ours.
	recv := mkTx("r", "receive", "0.5", "BTC", t0)
	recv.Network = &v2Network{Hash: "ffff", TransactionFee: &v2Money{Amount: "0.0001", Currency: "BTC"}}
	recv.From = &v2Party{Resource: "bitcoin_network"}
	got, _ = s.mapTx(w, recv)
	if got.Fee != nil || got.TxHash != "ffff" || got.Counterparty != "bitcoin_network" {
		t.Errorf("receive = %+v", got)
	}

	// Within Coinbase there is no hash.
	internal := mkTx("i", "send", "-0.1", "BTC", t0)
	internal.Network = &v2Network{Status: "off_blockchain", Name: "bitcoin"}
	internal.To = &v2Party{ID: "u2", Resource: "user"}
	got, _ = s.mapTx(w, internal)
	if got.TxHash != "" || got.ExplorerURL != "" || got.Fee != nil || got.Counterparty != "" {
		t.Errorf("internal send = %+v", got)
	}

	// Counterparty from a user name and an email.
	in := mkTx("n", "send", "0.1", "BTC", t0)
	in.From = &v2Party{ID: "u1", Resource: "user", Name: "Alice"}
	if got, _ = s.mapTx(w, in); got.Kind != core.TxReceive || got.Counterparty != "Alice" {
		t.Errorf("incoming send = %+v", got)
	}
	out := mkTx("m", "send", "-0.1", "BTC", t0)
	out.To = &v2Party{Resource: "email", Email: "bob@example.com"}
	if got, _ = s.mapTx(w, out); got.Counterparty != "bob@example.com" {
		t.Errorf("counterparty = %q", got.Counterparty)
	}
}

func TestMapTxRejectsBadData(t *testing.T) {
	s, _ := newES256Source(t, func(http.ResponseWriter, *http.Request) {})
	w := walletMeta{ID: "a", Code: "BTC"}
	bad := mkTx("t", "buy", "1", "BTC", t0)
	bad.CreatedAt = "yesterday"
	if _, err := s.mapTx(w, bad); err == nil {
		t.Error("expected an error for a bad created_at")
	}
	bad = mkTx("t", "buy", "lots", "BTC", t0)
	if _, err := s.mapTx(w, bad); err == nil {
		t.Error("expected an error for a bad amount")
	}
}

// The fixture is a realistic v2 list response (nulls, extra fields, offsets in timestamps).
func TestActivityFixtureEndToEnd(t *testing.T) {
	raw, err := os.ReadFile("testdata/v2_transactions_btc.json")
	if err != nil {
		t.Fatal(err)
	}
	var list v2TxList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	f := newV2Fake(fakeWallet{"acc-btc", "BTC", "Bitcoin"})
	f.add("acc-btc", list.Data...)
	s, _ := newActivitySource(t, f)

	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.BackfillDone || len(res.Txs) != len(list.Data) {
		t.Fatalf("done=%v txs=%d, want %d", res.BackfillDone, len(res.Txs), len(list.Data))
	}
	by := map[string]core.Tx{}
	for _, tx := range res.Txs {
		by[tx.ExternalID] = tx
	}
	check := func(id string, kind core.TxKind, status core.TxStatus, amount float64) core.Tx {
		t.Helper()
		tx, ok := by[id]
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if tx.Kind != kind || tx.Status != status || tx.Legs[0].Amount != amount {
			t.Errorf("%s: kind=%s status=%s amount=%v; want %s %s %v", id, tx.Kind, tx.Status, tx.Legs[0].Amount, kind, status, amount)
		}
		return tx
	}
	if tx := check("tx-buy", core.TxTrade, core.TxConfirmed, 0.5); tx.Legs[0].USD == nil || *tx.Legs[0].USD != 20000 || tx.Note != "Bought bitcoin · using Capital One Bank" {
		t.Errorf("tx-buy = %+v", tx)
	}
	check("tx-sell", core.TxTrade, core.TxConfirmed, -0.1)
	check("tx-fill", core.TxTrade, core.TxConfirmed, 0.01)
	send := check("tx-send", core.TxSend, core.TxConfirmed, -0.001) // fee cut out: 0.00133 = 0.001 + 0.00033
	if send.Fee == nil || send.Fee.Amount != -0.00033 || send.TxHash == "" || !strings.HasPrefix(send.ExplorerURL, "https://blockchain.info/tx/") ||
		send.Counterparty != "bc1qexampledestinationaddress0000000000" || send.Note != "Rent" {
		t.Errorf("tx-send = %+v", send)
	}
	if want := time.Date(2025, 3, 23, 15, 30, 0, 0, time.UTC); !send.Time.Equal(want) || send.Time.Location() != time.UTC {
		t.Errorf("time = %v, want %v in UTC", send.Time, want)
	}
	if r := check("tx-receive", core.TxReceive, core.TxConfirmed, 0.3); r.Counterparty != "bitcoin_network" || r.TxHash == "" {
		t.Errorf("tx-receive = %+v", r)
	}
	if r := check("tx-send-in", core.TxReceive, core.TxConfirmed, 0.2); r.Counterparty != "Alice" {
		t.Errorf("tx-send-in = %+v", r)
	}
	check("tx-reward", core.TxReward, core.TxConfirmed, 0.0001)
	check("tx-earn", core.TxReward, core.TxConfirmed, 0.00002)
	check("tx-xfer", core.TxTransfer, core.TxConfirmed, -0.05)
	check("tx-pending", core.TxSend, core.TxPending, -0.002)
	if tx := check("tx-failed", core.TxSend, core.TxFailed, -0.003); tx.Counterparty != "friend@example.com" {
		t.Errorf("tx-failed counterparty = %q", tx.Counterparty)
	}
	if tx := check("tx-unknown", core.TxOther, core.TxConfirmed, 0.00001); tx.Note != "coinbase type: subscription_rebate · Coinbase One rebate" {
		t.Errorf("tx-unknown note = %q", tx.Note)
	}
	for i := 1; i < len(res.Txs); i++ {
		if res.Txs[i].Time.After(res.Txs[i-1].Time) {
			t.Fatal("transactions are not sorted newest first")
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Pagination: new activity vs backfill
// ---------------------------------------------------------------------------------------------

func TestActivityBackfillFollowsNextURIThenNewUsesEndingBefore(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc-btc", "BTC", "Bitcoin"})
	f.add("acc-btc", history("t", 250, t0)...)
	s, fake := newActivitySource(t, f)

	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.BackfillDone || len(res.Txs) != 250 {
		t.Fatalf("done=%v txs=%d", res.BackfillDone, len(res.Txs))
	}
	if got := slices.Compact(ids(res.Txs)); len(got) != 250 {
		t.Fatalf("duplicates in the first result")
	}

	// 1 account list + 3 pages: the first has no cursor, the others follow next_uri (older).
	reqs := fake.requests()
	if len(reqs) != 4 || reqs[0].path != "/v2/accounts" {
		t.Fatalf("requests = %+v", reqs)
	}
	q0 := mustQuery(t, reqs[1].query)
	if q0.Get("limit") != "100" || q0.Get("order") != "desc" || q0.Get("starting_after") != "" || q0.Get("ending_before") != "" {
		t.Errorf("first page query = %v", q0)
	}
	if q := mustQuery(t, reqs[2].query); q.Get("starting_after") != "t-0100" {
		t.Errorf("second page query = %v, want starting_after=t-0100", q)
	}
	if q := mustQuery(t, reqs[3].query); q.Get("starting_after") != "t-0200" {
		t.Errorf("third page query = %v, want starting_after=t-0200", q)
	}

	// Three new transactions: one request, ending_before the previous newest, no old ones again.
	f.add("acc-btc", mkTx("n-3", "buy", "1", "BTC", t0.Add(3*time.Hour)), mkTx("n-2", "buy", "1", "BTC", t0.Add(2*time.Hour)), mkTx("n-1", "buy", "1", "BTC", t0.Add(time.Hour)))
	before := len(fake.requests())
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res2.Txs); !slices.Equal(got, []string{"n-3", "n-2", "n-1"}) {
		t.Errorf("new txs = %v", got)
	}
	if !res2.BackfillDone {
		t.Error("BackfillDone must stay true")
	}
	reqs = fake.requests()[before:]
	if len(reqs) != 1 {
		t.Fatalf("new-activity call made %d requests: %+v", len(reqs), reqs)
	}
	if q := mustQuery(t, reqs[0].query); q.Get("ending_before") != "t-0001" || q.Get("limit") != "100" || q.Get("order") != "desc" {
		t.Errorf("new-activity query = %v", q)
	}

	// Nothing new: still one request, nothing returned, state unchanged in substance.
	res3, err := s.Activity(context.Background(), core.ActivityRequest{State: res2.State})
	if err != nil || len(res3.Txs) != 0 || !res3.BackfillDone {
		t.Fatalf("idle call: %+v %v", res3, err)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestActivityManyNewPagesAreContiguous(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc", "BTC", "Bitcoin"})
	f.add("acc", history("old", 5, t0)...)
	s, fake := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || !res.BackfillDone {
		t.Fatalf("%v %v", res.BackfillDone, err)
	}

	// 230 new transactions arrive: three ending_before pages (100, 100, 30), oldest of the new first.
	newer := history("new", 230, t0.Add(1000*time.Hour))
	f.add("acc", newer...)
	before := len(fake.requests())
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Txs) != 230 {
		t.Fatalf("got %d new txs, want 230", len(res2.Txs))
	}
	if got := len(fake.requests()) - before; got != 3 {
		t.Errorf("%d requests, want 3 pages", got)
	}
	seen := map[string]bool{}
	for _, tx := range res2.Txs {
		if seen[tx.ID] || !strings.HasPrefix(tx.ExternalID, "new-") {
			t.Fatalf("bad or repeated tx %s", tx.ID)
		}
		seen[tx.ID] = true
	}
}

func TestActivityBudgetResumeWithoutDuplicates(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc", "BTC", "Bitcoin"})
	f.add("acc", history("t", 250, t0)...)
	s, fake := newActivitySource(t, f)

	var state []byte
	seen := map[string]bool{}
	calls := 0
	for ; calls < 20; calls++ {
		before := len(fake.requests())
		res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 2})
		if err != nil {
			t.Fatal(err)
		}
		if used := len(fake.requests()) - before; used > 2 {
			t.Fatalf("call %d made %d requests with a budget of 2", calls, used)
		}
		for _, tx := range res.Txs {
			if seen[tx.ID] {
				t.Fatalf("call %d repeated %s", calls, tx.ID)
			}
			seen[tx.ID] = true
		}
		// New activity arrives while the backfill is in progress.
		if calls == 1 {
			f.add("acc", mkTx("late-2", "buy", "1", "BTC", t0.Add(2*time.Hour)), mkTx("late-1", "buy", "1", "BTC", t0.Add(time.Hour)))
		}
		state = res.State
		if res.BackfillDone {
			break
		}
		if len(res.Txs) == 0 && calls > 3 && len(seen) == 252 {
			t.Fatal("all transactions seen but BackfillDone never set")
		}
	}
	if len(seen) != 252 {
		t.Fatalf("saw %d transactions in %d calls, want 252", len(seen), calls+1)
	}
	for _, id := range []string{"coinbase:late-1", "coinbase:late-2", "coinbase:t-0001", "coinbase:t-0250"} {
		if !seen[id] {
			t.Errorf("%s never returned", id)
		}
	}
}

func TestActivityBudgetOneStillMakesProgress(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc", "BTC", "Bitcoin"})
	f.add("acc", history("t", 230, t0)...)
	s, fake := newActivitySource(t, f)

	var state []byte
	seen := map[string]bool{}
	for call := 0; call < 30; call++ {
		before := len(fake.requests())
		res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 1})
		if err != nil {
			t.Fatal(err)
		}
		if used := len(fake.requests()) - before; used > 1 {
			t.Fatalf("call %d made %d requests with a budget of 1", call, used)
		}
		for _, tx := range res.Txs {
			if seen[tx.ID] {
				t.Fatalf("call %d repeated %s", call, tx.ID)
			}
			seen[tx.ID] = true
		}
		if call == 3 { // new activity between calls is picked up as well
			f.add("acc", mkTx("late", "buy", "1", "BTC", t0.Add(time.Hour)))
		}
		state = res.State
		if res.BackfillDone && seen["coinbase:late"] {
			if len(seen) != 231 {
				t.Fatalf("saw %d, want 231", len(seen))
			}
			return
		}
	}
	t.Fatalf("no completion with a budget of 1; saw %d", len(seen))
}

func TestActivityDefaultBudgetIs20(t *testing.T) {
	var wallets []fakeWallet
	for i := 0; i < 30; i++ {
		wallets = append(wallets, fakeWallet{fmt.Sprintf("a%02d", i), "BTC", "Bitcoin"})
	}
	f := newV2Fake(wallets...)
	for _, w := range wallets {
		f.add(w.ID, history(w.ID, 150, t0)...) // two pages each
	}
	s, fake := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{}) // Budget 0
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n != 20 {
		t.Errorf("a call with no budget made %d requests, want exactly the default of 20", n)
	}
	if res.BackfillDone {
		t.Error("BackfillDone with 30 unfinished accounts")
	}
}

func TestActivityRotatesThroughManyAccounts(t *testing.T) {
	var wallets []fakeWallet
	for i := 0; i < 12; i++ {
		wallets = append(wallets, fakeWallet{fmt.Sprintf("a%02d", i), "BTC", "Bitcoin"})
	}
	f := newV2Fake(wallets...)
	for _, w := range wallets {
		f.add(w.ID, history(w.ID, 130, t0)...)
	}
	s, _ := newActivitySource(t, f)
	all, state, calls := collect(t, s, time.Time{}, 5, 100)
	if len(all) != 12*130 {
		t.Fatalf("got %d txs after %d calls, want %d", len(all), calls, 12*130)
	}

	// Steady state with more accounts than budget: every account is still checked in turn.
	for _, w := range wallets {
		f.add(w.ID, mkTx("fresh-"+w.ID, "buy", "1", "BTC", t0.Add(time.Hour)))
	}
	got := map[string]bool{}
	for i := 0; i < 10 && len(got) < 12; i++ {
		res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: 3})
		if err != nil {
			t.Fatal(err)
		}
		state = res.State
		for _, tx := range res.Txs {
			got[tx.ExternalID] = true
		}
	}
	if len(got) != 12 {
		t.Errorf("only %d of 12 accounts' new activity found with a budget of 3", len(got))
	}
}

func TestActivitySinceCutoff(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc", "BTC", "Bitcoin"})
	f.add("acc", history("t", 250, t0)...) // t-0001 at t0, t-0250 at t0-249h
	s, fake := newActivitySource(t, f)

	since := t0.Add(-120*time.Hour - 30*time.Minute) // keeps t-0001 .. t-0121
	res, err := s.Activity(context.Background(), core.ActivityRequest{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if !res.BackfillDone {
		t.Error("BackfillDone must be set once the horizon is crossed")
	}
	if len(res.Txs) != 121 {
		t.Fatalf("got %d txs, want 121", len(res.Txs))
	}
	for _, tx := range res.Txs {
		if tx.Time.Before(since) {
			t.Errorf("%s at %v is older than Since", tx.ID, tx.Time)
		}
	}
	// list + page 1 + page 2 (which crosses the horizon); page 3 is never fetched.
	if n := len(fake.requests()); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}

	// Later calls with the same horizon do not go back for more.
	before := len(fake.requests())
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State, Since: since})
	if err != nil || len(res2.Txs) != 0 || !res2.BackfillDone {
		t.Fatalf("second call: %+v %v", res2, err)
	}
	if n := len(fake.requests()) - before; n != 1 {
		t.Errorf("idle call made %d requests, want 1", n)
	}

	// Moving the horizon further back resumes the backfill and brings in the rest.
	res3, err := s.Activity(context.Background(), core.ActivityRequest{State: res2.State, Since: time.Time{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res3.BackfillDone {
		t.Error("BackfillDone after extending the horizon")
	}
	first := map[string]bool{}
	for _, tx := range res.Txs {
		first[tx.ID] = true
	}
	got := map[string]bool{}
	for _, tx := range res3.Txs {
		got[tx.ExternalID] = true
	}
	for i := 122; i <= 250; i++ {
		if id := fmt.Sprintf("t-%04d", i); !got[id] {
			t.Fatalf("%s missing after extending Since", id)
		}
	}
	// The page that crossed the old horizon is read again, so some already-returned ids may repeat
	// (the caller's store is keyed by id); nothing older than that page repeats.
	for id := range got {
		if first["coinbase:"+id] && id < "t-0101" {
			t.Errorf("%s was returned twice although it is not on the replayed page", id)
		}
	}
}

func TestActivityEmptyAccountDoesNotBlockOrRepeat(t *testing.T) {
	f := newV2Fake(fakeWallet{"empty", "ETH", "Ethereum"}, fakeWallet{"acc", "BTC", "Bitcoin"})
	f.add("acc", history("t", 3, t0)...)
	s, fake := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || !res.BackfillDone || len(res.Txs) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	// An empty account costs one check per call, and picks up its first transaction.
	f.add("empty", mkTx("first", "receive", "1", "ETH", t0.Add(time.Hour)))
	before := len(fake.requests())
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res2.Txs); !slices.Equal(got, []string{"first"}) || !res2.BackfillDone {
		t.Errorf("second call: %v done=%v", got, res2.BackfillDone)
	}
	if n := len(fake.requests()) - before; n != 2 {
		t.Errorf("%d requests for two accounts, want 2", n)
	}
}

// ---------------------------------------------------------------------------------------------
// Pending transactions, accounts, errors
// ---------------------------------------------------------------------------------------------

func TestActivityPendingBecomesConfirmed(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc", "BTC", "Bitcoin"})
	p := mkTx("p1", "send", "-0.5", "BTC", time.Now().Add(-time.Hour))
	p.Status = "pending"
	f.add("acc", p)
	s, fake := newActivitySource(t, f)

	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || len(res.Txs) != 1 || res.Txs[0].Status != core.TxPending {
		t.Fatalf("first call: %+v %v", res.Txs, err)
	}
	// Still pending: re-checked (one show request), not returned again.
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil || len(res2.Txs) != 0 {
		t.Fatalf("still pending: %+v %v", res2.Txs, err)
	}
	if f.showCalls != 1 {
		t.Fatalf("show calls = %d, want 1", f.showCalls)
	}
	// Settled: returned again with the new status, then forgotten.
	f.mu.Lock()
	f.showStatus["p1"] = "completed"
	f.mu.Unlock()
	res3, err := s.Activity(context.Background(), core.ActivityRequest{State: res2.State})
	if err != nil || len(res3.Txs) != 1 || res3.Txs[0].Status != core.TxConfirmed || res3.Txs[0].ID != "coinbase:p1" {
		t.Fatalf("settled: %+v %v", res3.Txs, err)
	}
	res4, err := s.Activity(context.Background(), core.ActivityRequest{State: res3.State})
	if err != nil || len(res4.Txs) != 0 || f.showCalls != 2 {
		t.Fatalf("after settling: %+v %v show=%d", res4.Txs, err, f.showCalls)
	}
	_ = fake
}

func TestActivityPendingGivenUpAfterMaxAge(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc", "BTC", "Bitcoin"})
	p := mkTx("p1", "send", "-0.5", "BTC", time.Now().Add(-pendingMaxAge-time.Hour))
	p.Status = "pending"
	f.add("acc", p)
	s, _ := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || len(res.Txs) != 1 {
		t.Fatalf("%v %v", res.Txs, err)
	}
	if _, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State}); err != nil {
		t.Fatal(err)
	}
	if f.showCalls != 0 {
		t.Errorf("a transaction pending for more than %v was still re-checked", pendingMaxAge)
	}
}

func TestActivityAccountListCachedThenRefreshed(t *testing.T) {
	f := newV2Fake(fakeWallet{"a1", "BTC", "Bitcoin"})
	f.acctPage = 1
	f.add("a1", history("a1", 2, t0)...)
	s, _ := newActivitySource(t, f)

	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || f.listCalls != 1 {
		t.Fatalf("%v listCalls=%d", err, f.listCalls)
	}
	res, err = s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil || f.listCalls != 1 {
		t.Fatalf("list was repeated within the TTL: %v listCalls=%d", err, f.listCalls)
	}

	// A wallet created later is found once the cached list expires; the listing is paged.
	f.mu.Lock()
	f.wallets = append(f.wallets, fakeWallet{"a2", "ETH", "Ethereum"}, fakeWallet{"a3", "SOL", "Solana"})
	f.mu.Unlock()
	f.add("a2", mkTx("e1", "receive", "2", "ETH", t0))
	f.add("a3", mkTx("s1", "receive", "3", "SOL", t0))
	defer func(old time.Duration) { accountsTTL = old }(accountsTTL)
	accountsTTL = 0 // the cached list has expired
	res, err = s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil {
		t.Fatal(err)
	}
	if f.listCalls != 1+3 {
		t.Errorf("listCalls = %d, want 1 + 3 pages", f.listCalls)
	}
	if got := ids(res.Txs); !slices.Equal(got, []string{"e1", "s1"}) && !slices.Equal(got, []string{"s1", "e1"}) {
		t.Errorf("new wallets' txs = %v", got)
	}
	if !res.BackfillDone {
		t.Error("BackfillDone")
	}
}

func TestActivityOneAccountForbiddenOthersContinue(t *testing.T) {
	f := newV2Fake(fakeWallet{"bad", "BTC", "Bitcoin"}, fakeWallet{"good", "ETH", "Ethereum"})
	f.add("bad", history("bad", 2, t0)...)
	f.add("good", history("good", 3, t0)...)
	f.failTxs["bad"] = http.StatusForbidden
	s, _ := newActivitySource(t, f)

	// The healthy account's progress is kept: the call ends cleanly, and the account that cannot
	// be read does not hold BackfillDone back.
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || len(res.Txs) != 3 || !res.BackfillDone {
		t.Fatalf("txs=%d done=%v err=%v", len(res.Txs), res.BackfillDone, err)
	}
	// It is not hammered within a call, is tried again on the next one, and recovers.
	if n := f.txListCalls["bad"]; n != 1 {
		t.Errorf("bad account requested %d times in one call", n)
	}
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil || len(res2.Txs) != 0 || f.txListCalls["bad"] != 2 {
		t.Fatalf("second call: txs=%d err=%v bad calls=%d", len(res2.Txs), err, f.txListCalls["bad"])
	}
	f.mu.Lock()
	delete(f.failTxs, "bad")
	f.mu.Unlock()
	res3, err := s.Activity(context.Background(), core.ActivityRequest{State: res2.State})
	if err != nil || len(res3.Txs) != 2 || !res3.BackfillDone {
		t.Fatalf("after recovery: txs=%d done=%v err=%v", len(res3.Txs), res3.BackfillDone, err)
	}
}

func TestActivityEveryAccountFailingIsAnError(t *testing.T) {
	f := newV2Fake(fakeWallet{"a", "BTC", "Bitcoin"}, fakeWallet{"b", "ETH", "Ethereum"})
	f.add("a", history("a", 2, t0)...)
	f.failTxs["a"], f.failTxs["b"] = http.StatusForbidden, http.StatusNotFound
	s, _ := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want both accounts' failures", err)
	}
	if len(res.Txs) != 0 || res.BackfillDone || res.State == nil {
		t.Errorf("result = %+v", res)
	}
}

func TestActivityServerErrorKeepsProgressAndResumes(t *testing.T) {
	f := newV2Fake(fakeWallet{"a", "BTC", "Bitcoin"}, fakeWallet{"b", "ETH", "Ethereum"})
	f.add("a", history("a", 3, t0)...)
	f.add("b", history("b", 3, t0)...)
	f.failTxs["b"] = http.StatusInternalServerError
	s, _ := newActivitySource(t, f)

	// a worked, b failed with a 500 (after httpx's retries): a's transactions are kept, the call
	// ends cleanly and is not done.
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	if err != nil || len(res.Txs) != 3 || res.BackfillDone {
		t.Fatalf("txs=%d done=%v err=%v", len(res.Txs), res.BackfillDone, err)
	}
	// Nothing works any more: now the 500 is reported.
	f.mu.Lock()
	f.failTxs["a"] = http.StatusInternalServerError
	f.mu.Unlock()
	if _, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the 500", err)
	}
	f.mu.Lock()
	delete(f.failTxs, "a")
	f.mu.Unlock()
	f.mu.Lock()
	delete(f.failTxs, "b")
	f.mu.Unlock()
	res2, err := s.Activity(context.Background(), core.ActivityRequest{State: res.State})
	if err != nil || !res2.BackfillDone || len(res2.Txs) != 3 {
		t.Fatalf("resume: txs=%d done=%v err=%v", len(res2.Txs), res2.BackfillDone, err)
	}
	for _, tx := range res2.Txs {
		if !strings.HasPrefix(tx.ExternalID, "b-") {
			t.Errorf("%s repeated from the first call", tx.ID)
		}
	}
}

func TestActivityAccountsListingFailure(t *testing.T) {
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"id":"authentication_error"}]}`, http.StatusUnauthorized)
	})
	state := []byte(`{"seq":3}`)
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: state})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	if string(res.State) != string(state) {
		t.Errorf("state must be returned unchanged on failure, got %s", res.State)
	}
}

func TestActivityBadStateStartsOver(t *testing.T) {
	f := newV2Fake(fakeWallet{"a", "BTC", "Bitcoin"})
	f.add("a", history("a", 2, t0)...)
	s, _ := newActivitySource(t, f)
	res, err := s.Activity(context.Background(), core.ActivityRequest{State: []byte("{not json")})
	if err != nil || len(res.Txs) != 2 || !res.BackfillDone {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestActivityHonoursContext(t *testing.T) {
	f := newV2Fake(fakeWallet{"a", "BTC", "Bitcoin"})
	s, _ := newActivitySource(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Activity(ctx, core.ActivityRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------------------------

// The fake gate in newFakeCB already rejects a request whose JWT is invalid, expired, signed for a
// different uri or carries a nonce seen before; this checks the bookkeeping from the outside.
func TestActivityFreshValidJWTPerRequest(t *testing.T) {
	f := newV2Fake(fakeWallet{"acc/1", "BTC", "Bitcoin"})
	f.add("acc/1", history("t", 250, t0)...)
	s, fake := newActivitySource(t, f)
	if _, err := s.Activity(context.Background(), core.ActivityRequest{}); err != nil {
		t.Fatal(err)
	}
	// ... and a second call with the new-activity query.
	f.add("acc/1", mkTx("n", "buy", "1", "BTC", t0.Add(time.Hour)))
	res, err := s.Activity(context.Background(), core.ActivityRequest{})
	_ = res
	if err != nil {
		t.Fatal(err)
	}

	reqs := fake.requests()
	if len(reqs) < 5 {
		t.Fatalf("only %d requests", len(reqs))
	}
	if len(fake.nonces) != len(reqs) {
		t.Errorf("%d distinct JWT nonces for %d requests", len(fake.nonces), len(reqs))
	}
	for _, r := range reqs {
		if strings.Contains(r.uriClaim, "?") {
			t.Errorf("uri claim %q contains a query", r.uriClaim)
		}
		if !strings.HasSuffix(r.uriClaim, r.path) {
			t.Errorf("uri claim %q does not match path %q", r.uriClaim, r.path)
		}
	}
}

func TestActivityRequestsCarryCBVersion(t *testing.T) {
	var mu sync.Mutex
	var versions []string
	f := newV2Fake(fakeWallet{"a", "BTC", "Bitcoin"})
	f.add("a", history("a", 1, t0)...)
	h := f.handler(t)
	s, _ := newES256Source(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		versions = append(versions, r.Header.Get("CB-VERSION"))
		mu.Unlock()
		h(w, r)
	})
	if _, err := s.Activity(context.Background(), core.ActivityRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(versions) < 2 {
		t.Fatalf("versions = %v", versions)
	}
	for _, v := range versions {
		if v != v2Version {
			t.Errorf("CB-VERSION = %q", v)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Randomised: whatever the budgets, arrivals and account count, every transaction comes out once
// ---------------------------------------------------------------------------------------------

type cbRand struct{ s uint64 }

func (r *cbRand) next(n int) int {
	r.s = r.s*6364136223846793005 + 1442695040888963407
	return int((r.s >> 33) % uint64(n))
}

func TestActivityRandomisedBudgetsAndArrivals(t *testing.T) {
	for seed := uint64(1); seed <= 10; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := &cbRand{s: seed * 104729}
			var wallets []fakeWallet
			for i := 0; i < 1+rng.next(4); i++ {
				wallets = append(wallets, fakeWallet{fmt.Sprintf("acc%d", i), "BTC", "Bitcoin"})
			}
			f := newV2Fake(wallets...)
			clock := t0
			serial := 0
			arrive := func(account string, k int) {
				clock = clock.Add(1000 * time.Hour)
				var batch []v2Tx
				for i := 0; i < k; i++ {
					serial++
					batch = append(batch, mkTx(fmt.Sprintf("%s-%05d", account, serial), "buy", "0.01", "BTC", clock.Add(-time.Duration(i)*time.Minute)))
				}
				f.txs[account] = append(batch, f.txs[account]...)
			}
			f.mu.Lock()
			for _, w := range wallets {
				arrive(w.ID, rng.next(250))
			}
			f.beforeTxs = func(account string) { // arrivals between two requests of a call
				if rng.next(10) == 0 {
					arrive(account, 1+rng.next(130))
				}
			}
			f.mu.Unlock()
			s, _ := newActivitySource(t, f)

			var state []byte
			seen := map[string]bool{}
			for call := 1; call <= 150; call++ {
				if rng.next(3) == 0 {
					f.mu.Lock()
					arrive(wallets[rng.next(len(wallets))].ID, 1+rng.next(120))
					f.mu.Unlock()
				}
				budget := 1 + rng.next(5)
				if call > 100 {
					f.mu.Lock()
					f.beforeTxs = nil
					f.mu.Unlock()
					budget = 40
				}
				res, err := s.Activity(context.Background(), core.ActivityRequest{State: state, Budget: budget})
				if err != nil {
					t.Fatalf("call %d: %v", call, err)
				}
				for _, tx := range res.Txs {
					if seen[tx.ID] {
						t.Fatalf("call %d (budget %d) returned %s again", call, budget, tx.ID)
					}
					seen[tx.ID] = true
				}
				state = res.State
				if call > 100 && res.BackfillDone {
					break
				}
			}
			total := 0
			for _, w := range wallets {
				total += len(f.txs[w.ID])
			}
			if len(seen) != total {
				t.Fatalf("saw %d transactions, the accounts hold %d", len(seen), total)
			}
		})
	}
}
