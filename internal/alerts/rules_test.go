package alerts

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseGoodFile(t *testing.T) {
	f, err := Parse(readTestdata(t, "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != 1 || len(f.Rules) != 6 {
		t.Fatalf("version %d, %d rules", f.Version, len(f.Rules))
	}
	cold := f.Rules[0]
	if cold.ID != "cold-drop" || cold.Type != TypeBalanceDrop || !reflect.DeepEqual(cold.Wallets, []string{"cold", "vault"}) || cold.MinPct != 0 {
		t.Fatalf("cold-drop: %+v", cold)
	}
	if r := f.Rules[1]; r.Type != TypePrice || r.Asset != "BTC" || r.Above != 100000 || r.Below != 0 {
		t.Fatalf("btc-100k: %+v", r)
	}
	if r := f.Rules[2]; r.Below != 80 || r.Name != "SOL floor" || r.CooldownDuration() != time.Hour {
		t.Fatalf("sol-low: %+v", r)
	}
	if r := f.Rules[3]; r.Portfolio != "all" || r.Pct != 10 || r.WindowDuration() != 24*time.Hour {
		t.Fatalf("lt-move: %+v", r)
	}
	if r := f.Rules[4]; r.IsEnabled() || r.Minutes != 60 {
		t.Fatalf("src-down: %+v", r)
	}
	if r := f.Rules[5]; !r.IsEnabled() || r.MinUsd != 1000 || r.CooldownDuration() != 30*time.Minute {
		t.Fatalf("big-send: %+v", r)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	f, err := Parse(readTestdata(t, "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(f, g) {
		t.Fatalf("round trip differs:\n%+v\n%+v\n%s", f, g, out)
	}
	out2, _ := Encode(g)
	if string(out) != string(out2) {
		t.Fatalf("encoding not stable:\n%s\n%s", out, out2)
	}
	if !strings.Contains(string(out), "enabled: false") {
		t.Fatalf("disabled flag lost:\n%s", out)
	}
}

func TestParseEmptyAndVersion(t *testing.T) {
	f, err := Parse(nil)
	if err != nil || f.Version != 1 || len(f.Rules) != 0 {
		t.Fatalf("empty: %+v %v", f, err)
	}
	f, err = Parse([]byte("rules: []\n"))
	if err != nil || f.Version != 1 {
		t.Fatalf("version 0 should become 1: %+v %v", f, err)
	}
	if _, err := Parse([]byte("version: 2\nrules: []\n")); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version error, got %v", err)
	}
}

func TestParseTypoHasLineNumber(t *testing.T) {
	_, err := Parse(readTestdata(t, "typo.yaml"))
	if err == nil || !strings.Contains(err.Error(), "line 6") || !strings.Contains(err.Error(), "abov") {
		t.Fatalf("want a line-numbered error naming the typo, got %v", err)
	}
	if strings.Contains(err.Error(), "alerts.Rule") {
		t.Fatalf("internal type name leaked: %v", err)
	}
	_, err = Parse([]byte("version: 1\nrule: []\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want line 2 error, got %v", err)
	}
	if _, err := Parse([]byte("rules: [unclosed")); err == nil {
		t.Fatal("want syntax error")
	}
}

func TestParseBadDurationHasLineNumber(t *testing.T) {
	_, err := Parse(readTestdata(t, "baddur.yaml"))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"line 6", "lt-move", "window", "7days"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	_, err = Parse([]byte("rules:\n  - {id: a, type: source_down, minutes: 5, cooldown: 6hrs}\n"))
	if err == nil || !strings.Contains(err.Error(), "cooldown") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want cooldown error on line 2, got %v", err)
	}
}

func TestParseDuplicateIDs(t *testing.T) {
	_, err := Parse([]byte("rules:\n  - {id: a, type: source_down, minutes: 5}\n  - {id: a, type: large_activity}\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("want duplicate id error on line 3, got %v", err)
	}
}

func TestParseValidation(t *testing.T) {
	cases := []struct{ name, rule, want string }{
		{"missing id", `{type: source_down, minutes: 5}`, "id is required"},
		{"bad id", `{id: Bad_ID, type: source_down, minutes: 5}`, "id"},
		{"id too long", `{id: ` + strings.Repeat("a", 41) + `, type: source_down, minutes: 5}`, "id"},
		{"missing type", `{id: a}`, "type is required"},
		{"unknown type", `{id: a, type: moon_phase}`, "unknown type"},
		{"price needs asset", `{id: a, type: price, above: 5}`, "asset"},
		{"price needs a threshold", `{id: a, type: price, asset: BTC}`, "above or below"},
		{"price not both", `{id: a, type: price, asset: BTC, above: 5, below: 2}`, "not both"},
		{"price negative", `{id: a, type: price, asset: BTC, above: -5}`, "greater than 0"},
		{"balance minPct high", `{id: a, type: balance_drop, minPct: 150}`, "minPct"},
		{"balance minPct negative", `{id: a, type: balance_drop, minPct: -1}`, "minPct"},
		{"balance empty wallets", `{id: a, type: balance_drop, wallets: []}`, "wallets"},
		{"move needs pct", `{id: a, type: portfolio_move, window: 24h}`, "pct"},
		{"move bad portfolio", `{id: a, type: portfolio_move, pct: 5, portfolio: "Not Ok"}`, "portfolio"},
		{"move zero window", `{id: a, type: portfolio_move, pct: 5, window: 0s}`, "window"},
		{"down needs minutes", `{id: a, type: source_down}`, "minutes"},
		{"large negative min", `{id: a, type: large_activity, minUsd: -5}`, "minUsd"},
		{"negative cooldown", `{id: a, type: large_activity, cooldown: -1h}`, "cooldown"},
		{"misplaced field", `{id: a, type: balance_drop, above: 5}`, `"above" does not apply`},
		{"misplaced asset", `{id: a, type: source_down, minutes: 5, asset: BTC}`, `"asset" does not apply`},
		{"nan", `{id: a, type: price, asset: BTC, above: .nan}`, "above"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("version: 1\nrules:\n  - " + tc.rule + "\n"))
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q lacks %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "line 3") {
				t.Fatalf("error %q lacks the rule's line number", err)
			}
		})
	}
}

func TestParseNormalizesAndDefaults(t *testing.T) {
	f, err := Parse([]byte(`rules:
  - {id: p, type: PRICE, asset: " btc ", above: 5, enabled: true}
  - {id: s, type: price, asset: "spl:AbCd", below: 5}
  - {id: m, type: portfolio_move, pct: 5}
  - {id: w, type: balance_drop, wallets: [" cold ", cold, ""]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if r := f.Rules[0]; r.Type != TypePrice || r.Asset != "BTC" || r.Enabled != nil {
		t.Fatalf("price rule not normalized: %+v", r)
	}
	if r := f.Rules[1]; r.Asset != "spl:AbCd" {
		t.Fatalf("spl key case must be kept: %+v", r)
	}
	if r := f.Rules[2]; r.Portfolio != "all" || r.Window != "24h" {
		t.Fatalf("move defaults: %+v", r)
	}
	if r := f.Rules[3]; !reflect.DeepEqual(r.Wallets, []string{"cold"}) {
		t.Fatalf("wallets: %+v", r.Wallets)
	}
}

func TestCooldownDefaults(t *testing.T) {
	if got := (Rule{}).CooldownDuration(); got != 6*time.Hour {
		t.Fatalf("default cooldown %v", got)
	}
	if got := (Rule{Cooldown: "0s"}).CooldownDuration(); got != 0 {
		t.Fatalf("explicit zero cooldown %v", got)
	}
	if got := (Rule{Cooldown: "90m"}).CooldownDuration(); got != 90*time.Minute {
		t.Fatalf("90m cooldown %v", got)
	}
	if got := (Rule{}).WindowDuration(); got != 24*time.Hour {
		t.Fatalf("default window %v", got)
	}
}

// TestNormalizeAssetKeys: symbols are upper-cased; mints keep their case; stock keys keep
// their lower-case prefix, so a price alert on a ticker matches its holding.
func TestNormalizeAssetKeys(t *testing.T) {
	for in, want := range map[string]string{
		" btc ":     "BTC",
		"spl:JUPyi": "spl:JUPyi",
		"stock:vti": "stock:VTI",
		"STOCK:BND": "stock:BND",
	} {
		if got := normalizeAsset(in); got != want {
			t.Errorf("normalizeAsset(%q) = %q, want %q", in, got, want)
		}
	}
}
