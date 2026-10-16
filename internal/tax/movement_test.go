package tax

import (
	"testing"

	"crypto-portfolio/internal/core"
)

func TestUnmatchedReceiveIsPurchase(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Txs:  []core.Tx{mk("r1", "sol", at(2026, 3, 1, 0), core.TxReceive, lg("SOL", 2, usd(300)))},
	})
	sol := lotsIn(r, "sol", "SOL")
	if len(sol) != 1 {
		t.Fatalf("SOL lots = %+v, want 1", sol)
	}
	near(t, "amount", sol[0].Amount, 2)
	near(t, "cost", sol[0].CostUSD, 300)
	if !sol[0].Acquired.Equal(at(2026, 3, 1, 0)) || sol[0].TxID != "sol:r1" {
		t.Errorf("lot = %+v, want acquired at tx time from sol:r1", sol[0])
	}
	if len(r.Income) != 0 {
		t.Errorf("income = %+v, want none", r.Income)
	}
	is := issuesOf(r, IssueUnmatchedReceive)
	if len(is) != 1 || len(r.Issues) != 1 || is[0].Asset != "SOL" || is[0].TxID != "sol:r1" {
		t.Fatalf("issues = %+v, want one unmatched-receive", r.Issues)
	}
	near(t, "issue amount", is[0].Amount, 2)
}

func TestUnmatchedSendIsSale(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 2, 300, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{mk("s1", "sol", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200)))},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want 1", r.Disposals)
	}
	d := r.Disposals[0]
	near(t, "proceeds", d.ProceedsUSD, 200)
	near(t, "cost", d.CostUSD, 150)
	near(t, "gain", d.GainUSD, 50)
	is := issuesOf(r, IssueUnmatchedSend)
	if len(is) != 1 || len(r.Issues) != 1 {
		t.Fatalf("issues = %+v, want one unmatched-send", r.Issues)
	}
	near(t, "issue amount", is[0].Amount, 1)
}

func TestRewardIsIncomeAndLot(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Txs:  []core.Tx{mk("w1", "sol", at(2026, 3, 1, 0), core.TxReward, lg("SOL", 0.1, usd(15)))},
	})
	if len(r.Income) != 1 {
		t.Fatalf("income = %+v, want 1", r.Income)
	}
	in := r.Income[0]
	if in.Type != "reward" || in.Asset != "SOL" || in.Wallet != "sol" || in.TxID != "sol:w1" || !in.Date.Equal(at(2026, 3, 1, 0)) {
		t.Errorf("income = %+v", in)
	}
	near(t, "income amount", in.Amount, 0.1)
	near(t, "income value", in.ValueUSD, 15)
	sol := lotsIn(r, "sol", "SOL")
	if len(sol) != 1 {
		t.Fatalf("SOL lots = %+v, want 1", sol)
	}
	near(t, "lot amount", sol[0].Amount, 0.1)
	near(t, "lot cost", sol[0].CostUSD, 15)
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

func TestNegativeRewardLegIsDisposalWithoutIssue(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{mk("w1", "sol", at(2026, 3, 1, 0), core.TxReward, lg("SOL", -1, usd(-150)))},
	})
	if len(r.Disposals) != 1 || len(r.Issues) != 0 || len(r.Income) != 0 {
		t.Fatalf("disposals = %+v issues = %+v income = %+v", r.Disposals, r.Issues, r.Income)
	}
	near(t, "proceeds", r.Disposals[0].ProceedsUSD, 150)
}

func TestOtherKindBySign(t *testing.T) {
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("sol", "BTC", 1, 100, at(2025, 6, 1, 0))},
		Txs: []core.Tx{mk("o1", "sol", at(2026, 3, 1, 0), core.TxOther,
			lg("SOL", 2, usd(300)), lg("BTC", -1, usd(-200)))},
	})
	sol := lotsIn(r, "sol", "SOL")
	if len(sol) != 1 {
		t.Fatalf("SOL lots = %+v, want 1", sol)
	}
	near(t, "SOL cost", sol[0].CostUSD, 300)
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want 1", r.Disposals)
	}
	near(t, "proceeds", r.Disposals[0].ProceedsUSD, 200)
	if len(issuesOf(r, IssueUnmatchedReceive)) != 1 || len(issuesOf(r, IssueUnmatchedSend)) != 1 || len(r.Issues) != 2 {
		t.Fatalf("issues = %+v, want one receive and one send", r.Issues)
	}
	if len(r.Income) != 0 {
		t.Errorf("income = %+v, want none", r.Income)
	}
}

func TestUnknownKindTreatedAsOther(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Txs:  []core.Tx{mk("u1", "sol", at(2026, 3, 1, 0), core.TxKind("weird"), lg("SOL", 1, usd(10)))},
	})
	if len(lotsIn(r, "sol", "SOL")) != 1 || len(issuesOf(r, IssueUnmatchedReceive)) != 1 {
		t.Fatalf("lots = %+v issues = %+v", r.Lots, r.Issues)
	}
}

func TestReceiveMissingPrice(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Txs:  []core.Tx{mk("r1", "sol", at(2026, 3, 1, 0), core.TxReceive, lg("spl:X", 5, nil))},
	})
	sol := lotsIn(r, "sol", "spl:X")
	if len(sol) != 1 {
		t.Fatalf("lots = %+v", r.Lots)
	}
	near(t, "cost", sol[0].CostUSD, 0)
	if len(issuesOf(r, IssueMissingPrice)) != 1 || len(issuesOf(r, IssueUnmatchedReceive)) != 1 {
		t.Fatalf("issues = %+v, want missing-price and unmatched-receive", r.Issues)
	}
}

func TestManualLotAtSaleTimeFundsSale(t *testing.T) {
	r := Compute(Input{
		From:   from2026,
		Manual: []Lot{lot("btc", "BTC", 1, 50000, at(2026, 3, 1, 0))},
		Txs: []core.Tx{mk("s1", "btc", at(2026, 3, 1, 0), core.TxTrade,
			lg("BTC", -0.5, usd(-30000)), lg("USD", 30000, nil))},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "" || len(issuesOf(r, IssueMissingBasis)) != 0 {
		t.Fatalf("disposals = %+v issues = %+v, want one funded sale", r.Disposals, r.Issues)
	}
	near(t, "cost", r.Disposals[0].CostUSD, 25000)
}

func TestPartialSaleOfVariousOpeningLot(t *testing.T) {
	l := lot("btc", "BTC", 2, 1000, at(2024, 12, 31, 0))
	l.Various = true
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{l},
		Txs: []core.Tx{mk("s1", "btc", at(2026, 3, 1, 0), core.TxTrade,
			lg("BTC", -1, usd(-900)), lg("USD", 900, nil))},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want 1", r.Disposals)
	}
	d := r.Disposals[0]
	if !d.Various || d.Term != LongTerm {
		t.Errorf("disposal = %+v, want Various and LongTerm", d)
	}
	near(t, "cost", d.CostUSD, 500)
}

func TestIssueAmountsArePositive(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Txs: []core.Tx{mk("s1", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("spl:X", -100, nil), lg("spl:Y", 50, nil))},
	})
	if len(r.Issues) == 0 {
		t.Fatal("want issues")
	}
	for _, is := range r.Issues {
		if is.Amount <= 0 {
			t.Errorf("issue %+v has non-positive amount", is)
		}
	}
}

func TestSpamDroppedBeforeValuation(t *testing.T) {
	r := Compute(Input{
		From: from2026,
		Spam: map[string]bool{"spl:SPAM": true},
		Txs: []core.Tx{mk("sw", "sol", at(2026, 3, 1, 0), core.TxSwap,
			lg("USDC", -100, usd(-100)), lg("BTC", 0.001, nil), lg("spl:SPAM", 1, nil))},
	})
	btc := lotsIn(r, "sol", "BTC")
	if len(btc) != 1 {
		t.Fatalf("BTC lots = %+v", r.Lots)
	}
	near(t, "BTC cost", btc[0].CostUSD, 100)
	if len(issuesOf(r, IssueMissingPrice)) != 0 {
		t.Errorf("issues = %+v, want no missing-price", r.Issues)
	}
}

// Dust legs (as fee() ignores a dust fee) neither open lots nor raise issues.
func TestMovementIgnoresDust(t *testing.T) {
	e := newEngine(Input{From: from2026})
	tx := mk("r1", "sol", at(2026, 3, 1, 0), core.TxReceive)
	for _, amount := range []float64{1e-13, -1e-13} {
		e.movement(tx, []*valued{{Leg: lg("SOL", amount, usd(0.00001)), usd: 0.00001, known: true}})
	}
	r := e.result()
	if len(r.Lots) != 0 || len(r.Disposals) != 0 || len(r.Issues) != 0 {
		t.Fatalf("lots = %+v disposals = %+v issues = %+v, want none", r.Lots, r.Disposals, r.Issues)
	}
}
