package brokerage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"crypto-portfolio/internal/sources/manual"
)

func TestParseLines(t *testing.T) {
	hs, err := ParseLines("vti 40\n\n# retirement\nBND 1,200.5 bond fund\nbrk-b 3")
	if err != nil {
		t.Fatal(err)
	}
	want := []manual.Holding{{Asset: "VTI", Amount: 40}, {Asset: "BND", Amount: 1200.5, Note: "bond fund"}, {Asset: "BRK-B", Amount: 3}}
	if len(hs) != len(want) {
		t.Fatalf("%+v", hs)
	}
	for i := range want {
		if hs[i] != want[i] {
			t.Errorf("line %d: %+v, want %+v", i, hs[i], want[i])
		}
	}
	back, err := ParseLines(manual.FormatLines(hs))
	if err != nil || len(back) != 3 || back[1] != hs[1] {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	for _, in := range []string{"", "VTI", "VTI abc", "VTI -1", "spl:JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN 3", "a/b 1"} {
		if _, err := ParseLines(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

func TestFetchNamesAndWarnings(t *testing.T) {
	lookup := func(_ context.Context, tickers []string) (map[string]Security, error) {
		return map[string]Security{
			"VTI":   {Name: "Vanguard Total Stock Market ETF", Currency: "USD"},
			"VOD.L": {Name: "Vodafone Group Plc", Currency: "GBp"},
		}, nil
	}
	hs := []manual.Holding{{Asset: "VTI", Amount: 40, Note: "taxable"}, {Asset: "VTIX", Amount: 1}, {Asset: "VOD.L", Amount: 10}, {Asset: "VTIX", Amount: 2}}
	res, err := New("fidelity", hs, lookup).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := res.Holdings[0]
	if h.AssetKey != "stock:VTI" || h.Symbol != "VTI" || h.Name != "Vanguard Total Stock Market ETF" || h.Note != "taxable" || h.WalletID != "fidelity" {
		t.Errorf("VTI holding %+v", h)
	}
	if res.Holdings[1].Name != "VTIX" {
		t.Errorf("an unknown ticker is named by itself, got %q", res.Holdings[1].Name)
	}
	got := strings.Join(res.Warnings, " | ")
	if strings.Count(got, "VTIX") != 1 || !strings.Contains(got, "doesn't know VTIX") || !strings.Contains(got, "VOD.L trades in GBp") {
		t.Errorf("warnings %q", got)
	}
}

// TestFetchLookupDown: when the lookup fails the holdings still list, named by ticker, with
// one warning and no error (the account isn't broken, Yahoo is unreachable).
func TestFetchLookupDown(t *testing.T) {
	lookup := func(context.Context, []string) (map[string]Security, error) { return nil, errors.New("down") }
	res, err := New("roth", []manual.Holding{{Asset: "VTSAX", Amount: 210}}, lookup).Fetch(context.Background())
	if err != nil || len(res.Holdings) != 1 || res.Holdings[0].Name != "VTSAX" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Couldn't reach Yahoo Finance") {
		t.Errorf("warnings %q", res.Warnings)
	}
}
