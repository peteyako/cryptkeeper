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
