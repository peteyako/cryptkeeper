package report

import (
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/tax"
)

func must8949(t *testing.T, r YearReport) []Form8949Part {
	t.Helper()
	parts, err := Form8949(r)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

func mustSchedD(t *testing.T, r YearReport) []ScheduleDLine {
	t.Helper()
	lines, err := ScheduleD(r)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func shortRow(wallet string, gain float64) tax.Disposal {
	return tax.Disposal{
		Sold: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), Acquired: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		Asset: "BTC", Amount: 1, CostUSD: 100, ProceedsUSD: 100 + gain, GainUSD: gain, Wallet: wallet, Term: tax.ShortTerm,
	}
}

// TestForm8949RefusesUnknownBox: a row in no digital-asset box, or in a box of the other
// part, is an error rather than a row silently left off the forms.
func TestForm8949RefusesUnknownBox(t *testing.T) {
	ok := Row{Disposal: shortRow("cb", 1), Box: "H"}
	if _, err := Form8949(YearReport{Year: 2026, Loc: time.UTC, Rows: []Row{ok}}); err != nil {
		t.Fatalf("valid row: %v", err)
	}
	for _, box := range []string{"h", "C", "", "J"} {
		bad := Row{Disposal: shortRow("cb", 1), Box: box}
		if _, err := Form8949(YearReport{Year: 2026, Loc: time.UTC, Rows: []Row{ok, bad}}); err == nil {
			t.Errorf("box %q accepted", box)
		}
		if _, err := ScheduleD(YearReport{Year: 2026, Loc: time.UTC, Rows: []Row{ok, bad}}); err == nil {
			t.Errorf("Schedule D with box %q accepted", box)
		}
	}
}

// TestBoxForUpperCasesOverride: a lower-case override still lands in its box.
func TestBoxForUpperCasesOverride(t *testing.T) {
	if got := BoxFor(shortRow("cb", 1), "", "", "h", false); got != "H" {
		t.Errorf("short box = %q, want H", got)
	}
	long := shortRow("cb", 1)
	long.Term = tax.LongTerm
	if got := BoxFor(long, "", "", "h", false); got != "K" {
		t.Errorf("long box = %q, want K", got)
	}
}

// TestTotalsMatchScheduleD: the summary totals are the Schedule D lines' cents, not the float
// sums of the rows.
func TestTotalsMatchScheduleD(t *testing.T) {
	var ds []tax.Disposal
	for range 3 {
		ds = append(ds, shortRow("w", 10.004)) // each prints 10.00; the float sum 30.012
	}
	r := FromOpen(2026, tax.Result{Disposals: ds}, Resolver{}, time.UTC, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	short, long, err := Totals(r)
	if err != nil {
		t.Fatal(err)
	}
	st, lt := ScheduleDNet(mustSchedD(t, r))
	if short.Gain != st || long.Gain != lt || short.Gain != 30 {
		t.Errorf("totals gain %v/%v, Schedule D %v/%v, want 30.00 short", short.Gain, long.Gain, st, lt)
	}
	if short.Proceeds != 330 || short.Cost != 300 { // each 110.004 prints 110.00
		t.Errorf("short proceeds %v cost %v", short.Proceeds, short.Cost)
	}
	if short.Count != 3 {
		t.Errorf("count %d", short.Count)
	}
}

// TestTurboTaxCSVGuardsSymbol: a symbol that starts like a formula is quoted out, and the
// fallback for a symbol-less mint is plain ASCII.
func TestTurboTaxCSVGuardsSymbol(t *testing.T) {
	evil := shortRow("w", 2)
	evil.Asset = "spl:EvilMint111111111111111111111111111111111"
	plain := shortRow("w", 2)
	plain.Asset = "spl:AbCdNoSymbol22222222222222222222222222WxYz"
	rs := Resolver{Symbol: func(k string) (string, string) {
		if k == evil.Asset {
			return `=HYPERLINK("http://x.invalid")`, "evil"
		}
		return "", ""
	}}
	r := FromOpen(2026, tax.Result{Disposals: []tax.Disposal{evil, plain}}, rs, time.UTC, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	var b strings.Builder
	if err := WriteTurboTaxCSV(&b, r); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "\n=") || strings.Contains(out, ",=") || !strings.Contains(out, `'=HYPERLINK`) {
		t.Errorf("formula not guarded:\n%s", out)
	}
	if !strings.Contains(out, "AbCd...WxYz") {
		t.Errorf("fallback symbol not ASCII:\n%s", out)
	}
	for _, c := range out {
		if c > 127 {
			t.Fatalf("non-ASCII %q in TurboTax CSV:\n%s", c, out)
		}
	}
}

// TestHoldingsTitleOpenYear: the open year's balances are as at now, not Dec 31.
func TestHoldingsTitleOpenYear(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 30, 0, 0, time.UTC)
	r := FromOpen(2026, tax.Result{}, Resolver{}, time.UTC, now)
	var b strings.Builder
	if err := WriteHoldings(&b, r); err != nil {
		t.Fatal(err)
	}
	if got := lines(b.String())[0]; got != "Balances as at 10/16/2026 12:30" {
		t.Errorf("title %q", got)
	}
	closed := FromClosed(tax.Year{Year: 2025, Source: "cryptkeeper"}, Resolver{}, time.UTC)
	b.Reset()
	_ = WriteHoldings(&b, closed)
	if got := lines(b.String())[0]; got != "Balances as at 12/31/2025 23:59" {
		t.Errorf("closed title %q", got)
	}
}

// TestFromOpenRangeKeepsWindow: the first open year after a close in another zone keeps the
// rows from the close's boundary, not from Jan 1 in the current zone.
func TestFromOpenRangeKeepsWindow(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC) // 2026-12-31 19:00 in New York
	early := shortRow("w", 5)
	early.Sold = time.Date(2027, 1, 1, 2, 0, 0, 0, time.UTC)
	before := shortRow("w", 7)
	before.Sold = time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)
	res := tax.Result{Disposals: []tax.Disposal{before, early}, Income: []tax.Income{{Date: early.Sold, Asset: "SOL", Amount: 1, ValueUSD: 3, Wallet: "w", Type: "reward"}}}
	r := FromOpenRange(2027, res, Resolver{}, ny, time.Date(2027, 6, 1, 0, 0, 0, 0, ny), from, time.Date(2028, 1, 1, 0, 0, 0, 0, ny))
	if len(r.Rows) != 1 || r.Rows[0].GainUSD != 5 || len(r.Income) != 1 {
		t.Errorf("rows %+v income %+v, want the one after the boundary", r.Rows, r.Income)
	}
}
