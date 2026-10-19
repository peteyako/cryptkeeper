package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/tax/irs"
	"crypto-portfolio/internal/tax/report"
	"crypto-portfolio/web/templates"
)

// Downloads of the Tax section: Koinly-format CSVs, TurboTax CSV/TXF, the printable Form 8949
// + Schedule D page, the complete tax report and the zip of all of them.

const koinlyTransactionsOnly = "Koinly years: use Koinly's own transaction export"

// taxBundle is one year's report with what the writers need besides it.
type taxBundle struct {
	s    *Server
	ctx  context.Context
	rep  report.YearReport
	rs   report.Resolver
	year int
}

// taxBundleFor loads the report of the {year} URL parameter. A year that isn't offered by
// the selector is a 404 (ok false, the response is written); a year with no data is an
// empty report, never an error.
func (s *Server) taxBundleFor(w http.ResponseWriter, r *http.Request) (*taxBundle, context.CancelFunc, bool) {
	ctx, cancel := ctxTimeout(r, 60*time.Second)
	ctx = s.withActivity(ctx)
	year, err := strconv.Atoi(chi.URLParam(r, "year"))
	if err != nil || !slices.Contains(s.taxYearsCtx(ctx), year) {
		cancel()
		http.NotFound(w, r)
		return nil, nil, false
	}
	rs := s.resolver(ctx, year, s.Now())
	rep, err := s.yearReportWith(ctx, year, rs)
	if errors.Is(err, errNoTaxData) {
		loc := s.taxLoc()
		rep, err = report.YearReport{
			Year: year, Source: "cryptkeep", Loc: loc,
			AsOf: time.Date(year, 12, 31, 23, 59, 59, 0, loc),
		}, nil
	}
	if err != nil {
		cancel()
		s.Log.Warn("tax download", "year", year, "err", err)
		http.Error(w, "Tax figures couldn't be loaded.", http.StatusInternalServerError)
		return nil, nil, false
	}
	return &taxBundle{s: s, ctx: ctx, rep: rep, rs: rs, year: year}, cancel, true
}

func (b *taxBundle) koinly() bool { return b.rep.Source == "koinly" }

// transactions writes the Koinly-format transaction history (CryptKeep years only) of the
// activity the engine replays: taxActivity (no CSV duplicate of a synced trade, CSV rows in
// their exchange's wallet) without what it ignores as spam.
func (b *taxBundle) transactions(w io.Writer) error {
	txs, err := b.s.taxActivity(b.ctx)
	if err != nil {
		return err
	}
	txs = b.s.withoutSpam(txs)
	res := tax.Result{Income: b.rep.Income}
	for _, row := range b.rep.Rows {
		res.Disposals = append(res.Disposals, row.Disposal)
	}
	for _, row := range b.rep.GiftsLost {
		res.GiftsLost = append(res.GiftsLost, row.Disposal)
	}
	if b.rep.Closed {
		return report.WriteTransactions(w, b.year, txs, res, b.rs, b.rep.Loc)
	}
	from, to := b.s.openYearRange(b.year) // the engine's range, not the calendar year
	return report.WriteTransactionsRange(w, b.year, from, to, txs, res, b.rs, b.rep.Loc)
}

// irsOptions are the IRS form filler's options: newer revisions cached under <data>/irs,
// downloaded with the server's client (none: no downloads). Name and SSN stay blank.
func (s *Server) irsOptions() irs.Options {
	return irs.Options{FormsDir: filepath.Join(s.DataDir, "irs"), HTTPClient: s.HTTPClient}
}

// fillIRSForms fills the IRS PDFs; tests replace it to make the fill fail.
var fillIRSForms = irs.FillFormsContext

func (b *taxBundle) irsForms(w io.Writer) error {
	return fillIRSForms(b.ctx, b.rep, w, b.s.irsOptions())
}

func (b *taxBundle) txf(w io.Writer) error { return report.WriteTXF(w, b.rep, b.s.Now()) }

// html renders the complete tax report page.
func (b *taxBundle) html(w io.Writer) error {
	return templates.TaxReportPage(b.s.taxReportView(b)).Render(b.ctx, w)
}

// taxCSV serves one CSV download; name is the file name's kind.
func (s *Server) taxCSV(name string, koinlyNamed bool, write func(*taxBundle, io.Writer) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, cancel, ok := s.taxBundleFor(w, r)
		if !ok {
			return
		}
		defer cancel()
		if name == "transactions" && b.koinly() {
			http.Error(w, koinlyTransactionsOnly, http.StatusNotFound)
			return
		}
		var buf bytes.Buffer
		if err := write(b, &buf); err != nil {
			s.Log.Warn("tax download", "kind", name, "err", err)
			http.Error(w, "The export failed.", http.StatusInternalServerError)
			return
		}
		prefix := ""
		if koinlyNamed {
			prefix = "koinly-"
		}
		s.csvHeaders(w, prefix+name+"-"+strconv.Itoa(b.year))
		_, _ = w.Write(buf.Bytes())
	}
}

func (s *Server) registerTaxDownloads(r chi.Router) {
	r.Get("/reports/tax/{year}/capital-gains.csv", s.taxCSV("capital-gains", true, func(b *taxBundle, w io.Writer) error { return report.WriteGains(w, b.rep) }))
	r.Get("/reports/tax/{year}/income.csv", s.taxCSV("income", true, func(b *taxBundle, w io.Writer) error { return report.WriteIncome(w, b.rep, b.rs) }))
	r.Get("/reports/tax/{year}/holdings.csv", s.taxCSV("holdings", true, func(b *taxBundle, w io.Writer) error { return report.WriteHoldings(w, b.rep) }))
	r.Get("/reports/tax/{year}/transactions.csv", s.taxCSV("transactions", true, (*taxBundle).transactions))
	r.Get("/reports/tax/{year}/other-gains.csv", s.taxCSV("other-gains", false, func(b *taxBundle, w io.Writer) error { return report.WriteOtherGains(w, b.rep) }))
	r.Get("/reports/tax/{year}/turbotax.csv", s.taxCSV("turbotax", false, func(b *taxBundle, w io.Writer) error { return report.WriteTurboTaxCSV(w, b.rep) }))
	r.Get("/reports/tax/{year}/turbotax.txf", s.taxTXF)
	r.Get("/reports/tax/{year}/8949", s.tax8949)
	r.Get("/reports/tax/{year}/report", s.taxReport)
	r.Get("/reports/tax/{year}/irs-forms.pdf", s.taxIRSForms)
	r.Get("/reports/tax/{year}/all.zip", s.taxZip)
}

func (s *Server) taxTXF(w http.ResponseWriter, r *http.Request) {
	b, cancel, ok := s.taxBundleFor(w, r)
	if !ok {
		return
	}
	defer cancel()
	var buf bytes.Buffer
	if err := b.txf(&buf); err != nil {
		http.Error(w, "The export failed.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="turbotax-%d-%s.txf"`, b.year, s.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) taxIRSForms(w http.ResponseWriter, r *http.Request) {
	b, cancel, ok := s.taxBundleFor(w, r)
	if !ok {
		return
	}
	defer cancel()
	var buf bytes.Buffer
	if err := b.irsForms(&buf); err != nil {
		s.Log.Warn("irs forms", "year", b.year, "err", err)
		http.Error(w, "The IRS forms could not be filled.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cryptkeep_%d_irs_reports_form_8949_schedule_d.pdf"`, b.year))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) tax8949(w http.ResponseWriter, r *http.Request) {
	b, cancel, ok := s.taxBundleFor(w, r)
	if !ok {
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.Form8949Doc(b.rep).Render(b.ctx, w); err != nil {
		s.Log.Warn("render 8949", "err", err)
	}
}

func (s *Server) taxReport(w http.ResponseWriter, r *http.Request) {
	b, cancel, ok := s.taxBundleFor(w, r)
	if !ok {
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := b.html(w); err != nil {
		s.Log.Warn("render tax report", "err", err)
	}
}

// taxZip bundles every report of the year, named like Koinly's files with a cryptkeep_<year>_
// prefix. The IRS forms are optional: when they can't be filled the zip goes out without
// them, with a NOTE.txt saying why.
func (s *Server) taxZip(w http.ResponseWriter, r *http.Request) {
	b, cancel, ok := s.taxBundleFor(w, r)
	if !ok {
		return
	}
	defer cancel()
	entries := []struct {
		name     string
		write    func(io.Writer) error
		skip     bool
		optional string // what the file is, for the note when it fails; "" = required
	}{
		{"capital_gains_report.csv", func(w io.Writer) error { return report.WriteGains(w, b.rep) }, false, ""},
		{"income_report.csv", func(w io.Writer) error { return report.WriteIncome(w, b.rep, b.rs) }, false, ""},
		{"other_gains_report.csv", func(w io.Writer) error { return report.WriteOtherGains(w, b.rep) }, false, ""},
		{"end_of_year_holdings_report.csv", func(w io.Writer) error { return report.WriteHoldings(w, b.rep) }, false, ""},
		{"transaction_history.csv", b.transactions, b.koinly(), ""},
		{"turbotax_online.csv", func(w io.Writer) error { return report.WriteTurboTaxCSV(w, b.rep) }, false, ""},
		{"turbotax.txf", b.txf, false, ""},
		{"irs_reports_form_8949_schedule_d.pdf", b.irsForms, false, "The IRS forms PDF (Form 8949 and Schedule D)"},
		{"complete_tax_report.html", b.html, false, ""},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var notes []string
	for _, e := range entries {
		if e.skip {
			continue
		}
		name := fmt.Sprintf("cryptkeep_%d_%s", b.year, e.name)
		write := e.write
		if e.optional != "" {
			// Filled aside first, so a failure leaves no partial file in the zip.
			var part bytes.Buffer
			if ferr := e.write(&part); ferr != nil {
				s.Log.Warn("tax zip: left out", "entry", e.name, "err", ferr)
				notes = append(notes, fmt.Sprintf("%s could not be made, so %s is not in this zip: %v\n", e.optional, name, ferr))
				continue
			}
			write = func(w io.Writer) error { _, err := w.Write(part.Bytes()); return err }
		}
		f, err := zw.Create(name)
		if err == nil {
			err = write(f)
		}
		if err != nil {
			s.Log.Warn("tax zip", "entry", e.name, "err", err)
			http.Error(w, "The export failed.", http.StatusInternalServerError)
			return
		}
	}
	if len(notes) > 0 {
		f, err := zw.Create(fmt.Sprintf("cryptkeep_%d_NOTE.txt", b.year))
		if err == nil {
			_, err = io.WriteString(f, strings.Join(notes, "")+
				"\nEvery other file is complete. Download the file again from the Tax section later, or copy the figures from the Form 8949 and Schedule D pages of complete_tax_report.html.\n")
		}
		if err != nil {
			http.Error(w, "The export failed.", http.StatusInternalServerError)
			return
		}
	}
	if err := zw.Close(); err != nil {
		http.Error(w, "The export failed.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cryptkeep-tax-%d-%s.zip"`, b.year, s.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// taxMethod is the report's method statement.
func (s *Server) taxMethod(rep report.YearReport) string {
	y := strconv.Itoa(rep.Year)
	if rep.Source == "koinly" {
		return "Figures for " + y + " are Koinly's, imported verbatim."
	}
	base := "Cost basis method: FIFO, tracked per wallet (Rev. Proc. 2024-28)."
	if s.TaxYears != nil {
		first := 0
		// The latest Koinly-closed year before this one: where the CryptKeep run continues from.
		for _, yr := range s.TaxYears.Years() {
			if ty, ok := s.TaxYears.Get(yr); ok && ty.Source == "koinly" && yr < rep.Year && yr > first {
				first = yr
			}
		}
		if first > 0 && first < rep.Year {
			k := strconv.Itoa(first)
			return fmt.Sprintf("%s Opening basis on 01/01/%d imported from Koinly's %s year-end holdings; acquisition dates of %s purchases back-filled from Koinly's transaction history; older units are reported as VARIOUS (long-term).",
				base, first+1, k, k)
		}
	}
	return base + " Opening basis computed by CryptKeep from the wallets' full history."
}

func (s *Server) taxReportView(b *taxBundle) templates.TaxReportView {
	rep, rs := b.rep, b.rs
	loc := rep.Loc
	if loc == nil {
		loc = time.UTC
	}
	label := func(w string) string {
		if rs.WalletLabel != nil {
			if l := rs.WalletLabel(w); l != "" {
				return l
			}
		}
		if w == tax.Untracked {
			return "Untracked (from Koinly)"
		}
		return w
	}
	v := templates.TaxReportView{Report: rep, Generated: s.Now().In(loc).Format("01/02/2006"), Method: s.taxMethod(rep)}
	var err error
	if v.Short, v.Long, err = report.Totals(rep); err != nil {
		v.TotalsErr = err.Error()
	}
	for _, in := range rep.Income {
		sym := in.Asset
		if rs.Symbol != nil {
			if x, _ := rs.Symbol(in.Asset); x != "" {
				sym = x
			}
		}
		v.Income = append(v.Income, templates.ReportIncomeRow{
			Date: in.Date.In(loc).Format("01/02/2006"), Asset: sym, Amount: templates.FormatQty(in.Amount),
			Value: report.Money8949(in.ValueUSD), Type: templates.IncomeTypeName(in.Type), Wallet: label(in.Wallet),
		})
	}
	v.Wallets = s.reportWallets(rep, label)
	if !rep.Closed {
		counts := map[string]int{}
		for _, i := range rep.Issues {
			counts[string(i.Kind)]++
		}
		for k, n := range counts {
			v.Issues = append(v.Issues, templates.ReportIssueCount{Kind: k, Count: n})
		}
		slices.SortFunc(v.Issues, func(a, b templates.ReportIssueCount) int { return strings.Compare(a.Kind, b.Kind) })
	}
	return v
}

// longBox is the Part II box matching a short-term box letter.
func longBox(short string) string {
	switch short {
	case "G":
		return "J"
	case "H":
		return "K"
	}
	return "L"
}

// reportWallets lists the wallets of the report with their type and Form 8949 box. A
// CryptKeep year lists the registered wallets (plus the untracked one when rows use it); a
// Koinly year lists the wallets of its rows with the boxes they landed in.
func (s *Server) reportWallets(rep report.YearReport, label func(string) string) []templates.ReportWallet {
	if rep.Source == "koinly" {
		boxes := map[string][]string{}
		var order []string
		for _, row := range rep.Rows {
			if _, ok := boxes[row.Wallet]; !ok {
				order = append(order, row.Wallet)
			}
			if !slices.Contains(boxes[row.Wallet], row.Box) {
				boxes[row.Wallet] = append(boxes[row.Wallet], row.Box)
			}
		}
		out := make([]templates.ReportWallet, 0, len(order))
		for _, w := range order {
			bx := boxes[w]
			slices.Sort(bx)
			out = append(out, templates.ReportWallet{Label: label(w), Type: "Koinly wallet", Box: strings.Join(bx, ", ")})
		}
		return out
	}
	var override map[string]string
	if s.TaxSettings != nil {
		override = s.TaxSettings.Get().WalletBox
	}
	var out []templates.ReportWallet
	for _, d := range s.Registry.Defs() {
		if d.Type == sources.TypeBrokerage {
			continue // the broker's 1099-B covers it
		}
		var box string
		switch {
		case override[d.ID] != "":
			o := override[d.ID]
			box = o + " (short) / " + longBox(o) + " (long)"
		case d.Type == "coinbase" || d.Type == "kraken":
			box = "G or H (short) / J or K (long)"
		default:
			box = "I (short) / L (long)"
		}
		name := d.Label
		if name == "" {
			name = d.ID
		}
		out = append(out, templates.ReportWallet{Label: name, Description: d.Description, Type: d.Type, Box: box})
	}
	for _, row := range rep.Rows {
		if row.Wallet == tax.Untracked {
			out = append(out, templates.ReportWallet{Label: label(tax.Untracked), Type: "untracked", Box: "I (short) / L (long)"})
			break
		}
	}
	return out
}
