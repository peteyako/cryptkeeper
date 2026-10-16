package secrets

import (
	"bytes"
	"strings"
	"testing"
)

const plainExport = "version: 1\nsources:\n  - {id: cold, type: bitcoin, label: Cold, key: zpub6rSecretXpub}\n"

func TestSealFileRoundTrip(t *testing.T) {
	b, _ := NewBox(bytes.Repeat([]byte{7}, 32))
	sealed, err := b.SealFile([]byte(plainExport))
	if err != nil {
		t.Fatal(err)
	}
	s := string(sealed)
	if !strings.Contains(s, "cryptkeep: encrypted\n") || !strings.Contains(s, "data: \"enc:v1:") {
		t.Fatalf("not an encrypted envelope:\n%s", s)
	}
	for _, leak := range []string{"zpub6rSecretXpub", "cold", "sources:"} {
		if strings.Contains(s, leak) {
			t.Fatalf("envelope leaks %q:\n%s", leak, s)
		}
	}
	got, err := b.OpenFile(sealed)
	if err != nil || string(got) != plainExport {
		t.Fatalf("OpenFile = %q, %v", got, err)
	}
}

func TestOpenFilePassesPlainFilesThrough(t *testing.T) {
	b, _ := NewBox(bytes.Repeat([]byte{7}, 32))
	for _, raw := range []string{plainExport, "not: [valid yaml", ""} {
		got, err := b.OpenFile([]byte(raw))
		if err != nil || string(got) != raw {
			t.Fatalf("OpenFile(%q) = %q, %v; want it unchanged", raw, got, err)
		}
	}
}

func TestOpenFileRejectsWrongOrMissingKey(t *testing.T) {
	b, _ := NewBox(bytes.Repeat([]byte{7}, 32))
	other, _ := NewBox(bytes.Repeat([]byte{8}, 32))
	sealed, _ := b.SealFile([]byte(plainExport))
	if _, err := other.OpenFile(sealed); err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("wrong key: err = %v", err)
	}
	var none *Box
	if _, err := none.OpenFile(sealed); err == nil || !strings.Contains(err.Error(), "CRYPTKEEP_SECRET_KEY") {
		t.Fatalf("no key: err = %v", err)
	}
	if got, err := none.OpenFile([]byte(plainExport)); err != nil || string(got) != plainExport {
		t.Fatalf("a plain file needs no key: %q, %v", got, err)
	}
	if _, err := b.OpenFile([]byte("cryptkeep: encrypted\nversion: 2\ndata: \"enc:v1:AAAA\"\n")); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("future version: err = %v", err)
	}
}
