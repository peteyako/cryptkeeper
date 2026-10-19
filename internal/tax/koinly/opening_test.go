package koinly

import (
	"cmp"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/tax"
)

const solW = "Wallet A"

// day is a 2025 UTC timestamp, as Koinly's transaction history dates are.
func day(m, d int) time.Time { return time.Date(2025, time.Month(m), d, 15, 0, 0, 0, time.UTC) }

func holding(asset string, q, c float64) Holding {
	sym, name := splitAsset(asset)
	return Holding{Asset: asset, Symbol: sym, Name: name, Quantity: q, CostUSD: c}
}

func deposit(wallet, cur string, amt, cost float64, t time.Time, hash string) Txn {
	return Txn{Time: t, Type: "crypto_deposit", RecvWallet: wallet, RecvAmount: amt,
		RecvCurrency: cur, RecvCost: cost, TxHash: hash}
}

func transfer(from, to, cur string, amt, cost float64, t time.Time) Txn {
	return Txn{Time: t, Type: "transfer", SendWallet: from, SentAmount: amt, SentCurrency: cur,
		SentCost: cost, RecvWallet: to, RecvAmount: amt, RecvCurrency: cur, RecvCost: cost}
}

// solInput is a 2025 import of one SOL holding, Koinly wallet solW mapped to "sol".
func solInput(t *testing.T, q, c float64, bal map[string]map[string]float64, txns ...Txn) OpeningInput {
	return OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings:  []Holding{holding("SOL (Solana)", q, c)},
		Txns:      txns,
		WalletMap: map[string]string{solW: "sol"},
		AssetMap:  map[string]string{"SOL (Solana)": "SOL"},
		Balances:  bal,
	}
}

func before2025(t *testing.T) time.Time  { return time.Date(2024, 12, 31, 23, 59, 59, 0, loadNY(t)) }
func cutover2026(t *testing.T) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, loadNY(t)) }

// approx is false for a NaN got, since every comparison with NaN is.
func approx(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if !approx(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// wantLot checks one lot field by field.
func wantLot(t *testing.T, l tax.Lot, wallet, asset string, amount, cost float64, acquired time.Time, various bool, flag string) {
	t.Helper()
	if l.Wallet != wallet || l.Asset != asset || !approx(l.Amount, amount) || !approx(l.CostUSD, cost) ||
		!l.Acquired.Equal(acquired) || l.Various != various || l.Flag != flag {
		t.Errorf("lot %+v\nwant wallet %s asset %s amount %v cost %v acquired %v various %v flag %q",
			l, wallet, asset, amount, cost, acquired, various, flag)
	}
}

func rowFor(t *testing.T, op Opening, key string) ReconRow {
	t.Helper()
	for _, r := range op.Rows {
		if r.AssetKey == key {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", key, op.Rows)
	return ReconRow{}
}

func sums(lots []tax.Lot) (amount, cost float64) {
	for _, l := range lots {
		amount += l.Amount
		cost += l.CostUSD
	}
	return amount, cost
}

func hasProblem(op Opening, want string) bool {
	return slices.Contains(op.Problems, want)
}

// hasCostProblem reports whether a problem explains why key's lot cost may differ from
// Koinly's cost: a negative-remainder clamp (older lots at $0, or every unit dated), or
// cost with no lots at all to carry it.
// A leftover spread over dated lots is not one: the total must still match.
func hasCostProblem(op Opening, key string) bool {
	for _, p := range op.Problems {
		if strings.HasPrefix(p, key+": ") &&
			(strings.HasSuffix(p, "older lots set to $0") || strings.HasSuffix(p, "dated lots kept at their cost") ||
				strings.HasSuffix(p, "has no lots to carry it")) {
			return true
		}
	}
	return false
}

func byWalletAssetAcquired(a, b tax.Lot) int {
	return cmp.Or(strings.Compare(a.Wallet, b.Wallet), strings.Compare(a.Asset, b.Asset), a.Acquired.Compare(b.Acquired))
}

// checkInvariants verifies, for every row: Σ lot amount == max(Koinly, CryptKeeper);
// Σ lot cost == Koinly cost (+ extra basis when there are missing-basis units) unless a
// cost problem is reported; positive amounts and non-negative costs; the row's
// breakdown adds up; a quantity difference is listed as a problem exactly when it
// exceeds max(dust, 1e-6×Q). Also Opening.Lots is every row's lots in (wallet, asset,
// acquired) order and Rows are by Koinly cost, highest first.
func checkInvariants(t *testing.T, in OpeningInput, op Opening) {
	t.Helper()
	var all []tax.Lot
	for _, r := range op.Rows {
		var untracked float64
		for _, l := range r.Lots {
			if l.Asset != r.AssetKey {
				t.Errorf("%s: lot of %s in its row", r.AssetKey, l.Asset)
			}
			if !(l.Amount > 0) || !(l.CostUSD >= 0) {
				t.Errorf("%s: lot with amount %v cost %v", r.AssetKey, l.Amount, l.CostUSD)
			}
			if l.Wallet == tax.Untracked {
				untracked += l.Amount
			}
		}
		amount, cost := sums(r.Lots)
		near(t, r.AssetKey+" Σ amount", amount, math.Max(r.KoinlyQty, r.CryptKeeperQty))
		near(t, r.AssetKey+" dated+various+missing", r.Dated+r.Various+r.MissingBasis, amount)
		near(t, r.AssetKey+" untracked", r.Untracked, untracked)
		want := r.KoinlyCost
		if r.MissingBasis > 0 {
			want += in.ExtraBasis[r.AssetKey]
		}
		if !hasCostProblem(op, r.AssetKey) {
			near(t, r.AssetKey+" Σ cost", cost, want)
		}
		differs := math.Abs(r.KoinlyQty-r.CryptKeeperQty) > math.Max(1e-12, 1e-6*r.KoinlyQty)
		listed := false
		for _, p := range op.Problems {
			listed = listed || strings.HasPrefix(p, r.AssetKey+": Koinly ")
		}
		if differs != listed {
			t.Errorf("%s: quantity differs %v, listed %v (%q)", r.AssetKey, differs, listed, op.Problems)
		}
		if !slices.IsSortedFunc(r.Lots, byWalletAssetAcquired) {
			t.Errorf("%s: row lots not sorted", r.AssetKey)
		}
		all = append(all, r.Lots...)
	}
	slices.SortStableFunc(all, byWalletAssetAcquired)
	if !reflect.DeepEqual(all, op.Lots) && !(len(all) == 0 && len(op.Lots) == 0) {
		t.Errorf("Opening.Lots is not the rows' lots in order:\n got %+v\nwant %+v", op.Lots, all)
	}
	if !slices.IsSortedFunc(op.Rows, func(a, b ReconRow) int { return cmp.Compare(b.KoinlyCost, a.KoinlyCost) }) {
		t.Errorf("rows not sorted by Koinly cost descending")
	}
}

func TestOpeningBackfillNewestFirst(t *testing.T) {
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10}},
		deposit(solW, "SOL", 3, 450, day(6, 3), ""),
		Txn{Time: day(9, 4), Type: "exchange", SendWallet: solW, SentAmount: 800, SentCurrency: "USDC",
			RecvWallet: solW, RecvAmount: 4, RecvCurrency: "SOL", RecvCost: 800, TxHash: "FakeHash2"},
		// Not acquisitions of SOL: a withdrawal and another currency.
		Txn{Time: day(10, 1), Type: "crypto_withdrawal", SendWallet: solW, SentAmount: 1, SentCurrency: "SOL"},
		deposit(solW, "USDC", 50, 50, day(11, 1), "FakeHash3"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 3 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 3, 250, before2025(t), true, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 3, 450, day(6, 3), false, "")
	wantLot(t, op.Lots[2], "sol", "SOL", 4, 800, day(9, 4), false, "")
	if op.Lots[1].TxID != "koinly:row1" || op.Lots[2].TxID != "koinly:FakeHash2" {
		t.Errorf("tx ids %q %q", op.Lots[1].TxID, op.Lots[2].TxID)
	}
	amount, cost := sums(op.Lots)
	near(t, "Σ amount", amount, 10)
	near(t, "Σ cost", cost, 1500)
	r := rowFor(t, op, "SOL")
	if r.KoinlyAsset != "SOL (Solana)" || r.KoinlyQty != 10 || r.KoinlyCost != 1500 || r.CryptKeeperQty != 10 {
		t.Errorf("row %+v", r)
	}
	near(t, "dated", r.Dated, 7)
	near(t, "various", r.Various, 3)
	if r.Untracked != 0 || r.MissingBasis != 0 {
		t.Errorf("untracked %v missing %v", r.Untracked, r.MissingBasis)
	}
	if len(op.Problems) != 0 {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningPartialRowAndLeftoverSpread(t *testing.T) {
	// The oldest row is only half needed and the rows date all 10 units, so the $450 of
	// Koinly's cost they don't carry is spread over them by cost (×2000/1550).
	in := solInput(t, 10, 2000, map[string]map[string]float64{"sol": {"SOL": 10}},
		deposit(solW, "SOL", 6, 600, day(3, 1), "FakeHash4"),
		deposit(solW, "SOL", 3, 450, day(6, 3), "FakeHash5"),
		deposit(solW, "SOL", 4, 800, day(9, 4), "FakeHash2"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 3 {
		t.Fatalf("lots %+v", op.Lots)
	}
	f := 2000.0 / 1550
	wantLot(t, op.Lots[0], "sol", "SOL", 3, 300*f, day(3, 1), false, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 3, 450*f, day(6, 3), false, "")
	wantLot(t, op.Lots[2], "sol", "SOL", 4, 800*f, day(9, 4), false, "")
	if !hasProblem(op, "SOL: Koinly's cost exceeds the dated 2025 lots by $450.00; spread across them") {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningLeftoverSpreadByCost(t *testing.T) {
	// Two dated lots costing 300 and 100 cover everything; Koinly's cost is 600.
	in := solInput(t, 4, 600, map[string]map[string]float64{"sol": {"SOL": 4}},
		deposit(solW, "SOL", 1, 300, day(4, 1), "FakeHash15"),
		deposit(solW, "SOL", 3, 100, day(8, 1), "FakeHash16"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 1, 450, day(4, 1), false, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 3, 150, day(8, 1), false, "")
	if !hasProblem(op, "SOL: Koinly's cost exceeds the dated 2025 lots by $200.00; spread across them") {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningLeftoverSpreadByAmountWhenDatedCostZero(t *testing.T) {
	in := solInput(t, 4, 100, map[string]map[string]float64{"sol": {"SOL": 4}},
		deposit(solW, "SOL", 1, 0, day(4, 1), "FakeHash17"),
		deposit(solW, "SOL", 3, 0, day(8, 1), "FakeHash18"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 1, 25, day(4, 1), false, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 3, 75, day(8, 1), false, "")
}

func TestOpeningLeftoverNotSpreadOverMissingBasis(t *testing.T) {
	// Koinly's 2 units are both dated; CryptKeeper's extra unit has no Koinly basis and
	// must stay at its ExtraBasis ($30), so the $100 leftover goes to the dated lot only.
	// The user entered that basis, so the lot is not flagged missing-basis.
	in := solInput(t, 2, 300, map[string]map[string]float64{"sol": {"SOL": 3}},
		deposit(solW, "SOL", 2, 200, day(9, 1), "FakeHash19"))
	in.ExtraBasis = map[string]float64{"SOL": 30}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 2, 300, day(9, 1), false, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 1, 30, cutover2026(t), false, "")
}

func TestOpeningCostWithNoLots(t *testing.T) {
	// Koinly reports cost on a zero quantity: there is no lot to put it on.
	in := solInput(t, 0, 50, nil)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 0 || !hasProblem(op, "SOL: $50.00 of Koinly's cost has no lots to carry it") {
		t.Errorf("lots %+v problems %q", op.Lots, op.Problems)
	}
}

func TestOpeningUntrackedExcess(t *testing.T) {
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings:  []Holding{holding("ETH (Ethereum)", 1.5, 2400)},
		WalletMap: map[string]string{solW: "sol"},
		AssetMap:  map[string]string{"ETH (Ethereum)": "ETH"},
		Balances:  map[string]map[string]float64{"sol": {"SOL": 5}},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 1 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], tax.Untracked, "ETH", 1.5, 2400, before2025(t), true, "")
	r := rowFor(t, op, "ETH")
	if r.CryptKeeperQty != 0 || r.Untracked != 1.5 || r.Various != 1.5 {
		t.Errorf("row %+v", r)
	}
	if !hasProblem(op, "ETH: Koinly 1.5 vs CryptKeeper 0") {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningAllUntracked(t *testing.T) {
	// The mapped wallet holds no USDC at the cutover, so all 100 go to untracked; its
	// 2025 USDC deposit then dates 60 of them. kraken rolls back below zero, which counts
	// as 0.
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings:  []Holding{holding("USDC (USD Coin)", 100, 100)},
		Txns:      []Txn{deposit(solW, "USDC", 60, 60, day(11, 1), "FakeHash11")},
		WalletMap: map[string]string{solW: "sol", "Wallet K": "kraken"},
		AssetMap:  map[string]string{"USDC (USD Coin)": "USDC"},
		Balances:  map[string]map[string]float64{"sol": {"SOL": 3}, "kraken": {"USDC": -2}},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], tax.Untracked, "USDC", 40, 40, before2025(t), true, "")
	wantLot(t, op.Lots[1], tax.Untracked, "USDC", 60, 60, day(11, 1), false, "")
	if r := rowFor(t, op, "USDC"); r.CryptKeeperQty != 0 || r.Untracked != 100 || r.Dated != 60 {
		t.Errorf("row %+v", r)
	}
	for _, want := range []string{"USDC: kraken rolls back to -2; treated as 0", "USDC: Koinly 100 vs CryptKeeper 0"} {
		if !hasProblem(op, want) {
			t.Errorf("missing problem %q in %q", want, op.Problems)
		}
	}
}

func TestOpeningUntrackedBackfill(t *testing.T) {
	// A Koinly wallet with no mapping is untracked, so its acquisitions date untracked units.
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings:  []Holding{holding("ETH (Ethereum)", 2, 3000)},
		Txns:      []Txn{deposit("Wallet Old", "ETH", 0.5, 1000, day(7, 1), "")},
		WalletMap: map[string]string{solW: "sol"},
		AssetMap:  map[string]string{"ETH (Ethereum)": "ETH"},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], tax.Untracked, "ETH", 1.5, 2000, before2025(t), true, "")
	wantLot(t, op.Lots[1], tax.Untracked, "ETH", 0.5, 1000, day(7, 1), false, "")
}

func TestOpeningMissingBasisExcess(t *testing.T) {
	in := solInput(t, 100, 20000, map[string]map[string]float64{"sol1": {"SOL": 60}, "sol2": {"SOL": 41.5}})
	in.WalletMap = map[string]string{solW: "sol1"}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 4 {
		t.Fatalf("lots %+v", op.Lots)
	}
	m1, m2 := 1.5*60/101.5, 1.5*41.5/101.5
	v1, v2 := 60-m1, 41.5-m2
	wantLot(t, op.Lots[0], "sol1", "SOL", v1, 20000*v1/100, before2025(t), true, "")
	wantLot(t, op.Lots[1], "sol1", "SOL", m1, 0, cutover2026(t), false, tax.FlagMissingBasis)
	wantLot(t, op.Lots[2], "sol2", "SOL", v2, 20000*v2/100, before2025(t), true, "")
	wantLot(t, op.Lots[3], "sol2", "SOL", m2, 0, cutover2026(t), false, tax.FlagMissingBasis)
	r := rowFor(t, op, "SOL")
	near(t, "missing basis", r.MissingBasis, 1.5)
	near(t, "ck", r.CryptKeeperQty, 101.5)
	_, cost := sums(op.Lots)
	near(t, "Σ cost", cost, 20000)
	if !hasProblem(op, "SOL: Koinly 100 vs CryptKeeper 101.5") {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningMissingBasisExtraBasis(t *testing.T) {
	in := solInput(t, 100, 20000, map[string]map[string]float64{"sol1": {"SOL": 60}, "sol2": {"SOL": 41.5}})
	in.ExtraBasis = map[string]float64{"SOL": 150}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 4 {
		t.Fatalf("lots %+v", op.Lots)
	}
	near(t, "sol1 missing cost", op.Lots[1].CostUSD, 150*60/101.5)
	near(t, "sol2 missing cost", op.Lots[3].CostUSD, 150*41.5/101.5)
	_, cost := sums(op.Lots)
	near(t, "Σ cost", cost, 20150)
}

// TestOpeningExtraBasisClearsMissingFlag: a basis the user entered for CryptKeeper's extra
// units is a real basis, so those lots are not flagged missing-basis; with no entry (0)
// they keep the flag.
func TestOpeningExtraBasisClearsMissingFlag(t *testing.T) {
	bal := map[string]map[string]float64{"sol1": {"SOL": 60}, "sol2": {"SOL": 41.5}}
	in := solInput(t, 100, 20000, bal)
	in.ExtraBasis = map[string]float64{"SOL": 150}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 4 {
		t.Fatalf("lots %+v", op.Lots)
	}
	m1, m2 := 1.5*60/101.5, 1.5*41.5/101.5
	wantLot(t, op.Lots[1], "sol1", "SOL", m1, 150*m1/1.5, cutover2026(t), false, "")
	wantLot(t, op.Lots[3], "sol2", "SOL", m2, 150*m2/1.5, cutover2026(t), false, "")
	near(t, "row missing basis", rowFor(t, op, "SOL").MissingBasis, 1.5)

	in.ExtraBasis = map[string]float64{"SOL": 0}
	op = BuildOpening(in)
	wantLot(t, op.Lots[1], "sol1", "SOL", m1, 0, cutover2026(t), false, tax.FlagMissingBasis)
	wantLot(t, op.Lots[3], "sol2", "SOL", m2, 0, cutover2026(t), false, tax.FlagMissingBasis)
}

func TestOpeningMissingBasisAfterBackfill(t *testing.T) {
	// sol1 has 6 dated units, so the 2 extra units spread over the undated
	// remainders 2 (sol1) and 4 (sol2).
	in := solInput(t, 10, 1000, map[string]map[string]float64{"sol1": {"SOL": 8}, "sol2": {"SOL": 4}},
		deposit(solW, "SOL", 6, 900, day(11, 1), "FakeHash12"))
	in.WalletMap = map[string]string{solW: "sol1"}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 5 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol1", "SOL", 4.0/3, 100.0/3, before2025(t), true, "")
	wantLot(t, op.Lots[1], "sol1", "SOL", 6, 900, day(11, 1), false, "")
	wantLot(t, op.Lots[2], "sol1", "SOL", 2.0/3, 0, cutover2026(t), false, tax.FlagMissingBasis)
	wantLot(t, op.Lots[3], "sol2", "SOL", 8.0/3, 200.0/3, before2025(t), true, "")
	wantLot(t, op.Lots[4], "sol2", "SOL", 4.0/3, 0, cutover2026(t), false, tax.FlagMissingBasis)
}

func TestOpeningDatedCappedAtKoinlyQty(t *testing.T) {
	// CryptKeeper holds 8, Koinly 5: dating stops at 5 units, taking the newest rows across
	// wallets first (sol2's November row before sol1's October row).
	in := solInput(t, 5, 475, map[string]map[string]float64{"sol1": {"SOL": 4}, "sol2": {"SOL": 4}},
		deposit("Wallet B", "SOL", 4, 400, day(11, 1), "FakeHash7"),
		deposit("Wallet C", "SOL", 4, 300, day(10, 1), "FakeHash8"),
	)
	in.WalletMap = map[string]string{"Wallet C": "sol1", "Wallet B": "sol2"}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 3 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol1", "SOL", 1, 75, day(10, 1), false, "")
	wantLot(t, op.Lots[1], "sol1", "SOL", 3, 0, cutover2026(t), false, tax.FlagMissingBasis)
	wantLot(t, op.Lots[2], "sol2", "SOL", 4, 400, day(11, 1), false, "")
	if r := rowFor(t, op, "SOL"); r.Dated != 5 || r.Various != 0 || r.MissingBasis != 3 {
		t.Errorf("row %+v", r)
	}
	if !hasProblem(op, "SOL: Koinly 5 vs CryptKeeper 8") || hasCostProblem(op, "SOL") {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningNegativeRemainderClamped(t *testing.T) {
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10}},
		deposit(solW, "SOL", 3, 800, day(6, 3), "FakeHash5"),
		deposit(solW, "SOL", 4, 1200, day(9, 4), "FakeHash2"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 3 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 3, 0, before2025(t), true, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 3, 800, day(6, 3), false, "")
	wantLot(t, op.Lots[2], "sol", "SOL", 4, 1200, day(9, 4), false, "")
	_, cost := sums(op.Lots)
	near(t, "Σ cost", cost, 2000)
	if !hasProblem(op, "SOL: dated 2025 cost exceeds Koinly's total cost; older lots set to $0") {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningTransferFlag(t *testing.T) {
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10}},
		deposit(solW, "SOL", 2, 300, day(5, 1), "FakeHash6"),
		transfer("Wallet L", solW, "SOL", 4, 600, day(8, 1)),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 3 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 4, 600, before2025(t), true, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 2, 300, day(5, 1), false, "")
	wantLot(t, op.Lots[2], "sol", "SOL", 4, 600, day(8, 1), false, tax.FlagTransferDate)
}

func TestOpeningSameWalletTransferSkipped(t *testing.T) {
	// Transfers between Koinly wallets that map to the same CryptKeeper wallet (including a
	// wallet name to itself, which Koinly exports can contain) bring no units into it.
	in := solInput(t, 5, 500, map[string]map[string]float64{"sol": {"SOL": 5}},
		deposit(solW, "SOL", 5, 500, day(3, 1), "FakeHash4"),
		transfer(solW, "Wallet A2", "SOL", 5, 500, day(6, 1)),
		transfer(solW, solW, "SOL", 5, 500, day(7, 1)),
	)
	in.WalletMap["Wallet A2"] = "sol"
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 1 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 5, 500, day(3, 1), false, "")
}

func TestOpeningSymbolCollision(t *testing.T) {
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings: []Holding{
			holding("JUP (Some Clone)", 2.5, 0),
			holding("JUP (Jupiter)", 100, 5000),
		},
		Txns:      []Txn{deposit(solW, "JUP", 40, 2000, day(8, 1), "FakeHash9")},
		WalletMap: map[string]string{solW: "sol"},
		AssetMap:  map[string]string{"JUP (Jupiter)": "JUP", "JUP (Some Clone)": "spl:X"},
		Balances:  map[string]map[string]float64{"sol": {"JUP": 100, "spl:X": 2.5}},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Rows) != 2 || op.Rows[0].AssetKey != "JUP" || op.Rows[1].AssetKey != "spl:X" {
		t.Fatalf("rows %+v", op.Rows)
	}
	jup, x := op.Rows[0], op.Rows[1]
	if jup.KoinlyAsset != "JUP (Jupiter)" || x.KoinlyAsset != "JUP (Some Clone)" {
		t.Errorf("koinly assets %q %q", jup.KoinlyAsset, x.KoinlyAsset)
	}
	if jup.Dated != 40 || jup.Various != 60 || x.Dated != 0 || x.Various != 2.5 {
		t.Errorf("JUP dated %v various %v; spl:X dated %v various %v", jup.Dated, jup.Various, x.Dated, x.Various)
	}
	wantLot(t, jup.Lots[0], "sol", "JUP", 60, 3000, before2025(t), true, "")
	wantLot(t, jup.Lots[1], "sol", "JUP", 40, 2000, day(8, 1), false, "")
	if !hasProblem(op, "spl:X: shares symbol JUP with JUP; its 2025 rows date JUP") {
		t.Errorf("problems %q", op.Problems)
	}
	for _, p := range op.Problems {
		if strings.HasPrefix(p, "JUP: shares") {
			t.Errorf("winning key reported: %q", p)
		}
	}
}

func TestOpeningSharedSymbolThreeKeys(t *testing.T) {
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings: []Holding{
			holding("FOO (Foo Token)", 1, 10), holding("FOO (Foo Clone)", 1, 30), holding("FOO (Foo Fake)", 1, 0),
		},
		AssetMap: map[string]string{"FOO (Foo Token)": "spl:A", "FOO (Foo Clone)": "spl:B", "FOO (Foo Fake)": "spl:C"},
		Balances: map[string]map[string]float64{"sol": {"spl:A": 1, "spl:B": 1, "spl:C": 1}},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	for _, want := range []string{
		"spl:A: shares symbol FOO with spl:B, spl:C; its 2025 rows date spl:B",
		"spl:C: shares symbol FOO with spl:A, spl:B; its 2025 rows date spl:B",
	} {
		if !hasProblem(op, want) {
			t.Errorf("missing problem %q in %q", want, op.Problems)
		}
	}
	if len(op.Problems) != 2 {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningSkippedAsset(t *testing.T) {
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10, "spl:N": 1}},
		deposit(solW, "NFTX#1", 1, 0, day(4, 1), "FakeHash10"))
	in.Holdings = append(in.Holdings, holding("NFTX#1 (Test NFT)", 1, 0))
	in.AssetMap["NFTX#1 (Test NFT)"] = ""
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Rows) != 1 || op.Rows[0].AssetKey != "SOL" {
		t.Fatalf("rows %+v", op.Rows)
	}
	for _, l := range op.Lots {
		if l.Asset != "SOL" {
			t.Errorf("lot for skipped asset: %+v", l)
		}
	}
	if len(op.Problems) != 0 {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningUnmappedHoldingReported(t *testing.T) {
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10}})
	in.Holdings = append(in.Holdings, holding("FOO (Foo Token)", 5, 50))
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Rows) != 1 || !hasProblem(op, "FOO (Foo Token): no asset mapping; skipped") {
		t.Errorf("rows %+v problems %q", op.Rows, op.Problems)
	}
}

func TestOpeningDuplicateHoldingsGrouped(t *testing.T) {
	// Koinly lists the same "SYM (Name)" more than once; they share one asset key.
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings: []Holding{
			holding("SPAM#7 (Spam Collection)", 1, 0),
			holding("SPAM#7 (Spam Collection)", 1, 5),
		},
		AssetMap: map[string]string{"SPAM#7 (Spam Collection)": "spl:G"},
		Balances: map[string]map[string]float64{"sol": {"spl:G": 2}},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Rows) != 1 {
		t.Fatalf("rows %+v", op.Rows)
	}
	r := op.Rows[0]
	if r.KoinlyAsset != "SPAM#7 (Spam Collection)" || r.KoinlyQty != 2 || r.KoinlyCost != 5 || r.CryptKeeperQty != 2 {
		t.Errorf("row %+v", r)
	}
	if len(op.Problems) != 0 {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningVariousDate(t *testing.T) {
	ny := loadNY(t)
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10}})
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 1 {
		t.Fatalf("lots %+v", op.Lots)
	}
	l := op.Lots[0]
	if !l.Various || !l.Acquired.Equal(time.Date(2024, 12, 31, 23, 59, 59, 0, ny)) {
		t.Errorf("lot %+v", l)
	}
	if got := l.Acquired.In(ny).Format(time.DateTime); got != "2024-12-31 23:59:59" {
		t.Errorf("acquired in loc %s", got)
	}
}

func TestOpeningIgnoresRowsAfterCutover(t *testing.T) {
	// 03:00 UTC on Jan 1 is still Dec 31 in New York; 06:00 UTC is past the cutover.
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10}},
		deposit(solW, "SOL", 2, 400, time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), "in"),
		deposit(solW, "SOL", 3, 600, time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC), "out"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 8, 1100, before2025(t), true, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 2, 400, time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), false, "")
}

func TestOpeningParkedPassThrough(t *testing.T) {
	in := solInput(t, 10, 1500, map[string]map[string]float64{"sol": {"SOL": 10}})
	in.Parked = []tax.Parked{{Wallet: "sol", Asset: "JUP", Amount: 5}, {Wallet: "kraken", Asset: "SOL", Amount: 1}, {Wallet: "sol", Asset: "FOO", Amount: 2}}
	orig := slices.Clone(in.Parked)
	op := BuildOpening(in)
	want := []tax.Parked{{Wallet: "kraken", Asset: "SOL", Amount: 1}, {Wallet: "sol", Asset: "FOO", Amount: 2}, {Wallet: "sol", Asset: "JUP", Amount: 5}}
	if !reflect.DeepEqual(op.Parked, want) {
		t.Errorf("parked %+v", op.Parked)
	}
	if !reflect.DeepEqual(in.Parked, orig) {
		t.Errorf("input parked reordered: %+v", in.Parked)
	}
}

func TestOpeningDeterministic(t *testing.T) {
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings: []Holding{
			holding("SOL (Solana)", 10, 1000), holding("JUP (Jupiter)", 50, 500),
			holding("ETH (Ethereum)", 1, 2000), holding("USDC (USD Coin)", 7, 7),
		},
		Txns: []Txn{
			deposit("Wallet A", "SOL", 3, 300, day(5, 1), ""), deposit("Wallet B", "SOL", 3, 360, day(5, 1), ""),
			deposit("Wallet C", "JUP", 10, 100, day(7, 1), "FakeHash13"), deposit("Wallet Old", "ETH", 0.25, 600, day(2, 1), "FakeHash14"),
		},
		WalletMap:  map[string]string{"Wallet A": "sol1", "Wallet B": "sol2", "Wallet C": "sol3", "Wallet Old": tax.Untracked},
		AssetMap:   map[string]string{"SOL (Solana)": "SOL", "JUP (Jupiter)": "JUP", "ETH (Ethereum)": "ETH", "USDC (USD Coin)": "USDC"},
		Balances:   map[string]map[string]float64{"sol1": {"SOL": 5, "JUP": 20}, "sol2": {"SOL": 4, "USDC": 7}, "sol3": {"SOL": 3, "JUP": 20}},
		ExtraBasis: map[string]float64{"SOL": 40},
	}
	first := BuildOpening(in)
	checkInvariants(t, in, first)
	for range 30 {
		if got := BuildOpening(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("result differs between runs:\n%+v\n%+v", got, first)
		}
	}
}

func TestOpeningBuyRowDates(t *testing.T) {
	// Koinly's "buy" (fiat → crypto): the received side is an acquisition.
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings: []Holding{holding("BTC (Bitcoin)", 1, 50000)},
		Txns: []Txn{{Time: day(10, 1), Type: "buy", SendWallet: "Wallet A", SentAmount: 30000, SentCurrency: "USD",
			RecvWallet: "Wallet A", RecvAmount: 0.6, RecvCurrency: "BTC", RecvCost: 30000, TxHash: "FakeHash20"}},
		WalletMap: map[string]string{"Wallet A": "btc"},
		AssetMap:  map[string]string{"BTC (Bitcoin)": "BTC"},
		Balances:  map[string]map[string]float64{"btc": {"BTC": 1}},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "btc", "BTC", 0.4, 20000, before2025(t), true, "")
	wantLot(t, op.Lots[1], "btc", "BTC", 0.6, 30000, day(10, 1), false, "")
	if op.Lots[1].TxID != "koinly:FakeHash20" {
		t.Errorf("tx id %q", op.Lots[1].TxID)
	}
}

func TestOpeningUntrackedFromZeroQtyWallet(t *testing.T) {
	// Koinly holds 2 ETH; the mapped ledger holds none in CryptKeeper, so both are
	// untracked. The ledger's November buy most likely bought them: it dates 1.5.
	in := OpeningInput{
		Year: 2025, Loc: loadNY(t),
		Holdings: []Holding{holding("ETH (Ethereum)", 2, 3000)},
		Txns: []Txn{{Time: day(11, 10), Type: "buy", SendWallet: "Wallet L", SentAmount: 2400, SentCurrency: "USD",
			RecvWallet: "Wallet L", RecvAmount: 1.5, RecvCurrency: "ETH", RecvCost: 2400, TxHash: "FakeHash21"}},
		WalletMap: map[string]string{"Wallet L": "ledger"},
		AssetMap:  map[string]string{"ETH (Ethereum)": "ETH"},
		Balances:  map[string]map[string]float64{"ledger": {"BTC": 0.1}},
	}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], tax.Untracked, "ETH", 0.5, 600, before2025(t), true, "")
	wantLot(t, op.Lots[1], tax.Untracked, "ETH", 1.5, 2400, day(11, 10), false, "")
	if r := rowFor(t, op, "ETH"); r.Untracked != 2 || r.Dated != 1.5 || r.Various != 0.5 {
		t.Errorf("row %+v", r)
	}
}

func TestOpeningUntrackedFromLeftoverRows(t *testing.T) {
	// sol holds 3 of Koinly's 5; 2 are untracked. Untracked first takes its own wallet's
	// row (Wallet X, unmapped), then the unused 1 of sol's October row, then Various.
	in := solInput(t, 5, 600, map[string]map[string]float64{"sol": {"SOL": 3}},
		deposit(solW, "SOL", 4, 400, day(10, 1), "FakeHash22"),
		deposit("Wallet X", "SOL", 0.5, 50, day(9, 1), "FakeHash23"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 4 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 3, 300, day(10, 1), false, "")
	wantLot(t, op.Lots[1], tax.Untracked, "SOL", 0.5, 150, before2025(t), true, "")
	wantLot(t, op.Lots[2], tax.Untracked, "SOL", 0.5, 50, day(9, 1), false, "")
	wantLot(t, op.Lots[3], tax.Untracked, "SOL", 1, 100, day(10, 1), false, "")
	if op.Lots[3].TxID != "koinly:FakeHash22" {
		t.Errorf("tx id %q", op.Lots[3].TxID)
	}
}

func TestOpeningAllDatedOverCost(t *testing.T) {
	// Every unit is dated and the dated cost (400) exceeds Koinly's (300): there are no
	// older lots to zero, so the dated lots keep their cost.
	in := solInput(t, 4, 300, map[string]map[string]float64{"sol": {"SOL": 4}},
		deposit(solW, "SOL", 1, 300, day(4, 1), "FakeHash24"),
		deposit(solW, "SOL", 3, 100, day(8, 1), "FakeHash25"),
	)
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	if len(op.Lots) != 2 {
		t.Fatalf("lots %+v", op.Lots)
	}
	wantLot(t, op.Lots[0], "sol", "SOL", 1, 300, day(4, 1), false, "")
	wantLot(t, op.Lots[1], "sol", "SOL", 3, 100, day(8, 1), false, "")
	if !hasProblem(op, "SOL: dated 2025 cost exceeds Koinly's total cost; dated lots kept at their cost") {
		t.Errorf("problems %q", op.Problems)
	}
}

func TestOpeningMissingBasisSkipsTinyRemainder(t *testing.T) {
	// sol1's back-fill leaves 1e-10 undated, below tolerance (1e-8 for Q=10): the extra
	// unit must all land in sol2 rather than leave a sliver lot in sol1.
	in := solInput(t, 10, 1000, map[string]map[string]float64{"sol1": {"SOL": 5 + 1e-10}, "sol2": {"SOL": 6}},
		deposit(solW, "SOL", 5, 500, day(11, 1), "FakeHash26"))
	in.WalletMap = map[string]string{solW: "sol1"}
	op := BuildOpening(in)
	checkInvariants(t, in, op)
	for _, l := range op.Lots {
		if l.Amount < 1e-6 {
			t.Errorf("sliver lot %+v", l)
		}
	}
	if len(op.Lots) != 3 {
		t.Fatalf("lots %+v", op.Lots)
	}
	excess := 5 + 1e-10 + 6 - 10
	wantLot(t, op.Lots[0], "sol1", "SOL", 5, 500, day(11, 1), false, "")
	wantLot(t, op.Lots[1], "sol2", "SOL", 6-excess, 500, before2025(t), true, "")
	wantLot(t, op.Lots[2], "sol2", "SOL", excess, 0, cutover2026(t), false, tax.FlagMissingBasis)
}
