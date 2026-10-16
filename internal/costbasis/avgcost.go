package costbasis

import (
	"math"
	"sort"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

// dust is the unit amount below which a position counts as fully closed. It absorbs
// floating-point residue so a full disposal leaves exactly zero.
const dust = 1e-12

// fiat are the currencies the engine does not track as assets: they only carry value.
var fiat = map[string]bool{"USD": true, "EUR": true, "GBP": true, "CAD": true, "JPY": true, "AUD": true, "CHF": true}

// AssetResult is the engine's view of one asset.
type AssetResult struct {
	AssetKey string
	// Amount and Basis cover the units with a known USD cost; they can differ from the
	// current holdings (units acquired outside the tracked history are not in here).
	Amount  float64
	Basis   float64 // USD paid for Amount
	AvgCost float64 // Basis / Amount, 0 when Amount is 0
	// RealizedByYear is the realized P&L per calendar year (UTC) of the disposal.
	RealizedByYear map[int]float64
	// UnknownBasisUnits are units that were acquired with no USD value (receives from
	// outside, rewards or swaps without a price) and not yet sold. They are excluded from
	// Amount, Basis and AvgCost.
	UnknownBasisUnits float64
	// Overridden is set when a manual entry replaced the computed Amount and Basis.
	Overridden bool
}

// Result is the output of Compute.
type Result struct {
	Assets map[string]AssetResult
}

// Compute runs the global average-cost engine over txs and the manual entries.
//
// Rules (this is an estimate, not tax advice):
//
//   - Cost is averaged per asset across every source; moving coins between your own wallets
//     never changes basis. Transactions are processed oldest first; input order is irrelevant.
//   - Skipped entirely: failed txs, kind transfer, kind stake, and any tx with PeerID set
//     (one side of an internal transfer). This includes network fees on those moves.
//   - Fiat legs (USD, EUR, ...) are not tracked; they only supply values. Stablecoins
//     (core.Pegged) are tracked, and a stablecoin leg without a USD value is worth its
//     amount.
//   - trade / swap: every negative leg is a disposal and every positive leg an acquisition.
//     Leg.USD is used when present. When a side has exactly one unvalued leg and the other
//     side is fully valued, it takes the other side's total (acquisition cost = disposal
//     proceeds and vice versa). Otherwise the value is unknown: an unknown-value disposal
//     realizes nothing, an unknown-value acquisition adds UnknownBasisUnits.
//   - reward: positive legs are acquisitions at fair market value (Leg.USD); without a
//     price they are unknown-basis units.
//   - receive, send, fee, other: movements. Their Leg.USD is display-only: a receive adds
//     unknown-basis units (set a basis in the manual file), a send or standalone fee removes
//     units without realizing anything.
//   - Disposal of q units: the known-basis units go first, at the running average cost:
//     basis_removed = avg x min(q, Amount). Realized = proceeds - basis_removed, using the
//     share of the proceeds that belongs to the known units (proceeds x known/q); the part
//     of a sale that consumed unknown-basis units realizes nothing. The rest of q is taken
//     from UnknownBasisUnits, clamped at zero.
//   - Fees (Tx.Fee): fee units are removed like a disposal without proceeds: Amount and
//     Basis shrink proportionally (the average cost is unchanged) and nothing is realized.
//     Fiat fees are ignored.
//   - Manual lots are acquisitions at their date (a lot without a date counts before all
//     transactions). A manual assets entry without Override is an opening lot. An entry with
//     Override replaces the computed Amount and Basis once everything else has run, clears
//     UnknownBasisUnits and sets Overridden; realized history stays.
func Compute(txs []core.Tx, manual Manual) Result {
	e := &engine{assets: map[string]*assetState{}}
	manual = usableManual(manual)

	// Opening lots (assets entries without override) come before all history.
	for _, a := range manual.Assets {
		if !a.Override {
			e.state(a.Asset).acquire(a.Amount, a.CostUSD)
		}
	}

	lots := append([]ManualLot(nil), manual.Lots...)
	sort.SliceStable(lots, func(i, j int) bool { return lots[i].Date.Time().Before(lots[j].Date.Time()) })

	sorted := append([]core.Tx(nil), txs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Time.Equal(sorted[j].Time) {
			return sorted[i].Time.Before(sorted[j].Time)
		}
		return sorted[i].ID < sorted[j].ID
	})

	next := 0
	for _, tx := range sorted {
		for next < len(lots) && !lots[next].Date.Time().After(tx.Time) {
			e.state(lots[next].Asset).acquire(lots[next].Amount, lots[next].CostUSD)
			next++
		}
		e.apply(tx)
	}
	for ; next < len(lots); next++ {
		e.state(lots[next].Asset).acquire(lots[next].Amount, lots[next].CostUSD)
	}

	for _, a := range manual.Assets {
		if a.Override {
			s := e.state(a.Asset)
			s.amount, s.basis, s.unknown, s.overridden = a.Amount, a.CostUSD, 0, true
		}
	}

	out := Result{Assets: make(map[string]AssetResult, len(e.assets))}
	for key, s := range e.assets {
		r := AssetResult{
			AssetKey: key, Amount: s.amount, Basis: s.basis,
			RealizedByYear: s.realized, UnknownBasisUnits: s.unknown, Overridden: s.overridden,
		}
		if s.amount > 0 {
			r.AvgCost = s.basis / s.amount
		}
		out.Assets[key] = r
	}
	return out
}

// usableManual canonicalizes asset keys and drops entries that would corrupt the state
// (non-positive amounts, negative or non-finite costs), so a hand-built Manual that skipped
// ParseManual cannot break the engine.
func usableManual(m Manual) Manual {
	ok := func(amount, cost float64) bool { return checkAmounts(amount, cost) == nil }
	var out Manual
	for _, a := range m.Assets {
		if key, err := normalizeAssetKey(a.Asset); err == nil && ok(a.Amount, a.CostUSD) {
			a.Asset = key
			out.Assets = append(out.Assets, a)
		}
	}
	for _, l := range m.Lots {
		if key, err := normalizeAssetKey(l.Asset); err == nil && ok(l.Amount, l.CostUSD) {
			l.Asset = key
			out.Lots = append(out.Lots, l)
		}
	}
	return out
}

type assetState struct {
	amount, basis, unknown float64
	realized               map[int]float64
	overridden             bool
}

type engine struct{ assets map[string]*assetState }

func (e *engine) state(key string) *assetState {
	s, ok := e.assets[key]
	if !ok {
		s = &assetState{realized: map[int]float64{}}
		e.assets[key] = s
	}
	return s
}

// acquire adds q units with a known total cost.
func (s *assetState) acquire(q, usd float64) {
	s.amount += q
	s.basis += usd
}

// dispose removes q units, known-basis units first. proceeds (nil when unknown) is the total
// USD received for all q units.
func (s *assetState) dispose(q float64, proceeds *float64, year int) {
	if q <= 0 {
		return
	}
	known := math.Min(q, s.amount)
	if known > 0 {
		removed := s.basis / s.amount * known
		if proceeds != nil {
			s.realized[year] += *proceeds*known/q - removed
		}
		s.amount -= known
		s.basis -= removed
		if s.amount <= dust {
			s.amount, s.basis = 0, 0
		}
	}
	if rest := q - known; rest > 0 {
		s.unknown = math.Max(0, s.unknown-rest)
		if s.unknown <= dust {
			s.unknown = 0
		}
	}
}

func tracked(assetKey string) bool { return !fiat[assetKey] }

func (e *engine) apply(tx core.Tx) {
	if tx.Status == core.TxFailed || tx.PeerID != "" {
		return
	}
	year := tx.Time.UTC().Year()
	switch tx.Kind {
	case core.TxTransfer, core.TxStake:
		return
	case core.TxTrade, core.TxSwap:
		e.trade(tx, year)
	case core.TxReward:
		for _, l := range tx.Legs {
			if !tracked(l.AssetKey) || l.Amount == 0 {
				continue
			}
			if l.Amount < 0 {
				e.state(l.AssetKey).dispose(-l.Amount, nil, year)
			} else if v, ok := legValue(l); ok {
				e.state(l.AssetKey).acquire(l.Amount, v)
			} else {
				e.state(l.AssetKey).unknown += l.Amount
			}
		}
	default: // receive, send, fee, other: plain movements
		for _, l := range tx.Legs {
			if !tracked(l.AssetKey) || l.Amount == 0 {
				continue
			}
			if l.Amount < 0 {
				e.state(l.AssetKey).dispose(-l.Amount, nil, year)
			} else {
				e.state(l.AssetKey).unknown += l.Amount
			}
		}
	}
	if f := tx.Fee; f != nil && f.Amount != 0 && tracked(f.AssetKey) {
		e.state(f.AssetKey).dispose(math.Abs(f.Amount), nil, year)
	}
}

// legValue is the absolute USD value of a leg: Leg.USD when it is a positive number,
// else the amount for pegged stablecoins and USD.
func legValue(l core.Leg) (float64, bool) {
	if l.USD != nil && *l.USD != 0 && !math.IsNaN(*l.USD) && !math.IsInf(*l.USD, 0) {
		return math.Abs(*l.USD), true
	}
	if core.Pegged[l.AssetKey] {
		return math.Abs(l.Amount), true
	}
	return 0, false
}

type valuedLeg struct {
	core.Leg
	usd   float64
	known bool
}

// trade applies a trade or swap: disposals first, then acquisitions.
func (e *engine) trade(tx core.Tx, year int) {
	var neg, pos []*valuedLeg
	for _, l := range tx.Legs {
		if l.Amount == 0 {
			continue
		}
		v := &valuedLeg{Leg: l}
		v.usd, v.known = legValue(l)
		if l.Amount < 0 {
			neg = append(neg, v)
		} else {
			pos = append(pos, v)
		}
	}
	// A side with one unvalued leg takes whatever the other, fully valued, side is worth.
	negTotal, negAll := sideValue(neg)
	posTotal, posAll := sideValue(pos)
	if !negAll && posAll {
		fillSide(neg, posTotal)
	} else if !posAll && negAll {
		fillSide(pos, negTotal)
	}

	for _, l := range neg {
		if !tracked(l.AssetKey) {
			continue
		}
		var proceeds *float64
		if l.known {
			proceeds = fptr(l.usd)
		}
		e.state(l.AssetKey).dispose(-l.Amount, proceeds, year)
	}
	for _, l := range pos {
		if !tracked(l.AssetKey) {
			continue
		}
		if l.known {
			e.state(l.AssetKey).acquire(l.Amount, l.usd)
		} else {
			e.state(l.AssetKey).unknown += l.Amount
		}
	}
}

// sideValue sums the known values of a side and reports whether every leg has one.
func sideValue(legs []*valuedLeg) (total float64, all bool) {
	all = len(legs) > 0
	for _, l := range legs {
		if l.known {
			total += l.usd
		} else {
			all = false
		}
	}
	return total, all
}

// fillSide values the single unvalued leg of a side as total minus the side's known legs.
func fillSide(legs []*valuedLeg, total float64) {
	var unknown *valuedLeg
	known := 0.0
	for _, l := range legs {
		switch {
		case l.known:
			known += l.usd
		case unknown == nil:
			unknown = l
		default:
			return // two unvalued legs: cannot split the value
		}
	}
	if unknown != nil && total-known > 0 {
		unknown.usd, unknown.known = total-known, true
	}
}

// Unrealized returns the basis attributed to the current holding and its unrealized P&L.
// The basis is avg cost x min(currentAmount, tracked amount): only units with a known cost
// are covered, and unrealized = covered units x price - basis. pct is in percent (50 means
// +50%) and 0 when the basis is 0. ok is false when there is nothing with a known basis.
func (r Result) Unrealized(asset string, currentAmount, priceUSD float64) (basis, unrealized, pct float64, ok bool) {
	a, found := r.Assets[asset]
	if !found || a.Amount <= 0 || currentAmount <= 0 {
		return 0, 0, 0, false
	}
	covered := math.Min(currentAmount, a.Amount)
	basis = a.AvgCost * covered
	unrealized = covered*priceUSD - basis
	if basis > 0 {
		pct = unrealized / basis * 100
	}
	return basis, unrealized, pct, true
}

// Coverage is the share (0..1) of the current USD value whose units have a known basis. An
// asset with a tracked amount below the holding is covered proportionally. It is 0 when the
// assets have no value.
func Coverage(r Result, assets []portfolio.AssetRow) float64 {
	var total, covered float64
	for _, row := range assets {
		if row.ValueUSD <= 0 || row.Amount <= 0 {
			continue
		}
		total += row.ValueUSD
		if a, ok := r.Assets[row.AssetKey]; ok && a.Amount > 0 {
			covered += row.ValueUSD * math.Min(1, a.Amount/row.Amount)
		}
	}
	if total == 0 {
		return 0
	}
	return covered / total
}

// ForPortfolio attributes basis to the assets of a (possibly filtered) snapshot pro-rata by
// amount: per asset, the covered units are min(row amount, tracked amount), the basis is the
// average cost times those units, and value is the current value of the same units
// (row value x covered/amount). unrealized = value - basis, so assets without a known basis
// are left out of all three numbers (see Coverage).
func ForPortfolio(r Result, assets []portfolio.AssetRow) (basis, value, unrealized float64) {
	for _, row := range assets {
		a, ok := r.Assets[row.AssetKey]
		if !ok || a.Amount <= 0 || row.Amount <= 0 {
			continue
		}
		covered := math.Min(row.Amount, a.Amount)
		basis += a.AvgCost * covered
		value += row.ValueUSD * covered / row.Amount
	}
	return basis, value, value - basis
}
