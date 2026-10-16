package config

import (
	"path/filepath"
	"testing"
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
