package koinly

import (
	"os"
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/tax"
)

func loadNY(t *testing.T) *time.Location {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return ny
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDetect(t *testing.T) {
	cases := map[string]Kind{
		"holdings.csv": "holdings", "transactions.csv": "transactions", "gains.csv": "gains",
		"income.csv": "income", "other_gains.csv": "other-gains",
	}
	for f, want := range cases {
		got, err := Detect(fixture(t, f))
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v want %q", f, got, err, want)
		}
	}
	// BOM + CRLF tolerated.
	raw := append([]byte("\xef\xbb\xbf"), []byte("Income report 2025\r\n\r\nDate,Asset,Amount,Value (USD),Type,Description,Wallet Name\r\n")...)
	if k, err := Detect(raw); err != nil || k != "income" {
		t.Errorf("bom/crlf: %q %v", k, err)
	}
	if _, err := Detect([]byte("a,b,c\n1,2,3\n")); err == nil || err.Error() != "not a Koinly report" {
		t.Errorf("random csv: %v", err)
	}
}

func TestParseHoldings(t *testing.T) {
	asOf, rows, err := ParseHoldings(fixture(t, "holdings.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !asOf.Equal(time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC)) {
		t.Errorf("asOf %v", asOf)
	}
	if len(rows) != 3 {
		t.Fatalf("rows %d (Total must be skipped)", len(rows))
	}
	r := rows[0]
	if r.Asset != "BTC (Bitcoin)" || r.Symbol != "BTC" || r.Name != "Bitcoin" ||
		r.Quantity != 0.5 || r.CostUSD != 20000 || r.ValueUSD != 45000 {
		t.Errorf("row %+v", r)
	}
}

func TestParseHoldingsNFTName(t *testing.T) {
	_, rows, err := ParseHoldings(fixture(t, "holdings.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if rows[1].Symbol != "TNFT#42" || rows[1].Name != "TestPass" {
		t.Errorf("%+v", rows[1])
	}
	if rows[2].Symbol != "$ CLAIM AT EXAMPLE.COM" || rows[2].Name != "$ Example.com" {
		t.Errorf("%+v", rows[2])
	}
}

func TestParseTransactions(t *testing.T) {
	txs, err := ParseTransactions(fixture(t, "transactions.csv"))
	if err != nil || len(txs) != 1 {
		t.Fatalf("%v %d", err, len(txs))
	}
	x := txs[0]
	if !x.Time.Equal(time.Date(2025, 3, 14, 9, 30, 0, 0, time.UTC)) || x.Type != "exchange" ||
		x.SentAmount != 0.12345678 || x.SentCurrency != "SOL" || x.SentCost != 15 ||
		x.RecvAmount != 31.250123 || x.RecvCurrency != "USDC" || x.RecvCost != 31.25 ||
		x.Gain != 16.25 || x.NetValue != 31.25 || x.FeeAmount != 0 || x.FeeValue != 0 ||
		x.TxSrc != "srcaddr" || x.TxHash != "hash123" {
		t.Errorf("%+v", x)
	}
}

func TestParseGains(t *testing.T) {
	ny := loadNY(t)
	ds, err := ParseGains(fixture(t, "gains.csv"), ny)
	if err != nil || len(ds) != 3 {
		t.Fatalf("%v %d", err, len(ds))
	}
	d := ds[0]
	if !d.Sold.Equal(time.Date(2025, 2, 11, 14, 27, 0, 0, ny)) || !d.Acquired.Equal(time.Date(2024, 8, 19, 10, 52, 0, 0, ny)) ||
		d.Term != tax.ShortTerm || d.Wallet != "Test Wallet A" || d.Asset != "SOL" ||
		d.Amount != 0.0025 || d.CostUSD != 0.30 || d.ProceedsUSD != 0.50 || d.GainUSD != 0.20 {
		t.Errorf("%+v", d)
	}
	if ds[1].Term != tax.LongTerm || ds[1].Note != "note here" || ds[1].ProceedsUSD != 2500.5 {
		t.Errorf("%+v", ds[1])
	}
}

func TestParseIncome(t *testing.T) {
	ny := loadNY(t)
	in, err := ParseIncome(fixture(t, "income.csv"), ny)
	if err != nil || len(in) != 1 {
		t.Fatalf("%v %d", err, len(in))
	}
	i := in[0]
	if i.Type != "staking" || i.Wallet != "Test Wallet" || i.Asset != "SOL" || i.Amount != 1.5 ||
		i.ValueUSD != 300.25 || !i.Date.Equal(time.Date(2025, 2, 3, 4, 5, 0, 0, ny)) {
		t.Errorf("%+v", i)
	}
}

func TestParseGainsVarious(t *testing.T) {
	ds, err := ParseGains(fixture(t, "gains.csv"), loadNY(t))
	if err != nil {
		t.Fatal(err)
	}
	if d := ds[2]; !d.Various || !d.Acquired.IsZero() || d.Term != tax.LongTerm {
		t.Errorf("%+v", d)
	}
}

func TestParseErrors(t *testing.T) {
	ny := loadNY(t)
	gh := "Capital gains report 2025\n\nDate Sold,Date Acquired,Asset,Amount,Cost (USD),Proceeds (USD),Gain / loss,Notes,Wallet Name,Holding period\n"
	cases := []struct {
		name, csv, want string
	}{
		{"bad number", gh + "02/11/2025 14:27,08/19/2024 10:52,SOL,abc,0.30,0.50,0.20,,W,Short term\n", "row 1 (file line 4)"},
		{"bad date", gh + "13/45/2025 14:27,08/19/2024 10:52,SOL,1,0.30,0.50,0.20,,W,Short term\n", "row 1"},
		{"unknown term", gh + "02/11/2025 14:27,08/19/2024 10:52,SOL,1,0.30,0.50,0.20,,W,Medium\n", "unknown holding period"},
		{"empty acquired", gh + "02/11/2025 14:27,,SOL,1,0.30,0.50,0.20,,W,Short term\n", "missing Date Acquired"},
		{"empty date with content", gh + "02/11/2025 14:27,08/19/2024 10:52,SOL,1,0.30,0.50,0.20,,W,Short term\n,08/19/2024 10:52,SOL,1,0.30,0.50,0.20,,W,Short term\n", "row 2 (file line 5): missing date"},
		{"wrong kind", string(fixture(t, "holdings.csv")), "expected a gains report"},
	}
	for _, c := range cases {
		_, err := ParseGains([]byte(c.csv), ny)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v want containing %q", c.name, err, c.want)
		}
	}
	// A fully blank row is skipped, not an error.
	if ds, err := ParseGains([]byte(gh+",,,,,,,,,\n"), ny); err != nil || len(ds) != 0 {
		t.Errorf("blank row: %v %d", err, len(ds))
	}
}

func TestSplitAssetNestedName(t *testing.T) {
	cases := []struct{ in, sym, name string }{
		{"WETH (Wrapped Ether (Wormhole))", "WETH", "Wrapped Ether (Wormhole)"},
		{"USDC.e (USD Coin (Bridged) v2)", "USDC.e", "USD Coin (Bridged) v2"},
		{"$ CLAIM AT EXAMPLE.COM ($ Example.com)", "$ CLAIM AT EXAMPLE.COM", "$ Example.com"},
		{"BTC (Bitcoin)", "BTC", "Bitcoin"},
		{"A (B) (C)", "A (B)", "C"},
		{"FOO", "FOO", ""},
	}
	for _, c := range cases {
		if sym, name := splitAsset(c.in); sym != c.sym || name != c.name {
			t.Errorf("splitAsset(%q) = %q, %q; want %q, %q", c.in, sym, name, c.sym, c.name)
		}
	}
}
