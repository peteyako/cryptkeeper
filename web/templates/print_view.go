package templates

import (
	"strconv"
	"strings"

	"crypto-portfolio/internal/tax/report"
)

// form8949PerPage is the rows per Form 8949 page on the 2025 revision.
const form8949PerPage = 11

func form8949PartName(p report.Form8949Part) string {
	if p.Long {
		return "Part II (long-term)"
	}
	return "Part I (short-term)"
}

// FormatQty formats a quantity with up to 8 decimals, trailing zeros trimmed.
func FormatQty(f float64) string {
	s := strconv.FormatFloat(f, 'f', 8, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// TaxReportView is what the complete tax report page shows besides the year's report.
type TaxReportView struct {
	Report    report.YearReport
	Generated string
	Method    string
	Income    []ReportIncomeRow
	Wallets   []ReportWallet
	Issues    []ReportIssueCount // open year only
}

// ReportIncomeRow is one income line, formatted.
type ReportIncomeRow struct{ Date, Asset, Amount, Value, Type, Wallet string }

// ReportWallet is one wallet of the report: label, type and Form 8949 box.
type ReportWallet struct{ Label, Type, Box string }

// ReportIssueCount is the number of open review issues of one kind.
type ReportIssueCount struct {
	Kind  string
	Count int
}
