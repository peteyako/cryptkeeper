package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/store"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "cryptkeep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Store{DB: db}
}

func ptr(f float64) *float64 { return &f }

// mk builds a one-leg tx on a source; positive amount is a receive, negative a send.
func mk(source, ext string, at time.Time, asset string, amount float64) core.Tx {
	kind := core.TxReceive
	if amount < 0 {
		kind = core.TxSend
	}
	return core.Tx{
		ID: core.TxID(source, ext), SourceID: source, ExternalID: ext, Time: at,
		Kind: kind, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: asset, Symbol: asset, Amount: amount}},
	}
}

func ids(txs []core.Tx) []string {
	out := make([]string, len(txs))
	for i, t := range txs {
		out[i] = t.ID
	}
	return out
}

func sameJSON(t *testing.T, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("mismatch\n got  %s\n want %s", g, w)
	}
}

func mustUpsert(t *testing.T, s *Store, txs ...core.Tx) int {
	t.Helper()
	n, err := s.Upsert(context.Background(), txs)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustList(t *testing.T, s *Store, f Filter) []string {
	t.Helper()
	txs, err := s.List(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return ids(txs)
}

func TestStoreRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	tx := core.Tx{
		ID: "cb:1", SourceID: "cb", ExternalID: "1", Time: t0.Add(123456789 * time.Nanosecond),
		Kind: core.TxSwap, Status: core.TxPending, Confirmations: 2,
		Legs: []core.Leg{
			{AssetKey: "ETH", Symbol: "ETH", Amount: -1.5, USD: ptr(-4500)},
			{AssetKey: "spl:Mint111", Symbol: "FOO", Mint: "Mint111", Amount: 9},
		},
		Fee:          &core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -0.000005},
		Counterparty: "Jupiter", TxHash: "abc", ExplorerURL: "https://x/abc", Note: "n",
		PeerID: "kr:9", PeerSource: "kr",
	}
	if n := mustUpsert(t, s, tx); n != 1 {
		t.Fatalf("inserted %d", n)
	}
	got, ok, err := s.Get(ctx, "cb:1")
	if err != nil || !ok {
		t.Fatalf("get: %v %v", ok, err)
	}
	if !reflect.DeepEqual(got, tx) {
		t.Fatalf("round trip differs\n got  %+v\n want %+v", got, tx)
	}
	if _, ok, err := s.Get(ctx, "nope"); ok || err != nil {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if n := mustUpsert(t, s); n != 0 {
		t.Fatal("empty upsert inserted")
	}
	if _, err := s.Upsert(ctx, []core.Tx{{SourceID: "x"}}); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestUpsertCountsAndPendingToConfirmed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := mk("w", "a", t0, "BTC", 1)
	a.Status = core.TxPending
	b := mk("w", "b", t0.Add(time.Minute), "BTC", 2)
	if n := mustUpsert(t, s, a, b); n != 2 {
		t.Fatalf("first insert = %d", n)
	}

	a2 := a
	a2.Status = core.TxConfirmed
	a2.Confirmations = 6
	a2.Time = t0.Add(5 * time.Minute) // confirmation time replaces the first-seen time
	c := mk("w", "c", t0.Add(2*time.Minute), "BTC", 3)
	if n := mustUpsert(t, s, a2, c); n != 1 {
		t.Fatalf("update + 1 new = %d inserted, want 1", n)
	}
	got, _, _ := s.Get(ctx, "w:a")
	if got.Status != core.TxConfirmed || got.Confirmations != 6 || !got.Time.Equal(a2.Time) {
		t.Fatalf("update not applied: %+v", got)
	}
	// the indexed columns follow the update too
	if l := mustList(t, s, Filter{}); !reflect.DeepEqual(l, []string{"w:a", "w:c", "w:b"}) {
		t.Fatalf("order after update: %v", l)
	}
	all, _ := s.All(ctx, nil)
	if len(all) != 3 {
		t.Fatalf("rows = %d, want 3", len(all))
	}

	// duplicates inside one batch count once
	d := mk("w", "d", t0, "BTC", 1)
	if n := mustUpsert(t, s, d, d); n != 1 {
		t.Fatalf("dup in batch inserted %d", n)
	}
}

func TestUpsertKeepsPeer(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	send := mk("a", "1", t0, "BTC", -1)
	send.Status = core.TxPending
	mustUpsert(t, s, send)

	linked := send
	linked.Kind, linked.PeerID, linked.PeerSource = core.TxTransfer, "b:9", "b"
	mustUpsert(t, s, linked)

	// the source re-reports the entry as confirmed, knowing nothing about peers
	again := send
	again.Status = core.TxConfirmed
	if n := mustUpsert(t, s, again); n != 0 {
		t.Fatalf("inserted %d", n)
	}
	got, _, _ := s.Get(ctx, "a:1")
	if got.Status != core.TxConfirmed || got.PeerID != "b:9" || got.PeerSource != "b" || got.Kind != core.TxTransfer {
		t.Fatalf("peer lost: %+v", got)
	}
	if l := mustList(t, s, Filter{Kinds: []core.TxKind{core.TxTransfer}}); len(l) != 1 {
		t.Fatalf("kind column not kept: %v", l)
	}

	// an explicit new peer wins
	moved := send
	moved.Kind, moved.PeerID, moved.PeerSource = core.TxTransfer, "c:3", "c"
	mustUpsert(t, s, moved)
	got, _, _ = s.Get(ctx, "a:1")
	if got.PeerID != "c:3" || got.PeerSource != "c" {
		t.Fatalf("new peer not applied: %+v", got)
	}

	// without a stored peer, nothing is invented and the kind is the source's
	plain := mk("a", "2", t0, "BTC", -1)
	mustUpsert(t, s, plain)
	mustUpsert(t, s, plain)
	got, _, _ = s.Get(ctx, "a:2")
	if got.PeerID != "" || got.Kind != core.TxSend {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestListFilters(t *testing.T) {
	s := newStore(t)
	// newest to oldest: e, d, c, b, a
	a := mk("btc", "a", t0, "BTC", 1)
	b := mk("sol", "b", t0.Add(1*time.Hour), "SOL", -2)
	b.Fee = &core.Leg{AssetKey: "SOL", Amount: -0.01}
	c := mk("cb", "c", t0.Add(2*time.Hour), "ETH", 3)
	c.Kind = core.TxReward
	d := core.Tx{ // swap: SOL out, spl token in, fee paid in a third asset
		ID: "sol:d", SourceID: "sol", ExternalID: "d", Time: t0.Add(3 * time.Hour), Kind: core.TxSwap, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "SOL", Amount: -1}, {AssetKey: "spl:Mint1", Amount: 100}},
		Fee:  &core.Leg{AssetKey: "FEE", Amount: -1},
	}
	e := mk("cb", "e", t0.Add(4*time.Hour), "BTC", -0.5)
	mustUpsert(t, s, a, b, c, d, e)

	tests := []struct {
		name string
		f    Filter
		want []string
	}{
		{"all newest first", Filter{}, []string{"cb:e", "sol:d", "cb:c", "sol:b", "btc:a"}},
		{"source", Filter{SourceIDs: []string{"cb"}}, []string{"cb:e", "cb:c"}},
		{"two sources", Filter{SourceIDs: []string{"btc", "sol"}}, []string{"sol:d", "sol:b", "btc:a"}},
		{"unknown source", Filter{SourceIDs: []string{"zzz"}}, []string{}},
		{"asset", Filter{AssetKeys: []string{"BTC"}}, []string{"cb:e", "btc:a"}},
		{"asset on a second leg", Filter{AssetKeys: []string{"spl:Mint1"}}, []string{"sol:d"}},
		{"fee asset", Filter{AssetKeys: []string{"FEE"}}, []string{"sol:d"}},
		{"any of assets", Filter{AssetKeys: []string{"ETH", "FEE"}}, []string{"sol:d", "cb:c"}},
		{"asset is a whole token", Filter{AssetKeys: []string{"SO"}}, []string{}},
		{"asset is case sensitive", Filter{AssetKeys: []string{"btc"}}, []string{}},
		{"kind", Filter{Kinds: []core.TxKind{core.TxReward}}, []string{"cb:c"}},
		{"kinds", Filter{Kinds: []core.TxKind{core.TxSend, core.TxSwap}}, []string{"cb:e", "sol:d", "sol:b"}},
		{"before is strict", Filter{Before: t0.Add(2 * time.Hour)}, []string{"sol:b", "btc:a"}},
		{"since is inclusive", Filter{Since: t0.Add(2 * time.Hour)}, []string{"cb:e", "sol:d", "cb:c"}},
		{"window", Filter{Since: t0.Add(time.Hour), Before: t0.Add(3 * time.Hour)}, []string{"cb:c", "sol:b"}},
		{"combined", Filter{SourceIDs: []string{"cb"}, AssetKeys: []string{"BTC"}, Kinds: []core.TxKind{core.TxSend}}, []string{"cb:e"}},
		{"limit", Filter{Limit: 2}, []string{"cb:e", "sol:d"}},
		{"before in the future is fine", Filter{Before: t0.Add(100 * time.Hour), Limit: 1}, []string{"cb:e"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustList(t, s, tc.f); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	// zero Before = now: an old entry shows up, and so does one stamped right now
	now := mk("n", "now", time.Now(), "BTC", 1)
	mustUpsert(t, s, now)
	if got := mustList(t, s, Filter{Limit: 1}); got[0] != "n:now" {
		t.Fatalf("recent entry missing: %v", got)
	}
}

func TestListLimitsAndPagination(t *testing.T) {
	s := newStore(t)
	var batch []core.Tx
	for i := 0; i < 520; i++ {
		batch = append(batch, mk("w", fmt.Sprint(i), t0.Add(time.Duration(i)*time.Minute), "BTC", 1))
	}
	mustUpsert(t, s, batch...)

	if got := mustList(t, s, Filter{}); len(got) != 50 {
		t.Fatalf("default limit gave %d", len(got))
	}
	if got := mustList(t, s, Filter{Limit: -1}); len(got) != 50 {
		t.Fatalf("negative limit gave %d", len(got))
	}
	if got := mustList(t, s, Filter{Limit: 100000}); len(got) != 500 {
		t.Fatalf("max limit gave %d", len(got))
	}

	// walk everything with a Before cursor, 200 at a time, newest first
	var seen []string
	var before time.Time
	for {
		page, err := s.List(context.Background(), Filter{Limit: 200, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, tx := range page {
			seen = append(seen, tx.ExternalID)
		}
		before = page[len(page)-1].Time
	}
	if len(seen) != 520 || seen[0] != "519" || seen[519] != "0" {
		t.Fatalf("pagination: %d rows, first %s last %s", len(seen), seen[0], seen[len(seen)-1])
	}
}

func TestAllOldestFirst(t *testing.T) {
	s := newStore(t)
	mustUpsert(t, s,
		mk("b", "2", t0.Add(2*time.Hour), "BTC", 1),
		mk("a", "1", t0.Add(time.Hour), "BTC", 1),
		mk("b", "3", t0.Add(3*time.Hour), "BTC", 1),
		mk("c", "0", t0, "BTC", 1),
	)
	ctx := context.Background()
	all, err := s.All(ctx, nil)
	if err != nil || !reflect.DeepEqual(ids(all), []string{"c:0", "a:1", "b:2", "b:3"}) {
		t.Fatalf("%v %v", ids(all), err)
	}
	some, _ := s.All(ctx, []string{"b", "c"})
	if !reflect.DeepEqual(ids(some), []string{"c:0", "b:2", "b:3"}) {
		t.Fatalf("%v", ids(some))
	}
	none, err := s.All(ctx, []string{"zzz"})
	if err != nil || len(none) != 0 {
		t.Fatalf("%v %v", none, err)
	}
}

func TestDeleteSourceAndStatuses(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustUpsert(t, s, mk("a", "1", t0, "BTC", 1), mk("a", "2", t0, "BTC", 1), mk("b", "1", t0, "BTC", 1))

	if err := s.saveProgress(ctx, "a", []byte("cursor"), true, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.saveError(ctx, "b", "boom"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Statuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 {
		t.Fatalf("statuses: %+v", st)
	}
	a := st["a"]
	if !a.BackfillDone || !a.LastSync.Equal(t0) || a.LastError != "" || a.Count != 2 || a.SourceID != "a" {
		t.Fatalf("a: %+v", a)
	}
	b := st["b"]
	if b.BackfillDone || !b.LastSync.IsZero() || b.LastError != "boom" || b.Count != 0 {
		t.Fatalf("b: %+v", b)
	}
	state, done, err := s.loadState(ctx, "a")
	if err != nil || string(state) != "cursor" || !done {
		t.Fatalf("state: %q %v %v", state, done, err)
	}

	// an error leaves state and last_sync alone; a later success clears the error
	if err := s.saveError(ctx, "a", "later"); err != nil {
		t.Fatal(err)
	}
	state, done, _ = s.loadState(ctx, "a")
	st, _ = s.Statuses(ctx)
	if string(state) != "cursor" || !done || st["a"].LastError != "later" || !st["a"].LastSync.Equal(t0) {
		t.Fatalf("error clobbered progress: %q %v %+v", state, done, st["a"])
	}
	if err := s.saveProgress(ctx, "a", nil, true, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Statuses(ctx)
	if st["a"].LastError != "" {
		t.Fatalf("error not cleared: %+v", st["a"])
	}
	if state, _, _ := s.loadState(ctx, "a"); len(state) != 0 {
		t.Fatalf("nil state stored as %q", state)
	}
	if state, done, err := s.loadState(ctx, "never"); state != nil || done || err != nil {
		t.Fatalf("unknown source: %v %v %v", state, done, err)
	}

	if err := s.DeleteSource(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if got := mustList(t, s, Filter{}); !reflect.DeepEqual(got, []string{"b:1"}) {
		t.Fatalf("rows after delete: %v", got)
	}
	st, _ = s.Statuses(ctx)
	if _, ok := st["a"]; ok || len(st) != 1 {
		t.Fatalf("state not deleted: %+v", st)
	}
	if err := s.DeleteSource(ctx, "never"); err != nil {
		t.Fatal(err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cryptkeep.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{DB: db}
	mustUpsert(t, s, mk("a", "1", t0, "BTC", 1))
	db.Close()

	db, err = store.Open(path) // migrations must be idempotent
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := mustList(t, &Store{DB: db}, Filter{}); !reflect.DeepEqual(got, []string{"a:1"}) {
		t.Fatalf("%v", got)
	}
}
