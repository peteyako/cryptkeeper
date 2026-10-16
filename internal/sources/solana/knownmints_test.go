package solana

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/base58"
)

func TestKnownMintLookups(t *testing.T) {
	// The mints listed in the task, each verified against the Jupiter token API (and the
	// ticker against Coinbase spot prices / Kraken USD pairs) on 2026-10-16.
	want := map[string]string{
		"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC",
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT",
		"JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN":  "JUP",
		"DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263": "BONK",
		"EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm": "WIF",
		"HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3": "PYTH",
		"4k3Dyjzvzp8eMZWUXbBCjEvwSkkk59S5iCNLY3QrkX6R": "RAY",
		"jtojtomepa8beP8AuQc6eXt5FriJwfFMwQx2v2f9mCL":  "JTO",
		"85VBFQZC9TZkfaptBWjvUw7YbZjy52A6mjtPGjstQAmQ": "W",
		"rndrizKT3MK1iimdxRdWabcF7Zg7AR5T4nud4EkHBof":  "RENDER",
		"hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux":  "HNT",
	}
	for mint, sym := range want {
		got, name, ok := KnownMint(mint)
		if !ok || got != sym {
			t.Errorf("KnownMint(%s) = %q, %v; want %q", mint, got, ok, sym)
		}
		if ok && name == "" {
			t.Errorf("KnownMint(%s): empty name", mint)
		}
	}
	if _, _, ok := KnownMint("So11111111111111111111111111111111111111112"); ok {
		t.Error("wSOL must not be in the table: it merges into SOL")
	}
	if _, _, ok := KnownMint("nope"); ok {
		t.Error("unknown mint reported as known")
	}
}

func TestKnownMintTableIsSane(t *testing.T) {
	seenSym := map[string]string{}
	for mint, k := range knownMints {
		if b := base58.Decode(mint); len(b) != 32 {
			t.Errorf("%s (%s): not a 32-byte base58 key", k.Symbol, mint)
		}
		if k.Symbol == "" || k.Name == "" {
			t.Errorf("%s: empty symbol or name", mint)
		}
		if other, dup := seenSym[k.Symbol]; dup {
			t.Errorf("symbol %s maps to both %s and %s", k.Symbol, other, mint)
		}
		seenSym[k.Symbol] = mint
	}
	if len(knownMints) < 11 {
		t.Errorf("table has only %d entries", len(knownMints))
	}
}
