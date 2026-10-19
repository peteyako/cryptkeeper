package tax

import (
	"strings"
	"testing"
	"time"
)

func summaryYear() Year {
	d := []Disposal{
		{Sold: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), Asset: "AAA", ProceedsUSD: 120, CostUSD: 100, GainUSD: 20, Term: ShortTerm},
		{Sold: time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC), Asset: "BBB", ProceedsUSD: 80, CostUSD: 90, GainUSD: -10, Term: LongTerm},
	}
	in := []Income{{Date: time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC), Asset: "CCC", ValueUSD: 15.5, Type: "reward"}}
	return Year{Year: 2024, Source: "cryptkeeper", Summary: Summarize(d, in), Disposals: d, Income: in}
}

func TestYearValidateSummaryAgreesWithRows(t *testing.T) {
	y := summaryYear()
	if err := y.Validate(); err != nil {
		t.Fatalf("consistent year refused: %v", err)
	}
	cut := y
	cut.Disposals = y.Disposals[:1] // a truncated file loses rows but keeps the summary
	if err := cut.Validate(); err == nil || !strings.Contains(err.Error(), "summary") {
		t.Fatalf("truncated disposals accepted: %v", err)
	}
	cut = y
	cut.Income = nil
	if err := cut.Validate(); err == nil || !strings.Contains(err.Error(), "summary") {
		t.Fatalf("dropped income accepted: %v", err)
	}
	// A summary-only year (an import with no row detail) has nothing to compare.
	bare := Year{Year: 2023, Source: "koinly", Summary: Summary{ShortGain: 5}}
	if err := bare.Validate(); err != nil {
		t.Fatalf("summary-only year refused: %v", err)
	}
}
