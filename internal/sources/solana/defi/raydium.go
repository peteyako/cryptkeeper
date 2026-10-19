package defi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"

	"cryptkeeper/internal/httpx"
)

// Raydium CLMM. Program and layouts:
// https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/lib.rs (declare_id!)
// https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/states/personal_position.rs
// https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/states/pool.rs
// Personal position PDA seeds ["position", nft_mint]:
// https://github.com/raydium-io/raydium-sdk-V2/blob/master/src/raydium/clmm/libraries/pda.ts
const RaydiumCLMMProgram = "CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK"

var (
	raydiumPositionDisc = anchorDiscriminator("PersonalPositionState") // 466f967ee60f1975
	raydiumPoolDisc     = anchorDiscriminator("PoolState")             // f7ede3f5d7c3de46
)

// PersonalPositionState (281 bytes): disc 8 | bump 1 @8 | nft_mint 32 @9 | pool_id 32 @41 |
// tick_lower_index i32 @73 | tick_upper_index i32 @77 | liquidity u128 @81 | …
const (
	rayPosMint      = 9
	rayPosPool      = 41
	rayPosTickLower = 73
	rayPosTickUpper = 77
	rayPosLiquidity = 81
	rayPosMinLen    = 97
)

// PoolState (1544 bytes, #[repr(C, packed)]): disc 8 | bump 1 @8 | amm_config 32 @9 |
// owner 32 @41 | token_mint_0 32 @73 | token_mint_1 32 @105 | token_vault_0/1 @137/@169 |
// observation_key 32 @201 | mint_decimals_0 u8 @233 | mint_decimals_1 u8 @234 |
// tick_spacing u16 @235 | liquidity u128 @237 | sqrt_price_x64 u128 @253 | tick_current i32 @269 | …
const (
	rayPoolMint0     = 73
	rayPoolMint1     = 105
	rayPoolDecimals0 = 233
	rayPoolDecimals1 = 234
	rayPoolSqrtPrice = 253
	rayPoolMinLen    = 273
)

// Raydium AMM v4 and CPMM program ids, for labelling LP positions.
// https://github.com/raydium-io/raydium-sdk-V2/blob/master/src/common/programId.ts
const (
	RaydiumAMMv4Program = "675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8"
	RaydiumCPMMProgram  = "CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C"
)

// RaydiumAPIBase is Raydium's public API v3 (keyless). LP pools are looked up with
// GET /pools/info/lps?lps=<mint>,<mint>,… → {"success":true,"data":[pool|null,…]}, one entry per
// requested mint (null for mints that are not Raydium LP tokens).
// https://api-v3.raydium.io/docs/
const RaydiumAPIBase = "https://api-v3.raydium.io"

// lpMintsPerCall bounds the URL length; the API accepted 150 mints in testing.
const lpMintsPerCall = 100

func decodeRaydiumPosition(key string, data []byte) (clmmPos, bool) {
	if len(data) < rayPosMinLen || !hasDiscriminator(data, raydiumPositionDisc) {
		return clmmPos{}, false
	}
	return clmmPos{
		key:       key,
		mint:      pubkeyAt(data, rayPosMint),
		pool:      pubkeyAt(data, rayPosPool),
		tickLower: i32At(data, rayPosTickLower),
		tickUpper: i32At(data, rayPosTickUpper),
		liquidity: u128At(data, rayPosLiquidity),
	}, true
}

func decodeRaydiumPool(data []byte) (clmmPoolState, bool) {
	if len(data) < rayPoolMinLen || !hasDiscriminator(data, raydiumPoolDisc) {
		return clmmPoolState{}, false
	}
	return clmmPoolState{
		sqrtPrice: u128At(data, rayPoolSqrtPrice),
		mintA:     pubkeyAt(data, rayPoolMint0),
		mintB:     pubkeyAt(data, rayPoolMint1),
		decA:      int(data[rayPoolDecimals0]),
		decB:      int(data[rayPoolDecimals1]),
	}, true
}

type raydiumAdapter struct {
	hc      *httpx.Client
	apiBase string
}

func newRaydium(c *httpx.Client) *raydiumAdapter {
	return &raydiumAdapter{hc: c, apiBase: RaydiumAPIBase}
}

func (*raydiumAdapter) Name() string { return NameRaydium }

// Positions finds Raydium CLMM positions (NFTs) and AMM v4/CPMM LP tokens. Uncollected fees and
// rewards are not included; locked ("Burn & Earn") CLMM positions are not held by the wallet
// and are not found.
func (r *raydiumAdapter) Positions(ctx context.Context, rpc RPC, in Input) ([]Position, error) {
	var errs []error
	out, err := clmmPositions(ctx, rpc, in, clmmSpec{
		protocol:       NameRaydium,
		program:        RaydiumCLMMProgram,
		tickMath:       RaydiumTickMath,
		poolHasDec:     true,
		decodePosition: decodeRaydiumPosition,
		decodePool:     decodeRaydiumPool,
	})
	if err != nil {
		errs = append(errs, err)
	}
	claimed := map[string]bool{}
	for _, p := range out {
		for _, m := range p.ClaimedMints {
			claimed[m] = true
		}
	}
	lps, err := r.lpPositions(ctx, in, claimed)
	if err != nil {
		errs = append(errs, err)
	}
	out = append(out, lps...)
	return out, errors.Join(errs...)
}

// ---- AMM v4 / CPMM LP tokens via the Raydium API ----

type raydiumMint struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

type raydiumLPPool struct {
	Type        string      `json:"type"`
	ProgramID   string      `json:"programId"`
	ID          string      `json:"id"`
	MintA       raydiumMint `json:"mintA"`
	MintB       raydiumMint `json:"mintB"`
	MintAmountA json.Number `json:"mintAmountA"` // pool reserves, in whole tokens
	MintAmountB json.Number `json:"mintAmountB"`
	LPMint      raydiumMint `json:"lpMint"`
	LPAmount    json.Number `json:"lpAmount"` // LP supply, in whole tokens
}

type raydiumLPResponse struct {
	Success bool             `json:"success"`
	Data    []*raydiumLPPool `json:"data"`
	Msg     string           `json:"msg"`
}

// lpPositions values the wallet's Raydium LP tokens: share = balance / LP supply, and each leg
// is share × pool reserve. Only fungible balances not already claimed are sent to the API.
func (r *raydiumAdapter) lpPositions(ctx context.Context, in Input, claimed map[string]bool) ([]Position, error) {
	balances := map[string]TokenBalance{}
	var mints []string
	for _, t := range in.Tokens {
		if t.Amount == nil || t.Amount.Sign() <= 0 || t.Decimals == 0 || claimed[t.Mint] {
			continue
		}
		if _, err := pubkeyBytes(t.Mint); err != nil {
			continue // one malformed mint makes the API reject the whole batch
		}
		if prev, ok := balances[t.Mint]; ok {
			prev.Amount = new(big.Int).Add(prev.Amount, t.Amount)
			balances[t.Mint] = prev
			continue
		}
		balances[t.Mint] = TokenBalance{Mint: t.Mint, Amount: new(big.Int).Set(t.Amount), Decimals: t.Decimals}
		mints = append(mints, t.Mint)
	}
	var out []Position
	for start := 0; start < len(mints); start += lpMintsPerCall {
		chunk := mints[start:min(start+lpMintsPerCall, len(mints))]
		u := r.apiBase + "/pools/info/lps?lps=" + url.QueryEscape(strings.Join(chunk, ","))
		var resp raydiumLPResponse
		if err := r.hc.GetJSON(ctx, u, nil, &resp); err != nil {
			return out, fmt.Errorf("raydium: LP pools: %w", err)
		}
		if !resp.Success {
			return out, fmt.Errorf("raydium: LP pools: API error %q", resp.Msg)
		}
		for _, pool := range resp.Data {
			if pool == nil {
				continue
			}
			bal, ok := balances[pool.LPMint.Address]
			if !ok {
				continue // matched by LP mint, never by position in the array
			}
			pos, err := lpPosition(in, pool, bal)
			if err != nil {
				return out, fmt.Errorf("raydium: LP %s: %w", pool.LPMint.Address, err)
			}
			out = append(out, pos)
			delete(balances, pool.LPMint.Address) // a mint is valued once
		}
	}
	return out, nil
}

func lpPosition(in Input, pool *raydiumLPPool, bal TokenBalance) (Position, error) {
	supply, ok := new(big.Rat).SetString(pool.LPAmount.String())
	if !ok || supply.Sign() <= 0 {
		return Position{}, fmt.Errorf("bad LP supply %q", pool.LPAmount)
	}
	resA, okA := new(big.Rat).SetString(pool.MintAmountA.String())
	resB, okB := new(big.Rat).SetString(pool.MintAmountB.String())
	if !okA || !okB || resA.Sign() < 0 || resB.Sign() < 0 {
		return Position{}, fmt.Errorf("bad reserves %q/%q", pool.MintAmountA, pool.MintAmountB)
	}
	if pool.MintA.Address == "" || pool.MintB.Address == "" || pool.MintA.Decimals < 0 || pool.MintB.Decimals < 0 ||
		pool.MintA.Decimals > 30 || pool.MintB.Decimals > 30 {
		return Position{}, errors.New("bad pool mints")
	}
	// share = (raw / 10^decimals) / supply, using the wallet's decimals for the LP mint.
	share := new(big.Rat).SetFrac(bal.Amount, pow10(bal.Decimals))
	share.Quo(share, supply)
	if share.Cmp(big.NewRat(1, 1)) > 0 {
		return Position{}, errors.New("balance exceeds LP supply")
	}
	kind := "LP"
	switch pool.ProgramID {
	case RaydiumCPMMProgram:
		kind = "CPMM LP"
	case RaydiumAMMv4Program:
		kind = "AMM LP"
	}
	id := pool.ID
	if id == "" {
		id = pool.LPMint.Address
	}
	return Position{
		Protocol: NameRaydium,
		ID:       id,
		Kind:     KindLP,
		Label:    in.symbol(pool.MintA.Address, pool.MintA.Symbol) + "/" + in.symbol(pool.MintB.Address, pool.MintB.Symbol) + " · " + kind,
		Legs: []Leg{
			legFromRat(pool.MintA.Address, new(big.Rat).Mul(share, resA), pool.MintA.Decimals),
			legFromRat(pool.MintB.Address, new(big.Rat).Mul(share, resB), pool.MintB.Decimals),
		},
		ClaimedMints: []string{bal.Mint},
	}, nil
}
