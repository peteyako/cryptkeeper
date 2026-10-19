package portfolio

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"sort"
	"time"

	"cryptkeeper/internal/core"
)

// PriceAt returns the historical USD price (the daily close) of an asset on a UTC day. ok is
// false with a nil error when no price is known; an error is a failed lookup.
type PriceAt func(ctx context.Context, assetKey, mint string, day time.Time) (usd float64, ok bool, err error)

// NotSyncedError refuses a backfill while a wallet has no current holdings to roll back from
// (its last fetch failed, or it was never fetched): the past rows would be written without
// it, for good.
type NotSyncedError struct{ Label string }

func (e *NotSyncedError) Error() string { return e.Label + " isn't syncing" }

// PriceError is a failed historical price lookup. Backfill then writes nothing, since the
// fallback price would be stored in a past row for good.
type PriceError struct {
	Asset string
	Day   time.Time
	Err   error
}

func (e *PriceError) Error() string {
	return fmt.Sprintf("price of %s on %s: %v", e.Asset, e.Day.Format(time.DateOnly), e.Err)
}

func (e *PriceError) Unwrap() error { return e.Err }

// Backfill fills past DAILY history for the last days days (days+1 UTC days ending
// yesterday, so the oldest row lies at least `days` back), built from the current holdings
// rolled back through txs and historical prices. It works per wallet: a wallet is filled
// for the days strictly before its first recorded row (every day when it has none). On a
// day with no row at all it writes a new row stamped at the END of the UTC day (23:59:59),
// but only before the earliest recorded row's UTC day: gaps after that are left alone. On
// a day that has rows, the wallets missing from them are merged into every row of that day,
// so a source added after a backfill still gets its past; the rows' own amounts and prices
// are kept. It returns how many rows it inserted or extended. While a wallet in current is
// in error or was never fetched it refuses with a *NotSyncedError.
//
// A filled wallet holds end-of-day amounts (after day D's txs) and day D's daily close
// price (a merged row keeps the price it already has). Amounts for day D are the current
// amount minus the signed effect of every tx at or after D+1d 00:00 UTC, clamped at 0,
// for wallets in current only; assets at 0 are left out (a wallet holding nothing that
// day is written with no assets, so it still counts as recorded). The rules mirror tax.RollBack
// (portfolio must not import tax): a failed tx only cost its fee; stake and position txs,
// and those isPosition reports (tagged position moves; nil is tolerated), keep units
// inside the wallet (legs ignored) but still pay their fee.
//
// All price lookups happen first, before the write transaction opens (the store has a
// single connection, and price lookups may use it), and only for the days and wallets that
// can be written. Then all rows are written in one transaction: a cancelled context or
// error, including a failed price lookup (*PriceError), writes nothing. A row that changed
// since the lookups is left as it is.
//
// Prices are an estimate: price(...) when it has one, else the asset's current snapshot
// price (a flat line), else 1 for pegged stablecoins.
func (h *History) Backfill(ctx context.Context, days int, now time.Time, current Snapshot, txs []core.Tx,
	isPosition func(txID string) bool, price PriceAt) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	for _, w := range current.Wallets {
		if w.Status == StatusError || w.Status == StatusPending {
			label := w.Label
			if label == "" {
				label = w.ID
			}
			return 0, &NotSyncedError{Label: label}
		}
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	first := today.AddDate(0, 0, -days-1)
	yesterday := today.AddDate(0, 0, -1)

	// Current amounts, mints and snapshot prices.
	cur := map[string]map[string]float64{}
	mints := map[string]string{}
	snapPrice := map[string]float64{}
	for k, p := range current.Prices {
		snapPrice[k] = p.USD
	}
	for _, w := range current.Wallets {
		m := cur[w.ID]
		if m == nil {
			m = map[string]float64{}
			cur[w.ID] = m
		}
		for _, r := range w.Rows {
			m[r.AssetKey] += r.Amount
			if r.Mint != "" {
				mints[r.AssetKey] = r.Mint
			}
			if _, ok := snapPrice[r.AssetKey]; !ok && r.Price != nil {
				snapPrice[r.AssetKey] = r.Price.USD
			}
		}
	}

	// What is recorded already: each wallet's first day, and the rows of the days in range.
	rec, err := recordedBefore(ctx, h.DB.QueryContext, first, today)
	if err != nil {
		return 0, err
	}
	// newUntil is the newest day a new row may be written on; fillUntil the newest day each
	// wallet may be filled on.
	newUntil := yesterday
	if !rec.earliest.IsZero() && rec.earliest.AddDate(0, 0, -1).Before(newUntil) {
		newUntil = rec.earliest.AddDate(0, 0, -1)
	}
	fillUntil := map[string]time.Time{}
	for w := range cur {
		until := yesterday
		if fs, ok := rec.firstSeen[w]; ok && fs.AddDate(0, 0, -1).Before(until) {
			until = fs.AddDate(0, 0, -1)
		}
		if !until.Before(first) {
			fillUntil[w] = until
		}
	}
	if len(fillUntil) == 0 {
		return 0, nil
	}

	// Only txs of wallets we are writing matter.
	var rel []core.Tx
	for _, t := range txs {
		if _, ok := cur[t.SourceID]; ok {
			rel = append(rel, t)
			for _, l := range t.Legs {
				if _, ok := mints[l.AssetKey]; !ok && l.Mint != "" {
					mints[l.AssetKey] = l.Mint
				}
			}
		}
	}

	// Pre-pass: build every write, doing all price lookups before any transaction is opened.
	type write struct {
		ts         int64
		prices     string
		amounts    string
		oldAmounts string // non-empty: an update of the row with these amounts
	}
	var writes []write
	for d := first; !d.After(yesterday); d = d.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		existing := rec.rows[d.Unix()]
		if len(existing) == 0 && d.After(newUntil) {
			continue // a gap after the earliest recorded day
		}
		var need []string
		for w, until := range fillUntil {
			if !d.After(until) {
				need = append(need, w)
			}
		}
		if len(need) == 0 {
			continue
		}
		sort.Strings(need)
		cutoff := d.AddDate(0, 0, 1)
		amounts := map[string]map[string]float64{}
		for _, w := range need {
			m := make(map[string]float64, len(cur[w]))
			maps.Copy(m, cur[w])
			amounts[w] = m
		}
		for _, t := range rel {
			m := amounts[t.SourceID]
			if m == nil || t.Time.Before(cutoff) {
				continue
			}
			if t.Status != core.TxFailed && t.Kind != core.TxStake && t.Kind != core.TxPosition &&
				(isPosition == nil || !isPosition(t.ID)) {
				for _, l := range t.Legs {
					m[l.AssetKey] -= l.Amount
				}
			}
			if t.Fee != nil {
				m[t.Fee.AssetKey] -= t.Fee.Amount
			}
		}
		prices := map[string]float64{}
		out := map[string]map[string]float64{}
		for _, w := range need {
			o := map[string]float64{}
			for a, v := range amounts[w] {
				if v < 1e-9 {
					continue // nothing held: no amount, no price lookup
				}
				o[a] = v
				if _, ok := prices[a]; ok {
					continue
				}
				p, ok, err := price(ctx, a, mints[a], d)
				if err != nil {
					if cerr := ctx.Err(); cerr != nil {
						return 0, cerr
					}
					return 0, &PriceError{Asset: a, Day: d, Err: err}
				}
				if ok && p > 0 && !math.IsInf(p, 0) && !math.IsNaN(p) {
					prices[a] = p
				} else if p, ok := snapPrice[a]; ok {
					prices[a] = p
				} else if core.Pegged[a] {
					prices[a] = 1
				}
			}
			out[w] = o
		}
		if len(existing) == 0 {
			pj, _ := json.Marshal(prices)
			aj, _ := json.Marshal(out)
			writes = append(writes, write{ts: d.Add(24*time.Hour - time.Second).Unix(), prices: string(pj), amounts: string(aj)})
			continue
		}
		for _, r := range existing {
			changed := false
			for w, o := range out {
				if _, ok := r.amounts[w]; ok {
					continue // recorded on that row already: keep it
				}
				r.amounts[w] = o
				changed = true
				for a := range o {
					if _, ok := r.prices[a]; !ok {
						if p, ok := prices[a]; ok {
							r.prices[a] = p
						}
					}
				}
			}
			if !changed {
				continue
			}
			pj, _ := json.Marshal(r.prices)
			aj, _ := json.Marshal(r.amounts)
			writes = append(writes, write{ts: r.ts, prices: string(pj), amounts: string(aj), oldAmounts: r.rawAmounts})
		}
	}
	if len(writes) == 0 {
		return 0, nil
	}

	dbtx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer dbtx.Rollback() // no-op after Commit; all-or-nothing on any error or cancellation
	written := 0
	for _, w := range writes {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var res sql.Result
		if w.oldAmounts == "" {
			// A row that appeared during the pre-pass wins.
			res, err = dbtx.ExecContext(ctx, `INSERT OR IGNORE INTO history (ts, prices, amounts) VALUES (?, ?, ?)`,
				w.ts, w.prices, w.amounts)
		} else {
			// Only extend the row as it was read; a row that changed since is left alone.
			res, err = dbtx.ExecContext(ctx, `UPDATE history SET prices = ?, amounts = ? WHERE ts = ? AND amounts = ?`,
				w.prices, w.amounts, w.ts, w.oldAmounts)
		}
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}
	}
	if err := dbtx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// recordedRow is a history row read back for merging.
type recordedRow struct {
	ts         int64
	prices     map[string]float64
	amounts    map[string]map[string]float64
	rawAmounts string
}

// recorded is what Backfill learns from the rows before today: the UTC day of the earliest
// row, each wallet's first UTC day, and the rows of each UTC day from `from` on, keyed by
// the day's 00:00 UTC Unix time.
type recorded struct {
	earliest  time.Time
	firstSeen map[string]time.Time
	rows      map[int64][]*recordedRow
}

func recordedBefore(ctx context.Context, query func(context.Context, string, ...any) (*sql.Rows, error),
	from, today time.Time) (recorded, error) {
	rec := recorded{firstSeen: map[string]time.Time{}, rows: map[int64][]*recordedRow{}}
	rows, err := query(ctx, `SELECT ts, prices, amounts FROM history WHERE ts < ? ORDER BY ts`, today.Unix())
	if err != nil {
		return rec, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var pj, aj string
		if err := rows.Scan(&ts, &pj, &aj); err != nil {
			return rec, err
		}
		r := &recordedRow{ts: ts, rawAmounts: aj}
		if json.Unmarshal([]byte(pj), &r.prices) != nil || json.Unmarshal([]byte(aj), &r.amounts) != nil {
			continue
		}
		if r.prices == nil {
			r.prices = map[string]float64{}
		}
		if r.amounts == nil {
			r.amounts = map[string]map[string]float64{}
		}
		t := time.Unix(ts, 0).UTC()
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		if rec.earliest.IsZero() {
			rec.earliest = day
		}
		for w := range r.amounts {
			if _, ok := rec.firstSeen[w]; !ok {
				rec.firstSeen[w] = day
			}
		}
		if !day.Before(from) {
			rec.rows[day.Unix()] = append(rec.rows[day.Unix()], r)
		}
	}
	return rec, rows.Err()
}
