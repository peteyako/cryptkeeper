package server

import (
	"context"
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

// TestSourceDescription: a wallet's description is saved from the form, refilled on edit,
// and shown on the wallet page, the Wallets list and Settings.
func TestSourceDescription(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"type": {"manual"}, "label": {"Safe"}, "holdings": {"BTC 0.25"},
		"description": {"  Paper wallet in the safe,\n  for the kids  "}})
	if resp, out := e.do("POST", "/sources", body, hdr); resp.StatusCode != 200 || !strings.Contains(out, "/w/safe") {
		t.Fatalf("save: %d %s", resp.StatusCode, out)
	}
	const want = "Paper wallet in the safe, for the kids"
	if d, _ := e.reg.Get("safe"); d.Description != want {
		t.Fatalf("stored description %q, want %q", d.Description, want)
	}
	e.ref.Force(context.Background())
	for _, path := range []string{"/sources/safe/edit", "/w/safe", "/wallets", "/settings"} {
		if _, page := e.do("GET", path, nil, nil); !strings.Contains(page, want) {
			t.Errorf("%s doesn't show the description", path)
		}
	}

	body, hdr = form(url.Values{"type": {"manual"}, "label": {"Safe"}, "holdings": {"BTC 0.25"},
		"description": {strings.Repeat("x", sources.MaxDescription+1)}})
	if _, out := e.do("POST", "/sources/safe", body, hdr); !strings.Contains(out, "280 characters") {
		t.Fatalf("an over-long description must be refused on the form: %.600s", out)
	}
	if d, _ := e.reg.Get("safe"); d.Description != want {
		t.Fatalf("refused edit changed the description to %q", d.Description)
	}
}
