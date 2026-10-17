package tax

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	"crypto-portfolio/internal/core"
)

// RollBack derives each wallet's quantities at the cutover from the current ones:
// amount at cutover = current - the effect of every tx at or after the cutover.
// Keys are source ID then asset key. Stake and position moves keep the units inside
// the wallet's holdings, so their legs have no effect (as do txs isPosition reports
// as tagged position moves; nil is tolerated), though their fee is still added back; a failed tx only cost its fee.
// The result is a fresh map. Values within dust of zero become 0; negatives are kept
// so the caller can show them.
func RollBack(current map[string]map[string]float64, txs []core.Tx, cutover time.Time, isPosition func(txID string) bool) map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(current))
	for w, assets := range current {
		m := make(map[string]float64, len(assets))
		for a, v := range assets {
			m[a] = v
		}
		out[w] = m
	}
	add := func(w, asset string, delta float64) {
		if out[w] == nil {
			out[w] = map[string]float64{}
		}
		out[w][asset] += delta
	}
	for _, tx := range txs {
		if tx.Time.Before(cutover) {
			continue
		}
		if tx.Status != core.TxFailed && tx.Kind != core.TxStake && tx.Kind != core.TxPosition &&
			(isPosition == nil || !isPosition(tx.ID)) {
			for _, l := range tx.Legs {
				add(tx.SourceID, l.AssetKey, -l.Amount)
			}
		}
		// A stake/position move still pays its fee, and a failed tx only did.
		if tx.Fee != nil {
			add(tx.SourceID, tx.Fee.AssetKey, -tx.Fee.Amount)
		}
	}
	for _, assets := range out {
		for a, v := range assets {
			if math.Abs(v) <= dust {
				assets[a] = 0
			}
		}
	}
	return out
}

// RollBackParked derives the balances parked inside the user's own positions at the
// cutover. positions is the amount inside positions now (source ID, asset key). A tx
// is a position move when its kind is position or isPosition reports it tagged;
// parked at cutover = parked now + the signed legs of those moves since (a deposit
// leg is negative). Failed txs are ignored. Negative and dust results are dropped.
// The result is sorted by wallet then asset.
func RollBackParked(positions map[string]map[string]float64, txs []core.Tx, cutover time.Time, isPosition func(txID string) bool) []Parked {
	sum := map[string]map[string]float64{}
	for w, assets := range positions {
		sum[w] = map[string]float64{}
		for a, v := range assets {
			sum[w][a] = v
		}
	}
	for _, tx := range txs {
		if tx.Time.Before(cutover) || tx.Status == core.TxFailed {
			continue
		}
		if tx.Kind != core.TxPosition && (isPosition == nil || !isPosition(tx.ID)) {
			continue
		}
		if sum[tx.SourceID] == nil {
			sum[tx.SourceID] = map[string]float64{}
		}
		for _, l := range tx.Legs {
			sum[tx.SourceID][l.AssetKey] += l.Amount
		}
	}
	var out []Parked
	for w, assets := range sum {
		for a, v := range assets {
			if v > dust {
				out = append(out, Parked{Wallet: w, Asset: a, Amount: v})
			}
		}
	}
	slices.SortFunc(out, func(a, b Parked) int {
		return cmp.Or(strings.Compare(a.Wallet, b.Wallet), strings.Compare(a.Asset, b.Asset))
	})
	return out
}
