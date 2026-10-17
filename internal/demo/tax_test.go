package demo

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"

	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/tax/report"
	"crypto-portfolio/web/templates"
)

func TestDemoTaxConsistent(t *testing.T) {
	for _, year := range []int{2025, 2026} {
		r := YearReport(year)
		if len(r.Rows) == 0 {
			t.Fatalf("%d: no rows", year)
		}
		var short, long tax.Summary
		var boxSum, boxN float64
		for _, row := range r.Rows {
			if row.Term == tax.LongTerm {
				long.LongGain += row.GainUSD
				long.LongProceeds += row.ProceedsUSD
				long.LongCost += row.CostUSD
			} else {
				short.ShortGain += row.GainUSD
				short.ShortProceeds += row.ProceedsUSD
				short.ShortCost += row.CostUSD
			}
		}
		s := r.Summary
		for name, p := range map[string][2]float64{
			"short gain": {short.ShortGain, s.ShortGain}, "short proceeds": {short.ShortProceeds, s.ShortProceeds},
			"short cost": {short.ShortCost, s.ShortCost}, "long gain": {long.LongGain, s.LongGain},
			"long proceeds": {long.LongProceeds, s.LongProceeds}, "long cost": {long.LongCost, s.LongCost},
		} {
			if math.Abs(p[0]-p[1]) > 0.005 {
				t.Errorf("%d %s: rows %v, summary %v", year, name, p[0], p[1])
			}
		}
		var gain float64
		for _, row := range r.Rows {
			gain += row.GainUSD
		}
		parts, err := report.Form8949(r)
		if err != nil {
			t.Fatalf("%d: %v", year, err)
		}
		var bgain, bproc, bcost float64
		for _, p := range parts {
			bgain += p.Total.Gain
			bproc += p.Total.Proceeds
			bcost += p.Total.Cost
			boxN += float64(p.Total.Count)
		}
		var proc, cost float64
		for _, row := range r.Rows {
			proc += row.ProceedsUSD
			cost += row.CostUSD
		}
		boxSum = bgain
		tol := 0.01 * float64(len(r.Rows)) // the forms round each row to cents
		if math.Abs(boxSum-gain) > tol || math.Abs(bproc-proc) > tol || math.Abs(bcost-cost) > tol || int(boxN) != len(r.Rows) {
			t.Errorf("%d: boxes %v/%v/%v (%v rows) != rows %v/%v/%v (%d)", year, bgain, bproc, bcost, boxN, gain, proc, cost, len(r.Rows))
		}
	}
	if r := YearReport(2026); r.Closed || len(r.Issues) == 0 || len(r.Income) == 0 {
		t.Errorf("2026 should be open with issues and income: %+v", r.Summary)
	}
	if r := YearReport(2025); !r.Closed || r.Source != "koinly" {
		t.Errorf("2025 should be a closed Koinly year")
	}
}

func TestPreviewTaxEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := templates.TaxSection(TaxPage(2026, "gains", true)).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No disposals") {
		t.Errorf("empty tax section lacks the empty note:\n%.400s", buf.String())
	}
}

func TestPreviewTaxTabs(t *testing.T) {
	for _, tab := range []string{"gains", "income", "holdings", "review"} {
		var buf bytes.Buffer
		if err := templates.TaxSection(TaxPage(2026, tab, false)).Render(context.Background(), &buf); err != nil {
			t.Fatalf("%s: %v", tab, err)
		}
	}
	if p := TaxPage(2025, "review", false); p.ReviewTab || p.Tab == "review" {
		t.Error("a closed year has no review tab")
	}
	p := TaxPage(2026, "review", false)
	if len(p.Review.Groups) == 0 || len(p.Review.Parked) == 0 {
		t.Error("review tab needs groups and a parked position")
	}
}

func TestPreviewNoNetwork(t *testing.T) {
	var buf bytes.Buffer
	if err := templates.KoinlyFull(KoinlyPage(3)).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if p := KoinlyPage(3); len(p.Wizard.Recon) == 0 || len(p.Wizard.Problems) == 0 {
		t.Error("step 3 needs a reconciliation with problems")
	}
	if p := KoinlyPage(1); p.Wizard.HasDraft {
		t.Error("step 1 is the upload alone")
	}
	for _, view := range []func() error{
		func() error { return templates.Form8949Doc(YearReport(2026)).Render(context.Background(), &buf) },
		func() error { return templates.TaxReportPage(TaxReport(2026)).Render(context.Background(), &buf) },
		func() error { return templates.TaxReportPage(TaxReport(2025)).Render(context.Background(), &buf) },
	} {
		if err := view(); err != nil {
			t.Error(err)
		}
	}
}
