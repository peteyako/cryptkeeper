// Package alerts evaluates user-defined alert rules against portfolio snapshots and new
// activity, and delivers notifications (ntfy). The rules live in data/alerts.yaml (a
// human-editable YAML file, see RuleStore); alert state (baselines, armed flags, cooldowns)
// and the log of fired alerts live in SQLite.
package alerts

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"crypto-portfolio/internal/yamlfile"
)

// Rule types.
const (
	TypeBalanceDrop   = "balance_drop"
	TypePrice         = "price"
	TypePortfolioMove = "portfolio_move"
	TypeSourceDown    = "source_down"
	TypeLargeActivity = "large_activity"
)

const (
	// Version is the only file format version this build understands.
	Version = 1
	// DefaultCooldown applies to rules that do not set `cooldown`.
	DefaultCooldown = 6 * time.Hour
	// DefaultWindow is the portfolio_move window when `window` is omitted.
	DefaultWindow = "24h"
	// AllPortfolios is the portfolio_move target meaning every holding.
	AllPortfolios = "all"

	maxIDLen = 40
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Rule is one alert rule. Which fields apply depends on Type; a field that belongs to
// another type is a validation error, so a typo cannot silently do nothing.
//
//	balance_drop    Wallets (omitted = every non-exchange wallet), MinPct (0 = any decrease)
//	price           Asset plus exactly one of Above / Below (USD)
//	portfolio_move  Portfolio (id or "all"), Pct, Window
//	source_down     Minutes
//	large_activity  MinUsd (0 = any send)
//
// Every rule may set Name (used as the notification title), Enabled (nil = enabled) and
// Cooldown (a Go duration, default 6h; balance_drop and source_down ignore it).
type Rule struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name,omitempty"`
	Type    string `yaml:"type"`
	Enabled *bool  `yaml:"enabled,omitempty"`

	// balance_drop
	Wallets []string `yaml:"wallets,omitempty,flow"`
	MinPct  float64  `yaml:"minPct,omitempty"`
	// price
	Asset string  `yaml:"asset,omitempty"`
	Above float64 `yaml:"above,omitempty"`
	Below float64 `yaml:"below,omitempty"`
	// portfolio_move
	Portfolio string  `yaml:"portfolio,omitempty"`
	Pct       float64 `yaml:"pct,omitempty"`
	Window    string  `yaml:"window,omitempty"`
	// source_down
	Minutes int `yaml:"minutes,omitempty"`
	// large_activity
	MinUsd float64 `yaml:"minUsd,omitempty"`

	Cooldown string `yaml:"cooldown,omitempty"`
}

// IsEnabled reports whether the rule is active (the default).
func (r Rule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// CooldownDuration is the rule's cooldown: DefaultCooldown when unset, 0 for "0s".
func (r Rule) CooldownDuration() time.Duration {
	if r.Cooldown == "" {
		return DefaultCooldown
	}
	d, err := time.ParseDuration(r.Cooldown)
	if err != nil || d < 0 {
		return DefaultCooldown
	}
	return d
}

// WindowDuration is the portfolio_move window (DefaultWindow when unset).
func (r Rule) WindowDuration() time.Duration {
	w := r.Window
	if w == "" {
		w = DefaultWindow
	}
	d, err := time.ParseDuration(w)
	if err != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultWindow)
	}
	return d
}

func (r Rule) clone() Rule {
	r.Wallets = slices.Clone(r.Wallets)
	if r.Enabled != nil {
		v := *r.Enabled
		r.Enabled = &v
	}
	return r
}

// normalize trims and canonicalizes fields and fills defaults. It never fails; validate
// reports what is wrong afterwards.
func (r *Rule) normalize() {
	r.ID = strings.TrimSpace(r.ID)
	r.Name = strings.TrimSpace(r.Name)
	r.Type = strings.ToLower(strings.TrimSpace(r.Type))
	r.Cooldown = strings.TrimSpace(r.Cooldown)
	if r.Enabled != nil && *r.Enabled {
		r.Enabled = nil // enabled is the default; keep files minimal
	}
	r.Asset = normalizeAsset(r.Asset)
	r.Portfolio = strings.ToLower(strings.TrimSpace(r.Portfolio))
	r.Window = strings.TrimSpace(r.Window)
	var wallets []string
	for _, w := range r.Wallets {
		if w = strings.TrimSpace(w); w != "" && !slices.Contains(wallets, w) {
			wallets = append(wallets, w)
		}
	}
	r.Wallets = wallets
	if r.Type == TypePortfolioMove {
		if r.Portfolio == "" {
			r.Portfolio = AllPortfolios
		}
		if r.Window == "" {
			r.Window = DefaultWindow
		}
	}
}

// normalizeAsset upper-cases symbols but keeps the case of "spl:<mint>" keys.
func normalizeAsset(a string) string {
	a = strings.TrimSpace(a)
	if len(a) >= 4 && strings.EqualFold(a[:4], "spl:") {
		return "spl:" + a[4:]
	}
	return strings.ToUpper(a)
}

func validType(t string) bool {
	switch t {
	case TypeBalanceDrop, TypePrice, TypePortfolioMove, TypeSourceDown, TypeLargeActivity:
		return true
	}
	return false
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// validate checks one normalized rule. The message does not name the rule; callers add that.
func (r Rule) validate() error {
	switch {
	case r.ID == "":
		return errors.New("id is required")
	case !idPattern.MatchString(r.ID):
		return fmt.Errorf("id %q must be 1-%d characters of lowercase letters, digits and dashes, starting with a letter or digit", r.ID, maxIDLen)
	case r.Type == "":
		return errors.New("type is required")
	case !validType(r.Type):
		return fmt.Errorf("unknown type %q (use balance_drop, price, portfolio_move, source_down or large_activity)", r.Type)
	}

	// Fields that belong to another type are mistakes, not extras.
	for _, f := range []struct {
		name string
		set  bool
		typ  string
	}{
		{"wallets", len(r.Wallets) > 0, TypeBalanceDrop},
		{"minPct", r.MinPct != 0, TypeBalanceDrop},
		{"asset", r.Asset != "", TypePrice},
		{"above", r.Above != 0, TypePrice},
		{"below", r.Below != 0, TypePrice},
		{"portfolio", r.Portfolio != "", TypePortfolioMove},
		{"pct", r.Pct != 0, TypePortfolioMove},
		{"window", r.Window != "", TypePortfolioMove},
		{"minutes", r.Minutes != 0, TypeSourceDown},
		{"minUsd", r.MinUsd != 0, TypeLargeActivity},
	} {
		if f.set && r.Type != f.typ {
			return fmt.Errorf("%q does not apply to %s rules (it belongs to %s)", f.name, r.Type, f.typ)
		}
	}

	switch r.Type {
	case TypeBalanceDrop:
		if !finite(r.MinPct) || r.MinPct < 0 || r.MinPct > 100 {
			return fmt.Errorf("minPct must be between 0 and 100 (0 = any decrease), got %v", r.MinPct)
		}
	case TypePrice:
		if r.Asset == "" {
			return errors.New("asset is required for price rules")
		}
		for _, t := range []struct {
			name string
			v    float64
		}{{"above", r.Above}, {"below", r.Below}} {
			if t.v != 0 && (!finite(t.v) || t.v < 0) {
				return fmt.Errorf("%s must be greater than 0, got %v", t.name, t.v)
			}
		}
		switch {
		case r.Above == 0 && r.Below == 0:
			return errors.New("price rules need above or below")
		case r.Above != 0 && r.Below != 0:
			return errors.New("set above or below, not both")
		}
	case TypePortfolioMove:
		if r.Portfolio != AllPortfolios && !idPattern.MatchString(r.Portfolio) {
			return fmt.Errorf("portfolio %q must be a portfolio id or %q", r.Portfolio, AllPortfolios)
		}
		if !finite(r.Pct) || r.Pct <= 0 {
			return errors.New("pct is required and must be greater than 0")
		}
		d, err := time.ParseDuration(r.Window)
		if err != nil {
			return fmt.Errorf("window %q is not a valid duration (use e.g. 24h, or 168h for a week): %v", r.Window, err)
		}
		if d <= 0 {
			return fmt.Errorf("window must be greater than 0, got %q", r.Window)
		}
	case TypeSourceDown:
		if r.Minutes < 1 {
			return errors.New("minutes is required and must be at least 1")
		}
	case TypeLargeActivity:
		if !finite(r.MinUsd) || r.MinUsd < 0 {
			return fmt.Errorf("minUsd must be 0 or more (0 = any send), got %v", r.MinUsd)
		}
	}

	if r.Cooldown != "" {
		d, err := time.ParseDuration(r.Cooldown)
		if err != nil {
			return fmt.Errorf("cooldown %q is not a valid duration (use e.g. 90m, 6h, 24h): %v", r.Cooldown, err)
		}
		if d < 0 {
			return fmt.Errorf("cooldown must not be negative, got %q", r.Cooldown)
		}
	}
	return nil
}

// File is the on-disk document.
type File struct {
	Version int    `yaml:"version"`
	Rules   []Rule `yaml:"rules"`
}

func (f File) clone() File {
	out := File{Version: f.Version}
	for _, r := range f.Rules {
		out.Rules = append(out.Rules, r.clone())
	}
	return out
}

func (f *File) normalize() {
	if f.Version == 0 {
		f.Version = Version
	}
	if len(f.Rules) == 0 {
		f.Rules = nil
	}
	for i := range f.Rules {
		f.Rules[i].normalize()
	}
}

func (f File) index(id string) int {
	return slices.IndexFunc(f.Rules, func(r Rule) bool { return r.ID == id })
}

// Validate checks the version, every rule and id uniqueness.
func (f File) Validate() error { return f.validate(nil) }

// validate is Validate with optional source line numbers (one per rule) for the messages.
func (f File) validate(lines []int) error {
	if f.Version != 0 && f.Version != Version {
		return fmt.Errorf("unsupported alerts version %d (this build reads version %d)", f.Version, Version)
	}
	at := func(i int) string {
		if i < len(lines) && lines[i] > 0 {
			return fmt.Sprintf("line %d: ", lines[i])
		}
		return ""
	}
	seen := map[string]bool{}
	for i, r := range f.Rules {
		where := fmt.Sprintf("rule #%d", i+1)
		if r.ID != "" {
			where = fmt.Sprintf("rule %q", r.ID)
		}
		if err := r.validate(); err != nil {
			return fmt.Errorf("%s%s: %w", at(i), where, err)
		}
		if seen[r.ID] {
			return fmt.Errorf("%sduplicate rule id %q", at(i), r.ID)
		}
		seen[r.ID] = true
	}
	return nil
}

// Parse decodes and validates an alerts document. Unknown fields are errors with line
// numbers, and so is every validation failure. Version 0 becomes 1, fields are normalized
// and portfolio_move defaults are filled in. Empty input is an empty file.
func Parse(raw []byte) (File, error) {
	var f File
	if err := yamlfile.Decode(raw, &f); err != nil {
		return File{}, cleanDecodeError(err)
	}
	lines := ruleLines(raw)
	for i, r := range f.Rules {
		if r.Wallets != nil && len(r.Wallets) == 0 {
			return File{}, fmt.Errorf("line %d: rule %q: wallets is empty (omit it to watch every non-exchange wallet)", lineAt(lines, i), r.ID)
		}
	}
	f.normalize()
	if err := f.validate(lines); err != nil {
		return File{}, err
	}
	return f, nil
}

func lineAt(lines []int, i int) int {
	if i < len(lines) {
		return lines[i]
	}
	return 0
}

// cleanDecodeError hides internal type names from yaml's "field x not found in type ..." errors.
func cleanDecodeError(err error) error {
	msg := strings.NewReplacer("alerts.Rule", "rule", "alerts.File", "file").Replace(err.Error())
	return errors.New(msg)
}

// ruleLines returns the source line of each entry in the top-level `rules` sequence, or nil
// if the document does not have that shape.
func ruleLines(raw []byte) []int {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "rules" || root.Content[i+1].Kind != yaml.SequenceNode {
			continue
		}
		var lines []int
		for _, item := range root.Content[i+1].Content {
			lines = append(lines, item.Line)
		}
		return lines
	}
	return nil
}

// Encode renders the file with a stable key order.
func (f File) Encode() ([]byte, error) {
	if f.Version == 0 {
		f.Version = Version
	}
	return yamlfile.Encode(f)
}

// Encode renders f as YAML (same as f.Encode()).
func Encode(f File) ([]byte, error) { return f.Encode() }

// slug lowercases s and turns every run of characters outside a-z0-9 into one dash.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
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
	out := b.String()
	if len(out) > maxIDLen {
		out = strings.TrimRight(out[:maxIDLen], "-")
	}
	return out
}

// autoID derives an id from the rule's type and, for price rules, its asset.
func autoID(r Rule) string {
	base := strings.ReplaceAll(r.Type, "_", "-")
	if r.Type == TypePrice && r.Asset != "" {
		base += "-" + r.Asset
	}
	if base = slug(base); base == "" {
		base = "rule"
	}
	return base
}

// uniqueID returns base, or base-2, base-3, … (shortening base to stay within the id
// limit) until taken reports false.
func uniqueID(base string, taken func(string) bool) string {
	id := base
	for n := 2; taken(id); n++ {
		suffix := fmt.Sprintf("-%d", n)
		stem := base
		if len(stem)+len(suffix) > maxIDLen {
			stem = strings.TrimRight(stem[:maxIDLen-len(suffix)], "-")
		}
		id = stem + suffix
	}
	return id
}

// NormalizeRules applies the same defaults and validation as Parse to rules that arrive
// embedded in another document (the alerts section of a portfolios export).
func NormalizeRules(rules []Rule) ([]Rule, error) {
	f := File{Version: Version}
	for _, r := range rules {
		f.Rules = append(f.Rules, r.clone())
	}
	f.normalize()
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f.Rules, nil
}
