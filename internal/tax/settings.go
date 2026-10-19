package tax

import (
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"cryptkeeper/internal/yamlfile"
)

const (
	settingsVersion = 1
	settingsBackups = 20
)

// TagEntry is the user's tax tag for one transaction.
type TagEntry struct {
	Tag  Tag    `yaml:"tag"`
	Note string `yaml:"note,omitempty"`
}

// Settings is the content of data/tax.yaml: the user's choices that feed the engine.
type Settings struct {
	Version    int                 `yaml:"version"`
	WalletMap  map[string]string   `yaml:"walletMap,omitempty"`  // Koinly wallet name → source ID | Untracked
	AssetMap   map[string]string   `yaml:"assetMap,omitempty"`   // Koinly asset "SYM (Name)" → asset key | "" (skip)
	ManualLots []Lot               `yaml:"manualLots,omitempty"` //
	Tags       map[string]TagEntry `yaml:"tags,omitempty"`       // by Tx.ID
	Prices     map[string]float64  `yaml:"prices,omitempty"`     // PriceKey / FeePriceKey → USD
	WalletBox  map[string]string   `yaml:"walletBox,omitempty"`  // source ID or Koinly wallet name → short-term 8949 box G, H or I
	WriteOffs  []WriteOff          `yaml:"writeOffs,omitempty"`  // parked units written off
	// Scope is the portfolio whose wallets the open year's tax covers; "" covers every wallet.
	Scope string `yaml:"scope,omitempty"`
}

// ParseSettings decodes and validates a tax.yaml. Decoding is strict (unknown fields are
// errors with line numbers). Empty input is an empty Settings.
func ParseSettings(raw []byte) (Settings, error) {
	var s Settings
	if err := yamlfile.Decode(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("tax.yaml: %w", err)
	}
	return s.normalized()
}

// normalized returns a validated deep copy with Version set and the walletBox letters in
// upper case.
func (s Settings) normalized() (Settings, error) {
	switch s.Version {
	case 0, settingsVersion:
	default:
		return Settings{}, fmt.Errorf("tax.yaml: unsupported version %d (want %d)", s.Version, settingsVersion)
	}
	for id, e := range s.Tags {
		if !knownTag[e.Tag] {
			return Settings{}, fmt.Errorf("tax.yaml: tags[%s]: unknown tag %q", id, e.Tag)
		}
	}
	for k, v := range s.Prices {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return Settings{}, fmt.Errorf("tax.yaml: prices[%s]: price must be a number >= 0", k)
		}
	}
	for i, l := range s.ManualLots {
		switch {
		case l.Wallet == "":
			return Settings{}, fmt.Errorf("tax.yaml: manualLots[%d]: wallet is required", i)
		case l.Asset == "":
			return Settings{}, fmt.Errorf("tax.yaml: manualLots[%d]: asset is required", i)
		case math.IsNaN(l.Amount) || math.IsInf(l.Amount, 0) || l.Amount <= 0:
			return Settings{}, fmt.Errorf("tax.yaml: manualLots[%d] (%s): amount must be a positive number", i, l.Asset)
		case math.IsNaN(l.CostUSD) || math.IsInf(l.CostUSD, 0) || l.CostUSD < 0:
			return Settings{}, fmt.Errorf("tax.yaml: manualLots[%d] (%s): costUsd must be a number >= 0", i, l.Asset)
		}
	}
	for i, w := range s.WriteOffs {
		switch {
		case w.Wallet == "":
			return Settings{}, fmt.Errorf("tax.yaml: writeOffs[%d]: wallet is required", i)
		case w.Asset == "":
			return Settings{}, fmt.Errorf("tax.yaml: writeOffs[%d]: asset is required", i)
		case math.IsNaN(w.Amount) || math.IsInf(w.Amount, 0) || w.Amount <= 0:
			return Settings{}, fmt.Errorf("tax.yaml: writeOffs[%d] (%s): amount must be a positive number", i, w.Asset)
		case math.IsNaN(w.ProceedsUSD) || math.IsInf(w.ProceedsUSD, 0) || w.ProceedsUSD < 0:
			return Settings{}, fmt.Errorf("tax.yaml: writeOffs[%d] (%s): proceedsUsd must be a number >= 0", i, w.Asset)
		case w.Date.IsZero():
			return Settings{}, fmt.Errorf("tax.yaml: writeOffs[%d] (%s): date is required", i, w.Asset)
		}
	}
	out := s.clone()
	for w, box := range out.WalletBox {
		box = strings.ToUpper(strings.TrimSpace(box))
		if box != "G" && box != "H" && box != "I" {
			return Settings{}, fmt.Errorf("tax.yaml: walletBox[%s]: %q is not a Form 8949 box; use G, H or I (the short-term letter; long-term rows go to J, K or L)", w, out.WalletBox[w])
		}
		out.WalletBox[w] = box
	}
	out.Version = settingsVersion
	return out, nil
}

// Normalized returns a validated deep copy with Version set.
func (s Settings) Normalized() (Settings, error) { return s.normalized() }

// clone returns a deep copy; empty maps and slices become nil.
func (s Settings) clone() Settings {
	s.WalletMap = cloneMap(s.WalletMap)
	s.AssetMap = cloneMap(s.AssetMap)
	s.Tags = cloneMap(s.Tags)
	s.Prices = cloneMap(s.Prices)
	s.WalletBox = cloneMap(s.WalletBox)
	s.ManualLots = slices.Clone(s.ManualLots)
	if len(s.ManualLots) == 0 {
		s.ManualLots = nil
	}
	s.WriteOffs = slices.Clone(s.WriteOffs)
	if len(s.WriteOffs) == 0 {
		s.WriteOffs = nil
	}
	return s
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(m)
}

// SettingsStore keeps data/tax.yaml in memory and on disk. Hand edits are picked up on the
// next Get (by file mtime/size); a file that no longer parses keeps the last good content
// and is reported by Err. Writes are atomic with timestamped backups.
type SettingsStore struct {
	path, backupDir string

	mu      sync.Mutex
	s       Settings
	modTime time.Time
	size    int64
	loadErr error
}

// NewSettingsStore loads path. A missing file is an empty Settings (it is created on the
// first write); an invalid file is an error.
func NewSettingsStore(path, backupDir string) (*SettingsStore, error) {
	st := &SettingsStore{path: path, backupDir: backupDir, s: Settings{Version: settingsVersion}}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reload()
	if st.loadErr != nil {
		return nil, st.loadErr
	}
	return st, nil
}

// reload re-reads the file when it changed since the last read. Caller holds s.mu.
func (st *SettingsStore) reload() {
	info, err := os.Stat(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			st.modTime, st.size, st.loadErr = time.Time{}, 0, nil
			return
		}
		st.loadErr = err
		return
	}
	if info.ModTime().Equal(st.modTime) && info.Size() == st.size {
		return
	}
	st.modTime, st.size = info.ModTime(), info.Size()
	raw, err := os.ReadFile(st.path)
	if err != nil {
		st.loadErr = err
		return
	}
	v, err := ParseSettings(raw)
	if err != nil {
		st.loadErr = err
		return
	}
	st.s, st.loadErr = v, nil
}

// Get returns a deep copy of the current content.
func (st *SettingsStore) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reload()
	return st.s.clone()
}

// Err reports why the file on disk could not be loaded (the last good content is still
// served by Get), or nil.
func (st *SettingsStore) Err() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reload()
	return st.loadErr
}

// Set validates and stores v, writing the file atomically (the previous content goes to
// the backup directory first).
func (st *SettingsStore) Set(v Settings) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.set(v)
}

// Update applies fn to a copy of the current content and stores the result, all under the
// store's lock. It refuses to overwrite a file that currently fails to parse.
func (st *SettingsStore) Update(fn func(*Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reload()
	if st.loadErr != nil {
		return fmt.Errorf("tax.yaml on disk is invalid, fix it first: %w", st.loadErr)
	}
	v := st.s.clone()
	fn(&v)
	return st.set(v)
}

func (st *SettingsStore) set(v Settings) error {
	n, err := v.normalized()
	if err != nil {
		return err
	}
	data, err := yamlfile.Encode(n)
	if err != nil {
		return err
	}
	if err := yamlfile.WriteAtomic(st.path, data, st.backupDir, settingsBackups); err != nil {
		return err
	}
	st.s, st.loadErr = n, nil
	if info, err := os.Stat(st.path); err == nil {
		st.modTime, st.size = info.ModTime(), info.Size()
	}
	return nil
}

// Exists reports whether the tax.yaml file is on disk.
func (st *SettingsStore) Exists() bool {
	_, err := os.Stat(st.path)
	return err == nil
}
