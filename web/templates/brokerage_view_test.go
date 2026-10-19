package templates

import (
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
)

func TestUnitsShares(t *testing.T) {
	for _, tc := range []struct {
		key  string
		v    float64
		want string
	}{
		{"stock:VTI", 40, "40 VTI"},
		{"stock:VTSAX", 12.5, "12.5 VTSAX"},
		{"stock:BND", 1234.56789, "1,234.5679 BND"},
		{"ETH", 3.75, "3.7500 ETH"}, // coins keep their own format
	} {
		sym := tc.key
		if t, ok := core.IsStockKey(tc.key); ok {
			sym = t
		}
		if got := Units(tc.key, tc.v, sym); got != tc.want {
			t.Errorf("Units(%s, %v) = %q, want %q", tc.key, tc.v, got, tc.want)
		}
	}
}

// TestSessionDay: a stock's change is "today" while its market is open or after today's
// close, and the weekday of the last session while the market is shut since then.
func TestSessionDay(t *testing.T) {
	fridayClose := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC) // 4:00 PM in New York
	saturday := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	if got := SessionDay(core.Price{MarketTime: fridayClose}, saturday); got != "Friday" {
		t.Errorf("Saturday: %q, want Friday", got)
	}
	if got := SessionDay(core.Price{MarketTime: fridayClose}, fridayClose.Add(3*time.Hour)); got != "today" {
		t.Errorf("Friday evening: %q, want today", got)
	}
	if got := SessionDay(core.Price{MarketTime: fridayClose, MarketOpen: true}, saturday); got != "today" {
		t.Errorf("open: %q, want today", got)
	}
	if got := ChangeLabel("BTC", &core.Price{}, saturday); got != "24h" {
		t.Errorf("coin label %q", got)
	}
}

func TestMarketNoteAndDayChange(t *testing.T) {
	fridayClose := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	saturday := fridayClose.Add(20 * time.Hour)
	up, down := 10.0, -50.0
	w := portfolio.WalletView{Kind: core.KindBrokerage, ValueUSD: 160, Rows: []portfolio.HoldingRow{
		{Holding: core.Holding{AssetKey: "stock:VTI"}, Priced: true, ValueUSD: 110, Price: &core.Price{Change24h: &up, MarketTime: fridayClose}},
		{Holding: core.Holding{AssetKey: "stock:BND"}, Priced: true, ValueUSD: 50, Price: &core.Price{Change24h: &down, MarketTime: fridayClose}},
	}}
	if note := MarketNote(w, saturday); !strings.Contains(note, "Friday's close") {
		t.Errorf("note %q", note)
	}
	usd, pct := DayChange(w) // before: 100 + 100
	if usd != -40 || pct != -20 {
		t.Errorf("day change %v %v%%, want -40 -20%%", usd, pct)
	}
	w.Rows[0].Price.MarketOpen = true
	if note := MarketNote(w, saturday); note != "" {
		t.Errorf("an open market still notes %q", note)
	}
}
