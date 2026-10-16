// Package templates renders the CryptKeep web UI (templ components) and holds the number
// formatting rules shared by every page.
//
// Number rules (from the Vault Foundations board):
//
//	Fiat        $12,480.20      2 decimals, thousands separators, "$" prefix, true minus "−$1,282.40"
//	Coin        0.84213907      min 4, max 8 decimals; trailing zeros trimmed past 4; stables 2 decimals
//	Change      ▲ +4.21%        arrow + sign + color, never color alone; zero is "– 0.00%"
//	Address     bc1q8f…a7x4     first 6 + "…" + last 4
//	Stale price ~$3,912.00      older than 15 minutes (or flagged stale) gets a leading tilde
package templates

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

// Minus is the true minus sign (U+2212) used for every negative figure.
const Minus = "−"

// StaleAfter is the age after which a price is shown with a tilde.
const StaleAfter = 15 * time.Minute

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// group inserts thousands separators into a string of ASCII digits.
func group(digits string) string {
	n := len(digits)
	if n <= 3 {
		return digits
	}
	var b strings.Builder
	b.Grow(n + n/3)
	pre := n % 3
	if pre > 0 {
		b.WriteString(digits[:pre])
	}
	for i := pre; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// groupDecimal groups the integer part of a plain decimal string like "3374.80".
func groupDecimal(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return group(s[:i]) + s[i:]
	}
	return group(s)
}

// isZeroDecimal reports whether a formatted decimal like "0.00" has only zero digits.
func isZeroDecimal(s string) bool {
	for _, r := range s {
		if r >= '1' && r <= '9' {
			return false
		}
	}
	return true
}

// Fiat formats a USD amount: "$12,480.20", negatives "−$1,282.40" (U+2212).
// A value that rounds to zero never gets a sign. NaN and ±Inf render as "—".
func Fiat(v float64) string {
	if !finite(v) {
		return "—"
	}
	s := strconv.FormatFloat(math.Abs(v), 'f', 2, 64)
	out := "$" + groupDecimal(s)
	if v < 0 && !isZeroDecimal(s) {
		return Minus + out
	}
	return out
}

// PriceFiat formats a unit price. At or above $1 it follows the Fiat rule (2 decimals).
// Below $1 more precision is needed to be meaningful: 4 decimals from $0.01 up, and up to
// 8 decimals (trailing zeros trimmed past 4) for sub-cent tokens such as BONK.
func PriceFiat(v float64) string {
	if !finite(v) {
		return "—"
	}
	a := math.Abs(v)
	switch {
	case a >= 1 || a == 0:
		return Fiat(v)
	case a >= 0.01:
		s := strconv.FormatFloat(a, 'f', 4, 64)
		out := "$" + s
		if v < 0 {
			return Minus + out
		}
		return out
	}
	s := trimPast(strconv.FormatFloat(a, 'f', 8, 64), 4)
	out := "$" + s
	if v < 0 && !isZeroDecimal(s) {
		return Minus + out
	}
	return out
}

// trimPast drops trailing zeros of a decimal string but keeps at least min fractional digits.
func trimPast(s string, min int) string {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	floor := dot + 1 + min
	for len(s) > floor && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	return s
}

// CoinAmount formats a coin quantity without its symbol.
//
// Rule: up to 8 decimals, trailing zeros trimmed past the 4th, so a value always shows at
// least 4 decimals ("3.25" -> "3.2500", "185.2" -> "185.2000", "0.42108000" -> "0.42108",
// "0.84213907" stays). Stablecoins (core.Pegged: USD, USDC, USDT) use 2 decimals
// ("3,374.80"). The integer part gets thousands separators. Negative amounts (debt) use
// the true minus sign. NaN and ±Inf render as "—".
func CoinAmount(v float64, sym string) string {
	if !finite(v) {
		return "—"
	}
	var s string
	if core.Pegged[strings.ToUpper(sym)] {
		s = strconv.FormatFloat(math.Abs(v), 'f', 2, 64)
	} else {
		s = trimPast(strconv.FormatFloat(math.Abs(v), 'f', 8, 64), 4)
	}
	out := groupDecimal(s)
	if v < 0 && !isZeroDecimal(s) {
		return Minus + out
	}
	return out
}

// Coin formats a coin quantity followed by its symbol: "0.84213907 BTC", "3,374.80 USDC".
// With an empty symbol it is the same as CoinAmount.
func Coin(v float64, sym string) string {
	n := CoinAmount(v, sym)
	if sym == "" {
		return n
	}
	return n + " " + sym
}

// ChangePct formats a percentage change with an arrow and a true minus sign, and says
// which way it went: "▲ +4.21%" (up), "▼ −1.08%" (down), "– 0.00%" (flat, also for NaN
// and values that round to zero).
func ChangePct(p float64) (text string, dir string) {
	if !finite(p) {
		return "– 0.00%", "flat"
	}
	s := strconv.FormatFloat(math.Abs(p), 'f', 2, 64)
	switch {
	case isZeroDecimal(s):
		return "– 0.00%", "flat"
	case p > 0:
		return "▲ +" + s + "%", "up"
	default:
		return "▼ " + Minus + s + "%", "down"
	}
}

// SignedPct is the percentage without the arrow: "+2.56%", "−1.08%", "0.00%".
func SignedPct(p float64) string {
	if !finite(p) {
		return "0.00%"
	}
	s := strconv.FormatFloat(math.Abs(p), 'f', 2, 64)
	switch {
	case isZeroDecimal(s):
		return "0.00%"
	case p > 0:
		return "+" + s + "%"
	default:
		return Minus + s + "%"
	}
}

// ChangeAbs formats an absolute USD change with its sign: "+$1,204.18", "−$412.06".
// Zero is plain "$0.00".
func ChangeAbs(v float64) string {
	if !finite(v) {
		return "—"
	}
	s := strconv.FormatFloat(math.Abs(v), 'f', 2, 64)
	switch {
	case isZeroDecimal(s):
		return "$0.00"
	case v > 0:
		return "+$" + groupDecimal(s)
	default:
		return Minus + "$" + groupDecimal(s)
	}
}

// HeldChange is what a price change of pct percent did to a holding now worth value: its
// value now minus its value at the old price. ok is false when the percentage can't be
// applied (NaN, or a fall of 100% or more).
func HeldChange(value, pct float64) (usd float64, ok bool) {
	if !finite(value) || !finite(pct) || pct <= -100 {
		return 0, false
	}
	return value * pct / (100 + pct), true
}

// Direction classifies a change as "up", "down" or "flat" using the same rounding as
// ChangePct (two decimals).
func Direction(p float64) string {
	_, dir := ChangePct(p)
	return dir
}

// Arrow returns the glyph for a direction: "▲", "▼" or "–".
func Arrow(dir string) string {
	switch dir {
	case "up":
		return "▲"
	case "down":
		return "▼"
	}
	return "–"
}

// ShortAddr shortens an address to its first 6 and last 4 characters around an ellipsis
// ("bc1q8f…a7x4"). Addresses of 11 characters or fewer are returned unchanged.
func ShortAddr(a string) string {
	if utf8.RuneCountInString(a) <= 11 {
		return a
	}
	r := []rune(a)
	return string(r[:6]) + "…" + string(r[len(r)-4:])
}

// StalePrice reports whether a price should render with a tilde and the warn color: the
// provider flagged it stale (served from the last-good cache) or it is older than 15
// minutes. A price with no timestamp (for example a pegged stablecoin) is only stale when
// flagged.
func StalePrice(p core.Price, now time.Time) bool {
	if p.Stale {
		return true
	}
	if p.AsOf.IsZero() {
		return false
	}
	return now.Sub(p.AsOf) > StaleAfter
}

// PriceText renders a price cell: "$3,180.40", or "~$3,912.00" with stale=true. A missing
// price renders "—".
func PriceText(p *core.Price, now time.Time) (text string, stale bool) {
	if p == nil {
		return "—", false
	}
	text = PriceFiat(p.USD)
	if StalePrice(*p, now) {
		return "~" + text, true
	}
	return text, false
}

// Since formats the age of t relative to now compactly: "<1m", "12m", "3h", "2d".
// A zero time returns "".
func Since(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// Ago formats a relative time: "just now" (under 45 s), "2m ago", "3h ago", "4d ago".
// A zero time is "never".
func Ago(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	if now.Sub(t) < 45*time.Second {
		return "just now"
	}
	return Since(t, now) + " ago"
}

func num1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func numTrim(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// SVGPath turns chart points into SVG path data for a viewBox of w by h, like the
// Portfolio board: points are spread evenly along x, and y is padded so the line uses
// about 85% of the height (7.5% above, 7.5% below). The area path is the line closed down
// to the bottom edge. A flat series is drawn through the middle. Non-finite values are
// skipped; with no usable points both results are empty.
func SVGPath(points []portfolio.Point, w, h float64) (line, area string) {
	vals := make([]float64, 0, len(points))
	for _, p := range points {
		if finite(p.V) {
			vals = append(vals, p.V)
		}
	}
	n := len(vals)
	if n == 0 {
		return "", ""
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	top, usable := h*0.075, h*0.85
	y := func(v float64) float64 {
		if hi == lo {
			return h / 2
		}
		return top + usable - (v-lo)/(hi-lo)*usable
	}
	var b strings.Builder
	if n == 1 {
		fmt.Fprintf(&b, "M0 %s L%s %s", num1(y(vals[0])), num1(w), num1(y(vals[0])))
	} else {
		for i, v := range vals {
			if i == 0 {
				b.WriteByte('M')
			} else {
				b.WriteString(" L")
			}
			b.WriteString(num1(float64(i) * w / float64(n-1)))
			b.WriteByte(' ')
			b.WriteString(num1(y(v)))
		}
	}
	line = b.String()
	area = line + " L" + numTrim(w) + " " + numTrim(h) + " L0 " + numTrim(h) + " Z"
	return line, area
}

// DiscKey maps a ticker to its coin-mark tint: "btc", "eth", "sol", "usd" or "neutral".
// Wrapped and staked variants follow their base asset (WBTC, stETH, jitoSOL, USDC...).
func DiscKey(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	switch {
	case s == "BTC" || strings.HasSuffix(s, "BTC"):
		return "btc"
	case s == "ETH" || strings.HasSuffix(s, "ETH"):
		return "eth"
	case s == "SOL" || strings.HasSuffix(s, "SOL"):
		return "sol"
	case s == "USD" || strings.HasPrefix(s, "USD") || s == "DAI" || s == "PYUSD" || s == "FDUSD":
		return "usd"
	}
	return "neutral"
}

// Monogram returns the coin mark for a symbol: the ticker (at most 4 characters, upper
// case) and the CSS custom properties for the disc background and foreground
// ("--disc-btc-bg", "--disc-btc-fg"). Use var() around them in CSS, or DiscClass for the
// ready-made class.
func Monogram(symbol string) (text string, bgVar, fgVar string) {
	text = strings.ToUpper(strings.TrimSpace(symbol))
	if r := []rune(text); len(r) > 4 {
		text = string(r[:4])
	}
	if text == "" {
		text = "?"
	}
	k := DiscKey(symbol)
	return text, "--disc-" + k + "-bg", "--disc-" + k + "-fg"
}

// DiscClass is the app.css class that paints a coin-mark disc for a symbol.
func DiscClass(symbol string) string { return "disc disc--" + DiscKey(symbol) }

// AllocSlice is one segment of the allocation bar and one legend row.
type AllocSlice struct {
	Name     string  // "Bitcoin", or "Other"
	Symbol   string  // "BTC"; empty for Other
	Pct      float64 // 0..100
	PctText  string  // "56%", or "<1%"
	ValueUSD float64
	ColorVar string // CSS custom property name, e.g. "--src-hardware"
}

// Color is ColorVar wrapped in var(), ready for a style declaration.
func (s AllocSlice) Color() string { return "var(" + s.ColorVar + ")" }

// Allocation builds the allocation bar from merged asset rows. Colors keep a fixed order
// in both themes: BTC is pinned first and warm (--src-hardware), then --accent,
// --src-hot and --gain for the next largest assets, and everything past the top 4 is
// summed into "Other" (--src-other). Without BTC the warm color stays unused, so only the
// top 3 assets get their own segment. Assets with no positive value are ignored.
func Allocation(assets []portfolio.AssetRow) []AllocSlice {
	var rows []portfolio.AssetRow
	var total float64
	for _, a := range assets {
		if a.ValueUSD > 0 && finite(a.ValueUSD) {
			rows = append(rows, a)
			total += a.ValueUSD
		}
	}
	if total <= 0 {
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ValueUSD > rows[j].ValueUSD })

	palette := []string{"--accent", "--src-hot", "--gain"}
	var named []portfolio.AssetRow
	for i, a := range rows {
		if strings.EqualFold(a.Symbol, "BTC") {
			named = append(named, a)
			rows = append(rows[:i:i], rows[i+1:]...)
			palette = []string{"--src-hardware", "--accent", "--src-hot", "--gain"}
			break
		}
	}
	for len(named) < len(palette) && len(rows) > 0 {
		named = append(named, rows[0])
		rows = rows[1:]
	}

	out := make([]AllocSlice, 0, len(named)+1)
	for i, a := range named {
		name := a.Name
		if name == "" {
			name = a.Symbol
		}
		out = append(out, newSlice(name, a.Symbol, a.ValueUSD, total, palette[i]))
	}
	if len(rows) > 0 {
		var other float64
		for _, a := range rows {
			other += a.ValueUSD
		}
		out = append(out, newSlice("Other", "", other, total, "--src-other"))
	}
	return out
}

func newSlice(name, sym string, v, total float64, color string) AllocSlice {
	pct := v / total * 100
	text := "<1%"
	if r := math.Round(pct); r >= 1 {
		text = strconv.Itoa(int(r)) + "%"
	}
	return AllocSlice{Name: name, Symbol: sym, Pct: pct, PctText: text, ValueUSD: v, ColorVar: color}
}

// Plural renders "1 asset" / "3 assets" / "7 addresses".
func Plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "s") {
		return strconv.Itoa(n) + " " + word + "es"
	}
	return strconv.Itoa(n) + " " + word + "s"
}
