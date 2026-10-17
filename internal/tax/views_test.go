package tax

import (
	"reflect"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

func TestHoldingExcludesUntracked(t *testing.T) {
	r := Result{Lots: []Lot{
		{Wallet: "sol", Asset: "SOL", Amount: 2, CostUSD: 200},
		{Wallet: Untracked, Asset: "SOL", Amount: 1, CostUSD: 50},
		{Wallet: "sol", Asset: "JUP", Amount: 7, CostUSD: 70},
	}}
	amount, cost, missing := r.Holding("SOL", nil)
	if amount != 2 || cost != 200 || missing != 0 {
		t.Errorf("Holding(SOL, nil) = (%v, %v, %v), want (2, 200, 0)", amount, cost, missing)
	}
	only := func(w string) bool { return w == Untracked }
	if amount, cost, _ := r.Holding("SOL", only); amount != 1 || cost != 50 {
		t.Errorf("Holding(SOL, untracked) = (%v, %v), want (1, 50)", amount, cost)
	}
}

func TestHoldingCountsMissingBasis(t *testing.T) {
	r := Result{Lots: []Lot{
		{Wallet: "sol", Asset: "SOL", Amount: 2, CostUSD: 200},
		{Wallet: "sol", Asset: "SOL", Amount: 1, CostUSD: 30, Flag: FlagMissingBasis},
		{Wallet: "sol", Asset: "SOL", Amount: 0.5, CostUSD: 40, Flag: FlagTransferDate},
	}}
	amount, cost, missing := r.Holding("SOL", nil)
	near(t, "amount", amount, 3.5)
	near(t, "cost", cost, 270)
	near(t, "missing", missing, 1)
}

func TestUnrealizedSkipsMissingBasis(t *testing.T) {
	r := Result{Lots: []Lot{
		{Wallet: "sol", Asset: "SOL", Amount: 2, CostUSD: 200},
		{Wallet: "sol", Asset: "SOL", Amount: 1, CostUSD: 0, Flag: FlagMissingBasis},
	}}
	basis, unrealized, pct, ok := r.Unrealized("SOL", nil, 3, 150)
	if !ok {
		t.Fatal("ok = false")
	}
	near(t, "basis", basis, 200)
	near(t, "unrealized", unrealized, 100)
	near(t, "pct", pct, 50)
}

func TestUnrealizedCoversPartOfTheLots(t *testing.T) {
	// Holding 1 of the 4 units with a basis: a quarter of their cost.
	r := Result{Lots: []Lot{
		{Wallet: "a", Asset: "BTC", Amount: 1, CostUSD: 100},
		{Wallet: "a", Asset: "BTC", Amount: 3, CostUSD: 500},
	}}
	basis, unrealized, pct, ok := r.Unrealized("BTC", nil, 1, 200)
	if !ok {
		t.Fatal("ok = false")
	}
	near(t, "basis", basis, 150)
	near(t, "unrealized", unrealized, 50)
	near(t, "pct", pct, 100.0/3)
	// Holding more than the lots: only the lots' units are covered.
	basis, unrealized, _, _ = r.Unrealized("BTC", nil, 10, 200)
	near(t, "basis (over)", basis, 600)
	near(t, "unrealized (over)", unrealized, 4*200-600)
}

func TestUnrealizedNotOK(t *testing.T) {
	r := Result{Lots: []Lot{
		{Wallet: "sol", Asset: "SOL", Amount: 1, Flag: FlagMissingBasis},
		{Wallet: Untracked, Asset: "BTC", Amount: 1, CostUSD: 100},
		{Wallet: "a", Asset: "ETH", Amount: 1},
	}}
	for _, tc := range []struct {
		name, asset string
		amount      float64
	}{
		{"only missing-basis lots", "SOL", 1},
		{"only untracked lots", "BTC", 1},
		{"no lots", "DOT", 1},
		{"nothing held", "ETH", 0},
	} {
		if _, _, _, ok := r.Unrealized(tc.asset, nil, tc.amount, 100); ok {
			t.Errorf("%s: ok = true", tc.name)
		}
	}
	// A zero-cost lot is a known basis of 0: ok, with no percentage.
	basis, unrealized, pct, ok := r.Unrealized("ETH", nil, 1, 100)
	if !ok || basis != 0 || unrealized != 100 || pct != 0 {
		t.Errorf("zero cost: (%v, %v, %v, %v)", basis, unrealized, pct, ok)
	}
}

func TestRealizedByYear(t *testing.T) {
	r := Result{Disposals: []Disposal{
		{Sold: at(2026, 3, 1, 12), Asset: "SOL", GainUSD: 7},
		{Sold: at(2026, 9, 1, 12), Asset: "BTC", GainUSD: -2},
		{Sold: at(2027, 2, 1, 12), Asset: "SOL", GainUSD: -2},
	}}
	got := r.RealizedByYear(time.UTC)
	if want := map[int]float64{2026: 5, 2027: -2}; !reflect.DeepEqual(got, want) {
		t.Errorf("RealizedByYear = %v, want %v", got, want)
	}
}

func TestRealizedByYearUsesTaxTimezone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// 03:00 UTC on 1 January is still the old year in New York.
	r := Result{Disposals: []Disposal{{Sold: at(2027, 1, 1, 3), GainUSD: 4}}}
	if got := r.RealizedByYear(ny); got[2026] != 4 || len(got) != 1 {
		t.Errorf("New York: %v", got)
	}
	if got := r.RealizedByYear(nil); got[2027] != 4 || len(got) != 1 {
		t.Errorf("nil location is UTC: %v", got)
	}
}

func TestCoverage(t *testing.T) {
	r := Result{Lots: []Lot{
		{Wallet: "a", Asset: "BTC", Amount: 1, CostUSD: 100},
		{Wallet: "a", Asset: "SOL", Amount: 5, CostUSD: 50},
		{Wallet: "a", Asset: "SOL", Amount: 5, Flag: FlagMissingBasis},
		{Wallet: Untracked, Asset: "ETH", Amount: 3, CostUSD: 30},
	}}
	rows := []portfolio.AssetRow{
		{AssetKey: "BTC", Amount: 1, ValueUSD: 400},  // fully covered
		{AssetKey: "SOL", Amount: 10, ValueUSD: 400}, // half: the rest has no basis
		{AssetKey: "ETH", Amount: 1, ValueUSD: 200},  // untracked lots don't count
		{AssetKey: "DUST", Amount: 1, ValueUSD: 0},   // no value: ignored
	}
	near(t, "coverage", Coverage(r, rows), (400+200)/1000.0)
	if got := Coverage(r, nil); got != 0 {
		t.Errorf("no rows: %v", got)
	}
}

// TestViewsOverCompute ties the helpers to a real Compute result.
func TestViewsOverCompute(t *testing.T) {
	res := Compute(Input{
		From:    at(2026, 1, 1, 0),
		Opening: []Lot{{Wallet: "sol", Asset: "SOL", Amount: 10, CostUSD: 1000, Acquired: at(2025, 6, 1, 0)}},
		Txs: []core.Tx{
			mk("s1", "sol", at(2026, 2, 1, 0), core.TxTrade, lg("SOL", -4, usd(-600)), lg("USDC", 600, usd(600))),
		},
	})
	amount, cost, _ := res.Holding("SOL", nil)
	near(t, "amount", amount, 6)
	near(t, "cost", cost, 600)
	if got := res.RealizedByYear(time.UTC); got[2026] != 200 {
		t.Errorf("realized %v", got)
	}
}

// TestMissingBasisSales counts the disposals the lots could not cover.
func TestMissingBasisSales(t *testing.T) {
	res := Compute(Input{
		From:    at(2026, 1, 1, 0),
		Opening: []Lot{{Wallet: "sol", Asset: "SOL", Amount: 1, CostUSD: 100, Acquired: at(2025, 6, 1, 0)}},
		Txs: []core.Tx{
			// 1 SOL has lots; 3 more do not: one covered row, one missing-basis row.
			mk("s1", "sol", at(2026, 2, 1, 0), core.TxTrade, lg("SOL", -4, usd(-600)), lg("USDC", 600, usd(600))),
			// No lots at all for ETH.
			mk("s2", "eth", at(2026, 3, 1, 0), core.TxTrade, lg("ETH", -1, usd(-3000)), lg("USDC", 3000, usd(3000))),
		},
	})
	if got := res.MissingBasisSales(); got != 2 {
		t.Errorf("MissingBasisSales = %d, want 2 (disposals %+v)", got, res.Disposals)
	}
	if got := (Result{}).MissingBasisSales(); got != 0 {
		t.Errorf("empty: %d", got)
	}
}
