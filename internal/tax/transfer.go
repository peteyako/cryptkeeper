package tax

import (
	"math"

	"crypto-portfolio/internal/core"
)

// index records the txs a transfer half can pair with: the replayed ones (at or after
// From) that did not fail and are linked to a peer themselves.
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
// A half whose peer is missing from the replay (before From, failed, or not linked back)
// changes no lots and raises IssueTransferUnpaired per leg.
func (e *engine) transfer(tx core.Tx) {
	if e.paired[tx.ID] {
		return
	}
	peer, ok := e.peers[tx.PeerID]
	if !ok || peer.ID == tx.ID || peer.PeerID != tx.ID {
		for _, v := range e.legs(tx) {
			e.issue(IssueTransferUnpaired, tx, v.AssetKey, v.Amount)
		}
		return
	}
	e.paired[tx.ID], e.paired[peer.ID] = true, true

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
