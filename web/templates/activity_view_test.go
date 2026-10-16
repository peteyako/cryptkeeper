package templates

import (
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

func usd(v float64) *float64 { return &v }

func lg(sym string, amount float64) core.Leg {
	return core.Leg{AssetKey: sym, Symbol: sym, Amount: amount}
}

func TestTxTitle(t *testing.T) {
	tests := []struct {
		name string
		row  ActivityRow
		want string
	}{
		{"receive", ActivityRow{Tx: core.Tx{Kind: core.TxReceive, Legs: []core.Leg{lg("BTC", 0.05)}}}, "Received BTC"},
		{"receive from address", ActivityRow{Tx: core.Tx{Kind: core.TxReceive, Legs: []core.Leg{lg("BTC", 0.05)}, Counterparty: "bc1qx2k9d7p4m8w3vz6c5n0r1t8y2u4h7j9m8rt"}}, "Received BTC from bc1qx2…m8rt"},
		{"receive from a name", ActivityRow{Tx: core.Tx{Kind: core.TxReceive, Legs: []core.Leg{lg("ETH", 1)}, Counterparty: "Coinbase"}}, "Received ETH from Coinbase"},
		{"receive without legs", ActivityRow{Tx: core.Tx{Kind: core.TxReceive}}, "Received"},
		{"send", ActivityRow{Tx: core.Tx{Kind: core.TxSend, Legs: []core.Leg{lg("BTC", -0.02)}}}, "Sent BTC"},
		{"send to address", ActivityRow{Tx: core.Tx{Kind: core.TxSend, Legs: []core.Leg{lg("SOL", -5)}, Counterparty: "9wQmRk2t7VdC5nXy3hYfPzE8uLb4aJ6sKq1gT3xNvB2e"}}, "Sent SOL to 9wQmRk…vB2e"},
		{"transfer out", ActivityRow{SourceLabel: "Coinbase", PeerLabel: "Cold storage", Tx: core.Tx{Kind: core.TxTransfer, Legs: []core.Leg{lg("BTC", -0.05)}}}, "Transfer · Coinbase → Cold storage"},
		{"transfer in says the same", ActivityRow{SourceLabel: "Cold storage", PeerLabel: "Coinbase", Tx: core.Tx{Kind: core.TxTransfer, Legs: []core.Leg{lg("BTC", 0.05)}}}, "Transfer · Coinbase → Cold storage"},
		{"transfer without a peer", ActivityRow{SourceLabel: "Cold storage", Tx: core.Tx{Kind: core.TxTransfer, Legs: []core.Leg{lg("BTC", 0.05)}}}, "Transfer BTC"},
		{"swap", ActivityRow{Tx: core.Tx{Kind: core.TxSwap, Legs: []core.Leg{lg("SOL", -2), lg("USDC", 296)}}}, "Swapped SOL → USDC"},
		{"swap with three assets", ActivityRow{Tx: core.Tx{Kind: core.TxSwap, Legs: []core.Leg{lg("SOL", -2), lg("JUP", -3), lg("BONK", -4), lg("USDC", 296)}}}, "Swapped SOL + JUP +1 → USDC"},
		{"buy", ActivityRow{Tx: core.Tx{Kind: core.TxTrade, Legs: []core.Leg{lg("BTC", 0.005), lg("USD", -320)}}}, "Bought BTC"},
		{"sell for a stablecoin", ActivityRow{Tx: core.Tx{Kind: core.TxTrade, Legs: []core.Leg{lg("ETH", -0.5), lg("USDC", 1582)}}}, "Sold ETH"},
		{"sell for euros", ActivityRow{Tx: core.Tx{Kind: core.TxTrade, Legs: []core.Leg{lg("DOT", -60), lg("EUR", 380)}}}, "Sold DOT"},
		{"crypto to crypto trade", ActivityRow{Tx: core.Tx{Kind: core.TxTrade, Legs: []core.Leg{lg("ETH", -1), lg("BTC", 0.05)}}}, "Traded ETH → BTC"},
		{"reward", ActivityRow{Tx: core.Tx{Kind: core.TxReward, Legs: []core.Leg{lg("SOL", 0.03)}}}, "Staking reward"},
		{"stake", ActivityRow{Tx: core.Tx{Kind: core.TxStake, Legs: []core.Leg{lg("SOL", -10)}}}, "Staked SOL"},
		{"unstake", ActivityRow{Tx: core.Tx{Kind: core.TxStake, Legs: []core.Leg{lg("SOL", 10)}}}, "Unstaked SOL"},
		{"fee", ActivityRow{Tx: core.Tx{Kind: core.TxFee, Legs: []core.Leg{lg("USD", -1.99)}}}, "Fee USD"},
		{"other with a note", ActivityRow{Tx: core.Tx{Kind: core.TxOther, Note: "Program interaction"}}, "Program interaction"},
		{"other", ActivityRow{Tx: core.Tx{Kind: core.TxOther, Legs: []core.Leg{lg("SOL", 1)}}}, "Activity SOL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TxTitle(tc.row); got != tc.want {
				t.Errorf("TxTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTxGlyph(t *testing.T) {
	want := map[core.TxKind][2]string{
		core.TxReceive: {"↓", "gain"}, core.TxSend: {"↑", "loss"}, core.TxTransfer: {"⇄", "accent"},
		core.TxSwap: {"⇄", "accent"}, core.TxTrade: {"⇄", "accent"}, core.TxReward: {"★", "gain"},
		core.TxFee: {Minus, "neutral"}, core.TxOther: {"•", "neutral"},
	}
	for kind, w := range want {
		if g, tone := TxGlyph(core.Tx{Kind: kind}); g != w[0] || tone != w[1] {
			t.Errorf("%s: glyph %q tone %q, want %q %q", kind, g, tone, w[0], w[1])
		}
	}
}

func TestTxStatusView(t *testing.T) {
	tests := []struct {
		tx                 core.Tx
		glyph, label, tone string
	}{
		{core.Tx{Status: core.TxPending, Confirmations: 2}, "◐", "2 conf", "warn"},
		{core.Tx{Status: core.TxPending}, "◐", "Pending", "warn"},
		{core.Tx{Status: core.TxFailed}, "✕", "Failed", "loss"},
		{core.Tx{Status: core.TxConfirmed, Confirmations: 40}, "", "Confirmed", "neutral"},
		{core.Tx{}, "", "Confirmed", "neutral"},
	}
	for _, tc := range tests {
		if g, l, tone := TxStatusView(tc.tx); g != tc.glyph || l != tc.label || tone != tc.tone {
			t.Errorf("%+v: got %q %q %q", tc.tx, g, l, tone)
		}
	}
}

func TestLegText(t *testing.T) {
	if got := LegText(lg("BTC", 0.05)); got != "+0.0500 BTC" {
		t.Errorf("positive: %q", got)
	}
	if got := LegText(lg("BTC", -0.02)); got != "−0.0200 BTC" {
		t.Errorf("negative must use the true minus: %q", got)
	}
	if got := LegText(lg("USDC", 296.12)); got != "+296.12 USDC" {
		t.Errorf("stablecoin: %q", got)
	}
	if got := LegMask(lg("BTC", 0.05)); got != "+•••• BTC" {
		t.Errorf("mask: %q", got)
	}
	if got := LegMask(lg("BTC", -1)); got != "−•••• BTC" {
		t.Errorf("negative mask: %q", got)
	}
}

func TestTxFiat(t *testing.T) {
	one := core.Tx{Legs: []core.Leg{{Symbol: "BTC", Amount: 1, USD: usd(100)}}}
	if v, ok := TxFiat(one); !ok || v != 100 {
		t.Errorf("one leg: %v %v", v, ok)
	}
	out := core.Tx{Legs: []core.Leg{{Symbol: "BTC", Amount: -1, USD: usd(-100)}}}
	if v, ok := TxFiat(out); !ok || v != 100 {
		t.Errorf("a send shows the unsigned value: %v %v", v, ok)
	}
	swap := core.Tx{Legs: []core.Leg{{Symbol: "SOL", Amount: -2, USD: usd(-296.44)}, {Symbol: "USDC", Amount: 296.12, USD: usd(296.12)}}}
	if v, ok := TxFiat(swap); !ok || v != 296.44 {
		t.Errorf("a swap shows the larger side: %v %v", v, ok)
	}
	if _, ok := TxFiat(core.Tx{Legs: []core.Leg{{Symbol: "X", Amount: 1}}}); ok {
		t.Error("no value known must report not ok")
	}
	if _, ok := TxFiat(core.Tx{}); ok {
		t.Error("no legs must report not ok")
	}
}

func TestFeeLegAndHash(t *testing.T) {
	if _, ok := FeeLeg(core.Tx{}); ok {
		t.Error("no fee")
	}
	l, ok := FeeLeg(core.Tx{Fee: &core.Leg{Symbol: "BTC", Amount: -0.0000184}})
	if !ok || l.Amount != 0.0000184 {
		t.Errorf("fee leg must be positive: %+v %v", l, ok)
	}
	if got := Coin(l.Amount, l.Symbol); got != "0.0000184 BTC" {
		t.Errorf("fee text: %q", got)
	}
	if got := TxHash(core.Tx{TxHash: "abc", ExternalID: "ext"}); got != "abc" {
		t.Errorf("hash: %q", got)
	}
	if got := TxHash(core.Tx{ExternalID: "ext", ExplorerURL: "https://x.test/tx/ext"}); got != "ext" {
		t.Errorf("an explorer link falls back to the external id: %q", got)
	}
	if got := TxHash(core.Tx{ExternalID: "ext"}); got != "" {
		t.Errorf("an exchange entry has no hash to show: %q", got)
	}
	if SafeExplorerURL("http://x.test") != "" || SafeExplorerURL("javascript:alert(1)") != "" || SafeExplorerURL("https://mempool.space/tx/ab") == "" {
		t.Error("only https explorer links are kept")
	}
}

func TestTxDateAndClock(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 14, 14, 32, 0, 0, time.UTC)
	if got := TxWhen(at, now); got != "Oct 14, 14:32" {
		t.Errorf("same year: %q", got)
	}
	old := time.Date(2025, 12, 3, 9, 5, 0, 0, time.UTC)
	if got := TxWhen(old, now); got != "Dec 3, 2025, 09:05" {
		t.Errorf("other year: %q", got)
	}
	if TxWhen(time.Time{}, now) != "" {
		t.Error("zero time")
	}
}

func TestActivityFilterOptions(t *testing.T) {
	p := ActivityPage{
		Sources: []WalletOption{{ID: "cold", Label: "Cold storage"}},
		Assets:  []AssetOption{{Key: "BTC", Symbol: "BTC", Name: "Bitcoin"}, {Key: "USDC", Symbol: "USDC", Name: "USDC"}},
	}
	if got := p.sourceOpts(); len(got) != 2 || got[0].Value != "" || got[1].Value != "cold" {
		t.Errorf("source options: %+v", got)
	}
	if got := p.assetOpts(); got[1].Label != "BTC · Bitcoin" || got[2].Label != "USDC" {
		t.Errorf("asset options: %+v", got)
	}
	// A filter that is no longer an option still shows up as selected.
	if got := withCurrent(p.sourceOpts(), "gone"); got[len(got)-1].Value != "gone" {
		t.Errorf("withCurrent: %+v", got)
	}
	if got := withCurrent(p.sourceOpts(), "cold"); len(got) != 2 {
		t.Errorf("withCurrent must not duplicate: %+v", got)
	}
	if p.activityFiltered() || !(ActivityPage{Kind: "send"}).activityFiltered() {
		t.Error("activityFiltered")
	}
	if len(actKinds) != 9 || actKinds[0].Label != "All kinds" {
		t.Errorf("kinds: %+v", actKinds)
	}
}

func TestSyncText(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if syncEntries(1) != "1 entry" || syncEntries(1204) != "1,204 entries" || syncEntries(0) != "0 entries" {
		t.Error("entries")
	}
	if got := syncWhen(SyncRow{LastSync: now.Add(-2 * time.Minute)}, now); got != "Synced 2m ago" {
		t.Errorf("when: %q", got)
	}
	if got := syncWhen(SyncRow{}, now); got != "Not synced yet" {
		t.Errorf("never: %q", got)
	}
	if activityViewAll("a b&c") != "/activity?source=a+b%26c" {
		t.Errorf("view all link: %s", activityViewAll("a b&c"))
	}
}

func TestCoverageText(t *testing.T) {
	for in, want := range map[float64]string{
		0: "No cost basis known yet", 0.001: "Basis known for <1% of value", 0.82: "Basis known for 82% of value",
		0.999: "Basis known for all of the value", 1: "Basis known for all of the value",
	} {
		if got := CoverageText(in); got != want {
			t.Errorf("CoverageText(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestAlertFormValues(t *testing.T) {
	p := AlertFormPage{Values: map[string]string{}}
	if p.ruleType() != "balance_drop" || p.cooldown() != "6h" || p.window() != "24h" || p.portfolio() != "all" || !p.enabled() {
		t.Error("a new rule defaults to a balance drop, 6h, 24h, all holdings and enabled")
	}
	p.EditID = "x"
	if p.enabled() {
		t.Error("an edited rule with no enabled value is switched off")
	}
	p.Values["enabled"] = "on"
	if !p.enabled() {
		t.Error("enabled=on")
	}
	p.Values["enabled"] = "false"
	if p.enabled() {
		t.Error("enabled=false")
	}
	p.Values["type"] = "bogus"
	if p.ruleType() != "balance_drop" {
		t.Error("an unknown type falls back to the first")
	}
	p.Values["type"] = "price"
	if !p.shows("price") || p.shows("source_down") || !p.shows("source_down", "price") {
		t.Error("shows")
	}
	if got := showExpr("price", "large_activity"); got != "$ruleType === 'price' || $ruleType === 'large_activity'" {
		t.Errorf("showExpr = %s", got)
	}
	if got := hideExpr("price"); got != "!($ruleType === 'price')" {
		t.Errorf("hideExpr = %s", got)
	}
	opts := p.portfolioOpts()
	if opts[0].Value != "all" {
		t.Errorf("all holdings is always an option: %+v", opts)
	}
	p.Values["portfolio"] = "gone"
	if opts := p.portfolioOpts(); opts[len(opts)-1].Value != "gone" {
		t.Errorf("a portfolio that no longer exists stays selectable: %+v", opts)
	}
	for typ, label := range map[string]string{"balance_drop": "Balance drop", "price": "Price", "portfolio_move": "Portfolio move", "source_down": "Source down", "large_activity": "Large send", "x": "x"} {
		if AlertTypeLabel(typ) != label {
			t.Errorf("AlertTypeLabel(%s) = %s", typ, AlertTypeLabel(typ))
		}
	}
	if ruleName(AlertRuleRow{Type: "price"}) != "Price" || ruleName(AlertRuleRow{Name: "Mine", Type: "price"}) != "Mine" {
		t.Error("ruleName")
	}
}

func TestPeriodOptionsAndExportURL(t *testing.T) {
	p := ActivityPage{Years: []int{2026, 2025}, Kind: "send", Period: "y2025"}
	opts := p.periodOpts()
	var vals []string
	for _, o := range opts {
		vals = append(vals, o.Value)
	}
	want := []string{"", "7d", "30d", "90d", "mtd", "ytd", "y2026", "y2025", "custom"}
	if strings.Join(vals, ",") != strings.Join(want, ",") {
		t.Fatalf("period options %v", vals)
	}
	if got := actExportURL(p); got != "/export/activity.csv?kind=send&range=y2025" {
		t.Fatalf("export url %q", got)
	}
	if got := actExportURL(ActivityPage{}); got != "/export/activity.csv" {
		t.Fatalf("unfiltered export url %q", got)
	}
	if !p.activityFiltered() || (ActivityPage{}).activityFiltered() {
		t.Fatal("a period counts as a filter")
	}
}
