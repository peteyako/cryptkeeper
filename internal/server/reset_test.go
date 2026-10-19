package server

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/manual"
)

// TestReset: Settings offers the reset; without the typed word nothing is erased; with it
// the sources, portfolios, activity and the keys saved in Settings are gone and the
// dashboard opens on an empty snapshot.
func TestReset(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	e.ps.Upsert(portfolios.Portfolio{ID: "fam", Name: "Family", Wallets: portfolios.Selector{IDs: []string{"kids"}}, Assets: portfolios.AllSelector()})
	tx := core.Tx{ID: "kids:t1", SourceID: "kids", Time: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Kind: core.TxReceive, Status: core.TxConfirmed,
		Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: 1}}}
	if _, err := e.act.Upsert(ctx, []core.Tx{tx}); err != nil {
		t.Fatal(err)
	}
	e.db.PutJSON(ctx, PricesKeyKV, "sealed-key")
	e.ref.Force(ctx)

	if _, page := e.do("GET", "/settings", nil, nil); !strings.Contains(page, `id="f-reset-confirm"`) || !strings.Contains(page, "Erase everything") {
		t.Fatal("Settings doesn't offer the reset")
	}

	body, hdr := form(url.Values{"confirm": {"yes"}})
	if _, out := e.do("POST", "/backup/reset", body, hdr); !strings.Contains(out, "Nothing was erased") {
		t.Fatalf("a wrong word: %.600s", out)
	}
	if _, ok := e.reg.Get("kids"); !ok {
		t.Fatal("a reset without the word erased the sources")
	}

	body, hdr = form(url.Values{"confirm": {" reset "}})
	if _, out := e.do("POST", "/backup/reset", body, hdr); !strings.Contains(out, `window.location.href = "/"`) {
		t.Fatalf("reset: %.600s", out)
	}
	if len(e.reg.Defs()) != 0 || len(e.ps.List()) != 0 {
		t.Errorf("left: sources %v, portfolios %v", e.reg.Defs(), e.ps.List())
	}
	if all, err := e.act.All(ctx, nil); err != nil || len(all) != 0 {
		t.Errorf("activity left: %v %v", all, err)
	}
	var key string
	if ok, _ := e.db.GetJSON(ctx, PricesKeyKV, &key); ok {
		t.Error("the key saved in Settings survived")
	}
	if snap, _ := e.ref.Latest(); len(snap.Wallets) != 0 || snap.TotalUSD != 0 {
		t.Errorf("the dashboard still shows %d wallets, $%v", len(snap.Wallets), snap.TotalUSD)
	}
}
