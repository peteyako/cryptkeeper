package templates

import (
	"strconv"

	"github.com/a-h/templ"
)

// The display decisions of the Reports screen.

// anyBasis reports whether at least one row has a known cost.
func (p ReportsPage) anyBasis() bool {
	for _, r := range p.Rows {
		if r.HasBasis {
			return true
		}
	}
	return false
}

// unrealizedPct is the total unrealized P/L as a percent of the total cost basis.
func (p ReportsPage) unrealizedPct() (float64, bool) {
	return UnrealizedPct(&p.TotalUnrealized, &p.TotalBasis)
}

// CoverageText says how much of the portfolio's value has a known cost:
// "Basis known for 82% of value".
func CoverageText(c float64) string {
	switch {
	case !finite(c) || c <= 0:
		return "No cost basis known yet"
	case c < 0.005:
		return "Basis known for <1% of value"
	case c >= 0.995:
		return "Basis known for all of the value"
	}
	return "Basis known for " + strconv.Itoa(int(c*100+0.5)) + "% of value"
}

func coverStyle(c float64) templ.SafeCSS {
	if !finite(c) || c < 0 {
		c = 0
	}
	return templ.SafeCSS("width:" + strconv.FormatFloat(min(c, 1)*100, 'f', 1, 64) + "%;")
}

// pnlName is the display name of a P/L row: its name, else its symbol.
func pnlName(r PnLRow) string {
	if r.Name != "" {
		return r.Name
	}
	return r.Symbol
}

// unknownUnitsText is "0.1000 BTC with unknown cost".
func unknownUnitsText(r PnLRow) string {
	return Coin(r.UnknownUnits, r.Symbol) + " with unknown cost"
}

// exportLinks are the plain download links of the Exports card.
type exportLink struct{ Title, Text, Href, Label string }

var exportLinks = []exportLink{
	{"Holdings", "Every asset you hold, with price and value.", "/export/holdings.csv", "Download holdings CSV"},
	{"History", "Portfolio value over time, for all of your history.", "/export/history.csv?range=All", "Download history CSV"},
	{"Profit and loss", "Cost basis and unrealized P/L for each asset.", "/export/pnl.csv", "Download profit and loss CSV"},
	{"Activity", "Every transaction CryptKeep has synced.", "/export/activity.csv", "Download activity CSV"},
}
