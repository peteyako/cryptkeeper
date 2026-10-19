package demo

import (
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
	"cryptkeeper/web/templates"
)

// costSpec is what the fixtures say the tax lots know about the cost of one asset: the
// total paid for the units they cover, how many units of the holding have no known cost,
// and how many of those sit in missing-basis lots.
type costSpec struct {
	basis   float64
	unknown float64
	missing float64
	has     bool
}

// costSpecs: BTC holds 0.05 beyond its imported cost (a missing-basis lot); DOT and BONK
// have no lots; USDC is at par; ETH and JUP are under water; the rest are up.
var costSpecs = map[string]costSpec{
	"BTC":  {basis: 17980.40, unknown: 0.05, missing: 0.05, has: true},
	"ETH":  {basis: 12480.00, has: true},
	"SOL":  {basis: 5962.30, has: true},
	"USDC": {basis: 4225.60, has: true},
	"LINK": {basis: 525.62, has: true},
	"JUP":  {basis: 1375.00, has: true},
	"DOT":  {unknown: 185.20},
	"BONK": {unknown: 12_500_000},
}

// pnlRows builds the profit and loss rows from the snapshot and costSpecs. Every number is
// derived from the others: the average cost is the basis over the known units, the
// unrealized result compares the value of the known units with that basis.
func pnlRows(snap portfolio.Snapshot) (rows []templates.PnLRow, basis, unrealized, knownValue float64) {
	for _, a := range snap.Assets {
		if _, stock := core.IsStockKey(a.AssetKey); stock {
			continue // brokerage holdings have no tax lots
		}
		spec := costSpecs[a.AssetKey]
		r := templates.PnLRow{
			AssetKey: a.AssetKey, Symbol: a.Symbol, Name: a.Name, Amount: a.Amount, Value: a.ValueUSD,
			HasBasis: spec.has, UnknownUnits: spec.unknown, MissingUnits: spec.missing,
		}
		if spec.has && a.Amount > 0 {
			known := a.Amount - spec.unknown
			kv := a.ValueUSD * known / a.Amount
			r.Basis = spec.basis
			r.AvgCost = spec.basis / known
			r.Unrealized = round2(kv - spec.basis)
			if spec.basis > 0 {
				r.UnrealizedPct = r.Unrealized / spec.basis * 100
			}
			basis += r.Basis
			unrealized += r.Unrealized
			knownValue += kv
		}
		rows = append(rows, r)
	}
	return rows, round2(basis), round2(unrealized), knownValue
}

// Reports returns the Reports page: profit and loss for every holding from the tax lots,
// realized results by year and no import result.
func Reports(now time.Time) templates.ReportsPage { return baseReports(now) }

func baseReports(now time.Time) templates.ReportsPage {
	snap := Snapshot(now)
	rows, basis, unrealized, known := pnlRows(snap)
	var cryptoValue float64
	for _, r := range rows {
		cryptoValue += r.Value
	}
	p := templates.ReportsPage{
		Shell:           Shell("Reports", "reports", "all", ""),
		Rows:            rows,
		TotalBasis:      basis,
		TotalValue:      round2(cryptoValue),
		TotalUnrealized: unrealized,
		Realized: []templates.YearAmount{
			{Year: now.Year(), USD: 1204.12}, {Year: now.Year() - 1, USD: -212.40}, {Year: now.Year() - 2, USD: 3880.00},
		},
		Now:    now,
		Tax:    TaxPage(taxOpenYear, "gains", false),
		Review: reviewPage(),
	}
	if cryptoValue > 0 {
		p.Coverage = known / cryptoValue
	}
	return p
}

// ReportsEmpty is the Reports page for a fresh install: nothing held, nothing known.
func ReportsEmpty(now time.Time) templates.ReportsPage {
	return templates.ReportsPage{
		Shell: Shell("Reports", "reports", "all", ""), Now: now, Tax: TaxPage(taxOpenYear, "gains", true),
	}
}

// PnLCells is the unrealized result per asset key, for the overview's P/L column. Assets
// without a known cost have OK false.
func PnLCells(now time.Time) map[string]templates.PnLCell {
	rows, _, _, _ := pnlRows(Snapshot(now))
	cells := make(map[string]templates.PnLCell, len(rows))
	for _, r := range rows {
		cells[r.AssetKey] = templates.PnLCell{Unrealized: r.Unrealized, Pct: r.UnrealizedPct, OK: r.HasBasis}
	}
	return cells
}
