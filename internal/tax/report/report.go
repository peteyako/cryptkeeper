// Package report is the shared model behind every tax report (CSV, TXF, print pages): a
// year's rows with symbols, wallet labels and Form 8949 boxes resolved.
package report

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"crypto-portfolio/internal/tax"
)

// Resolver looks up the display data a report needs. Any field may be nil.
type Resolver struct {
	// Symbol returns the ticker and name of an asset key; for an spl: key without a
	// symbol the report falls back to the short mint form.
	Symbol func(assetKey string) (symbol, name string)
	// WalletLabel names a wallet; tax.Untracked reads "Untracked (from Koinly)".
	WalletLabel func(walletID string) string
	// WalletType is "coinbase" | "kraken" | "bitcoin" | "solana" | "manual" | "".
	WalletType func(walletID string) string
	// Box is tax.yaml WalletBox: wallet to short box letter G/H/I.
	Box map[string]string
	// LotWallet maps the wallet part of a lot's acquiring tx ID to the wallet that holds it
	// now (CSV imports counted as their exchange's wallet); nil keeps it.
	LotWallet func(wallet string) string
	// Price is the year-end price of an asset.
	Price func(assetKey string, day time.Time) (float64, bool)
}

// Row is a disposal with display data and its 8949 box (G..L).
type Row struct {
	tax.Disposal
	Symbol, WalletLabel, Box string
}

// HoldingRow is one asset held at year end.
type HoldingRow struct {
	AssetKey, Symbol, Name             string
	Quantity, CostUSD, ValueUSD, Price float64
	Priced                             bool
}

// BoxTotal totals the rows of one box (or of one Schedule D part, see Totals).
type BoxTotal struct {
	Proceeds, Cost, Gain float64
	Count                int
}

// YearReport is everything a report needs about one tax year.
type YearReport struct {
	Year      int
	Source    string
	Closed    bool
	AsOf      time.Time
	Loc       *time.Location
	Rows      []Row
	Income    []tax.Income
	GiftsLost []Row
	Holdings  []HoldingRow
	Summary   tax.Summary
	Issues    []tax.Issue
}

var digitalAssetBoxesFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// lotWallet is the wallet a lot's acquiring tx belongs to: the part of its "<source>:<id>"
// tx ID before the first colon.
func lotWallet(lotTxID string) string {
	w, _, _ := strings.Cut(lotTxID, ":")
	return w
}

// BoxFor returns the Form 8949 box of d. override is a short letter from tax.yaml
// walletBox (any case); lotWallet is the wallet the disposed lot was acquired in ("" when
// unknown). koinly means the wallet name is a Koinly label rather than a wallet ID. An
// override that is not G, H or I is returned as it is, for Form8949 to refuse.
func BoxFor(d tax.Disposal, walletType, lotWallet, override string, koinly bool) string {
	short := strings.ToUpper(override)
	switch short {
	case "", "G", "H", "I":
	default:
		return short
	}
	if short == "" {
		low := strings.ToLower(d.Wallet)
		exchange := walletType == "coinbase" || walletType == "kraken" ||
			(koinly && (strings.Contains(low, "coinbase") || strings.Contains(low, "kraken")))
		switch {
		case !exchange:
			short = "I"
		case lotWallet != "" && lotWallet == d.Wallet && !d.Acquired.Before(digitalAssetBoxesFrom):
			short = "G"
		default:
			short = "H"
		}
	}
	if d.Term == tax.LongTerm {
		switch short {
		case "G":
			return "J"
		case "H":
			return "K"
		default:
			return "L"
		}
	}
	return short
}

func (r Resolver) symbol(key string) (string, string) {
	var sym, name string
	if r.Symbol != nil {
		sym, name = r.Symbol(key)
	}
	if sym == "" {
		sym = key
		if mint, ok := strings.CutPrefix(key, "spl:"); ok {
			if rs := []rune(mint); len(rs) > 8 {
				sym = string(rs[:4]) + "..." + string(rs[len(rs)-4:]) // ASCII: it goes into TurboTax files
			} else {
				sym = mint
			}
		}
	}
	if name == "" {
		name = sym
	}
	return sym, name
}

func (r Resolver) label(w string) string {
	if r.WalletLabel != nil {
		if l := r.WalletLabel(w); l != "" {
			return l
		}
	}
	if w == tax.Untracked {
		return "Untracked (from Koinly)"
	}
	return w
}

func (r Resolver) row(d tax.Disposal, koinly bool) Row {
	sym, _ := r.symbol(d.Asset)
	var wt string
	if r.WalletType != nil {
		wt = r.WalletType(d.Wallet)
	}
	lw := lotWallet(d.LotTxID)
	if r.LotWallet != nil && lw != "" {
		lw = r.LotWallet(lw)
	}
	return Row{
		Disposal: d, Symbol: sym, WalletLabel: r.label(d.Wallet),
		Box: BoxFor(d, wt, lw, r.Box[d.Wallet], koinly),
	}
}

func (r Resolver) rows(ds []tax.Disposal, koinly bool) []Row {
	out := make([]Row, len(ds))
	for i, d := range ds {
		out[i] = r.row(d, koinly)
	}
	return out
}

func (r Resolver) holdings(lots []tax.Lot, asOf time.Time) []HoldingRow {
	byAsset := map[string]*HoldingRow{}
	var order []string
	for _, l := range lots {
		h := byAsset[l.Asset]
		if h == nil {
			sym, name := r.symbol(l.Asset)
			h = &HoldingRow{AssetKey: l.Asset, Symbol: sym, Name: name}
			byAsset[l.Asset] = h
			order = append(order, l.Asset)
		}
		h.Quantity += l.Amount
		h.CostUSD += l.CostUSD
	}
	out := make([]HoldingRow, 0, len(order))
	for _, k := range order {
		h := byAsset[k]
		if r.Price != nil {
			if p, ok := r.Price(k, asOf); ok {
				h.Price, h.Priced = p, true
				h.ValueUSD = p * h.Quantity
			}
		}
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b HoldingRow) int {
		return cmp.Or(strings.Compare(a.Symbol, b.Symbol), strings.Compare(a.AssetKey, b.AssetKey))
	})
	return out
}

func yearEnd(year int, loc *time.Location) time.Time {
	return time.Date(year, 12, 31, 23, 59, 59, 0, loc)
}

// FromClosed builds the report of a closed year from its stored rows. Koinly-sourced years
// carry Koinly wallet names, so their boxes come from the name.
func FromClosed(y tax.Year, r Resolver, loc *time.Location) YearReport {
	if loc == nil {
		loc = time.UTC
	}
	koinly := y.Source == "koinly"
	rows := r.rows(y.Disposals, koinly)
	asOf := yearEnd(y.Year, loc)
	return YearReport{
		Year: y.Year, Source: y.Source, Closed: true, AsOf: asOf, Loc: loc,
		Rows: rows, Income: slices.Clone(y.Income), GiftsLost: r.rows(y.GiftsLost, koinly),
		Holdings: r.holdings(y.ClosingLots, asOf), Summary: y.Summary,
	}
}

// FromOpen builds the report of the open year from a computation, keeping only the
// disposals, income and gifts/lost dated in year (in loc).
func FromOpen(year int, res tax.Result, r Resolver, loc *time.Location, now time.Time) YearReport {
	if loc == nil {
		loc = time.UTC
	}
	return FromOpenRange(year, res, r, loc, now, time.Date(year, 1, 1, 0, 0, 0, 0, loc), time.Date(year+1, 1, 1, 0, 0, 0, 0, loc))
}

// FromOpenRange is FromOpen keeping the rows dated in [from, to) rather than in the calendar
// year: the first open year after a year closed in another timezone starts at that close's
// boundary (see tax.OpenYearStart).
func FromOpenRange(year int, res tax.Result, r Resolver, loc *time.Location, now, from, to time.Time) YearReport {
	if loc == nil {
		loc = time.UTC
	}
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	var disp, gl []tax.Disposal
	for _, d := range res.Disposals {
		if in(d.Sold) {
			disp = append(disp, d)
		}
	}
	for _, d := range res.GiftsLost {
		if in(d.Sold) {
			gl = append(gl, d)
		}
	}
	var inc []tax.Income
	for _, i := range res.Income {
		if in(i.Date) {
			inc = append(inc, i)
		}
	}
	asOf := yearEnd(year, loc)
	if now.Before(asOf) {
		asOf = now
	}
	rows := r.rows(disp, false)
	return YearReport{
		Year: year, Source: "cryptkeeper", AsOf: asOf, Loc: loc,
		Rows: rows, Income: inc, GiftsLost: r.rows(gl, false),
		Holdings: r.holdings(res.Lots, asOf), Summary: tax.Summarize(disp, inc),
		Issues: slices.Clone(res.Issues),
	}
}
