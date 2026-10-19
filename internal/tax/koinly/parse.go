// Package koinly parses Koinly's CSV exports (holdings, transactions, capital gains,
// income) so a year's closing state can seed CryptKeeper's tax lots.
package koinly

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/tax"
)

// Kind identifies a Koinly report.
type Kind string

const (
	KindHoldings     Kind = "holdings"
	KindTransactions Kind = "transactions"
	KindGains        Kind = "gains"
	KindIncome       Kind = "income"
	KindOtherGains   Kind = "other-gains"
)

var headers = map[string]Kind{
	"Asset,Quantity,Cost (USD),Value (USD),Description": KindHoldings,
	"Date,Type,Tag,Sending Wallet,Sent Amount,Sent Currency,Sent Cost Basis,Receiving Wallet,Received Amount,Received Currency,Received Cost Basis,Fee Amount,Fee Currency,Gain (USD),Net Value (USD),Fee Value (USD),TxSrc,TxDest,TxHash,Description": KindTransactions,
	"Date Sold,Date Acquired,Asset,Amount,Cost (USD),Proceeds (USD),Gain / loss,Notes,Wallet Name,Holding period":                                                                                                                                      KindGains,
	"Date,Asset,Amount,Value (USD),Type,Description,Wallet Name": KindIncome,
	"Date,Asset,Amount,Value (USD),Type,Wallet Name":             KindOtherGains,
}

var errNotKoinly = errors.New("not a Koinly report")

// split returns the title (line 1), the header (line 3) and the body starting at the
// header, tolerating a BOM and CRLF.
func split(raw []byte) (title, header string, body []byte, err error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	parts := bytes.SplitN(raw, []byte("\n"), 4)
	if len(parts) < 3 {
		return "", "", nil, errNotKoinly
	}
	title = strings.TrimSpace(string(parts[0]))
	header = strings.TrimSpace(string(parts[2]))
	rest := parts[2]
	if len(parts) == 4 {
		rest = append(append([]byte{}, parts[2]...), append([]byte("\n"), parts[3]...)...)
	}
	return title, header, rest, nil
}

// Detect identifies a report by the header on line 3.
func Detect(raw []byte) (Kind, error) {
	_, header, _, err := split(raw)
	if err != nil {
		return "", err
	}
	if k, ok := headers[header]; ok {
		return k, nil
	}
	return "", errNotKoinly
}

// records parses the CSV from the header line, returning data rows (header dropped).
func records(raw []byte, want Kind) (title string, rows [][]string, err error) {
	title, _, body, err := split(raw)
	if err != nil {
		return "", nil, err
	}
	k, err := Detect(raw)
	if err != nil {
		return "", nil, err
	}
	if k != want {
		return "", nil, fmt.Errorf("expected a %s report, got %s", want, k)
	}
	r := csv.NewReader(bytes.NewReader(body))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, err
		}
		rows = append(rows, rec)
	}
	if len(rows) > 0 {
		rows = rows[1:]
	}
	return title, rows, nil
}

// blank reports whether every cell of rec is empty.
func blank(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// rowErr labels an error with the data row number and its line in the file (row + 3).
func rowErr(what string, n int, err error) error {
	return fmt.Errorf("%s row %d (file line %d): %w", what, n+1, n+4, err)
}

var errMissingDate = errors.New("missing date")

func cell(rec []string, i int) string {
	if i < len(rec) {
		return strings.TrimSpace(rec[i])
	}
	return ""
}

// num parses a number that may carry thousands separators; empty and "-" are 0.
func num(s string) (float64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "-" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// nums parses the given columns of rec in order.
func nums(rec []string, cols ...int) ([]float64, error) {
	out := make([]float64, len(cols))
	for i, c := range cols {
		v, err := num(cell(rec, c))
		if err != nil {
			return nil, fmt.Errorf("column %d: %w", c+1, err)
		}
		out[i] = v
	}
	return out, nil
}

// Holding is one row of the year-end holdings report.
type Holding struct {
	Asset, Symbol, Name         string
	Quantity, CostUSD, ValueUSD float64
}

// splitAsset splits "SYM (Name)" at the " (" that opens the parenthesis group closed by
// the final ")", so a name may itself hold parentheses: "WETH (Wrapped Ether (Wormhole))"
// is "WETH", "Wrapped Ether (Wormhole)". Without such a group it splits at the last " (".
func splitAsset(a string) (sym, name string) {
	if !strings.HasSuffix(a, ")") {
		return a, ""
	}
	depth := 0
scan:
	for i := len(a) - 1; i > 0; i-- {
		switch a[i] {
		case ')':
			depth++
		case '(':
			if depth--; depth == 0 {
				if a[i-1] == ' ' {
					return a[:i-1], a[i+1 : len(a)-1]
				}
				break scan
			}
		}
	}
	if i := strings.LastIndex(a, " ("); i >= 0 {
		return a[:i], a[i+2 : len(a)-1]
	}
	return a, ""
}

// ParseHoldings parses the year-end holdings report. asOf is the title's timestamp as
// UTC wall time; the caller re-interprets it in the tax zone.
func ParseHoldings(raw []byte) (asOf time.Time, rows []Holding, err error) {
	title, recs, err := records(raw, KindHoldings)
	if err != nil {
		return time.Time{}, nil, err
	}
	ts := strings.TrimSpace(strings.TrimPrefix(title, "Balances as at"))
	if asOf, err = time.ParseInLocation("01/02/2006 15:04", ts, time.UTC); err != nil {
		return time.Time{}, nil, fmt.Errorf("holdings title %q: %w", title, err)
	}
	for n, rec := range recs {
		if blank(rec) {
			continue
		}
		asset := cell(rec, 0)
		if asset == "Total" {
			continue
		}
		if asset == "" {
			return time.Time{}, nil, rowErr("holdings", n, errors.New("missing asset"))
		}
		v, err := nums(rec, 1, 2, 3)
		if err != nil {
			return time.Time{}, nil, rowErr("holdings", n, err)
		}
		h := Holding{Asset: asset, Quantity: v[0], CostUSD: v[1], ValueUSD: v[2]}
		h.Symbol, h.Name = splitAsset(asset)
		rows = append(rows, h)
	}
	return asOf, rows, nil
}

// Txn is one row of the transaction history.
type Txn struct {
	Time                               time.Time
	Type, Tag                          string
	SendWallet                         string
	SentAmount                         float64
	SentCurrency                       string
	SentCost                           float64
	RecvWallet                         string
	RecvAmount                         float64
	RecvCurrency                       string
	RecvCost                           float64
	FeeAmount                          float64
	FeeCurrency                        string
	Gain, NetValue, FeeValue           float64
	TxSrc, TxDest, TxHash, Description string
}

// ParseTransactions parses the transaction history report.
func ParseTransactions(raw []byte) ([]Txn, error) {
	_, recs, err := records(raw, KindTransactions)
	if err != nil {
		return nil, err
	}
	var out []Txn
	for n, rec := range recs {
		if blank(rec) {
			continue
		}
		if cell(rec, 0) == "" {
			return nil, rowErr("transaction", n, errMissingDate)
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05 UTC", cell(rec, 0), time.UTC)
		if err != nil {
			return nil, rowErr("transaction", n, err)
		}
		v, err := nums(rec, 4, 6, 8, 10, 11, 13, 14, 15)
		if err != nil {
			return nil, rowErr("transaction", n, err)
		}
		out = append(out, Txn{
			Time: t, Type: cell(rec, 1), Tag: cell(rec, 2),
			SendWallet: cell(rec, 3), SentAmount: v[0], SentCurrency: cell(rec, 5), SentCost: v[1],
			RecvWallet: cell(rec, 7), RecvAmount: v[2], RecvCurrency: cell(rec, 9), RecvCost: v[3],
			FeeAmount: v[4], FeeCurrency: cell(rec, 12), Gain: v[5], NetValue: v[6], FeeValue: v[7],
			TxSrc: cell(rec, 16), TxDest: cell(rec, 17), TxHash: cell(rec, 18), Description: cell(rec, 19),
		})
	}
	return out, nil
}

const localLayout = "01/02/2006 15:04"

// ParseGains parses the capital gains report; dates are read in loc.
func ParseGains(raw []byte, loc *time.Location) ([]tax.Disposal, error) {
	_, recs, err := records(raw, KindGains)
	if err != nil {
		return nil, err
	}
	var out []tax.Disposal
	for n, rec := range recs {
		if blank(rec) {
			continue
		}
		if cell(rec, 0) == "" {
			return nil, rowErr("gains", n, errMissingDate)
		}
		sold, err := time.ParseInLocation(localLayout, cell(rec, 0), loc)
		if err != nil {
			return nil, rowErr("gains", n, err)
		}
		d := tax.Disposal{Sold: sold, Asset: cell(rec, 2), Wallet: cell(rec, 8), Note: cell(rec, 7)}
		if acq := cell(rec, 1); acq == "" {
			return nil, rowErr("gains", n, errors.New("missing Date Acquired"))
		} else if strings.EqualFold(acq, "various") {
			d.Various = true
		} else if d.Acquired, err = time.ParseInLocation(localLayout, acq, loc); err != nil {
			return nil, rowErr("gains", n, err)
		}
		v, err := nums(rec, 3, 4, 5, 6)
		if err != nil {
			return nil, rowErr("gains", n, err)
		}
		d.Amount, d.CostUSD, d.ProceedsUSD, d.GainUSD = v[0], v[1], v[2], v[3]
		switch strings.ToLower(cell(rec, 9)) {
		case "short term":
			d.Term = tax.ShortTerm
		case "long term":
			d.Term = tax.LongTerm
		default:
			return nil, rowErr("gains", n, fmt.Errorf("unknown holding period %q", cell(rec, 9)))
		}
		out = append(out, d)
	}
	return out, nil
}

// ParseIncome parses the income report; dates are read in loc.
func ParseIncome(raw []byte, loc *time.Location) ([]tax.Income, error) {
	_, recs, err := records(raw, KindIncome)
	if err != nil {
		return nil, err
	}
	var out []tax.Income
	for n, rec := range recs {
		if blank(rec) {
			continue
		}
		if cell(rec, 0) == "" {
			return nil, rowErr("income", n, errMissingDate)
		}
		t, err := time.ParseInLocation(localLayout, cell(rec, 0), loc)
		if err != nil {
			return nil, rowErr("income", n, err)
		}
		v, err := nums(rec, 2, 3)
		if err != nil {
			return nil, rowErr("income", n, err)
		}
		out = append(out, tax.Income{Date: t, Asset: cell(rec, 1), Amount: v[0], ValueUSD: v[1],
			Type: cell(rec, 4), Wallet: cell(rec, 6)})
	}
	return out, nil
}
