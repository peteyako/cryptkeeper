package tax

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"cryptkeeper/internal/core"
)

// linked links send and recv as the two halves of one transfer, as
// activity.MatchTransfers does (the kinds are left as given).
func linked(send, recv core.Tx) (core.Tx, core.Tx) {
	send.PeerID, send.PeerSource = recv.ID, recv.SourceID
	recv.PeerID, recv.PeerSource = send.ID, send.SourceID
	return send, recv
}

// btcMove is a BTC transfer from wallet "a" to wallet "b", sent at sendAt with a network
// fee and received at recvAt.
func btcMove(sendAt, recvAt time.Time) Input {
	send, recv := linked(
		withFee(mk("send", "a", sendAt, core.TxTransfer, lg("BTC", -0.6, usd(-60000))), lg("BTC", -0.0001, usd(-10))),
		mk("recv", "b", recvAt, core.TxTransfer, lg("BTC", 0.6, nil)),
	)
	return Input{
		From: from2026,
		Opening: []Lot{
			lot("a", "BTC", 0.5, 10000, at(2024, 6, 1, 0)),
			lot("a", "BTC", 0.5, 30000, at(2025, 9, 1, 0)),
		},
		Txs: []core.Tx{recv, send},
	}
}

// checkBTCMove checks the lots and the single fee disposal, sold at sold with term, that
// btcMove leads to.
func checkBTCMove(t *testing.T, r Result, sold time.Time, term Term) {
	t.Helper()
	b := lotsIn(r, "b", "BTC")
	if len(b) != 2 {
		t.Fatalf("b lots = %+v, want 2", b)
	}
	near(t, "b[0] amount", b[0].Amount, 0.5)
	near(t, "b[0] cost", b[0].CostUSD, 10000)
	near(t, "b[1] amount", b[1].Amount, 0.1)
	near(t, "b[1] cost", b[1].CostUSD, 6000)
	if !b[0].Acquired.Equal(at(2024, 6, 1, 0)) || !b[1].Acquired.Equal(at(2025, 9, 1, 0)) {
		t.Errorf("b lots = %+v, want acquired 2024-06-01 and 2025-09-01", b)
	}
	if b[0].TxID != "opening" || b[1].TxID != "opening" || b[0].Flag != "" || b[1].Flag != "" {
		t.Errorf("b lots = %+v, want the opening lots' TxID and no flag", b)
	}
	a := lotsIn(r, "a", "BTC")
	if len(a) != 1 {
		t.Fatalf("a lots = %+v, want 1", a)
	}
	near(t, "a amount", a[0].Amount, 0.3999)
	near(t, "a cost", a[0].CostUSD, 23994)
	if !a[0].Acquired.Equal(at(2025, 9, 1, 0)) {
		t.Errorf("a lot = %+v, want acquired 2025-09-01", a[0])
	}
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want only the fee", r.Disposals)
	}
	d := r.Disposals[0]
	near(t, "fee amount", d.Amount, 0.0001)
	near(t, "fee cost", d.CostUSD, 6)
	near(t, "fee proceeds", d.ProceedsUSD, 10)
	near(t, "fee gain", d.GainUSD, 4)
	if d.Note != "Fee" || d.Term != term || d.Wallet != "a" || d.TxID != "a:send" || !d.Sold.Equal(sold) {
		t.Errorf("fee = %+v, want a %s-term Fee from a:send in a at %v", d, term, sold)
	}
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

func TestTransferMovesLotsKeepingDates(t *testing.T) {
	when := at(2026, 2, 1, 0)
	checkBTCMove(t, Compute(btcMove(when, when)), when, ShortTerm)
}

func TestTransferReceivedLessIsFee(t *testing.T) {
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("SOL", -1, usd(-200))),
		mk("recv", "b", at(2026, 3, 1, 1), core.TxTransfer, lg("SOL", 0.99, usd(198))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	fees := disposalsNoted(r, "Fee")
	if len(fees) != 1 || len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v, want one Fee", r.Disposals)
	}
	f := fees[0]
	near(t, "fee amount", f.Amount, 0.01)
	near(t, "fee cost", f.CostUSD, 1)
	near(t, "fee proceeds", f.ProceedsUSD, 2)
	near(t, "fee gain", f.GainUSD, 1)
	if f.Wallet != "a" || f.TxID != "a:send" || !f.Sold.Equal(at(2026, 3, 1, 0)) {
		t.Errorf("fee = %+v, want from a:send in a", f)
	}
	b := lotsIn(r, "b", "SOL")
	if len(b) != 1 {
		t.Fatalf("b lots = %+v, want 1", b)
	}
	near(t, "b amount", b[0].Amount, 0.99)
	near(t, "b cost", b[0].CostUSD, 99)
	if !b[0].Acquired.Equal(at(2025, 6, 1, 0)) {
		t.Errorf("b lot = %+v, want acquired 2025-06-01", b[0])
	}
	if a := lotsIn(r, "a", "SOL"); len(a) != 0 {
		t.Errorf("a lots = %+v, want none", a)
	}
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

func TestTransferShortfallMissingBasis(t *testing.T) {
	when := at(2026, 3, 1, 0)
	send, recv := linked(
		mk("send", "a", when, core.TxTransfer, lg("BTC", -0.5, usd(-50000))),
		mk("recv", "b", when, core.TxTransfer, lg("BTC", 0.5, usd(50000))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "BTC", 0.2, 8000, at(2025, 3, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	b := lotsIn(r, "b", "BTC")
	if len(b) != 2 {
		t.Fatalf("b lots = %+v, want 2", b)
	}
	near(t, "b[0] amount", b[0].Amount, 0.2)
	near(t, "b[0] cost", b[0].CostUSD, 8000)
	if !b[0].Acquired.Equal(at(2025, 3, 1, 0)) || b[0].Flag != "" || b[0].TxID != "opening" {
		t.Errorf("b[0] = %+v, want the opening lot, unflagged", b[0])
	}
	near(t, "b[1] amount", b[1].Amount, 0.3)
	near(t, "b[1] cost", b[1].CostUSD, 0)
	if !b[1].Acquired.Equal(when) || b[1].Flag != FlagMissingBasis || b[1].TxID != "b:recv" {
		t.Errorf("b[1] = %+v, want acquired at the transfer by b:recv, flagged missing-basis", b[1])
	}
	if a := lotsIn(r, "a", "BTC"); len(a) != 0 {
		t.Errorf("a lots = %+v, want none", a)
	}
	if len(r.Disposals) != 0 {
		t.Errorf("disposals = %+v, want none", r.Disposals)
	}
	missing := issuesOf(r, IssueMissingBasis)
	if len(missing) != 1 || len(r.Issues) != 1 {
		t.Fatalf("issues = %+v, want one missing-basis", r.Issues)
	}
	near(t, "issue amount", missing[0].Amount, 0.3)
	if missing[0].Asset != "BTC" || missing[0].Wallet != "a" || missing[0].TxID != "a:send" {
		t.Errorf("issue = %+v, want BTC short in a at a:send", missing[0])
	}
}

func TestTransferReceiveBeforeSend(t *testing.T) {
	sent := at(2026, 2, 1, 12)
	checkBTCMove(t, Compute(btcMove(sent, sent.Add(-5*time.Minute))), sent, ShortTerm)
}

func TestTransferPeerMissing(t *testing.T) {
	recv := mk("recv", "b", at(2026, 3, 1, 0), core.TxTransfer, lg("BTC", 0.6, usd(60000)))
	recv.PeerID, recv.PeerSource = "a:gone", "a"
	r := Compute(Input{From: from2026, Txs: []core.Tx{recv}})
	if len(r.Lots) != 0 || len(r.Disposals) != 0 {
		t.Fatalf("lots = %+v disposals = %+v, want none", r.Lots, r.Disposals)
	}
	unpaired := issuesOf(r, IssueTransferUnpaired)
	if len(unpaired) != 1 || len(r.Issues) != 1 {
		t.Fatalf("issues = %+v, want one transfer-unpaired", r.Issues)
	}
	is := unpaired[0]
	near(t, "issue amount", is.Amount, 0.6)
	if is.Asset != "BTC" || is.Wallet != "b" || is.TxID != "b:recv" {
		t.Errorf("issue = %+v, want BTC in b from b:recv", is)
	}
}

// A side whose peer failed or links to another tx changes no lots and raises
// IssueTransferUnpaired; the peer is then handled on its own. (For a peer before From see
// TestTransferPeerBeforeFrom.)
func TestTransferUnpairedPeers(t *testing.T) {
	cases := []struct {
		name  string
		setup func(send, recv *core.Tx)
	}{
		{"peer failed", func(send, recv *core.Tx) { send.Status = core.TxFailed }},
		{"peer links elsewhere", func(send, recv *core.Tx) { send.PeerID = "c:other" }},
		{"peer not linked", func(send, recv *core.Tx) { send.PeerID = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			send, recv := linked(
				mk("send", "a", at(2026, 1, 1, 0), core.TxTransfer, lg("BTC", -0.5, usd(-50000))),
				mk("recv", "b", at(2026, 1, 1, 1), core.TxTransfer, lg("BTC", 0.5, usd(50000))),
			)
			c.setup(&send, &recv)
			r := Compute(Input{
				From:    from2026,
				Opening: []Lot{lot("a", "BTC", 1, 40000, at(2025, 3, 1, 0))},
				Txs:     []core.Tx{send, recv},
			})
			if b := lotsIn(r, "b", "BTC"); len(b) != 0 {
				t.Errorf("b lots = %+v, want none", b)
			}
			n := 0
			for _, is := range issuesOf(r, IssueTransferUnpaired) {
				if is.TxID == "b:recv" {
					n++
				}
			}
			if n != 1 {
				t.Errorf("issues = %+v, want one transfer-unpaired for b:recv", r.Issues)
			}
		})
	}
}

func TestTransferReceivedMoreIsAcquisition(t *testing.T) {
	recvAt := at(2026, 3, 1, 1)
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("SOL", -1, usd(-200))),
		mk("recv", "b", recvAt, core.TxTransfer, lg("SOL", 1.5, usd(300))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	b := lotsIn(r, "b", "SOL")
	if len(b) != 2 {
		t.Fatalf("b lots = %+v, want the moved lot and the extra", b)
	}
	near(t, "b[0] amount", b[0].Amount, 1)
	near(t, "b[0] cost", b[0].CostUSD, 100)
	near(t, "b[1] amount", b[1].Amount, 0.5)
	near(t, "b[1] cost", b[1].CostUSD, 100)
	if !b[1].Acquired.Equal(recvAt) || b[1].TxID != "b:recv" || b[1].Flag != "" {
		t.Errorf("b[1] = %+v, want acquired at the receive by b:recv", b[1])
	}
	if len(r.Disposals) != 0 || len(r.Issues) != 0 {
		t.Errorf("disposals = %+v issues = %+v, want none", r.Disposals, r.Issues)
	}
}

func TestTransferFeeMissingPrice(t *testing.T) {
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("spl:X", -100, nil)),
		mk("recv", "b", at(2026, 3, 1, 0), core.TxTransfer, lg("spl:X", 99, nil)),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "spl:X", 100, 10, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	near(t, "b held", heldIn(r, "b", "spl:X"), 99)
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want one Fee", r.Disposals)
	}
	near(t, "fee amount", r.Disposals[0].Amount, 1)
	near(t, "fee cost", r.Disposals[0].CostUSD, 0.1)
	near(t, "fee proceeds", r.Disposals[0].ProceedsUSD, 0)
	missing := issuesOf(r, IssueMissingPrice)
	if len(missing) != 1 || len(r.Issues) != 1 {
		t.Fatalf("issues = %+v, want one missing-price", r.Issues)
	}
	near(t, "issue amount", missing[0].Amount, 1)
	if missing[0].TxID != "a:send" || missing[0].Wallet != "a" {
		t.Errorf("issue = %+v, want a:send in a", missing[0])
	}
}

// Each side's own network fee is disposed exactly once, by that side's wallet, also when
// the receive comes first.
func TestTransferEachSideFeeOnce(t *testing.T) {
	sent := at(2026, 3, 1, 12)
	send, recv := linked(
		withFee(mk("send", "a", sent, core.TxTransfer, lg("SOL", -2, usd(-400))), lg("SOL", -0.01, usd(-2))),
		withFee(mk("recv", "b", sent.Add(-5*time.Minute), core.TxTransfer, lg("SOL", 2, usd(400))), lg("SOL", -0.001, usd(-0.2))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 10, 1000, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	fees := disposalsNoted(r, "Fee")
	if len(fees) != 2 || len(r.Disposals) != 2 {
		t.Fatalf("disposals = %+v, want one Fee per side", r.Disposals)
	}
	// Sorted by Sold: the receive's fee comes first.
	near(t, "b fee amount", fees[0].Amount, 0.001)
	near(t, "b fee cost", fees[0].CostUSD, 0.1)
	near(t, "b fee proceeds", fees[0].ProceedsUSD, 0.2)
	if fees[0].Wallet != "b" || fees[0].TxID != "b:recv" {
		t.Errorf("fees[0] = %+v, want b:recv in b", fees[0])
	}
	near(t, "a fee amount", fees[1].Amount, 0.01)
	near(t, "a fee cost", fees[1].CostUSD, 1)
	near(t, "a fee proceeds", fees[1].ProceedsUSD, 2)
	if fees[1].Wallet != "a" || fees[1].TxID != "a:send" {
		t.Errorf("fees[1] = %+v, want a:send in a", fees[1])
	}
	near(t, "a held", heldIn(r, "a", "SOL"), 7.99)
	near(t, "b held", heldIn(r, "b", "SOL"), 1.999)
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

// Any tx with a PeerID is a transfer half, whatever its kind: no unmatched issues.
func TestTransferRoutedByPeerID(t *testing.T) {
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxSend, lg("SOL", -1, usd(-200))),
		mk("recv", "b", at(2026, 3, 1, 0), core.TxReceive, lg("SOL", 1, usd(200))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	if len(r.Disposals) != 0 || len(r.Issues) != 0 {
		t.Fatalf("disposals = %+v issues = %+v, want none", r.Disposals, r.Issues)
	}
	b := lotsIn(r, "b", "SOL")
	if len(b) != 1 || len(r.Lots) != 1 {
		t.Fatalf("lots = %+v, want one SOL lot in b", r.Lots)
	}
	near(t, "b cost", b[0].CostUSD, 100)
}

func TestTransferKeepsVariousAndFlag(t *testing.T) {
	l := lot("a", "BTC", 1, 1000, at(2024, 12, 31, 0))
	l.Various, l.Flag = true, FlagTransferDate
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("BTC", -1, usd(-90000))),
		mk("recv", "b", at(2026, 3, 1, 0), core.TxTransfer, lg("BTC", 1, usd(90000))),
	)
	r := Compute(Input{From: from2026, Opening: []Lot{l}, Txs: []core.Tx{send, recv}})
	b := lotsIn(r, "b", "BTC")
	if len(b) != 1 || len(r.Lots) != 1 {
		t.Fatalf("lots = %+v, want one BTC lot in b", r.Lots)
	}
	want := l
	want.Wallet = "b"
	if b[0] != want {
		t.Errorf("b lot = %+v, want %+v", b[0], want)
	}
}

// An asset on one side only is settled on its half: a negative leg as a "Fee" disposal.
func TestTransferOneSidedLegIsFee(t *testing.T) {
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer,
			lg("BTC", -0.5, usd(-50000)), lg("SOL", -0.002, usd(-0.4))),
		mk("recv", "b", at(2026, 3, 1, 0), core.TxTransfer, lg("BTC", 0.5, usd(50000))),
	)
	r := Compute(Input{
		From: from2026,
		Opening: []Lot{
			lot("a", "BTC", 0.5, 20000, at(2025, 6, 1, 0)),
			lot("a", "SOL", 1, 100, at(2025, 6, 1, 0)),
		},
		Txs: []core.Tx{send, recv},
	})
	near(t, "b BTC", heldIn(r, "b", "BTC"), 0.5)
	near(t, "a BTC", heldIn(r, "a", "BTC"), 0)
	if len(r.Disposals) != 1 || r.Disposals[0].Asset != "SOL" || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want one SOL Fee", r.Disposals)
	}
	near(t, "SOL proceeds", r.Disposals[0].ProceedsUSD, 0.4)
	if len(r.Issues) != 0 {
		t.Fatalf("issues = %+v, want none (a transfer leg is not unmatched)", r.Issues)
	}
}

// A positive leg on one half only is an acquisition at market value, with no issue; an
// unvalued one still raises IssueMissingPrice.
func TestTransferOneSidedPositiveLeg(t *testing.T) {
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("BTC", -0.5, usd(-50000))),
		mk("recv", "b", at(2026, 3, 1, 0), core.TxTransfer,
			lg("BTC", 0.5, usd(50000)), lg("ETH", 1, usd(2000)), lg("spl:X", 5, nil)),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "BTC", 0.5, 20000, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	eth := lotsIn(r, "b", "ETH")
	if len(eth) != 1 {
		t.Fatalf("ETH lots = %+v", eth)
	}
	near(t, "ETH cost", eth[0].CostUSD, 2000)
	if len(r.Issues) != 1 || r.Issues[0].Kind != IssueMissingPrice || r.Issues[0].Asset != "spl:X" {
		t.Errorf("issues = %+v, want only missing-price for spl:X", r.Issues)
	}
}

// A transfer half is not a trade: its unvalued leg is not filled from its other legs.
func TestTransferSkipsFillTrade(t *testing.T) {
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("BTC", -0.5, usd(-50000)), lg("ETH", 1, nil)),
		mk("recv", "b", at(2026, 3, 1, 0), core.TxTransfer, lg("BTC", 0.5, usd(50000))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "BTC", 0.5, 20000, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	eth := lotsIn(r, "a", "ETH")
	if len(eth) != 1 || eth[0].CostUSD != 0 {
		t.Fatalf("ETH lots = %+v, want cost 0 (not filled from BTC)", eth)
	}
	if got := issuesOf(r, IssueMissingPrice); len(got) != 1 || got[0].Asset != "ETH" {
		t.Errorf("issues = %+v, want missing-price for ETH", r.Issues)
	}
}

// When the shortfall is the sender's own fee in the same asset, the fee path disposes it
// and the transfer difference must not be disposed a second time.
func TestTransferDiffEqualsFeeNotDoubleCounted(t *testing.T) {
	send, recv := linked(
		withFee(mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("SOL", -1, usd(-200))), lg("SOL", -0.01, usd(-2))),
		mk("recv", "b", at(2026, 3, 1, 1), core.TxTransfer, lg("SOL", 0.99, usd(198))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" {
		t.Fatalf("disposals = %+v, want the single fee-path disposal", r.Disposals)
	}
	near(t, "fee amount", r.Disposals[0].Amount, 0.01)
	near(t, "b held", heldIn(r, "b", "SOL"), 0.99)
	near(t, "a held", heldIn(r, "a", "SOL"), 0)
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

// A shortfall larger than the fee is still disposed.
func TestTransferDiffBeyondFeeStillDisposed(t *testing.T) {
	send, recv := linked(
		withFee(mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("SOL", -1, usd(-200))), lg("SOL", -0.01, usd(-2))),
		mk("recv", "b", at(2026, 3, 1, 1), core.TxTransfer, lg("SOL", 0.98, usd(196))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 2, 200, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	if len(r.Disposals) != 2 {
		t.Fatalf("disposals = %+v, want the difference and the fee", r.Disposals)
	}
}

// The difference and the extra take the other side's unit value when their own leg has none.
func TestTransferDiffFallsBackToOtherSideValue(t *testing.T) {
	send, recv := linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("SOL", -1, nil)),
		mk("recv", "b", at(2026, 3, 1, 1), core.TxTransfer, lg("SOL", 0.99, usd(198))),
	)
	r := Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	if len(r.Disposals) != 1 {
		t.Fatalf("disposals = %+v", r.Disposals)
	}
	near(t, "fee proceeds", r.Disposals[0].ProceedsUSD, 2)
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}

	send, recv = linked(
		mk("send", "a", at(2026, 3, 1, 0), core.TxTransfer, lg("SOL", -1, usd(-200))),
		mk("recv", "b", at(2026, 3, 1, 1), core.TxTransfer, lg("SOL", 1.5, nil)),
	)
	r = Compute(Input{
		From:    from2026,
		Opening: []Lot{lot("a", "SOL", 1, 100, at(2025, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	})
	b := lotsIn(r, "b", "SOL")
	if len(b) != 2 {
		t.Fatalf("b lots = %+v", b)
	}
	near(t, "extra cost", b[1].CostUSD, 100)
	if len(r.Issues) != 0 {
		t.Errorf("issues = %+v, want none", r.Issues)
	}
}

// A half whose peer is before From is skipped silently only when its own ID is in
// PairedBeforeFrom (the close of the prior year carried it): that close processed the pair
// and the lots are in the opening lots. Otherwise (the pair was linked after the close, or
// the prior year is a Koinly import) nothing says the lots moved: the half raises
// IssueTransferUnpaired. Either way no lots change and the half's own fee is disposed once.
func TestTransferPeerBeforeFrom(t *testing.T) {
	cases := []struct {
		name    string
		paired  map[string]bool
		skipped bool
	}{
		{"carried by the close", map[string]bool{"b:recv": true}, true},
		{"not carried", nil, false},
		{"only the peer listed", map[string]bool{"a:send": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			send, recv := linked(
				mk("send", "a", at(2025, 12, 31, 23), core.TxTransfer, lg("BTC", -0.5, usd(-50000))),
				withFee(mk("recv", "b", at(2026, 1, 1, 1), core.TxTransfer, lg("BTC", 0.5, usd(50000))), lg("BTC", -0.00001, usd(-1))),
			)
			r := Compute(Input{
				From:             from2026,
				PairedBeforeFrom: c.paired,
				// The closing lots of 2025, where the pair already moved 0.5 BTC from a to b.
				Opening: []Lot{
					lot("a", "BTC", 0.5, 20000, at(2025, 3, 1, 0)),
					lot("b", "BTC", 0.5, 20000, at(2025, 3, 1, 0)),
				},
				Txs: []core.Tx{send, recv},
			})
			unpaired := issuesOf(r, IssueTransferUnpaired)
			if c.skipped && len(r.Issues) != 0 {
				t.Errorf("issues = %+v, want none", r.Issues)
			}
			if !c.skipped && (len(unpaired) != 1 || len(r.Issues) != 1 || unpaired[0].TxID != "b:recv") {
				t.Errorf("issues = %+v, want one transfer-unpaired for b:recv", r.Issues)
			}
			near(t, "a held", heldIn(r, "a", "BTC"), 0.5)
			near(t, "b held", heldIn(r, "b", "BTC"), 0.49999)
			if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" || r.Disposals[0].TxID != "b:recv" {
				t.Fatalf("disposals = %+v, want only b:recv's fee", r.Disposals)
			}
			near(t, "fee amount", r.Disposals[0].Amount, 0.00001)
		})
	}
}

// nextYear is the input for computing 2027 from the result of closing 2026 with in.
func nextYear(in Input, res2026 Result) Input {
	in.From, in.To, in.Opening, in.OpeningParked = from2027, time.Time{}, res2026.Lots, res2026.Parked
	in.PairedBeforeFrom = idSet(res2026.CarriedPeers)
	return in
}

// checkInYear fails when the result of closing 2026 holds a row dated in 2027.
func checkInYear(t *testing.T, r Result) {
	t.Helper()
	for _, d := range r.Disposals {
		if !d.Sold.Before(from2027) {
			t.Errorf("disposal %+v is dated in 2027", d)
		}
	}
	for _, l := range r.Lots {
		if !l.Acquired.Before(from2027) {
			t.Errorf("lot %+v is dated in 2027", l)
		}
	}
	for _, is := range r.Issues {
		if !is.Time.Before(from2027) {
			t.Errorf("issue %+v is dated in 2027", is)
		}
	}
	for _, in := range r.Income {
		if !in.Date.Before(from2027) {
			t.Errorf("income %+v is dated in 2027", in)
		}
	}
}

// A send on Dec 31 whose receive is on Jan 1: closing the year moves the lots, and the next
// year, computed from the closing lots, leaves them alone.
func TestTransferStraddlesBoundary(t *testing.T) {
	sendAt := time.Date(2026, 12, 31, 23, 58, 0, 0, time.UTC)
	in := btcMove(sendAt, time.Date(2027, 1, 1, 0, 3, 0, 0, time.UTC))
	in.To = from2027
	res2026 := Compute(in)
	// The lots moved to b, the send's fee is disposed in 2026, and nothing is unpaired.
	checkBTCMove(t, res2026, sendAt, LongTerm)
	checkInYear(t, res2026)

	res2027 := Compute(nextYear(in, res2026))
	if len(res2027.Disposals) != 0 || len(res2027.Issues) != 0 {
		t.Fatalf("2027 disposals = %+v issues = %+v, want none", res2027.Disposals, res2027.Issues)
	}
	if !slices.Equal(res2027.Lots, res2026.Lots) {
		t.Errorf("2027 lots = %+v, want the 2026 closing lots %+v", res2027.Lots, res2026.Lots)
	}
}

// A receive on Dec 31 stamped before its send on Jan 1: closing the year moves the lots at
// the receive, out of the sender's lots as they are then; the next year skips the send's
// half silently and disposes only its fee.
func TestTransferReceiveBeforeSendAcrossBoundary(t *testing.T) {
	sendAt := time.Date(2027, 1, 1, 0, 2, 0, 0, time.UTC)
	in := btcMove(sendAt, time.Date(2026, 12, 31, 23, 55, 0, 0, time.UTC))
	in.To = from2027
	res2026 := Compute(in)
	if len(res2026.Disposals) != 0 || len(res2026.Issues) != 0 {
		t.Fatalf("2026 disposals = %+v issues = %+v, want none (the send and its fee are in 2027)", res2026.Disposals, res2026.Issues)
	}
	b := lotsIn(res2026, "b", "BTC")
	if len(b) != 2 {
		t.Fatalf("2026 b lots = %+v, want 2", b)
	}
	near(t, "b[0] amount", b[0].Amount, 0.5)
	near(t, "b[0] cost", b[0].CostUSD, 10000)
	near(t, "b[1] amount", b[1].Amount, 0.1)
	near(t, "b[1] cost", b[1].CostUSD, 6000)
	near(t, "2026 a held", heldIn(res2026, "a", "BTC"), 0.4)

	// The same end state as one pass over both years: the fee disposed at the send, in a.
	checkBTCMove(t, Compute(nextYear(in, res2026)), sendAt, LongTerm)
}

// The rows a straddling pair creates while a year is closed (the sent − received
// difference, a one-sided leg, a shortfall, the issues) are dated at the half inside the
// year, so the closed year holds no row of the next one. The late send's own fee is left to
// the next year, which disposes it once at the send.
func TestBoundaryPairRowsDatedInYear(t *testing.T) {
	recvAt := time.Date(2026, 12, 31, 23, 55, 0, 0, time.UTC)
	sendAt := time.Date(2027, 1, 1, 0, 2, 0, 0, time.UTC)
	send, recv := linked(
		withFee(mk("send", "a", sendAt, core.TxTransfer, lg("BTC", -0.6, usd(-60000)), lg("SOL", -0.01, usd(-2))), lg("BTC", -0.0001, usd(-10))),
		mk("recv", "b", recvAt, core.TxTransfer, lg("BTC", 0.59, nil)),
	)
	in := Input{
		From: from2026, To: from2027,
		Opening: []Lot{
			lot("a", "BTC", 0.5, 10000, at(2024, 6, 1, 0)),
			lot("a", "BTC", 0.5, 30000, at(2025, 9, 1, 0)),
			lot("a", "SOL", 1, 100, at(2025, 1, 1, 0)),
		},
		Txs: []core.Tx{send, recv},
	}
	res2026 := Compute(in)
	checkInYear(t, res2026)
	if len(res2026.Issues) != 0 {
		t.Errorf("2026 issues = %+v, want none", res2026.Issues)
	}
	if len(res2026.Disposals) != 2 {
		t.Fatalf("2026 disposals = %+v, want the BTC difference and the SOL leg", res2026.Disposals)
	}
	for _, d := range res2026.Disposals {
		if d.Note != "Fee" || d.TxID != "a:send" || d.Wallet != "a" || !d.Sold.Equal(recvAt) {
			t.Errorf("disposal = %+v, want a Fee from a:send in a at the receive", d)
		}
	}
	diff := res2026.Disposals[0]
	if diff.Asset != "BTC" {
		diff = res2026.Disposals[1]
	}
	near(t, "difference amount", diff.Amount, 0.01)
	near(t, "difference cost", diff.CostUSD, 600)
	near(t, "difference proceeds", diff.ProceedsUSD, 1000)
	near(t, "2026 b BTC", heldIn(res2026, "b", "BTC"), 0.59)
	near(t, "2026 a BTC", heldIn(res2026, "a", "BTC"), 0.4)
	near(t, "2026 a SOL", heldIn(res2026, "a", "SOL"), 0.99)

	res2027 := Compute(nextYear(in, res2026))
	if len(res2027.Issues) != 0 {
		t.Errorf("2027 issues = %+v, want none", res2027.Issues)
	}
	if len(res2027.Disposals) != 1 {
		t.Fatalf("2027 disposals = %+v, want only the send's own fee", res2027.Disposals)
	}
	f := res2027.Disposals[0]
	near(t, "fee amount", f.Amount, 0.0001)
	near(t, "fee proceeds", f.ProceedsUSD, 10)
	if f.Note != "Fee" || f.TxID != "a:send" || !f.Sold.Equal(sendAt) {
		t.Errorf("fee = %+v, want a:send's Fee at the send", f)
	}
	near(t, "2027 a BTC", heldIn(res2027, "a", "BTC"), 0.3999)
	near(t, "2027 b BTC", heldIn(res2027, "b", "BTC"), 0.59)
}

// The other way round, with the receive on Jan 1: the shortfall lot, the extra received and
// a one-sided leg of the receive are dated at the send, as is the missing-basis issue.
func TestBoundaryPairLotsDatedInYear(t *testing.T) {
	sendAt := time.Date(2026, 12, 31, 23, 58, 0, 0, time.UTC)
	send, recv := linked(
		mk("send", "a", sendAt, core.TxTransfer, lg("BTC", -0.6, usd(-60000))),
		mk("recv", "b", time.Date(2027, 1, 1, 0, 3, 0, 0, time.UTC), core.TxTransfer,
			lg("BTC", 0.65, nil), lg("ETH", 1, usd(2000))),
	)
	in := Input{
		From: from2026, To: from2027,
		Opening: []Lot{lot("a", "BTC", 0.5, 10000, at(2024, 6, 1, 0))},
		Txs:     []core.Tx{send, recv},
	}
	res2026 := Compute(in)
	checkInYear(t, res2026)
	b := lotsIn(res2026, "b", "BTC")
	if len(b) != 3 {
		t.Fatalf("2026 b lots = %+v, want the moved lot, the shortfall and the extra", b)
	}
	near(t, "moved", b[0].Amount, 0.5)
	near(t, "shortfall", b[1].Amount, 0.1)
	near(t, "extra", b[2].Amount, 0.05)
	near(t, "extra cost", b[2].CostUSD, 5000)
	if b[1].Flag != FlagMissingBasis || !b[1].Acquired.Equal(sendAt) || !b[2].Acquired.Equal(sendAt) {
		t.Errorf("2026 b lots = %+v, want the shortfall (flagged) and the extra acquired at the send", b)
	}
	if eth := lotsIn(res2026, "b", "ETH"); len(eth) != 1 || !eth[0].Acquired.Equal(sendAt) {
		t.Errorf("2026 ETH lots = %+v, want one acquired at the send", eth)
	}
	missing := issuesOf(res2026, IssueMissingBasis)
	if len(missing) != 1 || len(res2026.Issues) != 1 || !missing[0].Time.Equal(sendAt) {
		t.Fatalf("2026 issues = %+v, want one missing-basis at the send", res2026.Issues)
	}
	near(t, "missing amount", missing[0].Amount, 0.1)

	res2027 := Compute(nextYear(in, res2026))
	if len(res2027.Disposals) != 0 || len(res2027.Issues) != 0 {
		t.Fatalf("2027 disposals = %+v issues = %+v, want none", res2027.Disposals, res2027.Issues)
	}
	if !slices.Equal(res2027.Lots, res2026.Lots) {
		t.Errorf("2027 lots = %+v, want the 2026 closing lots %+v", res2027.Lots, res2026.Lots)
	}
}

// A half whose peer is absent, failed or not linked back still changes no lots and raises
// IssueTransferUnpaired when the peer would be outside [From, To), even with its own ID in
// PairedBeforeFrom.
func TestTransferPeerMissingStillUnpaired(t *testing.T) {
	inYear := time.Date(2026, 12, 31, 23, 55, 0, 0, time.UTC)
	afterTo := time.Date(2027, 1, 1, 0, 2, 0, 0, time.UTC)
	firstOfYear := time.Date(2026, 1, 1, 0, 3, 0, 0, time.UTC)
	beforeFrom := time.Date(2025, 12, 31, 23, 58, 0, 0, time.UTC)
	cases := []struct {
		name           string
		sendAt, recvAt time.Time
		setup          func(send, recv *core.Tx)
	}{
		{"peer absent", afterTo, inYear, func(send, recv *core.Tx) { recv.PeerID = "a:gone" }},
		{"peer after To failed", afterTo, inYear, func(send, recv *core.Tx) { send.Status = core.TxFailed }},
		{"peer after To links elsewhere", afterTo, inYear, func(send, recv *core.Tx) { send.PeerID = "c:other" }},
		{"peer before From failed", beforeFrom, firstOfYear, func(send, recv *core.Tx) { send.Status = core.TxFailed }},
		{"peer before From not linked", beforeFrom, firstOfYear, func(send, recv *core.Tx) { send.PeerID = "" }},
	}
	for _, c := range cases {
		for _, paired := range []map[string]bool{nil, {"b:recv": true}} {
			t.Run(fmt.Sprintf("%s/PairedBeforeFrom=%v", c.name, paired), func(t *testing.T) {
				send, recv := linked(
					mk("send", "a", c.sendAt, core.TxTransfer, lg("BTC", -0.5, usd(-50000))),
					mk("recv", "b", c.recvAt, core.TxTransfer, lg("BTC", 0.5, usd(50000))),
				)
				c.setup(&send, &recv)
				r := Compute(Input{
					From: from2026, To: from2027, PairedBeforeFrom: paired,
					Opening: []Lot{lot("a", "BTC", 1, 40000, at(2025, 3, 1, 0))},
					Txs:     []core.Tx{send, recv},
				})
				if b := lotsIn(r, "b", "BTC"); len(b) != 0 {
					t.Errorf("b lots = %+v, want none", b)
				}
				near(t, "a held", heldIn(r, "a", "BTC"), 1)
				unpaired := issuesOf(r, IssueTransferUnpaired)
				if len(unpaired) != 1 || len(r.Issues) != 1 || unpaired[0].TxID != "b:recv" {
					t.Errorf("issues = %+v, want one transfer-unpaired for b:recv", r.Issues)
				}
			})
		}
	}
}

// Closing 2026 lists the peers after To it paired (Result.CarriedPeers), in either order of
// the halves. Computing 2027 with them as PairedBeforeFrom skips those halves silently;
// without them the halves raise IssueTransferUnpaired and change no lots.
func TestCarriedPeerSkipped(t *testing.T) {
	cases := []struct {
		name           string
		sendAt, recvAt time.Time
		carried        string
	}{
		{"send first", time.Date(2026, 12, 31, 23, 58, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 3, 0, 0, time.UTC), "b:recv"},
		{"receive first", time.Date(2027, 1, 1, 0, 2, 0, 0, time.UTC), time.Date(2026, 12, 31, 23, 55, 0, 0, time.UTC), "a:send"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := btcMove(c.sendAt, c.recvAt)
			if r := Compute(in); len(r.CarriedPeers) != 0 {
				t.Errorf("one pass CarriedPeers = %v, want none (no To)", r.CarriedPeers)
			}
			in.To = from2027
			res2026 := Compute(in)
			if !slices.Equal(res2026.CarriedPeers, []string{c.carried}) {
				t.Fatalf("CarriedPeers = %v, want [%s]", res2026.CarriedPeers, c.carried)
			}
			next := nextYear(in, res2026)
			r := Compute(next)
			if len(r.Issues) != 0 {
				t.Errorf("2027 issues = %+v, want none", r.Issues)
			}
			near(t, "2027 a held", heldIn(r, "a", "BTC"), 0.3999)
			near(t, "2027 b held", heldIn(r, "b", "BTC"), 0.6)

			next.PairedBeforeFrom = nil
			r = Compute(next)
			unpaired := issuesOf(r, IssueTransferUnpaired)
			if len(unpaired) != 1 || len(r.Issues) != 1 || unpaired[0].TxID != c.carried {
				t.Errorf("2027 issues without the carried peers = %+v, want one transfer-unpaired for %s", r.Issues, c.carried)
			}
			near(t, "unpaired a held", heldIn(r, "a", "BTC"), 0.3999)
			near(t, "unpaired b held", heldIn(r, "b", "BTC"), 0.6)
		})
	}
}

// A pair the syncer links after the earlier half's year was closed was not carried by the
// close, which booked the receive on its own (an acquisition). The next year does not skip
// the send silently: it raises IssueTransferUnpaired for review and changes no lots.
func TestLateLinkedPairNotSkipped(t *testing.T) {
	recv := mk("recv", "b", time.Date(2026, 12, 31, 20, 0, 0, 0, time.UTC), core.TxReceive, lg("BTC", 0.5, usd(50000)))
	send := withFee(mk("send", "a", time.Date(2027, 1, 1, 2, 0, 0, 0, time.UTC), core.TxSend, lg("BTC", -0.5, usd(-50000))), lg("BTC", -0.0001, usd(-10)))
	in := Input{
		From: from2026, To: from2027,
		Opening: []Lot{lot("a", "BTC", 1, 40000, at(2025, 3, 1, 0))},
		Txs:     []core.Tx{recv, send},
	}
	res2026 := Compute(in)
	if len(res2026.CarriedPeers) != 0 {
		t.Errorf("CarriedPeers = %v, want none", res2026.CarriedPeers)
	}
	if len(issuesOf(res2026, IssueUnmatchedReceive)) != 1 {
		t.Errorf("2026 issues = %+v, want the unmatched receive", res2026.Issues)
	}
	near(t, "2026 b held", heldIn(res2026, "b", "BTC"), 0.5)

	send, recv = linked(send, recv) // linked after 2026 was closed
	in.Txs = []core.Tx{recv, send}
	r := Compute(nextYear(in, res2026))
	unpaired := issuesOf(r, IssueTransferUnpaired)
	if len(unpaired) != 1 || len(r.Issues) != 1 || unpaired[0].TxID != "a:send" {
		t.Errorf("2027 issues = %+v, want one transfer-unpaired for a:send", r.Issues)
	}
	near(t, "2027 a held", heldIn(r, "a", "BTC"), 0.9999) // only the send's own fee left
	near(t, "2027 b held", heldIn(r, "b", "BTC"), 0.5)
	if len(r.Disposals) != 1 || r.Disposals[0].Note != "Fee" || r.Disposals[0].TxID != "a:send" {
		t.Errorf("2027 disposals = %+v, want only a:send's fee", r.Disposals)
	}
}
