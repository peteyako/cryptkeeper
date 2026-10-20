package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/sources/manual"
)

// TestHoldingsPriceLine: the overview draws each asset's price over the last week from
// history, ending at the current price; an asset without a day of history gets a dash.
func TestHoldingsPriceLine(t *testing.T) {
	e := newEnv(t, "")
	e.price["BTC"] = 120
	if _, err := e.reg.Upsert(sources.Def{ID: "cold", Type: sources.TypeManual, Label: "Cold",
		Holdings: []manual.Holding{{Asset: "BTC", Amount: 1}, {Asset: "ETH", Amount: 2}}}); err != nil {
		t.Fatal(err)
	}
	// BTC was 100 three days ago; ETH has no history before this refresh.
	if _, err := e.db.Exec(`INSERT INTO history (ts, prices, amounts) VALUES (?, ?, ?)`,
		time.Now().Add(-72*time.Hour).Unix(), `{"BTC":100}`, `{"cold":{"BTC":1}}`); err != nil {
		t.Fatal(err)
	}
	e.ref.Rebuild(context.Background())

	_, ov := e.do("GET", "/", nil, nil)
	if n := strings.Count(ov, `<svg class="spark spark--up"`); n != 1 {
		t.Errorf("want one rising price line (BTC), got %d", n)
	}
	if !strings.Contains(ov, ": $100.00 to $120.00</title>") {
		t.Error("BTC's line should run from $100.00 to the current $120.00")
	}
	if n := strings.Count(ov, `aria-label="No 7 day price history yet"`); n != 1 {
		t.Errorf("want a dash for ETH, got %d", n)
	}
}
