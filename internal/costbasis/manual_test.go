package costbasis

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto-portfolio/internal/yamlfile"
)

func TestParseManualFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/costbasis.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManual(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 1 || len(m.Assets) != 2 || len(m.Lots) != 1 {
		t.Fatalf("manual = %+v", m)
	}
	a := m.Assets[0]
	if a.Asset != "BTC" || a.Amount != 0.5 || a.CostUSD != 15000 || !a.Override || a.Note != "pre-2020 buys" {
		t.Errorf("asset 0 = %+v", a)
	}
	// The mint is case-sensitive base58 and must be preserved; only the prefix is canonical.
	if got := m.Assets[1].Asset; got != "spl:EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" {
		t.Errorf("spl key = %q", got)
	}
	if m.Assets[1].Override {
		t.Error("override defaults to false")
	}
	lot := m.Lots[0]
	if lot.Asset != "ETH" || lot.Amount != 2 || lot.CostUSD != 3000 || lot.Note != "paper wallet" {
		t.Errorf("lot = %+v", lot)
	}
	if !lot.Date.Time().Equal(time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("lot date = %v", lot.Date.Time())
	}
}

func TestParseManualEmptyAndVersion(t *testing.T) {
	for _, raw := range []string{"", "\n", "version: 1\n"} {
		m, err := ParseManual([]byte(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if m.Version != 1 || len(m.Assets) != 0 || len(m.Lots) != 0 {
			t.Errorf("%q: %+v", raw, m)
		}
	}
	if _, err := ParseManual([]byte("version: 2\n")); err == nil {
		t.Error("version 2 accepted")
	}
}

func TestParseManualRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown field":     "version: 1\nassets:\n  - {asset: BTC, amount: 1, costUsd: 1, bogus: true}\n",
		"unknown top":       "version: 1\nextra: 1\n",
		"zero amount":       "assets:\n  - {asset: BTC, amount: 0, costUsd: 1}\n",
		"negative amount":   "assets:\n  - {asset: BTC, amount: -1, costUsd: 1}\n",
		"negative cost":     "assets:\n  - {asset: BTC, amount: 1, costUsd: -5}\n",
		"missing asset":     "assets:\n  - {amount: 1, costUsd: 1}\n",
		"empty spl mint":    "assets:\n  - {asset: 'spl:', amount: 1, costUsd: 1}\n",
		"duplicate asset":   "assets:\n  - {asset: BTC, amount: 1, costUsd: 1}\n  - {asset: btc, amount: 2, costUsd: 1}\n",
		"lot zero amount":   "lots:\n  - {asset: ETH, amount: 0, costUsd: 1, date: 2021-01-01}\n",
		"lot negative cost": "lots:\n  - {asset: ETH, amount: 1, costUsd: -1, date: 2021-01-01}\n",
		"lot bad date":      "lots:\n  - {asset: ETH, amount: 1, costUsd: 1, date: yesterday}\n",
		"lot unknown field": "lots:\n  - {asset: ETH, amount: 1, costUsd: 1, extra: 1}\n",
		"not yaml":          "assets: [",
		"nan amount":        "assets:\n  - {asset: BTC, amount: .nan, costUsd: 1}\n",
		"infinite cost":     "assets:\n  - {asset: BTC, amount: 1, costUsd: .inf}\n",
	}
	for name, raw := range cases {
		if _, err := ParseManual([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseManualAllowsZeroCostAndUndatedLots(t *testing.T) {
	m, err := ParseManual([]byte("assets:\n  - {asset: sol, amount: 3, costUsd: 0}\nlots:\n  - {asset: eth, amount: 1, costUsd: 0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Assets[0].Asset != "SOL" || m.Lots[0].Asset != "ETH" || !m.Lots[0].Date.IsZero() {
		t.Errorf("manual = %+v", m)
	}
	out, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "date:") {
		t.Errorf("undated lot should not render a date:\n%s", out)
	}
	if back, err := ParseManual(out); err != nil || !reflect.DeepEqual(back, m) {
		t.Errorf("round trip: %v\n%+v\n%+v", err, back, m)
	}
}

func TestManualEncodeRoundTrip(t *testing.T) {
	raw, _ := os.ReadFile("testdata/costbasis.yaml")
	m, err := ParseManual(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "date: 2021-05-01\n") {
		t.Errorf("date not rendered as a plain date:\n%s", out)
	}
	if strings.Contains(string(out), "override: false") || strings.Contains(string(out), "note: \"\"") {
		t.Errorf("zero values should be omitted:\n%s", out)
	}
	back, err := ParseManual(out)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(m, back) {
		t.Errorf("round trip differs:\n%+v\n%+v", m, back)
	}

	// Encode refuses invalid data instead of writing a file ParseManual would reject.
	bad := Manual{Assets: []ManualAsset{{Asset: "BTC", Amount: -1}}}
	if _, err := bad.Encode(); err == nil {
		t.Error("invalid manual encoded")
	}
}

type storeFixture struct{ dir, path, backups string }

func newStoreFixture(t *testing.T) storeFixture {
	t.Helper()
	d := t.TempDir()
	return storeFixture{d, filepath.Join(d, "costbasis.yaml"), filepath.Join(d, "backups")}
}

func TestManualStoreMissingFileIsEmpty(t *testing.T) {
	fx := newStoreFixture(t)
	s, err := NewManualStore(fx.path, fx.backups)
	if err != nil {
		t.Fatal(err)
	}
	m := s.Get()
	if len(m.Assets) != 0 || len(m.Lots) != 0 {
		t.Errorf("manual = %+v", m)
	}
	if _, err := os.Stat(fx.path); !os.IsNotExist(err) {
		t.Error("constructor must not create the file")
	}
}

func TestManualStoreSetPersistsAndBacksUp(t *testing.T) {
	fx := newStoreFixture(t)
	s, err := NewManualStore(fx.path, fx.backups)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Manual{Assets: []ManualAsset{{Asset: "btc", Amount: 1, CostUSD: 100, Override: true}}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Get().Assets; len(got) != 1 || got[0].Asset != "BTC" {
		t.Errorf("not normalized: %+v", got)
	}
	if _, err := os.Stat(fx.backups); !os.IsNotExist(err) {
		t.Error("first write has nothing to back up")
	}
	if err := s.Set(Manual{Assets: []ManualAsset{{Asset: "ETH", Amount: 2, CostUSD: 50}}}); err != nil {
		t.Fatal(err)
	}
	bs, err := yamlfile.ListBackups(fx.backups, "costbasis")
	if err != nil || len(bs) != 1 {
		t.Fatalf("backups = %v, %v", bs, err)
	}
	old, err := yamlfile.ReadBackup(fx.backups, "costbasis", bs[0].Name)
	if err != nil || !strings.Contains(string(old), "BTC") {
		t.Errorf("backup content %q, %v", old, err)
	}

	// A fresh store reads what was written.
	s2, err := NewManualStore(fx.path, fx.backups)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get().Assets; len(got) != 1 || got[0].Asset != "ETH" || got[0].Amount != 2 {
		t.Errorf("reloaded %+v", got)
	}
	info, _ := os.Stat(fx.path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

func TestManualStoreRejectsInvalidWithoutTouchingDisk(t *testing.T) {
	fx := newStoreFixture(t)
	s, _ := NewManualStore(fx.path, fx.backups)
	if err := s.Set(Manual{Assets: []ManualAsset{{Asset: "BTC", Amount: 0}}}); err == nil {
		t.Fatal("invalid manual stored")
	}
	if _, err := os.Stat(fx.path); !os.IsNotExist(err) {
		t.Error("file written for invalid input")
	}
	if len(s.Get().Assets) != 0 {
		t.Error("memory changed for invalid input")
	}
}

func TestManualStoreSetAndDeleteAsset(t *testing.T) {
	fx := newStoreFixture(t)
	s, _ := NewManualStore(fx.path, fx.backups)
	if err := s.Set(Manual{Lots: []ManualLot{{Asset: "ETH", Amount: 1, CostUSD: 10}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAsset("btc", 0.5, 15000, "cold storage"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAsset("ETH", 3, 6000, ""); err != nil {
		t.Fatal(err)
	}
	// Updating replaces the entry rather than duplicating it.
	if err := s.SetAsset("BTC", 0.75, 20000, "updated"); err != nil {
		t.Fatal(err)
	}
	m := s.Get()
	if len(m.Assets) != 2 || len(m.Lots) != 1 {
		t.Fatalf("manual = %+v", m)
	}
	var btc ManualAsset
	for _, a := range m.Assets {
		if a.Asset == "BTC" {
			btc = a
		}
	}
	if btc.Amount != 0.75 || btc.CostUSD != 20000 || btc.Note != "updated" || !btc.Override {
		t.Errorf("btc = %+v", btc)
	}
	if err := s.SetAsset("BTC", -1, 5, ""); err == nil {
		t.Error("negative amount accepted")
	}

	if err := s.DeleteAsset("btc"); err != nil {
		t.Fatal(err)
	}
	if got := s.Get(); len(got.Assets) != 1 || got.Assets[0].Asset != "ETH" || len(got.Lots) != 1 {
		t.Errorf("after delete %+v", got)
	}
	// Deleting something absent is not an error.
	if err := s.DeleteAsset("DOGE"); err != nil {
		t.Errorf("delete absent: %v", err)
	}
}

func TestManualStoreGetReturnsACopy(t *testing.T) {
	fx := newStoreFixture(t)
	s, _ := NewManualStore(fx.path, fx.backups)
	_ = s.SetAsset("BTC", 1, 1, "")
	m := s.Get()
	m.Assets[0].Amount = 999
	if s.Get().Assets[0].Amount != 1 {
		t.Error("Get exposes internal state")
	}
}

func TestManualStoreReloadsHandEdits(t *testing.T) {
	fx := newStoreFixture(t)
	s, _ := NewManualStore(fx.path, fx.backups)
	_ = s.SetAsset("BTC", 1, 1, "")

	edit := "version: 1\nassets:\n  - {asset: SOL, amount: 7, costUsd: 70, override: true}\n"
	if err := os.WriteFile(fx.path, []byte(edit), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(fx.path, future, future)
	if got := s.Get().Assets; len(got) != 1 || got[0].Asset != "SOL" {
		t.Fatalf("hand edit not seen: %+v", got)
	}
	if s.Err() != nil {
		t.Errorf("err = %v", s.Err())
	}

	// A broken edit keeps the last good state and reports the error.
	if err := os.WriteFile(fx.path, []byte("assets: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	future = future.Add(time.Hour)
	_ = os.Chtimes(fx.path, future, future)
	if got := s.Get().Assets; len(got) != 1 || got[0].Asset != "SOL" {
		t.Errorf("last good state lost: %+v", got)
	}
	if s.Err() == nil {
		t.Error("broken file not reported")
	}
	// Saving through the store repairs it.
	if err := s.SetAsset("ETH", 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	if s.Err() != nil {
		t.Errorf("err after repair = %v", s.Err())
	}
}

func TestNewManualStoreFailsOnInvalidFile(t *testing.T) {
	fx := newStoreFixture(t)
	_ = os.WriteFile(fx.path, []byte("assets:\n  - {asset: BTC, amount: -1, costUsd: 1}\n"), 0o600)
	_, err := NewManualStore(fx.path, fx.backups)
	if err == nil {
		t.Fatal("invalid file accepted")
	}
}

func TestManualStoreConcurrentUse(t *testing.T) {
	fx := newStoreFixture(t)
	s, err := NewManualStore(fx.path, fx.backups)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			asset := string(rune('A' + i))
			for j := 1; j <= 5; j++ {
				if err := s.SetAsset(asset, float64(j), float64(j), ""); err != nil {
					t.Error(err)
					return
				}
				_ = s.Get()
				_ = s.Err()
			}
		}(i)
	}
	wg.Wait()
	if got := len(s.Get().Assets); got != 8 {
		t.Errorf("assets = %d, want 8", got)
	}
	s2, err := NewManualStore(fx.path, fx.backups)
	if err != nil || len(s2.Get().Assets) != 8 {
		t.Errorf("reload: %v", err)
	}
}
