package prices_test

import (
	"context"
	"testing"
	"time"
)

func TestSeriesDailyFromCandles(t *testing.T) {
	e := newEnv(t)
	today := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	seedHistory(e.fk, "BTC", today)
	pts, err := e.svc.Series(context.Background(), "BTC", "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 7 {
		t.Fatalf("got %d points, want 7", len(pts))
	}
	for i, p := range pts {
		want := today.AddDate(0, 0, i-6)
		if !p.Day.Equal(want) {
			t.Errorf("point %d day = %s, want %s", i, p.Day, want)
		}
		if !near(p.USD, closeFor(want)) {
			t.Errorf("point %d usd = %v, want %v", i, p.USD, closeFor(want))
		}
	}
}

func TestSeriesUsesMintForSPL(t *testing.T) {
	e := newEnv(t)
	m := map[string]float64{}
	for i := 0; i < 10; i++ {
		d := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -i)
		m[d.Format(dayFmt)] = 0.1 * float64(10-i)
	}
	e.fk.cg["MintX"] = m
	pts, err := e.svc.Series(context.Background(), "spl:MintX", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) < 4 {
		t.Fatalf("got %d points, want >= 4: %v", len(pts), pts)
	}
	for i := 1; i < len(pts); i++ {
		if !pts[i].Day.After(pts[i-1].Day) {
			t.Fatalf("not ascending: %v", pts)
		}
	}
	if !near(pts[0].USD, m[pts[0].Day.Format(dayFmt)]) {
		t.Errorf("first point %v does not match series", pts[0])
	}
}

func TestSeriesPegged(t *testing.T) {
	e := newEnv(t)
	pts, err := e.svc.Series(context.Background(), "USDC", "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 7 {
		t.Fatalf("got %d points, want 7", len(pts))
	}
	for _, p := range pts {
		if p.USD != 1 {
			t.Errorf("pegged point = %v", p)
		}
	}
	if e.fk.total() != 0 {
		t.Errorf("pegged series made %d requests", e.fk.total())
	}
}

func TestSeriesSkipsMissingDays(t *testing.T) {
	e := newEnv(t)
	today := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	seedHistory(e.fk, "ETH", today)
	e.fk.mu.Lock()
	delete(e.fk.history["ETH"], today.AddDate(0, 0, -2).Format(dayFmt))
	delete(e.fk.history["ETH"], today.AddDate(0, 0, -4).Format(dayFmt))
	e.fk.mu.Unlock()
	pts, err := e.svc.Series(context.Background(), "ETH", "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 5 {
		t.Fatalf("got %d points, want 5", len(pts))
	}
}
