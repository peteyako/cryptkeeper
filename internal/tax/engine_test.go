package tax

import (
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

// from2026 is the start of the open year in UTC.
var from2026 = at(2026, 1, 1, 0)

// lot builds an opening or manual lot.
func lot(wallet, asset string, amount, cost float64, acquired time.Time) Lot {
	return Lot{Wallet: wallet, Asset: asset, Amount: amount, CostUSD: cost, Acquired: acquired, TxID: "opening"}
}

// withFee sets tx's network fee.
func withFee(tx core.Tx, fee core.Leg) core.Tx {
	tx.Fee = &fee
	return tx
}

// lotsIn returns the open lots of one asset in one wallet.
func lotsIn(r Result, wallet, asset string) []Lot {
	var out []Lot
	for _, l := range r.Lots {
		if l.Wallet == wallet && l.Asset == asset {
			out = append(out, l)
		}
	}
	return out
}

// heldIn is the amount of one asset held in one wallet.
func heldIn(r Result, wallet, asset string) float64 {
	var s float64
	for _, l := range lotsIn(r, wallet, asset) {
		s += l.Amount
	}
	return s
}

// issuesOf returns the issues of one kind.
func issuesOf(r Result, kind IssueKind) []Issue {
	var out []Issue
	for _, is := range r.Issues {
		if is.Kind == kind {
			out = append(out, is)
		}
	}
	return out
}

// disposalsNoted returns the disposals with the given note.
func disposalsNoted(r Result, note string) []Disposal {
	var out []Disposal
	for _, d := range r.Disposals {
		if d.Note == note {
			out = append(out, d)
		}
	}
	return out
}

func TestTradeFIFOAndTerms(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Opening: []Lot{
			lot("sol", "SOL", 10, 1000, at(2025, 2, 1, 0)),
			lot("sol", "SOL", 10, 2000, at(2025, 8, 1, 0)),
		},
		Txs: []core.Tx{mk("s1", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("SOL", -15, usd(-3000)), lg("USDC", 3000, nil))},
	})
	if len(r.Disposals) != 2 {
		t.Fatalf("disposals = %+v, want 2", r.Disposals)
	}
	d0, d1 := r.Disposals[0], r.Disposals[1]
	near(t, "d0.Amount", d0.Amount, 10)
	near(t, "d0.CostUSD", d0.CostUSD, 1000)
	near(t, "d0.ProceedsUSD", d0.ProceedsUSD, 2000)
	near(t, "d0.GainUSD", d0.GainUSD, 1000)
	if d0.Term != LongTerm || d0.Note != "" || !d0.Acquired.Equal(at(2025, 2, 1, 0)) {
		t.Errorf("d0 = %+v, want long-term, no note, acquired 2025-02-01", d0)
	}
	if d0.Asset != "SOL" || d0.Wallet != "sol" || d0.TxID != "sol:s1" || !d0.Sold.Equal(at(2026, 3, 1, 0)) {
		t.Errorf("d0 = %+v, want SOL in sol from sol:s1 sold 2026-03-01", d0)
	}
	near(t, "d1.Amount", d1.Amount, 5)
	near(t, "d1.CostUSD", d1.CostUSD, 1000)
	near(t, "d1.ProceedsUSD", d1.ProceedsUSD, 1000)
	near(t, "d1.GainUSD", d1.GainUSD, 0)
	if d1.Term != ShortTerm || !d1.Acquired.Equal(at(2025, 8, 1, 0)) {
		t.Errorf("d1 = %+v, want short-term, acquired 2025-08-01", d1)
	}

	if len(r.Lots) != 2 {
		t.Fatalf("lots = %+v, want SOL and USDC", r.Lots)
	}
	sol, usdc := r.Lots[0], r.Lots[1]
	if sol.Asset != "SOL" || !sol.Acquired.Equal(at(2025, 8, 1, 0)) {
		t.Errorf("lots[0] = %+v, want SOL acquired 2025-08-01", sol)
	}
	near(t, "SOL amount", sol.Amount, 5)
	near(t, "SOL cost", sol.CostUSD, 1000)
	if usdc.Asset != "USDC" || usdc.Wallet != "sol" || usdc.TxID != "sol:s1" || !usdc.Acquired.Equal(at(2026, 3, 1, 0)) {
		t.Errorf("lots[1] = %+v, want USDC in sol from sol:s1 acquired 2026-03-01", usdc)
	}
	near(t, "USDC amount", usdc.Amount, 3000)
	near(t, "USDC cost", usdc.CostUSD, 3000)
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

func TestFeeIsDisposal(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 2, 1, 0))},
		Txs: []core.Tx{withFee(mk("s1", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("SOL", -1, usd(-200)), lg("USDC", 200, nil)), lg("SOL", -0.01, usd(-2)))},
	})
	if len(r.Disposals) != 2 {
		t.Fatalf("disposals = %+v, want the swap and the fee", r.Disposals)
	}
	fees := disposalsNoted(r, "Fee")
	if len(fees) != 1 {
		t.Fatalf("fee disposals = %+v, want 1", fees)
	}
	f := fees[0]
	near(t, "fee Amount", f.Amount, 0.01)
	near(t, "fee ProceedsUSD", f.ProceedsUSD, 2)
	near(t, "fee CostUSD", f.CostUSD, 1)
	near(t, "fee GainUSD", f.GainUSD, 1)
	if f.Asset != "SOL" || f.TxID != "sol:s1" || f.Term != LongTerm {
		t.Errorf("fee = %+v, want SOL from sol:s1, long-term", f)
	}
	near(t, "SOL held", heldIn(r, "sol", "SOL"), 8.99)
}

func TestFailedTxOnlyFee(t *testing.T) {
	tx := withFee(mk("f1", "sol", at(2026, 3, 1, 0), core.TxSwap,
		lg("SOL", -1, usd(-200)), lg("USDC", 200, nil)), lg("SOL", -0.00001, usd(-0.002)))
	tx.Status = core.TxFailed
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 2, 1, 0))},
		Txs:     []core.Tx{tx},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want one Fee", r.Disposals)
	}
	near(t, "fee Amount", r.Disposals[0].Amount, 0.00001)
	near(t, "fee ProceedsUSD", r.Disposals[0].ProceedsUSD, 0.002)
	if got := lotsIn(r, "sol", "USDC"); len(got) != 0 {
		t.Errorf("USDC lots = %+v, want none", got)
	}
	near(t, "SOL held", heldIn(r, "sol", "SOL"), 9.99999)
}

func TestMissingCostBasis(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs: []core.Tx{mk("s1", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("SOL", -2, usd(-400)), lg("USDC", 400, nil))},
	})
	if len(r.Disposals) != 2 {
		t.Fatalf("disposals = %+v, want 2", r.Disposals)
	}
	d0, d1 := r.Disposals[0], r.Disposals[1]
	near(t, "d0.Amount", d0.Amount, 1)
	near(t, "d0.CostUSD", d0.CostUSD, 100)
	near(t, "d0.ProceedsUSD", d0.ProceedsUSD, 200)
	if d0.Note != "" {
		t.Errorf("d0.Note = %q, want empty", d0.Note)
	}
	near(t, "d1.Amount", d1.Amount, 1)
	near(t, "d1.CostUSD", d1.CostUSD, 0)
	near(t, "d1.ProceedsUSD", d1.ProceedsUSD, 200)
	near(t, "d1.GainUSD", d1.GainUSD, 200)
	if d1.Note != "Missing cost basis" || !d1.Acquired.Equal(d1.Sold) || d1.Term != ShortTerm {
		t.Errorf("d1 = %+v, want Missing cost basis, Acquired == Sold, short-term", d1)
	}
	if d1.Asset != "SOL" || d1.Wallet != "sol" || d1.TxID != "sol:s1" {
		t.Errorf("d1 = %+v, want SOL in sol from sol:s1", d1)
	}
	missing := issuesOf(r, IssueMissingBasis)
	if len(missing) != 1 {
		t.Fatalf("missing-basis issues = %+v, want 1", missing)
	}
	is := missing[0]
	near(t, "issue Amount", is.Amount, 1)
	if is.Asset != "SOL" || is.Wallet != "sol" || is.TxID != "sol:s1" || !is.Time.Equal(at(2026, 3, 1, 0)) {
		t.Errorf("issue = %+v, want SOL in sol from sol:s1 at the sale", is)
	}
	if got := lotsIn(r, "sol", "SOL"); len(got) != 0 {
		t.Errorf("SOL lots = %+v, want none", got)
	}
}

func TestMissingPrice(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "spl:X", 100, 10, at(2025, 6, 1, 0))},
		Txs: []core.Tx{mk("s1", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("spl:X", -100, nil), lg("spl:Y", 50, nil))},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want 1", r.Disposals)
	}
	near(t, "ProceedsUSD", r.Disposals[0].ProceedsUSD, 0)
	near(t, "GainUSD", r.Disposals[0].GainUSD, -10)
	y := lotsIn(r, "sol", "spl:Y")
	if len(y) != 1 {
		t.Fatalf("spl:Y lots = %+v, want 1", y)
	}
	near(t, "spl:Y amount", y[0].Amount, 50)
	near(t, "spl:Y cost", y[0].CostUSD, 0)
	missing := issuesOf(r, IssueMissingPrice)
	if len(missing) != 2 || len(r.Issues) != 2 {
		t.Fatalf("issues = %+v, want two missing-price", r.Issues)
	}
	if missing[0].Asset != "spl:X" || missing[1].Asset != "spl:Y" || missing[0].TxID != "sol:s1" {
		t.Errorf("issues = %+v, want spl:X then spl:Y from sol:s1", missing)
	}
}

func TestFromUsesTaxZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	r := Compute(Input{
		From: time.Date(2026, 1, 1, 0, 0, 0, 0, ny),
		Loc:  ny,
		Txs: []core.Tx{
			// 2025-12-31 22:00 in New York: before the open year.
			mk("early", "sol", time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), core.TxTrade,
				lg("USD", -100, nil), lg("BTC", 0.001, usd(100))),
			// 2026-01-01 01:00 in New York.
			mk("late", "sol", time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC), core.TxTrade,
				lg("USD", -200, nil), lg("BTC", 0.002, usd(200))),
		},
	})
	btc := lotsIn(r, "sol", "BTC")
	if len(btc) != 1 || btc[0].TxID != "sol:late" {
		t.Fatalf("BTC lots = %+v, want only the one from sol:late", btc)
	}
	near(t, "BTC amount", btc[0].Amount, 0.002)
}

func TestManualLotAppliedAtDate(t *testing.T) {
	r := Compute(Input{
		From:   from2026,
		Manual: []Lot{lot("btc", "BTC", 1, 50000, at(2026, 2, 1, 0))},
		Txs: []core.Tx{
			mk("late", "btc", at(2026, 3, 1, 0), core.TxTrade, lg("BTC", -0.5, usd(-30000)), lg("USD", 30000, nil)),
			mk("early", "btc", at(2026, 1, 15, 0), core.TxTrade, lg("BTC", -0.5, usd(-20000)), lg("USD", 20000, nil)),
		},
	})
	if len(r.Disposals) != 2 {
		t.Fatalf("disposals = %+v, want 2", r.Disposals)
	}
	early, late := r.Disposals[0], r.Disposals[1]
	if early.TxID != "btc:early" || early.Note != "Missing cost basis" {
		t.Errorf("early = %+v, want Missing cost basis", early)
	}
	near(t, "early cost", early.CostUSD, 0)
	if late.TxID != "btc:late" || late.Note != "" || !late.Acquired.Equal(at(2026, 2, 1, 0)) {
		t.Errorf("late = %+v, want the manual lot", late)
	}
	near(t, "late cost", late.CostUSD, 25000)
	near(t, "late proceeds", late.ProceedsUSD, 30000)
	btc := lotsIn(r, "btc", "BTC")
	if len(btc) != 1 {
		t.Fatalf("BTC lots = %+v, want 1", btc)
	}
	near(t, "BTC amount", btc[0].Amount, 0.5)
	near(t, "BTC cost", btc[0].CostUSD, 25000)
}

func TestManualLotWithoutDateIsAtFrom(t *testing.T) {
	r := Compute(Input{
		From:   from2026,
		Manual: []Lot{lot("btc", "BTC", 1, 50000, time.Time{})},
		Txs: []core.Tx{mk("s1", "btc", at(2026, 1, 15, 0), core.TxTrade,
			lg("BTC", -0.5, usd(-20000)), lg("USD", 20000, nil))},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "" || !r.Disposals[0].Acquired.Equal(from2026) {
		t.Fatalf("disposals = %+v, want one from the manual lot acquired at From", r.Disposals)
	}
	near(t, "cost", r.Disposals[0].CostUSD, 25000)
}

func TestFiatAndSpamIgnored(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Spam: map[string]bool{"spl:SPAM": true},
		Txs: []core.Tx{
			mk("buy", "cb", at(2026, 2, 1, 0), core.TxTrade, lg("USD", -100, nil), lg("BTC", 0.001, nil)),
			mk("drop", "sol", at(2026, 2, 2, 0), core.TxReceive, lg("spl:SPAM", 1e6, nil)),
			// Without the spam filter this would be a Missing cost basis disposal.
			mk("dump", "sol", at(2026, 2, 3, 0), core.TxSwap, lg("spl:SPAM", -1000, nil), lg("USDC", 1, nil)),
		},
	})
	if len(r.Disposals) != 0 || len(r.Issues) != 0 {
		t.Fatalf("disposals = %+v, issues = %+v, want none", r.Disposals, r.Issues)
	}
	if len(r.Lots) != 2 {
		t.Fatalf("lots = %+v, want BTC in cb and USDC in sol", r.Lots)
	}
	btc := lotsIn(r, "cb", "BTC")
	if len(btc) != 1 {
		t.Fatalf("BTC lots = %+v, want 1", btc)
	}
	near(t, "BTC amount", btc[0].Amount, 0.001)
	near(t, "BTC cost", btc[0].CostUSD, 100)
	near(t, "USDC held", heldIn(r, "sol", "USDC"), 1)
}

func TestSameAssetLegsNetted(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 6, 1, 0))},
		Txs: []core.Tx{mk("s1", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("SOL", -1, usd(-200)), lg("SOL", 0.002, usd(0.4)), lg("JUP", 500, usd(199.6)))},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want one SOL disposal", r.Disposals)
	}
	d := r.Disposals[0]
	near(t, "Amount", d.Amount, 0.998)
	near(t, "ProceedsUSD", d.ProceedsUSD, 199.6)
	near(t, "CostUSD", d.CostUSD, 99.8)
	near(t, "SOL held", heldIn(r, "sol", "SOL"), 9.002)
	jup := lotsIn(r, "sol", "JUP")
	if len(jup) != 1 {
		t.Fatalf("JUP lots = %+v, want 1", jup)
	}
	near(t, "JUP cost", jup[0].CostUSD, 199.6)
}

func TestNettedLegUsesFirstLegPriceKey(t *testing.T) {
	id := core.TxID("sol", "s1")
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 6, 1, 0))},
		Txs: []core.Tx{mk("s1", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("JUP", 500, nil), lg("SOL", -1, nil), lg("SOL", 0.002, nil))},
		Prices: map[string]float64{PriceKey(id, 1): 150},
	})
	if len(r.Disposals) != 1 || len(r.Issues) != 0 {
		t.Fatalf("disposals = %+v, issues = %+v, want one SOL disposal and no issue", r.Disposals, r.Issues)
	}
	near(t, "ProceedsUSD", r.Disposals[0].ProceedsUSD, 150)
	jup := lotsIn(r, "sol", "JUP")
	if len(jup) != 1 {
		t.Fatalf("JUP lots = %+v, want 1", jup)
	}
	near(t, "JUP cost (filled from SOL)", jup[0].CostUSD, 150)
}

func TestSameTimeOrderedByID(t *testing.T) {
	when := at(2026, 3, 1, 12)
	r := Compute(Input{
		From: from2026,
		Txs: []core.Tx{
			mk("b", "sol", when, core.TxTrade, lg("USD", -200, nil), lg("SOL", 1, usd(200))),
			mk("a", "sol", when, core.TxTrade, lg("SOL", -1, usd(-200)), lg("USD", 200, nil)),
		},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want 1", r.Disposals)
	}
	if d := r.Disposals[0]; d.TxID != "sol:a" || d.Note != "Missing cost basis" {
		t.Errorf("disposal = %+v, want sol:a with Missing cost basis", d)
	}
	sol := lotsIn(r, "sol", "SOL")
	if len(sol) != 1 || sol[0].TxID != "sol:b" {
		t.Fatalf("SOL lots = %+v, want the lot bought by sol:b", sol)
	}
}

func TestStakeIsNoop(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 6, 1, 0))},
		Txs: []core.Tx{withFee(mk("st", "sol", at(2026, 3, 1, 0), core.TxStake,
			lg("SOL", -5, usd(-1000))), lg("SOL", -0.000005, usd(-0.001)))},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want only the Fee", r.Disposals)
	}
	near(t, "fee Amount", r.Disposals[0].Amount, 0.000005)
	near(t, "SOL held", heldIn(r, "sol", "SOL"), 9.999995)
}

func TestSelfTransferOnlyFee(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 6, 1, 0))},
		Txs: []core.Tx{withFee(mk("self", "sol", at(2026, 3, 1, 0), core.TxTransfer,
			lg("SOL", -1, usd(-200))), lg("SOL", -0.000005, usd(-0.001)))},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want only the Fee", r.Disposals)
	}
	near(t, "SOL held", heldIn(r, "sol", "SOL"), 9.999995)
}

func TestPendingCountsAsConfirmed(t *testing.T) {
	tx := mk("p", "sol", at(2026, 3, 1, 0), core.TxSwap, lg("SOL", -1, usd(-200)), lg("USDC", 200, nil))
	tx.Status = core.TxPending
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{tx},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want the swap's", r.Disposals)
	}
	near(t, "USDC held", heldIn(r, "sol", "USDC"), 200)
}

func TestFeeKindLegsAreFees(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{mk("f", "sol", at(2026, 3, 1, 0), core.TxFee, lg("SOL", -0.01, usd(-2)))},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want one Fee", r.Disposals)
	}
	near(t, "Amount", r.Disposals[0].Amount, 0.01)
	near(t, "ProceedsUSD", r.Disposals[0].ProceedsUSD, 2)
	near(t, "CostUSD", r.Disposals[0].CostUSD, 1)
}

func TestFeePrice(t *testing.T) {
	opening := []Lot{lot("sol", "SOL", 10, 1000, at(2025, 6, 1, 0))}
	tx := withFee(mk("s", "sol", at(2026, 3, 1, 0), core.TxStake), lg("SOL", -0.01, nil))

	r := Compute(Input{From: from2026, Opening: opening, Txs: []core.Tx{tx}})
	if len(r.Disposals) != 1 || r.Disposals[0].ProceedsUSD != 0 {
		t.Fatalf("disposals = %+v, want one Fee at 0", r.Disposals)
	}
	if missing := issuesOf(r, IssueMissingPrice); len(missing) != 1 || missing[0].Asset != "SOL" {
		t.Errorf("issues = %+v, want one missing-price for the fee", r.Issues)
	}

	r = Compute(Input{From: from2026, Opening: opening, Txs: []core.Tx{tx},
		Prices: map[string]float64{FeePriceKey(tx.ID): 3}})
	if len(r.Disposals) != 1 || len(r.Issues) != 0 {
		t.Fatalf("disposals = %+v, issues = %+v, want one Fee and no issue", r.Disposals, r.Issues)
	}
	near(t, "ProceedsUSD", r.Disposals[0].ProceedsUSD, 3)
}

func TestFiatFeeIgnored(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Txs: []core.Tx{withFee(mk("buy", "cb", at(2026, 3, 1, 0), core.TxTrade,
			lg("USD", -100, nil), lg("BTC", 0.001, usd(100))), lg("USD", -1.49, nil))},
	})
	if len(r.Disposals) != 0 || len(r.Issues) != 0 {
		t.Fatalf("disposals = %+v, issues = %+v, want none", r.Disposals, r.Issues)
	}
}

func TestResultSorted(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Opening: []Lot{
			lot("b", "SOL", 1, 100, at(2025, 6, 1, 0)),
			lot("a", "SOL", 1, 100, at(2025, 7, 1, 0)),
			lot("a", "SOL", 1, 100, at(2025, 5, 1, 0)),
			lot("a", "BTC", 1, 100, at(2025, 6, 1, 0)),
		},
	})
	want := []struct {
		wallet, asset string
		acq           time.Time
	}{
		{"a", "BTC", at(2025, 6, 1, 0)},
		{"a", "SOL", at(2025, 5, 1, 0)},
		{"a", "SOL", at(2025, 7, 1, 0)},
		{"b", "SOL", at(2025, 6, 1, 0)},
	}
	if len(r.Lots) != len(want) {
		t.Fatalf("lots = %+v, want %d", r.Lots, len(want))
	}
	for i, w := range want {
		if l := r.Lots[i]; l.Wallet != w.wallet || l.Asset != w.asset || !l.Acquired.Equal(w.acq) {
			t.Errorf("lots[%d] = %+v, want %s %s %v", i, l, w.wallet, w.asset, w.acq)
		}
	}
}
