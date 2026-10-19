package costbasis

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
)

const coinbaseCSVSource = "coinbase-csv"

// Coinbase "Transaction history" report types. The classification is case-insensitive.
var coinbaseTypes = map[string]cbType{
	"buy":                 cbBuy,
	"advanced trade buy":  cbBuy,
	"sell":                cbSell,
	"advanced trade sell": cbSell,
	"convert":             cbConvert,
	"send":                cbSend,
	"receive":             cbReceive,

	"rewards income":   cbReward,
	"staking income":   cbReward,
	"learning reward":  cbReward,
	"inflation reward": cbReward,
	"coinbase earn":    cbReward,
	"reward":           cbReward,
	"staking reward":   cbReward,
}

// Types that are recognized but deliberately not imported: fiat movements and moves between
// Coinbase and Coinbase Pro/Exchange do not change cost basis.
var coinbaseIgnored = map[string]string{
	"deposit":             "fiat deposit",
	"withdrawal":          "fiat withdrawal",
	"pro deposit":         "transfer to Coinbase Pro",
	"pro withdrawal":      "transfer from Coinbase Pro",
	"exchange deposit":    "transfer to Coinbase Exchange",
	"exchange withdrawal": "transfer from Coinbase Exchange",
}

type cbType int

const (
	cbBuy cbType = iota + 1
	cbSell
	cbConvert
	cbSend
	cbReceive
	cbReward
)

var (
	convertRe = regexp.MustCompile(`(?i)converted\s+([0-9][0-9,]*(?:\.[0-9]+)?|\.[0-9]+)\s+(\S+)\s+to\s+([0-9][0-9,]*(?:\.[0-9]+)?|\.[0-9]+)\s+(\S+)`)
	peerRe    = regexp.MustCompile(`(?i)^(?:sent|received)\s+\S+\s+\S+\s+(?:to|from)\s+(.+)$`)
)

type cbCols struct {
	id, ts, typ, asset, qty, ccy, price, subtotal, total, fees, notes int
}

// ParseCoinbaseCSV parses Coinbase's "Transaction history" report into activity entries.
//
// Lines before the header row (the one containing both "Timestamp" and "Transaction Type")
// are skipped. Columns are matched by name, so both the older ("Spot Price ...") and newer
// ("ID", "Price Currency", "Price at Transaction") layouts work. Mapping:
//
//   - Buy / Advanced Trade Buy: kind trade, one leg {asset, +qty, USD: +Total} where Total
//     is the cost including fees and spread; Note "buy".
//   - Sell / Advanced Trade Sell: trade, one leg {asset, -qty, USD: -Total}, the proceeds net
//     of fees; Note "sell".
//   - Convert: trade, legs {asset, -qty, USD: -Subtotal} and {other, +qty2, USD: +Subtotal},
//     the other side parsed from Notes ("Converted 0.5 ETH to 900 USDC"); Note "convert".
//   - Send / Receive: send / receive with a single signed leg and no USD value. A transfer is
//     not a priced event (the transfer matcher pairs the two sides; an unmatched receive has
//     an unknown basis until the user sets it).
//   - Rewards Income, Staking Income, Learning Reward, Inflation Reward, Coinbase Earn:
//     reward, +qty valued at quantity x price at transaction (fair market value).
//
// USD values are only set when the price currency is USD (or absent); other currencies are
// left nil for the valuation step. ExternalID is the ID column when present, otherwise a
// stable hash of the economic content of the row (timestamp, type, asset, quantity, money
// columns, notes) with an occurrence counter for genuinely identical rows, so importing the
// same file or the same history in the other layout twice gives the same ids. Fiat deposits
// and withdrawals, and transfers to/from Coinbase Pro, are skipped (see ParseOptions.OnSkip).
// The result is sorted by time.
func ParseCoinbaseCSV(r io.Reader, opts ...ParseOptions) ([]core.Tx, error) {
	o := resolveOptions(opts, coinbaseCSVSource)
	cr := csv.NewReader(newCSVInput(r))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true

	var cols cbCols
	haveHeader := false
	var txs []core.Tx
	occurrences := map[string]int{}

	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !haveHeader {
				continue // malformed preamble line: keep looking for the header
			}
			return nil, fmt.Errorf("coinbase csv: %w", err)
		}
		line, _ := cr.FieldPos(0)
		if !haveHeader {
			h := newHeaderIndex(rec)
			if h.col("timestamp") < 0 || h.col("transaction type") < 0 {
				continue
			}
			cols = cbCols{
				id: h.col("id", "transaction id"), ts: h.col("timestamp"), typ: h.col("transaction type"),
				asset: h.col("asset"), qty: h.col("quantity transacted", "quantity"),
				ccy:      h.col("spot price currency", "price currency"),
				price:    h.col("spot price at transaction", "price at transaction"),
				subtotal: h.col("subtotal"), total: h.prefixCol("total"),
				fees: h.prefixCol("fees"), notes: h.col("notes", "note"),
			}
			if cols.asset < 0 || cols.qty < 0 {
				return nil, fmt.Errorf("coinbase csv line %d: header lacks Asset or Quantity Transacted", line)
			}
			haveHeader = true
			continue
		}
		if allBlank(rec) {
			continue
		}
		tx, ok, err := cols.parseRow(rec, o, occurrences)
		if err != nil {
			return nil, fmt.Errorf("coinbase csv line %d: %w", line, err)
		}
		if !ok {
			o.skip(line, "%s", skipReason(rec, cols))
			continue
		}
		txs = append(txs, tx)
	}
	if !haveHeader {
		return nil, errors.New("coinbase csv: no header row with Timestamp and Transaction Type found")
	}
	sort.SliceStable(txs, func(i, j int) bool { return txs[i].Time.Before(txs[j].Time) })
	return txs, nil
}

func allBlank(rec []string) bool {
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}

func skipReason(rec []string, c cbCols) string {
	typ := field(rec, c.typ)
	if why, ok := coinbaseIgnored[strings.ToLower(typ)]; ok {
		return fmt.Sprintf("%s: %s", typ, why)
	}
	return fmt.Sprintf("unsupported transaction type %q", typ)
}

// parseRow converts one data row. ok=false means the row is intentionally ignored.
func (c cbCols) parseRow(rec []string, o ParseOptions, occurrences map[string]int) (core.Tx, bool, error) {
	typeName := field(rec, c.typ)
	kind, supported := coinbaseTypes[strings.ToLower(typeName)]
	if !supported {
		return core.Tx{}, false, nil
	}
	ts, err := parseTime(field(rec, c.ts))
	if err != nil {
		return core.Tx{}, false, err
	}
	asset := strings.ToUpper(field(rec, c.asset))
	if asset == "" {
		return core.Tx{}, false, errors.New("missing asset")
	}
	qty, ok := parseMoney(field(rec, c.qty))
	if !ok {
		return core.Tx{}, false, fmt.Errorf("invalid quantity %q", field(rec, c.qty))
	}
	qty = math.Abs(qty)

	// Money columns are only usable as USD when the price currency is USD (or not stated).
	usdOK := true
	if ccy := field(rec, c.ccy); ccy != "" && !strings.EqualFold(ccy, "USD") {
		usdOK = false
	}
	money := func(col int) (float64, bool) {
		if !usdOK {
			return 0, false
		}
		return parseMoney(field(rec, col))
	}
	subtotal, haveSub := money(c.subtotal)
	total, haveTotal := money(c.total)
	fees, haveFees := money(c.fees)
	price, havePrice := money(c.price)
	notes := field(rec, c.notes)

	// valueOf resolves a transaction value: Total, else Subtotal + fees, else Subtotal, else
	// quantity x price. preferSubtotal is used for converts and rewards.
	valueOf := func(preferSubtotal bool) (float64, bool) {
		pick := []func() (float64, bool){
			func() (float64, bool) { return total, haveTotal },
			func() (float64, bool) { return subtotal + fees, haveSub && haveFees },
			func() (float64, bool) { return subtotal, haveSub },
			func() (float64, bool) { return qty * price, havePrice },
		}
		if preferSubtotal {
			pick[0], pick[2] = pick[2], pick[0]
		}
		for _, p := range pick {
			if v, ok := p(); ok {
				return math.Abs(v), true
			}
		}
		return 0, false
	}

	sym := func(a string) core.Leg { return core.Leg{AssetKey: a, Symbol: a} }
	tx := core.Tx{Time: ts, Status: core.TxConfirmed}

	switch kind {
	case cbBuy:
		tx.Kind, tx.Note = core.TxTrade, "buy"
		l := sym(asset)
		l.Amount = qty
		if v, ok := valueOf(false); ok {
			l.USD = fptr(v)
		}
		tx.Legs = []core.Leg{l}
	case cbSell:
		tx.Kind, tx.Note = core.TxTrade, "sell"
		l := sym(asset)
		l.Amount = -qty
		if v, ok := valueOf(false); ok {
			l.USD = fptr(-v)
		}
		tx.Legs = []core.Leg{l}
	case cbConvert:
		m := convertRe.FindStringSubmatch(notes)
		if m == nil {
			return core.Tx{}, false, fmt.Errorf("cannot read the converted amounts from Notes %q", notes)
		}
		fromQty, _ := parseMoney(m[1])
		toQty, ok := parseMoney(m[3])
		if !ok {
			return core.Tx{}, false, fmt.Errorf("invalid converted quantity in Notes %q", notes)
		}
		if fromQty = math.Abs(fromQty); fromQty > 0 && math.Abs(fromQty-qty) > 1e-9*math.Max(1, qty) {
			qty = fromQty // the Notes carry the exact figure when the column was rounded
		}
		from, to := sym(asset), sym(strings.ToUpper(m[4]))
		from.Amount, to.Amount = -qty, math.Abs(toQty)
		if v, ok := valueOf(true); ok {
			from.USD, to.USD = fptr(-v), fptr(v)
		}
		tx.Kind, tx.Note = core.TxTrade, "convert"
		tx.Legs = []core.Leg{from, to}
	case cbSend, cbReceive:
		l := sym(asset)
		if kind == cbSend {
			tx.Kind, tx.Note, l.Amount = core.TxSend, "send", -qty
		} else {
			tx.Kind, tx.Note, l.Amount = core.TxReceive, "receive", qty
		}
		if m := peerRe.FindStringSubmatch(notes); m != nil {
			tx.Counterparty = strings.TrimSpace(m[1])
		}
		tx.Legs = []core.Leg{l}
	case cbReward:
		tx.Kind, tx.Note = core.TxReward, strings.ToLower(typeName)
		l := sym(asset)
		l.Amount = qty
		// Fair market value: quantity x price at transaction; fall back to the money columns.
		if havePrice {
			l.USD = fptr(qty * price)
		} else if v, ok := valueOf(true); ok {
			l.USD = fptr(v)
		}
		tx.Legs = []core.Leg{l}
	}

	tx.SourceID = o.SourceID
	if id := field(rec, c.id); id != "" {
		tx.ExternalID = id
	} else {
		tx.ExternalID = c.hashID(rec, ts, asset, qty, occurrences)
	}
	tx.ID = core.TxID(tx.SourceID, tx.ExternalID)
	return tx, true, nil
}

// hashID derives a stable id from the economic content of a row, independent of the header
// layout and the timestamp style. Identical rows get an occurrence suffix so none is lost.
func (c cbCols) hashID(rec []string, ts time.Time, asset string, qty float64, occurrences map[string]int) string {
	num := func(col int) string {
		if v, ok := parseMoney(field(rec, col)); ok {
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	key := strings.Join([]string{
		strconv.FormatInt(ts.UnixNano(), 10),
		strings.ToLower(field(rec, c.typ)),
		asset,
		strconv.FormatFloat(qty, 'f', -1, 64),
		num(c.subtotal), num(c.total),
		field(rec, c.notes),
	}, "\x1f")
	n := occurrences[key]
	occurrences[key] = n + 1
	sum := sha256.Sum256([]byte(key + "\x1f" + strconv.Itoa(n)))
	return "cb-" + hex.EncodeToString(sum[:12])
}
