// Package backup exports and restores everything a user configures in one YAML file:
// sources, portfolios, alert rules and manual cost basis. Every section is optional on
// import, so a sources-only or portfolios-only export restores through the same path.
package backup

import (
	"fmt"
	"slices"

	"crypto-portfolio/internal/alerts"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/sources"
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
}

// Stores are the places a backup reads from and restores into. Any may be nil (that
// section is then skipped on export and rejected on import).
type Stores struct {
	Sources    *sources.Registry
	Portfolios *portfolios.Store
	Alerts     *alerts.RuleStore
	CostBasis  *costbasis.ManualStore
	// OnSourceRemoved runs for each source a replace import deletes, before portfolios are
	// restored (callers drop the source's activity and portfolio references).
	OnSourceRemoved func(id string)
}

// Export builds a backup. Without withSecrets, exchange secrets are blanked; with it they
// stay sealed (only importable by a Vault with the same secret key).
func Export(st Stores, withSecrets bool) ([]byte, error) {
	f := File{Version: Version}
	if st.Sources != nil {
		raw, err := st.Sources.Export(withSecrets)
		if err != nil {
			return nil, err
		}
		sf, err := sources.ParseFile(raw)
		if err != nil {
			return nil, err
		}
		f.Sources = sf.Sources
	}
	if st.Portfolios != nil {
		raw, err := st.Portfolios.Export("")
		if err != nil {
			return nil, err
		}
		pf, err := portfolios.Parse(raw)
		if err != nil {
			return nil, err
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
	return yamlfile.Encode(f)
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
	if len(f.Sources) == 0 && len(f.Portfolios) == 0 && len(f.Alerts) == 0 && f.CostBasis == nil {
		return File{}, fmt.Errorf("the file has nothing to import (no sources, portfolios, alerts or cost basis)")
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
	return f, nil
}

// Result reports what an import did, per section present in the file.
type Result struct {
	Sources    *sources.ImportResult
	Portfolios *portfolios.ImportResult
	Alerts     *alerts.ImportResult
	CostBasis  bool // the cost basis section was applied
}

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
	return res, nil
}

func (st Stores) check(f File) error {
	switch {
	case len(f.Sources) > 0 && st.Sources == nil,
		len(f.Portfolios) > 0 && st.Portfolios == nil,
		len(f.Alerts) > 0 && st.Alerts == nil,
		f.CostBasis != nil && st.CostBasis == nil:
		return fmt.Errorf("this Vault can't restore every section of the file")
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
