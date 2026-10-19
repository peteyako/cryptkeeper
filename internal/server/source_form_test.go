package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"crypto-portfolio/internal/sources"
)

// TestSourceFormSolanaVault: the Solana form carries the Jupiter limit-order vault both ways.
func TestSourceFormSolanaVault(t *testing.T) {
	form := url.Values{"type": {sources.TypeSolana}, "label": {"K1S"}, "kind": {"hardware"},
		"address": {"74u2Gq5JmZEeiSZZF1uYAUonois2wbwez8Q7urRrtJRN"}, "jupiterVault": {" 4N1FkXvBrAoyTwzM3R97RbDLpTZV9Jo17UYRnFnvNeDk "}}
	r := httptest.NewRequest("POST", "/sources", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	d, errs := (&Server{}).parseSourceForm(r, "")
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if d.JupiterVault != "4N1FkXvBrAoyTwzM3R97RbDLpTZV9Jo17UYRnFnvNeDk" {
		t.Fatalf("vault = %q", d.JupiterVault)
	}
	if v := formValues(d); v["jupiterVault"] != d.JupiterVault {
		t.Fatalf("formValues = %v", v)
	}

	form.Set("jupiterVault", "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	r = httptest.NewRequest("POST", "/sources", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, errs := (&Server{}).parseSourceForm(r, ""); !strings.Contains(errs[""], "seed phrase") {
		t.Fatalf("a seed phrase in the vault field must be refused: %v", errs)
	}
}

// TestSourceFormLedgerExport: Ledger's account JSON pasted as the key is saved as its xpub,
// with the export's script type unless the form chose one.
func TestSourceFormLedgerExport(t *testing.T) {
	s := &Server{}
	s.ParseLedgerExport = func(in string) (string, string, bool) {
		if strings.HasPrefix(in, "{") {
			return "xpub6LedgerAccountKey", "p2wpkh", true
		}
		return "", "", false
	}
	for _, tc := range []struct{ chosen, want string }{{"", "p2wpkh"}, {"p2tr", "p2tr"}} {
		form := url.Values{"type": {sources.TypeBitcoin}, "label": {"Ledger"}, "kind": {"hardware"},
			"key": {"{\n  \"xpub\": \"xpub6LedgerAccountKey\",\n  \"freshAddressPath\": \"84'/0'/0'/0/3\"\n}"}, "scriptType": {tc.chosen}}
		r := httptest.NewRequest("POST", "/sources", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		d, errs := s.parseSourceForm(r, "")
		if len(errs) != 0 {
			t.Fatalf("errs = %v", errs)
		}
		if d.Key != "xpub6LedgerAccountKey" || d.ScriptType != tc.want {
			t.Errorf("chosen %q: key %q script %q, want the xpub and %q", tc.chosen, d.Key, d.ScriptType, tc.want)
		}
	}
}
