package server

import (
	"archive/zip"
	"context"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/tax"
)

var taxNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type fakeHist struct{ bySym, byMint map[string]float64 }

func (f fakeHist) Historical(_ context.Context, sym string, _ time.Time) (float64, bool, error) {
	v, ok := f.bySym[sym]
	return v, ok, nil
}

func (f fakeHist) HistoricalMint(_ context.Context, mint string, _ time.Time) (float64, bool, error) {
	v, ok := f.byMint[mint]
	return v, ok, nil
}

func (e *env) taxServer() *Server {
	d := e.deps
	d.Log = slog.Default()
	return &Server{Deps: d}
}

// koinlyYear closes 2025 with invented Koinly rows.
func (e *env) koinlyYear(n int) {
	e.t.Helper()
	var ds []tax.Disposal
	for i := range n {
		ds = append(ds, tax.Disposal{
			Sold: time.Date(2025, 5, 1, 0, 0, i, 0, time.UTC), Acquired: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			Asset: "BTC", Amount: 0.5, CostUSD: 100, ProceedsUSD: 150, GainUSD: 50, Wallet: "Invented Coinbase", Term: tax.LongTerm,
		})
	}
	y := tax.Year{Year: 2025, Source: "koinly", ClosedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), Disposals: ds,
		Summary:     tax.Summarize(ds, nil),
		ClosingLots: []tax.Lot{{Wallet: "Invented Coinbase", Asset: "SOL", Amount: 2, CostUSD: 40, Acquired: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)}}}
	if err := e.taxYears.Save(y); err != nil {
		e.t.Fatal(err)
	}
}

func TestTaxViewYears(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	if got := e.taxServer().taxYears(); !slices.Equal(got, []int{2025, 2026}) {
		t.Fatalf("years %v", got)
	}
	_, body := e.do("GET", "/reports/tax", nil, nil)
	if !strings.Contains(body, `id="tax-section"`) || !strings.Contains(body, `<option value="2026" selected`) {
		t.Errorf("default year must be 2026: %s", within(body, `id="tax-section"`, 800))
	}
	if strings.Contains(body, `id="review-panel"`) {
		t.Error("the review panel lives in its tab, not on the page")
	}
	_, body = e.do("GET", "/reports/tax?year=2026&tab=review", nil, nil)
	if strings.Count(body, `id="review-panel"`) != 1 || !strings.Contains(body, `id="close-card"`) {
		t.Error("the review tab hosts the review panel once, with the close card")
	}
}

func TestTaxViewClosedKoinlyYear(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	_, body := e.do("GET", "/reports/tax?year=2025", nil, nil)
	for _, want := range []string{"Koinly", "Invented Coinbase", "tax-section"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "Needs review") || strings.Contains(body, `id="review-panel"`) {
		t.Error("a closed year has no Needs review tab")
	}
	// Coinbase-named Koinly wallet, long term, acquired before 2026: box K.
	if !strings.Contains(body, ">K<") {
		t.Errorf("box K expected: %s", within(body, "tbl--tax-gains", 1500))
	}
}

func TestHoldingsOpenYearUsesToday(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) {
		d.Historical = fakeHist{bySym: map[string]float64{"SOL": 150}}
	})
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	_, body := e.do("GET", "/reports/tax?year=2026&tab=holdings", nil, nil)
	if !strings.Contains(body, "as of 10/16/2026") {
		t.Errorf("holdings header: %s", within(body, "tbl--tax-holdings", 600))
	}
	if !strings.Contains(body, "$150.00") || !strings.Contains(body, "$1,500.00") {
		t.Errorf("price and value expected: %s", within(body, "tbl--tax-holdings", 2000))
	}
}

func TestHoldingsWithoutHistoricalAreUnpriced(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	_, body := e.do("GET", "/reports/tax?year=2026&tab=holdings", nil, nil)
	if !strings.Contains(body, "tbl--tax-holdings") || !strings.Contains(body, "as of 10/16/2026") {
		t.Errorf("holdings table missing: %s", body)
	}
	if strings.Count(body, `<td class="num">—</td>`) != 2 {
		t.Errorf("unpriced price and value cells expected: %s", within(body, "tbl--tax-holdings", 2000))
	}
}

func TestTaxViewTabs(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.closeYear(2025, tax.Summary{}, solLot("sol", 10, 1000))
	e.upsertTxs(priced(reviewRecv("inc", "hot", "SOL", 2, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))))
	if err := e.taxSet.Update(func(s *tax.Settings) {
		s.Tags = map[string]tax.TagEntry{"hot:inc": {Tag: tax.TagIncome}}
	}); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do("POST", "/reports/tax/year", strings.NewReader(url.Values{"year": {"2026"}, "tab": {"income"}}.Encode()),
		ds(map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, `id="tax-section"`) || !strings.Contains(body, "tbl--tax-income") || !strings.Contains(body, "03/01/2026") ||
		!strings.Contains(body, "<td>TKN</td>") || !strings.Contains(body, "<td>Income</td>") || !strings.Contains(body, "$10.00") {
		t.Errorf("patch: %s", body)
	}
	if strings.Contains(body, "tbl--tax-gains") {
		t.Error("only the income tab is rendered")
	}
}

func TestTaxViewGainsPaged(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(250)
	count := func(page string) int {
		_, body := e.do("GET", "/reports/tax?year=2025&page="+page, nil, ds(nil))
		return strings.Count(body, `class="gain-row`)
	}
	if n := count("1"); n != 200 {
		t.Errorf("page 1 has %d rows", n)
	}
	if n := count("2"); n != 50 {
		t.Errorf("page 2 has %d rows", n)
	}
	if n := count("99"); n != 50 {
		t.Errorf("page past the end clamps to the last: %d", n)
	}
}

func TestTaxViewResolvesWalletTypes(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	srv := e.taxServer()
	r := srv.resolver(context.Background(), 2026, taxNow)
	if r.WalletType("coinbase-csv") != "coinbase" || r.WalletType(tax.Untracked) != "" {
		t.Errorf("wallet types")
	}
}

func TestTaxViewFallbacks(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	_, body := e.do("GET", "/reports/tax?year=1999", nil, nil)
	if !strings.Contains(body, `<option value="2026" selected`) {
		t.Error("unknown year falls back to the latest")
	}
	_, body = e.do("GET", "/reports/tax?year=2025&tab=review", nil, nil)
	if !strings.Contains(body, "tbl--tax-gains") || strings.Contains(body, `id="review-panel"`) {
		t.Error("review tab on a closed year falls back to gains")
	}
}

func TestTaxViewReviewTabOnlyOnLatestOpenYear(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.upsertTxs(priced(reviewRecv("a", "hot", "SOL", 1, time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))))
	_, body := e.do("GET", "/reports/tax?year=2025", nil, nil)
	if strings.Contains(body, "Needs review") {
		t.Error("an earlier open year has no review tab")
	}
	_, body = e.do("GET", "/reports/tax?year=2026", nil, nil)
	if !strings.Contains(body, "Needs review") {
		t.Error("the latest open year has one")
	}
}

func TestTaxViewSPLPricedByMint(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) {
		d.Historical = fakeHist{bySym: map[string]float64{"TKN": 999}, byMint: map[string]float64{"MintA": 2}}
	})
	e.closeYear(2025, tax.Summary{}, tax.Lot{Wallet: "sol", Asset: "spl:MintA", Amount: 10, CostUSD: 5,
		Acquired: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)})
	e.upsertTxs(reviewRecv("x", "sol", "spl:MintA", 0, time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)))
	_, body := e.do("GET", "/reports/tax?year=2026&tab=holdings", nil, nil)
	if !strings.Contains(body, "$20.00") || strings.Contains(body, "$9,990.00") {
		t.Errorf("priced by mint, not by ticker: %s", within(body, "tbl--tax-holdings", 2000))
	}
}

func TestTaxViewMalformedBody(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	resp, body := e.do("POST", "/reports/tax/year", strings.NewReader("%zz"),
		ds(map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	if resp.StatusCode != 200 || !strings.Contains(body, `id="tax-section"`) || !strings.Contains(body, "could not be read") {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}
}

func TestReviewActionsRefreshTaxSection(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.ref.Force(context.Background())
	e.upsertTxs(reviewRecv("u", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)))
	out := e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:nope"}, "tag": {"spam"}})
	if !strings.Contains(out, `id="tax-section"`) || !strings.Contains(out, "Needs review (") {
		t.Fatalf("section must be re-patched with the review count: %s", out)
	}
	if !strings.Contains(out, "Unmatched receives") {
		t.Fatalf("untagged receive is listed: %s", out)
	}
	out = e.reviewPost("/reports/tax/tag", url.Values{"tx": {"hot:u"}, "tag": {"spam"}})
	if !strings.Contains(out, `id="tax-section"`) || strings.Contains(out, "Unmatched receives") || strings.Contains(out, "Needs review (") {
		t.Errorf("section not refreshed after tagging: %s", out)
	}
}

func TestCloseRefreshesTaxSection(t *testing.T) {
	e := newEnv(t, "", nowAt(time.Date(2027, 2, 1, 12, 0, 0, 0, time.UTC)))
	e.ref.Force(context.Background())
	e.upsertTxs(priced(reviewRecv("a", "hot", "SOL", 1, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))))
	out := e.reviewPost("/reports/tax/close", url.Values{"year": {"2026"}, "confirm": {"1"}})
	if !strings.Contains(out, `id="tax-section"`) || !strings.Contains(out, `<option value="2027" selected`) ||
		!strings.Contains(out, "Reopen 2026") || !strings.Contains(out, `<option value="2026"`) {
		t.Errorf("close must refresh the section: %s", out)
	}
	_, body := e.do("GET", "/reports/tax?year=2026", nil, nil)
	if !strings.Contains(body, "Closed · computed by CryptKeeper") {
		t.Error("2026 now closed")
	}
}

// ---- downloads, 8949, CPA report, zip -------------------------------------------------

// saleYear2026 records one invented receive and sale in 2026 (a disposal in wallet "hot").
func (e *env) saleYear2026() {
	e.upsertTxs(
		priced(reviewRecv("a", "hot", "SOL", 2, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))),
		sendTx("b", "hot", "SOL", 1, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)),
	)
}

func TestTaxDownloadRoutes(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	e.saleYear2026()
	for path, first := range map[string]string{
		"capital-gains.csv": "Capital gains report 2026\n\nDate Sold,Date Acquired,Asset,Amount,Cost (USD),Proceeds (USD),Gain / loss,Notes,Wallet Name,Holding period",
		"income.csv":        "Income report 2026\n\nDate,Asset,Amount,Value (USD),Type,Description,Wallet Name",
		"holdings.csv":      "Balances as at 10/16/2026 12:00\n\nAsset,Quantity,Cost (USD),Value (USD),Description", // the open year: as at now
		"transactions.csv":  "Transaction report 2026\n\nDate,Type,Tag,Sending Wallet",
		"other-gains.csv":   "Other gains report 2026\n\nDate,Asset,Amount,Value (USD),Type,Wallet Name",
		"turbotax.csv":      "Currency Name,Purchase Date,Cost Basis,Date Sold,Proceeds",
	} {
		resp, body := e.do("GET", "/reports/tax/2026/"+path, nil, nil)
		if resp.StatusCode != 200 || !strings.HasPrefix(body, first) {
			t.Errorf("%s: %d %q", path, resp.StatusCode, body)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "2026") {
			t.Errorf("%s disposition %q", path, cd)
		}
	}
	resp, _ := e.do("GET", "/reports/tax/2026/capital-gains.csv", nil, nil)
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="koinly-capital-gains-2026-`) {
		t.Errorf("filename %q", cd)
	}
	resp, body := e.do("GET", "/reports/tax/2026/turbotax.txf", nil, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "V042\nACryptKeeper\nD10/16/2026\n^") {
		t.Errorf("txf: %d %q", resp.StatusCode, body)
	}
	// Koinly years have no transactions export; their other files come from stored rows.
	resp, body = e.do("GET", "/reports/tax/2025/transactions.csv", nil, nil)
	if resp.StatusCode != 404 || !strings.Contains(body, "Koinly years: use Koinly's own transaction export") {
		t.Errorf("koinly transactions: %d %q", resp.StatusCode, body)
	}
	_, body = e.do("GET", "/reports/tax/2025/capital-gains.csv", nil, nil)
	if !strings.Contains(body, "Invented Coinbase") || !strings.Contains(body, "Long term") {
		t.Errorf("koinly gains: %q", body)
	}
	for _, p := range []string{"1999/capital-gains.csv", "abc/capital-gains.csv", "1999/8949", "1999/report", "1999/all.zip"} {
		if resp, _ := e.do("GET", "/reports/tax/"+p, nil, nil); resp.StatusCode != 404 {
			t.Errorf("%s: status %d", p, resp.StatusCode)
		}
	}
}

func TestCSVQuotesLabelsRoute(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	if _, err := e.reg.Upsert(sources.Def{ID: "hot", Type: sources.TypeManual, Label: "Cold, Ledger", Holdings: []manual.Holding{{Asset: "SOL", Amount: 1}}}); err != nil {
		t.Fatal(err)
	}
	e.saleYear2026()
	_, body := e.do("GET", "/reports/tax/2026/capital-gains.csv", nil, nil)
	if !strings.Contains(body, `"Cold, Ledger"`) {
		t.Errorf("label must be quoted: %q", body)
	}
}

func TestEmpty8949(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	resp, body := e.do("GET", "/reports/tax/2026/8949", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "No disposals in 2026") {
		t.Errorf("%d %q", resp.StatusCode, body)
	}
	for _, p := range []string{"capital-gains.csv", "turbotax.csv", "income.csv", "holdings.csv", "other-gains.csv", "report", "all.zip"} {
		if resp, _ := e.do("GET", "/reports/tax/2026/"+p, nil, nil); resp.StatusCode != 200 {
			t.Errorf("empty year %s: %d", p, resp.StatusCode)
		}
	}
}

func TestKoinlyYearBoxesRoute(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(25)
	resp, body := e.do("GET", "/reports/tax/2025/8949", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if !strings.Contains(body, "Box K") || strings.Contains(body, "Box L") {
		t.Errorf("Koinly Coinbase long-term rows belong in box K: %s", within(body, "Box", 300))
	}
	// 25 rows at 11 per page: three pages, each with its own totals row.
	for _, want := range []string{"Page 1 of 3", "Page 2 of 3", "Page 3 of 3", "Print / Save as PDF"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "Page 4 of") || strings.Count(body, `class="f8949-total"`) != 3 {
		t.Errorf("pages/totals: %d", strings.Count(body, `class="f8949-total"`))
	}
	if !strings.Contains(body, "Schedule D") || !strings.Contains(body, "<td>10</td>") {
		t.Errorf("schedule D line 10 expected")
	}
}

func TestTaxReportSections(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	e.saleYear2026()
	resp, body := e.do("GET", "/reports/tax/2026/report", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	last := -1
	for _, h := range []string{"Crypto tax report 2026", "<h2>Summary</h2>", "<h2>Method</h2>", "<h2>Schedule D</h2>", "<h2>Form 8949 detail</h2>", "<h2>Income</h2>", "<h2>Year-end holdings</h2>", "<h2>Wallets</h2>", "<h2>Open review issues</h2>", "Not tax advice."} {
		i := strings.Index(body, h)
		if i < 0 || i < last {
			t.Fatalf("section %q missing or out of order (at %d, after %d)", h, i, last)
		}
		last = i
	}
	want := "Cost basis method: FIFO, tracked per wallet (Rev. Proc. 2024-28). Opening basis on 01/01/2026 imported from Koinly&#39;s 2025 year-end holdings; acquisition dates of 2025 purchases back-filled from Koinly&#39;s transaction history; older units are reported as VARIOUS (long-term)."
	if !strings.Contains(body, want) {
		t.Errorf("method statement missing: %s", within(body, "Cost basis method", 400))
	}
	if !strings.Contains(body, "Prepared by CryptKeeper from the user") {
		t.Error("closing line")
	}
}

func TestTaxReportKoinlyYear(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	_, body := e.do("GET", "/reports/tax/2025/report", nil, nil)
	if !strings.Contains(body, "Figures for 2025 are Koinly") || !strings.Contains(body, "imported verbatim.") {
		t.Errorf("method: %s", within(body, "Figures for", 200))
	}
	if strings.Contains(body, "<h2>Open review issues</h2>") {
		t.Error("closed years have no review section")
	}
}

func zipNames(t *testing.T, raw string) (map[string]string, []string) {
	t.Helper()
	zr, err := zip.NewReader(strings.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	files, order := map[string]string{}, []string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name] = string(b)
		order = append(order, f.Name)
	}
	return files, order
}

func TestAllZipContents(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	e.saleYear2026()
	resp, body := e.do("GET", "/reports/tax/2026/all.zip", nil, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	files, order := zipNames(t, body)
	if len(order) != 9 {
		t.Errorf("want exactly 9 entries, got %v", order)
	}
	if pdf := files["cryptkeeper_2026_irs_reports_form_8949_schedule_d.pdf"]; !strings.HasPrefix(pdf, "%PDF") {
		t.Errorf("pdf entry: %d bytes", len(pdf))
	}
	for _, n := range []string{"capital_gains_report.csv", "income_report.csv", "other_gains_report.csv",
		"end_of_year_holdings_report.csv", "transaction_history.csv", "turbotax_online.csv", "turbotax.txf", "irs_reports_form_8949_schedule_d.pdf", "complete_tax_report.html"} {
		if _, ok := files["cryptkeeper_2026_"+n]; !ok {
			t.Errorf("missing %s", n)
		}
	}
	if !strings.HasPrefix(files["cryptkeeper_2026_capital_gains_report.csv"], "Capital gains report 2026") {
		t.Errorf("gains entry: %q", files["cryptkeeper_2026_capital_gains_report.csv"])
	}
	html := files["cryptkeeper_2026_complete_tax_report.html"]
	if !strings.Contains(html, "<style>") || strings.Contains(html, "/static/") || strings.Contains(html, "<link") {
		t.Error("report html must be self-contained")
	}
}

func TestAllZipKoinlyYear(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.koinlyYear(1)
	_, body := e.do("GET", "/reports/tax/2025/all.zip", nil, nil)
	files, _ := zipNames(t, body)
	if _, ok := files["cryptkeeper_2025_transaction_history.csv"]; ok {
		t.Error("Koinly years have no transaction history")
	}
	if _, ok := files["cryptkeeper_2025_capital_gains_report.csv"]; !ok {
		t.Error("gains missing")
	}
}

func TestTaxReportMethodUsesLatestKoinlyYear(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	for _, y := range []int{2024, 2025} {
		if err := e.taxYears.Save(tax.Year{Year: y, Source: "koinly", ClosedAt: time.Date(y+1, 1, 5, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := e.do("GET", "/reports/tax/2026/report", nil, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	if !strings.Contains(body, "Opening basis on 01/01/2026 imported from Koinly&#39;s 2025 year-end holdings") {
		t.Errorf("method: %s", within(body, "Cost basis method", 300))
	}
}

func TestIRSFormsRoute(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.saleYear2026()
	resp, body := e.do("GET", "/reports/tax/2026/irs-forms.pdf", nil, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(body, "%PDF") {
		t.Fatalf("status %d, %.20q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("content type %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, `filename="cryptkeeper_2026_irs_reports_form_8949_schedule_d.pdf"`) {
		t.Errorf("disposition %q", cd)
	}
	if resp, _ := e.do("GET", "/reports/tax/1999/irs-forms.pdf", nil, nil); resp.StatusCode != 404 {
		t.Errorf("unknown year: %d", resp.StatusCode)
	}
}
