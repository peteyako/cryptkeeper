package templates

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/tax"
	"cryptkeeper/internal/tax/report"
)

// taxDownloads are the report files of a year: label and the path after /reports/tax/<year>/.
var taxDownloads = []struct{ Label, Path string }{
	{"Everything (zip)", "all.zip"},
	{"Capital gains CSV", "capital-gains.csv"},
	{"Income CSV", "income.csv"},
	{"Holdings CSV", "holdings.csv"},
	{"Transactions CSV", "transactions.csv"},
	{"Other gains CSV", "other-gains.csv"},
	{"TurboTax CSV", "turbotax.csv"},
	{"TurboTax TXF", "turbotax.txf"},
	{"Form 8949", "8949"},
	{"CPA report", "report"},
	{"IRS forms PDF", "irs-forms.pdf"},
}

// taxDownloadsFor is the downloads a year offers: a Koinly year has no transaction history
// (CryptKeeper never replayed it), so its link is left out.
func taxDownloadsFor(r report.YearReport) []struct{ Label, Path string } {
	if r.Source != "koinly" {
		return taxDownloads
	}
	out := make([]struct{ Label, Path string }, 0, len(taxDownloads))
	for _, d := range taxDownloads {
		if d.Path != "transactions.csv" {
			out = append(out, d)
		}
	}
	return out
}

// txfNote warns that the TXF file can't say "digital asset".
const txfNote = "TurboTax TXF has no digital-asset codes: its rows map to the securities boxes A–F (G→A … L→F). Check them in TurboTax Desktop, or prefer the TurboTax CSV."

// writeOffDeleteSubmit asks first, then deletes the write-off the form names.
const writeOffDeleteSubmit = `confirm('Delete this write-off? The parked units it disposed of come back.') && @post(el.action, {contentType: 'form'})`

func taxDownloadHref(year int, path string) string {
	return "/reports/tax/" + strconv.Itoa(year) + "/" + path
}

type taxTabItem struct{ Key, Label string }

// taxTabs are the tabs of the page's year; Needs review exists only while the year is open.
func taxTabs(p TaxPage) []taxTabItem {
	tabs := []taxTabItem{{"gains", "Capital gains"}, {"income", "Income"}, {"holdings", "Year-end holdings"}}
	if p.ReviewTab {
		label := "Needs review"
		if n := len(p.Report.Issues); n > 0 {
			label += " (" + strconv.Itoa(n) + ")"
		}
		tabs = append(tabs, taxTabItem{"review", label})
	}
	return tabs
}

func taxActiveTab(p TaxPage) string {
	for _, t := range taxTabs(p) {
		if t.Key == p.Tab {
			return t.Key
		}
	}
	return "gains"
}

func taxHref(year int, tab string, page int) string {
	q := url.Values{"year": {strconv.Itoa(year)}, "tab": {tab}}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	return "/reports/tax?" + q.Encode()
}

func taxGet(year int, tab string, page int) string {
	return "@get(" + JSString(taxHref(year, tab, page)) + ")"
}

func taxSourceText(r report.YearReport) string {
	switch {
	case r.Source == "koinly":
		return "Closed · imported from Koinly"
	case r.Closed:
		return "Closed · computed by CryptKeeper"
	}
	return "Open · computed by CryptKeeper"
}

func (p TaxPage) pageRows() []report.Row {
	rows := p.Report.Rows
	from := min(len(rows), (max(p.Page, 1)-1)*TaxPageSize)
	return rows[from:min(len(rows), from+TaxPageSize)]
}

func taxDate(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("01/02/2006")
}

func taxAcquired(r report.Row, loc *time.Location) string {
	if r.Various {
		return "VARIOUS"
	}
	return taxDate(r.Acquired, loc)
}

func taxTerm(t tax.Term) string {
	if t == tax.LongTerm {
		return "Long"
	}
	return "Short"
}

// IncomeTypeName is the display name of an income type.
func IncomeTypeName(t string) string {
	switch t {
	case "reward":
		return "Reward"
	case "income":
		return "Income"
	case "airdrop":
		return "Airdrop"
	case "position":
		return "Other income"
	}
	return t
}

func taxName(m map[string]string, k string) string {
	if v := m[k]; v != "" {
		return v
	}
	if k == tax.Untracked {
		return "Untracked (from Koinly)"
	}
	return strings.TrimPrefix(k, "spl:")
}

func taxHoldingTotals(hs []report.HoldingRow) (cost, value float64) {
	for _, h := range hs {
		cost += h.CostUSD
		value += h.ValueUSD
	}
	return cost, value
}
