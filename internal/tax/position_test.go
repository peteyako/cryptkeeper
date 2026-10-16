package tax

import (
	"testing"

	"crypto-portfolio/internal/core"
)

const jup = "spl:JUP"

func jupOpening() []Lot { return []Lot{lot("w", jup, 1000, 500, at(2025, 1, 1, 0))} }

func positionDeposit() core.Tx {
	return withFee(mk("dep", "w", at(2026, 3, 1, 0), core.TxSend, lg(jup, -1000, usd(-800))),
		lg("SOL", -0.000005, usd(-0.001)))
}

func TestPositionDepositKeepsLots(t *testing.T) {
	tx := positionDeposit()
	r := tagged(Input{Opening: append(jupOpening(), lot("w", "SOL", 1, 100, at(2025, 1, 1, 0)))}, tx, TagPosition)
	l := lotsIn(r, "w", jup)
	if len(l) != 1 {
		t.Fatalf("JUP lots = %+v, want the one unchanged", l)
	}
	near(t, "JUP amount", l[0].Amount, 1000)
	near(t, "JUP cost", l[0].CostUSD, 500)
	if len(r.Disposals) != 1 || r.Disposals[0].Asset != "SOL" || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want only the SOL Fee", r.Disposals)
	}
	if len(r.Issues) != 0 || len(r.Income) != 0 {
		t.Errorf("issues = %+v income = %+v, want none", r.Issues, r.Income)
	}
}

func TestPositionWithdrawWithinParked(t *testing.T) {
	wd := mk("wd", "w", at(2026, 4, 1, 0), core.TxReceive, lg(jup, 1000, usd(900)))
	in := Input{
		From: from2026, Opening: append(jupOpening(), lot("w", "SOL", 1, 100, at(2025, 1, 1, 0))),
		Txs:  []core.Tx{positionDeposit(), wd},
		Tags: map[string]Tag{positionDeposit().ID: TagPosition, wd.ID: TagPosition},
	}
	r := Compute(in)
	l := lotsIn(r, "w", jup)
	if len(l) != 1 {
		t.Fatalf("JUP lots = %+v", l)
	}
	near(t, "JUP amount", l[0].Amount, 1000)
	near(t, "JUP cost", l[0].CostUSD, 500)
	if len(r.Income) != 0 || len(r.Issues) != 0 {
		t.Errorf("income = %+v issues = %+v, want none", r.Income, r.Issues)
	}
}

func TestPositionWithdrawExcess(t *testing.T) {
	dep := mk("dep", "w", at(2026, 3, 1, 0), core.TxSend, lg(jup, -1000, usd(-800)))
	wd := mk("wd", "w", at(2026, 4, 1, 0), core.TxReceive, lg(jup, 1050, usd(52.5)))
	r := Compute(Input{
		From: from2026, Opening: jupOpening(), Txs: []core.Tx{dep, wd},
		Tags: map[string]Tag{dep.ID: TagPosition, wd.ID: TagPosition},
	})
	l := lotsIn(r, "w", jup)
	if len(l) != 2 {
		t.Fatalf("JUP lots = %+v, want the original and the excess", l)
	}
	near(t, "excess amount", l[1].Amount, 50)
	near(t, "excess cost", l[1].CostUSD, 2.5)
	if len(r.Income) != 1 || r.Income[0].Type != "position" || r.Income[0].TxID != wd.ID {
		t.Fatalf("income = %+v, want one position income", r.Income)
	}
	near(t, "income value", r.Income[0].ValueUSD, 2.5)
	near(t, "income amount", r.Income[0].Amount, 50)
	if len(r.Issues) != 1 || r.Issues[0].Kind != IssuePositionExcess {
		t.Fatalf("issues = %+v, want one position-excess", r.Issues)
	}
	near(t, "issue amount", r.Issues[0].Amount, 50)
}

// A second withdrawal draws on what is left parked; a withdrawal with nothing parked is
// all excess, and an unvalued excess is worth 0 with IssueMissingPrice.
func TestPositionWithdrawNoDepositUnpriced(t *testing.T) {
	wd := mk("wd", "w", at(2026, 4, 1, 0), core.TxReceive, lg(jup, 10, nil))
	r := tagged(Input{}, wd, TagPosition)
	l := lotsIn(r, "w", jup)
	if len(l) != 1 {
		t.Fatalf("lots = %+v", l)
	}
	near(t, "cost", l[0].CostUSD, 0)
	kinds := map[IssueKind]bool{}
	for _, is := range r.Issues {
		kinds[is.Kind] = true
	}
	if len(r.Issues) != 2 || !kinds[IssueMissingPrice] || !kinds[IssuePositionExcess] {
		t.Errorf("issues = %+v, want missing-price and position-excess", r.Issues)
	}
	if len(r.Income) != 1 {
		t.Errorf("income = %+v", r.Income)
	}
}

// Parked amounts are per wallet and asset.
func TestPositionParkedPerWalletAsset(t *testing.T) {
	dep := mk("dep", "w", at(2026, 3, 1, 0), core.TxSend, lg(jup, -100, usd(-80)))
	wd := mk("wd", "x", at(2026, 4, 1, 0), core.TxReceive, lg(jup, 100, usd(90)))
	r := Compute(Input{
		From: from2026, Opening: []Lot{lot("w", jup, 100, 50, at(2025, 1, 1, 0))},
		Txs:  []core.Tx{dep, wd},
		Tags: map[string]Tag{dep.ID: TagPosition, wd.ID: TagPosition},
	})
	if len(r.Issues) != 1 || r.Issues[0].Kind != IssuePositionExcess || r.Issues[0].Wallet != "x" {
		t.Errorf("issues = %+v, want position-excess in wallet x", r.Issues)
	}
}

// Only known tags take effect: an unknown one leaves the tx untagged.
func TestUnknownTagIsUntagged(t *testing.T) {
	tx := mk("r", "w", at(2026, 3, 1, 0), core.TxReceive, lg("SOL", 1, usd(100)))
	r := tagged(Input{}, tx, Tag("bogus"))
	if len(r.Issues) != 1 || r.Issues[0].Kind != IssueUnmatchedReceive {
		t.Errorf("issues = %+v, want unmatched-receive", r.Issues)
	}
}

// A gift or loss that the lots cannot cover keeps its note and raises IssueMissingBasis.
func TestTagGiftLostShortfall(t *testing.T) {
	for tag, note := range map[Tag]string{TagGift: "Gift", TagLost: "Lost"} {
		tx := mk("s", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -2, usd(-400)))
		r := tagged(Input{Opening: []Lot{lot("w", "SOL", 1, 50, at(2025, 1, 1, 0))}}, tx, tag)
		if len(r.GiftsLost) != 2 || len(r.Disposals) != 0 {
			t.Fatalf("%s: giftsLost = %+v disposals = %+v", tag, r.GiftsLost, r.Disposals)
		}
		for _, g := range r.GiftsLost {
			if g.Note != note {
				t.Errorf("%s: note = %q, want %q", tag, g.Note, note)
			}
		}
		if len(r.Issues) != 1 || r.Issues[0].Kind != IssueMissingBasis {
			t.Errorf("%s: issues = %+v, want missing-basis", tag, r.Issues)
		}
	}
}
