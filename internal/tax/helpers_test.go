package tax

import (
	"math"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

func usd(v float64) *float64 { return &v }

// at is a UTC timestamp.
func at(y, m, d, h int) time.Time {
	return time.Date(y, time.Month(m), d, h, 0, 0, 0, time.UTC)
}

func lg(asset string, amount float64, usd *float64) core.Leg {
	return core.Leg{AssetKey: asset, Symbol: asset, Amount: amount, USD: usd}
}

// mk builds a confirmed tx in the given wallet (the tx's SourceID).
func mk(id, wallet string, t time.Time, kind core.TxKind, legs ...core.Leg) core.Tx {
	return core.Tx{
		ID: core.TxID(wallet, id), SourceID: wallet, ExternalID: id,
		Time: t, Kind: kind, Status: core.TxConfirmed, Legs: legs,
	}
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	// Negated so a NaN got fails too.
	if !(math.Abs(got-want) <= 1e-6*math.Max(1, math.Abs(want))) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
