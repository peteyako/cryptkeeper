package activity

import (
	"math"
	"sort"
	"strings"
	"time"

	"cryptkeeper/internal/core"
)

const (
	// A receive may be stamped slightly before the matching send (clock skew, block time
	// versus exchange ledger time) or hours after it (exchange crediting delays).
	matchBefore = 10 * time.Minute
	matchAfter  = 6 * time.Hour
	// matchTolerance is the relative amount slack for a weak match; the send's own fee in
	// the same asset widens it when larger.
	matchTolerance = 0.005
)

// transferSide is one tx seen as the sending or receiving half of a transfer in one asset.
type transferSide struct {
	idx    int     // index into the input slice
	asset  string  // asset key
	amount float64 // magnitude of the net movement of that asset (always > 0)
	fee    float64 // sender only: magnitude of the fee when charged in the same asset
}

type pair struct {
	send, recv transferSide
	strong     bool
	dt         time.Duration // |recv.Time - send.Time|
}

// MatchTransfers finds sends and receives that are the two halves of one transfer between
// different sources and returns the txs that changed: both sides of every new pair get
// Kind = transfer, PeerID = the other tx's ID and PeerSource = the other's SourceID.
//
// A send (net outflow of asset X, not failed) on source A matches a receive (net inflow of
// X) on a different source B when either
//   - both carry the same non-empty TxHash (strong), or
//   - |received - sent| <= max(0.5% of sent, the send's fee in X) and the receive is stamped
//     within [send - 10 min, send + 6 h] (weak).
//
// Strong matches are made first; then candidate pairs are taken closest-in-time first.
// Each tx matches at most once, txs that already have a PeerID are left alone, failed txs
// never match, and trades, swaps, rewards, stakes and fee entries are not transfers.
// The input is not modified; the result keeps input order and is empty when nothing new
// matches, so applying it and running again is a no-op.
func MatchTransfers(txs []core.Tx) []core.Tx {
	var sends []transferSide
	recvsByAsset := map[string][]transferSide{}
	for i := range txs {
		t := &txs[i]
		if t.PeerID != "" || t.Status == core.TxFailed || !transferCapable(t.Kind) {
			continue
		}
		net := map[string]float64{}
		var order []string
		for _, l := range t.Legs {
			if l.AssetKey == "" {
				continue
			}
			if _, ok := net[l.AssetKey]; !ok {
				order = append(order, l.AssetKey)
			}
			net[l.AssetKey] += l.Amount
		}
		for _, asset := range order {
			switch n := net[asset]; {
			case n < 0:
				side := transferSide{idx: i, asset: asset, amount: -n}
				if t.Fee != nil && t.Fee.AssetKey == asset {
					side.fee = math.Abs(t.Fee.Amount)
				}
				sends = append(sends, side)
			case n > 0:
				recvsByAsset[asset] = append(recvsByAsset[asset], transferSide{idx: i, asset: asset, amount: n})
			}
		}
	}
	if len(sends) == 0 || len(recvsByAsset) == 0 {
		return nil
	}
	for _, rs := range recvsByAsset {
		sort.SliceStable(rs, func(a, b int) bool { return txs[rs[a].idx].Time.Before(txs[rs[b].idx].Time) })
	}

	// Strong lookup: (asset, lower-cased hash) -> receives.
	type hashKey struct{ asset, hash string }
	byHash := map[hashKey][]transferSide{}
	for asset, rs := range recvsByAsset {
		for _, r := range rs {
			if h := hashOf(&txs[r.idx]); h != "" {
				k := hashKey{asset, h}
				byHash[k] = append(byHash[k], r)
			}
		}
	}

	var pairs []pair
	for _, s := range sends {
		st := &txs[s.idx]
		if h := hashOf(st); h != "" {
			for _, r := range byHash[hashKey{s.asset, h}] {
				if txs[r.idx].SourceID != st.SourceID {
					pairs = append(pairs, pair{s, r, true, absDur(txs[r.idx].Time.Sub(st.Time))})
				}
			}
		}
		rs := recvsByAsset[s.asset]
		lo := sort.Search(len(rs), func(i int) bool { return !txs[rs[i].idx].Time.Before(st.Time.Add(-matchBefore)) })
		hi := st.Time.Add(matchAfter)
		tol := math.Max(matchTolerance*s.amount, s.fee)
		for _, r := range rs[lo:] {
			rt := &txs[r.idx]
			if rt.Time.After(hi) {
				break
			}
			if rt.SourceID == st.SourceID || !withinTolerance(r.amount, s.amount, tol) {
				continue
			}
			pairs = append(pairs, pair{s, r, false, absDur(rt.Time.Sub(st.Time))})
		}
	}

	sort.SliceStable(pairs, func(a, b int) bool {
		pa, pb := pairs[a], pairs[b]
		if pa.strong != pb.strong {
			return pa.strong
		}
		if pa.dt != pb.dt {
			return pa.dt < pb.dt
		}
		da := math.Abs(pa.recv.amount - pa.send.amount)
		db := math.Abs(pb.recv.amount - pb.send.amount)
		if da != db {
			return da < db
		}
		if txs[pa.send.idx].ID != txs[pb.send.idx].ID {
			return txs[pa.send.idx].ID < txs[pb.send.idx].ID
		}
		return txs[pa.recv.idx].ID < txs[pb.recv.idx].ID
	})

	matched := map[int]int{} // tx index -> index of its peer
	for _, p := range pairs {
		_, sDone := matched[p.send.idx]
		_, rDone := matched[p.recv.idx]
		if sDone || rDone || p.send.idx == p.recv.idx {
			continue
		}
		matched[p.send.idx] = p.recv.idx
		matched[p.recv.idx] = p.send.idx
	}
	if len(matched) == 0 {
		return nil
	}

	out := make([]core.Tx, 0, len(matched))
	for i := range txs {
		peer, ok := matched[i]
		if !ok {
			continue
		}
		t := txs[i]
		t.Kind = core.TxTransfer
		t.PeerID = txs[peer].ID
		t.PeerSource = txs[peer].SourceID
		out = append(out, t)
	}
	return out
}

// transferCapable reports whether a tx of this kind can be one half of a transfer.
func transferCapable(k core.TxKind) bool {
	switch k {
	case core.TxTrade, core.TxSwap, core.TxReward, core.TxStake, core.TxFee, core.TxPosition:
		return false
	}
	return true
}

func hashOf(t *core.Tx) string { return strings.ToLower(strings.TrimSpace(t.TxHash)) }

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// withinTolerance is |a-b| <= tol with a hair of slack for floating-point noise.
func withinTolerance(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol+1e-9*math.Max(a, b)
}
