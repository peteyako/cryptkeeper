package costbasis

import (
	"math"
	"testing"

	"cryptkeeper/internal/tax"
)

func usd(v float64) *float64 { return &v }

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// gains sums the gain of asset's disposals per UTC year of the sale.
func gains(res tax.Result, asset string) map[int]float64 {
	out := map[int]float64{}
	for _, d := range res.Disposals {
		if d.Asset == asset {
			out[d.Sold.UTC().Year()] += d.GainUSD
		}
	}
	return out
}

// hasIssue reports whether res has an issue of kind for asset.
func hasIssue(res tax.Result, kind tax.IssueKind, asset string) bool {
	for _, is := range res.Issues {
		if is.Kind == kind && is.Asset == asset {
			return true
		}
	}
	return false
}
