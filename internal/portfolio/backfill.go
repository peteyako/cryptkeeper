package portfolio

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"crypto-portfolio/internal/core"
)

// PriceAt returns the historical USD price of an asset on a UTC day.
type PriceAt func(ctx context.Context, assetKey, mint string, day time.Time) (float64, bool)

// Backfill writes past DAILY history rows, stamped at the END of each UTC day (23:59:59) for the last days days, built from
// the current holdings rolled back through txs and historical prices. It only adds rows
// for days strictly before the earliest recorded row's UTC day (or the last days+1 days
// ending yesterday when the table is empty), never replaces a row, and returns how many
// rows were inserted.
//
// Each row holds end-of-day amounts (after day D's txs) and day D's daily close price.
// Amounts for day D are the current amount minus the signed effect of every tx at or
// after D+1d 00:00 UTC, clamped at 0, for wallets in current only. The rules mirror
// tax.RollBack (portfolio must not import tax): a failed tx only cost its fee; stake and
// position txs keep units inside the wallet (legs ignored) but still pay their fee.
//
// All price lookups happen first, before the write transaction opens (the store has a
// single connection, and price lookups may use it). Then all rows are written in one
// transaction: a cancelled context or error writes nothing.
//
// Prices are an estimate: price(...) when it has one, else the asset's current snapshot
// price (a flat line), else 1 for pegged stablecoins.
func (h *History) Backfill(ctx context.Context, days int, now time.Time, current Snapshot, txs []core.Tx, price PriceAt) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	first := today.AddDate(0, 0, -days-1) // days+1 days so the oldest row lies at least `days` back

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

	// Pre-pass: build every row for the widest possible range, doing all price lookups
	// before any transaction is opened.
	type dayRow struct {
		ts      int64
		day     time.Time
		prices  string
		amounts string
	}
	var rows []dayRow
	for d := first; !d.After(yesterday); d = d.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		cutoff := d.AddDate(0, 0, 1)
		amounts := map[string]map[string]float64{}
		for w, assets := range cur {
			m := make(map[string]float64, len(assets))
			for a, v := range assets {
				m[a] = v
			}
			amounts[w] = m
		}
		for _, t := range rel {
			if t.Time.Before(cutoff) {
				continue
			}
			m := amounts[t.SourceID]
			if t.Status != core.TxFailed && t.Kind != core.TxStake && t.Kind != core.TxPosition {
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
		for w, m := range amounts {
			o := map[string]float64{}
			for a, v := range m {
				if v < 1e-9 {
					v = 0
				}
				o[a] = v
				if _, ok := prices[a]; ok {
					continue
				}
				if p, ok := price(ctx, a, mints[a], d); ok && p > 0 && !math.IsInf(p, 0) && !math.IsNaN(p) {
					prices[a] = p
				} else if p, ok := snapPrice[a]; ok {
					prices[a] = p
				} else if core.Pegged[a] {
					prices[a] = 1
				}
			}
			if len(o) > 0 {
				out[w] = o
			}
		}
		if len(out) == 0 {
			continue
		}
		pj, _ := json.Marshal(prices)
		aj, _ := json.Marshal(out)
		rows = append(rows, dayRow{ts: d.Add(24*time.Hour - time.Second).Unix(), day: d, prices: string(pj), amounts: string(aj)})
	}

	dbtx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer dbtx.Rollback() // no-op after Commit; all-or-nothing on any error or cancellation
	// Only days strictly before the earliest recorded row's UTC day (read in the tx, so
	// rows that appeared during the pre-pass are respected).
	last := yesterday
	var minTS int64
	if err := dbtx.QueryRowContext(ctx, `SELECT COALESCE(MIN(ts), 0) FROM history`).Scan(&minTS); err != nil {
		return 0, err
	}
	if minTS > 0 {
		e := time.Unix(minTS, 0).UTC()
		eDay := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, time.UTC)
		if p := eDay.AddDate(0, 0, -1); p.Before(last) {
			last = p
		}
	}
	inserted := 0
	for _, r := range rows {
		if r.day.After(last) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		res, err := dbtx.ExecContext(ctx, `INSERT OR IGNORE INTO history (ts, prices, amounts) VALUES (?, ?, ?)`,
			r.ts, r.prices, r.amounts)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if err := dbtx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}
