package tax

import (
	"math"

	"cryptkeeper/internal/core"
)

// index records the txs a transfer half can pair with: those that did not fail and are
// linked to a peer themselves. It is given all of Input.Txs, not only the replayed ones,
// so a pair that straddles From or To is still found (see transfer).
func (e *engine) index(txs []core.Tx) {
	for _, tx := range txs {
		if tx.Status != core.TxFailed && tx.PeerID != "" {
			e.peers[tx.ID] = tx
		}
	}
}

// transfer handles a tx linked to the other half of a transfer by PeerID (see
// activity.MatchTransfers), whatever its kind. The pair is processed once, when its first
// half comes up (a receive may be stamped before its send); the second half then changes
// nothing. Each half's Tx.Fee is still disposed by apply.
//
// For each asset that leaves one half and arrives on the other, the received amount moves
// FIFO from the sender's lots to the receiver's wallet (see move). An asset found on one
// half only, or with the same sign on both, is settled on its half at market value: a
// negative leg as a disposal with Note "Fee", a positive one as an acquisition. Neither
// raises an unmatched issue, as the tx is a transfer.
//
// The peer may be outside the replayed [From, To):
//
//   - at or after To (closing a year): the pair is processed now, when this half comes up,
//     as if both halves were replayed, so the closing lots hold the moved lots, and the
//     peer's ID goes to Result.CarriedPeers. Every row the pair creates (a difference, an
//     extra, a shortfall lot, a one-sided leg, an issue) is dated at this half's time, so a
//     closed year holds no row of the next one. The peer's own Tx.Fee is not disposed here:
//     the next year disposes it, at the peer's time, when it skips the pair.
//   - before From: when this half's ID is in Input.PairedBeforeFrom (the close of the prior
//     year carried it, as above), that close processed the pair and the lots are in the
//     opening lots, so this half changes nothing and raises nothing. Otherwise nothing says
//     the lots moved (the pair was linked after the close, the prior year is a Koinly
//     import, or the other half was ignored as spam) and the half is unpaired, as below.
//
// A half whose peer is not in Input.Txs, failed, or is not linked back (see unpaired) is
// listed in Result.Unpaired. Untagged, it changes no lots and raises IssueTransferUnpaired per
// leg; tagged, the tag applies as on a receive or send (see taggedMove).
func (e *engine) transfer(tx core.Tx) {
	if e.paired[tx.ID] {
		return
	}
	if e.unpaired(tx) {
		e.out.Unpaired = append(e.out.Unpaired, tx.ID)
		legs := e.legs(tx)
		if tag, ok := e.tagOf(tx); ok {
			e.taggedMove(tx, tag, legs)
			return
		}
		for _, v := range legs {
			e.issue(IssueTransferUnpaired, tx, v.AssetKey, v.Amount)
		}
		return
	}
	peer := e.peers[tx.PeerID]
	before := peer.Time.Before(e.in.From)
	e.paired[tx.ID], e.paired[peer.ID] = true, true
	if before {
		return
	}
	if !e.in.To.IsZero() && !peer.Time.Before(e.in.To) {
		e.out.CarriedPeers = append(e.out.CarriedPeers, peer.ID)
		peer.Time = tx.Time // a local copy: the rows booked on the peer stay in this year
	}

	theirs := e.legs(peer)
	byAsset := make(map[string]*valued, len(theirs))
	for _, v := range theirs {
		byAsset[v.AssetKey] = v
	}
	var onlyMine []*valued
	for _, m := range e.legs(tx) {
		p := byAsset[m.AssetKey]
		switch {
		case p == nil || (m.Amount < 0) == (p.Amount < 0):
			onlyMine = append(onlyMine, m)
			continue
		case m.Amount < 0:
			e.move(tx, m, peer, p)
		default:
			e.move(peer, p, tx, m)
		}
		delete(byAsset, m.AssetKey)
	}
	for _, v := range onlyMine {
		e.settle(tx, v, noteFee)
	}
	for _, v := range theirs {
		if byAsset[v.AssetKey] != nil {
			e.settle(peer, v, noteFee)
		}
	}
}

// unpaired reports whether tx, a transfer half not yet processed, has no usable peer: the
// peer is not in Input.Txs, failed, is tx itself or is not linked back, or is before From
// without tx in Input.PairedBeforeFrom. A failed half is never unpaired: it only pays its fee.
func (e *engine) unpaired(tx core.Tx) bool {
	if tx.PeerID == "" || tx.Status == core.TxFailed || e.paired[tx.ID] {
		return false
	}
	peer, ok := e.peers[tx.PeerID]
	before := ok && peer.Time.Before(e.in.From)
	return !ok || peer.ID == tx.ID || peer.PeerID != tx.ID || (before && !e.in.PairedBeforeFrom[tx.ID])
}

// move carries one asset from the sending half of a transfer (leg out < 0 of tx from) to
// the receiving half (leg in > 0 of tx to).
//
// min(sent, received) of lots moves FIFO from the sender's wallet to the receiver's,
// keeping Acquired, Various, cost, TxID and Flag. What the sender's lots cannot cover
// arrives as a lot at cost 0, acquired at the receive, with FlagMissingBasis, plus
// IssueMissingBasis for the sender. Then sent minus received (a withdrawal fee) is a
// disposal from the sender with Note "Fee", and received minus sent an acquisition by the
// receiver. Each is valued at its own leg's unit value, else at the other leg's; with neither
// it is worth 0 and raises IssueMissingPrice. When the sender's Tx.Fee is in this asset and
// equals sent minus received, the difference is that fee, which apply disposes: it is not
// disposed here too.
func (e *engine) move(from core.Tx, out *valued, to core.Tx, in *valued) {
	asset := out.AssetKey
	sent, received := -out.Amount, in.Amount
	taken, short := e.queueFor(from.SourceID, asset).take(math.Min(sent, received))
	for _, l := range taken {
		l.Wallet = to.SourceID
		e.addLot(l)
	}
	if short > 0 {
		e.addLot(Lot{Wallet: to.SourceID, Asset: asset, Amount: short, Acquired: to.Time, TxID: to.ID, Flag: FlagMissingBasis})
		e.issue(IssueMissingBasis, from, asset, short)
	}
	switch diff := sent - received; {
	case diff > dust:
		if !paidByFee(from, asset, diff, sent) {
			e.dispose(from, asset, diff, e.unitValue(from, out, in, diff), noteFee)
		}
	case -diff > dust:
		e.acquire(to, asset, -diff, e.unitValue(to, in, out, -diff))
	}
}

// paidByFee reports whether diff, the amount of asset that left the sender and did not
// arrive, is the sender's own network fee in that asset (within rounding of sent).
func paidByFee(from core.Tx, asset string, diff, sent float64) bool {
	f := from.Fee
	if f == nil || f.AssetKey != asset {
		return false
	}
	return math.Abs(diff-math.Abs(f.Amount)) <= math.Max(dust, 1e-9*sent)
}

// unitValue is the value of amount units at a prepared leg's unit value, else at the
// other leg's. With neither valued it gives 0 and raises IssueMissingPrice for amount on tx.
func (e *engine) unitValue(tx core.Tx, v, other *valued, amount float64) float64 {
	switch {
	case v.known:
	case other.known:
		v = other
	default:
		e.issue(IssueMissingPrice, tx, v.AssetKey, amount)
		return 0
	}
	return v.usd * amount / math.Abs(v.Amount)
}
