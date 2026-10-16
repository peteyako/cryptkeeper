package defi

import (
	"context"
	"fmt"
	"math/big"
)

// Orca Whirlpools. Program and layouts:
// https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/lib.rs (declare_id!)
// https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/state/position.rs
// https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/state/whirlpool.rs
// Position PDA seeds ["position", position_mint]:
// https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/instructions/open_position.rs
const OrcaWhirlpoolProgram = "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc"

var (
	orcaPositionDisc  = anchorDiscriminator("Position")  // aabc8fe47a40f7d0
	orcaWhirlpoolDisc = anchorDiscriminator("Whirlpool") // 3f95d10ce1806309
)

// Position (216 bytes): disc 8 | whirlpool 32 @8 | position_mint 32 @40 | liquidity u128 @72 |
// tick_lower_index i32 @88 | tick_upper_index i32 @92 | fee/reward checkpoints …
const (
	orcaPosWhirlpool = 8
	orcaPosMint      = 40
	orcaPosLiquidity = 72
	orcaPosTickLower = 88
	orcaPosTickUpper = 92
	orcaPosMinLen    = 96
)

// Whirlpool (653 bytes): disc 8 | whirlpools_config 32 @8 | bump 1 @40 | tick_spacing u16 @41 |
// fee_tier_index_seed 2 @43 | fee_rate u16 @45 | protocol_fee_rate u16 @47 | liquidity u128 @49 |
// sqrt_price u128 @65 | tick_current_index i32 @81 | protocol_fee_owed_a/b u64 @85/@93 |
// token_mint_a 32 @101 | token_vault_a 32 @133 | fee_growth_global_a u128 @165 |
// token_mint_b 32 @181 | …
const (
	orcaPoolSqrtPrice = 65
	orcaPoolMintA     = 101
	orcaPoolMintB     = 181
	orcaPoolMinLen    = 213
)

type orcaPosition struct {
	key, whirlpool, mint string
	liquidity            *big.Int
	tickLower, tickUpper int32
}

type orcaPool struct {
	sqrtPrice    *big.Int
	mintA, mintB string
}

func decodeOrcaPosition(key string, data []byte) (orcaPosition, bool) {
	if len(data) < orcaPosMinLen || !hasDiscriminator(data, orcaPositionDisc) {
		return orcaPosition{}, false
	}
	return orcaPosition{
		key:       key,
		whirlpool: pubkeyAt(data, orcaPosWhirlpool),
		mint:      pubkeyAt(data, orcaPosMint),
		liquidity: u128At(data, orcaPosLiquidity),
		tickLower: i32At(data, orcaPosTickLower),
		tickUpper: i32At(data, orcaPosTickUpper),
	}, true
}

func decodeOrcaPool(data []byte) (orcaPool, bool) {
	if len(data) < orcaPoolMinLen || !hasDiscriminator(data, orcaWhirlpoolDisc) {
		return orcaPool{}, false
	}
	return orcaPool{
		sqrtPrice: u128At(data, orcaPoolSqrtPrice),
		mintA:     pubkeyAt(data, orcaPoolMintA),
		mintB:     pubkeyAt(data, orcaPoolMintB),
	}, true
}

type orcaAdapter struct{}

func newOrca() *orcaAdapter { return &orcaAdapter{} }

func (*orcaAdapter) Name() string { return NameOrca }

// Positions finds Whirlpool positions held as NFTs. Uncollected fees and rewards are not
// included, and position bundles are not supported.
func (*orcaAdapter) Positions(ctx context.Context, rpc RPC, in Input) ([]Position, error) {
	return clmmPositions(ctx, rpc, in, clmmSpec{
		protocol:   NameOrca,
		program:    OrcaWhirlpoolProgram,
		tickMath:   OrcaTickMath,
		poolHasDec: false,
		decodePosition: func(key string, data []byte) (clmmPos, bool) {
			p, ok := decodeOrcaPosition(key, data)
			return clmmPos{key: p.key, pool: p.whirlpool, mint: p.mint, liquidity: p.liquidity, tickLower: p.tickLower, tickUpper: p.tickUpper}, ok
		},
		decodePool: func(data []byte) (clmmPoolState, bool) {
			p, ok := decodeOrcaPool(data)
			return clmmPoolState{sqrtPrice: p.sqrtPrice, mintA: p.mintA, mintB: p.mintB}, ok
		},
	})
}

// ---- Shared Orca/Raydium CLMM flow ----

type clmmPos struct {
	key, pool, mint      string
	liquidity            *big.Int
	tickLower, tickUpper int32
}

type clmmPoolState struct {
	sqrtPrice    *big.Int
	mintA, mintB string
	decA, decB   int // only when the pool account stores decimals (Raydium)
}

type clmmSpec struct {
	protocol       string
	program        string
	tickMath       TickMath
	poolHasDec     bool
	decodePosition func(key string, data []byte) (clmmPos, bool)
	decodePool     func(data []byte) (clmmPoolState, bool)
}

// clmmPositions: wallet NFTs → position PDAs ["position", mint] → batch fetch → decode positions
// → batch fetch pools (and mints for decimals) → amounts.
func clmmPositions(ctx context.Context, rpc RPC, in Input, s clmmSpec) ([]Position, error) {
	mints := nftMints(in.Tokens)
	if len(mints) == 0 {
		return nil, nil
	}
	pdaToMint := make(map[string]string, len(mints))
	keys := make([]string, 0, len(mints))
	for _, m := range mints {
		mb, err := pubkeyBytes(m)
		if err != nil {
			continue // not a valid mint; can't be a position
		}
		pda, err := findProgramAddress([][]byte{[]byte("position"), mb}, s.program)
		if err != nil {
			continue
		}
		pdaToMint[pda] = m
		keys = append(keys, pda)
	}
	accts, err := fetchAccounts(ctx, rpc, keys)
	if err != nil {
		return nil, fmt.Errorf("%s: fetch positions: %w", s.protocol, err)
	}
	var positions []clmmPos
	var poolKeys []string
	for _, k := range keys {
		data, ok := accts[k]
		if !ok {
			continue
		}
		p, ok := s.decodePosition(k, data)
		if !ok || p.mint != pdaToMint[k] {
			continue // not a position account of this program (or not this NFT's position)
		}
		positions = append(positions, p)
		poolKeys = append(poolKeys, p.pool)
	}
	if len(positions) == 0 {
		return nil, nil
	}
	poolAccts, err := fetchAccounts(ctx, rpc, poolKeys)
	if err != nil {
		return nil, fmt.Errorf("%s: fetch pools: %w", s.protocol, err)
	}
	pools := map[string]clmmPoolState{}
	var mintKeys []string
	for key, data := range poolAccts {
		p, ok := s.decodePool(data)
		if !ok {
			continue
		}
		pools[key] = p
		mintKeys = append(mintKeys, p.mintA, p.mintB)
	}
	decimals := map[string]int{}
	if !s.poolHasDec && len(mintKeys) > 0 {
		mintAccts, err := fetchAccounts(ctx, rpc, mintKeys)
		if err != nil {
			return nil, fmt.Errorf("%s: fetch mints: %w", s.protocol, err)
		}
		for k, data := range mintAccts {
			if d, ok := mintDecimals(data); ok {
				decimals[k] = d
			}
		}
	}
	var out []Position
	var problems []string
	for _, p := range positions {
		pool, ok := pools[p.pool]
		if !ok {
			problems = append(problems, fmt.Sprintf("pool %s of position %s missing or undecodable", p.pool, p.key))
			continue
		}
		decA, decB := pool.decA, pool.decB
		if !s.poolHasDec {
			var okA, okB bool
			decA, okA = decimals[pool.mintA]
			decB, okB = decimals[pool.mintB]
			if !okA || !okB {
				problems = append(problems, fmt.Sprintf("mint decimals for pool %s unavailable", p.pool))
				continue
			}
		}
		pos, err := clmmPosition(s.protocol, p.key, p.mint, in, pool.mintA, pool.mintB, decA, decB,
			p.liquidity, pool.sqrtPrice, p.tickLower, p.tickUpper, s.tickMath)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		out = append(out, pos)
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("%s: %d position(s) skipped: %s", s.protocol, len(problems), problems[0])
	}
	return out, nil
}
