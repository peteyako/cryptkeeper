// Package costbasis parses exchange CSV exports (Coinbase transaction history, Kraken
// ledgers) into activity, and keeps the manual cost entries of data/costbasis.yaml, which
// predate the tax lots (internal/tax) and are carried by backups.
package costbasis

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/yamlfile"
)

const (
	manualVersion = 1
	manualBackups = 20
)

// Date is a calendar date in UTC. In YAML it is a plain "2006-01-02" scalar (a full
// RFC 3339 timestamp is accepted too and truncated to UTC).
type Date time.Time

// DateOf converts t to a Date (UTC, time of day kept).
func DateOf(t time.Time) Date { return Date(t.UTC()) }

// Time returns the date as a time.Time.
func (d Date) Time() time.Time { return time.Time(d) }

// IsZero reports whether no date is set.
func (d Date) IsZero() bool { return time.Time(d).IsZero() }

func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return time.Time(d).Format("2006-01-02")
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Date) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: date must be a scalar like 2021-05-01", n.Line)
	}
	s := strings.TrimSpace(n.Value)
	if s == "" {
		*d = Date{}
		return nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		*d = Date(t)
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		*d = Date(t.UTC())
		return nil
	}
	return fmt.Errorf("line %d: invalid date %q (want YYYY-MM-DD)", n.Line, s)
}

// MarshalYAML implements yaml.Marshaler.
func (d Date) MarshalYAML() (any, error) {
	t := time.Time(d)
	layout := "2006-01-02"
	if t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0 || t.Nanosecond() != 0 {
		layout = time.RFC3339
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: t.UTC().Format(layout)}, nil
}

// ManualAsset states what the user invested in an asset: Amount units for CostUSD in total.
//
// Without Override it is an opening lot (added before every transaction). With Override it
// replaces the engine's computed state for the asset: the amount and basis of the result
// become exactly these values (realized history still comes from transactions).
type ManualAsset struct {
	Asset    string  `yaml:"asset" json:"asset"`
	Amount   float64 `yaml:"amount" json:"amount"`
	CostUSD  float64 `yaml:"costUsd" json:"costUsd"`
	Override bool    `yaml:"override,omitempty" json:"override,omitempty"`
	Note     string  `yaml:"note,omitempty" json:"note,omitempty"`
}

// ManualLot is an extra acquisition on a date (a paper wallet, a gift, an OTC buy). A lot
// without a date is processed before every dated transaction.
type ManualLot struct {
	Asset   string  `yaml:"asset" json:"asset"`
	Amount  float64 `yaml:"amount" json:"amount"`
	CostUSD float64 `yaml:"costUsd" json:"costUsd"`
	Date    Date    `yaml:"date,omitempty" json:"date,omitempty"`
	Note    string  `yaml:"note,omitempty" json:"note,omitempty"`
}

// Manual is the content of data/costbasis.yaml.
type Manual struct {
	Version int           `yaml:"version" json:"version"`
	Assets  []ManualAsset `yaml:"assets,omitempty" json:"assets,omitempty"`
	Lots    []ManualLot   `yaml:"lots,omitempty" json:"lots,omitempty"`
}

// ParseManual decodes and validates a costbasis.yaml. Decoding is strict (unknown fields
// are errors with line numbers). Asset keys are upper-cased, except that the mint after an
// "spl:" prefix keeps its case (base58 is case-sensitive). Empty input is an empty Manual.
func ParseManual(raw []byte) (Manual, error) {
	var m Manual
	if err := yamlfile.Decode(raw, &m); err != nil {
		return Manual{}, fmt.Errorf("costbasis.yaml: %w", err)
	}
	return m.normalized()
}

// Encode validates m and renders it as YAML.
func (m Manual) Encode() ([]byte, error) {
	n, err := m.normalized()
	if err != nil {
		return nil, err
	}
	return yamlfile.Encode(n)
}

// normalized returns a validated deep copy with canonical asset keys and Version set.
func (m Manual) normalized() (Manual, error) {
	switch m.Version {
	case 0, manualVersion:
	default:
		return Manual{}, fmt.Errorf("costbasis.yaml: unsupported version %d (want %d)", m.Version, manualVersion)
	}
	out := Manual{Version: manualVersion}
	seen := map[string]bool{}
	for i, a := range m.Assets {
		key, err := normalizeAssetKey(a.Asset)
		if err != nil {
			return Manual{}, fmt.Errorf("costbasis.yaml: assets[%d]: %w", i, err)
		}
		if err := checkAmounts(a.Amount, a.CostUSD); err != nil {
			return Manual{}, fmt.Errorf("costbasis.yaml: assets[%d] (%s): %w", i, key, err)
		}
		if seen[key] {
			return Manual{}, fmt.Errorf("costbasis.yaml: assets[%d]: duplicate entry for %s", i, key)
		}
		seen[key] = true
		a.Asset = key
		out.Assets = append(out.Assets, a)
	}
	for i, l := range m.Lots {
		key, err := normalizeAssetKey(l.Asset)
		if err != nil {
			return Manual{}, fmt.Errorf("costbasis.yaml: lots[%d]: %w", i, err)
		}
		if err := checkAmounts(l.Amount, l.CostUSD); err != nil {
			return Manual{}, fmt.Errorf("costbasis.yaml: lots[%d] (%s): %w", i, key, err)
		}
		l.Asset = key
		out.Lots = append(out.Lots, l)
	}
	return out, nil
}

func checkAmounts(amount, cost float64) error {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return errors.New("amount must be a positive number")
	}
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return errors.New("costUsd must be a number >= 0")
	}
	return nil
}

// normalizeAssetKey upper-cases an asset key, keeping the mint of "spl:<mint>" as written.
func normalizeAssetKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("asset is required")
	}
	if len(s) >= 4 && strings.EqualFold(s[:4], "spl:") {
		mint := strings.TrimSpace(s[4:])
		if mint == "" {
			return "", errors.New("spl: asset needs a mint")
		}
		return core.SPLKey(mint), nil
	}
	return strings.ToUpper(s), nil
}

func (m Manual) clone() Manual {
	m.Assets = append([]ManualAsset(nil), m.Assets...)
	m.Lots = append([]ManualLot(nil), m.Lots...)
	return m
}

// ManualStore keeps data/costbasis.yaml in memory and on disk. Hand edits are picked up on
// the next Get (by file mtime/size); a file that no longer parses keeps the last good
// content and is reported by Err. Writes are atomic with timestamped backups.
type ManualStore struct {
	path, backupDir string

	mu      sync.Mutex
	m       Manual
	modTime time.Time
	size    int64
	loadErr error
}

// NewManualStore loads path. A missing file is an empty Manual (it is created on the first
// write); an invalid file is an error.
func NewManualStore(path, backupDir string) (*ManualStore, error) {
	s := &ManualStore{path: path, backupDir: backupDir, m: Manual{Version: manualVersion}}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s, nil
}

// reload re-reads the file when it changed since the last read. Caller holds s.mu.
func (s *ManualStore) reload() {
	info, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.modTime, s.size, s.loadErr = time.Time{}, 0, nil
			return
		}
		s.loadErr = err
		return
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return
	}
	s.modTime, s.size = info.ModTime(), info.Size()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		s.loadErr = err
		return
	}
	m, err := ParseManual(raw)
	if err != nil {
		s.loadErr = err
		return
	}
	s.m, s.loadErr = m, nil
}

// Get returns a copy of the current content.
func (s *ManualStore) Get() Manual {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	return s.m.clone()
}

// Err reports why the file on disk could not be loaded (the last good content is still
// served by Get), or nil.
func (s *ManualStore) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	return s.loadErr
}

// Set validates and stores m, writing the file atomically (the previous content goes to the
// backup directory first).
func (s *ManualStore) Set(m Manual) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set(m)
}

func (s *ManualStore) set(m Manual) error {
	n, err := m.normalized()
	if err != nil {
		return err
	}
	data, err := yamlfile.Encode(n)
	if err != nil {
		return err
	}
	if err := yamlfile.WriteAtomic(s.path, data, s.backupDir, manualBackups); err != nil {
		return err
	}
	s.m, s.loadErr = n, nil
	if info, err := os.Stat(s.path); err == nil {
		s.modTime, s.size = info.ModTime(), info.Size()
	}
	return nil
}

// SetAsset sets the user's cost for asset ("I hold amount, I paid costUSD in total"),
// replacing any existing assets entry for it. The entry is stored with override: true: it
// is the user's statement of the truth for that asset, replacing what the engine derived.
func (s *ManualStore) SetAsset(asset string, amount, costUSD float64, note string) error {
	key, err := normalizeAssetKey(asset)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	m := s.m.clone()
	entry := ManualAsset{Asset: key, Amount: amount, CostUSD: costUSD, Override: true, Note: note}
	replaced := false
	for i := range m.Assets {
		if m.Assets[i].Asset == key {
			m.Assets[i], replaced = entry, true
			break
		}
	}
	if !replaced {
		m.Assets = append(m.Assets, entry)
	}
	return s.set(m)
}

// DeleteAsset removes the assets entry for asset (lots are kept). Deleting an absent entry
// is not an error and does not write.
func (s *ManualStore) DeleteAsset(asset string) error {
	key, err := normalizeAssetKey(asset)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	m := s.m.clone()
	kept := m.Assets[:0:0]
	for _, a := range m.Assets {
		if a.Asset != key {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(m.Assets) {
		return nil
	}
	m.Assets = kept
	return s.set(m)
}
