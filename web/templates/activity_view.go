package templates

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/core"
)

// The display decisions of the activity feed, kept in Go so they can be unit tested.

// TxGlyph is the character in the direction disc and its color family ("gain", "loss",
// "accent" or "neutral"). The glyph carries the direction, so it never relies on color.
func TxGlyph(t core.Tx) (glyph, tone string) {
	switch t.Kind {
	case core.TxReceive:
		return "↓", "gain"
	case core.TxSend:
		return "↑", "loss"
	case core.TxTransfer, core.TxSwap, core.TxTrade, core.TxStake:
		return "⇄", "accent"
	case core.TxReward:
		return "★", "gain"
	case core.TxFee:
		return Minus, "neutral"
	}
	return "•", "neutral"
}

// fiatLike reports whether a symbol is cash (a stablecoin or a fiat currency), which makes
// a trade a buy or a sell.
func fiatLike(sym string) bool {
	s := strings.ToUpper(strings.TrimSpace(sym))
	if core.Pegged[s] {
		return true
	}
	switch s {
	case "USD", "EUR", "GBP", "CAD", "AUD", "CHF", "JPY", "DAI", "PYUSD", "FDUSD":
		return true
	}
	return false
}

// legSymbols lists the distinct symbols of some legs, at most two, then "+N".
func legSymbols(legs []core.Leg) string {
	var syms []string
	seen := map[string]bool{}
	for _, l := range legs {
		s := strings.TrimSpace(l.Symbol)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		syms = append(syms, s)
	}
	switch {
	case len(syms) == 0:
		return ""
	case len(syms) <= 2:
		return strings.Join(syms, " + ")
	}
	return syms[0] + " + " + syms[1] + " +" + strconv.Itoa(len(syms)-2)
}

// splitLegs separates what left the source (negative) from what arrived (positive).
func splitLegs(legs []core.Leg) (out, in []core.Leg) {
	for _, l := range legs {
		switch {
		case l.Amount < 0:
			out = append(out, l)
		case l.Amount > 0:
			in = append(in, l)
		}
	}
	return out, in
}

// peerText shortens a counterparty that is an address, and leaves a name alone.
func peerText(cp string) string {
	cp = strings.TrimSpace(cp)
	if len(cp) > 24 && !strings.ContainsAny(cp, " ") {
		return ShortAddr(cp)
	}
	return cp
}

func joinWords(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " ")
}

// TxTitle is the headline of a row: "Received BTC", "Sent BTC to bc1qx2…m8rt",
// "Transfer · Coinbase → Cold storage", "Swapped SOL → USDC", "Bought BTC", "Sold ETH",
// "Staking reward".
func TxTitle(r ActivityRow) string {
	t := r.Tx
	out, in := splitLegs(t.Legs)
	switch t.Kind {
	case core.TxReceive:
		title := joinWords("Received", legSymbols(in))
		if t.Counterparty != "" {
			title += " from " + peerText(t.Counterparty)
		}
		return title
	case core.TxSend:
		title := joinWords("Sent", legSymbols(out))
		if t.Counterparty != "" {
			title += " to " + peerText(t.Counterparty)
		}
		return title
	case core.TxTransfer:
		if r.PeerLabel != "" && r.SourceLabel != "" {
			if len(t.Legs) > 0 && t.Legs[0].Amount < 0 {
				return "Transfer · " + r.SourceLabel + " → " + r.PeerLabel
			}
			return "Transfer · " + r.PeerLabel + " → " + r.SourceLabel
		}
		return joinWords("Transfer", legSymbols(t.Legs))
	case core.TxSwap:
		return swapTitle("Swapped", out, in)
	case core.TxTrade:
		var coins []core.Leg
		cash := false
		for _, l := range t.Legs {
			if fiatLike(l.Symbol) {
				cash = true
			} else {
				coins = append(coins, l)
			}
		}
		if cash && len(coins) == 1 {
			if coins[0].Amount < 0 {
				return joinWords("Sold", coins[0].Symbol)
			}
			return joinWords("Bought", coins[0].Symbol)
		}
		return swapTitle("Traded", out, in)
	case core.TxReward:
		return "Staking reward"
	case core.TxStake:
		if len(out) > 0 {
			return joinWords("Staked", legSymbols(out))
		}
		return joinWords("Unstaked", legSymbols(in))
	case core.TxFee:
		return joinWords("Fee", legSymbols(t.Legs))
	}
	if n := strings.TrimSpace(t.Note); n != "" {
		return n
	}
	return joinWords("Activity", legSymbols(t.Legs))
}

func swapTitle(verb string, out, in []core.Leg) string {
	from, to := legSymbols(out), legSymbols(in)
	if from == "" || to == "" {
		return joinWords(verb, from, to)
	}
	return verb + " " + from + " → " + to
}

// TxStatusView is the status chip of a row: pending shows the confirmations so far, a
// failed entry says so, and a settled one is the quiet neutral "Confirmed".
func TxStatusView(t core.Tx) (glyph, label, tone string) {
	switch t.Status {
	case core.TxPending:
		if t.Confirmations > 0 {
			return "◐", strconv.Itoa(t.Confirmations) + " conf", "warn"
		}
		return "◐", "Pending", "warn"
	case core.TxFailed:
		return "✕", "Failed", "loss"
	}
	return "", "Confirmed", "neutral"
}

// TxStatusTitle is the longer text behind the status chip (its tooltip).
func TxStatusTitle(t core.Tx) string {
	switch t.Status {
	case core.TxPending:
		if t.Confirmations > 0 {
			return "Pending, " + Plural(t.Confirmations, "confirmation") + " so far"
		}
		return "Pending, not confirmed yet"
	case core.TxFailed:
		return "Failed, no funds moved"
	}
	return "Confirmed"
}

// TxDate is the day of an entry: "Oct 14", or "Oct 14, 2025" when it is not from the
// current year.
func TxDate(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t, now = Local(t), Local(now)
	if now.IsZero() || t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

// TxClock is the time of day of an entry: "14:32".
func TxClock(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return Local(t).Format("15:04")
}

// TxWhen is the date and time of an entry: "Oct 14, 14:32".
func TxWhen(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	return TxDate(t, now) + ", " + TxClock(t)
}

// TxHash is the identifier shown (shortened) in the detail line: the on-chain hash, or,
// for an entry that links to an explorer, its external id. Exchange entries without a hash
// show none.
func TxHash(t core.Tx) string {
	if t.TxHash != "" {
		return t.TxHash
	}
	if t.ExplorerURL != "" {
		return t.ExternalID
	}
	return ""
}

// SafeExplorerURL returns an explorer link only when it is a plain https URL.
func SafeExplorerURL(u string) string { return SafeLogoURL(u) }

// LegText is the signed amount of a leg: "+0.0500 BTC", "−0.0200 BTC" (true minus).
func LegText(l core.Leg) string {
	if l.Amount > 0 {
		return "+" + Coin(l.Amount, l.Symbol)
	}
	return Coin(l.Amount, l.Symbol)
}

// LegMask is LegText with the digits hidden, for privacy mode. The sign stays: the
// direction disc already tells it.
func LegMask(l core.Leg) string {
	sign := ""
	switch {
	case l.Amount > 0:
		sign = "+"
	case l.Amount < 0:
		sign = Minus
	}
	return sign + "•••• " + l.Symbol
}

// TxFiat is the value of an entry at the time it happened. For a one-sided entry it is
// the total of the legs; for a trade or swap it is the larger side. ok is false when no
// leg carries a value.
func TxFiat(t core.Tx) (v float64, ok bool) {
	var pos, neg float64
	for _, l := range t.Legs {
		if l.USD == nil || !finite(*l.USD) {
			continue
		}
		ok = true
		if *l.USD >= 0 {
			pos += *l.USD
		} else {
			neg -= *l.USD
		}
	}
	return max(pos, neg), ok
}

// FeeLeg is the fee of an entry as a positive leg, or false when it has none.
func FeeLeg(t core.Tx) (core.Leg, bool) {
	if t.Fee == nil || t.Fee.Amount == 0 {
		return core.Leg{}, false
	}
	l := *t.Fee
	if l.Amount < 0 {
		l.Amount = -l.Amount
	}
	return l, true
}

// activityFiltered reports whether any filter is set.
func (p ActivityPage) activityFiltered() bool {
	return p.SourceID != "" || p.AssetKey != "" || p.Kind != "" || p.Period != ""
}

// periodOpts are the Period filter's options: presets, each year with activity, custom.
func (p ActivityPage) periodOpts() []selectOpt {
	opts := []selectOpt{
		{"", "All time"}, {"7d", "Last 7 days"}, {"30d", "Last 30 days"}, {"90d", "Last 90 days"},
		{"mtd", "Month to date"}, {"ytd", "Year to date"},
	}
	for _, y := range p.Years {
		opts = append(opts, selectOpt{"y" + strconv.Itoa(y), strconv.Itoa(y)})
	}
	return append(opts, selectOpt{"custom", "Custom range…"})
}

// actExportURL is the CSV link for a full page render (before Datastar takes over its href).
func actExportURL(p ActivityPage) string {
	q := url.Values{}
	for k, v := range map[string]string{"source": p.SourceID, "asset": p.AssetKey, "kind": p.Kind, "range": p.Period, "from": p.From, "to": p.To} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return "/export/activity.csv"
	}
	return "/export/activity.csv?" + q.Encode()
}

// actKinds are the options of the Kind filter, in display order. The values are core.TxKind.
var actKinds = []selectOpt{
	{"", "All kinds"}, {string(core.TxReceive), "Received"}, {string(core.TxSend), "Sent"},
	{string(core.TxTransfer), "Transfers"}, {string(core.TxTrade), "Trades"}, {string(core.TxSwap), "Swaps"},
	{string(core.TxReward), "Rewards"}, {string(core.TxStake), "Staking"}, {string(core.TxFee), "Fees"},
}

func (p ActivityPage) sourceOpts() []selectOpt {
	opts := []selectOpt{{"", "All sources"}}
	for _, s := range p.Sources {
		opts = append(opts, selectOpt{s.ID, s.Label})
	}
	return opts
}

func (p ActivityPage) assetOpts() []selectOpt {
	opts := []selectOpt{{"", "All assets"}}
	for _, a := range p.Assets {
		label := a.Symbol
		if a.Name != "" && a.Name != a.Symbol {
			label += " · " + a.Name
		}
		opts = append(opts, selectOpt{a.Key, label})
	}
	return opts
}

// withCurrent makes sure the current filter value is one of the options (a source that
// was removed, an asset that is no longer held), so the select never silently shows
// "All" while a filter is applied.
func withCurrent(opts []selectOpt, current string) []selectOpt {
	return withCurrentLabel(opts, current, current)
}

// withCurrentLabel is withCurrent with a label for the added option.
func withCurrentLabel(opts []selectOpt, current, label string) []selectOpt {
	if current == "" {
		return opts
	}
	for _, o := range opts {
		if o.Value == current {
			return opts
		}
	}
	return append(opts, selectOpt{current, label})
}

// syncEntries is "1,204 entries" / "1 entry".
func syncEntries(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return Comma(int64(n)) + " entries"
}

// syncWhen is "Synced 2m ago" / "Not synced yet".
func syncWhen(s SyncRow, now time.Time) string {
	if s.LastSync.IsZero() {
		return "Not synced yet"
	}
	return "Synced " + Ago(s.LastSync, now)
}

// nowOr is now, or the wall clock when the page did not carry one.
func nowOr(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now()
	}
	return now
}

// activityViewAll links from a wallet's Activity tab to the filtered Activity page.
func activityViewAll(sourceID string) string {
	return "/activity?source=" + url.QueryEscape(sourceID)
}
