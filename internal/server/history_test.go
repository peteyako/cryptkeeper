package server

import (
	"context"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/activity"
	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/sources/manual"
	"cryptkeeper/internal/store"
)

func chartBody(rng string) *strings.Reader {
	return strings.NewReader(`{"pf":"all","range":"` + rng + `"}`)
}

func TestValueCardOffersBackfill(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	_, body := e.do("GET", "/chart?datastar="+url.QueryEscape(`{"pf":"all","range":"1M"}`), nil, ds(nil))
	if !strings.Contains(body, "Load last 30 days") || !strings.Contains(body, "/chart/backfill") {
		t.Errorf("offer expected: %s", within(body, "value-card", 1500))
	}
	_, body = e.do("GET", "/chart?datastar="+url.QueryEscape(`{"pf":"all","range":"1D"}`), nil, ds(nil))
	if strings.Contains(body, "Load last") {
		t.Error("1D never offers a load")
	}
}

func TestValueCardNoOfferWhenValid(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	e.ref.Force(context.Background())
	for _, ts := range []time.Time{taxNow.Add(-7*24*time.Hour + time.Hour), taxNow.Add(-time.Hour)} {
		if _, err := e.db.ExecContext(context.Background(), `INSERT INTO history (ts, prices, amounts) VALUES (?, ?, ?)`,
			ts.Unix(), `{"SOL":100}`, `{"kids":{"SOL":3}}`); err != nil {
			t.Fatal(err)
		}
	}
	_, body := e.do("GET", "/chart?datastar="+url.QueryEscape(`{"pf":"all","range":"1W"}`), nil, ds(nil))
	if strings.Contains(body, "Load last") {
		t.Errorf("no offer when the range is covered: %s", within(body, "value-card", 1500))
	}
}

func TestChartBackfillPatches(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) {
		d.Historical = fakeHist{bySym: map[string]float64{"SOL": 150}}
	})
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	e.ref.Force(context.Background())
	_, body := e.do("POST", "/chart/backfill", chartBody("1W"), ds(map[string]string{"Content-Type": "application/json"}))
	if !strings.Contains(body, `id="value-card"`) {
		t.Fatalf("value card expected: %s", body)
	}
	if strings.Contains(body, "Couldn't load history") {
		t.Fatalf("unexpected error: %s", within(body, "Couldn't", 300))
	}
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM history WHERE ts < ?`, taxNow.Unix()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Errorf("7 days + 1 = 8 past rows, got %d", n)
	}
	if strings.Contains(body, "Load last 7 days") {
		t.Errorf("offer should be gone after loading: %s", within(body, "value-card", 1500))
	}
}

func TestChartBackfillNothingToLoadFor1D(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	_, body := e.do("POST", "/chart/backfill", chartBody("1D"), ds(map[string]string{"Content-Type": "application/json"}))
	if !strings.Contains(body, `id="value-card"`) || strings.Contains(body, "Load last") || strings.Contains(body, "Couldn't") {
		t.Errorf("1D has nothing to load; plain card expected: %s", body)
	}
}

func TestChartBackfillErrorCard(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow))
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	e.ref.Force(context.Background())
	broken, err := store.Open(filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	broken.Close() // every activity read now fails
	srv := e.taxServer()
	srv.Activity = &activity.Store{DB: broken}
	req := httptest.NewRequest("POST", "/chart/backfill", chartBody("1W"))
	req.Header.Set("Datastar-Request", "true")
	rec := httptest.NewRecorder()
	srv.chartBackfill(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `id="value-card"`) || !strings.Contains(body, "Couldn&#39;t load history") && !strings.Contains(body, "Couldn't load history") {
		t.Errorf("error note expected: %s", body)
	}
}

func TestChartBackfillDedupesCSVActivity(t *testing.T) {
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) {
		d.Historical = fakeHist{bySym: map[string]float64{"BTC": 100}}
	})
	if _, err := e.reg.Upsert(sources.Def{ID: "kr", Type: sources.TypeKraken, Label: "Kraken", APIKey: "k", APISecret: "s"}); err != nil {
		t.Fatal(err)
	}
	e.reg.Upsert(sources.Def{ID: "kraken-csv", Type: sources.TypeManual, Label: "CSV", Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}}})
	at := taxNow.Add(-3 * 24 * time.Hour)
	e.upsertTxs(reviewRecv("dup", "kr", "BTC", 0.2, at), reviewRecv("dup", "kraken-csv", "BTC", 0.2, at))
	e.ref.Force(context.Background())
	e.do("POST", "/chart/backfill", chartBody("1W"), ds(map[string]string{"Content-Type": "application/json"}))
	var amounts string
	if err := e.db.QueryRowContext(context.Background(), `SELECT amounts FROM history ORDER BY ts LIMIT 1`).Scan(&amounts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(amounts, `"kraken-csv":{"BTC":1}`) {
		t.Errorf("the CSV duplicate must not be rolled back: %s", amounts)
	}
}
