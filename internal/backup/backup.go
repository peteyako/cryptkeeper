// Package backup exports and restores everything a user configures in one YAML file:
// sources, portfolios, alert rules and manual cost basis, tax settings and closed tax years,
// and optionally the database (activity, price and chart history, saved scans). Every
// section is optional on import, so a sources-only or portfolios-only export restores
// through the same path.
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"crypto-portfolio/internal/alerts"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/store"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/yamlfile"
)

// Version of the backup format.
const Version = 1

// File is the backup document. Sections are omitted when empty.
type File struct {
	Version    int                    `yaml:"version"`
	Sources    []sources.Def          `yaml:"sources,omitempty"`
	Portfolios []portfolios.Portfolio `yaml:"portfolios,omitempty"`
	Alerts     []alerts.Rule          `yaml:"alerts,omitempty"`
	CostBasis  *costbasis.Manual      `yaml:"costBasis,omitempty"`
	Tax        *tax.Settings          `yaml:"tax,omitempty"`
	TaxYears   []tax.Year             `yaml:"taxYears,omitempty"`
	// Database is the SQLite database (store.DB.Snapshot), gzip-compressed and base64
	// encoded; present only in an export that asked for it.
	Database string `yaml:"database,omitempty"`
}

// Stores are the places a backup reads from and restores into. Any may be nil (that
// section is then skipped on export and rejected on import).
type Stores struct {
	Sources    *sources.Registry
	Portfolios *portfolios.Store
	Alerts     *alerts.RuleStore
	CostBasis  *costbasis.ManualStore
	Tax        *tax.SettingsStore
	TaxYears   *tax.YearStore
	DB         *store.DB
	// OnSourceRemoved runs for each source a replace import deletes, before portfolios are
	// restored (callers drop the source's activity and portfolio references).
	OnSourceRemoved func(id string)
}

// Export builds a backup. Without withSecrets, exchange secrets are blanked; with it they
// stay sealed (only importable by a CryptKeep with the same secret key).
func Export(st Stores, withSecrets bool) ([]byte, error) {
	f, err := build(st, withSecrets)
	if err != nil {
		return nil, err
	}
	return yamlfile.Encode(f)
}

// ExportDatabase is Export with the database included (st.DB): a consistent snapshot taken
// while the app runs. Secrets the database holds (saved API keys) stay sealed either way.
func ExportDatabase(ctx context.Context, st Stores, withSecrets bool) ([]byte, error) {
	if st.DB == nil {
		return nil, errors.New("no database to export")
	}
	f, err := build(st, withSecrets)
	if err != nil {
		return nil, err
	}
	snap, err := st.DB.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(snap); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	f.Database = base64.StdEncoding.EncodeToString(buf.Bytes())
	return yamlfile.Encode(f)
}

// maxDatabase bounds the database a restore unpacks, against a file that would inflate
// without end.
const maxDatabase = 2 << 30

// database unpacks the backup's database section.
func (f File) database() ([]byte, error) {
	packed, err := base64.StdEncoding.DecodeString(f.Database)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxDatabase+1))
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if len(raw) > maxDatabase {
		return nil, errors.New("database: larger than 2 GB unpacked")
	}
	return raw, nil
}

// build gathers every section but the database.
func build(st Stores, withSecrets bool) (File, error) {
	f := File{Version: Version}
	if st.Sources != nil {
		raw, err := st.Sources.Export(withSecrets)
		if err != nil {
			return File{}, err
		}
		sf, err := sources.ParseFile(raw)
		if err != nil {
			return File{}, err
		}
		f.Sources = sf.Sources
	}
	if st.Portfolios != nil {
		raw, err := st.Portfolios.Export("")
		if err != nil {
			return File{}, err
		}
		pf, err := portfolios.Parse(raw)
		if err != nil {
			return File{}, err
		}
		f.Portfolios = pf.Portfolios
	}
	if st.Alerts != nil {
		f.Alerts = st.Alerts.List()
	}
	if st.CostBasis != nil {
		if m := st.CostBasis.Get(); len(m.Assets) > 0 || len(m.Lots) > 0 {
			f.CostBasis = &m
		}
	}
	if st.Tax != nil {
		if t := st.Tax.Get(); !isEmptySettings(t) {
			f.Tax = &t
		}
	}
	if st.TaxYears != nil {
		for _, y := range st.TaxYears.Years() {
			if v, ok := st.TaxYears.Get(y); ok {
				f.TaxYears = append(f.TaxYears, v)
			}
		}
	}
	return f, nil
}

func isEmptySettings(t tax.Settings) bool {
	return len(t.WalletMap) == 0 && len(t.AssetMap) == 0 && len(t.ManualLots) == 0 &&
		len(t.Tags) == 0 && len(t.Prices) == 0 && len(t.WalletBox) == 0 &&
		len(t.WriteOffs) == 0 && t.Scope == ""
}

// Parse strictly decodes a backup (unknown fields are line-numbered errors) and validates
// every section, so an import can check everything before writing anything.
func Parse(raw []byte) (File, error) {
	var f File
	if err := yamlfile.Decode(raw, &f); err != nil {
		return File{}, err
	}
	if f.Version == 0 {
		f.Version = Version
	}
	if f.Version != Version {
		return File{}, fmt.Errorf("unsupported backup version %d (this build reads version %d)", f.Version, Version)
	}
	if len(f.Sources) == 0 && len(f.Portfolios) == 0 && len(f.Alerts) == 0 && f.CostBasis == nil &&
		f.Tax == nil && len(f.TaxYears) == 0 && f.Database == "" {
		return File{}, fmt.Errorf("the file has nothing to import (no sources, portfolios, alerts, cost basis, tax data or database)")
	}
	for i := range f.Sources {
		if f.Sources[i].Kind == "" {
			f.Sources[i].Kind = sources.DefaultKind(f.Sources[i].Type)
		}
		if err := f.Sources[i].Validate(); err != nil {
			return File{}, fmt.Errorf("sources: %w", err)
		}
	}
	if len(f.Portfolios) > 0 {
		pf := portfolios.File{Version: portfolios.Version, Portfolios: f.Portfolios}
		raw, err := pf.Encode()
		if err != nil {
			return File{}, err
		}
		if pf, err = portfolios.Parse(raw); err != nil {
			return File{}, fmt.Errorf("portfolios: %w", err)
		}
		f.Portfolios = pf.Portfolios
	}
	if len(f.Alerts) > 0 {
		rules, err := alerts.NormalizeRules(f.Alerts)
		if err != nil {
			return File{}, fmt.Errorf("alerts: %w", err)
		}
		f.Alerts = rules
	}
	if f.CostBasis != nil {
		raw, err := f.CostBasis.Encode()
		if err != nil {
			return File{}, err
		}
		m, err := costbasis.ParseManual(raw)
		if err != nil {
			return File{}, fmt.Errorf("cost basis: %w", err)
		}
		f.CostBasis = &m
	}
	if f.Tax != nil {
		t, err := f.Tax.Normalized()
		if err != nil {
			return File{}, err
		}
		f.Tax = &t
	}
	seen := map[int]bool{}
	for _, y := range f.TaxYears {
		if err := y.Validate(); err != nil {
			return File{}, fmt.Errorf("taxYears: %w", err)
		}
		if seen[y.Year] {
			return File{}, fmt.Errorf("taxYears: year %d appears twice", y.Year)
		}
		seen[y.Year] = true
	}
	slices.SortFunc(f.TaxYears, func(a, b tax.Year) int { return a.Year - b.Year })
	return f, nil
}

// Result reports what an import did, per section present in the file.
type Result struct {
	Sources    *sources.ImportResult
	Portfolios *portfolios.ImportResult
	Alerts     *alerts.ImportResult
	CostBasis  bool // the cost basis section was applied
	Tax        bool // the tax settings section was applied
	TaxYears   int  // closed tax years written
	// TaxYearsSkipped are incoming years a merge left out: the year exists locally, or a
	// later local year is closed.
	TaxYearsSkipped []int
	// Database is set when the file's database was restored (merged, or replacing this one).
	Database bool
}

// TouchedTax reports whether the import changed tax settings or closed years.
func (r Result) TouchedTax() bool { return r.Tax || r.TaxYears > 0 }

// Import restores every section present in raw. Sections absent from the file are left
// alone, even with replace. With replace, each present section becomes exactly the file's
// content; otherwise entries are merged by id (sources, portfolios, alert rules) or by asset
// (cost basis; lots are added unless identical). All sections are validated first.
func Import(st Stores, raw []byte, replace bool) (Result, error) {
	f, err := Parse(raw)
	if err != nil {
		return Result{}, err
	}
	if err := st.check(f); err != nil {
		return Result{}, err
	}
	var db []byte
	if f.Database != "" {
		if db, err = f.database(); err != nil { // unpacked before anything changes
			return Result{}, err
		}
	}
	var res Result
	if len(f.Sources) > 0 {
		r, err := st.Sources.Import(sources.File{Version: 1, Sources: f.Sources}, replace)
		if err != nil {
			return res, fmt.Errorf("sources: %w", err)
		}
		res.Sources = &r
		if st.OnSourceRemoved != nil {
			for _, id := range r.Removed {
				st.OnSourceRemoved(id)
			}
		}
	}
	if len(f.Portfolios) > 0 {
		pf, err := portfolios.File{Version: portfolios.Version, Portfolios: f.Portfolios}.Encode()
		if err != nil {
			return res, err
		}
		mode := portfolios.ImportMerge
		if replace {
			mode = portfolios.ImportReplace
		}
		r, err := st.Portfolios.Import(pf, mode)
		if err != nil {
			return res, fmt.Errorf("portfolios: %w", err)
		}
		res.Portfolios = &r
	}
	if len(f.Alerts) > 0 {
		r, err := st.Alerts.Import(f.Alerts, replace)
		if err != nil {
			return res, fmt.Errorf("alerts: %w", err)
		}
		res.Alerts = &r
	}
	if f.CostBasis != nil {
		next := *f.CostBasis
		if !replace {
			next = mergeManual(st.CostBasis.Get(), next)
		}
		if err := st.CostBasis.Set(next); err != nil {
			return res, fmt.Errorf("cost basis: %w", err)
		}
		res.CostBasis = true
	}
	if f.Tax != nil {
		next := *f.Tax
		if !replace {
			next = mergeTax(st.Tax.Get(), next)
		}
		cur, err := st.Tax.Get().Normalized()
		if err != nil {
			return res, fmt.Errorf("tax settings: %w", err)
		}
		if next, err = next.Normalized(); err != nil {
			return res, fmt.Errorf("tax settings: %w", err)
		}
		if !reflect.DeepEqual(cur, next) {
			if err := st.Tax.Set(next); err != nil {
				return res, fmt.Errorf("tax settings: %w", err)
			}
			res.Tax = true
		}
	}
	if len(f.TaxYears) > 0 {
		if err := importTaxYears(st.TaxYears, f.TaxYears, replace, &res); err != nil {
			return res, fmt.Errorf("tax years: %w", err)
		}
	}
	// Last, so a replace that removed sources (and their activity) gets the backup's back.
	if f.Database != "" {
		if _, err := st.DB.Restore(context.Background(), db, replace); err != nil {
			return res, fmt.Errorf("database: %w", err)
		}
		res.Database = true
	}
	return res, nil
}

func (st Stores) check(f File) error {
	switch {
	case len(f.Sources) > 0 && st.Sources == nil,
		len(f.Portfolios) > 0 && st.Portfolios == nil,
		len(f.Alerts) > 0 && st.Alerts == nil,
		f.CostBasis != nil && st.CostBasis == nil,
		f.Tax != nil && st.Tax == nil,
		len(f.TaxYears) > 0 && st.TaxYears == nil,
		f.Database != "" && st.DB == nil:
		return fmt.Errorf("this CryptKeep can't restore every section of the file")
	}
	return nil
}

// mergeManual upserts the backup's per-asset entries and adds lots that aren't already there.
func mergeManual(cur, in costbasis.Manual) costbasis.Manual {
	out := cur
	out.Assets = slices.Clone(cur.Assets)
	out.Lots = slices.Clone(cur.Lots)
	for _, a := range in.Assets {
		if i := slices.IndexFunc(out.Assets, func(x costbasis.ManualAsset) bool { return x.Asset == a.Asset }); i >= 0 {
			out.Assets[i] = a
		} else {
			out.Assets = append(out.Assets, a)
		}
	}
	for _, l := range in.Lots {
		if !slices.Contains(out.Lots, l) {
			out.Lots = append(out.Lots, l)
		}
	}
	if out.Version == 0 {
		out.Version = in.Version
	}
	return out
}

// mergeTax overlays the backup's map entries per key, takes its tax scope when it has one,
// and appends manual lots that aren't already there.
func mergeTax(cur, in tax.Settings) tax.Settings {
	out := cur
	out.WalletMap = overlay(cur.WalletMap, in.WalletMap)
	out.AssetMap = overlay(cur.AssetMap, in.AssetMap)
	out.Tags = overlay(cur.Tags, in.Tags)
	out.Prices = overlay(cur.Prices, in.Prices)
	out.WalletBox = overlay(cur.WalletBox, in.WalletBox)
	if in.Scope != "" {
		out.Scope = in.Scope
	}
	out.ManualLots = slices.Clone(cur.ManualLots)
	for _, l := range in.ManualLots {
		if !slices.ContainsFunc(out.ManualLots, func(x tax.Lot) bool {
			return x.Wallet == l.Wallet && x.Asset == l.Asset && x.Amount == l.Amount && x.CostUSD == l.CostUSD && x.Acquired.Equal(l.Acquired) && x.Various == l.Various && x.TxID == l.TxID && x.Flag == l.Flag
		}) {
			out.ManualLots = append(out.ManualLots, l)
		}
	}
	out.WriteOffs = slices.Clone(cur.WriteOffs)
	for _, w := range in.WriteOffs {
		if !slices.ContainsFunc(out.WriteOffs, func(x tax.WriteOff) bool {
			return x.Wallet == w.Wallet && x.Asset == w.Asset && x.Amount == w.Amount && x.Date.Equal(w.Date) && x.ProceedsUSD == w.ProceedsUSD && x.Note == w.Note
		}) {
			out.WriteOffs = append(out.WriteOffs, w)
		}
	}
	return out
}

func overlay[V any](cur, in map[string]V) map[string]V {
	if len(cur) == 0 && len(in) == 0 {
		return nil
	}
	out := maps.Clone(cur)
	if out == nil {
		out = map[string]V{}
	}
	maps.Copy(out, in)
	return out
}

// importTaxYears replaces every closed year (replace) or adds the years that aren't closed
// locally, ascending, leaving out any year earlier than a later local one. years is ascending.
func importTaxYears(ys *tax.YearStore, years []tax.Year, replace bool, res *Result) error {
	if replace {
		if err := ys.ReplaceAll(years); err != nil {
			return err
		}
		res.TaxYears = len(years)
		return nil
	}
	for _, y := range years {
		last, ok := ys.LastClosed()
		_, have := ys.Get(y.Year)
		if have || (ok && last.Year > y.Year) {
			res.TaxYearsSkipped = append(res.TaxYearsSkipped, y.Year)
			continue
		}
		if err := ys.Save(y); err != nil {
			return err
		}
		res.TaxYears++
	}
	return nil
}

// Summary is a one-line description of an import for the UI and the CLI.
func (r Result) Summary() string {
	var parts []string
	if r.Sources != nil {
		s := fmt.Sprintf("sources %d added, %d replaced, %d unchanged", r.Sources.Added, r.Sources.Replaced, r.Sources.Unchanged)
		if n := len(r.Sources.Removed); n > 0 {
			s += fmt.Sprintf(", %d removed", n)
		}
		parts = append(parts, s)
	}
	if r.Portfolios != nil {
		s := fmt.Sprintf("portfolios %d added, %d replaced, %d unchanged", r.Portfolios.Added, r.Portfolios.Replaced, r.Portfolios.Unchanged)
		if r.Portfolios.Removed > 0 {
			s += fmt.Sprintf(", %d removed", r.Portfolios.Removed)
		}
		parts = append(parts, s)
	}
	if r.Alerts != nil {
		s := fmt.Sprintf("alert rules %d added, %d replaced, %d unchanged", r.Alerts.Added, r.Alerts.Replaced, r.Alerts.Unchanged)
		if r.Alerts.Removed > 0 {
			s += fmt.Sprintf(", %d removed", r.Alerts.Removed)
		}
		parts = append(parts, s)
	}
	if r.CostBasis {
		parts = append(parts, "manual cost basis restored")
	}
	if r.Tax {
		parts = append(parts, "tax settings restored")
	}
	if r.Database {
		parts = append(parts, "database restored")
	}
	if r.TaxYears > 0 || len(r.TaxYearsSkipped) > 0 {
		s := fmt.Sprintf("%d closed tax %s restored", r.TaxYears, plural(r.TaxYears, "year", "years"))
		if n := len(r.TaxYearsSkipped); n > 0 {
			s += fmt.Sprintf(", %d skipped (%s already closed here or earlier than a closed year)", n, joinInts(r.TaxYearsSkipped))
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "Nothing to import."
	}
	out := "Imported: "
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out + "."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func joinInts(ns []int) string {
	var b strings.Builder
	for i, n := range ns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Itoa(n))
	}
	return b.String()
}
