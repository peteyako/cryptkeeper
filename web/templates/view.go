package templates

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
	"cryptkeeper/web/static"
)

// This file holds the small display decisions the templates share, kept in Go so they
// can be unit tested.

// Theme colors for <meta name="theme-color">. They mirror --bg in tokens.css (a test
// keeps them in sync); a meta tag cannot reference a CSS variable.
const (
	ThemeColorDark  = "#0D0F12"
	ThemeColorLight = "#F6F7F9"
)

func typeName(t string) string {
	switch t {
	case "coinbase":
		return "Coinbase"
	case "kraken":
		return "Kraken"
	case "bitcoin":
		return "Bitcoin"
	case "solana":
		return "Solana"
	case "manual":
		return "Manual"
	case "brokerage":
		return "Brokerage"
	}
	if t == "" {
		return ""
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

// NativeSymbol is the chain's own coin for a source type ("BTC" for bitcoin), or "".
func NativeSymbol(sourceType string) string {
	switch sourceType {
	case "bitcoin":
		return "BTC"
	case "solana":
		return "SOL"
	}
	return ""
}

// KindLabel is the small caption under an account name: "Exchange · API",
// "Hardware · Bitcoin", "Hot wallet · Solana", "Manual · holdings".
func KindLabel(w portfolio.WalletView) string {
	switch w.Kind {
	case core.KindExchange:
		return "Exchange · API"
	case core.KindHardware:
		return "Hardware · " + typeName(w.Type)
	case core.KindHot:
		return "Hot wallet · " + typeName(w.Type)
	case core.KindManual:
		return "Manual · holdings"
	case core.KindBrokerage:
		return "Brokerage · stocks & funds"
	}
	return typeName(w.Type)
}

// SourceChipText is the text of the source chip in the wallet header:
// "Bitcoin · hardware", "Coinbase · exchange", "Solana · hot wallet", "Manual · holdings".
func SourceChipText(w portfolio.WalletView) string {
	switch w.Kind {
	case core.KindExchange:
		return typeName(w.Type) + " · exchange"
	case core.KindHardware:
		return typeName(w.Type) + " · hardware"
	case core.KindHot:
		return typeName(w.Type) + " · hot wallet"
	case core.KindManual:
		return "Manual · holdings"
	case core.KindBrokerage:
		return "Brokerage · stocks & funds"
	}
	return typeName(w.Type)
}

// AccessLabel says how CryptKeeper sees the source: "Read-only" for exchanges (a view-only API
// key), "Watch-only" for wallets, "Hand-entered" for manual holdings.
func AccessLabel(w portfolio.WalletView) string {
	switch w.Kind {
	case core.KindExchange:
		return "Read-only"
	case core.KindManual, core.KindBrokerage:
		return "Hand-entered"
	}
	return "Watch-only"
}

// WalletMeta is the footer line of an account card: "1 asset · watch-only".
func WalletMeta(w portfolio.WalletView) string {
	n := Plural(len(w.Rows), "asset")
	switch w.Kind {
	case core.KindExchange:
		return n + " · read-only key"
	case core.KindManual:
		return n + " · entered by hand"
	case core.KindBrokerage:
		return Plural(len(w.Rows), "holding") + " · market prices"
	}
	return n + " · watch-only"
}

// SourceTone is the color family of a source kind: "exchange", "hot", "hardware", "manual",
// "brokerage".
func SourceTone(k core.Kind) string {
	switch k {
	case core.KindExchange, core.KindHot, core.KindHardware, core.KindManual, core.KindBrokerage:
		return string(k)
	}
	return "manual"
}

// SourceIcon is the icon name for a source kind tile.
func SourceIcon(k core.Kind) string {
	switch k {
	case core.KindHardware:
		return "device"
	case core.KindExchange:
		return "exchange"
	case core.KindManual:
		return "pencil"
	case core.KindBrokerage:
		return "reports"
	}
	return "wallet"
}

// StatusView describes a wallet's sync state as a chip: a glyph (so state never relies on
// color alone), a label and a tone ("gain", "warn", "loss", "neutral").
func StatusView(w portfolio.WalletView, now time.Time) (glyph, label, tone string) {
	switch w.Status {
	case portfolio.StatusSynced:
		return "●", "Synced", "gain"
	case portfolio.StatusStale:
		label = "Stale"
		if s := Since(w.AsOf, now); s != "" {
			label += " " + s
		}
		return "◐", label, "warn"
	case portfolio.StatusError:
		return "✕", "Error", "loss"
	}
	return "◌", "Syncing…", "neutral"
}

// SyncView is StatusView for the header sync chip.
// chipTitle is a sync chip's tooltip: how often it refreshes, and that a click refreshes now.
func chipTitle(s SyncInfo, action string) string {
	if s.Title == "" {
		return action + " now"
	}
	return s.Title + ". " + action + " now."
}

func SyncView(s SyncInfo) (glyph, label, tone string) {
	label = s.Label
	switch s.Status {
	case portfolio.StatusSynced:
		glyph, tone = "●", "gain"
		if label == "" {
			label = "Synced"
		}
	case portfolio.StatusStale:
		glyph, tone = "◐", "warn"
		if label == "" {
			label = "Stale"
		}
	case portfolio.StatusError:
		glyph, tone = "✕", "loss"
		if label == "" {
			label = "Error"
		}
	default:
		glyph, tone = "◌", "neutral"
		if label == "" {
			label = "Syncing…"
		}
	}
	return glyph, label, tone
}

// pct24h is a price's 24h change in percent, or nil when it has none.
func pct24h(p *core.Price) *float64 {
	if p == nil {
		return nil
	}
	return p.Change24h
}

// SearchKey is the lower-case string the holdings search box matches against.
func SearchKey(a portfolio.AssetRow) string {
	parts := []string{a.Symbol, a.Name}
	parts = append(parts, a.HeldIn...)
	return strings.ToLower(strings.Join(parts, " "))
}

// HoldingName is the display name of a holding: its name, else its symbol.
func HoldingName(h core.Holding) string {
	if h.Name != "" {
		return h.Name
	}
	return h.Symbol
}

// AssetName is the display name of a merged asset: its name, else its symbol.
func AssetName(a portfolio.AssetRow) string {
	if a.Name != "" {
		return a.Name
	}
	return a.Symbol
}

// HoldingGroup is a DeFi position and its legs.
type HoldingGroup struct {
	Protocol string
	Position string
	Kind     string // "lp", "perp", "locked" or ""
	Rows     []portfolio.HoldingRow
	ValueUSD float64
}

// SplitPositions separates plain balances from DeFi positions. Positions are grouped by
// protocol and position name in order of first appearance.
func SplitPositions(rows []portfolio.HoldingRow) (plain []portfolio.HoldingRow, groups []HoldingGroup) {
	idx := map[string]int{}
	for _, r := range rows {
		if r.Position == "" {
			plain = append(plain, r)
			continue
		}
		key := r.Protocol + "\x00" + r.Position
		i, ok := idx[key]
		if !ok {
			i = len(groups)
			idx[key] = i
			groups = append(groups, HoldingGroup{Protocol: r.Protocol, Position: r.Position, Kind: r.PositionKind})
		}
		groups[i].Rows = append(groups[i].Rows, r)
		groups[i].ValueUSD += r.ValueUSD
	}
	return plain, groups
}

// PositionKindLabel is the badge text for a DeFi position kind.
func PositionKindLabel(kind string) string {
	switch kind {
	case "lp":
		return "Liquidity pool"
	case "perp":
		return "Perpetuals"
	case "locked":
		return "Locked"
	case "limit":
		return "Limit orders"
	}
	return ""
}

// SingleAsset returns the only holding of a single-asset wallet (for the native amount
// under the balance).
func SingleAsset(w portfolio.WalletView) (portfolio.HoldingRow, bool) {
	if len(w.Rows) != 1 {
		return portfolio.HoldingRow{}, false
	}
	return w.Rows[0], true
}

// UnrealizedPct is the unrealized P/L as a percent of cost basis, or ok=false when it
// cannot be computed.
func UnrealizedPct(pl, cost *float64) (pct float64, ok bool) {
	if pl == nil || cost == nil || *cost <= 0 {
		return 0, false
	}
	return *pl / *cost * 100, true
}

// PortfolioNameFor finds the display name of the current portfolio in the shell.
func (s Shell) PortfolioNameFor() string {
	pf := normPF(s.PF)
	for _, p := range s.Portfolios {
		if p.ID == pf {
			return p.Name
		}
	}
	if pf == "all" {
		return "All holdings"
	}
	return pf
}

// BackLabel is the text of the "back to portfolio" link on the wallet page.
func (s Shell) BackLabel() string {
	if normPF(s.PF) == "all" {
		return "Overview"
	}
	return s.PortfolioNameFor() + " portfolio"
}

// FieldLabel is the visible label for a source form field name, used in the error summary.
func FieldLabel(name string) string {
	switch name {
	case "label":
		return "Label"
	case "description":
		return "Description"
	case "keyName":
		return "API key name"
	case "privateKey":
		return "Private key"
	case "apiKey":
		return "API key"
	case "apiSecret":
		return "API secret"
	case "key":
		return "Public key"
	case "kind":
		return "Wallet type"
	case "scriptType":
		return "Script type"
	case "walletFile":
		return "Wallet file"
	case "address":
		return "Address"
	case "holdings":
		return "Holdings"
	case "name":
		return "Name"
	case "wallets":
		return "Wallets"
	case "assets":
		return "Assets"
	case "extraAssets":
		return "Extra assets"
	case "watch":
		return "Watch list"
	case "type":
		return "Alert type"
	case "minPct":
		return "Minimum drop"
	case "asset":
		return "Asset"
	case "amount":
		return "Amount"
	case "cost":
		return "Total cost"
	case "above":
		return "Rises above"
	case "below":
		return "Falls below"
	case "portfolio":
		return "Portfolio"
	case "pct":
		return "Move"
	case "window":
		return "Window"
	case "minutes":
		return "Minutes"
	case "minUsd":
		return "Minimum value"
	case "cooldown":
		return "Wait before telling you again"
	}
	return name
}

func hasHidden(w portfolio.WalletView) bool { return len(w.HiddenRows) > 0 || w.NFTCount > 0 }

func hiddenSummary(w portfolio.WalletView) string {
	var parts []string
	if n := len(w.HiddenRows); n > 0 {
		parts = append(parts, Plural(n, "hidden token"))
	}
	if w.NFTCount > 0 {
		parts = append(parts, Plural(w.NFTCount, "NFT"))
	}
	return strings.Join(parts, " · ")
}

func shortTx(txid string) string {
	if len(txid) <= 12 {
		return txid
	}
	return txid[:4] + "…" + txid[len(txid)-4:]
}

func uintStr(v uint32) string { return strconv.FormatUint(uint64(v), 10) }

func sortStrings(s []string) { sort.Strings(s) }

// Comma groups an integer with thousands separators: 866412 -> "866,412".
func Comma(v int64) string {
	if v < 0 {
		return Minus + group(strconv.FormatInt(-v, 10))
	}
	return group(strconv.FormatInt(v, 10))
}

// SafeLogoURL returns a token logo URL only when it is a plain https URL; anything else
// (empty, http, data:, javascript:) is dropped and the monogram is used instead.
func SafeLogoURL(u string) string {
	if len(u) > 8 && strings.EqualFold(u[:8], "https://") && !strings.ContainsAny(u, " \t\r\n\"'<>") {
		return u
	}
	return ""
}

// maxHiddenRows caps the collapsed "hidden tokens" list: a wallet full of spam airdrops can
// hold thousands, and every row would ride along on each live patch.
const maxHiddenRows = 50

func firstN[T any](xs []T, n int) []T { return xs[:min(n, len(xs))] }

// asset returns a cache-busted URL for an embedded static file.
func asset(name string) string { return "/static/" + name + "?v=" + static.Version }

// ---- portfolios ---------------------------------------------------------------------------

// ShareText is a portfolio's share of net worth: "32% of net worth", "<1% of net worth".
func ShareText(share float64) string {
	switch {
	case !finite(share) || share <= 0:
		return "0% of net worth"
	case share < 0.005:
		return "<1% of net worth"
	}
	return strconv.Itoa(int(min(share, 1)*100+0.5)) + "% of net worth"
}

// ByteSize is a file size for people: "812 B", "1.2 KB", "3.4 MB".
func ByteSize(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(max(n, 0), 10) + " B"
	case n < 1024*1024:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
	}
	return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MB"
}

// BackupWhen is the time of a backup: "Oct 16, 11:42".
func BackupWhen(t time.Time) string { return Local(t).Format("Jan 2, 15:04") }

// Location is the time zone dates and clock times are shown in: the server's local zone
// (set TZ, e.g. TZ=America/Los_Angeles, in Docker). Tests pin it to UTC.
var Location = time.Local

// Local converts t to the display time zone.
func Local(t time.Time) time.Time { return t.In(Location) }

// missingText says which wallets a portfolio still lists but no longer exist.
func missingText(ids []string) string {
	if len(ids) == 1 {
		return "1 wallet in this portfolio no longer exists: "
	}
	return Plural(len(ids), "wallet") + " in this portfolio no longer exist: "
}

// kindCaption is the small caption next to a wallet in the picker. The dot already
// encodes the kind by shape; this says it in words.
func kindCaption(k core.Kind) string {
	switch k {
	case core.KindExchange:
		return "Exchange"
	case core.KindHardware:
		return "Hardware wallet"
	case core.KindHot:
		return "Hot wallet"
	case core.KindManual:
		return "Manual"
	case core.KindBrokerage:
		return "Brokerage"
	}
	return ""
}

// walletRow is a wallet option with its position in the full option list, which makes
// the input ids unique across groups.
type walletRow struct {
	WalletOption
	Index int
}

type walletGroup struct {
	Title string
	Rows  []walletRow
}

var walletGroupOrder = []string{"Exchanges", "Bitcoin", "Solana", "Manual", "Brokerage", "Other"}

func walletGroupTitle(o WalletOption) string {
	switch o.Type {
	case "coinbase", "kraken":
		return "Exchanges"
	case "bitcoin":
		return "Bitcoin"
	case "solana":
		return "Solana"
	case "manual":
		return "Manual"
	case "brokerage":
		return "Brokerage"
	}
	switch o.Kind {
	case core.KindExchange:
		return "Exchanges"
	case core.KindManual:
		return "Manual"
	}
	return "Other"
}

// walletGroups sorts the wallet options into Exchanges / Bitcoin / Solana / Manual /
// Brokerage (and Other for a type the UI does not know), keeping their order inside a
// group. Empty groups are left out.
func walletGroups(opts []WalletOption) []walletGroup {
	by := map[string][]walletRow{}
	for i, o := range opts {
		t := walletGroupTitle(o)
		by[t] = append(by[t], walletRow{WalletOption: o, Index: i})
	}
	var out []walletGroup
	for _, t := range walletGroupOrder {
		if len(by[t]) > 0 {
			out = append(out, walletGroup{Title: t, Rows: by[t]})
		}
	}
	return out
}

// AssetSearchKey is the lower-case string the asset picker's search box matches against.
func AssetSearchKey(o AssetOption) string {
	return strings.ToLower(o.Symbol + " " + o.Name + " " + o.Key)
}

func (p PortfolioFormPage) isEdit() bool { return p.EditID != "" }

// missingWallets are wallet ids the portfolio selects that are not among the options (the
// source was removed). Saving the form drops them, so the form says so.
func (p PortfolioFormPage) missingWallets() []string {
	known := make(map[string]bool, len(p.WalletOptions))
	for _, o := range p.WalletOptions {
		known[o.ID] = true
	}
	var out []string
	for id, on := range p.SelectedWallets {
		if on && !known[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func pfFormTitle(p PortfolioFormPage) string {
	if !p.isEdit() {
		return "New portfolio"
	}
	if n := strings.TrimSpace(p.Name); n != "" {
		return "Edit " + n
	}
	return "Edit portfolio"
}

func pfFormLede(p PortfolioFormPage) string {
	if p.isEdit() {
		return "Change what this portfolio counts. Your wallets and balances are not affected."
	}
	return "Group some of your holdings under a name. Pick wallets, assets, or both, and CryptKeeper shows just those."
}

func pfSaveLabel(p PortfolioFormPage) string {
	if p.isEdit() {
		return "Save changes"
	}
	return "Create portfolio"
}

type navCtxKey struct{}

// WithNav marks a render as a Datastar navigation: Layout then emits only the page
// regions (see navRegions) for the server to send as one SSE patch.
func WithNav(ctx context.Context) context.Context { return context.WithValue(ctx, navCtxKey{}, true) }

// IsNav reports whether ctx is a navigation render.
func IsNav(ctx context.Context) bool { v, _ := ctx.Value(navCtxKey{}).(bool); return v }

// navPF is the portfolio signal a main-nav link sets: Overview means all holdings.
func navPF(key string) string {
	if key == "overview" {
		return "all"
	}
	return ""
}

// newYork is the US markets' time zone, for naming a trading day.
var newYork = func() *time.Location {
	if l, err := time.LoadLocation("America/New_York"); err == nil {
		return l
	}
	return time.FixedZone("ET", -5*3600)
}()

// SessionDay names the trading day a stock's price and change are from: "today", or while
// the market is shut since an earlier session, that session's weekday ("Friday").
func SessionDay(p core.Price, now time.Time) string {
	if p.MarketOpen || p.MarketTime.IsZero() {
		return "today"
	}
	if now.IsZero() {
		now = time.Now()
	}
	mt, n := p.MarketTime.In(newYork), now.In(newYork)
	if mt.YearDay() == n.YearDay() && mt.Year() == n.Year() {
		return "today"
	}
	return mt.Weekday().String()
}

// ChangeLabel is the caption of an asset's price change: "24h" for a coin, the trading day
// for a stock ("today", "Friday").
func ChangeLabel(key string, p *core.Price, now time.Time) string {
	if _, stock := core.IsStockKey(key); stock && p != nil {
		return SessionDay(*p, now)
	}
	return "24h"
}

// MarketNote is a brokerage account's note while its prices are a past close: none while a
// market is open, or before any price says when it was set.
func MarketNote(w portfolio.WalletView, now time.Time) string {
	var newest *core.Price
	for _, r := range w.Rows {
		if _, stock := core.IsStockKey(r.AssetKey); !stock || r.Price == nil || r.Price.MarketTime.IsZero() {
			continue
		}
		if r.Price.MarketOpen {
			return ""
		}
		if newest == nil || r.Price.MarketTime.After(newest.MarketTime) {
			newest = r.Price
		}
	}
	if newest == nil {
		return ""
	}
	return "Markets are closed. Prices are " + SessionDay(*newest, now) + "'s close and update when trading opens."
}

// DayChange is how much a wallet's value moved over its holdings' latest session, in USD
// and percent of the value before it. Rows without a change count as unchanged.
func DayChange(w portfolio.WalletView) (usd, pct float64) {
	var before float64
	for _, r := range w.Rows {
		if !r.Priced || r.Price == nil || r.Price.Change24h == nil || *r.Price.Change24h <= -100 {
			before += r.ValueUSD
			continue
		}
		before += r.ValueUSD / (1 + *r.Price.Change24h/100)
	}
	usd = w.ValueUSD - before
	if before > 0 {
		pct = usd / before * 100
	}
	return usd, pct
}

// SessionCaption says which session a brokerage account's day change is over: "today", or
// "on Friday" while the market is shut.
func SessionCaption(w portfolio.WalletView, now time.Time) string {
	for _, r := range w.Rows {
		if r.Price == nil || r.Price.MarketTime.IsZero() {
			continue
		}
		if day := SessionDay(*r.Price, now); day != "today" {
			return "on " + day
		}
		break
	}
	return "today"
}

// ChangeHeader is the wallet holdings table's change column: "Day" for a brokerage
// account, "24h" otherwise.
func ChangeHeader(w portfolio.WalletView) string {
	if w.Kind == core.KindBrokerage {
		return "Day"
	}
	return "24h"
}
