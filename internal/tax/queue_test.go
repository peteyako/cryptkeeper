package tax

import (
	"math"
	"testing"
	"time"
)

func TestQueueFIFOByAcquiredDate(t *testing.T) {
	var q queue
	q.add(Lot{Asset: "SOL", Amount: 1, CostUSD: 100, Acquired: at(2025, 6, 1, 0)})
	q.add(Lot{Asset: "SOL", Amount: 1, CostUSD: 50, Acquired: at(2025, 1, 1, 0)})
	taken, short := q.take(1.5)
	if len(taken) != 2 {
		t.Fatalf("taken = %d lots, want 2", len(taken))
	}
	near(t, "taken[0].Amount", taken[0].Amount, 1)
	near(t, "taken[0].CostUSD", taken[0].CostUSD, 50)
	if !taken[0].Acquired.Equal(at(2025, 1, 1, 0)) {
		t.Errorf("taken[0].Acquired = %v", taken[0].Acquired)
	}
	near(t, "taken[1].Amount", taken[1].Amount, 0.5)
	near(t, "taken[1].CostUSD", taken[1].CostUSD, 50)
	if !taken[1].Acquired.Equal(at(2025, 6, 1, 0)) {
		t.Errorf("taken[1].Acquired = %v", taken[1].Acquired)
	}
	near(t, "short", short, 0)
	near(t, "total", q.total(), 0.5)
	near(t, "remaining cost", q.lots[0].CostUSD, 50)
}

func TestQueueStableForEqualTimes(t *testing.T) {
	var q queue
	q.add(Lot{Amount: 1, TxID: "a", Acquired: at(2025, 1, 1, 0)})
	q.add(Lot{Amount: 1, TxID: "b", Acquired: at(2025, 1, 1, 0)})
	taken, _ := q.take(1)
	if len(taken) != 1 || taken[0].TxID != "a" {
		t.Errorf("taken = %+v, want the first-added lot", taken)
	}
}

func TestQueueTakeShortfall(t *testing.T) {
	var q queue
	q.add(Lot{Asset: "SOL", Amount: 1, CostUSD: 100, Acquired: at(2025, 1, 1, 0)})
	taken, short := q.take(1.25)
	if len(taken) != 1 {
		t.Fatalf("taken = %d lots, want 1", len(taken))
	}
	near(t, "taken.Amount", taken[0].Amount, 1)
	near(t, "taken.CostUSD", taken[0].CostUSD, 100)
	near(t, "short", short, 0.25)
	near(t, "total", q.total(), 0)
}

func TestQueueDustResidue(t *testing.T) {
	var q queue
	q.add(Lot{Amount: 0.1, CostUSD: 1, Acquired: at(2025, 1, 1, 0)})
	q.add(Lot{Amount: 0.2, CostUSD: 2, Acquired: at(2025, 1, 2, 0)})
	_, short := q.take(0.3)
	near(t, "short", short, 0)
	if q.total() != 0 || len(q.lots) != 0 {
		t.Errorf("total = %v, lots = %d, want empty", q.total(), len(q.lots))
	}
}

func TestTermBoundary(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	acq := time.Date(2025, 3, 10, 3, 0, 0, 0, time.UTC) // 2025-03-09 in NY
	if got := termOf(acq, time.Date(2026, 3, 10, 2, 0, 0, 0, time.UTC), ny); got != ShortTerm {
		t.Errorf("sale on the anniversary = %v, want short", got)
	}
	if got := termOf(acq, time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC), ny); got != LongTerm {
		t.Errorf("sale after the anniversary = %v, want long", got)
	}
	if got := termOf(time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), nil); got != LongTerm {
		t.Errorf("various lot = %v, want long", got)
	}
}

func TestQueueIgnoresDustLots(t *testing.T) {
	var q queue
	for _, a := range []float64{0, -1, 1e-13, dust, math.NaN()} {
		q.add(Lot{Amount: a, CostUSD: 5, Acquired: at(2025, 1, 1, 0)})
	}
	if len(q.lots) != 0 {
		t.Fatalf("lots = %+v, want none", q.lots)
	}
	q.add(Lot{Amount: 1, CostUSD: 100, Acquired: at(2025, 1, 1, 0)})
	_, short := q.take(1.5)
	near(t, "short", short, 0.5)
}
