package activity

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Period is a date-range choice for the activity feed: a preset key ("7d", "30d", "90d",
// "mtd", "ytd"), a year ("y2025"), "custom" with From/To dates (YYYY-MM-DD, inclusive), or
// "" for all time.
type Period struct {
	Key, From, To string
}

// Bounds returns [since, until) for the period in loc. Zero times mean unbounded. A custom
// period with an unparseable date ignores that bound.
func (p Period) Bounds(now time.Time, loc *time.Location) (since, until time.Time) {
	now = now.In(loc)
	day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc) }
	switch k := p.Key; {
	case k == "7d":
		return now.AddDate(0, 0, -7), time.Time{}
	case k == "30d":
		return now.AddDate(0, 0, -30), time.Time{}
	case k == "90d":
		return now.AddDate(0, 0, -90), time.Time{}
	case k == "mtd":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), time.Time{}
	case k == "ytd":
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc), time.Time{}
	case strings.HasPrefix(k, "y"):
		y, err := strconv.Atoi(k[1:])
		if err != nil || y < 2009 || y > 9999 {
			return time.Time{}, time.Time{}
		}
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc), time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)
	case k == "custom":
		if t, err := time.ParseInLocation("2006-01-02", p.From, loc); err == nil {
			since = day(t)
		}
		if t, err := time.ParseInLocation("2006-01-02", p.To, loc); err == nil {
			until = day(t).AddDate(0, 0, 1) // To is inclusive
		}
		return since, until
	}
	return time.Time{}, time.Time{}
}

// Label describes the period for display ("Year to date", "2025", "Mar 1 – Mar 31, 2026").
func (p Period) Label() string {
	switch k := p.Key; {
	case k == "":
		return "All time"
	case k == "7d":
		return "Last 7 days"
	case k == "30d":
		return "Last 30 days"
	case k == "90d":
		return "Last 90 days"
	case k == "mtd":
		return "Month to date"
	case k == "ytd":
		return "Year to date"
	case strings.HasPrefix(k, "y"):
		return k[1:]
	case k == "custom":
		from, to := p.From, p.To
		if from == "" {
			from = "start"
		}
		if to == "" {
			to = "today"
		}
		return fmt.Sprintf("%s – %s", from, to)
	}
	return p.Key
}

// Years lists the years that have activity (in loc), newest first.
func (s *Store) Years(ctx context.Context, loc *time.Location) ([]int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT MIN(time), MAX(time) FROM activity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lo, hi *int64
	if rows.Next() {
		if err := rows.Scan(&lo, &hi); err != nil {
			return nil, err
		}
	}
	if lo == nil || hi == nil {
		return nil, rows.Err()
	}
	first, last := time.UnixMilli(*lo).In(loc).Year(), time.UnixMilli(*hi).In(loc).Year()
	var out []int
	for y := last; y >= first; y-- {
		out = append(out, y)
	}
	return out, rows.Err()
}
