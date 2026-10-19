// Package demo builds realistic, internally consistent fixtures for the web UI: the
// sample portfolio from the Vault design boards. It backs cmd/uipreview and the template
// tests, so the UI can be seen and tested before any real data exists.
//
// Consistency rules the builders keep: every row's value is amount x price rounded to
// cents, a wallet's value is the sum of its rows, the snapshot total is the sum of the
// wallets, an asset's value is the sum of its rows, and asset shares sum to 1.
package demo

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/a-h/templ"
	"rsc.io/qr"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/web/templates"
)

// Source ids used by the fixtures.
const (
	IDCold     = "cold"
	IDCoinbase = "coinbase"
	IDKraken   = "kraken"
	IDPhantom  = "phantom"
	IDMom      = "mom-safe"
)

// Portfolio ids used by the fixtures (besides "all").
const (
	PFLongTerm = "long-term"
	PFTrading  = "trading"
	PFMajors   = "majors"
)

// portfolioMembers maps a portfolio id to its source ids. Long-term and Trading partition
// the five sources; Majors takes every source but only five assets (see portfolioAssets).
var portfolioMembers = map[string][]string{
	PFLongTerm: {IDCold, IDPhantom, IDMom},
	PFTrading:  {IDCoinbase, IDKraken},
	PFMajors:   {IDCold, IDCoinbase, IDKraken, IDPhantom, IDMom},
}

// portfolioAssets limits a portfolio to some asset keys (absent means every asset).
var portfolioAssets = map[string][]string{
	PFMajors: {"BTC", "ETH", "SOL", "LINK", "DOT"},
}

// change7d holds the fixture 7-day price changes in percent. DOT and BONK have none, so
// the 7d column shows its dash.
var change7d = map[string]float64{
	"BTC": 5.84, "ETH": -3.12, "SOL": 12.40, "USDC": 0.0, "LINK": 2.07, "JUP": -8.90,
}

var assetNames = map[string]string{
	"BTC": "Bitcoin", "ETH": "Ethereum", "SOL": "Solana", "USDC": "USD Coin",
	"DOT": "Polkadot", "LINK": "Chainlink", "JUP": "Jupiter", "BONK": "Bonk",
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func ptr[T any](v T) *T { return &v }

// Prices returns the fixture prices keyed by asset key. DOT is stale on purpose (it comes
// from the Kraken feed that is 3 hours behind).
func Prices(now time.Time) map[string]core.Price {
	fresh := now.Add(-2 * time.Minute)
	stale := now.Add(-3 * time.Hour)
	return map[string]core.Price{
		"BTC":  {USD: 64120.00, AsOf: fresh, Change24h: ptr(2.14)},
		"ETH":  {USD: 3180.40, AsOf: fresh, Change24h: ptr(-0.86)},
		"SOL":  {USD: 148.22, AsOf: fresh, Change24h: ptr(4.92)},
		"USDC": {USD: 1.00, AsOf: fresh, Change24h: ptr(0.0)},
		"DOT":  {USD: 6.41, AsOf: stale, Stale: true, Change24h: ptr(-2.31)},
		"LINK": {USD: 14.06, AsOf: fresh, Change24h: ptr(1.07)},
		"JUP":  {USD: 0.92, AsOf: fresh, Change24h: ptr(3.10)},
		"BONK": {USD: 0.0000214, AsOf: fresh, Change24h: ptr(-5.20)},
	}
}

func row(h core.Holding, prices map[string]core.Price) portfolio.HoldingRow {
	if h.Name == "" {
		h.Name = assetNames[h.AssetKey]
	}
	r := portfolio.HoldingRow{Holding: h}
	if p, ok := prices[h.AssetKey]; ok {
		pc := p
		r.Price, r.Priced = &pc, true
		r.ValueUSD = round2(h.Amount * p.USD)
	}
	return r
}

func hold(wallet, key string, amount float64) core.Holding {
	return core.Holding{WalletID: wallet, AssetKey: key, Symbol: key, Amount: amount}
}

func sum(rows []portfolio.HoldingRow) float64 {
	var t float64
	for _, r := range rows {
		t += r.ValueUSD
	}
	return round2(t)
}

// Wallets returns the fixture sources: Cold storage (Ledger, BTC), Coinbase, Kraken (stale
// 3 hours), Phantom (Solana, with a DeFi position, hidden tokens and NFTs) and a manual
// wallet.
func Wallets(now time.Time) []portfolio.WalletView {
	prices := Prices(now)
	fresh := now.Add(-2 * time.Minute)

	cold := portfolio.WalletView{
		ID: IDCold, Label: "Cold storage", Type: "bitcoin", Kind: core.KindHardware,
		Description: "Long-term savings on the Ledger. Never sold.",
		Status:      portfolio.StatusSynced, AsOf: fresh,
		Rows: []portfolio.HoldingRow{row(hold(IDCold, "BTC", 0.30006114), prices)},
		Meta: []core.KV{
			{K: "Imported via", V: "xpub / zpub"},
			{K: "Derivation", V: "m/84'/0'/0'"},
			{K: "Addresses used", V: "7 of 27"},
			{K: "Block height", V: "866,412"},
			{K: "Node", V: "Public · Esplora"},
		},
	}

	coinbase := portfolio.WalletView{
		ID: IDCoinbase, Label: "Coinbase", Type: "coinbase", Kind: core.KindExchange,
		Status: portfolio.StatusSynced, AsOf: fresh,
		Rows: []portfolio.HoldingRow{
			row(hold(IDCoinbase, "BTC", 0.08), prices),
			row(hold(IDCoinbase, "ETH", 3.25), prices),
			row(hold(IDCoinbase, "USDC", 3374.80), prices),
			row(hold(IDCoinbase, "LINK", 37.38), prices),
		},
		Meta: []core.KV{
			{K: "Access", V: "View-only API key"},
			{K: "Key", V: "organizations/…/apiKeys/••••3f9a"},
			{K: "Key status", V: "Valid"},
		},
	}

	kraken := portfolio.WalletView{
		ID: IDKraken, Label: "Kraken", Type: "kraken", Kind: core.KindExchange,
		Status: portfolio.StatusStale, AsOf: now.Add(-3 * time.Hour),
		Rows: []portfolio.HoldingRow{
			row(hold(IDKraken, "BTC", 0.04108), prices),
			row(hold(IDKraken, "SOL", 25), prices),
			row(hold(IDKraken, "DOT", 185.20), prices),
		},
		Warnings: []string{"Kraken last synced 3 hours ago. Values may be out of date."},
		Meta: []core.KV{
			{K: "Access", V: "Query Funds + Query Ledger"},
			{K: "Key", V: "••••b71c"},
		},
	}

	sol := hold(IDPhantom, "SOL", 14.041)
	staked := hold(IDPhantom, "SOL", 10)
	staked.Staked, staked.Note = true, "Native stake"
	usdc := hold(IDPhantom, "USDC", 480.25)
	jup := hold(IDPhantom, "JUP", 1250)
	bonk := hold(IDPhantom, "BONK", 12_500_000)
	lpSOL := hold(IDPhantom, "SOL", 2.5)
	lpUSDC := hold(IDPhantom, "USDC", 370.55)
	for _, h := range []*core.Holding{&lpSOL, &lpUSDC} {
		h.Protocol, h.Position, h.PositionKind = "Orca", "SOL/USDC whirlpool", "lp"
	}
	dust := func(sym string, amt float64) portfolio.HoldingRow {
		return portfolio.HoldingRow{Holding: core.Holding{
			WalletID: IDPhantom, AssetKey: core.SPLKey("Dust" + sym), Symbol: sym, Name: sym + " (unverified)", Amount: amt,
		}}
	}
	phantom := portfolio.WalletView{
		ID: IDPhantom, Label: "Phantom", Type: "solana", Kind: core.KindHot,
		Description: "Everyday DeFi: staking, swaps and Jupiter orders.",
		Status:      portfolio.StatusSynced, AsOf: fresh,
		Rows: []portfolio.HoldingRow{
			row(sol, prices), row(staked, prices), row(usdc, prices), row(jup, prices), row(bonk, prices),
			row(lpSOL, prices), row(lpUSDC, prices),
		},
		HiddenRows: []portfolio.HoldingRow{dust("AIRDR0P", 1_000_000), dust("FREE$SOL", 42), dust("CLAIMME", 7_777)},
		NFTCount:   12,
		Meta: []core.KV{
			{K: "Address", V: "7xKXq…Wq3v"},
			{K: "Node", V: "Helius RPC"},
			{K: "Tokens", V: "4 priced · 3 hidden"},
		},
	}

	mom := portfolio.WalletView{
		ID: IDMom, Label: "Paper wallet at mom's", Type: "manual", Kind: core.KindManual,
		Status: portfolio.StatusSynced, AsOf: fresh,
		Rows: []portfolio.HoldingRow{
			withNote(row(hold(IDMom, "BTC", 0.05), prices), "owed by Sam"),
			withNote(row(hold(IDMom, "ETH", 0.5), prices), "cold backup"),
		},
		Meta: []core.KV{{K: "Entered", V: "By hand"}},
	}

	ws := []portfolio.WalletView{cold, coinbase, kraken, phantom, mom}
	for i := range ws {
		ws[i].ValueUSD = sum(ws[i].Rows)
	}
	return ws
}

func withNote(r portfolio.HoldingRow, note string) portfolio.HoldingRow {
	r.Note = note
	return r
}

// merge builds the merged asset rows for a set of wallets, sorted by value, with shares.
func merge(ws []portfolio.WalletView, prices map[string]core.Price) ([]portfolio.AssetRow, float64) {
	by := map[string]*portfolio.AssetRow{}
	var order []string
	for _, w := range ws {
		for _, r := range w.Rows {
			a := by[r.AssetKey]
			if a == nil {
				a = &portfolio.AssetRow{AssetKey: r.AssetKey, Symbol: r.Symbol, Name: r.Name, Price: r.Price, Priced: r.Priced}
				by[r.AssetKey] = a
				order = append(order, r.AssetKey)
			}
			a.Amount += r.Amount
			a.ValueUSD = round2(a.ValueUSD + r.ValueUSD)
			if r.Staked {
				a.StakedAmt += r.Amount
			}
			if !contains(a.HeldIn, w.Label) {
				a.HeldIn = append(a.HeldIn, w.Label)
			}
		}
	}
	var total float64
	rows := make([]portfolio.AssetRow, 0, len(order))
	for _, k := range order {
		if v, ok := change7d[k]; ok {
			by[k].Change7d = ptr(v)
		}
		total += by[k].ValueUSD
		rows = append(rows, *by[k])
	}
	total = round2(total)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ValueUSD > rows[j].ValueUSD })
	for i := range rows {
		if total > 0 {
			rows[i].Share = rows[i].ValueUSD / total
		}
	}
	return rows, total
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Snapshot returns the full fixture snapshot (all five sources).
func Snapshot(now time.Time) portfolio.Snapshot {
	return snapshotOf(Wallets(now), now)
}

func snapshotOf(ws []portfolio.WalletView, now time.Time) portfolio.Snapshot {
	prices := Prices(now)
	assets, total := merge(ws, prices)
	hasStale := false
	for _, w := range ws {
		if w.Status == portfolio.StatusStale {
			hasStale = true
		}
	}
	return portfolio.Snapshot{
		UpdatedAt: now.Add(-2 * time.Minute),
		TotalUSD:  total,
		HasStale:  hasStale,
		Assets:    assets,
		Wallets:   ws,
		Prices:    prices,
	}
}

// SnapshotFor returns the snapshot filtered to a portfolio ("all" or a portfolio id). The
// second result is false for an unknown portfolio.
func SnapshotFor(pf string, now time.Time) (portfolio.Snapshot, bool) {
	if pf == "" || pf == "all" {
		return Snapshot(now), true
	}
	ids, ok := portfolioMembers[pf]
	if !ok {
		return portfolio.Snapshot{}, false
	}
	keys := portfolioAssets[pf]
	var ws []portfolio.WalletView
	for _, w := range Wallets(now) {
		if !contains(ids, w.ID) {
			continue
		}
		if keys != nil {
			var rows []portfolio.HoldingRow
			for _, r := range w.Rows {
				if contains(keys, r.AssetKey) {
					rows = append(rows, r)
				}
			}
			if len(rows) == 0 {
				continue
			}
			w.Rows, w.HiddenRows, w.NFTCount = rows, nil, 0
			w.ValueUSD = sum(rows)
		}
		ws = append(ws, w)
	}
	return snapshotOf(ws, now), true
}

// ranges holds the board's sample series (in thousands), oldest to newest.
var ranges = map[string]struct {
	series []float64
	span   time.Duration
	label  string
}{
	"1D":  {[]float64{47.6, 47.9, 47.7, 48.0, 47.8, 48.1, 48.3, 48.0, 48.2, 48.4, 48.1, 48.21}, 24 * time.Hour, "past 24 hours"},
	"1W":  {[]float64{46.9, 47.2, 46.6, 46.8, 47.5, 47.3, 47.9, 47.6, 48.4, 48.0, 48.5, 48.21}, 7 * 24 * time.Hour, "past week"},
	"1M":  {[]float64{44.2, 45.1, 44.6, 43.8, 45.0, 46.2, 45.7, 46.9, 47.4, 46.8, 47.9, 48.21}, 30 * 24 * time.Hour, "past month"},
	"YTD": {[]float64{36.4, 35.1, 38.2, 37.5, 40.8, 39.9, 42.6, 44.1, 43.3, 46.0, 47.2, 48.21}, 0, "this year"},
	"1Y":  {[]float64{29.4, 31.2, 30.1, 33.8, 36.0, 34.2, 38.5, 41.0, 39.6, 43.2, 46.1, 48.21}, 365 * 24 * time.Hour, "past year"},
	"All": {[]float64{6.2, 9.8, 14.5, 12.1, 18.9, 24.0, 21.4, 29.6, 33.0, 38.7, 43.9, 48.21}, 730 * 24 * time.Hour, "since first deposit"},
}

// Chart returns the chart series for a range ("1D","1W","1M","YTD","1Y","All"; anything
// else is 1W), scaled so the last point equals the snapshot total. YTD spans the year so
// far (at least a day, so January 1st still draws something).
func Chart(rng string, now time.Time) []portfolio.Point {
	r, ok := ranges[rng]
	if !ok {
		r = ranges["1W"]
	}
	if rng == "YTD" {
		r.span = max(now.Sub(portfolio.YearStart(now)), 24*time.Hour)
	}
	total := Snapshot(now).TotalUSD
	last := r.series[len(r.series)-1]
	pts := make([]portfolio.Point, len(r.series))
	step := r.span / time.Duration(len(r.series)-1)
	for i, v := range r.series {
		pts[i] = portfolio.Point{
			T: now.Add(-r.span + time.Duration(i)*step),
			V: round2(v / last * total),
		}
	}
	pts[len(pts)-1].V = total
	return pts
}

// Change returns the change over a range, computed from Chart.
func Change(rng string, now time.Time) portfolio.Change {
	r, ok := ranges[rng]
	if !ok {
		r = ranges["1W"]
	}
	pts := Chart(rng, now)
	first, last := pts[0].V, pts[len(pts)-1].V
	c := portfolio.Change{Abs: round2(last - first), Valid: true, Label: r.label}
	if first != 0 {
		c.Pct = (last - first) / first * 100
	}
	return c
}

// Shell builds the app chrome for a page.
func Shell(title, active, pf, rng string) templates.Shell {
	if pf == "" {
		pf = "all"
	}
	if rng == "" {
		rng = "1W"
	}
	return templates.Shell{
		Title: title, Active: active, PF: pf, Range: rng,
		Portfolios: []templates.PortfolioNav{
			{ID: "all", Name: "All holdings", Count: "5 sources"},
			{ID: PFLongTerm, Name: "Long-term", Count: "3 sources"},
			{ID: PFTrading, Name: "Trading", Count: "2 sources"},
			{ID: PFMajors, Name: "Majors", Count: "5 sources"},
		},
	}
}

var portfolioNames = map[string]string{"all": "All holdings", PFLongTerm: "Long-term", PFTrading: "Trading", PFMajors: "Majors"}

// WatchList is the fixture "Watching" table.
func WatchList(now time.Time) []templates.WatchRow {
	fresh := now.Add(-2 * time.Minute)
	return []templates.WatchRow{
		{AssetKey: "DOGE", Symbol: "DOGE", Name: "Dogecoin", Price: &core.Price{USD: 0.1342, AsOf: fresh, Change24h: ptr(1.20)}},
		{AssetKey: "AVAX", Symbol: "AVAX", Name: "Avalanche", Price: &core.Price{USD: 38.76, AsOf: fresh, Change24h: ptr(-3.41)}},
		{AssetKey: "ARB", Symbol: "ARB", Name: "Arbitrum", Price: &core.Price{USD: 0.81, AsOf: now.Add(-3 * time.Hour), Stale: true, Change24h: ptr(0.0)}},
	}
}

// Overview returns the overview page for all holdings on the 1W range.
func Overview(now time.Time) templates.OverviewPage {
	p, _ := OverviewFor("all", "1W", now)
	return p
}

// OverviewFor returns the overview page for a portfolio and range; false for an unknown
// portfolio.
func OverviewFor(pf, rng string, now time.Time) (templates.OverviewPage, bool) {
	if pf == "" {
		pf = "all"
	}
	if _, ok := ranges[rng]; !ok {
		rng = "1W"
	}
	snap, ok := SnapshotFor(pf, now)
	if !ok {
		return templates.OverviewPage{}, false
	}
	p := templates.OverviewPage{
		Shell:         Shell("Overview", "overview", pf, rng),
		PortfolioName: portfolioNames[pf],
		Snap:          snap,
		Chart:         Chart(rng, now),
		Change:        Change(rng, now),
		Sync:          SyncSynced(),
		Watch:         WatchList(now),
		PnL:           PnLCells(now),
		Now:           now,
	}
	if pf != "all" {
		if total := Snapshot(now).TotalUSD; total > 0 {
			p.NetWorthShare = snap.TotalUSD / total
		}
		// Scale the series to the filtered total so the chart ends where the number does.
		scale := snap.TotalUSD / Snapshot(now).TotalUSD
		for i := range p.Chart {
			p.Chart[i].V = round2(p.Chart[i].V * scale)
		}
		p.Change = scaleChange(p.Change, scale)
	}
	return p, true
}

func scaleChange(c portfolio.Change, k float64) portfolio.Change {
	c.Abs = round2(c.Abs * k)
	return c
}

// SyncSynced is the happy-path sync chip.
func SyncSynced() templates.SyncInfo {
	return templates.SyncInfo{Status: portfolio.StatusSynced, Label: "Synced 2m ago"}
}

// OverviewEmpty is the overview with no sources at all.
func OverviewEmpty(now time.Time) templates.OverviewPage {
	return templates.OverviewPage{
		Shell:         Shell("Overview", "overview", "all", "1W"),
		PortfolioName: "All holdings",
		Snap:          portfolio.Snapshot{UpdatedAt: now, Prices: map[string]core.Price{}},
		Change:        portfolio.Change{Label: "past week"},
		Sync:          templates.SyncInfo{Status: portfolio.StatusPending, Label: "Syncing…"},
		Now:           now,
	}
}

// WalletsPage returns the wallets list page.
func WalletsPage(now time.Time) templates.WalletsPage {
	return templates.WalletsPage{
		Shell:   Shell("Wallets", "wallets", "all", ""),
		Wallets: Wallets(now),
		Sync:    SyncSynced(),
		Now:     now,
	}
}

// ColdAddresses are sample derived addresses for the Bitcoin Addresses tab.
func ColdAddresses() []core.Address {
	return []core.Address{
		{Address: "bc1q8f3k7z2m9w5x6p4n8d0r3t7y2u6h4j9a7x4", Path: "0/6", TxCount: 2, Balance: 0.12500000},
		{Address: "bc1qp7w4c9n2xk5v8m3d6h0s1y4t7r2e9u3l5a8z0", Path: "0/5", TxCount: 1, Balance: 0.08000000},
		{Address: "bc1qzr3m8x5k2n7v0c4w9d6h1s4y7t2e5u8l3a6p9", Path: "0/4", TxCount: 1, Balance: 0.10006114},
		{Address: "bc1qh4d9s2y6t0e3u7l1a5p8z4r7m2x9k3n6v0c5w", Path: "1/2", Change: true, TxCount: 2, Balance: 0.00000000},
		{Address: "bc1qy2t6e0u4l8a1p5z9r3m7x2k6n0v4c8w1d5h9s", Path: "0/3", TxCount: 1, Balance: 0.00000000},
		{Address: "bc1qe5u9l3a7p1z5r9m3x7k1n5v9c3w7d1h5s9y3t", Path: "1/1", Change: true, TxCount: 1, Balance: 0.00000000},
		{Address: "bc1ql8a2p6z0r4m8x2k6n0v4c8w2d6h0s4y8t2e6u", Path: "0/7", TxCount: 0, Balance: 0},
		{Address: "bc1qa1p5z9r3m7x1k5n9v3c7w1d5h9s3y7t1e5u9l", Path: "0/8", TxCount: 0, Balance: 0},
	}
}

// ColdUTXOs are sample unspent outputs for the Bitcoin UTXOs tab.
func ColdUTXOs() []core.UTXO {
	return []core.UTXO{
		{TxID: "3f9ac21e5e02ab7f0c4d77e2e91a4b50a77cd913b0f4a81bd0f4a3f9a2c1e5d6", Vout: 0, Address: "bc1q8f3k7z2m9w5x6p4n8d0r3t7y2u6h4j9a7x4", Value: 0.05000000, Confirmed: false},
		{TxID: "81bd0f4a3f9a2c1e5d65e02ab7f0c4d77e2e91a4b50a77cd913b0f4a3f9ac21e", Vout: 1, Address: "bc1qp7w4c9n2xk5v8m3d6h0s1y4t7r2e9u3l5a8z0", Value: 0.08000000, Confirmed: true, Height: 866_201},
		{TxID: "5e02ab7f0c4d77e2e91a4b50a77cd913b0f4a81bd0f4a3f9a2c1e5d63f9ac21e", Vout: 0, Address: "bc1qzr3m8x5k2n7v0c4w9d6h1s4y7t2e5u8l3a6p9", Value: 0.10006114, Confirmed: true, Height: 861_977},
		{TxID: "0c4d77e2e91a4b50a77cd913b0f4a81bd0f4a3f9a2c1e5d65e02ab7f3f9ac21e", Vout: 2, Address: "bc1qh4d9s2y6t0e3u7l1a5p8z4r7m2x9k3n6v0c5w", Value: 0.06999999, Confirmed: true, Height: 850_412},
	}
}

// ReceiveAddress is the fixture next unused receive address of the cold wallet.
const ReceiveAddress = "bc1q8f3k7z2m9w5x6p4n8d0r3t7y2u6h4j9a7x4"

// QR renders text as an SVG QR code component (dark modules on a light quiet zone, as in
// the Wallet board). The real server builds its own; this one backs the preview.
func QR(text string) templ.Component {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil
	}
	n := code.Size
	const quiet = 2
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="QR code for the receive address" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#ECEEF1"/><path fill="#0D0F12" d="`, n+2*quiet, n+2*quiet)
	for y := range n {
		for x := range n {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return templ.Raw(b.String())
}

// Wallet returns the wallet detail page for a source id and tab; false for an unknown id.
// Tabs that the wallet does not have fall back to "holdings".
func Wallet(id, tab string, now time.Time) (templates.WalletPage, bool) {
	var w portfolio.WalletView
	found := false
	for _, x := range Wallets(now) {
		if x.ID == id {
			w, found = x, true
		}
	}
	if !found {
		return templates.WalletPage{}, false
	}
	p := templates.WalletPage{
		Shell:     Shell(w.Label, "wallets", "all", ""),
		W:         w,
		Tab:       tab,
		IsBitcoin: w.Type == "bitcoin",
		Now:       now,
	}
	if tab != "activity" && (!p.IsBitcoin || (tab != "addresses" && tab != "utxos")) {
		p.Tab = "holdings"
	}
	if id == IDCold {
		p.Addresses = ColdAddresses()
		p.UTXOs = ColdUTXOs()
		p.Receive = ReceiveAddress
		p.ReceiveQR = QR("bitcoin:" + ReceiveAddress)
		// The cold wallet shows the filled cost basis state (every unit has a known cost);
		// every other wallet shows the empty one.
		p.CostBasis = ptr(11402.50)
		p.Unrealized = ptr(round2(w.ValueUSD - 11402.50))
		for _, r := range w.Rows {
			p.CostUnits += r.Amount
		}
	}
	walletActivity(&p, now)
	return p, true
}

// SourcePick returns step 1 of the add-source flow.
func SourcePick() templates.SourcePickPage {
	return templates.SourcePickPage{Shell: Shell("Add a source", "wallets", "all", "")}
}

// SourceForm returns the empty add form for a source type.
func SourceForm(typ string) templates.SourceFormPage {
	kind := "hardware"
	if typ == "solana" {
		kind = "hot"
	}
	return templates.SourceFormPage{
		Shell:  Shell("Add a source", "wallets", "all", ""),
		Type:   typ,
		Values: map[string]string{"kind": kind},
		Errors: map[string]string{},
	}
}

// SourceFormEdit returns the edit form for a source type, filled with non-secret values
// and the masked secrets of an existing source.
func SourceFormEdit(typ string) templates.SourceFormPage {
	p := SourceForm(typ)
	p.Shell.Title = "Edit source"
	switch typ {
	case "coinbase":
		p.EditID = IDCoinbase
		p.Values = map[string]string{"label": "Coinbase", "keyName": "organizations/9c1e…/apiKeys/3f9a"}
		p.SecretMasks = map[string]string{"privateKey": "••••3f9a"}
	case "kraken":
		p.EditID = IDKraken
		p.Values = map[string]string{"label": "Kraken"}
		p.SecretMasks = map[string]string{"apiKey": "••••b71c", "apiSecret": "••••9d02"}
	case "bitcoin":
		p.EditID = IDCold
		p.Values = map[string]string{"label": "Cold storage", "kind": "hardware", "key": "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"}
	case "solana":
		p.EditID = IDPhantom
		p.Values = map[string]string{"label": "Phantom", "kind": "hot", "address": "7xKXqYqHn1eGk4kVj3b8f2xZc9mPd5sLr6tNwQ3vJ8aB"}
	default:
		p.EditID = IDMom
		p.Values = map[string]string{"label": "Paper wallet at mom's", "holdings": "BTC 0.05 owed by Sam\nETH 0.5 cold backup"}
	}
	return p
}

// SourceFormInvalid returns an add form showing validation errors.
func SourceFormInvalid(typ string) templates.SourceFormPage {
	p := SourceForm(typ)
	p.Values = map[string]string{"label": ""}
	p.Errors = map[string]string{"label": "Give it a name, e.g. \"Cold storage\"."}
	switch typ {
	case "coinbase":
		p.Errors["privateKey"] = "Paste the private key."
	case "kraken":
		p.Errors["apiKey"] = "Paste the API key."
		p.Errors["apiSecret"] = "Paste the private key (secret)."
	case "bitcoin":
		p.Errors["key"] = "Paste an extended public key (xpub/zpub), an output descriptor or an address, or upload a wallet file."
	case "solana":
		p.Errors["address"] = "Paste the wallet's public address."
	default:
		p.Errors["holdings"] = "Line 2: \"abc\" is not an amount."
	}
	p.Errors[""] = "Nothing was saved. Fix the fields below and try again."
	return p
}

// Settings returns the settings page.
func Settings(now time.Time) templates.SettingsPage {
	return templates.SettingsPage{
		Shell:       Shell("Settings", "settings", "all", ""),
		Wallets:     Wallets(now),
		DataDir:     "/data",
		AuthEnabled: true,
		Version:     "v0.1.0-dev",
		Sync:        SyncRows(now),
		Now:         now,
	}
}

// TestOK is a successful connection test for the Coinbase fixture.
func TestOK(now time.Time) templates.TestResult {
	rows := Wallets(now)[1].Rows
	return templates.TestResult{OK: true, Message: fmt.Sprintf("Connected. Found %d balances.", len(rows)), Rows: rows}
}

// TestFailed is a failed connection test.
func TestFailed() templates.TestResult {
	return templates.TestResult{OK: false, Message: "Coinbase rejected the key (401). Check the key name and that the private key is the matching one."}
}

// ---- portfolios manager and editor ---------------------------------------------------------

// assetsText is "All assets", or the first three symbols and then "+N".
func assetsText(syms []string) string {
	if len(syms) == 0 {
		return "All assets"
	}
	if len(syms) <= 3 {
		return strings.Join(syms, ", ")
	}
	return fmt.Sprintf("%s +%d", strings.Join(syms[:3], ", "), len(syms)-3)
}

// Portfolios returns the portfolio manager page: Long-term and Trading (which partition
// the sources) and Majors (every source, five assets), plus four backups.
func Portfolios(now time.Time) templates.PortfoliosPage {
	net := Snapshot(now).TotalUSD
	item := func(id, name, wallets string, syms []string, watch int) templates.PortfolioItem {
		snap, _ := SnapshotFor(id, now)
		stable, hasValue := snap.StableShare()
		return templates.PortfolioItem{
			ID: id, Name: name, ValueUSD: snap.TotalUSD, Share: snap.TotalUSD / net,
			StableShare: stable, HasValue: hasValue,
			WalletsText: wallets, AssetsText: assetsText(syms), WatchCount: watch,
		}
	}
	return templates.PortfoliosPage{
		Shell: Shell("Portfolios", "portfolios", "all", ""),
		Items: []templates.PortfolioItem{
			item(PFLongTerm, "Long-term", "3 wallets", nil, 2),
			item(PFTrading, "Trading", "2 wallets", nil, 0),
			item(PFMajors, "Majors", "All wallets", portfolioAssets[PFMajors], 3),
		},
		Backups: []templates.BackupRow{
			{Name: "20261016-114205.yaml", Time: now.Add(-25 * time.Minute), Size: 1284},
			{Name: "20261016-084511.yaml", Time: now.Add(-3 * time.Hour), Size: 1190},
			{Name: "20261015-100230.yaml", Time: now.Add(-26 * time.Hour), Size: 980},
			{Name: "20261007-190944.yaml", Time: now.Add(-9 * 24 * time.Hour), Size: 612},
		},
		Now: now,
	}
}

// PortfolioForm returns the create form (editID "") or the edit form of a fixture
// portfolio. An unknown id gives the create form.
func PortfolioForm(editID string, now time.Time) templates.PortfolioFormPage {
	p := templates.PortfolioFormPage{
		Shell:           Shell("New portfolio", "portfolios", "all", ""),
		AllWallets:      true,
		AllAssets:       true,
		SelectedWallets: map[string]bool{},
		SelectedAssets:  map[string]bool{},
		Errors:          map[string]string{},
	}
	for _, w := range Wallets(now) {
		p.WalletOptions = append(p.WalletOptions, templates.WalletOption{ID: w.ID, Label: w.Label, Type: w.Type, Kind: w.Kind})
	}
	for _, a := range Snapshot(now).Assets {
		name := a.Name
		if name == "" {
			name = a.Symbol
		}
		p.AssetOptions = append(p.AssetOptions, templates.AssetOption{Key: a.AssetKey, Symbol: a.Symbol, Name: name, ValueUSD: a.ValueUSD})
	}
	switch editID {
	case PFLongTerm:
		p.EditID, p.Name = PFLongTerm, "Long-term"
		p.AllWallets = false
		for _, id := range portfolioMembers[PFLongTerm] {
			p.SelectedWallets[id] = true
		}
		p.Watch = "ETH LINK"
	case PFTrading:
		p.EditID, p.Name = PFTrading, "Trading"
		p.AllWallets = false
		for _, id := range portfolioMembers[PFTrading] {
			p.SelectedWallets[id] = true
		}
	case PFMajors:
		p.EditID, p.Name = PFMajors, "Majors"
		p.AllAssets = false
		for _, k := range portfolioAssets[PFMajors] {
			p.SelectedAssets[k] = true
		}
		p.ExtraAssets = "AVAX spl:J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn"
		p.Watch = "DOGE ARB"
	}
	if p.EditID != "" {
		p.Shell.Title = "Edit portfolio"
	}
	return p
}

// PortfolioFormInvalid is the create form after a failed save: no name, "Only selected
// wallets" with nothing ticked, and the errors that go with them.
func PortfolioFormInvalid(now time.Time) templates.PortfolioFormPage {
	p := PortfolioForm("", now)
	p.AllWallets = false
	p.Errors = map[string]string{
		"":        "Nothing was saved. Fix the fields below and try again.",
		"name":    "Give the portfolio a name.",
		"wallets": "Pick at least one wallet, or choose All wallets.",
	}
	return p
}
