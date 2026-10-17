package export

import (
	"bytes"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

func f64(v float64) *float64 { return &v }

func golden(t *testing.T, got bytes.Buffer, want string) {
	t.Helper()
	if got.String() != want {
		t.Errorf("csv mismatch\n--- got ---\n%s--- want ---\n%s", got.String(), want)
	}
	// Whatever we write must be readable as RFC 4180 with a constant column count.
	r := csv.NewReader(strings.NewReader(got.String()))
	if _, err := r.ReadAll(); err != nil {
		t.Errorf("not valid CSV: %v", err)
	}
}

func TestHoldings(t *testing.T) {
	snap := portfolio.Snapshot{Wallets: []portfolio.WalletView{
		{
			ID: "w1", Label: "Cold, vault", Type: "bitcoin", Status: portfolio.StatusSynced,
			Rows: []portfolio.HoldingRow{{
				Holding:  core.Holding{AssetKey: "BTC", Symbol: "BTC", Amount: 0.12345678},
				Price:    &core.Price{USD: 60000.5},
				ValueUSD: 7407.4,
				Priced:   true,
			}},
			// Hidden holdings (spam, dust, unpriced) are not part of the portfolio and not exported.
			HiddenRows: []portfolio.HoldingRow{{Holding: core.Holding{AssetKey: "spl:scam", Symbol: "SCAM", Amount: 1}}},
		},
		{
			ID: "w2", Label: "Kraken", Type: "kraken", Status: portfolio.StatusStale,
			Rows: []portfolio.HoldingRow{
				{
					Holding:  core.Holding{AssetKey: "DOT", Symbol: "DOT", Amount: 10, Protocol: "Kraken Earn"},
					Price:    &core.Price{USD: 5},
					ValueUSD: 50, Priced: true,
				},
				{
					Holding: core.Holding{AssetKey: "spl:Mint111", Symbol: "WEIRD", Amount: 1000000},
				},
			},
		},
		{
			ID: "w3", Label: "Solana", Type: "solana", Status: portfolio.StatusSynced,
			Rows: []portfolio.HoldingRow{{
				Holding:  core.Holding{AssetKey: "SOL", Symbol: "SOL", Amount: 2},
				Price:    &core.Price{USD: 150, Stale: true},
				ValueUSD: 300, Priced: true,
			}},
		},
	}}
	var buf bytes.Buffer
	if err := Holdings(&buf, snap); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, `wallet,source_type,asset,symbol,amount,price_usd,value_usd,stale,protocol
"Cold, vault",bitcoin,BTC,BTC,0.12345678,60000.5,7407.4,false,
Kraken,kraken,DOT,DOT,10,5,50,true,Kraken Earn
Kraken,kraken,spl:Mint111,WEIRD,1000000,,,true,
Solana,solana,SOL,SOL,2,150,300,true,
`)
}

func TestHoldingsEmptyIsHeaderOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := Holdings(&buf, portfolio.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, "wallet,source_type,asset,symbol,amount,price_usd,value_usd,stale,protocol\n")
}

func TestHistory(t *testing.T) {
	pts := []portfolio.Point{
		{T: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), V: 1234.5},
		{T: time.Date(2024, 1, 3, 0, 0, 0, 0, time.FixedZone("CET", 3600)), V: 0},
		{T: time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), V: 1e-7},
		{T: time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC), V: 12345678901.25},
	}
	var buf bytes.Buffer
	if err := History(&buf, pts); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, `timestamp,value_usd
2024-01-02T03:04:05Z,1234.5
2024-01-02T23:00:00Z,0
2024-01-04T00:00:00Z,0.0000001
2024-01-05T00:00:00Z,12345678901.25
`)
	buf.Reset()
	if err := History(&buf, nil); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, "timestamp,value_usd\n")
}

func TestPnLUnionOfYearsAndMissingBasis(t *testing.T) {
	rows := []PnLRow{
		{
			AssetKey: "BTC", Symbol: "BTC", Amount: 1.5, AvgCost: 20000, Basis: 30000, Value: 45000,
			Unrealized: 15000, UnrealizedPct: 50, HasBasis: true, MissingUnits: 0.25,
			Realized: map[int]float64{2023: -100.5, 2021: 250},
		},
		{AssetKey: "spl:Mint111", Symbol: "WEIRD", Amount: 1000000, Value: 12, HasBasis: false},
		{
			AssetKey: "ETH", Symbol: "", Amount: 0, HasBasis: false,
			Realized: map[int]float64{2022: 80},
		},
	}
	var buf bytes.Buffer
	if err := PnL(&buf, rows); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, `asset,amount,avg_cost,basis,value,unrealized,unrealized_pct,missing_basis_units,realized_2021,realized_2022,realized_2023
BTC,1.5,20000,30000,45000,15000,50,0.25,250,,-100.5
WEIRD,1000000,,,12,,,0,,,
ETH,0,,,0,,,0,,80,
`)
}

func TestPnLWithoutAnyRealized(t *testing.T) {
	var buf bytes.Buffer
	err := PnL(&buf, []PnLRow{{AssetKey: "BTC", Symbol: "BTC", Amount: 1, AvgCost: 10, Basis: 10, Value: 12, Unrealized: 2, UnrealizedPct: 20, HasBasis: true}})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, buf, "asset,amount,avg_cost,basis,value,unrealized,unrealized_pct,missing_basis_units\nBTC,1,10,10,12,2,20,0\n")
}

func TestActivityOneRowPerLeg(t *testing.T) {
	txs := []core.Tx{
		{
			ID: "kr:T1", SourceID: "kr", ExternalID: "T1", Time: time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC),
			Kind: core.TxTrade, Status: core.TxConfirmed,
			Legs: []core.Leg{
				{AssetKey: "BTC", Symbol: "BTC", Amount: 0.05, USD: f64(1503.9)},
				{AssetKey: "USD", Symbol: "USD", Amount: -1500, USD: f64(-1500)},
			},
			Fee: &core.Leg{AssetKey: "USD", Symbol: "USD", Amount: -3.9},
		},
		{
			ID: "btc:abc", SourceID: "btc", ExternalID: "abc", Time: time.Date(2024, 3, 2, 11, 30, 0, 0, time.UTC),
			Kind: core.TxSend, Status: core.TxPending,
			Legs:         []core.Leg{{AssetKey: "BTC", Symbol: "BTC", Amount: -0.01}},
			Counterparty: "bc1qexample", TxHash: "abc123",
		},
		{
			// Unknown source label falls back to the id; a note-only tx without legs still gets a row.
			ID: "x:1", SourceID: "x", ExternalID: "1", Time: time.Date(2024, 3, 3, 0, 0, 0, 0, time.UTC),
			Kind: core.TxFee, Status: core.TxFailed,
			Fee: &core.Leg{AssetKey: "SOL", Symbol: "SOL", Amount: -0.000005},
		},
	}
	labels := map[string]string{"kr": "Kraken, main", "btc": "Cold wallet"}
	var buf bytes.Buffer
	if err := Activity(&buf, txs, labels); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, `time,source,kind,status,asset,amount,usd,fee_asset,fee_amount,counterparty,tx_hash
2024-03-01T10:00:00Z,"Kraken, main",trade,confirmed,BTC,0.05,1503.9,USD,-3.9,,
2024-03-01T10:00:00Z,"Kraken, main",trade,confirmed,USD,-1500,-1500,,,,
2024-03-02T11:30:00Z,Cold wallet,send,pending,BTC,-0.01,,,,bc1qexample,abc123
2024-03-03T00:00:00Z,x,fee,failed,,,,SOL,-0.000005,,
`)
}

func TestActivityNilLabelsAndEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := Activity(&buf, nil, nil); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, "time,source,kind,status,asset,amount,usd,fee_asset,fee_amount,counterparty,tx_hash\n")
}

func TestTextCellsAreDefusedAgainstSpreadsheetFormulas(t *testing.T) {
	snap := portfolio.Snapshot{Wallets: []portfolio.WalletView{{
		Label: "=cmd|' /C calc'!A0", Type: "solana",
		Rows: []portfolio.HoldingRow{{
			Holding: core.Holding{AssetKey: "spl:x", Symbol: "@SUM(1+1)", Amount: -5, Protocol: "+evil"},
		}},
	}}}
	var buf bytes.Buffer
	if err := Holdings(&buf, snap); err != nil {
		t.Fatal(err)
	}
	rec, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	row := rec[1]
	if !strings.HasPrefix(row[0], "'=") || !strings.HasPrefix(row[3], "'@") || !strings.HasPrefix(row[8], "'+") {
		t.Errorf("text not defused: %q", row)
	}
	// Negative numbers are numbers, not text, and stay untouched.
	if row[4] != "-5" {
		t.Errorf("amount = %q", row[4])
	}
}

type failWriter struct{ after int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("disk full")
	}
	f.after -= len(p)
	return len(p), nil
}

func TestWriteErrorsAreReturned(t *testing.T) {
	big := make([]portfolio.Point, 5000)
	if err := History(&failWriter{}, big); err == nil {
		t.Error("History swallowed the write error")
	}
	if err := Holdings(&failWriter{}, portfolio.Snapshot{}); err == nil {
		t.Error("Holdings swallowed the write error")
	}
	if err := PnL(&failWriter{}, nil); err == nil {
		t.Error("PnL swallowed the write error")
	}
	if err := Activity(&failWriter{}, nil, nil); err == nil {
		t.Error("Activity swallowed the write error")
	}
}

func TestNonFiniteNumbersAreBlank(t *testing.T) {
	var buf bytes.Buffer
	nan := func() float64 { var z float64; return z / z }()
	if err := PnL(&buf, []PnLRow{{AssetKey: "BTC", Symbol: "BTC", Amount: 1, AvgCost: nan, HasBasis: true}}); err != nil {
		t.Fatal(err)
	}
	golden(t, buf, "asset,amount,avg_cost,basis,value,unrealized,unrealized_pct,missing_basis_units\nBTC,1,,0,0,0,0,0\n")
}
