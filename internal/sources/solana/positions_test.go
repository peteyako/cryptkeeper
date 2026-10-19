package solana

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cryptkeeper/internal/sources/solana/defi"
)

type fakeAdapter struct {
	name string
	pos  []defi.Position
	err  error
	saw  defi.Input
}

func (f *fakeAdapter) Name() string { return f.name }
func (f *fakeAdapter) Positions(_ context.Context, _ defi.RPC, in defi.Input) ([]defi.Position, error) {
	f.saw = in
	return f.pos, f.err
}

func TestDeFiPositionsBecomeHoldingsAndClaimMints(t *testing.T) {
	f, w := richWallet(t)
	s := f.source(func(c *Config) { c.RPCURL += "/?api-key=SECRET" }) // Helius-style keyed URL
	orca := &fakeAdapter{name: defi.NameOrca, pos: []defi.Position{{
		Protocol: defi.NameOrca, ID: "PosAcct1", Kind: defi.KindLP, Label: "SOL/USDC · in range",
		ClaimedMints: []string{w.nft},
		Legs: []defi.Leg{
			{Mint: WrappedSOLMint, Amount: 3, AmountStr: "3"},
			{Mint: usdcMint, Amount: 450.5, AmountStr: "450.5"},
			{Mint: w.mpx, Amount: 0}, // zero leg: out of range side, not shown
		},
	}}}
	// The failing adapter's error mentions the RPC URL with an API key: it must be scrubbed.
	ray := &fakeAdapter{name: defi.NameRaydium, err: errors.New("fetch " + s.cfg.RPCURL + ": boom")}
	s.defi = []defi.Adapter{orca, ray}

	res, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.NFTCount != 0 {
		t.Errorf("claimed position NFT still counted: %d", res.NFTCount)
	}
	var legs int
	for _, h := range res.Holdings {
		if h.Protocol == "" {
			continue
		}
		legs++
		if h.Position != "PosAcct1" || h.PositionKind != defi.KindLP || h.Note != "SOL/USDC · in range" || !h.Verified {
			t.Errorf("leg tagging: %+v", h)
		}
		if h.Mint == WrappedSOLMint && (h.AssetKey != "SOL" || h.Amount != 3) {
			t.Errorf("wSOL leg must map to SOL: %+v", h)
		}
		if h.Mint == usdcMint && h.AssetKey != "USDC" {
			t.Errorf("known mint leg must take the exchange symbol: %+v", h)
		}
	}
	if legs != 2 {
		t.Errorf("want 2 non-zero position legs, got %d", legs)
	}
	var warned bool
	for _, wmsg := range res.Warnings {
		if strings.Contains(wmsg, "Raydium positions unavailable") {
			warned = true
			if strings.Contains(wmsg, "SECRET") {
				t.Errorf("API key leaked into a warning: %q", wmsg)
			}
		}
	}
	if !warned {
		t.Errorf("adapter failure must be a warning: %q", res.Warnings)
	}
	// Adapters see the wallet's tokens incl. the NFT, with names from the known-mint table.
	if len(orca.saw.Tokens) == 0 || orca.saw.Symbol(usdcMint) != "USDC" {
		t.Errorf("adapter input: %d tokens, USDC symbol %q", len(orca.saw.Tokens), orca.saw.Symbol(usdcMint))
	}
}
