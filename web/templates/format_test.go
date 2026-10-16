package templates

import (
	"math"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

func TestFiat(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{12480.2, "$12,480.20"},
		{0, "$0.00"},
		{999.999, "$1,000.00"},
		{1000000, "$1,000,000.00"},
		{48210.37, "$48,210.37"},
		{-1282.4, "−$1,282.40"},
		{-0.001, "$0.00"}, // rounds to zero: no sign
		{-0.0, "$0.00"},
		{0.5, "$0.50"},
		{math.NaN(), "—"},
		{math.Inf(1), "—"},
	}
	for _, tc := range tests {
		if got := Fiat(tc.in); got != tc.want {
			t.Errorf("Fiat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if !strings.Contains(Fiat(-5), "−") || strings.Contains(Fiat(-5), "-") {
		t.Errorf("negative fiat must use U+2212, got %q", Fiat(-5))
	}
}

func TestPriceFiat(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{64120, "$64,120.00"},
		{1, "$1.00"},
		{0.92, "$0.9200"},
		{0.0123, "$0.0123"},
		{0.0000214, "$0.0000214"},
		{0.00000001, "$0.00000001"},
		{0, "$0.00"},
	}
	for _, tc := range tests {
		if got := PriceFiat(tc.in); got != tc.want {
			t.Errorf("PriceFiat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCoin(t *testing.T) {
	tests := []struct {
		v    float64
		sym  string
		want string
	}{
		{0.84213907, "BTC", "0.84213907 BTC"}, // up to 8 decimals
		{0.42108, "BTC", "0.42108 BTC"},       // zeros trimmed past 4
		{0.42108000, "BTC", "0.42108 BTC"},
		{3.25, "ETH", "3.2500 ETH"}, // never fewer than 4
		{185.2, "DOT", "185.2000 DOT"},
		{39.041, "SOL", "39.0410 SOL"},
		{1234567.5, "BONK", "1,234,567.5000 BONK"},
		{12500000, "BONK", "12,500,000.0000 BONK"},
		{0.123456789, "X", "0.12345679 X"},
		{0, "BTC", "0.0000 BTC"},
		{-0.5, "BTC", "−0.5000 BTC"},
		{3374.8, "USDC", "3,374.80 USDC"}, // stables use 2 decimals
		{3374.8, "usdt", "3,374.80 usdt"},
		{12, "USD", "12.00 USD"},
		{12.5, "", "12.5000"},
		{math.NaN(), "BTC", "— BTC"},
	}
	for _, tc := range tests {
		if got := Coin(tc.v, tc.sym); got != tc.want {
			t.Errorf("Coin(%v, %q) = %q, want %q", tc.v, tc.sym, got, tc.want)
		}
	}
	if got := CoinAmount(0.5, "BTC"); got != "0.5000" {
		t.Errorf("CoinAmount = %q", got)
	}
}

func TestChangePct(t *testing.T) {
	tests := []struct {
		in     float64
		text   string
		dir    string
		signed string
		arrow  string
	}{
		{4.21, "▲ +4.21%", "up", "+4.21%", "▲"},
		{-1.08, "▼ −1.08%", "down", "−1.08%", "▼"},
		{0, "– 0.00%", "flat", "0.00%", "–"},
		{0.004, "– 0.00%", "flat", "0.00%", "–"},
		{-0.004, "– 0.00%", "flat", "0.00%", "–"},
		{186.974, "▲ +186.97%", "up", "+186.97%", "▲"},
		{math.NaN(), "– 0.00%", "flat", "0.00%", "–"},
	}
	for _, tc := range tests {
		text, dir := ChangePct(tc.in)
		if text != tc.text || dir != tc.dir {
			t.Errorf("ChangePct(%v) = %q, %q; want %q, %q", tc.in, text, dir, tc.text, tc.dir)
		}
		if got := SignedPct(tc.in); got != tc.signed {
			t.Errorf("SignedPct(%v) = %q, want %q", tc.in, got, tc.signed)
		}
		if got := Arrow(Direction(tc.in)); got != tc.arrow {
			t.Errorf("Arrow(Direction(%v)) = %q, want %q", tc.in, got, tc.arrow)
		}
	}
}

func TestChangeAbs(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{1204.18, "+$1,204.18"},
		{-412.06, "−$412.06"},
		{0, "$0.00"},
		{0.001, "$0.00"},
		{-1234567.891, "−$1,234,567.89"},
	}
	for _, tc := range tests {
		if got := ChangeAbs(tc.in); got != tc.want {
			t.Errorf("ChangeAbs(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"bc1q8f3k7z2m9w5x6p4n8d0r3t7y2u6h4j9a7x4", "bc1q8f…a7x4"},
		{"7xKXqYqHn1eGk4kVj3b8f2xZc9mPd5sLr6tNwQ3vJ8aB", "7xKXqY…J8aB"},
		{"short", "short"},
		{"12345678901", "12345678901"}, // 11 chars: nothing to gain
		{"123456789012", "123456…9012"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := ShortAddr(tc.in); got != tc.want {
			t.Errorf("ShortAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStalePrice(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		p    core.Price
		want bool
	}{
		{"fresh", core.Price{USD: 1, AsOf: now.Add(-2 * time.Minute)}, false},
		{"exactly 15m", core.Price{USD: 1, AsOf: now.Add(-15 * time.Minute)}, false},
		{"older than 15m", core.Price{USD: 1, AsOf: now.Add(-15*time.Minute - time.Second)}, true},
		{"flagged stale though recent", core.Price{USD: 1, AsOf: now, Stale: true}, true},
		{"no timestamp, not flagged", core.Price{USD: 1}, false},
		{"no timestamp, flagged", core.Price{USD: 1, Stale: true}, true},
	}
	for _, tc := range tests {
		if got := StalePrice(tc.p, now); got != tc.want {
			t.Errorf("%s: StalePrice = %v, want %v", tc.name, got, tc.want)
		}
	}

	if text, stale := PriceText(&core.Price{USD: 3912, AsOf: now.Add(-time.Hour)}, now); text != "~$3,912.00" || !stale {
		t.Errorf("PriceText stale = %q, %v", text, stale)
	}
	if text, stale := PriceText(&core.Price{USD: 3912, AsOf: now}, now); text != "$3,912.00" || stale {
		t.Errorf("PriceText fresh = %q, %v", text, stale)
	}
	if text, stale := PriceText(nil, now); text != "—" || stale {
		t.Errorf("PriceText nil = %q, %v", text, stale)
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		d     time.Duration
		ago   string
		since string
	}{
		{10 * time.Second, "just now", "<1m"},
		{44 * time.Second, "just now", "<1m"},
		{50 * time.Second, "<1m ago", "<1m"},
		{2 * time.Minute, "2m ago", "2m"},
		{59 * time.Minute, "59m ago", "59m"},
		{3 * time.Hour, "3h ago", "3h"},
		{23*time.Hour + 59*time.Minute, "23h ago", "23h"},
		{49 * time.Hour, "2d ago", "2d"},
		{-time.Minute, "just now", "<1m"}, // clock skew
	}
	for _, tc := range tests {
		at := now.Add(-tc.d)
		if got := Ago(at, now); got != tc.ago {
			t.Errorf("Ago(-%v) = %q, want %q", tc.d, got, tc.ago)
		}
		if got := Since(at, now); got != tc.since {
			t.Errorf("Since(-%v) = %q, want %q", tc.d, got, tc.since)
		}
	}
	if got := Ago(time.Time{}, now); got != "never" {
		t.Errorf("Ago(zero) = %q", got)
	}
	if got := Since(time.Time{}, now); got != "" {
		t.Errorf("Since(zero) = %q", got)
	}
}

func TestSVGPath(t *testing.T) {
	pts := []portfolio.Point{{V: 10}, {V: 20}, {V: 15}, {V: 30}}
	line, area := SVGPath(pts, 800, 220)
	if !strings.HasPrefix(line, "M0.0 ") {
		t.Errorf("line should start at x=0: %q", line)
	}
	if got := strings.Count(line, "L"); got != 3 {
		t.Errorf("want 3 segments, got %d in %q", got, line)
	}
	if !strings.Contains(line, "L800.0 ") {
		t.Errorf("line should reach x=800: %q", line)
	}
	if want := line + " L800 220 L0 220 Z"; area != want {
		t.Errorf("area = %q, want %q", area, want)
	}
	// The line uses about 85% of the height: the max sits near the top pad, the min near
	// the bottom pad.
	if !strings.Contains(line, "M0.0 203.5") { // 220*0.925 = 203.5
		t.Errorf("min should sit at y=203.5: %q", line)
	}
	if !strings.HasSuffix(line, " 16.5") { // 220*0.075 = 16.5
		t.Errorf("max should sit at y=16.5: %q", line)
	}

	flat, _ := SVGPath([]portfolio.Point{{V: 5}, {V: 5}, {V: 5}}, 360, 96)
	if flat != "M0.0 48.0 L180.0 48.0 L360.0 48.0" {
		t.Errorf("flat series should run through the middle: %q", flat)
	}

	one, oneArea := SVGPath([]portfolio.Point{{V: 5}}, 800, 220)
	if one != "M0 110.0 L800.0 110.0" || !strings.HasSuffix(oneArea, "Z") {
		t.Errorf("single point: %q / %q", one, oneArea)
	}

	if l, a := SVGPath(nil, 800, 220); l != "" || a != "" {
		t.Errorf("no points should give empty paths, got %q %q", l, a)
	}
	if l, _ := SVGPath([]portfolio.Point{{V: math.NaN()}, {V: 1}, {V: 2}}, 100, 100); strings.Contains(l, "NaN") {
		t.Errorf("NaN leaked into the path: %q", l)
	}
}

func TestMonogram(t *testing.T) {
	tests := []struct {
		sym  string
		text string
		key  string
	}{
		{"BTC", "BTC", "btc"},
		{"WBTC", "WBTC", "btc"},
		{"ETH", "ETH", "eth"},
		{"stETH", "STET", "eth"},
		{"SOL", "SOL", "sol"},
		{"jitoSOL", "JITO", "sol"},
		{"mSOL", "MSOL", "sol"},
		{"USDC", "USDC", "usd"},
		{"USDT", "USDT", "usd"},
		{"USD", "USD", "usd"},
		{"JUP", "JUP", "neutral"},
		{"bonk", "BONK", "neutral"},
		{"DOT", "DOT", "neutral"},
		{"", "?", "neutral"},
	}
	for _, tc := range tests {
		text, bg, fg := Monogram(tc.sym)
		if text != tc.text {
			t.Errorf("Monogram(%q) text = %q, want %q", tc.sym, text, tc.text)
		}
		if bg != "--disc-"+tc.key+"-bg" || fg != "--disc-"+tc.key+"-fg" {
			t.Errorf("Monogram(%q) vars = %q, %q, want key %q", tc.sym, bg, fg, tc.key)
		}
		if got := DiscClass(tc.sym); got != "disc disc--"+tc.key {
			t.Errorf("DiscClass(%q) = %q", tc.sym, got)
		}
	}
}

func row(sym, name string, v float64) portfolio.AssetRow {
	return portfolio.AssetRow{AssetKey: sym, Symbol: sym, Name: name, ValueUSD: v, Priced: true}
}

func TestAllocationPinsBTCFirst(t *testing.T) {
	// BTC is not the largest asset: it must still come first and get the warm color.
	assets := []portfolio.AssetRow{
		row("ETH", "Ethereum", 500),
		row("BTC", "Bitcoin", 300),
		row("SOL", "Solana", 100),
		row("USDC", "USD Coin", 50),
		row("DOT", "Polkadot", 30),
		row("LINK", "Chainlink", 20),
	}
	got := Allocation(assets)
	wantNames := []string{"Bitcoin", "Ethereum", "Solana", "USD Coin", "Other"}
	wantColors := []string{"--src-hardware", "--accent", "--src-hot", "--gain", "--src-other"}
	if len(got) != len(wantNames) {
		t.Fatalf("got %d slices: %+v", len(got), got)
	}
	var total float64
	for i, s := range got {
		if s.Name != wantNames[i] || s.ColorVar != wantColors[i] {
			t.Errorf("slice %d = %s/%s, want %s/%s", i, s.Name, s.ColorVar, wantNames[i], wantColors[i])
		}
		total += s.Pct
	}
	if math.Abs(total-100) > 1e-9 {
		t.Errorf("percentages sum to %v, want 100", total)
	}
	if got[4].ValueUSD != 50 { // DOT + LINK
		t.Errorf("Other = %v, want 50", got[4].ValueUSD)
	}
	if got[0].PctText != "30%" || got[4].PctText != "5%" {
		t.Errorf("pct text: %q %q", got[0].PctText, got[4].PctText)
	}
	if got[0].Color() != "var(--src-hardware)" {
		t.Errorf("Color() = %q", got[0].Color())
	}
}

func TestAllocationWithoutBTC(t *testing.T) {
	assets := []portfolio.AssetRow{
		row("SOL", "Solana", 400), row("ETH", "Ethereum", 300), row("USDC", "USD Coin", 200),
		row("DOT", "Polkadot", 60), row("LINK", "Chainlink", 40),
	}
	got := Allocation(assets)
	// The warm color is reserved for BTC, so only the top 3 get their own segment.
	want := []string{"--accent", "--src-hot", "--gain", "--src-other"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, s := range got {
		if s.ColorVar != want[i] {
			t.Errorf("slice %d color %s, want %s", i, s.ColorVar, want[i])
		}
		if s.ColorVar == "--src-hardware" {
			t.Errorf("warm color used without BTC")
		}
	}
	if got[0].Name != "Solana" || got[3].Name != "Other" || got[3].ValueUSD != 100 {
		t.Errorf("unexpected slices: %+v", got)
	}
}

func TestAllocationEdgeCases(t *testing.T) {
	if got := Allocation(nil); got != nil {
		t.Errorf("nil assets: %v", got)
	}
	if got := Allocation([]portfolio.AssetRow{row("BTC", "Bitcoin", 0), row("ETH", "Ethereum", -5)}); got != nil {
		t.Errorf("non-positive values must be ignored: %v", got)
	}
	got := Allocation([]portfolio.AssetRow{row("BTC", "", 1000), row("ETH", "Ethereum", 0.1)})
	if len(got) != 2 || got[0].Name != "BTC" || got[1].PctText != "<1%" {
		t.Errorf("name falls back to symbol; tiny share shows <1%%: %+v", got)
	}
	// The input must not be reordered.
	in := []portfolio.AssetRow{row("ETH", "Ethereum", 1), row("BTC", "Bitcoin", 2)}
	_ = Allocation(in)
	if in[0].Symbol != "ETH" || in[1].Symbol != "BTC" {
		t.Errorf("Allocation mutated its input: %+v", in)
	}
}

func TestPluralAndComma(t *testing.T) {
	tests := []struct{ got, want string }{
		{Plural(1, "asset"), "1 asset"},
		{Plural(0, "asset"), "0 assets"},
		{Plural(7, "address"), "7 addresses"},
		{Comma(866412), "866,412"},
		{Comma(12), "12"},
		{Comma(-1234), "−1,234"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestSplitPositions(t *testing.T) {
	mk := func(sym, proto, pos string) portfolio.HoldingRow {
		return portfolio.HoldingRow{Holding: core.Holding{Symbol: sym, Protocol: proto, Position: pos}, ValueUSD: 10}
	}
	rows := []portfolio.HoldingRow{
		mk("SOL", "", ""), mk("SOL", "Orca", "SOL/USDC"), mk("USDC", "", ""), mk("USDC", "Orca", "SOL/USDC"), mk("JUP", "Jupiter", "locked"),
	}
	plain, groups := SplitPositions(rows)
	if len(plain) != 2 || len(groups) != 2 {
		t.Fatalf("plain=%d groups=%d", len(plain), len(groups))
	}
	if len(groups[0].Rows) != 2 || groups[0].ValueUSD != 20 || groups[0].Protocol != "Orca" {
		t.Errorf("group 0: %+v", groups[0])
	}
}

func TestStatusViewAndLabels(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	w := portfolio.WalletView{Status: portfolio.StatusStale, AsOf: now.Add(-3 * time.Hour), Kind: core.KindExchange, Type: "kraken"}
	if g, l, tone := StatusView(w, now); g != "◐" || l != "Stale 3h" || tone != "warn" {
		t.Errorf("stale: %q %q %q", g, l, tone)
	}
	w.Status = portfolio.StatusSynced
	if g, l, tone := StatusView(w, now); g != "●" || l != "Synced" || tone != "gain" {
		t.Errorf("synced: %q %q %q", g, l, tone)
	}
	w.Status = portfolio.StatusError
	if g, l, tone := StatusView(w, now); g != "✕" || l != "Error" || tone != "loss" {
		t.Errorf("error: %q %q %q", g, l, tone)
	}
	w.Status = portfolio.StatusPending
	if _, l, tone := StatusView(w, now); l != "Syncing…" || tone != "neutral" {
		t.Errorf("pending: %q %q", l, tone)
	}
	if got := KindLabel(w); got != "Exchange · API" {
		t.Errorf("KindLabel = %q", got)
	}
	if got := AccessLabel(w); got != "Read-only" {
		t.Errorf("AccessLabel = %q", got)
	}
	w.Kind, w.Type = core.KindHardware, "bitcoin"
	if KindLabel(w) != "Hardware · Bitcoin" || AccessLabel(w) != "Watch-only" || SourceChipText(w) != "Bitcoin · hardware" {
		t.Errorf("hardware labels: %q %q %q", KindLabel(w), AccessLabel(w), SourceChipText(w))
	}
	if got := WalletMeta(portfolio.WalletView{Kind: core.KindHardware, Rows: make([]portfolio.HoldingRow, 1)}); got != "1 asset · watch-only" {
		t.Errorf("WalletMeta = %q", got)
	}
	if g, l, tone := SyncView(SyncInfo{Status: portfolio.StatusError, Label: "2 sources failing"}); g != "✕" || l != "2 sources failing" || tone != "loss" {
		t.Errorf("SyncView: %q %q %q", g, l, tone)
	}
}
