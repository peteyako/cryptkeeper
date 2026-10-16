package secrets

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Seal("-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "enc:v1:") || strings.Contains(s, "PRIVATE") {
		t.Fatalf("not sealed: %q", s)
	}
	got, err := b.Open(s)
	if err != nil || !strings.Contains(got, "PRIVATE KEY") {
		t.Fatalf("open: %q %v", got, err)
	}
	s2, _ := b.Seal("same")
	s3, _ := b.Seal("same")
	if s2 == s3 {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	a, _ := NewBox(bytes.Repeat([]byte{1}, 32))
	b, _ := NewBox(bytes.Repeat([]byte{2}, 32))
	s, _ := a.Seal("secret")
	if _, err := b.Open(s); err == nil {
		t.Fatal("expected error with wrong key")
	}
}

func TestOpenPlaintextPassthrough(t *testing.T) {
	b, _ := NewBox(bytes.Repeat([]byte{1}, 32))
	if got, err := b.Open("plain"); err != nil || got != "plain" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, _ := b.Seal(""); got != "" {
		t.Fatalf("empty should stay empty, got %q", got)
	}
}

func TestLoadKeyCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "secret.key")
	k1, err := LoadKey("", path)
	if err != nil || len(k1) != 32 {
		t.Fatalf("%v %d", err, len(k1))
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v %v", st.Mode(), err)
	}
	k2, _ := LoadKey("", path)
	if !bytes.Equal(k1, k2) {
		t.Fatal("key changed between loads")
	}
}

func TestMask(t *testing.T) {
	if got := Mask("kr_live_abcdef3f9a"); got != "••••3f9a" {
		t.Fatal(got)
	}
}
