package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
