package koinly

import (
	"reflect"
	"testing"

	"crypto-portfolio/internal/tax"
)

func TestSuggestWalletByAddress(t *testing.T) {
	txns := []Txn{{Type: "crypto_deposit", RecvWallet: "Solana (SOL) - Main", TxDest: "SoLAddr1"}}
	wallets := []Wallet{{ID: "btc", Type: "bitcoin", Addresses: []string{"bc1q"}}, {ID: "sol-main", Type: "solana", Addresses: []string{"SoLAddr1"}}}
	got := SuggestWallets(txns, nil, wallets)
	if got["Solana (SOL) - Main"] != "sol-main" {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestWalletBySendingAddress(t *testing.T) {
	txns := []Txn{{Type: "crypto_withdrawal", SendWallet: "my sol", TxSrc: "AAA"}}
	got := SuggestWallets(txns, nil, []Wallet{{ID: "s1", Addresses: []string{"AAA"}}})
	if got["my sol"] != "s1" {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestWalletByName(t *testing.T) {
	txns := []Txn{{RecvWallet: "Kraken (API keys)"}}
	got := SuggestWallets(txns, nil, []Wallet{{ID: "kraken", Type: "kraken"}})
	if got["Kraken (API keys)"] != "kraken" {
		t.Fatalf("got %v", got)
	}
	// two kraken wallets: ambiguous -> untracked
	got = SuggestWallets(txns, nil, []Wallet{{ID: "k1", Type: "kraken"}, {ID: "k2", Type: "kraken"}})
	if got["Kraken (API keys)"] != tax.Untracked {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestWalletUntracked(t *testing.T) {
	txns := []Txn{{RecvWallet: "Ethereum (ETH & Tokens) Main"}}
	gains := []tax.Disposal{{Wallet: "Coinbase Pro"}}
	got := SuggestWallets(txns, gains, []Wallet{{ID: "kraken", Type: "kraken"}})
	if got["Ethereum (ETH & Tokens) Main"] != tax.Untracked {
		t.Fatalf("got %v", got)
	}
	if got["Coinbase Pro"] != tax.Untracked {
		t.Fatalf("got %v", got)
	}
}

func TestWalletNames(t *testing.T) {
	txns := []Txn{{SendWallet: "b", RecvWallet: "a"}, {RecvWallet: "b"}}
	got := WalletNames(txns, []tax.Disposal{{Wallet: "c"}})
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestAssets(t *testing.T) {
	known := []AssetInfo{
		{Key: "BTC", Symbol: "BTC", Name: "Bitcoin"},
		{Key: "JUP", Symbol: "JUP", Name: "Jupiter"},
		{Key: "spl:X", Symbol: "JUP", Name: "Jupiter Clone"},
	}
	hs := []Holding{
		{Asset: "BTC (Bitcoin)", Symbol: "BTC", Name: "Bitcoin", CostUSD: 1, ValueUSD: 2},
		{Asset: "JUP (Jupiter)", Symbol: "JUP", Name: "Jupiter", ValueUSD: 1},
		{Asset: "JUP (Jupiter Clone)", Symbol: "JUP", Name: "Jupiter Clone", ValueUSD: 1},
		{Asset: "XMR (Monero)", Symbol: "XMR", Name: "Monero", CostUSD: 5, ValueUSD: 6},
		{Asset: "TNFT#42 (TestPass)", Symbol: "TNFT#42", Name: "TestPass"},
	}
	m, amb := SuggestAssets(hs, known)
	want := map[string]string{"BTC (Bitcoin)": "BTC", "JUP (Jupiter)": "JUP", "JUP (Jupiter Clone)": "spl:X", "XMR (Monero)": "XMR", "TNFT#42 (TestPass)": ""}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %v", m)
	}
	if len(amb) != 0 {
		t.Fatalf("amb %v", amb)
	}
}

func TestSuggestAssetsAmbiguous(t *testing.T) {
	known := []AssetInfo{{Key: "spl:A", Symbol: "BOZO", Name: "One"}, {Key: "spl:B", Symbol: "bozo", Name: "Two"}}
	hs := []Holding{{Asset: "BOZO (Bozo)", Symbol: "BOZO", Name: "Bozo", ValueUSD: 1}}
	m, amb := SuggestAssets(hs, known)
	if m["BOZO (Bozo)"] != "" || !reflect.DeepEqual(amb["BOZO (Bozo)"], []string{"spl:A", "spl:B"}) {
		t.Fatalf("m=%v amb=%v", m, amb)
	}
}
