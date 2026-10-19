package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/sources/manual"
	"cryptkeeper/internal/tax"
)

// fiBackfill posts "Load last N days" for the 1W range and returns the patched card's text.
func fiBackfill(e *env) string {
	e.t.Helper()
	_, body := e.do("POST", "/chart/backfill", chartBody("1W"), ds(map[string]string{"Content-Type": "application/json"}))
	if !strings.Contains(body, `id="value-card"`) {
		e.t.Fatalf("value card expected: %s", body)
	}
	return html.UnescapeString(body)
}

// fiPastRows counts the history rows before now.
func fiPastRows(e *env) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM history WHERE ts < ?`, taxNow.Unix()).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// fiHist is a HistoricalPrices whose lookups fail with err, or answer usd.
type fiHist struct {
	usd float64
	err error
}

func (f fiHist) Historical(context.Context, string, time.Time) (float64, bool, error) {
	return f.usd, f.err == nil, f.err
}

func (f fiHist) HistoricalMint(context.Context, string, time.Time) (float64, bool, error) {
	return f.usd, f.err == nil, f.err
}

// fiCloseHist also has the daily close by mint, at a different price.
type fiCloseHist struct{ fiHist }

func (fiCloseHist) HistoricalMintClose(context.Context, string, time.Time) (float64, bool, error) {
	return 7, true, nil
}

func fiKids(e *env, sol float64) {
	e.t.Helper()
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: sol}}})
}

func TestFIBackfillBeforeFirstRefresh(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	fiKids(e, 3) // added, but the balances have not been fetched yet
	body := fiBackfill(e)
	if !strings.Contains(body, "Your balances haven't loaded yet — try again in a moment.") {
		t.Errorf("note expected: %s", within(body, "value-card", 2000))
	}
	if n := fiPastRows(e); n != 0 {
		t.Errorf("%d rows written without a snapshot", n)
	}
}

func TestFIBackfillRefusesFailingWallet(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) { d.Historical = fiHist{usd: 150} })
	fiKids(e, 3)
	e.reg.Upsert(sources.Def{ID: "kr", Type: sources.TypeKraken, Label: "Invented Exchange", APIKey: "k"}) // no secret: fails
	e.ref.Force(context.Background())
	body := fiBackfill(e)
	if !strings.Contains(body, "Invented Exchange isn't syncing — try again once it has synced.") {
		t.Errorf("refusal expected: %s", within(body, "value-card", 2000))
	}
	if n := fiPastRows(e); n != 0 {
		t.Errorf("%d rows written while a wallet is failing", n)
	}
}

func TestFIBackfillPriceErrorIsFriendly(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) {
		d.Historical = fiHist{err: errors.New("https://upstream.invalid/candles: HTTP 503: overloaded")}
	})
	fiKids(e, 3)
	e.ref.Force(context.Background())
	body := fiBackfill(e)
	if !strings.Contains(body, "Couldn't fetch past prices right now — try again in a few minutes.") {
		t.Errorf("friendly note expected: %s", within(body, "value-card", 2000))
	}
	if strings.Contains(body, "HTTP 503") || strings.Contains(body, "upstream.invalid") {
		t.Errorf("raw error text shown: %s", within(body, "value-card", 2000))
	}
	if n := fiPastRows(e); n != 0 {
		t.Errorf("%d rows written with a fallback price", n)
	}
}

func TestFIBackfillNothingToFillShowsNote(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) { d.Historical = fiHist{usd: 150} })
	fiKids(e, 3)
	e.ref.Force(context.Background())
	// History starts 20 days ago (with a gap since): nothing before it is inside the week.
	if _, err := e.db.ExecContext(context.Background(), `INSERT INTO history (ts, prices, amounts) VALUES (?, ?, ?)`,
		taxNow.AddDate(0, 0, -20).Unix(), `{"SOL":100}`, `{"kids":{"SOL":3}}`); err != nil {
		t.Fatal(err)
	}
	body := fiBackfill(e)
	if !strings.Contains(body, "History already covers these days for every wallet; gaps after a wallet's first recorded day can't be backfilled.") {
		t.Errorf("note expected: %s", within(body, "value-card", 2000))
	}
	if strings.Contains(body, "Load last 7 days") || strings.Contains(body, "Couldn't") {
		t.Errorf("no offer and no error after a load that could add nothing: %s", within(body, "value-card", 2000))
	}
}

func TestFIBackfillHonoursPositionTags(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) { d.Historical = fiHist{usd: 150} })
	fiKids(e, 3)
	e.ref.Force(context.Background())
	// 2 SOL went into the user's own position yesterday; the send is tagged as a position move.
	send := core.Tx{ID: "kids:p", SourceID: "kids", ExternalID: "p", Time: taxNow.AddDate(0, 0, -1), Kind: core.TxSend,
		Status: core.TxConfirmed, Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -2}}}
	e.upsertTxs(send)
	if err := e.taxSet.Update(func(s *tax.Settings) {
		s.Tags = map[string]tax.TagEntry{"kids:p": {Tag: tax.TagPosition}}
	}); err != nil {
		t.Fatal(err)
	}
	fiBackfill(e)
	var amounts string
	if err := e.db.QueryRowContext(context.Background(), `SELECT amounts FROM history ORDER BY ts LIMIT 1`).Scan(&amounts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(amounts, `"kids":{"SOL":3}`) {
		t.Errorf("a tagged position move must keep its units: %s", amounts)
	}
}

func TestFIPriceAtUsesTheMintClose(t *testing.T) {
	day := taxNow.AddDate(0, 0, -5)
	srv := &Server{Deps: Deps{Historical: fiCloseHist{fiHist{usd: 3}}}}
	if usd, ok, err := srv.priceAt(context.Background(), "spl:Mint1", "Mint1", day); err != nil || !ok || usd != 7 {
		t.Errorf("SPL token: %v %v %v, want the daily close 7", usd, ok, err)
	}
	// A plain HistoricalPrices still prices by mint.
	srv.Historical = fiHist{usd: 3}
	if usd, ok, err := srv.priceAt(context.Background(), "spl:Mint1", "Mint1", day); err != nil || !ok || usd != 3 {
		t.Errorf("without the close lookup: %v %v %v", usd, ok, err)
	}
}

func TestFICoinChartRangeShowsLoading(t *testing.T) {
	e := coinEnv(t, func(d *Deps) { d.Historical = fakeSeries{} })
	_, body := e.do("GET", "/a/SOL", nil, nil)
	chart := within(body, `id="coin-chart"`, 4000)
	for _, want := range []string{`data-indicator="coinCharting"`, `$coinRange = `, `seg--on`, `aria-busy`} {
		if !strings.Contains(html.UnescapeString(chart), want) {
			t.Errorf("missing %q in the coin chart: %s", want, chart)
		}
	}
}

// countHist counts every upstream lookup and answers none.
type countHist struct{ n *atomic.Int32 }

func (c countHist) Historical(context.Context, string, time.Time) (float64, bool, error) {
	c.n.Add(1)
	return 0, false, nil
}

func (c countHist) HistoricalMint(context.Context, string, time.Time) (float64, bool, error) {
	c.n.Add(1)
	return 0, false, nil
}

func TestFIResolverSkipsQuietTokens(t *testing.T) {
	var n atomic.Int32
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) { d.Historical = countHist{&n} })
	srv := e.taxServer()
	quiet := map[string]bool{}
	for i := range 50 {
		quiet[fmt.Sprintf("spl:Junk%02d", i)] = true
	}
	r := srv.resolverWith(context.Background(), 2026, taxNow, quiet)
	for k := range quiet {
		if _, ok := r.Price(k, taxNow); ok {
			t.Fatalf("%s priced", k)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d upstream lookups for quiet tokens, want 0", n.Load())
	}
	r.Price("spl:Real1", taxNow) // a shown token still asks
	if n.Load() == 0 {
		t.Error("a shown token was not looked up")
	}
}

func TestFIResolverPricesSolanaYearEndByMintClose(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) { d.Historical = fiCloseHist{fiHist{usd: 3}} })
	r := e.taxServer().resolverWith(context.Background(), 2025, taxNow, nil)
	if v, ok := r.Price("spl:Mint1", time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC)); !ok || v != 7 {
		t.Errorf("year-end price = %v %v, want the daily close 7", v, ok)
	}
}
