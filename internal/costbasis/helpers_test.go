package costbasis

import (
	"math"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

func usd(v float64) *float64 { return &v }

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

func lg(asset string, amount float64, usd *float64) core.Leg {
	return core.Leg{AssetKey: asset, Symbol: asset, Amount: amount, USD: usd}
}

func mk(id string, t time.Time, kind core.TxKind, legs ...core.Leg) core.Tx {
	return core.Tx{
		ID: core.TxID("test", id), SourceID: "test", ExternalID: id,
		Time: t, Kind: kind, Status: core.TxConfirmed, Legs: legs,
	}
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
