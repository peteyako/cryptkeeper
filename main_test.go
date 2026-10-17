package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/tax"
)

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	replace := fs.Bool("replace", false, "")
	data := fs.String("data", "data", "")
	if err := parseInterspersed(fs, []string{"file.yaml", "-replace", "-data", "/x"}); err != nil {
		t.Fatal(err)
	}
	if !*replace || *data != "/x" || !slices.Equal(fs.Args(), []string{"file.yaml"}) {
		t.Fatalf("replace=%v data=%q args=%v", *replace, *data, fs.Args())
	}
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	fs2.Bool("replace", false, "")
	if err := parseInterspersed(fs2, []string{"-replace", "a.yaml", "b.yaml"}); err != nil || !slices.Equal(fs2.Args(), []string{"a.yaml", "b.yaml"}) {
		t.Fatalf("%v %v", err, fs2.Args())
	}
}

func TestRenameLegacyDB(t *testing.T) {
	read := func(dir, name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	write := func(dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	write(dir, "vault.db")
	write(dir, "vault.db-wal")
	if err := renameLegacyDB(dir); err != nil {
		t.Fatal(err)
	}
	if read(dir, "cryptkeep.db") != "vault.db" || read(dir, "cryptkeep.db-wal") != "vault.db-wal" {
		t.Fatal("legacy database and WAL not moved to cryptkeep.db")
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.db")); !os.IsNotExist(err) {
		t.Fatalf("vault.db still present: %v", err)
	}

	// Once cryptkeep.db exists, a vault.db is left alone and never clobbers it.
	write(dir, "vault.db")
	if err := renameLegacyDB(dir); err != nil {
		t.Fatal(err)
	}
	if read(dir, "cryptkeep.db") != "vault.db" || read(dir, "vault.db") != "vault.db" {
		t.Fatal("existing cryptkeep.db was replaced")
	}

	// A fresh data directory has nothing to move.
	if err := renameLegacyDB(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestExportCommandsEncryptWithKey(t *testing.T) {
	t.Setenv("CRYPTKEEP_SECRET_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	missing := filepath.Join(t.TempDir(), "missing")
	install := func(dir string) []string { return []string{"-data", dir, "-config", missing, "-env", missing} }
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "sources.yaml"), []byte("version: 1\nsources:\n  - {id: kids, type: manual, label: Kids, kind: manual, holdings: [{asset: SOL, amount: 3}]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{}
	for name, cmd := range map[string]func([]string) error{"backup": backupCmd, "sources": sourcesCmd, "portfolios": portfoliosCmd} {
		out := filepath.Join(t.TempDir(), name+".yaml")
		if err := cmd(append([]string{"export", "-o", out}, install(src)...)); err != nil {
			t.Fatalf("%s export: %v", name, err)
		}
		if f := read(out); !strings.Contains(f, "cryptkeep: encrypted") || strings.Contains(f, "Kids") {
			t.Fatalf("%s export is not encrypted:\n%s", name, f)
		}
		files[name] = out
	}

	// Both encrypted files restore into a new install that has the same key.
	for name, cmd := range map[string]func([]string) error{"backup": backupCmd, "sources": sourcesCmd} {
		dst := t.TempDir()
		if err := cmd(append([]string{"import", files[name]}, install(dst)...)); err != nil {
			t.Fatalf("%s import: %v", name, err)
		}
		if !strings.Contains(read(filepath.Join(dst, "sources.yaml")), "Kids") {
			t.Fatalf("%s import did not restore the source", name)
		}
	}
}

func TestSyncSince(t *testing.T) {
	dir := t.TempDir()
	years, err := tax.NewYearStore(filepath.Join(dir, "tax"), filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	// No closed year: just the backfill window.
	if got, want := syncSince(now, 90, years, time.UTC), now.AddDate(0, 0, -90); !got.Equal(want) {
		t.Errorf("no closed year: %v, want %v", got, want)
	}
	// 2025 closed: the open year starts 2026-01-01, earlier than 90 days back.
	if err := years.Save(tax.Year{Year: 2025, Source: "koinly"}); err != nil {
		t.Fatal(err)
	}
	if got, want := syncSince(now, 90, years, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("open year earlier: %v, want %v", got, want)
	}
	// A backfill window reaching further back wins.
	if got, want := syncSince(now, 800, years, time.UTC), now.AddDate(0, 0, -800); !got.Equal(want) {
		t.Errorf("backfill earlier: %v, want %v", got, want)
	}
}

func TestMigrateCostBasisLots(t *testing.T) {
	dir := t.TempDir()
	bk := filepath.Join(dir, "backups")
	cb, err := costbasis.NewManualStore(filepath.Join(dir, "costbasis.yaml"), bk)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb.Set(costbasis.Manual{
		Assets: []costbasis.ManualAsset{{Asset: "BTC", Amount: 1, CostUSD: 10}},
		Lots: []costbasis.ManualLot{
			{Asset: "SOL", Amount: 4, CostUSD: 80, Date: costbasis.DateOf(time.Date(2022, 3, 4, 0, 0, 0, 0, time.UTC)), Note: "gift"},
			{Asset: "ETH", Amount: 1, CostUSD: 5},
		},
	}); err != nil {
		t.Fatal(err)
	}
	taxPath := filepath.Join(dir, "tax.yaml")
	ts, err := tax.NewSettingsStore(taxPath, bk)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	migrateCostBasis(cb, ts, log)

	lots := ts.Get().ManualLots
	if len(lots) != 2 {
		t.Fatalf("lots = %+v", lots)
	}
	if l := lots[0]; l.Wallet != tax.Untracked || l.Asset != "SOL" || l.Amount != 4 || l.CostUSD != 80 || !l.Acquired.Equal(time.Date(2022, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("lot 0 = %+v", l)
	}
	if l := lots[1]; !l.Acquired.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("lot 1 = %+v", l)
	}
	if !strings.Contains(logs.String(), "moved 2 manual lots from costbasis.yaml to tax.yaml (wallet: untracked); edit tax.yaml to assign wallets") {
		t.Fatalf("log = %q", logs.String())
	}
	if !strings.Contains(logs.String(), "asset override") {
		t.Fatalf("no asset override warning: %q", logs.String())
	}

	// Second run: tax.yaml now exists, nothing changes.
	if err := ts.Update(func(s *tax.Settings) { s.ManualLots = s.ManualLots[:1] }); err != nil {
		t.Fatal(err)
	}
	migrateCostBasis(cb, ts, log)
	if n := len(ts.Get().ManualLots); n != 1 {
		t.Fatalf("second run changed lots: %d", n)
	}
}
