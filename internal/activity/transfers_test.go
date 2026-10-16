package activity

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

// byID indexes the matcher's output.
func byID(txs []core.Tx) map[string]core.Tx {
	m := map[string]core.Tx{}
	for _, t := range txs {
		m[t.ID] = t
	}
	return m
}

// wantPair checks that out links a and b to each other as a transfer.
func wantPair(t *testing.T, out []core.Tx, a, b string) {
	t.Helper()
	m := byID(out)
	for _, p := range [][2]string{{a, b}, {b, a}} {
		got, ok := m[p[0]]
		if !ok {
			t.Fatalf("%s not in result %v", p[0], ids(out))
		}
		src, _, _ := strings.Cut(p[1], ":")
		if got.Kind != core.TxTransfer || got.PeerID != p[1] || got.PeerSource != src {
			t.Fatalf("%s: kind=%s peer=%q/%q, want transfer with %s", p[0], got.Kind, got.PeerID, got.PeerSource, p[1])
		}
	}
}

func send(source, ext string, at time.Time, asset string, amount float64) core.Tx {
	return mk(source, ext, at, asset, -amount)
}

func TestMatchTransfersHash(t *testing.T) {
	s := send("a", "s", t0, "BTC", 1)
	s.TxHash = "0xABCDEF"
	r := mk("b", "r", t0.Add(30*24*time.Hour), "BTC", 0.7) // far apart in time and amount
	r.TxHash = "0xabcdef"                                  // hash match ignores case, amount and time
	other := mk("c", "o", t0, "BTC", 1)
	other.TxHash = "0x999"
	out := MatchTransfers([]core.Tx{s, other, r})
	if len(out) != 2 {
		t.Fatalf("got %v", ids(out))
	}
	wantPair(t, out, "a:s", "b:r")
	if out[0].ID != "a:s" || out[1].ID != "b:r" {
		t.Fatalf("result not in input order: %v", ids(out))
	}
}

func TestMatchTransfersHashNeedsSameAssetAndSource(t *testing.T) {
	s := send("a", "s", t0, "BTC", 1)
	s.TxHash = "h"
	wrongAsset := mk("b", "r", t0, "ETH", 1)
	wrongAsset.TxHash = "h"
	sameSource := mk("a", "r2", t0, "BTC", 1)
	sameSource.TxHash = "h"
	if out := MatchTransfers([]core.Tx{s, wrongAsset, sameSource}); len(out) != 0 {
		t.Fatalf("unexpected %v", ids(out))
	}
	// empty hashes never count as equal
	s.TxHash = ""
	r := mk("b", "r", t0.Add(24*time.Hour), "BTC", 1)
	if out := MatchTransfers([]core.Tx{s, r}); len(out) != 0 {
		t.Fatalf("unexpected %v", ids(out))
	}
}

func TestMatchTransfersAmountAndTime(t *testing.T) {
	tests := []struct {
		name   string
		recv   float64
		offset time.Duration
		fee    *core.Leg
		want   bool
	}{
		{"exact, same instant", 1, 0, nil, true},
		{"within 0.5%", 0.9951, time.Hour, nil, true},
		{"at 0.5%", 0.995, time.Hour, nil, true},
		{"over 0.5%", 0.994, time.Hour, nil, false},
		{"larger receive within 0.5%", 1.004, time.Hour, nil, true},
		{"fee widens tolerance", 0.9, time.Hour, &core.Leg{AssetKey: "BTC", Amount: -0.1}, true},
		{"fee too small", 0.89, time.Hour, &core.Leg{AssetKey: "BTC", Amount: -0.1}, false},
		{"fee in another asset is ignored", 0.9, time.Hour, &core.Leg{AssetKey: "ETH", Amount: -0.1}, false},
		{"10 minutes before is ok", 1, -10 * time.Minute, nil, true},
		{"11 minutes before is not", 1, -11 * time.Minute, nil, false},
		{"6 hours after is ok", 1, 6 * time.Hour, nil, true},
		{"6 hours and a minute after is not", 1, 6*time.Hour + time.Minute, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := send("a", "s", t0, "BTC", 1)
			s.Fee = tc.fee
			r := mk("b", "r", t0.Add(tc.offset), "BTC", tc.recv)
			out := MatchTransfers([]core.Tx{s, r})
			if !tc.want {
				if len(out) != 0 {
					t.Fatalf("matched %v", ids(out))
				}
				return
			}
			if len(out) != 2 {
				t.Fatalf("no match")
			}
			wantPair(t, out, "a:s", "b:r")
		})
	}
}

func TestMatchTransfersRejects(t *testing.T) {
	failedSend := send("a", "s", t0, "BTC", 1)
	failedSend.Status = core.TxFailed
	failedRecv := mk("b", "r", t0, "BTC", 1)
	failedRecv.Status = core.TxFailed
	okSend := send("a", "s2", t0, "BTC", 1)
	okRecv := mk("b", "r2", t0, "BTC", 1)

	tests := []struct {
		name string
		txs  []core.Tx
	}{
		{"cross asset", []core.Tx{send("a", "s", t0, "BTC", 1), mk("b", "r", t0, "WBTC", 1)}},
		{"same source", []core.Tx{send("a", "s", t0, "BTC", 1), mk("a", "r", t0, "BTC", 1)}},
		{"failed send", []core.Tx{failedSend, okRecv}},
		{"failed receive", []core.Tx{okSend, failedRecv}},
		{"two receives", []core.Tx{mk("a", "r", t0, "BTC", 1), mk("b", "r", t0, "BTC", 1)}},
		{"two sends", []core.Tx{send("a", "s", t0, "BTC", 1), send("b", "s", t0, "BTC", 1)}},
		{"trade", []core.Tx{func() core.Tx { x := send("a", "s", t0, "BTC", 1); x.Kind = core.TxTrade; return x }(), mk("b", "r", t0, "BTC", 1)}},
		{"reward", []core.Tx{send("a", "s", t0, "BTC", 1), func() core.Tx { x := mk("b", "r", t0, "BTC", 1); x.Kind = core.TxReward; return x }()}},
		{"nil", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if out := MatchTransfers(tc.txs); len(out) != 0 {
				t.Fatalf("matched %v", ids(out))
			}
		})
	}
}

func TestMatchTransfersIgnoresSwapsAndNetZero(t *testing.T) {
	// A swap is never a transfer, even though it has an outgoing ETH leg.
	swap := core.Tx{
		ID: "a:swap", SourceID: "a", Time: t0, Kind: core.TxSwap, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "ETH", Amount: -1}, {AssetKey: "USDC", Amount: 3000}},
	}
	if out := MatchTransfers([]core.Tx{swap, mk("b", "r", t0, "ETH", 1)}); len(out) != 0 {
		t.Fatalf("swap matched: %v", ids(out))
	}
	// A send whose legs net to zero in the asset is not a send of it.
	self := core.Tx{ID: "a:self", SourceID: "a", Time: t0, Kind: core.TxOther, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "BTC", Amount: -1}, {AssetKey: "BTC", Amount: 1}}}
	if out := MatchTransfers([]core.Tx{self, mk("b", "r", t0, "BTC", 1)}); len(out) != 0 {
		t.Fatalf("self-send matched: %v", ids(out))
	}
}

func TestMatchTransfersClosestInTime(t *testing.T) {
	s := send("a", "s", t0, "BTC", 1)
	far := mk("b", "far", t0.Add(3*time.Hour), "BTC", 1)
	near := mk("c", "near", t0.Add(10*time.Minute), "BTC", 1)
	before := mk("d", "before", t0.Add(-9*time.Minute), "BTC", 1)

	out := MatchTransfers([]core.Tx{far, s, near}) // input order must not matter
	if len(out) != 2 {
		t.Fatalf("got %v", ids(out))
	}
	wantPair(t, out, "a:s", "c:near")

	out = MatchTransfers([]core.Tx{far, s, near, before})
	wantPair(t, out, "a:s", "d:before")
	if len(out) != 2 {
		t.Fatalf("got %v", ids(out))
	}
}

func TestMatchTransfersOneToOne(t *testing.T) {
	s1 := send("a", "s1", t0, "BTC", 1)
	s2 := send("a", "s2", t0.Add(time.Minute), "BTC", 1)
	r := mk("b", "r", t0.Add(2*time.Minute), "BTC", 1)

	out := MatchTransfers([]core.Tx{s1, s2, r})
	if len(out) != 2 {
		t.Fatalf("one receive must pair with exactly one send, got %v", ids(out))
	}
	wantPair(t, out, "a:s2", "b:r") // s2 is the closer send

	// two sends and two receives pair up without sharing
	r2 := mk("b", "r2", t0.Add(3*time.Minute), "BTC", 1)
	out = MatchTransfers([]core.Tx{s1, s2, r, r2})
	if len(out) != 4 {
		t.Fatalf("got %v", ids(out))
	}
	m := byID(out)
	peers := map[string]bool{}
	for _, tx := range m {
		if peers[tx.PeerID] {
			t.Fatalf("%s used as a peer twice", tx.PeerID)
		}
		peers[tx.PeerID] = true
		if m[tx.PeerID].PeerID != tx.ID {
			t.Fatalf("link %s -> %s is not mutual", tx.ID, tx.PeerID)
		}
	}
}

func TestMatchTransfersStrongBeatsWeak(t *testing.T) {
	s := send("a", "s", t0, "BTC", 1)
	s.TxHash = "h"
	nearby := mk("c", "close", t0.Add(time.Minute), "BTC", 1) // weak candidate, closer in time
	strong := mk("b", "strong", t0.Add(time.Hour), "BTC", 1)
	strong.TxHash = "h"
	out := MatchTransfers([]core.Tx{s, nearby, strong})
	if len(out) != 2 {
		t.Fatalf("got %v", ids(out))
	}
	wantPair(t, out, "a:s", "b:strong")
}

func TestMatchTransfersLeavesPeeredAlone(t *testing.T) {
	s := send("a", "s", t0, "BTC", 1)
	s.Kind, s.PeerID, s.PeerSource = core.TxTransfer, "z:9", "z"
	r := mk("b", "r", t0, "BTC", 1)
	if out := MatchTransfers([]core.Tx{s, r}); len(out) != 0 {
		t.Fatalf("peered send was matched again: %v", ids(out))
	}
	// the free receive can still pair with another free send
	s2 := send("c", "s2", t0, "BTC", 1)
	out := MatchTransfers([]core.Tx{s, r, s2})
	wantPair(t, out, "c:s2", "b:r")
	if len(out) != 2 {
		t.Fatalf("got %v", ids(out))
	}
}

func TestMatchTransfersIdempotentAndPure(t *testing.T) {
	txs := []core.Tx{
		send("a", "s", t0, "BTC", 1),
		mk("b", "r", t0.Add(time.Minute), "BTC", 1),
		send("b", "s2", t0.Add(time.Hour), "ETH", 2),
		mk("a", "r2", t0.Add(time.Hour+time.Minute), "ETH", 2),
		mk("c", "lonely", t0, "SOL", 5),
	}
	orig := append([]core.Tx(nil), txs...)

	out := MatchTransfers(txs)
	if len(out) != 4 {
		t.Fatalf("got %v", ids(out))
	}
	if !reflect.DeepEqual(txs, orig) {
		t.Fatal("input was modified")
	}
	again := MatchTransfers(txs)
	if !reflect.DeepEqual(ids(again), ids(out)) {
		t.Fatalf("same input gave a different answer: %v vs %v", ids(again), ids(out))
	}

	// apply the changes, run again: nothing left to do
	m := byID(out)
	applied := make([]core.Tx, len(txs))
	for i, tx := range txs {
		if c, ok := m[tx.ID]; ok {
			tx = c
		}
		applied[i] = tx
	}
	if out2 := MatchTransfers(applied); len(out2) != 0 {
		t.Fatalf("second run changed %v", ids(out2))
	}
}

func TestPositionNeverMatchedAsTransfer(t *testing.T) {
	a := mk("a", "p1", t0, "USDC", -100)
	a.Kind = core.TxPosition
	b := mk("b", "p2", t0, "USDC", 100)
	b.Kind = core.TxPosition
	if out := MatchTransfers([]core.Tx{a, b}); len(out) != 0 {
		t.Fatalf("position matched as transfer: %v", ids(out))
	}
}
