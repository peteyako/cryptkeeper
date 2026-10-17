package tax

import (
	"cmp"
	"math"
	"reflect"
	"slices"
	"strings"
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

// With nothing closed (From zero) an undated manual lot applies at the start of history,
// dated at the first tx (never year 0001) with IssueUndatedLot.
func TestNoClosedYearStillAppliesUndatedManual(t *testing.T) {
	r := Compute(Input{
		Manual: []Lot{lot("btc", "BTC", 1, 50000, time.Time{})},
		Txs: []core.Tx{mk("s1", "btc", at(2026, 1, 15, 0), core.TxTrade,
			lg("BTC", -0.5, usd(-20000)), lg("USD", 20000, nil))},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "" || !r.Disposals[0].Acquired.Equal(at(2026, 1, 15, 0)) ||
		r.Disposals[0].Term != ShortTerm {
		t.Fatalf("disposals = %+v, want one from the undated manual lot", r.Disposals)
	}
	if len(r.Issues) != 1 || len(issuesOf(r, IssueUndatedLot)) != 1 {
		t.Errorf("issues = %+v, want one undated-lot", r.Issues)
	}
	near(t, "cost", r.Disposals[0].CostUSD, 25000)
	near(t, "BTC held", heldIn(r, "btc", "BTC"), 0.5)
}

// from2027 is the start of the open year once 2026 is closed, in UTC.
var from2027 = at(2027, 1, 1, 0)

// Once a year is closed, the manual lots and write-offs before From are already in the
// opening lots and parked balances (the closed year's closing ones): they are not applied
// again. An undated manual lot counts as before From. Those at or after From still apply.
func TestManualAndWriteOffBeforeFromDropped(t *testing.T) {
	manual2026 := lot("btc", "BTC", 1, 50000, at(2026, 2, 1, 0))
	r := Compute(Input{
		From: from2027,
		Opening: []Lot{
			manual2026, // the 2026 manual lot, carried into the closing lots of 2026
			lot("w", "SOL", 6, 600, at(2025, 1, 1, 0)),
		},
		OpeningParked: []Parked{{Wallet: "w", Asset: "SOL", Amount: 6}},
		Manual: []Lot{
			manual2026,
			lot("btc", "BTC", 3, 900, time.Time{}),           // undated: applied when nothing was closed
			lot("btc", "BTC", 0.25, 100, from2027),           // exactly at From: applies
			lot("btc", "BTC", 0.5, 30000, at(2027, 3, 1, 0)), // in the open year: applies
		},
		WriteOffs: []WriteOff{
			{Wallet: "w", Asset: "SOL", Amount: 6, Date: at(2026, 6, 1, 0)}, // already in 2026
			{Wallet: "w", Asset: "SOL", Amount: 1, Date: at(2027, 4, 1, 0), ProceedsUSD: 5},
		},
	})
	btc := lotsIn(r, "btc", "BTC")
	if len(btc) != 3 {
		t.Fatalf("BTC lots = %+v, want the opening lot, the From lot and the 2027 lot", btc)
	}
	near(t, "BTC held", heldIn(r, "btc", "BTC"), 1.75)
	if !btc[0].Acquired.Equal(at(2026, 2, 1, 0)) || !btc[1].Acquired.Equal(from2027) || !btc[2].Acquired.Equal(at(2027, 3, 1, 0)) {
		t.Errorf("BTC lots = %+v, want acquired 2026-02-01, 2027-01-01 and 2027-03-01", btc)
	}
	for _, d := range r.Disposals {
		if d.Sold.Before(from2027) {
			t.Errorf("disposal %+v is dated before From", d)
		}
	}
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want only the 2027 write-off", r.Disposals)
	}
	d := r.Disposals[0]
	near(t, "write-off amount", d.Amount, 1)
	near(t, "write-off cost", d.CostUSD, 100)
	near(t, "write-off proceeds", d.ProceedsUSD, 5)
	if d.TxID != "writeoff:1" || !d.Sold.Equal(at(2027, 4, 1, 0)) {
		t.Errorf("write-off = %+v, want writeoff:1 on 2027-04-01", d)
	}
	near(t, "SOL held", heldIn(r, "w", "SOL"), 5)
	if len(r.Parked) != 1 || r.Parked[0] != (Parked{Wallet: "w", Asset: "SOL", Amount: 5}) {
		t.Errorf("parked = %+v, want SOL 5 in w (6 opening, minus only the 2027 write-off)", r.Parked)
	}
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

// idSet is a set of tx IDs; nil when there are none.
func idSet(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// Closing 2025 and 2026 in turn and then computing 2027, each year from the one before (its
// closing lots, parked balances and carried peers) with the same tax.yaml and activity,
// ends with exactly the lots of one pass over everything, and the three years' disposals
// are the one pass's, row for row. Pairs straddle both year ends in both orders, and one
// late sender's difference is its own fee. The only row that differs is the one a close
// dates at the half inside its year (clamped): only its date moves.
func TestCloseRoundTrip(t *testing.T) {
	dt := func(y, m, d, h, min int) time.Time { return time.Date(y, time.Month(m), d, h, min, 0, 0, time.UTC) }
	// 2025/2026: a send first (a → b), and a receive first whose late sender pays a
	// difference (a → c).
	send1, recv1 := linked(
		withFee(mk("send1", "a", dt(2025, 12, 31, 23, 58), core.TxTransfer, lg("BTC", -0.4, usd(-40000))), lg("BTC", -0.0001, usd(-10))),
		mk("recv1", "b", dt(2026, 1, 1, 0, 3), core.TxTransfer, lg("BTC", 0.4, usd(40000))),
	)
	send2, recv2 := linked(
		mk("send2", "a", dt(2026, 1, 1, 0, 1), core.TxTransfer, lg("SOL", -5, usd(-1000))),
		mk("recv2", "c", dt(2025, 12, 31, 23, 50), core.TxTransfer, lg("SOL", 4.9, usd(980))),
	)
	// 2026/2027: a receive first whose difference is the late sender's own fee (a → b), and
	// a send first (a → c).
	send3, recv3 := linked(
		withFee(mk("send3", "a", dt(2027, 1, 1, 0, 2), core.TxTransfer, lg("BTC", -0.4, usd(-40000))), lg("BTC", -0.0001, usd(-10))),
		mk("recv3", "b", dt(2026, 12, 31, 23, 55), core.TxTransfer, lg("BTC", 0.3999, usd(39990))),
	)
	send4, recv4 := linked(
		mk("send4", "a", dt(2026, 12, 31, 23, 59), core.TxTransfer, lg("ETH", -1, usd(-2500))),
		mk("recv4", "c", dt(2027, 1, 1, 0, 10), core.TxTransfer, lg("ETH", 1, usd(2500))),
	)
	dep := mk("dep", "a", at(2025, 6, 1, 0), core.TxSend, lg("SOL", -10, usd(-2000)))
	wd := mk("wd", "a", at(2027, 3, 1, 0), core.TxReceive, lg("SOL", 3, usd(750)))
	txs := []core.Tx{
		mk("buy", "a", at(2025, 3, 1, 0), core.TxTrade, lg("USD", -6000, nil), lg("SOL", 30, usd(6000))),
		dep, send1, recv1, send2, recv2,
		mk("sell", "b", at(2026, 2, 1, 0), core.TxTrade, lg("BTC", -0.2, usd(-22000)), lg("USD", 22000, nil)),
		send3, recv3, send4, recv4, wd,
		mk("eth", "c", at(2027, 5, 1, 0), core.TxTrade, lg("ETH", -0.5, usd(-1500)), lg("USD", 1500, nil)),
	}
	in := Input{
		Manual: []Lot{
			lot("a", "ETH", 2, 3000, time.Time{}),
			lot("a", "BTC", 1, 40000, at(2025, 2, 1, 0)),
			lot("c", "BTC", 0.5, 20000, at(2026, 4, 1, 0)),
			lot("c", "SOL", 1, 150, at(2027, 4, 1, 0)),
		},
		WriteOffs: []WriteOff{
			{Wallet: "a", Asset: "SOL", Amount: 4, Date: at(2025, 9, 1, 0), ProceedsUSD: 100},
			{Wallet: "a", Asset: "SOL", Amount: 2, Date: at(2026, 5, 1, 0)},
			{Wallet: "a", Asset: "SOL", Amount: 1, Date: at(2027, 6, 1, 0), ProceedsUSD: 5},
		},
		Txs:  txs,
		Tags: map[string]Tag{dep.ID: TagPosition, wd.ID: TagPosition},
	}

	close25 := in
	close25.To = from2026
	res2025 := Compute(close25)
	close26 := in
	close26.From, close26.To = from2026, from2027
	close26.Opening, close26.OpeningParked, close26.PairedBeforeFrom = res2025.Lots, res2025.Parked, idSet(res2025.CarriedPeers)
	res2026 := Compute(close26)
	open27 := in
	open27.From = from2027
	open27.Opening, open27.OpeningParked, open27.PairedBeforeFrom = res2026.Lots, res2026.Parked, idSet(res2026.CarriedPeers)
	res2027 := Compute(open27)
	onePass := Compute(in)

	if want := []string{"a:send2", "b:recv1"}; !slices.Equal(res2025.CarriedPeers, want) {
		t.Errorf("2025 CarriedPeers = %v, want %v", res2025.CarriedPeers, want)
	}
	if want := []string{"a:send3", "c:recv4"}; !slices.Equal(res2026.CarriedPeers, want) {
		t.Errorf("2026 CarriedPeers = %v, want %v", res2026.CarriedPeers, want)
	}
	if res2027.CarriedPeers != nil || onePass.CarriedPeers != nil {
		t.Errorf("CarriedPeers without To: 2027 %v, one pass %v, want none", res2027.CarriedPeers, onePass.CarriedPeers)
	}
	years := []struct {
		r        Result
		from, to time.Time
	}{{res2025, time.Time{}, from2026}, {res2026, from2026, from2027}, {res2027, from2027, time.Time{}}}
	for _, y := range years {
		for _, d := range y.r.Disposals {
			if d.Sold.Before(y.from) || (!y.to.IsZero() && !d.Sold.Before(y.to)) {
				t.Errorf("disposal %+v is outside [%v, %v)", d, y.from, y.to)
			}
		}
		// The undated ETH lot is flagged where it applies: the first year (nothing closed).
		wantUndated := 0
		if y.from.IsZero() {
			wantUndated = 1
		}
		if len(y.r.Issues) != wantUndated || len(issuesOf(y.r, IssueUndatedLot)) != wantUndated || len(y.r.Income) != 0 || len(y.r.GiftsLost) != 0 {
			t.Errorf("issues = %+v income = %+v gifts = %+v, want %d undated-lot only", y.r.Issues, y.r.Income, y.r.GiftsLost, wantUndated)
		}
	}
	if len(onePass.Issues) != 1 || len(issuesOf(onePass, IssueUndatedLot)) != 1 {
		t.Errorf("one pass issues = %+v, want the undated-lot one", onePass.Issues)
	}
	if !reflect.DeepEqual(res2027.Lots, onePass.Lots) {
		t.Errorf("2027 lots =\n%+v\nwant the one pass's\n%+v", res2027.Lots, onePass.Lots)
	}
	if !reflect.DeepEqual(res2027.Parked, onePass.Parked) || len(onePass.Parked) != 0 {
		t.Errorf("2027 parked = %+v, one pass %+v, want both empty", res2027.Parked, onePass.Parked)
	}

	// Row for row, once a clamped row (booked by a close on a carried peer, dated at the
	// half inside the year) gets its peer's own time back.
	when := map[string]time.Time{}
	for _, tx := range txs {
		when[tx.ID] = tx.Time
	}
	var chain []Disposal
	clamped := 0
	for _, y := range years {
		carried := idSet(y.r.CarriedPeers)
		for _, d := range y.r.Disposals {
			if carried[d.TxID] {
				if d.TxID != send2.ID || !d.Sold.Equal(recv2.Time) {
					t.Errorf("clamped row %+v, want a:send2's difference dated at c:recv2", d)
				}
				d.Sold = when[d.TxID]
				clamped++
			}
			chain = append(chain, d)
		}
	}
	if clamped != 1 {
		t.Errorf("clamped rows = %d, want 1 (a:send2's difference)", clamped)
	}
	slices.SortStableFunc(chain, func(a, b Disposal) int {
		return cmp.Or(a.Sold.Compare(b.Sold), strings.Compare(a.TxID, b.TxID))
	})
	if len(chain) != 8 || !reflect.DeepEqual(chain, onePass.Disposals) {
		t.Errorf("disposals of the three years =\n%+v\nwant the one pass's (8)\n%+v", chain, onePass.Disposals)
	}
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

func TestOpeningParkedSkipsDust(t *testing.T) {
	in := Input{From: from2026, OpeningParked: []Parked{{"a", "X", 0}, {"a", "Y", 1e-13}, {"a", "Z", -1}, {"a", "W", math.NaN()}, {"a", "V", 2}}}
	r := Compute(in)
	if len(r.Parked) != 1 || r.Parked[0].Asset != "V" {
		t.Fatalf("Parked = %+v, want only V", r.Parked)
	}
}

func TestDisposalCarriesLotTxID(t *testing.T) {
	buy := mk("b", "cb", at(2026, 2, 1, 0), core.TxTrade, lg("BTC", 1, nil), lg("USD", -100, usd(-100)))
	sell := mk("s", "cb", at(2026, 3, 1, 0), core.TxTrade, lg("BTC", -1, usd(-150)), lg("USD", 150, usd(150)))
	r := Compute(Input{From: from2026, Txs: []core.Tx{buy, sell}})
	var got []Disposal
	for _, d := range r.Disposals {
		if d.Asset == "BTC" {
			got = append(got, d)
		}
	}
	if len(got) != 1 || got[0].LotTxID != "cb:b" {
		t.Fatalf("direct sale = %+v, want LotTxID cb:b", got)
	}

	send, recv := linked(
		mk("send", "cb", at(2026, 2, 5, 0), core.TxTransfer, lg("BTC", -1, usd(-110))),
		mk("recv", "x", at(2026, 2, 5, 1), core.TxTransfer, lg("BTC", 1, nil)),
	)
	sell2 := mk("s2", "x", at(2026, 3, 1, 0), core.TxTrade, lg("BTC", -1, usd(-150)), lg("USD", 150, usd(150)))
	r = Compute(Input{From: from2026, Txs: []core.Tx{buy, send, recv, sell2}})
	got = nil
	for _, d := range r.Disposals {
		if d.Asset == "BTC" && d.Wallet == "x" {
			got = append(got, d)
		}
	}
	if len(got) != 1 || got[0].LotTxID != "cb:b" {
		t.Fatalf("after transfer = %+v, want LotTxID cb:b", got)
	}
}
