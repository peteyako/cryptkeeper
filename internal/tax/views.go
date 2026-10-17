package tax

import (
	"time"

	"crypto-portfolio/internal/portfolio"
)

// Read helpers over a Result, for the dashboard, the wallet tiles and Reports.

// holding is what the open lots of one asset add up to in a set of wallets.
type holding struct {
	amount, cost         float64 // every lot
	missing, missingCost float64 // the FlagMissingBasis lots: amount and cost
}

// based is the amount whose cost is known: every lot but the missing-basis ones.
func (h holding) based() (amount, cost float64) { return h.amount - h.missing, h.cost - h.missingCost }

// sum adds up the open lots of asset in the wallets that pass the filter; a nil filter is
// every wallet except Untracked.
func (r Result) sum(asset string, wallets func(string) bool) holding {
	var h holding
	for _, l := range r.Lots {
		if l.Asset != asset || !inWallets(l.Wallet, wallets) {
			continue
		}
		h.amount += l.Amount
		h.cost += l.CostUSD
		if l.Flag == FlagMissingBasis {
			h.missing += l.Amount
			h.missingCost += l.CostUSD
		}
	}
	return h
}

func inWallets(wallet string, wallets func(string) bool) bool {
	if wallets == nil {
		return wallet != Untracked
	}
	return wallets(wallet)
}

// Holding sums the open lots of asset in the wallets that pass the filter (nil = every
// wallet except Untracked): their amount and cost, and missing, the amount held in
// FlagMissingBasis lots (included in amount).
func (r Result) Holding(asset string, wallets func(string) bool) (amount, cost, missing float64) {
	h := r.sum(asset, wallets)
	return h.amount, h.cost, h.missing
}

// Unrealized is the basis and unrealized profit or loss of currentAmount units of asset at
// priceUSD, from the open lots in the wallets that pass the filter (nil = every wallet
// except Untracked). Missing-basis lots are left out: the covered units are
// min(currentAmount, amount − missing), and their basis is the cost of the other lots
// pro-rata by amount. unrealized = covered × price − basis; pct is in percent (50 = +50%)
// and 0 when the basis is 0. ok is false when nothing is covered.
func (r Result) Unrealized(asset string, wallets func(string) bool, currentAmount, priceUSD float64) (basis, unrealized, pct float64, ok bool) {
	amount, cost := r.sum(asset, wallets).based()
	covered := min(currentAmount, amount)
	if !(covered > dust) { // also catches NaN
		return 0, 0, 0, false
	}
	basis = cost * covered / amount
	unrealized = covered*priceUSD - basis
	if basis > 0 {
		pct = unrealized / basis * 100
	}
	return basis, unrealized, pct, true
}

// RealizedByYear totals the gain of the disposals by the calendar year of the sale in loc
// (nil = UTC).
func (r Result) RealizedByYear(loc *time.Location) map[int]float64 {
	if loc == nil {
		loc = time.UTC
	}
	out := map[int]float64{}
	for _, d := range r.Disposals {
		out[d.Sold.In(loc).Year()] += d.GainUSD
	}
	return out
}

// MissingBasisSales counts the disposals the lots could not cover (Note "Missing cost
// basis", cost 0): sales of units with no known cost, typically because the opening basis
// was never imported.
func (r Result) MissingBasisSales() int {
	n := 0
	for _, d := range r.Disposals {
		if d.Note == noteMissingBasis {
			n++
		}
	}
	return n
}

// Coverage is the share (0..1) of the rows' USD value whose units are covered by lots with
// a known basis, in every wallet except Untracked: per row, min(1, (amount − missing) /
// row amount). Rows without a value are ignored; with no value at all it is 0.
func Coverage(r Result, rows []portfolio.AssetRow) float64 {
	var total, covered float64
	for _, row := range rows {
		if !(row.ValueUSD > 0) || !(row.Amount > 0) {
			continue
		}
		total += row.ValueUSD
		if amount, _ := r.sum(row.AssetKey, nil).based(); amount > dust {
			covered += row.ValueUSD * min(1, amount/row.Amount)
		}
	}
	if total == 0 {
		return 0
	}
	return covered / total
}
