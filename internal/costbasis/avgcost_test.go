package costbasis

import (
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

func TestBuyBuySellAverageCostAndRealizedPerYear(t *testing.T) {
	txs := []core.Tx{
		mk("b1", day(2021, 1, 10), core.TxTrade, lg("BTC", 1, usd(10000))),
		mk("b2", day(2021, 6, 1), core.TxTrade, lg("BTC", 1, usd(20000))),
		mk("s1", day(2022, 3, 1), core.TxTrade, lg("BTC", -0.5, usd(-9000))),
		mk("s2", day(2023, 3, 1), core.TxTrade, lg("BTC", -0.5, usd(-6000))),
	}
	btc := Compute(txs, Manual{}).Assets["BTC"]
	near(t, "amount", btc.Amount, 1)
	near(t, "basis", btc.Basis, 15000)
	near(t, "avg", btc.AvgCost, 15000)
	near(t, "realized 2022", btc.RealizedByYear[2022], 1500)
	near(t, "realized 2023", btc.RealizedByYear[2023], -1500)
	if btc.AssetKey != "BTC" || btc.Overridden || btc.UnknownBasisUnits != 0 {
		t.Errorf("unexpected result %+v", btc)
	}
}

func TestInputOrderDoesNotMatter(t *testing.T) {
	txs := []core.Tx{
		mk("s1", day(2022, 3, 1), core.TxTrade, lg("BTC", -0.5, usd(-9000))),
		mk("b2", day(2021, 6, 1), core.TxTrade, lg("BTC", 1, usd(20000))),
		mk("b1", day(2021, 1, 10), core.TxTrade, lg("BTC", 1, usd(10000))),
	}
	btc := Compute(txs, Manual{}).Assets["BTC"]
	near(t, "realized 2022", btc.RealizedByYear[2022], 1500)
	near(t, "basis", btc.Basis, 22500)
}

func TestRealizedYearIsUTC(t *testing.T) {
	// 2021-12-31 20:00 in UTC-5 is already 2022-01-01 01:00 UTC.
	local := time.Date(2021, 12, 31, 20, 0, 0, 0, time.FixedZone("EST", -5*3600))
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("BTC", 1, usd(100))),
		mk("s", local, core.TxTrade, lg("BTC", -1, usd(-150))),
	}
	btc := Compute(txs, Manual{}).Assets["BTC"]
	near(t, "realized 2022", btc.RealizedByYear[2022], 50)
	if _, ok := btc.RealizedByYear[2021]; ok {
		t.Error("year bucket used the local zone instead of UTC")
	}
}

func TestSellMoreThanKnownUnitsConsumesUnknownAfter(t *testing.T) {
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("BTC", 1, usd(10000))),
		mk("r", day(2021, 2, 1), core.TxReceive, lg("BTC", 1, nil)),
		mk("s", day(2022, 1, 1), core.TxTrade, lg("BTC", -1.5, usd(-30000))),
	}
	btc := Compute(txs, Manual{}).Assets["BTC"]
	// 1 known unit sold for 30000*(1/1.5) = 20000 against a 10000 basis.
	near(t, "realized", btc.RealizedByYear[2022], 10000)
	near(t, "amount", btc.Amount, 0)
	near(t, "basis", btc.Basis, 0)
	near(t, "unknown", btc.UnknownBasisUnits, 0.5)

	// Selling the remaining unknown units realizes nothing and clamps at zero.
	txs = append(txs, mk("s2", day(2022, 2, 1), core.TxTrade, lg("BTC", -0.75, usd(-9000))))
	btc = Compute(txs, Manual{}).Assets["BTC"]
	near(t, "realized unchanged", btc.RealizedByYear[2022], 10000)
	near(t, "unknown clamped", btc.UnknownBasisUnits, 0)
}

func TestReceiveWithoutValueIsUnknownBasisAndIgnoresUSD(t *testing.T) {
	txs := []core.Tx{
		mk("r1", day(2021, 1, 1), core.TxReceive, lg("BTC", 0.3, nil)),
		// A valuation step may attach a fiat value to a receive; it is display-only,
		// the basis stays unknown until the user sets it.
		mk("r2", day(2021, 2, 1), core.TxReceive, lg("BTC", 0.2, usd(9000))),
	}
	btc := Compute(txs, Manual{}).Assets["BTC"]
	near(t, "amount", btc.Amount, 0)
	near(t, "basis", btc.Basis, 0)
	near(t, "unknown", btc.UnknownBasisUnits, 0.5)
}

func TestSwapRealizesDisposedAssetAndSetsBasisOnReceived(t *testing.T) {
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("ETH", 2, usd(4000))),
		// Only the received leg is valued: it is also the proceeds of the ETH leg.
		mk("sw", day(2022, 1, 1), core.TxSwap, lg("ETH", -1, nil), lg("USDC", 3000, usd(3000))),
	}
	res := Compute(txs, Manual{})
	eth, usdc := res.Assets["ETH"], res.Assets["USDC"]
	near(t, "eth realized", eth.RealizedByYear[2022], 1000)
	near(t, "eth amount", eth.Amount, 1)
	near(t, "eth basis", eth.Basis, 2000)
	near(t, "usdc amount", usdc.Amount, 3000)
	near(t, "usdc basis", usdc.Basis, 3000)
}

func TestSwapValuedOnDisposedLegOnly(t *testing.T) {
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("ETH", 1, usd(2000))),
		mk("sw", day(2022, 1, 1), core.TxSwap, lg("ETH", -1, usd(-3000)), lg("SOL", 20, nil)),
	}
	res := Compute(txs, Manual{})
	near(t, "eth realized", res.Assets["ETH"].RealizedByYear[2022], 1000)
	near(t, "sol basis", res.Assets["SOL"].Basis, 3000)
	near(t, "sol amount", res.Assets["SOL"].Amount, 20)
}

func TestSwapStablecoinLegIsPegged(t *testing.T) {
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("ETH", 1, usd(2000))),
		mk("sw", day(2022, 1, 1), core.TxSwap, lg("ETH", -1, nil), lg("USDC", 2500, nil)),
	}
	res := Compute(txs, Manual{})
	near(t, "eth realized", res.Assets["ETH"].RealizedByYear[2022], 500)
	near(t, "usdc basis", res.Assets["USDC"].Basis, 2500)
}

func TestSwapWithNoValuesLeavesUnknownBasis(t *testing.T) {
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("ETH", 1, usd(2000))),
		mk("sw", day(2022, 1, 1), core.TxSwap, lg("ETH", -0.5, nil), lg("SOL", 20, nil)),
	}
	res := Compute(txs, Manual{})
	eth, sol := res.Assets["ETH"], res.Assets["SOL"]
	if len(eth.RealizedByYear) != 0 {
		t.Errorf("realized without proceeds: %v", eth.RealizedByYear)
	}
	near(t, "eth amount", eth.Amount, 0.5)
	near(t, "eth basis", eth.Basis, 1000)
	near(t, "sol known", sol.Amount, 0)
	near(t, "sol unknown", sol.UnknownBasisUnits, 20)
}

func TestFiatLegsAreNotTracked(t *testing.T) {
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("USD", -500, usd(-500)), lg("BTC", 0.01, usd(501.3))),
		mk("e", day(2021, 1, 2), core.TxTrade, lg("EUR", -100, nil), lg("ETH", 0.06, nil)),
	}
	res := Compute(txs, Manual{})
	for _, k := range []string{"USD", "EUR"} {
		if _, ok := res.Assets[k]; ok {
			t.Errorf("fiat %s tracked as an asset", k)
		}
	}
	near(t, "btc basis", res.Assets["BTC"].Basis, 501.3)
	near(t, "eth unknown", res.Assets["ETH"].UnknownBasisUnits, 0.06)
}

func TestRewardsAreAcquiredAtFairMarketValue(t *testing.T) {
	txs := []core.Tx{
		mk("w1", day(2021, 7, 1), core.TxReward, lg("ETH", 0.1, usd(300))),
		mk("w2", day(2021, 8, 1), core.TxReward, lg("ETH", 0.1, usd(400))),
		mk("w3", day(2021, 9, 1), core.TxReward, lg("ETH", 0.05, nil)), // price missing
	}
	eth := Compute(txs, Manual{}).Assets["ETH"]
	near(t, "amount", eth.Amount, 0.2)
	near(t, "basis", eth.Basis, 700)
	near(t, "avg", eth.AvgCost, 3500)
	near(t, "unknown", eth.UnknownBasisUnits, 0.05)
}

func TestTransfersPeeredAndFailedTxsAreIgnored(t *testing.T) {
	peered := mk("p", day(2021, 3, 1), core.TxSend, lg("BTC", -1, nil))
	peered.PeerID, peered.PeerSource = "test:q", "other"
	peeredRx := mk("q", day(2021, 3, 1), core.TxReceive, lg("BTC", 1, nil))
	peeredRx.PeerID = "test:p"
	failed := mk("f", day(2021, 4, 1), core.TxTrade, lg("BTC", -1, usd(-50000)))
	failed.Status = core.TxFailed
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("BTC", 1, usd(10000))),
		mk("t", day(2021, 2, 1), core.TxTransfer, lg("BTC", -1, nil), lg("BTC", 1, nil)),
		peered, peeredRx, failed,
		mk("st", day(2021, 5, 1), core.TxStake, lg("BTC", -0.5, nil)),
	}
	btc := Compute(txs, Manual{}).Assets["BTC"]
	near(t, "amount", btc.Amount, 1)
	near(t, "basis", btc.Basis, 10000)
	near(t, "unknown", btc.UnknownBasisUnits, 0)
	if len(btc.RealizedByYear) != 0 {
		t.Errorf("realized = %v", btc.RealizedByYear)
	}
}

func TestFeesReduceBasisWithoutRealizing(t *testing.T) {
	sell := mk("s", day(2022, 1, 1), core.TxTrade, lg("ETH", -0.5, usd(-1500)))
	sell.Fee = &core.Leg{AssetKey: "ETH", Symbol: "ETH", Amount: -0.1}
	send := mk("x", day(2022, 2, 1), core.TxSend, lg("ETH", -0.2, nil))
	send.Fee = &core.Leg{AssetKey: "ETH", Symbol: "ETH", Amount: -0.1}
	fiatFee := mk("y", day(2022, 3, 1), core.TxTrade, lg("BTC", 1, usd(100)))
	fiatFee.Fee = &core.Leg{AssetKey: "USD", Symbol: "USD", Amount: -1}
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("ETH", 2, usd(4000))),
		sell, send, fiatFee,
	}
	res := Compute(txs, Manual{})
	eth := res.Assets["ETH"]
	// 0.5 sold at avg 2000 realizes 1500-1000; fees (0.1 + 0.1) and the 0.2 send only remove units.
	near(t, "realized", eth.RealizedByYear[2022], 500)
	near(t, "amount", eth.Amount, 1.1)
	near(t, "basis", eth.Basis, 2200)
	near(t, "avg unchanged", eth.AvgCost, 2000)
	if _, ok := res.Assets["USD"]; ok {
		t.Error("fiat fee tracked as asset")
	}
}

func TestStandaloneFeeTx(t *testing.T) {
	fee := mk("f", day(2022, 1, 1), core.TxFee, lg("SOL", -0.01, nil))
	txs := []core.Tx{mk("b", day(2021, 1, 1), core.TxTrade, lg("SOL", 10, usd(1000))), fee}
	sol := Compute(txs, Manual{}).Assets["SOL"]
	near(t, "amount", sol.Amount, 9.99)
	near(t, "avg", sol.AvgCost, 100)
}

func TestDustIsClearedAfterFullDisposal(t *testing.T) {
	txs := []core.Tx{
		mk("b1", day(2021, 1, 1), core.TxTrade, lg("BTC", 0.1, usd(1000))),
		mk("b2", day(2021, 1, 2), core.TxTrade, lg("BTC", 0.2, usd(2000))),
		mk("s", day(2021, 1, 3), core.TxTrade, lg("BTC", -0.3, usd(-4000))),
	}
	btc := Compute(txs, Manual{}).Assets["BTC"]
	if btc.Amount != 0 || btc.Basis != 0 || btc.AvgCost != 0 {
		t.Errorf("residue after full disposal: %+v", btc)
	}
}

func TestManualLotsAreAcquisitionsAtTheirDate(t *testing.T) {
	manual := Manual{Lots: []ManualLot{{Asset: "ETH", Amount: 2, CostUSD: 3000, Date: DateOf(day(2021, 5, 1))}}}
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("ETH", 1, usd(1000))),
		mk("s", day(2021, 6, 1), core.TxTrade, lg("ETH", -1, usd(-2500))),
	}
	eth := Compute(txs, manual).Assets["ETH"]
	// Average of (1000 + 3000) / 3 units at the time of the sale.
	near(t, "realized", eth.RealizedByYear[2021], 2500-4000.0/3)
	near(t, "amount", eth.Amount, 2)
	near(t, "basis", eth.Basis, 8000.0/3)

	// A lot dated after the sale does not affect it.
	manual.Lots[0].Date = DateOf(day(2021, 7, 1))
	eth = Compute(txs, manual).Assets["ETH"]
	near(t, "realized before lot", eth.RealizedByYear[2021], 1500)
	near(t, "amount after lot", eth.Amount, 2)
	near(t, "basis after lot", eth.Basis, 3000)
}

func TestManualAssetWithoutOverrideIsAnOpeningLot(t *testing.T) {
	manual := Manual{Assets: []ManualAsset{{Asset: "BTC", Amount: 0.5, CostUSD: 15000}}}
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("BTC", 0.5, usd(25000))),
		mk("s", day(2021, 2, 1), core.TxTrade, lg("BTC", -0.5, usd(-30000))),
	}
	btc := Compute(txs, manual).Assets["BTC"]
	// avg = 40000 / 1 BTC
	near(t, "realized", btc.RealizedByYear[2021], 10000)
	near(t, "amount", btc.Amount, 0.5)
	near(t, "basis", btc.Basis, 20000)
	if btc.Overridden {
		t.Error("not an override")
	}
}

func TestManualOverrideReplacesStateButKeepsRealizedHistory(t *testing.T) {
	manual := Manual{Assets: []ManualAsset{{Asset: "BTC", Amount: 0.5, CostUSD: 15000, Override: true}}}
	txs := []core.Tx{
		mk("b", day(2021, 1, 1), core.TxTrade, lg("BTC", 1, usd(10000))),
		mk("r", day(2021, 1, 2), core.TxReceive, lg("BTC", 0.25, nil)),
		mk("s", day(2021, 2, 1), core.TxTrade, lg("BTC", -0.5, usd(-12000))),
	}
	btc := Compute(txs, manual).Assets["BTC"]
	near(t, "amount", btc.Amount, 0.5)
	near(t, "basis", btc.Basis, 15000)
	near(t, "avg", btc.AvgCost, 30000)
	near(t, "unknown cleared", btc.UnknownBasisUnits, 0)
	near(t, "realized kept", btc.RealizedByYear[2021], 7000)
	if !btc.Overridden {
		t.Error("Overridden not set")
	}

	// An override for an asset with no transactions creates it.
	res := Compute(nil, Manual{Assets: []ManualAsset{{Asset: "SOL", Amount: 10, CostUSD: 0, Override: true}}})
	sol := res.Assets["SOL"]
	if !sol.Overridden || sol.Amount != 10 || sol.Basis != 0 {
		t.Errorf("sol = %+v", sol)
	}
}

func TestUnrealized(t *testing.T) {
	res := Result{Assets: map[string]AssetResult{
		"BTC": {AssetKey: "BTC", Amount: 1, Basis: 20000, AvgCost: 20000},
		"ETH": {AssetKey: "ETH", Amount: 0, UnknownBasisUnits: 3},
		"SOL": {AssetKey: "SOL", Amount: 10, Basis: 0, AvgCost: 0},
	}}

	// Holding exactly what the engine tracks.
	basis, unreal, pct, ok := res.Unrealized("BTC", 1, 30000)
	if !ok {
		t.Fatal("not ok")
	}
	near(t, "basis", basis, 20000)
	near(t, "unrealized", unreal, 10000)
	near(t, "pct", pct, 50)

	// Holding more than has a known basis: only the covered units count.
	basis, unreal, pct, ok = res.Unrealized("BTC", 3, 30000)
	if !ok {
		t.Fatal("not ok")
	}
	near(t, "basis", basis, 20000)
	near(t, "unrealized", unreal, 10000)
	near(t, "pct", pct, 50)

	// Holding less: basis is avg x current amount.
	basis, unreal, pct, ok = res.Unrealized("BTC", 0.5, 10000)
	if !ok {
		t.Fatal("not ok")
	}
	near(t, "basis", basis, 10000)
	near(t, "unrealized", unreal, -5000)
	near(t, "pct", pct, -50)

	// A known zero cost (gift/airdrop) is a basis; the percentage is undefined and reported as 0.
	basis, unreal, pct, ok = res.Unrealized("SOL", 10, 100)
	if !ok || basis != 0 {
		t.Fatalf("sol basis %v ok %v", basis, ok)
	}
	near(t, "unrealized", unreal, 1000)
	near(t, "pct", pct, 0)

	for _, c := range []struct {
		asset string
		amt   float64
	}{{"ETH", 3}, {"DOGE", 5}, {"BTC", 0}, {"BTC", -1}} {
		if _, _, _, ok := res.Unrealized(c.asset, c.amt, 100); ok {
			t.Errorf("Unrealized(%s, %v) ok, want false", c.asset, c.amt)
		}
	}
}

func rows(rs ...portfolio.AssetRow) []portfolio.AssetRow { return rs }

func TestCoverage(t *testing.T) {
	res := Result{Assets: map[string]AssetResult{
		"BTC": {Amount: 1, Basis: 20000, AvgCost: 20000},
		"ETH": {Amount: 5, Basis: 5000, AvgCost: 1000},
	}}
	assets := rows(
		portfolio.AssetRow{AssetKey: "BTC", Amount: 2, ValueUSD: 60000},  // half covered -> 30000
		portfolio.AssetRow{AssetKey: "ETH", Amount: 2, ValueUSD: 6000},   // fully covered -> 6000
		portfolio.AssetRow{AssetKey: "SOL", Amount: 100, ValueUSD: 4000}, // no basis
	)
	// (30000 + 6000) / 70000
	near(t, "coverage", Coverage(res, assets), 36000.0/70000)

	near(t, "empty", Coverage(res, nil), 0)
	near(t, "zero value", Coverage(res, rows(portfolio.AssetRow{AssetKey: "BTC", Amount: 1})), 0)
	near(t, "all", Coverage(res, rows(portfolio.AssetRow{AssetKey: "ETH", Amount: 5, ValueUSD: 100})), 1)
}

func TestForPortfolio(t *testing.T) {
	res := Result{Assets: map[string]AssetResult{
		"BTC": {Amount: 2, Basis: 40000, AvgCost: 20000},
		"ETH": {Amount: 1, Basis: 1000, AvgCost: 1000},
	}}
	assets := rows(
		portfolio.AssetRow{AssetKey: "BTC", Amount: 1, ValueUSD: 30000}, // pro-rata: basis 20000
		portfolio.AssetRow{AssetKey: "ETH", Amount: 4, ValueUSD: 8000},  // covered 1 of 4: basis 1000, value 2000
		portfolio.AssetRow{AssetKey: "SOL", Amount: 10, ValueUSD: 1500}, // no basis: excluded
		portfolio.AssetRow{AssetKey: "XRP", Amount: 0, ValueUSD: 0},     // empty
	)
	basis, value, unreal := ForPortfolio(res, assets)
	near(t, "basis", basis, 21000)
	near(t, "value", value, 32000)
	near(t, "unrealized", unreal, 11000)

	b, v, u := ForPortfolio(res, nil)
	if b != 0 || v != 0 || u != 0 {
		t.Errorf("empty = %v %v %v", b, v, u)
	}
}

func TestComputeCanonicalizesAndDropsBadManualEntries(t *testing.T) {
	manual := Manual{
		Assets: []ManualAsset{
			{Asset: "btc", Amount: 1, CostUSD: 100},
			{Asset: "ETH", Amount: -1, CostUSD: 5}, // invalid: dropped
			{Asset: "", Amount: 1, CostUSD: 5},     // invalid: dropped
			{Asset: "SOL", Amount: 1, CostUSD: -5}, // invalid: dropped
		},
		Lots: []ManualLot{{Asset: "spl:AbC", Amount: 2, CostUSD: 4}},
	}
	res := Compute(nil, manual)
	if len(res.Assets) != 2 {
		t.Fatalf("assets = %+v", res.Assets)
	}
	near(t, "btc", res.Assets["BTC"].Basis, 100)
	near(t, "spl", res.Assets["spl:AbC"].Amount, 2)
}
