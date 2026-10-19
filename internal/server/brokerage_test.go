package server

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/sources/brokerage"
	"crypto-portfolio/internal/tax/report"
)

// fakeTickers names the tickers the server tests use; any other is unknown.
func fakeTickers(_ context.Context, tickers []string) (map[string]brokerage.Security, error) {
	known := map[string]brokerage.Security{
		"VTI": {Name: "Vanguard Total Stock Market ETF", Currency: "USD"},
		"BND": {Name: "Vanguard Total Bond Market ETF", Currency: "USD"},
	}
	out := map[string]brokerage.Security{}
	for _, t := range tickers {
		if s, ok := known[t]; ok {
			out[t] = s
		}
	}
	return out, nil
}

func addFidelity(t *testing.T, e *env) {
	t.Helper()
	body, hdr := form(url.Values{"type": {"brokerage"}, "label": {"Fidelity"}, "holdings": {"vti 40\nBND 120 bond fund"}})
	if resp, out := e.do("POST", "/sources", body, hdr); resp.StatusCode != 200 || !strings.Contains(out, "/w/fidelity") {
		t.Fatalf("save: %d %s", resp.StatusCode, out)
	}
	e.ref.Force(context.Background())
}

// TestBrokerageAccount: a brokerage account is saved from tickers and shares, counts in the
// total, and its page shows shares, names and the day's change instead of cost basis.
func TestBrokerageAccount(t *testing.T) {
	e := newEnv(t, "")
	addFidelity(t, e)
	d, _ := e.reg.Get("fidelity")
	if d.Kind != core.KindBrokerage || len(d.Holdings) != 2 || d.Holdings[0].Asset != "VTI" || d.Holdings[1].Note != "bond fund" {
		t.Fatalf("stored %+v", d)
	}
	snap, _ := e.ref.Latest()
	if snap.TotalUSD != 16000 { // 160 shares at the fake 100
		t.Errorf("total %v, want the account's 16000", snap.TotalUSD)
	}

	_, page := e.do("GET", "/w/fidelity", nil, nil)
	for _, want := range []string{"Vanguard Total Stock Market ETF", "40 VTI", "120 BND", "Day change", "Brokerage · stocks &amp; funds"} {
		if !strings.Contains(page, want) {
			t.Errorf("wallet page lacks %q", want)
		}
	}
	for _, not := range []string{"Import cost basis", "Needs a cost basis", "?tab=activity"} {
		if strings.Contains(page, not) {
			t.Errorf("wallet page shows %q", not)
		}
	}
	if _, edit := e.do("GET", "/sources/fidelity/edit", nil, nil); !strings.Contains(edit, "VTI 40") || !strings.Contains(edit, "BND 120 bond fund") {
		t.Error("the edit form doesn't refill the holdings")
	}
	if _, pick := e.do("GET", "/sources/new", nil, nil); !strings.Contains(pick, "/sources/new/brokerage") {
		t.Error("the picker doesn't offer a brokerage account")
	}
}

// TestBrokerageForm: a line that isn't a ticker is refused; Test connection names unknown
// tickers.
func TestBrokerageForm(t *testing.T) {
	e := newEnv(t, "")
	body, hdr := form(url.Values{"type": {"brokerage"}, "label": {"Roth"}, "holdings": {"VTI 40\nnot/a/ticker 3"}})
	if _, out := e.do("POST", "/sources", body, hdr); !strings.Contains(out, "a ticker") {
		t.Fatalf("a bad ticker must be refused: %.600s", out)
	}
	if _, ok := e.reg.Get("roth"); ok {
		t.Fatal("refused account was saved")
	}
	body, hdr = form(url.Values{"type": {"brokerage"}, "label": {"Roth"}, "holdings": {"VTI 40\nVTIX 2"}})
	_, out := e.do("POST", "/sources/test", body, hdr)
	if !strings.Contains(out, "Found all 2 tickers.") || !strings.Contains(out, "know VTIX") {
		t.Fatalf("test result: %.800s", out)
	}
}

// TestBrokerageStaysOutOfCryptoTax: stocks have no tax lots here, so they don't count as
// holdings of unknown cost, and brokerage accounts aren't crypto wallets for the tax
// package, the Koinly import or activity sync.
func TestBrokerageStaysOutOfCryptoTax(t *testing.T) {
	e := newEnv(t, "")
	addFidelity(t, e)
	rows := taxedAssets([]portfolio.AssetRow{{AssetKey: "BTC"}, {AssetKey: "stock:VTI"}})
	if len(rows) != 1 || rows[0].AssetKey != "BTC" {
		t.Errorf("taxed assets %+v", rows)
	}
	s := &Server{Deps: e.deps}
	for _, w := range s.reportWallets(report.YearReport{}, func(id string) string { return id }) {
		if w.Label == "Fidelity" {
			t.Error("the CPA package lists the brokerage account")
		}
	}
	_, opts := s.walletRows(koinlyDraft{})
	for _, o := range opts {
		if o.Value == "fidelity" {
			t.Error("the Koinly import offers the brokerage account")
		}
	}
	for _, o := range s.walletOptions(false) {
		if o.ID == "fidelity" {
			t.Error("the activity filter offers the brokerage account")
		}
	}
}
