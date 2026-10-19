package koinly

import (
	"reflect"
	"testing"

	"crypto-portfolio/internal/tax"
)

// matchIDs is MatchWallets without the reasons: Koinly wallet name → wallet ID.
func matchIDs(txns []Txn, gains []tax.Disposal, wallets []Wallet) map[string]string {
	out := map[string]string{}
	for name, m := range MatchWallets(txns, gains, wallets) {
		out[name] = m.ID
	}
	return out
}

func TestSuggestWalletByAddress(t *testing.T) {
	txns := []Txn{{Type: "crypto_deposit", RecvWallet: "Solana (SOL) - Main", TxDest: "SoLAddr1"}}
	wallets := []Wallet{{ID: "btc", Type: "bitcoin", Addresses: []string{"bc1q"}}, {ID: "sol-main", Type: "solana", Addresses: []string{"SoLAddr1"}}}
	got := matchIDs(txns, nil, wallets)
	if got["Solana (SOL) - Main"] != "sol-main" {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestWalletBySendingAddress(t *testing.T) {
	txns := []Txn{{Type: "crypto_withdrawal", SendWallet: "my sol", TxSrc: "AAA"}}
	got := matchIDs(txns, nil, []Wallet{{ID: "s1", Addresses: []string{"AAA"}}})
	if got["my sol"] != "s1" {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestWalletByName(t *testing.T) {
	txns := []Txn{{RecvWallet: "Kraken (API keys)"}}
	got := matchIDs(txns, nil, []Wallet{{ID: "kraken", Type: "kraken"}})
	if got["Kraken (API keys)"] != "kraken" {
		t.Fatalf("got %v", got)
	}
	// two kraken wallets: ambiguous -> untracked
	got = matchIDs(txns, nil, []Wallet{{ID: "k1", Type: "kraken"}, {ID: "k2", Type: "kraken"}})
	if got["Kraken (API keys)"] != tax.Untracked {
		t.Fatalf("got %v", got)
	}
}

func TestSuggestWalletUntracked(t *testing.T) {
	txns := []Txn{{RecvWallet: "Ethereum (ETH & Tokens) Main"}}
	gains := []tax.Disposal{{Wallet: "Coinbase Pro"}}
	got := matchIDs(txns, gains, []Wallet{{ID: "kraken", Type: "kraken"}})
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

// TestMatchWalletByNameAddress: Koinly's Bitcoin wallets carry a shortened address in their
// name and no addresses in their transactions; the name finds the wallet that derives it.
func TestMatchWalletByNameAddress(t *testing.T) {
	txns := []Txn{{RecvWallet: "Bitcoin (BTC) - bc1q8w...dl"}, {RecvWallet: "Bitcoin (BTC) - 3Kx7Rt...Pq"}, {RecvWallet: "Bitcoin (BTC) - bc1q...dl"}}
	wallets := []Wallet{
		{ID: "k1", Label: "K1", Type: "bitcoin", Addresses: []string{"bc1qzz0000dl"}},
		{ID: "k2s", Label: "K2S", Type: "bitcoin", Addresses: []string{"bc1q8wx9f7dl", "bc1qr3k2p7v"}},
	}
	got := MatchWallets(txns, nil, wallets)
	if m := got["Bitcoin (BTC) - bc1q8w...dl"]; m.ID != "k2s" || m.Why != "bc1q8w…dl is one of K2S's addresses." {
		t.Errorf("bc1q8w: %+v", m)
	}
	if m := got["Bitcoin (BTC) - 3Kx7Rt...Pq"]; m.ID != tax.Untracked || m.Why == "" {
		t.Errorf("an address no wallet derives: %+v", m)
	}
	if m := got["Bitcoin (BTC) - bc1q...dl"]; m.ID != tax.Untracked {
		t.Errorf("a start too short to tell wallets apart matched: %+v", m)
	}
}

// TestMatchWalletByLabel: a label in the name matches within the chain the name is on, as a
// whole word, and only when one wallet has it.
func TestMatchWalletByLabel(t *testing.T) {
	txns := []Txn{{RecvWallet: "Solana (SOL) - K1S"}, {RecvWallet: "Solana (SOL) - K1S2"}, {RecvWallet: "Bitcoin (BTC) K1"}, {RecvWallet: "Cold K1"}}
	wallets := []Wallet{
		{ID: "k1s", Label: "K1S", Type: "solana"},
		{ID: "k1", Label: "K1", Type: "solana"},
		{ID: "btc-k1", Label: "K1", Type: "bitcoin"},
	}
	got := MatchWallets(txns, nil, wallets)
	for name, want := range map[string]string{
		"Solana (SOL) - K1S":  "k1s",
		"Solana (SOL) - K1S2": tax.Untracked,
		"Bitcoin (BTC) K1":    "btc-k1",
		"Cold K1":             tax.Untracked, // two wallets are named K1 and the name says no chain
	} {
		if got[name].ID != want {
			t.Errorf("%s: %+v, want %s", name, got[name], want)
		}
	}
	if got["Bitcoin (BTC) K1"].Why != "Named after K1." {
		t.Errorf("why: %q", got["Bitcoin (BTC) K1"].Why)
	}
}

// TestMatchWalletPrefersTransactions: the wallet whose addresses the transactions use most
// wins over a label in the name.
func TestMatchWalletPrefersTransactions(t *testing.T) {
	txns := []Txn{
		{RecvWallet: "Solana (SOL) - K1S", TxDest: "B"}, {RecvWallet: "Solana (SOL) - K1S", TxDest: "B"},
		{SendWallet: "Solana (SOL) - K1S", TxSrc: "B"}, {RecvWallet: "Solana (SOL) - K1S", TxDest: "A"},
	}
	wallets := []Wallet{{ID: "k1s", Label: "K1S", Type: "solana", Addresses: []string{"A"}}, {ID: "k2s", Label: "K2S", Type: "solana", Addresses: []string{"B", "C"}}}
	if m := MatchWallets(txns, nil, wallets)["Solana (SOL) - K1S"]; m.ID != "k2s" {
		t.Errorf("got %+v", m)
	}
}
