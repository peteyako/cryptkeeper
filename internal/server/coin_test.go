package server

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"crypto-portfolio/internal/prices"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
)

// fakeSeries is a Historical that also serves a price series.
type fakeSeries struct {
	fakeHist
	pts  []prices.Point
	days *int
}

func (f fakeSeries) Series(_ context.Context, _, _ string, days int) ([]prices.Point, error) {
	if f.days != nil {
		*f.days = days
	}
	return f.pts, nil
}

func coinEnv(t *testing.T, opts ...func(*Deps)) *env {
	e := newEnv(t, "", append([]func(*Deps){nowAt(taxNow)}, opts...)...)
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	e.reg.Upsert(sources.Def{ID: "vault", Type: sources.TypeManual, Label: "Vault", Holdings: []manual.Holding{{Asset: "SOL", Amount: 2}}})
	e.ref.Force(context.Background())
	return e
}

func coinPost(rng string) (string, map[string]string) {
	return "/chart?range=" + rng, ds(nil)
}

func TestCoinPage(t *testing.T) {
	var days int
	e := coinEnv(t, func(d *Deps) { d.Historical = fakeSeries{days: &days} })
	resp, body := e.do("GET", "/a/SOL", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if days != 0 {
		t.Errorf("the page must not load the series itself, Series(%d) was called", days)
	}
	for _, want := range []string{`id="coin-chart"`, "Loading…", "/a/SOL/chart?range=30D", "data-init", "Kids", "Vault", "30D", "7D", "90D", "1Y"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q: %s", want, within(body, "coin", 800))
		}
	}
}

func TestCoinChartRange(t *testing.T) {
	var days int
	pts := []prices.Point{
		{Day: taxNow.AddDate(0, 0, -2), USD: 100}, {Day: taxNow.AddDate(0, 0, -1), USD: 110}, {Day: taxNow, USD: 120},
	}
	e := coinEnv(t, func(d *Deps) { d.Historical = fakeSeries{pts: pts, days: &days} })
	q, h := coinPost("7D")
	_, body := e.do("POST", "/a/SOL"+q, nil, h)
	if !strings.Contains(body, `id="coin-chart"`) || !strings.Contains(body, "<path") || !strings.Contains(body, "past 7 days") {
		t.Errorf("chart patch expected: %s", body)
	}
	if days != 7 {
		t.Errorf("Series days = %d, want 7", days)
	}
}

func TestCoinChartEmptySeries(t *testing.T) {
	e := coinEnv(t)
	q, h := coinPost("30D")
	_, body := e.do("POST", "/a/SOL"+q, nil, h)
	if !strings.Contains(body, "No price history for SOL") {
		t.Errorf("empty note expected: %s", body)
	}
}

func TestCoinUnknown404(t *testing.T) {
	e := coinEnv(t)
	if resp, _ := e.do("GET", "/a/NOPE", nil, nil); resp.StatusCode != 404 {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	q, h := coinPost("7D")
	if resp, _ := e.do("POST", "/a/NOPE"+q, nil, h); resp.StatusCode != 404 {
		t.Errorf("chart status %d, want 404", resp.StatusCode)
	}
}

func TestCoinSPLKeyRoundTrip(t *testing.T) {
	var gotKey string
	pts := []prices.Point{{Day: taxNow.AddDate(0, 0, -1), USD: 1}, {Day: taxNow, USD: 2}}
	e := newEnv(t, "", nowAt(taxNow), func(d *Deps) { d.Historical = keySeries{fakeSeries: fakeSeries{pts: pts}, key: &gotKey} })
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "spl:MintX1", Amount: 4}}})
	e.ref.Force(context.Background())
	_, home := e.do("GET", "/", nil, nil)
	href := `/a/` + url.PathEscape("spl:MintX1")
	if !strings.Contains(home, `href="`+href+`"`) {
		t.Fatalf("overview should link %s: %s", href, within(home, "asset__name", 300))
	}
	resp, body := e.do("GET", href, nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `id="coin-chart"`) || !strings.Contains(body, "Kids") {
		t.Fatalf("held SPL coin page: %d %s", resp.StatusCode, within(body, "coin", 500))
	}
	e.do("POST", href+"/chart?range=7D", nil, ds(nil))
	if gotKey != "spl:MintX1" {
		t.Errorf("Series got key %q, want the unescaped spl: key", gotKey)
	}
	if resp, _ := e.do("GET", "/a/"+url.PathEscape("spl:Other"), nil, nil); resp.StatusCode != 404 {
		t.Errorf("unknown SPL key: %d", resp.StatusCode)
	}
}

type keySeries struct {
	fakeSeries
	key *string
}

func (k keySeries) Series(ctx context.Context, key, mint string, days int) ([]prices.Point, error) {
	*k.key = key
	return k.fakeSeries.Series(ctx, key, mint, days)
}

func TestAssetLinksToCoin(t *testing.T) {
	e := coinEnv(t)
	_, body := e.do("GET", "/", nil, nil)
	if !strings.Contains(body, `href="/a/SOL"`) {
		t.Errorf("overview should link the asset: %s", within(body, "asset__name", 300))
	}
	_, body = e.do("GET", "/w/kids", nil, nil)
	if !strings.Contains(body, `href="/a/SOL"`) {
		t.Errorf("wallet rows should link the asset: %s", within(body, "asset__name", 300))
	}
}
