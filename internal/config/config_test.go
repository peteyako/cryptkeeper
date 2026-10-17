package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadSecretKeyEnv(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.yaml")
	for _, tc := range []struct{ current, legacy, want string }{
		{"new", "", "new"},
		{"", "old", "old"}, // VAULT_SECRET_KEY, the name before the CryptKeep rename
		{"new", "old", "new"},
	} {
		t.Setenv("CRYPTKEEP_SECRET_KEY", tc.current)
		t.Setenv("VAULT_SECRET_KEY", tc.legacy)
		cfg, err := Load(missing)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SecretKey != tc.want {
			t.Errorf("CRYPTKEEP_SECRET_KEY=%q VAULT_SECRET_KEY=%q: SecretKey = %q, want %q", tc.current, tc.legacy, cfg.SecretKey, tc.want)
		}
	}
}

func TestEncryptExportsWhenKeyIsSet(t *testing.T) {
	if (Config{}).EncryptExports() {
		t.Fatal("no CRYPTKEEP_SECRET_KEY: exports stay plain")
	}
	if !(Config{SecretKey: "a2V5"}).EncryptExports() {
		t.Fatal("CRYPTKEEP_SECRET_KEY set: exports are encrypted")
	}
}

func TestTaxLocationDefaultLocal(t *testing.T) {
	loc, err := (Config{}).TaxLocation()
	if err != nil || loc != time.Local {
		t.Fatalf("empty timezone = %v, %v; want time.Local", loc, err)
	}
	c := Config{Tax: Tax{Timezone: "America/New_York"}}
	loc, err = c.TaxLocation()
	if err != nil || loc.String() != "America/New_York" {
		t.Fatalf("NY = %v, %v", loc, err)
	}
	c.Tax.Timezone = "Nope/Zone"
	if _, err := c.TaxLocation(); err == nil {
		t.Fatal("bad zone: want error")
	}
}

func TestLoadTaxTimezone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("tax:\n  timezone: Europe/Berlin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil || cfg.Tax.Timezone != "Europe/Berlin" {
		t.Fatalf("Load = %+v, %v", cfg.Tax, err)
	}
}
