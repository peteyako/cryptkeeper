// Package export writes the CSV downloads: holdings, portfolio history, P&L and activity.
//
// Conventions shared by all writers: a header row first, RFC 4180 quoting via encoding/csv,
// numbers rendered with strconv 'f' and shortest precision (no thousands separators, no
// exponent), timestamps as RFC 3339 in UTC, and an empty cell for an unknown value. Text
// cells that a spreadsheet could read as a formula (leading = + - @) get a leading
// apostrophe, because token symbols and counterparties come from outside.
package export

import (
	"encoding/csv"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
)

// num formats v for a CSV cell; NaN and Inf are unknown and become an empty cell.
func num(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	if v == 0 {
		return "0" // also normalizes -0
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func numPtr(v *float64) string {
	if v == nil {
		return ""
	}
	return num(*v)
}

// text defuses spreadsheet formula injection in a free-text cell.
func text(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// writer wraps csv.Writer so each exporter only builds rows.
type writer struct{ w *csv.Writer }

func newWriter(w io.Writer) writer { return writer{csv.NewWriter(w)} }

func (c writer) row(cells ...string) { _ = c.w.Write(cells) } // errors are sticky: see finish

func (c writer) finish() error {
	c.w.Flush()
	return c.w.Error()
}

// Holdings writes one row per visible holding of every wallet in the snapshot:
// wallet (label), source_type, asset (asset key), symbol, amount, price_usd, value_usd,
// stale, protocol. Hidden holdings (spam, dust, unpriced) are not exported. price_usd and
// value_usd are empty for unpriced holdings. stale is true when the wallet's balances or
// the holding's price come from a last-good cache.
func Holdings(w io.Writer, snap portfolio.Snapshot) error {
	c := newWriter(w)
	c.row("wallet", "source_type", "asset", "symbol", "amount", "price_usd", "value_usd", "stale", "protocol")
	for _, wal := range snap.Wallets {
		walletStale := wal.Status == portfolio.StatusStale
		for _, r := range wal.Rows {
			var price, value string
			stale := walletStale
			if r.Price != nil {
				price = num(r.Price.USD)
				stale = stale || r.Price.Stale
			}
			if r.Priced {
				value = num(r.ValueUSD)
			}
			c.row(text(wal.Label), text(wal.Type), text(r.AssetKey), text(r.Symbol), num(r.Amount),
				price, value, strconv.FormatBool(stale), text(r.Protocol))
		}
	}
	return c.finish()
}

// History writes the portfolio value series: timestamp (RFC 3339, UTC), value_usd.
func History(w io.Writer, pts []portfolio.Point) error {
	c := newWriter(w)
	c.row("timestamp", "value_usd")
	for _, p := range pts {
		c.row(stamp(p.T), num(p.V))
	}
	return c.finish()
}

// PnLRow is one asset of the P&L export, costed from the open tax lots.
type PnLRow struct {
	AssetKey, Symbol string
	Amount, AvgCost  float64 // AvgCost: basis per unit covered by lots with a known cost
	Basis, Value     float64
	Unrealized       float64
	UnrealizedPct    float64 // percent, 50 = +50%
	MissingUnits     float64 // units in missing-basis lots
	Realized         map[int]float64
	// HasBasis is false when no cost is known: avg_cost, basis, unrealized and
	// unrealized_pct are then left empty.
	HasBasis bool
}

// PnL writes one row per asset: asset (the symbol, else the asset key), amount, avg_cost,
// basis, value, unrealized, unrealized_pct, missing_basis_units, then one realized_<year>
// column per tax year that any row has, ascending. A row without realized P&L for a year
// leaves that cell empty.
func PnL(w io.Writer, rows []PnLRow) error {
	yearSet := map[int]bool{}
	for _, r := range rows {
		for y := range r.Realized {
			yearSet[y] = true
		}
	}
	years := make([]int, 0, len(yearSet))
	for y := range yearSet {
		years = append(years, y)
	}
	sort.Ints(years)

	c := newWriter(w)
	header := []string{"asset", "amount", "avg_cost", "basis", "value", "unrealized", "unrealized_pct", "missing_basis_units"}
	for _, y := range years {
		header = append(header, "realized_"+strconv.Itoa(y))
	}
	c.row(header...)

	for _, r := range rows {
		asset := r.Symbol
		if asset == "" {
			asset = r.AssetKey
		}
		cells := []string{text(asset), num(r.Amount), "", "", num(r.Value), "", "", num(r.MissingUnits)}
		if r.HasBasis {
			cells[2], cells[3] = num(r.AvgCost), num(r.Basis)
			cells[5], cells[6] = num(r.Unrealized), num(r.UnrealizedPct)
		}
		for _, y := range years {
			if v, ok := r.Realized[y]; ok {
				cells = append(cells, num(v))
			} else {
				cells = append(cells, "")
			}
		}
		c.row(cells...)
	}
	return c.finish()
}

// Activity writes one row per leg: time (RFC 3339, UTC), source (labels[SourceID], else
// the source id), kind, status, asset (symbol, else asset key), amount, usd, fee_asset,
// fee_amount, counterparty, tx_hash. The fee appears on the first row of its transaction
// only, so summing the column does not double count. A transaction without legs still gets
// one row (asset, amount and usd empty) so it is not lost.
func Activity(w io.Writer, txs []core.Tx, labels map[string]string) error {
	c := newWriter(w)
	c.row("time", "source", "kind", "status", "asset", "amount", "usd", "fee_asset", "fee_amount", "counterparty", "tx_hash")
	for _, tx := range txs {
		source := tx.SourceID
		if l := labels[tx.SourceID]; l != "" {
			source = l
		}
		var feeAsset, feeAmount string
		if tx.Fee != nil {
			feeAsset, feeAmount = text(legAsset(*tx.Fee)), num(tx.Fee.Amount)
		}
		emit := func(asset, amount, usd string) {
			c.row(stamp(tx.Time), text(source), string(tx.Kind), string(tx.Status), asset, amount, usd,
				feeAsset, feeAmount, text(tx.Counterparty), text(tx.TxHash))
			feeAsset, feeAmount = "", ""
		}
		if len(tx.Legs) == 0 {
			emit("", "", "")
			continue
		}
		for _, l := range tx.Legs {
			emit(text(legAsset(l)), num(l.Amount), numPtr(l.USD))
		}
	}
	return c.finish()
}

func legAsset(l core.Leg) string {
	if strings.TrimSpace(l.Symbol) != "" {
		return l.Symbol
	}
	return l.AssetKey
}
