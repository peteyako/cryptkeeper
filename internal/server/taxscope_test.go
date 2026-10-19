package server

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolios"
	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/sources/manual"
	"cryptkeeper/internal/tax"
)

func scopeUSD(v float64) *float64 { return &v }

// TestTaxScopeLeavesWalletsOut: with a scope, a left-out wallet's sale, lots and manual lots
// don't count; coins sent from a covered wallet to it move to the untracked wallet with
// their date and cost (no sale), and coins coming back draw on those lots (no purchase).
func TestTaxScopeLeavesWalletsOut(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	acquired := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return time.Date(2026, 3, d, 12, 0, 0, 0, time.UTC) }
	tx := func(id, wallet string, when time.Time, kind core.TxKind, btc, usd float64) core.Tx {
		return core.Tx{ID: wallet + ":" + id, SourceID: wallet, Time: when, Kind: kind, Status: core.TxConfirmed,
			Legs: []core.Leg{{AssetKey: "BTC", Symbol: "BTC", Amount: btc, USD: scopeUSD(usd)}}}
	}
	pair := func(send, recv core.Tx) (core.Tx, core.Tx) {
		send.PeerID, send.PeerSource = recv.ID, recv.SourceID
		recv.PeerID, recv.PeerSource = send.ID, send.SourceID
		return send, recv
	}
	out, in := pair(tx("out", "mine", day(2), core.TxTransfer, -0.5, -40000), tx("in", "theirs", day(2), core.TxTransfer, 0.5, 40000))
	back, home := pair(tx("back", "theirs", day(9), core.TxTransfer, -0.2, -16000), tx("home", "mine", day(9), core.TxTransfer, 0.2, 16000))
	input := tax.Input{
		From: from, Loc: time.UTC,
		Opening: []tax.Lot{
			{Wallet: "mine", Asset: "BTC", Amount: 1, CostUSD: 30000, Acquired: acquired},
			{Wallet: "theirs", Asset: "BTC", Amount: 2, CostUSD: 50000, Acquired: acquired},
		},
		Manual: []tax.Lot{{Wallet: "theirs", Asset: "BTC", Amount: 1, CostUSD: 1, Acquired: acquired}},
		Txs: []core.Tx{
			out, in, back, home,
			tx("sell", "theirs", day(5), core.TxTrade, -1, -80000),
		},
	}
	applyTaxScope(&input, func(w string) bool { return w != "theirs" })
	r := tax.Compute(input)

	if len(r.Disposals) != 0 || len(r.Income) != 0 {
		t.Fatalf("disposals %+v income %+v, want none: the left-out wallet's sale doesn't count, moves aren't sales", r.Disposals, r.Income)
	}
	held := map[string]float64{}
	for _, l := range r.Lots {
		held[l.Wallet] += l.Amount
		if l.Wallet != "theirs" && !l.Acquired.Equal(acquired) {
			t.Errorf("lot %+v lost its date", l)
		}
	}
	if held["theirs"] != 0 || held["mine"] != 0.7 || held[tax.Untracked] < 0.3-1e-9 || held[tax.Untracked] > 0.3+1e-9 {
		t.Errorf("held %v, want mine 0.7 (1 - 0.5 + 0.2), untracked 0.3, nothing in theirs", held)
	}
	for _, is := range r.Issues {
		t.Errorf("issue %+v: moves across the scope are own-wallet moves", is)
	}
}

// TestTaxScopePicker: Reports offers the portfolios that pick wallets; choosing one saves it,
// says what it means, and marks a left-out wallet's page; a portfolio that's gone falls back
// to every wallet, with a note.
func TestTaxScopePicker(t *testing.T) {
	e := newEnv(t, "")
	for _, id := range []string{"mine", "theirs"} {
		if _, err := e.reg.Upsert(sources.Def{ID: id, Type: sources.TypeManual, Label: strings.ToUpper(id[:1]) + id[1:], Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.ps.Upsert(portfolios.Portfolio{ID: "taxable", Name: "Taxable", Wallets: portfolios.Selector{IDs: []string{"mine"}}, Assets: portfolios.AllSelector()}); err != nil {
		t.Fatal(err)
	}
	e.ref.Force(context.Background())

	_, page := e.do("GET", "/reports", nil, nil)
	if !strings.Contains(page, `action="/reports/tax/scope"`) || !strings.Contains(page, "Taxes cover: Taxable") {
		t.Fatalf("no scope picker:\n%s", within(page, "tax__head", 900))
	}
	body, hdr := form(url.Values{"scope": {"taxable"}})
	if _, out := e.do("POST", "/reports/tax/scope", body, hdr); !strings.Contains(out, "window.location") && !strings.Contains(out, "redirect") {
		t.Fatalf("scope: %s", within(out, "notice", 600))
	}
	if got := e.taxSet.Get().Scope; got != "taxable" {
		t.Fatalf("saved scope %q", got)
	}
	_, page = e.do("GET", "/reports", nil, nil)
	if !strings.Contains(page, `<option value="taxable" selected>`) || !strings.Contains(page, "Covers the wallets of Taxable.") {
		t.Errorf("the chosen scope isn't shown:\n%s", within(page, "tax__head", 1400))
	}
	if _, w := e.do("GET", "/w/theirs", nil, nil); !strings.Contains(w, "Not in your tax reports") {
		t.Error("a left-out wallet's page doesn't say so")
	}
	if _, w := e.do("GET", "/w/mine", nil, nil); strings.Contains(w, "Not in your tax reports") {
		t.Error("a covered wallet says it's left out")
	}

	body, hdr = form(url.Values{"scope": {"bogus"}})
	if _, out := e.do("POST", "/reports/tax/scope", body, hdr); !strings.Contains(out, "no longer exists") {
		t.Errorf("an unknown portfolio must be refused: %.400s", out)
	}
	if err := e.ps.Delete("taxable"); err != nil {
		t.Fatal(err)
	}
	if _, page = e.do("GET", "/reports", nil, nil); !strings.Contains(html.UnescapeString(page), "so they cover every wallet") {
		t.Error("a deleted scope portfolio isn't explained")
	}
}
