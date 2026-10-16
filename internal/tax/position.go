package tax

import (
	"math"

	"crypto-portfolio/internal/core"
)

// position handles a tx that moves tokens into or out of a position (a JUP vote lock,
// perps collateral, an LP): not a taxable event. A negative leg (deposit) is parked and
// leaves the lots alone; a positive leg (withdrawal) first draws down what is parked in the
// tx's wallet for that asset, with no lot change. Any excess is new value: a lot at market
// value, Income of Type "position" at that value, and IssuePositionExcess for review (the
// position may have paid yield, or the deposit was not seen). An unvalued excess is worth 0
// and raises IssueMissingPrice. The tx's fee is disposed by apply, as for any tx.
//
// It does not depend on the tag, so a position-move tx kind can be routed to it directly.
func (e *engine) position(tx core.Tx, legs []*valued) {
	for _, v := range legs {
		switch {
		case math.Abs(v.Amount) <= dust:
		case v.Amount < 0:
			e.park(tx.SourceID, v.AssetKey, -v.Amount)
		default:
			excess := v.Amount - e.unpark(tx.SourceID, v.AssetKey, v.Amount)
			if excess <= dust {
				continue
			}
			value := e.usd(tx, v) * excess / v.Amount // pro-rata share of the leg's value
			e.acquire(tx, v.AssetKey, excess, value)
			e.out.Income = append(e.out.Income, Income{
				Date: tx.Time, Asset: v.AssetKey, Amount: excess, ValueUSD: value,
				Type: "position", Wallet: tx.SourceID, TxID: tx.ID,
			})
			e.issue(IssuePositionExcess, tx, v.AssetKey, excess)
		}
	}
}

// park adds amount of asset to what wallet has in positions.
func (e *engine) park(wallet, asset string, amount float64) {
	byAsset, ok := e.parked[wallet]
	if !ok {
		byAsset = map[string]float64{}
		e.parked[wallet] = byAsset
	}
	byAsset[asset] += amount
}

// unpark removes up to amount of asset from what wallet has in positions and returns what
// it removed.
func (e *engine) unpark(wallet, asset string, amount float64) float64 {
	got := math.Min(amount, e.parked[wallet][asset])
	if got > 0 {
		e.parked[wallet][asset] -= got
	}
	return got
}
