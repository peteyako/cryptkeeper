package tax

import (
	"slices"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

// totalGain sums GainUSD over the disposals.
func totalGain(r Result) float64 {
	var g float64
	for _, d := range r.Disposals {
		g += d.GainUSD
	}
	return g
}

// TestQuietAssetTradesKeepTheirGain: an auto-hidden token (Quiet) is not spam. Buying it
// for 10 SOL and selling most of it for 30 SOL keeps both gains.
func TestQuietAssetTradesKeepTheirGain(t *testing.T) {
	txs := []core.Tx{
		mk("buy", "sol", at(2026, 2, 1, 0), core.TxSwap, lg("SOL", -10, usd(-1500)), lg("spl:MEME", 1e6, nil)),
		mk("sell", "sol", at(2026, 5, 1, 0), core.TxSwap, lg("spl:MEME", -9.99e5, nil), lg("SOL", 30, usd(4500))),
	}
	open := []Lot{lot("sol", "SOL", 10, 1000, at(2025, 1, 1, 0))}
	r := Compute(Input{Opening: open, Txs: txs, Quiet: map[string]bool{"spl:MEME": true}})
	near(t, "total gain", totalGain(r), 3501.50)
	if len(r.Issues) != 0 {
		t.Errorf("issues: %+v", r.Issues)
	}
	near(t, "MEME left", heldIn(r, "sol", "spl:MEME"), 1000)
}

// TestQuietUnpricedAirdropOpensLotSilently: an unpriced receive of a quiet asset is a
// cost-0 lot with no unmatched-receive and no missing-price issue.
func TestQuietUnpricedAirdropOpensLotSilently(t *testing.T) {
	r := Compute(Input{
		Txs:   []core.Tx{mk("drop", "sol", at(2026, 3, 1, 0), core.TxReceive, lg("spl:JUNK", 500, nil))},
		Quiet: map[string]bool{"spl:JUNK": true},
	})
	ls := lotsIn(r, "sol", "spl:JUNK")
	if len(ls) != 1 || ls[0].Amount != 500 || ls[0].CostUSD != 0 {
		t.Fatalf("lots: %+v", ls)
	}
	if len(r.Issues) != 0 {
		t.Errorf("issues: %+v", r.Issues)
	}
}

// TestQuietOnlySilencesWorthlessReceives: a priced receive, a send and a reward of a quiet
// asset raise their usual issues.
func TestQuietOnlySilencesWorthlessReceives(t *testing.T) {
	r := Compute(Input{
		Txs: []core.Tx{
			mk("in", "sol", at(2026, 3, 1, 0), core.TxReceive, lg("spl:JUNK", 500, usd(25))),
			mk("out", "sol", at(2026, 4, 1, 0), core.TxSend, lg("spl:JUNK", -100, nil)),
			mk("rew", "sol", at(2026, 5, 1, 0), core.TxReward, lg("spl:JUNK", 7, nil)),
		},
		Quiet: map[string]bool{"spl:JUNK": true},
	})
	if n := len(issuesOf(r, IssueUnmatchedReceive)); n != 1 {
		t.Errorf("unmatched receives %d, want 1", n)
	}
	if n := len(issuesOf(r, IssueUnmatchedSend)); n != 1 {
		t.Errorf("unmatched sends %d, want 1", n)
	}
	if n := len(issuesOf(r, IssueMissingPrice)); n != 2 { // the send and the reward
		t.Errorf("missing prices %d, want 2: %+v", n, r.Issues)
	}
}

// TestSpamStillDropped: a hidden mint (Spam) is still ignored entirely.
func TestSpamStillDropped(t *testing.T) {
	r := Compute(Input{
		Opening: []Lot{lot("sol", "SOL", 10, 1000, at(2025, 1, 1, 0))},
		Txs: []core.Tx{
			mk("buy", "sol", at(2026, 2, 1, 0), core.TxSwap, lg("SOL", -10, usd(-1500)), lg("spl:MEME", 1e6, nil)),
			mk("drop", "sol", at(2026, 3, 1, 0), core.TxReceive, lg("spl:MEME", 5, nil)),
		},
		Spam: map[string]bool{"spl:MEME": true},
	})
	if got := lotsIn(r, "sol", "spl:MEME"); len(got) != 0 {
		t.Errorf("spam lots: %+v", got)
	}
	if len(r.Issues) != 0 {
		t.Errorf("issues: %+v", r.Issues)
	}
}

// untrackedSOL is an opening lot of SOL held outside the tracked wallets.
func untrackedSOL(amount, cost float64) Lot {
	return Lot{Wallet: Untracked, Asset: "SOL", Amount: amount, CostUSD: cost, Acquired: at(2024, 6, 1, 0), Various: true, TxID: "koinly", Flag: FlagTransferDate}
}

// TestOwnWalletReceiveDrawsUntrackedLots: an own-wallet receive takes the untracked lots
// first, keeping their date, cost and flags.
func TestOwnWalletReceiveDrawsUntrackedLots(t *testing.T) {
	rx := mk("rx", "hot", at(2026, 3, 1, 0), core.TxReceive, lg("SOL", 3, usd(450)))
	r := Compute(Input{
		From: from2026, Opening: []Lot{untrackedSOL(5, 500)}, Txs: []core.Tx{rx},
		Tags: map[string]Tag{rx.ID: TagOwnWallet},
	})
	got := lotsIn(r, "hot", "SOL")
	if len(got) != 1 || got[0].Amount != 3 || got[0].CostUSD != 300 || !got[0].Acquired.Equal(at(2024, 6, 1, 0)) ||
		!got[0].Various || got[0].Flag != FlagTransferDate || got[0].TxID != "koinly" {
		t.Fatalf("hot lots: %+v", got)
	}
	near(t, "untracked left", heldIn(r, Untracked, "SOL"), 2)
	if len(r.Issues) != 0 {
		t.Errorf("issues: %+v", r.Issues)
	}
}

// TestOwnWalletReceiveShortfallIsMissingBasis: what the untracked lots cannot cover is a
// $0 missing-basis lot dated at the receive.
func TestOwnWalletReceiveShortfallIsMissingBasis(t *testing.T) {
	rx := mk("rx", "hot", at(2026, 3, 1, 0), core.TxReceive, lg("SOL", 8, usd(1200)))
	r := Compute(Input{
		From: from2026, Opening: []Lot{untrackedSOL(5, 500)}, Txs: []core.Tx{rx},
		Tags: map[string]Tag{rx.ID: TagOwnWallet},
	})
	got := lotsIn(r, "hot", "SOL")
	if len(got) != 2 {
		t.Fatalf("hot lots: %+v", got)
	}
	short := got[1]
	if short.Amount != 3 || short.CostUSD != 0 || !short.Acquired.Equal(rx.Time) || short.Flag != FlagMissingBasis || short.TxID != rx.ID {
		t.Errorf("shortfall lot: %+v", short)
	}
	if is := issuesOf(r, IssueMissingBasis); len(is) != 1 || is[0].Amount != 3 {
		t.Errorf("missing-basis issues: %+v", is)
	}
	if heldIn(r, Untracked, "SOL") != 0 {
		t.Errorf("untracked not drained: %+v", r.Lots)
	}
}

// unpairedHalf is a transfer half whose peer is not in the activity.
func unpairedHalf(id, wallet string, t time.Time, legs ...core.Leg) core.Tx {
	tx := mk(id, wallet, t, core.TxTransfer, legs...)
	tx.PeerID = "elsewhere:" + id
	return tx
}

// TestTagOnUnpairedTransferHalf: a tag on a transfer half whose peer is missing applies as
// on a plain send or receive; without one the half stays unpaired.
func TestTagOnUnpairedTransferHalf(t *testing.T) {
	out := unpairedHalf("out", "hot", at(2026, 3, 1, 0), lg("SOL", -2, usd(-300)))
	in := unpairedHalf("in", "cold", at(2026, 4, 1, 0), lg("SOL", 4, usd(640)))
	bare := unpairedHalf("bare", "cold", at(2026, 5, 1, 0), lg("SOL", 1, usd(150)))
	r := Compute(Input{
		From: from2026, Opening: []Lot{lot("hot", "SOL", 5, 500, at(2025, 1, 1, 0))},
		Txs:  []core.Tx{out, in, bare},
		Tags: map[string]Tag{out.ID: TagOwnWallet, in.ID: TagPurchase},
	})
	near(t, "hot SOL", heldIn(r, "hot", "SOL"), 3)
	near(t, "untracked SOL", heldIn(r, Untracked, "SOL"), 2)
	if got := lotsIn(r, "cold", "SOL"); len(got) != 1 || got[0].CostUSD != 640 {
		t.Errorf("cold lots: %+v", got)
	}
	if is := issuesOf(r, IssueTransferUnpaired); len(is) != 1 || is[0].TxID != bare.ID {
		t.Errorf("unpaired issues: %+v", is)
	}
	want := []string{bare.ID, in.ID, out.ID}
	slices.Sort(want)
	if !slices.Equal(r.Unpaired, want) {
		t.Errorf("Unpaired = %v, want %v", r.Unpaired, want)
	}
}

// TestSpamTagOnUnpairedHalfIgnoresIt: spam on an unpaired half ignores it, fee included.
func TestSpamTagOnUnpairedHalfIgnoresIt(t *testing.T) {
	out := withFee(unpairedHalf("out", "hot", at(2026, 3, 1, 0), lg("SOL", -2, usd(-300))), lg("SOL", -0.01, usd(-1.5)))
	r := Compute(Input{
		From: from2026, Opening: []Lot{lot("hot", "SOL", 5, 500, at(2025, 1, 1, 0))},
		Txs: []core.Tx{out}, Tags: map[string]Tag{out.ID: TagSpam},
	})
	near(t, "hot SOL", heldIn(r, "hot", "SOL"), 5)
	if len(r.Disposals) != 0 || len(r.Issues) != 0 {
		t.Errorf("disposals %+v issues %+v", r.Disposals, r.Issues)
	}
	// still listed as unpaired, so the review queue keeps offering tag options on it
	if !slices.Equal(r.Unpaired, []string{out.ID}) {
		t.Errorf("Unpaired = %v, want [%s]", r.Unpaired, out.ID)
	}
}

// TestTagOnPairedHalfIgnored: a tag on a half whose pair is intact changes nothing.
func TestTagOnPairedHalfIgnored(t *testing.T) {
	out := mk("t1", "hot", at(2026, 3, 1, 0), core.TxTransfer, lg("SOL", -2, usd(-300)))
	in := mk("t1", "cold", at(2026, 3, 1, 1), core.TxTransfer, lg("SOL", 2, usd(300)))
	out.PeerID, in.PeerID = in.ID, out.ID
	r := Compute(Input{
		From: from2026, Opening: []Lot{lot("hot", "SOL", 5, 500, at(2025, 1, 1, 0))},
		Txs: []core.Tx{out, in}, Tags: map[string]Tag{out.ID: TagGift, in.ID: TagIncome},
	})
	near(t, "cold SOL", heldIn(r, "cold", "SOL"), 2)
	if len(r.GiftsLost) != 0 || len(r.Income) != 0 || len(r.Unpaired) != 0 {
		t.Errorf("gifts %+v income %+v unpaired %v", r.GiftsLost, r.Income, r.Unpaired)
	}
}

// TestUndatedManualLotDatedAtReplayStart: with nothing closed an undated manual lot is
// dated at the first replayed tx (so its sale is short-term, never year 0001 long-term)
// and raises IssueUndatedLot.
func TestUndatedManualLotDatedAtReplayStart(t *testing.T) {
	sell := mk("s", "hot", at(2026, 6, 1, 0), core.TxSwap, lg("SOL", -1, usd(-200)), lg("USDC", 200, usd(200)))
	first := mk("f", "hot", at(2026, 3, 1, 0), core.TxReceive, lg("USDC", 5, usd(5)))
	r := Compute(Input{
		Manual: []Lot{{Wallet: "hot", Asset: "SOL", Amount: 1, CostUSD: 50}},
		Txs:    []core.Tx{sell, first},
	})
	var sol []Disposal
	for _, d := range r.Disposals {
		if d.Asset == "SOL" {
			sol = append(sol, d)
		}
	}
	if len(sol) != 1 || sol[0].Term != ShortTerm || !sol[0].Acquired.Equal(first.Time) || sol[0].CostUSD != 50 {
		t.Fatalf("SOL disposals: %+v", sol)
	}
	is := issuesOf(r, IssueUndatedLot)
	if len(is) != 1 || is[0].Wallet != "hot" || is[0].Asset != "SOL" || is[0].Amount != 1 || !is[0].Time.Equal(first.Time) {
		t.Errorf("undated issues: %+v", is)
	}
}
