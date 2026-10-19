package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/tax"
)

func shortDate(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("01/02/2006")
}

func acquiredDate(row Row, loc *time.Location) string {
	if row.Various {
		return "VARIOUS"
	}
	return shortDate(row.Acquired, loc)
}

// amountText is an amount with up to 8 decimals, trailing zeros trimmed.
func amountText(f float64) string {
	s := strconv.FormatFloat(f, 'f', 8, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// WriteTurboTaxCSV writes the TurboTax Online import: one row per disposal row; gifts and
// lost removals are not in r.Rows.
func WriteTurboTaxCSV(w io.Writer, r YearReport) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"Currency Name", "Purchase Date", "Cost Basis", "Date Sold", "Proceeds"}); err != nil {
		return err
	}
	for _, row := range r.Rows {
		if err := cw.Write([]string{
			safeText(row.Symbol), acquiredDate(row, r.Loc), money(row.CostUSD), shortDate(row.Sold, r.Loc), money(row.ProceedsUSD),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// txfCode is the TXF format 042 reference number of a row's box. TXF has no codes for the
// digital-asset boxes G-L, so they map to the securities boxes A-F (G→A, H→B, I→C, J→D, K→E,
// L→F): the user checks them in TurboTax Desktop (the downloads and README say so).
func txfCode(box string) string {
	switch box {
	case "G":
		return "321"
	case "J":
		return "323"
	case "H":
		return "711"
	case "K":
		return "713"
	case "L":
		return "714"
	default: // I
		return "712"
	}
}

// WriteTXF writes TXF v042 with one record per disposal row.
func WriteTXF(w io.Writer, r YearReport, today time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "V042\nACryptKeeper\nD%s\n^\n", today.Format("01/02/2006"))
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "TD\nN%s\nC1\nL1\nP%s %s\nD%s\nD%s\n$%s\n$%s\n^\n",
			txfCode(row.Box), amountText(row.Amount), row.Symbol, acquiredDate(row, r.Loc),
			shortDate(row.Sold, r.Loc), money(row.CostUSD), money(row.ProceedsUSD))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// cents rounds f to whole cents exactly as money prints it, so a rounded amount and its
// printed text never disagree.
func cents(f float64) int64 {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	neg := strings.HasPrefix(s, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	w, _ := strconv.ParseInt(whole, 10, 64)
	c, _ := strconv.ParseInt(frac, 10, 64)
	c += w * 100
	if neg {
		return -c
	}
	return c
}

// dollars is a cents amount as float64; it prints back as exactly those cents.
func dollars(c int64) float64 { return float64(c) / 100 }

// centsTotal accumulates printed amounts in whole cents.
type centsTotal struct {
	proceeds, cost, gain int64
	count                int
}

// add adds one printed row: its (d), (e) and (h) as they print.
func (t *centsTotal) add(r Form8949Row) {
	t.proceeds += cents(r.Proceeds)
	t.cost += cents(r.Cost)
	t.gain += cents(r.Gain)
	t.count++
}

func (t centsTotal) box() BoxTotal {
	return BoxTotal{Proceeds: dollars(t.proceeds), Cost: dollars(t.cost), Gain: dollars(t.gain), Count: t.count}
}

// Form8949Row is one line of Form 8949 (columns a, b, c, d, e, h). Proceeds (d) and Cost
// (e) are rounded to cents and Gain (h) is d − e, so the printed columns add up.
type Form8949Row struct {
	Description, Acquired, Sold string
	Proceeds, Cost, Gain        float64
	// Symbol and AssetKey are the asset Description names, for renderers that must
	// rewrite the symbol.
	Symbol, AssetKey string
}

// Form8949Part is the rows of one box; Long is true for Part II (J/K/L).
type Form8949Part struct {
	Box   string
	Long  bool
	Rows  []Form8949Row
	Total BoxTotal
}

var boxOrder = []string{"G", "H", "I", "J", "K", "L"}

// boxTerm is the holding period each digital-asset box takes.
var boxTerm = map[string]tax.Term{
	"G": tax.ShortTerm, "H": tax.ShortTerm, "I": tax.ShortTerm,
	"J": tax.LongTerm, "K": tax.LongTerm, "L": tax.LongTerm,
}

// Form8949 groups the report's rows by box in order G..L, omitting empty boxes. A part's
// Total is the sum of its rounded rows. A row whose box is not G..L, or is a box of the
// other part than its term, is an error: no row is ever left off the forms.
func Form8949(r YearReport) ([]Form8949Part, error) {
	by := map[string]*Form8949Part{}
	for _, row := range r.Rows {
		term := row.Term
		if term != tax.LongTerm {
			term = tax.ShortTerm
		}
		if want, ok := boxTerm[row.Box]; !ok || want != term {
			return nil, fmt.Errorf("form 8949: the %s %s row sold %s in %s has box %q, not a %s-term digital-asset box (G-L); check walletBox in tax.yaml",
				amountText(row.Amount), row.Symbol, shortDate(row.Sold, r.Loc), row.WalletLabel, row.Box, term)
		}
		p := by[row.Box]
		if p == nil {
			p = &Form8949Part{Box: row.Box, Long: row.Box >= "J"}
			by[row.Box] = p
		}
		d, e := cents(row.ProceedsUSD), cents(row.CostUSD)
		p.Rows = append(p.Rows, Form8949Row{
			Description: amountText(row.Amount) + " " + row.Symbol,
			Acquired:    acquiredDate(row, r.Loc), Sold: shortDate(row.Sold, r.Loc),
			Proceeds: dollars(d), Cost: dollars(e), Gain: dollars(d - e),
			Symbol: row.Symbol, AssetKey: row.Asset,
		})
	}
	var out []Form8949Part
	for _, box := range boxOrder {
		if p := by[box]; p != nil {
			p.Total = PageTotal(p.Rows)
			out = append(out, *p)
		}
	}
	return out, nil
}

// Pages splits a part into form pages of at most perPage rows.
func Pages(p Form8949Part, perPage int) [][]Form8949Row {
	if perPage < 1 {
		perPage = 1
	}
	var pages [][]Form8949Row
	for rows := p.Rows; len(rows) > 0; {
		n := min(perPage, len(rows))
		pages = append(pages, rows[:n])
		rows = rows[n:]
	}
	return pages
}

// PageTotal is the line-2 totals row of one form page: the sum of the printed rows, in
// whole cents.
func PageTotal(rows []Form8949Row) BoxTotal {
	var t centsTotal
	for _, r := range rows {
		t.add(r)
	}
	return t.box()
}

// Money8949 formats a form amount: 2 decimals, losses in parentheses.
func Money8949(f float64) string {
	s := money(f)
	if neg, ok := strings.CutPrefix(s, "-"); ok {
		return "(" + neg + ")"
	}
	return s
}

// ScheduleDLine is one Schedule D line total.
type ScheduleDLine struct {
	Line, Box                         string
	Proceeds, Cost, Adjustments, Gain float64
}

var scheduleDLines = []struct{ line, box string }{
	{"1b", "G"}, {"2", "H"}, {"3", "I"}, {"8b", "J"}, {"9", "K"}, {"10", "L"},
}

// ScheduleD returns the six Schedule D lines for the digital-asset boxes, always all
// present, with adjustments 0. Each line is its box's Form 8949 total, which is the sum of
// that box's page totals. It fails as Form8949 does.
func ScheduleD(r YearReport) ([]ScheduleDLine, error) {
	parts, err := Form8949(r)
	if err != nil {
		return nil, err
	}
	tot := map[string]BoxTotal{}
	for _, p := range parts {
		tot[p.Box] = p.Total
	}
	out := make([]ScheduleDLine, len(scheduleDLines))
	for i, l := range scheduleDLines {
		t := tot[l.box]
		out[i] = ScheduleDLine{Line: l.line, Box: l.box, Proceeds: t.Proceeds, Cost: t.Cost, Gain: t.Gain}
	}
	return out, nil
}

// Totals is the short-term (Part I) and long-term (Part II) totals as Schedule D prints
// them: the sums of its lines (each a box's Form 8949 total), in whole cents, so a summary
// built from them never disagrees with the forms by a cent. It fails as Form8949 does.
func Totals(r YearReport) (short, long BoxTotal, err error) {
	parts, err := Form8949(r)
	if err != nil {
		return BoxTotal{}, BoxTotal{}, err
	}
	var s, l centsTotal
	for _, p := range parts {
		t := &s
		if p.Long {
			t = &l
		}
		t.proceeds += cents(p.Total.Proceeds)
		t.cost += cents(p.Total.Cost)
		t.gain += cents(p.Total.Gain)
		t.count += p.Total.Count
	}
	return s.box(), l.box(), nil
}

// ScheduleDNet returns line 7 (net short-term: lines 1b + 2 + 3, column h) and line 15
// (net long-term: lines 8b + 9 + 10), summed in whole cents.
func ScheduleDNet(lines []ScheduleDLine) (short, long float64) {
	var s, l int64
	for _, line := range lines {
		if line.Box >= "J" {
			l += cents(line.Gain)
		} else {
			s += cents(line.Gain)
		}
	}
	return dollars(s), dollars(l)
}
