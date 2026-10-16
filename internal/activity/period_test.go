package activity

import (
	"context"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
)

func TestPeriodBounds(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 10, 16, 15, 30, 0, 0, la)
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, la) }
	cases := []struct {
		p            Period
		since, until time.Time
		label        string
	}{
		{Period{}, time.Time{}, time.Time{}, "All time"},
		{Period{Key: "7d"}, now.AddDate(0, 0, -7), time.Time{}, "Last 7 days"},
		{Period{Key: "mtd"}, d(2026, 10, 1), time.Time{}, "Month to date"},
		{Period{Key: "ytd"}, d(2026, 1, 1), time.Time{}, "Year to date"},
		{Period{Key: "y2025"}, d(2025, 1, 1), d(2026, 1, 1), "2025"},
		{Period{Key: "custom", From: "2026-03-01", To: "2026-03-31"}, d(2026, 3, 1), d(2026, 4, 1), "2026-03-01 – 2026-03-31"},
		{Period{Key: "custom", From: "2026-03-01"}, d(2026, 3, 1), time.Time{}, "2026-03-01 – today"},
		{Period{Key: "custom", From: "garbage", To: "2026-03-31"}, time.Time{}, d(2026, 4, 1), "garbage – 2026-03-31"},
		{Period{Key: "y99"}, time.Time{}, time.Time{}, "99"},
	}
	for _, c := range cases {
		since, until := c.p.Bounds(now, la)
		if !since.Equal(c.since) || !until.Equal(c.until) {
			t.Errorf("%+v: got [%v, %v), want [%v, %v)", c.p, since, until, c.since, c.until)
		}
		if got := c.p.Label(); got != c.label {
			t.Errorf("%+v label %q, want %q", c.p, got, c.label)
		}
	}
}

func TestYears(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if ys, err := s.Years(ctx, time.UTC); err != nil || len(ys) != 0 {
		t.Fatalf("empty store: %v %v", ys, err)
	}
	s.Upsert(ctx, []core.Tx{
		{ID: "a:1", SourceID: "a", ExternalID: "1", Time: time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC), Kind: core.TxReceive, Status: core.TxConfirmed},
		{ID: "a:2", SourceID: "a", ExternalID: "2", Time: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Kind: core.TxReceive, Status: core.TxConfirmed},
	})
	ys, err := s.Years(ctx, time.UTC)
	if err != nil || len(ys) != 4 || ys[0] != 2026 || ys[3] != 2023 {
		t.Fatalf("years %v %v", ys, err)
	}
}
