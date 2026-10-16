package demo

import (
	"time"

	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/web/templates"
)

// costSpec is what the fixtures say CryptKeep knows about the cost of one asset: the total paid
// for its known units, how many units have no known cost, and whether a manual entry
// overrides the computed cost.
type costSpec struct {
	basis      float64
	unknown    float64
	has        bool
	overridden bool
}

// costSpecs: BTC and SOL carry manual costs; BTC also has 0.05 held by hand with no cost;
// DOT and BONK have none; USDC is at par; ETH and JUP are under water; the rest are up.
var costSpecs = map[string]costSpec{
	"BTC":  {basis: 17980.40, unknown: 0.05, has: true, overridden: true},
	"ETH":  {basis: 12480.00, has: true},
	"SOL":  {basis: 5962.30, has: true, overridden: true},
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
		spec := costSpecs[a.AssetKey]
		r := templates.PnLRow{
			AssetKey: a.AssetKey, Symbol: a.Symbol, Name: a.Name, Amount: a.Amount, Value: a.ValueUSD,
			HasBasis: spec.has, Overridden: spec.overridden, UnknownUnits: spec.unknown,
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

// Reports returns the Reports page: profit and loss for every holding, realized results by
// year, two manual costs and no import result.
func Reports(now time.Time) templates.ReportsPage { return baseReports(now) }

// ReportsWith is Reports with a different list of manual costs, as the preview's stub
// forms change it: an entry that differs from the fixture's overrides the computed cost
// of its asset, and dropping a fixture entry takes its "manual" badge away.
func ReportsWith(now time.Time, manual []templates.ManualBasisRow) templates.ReportsPage {
	p := baseReports(now)
	was := map[string]templates.ManualBasisRow{}
	for _, m := range p.Manual {
		was[m.Asset] = m
	}
	now2 := map[string]templates.ManualBasisRow{}
	for _, m := range manual {
		now2[m.Asset] = m
	}
	p.Manual = manual
	var basis, unrealized, known float64
	for i := range p.Rows {
		r := &p.Rows[i]
		m, has := now2[r.AssetKey]
		old, wasThere := was[r.AssetKey]
		switch {
		case has && !(wasThere && old == m) && r.Amount > 0:
			k := min(m.Amount, r.Amount)
			r.HasBasis, r.Overridden = true, true
			r.UnknownUnits = r.Amount - k
			if r.UnknownUnits < 1e-9 {
				r.UnknownUnits = 0
			}
			r.Basis = m.CostUSD
			r.AvgCost = 0
			if k > 0 {
				r.AvgCost = r.Basis / k
			}
			r.Unrealized = round2(r.Value*k/r.Amount - r.Basis)
			r.UnrealizedPct = 0
			if r.Basis > 0 {
				r.UnrealizedPct = r.Unrealized / r.Basis * 100
			}
		case !has && wasThere:
			r.Overridden = false
		}
		if r.HasBasis && r.Amount > 0 {
			basis += r.Basis
			unrealized += r.Unrealized
			known += r.Value * (r.Amount - r.UnknownUnits) / r.Amount
		}
	}
	p.TotalBasis, p.TotalUnrealized = round2(basis), round2(unrealized)
	p.Coverage = 0
	if p.TotalValue > 0 {
		p.Coverage = known / p.TotalValue
	}
	return p
}

func baseReports(now time.Time) templates.ReportsPage {
	snap := Snapshot(now)
	rows, basis, unrealized, known := pnlRows(snap)
	p := templates.ReportsPage{
		Shell:           Shell("Reports", "reports", "all", ""),
		Rows:            rows,
		TotalBasis:      basis,
		TotalValue:      snap.TotalUSD,
		TotalUnrealized: unrealized,
		Realized: []templates.YearAmount{
			{Year: now.Year(), USD: 1204.12}, {Year: now.Year() - 1, USD: -212.40}, {Year: now.Year() - 2, USD: 3880.00},
		},
		Manual: []templates.ManualBasisRow{
			{Asset: "BTC", Amount: 0.30006114, CostUSD: 11402.50, Note: "Bought over 2021 to 2023"},
			{Asset: "SOL", Amount: 25, CostUSD: 2410.00, Note: "Kraken, before the API key"},
		},
		AssetOptions: assetOptions(now),
		Now:          now,
	}
	if snap.TotalUSD > 0 {
		p.Coverage = known / snap.TotalUSD
	}
	return p
}

// ReportsEmpty is the Reports page for a fresh install: nothing held, nothing known.
func ReportsEmpty(now time.Time) templates.ReportsPage {
	return templates.ReportsPage{Shell: Shell("Reports", "reports", "all", ""), Now: now}
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
