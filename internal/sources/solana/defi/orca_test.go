package defi

import (
	"context"
	"crypto/sha256"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/base58"
)

const (
	orcaSOLUSDCPool = "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"
	// Three real positions in the Orca SOL/USDC whirlpool (captured 2026-10-16), by NFT mint.
	orcaMintIn    = "HdfvtgvhBBus2xPnAwUyH63Hw4M8aF1ssKvLHAgU9Ebk" // position 4N7wo…, ticks [-23028, -19660)
	orcaMintBelow = "73anSzosdCUZR61SR9EvWCug9JiqNRXvH7V5fNjMRxr4" // position GBNeR…, ticks [-16608, -12380)
	orcaMintAbove = "VpFvzdxC5ZtAcssusJqb9onUie3R97JELw1tZYSKHA5"  // position 6XMc1…, ticks [-26216, -25012)
	orcaPosIn     = "4N7woWHDb5MmFSv6STA1J4asde7xa2zkMEgSz7GG2Ram"
	orcaPosBelow  = "GBNeRpAE3nxiKmj5kgeAjWHoMN29eSGezktEQ1APFYNE"
	orcaPosAbove  = "6XMc1CEbtB9sveZyFmXgxSqK2DJvTjkfoMrKsTC1z6qH"
	bonkMint      = "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263" // its "position" PDA does not exist
)

// Expected amounts were computed independently with the official Orca SDK
// (@orca-so/whirlpools-core 3.1.1 decreaseLiquidityQuote, slippage 0, over the same captured
// accounts): see docs/defi-sources.md.
var orcaWant = map[string]struct {
	inRange bool
	a, b    string
	label   string
}{
	orcaPosIn:    {true, "31.715359929", "329.650911", "SOL/USDC · in range"},
	orcaPosBelow: {false, "47.445880791", "0", "SOL/USDC · out of range"},
	orcaPosAbove: {false, "0", "679.348979", "SOL/USDC · out of range"},
}

func orcaInput() Input {
	return Input{
		Owner: "94gKsZWhwNWRzkJ9a7JyL13sreDMepwLkmpEKndX3SPH",
		Tokens: []TokenBalance{
			nft(orcaMintIn), nft(orcaMintBelow), nft(orcaMintAbove),
			nft(bonkMint), // an NFT-shaped balance that is not a position
			{Mint: usdcMint, Amount: big.NewInt(5_000_000), Decimals: 6},
			nft(orcaMintIn), // duplicate balance: derived once
		},
	}
}

func TestOrcaPositionsFromMainnet(t *testing.T) {
	accts, _ := loadAccounts(t, "orca_accounts.json")
	rpc := newFakeRPC(accts)
	ps, err := newOrca().Positions(context.Background(), rpc, orcaInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 {
		t.Fatalf("got %d positions: %+v", len(ps), ps)
	}
	for id, w := range orcaWant {
		p := findPosition(t, ps, id)
		if p.Protocol != "orca" || p.Kind != KindLP || p.Label != w.label {
			t.Errorf("%s: protocol/kind/label = %q/%q/%q", id, p.Protocol, p.Kind, p.Label)
		}
		if p.InRange == nil || *p.InRange != w.inRange {
			t.Errorf("%s: InRange = %v, want %v", id, p.InRange, w.inRange)
		}
		if len(p.Legs) != 2 {
			t.Fatalf("%s: legs %+v", id, p.Legs)
		}
		a, b := p.Legs[0], p.Legs[1]
		if a.Mint != wsolMint || a.Decimals != 9 || a.AmountStr != w.a {
			t.Errorf("%s: leg A = %+v, want SOL %s", id, a, w.a)
		}
		if b.Mint != usdcMint || b.Decimals != 6 || b.AmountStr != w.b {
			t.Errorf("%s: leg B = %+v, want USDC %s", id, b, w.b)
		}
		if a.USDValue != nil || b.USDValue != nil {
			t.Errorf("%s: LP legs must not carry USDValue", id)
		}
	}
	if p := findPosition(t, ps, orcaPosIn); p.Legs[0].Amount != 31.715359929 || p.Legs[1].Amount != 329.650911 {
		t.Errorf("float amounts = %v, %v", p.Legs[0].Amount, p.Legs[1].Amount)
	}
	// Each position claims exactly its own NFT.
	for id, mint := range map[string]string{orcaPosIn: orcaMintIn, orcaPosBelow: orcaMintBelow, orcaPosAbove: orcaMintAbove} {
		if got := findPosition(t, ps, id).ClaimedMints; !slices.Equal(got, []string{mint}) {
			t.Errorf("%s: ClaimedMints = %v", id, got)
		}
	}
	// One batch each for positions, pools and mints; the fungible USDC balance is never probed.
	if len(rpc.calls) != 3 {
		t.Errorf("RPC calls = %d (%v), want 3", len(rpc.calls), rpc.calls)
	}
	if len(rpc.calls[0]) != 4 {
		t.Errorf("position batch = %v, want 4 distinct PDAs", rpc.calls[0])
	}
	if !rpc.requested(orcaSOLUSDCPool) || !rpc.requested(wsolMint) || !rpc.requested(usdcMint) {
		t.Errorf("pool/mints not fetched: %v", rpc.calls)
	}
}

func TestOrcaPositionPDA(t *testing.T) {
	mb, _ := pubkeyBytes(orcaMintIn)
	got, err := findProgramAddress([][]byte{[]byte("position"), mb}, OrcaWhirlpoolProgram)
	if err != nil || got != orcaPosIn {
		t.Fatalf("PDA = %s, %v; want %s", got, err, orcaPosIn)
	}
}

func TestOrcaSymbolResolver(t *testing.T) {
	accts, _ := loadAccounts(t, "orca_accounts.json")
	in := orcaInput()
	in.Symbol = func(m string) string {
		if m == wsolMint {
			return "wSOL"
		}
		return ""
	}
	ps, err := newOrca().Positions(context.Background(), newFakeRPC(accts), in)
	if err != nil {
		t.Fatal(err)
	}
	if l := findPosition(t, ps, orcaPosIn).Label; l != "wSOL/USDC · in range" {
		t.Errorf("label = %q", l)
	}
}

// testPubkey returns a deterministic valid-looking base58 pubkey.
func testPubkey(i int) string {
	sum := sha256.Sum256([]byte{byte(i), byte(i >> 8), 'k'})
	return base58.Encode(sum[:])
}

func TestOrcaChunksCandidates(t *testing.T) {
	accts, _ := loadAccounts(t, "orca_accounts.json")
	in := orcaInput()
	for i := range 250 {
		in.Tokens = append(in.Tokens, nft(testPubkey(i)))
	}
	rpc := newFakeRPC(accts)
	ps, err := newOrca().Positions(context.Background(), rpc, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 {
		t.Fatalf("got %d positions", len(ps))
	}
	// 254 distinct PDAs → 3 position chunks, then pools and mints.
	if len(rpc.calls) != 5 {
		t.Fatalf("calls = %d", len(rpc.calls))
	}
	for _, c := range rpc.calls {
		if len(c) > maxAccountsPerCall {
			t.Errorf("chunk of %d keys", len(c))
		}
	}
}

func TestOrcaNoCandidates(t *testing.T) {
	rpc := newFakeRPC(nil)
	ps, err := newOrca().Positions(context.Background(), rpc, Input{Owner: "x", Tokens: []TokenBalance{
		{Mint: usdcMint, Amount: big.NewInt(1), Decimals: 6},
		{Mint: "not-a-pubkey", Amount: big.NewInt(1)},
		{Mint: testPubkey(1), Amount: nil},
	}})
	if err != nil || len(ps) != 0 {
		t.Fatalf("got %v, %v", ps, err)
	}
	if len(rpc.calls) != 0 {
		t.Errorf("unexpected RPC calls %v", rpc.calls)
	}
}

func TestOrcaMissingAccounts(t *testing.T) {
	// Only the non-position NFT: its PDA is missing → no positions, no error, one call.
	accts, _ := loadAccounts(t, "orca_accounts.json")
	rpc := newFakeRPC(accts)
	ps, err := newOrca().Positions(context.Background(), rpc, Input{Tokens: []TokenBalance{nft(bonkMint)}})
	if err != nil || len(ps) != 0 || len(rpc.calls) != 1 {
		t.Fatalf("got %v, %v, calls %d", ps, err, len(rpc.calls))
	}

	// Pool missing: positions are skipped and reported.
	delete(accts, orcaSOLUSDCPool)
	ps, err = newOrca().Positions(context.Background(), newFakeRPC(accts), orcaInput())
	if err == nil || len(ps) != 0 || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("missing pool: got %v, %v", ps, err)
	}

	// Mint missing: decimals unknown → skipped and reported.
	accts, _ = loadAccounts(t, "orca_accounts.json")
	delete(accts, usdcMint)
	ps, err = newOrca().Positions(context.Background(), newFakeRPC(accts), orcaInput())
	if err == nil || len(ps) != 0 || !strings.Contains(err.Error(), "decimals") {
		t.Fatalf("missing mint: got %v, %v", ps, err)
	}
}

func TestOrcaRPCError(t *testing.T) {
	rpc := newFakeRPC(nil)
	rpc.gmaErr = errors.New("boom")
	if _, err := newOrca().Positions(context.Background(), rpc, orcaInput()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrcaMalformedAccounts(t *testing.T) {
	base, _ := loadAccounts(t, "orca_accounts.json")
	clone := func() map[string][]byte {
		m := make(map[string][]byte, len(base))
		for k, v := range base {
			m[k] = slices.Clone(v)
		}
		return m
	}

	// Truncated, wrong discriminator, or claiming another mint: not treated as a position.
	for name, mutate := range map[string]func([]byte) []byte{
		"truncated":     func(d []byte) []byte { return d[:orcaPosMinLen-1] },
		"discriminator": func(d []byte) []byte { d[0] ^= 0xff; return d },
		"other mint":    func(d []byte) []byte { copy(d[orcaPosMint:], make([]byte, 32)); return d },
	} {
		accts := clone()
		accts[orcaPosIn] = mutate(accts[orcaPosIn])
		ps, err := newOrca().Positions(context.Background(), newFakeRPC(accts), orcaInput())
		if err != nil || len(ps) != 2 {
			t.Errorf("%s: got %d positions, %v", name, len(ps), err)
		}
	}

	// Bad tick ranges are reported, not panicked on.
	for name, ticks := range map[string][2]int32{"inverted": {100, -100}, "out of bounds": {-500000, 0}} {
		accts := clone()
		d := accts[orcaPosIn]
		putI32(d, orcaPosTickLower, ticks[0])
		putI32(d, orcaPosTickUpper, ticks[1])
		ps, err := newOrca().Positions(context.Background(), newFakeRPC(accts), orcaInput())
		if err == nil || len(ps) != 2 {
			t.Errorf("%s: got %d positions, %v", name, len(ps), err)
		}
	}

	// Every truncation of every account decodes without panicking.
	for _, key := range []string{orcaPosIn, orcaSOLUSDCPool, wsolMint} {
		for n := 0; n <= len(base[key]); n++ {
			accts := clone()
			accts[key] = accts[key][:n]
			_, _ = newOrca().Positions(context.Background(), newFakeRPC(accts), orcaInput())
		}
	}
}

func putI32(d []byte, off int, v int32) {
	u := uint32(v)
	d[off], d[off+1], d[off+2], d[off+3] = byte(u), byte(u>>8), byte(u>>16), byte(u>>24)
}
