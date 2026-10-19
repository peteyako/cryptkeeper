// Package portfolios owns data/portfolios.yaml: named views over the holdings, each
// choosing which wallets and which assets count toward it. It provides the YAML model with
// strict parsing and normalization, a file-backed Store with hand-edit reloads, backups,
// import/export and restore, and the filter that turns the full snapshot into one
// portfolio's snapshot.
package portfolios

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"cryptkeeper/internal/alerts"
	"cryptkeeper/internal/sources/manual"
	"cryptkeeper/internal/yamlfile"
)

const (
	// Version is the only file format version this build understands.
	Version = 1
	// ReservedID is the id the UI uses for the unfiltered "All holdings" view.
	ReservedID = "all"

	maxNameLen = 60
	maxIDLen   = 40
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ErrNotFound is returned (wrapped) when a portfolio id does not exist.
var ErrNotFound = errors.New("portfolio not found")

// Selector chooses a set of ids: everything, or an explicit list. The zero value is the
// empty explicit list, which matches nothing.
type Selector struct {
	All bool
	IDs []string
}

// AllSelector matches everything.
func AllSelector() Selector { return Selector{All: true} }

// Has reports whether x is selected.
func (s Selector) Has(x string) bool { return s.All || slices.Contains(s.IDs, x) }

// IsNone reports whether the selector matches nothing.
func (s Selector) IsNone() bool { return !s.All && len(s.IDs) == 0 }

// explicit reports whether x is named in an explicit list (a selector of "all" names no one).
func (s Selector) explicit(x string) bool { return !s.All && slices.Contains(s.IDs, x) }

// UnmarshalYAML accepts the scalar "all" (any case) or a sequence of ids. A null or any
// other node is an error, so a stray `wallets:` is not silently read as "everything".
func (s *Selector) UnmarshalYAML(node *yaml.Node) error {
	sel, err := parseSelector("selector", node)
	if err != nil {
		return err
	}
	*s = sel
	return nil
}

// MarshalYAML writes All as the scalar `all` and IDs as a flow sequence (`[]` for none).
func (s Selector) MarshalYAML() (any, error) {
	if s.All {
		return "all", nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, id := range s.IDs {
		// !!str forces quoting of values that would otherwise read back as bool, number or null.
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id})
	}
	return seq, nil
}

func parseSelector(field string, node *yaml.Node) (Selector, error) {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.ShortTag() == "!!str" && strings.EqualFold(strings.TrimSpace(node.Value), "all") {
			return AllSelector(), nil
		}
	case yaml.SequenceNode:
		var sel Selector
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode {
				return Selector{}, fmt.Errorf("line %d: %s entries must be plain ids", item.Line, field)
			}
			if item.ShortTag() == "!!null" && (item.Value == "" || item.Value == "~") {
				return Selector{}, fmt.Errorf("line %d: %s has an empty entry", item.Line, field)
			}
			sel.IDs = append(sel.IDs, item.Value)
		}
		return sel, nil
	}
	return Selector{}, fmt.Errorf("line %d: %s must be \"all\" or a list of ids (use [] for none)", node.Line, field)
}

// Portfolio is one named view: a holding belongs to it iff its wallet is in Wallets and its
// asset is in Assets. Watch lists extra assets shown with a live price only.
type Portfolio struct {
	ID      string   `yaml:"id"`
	Name    string   `yaml:"name"`
	Wallets Selector `yaml:"wallets"`
	Assets  Selector `yaml:"assets"`
	Watch   []string `yaml:"watch,omitempty,flow"`
}

// rawPortfolio is the on-disk shape. Selectors are kept as nodes so a missing key (= all)
// can be told apart from an explicit `[]` (= none) and from a null.
type rawPortfolio struct {
	ID      string    `yaml:"id"`
	Name    string    `yaml:"name"`
	Wallets yaml.Node `yaml:"wallets"`
	Assets  yaml.Node `yaml:"assets"`
	Watch   []string  `yaml:"watch"`
}

// UnmarshalYAML decodes a portfolio, defaulting an omitted wallets/assets key to "all". It
// uses the callback form so the decoder's strict (KnownFields) mode applies to the fields.
func (p *Portfolio) UnmarshalYAML(unmarshal func(any) error) error {
	var raw rawPortfolio
	if err := unmarshal(&raw); err != nil {
		return err // keep *yaml.TypeError intact so its line numbers survive
	}
	out := Portfolio{ID: raw.ID, Name: raw.Name, Watch: raw.Watch, Wallets: AllSelector(), Assets: AllSelector()}
	var err error
	if raw.Wallets.Kind != 0 {
		if out.Wallets, err = parseSelector("wallets", &raw.Wallets); err != nil {
			return err
		}
	}
	if raw.Assets.Kind != 0 {
		if out.Assets, err = parseSelector("assets", &raw.Assets); err != nil {
			return err
		}
	}
	*p = out
	return nil
}

func (p Portfolio) clone() Portfolio {
	p.Wallets.IDs = slices.Clone(p.Wallets.IDs)
	p.Assets.IDs = slices.Clone(p.Assets.IDs)
	p.Watch = slices.Clone(p.Watch)
	return p
}

// normalize trims, upper-cases asset symbols (keeping spl:<mint> case), drops blanks and
// duplicates preserving order, and canonicalizes empty lists to nil.
func (p *Portfolio) normalize() {
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.Wallets = normalizeSelector(p.Wallets, strings.TrimSpace)
	p.Assets = normalizeSelector(p.Assets, manual.NormalizeAsset)
	p.Watch = normalizeList(p.Watch, manual.NormalizeAsset)
}

func normalizeSelector(s Selector, norm func(string) string) Selector {
	if s.All {
		return Selector{All: true}
	}
	return Selector{IDs: normalizeList(s.IDs, norm)}
}

func normalizeList(in []string, norm func(string) string) []string {
	var out []string
	for _, v := range in {
		v = norm(v)
		if v == "" || slices.Contains(out, v) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// File is the on-disk document.
type File struct {
	Version    int         `yaml:"version"`
	Portfolios []Portfolio `yaml:"portfolios"`
	// Alerts is an optional section carried by "Export all" so one file backs up portfolios
	// and alert rules together. data/portfolios.yaml itself never stores it: the store only
	// keeps Portfolios (clone drops this field).
	Alerts []alerts.Rule `yaml:"alerts,omitempty"`
}

func (f File) clone() File {
	out := File{Version: f.Version}
	for _, p := range f.Portfolios {
		out.Portfolios = append(out.Portfolios, p.clone())
	}
	return out
}

func (f *File) normalize() {
	if f.Version == 0 {
		f.Version = Version
	}
	if len(f.Portfolios) == 0 {
		f.Portfolios = nil
	}
	for i := range f.Portfolios {
		f.Portfolios[i].normalize()
	}
}

func (f File) index(id string) int {
	return slices.IndexFunc(f.Portfolios, func(p Portfolio) bool { return p.ID == id })
}

// Parse decodes and validates a portfolios document. Unknown fields are errors with line
// numbers. Version 0 becomes 1, asset keys are normalized and duplicates removed. Empty
// input is an empty file.
func Parse(raw []byte) (File, error) {
	var f File
	if err := yamlfile.Decode(raw, &f); err != nil {
		return File{}, cleanDecodeError(err)
	}
	f.normalize()
	if err := f.Validate(); err != nil {
		return File{}, err
	}
	if len(f.Alerts) > 0 {
		rules, err := alerts.NormalizeRules(f.Alerts)
		if err != nil {
			return File{}, fmt.Errorf("alerts section: %w", err)
		}
		f.Alerts = rules
	}
	return f, nil
}

// cleanDecodeError hides the internal decoding type from yaml's "field x not found in type
// portfolios.rawPortfolio" messages.
func cleanDecodeError(err error) error {
	msg := strings.NewReplacer("portfolios.rawPortfolio", "portfolio", "portfolios.File", "file").Replace(err.Error())
	return errors.New(msg)
}

// Validate checks ids (format, uniqueness, the reserved "all") and names.
func (f File) Validate() error {
	if f.Version != 0 && f.Version != Version {
		return fmt.Errorf("unsupported portfolios version %d (this build reads version %d)", f.Version, Version)
	}
	seen := map[string]bool{}
	for i, p := range f.Portfolios {
		where := fmt.Sprintf("portfolio #%d", i+1)
		if p.ID != "" {
			where += fmt.Sprintf(" (%s)", p.ID)
		}
		switch {
		case p.ID == ReservedID:
			return fmt.Errorf("%s: id %q is reserved for the all-holdings view", where, ReservedID)
		case !idPattern.MatchString(p.ID):
			return fmt.Errorf("%s: id %q must be 1-%d characters of lowercase letters, digits and dashes, starting with a letter or digit", where, p.ID, maxIDLen)
		case seen[p.ID]:
			return fmt.Errorf("duplicate portfolio id %q", p.ID)
		}
		seen[p.ID] = true
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if utf8.RuneCountInString(p.Name) > maxNameLen {
			return fmt.Errorf("%s: name is longer than %d characters", where, maxNameLen)
		}
		for _, list := range []struct {
			field string
			ids   []string
		}{{"wallets", p.Wallets.IDs}, {"assets", p.Assets.IDs}, {"watch", p.Watch}} {
			if slices.ContainsFunc(list.ids, func(s string) bool { return strings.TrimSpace(s) == "" }) {
				return fmt.Errorf("%s: %s has an empty entry", where, list.field)
			}
		}
	}
	return nil
}

// Encode renders the file with a stable key order (version, portfolios; id, name, wallets,
// assets, watch).
func (f File) Encode() ([]byte, error) {
	if f.Version == 0 {
		f.Version = Version
	}
	return yamlfile.Encode(f)
}

// Slug turns a display name into a portfolio id: lowercase, runs of anything other than
// a-z/0-9 become one dash, trimmed, at most 40 characters. It is never empty and never the
// reserved "all".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	s := b.String()
	if len(s) > maxIDLen {
		s = strings.TrimRight(s[:maxIDLen], "-")
	}
	switch s {
	case "":
		return "portfolio"
	case ReservedID:
		return ReservedID + "-1"
	}
	return s
}

// uniqueID returns base, or base-2, base-3, … (shortening base to stay within the id limit)
// until taken reports false.
func uniqueID(base string, taken func(string) bool) string {
	id := base
	for n := 2; taken(id); n++ {
		suffix := "-" + strconv.Itoa(n)
		stem := base
		if len(stem)+len(suffix) > maxIDLen {
			stem = strings.TrimRight(stem[:maxIDLen-len(suffix)], "-")
		}
		id = stem + suffix
	}
	return id
}

// WithAlerts adds an alerts section to an exported portfolios document (no-op without rules).
func WithAlerts(raw []byte, rules []alerts.Rule) ([]byte, error) {
	if len(rules) == 0 {
		return raw, nil
	}
	f, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	f.Alerts = rules
	return f.Encode()
}
