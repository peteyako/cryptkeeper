package report

import (
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/tax"
)

func btcReport(t *testing.T) YearReport { return sampleGains(t) }

func TestTurboTaxHeaderAndRow(t *testing.T) {
	var b strings.Builder
	if err := WriteTurboTaxCSV(&b, btcReport(t)); err != nil {
		t.Fatal(err)
	}
	l := lines(b.String())
	if l[0] != "Currency Name,Purchase Date,Cost Basis,Date Sold,Proceeds" {
		t.Errorf("header %q", l[0])
	}
	if l[1] != "BTC,VARIOUS,1000.00,03/02/2026,1500.00" || len(l) != 2 {
		t.Errorf("rows %q", l)
	}
}

func TestTurboTaxExcludesGiftsLost(t *testing.T) {
	r := btcReport(t)
	r.GiftsLost = r.Rows
	r.Rows = nil
	var b strings.Builder
	_ = WriteTurboTaxCSV(&b, r)
	if len(lines(b.String())) != 1 {
		t.Errorf("got %q", b.String())
	}
}

func TestTXFRecord(t *testing.T) {
	r := btcReport(t)
	r.Rows[0].Box = "L"
	var b strings.Builder
	today := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	if err := WriteTXF(&b, r, today); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if !strings.HasPrefix(s, "V042\nACryptKeeper\nD10/16/2026\n^\n") {
		t.Errorf("header %q", s)
	}
	rec := "TD\nN714\nC1\nL1\nP0.5 BTC\nDVARIOUS\nD03/02/2026\n$1000.00\n$1500.00\n^\n"
	if !strings.HasSuffix(s, rec) {
		t.Errorf("record %q", s)
	}
}

func TestTXFCodes(t *testing.T) {
	cases := map[string]string{"G": "321", "H": "711", "I": "712", "J": "323", "K": "713", "L": "714"}
	for box, code := range cases {
		r := btcReport(t)
		r.Rows[0].Box = box
		var b strings.Builder
		_ = WriteTXF(&b, r, time.Now())
		if !strings.Contains(b.String(), "TD\nN"+code+"\n") {
			t.Errorf("box %s want %s: %q", box, code, b.String())
		}
	}
}

func rowsIn(box string, n int, gain float64) []Row {
	rows := make([]Row, n)
	term := tax.ShortTerm
	if box >= "J" {
		term = tax.LongTerm
	}
	for i := range rows {
		rows[i] = Row{Box: box, Symbol: "SOL", Disposal: tax.Disposal{
			Amount: 1, ProceedsUSD: 10 + gain, CostUSD: 10, GainUSD: gain, Term: term,
			Sold: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), Acquired: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		}}
	}
	return rows
}

func TestForm8949Grouping(t *testing.T) {
	r := YearReport{Year: 2026, Loc: time.UTC, Rows: append(rowsIn("L", 1, 7), rowsIn("I", 2, 3)...)}
	parts := must8949(t, r)
	if len(parts) != 2 || parts[0].Box != "I" || parts[1].Box != "L" || parts[0].Long || !parts[1].Long {
		t.Fatalf("parts %+v", parts)
	}
	if parts[0].Total.Gain != 6 || parts[0].Total.Proceeds != 26 || parts[0].Total.Cost != 20 || len(parts[0].Rows) != 2 {
		t.Errorf("I total %+v", parts[0].Total)
	}
	row := parts[0].Rows[0]
	if row.Description != "1 SOL" || row.Acquired != "01/02/2026" || row.Sold != "03/02/2026" {
		t.Errorf("row %+v", row)
	}
}

func TestEmpty8949(t *testing.T) {
	if p := must8949(t, YearReport{Year: 2026}); len(p) != 0 {
		t.Errorf("got %v", p)
	}
}

func TestPages11(t *testing.T) {
	rows := rowsIn("I", 30, 1)
	rows[29].GainUSD, rows[29].ProceedsUSD = -0.09, 9.91
	parts := must8949(t, YearReport{Year: 2026, Loc: time.UTC, Rows: rows})
	pages := Pages(parts[0], 11)
	if len(pages) != 3 || len(pages[0]) != 11 || len(pages[1]) != 11 || len(pages[2]) != 8 {
		t.Fatalf("pages %d", len(pages))
	}
	if tot := PageTotal(pages[0]); tot.Gain != 11 || tot.Cost != 110 || tot.Proceeds != 121 {
		t.Errorf("tot %+v", tot)
	}
	if got := Money8949(pages[2][7].Gain); got != "(0.09)" {
		t.Errorf("loss %q", got)
	}
	if Money8949(1234.5) != "1234.50" || Money8949(-0.001) != "0.00" {
		t.Errorf("money")
	}
	if len(Pages(Form8949Part{}, 11)) != 0 {
		t.Errorf("empty part has pages")
	}
}

func TestScheduleDLines(t *testing.T) {
	y := tax.Year{Year: 2025, Disposals: []tax.Disposal{
		{Wallet: "w", Term: tax.ShortTerm, ProceedsUSD: 20, CostUSD: 10, GainUSD: 10},
		{Wallet: "w", Term: tax.ShortTerm, ProceedsUSD: 6, CostUSD: 10, GainUSD: -4},
	}}
	lines := mustSchedD(t, FromClosed(y, Resolver{}, time.UTC))
	if len(lines) != 6 {
		t.Fatalf("lines %+v", lines)
	}
	want := []string{"1b", "2", "3", "8b", "9", "10"}
	for i, l := range lines {
		if l.Line != want[i] {
			t.Errorf("line %d = %s", i, l.Line)
		}
	}
	if l := lines[2]; l.Box != "I" || l.Gain != 6 || l.Proceeds != 26 || l.Cost != 20 || l.Adjustments != 0 {
		t.Errorf("line 3 %+v", l)
	}
	if lines[0] != (ScheduleDLine{Line: "1b", Box: "G"}) {
		t.Errorf("1b %+v", lines[0])
	}
}

func TestKoinlyYearBoxes(t *testing.T) {
	y := tax.Year{Year: 2025, Source: "koinly", Disposals: []tax.Disposal{
		{Wallet: "Kraken", Term: tax.ShortTerm, Acquired: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)},
		{Wallet: "Solana (SOL) - Main", Term: tax.LongTerm},
	}}
	r := FromClosed(y, Resolver{}, time.UTC)
	if r.Rows[0].Box != "H" || r.Rows[1].Box != "L" {
		t.Errorf("boxes %s %s", r.Rows[0].Box, r.Rows[1].Box)
	}
}

// TestForm8949Cents: (d) and (e) are rounded to cents per row and (h) = d − e, and the
// TurboTax CSV uses the same rounded amounts.
func TestForm8949Cents(t *testing.T) {
	rows := rowsIn("I", 1, 0)
	rows[0].ProceedsUSD, rows[0].CostUSD, rows[0].GainUSD = 10.006, 5.004, 5.002
	r := YearReport{Year: 2026, Loc: time.UTC, Rows: rows}
	row := must8949(t, r)[0].Rows[0]
	if Money8949(row.Proceeds) != "10.01" || Money8949(row.Cost) != "5.00" || Money8949(row.Gain) != "5.01" {
		t.Errorf("row d/e/h = %s/%s/%s, want 10.01/5.00/5.01",
			Money8949(row.Proceeds), Money8949(row.Cost), Money8949(row.Gain))
	}
	if row.Symbol != "SOL" {
		t.Errorf("symbol %q", row.Symbol)
	}
	var b strings.Builder
	if err := WriteTurboTaxCSV(&b, r); err != nil {
		t.Fatal(err)
	}
	if l := lines(b.String()); l[1] != "SOL,01/02/2026,5.00,03/02/2026,10.01" {
		t.Errorf("turbotax row %q", l[1])
	}
}

// TestForm8949TotalsAddUp: page totals, the part total and the Schedule D line are sums of
// the printed (rounded) rows, not of the unrounded amounts.
func TestForm8949TotalsAddUp(t *testing.T) {
	rows := rowsIn("I", 12, 0)
	for i := range rows {
		rows[i].ProceedsUSD, rows[i].CostUSD, rows[i].GainUSD = 10.004, 0.004, 10
	}
	r := YearReport{Year: 2026, Loc: time.UTC, Rows: rows}
	p := must8949(t, r)[0]
	for _, row := range p.Rows {
		if Money8949(row.Proceeds) != "10.00" || Money8949(row.Cost) != "0.00" || Money8949(row.Gain) != "10.00" {
			t.Fatalf("row %+v", row)
		}
	}
	pages := Pages(p, 11)
	if tot := PageTotal(pages[0]); Money8949(tot.Proceeds) != "110.00" || Money8949(tot.Gain) != "110.00" || Money8949(tot.Cost) != "0.00" {
		t.Errorf("page total %+v, want 110.00 (raw sum 110.044)", tot)
	}
	if Money8949(p.Total.Proceeds) != "120.00" {
		t.Errorf("part total %+v", p.Total)
	}
	l3 := mustSchedD(t, r)[2]
	if Money8949(l3.Proceeds) != "120.00" || Money8949(l3.Cost) != "0.00" || Money8949(l3.Gain) != "120.00" {
		t.Errorf("line 3 %+v, want 120.00/0.00/120.00", l3)
	}
}

func TestScheduleDNet(t *testing.T) {
	var rows []Row
	for _, box := range []string{"G", "H", "I", "J", "K", "L"} {
		rs := rowsIn(box, 1, 0)
		rs[0].ProceedsUSD, rs[0].CostUSD = 1.004, 0 // 1.00 each when printed
		rows = append(rows, rs...)
	}
	rows[2].ProceedsUSD, rows[2].CostUSD = 0, 2.006 // box I: (2.01)
	short, long := ScheduleDNet(mustSchedD(t, YearReport{Year: 2026, Loc: time.UTC, Rows: rows}))
	if Money8949(short) != "(0.01)" || Money8949(long) != "3.00" {
		t.Errorf("net short %s long %s, want (0.01) and 3.00", Money8949(short), Money8949(long))
	}
}
