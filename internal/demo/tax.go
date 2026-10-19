package demo

import (
	"slices"
	"strings"
	"sync"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/tax/report"
	"crypto-portfolio/web/templates"
)

// The tax fixtures are invented and fixed in time (the preview has no clock-dependent tax
// year): 2025 is a closed Koinly year, 2026 is open and computed by the real tax.Compute
// over a dozen demo transactions. Every number is derived from those inputs: the summary is
// tax.Summarize over the rows, the box totals are the rows' sums, the holdings are the open
// lots at the fixed demo prices. Nothing here is hand-typed twice.

const (
	taxClosedYear = 2025
	taxOpenYear   = 2026
)

// taxNow is "today" for the tax screens.
var taxNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func taxDay(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }

// koinlyYear is the closed 2025 year as the Koinly import freezes it: wallet names are
// Koinly's labels, one lot predates the year (VARIOUS), and the closing lots carry the
// wallet IDs the open year continues from.
func koinlyYear() tax.Year {
	cb, ledger := "Coinbase", "Ledger"
	rows := []tax.Disposal{
		{Sold: taxDay(2025, 3, 14, 15), Acquired: taxDay(2024, 5, 2, 9), Asset: "ETH", Amount: 1.0, CostUSD: 2210, ProceedsUSD: 1880, GainUSD: -330, Wallet: cb, Term: tax.LongTerm},
		{Sold: taxDay(2025, 6, 2, 12), Acquired: taxDay(2025, 1, 20, 9), Asset: "SOL", Amount: 12, CostUSD: 2160, ProceedsUSD: 1980, GainUSD: -180, Wallet: cb, Term: tax.ShortTerm},
		{Sold: taxDay(2025, 9, 9, 17), Acquired: taxDay(2023, 11, 1, 0), Various: true, Asset: "BTC", Amount: 0.05, CostUSD: 1750, ProceedsUSD: 5680, GainUSD: 3930, Wallet: ledger, Term: tax.LongTerm},
		{Sold: taxDay(2025, 11, 3, 10), Acquired: taxDay(2025, 4, 8, 9), Asset: "LINK", Amount: 40, CostUSD: 540, ProceedsUSD: 905, GainUSD: 365, Wallet: cb, Term: tax.ShortTerm},
	}
	income := []tax.Income{
		{Date: taxDay(2025, 4, 30, 0), Asset: "SOL", Amount: 0.8, ValueUSD: 112, Type: "reward", Wallet: cb},
		{Date: taxDay(2025, 10, 31, 0), Asset: "USDC", Amount: 64, ValueUSD: 64, Type: "reward", Wallet: cb},
	}
	return tax.Year{
		Version: 1, Year: taxClosedYear, Source: "koinly", ClosedAt: taxDay(2026, 1, 20, 9),
		Summary: tax.Summarize(rows, income), Disposals: rows, Income: income,
		ClosingLots: []tax.Lot{
			{Wallet: IDCoinbase, Asset: "BTC", Amount: 0.20, CostUSD: 8000, Acquired: taxDay(2024, 12, 31, 0), Various: true},
			{Wallet: IDCoinbase, Asset: "ETH", Amount: 2.0, CostUSD: 5000, Acquired: taxDay(2025, 3, 1, 9)},
			{Wallet: IDPhantom, Asset: "SOL", Amount: 30, CostUSD: 3600, Acquired: taxDay(2025, 6, 10, 9)},
			{Wallet: IDCold, Asset: "BTC", Amount: 0.10, CostUSD: 5000, Acquired: taxDay(2024, 12, 31, 0), Various: true},
		},
	}
}

func taxLeg(key string, amount, usd float64) core.Leg {
	return core.Leg{AssetKey: key, Symbol: key, Amount: amount, USD: ptr(usd)}
}

func taxTx(wallet, id string, t time.Time, kind core.TxKind, legs ...core.Leg) core.Tx {
	return core.Tx{
		ID: core.TxID(wallet, id), SourceID: wallet, ExternalID: id, Time: t, Kind: kind,
		Status: core.TxConfirmed, Legs: legs,
		ExplorerURL: "https://explorer.invalid/tx/" + id,
	}
}

// taxTxs are the twelve demo transactions of 2026: a BTC sale, a fee, an ETH sale (long
// term), a transfer between Coinbase and Cold storage that pays a small network fee, an
// on-chain SOL to JUP swap, two staking rewards, a JUP position deposit, an unmatched BONK
// receive, a DOT receive with no price, and a SOL sale.
func taxTxs() []core.Tx {
	cbOut := taxTx(IDCoinbase, "xfer-out", taxDay(2026, 3, 20, 11), core.TxTransfer, taxLeg("BTC", -0.05, -3150))
	coldIn := taxTx(IDCold, "xfer-in", taxDay(2026, 3, 20, 12), core.TxTransfer, taxLeg("BTC", 0.0499, 3143.7))
	cbOut.PeerID, cbOut.PeerSource, cbOut.TxHash = coldIn.ID, IDCold, "demohash-xfer"
	coldIn.PeerID, coldIn.PeerSource, coldIn.TxHash = cbOut.ID, IDCoinbase, "demohash-xfer"
	dot := taxTx(IDKraken, "dot-in", taxDay(2026, 8, 5, 8), core.TxReceive, core.Leg{AssetKey: "DOT", Symbol: "DOT", Amount: 30})
	return []core.Tx{
		taxTx(IDCoinbase, "sell-btc", taxDay(2026, 2, 10, 14), core.TxTrade, taxLeg("BTC", -0.05, -4750), taxLeg("USDC", 4750, 4750)),
		taxTx(IDCoinbase, "fee-1", taxDay(2026, 3, 2, 9), core.TxFee, taxLeg("USDC", -2.5, -2.5)),
		cbOut, coldIn,
		taxTx(IDCoinbase, "sell-eth", taxDay(2026, 4, 10, 16), core.TxTrade, taxLeg("ETH", -0.8, -2640), taxLeg("USDC", 2640, 2640)),
		taxTx(IDPhantom, "swap-sol-jup", taxDay(2026, 5, 12, 13), core.TxSwap, taxLeg("SOL", -10, -1400), taxLeg("JUP", 1500, 1400)),
		taxTx(IDPhantom, "reward-sol", taxDay(2026, 6, 20, 0), core.TxReward, taxLeg("SOL", 0.35, 51.3)),
		taxTx(IDCoinbase, "reward-usdc", taxDay(2026, 6, 30, 0), core.TxReward, taxLeg("USDC", 18, 18)),
		taxTx(IDPhantom, "jup-lock", taxDay(2026, 7, 1, 10), core.TxPosition, taxLeg("JUP", -500, -460)),
		taxTx(IDPhantom, "bonk-in", taxDay(2026, 7, 14, 18), core.TxReceive, taxLeg("BONK", 2_000_000, 42.8)),
		dot,
		taxTx(IDPhantom, "sell-sol", taxDay(2026, 9, 3, 15), core.TxTrade, taxLeg("SOL", -5, -760), taxLeg("USDC", 760, 760)),
	}
}

var taxOnce struct {
	sync.Once
	res tax.Result
	txs []core.Tx
}

// taxResult is tax.Compute over the opening lots of the closed year and the demo txs.
func taxResult() (tax.Result, []core.Tx) {
	taxOnce.Do(func() {
		taxOnce.txs = taxTxs()
		taxOnce.res = tax.Compute(tax.Input{
			From: taxDay(taxOpenYear, 1, 1, 0), Opening: koinlyYear().ClosingLots, Txs: taxOnce.txs,
		})
	})
	return taxOnce.res, taxOnce.txs
}

func taxResolver() report.Resolver {
	labels, types := map[string]string{}, map[string]string{}
	for _, w := range Snapshot(taxNow).Wallets {
		labels[w.ID], types[w.ID] = w.Label, w.Type
	}
	prices := Prices(taxNow)
	return report.Resolver{
		Symbol:      func(k string) (string, string) { return k, assetNames[k] },
		WalletLabel: func(w string) string { return labels[w] },
		WalletType:  func(w string) string { return types[w] },
		Price: func(k string, _ time.Time) (float64, bool) {
			p, ok := prices[k]
			return p.USD, ok
		},
	}
}

// YearReport is the report of a demo tax year: 2025 (closed, from Koinly) or 2026 (open,
// from tax.Compute). Any other year is an empty open report.
func YearReport(year int) report.YearReport {
	res := taxResolver()
	switch year {
	case taxClosedYear:
		return report.FromClosed(koinlyYear(), res, time.UTC)
	case taxOpenYear:
		r, _ := taxResult()
		return report.FromOpen(year, r, res, time.UTC, taxNow)
	}
	return emptyYearReport(year)
}

func emptyYearReport(year int) report.YearReport {
	return report.YearReport{Year: year, Source: "cryptkeep", Loc: time.UTC, AsOf: time.Date(year, 12, 31, 23, 59, 59, 0, time.UTC)}
}

// reviewTitles names each issue kind's group, in display order (as the real server does).
var reviewTitles = []struct {
	kind  tax.IssueKind
	title string
}{
	{tax.IssueUnmatchedReceive, "Unmatched receives"}, {tax.IssueUnmatchedSend, "Unmatched sends"},
	{tax.IssueMissingPrice, "Missing prices"}, {tax.IssueMissingBasis, "Missing cost basis"},
	{tax.IssuePositionExcess, "Position returned more than went in"}, {tax.IssueTransferUnpaired, "Unpaired transfers"},
}

// reviewPage groups the open year's issues by kind and asset, and lists what is parked.
func reviewPage() templates.ReviewPage {
	res, txs := taxResult()
	rs := taxResolver()
	byID := map[string]core.Tx{}
	for _, tx := range txs {
		byID[tx.ID] = tx
	}
	p := templates.ReviewPage{OpenYear: taxOpenYear, TagsChecked: true, Close: templates.CloseCard{ReopenYear: taxClosedYear}}
	for _, kt := range reviewTitles {
		byAsset := map[string][]tax.Issue{}
		for _, is := range res.Issues {
			if is.Kind == kt.kind {
				byAsset[is.Asset] = append(byAsset[is.Asset], is)
			}
		}
		var groups []templates.ReviewGroup
		for asset, issues := range byAsset {
			g := templates.ReviewGroup{Kind: kt.kind, Title: kt.title, Asset: asset, Symbol: asset, Count: len(issues)}
			for _, is := range issues {
				tx := byID[is.TxID]
				it := templates.ReviewItem{
					TxID: is.TxID, WalletLabel: rs.WalletLabel(is.Wallet), Time: is.Time.Format("2006-01-02 15:04"),
					Asset: asset, Symbol: asset, Amount: templates.CoinAmount(is.Amount, asset),
					Explorer: templates.SafeExplorerURL(tx.ExplorerURL), Direction: "in",
				}
				if kt.kind == tax.IssueMissingPrice {
					it.PriceKey = tax.PriceKey(is.TxID, 0)
				}
				if tx.Kind == core.TxReceive {
					it.TagOptions = []string{"purchase", "income", "airdrop", "gift-received", "own-wallet", "position", "spam"}
				}
				g.Items = append(g.Items, it)
			}
			groups = append(groups, g)
		}
		slices.SortFunc(groups, func(a, b templates.ReviewGroup) int { return strings.Compare(a.Asset, b.Asset) })
		p.Groups = append(p.Groups, groups...)
	}
	for _, pk := range res.Parked {
		p.Parked = append(p.Parked, templates.ParkedRow{
			Wallet: pk.Wallet, WalletLabel: rs.WalletLabel(pk.Wallet), Asset: pk.Asset, Symbol: pk.Asset, Amount: pk.Amount,
		})
	}
	return p
}

// TaxPage is the Tax section for a year, tab and the empty-install variant: with empty set,
// there is only the open year and nothing in it.
func TaxPage(year int, tab string, empty bool) templates.TaxPage {
	years := []int{taxClosedYear, taxOpenYear}
	rep := YearReport(year)
	if empty {
		years, year = []int{taxOpenYear}, taxOpenYear
		rep = emptyYearReport(year)
	} else if !slices.Contains(years, year) {
		year = taxOpenYear
		rep = YearReport(year)
	}
	p := templates.TaxPage{Years: years, Year: year, Tab: "gains", Page: 1, Pages: 1, Report: rep}
	p.ReviewTab = !rep.Closed && year == years[len(years)-1]
	if !rep.Closed {
		// The open year's scope picker: every wallet, or a portfolio that picks wallets.
		p.ScopeOptions = []templates.ScopeOption{{ID: "", Name: "All wallets"}, {ID: PFLongTerm, Name: "Long-term"}, {ID: PFTrading, Name: "Trading"}}
	}
	switch {
	case tab == "review" && p.ReviewTab:
		p.Tab = tab
		p.Review = reviewPage()
		if empty {
			p.Review = templates.ReviewPage{OpenYear: year, TagsChecked: true}
		}
	case tab == "income" || tab == "holdings":
		p.Tab = tab
	}
	if p.Tab == "income" {
		p.Symbols, p.Wallets = map[string]string{}, map[string]string{}
		rs := taxResolver()
		for _, i := range rep.Income {
			p.Symbols[i.Asset], _ = rs.Symbol(i.Asset)
			p.Wallets[i.Wallet] = rs.WalletLabel(i.Wallet)
		}
	}
	return p
}

// KoinlyPage is the import wizard at a step: 1 is the upload alone, 2 adds the mapping
// (files loaded, nothing reconciled yet); 3 and later add the reconciliation with its
// differences, ready to accept.
func KoinlyPage(step int) templates.KoinlyPage {
	p := Koinly(taxDay(taxOpenYear, 10, 16, 12))
	w := &p.Wizard
	switch {
	case step <= 1:
		return KoinlyEmpty(taxNow)
	case step == 2:
		w.Recon, w.Problems = nil, nil
	}
	return p
}

// TaxReport is the view model of the complete tax report page (the CPA report) for a demo
// year, built like the real server does from the year's report.
func TaxReport(year int) templates.TaxReportView {
	rep, rs := YearReport(year), taxResolver()
	v := templates.TaxReportView{Report: rep, Generated: taxNow.Format("01/02/2006")}
	var err error
	if v.Short, v.Long, err = report.Totals(rep); err != nil {
		v.TotalsErr = err.Error()
	}
	if rep.Source == "koinly" {
		v.Method = "Figures for 2025 are Koinly's, imported verbatim."
	} else {
		v.Method = "Cost basis method: FIFO, tracked per wallet (Rev. Proc. 2024-28). Opening basis on 01/01/2026 imported from Koinly's 2025 year-end holdings; older units are reported as VARIOUS (long-term)."
	}
	for _, in := range rep.Income {
		w := rs.WalletLabel(in.Wallet)
		v.Income = append(v.Income, templates.ReportIncomeRow{
			Date: in.Date.Format("01/02/2006"), Asset: in.Asset, Amount: templates.FormatQty(in.Amount),
			Value: report.Money8949(in.ValueUSD), Type: templates.IncomeTypeName(in.Type), Wallet: w,
		})
	}
	for _, w := range Snapshot(taxNow).Wallets {
		box := "I (short) / L (long)"
		if w.Type == "coinbase" || w.Type == "kraken" {
			box = "G or H (short) / J or K (long)"
		}
		v.Wallets = append(v.Wallets, templates.ReportWallet{Label: w.Label, Type: w.Type, Box: box})
	}
	if !rep.Closed {
		counts := map[string]int{}
		for _, i := range rep.Issues {
			counts[string(i.Kind)]++
		}
		for k, n := range counts {
			v.Issues = append(v.Issues, templates.ReportIssueCount{Kind: k, Count: n})
		}
		slices.SortFunc(v.Issues, func(a, b templates.ReportIssueCount) int { return strings.Compare(a.Kind, b.Kind) })
	}
	return v
}
