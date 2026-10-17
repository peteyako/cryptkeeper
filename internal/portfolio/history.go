package portfolio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"crypto-portfolio/internal/store"
)

// History records amounts and prices (not just totals) so any portfolio, including one
// created today, can be charted retroactively.
type History struct {
	DB       *store.DB
	Interval time.Duration // minimum spacing between rows; default 30 min
	last     time.Time
}

type historyRow struct {
	TS      time.Time
	Prices  map[string]float64            // assetKey → USD
	Amounts map[string]map[string]float64 // walletID → assetKey → amount
}

// Record stores s if at least Interval has passed since the last row. Stale wallets and
// stale prices are recorded too (they are the best estimate) but errors are not.
func (h *History) Record(ctx context.Context, s Snapshot) error {
	interval := h.Interval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	if h.last.IsZero() {
		var ts int64
		if err := h.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), 0) FROM history`).Scan(&ts); err == nil && ts > 0 {
			h.last = time.Unix(ts, 0)
		}
	}
	if !h.last.IsZero() && s.UpdatedAt.Sub(h.last) < interval {
		return nil
	}
	prices := map[string]float64{}
	for k, p := range s.Prices {
		prices[k] = p.USD
	}
	amounts := map[string]map[string]float64{}
	for _, w := range s.Wallets {
		if w.Status == StatusError {
			continue
		}
		m := map[string]float64{}
		for _, r := range w.Rows {
			m[r.AssetKey] += r.Amount
		}
		if len(m) > 0 {
			amounts[w.ID] = m
		}
	}
	if len(amounts) == 0 {
		return nil
	}
	pj, _ := json.Marshal(prices)
	aj, _ := json.Marshal(amounts)
	if _, err := h.DB.ExecContext(ctx, `INSERT OR REPLACE INTO history (ts, prices, amounts) VALUES (?, ?, ?)`,
		s.UpdatedAt.Unix(), string(pj), string(aj)); err != nil {
		return err
	}
	h.last = s.UpdatedAt
	return nil
}

// Compact keeps one row per UTC day (the last) for rows older than keepFull.
func (h *History) Compact(ctx context.Context, now time.Time, keepFull time.Duration) error {
	cutoff := now.Add(-keepFull).Unix()
	_, err := h.DB.ExecContext(ctx, `DELETE FROM history WHERE ts < ? AND ts NOT IN (
		SELECT MAX(ts) FROM history WHERE ts < ? GROUP BY date(ts, 'unixepoch'))`, cutoff, cutoff)
	return err
}

// Include decides whether a (wallet, asset) pair counts toward a series.
type Include func(walletID, assetKey string) bool

// Series returns the value over time since from (zero = all history), valuing each row
// with that row's prices. current, if non-nil, is appended as the final point.
//
// The series starts once every wallet in current has history: otherwise adding a source
// would show up as a cliff of "growth" (and removing one as a crash). Wallets no longer in
// current are ignored in older rows, and rows recorded without one of the live wallets
// (it was failing or still loading) are left out, for the same reason.
func (h *History) Series(ctx context.Context, from time.Time, inc Include, current *Snapshot) ([]Point, error) {
	rows, err := h.DB.QueryContext(ctx, `SELECT ts, prices, amounts FROM history WHERE ts >= ? ORDER BY ts`, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	live := map[string]bool{} // wallets in the current snapshot that contribute to this series
	if current != nil {
		for _, w := range current.Wallets {
			for _, r := range w.Rows {
				if inc == nil || inc(w.ID, r.AssetKey) {
					live[w.ID] = true
					break
				}
			}
		}
	}
	type row struct {
		ts      time.Time
		prices  map[string]float64
		amounts map[string]map[string]float64
	}
	var all []row
	firstSeen := map[string]time.Time{}
	for rows.Next() {
		var ts int64
		var pj, aj string
		if err := rows.Scan(&ts, &pj, &aj); err != nil {
			return nil, err
		}
		var r row
		r.ts = time.Unix(ts, 0)
		if json.Unmarshal([]byte(pj), &r.prices) != nil || json.Unmarshal([]byte(aj), &r.amounts) != nil {
			continue
		}
		for wid := range r.amounts {
			if _, ok := firstSeen[wid]; !ok {
				firstSeen[wid] = r.ts
			}
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var start time.Time
	for wid := range live {
		fs, ok := firstSeen[wid]
		if !ok {
			start = time.Unix(1<<62, 0) // a live wallet with no history yet: only "now" is comparable
			break
		}
		if fs.After(start) {
			start = fs
		}
	}
	var pts []Point
	for _, r := range all {
		if r.ts.Before(start) || partialRow(r.amounts, live) {
			continue
		}
		v := 0.0
		for wid, m := range r.amounts {
			if current != nil && !live[wid] {
				continue
			}
			for k, amt := range m {
				if inc == nil || inc(wid, k) {
					v += amt * r.prices[k]
				}
			}
		}
		pts = append(pts, Point{T: r.ts, V: v})
	}
	if current != nil && (len(pts) == 0 || current.UpdatedAt.After(pts[len(pts)-1].T)) {
		v := 0.0
		for _, w := range current.Wallets {
			for _, r := range w.Rows {
				if inc == nil || inc(w.ID, r.AssetKey) {
					v += r.ValueUSD
				}
			}
		}
		pts = append(pts, Point{T: current.UpdatedAt, V: v})
	}
	return Downsample(pts, 500), nil
}

// partialRow reports whether a row lacks one of the live wallets.
func partialRow(amounts map[string]map[string]float64, live map[string]bool) bool {
	for wid := range live {
		if _, ok := amounts[wid]; !ok {
			return true
		}
	}
	return false
}

// Downsample keeps at most max points, always keeping the first and last.
func Downsample(pts []Point, max int) []Point {
	if len(pts) <= max || max < 2 {
		return pts
	}
	out := make([]Point, 0, max)
	step := float64(len(pts)-1) / float64(max-1)
	for i := range max {
		out = append(out, pts[int(float64(i)*step+0.5)])
	}
	return out
}

// Ranges maps the UI range keys to look-back windows. YTD (since January 1st, so its
// window depends on the date; see RangeWindow) and All (all history) are 0 here.
var Ranges = map[string]time.Duration{
	"1D": 24 * time.Hour, "1W": 7 * 24 * time.Hour, "1M": 30 * 24 * time.Hour, "YTD": 0, "1Y": 365 * time.Hour * 24, "All": 0,
}

var rangeLabels = map[string]string{
	"1D": "past 24 hours", "1W": "past week", "1M": "past month", "YTD": "this year", "1Y": "past year", "All": "since tracking began",
}

// YearStart is midnight on January 1st of now's year, in now's zone.
func YearStart(now time.Time) time.Time {
	return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location())
}

// RangeWindow is how far back a range reaches from now (0 for All; unknown keys are 1W).
func RangeWindow(rng string, now time.Time) time.Duration {
	if rng == "YTD" {
		return now.Sub(YearStart(now))
	}
	d, ok := Ranges[rng]
	if !ok {
		d = Ranges["1W"]
	}
	return d
}

// RangeStart returns the series start for a range key (unknown keys default to 1W).
func RangeStart(rng string, now time.Time) time.Time {
	if rng == "All" {
		return time.Time{}
	}
	return now.Add(-RangeWindow(rng, now))
}

// ChangeFor computes the change across pts for a range. It is valid only when history
// reaches back to (nearly) the start of the window: the first point may lie up to d/10
// after the window start, or up to 24h for ranges of 7 days or more (so daily backfilled
// rows always cover 1W/1M/1Y).
func ChangeFor(rng string, pts []Point, now time.Time) Change {
	label, ok := rangeLabels[rng]
	if !ok {
		rng, label = "1W", rangeLabels["1W"]
	}
	c := Change{Label: label}
	if len(pts) < 2 {
		return c
	}
	first, last := pts[0], pts[len(pts)-1]
	if d := RangeWindow(rng, now); d > 0 {
		tol := d / 10
		if d >= 7*24*time.Hour {
			tol = max(tol, 24*time.Hour)
		}
		if first.T.After(now.Add(-d).Add(tol)) {
			return c // history doesn't cover the window yet
		}
	}
	c.Abs = last.V - first.V
	if first.V != 0 {
		c.Pct = c.Abs / first.V * 100
	}
	c.Valid = true
	return c
}

// PriceChanges returns the percent price change over window for each asset in current,
// comparing current prices with the history row nearest to now-window. Assets without a
// comparable row (history younger than the window, or the asset wasn't priced then) are
// absent. A row only counts if it lies within 10% of the window from the target time.
func (h *History) PriceChanges(ctx context.Context, current map[string]float64, window time.Duration, now time.Time) (map[string]float64, error) {
	target := now.Add(-window).Unix()
	tol := int64(window.Seconds() / 10)
	var pj string
	err := h.DB.QueryRowContext(ctx, `SELECT prices FROM history WHERE ts BETWEEN ? AND ?
		ORDER BY ABS(ts - ?) LIMIT 1`, target-tol, target+tol, target).Scan(&pj)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]float64{}, nil
	}
	if err != nil {
		return nil, err
	}
	var then map[string]float64
	if err := json.Unmarshal([]byte(pj), &then); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for k, cur := range current {
		if old, ok := then[k]; ok && old > 0 && cur > 0 {
			out[k] = (cur - old) / old * 100
		}
	}
	return out, nil
}

// ValueAt values the history row nearest to at (within tol) with that row's prices, counting
// only (wallet, asset) pairs accepted by inc (nil = all). ok is false when no row is close.
func (h *History) ValueAt(ctx context.Context, at time.Time, tol time.Duration, inc Include) (float64, bool, error) {
	t := at.Unix()
	var pj, aj string
	err := h.DB.QueryRowContext(ctx, `SELECT prices, amounts FROM history WHERE ts BETWEEN ? AND ?
		ORDER BY ABS(ts - ?) LIMIT 1`, t-int64(tol.Seconds()), t+int64(tol.Seconds()), t).Scan(&pj, &aj)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var prices map[string]float64
	var amounts map[string]map[string]float64
	if err := json.Unmarshal([]byte(pj), &prices); err != nil {
		return 0, false, err
	}
	if err := json.Unmarshal([]byte(aj), &amounts); err != nil {
		return 0, false, err
	}
	v := 0.0
	for wid, m := range amounts {
		for k, amt := range m {
			if inc == nil || inc(wid, k) {
				v += amt * prices[k]
			}
		}
	}
	return v, true, nil
}
