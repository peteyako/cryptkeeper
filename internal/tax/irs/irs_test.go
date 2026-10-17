package irs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/tax/report"
)

// Field names relative to topmostSubform[0].
const (
	p1 = "Page1[0]."
	p2 = "Page2[0]."
	t1 = p1 + "Table_Line1_Part1[0]."
	t2 = p2 + "Table_Line1_Part2[0]."
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }

// disposal is an invented row of box with the given amounts.
func disposal(box string, amount float64, sym string, proceeds, cost float64) report.Row {
	term := tax.ShortTerm
	if box >= "J" {
		term = tax.LongTerm
	}
	return report.Row{Box: box, Symbol: sym, Disposal: tax.Disposal{
		Amount: amount, ProceedsUSD: proceeds, CostUSD: cost, GainUSD: proceeds - cost, Term: term,
		Acquired: day(2025, 1, 2), Sold: day(2025, 3, 2),
	}}
}

func yearReport(year int, rows ...report.Row) report.YearReport {
	return report.YearReport{Year: year, Loc: time.UTC, Rows: rows}
}

// filled is a PDF read back through pdfcpu's form export: per page, field values by name
// relative to topmostSubform[0].
type filled struct {
	pages  int
	title  string
	text   map[int]map[string]string
	checks map[int]map[string]bool
	pdf    []byte
}

func shortName(name string) string {
	const root = "topmostSubform[0]."
	if i := strings.Index(name, root); i >= 0 {
		return name[i+len(root):]
	}
	return name
}

func readBack(t *testing.T, pdf []byte) filled {
	t.Helper()
	n, err := api.PageCount(bytes.NewReader(pdf), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("page count: %v", err)
	}
	fg, err := api.ExportForm(bytes.NewReader(pdf), "out.pdf", model.NewDefaultConfiguration())
	if err != nil {
		t.Fatalf("export form: %v", err)
	}
	f := filled{pages: n, title: fg.Header.Title, text: map[int]map[string]string{}, checks: map[int]map[string]bool{}, pdf: pdf}
	setText := func(pages []int, name, v string) {
		for _, p := range pages {
			if f.text[p] == nil {
				f.text[p] = map[string]string{}
			}
			f.text[p][shortName(name)] = v
		}
	}
	for _, fm := range fg.Forms {
		for _, tf := range fm.TextFields {
			setText(tf.Pages, tf.Name, tf.Value)
		}
		// A filled value that parses as a date is exported as a date field.
		for _, df := range fm.DateFields {
			setText(df.Pages, df.Name, df.Value)
		}
		for _, cb := range fm.CheckBoxes {
			for _, p := range cb.Pages {
				if f.checks[p] == nil {
					f.checks[p] = map[string]bool{}
				}
				f.checks[p][shortName(cb.Name)] = cb.Value
			}
		}
	}
	return f
}

func (f filled) want(t *testing.T, page int, name, want string) {
	t.Helper()
	if got := f.text[page][name]; got != want {
		t.Errorf("page %d %s = %q, want %q", page, name, got, want)
	}
}

// checked returns the checked boxes of a page.
func (f filled) checked(page int) []string {
	var out []string
	for name, v := range f.checks[page] {
		if v {
			out = append(out, name)
		}
	}
	return out
}

// tableEmpty reports whether every row cell of an 8949 page's table is blank.
func (f filled) tableEmpty(page int, table string) bool {
	for name, v := range f.text[page] {
		if strings.HasPrefix(name, table) && v != "" {
			return false
		}
	}
	return true
}

// contains reports whether any decoded stream of the PDF holds s (page text, stamps).
func (f filled) contains(t *testing.T, s string) bool {
	t.Helper()
	ctx, err := api.ReadValidateAndOptimize(bytes.NewReader(f.pdf), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	for nr := range ctx.Table {
		sd, _, err := ctx.DereferenceStreamDict(*types.NewIndirectRef(nr, 0))
		if err != nil || sd == nil || sd.Decode() != nil {
			continue
		}
		if bytes.Contains(sd.Content, []byte(s)) {
			return true
		}
	}
	return false
}

func fillForms(t *testing.T, r report.YearReport, opts Options) filled {
	t.Helper()
	var b bytes.Buffer
	if err := FillForms(r, &b, opts); err != nil {
		t.Fatalf("FillForms: %v", err)
	}
	return readBack(t, b.Bytes())
}

func TestFillFormsOneShortRow(t *testing.T) {
	r := yearReport(2025, disposal("I", 0.5, "BTC", 1500, 1000))
	f := fillForms(t, r, Options{Name: "Jordan Q. Example", SSN: "000-12-3456"})

	if f.pages != 4 {
		t.Fatalf("pages = %d, want 4 (Schedule D + one 8949 copy)", f.pages)
	}
	// Schedule D page 1.
	f.want(t, 1, p1+"f1_1[0]", "Jordan Q. Example")
	f.want(t, 1, p1+"f1_2[0]", "000-12-3456")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_15[0]", "1500.00")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_16[0]", "1000.00")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_17[0]", "0.00")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_18[0]", "500.00")
	f.want(t, 1, p1+"Table_PartI[0].Row1b[0].f1_7[0]", "")
	f.want(t, 1, p1+"Table_PartI[0].Row2[0].f1_11[0]", "")
	f.want(t, 1, p1+"f1_22[0]", "500.00") // line 7
	f.want(t, 1, p1+"Table_PartII[0].Row10[0].f1_35[0]", "")
	f.want(t, 1, p1+"f1_43[0]", "0.00") // line 15
	if c := f.checked(1); len(c) != 0 {
		t.Errorf("Schedule D boxes checked: %v (QOF must stay unset)", c)
	}

	// Form 8949 page 1 (output page 3).
	if c := f.checked(3); len(c) != 1 || c[0] != p1+"c1_1[5]" {
		t.Errorf("8949 page 1 checked %v, want only box I (c1_1[5])", c)
	}
	f.want(t, 3, p1+"f1_01[0]", "Jordan Q. Example")
	f.want(t, 3, p1+"f1_02[0]", "000-12-3456")
	f.want(t, 3, t1+"Row1[0].f1_03[0]", "0.5 BTC")
	f.want(t, 3, t1+"Row1[0].f1_04[0]", "01/02/2025")
	f.want(t, 3, t1+"Row1[0].f1_05[0]", "03/02/2025")
	f.want(t, 3, t1+"Row1[0].f1_06[0]", "1500.00")
	f.want(t, 3, t1+"Row1[0].f1_07[0]", "1000.00")
	f.want(t, 3, t1+"Row1[0].f1_08[0]", "")
	f.want(t, 3, t1+"Row1[0].f1_09[0]", "")
	f.want(t, 3, t1+"Row1[0].f1_10[0]", "500.00")
	f.want(t, 3, t1+"Row2[0].f1_11[0]", "")
	f.want(t, 3, p1+"f1_91[0]", "1500.00")
	f.want(t, 3, p1+"f1_92[0]", "1000.00")
	f.want(t, 3, p1+"f1_94[0]", "0.00")
	f.want(t, 3, p1+"f1_95[0]", "500.00")

	// Form 8949 page 2 (output page 4): blank Part II, header still named.
	if c := f.checked(4); len(c) != 0 {
		t.Errorf("8949 page 2 checked %v, want none", c)
	}
	if !f.tableEmpty(4, t2) {
		t.Errorf("8949 Part II table not empty: %v", f.text[4])
	}
	f.want(t, 4, p2+"f2_01[0]", "Jordan Q. Example")
	f.want(t, 4, p2+"f2_02[0]", "000-12-3456")
	f.want(t, 4, p2+"f2_91[0]", "")
	if f.contains(t, "Filled on the 2025 form revision") {
		t.Error("2025 report must not carry the revision note")
	}
}

func TestFillFormsPaging(t *testing.T) {
	var rows []report.Row
	for i := range 23 {
		rows = append(rows, disposal("I", float64(i+1), "SOL", float64(100+i), 50))
	}
	f := fillForms(t, yearReport(2025, rows...), Options{})

	if f.pages != 2+3*2 {
		t.Fatalf("pages = %d, want 8 (Schedule D + 3 copies)", f.pages)
	}
	for _, page := range []int{3, 5, 7} {
		if c := f.checked(page); len(c) != 1 || c[0] != p1+"c1_1[5]" {
			t.Errorf("page %d checked %v, want only box I", page, c)
		}
	}
	// Copy 1: rows 1..11, totals of those rows.
	f.want(t, 3, t1+"Row1[0].f1_03[0]", "1 SOL")
	f.want(t, 3, t1+"Row11[0].f1_83[0]", "11 SOL")
	f.want(t, 3, t1+"Row11[0].f1_86[0]", "110.00")
	f.want(t, 3, p1+"f1_91[0]", "1155.00") // 100..110
	f.want(t, 3, p1+"f1_92[0]", "550.00")
	f.want(t, 3, p1+"f1_95[0]", "605.00")
	// Copy 2 starts at row 12.
	f.want(t, 5, t1+"Row1[0].f1_03[0]", "12 SOL")
	f.want(t, 5, t1+"Row11[0].f1_83[0]", "22 SOL")
	// Copy 3: only row 1 filled; totals equal that row.
	f.want(t, 7, t1+"Row1[0].f1_03[0]", "23 SOL")
	f.want(t, 7, t1+"Row1[0].f1_06[0]", "122.00")
	f.want(t, 7, t1+"Row2[0].f1_11[0]", "")
	f.want(t, 7, p1+"f1_91[0]", "122.00")
	f.want(t, 7, p1+"f1_92[0]", "50.00")
	f.want(t, 7, p1+"f1_95[0]", "72.00")
	// Schedule D line 3 is the whole box.
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_15[0]", "2553.00") // 100..122
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_16[0]", "1150.00")
}

func TestFillFormsLongTermPage2(t *testing.T) {
	r := yearReport(2025, disposal("L", 2, "ETH", 4000, 3000.5))
	f := fillForms(t, r, Options{})

	if f.pages != 4 {
		t.Fatalf("pages = %d, want 4", f.pages)
	}
	if c := f.checked(4); len(c) != 1 || c[0] != p2+"c2_1[5]" {
		t.Errorf("8949 page 2 checked %v, want only box L (c2_1[5])", c)
	}
	f.want(t, 4, t2+"Row1[0].f2_03[0]", "2 ETH")
	f.want(t, 4, t2+"Row1[0].f2_04[0]", "01/02/2025")
	f.want(t, 4, t2+"Row1[0].f2_05[0]", "03/02/2025")
	f.want(t, 4, t2+"Row1[0].f2_06[0]", "4000.00")
	f.want(t, 4, t2+"Row1[0].f2_07[0]", "3000.50")
	f.want(t, 4, t2+"Row1[0].f2_10[0]", "999.50")
	f.want(t, 4, p2+"f2_91[0]", "4000.00")
	f.want(t, 4, p2+"f2_95[0]", "999.50")
	if c := f.checked(3); len(c) != 0 {
		t.Errorf("8949 page 1 checked %v, want none", c)
	}
	if !f.tableEmpty(3, t1) {
		t.Errorf("8949 Part I table not empty: %v", f.text[3])
	}
	f.want(t, 1, p1+"Table_PartII[0].Row10[0].f1_35[0]", "4000.00")
	f.want(t, 1, p1+"Table_PartII[0].Row10[0].f1_38[0]", "999.50")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_15[0]", "")
	f.want(t, 1, p1+"f1_22[0]", "0.00")   // line 7
	f.want(t, 1, p1+"f1_43[0]", "999.50") // line 15
}

func TestFillFormsShortAndLongShareCopies(t *testing.T) {
	var rows []report.Row
	for range 12 {
		rows = append(rows, disposal("H", 1, "BTC", 10, 5))
	}
	rows = append(rows, disposal("G", 1, "BTC", 20, 5), disposal("K", 1, "SOL", 30, 5))
	f := fillForms(t, yearReport(2025, rows...), Options{})

	// Short chunks: G(1), H(11), H(1); long chunks: K(1) → 3 copies.
	if f.pages != 8 {
		t.Fatalf("pages = %d, want 8", f.pages)
	}
	want := map[int]string{3: p1 + "c1_1[3]", 5: p1 + "c1_1[4]", 7: p1 + "c1_1[4]", 4: p2 + "c2_1[4]"}
	for page, box := range want {
		if c := f.checked(page); len(c) != 1 || c[0] != box {
			t.Errorf("page %d checked %v, want %s", page, c, box)
		}
	}
	for _, page := range []int{6, 8} {
		if c := f.checked(page); len(c) != 0 || !f.tableEmpty(page, t2) {
			t.Errorf("page %d should be a blank Part II", page)
		}
	}
	f.want(t, 3, p1+"f1_91[0]", "20.00")
	f.want(t, 5, p1+"f1_91[0]", "110.00")
	f.want(t, 7, p1+"f1_91[0]", "10.00")
	f.want(t, 4, p2+"f2_91[0]", "30.00")
	f.want(t, 1, p1+"Table_PartI[0].Row1b[0].f1_7[0]", "20.00")
	f.want(t, 1, p1+"Table_PartI[0].Row2[0].f1_11[0]", "120.00")
	f.want(t, 1, p1+"Table_PartII[0].Row9[0].f1_31[0]", "30.00")
	f.want(t, 1, p1+"f1_22[0]", "75.00")
	f.want(t, 1, p1+"f1_43[0]", "25.00")
}

func TestFillFormsLoss(t *testing.T) {
	r := yearReport(2025, disposal("I", 0.1, "SOL", 10.00, 10.09))
	f := fillForms(t, r, Options{})
	f.want(t, 3, t1+"Row1[0].f1_10[0]", "(0.09)")
	f.want(t, 3, p1+"f1_95[0]", "(0.09)")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_18[0]", "(0.09)")
	f.want(t, 1, p1+"f1_22[0]", "(0.09)")
}

func TestFillFormsVariousAndNoRows(t *testing.T) {
	row := disposal("I", 1, "BTC", 10, 5)
	row.Various = true
	f := fillForms(t, yearReport(2025, row), Options{})
	f.want(t, 3, t1+"Row1[0].f1_04[0]", "VARIOUS")

	empty := fillForms(t, yearReport(2025), Options{})
	if empty.pages != 2 {
		t.Errorf("no disposals: pages = %d, want 2 (Schedule D only)", empty.pages)
	}
	empty.want(t, 1, p1+"f1_22[0]", "0.00")
	empty.want(t, 1, p1+"f1_43[0]", "0.00")
}

func TestFitToMaxLen(t *testing.T) {
	cases := []struct {
		v    string
		max  int
		want string
	}{
		{"01/02/2025", 0, "01/02/2025"},
		{"01/02/2025", 10, "01/02/2025"},
		{"01/02/2025", 8, "01/02/25"},
		{"VARIOUS", 8, "VARIOUS"},
		{"1234.56", 7, "1234.56"},
	}
	for _, c := range cases {
		if got, err := fit(c.v, c.max); err != nil || got != c.want {
			t.Errorf("fit(%q, %d) = %q, %v; want %q", c.v, c.max, got, err, c.want)
		}
	}
	// Anything but a date is never cut: an amount or SSN that does not fit is an error.
	for _, c := range []struct {
		v   string
		max int
	}{{"12345.67", 7}, {"000-12-3456-7", 11}, {"01/02/2025", 7}} {
		if got, err := fit(c.v, c.max); err == nil {
			t.Errorf("fit(%q, %d) = %q, want an error", c.v, c.max, got)
		}
	}
}

func TestSSNText(t *testing.T) {
	for in, want := range map[string]string{
		"000123456": "000-12-3456", " 000-12-3456 ": "000-12-3456", "": "", "XX-1234": "XX-1234",
	} {
		if got := ssnText(in); got != want {
			t.Errorf("ssnText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFieldMapMatchesEmbeddedForms checks every name the field map produces against the
// embedded 2025 forms.
func TestFieldMapMatchesEmbeddedForms(t *testing.T) {
	rev, err := embeddedRevision()
	if err != nil {
		t.Fatal(err)
	}
	var names8949, namesD []string
	for page := 1; page <= 2; page++ {
		names8949 = append(names8949, header8949(page)...)
		for i := range boxCount {
			names8949 = append(names8949, checkbox8949(page, i))
		}
		for row := range rowsPerPage {
			for col := range 8 {
				names8949 = append(names8949, cell8949(page, row, col))
			}
		}
		for _, n := range totals8949 {
			names8949 = append(names8949, field8949(page, n))
		}
	}
	namesD = append(namesD, scheduleDName, scheduleDSSN, scheduleDLine7, scheduleDLine15)
	for _, cols := range scheduleDRows {
		namesD = append(namesD, cols[:]...)
	}
	for _, n := range names8949 {
		if _, ok := rev.f8949.fields[n]; !ok {
			t.Errorf("8949 field %s not in the embedded form", n)
		}
	}
	for _, n := range namesD {
		if _, ok := rev.schedD.fields[n]; !ok {
			t.Errorf("Schedule D field %s not in the embedded form", n)
		}
	}
	if rev.f8949.year != 2025 || rev.schedD.year != 2025 {
		t.Errorf("embedded revision years %d/%d, want 2025", rev.f8949.year, rev.schedD.year)
	}
}

const note2026 = "Filled on the 2025 form revision; check irs.gov for the 2026 form."

func TestFormRevisionFallback(t *testing.T) {
	r := yearReport(2026, disposal("I", 0.5, "BTC", 1500, 1000))
	f := fillForms(t, r, Options{FormsDir: t.TempDir()}) // no HTTP client: download disabled

	if f.pages != 4 {
		t.Fatalf("pages = %d, want 4", f.pages)
	}
	if !strings.HasPrefix(f.title, "2025 ") {
		t.Errorf("title %q, want the embedded 2025 revision", f.title)
	}
	if !f.contains(t, note2026) {
		t.Error("fallback note missing")
	}
	f.want(t, 3, t1+"Row1[0].f1_03[0]", "0.5 BTC")
}

// retitled is an embedded form with its Info title changed, standing in for a newer
// revision with the same fields.
func retitled(t *testing.T, name, title string) []byte {
	t.Helper()
	pdf, err := embeddedForms.ReadFile("forms/2025/" + name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := api.ReadValidateAndOptimize(bytes.NewReader(pdf), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	d, err := ctx.DereferenceDict(*ctx.Info)
	if err != nil {
		t.Fatal(err)
	}
	d["Title"] = types.StringLiteral(title)
	var b bytes.Buffer
	if err := api.WriteContext(ctx, &b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestFormRevisionFromFormsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "2026"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, title := range map[string]string{"f8949.pdf": "2026 Form 8949", "f1040sd.pdf": "2026 Schedule D (Form 1040)"} {
		if err := os.WriteFile(filepath.Join(dir, "2026", name), retitled(t, name, title), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := fillForms(t, yearReport(2026, disposal("I", 0.5, "BTC", 1500, 1000)), Options{FormsDir: dir})
	if f.title != "2026 Schedule D (Form 1040)" {
		t.Errorf("title %q, want the 2026 revision from FormsDir", f.title)
	}
	if f.contains(t, note2026) {
		t.Error("note present although the 2026 forms were used")
	}
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_15[0]", "1500.00")
}

func serveForms(t *testing.T, forms map[string][]byte, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		pdf, ok := forms[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(pdf)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFormRevisionDownloadAndCache(t *testing.T) {
	var hits atomic.Int32
	srv := serveForms(t, map[string][]byte{
		"f8949.pdf":   retitled(t, "f8949.pdf", "2026 Form 8949"),
		"f1040sd.pdf": retitled(t, "f1040sd.pdf", "2026 Schedule D (Form 1040)"),
	}, &hits)
	dir := t.TempDir()
	opts := Options{FormsDir: dir, HTTPClient: srv.Client(), FormsURL: srv.URL + "/"}
	r := yearReport(2026, disposal("I", 0.5, "BTC", 1500, 1000))

	f := fillForms(t, r, opts)
	if f.title != "2026 Schedule D (Form 1040)" || f.contains(t, note2026) {
		t.Errorf("downloaded 2026 revision not used: title %q", f.title)
	}
	for _, name := range []string{"f8949.pdf", "f1040sd.pdf"} {
		if _, err := os.Stat(filepath.Join(dir, "2026", name)); err != nil {
			t.Errorf("%s not cached: %v", name, err)
		}
	}
	before := hits.Load()
	fillForms(t, r, opts)
	if hits.Load() != before {
		t.Errorf("second fill downloaded again (%d → %d requests)", before, hits.Load())
	}
}

func TestFormRevisionDownloadRejected(t *testing.T) {
	cases := map[string]map[string][]byte{
		// irs.gov still serves the 2025 revision: not the 2026 form.
		"older revision": {
			"f8949.pdf":   retitled(t, "f8949.pdf", "2025 Form 8949"),
			"f1040sd.pdf": retitled(t, "f1040sd.pdf", "2025 Schedule D (Form 1040)"),
		},
		// The fields changed: the 2025 field map would misplace values.
		"different fields": {
			"f8949.pdf":   retitled(t, "f1040sd.pdf", "2026 Form 8949"),
			"f1040sd.pdf": retitled(t, "f1040sd.pdf", "2026 Schedule D (Form 1040)"),
		},
		"missing form": {
			"f8949.pdf": retitled(t, "f8949.pdf", "2026 Form 8949"),
		},
	}
	for name, forms := range cases {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			srv := serveForms(t, forms, &hits)
			dir := t.TempDir()
			f := fillForms(t, yearReport(2026, disposal("I", 0.5, "BTC", 1500, 1000)),
				Options{FormsDir: dir, HTTPClient: srv.Client(), FormsURL: srv.URL + "/"})
			if hits.Load() == 0 {
				t.Fatal("no download attempted")
			}
			if !strings.HasPrefix(f.title, "2025 ") || !f.contains(t, note2026) {
				t.Errorf("want the embedded 2025 fallback with note; title %q", f.title)
			}
			for _, name := range []string{"f8949.pdf", "f1040sd.pdf"} {
				if _, err := os.Stat(filepath.Join(dir, "2026", name)); err == nil {
					t.Errorf("rejected %s cached", name)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "2026", ".unavailable")); err != nil {
				t.Errorf("no unavailable marker: %v", err)
			}
		})
	}
}

func TestOlderYearGetsNote(t *testing.T) {
	f := fillForms(t, yearReport(2024, disposal("I", 1, "BTC", 10, 5)), Options{})
	if !strings.HasPrefix(f.title, "2025 ") {
		t.Errorf("title %q", f.title)
	}
	if !f.contains(t, "Filled on the 2025 form revision; check irs.gov for the 2024 form.") {
		t.Error("a 2024 report printed on the 2025 revision must carry the note")
	}
}

func ExampleFillForms() {
	r := yearReport(2025, disposal("I", 0.5, "BTC", 1500, 1000))
	var b bytes.Buffer
	err := FillForms(r, &b, Options{Name: "Jordan Q. Example"})
	fmt.Println(err, bytes.HasPrefix(b.Bytes(), []byte("%PDF-")))
	// Output: <nil> true
}

// row is an invented disposal of an asset with the given symbol and key.
func assetRow(sym, key string) report.Row {
	r := disposal("I", 1, sym, 10, 5)
	r.Asset = key
	return r
}

func TestFillFormsNonLatin1(t *testing.T) {
	r := yearReport(2025,
		assetRow("ABCD…WXYZ", "spl:ABCDefgh1234WXYZ"), // report.Resolver's own spl fallback
		assetRow("🐶WIF", "spl:Wif1aaaabbbbccccdddd9999"),
		assetRow("狗狗币", "spl:Dog9oPq2xYz7aBcDeFgHiJkLmNo1234"),
		assetRow("JOSÉ", "spl:Jose1111222233334444zzzz"), // Latin-1 is kept
	)
	f := fillForms(t, r, Options{Name: "Łukasz Example"})
	f.want(t, 1, p1+"f1_1[0]", "Lukasz Example")
	f.want(t, 3, p1+"f1_01[0]", "Lukasz Example")
	f.want(t, 4, p2+"f2_01[0]", "Lukasz Example")
	f.want(t, 3, t1+"Row1[0].f1_03[0]", "1 ABCD...WXYZ")
	f.want(t, 3, t1+"Row2[0].f1_11[0]", "1 ?WIF")
	f.want(t, 3, t1+"Row3[0].f1_19[0]", "1 Dog9...1234")
	f.want(t, 3, t1+"Row4[0].f1_27[0]", "1 JOSÉ")
}

func TestLatin1(t *testing.T) {
	for in, want := range map[string]string{
		"plain ASCII":         "plain ASCII",
		"José Müller":         "José Müller",
		"Łukasz Żółć":         "Lukasz Zólc",
		"ABCD…WXYZ":           "ABCD...WXYZ",
		"“quoted” – dash":     "\"quoted\" - dash",
		"🐶WIF":                "?WIF",
		"狗狗币":                 "???",
		"e\u0301":             "é", // decomposed é is composed first
		"tab\there\x00\u0085": "tab here",
		"Œuvre ß":             "OEuvre ß",
	} {
		if got := latin1(in); got != want {
			t.Errorf("latin1(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDescriptionFallback(t *testing.T) {
	for _, c := range []struct{ desc, sym, key, want string }{
		{"1 狗狗币", "狗狗币", "spl:Dog9oPq2xYz7aBcDeFgHiJkLmNo1234", "1 Dog9...1234"},
		{"2.5 ", "", "WEIRD", "2.5 WEIRD"},
		{"3 €€", "€€", "COIN", "3 EUREUR"},
		{"4 ??", "??", "ÄBC", "4 ÄBC"},
		{"5 BTC", "BTC", "BTC", "5 BTC"},
	} {
		got := description(report.Form8949Row{Description: c.desc, Symbol: c.sym, AssetKey: c.key})
		if got != c.want {
			t.Errorf("description(%q, key %q) = %q, want %q", c.desc, c.key, got, c.want)
		}
	}
}

// TestFillFormsCentsAddUp: every printed total is the sum of the printed rows and lines.
func TestFillFormsCentsAddUp(t *testing.T) {
	var rows []report.Row
	for range 12 {
		rows = append(rows, disposal("I", 1, "SOL", 10.004, 0.004))
	}
	rows = append(rows, disposal("G", 1, "BTC", 1.004, 0), disposal("H", 1, "ETH", 1.004, 0))
	f := fillForms(t, yearReport(2025, rows...), Options{})
	// Copies: G(1) + H(1) + I(11) + I(1) → I pages are output pages 7 and 9.
	f.want(t, 7, t1+"Row1[0].f1_06[0]", "10.00")
	f.want(t, 7, t1+"Row1[0].f1_10[0]", "10.00")
	f.want(t, 7, p1+"f1_91[0]", "110.00")
	f.want(t, 7, p1+"f1_95[0]", "110.00")
	f.want(t, 9, p1+"f1_91[0]", "10.00")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_15[0]", "120.00")
	f.want(t, 1, p1+"Table_PartI[0].Row3[0].f1_18[0]", "120.00")
	f.want(t, 1, p1+"Table_PartI[0].Row1b[0].f1_10[0]", "1.00")
	f.want(t, 1, p1+"Table_PartI[0].Row2[0].f1_14[0]", "1.00")
	f.want(t, 1, p1+"f1_22[0]", "122.00") // line 7 = 1.00 + 1.00 + 120.00
}

func TestFillFormsUnknownBox(t *testing.T) {
	var b bytes.Buffer
	err := FillForms(yearReport(2025, disposal("I", 1, "BTC", 10, 5), disposal("C", 1, "BTC", 10, 5)), &b, Options{})
	if err == nil || !strings.Contains(err.Error(), `"C"`) {
		t.Errorf("err = %v, want an unknown-box error naming C", err)
	}
}

func TestFillFormsSSNTooLong(t *testing.T) {
	var b bytes.Buffer
	if err := FillForms(yearReport(2025), &b, Options{SSN: "000-12-3456-789"}); err == nil {
		t.Error("an SSN that does not fit must be an error, not cut")
	}
}

const markerName = ".unavailable"

func TestFormRevisionUnavailableMarker(t *testing.T) {
	var hits atomic.Int32
	srv := serveForms(t, map[string][]byte{ // irs.gov still serves 2025
		"f8949.pdf":   retitled(t, "f8949.pdf", "2025 Form 8949"),
		"f1040sd.pdf": retitled(t, "f1040sd.pdf", "2025 Schedule D (Form 1040)"),
	}, &hits)
	dir := t.TempDir()
	marker := filepath.Join(dir, "2026", markerName)
	opts := Options{FormsDir: dir, HTTPClient: srv.Client(), FormsURL: srv.URL}
	r := yearReport(2026, disposal("I", 1, "BTC", 10, 5))

	fillForms(t, r, opts)
	if hits.Load() == 0 {
		t.Fatal("no download attempted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker: %v", err)
	}
	n := hits.Load()
	if f := fillForms(t, r, opts); hits.Load() != n || !f.contains(t, note2026) {
		t.Errorf("second fill within 24h downloaded again (%d → %d) or lost the note", n, hits.Load())
	}

	later := opts
	later.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	fillForms(t, r, later)
	if hits.Load() == n {
		t.Error("25h later (injected clock) no download was attempted")
	}

	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	n = hits.Load()
	fillForms(t, r, opts)
	if hits.Load() == n {
		t.Error("a marker older than 24h must not block the download")
	}
}

func TestFormRevisionMarkerClearedOnAccept(t *testing.T) {
	var hits atomic.Int32
	srv := serveForms(t, map[string][]byte{
		"f8949.pdf":   retitled(t, "f8949.pdf", "2026 Form 8949"),
		"f1040sd.pdf": retitled(t, "f1040sd.pdf", "2026 Schedule D (Form 1040)"),
	}, &hits)
	dir := t.TempDir()
	marker := filepath.Join(dir, "2026", markerName)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	f := fillForms(t, yearReport(2026, disposal("I", 1, "BTC", 10, 5)),
		Options{FormsDir: dir, HTTPClient: srv.Client(), FormsURL: srv.URL})
	if f.title != "2026 Schedule D (Form 1040)" {
		t.Errorf("title %q, want the downloaded 2026 revision", f.title)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker not cleared after accepting a revision: %v", err)
	}
}

func TestRejectedCachedFormsReplaced(t *testing.T) {
	dir := t.TempDir()
	year := filepath.Join(dir, "2026")
	if err := os.MkdirAll(year, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, title := range map[string]string{"f8949.pdf": "2025 Form 8949", "f1040sd.pdf": "2025 Schedule D (Form 1040)"} {
		if err := os.WriteFile(filepath.Join(year, name), retitled(t, name, title), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := yearReport(2026, disposal("I", 1, "BTC", 10, 5))

	// No client: the rejected files are removed and the embedded revision used.
	if f := fillForms(t, r, Options{FormsDir: dir}); !f.contains(t, note2026) {
		t.Error("rejected cached forms used without the note")
	}
	for _, name := range []string{"f8949.pdf", "f1040sd.pdf"} {
		if _, err := os.Stat(filepath.Join(year, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("rejected %s not removed: %v", name, err)
		}
	}

	// Rejected files again, now with a client serving the 2026 forms: they are replaced.
	for name, title := range map[string]string{"f8949.pdf": "2025 Form 8949", "f1040sd.pdf": "2025 Schedule D (Form 1040)"} {
		if err := os.WriteFile(filepath.Join(year, name), retitled(t, name, title), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var hits atomic.Int32
	srv := serveForms(t, map[string][]byte{
		"f8949.pdf":   retitled(t, "f8949.pdf", "2026 Form 8949"),
		"f1040sd.pdf": retitled(t, "f1040sd.pdf", "2026 Schedule D (Form 1040)"),
	}, &hits)
	f := fillForms(t, r, Options{FormsDir: dir, HTTPClient: srv.Client(), FormsURL: srv.URL})
	if f.title != "2026 Schedule D (Form 1040)" {
		t.Errorf("title %q, want the replacement 2026 revision", f.title)
	}
	cached, err := os.ReadFile(filepath.Join(year, "f8949.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if tm, err := inspect(cached); err != nil || tm.year != 2026 {
		t.Errorf("cached 8949 not replaced: year %d, %v", tm.year, err)
	}
}

// withMaxLen sets MaxLen on the field whose partial name is name.
func withMaxLen(t *testing.T, pdf []byte, name string, n int) []byte {
	t.Helper()
	ctx, err := api.ReadValidateAndOptimize(bytes.NewReader(pdf), model.NewDefaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for nr := range ctx.Table {
		d, err := ctx.DereferenceDict(*types.NewIndirectRef(nr, 0))
		if err != nil || d == nil {
			continue
		}
		if o, ok := d.Find("T"); ok {
			if s, err := types.StringOrHexLiteral(o); err == nil && s != nil && *s == name {
				d["MaxLen"] = types.Integer(n)
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no field %s", name)
	}
	var b bytes.Buffer
	if err := api.WriteContext(ctx, &b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestFormRevisionMaxLenChanged: a newer revision that limits an amount field is rejected,
// so no amount is ever cut.
func TestFormRevisionMaxLenChanged(t *testing.T) {
	dir := t.TempDir()
	year := filepath.Join(dir, "2026")
	if err := os.MkdirAll(year, 0o755); err != nil {
		t.Fatal(err)
	}
	f8949 := withMaxLen(t, retitled(t, "f8949.pdf", "2026 Form 8949"), "f1_06[0]", 4)
	if err := os.WriteFile(filepath.Join(year, "f8949.pdf"), f8949, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(year, "f1040sd.pdf"), retitled(t, "f1040sd.pdf", "2026 Schedule D (Form 1040)"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := fillForms(t, yearReport(2026, disposal("I", 1, "BTC", 12345.67, 5)), Options{FormsDir: dir})
	if !strings.HasPrefix(f.title, "2025 ") || !f.contains(t, note2026) {
		t.Errorf("title %q: a revision with a new maxlen must fall back to 2025", f.title)
	}
	f.want(t, 3, t1+"Row1[0].f1_06[0]", "12345.67")
}

func TestFillFormsContextCanceled(t *testing.T) {
	var hits atomic.Int32
	srv := serveForms(t, map[string][]byte{}, &hits)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var b bytes.Buffer
	err := FillFormsContext(ctx, yearReport(2026, disposal("I", 1, "BTC", 10, 5)), &b,
		Options{FormsDir: dir, HTTPClient: srv.Client(), FormsURL: srv.URL})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026", markerName)); err == nil {
		t.Error("a canceled request must not mark the revision unavailable")
	}
}

// TestFillFormsContextCanceledDuringDownload: a request canceled mid-download is the
// caller's error, not a sign that irs.gov lacks the revision.
func TestFillFormsContextCanceledDuringDownload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	var b bytes.Buffer
	err := FillFormsContext(ctx, yearReport(2026, disposal("I", 1, "BTC", 10, 5)), &b,
		Options{FormsDir: dir, HTTPClient: srv.Client(), FormsURL: srv.URL})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026", markerName)); err == nil {
		t.Error("a canceled download must not mark the revision unavailable")
	}
}
