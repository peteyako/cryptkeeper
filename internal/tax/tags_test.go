package tax

import (
	"testing"

	"crypto-portfolio/internal/core"
)

// tagged computes in with tx tagged tag.
func tagged(in Input, tx core.Tx, tag Tag) Result {
	in.From = from2026
	in.Tags = map[string]Tag{tx.ID: tag}
	in.Txs = append(in.Txs, tx)
	return Compute(in)
}

func TestTagIncome(t *testing.T) {
	tx := mk("r", "w", at(2026, 3, 1, 0), core.TxReceive, lg("spl:JUP", 100, usd(80)))
	r := tagged(Input{}, tx, TagIncome)
	if len(r.Income) != 1 || r.Income[0].Type != "income" {
		t.Fatalf("income = %+v, want one income", r.Income)
	}
	near(t, "income value", r.Income[0].ValueUSD, 80)
	l := lotsIn(r, "w", "spl:JUP")
	if len(l) != 1 {
		t.Fatalf("lots = %+v", l)
	}
	near(t, "lot cost", l[0].CostUSD, 80)
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
	r = tagged(Input{}, tx, TagAirdrop)
	if len(r.Income) != 1 || r.Income[0].Type != "airdrop" || len(r.Issues) != 0 {
		t.Errorf("airdrop: income = %+v issues = %+v", r.Income, r.Issues)
	}
}

func giftReceivedTx() core.Tx {
	return mk("r", "w", at(2026, 3, 1, 0), core.TxReceive, lg("BTC", 0.01, usd(900)))
}

func TestTagGiftReceivedOverride(t *testing.T) {
	tx := giftReceivedTx()
	for _, tag := range []Tag{TagGiftReceived, TagOwnWallet} {
		r := tagged(Input{Prices: map[string]float64{PriceKey(tx.ID, 0): 400}}, tx, tag)
		l := lotsIn(r, "w", "BTC")
		if len(l) != 1 {
			t.Fatalf("%s: lots = %+v", tag, l)
		}
		near(t, "lot cost", l[0].CostUSD, 400)
		if l[0].Flag != "" || len(r.Issues) != 0 || len(r.Income) != 0 {
			t.Errorf("%s: lot = %+v issues = %+v income = %+v, want clean", tag, l[0], r.Issues, r.Income)
		}
	}
}

func TestTagGiftReceivedNoBasis(t *testing.T) {
	tx := giftReceivedTx()
	for _, tag := range []Tag{TagGiftReceived, TagOwnWallet} {
		r := tagged(Input{}, tx, tag)
		l := lotsIn(r, "w", "BTC")
		if len(l) != 1 || l[0].CostUSD != 0 || l[0].Flag != FlagMissingBasis {
			t.Fatalf("%s: lots = %+v, want cost 0 FlagMissingBasis", tag, l)
		}
		if len(r.Issues) != 1 || r.Issues[0].Kind != IssueMissingBasis || len(r.Income) != 0 {
			t.Errorf("%s: issues = %+v income = %+v", tag, r.Issues, r.Income)
		}
	}
}

func TestTagOwnWalletSend(t *testing.T) {
	tx := mk("s", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200)))
	r := tagged(Input{Opening: []Lot{lot("w", "SOL", 3, 300, at(2025, 5, 1, 0))}}, tx, TagOwnWallet)
	u := lotsIn(r, Untracked, "SOL")
	if len(u) != 1 {
		t.Fatalf("untracked lots = %+v", u)
	}
	near(t, "untracked amount", u[0].Amount, 1)
	near(t, "untracked cost", u[0].CostUSD, 100)
	if !u[0].Acquired.Equal(at(2025, 5, 1, 0)) || u[0].TxID != "opening" {
		t.Errorf("untracked lot = %+v, want acquired 2025-05-01", u[0])
	}
	near(t, "w held", heldIn(r, "w", "SOL"), 2)
	if len(r.Disposals) != 0 || len(r.Issues) != 0 {
		t.Errorf("disposals = %+v issues = %+v, want none", r.Disposals, r.Issues)
	}
}

func TestTagOwnWalletSendShortfall(t *testing.T) {
	tx := mk("s", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200)))
	r := tagged(Input{Opening: []Lot{lot("w", "SOL", 0.4, 40, at(2025, 5, 1, 0))}}, tx, TagOwnWallet)
	u := lotsIn(r, Untracked, "SOL")
	if len(u) != 2 {
		t.Fatalf("untracked lots = %+v, want moved + shortfall", u)
	}
	near(t, "short amount", u[1].Amount, 0.6)
	if u[1].CostUSD != 0 || u[1].Flag != FlagMissingBasis {
		t.Errorf("shortfall lot = %+v", u[1])
	}
	if len(r.Issues) != 1 || r.Issues[0].Kind != IssueMissingBasis || len(r.Disposals) != 0 {
		t.Errorf("issues = %+v disposals = %+v", r.Issues, r.Disposals)
	}
}

func TestTagGiftAndLost(t *testing.T) {
	for tag, note := range map[Tag]string{TagGift: "Gift", TagLost: "Lost"} {
		tx := mk("s", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -2, usd(-400)))
		r := tagged(Input{Opening: []Lot{
			lot("w", "SOL", 1, 50, at(2025, 1, 1, 0)),
			lot("w", "SOL", 2, 200, at(2025, 6, 1, 0)),
		}}, tx, tag)
		if len(r.Disposals) != 0 || len(r.Issues) != 0 {
			t.Errorf("%s: disposals = %+v issues = %+v, want none", tag, r.Disposals, r.Issues)
		}
		if len(r.GiftsLost) != 2 {
			t.Fatalf("%s: giftsLost = %+v, want 2 pieces", tag, r.GiftsLost)
		}
		for _, g := range r.GiftsLost {
			if g.Note != note || g.ProceedsUSD != 0 || g.GainUSD != 0 {
				t.Errorf("%s: row = %+v", tag, g)
			}
		}
		near(t, "piece 0 cost", r.GiftsLost[0].CostUSD, 50)
		near(t, "piece 1 cost", r.GiftsLost[1].CostUSD, 100)
		near(t, "held", heldIn(r, "w", "SOL"), 1)
	}
}

func TestTagSpamIgnoresFee(t *testing.T) {
	tx := withFee(mk("s", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200))), lg("SOL", -0.001, usd(-0.2)))
	r := tagged(Input{Opening: []Lot{lot("w", "SOL", 3, 300, at(2025, 5, 1, 0))}}, tx, TagSpam)
	if len(r.Disposals) != 0 || len(r.Issues) != 0 || len(r.GiftsLost) != 0 {
		t.Errorf("result = %+v, want nothing", r)
	}
	near(t, "held", heldIn(r, "w", "SOL"), 3)
}

func TestTagSaleClearsIssue(t *testing.T) {
	tx := mk("s", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200)))
	in := Input{Opening: []Lot{lot("w", "SOL", 1, 100, at(2025, 5, 1, 0))}}
	plain := Compute(Input{From: from2026, Opening: in.Opening, Txs: []core.Tx{tx}})
	if len(plain.Issues) != 1 || plain.Issues[0].Kind != IssueUnmatchedSend {
		t.Fatalf("untagged issues = %+v", plain.Issues)
	}
	r := tagged(in, tx, TagSale)
	if len(r.Disposals) != 1 || len(r.Issues) != 0 {
		t.Fatalf("disposals = %+v issues = %+v", r.Disposals, r.Issues)
	}
	near(t, "gain", r.Disposals[0].GainUSD, 100)
	rec := mk("r", "w", at(2026, 3, 2, 0), core.TxReceive, lg("SOL", 1, usd(150)))
	r = tagged(Input{}, rec, TagPurchase)
	if l := lotsIn(r, "w", "SOL"); len(l) != 1 || l[0].CostUSD != 150 || len(r.Issues) != 0 {
		t.Errorf("purchase: lots = %+v issues = %+v", l, r.Issues)
	}
}

func TestTagWrongDirectionFallsBack(t *testing.T) {
	rec := mk("r", "w", at(2026, 3, 1, 0), core.TxReceive, lg("SOL", 1, usd(150)))
	r := tagged(Input{}, rec, TagGift)
	if l := lotsIn(r, "w", "SOL"); len(l) != 1 || l[0].CostUSD != 150 || len(r.Issues) != 0 || len(r.GiftsLost) != 0 {
		t.Errorf("gift on receive: lots = %+v issues = %+v", l, r.Issues)
	}
	snd := mk("s", "w", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200)))
	r = tagged(Input{Opening: []Lot{lot("w", "SOL", 1, 100, at(2025, 5, 1, 0))}}, snd, TagIncome)
	if len(r.Disposals) != 1 || len(r.Issues) != 0 || len(r.Income) != 0 {
		t.Errorf("income on send: disposals = %+v issues = %+v", r.Disposals, r.Issues)
	}
}

func TestTagIgnoredOnTrade(t *testing.T) {
	tx := mk("x", "w", at(2026, 3, 1, 0), core.TxSwap, lg("SOL", -1, usd(-200)), lg("spl:JUP", 100, usd(200)))
	r := tagged(Input{Opening: []Lot{lot("w", "SOL", 1, 100, at(2025, 5, 1, 0))}}, tx, TagSpam)
	if len(r.Disposals) != 1 || len(lotsIn(r, "w", "spl:JUP")) != 1 {
		t.Errorf("disposals = %+v lots = %+v, want the swap applied", r.Disposals, r.Lots)
	}
}

func TestTagIgnoredOnPeerTx(t *testing.T) {
	send, recv := linked(
		mk("s", "a", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200))),
		mk("r", "b", at(2026, 3, 1, 0), core.TxReceive, lg("SOL", 1, usd(200))),
	)
	r := Compute(Input{
		From: from2026, Opening: []Lot{lot("a", "SOL", 1, 100, at(2025, 5, 1, 0))},
		Txs: []core.Tx{send, recv}, Tags: map[string]Tag{send.ID: TagSpam},
	})
	near(t, "b held", heldIn(r, "b", "SOL"), 1)
}
