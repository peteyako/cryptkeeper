package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/tax"
)

// writeTitled writes a title line, a blank line, the header and the records, all through
// encoding/csv so labels with commas or quotes are quoted.
func writeTitled(w io.Writer, title string, header []string, records [][]string) error {
	if _, err := io.WriteString(w, title+"\n\n"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	if err := cw.WriteAll(records); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// money formats USD with 2 decimals, never as "-0.00".
func money(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	if s == "-0.00" {
		return "0.00"
	}
	return s
}

func amt8(f float64) string { return strconv.FormatFloat(f, 'f', 8, 64) }

// koinlyTime is the MM/DD/YYYY HH:MM form of the Koinly reports, in the tax timezone.
func koinlyTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("01/02/2006 15:04")
}

func termLabel(t tax.Term) string {
	if t == tax.LongTerm {
		return "Long term"
	}
	return "Short term"
}

// WriteGains writes the Koinly-format capital gains report. Like Koinly, it leaves out the
// position-gain rows (they are in the Other gains report); Form 8949, Schedule D, TurboTax and
// TXF still include every disposal row.
func WriteGains(w io.Writer, r YearReport) error {
	recs := make([][]string, 0, len(r.Rows))
	for _, row := range r.Rows {
		if row.Source == tax.SourcePositionGain {
			continue
		}
		acquired := "VARIOUS"
		if !row.Various {
			acquired = koinlyTime(row.Acquired, r.Loc)
		}
		recs = append(recs, []string{
			koinlyTime(row.Sold, r.Loc), acquired, safeText(row.Symbol), amt8(row.Amount),
			money(row.CostUSD), money(row.ProceedsUSD), money(row.GainUSD),
			row.Note, safeText(row.WalletLabel), termLabel(row.Term),
		})
	}
	return writeTitled(w, fmt.Sprintf("Capital gains report %d", r.Year),
		[]string{"Date Sold", "Date Acquired", "Asset", "Amount", "Cost (USD)", "Proceeds (USD)", "Gain / loss", "Notes", "Wallet Name", "Holding period"},
		recs)
}

// WriteOtherGains writes the gains created by the position-gain tag (Koinly's "other gains"),
// which WriteGains excludes. Header only when there are none.
func WriteOtherGains(w io.Writer, r YearReport) error {
	var recs [][]string
	for _, row := range r.Rows {
		if row.Source != tax.SourcePositionGain {
			continue
		}
		recs = append(recs, []string{
			koinlyTime(row.Sold, r.Loc), safeText(row.Symbol), amt8(row.Amount), money(row.ProceedsUSD),
			"Position gain", safeText(row.WalletLabel),
		})
	}
	return writeTitled(w, fmt.Sprintf("Other gains report %d", r.Year),
		[]string{"Date", "Asset", "Amount", "Value (USD)", "Type", "Wallet Name"}, recs)
}

var incomeTypes = map[string]string{
	"reward": "Reward", "income": "Income", "airdrop": "Airdrop", "position": "Other income",
}

// WriteIncome writes the Koinly-format income report. rs resolves symbols and wallet labels.
func WriteIncome(w io.Writer, r YearReport, rs Resolver) error {
	recs := make([][]string, 0, len(r.Income))
	for _, in := range r.Income {
		sym, _ := rs.symbol(in.Asset)
		typ, ok := incomeTypes[in.Type]
		if !ok {
			typ = "Income"
		}
		recs = append(recs, []string{
			koinlyTime(in.Date, r.Loc), safeText(sym), amt8(in.Amount), money(in.ValueUSD), typ, "", safeText(rs.label(in.Wallet)),
		})
	}
	return writeTitled(w, fmt.Sprintf("Income report %d", r.Year),
		[]string{"Date", "Asset", "Amount", "Value (USD)", "Type", "Description", "Wallet Name"}, recs)
}

// priceText formats a unit price: 2 decimals, 6 below one dollar.
func priceText(p float64) string {
	if p < 1 {
		return strconv.FormatFloat(p, 'f', 6, 64)
	}
	return money(p)
}

// WriteHoldings writes the end-of-year balances report.
func WriteHoldings(w io.Writer, r YearReport) error {
	var recs [][]string
	var cost, value float64
	for _, h := range r.Holdings {
		desc := "-"
		if h.Priced {
			desc = fmt.Sprintf("@ $%s per %s", priceText(h.Price), h.Symbol)
		}
		recs = append(recs, []string{
			safeText(fmt.Sprintf("%s (%s)", h.Symbol, h.Name)), amt8(h.Quantity), money(h.CostUSD), money(h.ValueUSD), desc,
		})
		cost += h.CostUSD
		value += h.ValueUSD
	}
	recs = append(recs, []string{"Total", "-", money(cost), money(value), "-"})
	// As at the report's date: Dec 31 23:59 for a finished year, now for the year in progress.
	asOf := r.AsOf
	if asOf.IsZero() {
		loc := r.Loc
		if loc == nil {
			loc = time.UTC
		}
		asOf = yearEnd(r.Year, loc)
	}
	return writeTitled(w, "Balances as at "+koinlyTime(asOf, r.Loc),
		[]string{"Asset", "Quantity", "Cost (USD)", "Value (USD)", "Description"}, recs)
}

// koinlyType maps an activity kind (and, for kinds without a clear direction, its legs) to
// a Koinly transaction type.
func koinlyType(tx core.Tx) string {
	switch tx.Kind {
	case core.TxTrade, core.TxSwap:
		return "exchange"
	case core.TxReceive, core.TxReward:
		return "crypto_deposit"
	case core.TxSend:
		return "crypto_withdrawal"
	case core.TxTransfer:
		return "transfer"
	}
	var in, out bool
	for _, l := range tx.Legs {
		in = in || l.Amount > 0
		out = out || l.Amount < 0
	}
	switch {
	case in && out:
		return "exchange"
	case in:
		return "crypto_deposit"
	default:
		return "crypto_withdrawal"
	}
}

func legUSD(l core.Leg) string {
	if l.USD == nil {
		return ""
	}
	return money(math.Abs(*l.USD))
}

// safeText guards free text against spreadsheet formula injection.
func safeText(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}

// WriteTransactions writes the Koinly-format transaction history of year from activity.
// The cost basis and gain of a tx are the sums of its disposal rows (by TxID; the fee row
// counts toward the gain but not the cost basis, as the fee has its own columns: a deliberate
// choice). A transfer is one row per pair of sides (sending wallet = the side with the negative
// leg); a side whose peer is not in txs is written alone. The tag
// comes from the tx's income row (reward/income/airdrop) or gift/lost row; a gifts/lost row
// whose Note is "lost" is Lost, any other is Gift. Failed txs are skipped.
func WriteTransactions(w io.Writer, year int, txs []core.Tx, res tax.Result, rs Resolver, loc *time.Location) error {
	if loc == nil {
		loc = time.UTC
	}
	return WriteTransactionsRange(w, year, time.Date(year, 1, 1, 0, 0, 0, 0, loc), time.Date(year+1, 1, 1, 0, 0, 0, 0, loc), txs, res, rs, loc)
}

// WriteTransactionsRange is WriteTransactions for the txs dated in [from, to) rather than in
// the calendar year: the first open year after a year closed in another timezone starts at
// that close's midnight (see tax.OpenYearStart), as the engine's replay does.
func WriteTransactionsRange(w io.Writer, year int, from, to time.Time, txs []core.Tx, res tax.Result, rs Resolver, loc *time.Location) error {
	if loc == nil {
		loc = time.UTC
	}
	type agg struct{ cost, gain float64 }
	disp := map[string]*agg{}
	for _, ds := range [][]tax.Disposal{res.Disposals, res.GiftsLost} {
		for _, d := range ds {
			a := disp[d.TxID]
			if a == nil {
				a = &agg{}
				disp[d.TxID] = a
			}
			if d.Note != "Fee" {
				a.cost += d.CostUSD
			}
			a.gain += d.GainUSD
		}
	}
	tags := map[string]string{}
	for _, in := range res.Income {
		if t, ok := map[string]string{"reward": "Reward", "income": "Income", "airdrop": "Airdrop"}[in.Type]; ok {
			tags[in.TxID] = t
		}
	}
	for _, d := range res.GiftsLost {
		if strings.EqualFold(d.Note, "lost") {
			tags[d.TxID] = "Lost"
		} else {
			tags[d.TxID] = "Gift"
		}
	}

	sorted := slices.Clone(txs)
	slices.SortStableFunc(sorted, func(a, b core.Tx) int {
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	byID := make(map[string]*core.Tx, len(sorted))
	idx := make(map[string]int, len(sorted))
	for i := range sorted {
		byID[sorted[i].ID] = &sorted[i]
		idx[sorted[i].ID] = i
	}
	firstLeg := func(tx *core.Tx, neg bool) *core.Leg {
		for i := range tx.Legs {
			if (tx.Legs[i].Amount < 0) == neg && tx.Legs[i].Amount != 0 {
				return &tx.Legs[i]
			}
		}
		return nil
	}
	emitted := map[string]bool{}
	var recs [][]string
	for i := range sorted {
		tx := &sorted[i]
		if tx.Status == core.TxFailed || emitted[tx.ID] {
			continue
		}
		// A pair belongs to the year of its earlier side (the sort puts it first), so a pair
		// straddling a year end is written once; the later side is skipped here.
		if peer := byID[tx.PeerID]; tx.Kind == core.TxTransfer && peer != nil && peer != tx &&
			peer.Status != core.TxFailed && idx[peer.ID] < i {
			continue
		}
		if tx.Time.Before(from) || !tx.Time.Before(to) {
			continue
		}
		emitted[tx.ID] = true
		sendTx, recvTx := tx, tx
		sent, recv := firstLeg(tx, true), firstLeg(tx, false)
		if tx.Kind == core.TxTransfer {
			if peer := byID[tx.PeerID]; peer != nil && peer != tx && peer.Status != core.TxFailed {
				emitted[peer.ID] = true
				// One row per pair: the side with the negative leg sends, the other receives.
				if sent == nil {
					sendTx, recvTx = peer, tx
				} else {
					sendTx, recvTx = tx, peer
				}
				sent, recv = firstLeg(sendTx, true), firstLeg(recvTx, false)
			}
		}
		row := make([]string, 20)
		row[0] = tx.Time.UTC().Format("2006-01-02 15:04:05 UTC")
		row[1] = koinlyType(*tx)
		row[2] = tags[tx.ID]
		if row[2] == "" {
			row[2] = tags[sendTx.ID]
		}
		var net string
		if sent != nil {
			sym, _ := rs.symbol(sent.AssetKey)
			row[3], row[4], row[5] = safeText(rs.label(sendTx.SourceID)), amt8(-sent.Amount), safeText(sym)
			if a := disp[sendTx.ID]; a != nil {
				row[6] = money(a.cost)
			}
			net = legUSD(*sent)
		}
		if recv != nil {
			sym, _ := rs.symbol(recv.AssetKey)
			dest := recvTx.SourceID
			if tx.Kind == core.TxTransfer && recvTx == tx && sendTx == tx && tx.PeerSource != "" {
				dest = tx.PeerSource
			}
			row[7], row[8], row[9], row[10] = safeText(rs.label(dest)), amt8(recv.Amount), safeText(sym), legUSD(*recv)
			if net == "" {
				net = row[10]
			}
		}
		if sendTx.Fee != nil {
			sym, _ := rs.symbol(sendTx.Fee.AssetKey)
			row[11], row[12], row[15] = amt8(math.Abs(sendTx.Fee.Amount)), safeText(sym), legUSD(*sendTx.Fee)
		}
		var gain float64
		var has bool
		ids := []string{sendTx.ID}
		if recvTx != sendTx {
			ids = append(ids, recvTx.ID)
		}
		for _, id := range ids {
			if a := disp[id]; a != nil {
				gain += a.gain
				has = true
			}
		}
		if has {
			row[13] = money(gain)
		}
		row[14], row[18], row[19] = net, sendTx.TxHash, safeText(tx.Note)
		recs = append(recs, row)
	}
	return writeTitled(w, fmt.Sprintf("Transaction report %d", year),
		[]string{"Date", "Type", "Tag", "Sending Wallet", "Sent Amount", "Sent Currency", "Sent Cost Basis", "Receiving Wallet", "Received Amount", "Received Currency", "Received Cost Basis", "Fee Amount", "Fee Currency", "Gain (USD)", "Net Value (USD)", "Fee Value (USD)", "TxSrc", "TxDest", "TxHash", "Description"},
		recs)
}
