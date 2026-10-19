package tax

import (
	"testing"

	"cryptkeeper/internal/core"
)

var rbCutover = at(2026, 1, 1, 0)

func cur(v float64) map[string]map[string]float64 {
	return map[string]map[string]float64{"sol": {"SOL": v}}
}

func TestRollBackSubtractsLaterActivity(t *testing.T) {
	recv := mk("a", "sol", at(2026, 2, 1, 0), core.TxReceive, lg("SOL", 3, nil))
	send := mk("b", "sol", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, nil))
	send.Fee = &core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -0.01}
	in := cur(10)
	got := RollBack(in, []core.Tx{recv, send}, rbCutover, nil)
	near(t, "sol", got["sol"]["SOL"], 8.01)
	near(t, "input untouched", in["sol"]["SOL"], 10)
}

func TestRollBackIgnoresStakeAndPosition(t *testing.T) {
	st := mk("a", "sol", at(2026, 2, 1, 0), core.TxStake, lg("SOL", -5, nil))
	st.Fee = &core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -0.01}
	po := mk("b", "sol", at(2026, 3, 1, 0), core.TxPosition, lg("spl:JUP", -1000, nil))
	got := RollBack(map[string]map[string]float64{"sol": {"SOL": 10, "spl:JUP": 7}}, []core.Tx{st, po}, rbCutover, nil)
	near(t, "sol", got["sol"]["SOL"], 10.01)
	near(t, "jup", got["sol"]["spl:JUP"], 7)
}

func TestRollBackFailedFeeOnly(t *testing.T) {
	f := mk("a", "sol", at(2026, 2, 1, 0), core.TxSwap, lg("SOL", -2, nil), lg("USDC", 100, nil))
	f.Status = core.TxFailed
	f.Fee = &core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -0.000005}
	got := RollBack(cur(10), []core.Tx{f}, rbCutover, nil)
	near(t, "sol", got["sol"]["SOL"], 10.000005)
	if _, ok := got["sol"]["USDC"]; ok {
		t.Errorf("failed legs must not create USDC")
	}
}

func TestRollBackBeforeCutoverIgnored(t *testing.T) {
	x := mk("a", "sol", at(2025, 12, 31, 23), core.TxReceive, lg("SOL", 3, nil))
	got := RollBack(cur(10), []core.Tx{x}, rbCutover, nil)
	near(t, "sol", got["sol"]["SOL"], 10)
}

func TestRollBackDustAndNegative(t *testing.T) {
	a := mk("a", "sol", at(2026, 2, 1, 0), core.TxReceive, lg("SOL", 3, nil), lg("BONK", 5, nil))
	got := RollBack(map[string]map[string]float64{"sol": {"SOL": 3 + 1e-14, "BONK": 2}}, []core.Tx{a}, rbCutover, nil)
	if got["sol"]["SOL"] != 0 {
		t.Errorf("dust not zeroed: %v", got["sol"]["SOL"])
	}
	near(t, "bonk negative kept", got["sol"]["BONK"], -3)
}

func TestRollBackParked(t *testing.T) {
	pos := map[string]map[string]float64{"sol": {"JUP": 1500}}
	dep := mk("a", "sol", at(2026, 3, 1, 0), core.TxPosition, lg("JUP", -500, nil))
	got := RollBackParked(pos, []core.Tx{dep}, rbCutover, func(string) bool { return false })
	want := []Parked{{Wallet: "sol", Asset: "JUP", Amount: 1000}}
	if len(got) != 1 || got[0].Wallet != "sol" || got[0].Asset != "JUP" {
		t.Fatalf("got %v, want %v", got, want)
	}
	near(t, "amount", got[0].Amount, 1000)

	// Tagged position, failed ignored, negative dropped, sorted.
	tagged := mk("b", "sol", at(2026, 4, 1, 0), core.TxSend, lg("PERP", -20, nil))
	failed := mk("c", "sol", at(2026, 4, 2, 0), core.TxPosition, lg("JUP", -999, nil))
	failed.Status = core.TxFailed
	old := mk("d", "sol", at(2025, 1, 1, 0), core.TxPosition, lg("JUP", -999, nil))
	got = RollBackParked(map[string]map[string]float64{"sol": {"JUP": 1500, "PERP": 10}, "a": {"X": 1}},
		[]core.Tx{dep, tagged, failed, old}, rbCutover, func(id string) bool { return id == tagged.ID })
	// JUP 1000; PERP 10 + -20 = -10 -> dropped; a/X 1.
	if len(got) != 2 || got[0].Wallet != "a" || got[1].Asset != "JUP" {
		t.Fatalf("got %v", got)
	}
	near(t, "jup", got[1].Amount, 1000)
}

func TestRollBackTaggedPosition(t *testing.T) {
	tg := mk("a", "sol", at(2026, 2, 1, 0), core.TxSend, lg("SOL", -5, nil))
	tg.Fee = &core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -0.01}
	got := RollBack(cur(10), []core.Tx{tg}, rbCutover, func(id string) bool { return id == tg.ID })
	near(t, "tagged", got["sol"]["SOL"], 10.01)
	got = RollBack(cur(10), []core.Tx{tg}, rbCutover, nil)
	near(t, "untagged", got["sol"]["SOL"], 15.01)
}
