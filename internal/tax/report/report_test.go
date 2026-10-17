package report

import (
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/tax"
)

func d(wallet, lotTx string, term tax.Term, acquired time.Time) tax.Disposal {
	return tax.Disposal{Wallet: wallet, LotTxID: lotTx, Term: term, Acquired: acquired}
}

func TestBoxRules(t *testing.T) {
	feb26 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	jun25 := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		d        tax.Disposal
		wtype    string
		override string
		koinly   bool
		want     string
	}{
		{"coinbase new same wallet", d("cb", "cb:b", tax.ShortTerm, feb26), "coinbase", "", false, "G"},
		{"coinbase old lot", d("cb", "cb:b", tax.ShortTerm, jun25), "coinbase", "", false, "H"},
		{"coinbase lot from other wallet", d("cb", "sol:b", tax.ShortTerm, feb26), "coinbase", "", false, "H"},
		{"coinbase long new", d("cb", "cb:b", tax.LongTerm, feb26), "coinbase", "", false, "J"},
		{"solana", d("sol", "sol:b", tax.ShortTerm, feb26), "solana", "", false, "I"},
		{"solana long", d("sol", "sol:b", tax.LongTerm, feb26), "solana", "", false, "L"},
		{"override short", d("sol", "", tax.ShortTerm, feb26), "solana", "H", false, "H"},
		{"override long", d("sol", "", tax.LongTerm, feb26), "solana", "H", false, "K"},
		{"koinly kraken", d("Kraken (API keys)", "", tax.ShortTerm, jun25), "", "", true, "H"},
		{"koinly other", d("Ledger", "", tax.ShortTerm, jun25), "", "", true, "I"},
	}
	for _, c := range cases {
		if got := BoxFor(c.d, c.wtype, lotWallet(c.d.LotTxID), c.override, c.koinly); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestSymbolFallback(t *testing.T) {
	y := tax.Year{Year: 2025, Disposals: []tax.Disposal{{Asset: "spl:AbCdEfGh123WxYz", Term: tax.ShortTerm, Wallet: "w"}}}
	got := FromClosed(y, Resolver{}, time.UTC)
	if got.Rows[0].Symbol != "AbCd…WxYz" {
		t.Errorf("symbol = %q", got.Rows[0].Symbol)
	}
}

func TestFromClosedTotals(t *testing.T) {
	y := tax.Year{Year: 2025, Disposals: []tax.Disposal{
		{Asset: "BTC", Wallet: "w", Term: tax.ShortTerm, ProceedsUSD: 20, CostUSD: 10, GainUSD: 10},
		{Asset: "BTC", Wallet: "w", Term: tax.ShortTerm, ProceedsUSD: 6, CostUSD: 10, GainUSD: -4},
		{Asset: "BTC", Wallet: "w", Term: tax.LongTerm, ProceedsUSD: 17, CostUSD: 10, GainUSD: 7},
	}}
	r := FromClosed(y, Resolver{}, time.UTC)
	if b := r.Boxes["I"]; b.Gain != 6 || b.Count != 2 {
		t.Errorf("I = %+v", b)
	}
	if b := r.Boxes["L"]; b.Gain != 7 || b.Count != 1 {
		t.Errorf("L = %+v", b)
	}
}

func TestHoldingsFromClosingLots(t *testing.T) {
	y := tax.Year{Year: 2025, ClosingLots: []tax.Lot{
		{Wallet: "a", Asset: "SOL", Amount: 2, CostUSD: 200},
		{Wallet: "b", Asset: "SOL", Amount: 1, CostUSD: 50},
	}}
	res := Resolver{
		Symbol: func(k string) (string, string) { return "SOL", "Solana" },
		Price:  func(string, time.Time) (float64, bool) { return 100, true },
	}
	r := FromClosed(y, res, time.UTC)
	if len(r.Holdings) != 1 {
		t.Fatalf("holdings = %+v", r.Holdings)
	}
	h := r.Holdings[0]
	if h.Symbol != "SOL" || h.Quantity != 3 || h.CostUSD != 250 || h.ValueUSD != 300 || !h.Priced {
		t.Errorf("holding = %+v", h)
	}
}

func TestFromOpenFiltersYear(t *testing.T) {
	loc := time.UTC
	res := tax.Result{
		Disposals: []tax.Disposal{
			{Sold: time.Date(2026, 3, 1, 0, 0, 0, 0, loc), Term: tax.ShortTerm, Asset: "BTC"},
			{Sold: time.Date(2027, 1, 1, 0, 0, 0, 0, loc), Term: tax.ShortTerm, Asset: "BTC"},
		},
		Income: []tax.Income{{Date: time.Date(2025, 12, 31, 0, 0, 0, 0, loc)}, {Date: time.Date(2026, 5, 1, 0, 0, 0, 0, loc)}},
	}
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, loc)
	r := FromOpen(2026, res, Resolver{}, loc, now)
	if len(r.Rows) != 1 || len(r.Income) != 1 || r.Closed || !r.AsOf.Equal(now) {
		t.Errorf("got %d rows %d income closed=%v asof=%v", len(r.Rows), len(r.Income), r.Closed, r.AsOf)
	}
}

func TestUntrackedLabelAndUnpricedHoldings(t *testing.T) {
	y := tax.Year{Year: 2025,
		Disposals:   []tax.Disposal{{Wallet: tax.Untracked, Term: tax.ShortTerm}},
		ClosingLots: []tax.Lot{{Wallet: "a", Asset: "ZED", Amount: 1, CostUSD: 5}},
	}
	r := FromClosed(y, Resolver{Price: func(string, time.Time) (float64, bool) { return 0, false }}, time.UTC)
	if r.Rows[0].WalletLabel != "Untracked (from Koinly)" {
		t.Errorf("label = %q", r.Rows[0].WalletLabel)
	}
	if h := r.Holdings[0]; h.Priced || h.ValueUSD != 0 {
		t.Errorf("holding = %+v", h)
	}
}

func TestHoldingsOrder(t *testing.T) {
	y := tax.Year{Year: 2025, ClosingLots: []tax.Lot{
		{Wallet: "a", Asset: "spl:zzz", Amount: 1}, {Wallet: "a", Asset: "spl:aaa", Amount: 1},
		{Wallet: "a", Asset: "SOL", Amount: 1}, {Wallet: "a", Asset: "BTC", Amount: 1},
	}}
	same := Resolver{Symbol: func(k string) (string, string) {
		if strings.HasPrefix(k, "spl:") {
			return "DUP", "Dup"
		}
		return k, k
	}}
	r := FromClosed(y, same, time.UTC)
	var got []string
	for _, h := range r.Holdings {
		got = append(got, h.AssetKey)
	}
	// Sorted by symbol; the two DUP assets tie and fall back to the asset key.
	if want := "BTC spl:aaa spl:zzz SOL"; strings.Join(got, " ") != want {
		t.Errorf("order %v, want %s", got, want)
	}
}
