// Package irs fills the official IRS Form 8949 and Schedule D (Form 1040) PDFs from a tax
// year's report, as Koinly's IRS package does.
package irs

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/form"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"crypto-portfolio/internal/tax/report"
)

// Options personalise the filled forms and say where newer form revisions live.
type Options struct {
	// Name and SSN go in the header of every page that has one; empty leaves it blank.
	Name, SSN string
	// FormsDir caches newer revisions as <FormsDir>/<year>/f8949.pdf and f1040sd.pdf
	// (normally <data>/irs). Empty disables newer revisions.
	FormsDir string
	// HTTPClient downloads a missing newer revision from irs.gov; nil disables the
	// download.
	HTTPClient *http.Client
	// FormsURL is the directory URL the forms are downloaded from; "" means
	// https://www.irs.gov/pub/irs-pdf/.
	FormsURL string
	// Now is the clock for the 24-hour "revision unavailable" marker; nil means time.Now.
	Now func() time.Time
}

//go:embed forms/2025/f8949.pdf forms/2025/f1040sd.pdf
var embeddedForms embed.FS

func init() {
	// pdfcpu's default configuration writes a config dir under the user's home and exits
	// the process when it cannot; the forms need none of it (no user fonts).
	api.DisableConfigDir()
}

func conf() *model.Configuration { return model.NewDefaultConfiguration() }

// FillForms is FillFormsContext without cancellation.
func FillForms(r report.YearReport, w io.Writer, opts Options) error {
	return FillFormsContext(context.Background(), r, w, opts)
}

// FillFormsContext writes one merged PDF: Schedule D (2 pages), then one Form 8949 copy
// (2 pages) per page chunk. Part I chunks fill page 1 of a copy and Part II chunks page 2,
// paired in order; a copy without a chunk for one part keeps that page blank, as the IRS
// form does. ctx bounds the download of a newer form revision and the filling.
func FillFormsContext(ctx context.Context, r report.YearReport, w io.Writer, opts Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, row := range r.Rows {
		if _, ok := boxIndex[row.Box]; !ok {
			return fmt.Errorf("irs: disposal box %q is not a digital-asset box (G-L)", row.Box)
		}
	}
	rev, err := revisionFor(ctx, r.Year, opts)
	if err != nil {
		return err
	}
	parts, err := report.Form8949(r)
	if err != nil {
		return fmt.Errorf("irs: %w", err)
	}
	lines, err := report.ScheduleD(r)
	if err != nil {
		return fmt.Errorf("irs: %w", err)
	}

	schedD, err := rev.schedD.fill(scheduleDValues(lines, parts, opts), nil)
	if err != nil {
		return fmt.Errorf("irs: schedule D: %w", err)
	}
	if rev.note != "" {
		if schedD, err = stampNote(schedD, rev.note); err != nil {
			return fmt.Errorf("irs: revision note: %w", err)
		}
	}
	docs := []io.ReadSeeker{bytes.NewReader(schedD)}

	short, long := chunks(parts)
	for i := range max(len(short), len(long)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		values := map[string]string{}
		var checks []string
		for p, cs := range [][]chunk{short, long} {
			page := p + 1 // Part I on page 1, Part II on page 2
			h := header8949(page)
			values[h[0]], values[h[1]] = opts.Name, ssnText(opts.SSN)
			if i < len(cs) {
				box, err := cs[i].fill(page, values)
				if err != nil {
					return err
				}
				checks = append(checks, box)
			}
		}
		pdf, err := rev.f8949.fill(values, checks)
		if err != nil {
			return fmt.Errorf("irs: form 8949 copy %d: %w", i+1, err)
		}
		docs = append(docs, bytes.NewReader(pdf))
	}
	return api.MergeRaw(docs, w, false, conf())
}

// --- Form 8949 field map (2025 revision) ---

const (
	rowsPerPage = 11 // rows per Form 8949 page on the 2025 revision
	boxCount    = 6  // checkboxes per page: A B C G H I (page 1), D E F J K L (page 2)
)

// totals8949 are the line-2 totals fields (d) (e) (g) (h); f_93 (f) is a locked blank.
var totals8949 = [4]int{91, 92, 94, 95}

// boxIndex is a digital-asset box's checkbox index on its page.
var boxIndex = map[string]int{"G": 3, "H": 4, "I": 5, "J": 3, "K": 4, "L": 5}

func field8949(page, n int) string {
	return fmt.Sprintf("topmostSubform[0].Page%d[0].f%d_%02d[0]", page, page, n)
}

// header8949 is a page's name and SSN fields.
func header8949(page int) []string { return []string{field8949(page, 1), field8949(page, 2)} }

func checkbox8949(page, i int) string {
	return fmt.Sprintf("topmostSubform[0].Page%d[0].c%d_1[%d]", page, page, i)
}

// cell8949 is column col (0 = a … 7 = h) of row (0-based) in a page's table.
func cell8949(page, row, col int) string {
	return fmt.Sprintf("topmostSubform[0].Page%d[0].Table_Line1_Part%d[0].Row%d[0].f%d_%02d[0]",
		page, page, row+1, page, 3+8*row+col)
}

// chunk is the rows of one box that fit on one Form 8949 page.
type chunk struct {
	box  string
	rows []report.Form8949Row
}

// chunks splits the parts into page chunks, Part I (short) and Part II (long), in box order.
func chunks(parts []report.Form8949Part) (short, long []chunk) {
	for _, p := range parts {
		for _, rows := range report.Pages(p, rowsPerPage) {
			if p.Long {
				long = append(long, chunk{p.Box, rows})
			} else {
				short = append(short, chunk{p.Box, rows})
			}
		}
	}
	return short, long
}

// fill sets the chunk's rows and totals on page and returns the box checkbox to check.
// The amounts come rounded to cents from report (h = d − e, totals = Σ rows); columns (f)
// code and (g) adjustment stay blank on the rows, and the (g) total is 0.
func (c chunk) fill(page int, values map[string]string) (string, error) {
	idx, ok := boxIndex[c.box]
	if !ok {
		return "", fmt.Errorf("irs: box %q is not a digital-asset box (G-L)", c.box)
	}
	for i, row := range c.rows {
		for col, v := range map[int]string{
			0: description(row), 1: row.Acquired, 2: row.Sold,
			3: report.Money8949(row.Proceeds), 4: report.Money8949(row.Cost), 7: report.Money8949(row.Gain),
		} {
			values[cell8949(page, i, col)] = v
		}
	}
	t := report.PageTotal(c.rows)
	for i, v := range []float64{t.Proceeds, t.Cost, 0, t.Gain} {
		values[field8949(page, totals8949[i])] = report.Money8949(v)
	}
	return checkbox8949(page, idx), nil
}

// --- Schedule D field map (2025 revision) ---

const (
	scheduleDName   = "topmostSubform[0].Page1[0].f1_1[0]"
	scheduleDSSN    = "topmostSubform[0].Page1[0].f1_2[0]"
	scheduleDLine7  = "topmostSubform[0].Page1[0].f1_22[0]"
	scheduleDLine15 = "topmostSubform[0].Page1[0].f1_43[0]"
)

func scheduleDRow(table, row string, first int) [4]string {
	var out [4]string
	for i := range out {
		out[i] = fmt.Sprintf("topmostSubform[0].Page1[0].%s[0].%s[0].f1_%d[0]", table, row, first+i)
	}
	return out
}

// scheduleDRows are the (d) (e) (g) (h) fields of each digital-asset line.
var scheduleDRows = map[string][4]string{
	"1b": scheduleDRow("Table_PartI", "Row1b", 7),
	"2":  scheduleDRow("Table_PartI", "Row2", 11),
	"3":  scheduleDRow("Table_PartI", "Row3", 15),
	"8b": scheduleDRow("Table_PartII", "Row8b", 27),
	"9":  scheduleDRow("Table_PartII", "Row9", 31),
	"10": scheduleDRow("Table_PartII", "Row10", 35),
}

// scheduleDValues fills the lines whose box has rows, and the net lines 7 and 15. Each
// line is the sum of its box's printed rows and lines 7/15 the sums of the printed lines
// (report works in whole cents).
func scheduleDValues(lines []report.ScheduleDLine, parts []report.Form8949Part, opts Options) map[string]string {
	values := map[string]string{scheduleDName: opts.Name, scheduleDSSN: ssnText(opts.SSN)}
	for _, l := range lines {
		if !slices.ContainsFunc(parts, func(p report.Form8949Part) bool { return p.Box == l.Box }) {
			continue
		}
		f := scheduleDRows[l.Line]
		for i, v := range []float64{l.Proceeds, l.Cost, l.Adjustments, l.Gain} {
			values[f[i]] = report.Money8949(v)
		}
	}
	short, long := report.ScheduleDNet(lines)
	values[scheduleDLine7] = report.Money8949(short)
	values[scheduleDLine15] = report.Money8949(long)
	return values
}

// --- filling ---

// template is a blank fillable form.
type template struct {
	pdf  []byte
	year int // revision year, from the PDF title ("2025 Form 8949")
	// fields maps each field name to its kind and text maxlen.
	fields map[string]fieldInfo
}

type fieldInfo struct {
	kind   string // text | date | check | radio | combo | list
	maxLen int
}

// inspect reads a blank form's revision and fields.
func inspect(pdf []byte) (template, error) {
	fg, err := api.ExportForm(bytes.NewReader(pdf), "form.pdf", conf())
	if err != nil {
		return template{}, err
	}
	t := template{pdf: pdf, fields: map[string]fieldInfo{}}
	if y, err := strconv.Atoi(strings.SplitN(fg.Header.Title, " ", 2)[0]); err == nil {
		t.year = y
	}
	for _, f := range fg.Forms {
		for _, tf := range f.TextFields {
			t.fields[tf.Name] = fieldInfo{kind: "text", maxLen: tf.MaxLen}
		}
		for _, df := range f.DateFields {
			t.fields[df.Name] = fieldInfo{kind: "date"}
		}
		for _, cb := range f.CheckBoxes {
			t.fields[cb.Name] = fieldInfo{kind: "check"}
		}
		for _, g := range f.RadioButtonGroups {
			t.fields[g.Name] = fieldInfo{kind: "radio"}
		}
		for _, c := range f.ComboBoxes {
			t.fields[c.Name] = fieldInfo{kind: "combo"}
		}
		for _, l := range f.ListBoxes {
			t.fields[l.Name] = fieldInfo{kind: "list"}
		}
	}
	return t, nil
}

// sameFields reports whether t has exactly the fields of base (name, kind and maxlen), so
// the 2025 field map places every value correctly and nothing that fit there is too long.
func (t template) sameFields(base template) bool {
	if len(t.fields) != len(base.fields) {
		return false
	}
	for name, f := range base.fields {
		if g, ok := t.fields[name]; !ok || g != f {
			return false
		}
	}
	return true
}

// fill sets the text values (blank ones are skipped) and checks the named checkboxes.
// Every value is converted to Latin-1, which the forms' font can show; a value longer than
// its field is an error (dates excepted, see fit). pdfcpu drops the IRS signature (and the
// XFA form) when filling, as expected.
func (t template) fill(values map[string]string, checks []string) ([]byte, error) {
	var f form.Form
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		info, ok := t.fields[name]
		if !ok || info.kind != "text" {
			return nil, fmt.Errorf("no text field %s", name)
		}
		v, err := fit(latin1(values[name]), info.maxLen)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", name, err)
		}
		if v != "" {
			f.TextFields = append(f.TextFields, &form.TextField{Name: name, Value: v})
		}
	}
	for _, name := range checks {
		if t.fields[name].kind != "check" {
			return nil, fmt.Errorf("no checkbox %s", name)
		}
		f.CheckBoxes = append(f.CheckBoxes, &form.CheckBox{Name: name, Value: true})
	}
	if len(f.TextFields) == 0 && len(f.CheckBoxes) == 0 {
		return t.pdf, nil
	}
	data, err := json.Marshal(form.FormGroup{Forms: []form.Form{f}})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := api.FillForm(bytes.NewReader(t.pdf), bytes.NewReader(data), &b, conf()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// stampNote writes note across the top margin of page 1.
func stampNote(pdf []byte, note string) ([]byte, error) {
	wm, err := api.TextWatermark(note,
		"font:Helvetica, points:9, scale:1 abs, pos:tc, off:0 -14, fillcolor:#a00000, rot:0, opacity:1",
		true, false, types.POINTS)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := api.AddWatermarks(bytes.NewReader(pdf), &b, []string{"1"}, wm, conf()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
