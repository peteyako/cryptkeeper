package solana

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/sources/solana/defi"
)

// defiTimeout bounds each DeFi adapter per fetch; adapters run concurrently.
const defiTimeout = 20 * time.Second

// defiPositions runs the configured DeFi adapters over the wallet's tokens and returns the
// positions as holdings (one per non-zero leg, tagged with protocol/position/kind), the
// token mints the positions account for (position NFTs, LP tokens — removed from the plain
// token list and the NFT count), and warnings for adapters that failed.
func (s *Source) defiPositions(ctx context.Context, ts *tokenSet) ([]core.Holding, map[string]bool, []string) {
	if len(s.defi) == 0 {
		return nil, nil, nil
	}
	in := defi.Input{Owner: s.cfg.Address, Symbol: s.symbolFor}
	for mint, mb := range ts.byMint {
		if mb.Amount.Sign() > 0 {
			in.Tokens = append(in.Tokens, defi.TokenBalance{Mint: mint, Amount: new(big.Int).Set(mb.Amount), Decimals: mb.Decimals, Token2022: mb.Token2022})
		}
	}

	type result struct {
		name string
		pos  []defi.Position
		err  error
	}
	results := make([]result, len(s.defi))
	var wg sync.WaitGroup
	for i, a := range s.defi {
		wg.Go(func() {
			actx, cancel := context.WithTimeout(ctx, defiTimeout)
			defer cancel()
			defer func() {
				if r := recover(); r != nil {
					results[i] = result{name: a.Name(), err: fmt.Errorf("internal error: %v", r)}
				}
			}()
			pos, err := a.Positions(actx, s.defiRPC, in)
			results[i] = result{name: a.Name(), pos: pos, err: err}
		})
	}
	wg.Wait()

	var holdings []core.Holding
	claimed := map[string]bool{}
	var warnings []string
	for _, r := range results {
		if r.err != nil {
			warnings = append(warnings, fmt.Sprintf("%s positions unavailable: %v", protocolName(r.name), s.rpc.scrub(r.err)))
		}
		for _, p := range r.pos {
			for _, m := range p.ClaimedMints {
				claimed[m] = true
			}
			for _, leg := range p.Legs {
				if leg.Amount == 0 {
					continue
				}
				holdings = append(holdings, s.positionHolding(p, leg))
			}
		}
	}
	return holdings, claimed, warnings
}

func (s *Source) positionHolding(p defi.Position, leg defi.Leg) core.Holding {
	h := core.Holding{
		WalletID: s.cfg.ID, AssetKey: core.SPLKey(leg.Mint), Mint: leg.Mint,
		Symbol: s.symbolFor(leg.Mint), Amount: leg.Amount, AmountStr: leg.AmountStr,
		Protocol: p.Protocol, Position: p.ID, PositionKind: p.Kind, Note: p.Label,
		Verified: true, // position legs are identified by the protocol, not by a token list
	}
	switch {
	case leg.Mint == WrappedSOLMint:
		h.AssetKey, h.Symbol, h.Name = "SOL", "SOL", "Solana"
	default:
		if sym, name, ok := KnownMint(leg.Mint); ok {
			h.AssetKey, h.Symbol, h.Name = sym, sym, name
		} else if m, ok := s.res.cache.Get(leg.Mint); ok {
			h.Name = m.Name
		}
	}
	if h.Symbol == "" {
		h.Symbol = shortKey(leg.Mint)
	}
	if p.Kind == defi.KindLocked {
		h.Staked = true
	}
	return h
}

// symbolFor names a mint from the known-mint table, then the metadata cache ("" if unknown;
// the defi package falls back to its own table and a shortened mint).
func (s *Source) symbolFor(mint string) string {
	if mint == WrappedSOLMint {
		return "SOL"
	}
	if sym, _, ok := KnownMint(mint); ok {
		return sym
	}
	if o, ok := s.cfg.Overrides[mint]; ok && o.Symbol != "" {
		return o.Symbol
	}
	if m, ok := s.res.cache.Get(mint); ok {
		return m.Symbol
	}
	return ""
}

func protocolName(n string) string {
	switch n {
	case defi.NameOrca:
		return "Orca"
	case defi.NameRaydium:
		return "Raydium"
	case defi.NameJupiter:
		return "Jupiter"
	}
	return n
}
