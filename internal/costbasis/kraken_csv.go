package costbasis

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/sources/kraken"
)

const krakenCSVSource = "kraken-csv"

type krakenRow struct {
	line    int
	id      string // refid, or txid when the refid is blank
	t       time.Time
	typ     string
	subtype string
	asset   string // normalized symbol
	amount  float64
	fee     float64
}

// ParseKrakenLedgersCSV parses Kraken's "Ledgers" export (columns "txid","refid","time",
// "type","subtype","aclass","asset","wallet","amount","fee","balance"; wallet is optional).
//
// Rows are grouped by refid, like the API, into one core.Tx each (ExternalID = refid):
//
//   - trade / spend / receive rows with both signs: kind trade with the legs in ledger
//     order. A lone receive row is a receive, a lone spend a send.
//   - deposit: receive; withdrawal: send.
//   - staking / earn rewards and dividends (positive amount, or subtype "airdrop"/"reward"):
//     reward. Allocation moves (earn allocation/deallocation/autoallocation/migration,
//     staking <-> spot and futures transfers): transfer, which never changes cost basis.
//   - other rows: a plain positive-only transfer is a receive, negative-only a send,
//     anything else kind other.
//
// Assets are normalized with kraken.NormalizeAsset (XXBT -> BTC, DOT28.S -> DOT, ZUSD -> USD).
// Leg amounts are the ledger "amount" column; the ledger fee is carried in Tx.Fee (negative,
// not part of the legs). When a trade has exactly one USD/USDC/USDT leg, the other leg's USD
// value is set from it: for a buy the USD paid plus a fee charged in that currency (the
// acquisition cost), for a sell the USD received minus such a fee. Stablecoin legs carry their
// own face value. Trades quoted in EUR or against another crypto leave USD nil; the valuation
// step fills it from price history. Times are UTC. The result is sorted by time.
func ParseKrakenLedgersCSV(r io.Reader, opts ...ParseOptions) ([]core.Tx, error) {
	o := resolveOptions(opts, krakenCSVSource)
	cr := csv.NewReader(newCSVInput(r))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true

	var (
		refCol, txidCol, timeCol, typeCol, subCol, assetCol, amountCol, feeCol int
		haveHeader                                                             bool
		order                                                                  []string
		groups                                                                 = map[string][]krakenRow{}
	)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("kraken csv: %w", err)
		}
		line, _ := cr.FieldPos(0)
		if allBlank(rec) {
			continue
		}
		if !haveHeader {
			h := newHeaderIndex(rec)
			refCol, txidCol, timeCol, typeCol = h.col("refid"), h.col("txid"), h.col("time"), h.col("type")
			subCol, assetCol, amountCol, feeCol = h.col("subtype"), h.col("asset"), h.col("amount"), h.col("fee")
			if refCol < 0 || timeCol < 0 || typeCol < 0 || assetCol < 0 || amountCol < 0 {
				return nil, fmt.Errorf("kraken csv line %d: header must contain refid, time, type, asset and amount", line)
			}
			haveHeader = true
			continue
		}

		row := krakenRow{line: line, typ: strings.ToLower(field(rec, typeCol)), subtype: strings.ToLower(field(rec, subCol))}
		if row.id = field(rec, refCol); row.id == "" {
			row.id = field(rec, txidCol)
		}
		if row.id == "" {
			return nil, fmt.Errorf("kraken csv line %d: row has neither refid nor txid", line)
		}
		if row.t, err = parseTime(field(rec, timeCol)); err != nil {
			return nil, fmt.Errorf("kraken csv line %d: %w", line, err)
		}
		code := field(rec, assetCol)
		if code == "" {
			return nil, fmt.Errorf("kraken csv line %d: missing asset", line)
		}
		row.asset, _ = kraken.NormalizeAsset(code)
		var ok bool
		if row.amount, ok = parseMoney(field(rec, amountCol)); !ok {
			return nil, fmt.Errorf("kraken csv line %d: invalid amount %q", line, field(rec, amountCol))
		}
		if s := field(rec, feeCol); s != "" {
			if row.fee, ok = parseMoney(s); !ok {
				return nil, fmt.Errorf("kraken csv line %d: invalid fee %q", line, s)
			}
			row.fee = math.Abs(row.fee)
		}
		if row.amount == 0 && row.fee == 0 {
			o.skip(line, "zero-amount %s row %s", row.typ, row.id)
			continue
		}
		if _, seen := groups[row.id]; !seen {
			order = append(order, row.id)
		}
		groups[row.id] = append(groups[row.id], row)
	}
	if !haveHeader {
		return nil, errors.New("kraken csv: no header row found")
	}

	txs := make([]core.Tx, 0, len(order))
	for _, ref := range order {
		txs = append(txs, buildKrakenTx(o.SourceID, ref, groups[ref]))
	}
	sort.SliceStable(txs, func(i, j int) bool { return txs[i].Time.Before(txs[j].Time) })
	return txs, nil
}

func buildKrakenTx(sourceID, ref string, rows []krakenRow) core.Tx {
	tx := core.Tx{
		ID: core.TxID(sourceID, ref), SourceID: sourceID, ExternalID: ref,
		Time: rows[0].t, Status: core.TxConfirmed,
	}
	for _, r := range rows {
		if r.t.Before(tx.Time) {
			tx.Time = r.t
		}
	}
	tx.Kind = classifyKraken(rows)

	// Legs: ledger order, same asset and same sign merged.
	type legKey struct {
		asset string
		neg   bool
	}
	idx := map[legKey]int{}
	for _, r := range rows {
		if r.amount == 0 {
			continue
		}
		k := legKey{r.asset, r.amount < 0}
		if i, ok := idx[k]; ok {
			tx.Legs[i].Amount += r.amount
			continue
		}
		idx[k] = len(tx.Legs)
		tx.Legs = append(tx.Legs, core.Leg{AssetKey: r.asset, Symbol: r.asset, Amount: r.amount})
	}

	// Fees: one fee asset goes to Tx.Fee. A fee in a second asset (rare) is folded into the
	// leg of that asset so no cost disappears.
	var feeAsset string
	var feeTotal float64
	extra := map[string]float64{}
	for _, r := range rows {
		if r.fee == 0 {
			continue
		}
		switch {
		case feeAsset == "" || feeAsset == r.asset:
			feeAsset = r.asset
			feeTotal += r.fee
		default:
			extra[r.asset] += r.fee
		}
	}
	if feeTotal > 0 {
		tx.Fee = &core.Leg{AssetKey: feeAsset, Symbol: feeAsset, Amount: -feeTotal}
	}
	extraAssets := make([]string, 0, len(extra))
	for asset := range extra {
		extraAssets = append(extraAssets, asset)
	}
	sort.Strings(extraAssets)
	for _, asset := range extraAssets {
		fee := extra[asset]
		folded := false
		for i := range tx.Legs {
			if tx.Legs[i].AssetKey == asset {
				tx.Legs[i].Amount -= fee
				folded = true
				break
			}
		}
		if !folded {
			tx.Legs = append(tx.Legs, core.Leg{AssetKey: asset, Symbol: asset, Amount: -fee})
		}
	}

	valueKrakenLegs(&tx)
	return tx
}

// valueKrakenLegs sets Leg.USD from USD/USDC/USDT legs (see ParseKrakenLedgersCSV).
func valueKrakenLegs(tx *core.Tx) {
	stable := -1
	nStable := 0
	for i := range tx.Legs {
		if core.Pegged[tx.Legs[i].AssetKey] {
			tx.Legs[i].USD = fptr(tx.Legs[i].Amount)
			nStable++
			stable = i
		}
	}
	if tx.Kind != core.TxTrade || len(tx.Legs) != 2 || nStable != 1 {
		return
	}
	s, other := &tx.Legs[stable], &tx.Legs[1-stable]
	if (s.Amount < 0) == (other.Amount < 0) {
		return
	}
	v := math.Abs(s.Amount)
	if tx.Fee != nil && tx.Fee.AssetKey == s.AssetKey {
		if s.Amount < 0 { // paid in the quote currency: the fee is part of the cost
			v += math.Abs(tx.Fee.Amount)
		} else { // received in the quote currency: the fee reduces the proceeds
			v = math.Max(0, v-math.Abs(tx.Fee.Amount))
		}
	}
	if other.Amount < 0 {
		v = -v
	}
	other.USD = fptr(v)
}

// classifyKraken decides the activity kind of one refid group.
func classifyKraken(rows []krakenRow) core.TxKind {
	has := func(types ...string) bool {
		for _, r := range rows {
			for _, t := range types {
				if r.typ == t {
					return true
				}
			}
		}
		return false
	}
	var pos, neg bool
	for _, r := range rows {
		pos = pos || r.amount > 0
		neg = neg || r.amount < 0
	}
	first := rows[0]
	switch {
	case has("trade", "spend", "receive"):
		switch {
		case pos && neg:
			return core.TxTrade
		case pos:
			return core.TxReceive
		default:
			return core.TxSend
		}
	case has("withdrawal"):
		return core.TxSend
	case has("deposit"):
		return core.TxReceive
	case has("staking", "earn", "transfer"):
		for _, r := range rows {
			if isAllocation(r.subtype) {
				return core.TxTransfer
			}
		}
		switch {
		case strings.Contains(first.subtype, "reward") || first.subtype == "airdrop":
			return core.TxReward
		case first.typ == "staking" || first.typ == "earn":
			if pos && !neg {
				return core.TxReward
			}
			return core.TxTransfer
		case pos && !neg:
			return core.TxReceive
		case neg && !pos:
			return core.TxSend
		}
		return core.TxOther
	case has("dividend"):
		return core.TxReward
	}
	return core.TxOther
}

// isAllocation reports whether a ledger subtype moves funds between the user's own Kraken
// wallets (spot, staking, earn, futures) instead of earning or spending.
func isAllocation(sub string) bool {
	switch {
	case sub == "":
		return false
	case strings.Contains(sub, "allocation"), strings.Contains(sub, "migration"),
		strings.Contains(sub, "futures"), strings.HasPrefix(sub, "spot"),
		strings.HasSuffix(sub, "fromspot"), strings.HasSuffix(sub, "tospot"),
		strings.HasSuffix(sub, "fromstaking"), strings.HasSuffix(sub, "tostaking"):
		return true
	}
	return false
}
