package tax

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newSettingsStore(t *testing.T) (*SettingsStore, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "tax.yaml")
	s, err := NewSettingsStore(p, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestSettingsRoundTrip(t *testing.T) {
	s, p := newSettingsStore(t)
	want := Settings{
		Version:    1,
		WalletMap:  map[string]string{"Ledger": "cold", "Old": "untracked"},
		AssetMap:   map[string]string{"SOL (Solana)": "SOL", "SPAM (Spam)": ""},
		ManualLots: []Lot{{Wallet: "cold", Asset: "BTC", Amount: 1, CostUSD: 100, Acquired: at(2020, 1, 2, 3)}},
		Tags:       map[string]TagEntry{"a": {Tag: TagGift, Note: "to mum"}},
		Prices:     map[string]float64{"k": 1.5},
		WalletBox:  map[string]string{"cold": "H"},
	}
	if err := s.Set(want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSettingsStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if _, err := ParseSettings([]byte("version: 1\nbogus: 1\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("unknown field: err = %v, want line number", err)
	}
}

func TestSettingsRejectsBad(t *testing.T) {
	for name, y := range map[string]string{
		"tag":        "tags:\n  x: {tag: bogus}\n",
		"price":      "prices:\n  x: -1\n",
		"nan price":  "prices:\n  x: .nan\n",
		"lot amount": "manualLots:\n  - {wallet: w, asset: A, amount: 0, costUsd: 1}\n",
		"lot cost":   "manualLots:\n  - {wallet: w, asset: A, amount: 1, costUsd: -1}\n",
		"lot wallet": "manualLots:\n  - {wallet: '', asset: A, amount: 1, costUsd: 1}\n",
		"lot asset":  "manualLots:\n  - {wallet: w, asset: '', amount: 1, costUsd: 1}\n",
		"version":    "version: 7\n",
	} {
		if _, err := ParseSettings([]byte(y)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := ParseSettings(nil); err != nil {
		t.Errorf("empty input: %v", err)
	}
}

func TestSettingsStoreBadFileKeepsLastGood(t *testing.T) {
	s, p := newSettingsStore(t)
	good := Settings{Version: 1, Prices: map[string]float64{"k": 2}}
	if err := s.Set(good); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("prices: [garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.Get(); !reflect.DeepEqual(got, good) {
		t.Fatalf("Get = %+v, want last good", got)
	}
	if s.Err() == nil {
		t.Fatal("Err = nil, want parse error")
	}
}

func TestSettingsUpdate(t *testing.T) {
	s, p := newSettingsStore(t)
	if err := s.Update(func(v *Settings) {
		v.Tags = map[string]TagEntry{"a": {Tag: TagSpam}}
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSettingsStore(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get().Tags["a"].Tag; got != TagSpam {
		t.Fatalf("persisted tag = %q", got)
	}
	// Mutating the copy must not touch the store.
	g := s.Get()
	g.Tags["a"] = TagEntry{Tag: TagLost}
	if s.Get().Tags["a"].Tag != TagSpam {
		t.Fatal("Get must return a deep copy")
	}
	// An invalid update is rejected and not stored.
	if err := s.Update(func(v *Settings) { v.Prices = map[string]float64{"x": -1} }); err == nil {
		t.Fatal("want validation error")
	}
}
