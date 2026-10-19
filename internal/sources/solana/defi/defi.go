// Package defi finds a Solana wallet's DeFi positions and reports their underlying assets:
// Orca Whirlpool and Raydium CLMM concentrated-liquidity positions, Raydium AMM v4/CPMM LP
// tokens, Jupiter locked/staked JUP and Jupiter perps.
//
// Every program id, account layout, PDA seed and HTTP endpoint used here is pinned as a constant
// next to the code that uses it, with a link to its source. docs/defi-sources.md has the full
// write-up, including how each layout was confirmed against live mainnet accounts.
//
// The package deliberately does not import the solana package (which imports this one): the
// caller adapts its RPC client to the small RPC interface below.
package defi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"cryptkeeper/internal/httpx"

	"filippo.io/edwards25519"
	"github.com/btcsuite/btcd/btcutil/base58"
)

// Position kinds, matching core.Holding.PositionKind.
const (
	KindLP     = "lp"
	KindPerp   = "perp"
	KindLocked = "locked"
	KindLimit  = "limit" // a Jupiter limit-order vault (read by the solana package, not an adapter)
)

// TokenBalance is one of the wallet's token holdings, NFTs included (amount 1, decimals 0).
type TokenBalance struct {
	Mint      string
	Amount    *big.Int // raw integer amount (before decimals)
	Decimals  int
	Token2022 bool
}

// Input is what an adapter needs to know about the wallet.
type Input struct {
	Owner  string
	Tokens []TokenBalance
	// Symbol optionally names a mint for position labels ("SOL/USDC · in range"). It returns ""
	// for an unknown mint. When nil or "", a small built-in table is used, then a short mint.
	Symbol func(mint string) string
}

// Position is one DeFi position and the assets underneath it.
type Position struct {
	Protocol string // "orca", "raydium", "jupiter"
	ID       string // position account (or pool id for AMM LP tokens)
	Kind     string // KindLP, KindPerp, KindLocked
	Label    string // e.g. "SOL/USDC · in range", "Locked JUP · unlocks Mar 3, 2027"
	InRange  *bool  // concentrated-liquidity positions only
	Legs     []Leg  // underlying assets; a leg may be zero (e.g. a CLMM position out of range)
	// ClaimedMints are the position NFT / LP token mints this position accounts for. The caller
	// removes them from the plain token list so they are not counted twice.
	ClaimedMints []string
}

// Leg is one underlying asset of a position.
type Leg struct {
	Mint      string
	Amount    float64
	AmountStr string // exact decimal string (trailing zeros trimmed)
	Decimals  int
	// USDValue is set only where the value is not "amount × price of Mint": perps report
	// collateral + unrealized PnL in USD.
	USDValue *float64
}

// Adapter finds one protocol's positions for a wallet.
//
// Positions may return some positions together with a non-nil error when only part of the
// lookup failed (e.g. Raydium CLMM positions were found but the LP-token API was down). The
// caller should keep the positions and surface the error as a warning. Adapters never panic on
// malformed account data or API responses: such accounts are skipped or reported as errors.
type Adapter interface {
	Name() string
	Positions(ctx context.Context, rpc RPC, in Input) ([]Position, error)
}

// RPC is the subset of Solana JSON-RPC the adapters use.
type RPC interface {
	// GetMultipleAccounts returns the raw (base64-decoded) data of each account, nil for a
	// missing account, in the order of pubkeys. Callers pass at most 100 keys per call.
	GetMultipleAccounts(ctx context.Context, pubkeys []string) ([][]byte, error)
	GetProgramAccounts(ctx context.Context, program string, filters []Filter) ([]KeyedAccount, error)
}

// Filter is a getProgramAccounts filter: a dataSize filter when DataSize > 0, otherwise a
// memcmp of the base58 Bytes at Offset.
type Filter struct {
	Offset   int
	Bytes    string // base58
	DataSize int
}

// KeyedAccount is one getProgramAccounts result.
type KeyedAccount struct {
	Pubkey string
	Data   []byte
}

// Adapter names accepted by Adapters.
const (
	NameJupiter = "jupiter"
	NameOrca    = "orca"
	NameRaydium = "raydium"
)

// AllAdapterNames lists every adapter, in the order Adapters returns them by default.
var AllAdapterNames = []string{NameJupiter, NameOrca, NameRaydium}

// Adapters returns the adapters for names ("jupiter", "orca", "raydium"), in that order of
// appearance, skipping unknown names and duplicates. An empty names list means all adapters.
// c is used for the HTTP APIs (Raydium LP pools, Jupiter perps/prices).
func Adapters(names []string, c *httpx.Client) []Adapter {
	if len(names) == 0 {
		names = AllAdapterNames
	}
	if c == nil {
		c = httpx.New()
	}
	seen := map[string]bool{}
	var out []Adapter
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if seen[n] {
			continue
		}
		seen[n] = true
		switch n {
		case NameJupiter:
			out = append(out, newJupiter(c))
		case NameOrca:
			out = append(out, newOrca())
		case NameRaydium:
			out = append(out, newRaydium(c))
		}
	}
	return out
}

// ---- Well-known mints ----

const (
	wsolMint = "So11111111111111111111111111111111111111112"
	usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdtMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	jupMint  = "JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN"
)

// builtinSymbols names the mints most often found in pools, for labels only (the caller's
// Input.Symbol takes precedence). Mints checked against the Jupiter token API.
var builtinSymbols = map[string]string{
	wsolMint: "SOL",
	usdcMint: "USDC",
	usdtMint: "USDT",
	jupMint:  "JUP",
	"4k3Dyjzvzp8eMZWUXbBCjEvwSkkk59S5iCNLY3QrkX6R": "RAY",
	"orcaEKTdK7LKz57vaAYr9QeNsVEPfiu6QeMU1kektZE":  "ORCA",
	"DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263": "BONK",
	"jtojtomepa8beP8AuQc6eXt5FriJwfFMwQx2v2f9mCL":  "JTO",
	"mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So":  "mSOL",
	"J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn": "JitoSOL",
	"3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh": "WBTC",
	"7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs": "WETH",
}

// symbol names a mint for a label: the caller's resolver, then the built-in table, then the
// API-provided fallback, then a shortened mint.
func (in Input) symbol(mint, fallback string) string {
	if in.Symbol != nil {
		if s := in.Symbol(mint); s != "" {
			return s
		}
	}
	if s, ok := builtinSymbols[mint]; ok {
		return s
	}
	if fallback != "" {
		if fallback == "WSOL" {
			return "SOL"
		}
		return fallback
	}
	return shortMint(mint)
}

func shortMint(m string) string {
	if len(m) <= 10 {
		return m
	}
	return m[:4] + "…" + m[len(m)-4:]
}

// ---- Account helpers ----

// anchorDiscriminator is the first 8 bytes of an Anchor account: sha256("account:<Name>")[:8].
// https://github.com/coral-xyz/anchor/blob/master/lang/syn/src/codegen/program/common.rs
func anchorDiscriminator(name string) [8]byte {
	sum := sha256.Sum256([]byte("account:" + name))
	var d [8]byte
	copy(d[:], sum[:8])
	return d
}

func hasDiscriminator(data []byte, d [8]byte) bool {
	return len(data) >= 8 && [8]byte(data[:8]) == d
}

// pubkeyAt returns the base58 public key at data[off:off+32]. Callers check the length first.
func pubkeyAt(data []byte, off int) string { return base58.Encode(data[off : off+32]) }

func u64At(data []byte, off int) uint64 {
	b := data[off : off+8]
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func i64At(data []byte, off int) int64 { return int64(u64At(data, off)) }

func i32At(data []byte, off int) int32 {
	b := data[off : off+4]
	return int32(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
}

// u128At reads a little-endian u128.
func u128At(data []byte, off int) *big.Int {
	be := make([]byte, 16)
	for i := range 16 {
		be[15-i] = data[off+i]
	}
	return new(big.Int).SetBytes(be)
}

// SPL Token mint layout (Token and Token-2022 share the first 82 bytes):
// mint_authority COption<Pubkey> (36) | supply u64 (8) | decimals u8 @44 | is_initialized @45 | …
// https://github.com/solana-program/token/blob/main/interface/src/state.rs
const (
	mintLen            = 82
	mintDecimalsOffset = 44
	mintInitOffset     = 45
)

// mintDecimals decodes the decimals of an SPL (or Token-2022) mint account.
func mintDecimals(data []byte) (int, bool) {
	if len(data) < mintLen || data[mintInitOffset] != 1 {
		return 0, false
	}
	return int(data[mintDecimalsOffset]), true
}

// maxAccountsPerCall is the getMultipleAccounts limit of Solana RPC nodes.
const maxAccountsPerCall = 100

// fetchAccounts fetches the given accounts (deduplicated, in chunks of 100) and returns them by
// pubkey; missing accounts are absent from the map.
func fetchAccounts(ctx context.Context, rpc RPC, keys []string) (map[string][]byte, error) {
	uniq := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k != "" && !seen[k] {
			seen[k] = true
			uniq = append(uniq, k)
		}
	}
	out := make(map[string][]byte, len(uniq))
	for start := 0; start < len(uniq); start += maxAccountsPerCall {
		chunk := uniq[start:min(start+maxAccountsPerCall, len(uniq))]
		datas, err := rpc.GetMultipleAccounts(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if len(datas) != len(chunk) {
			return nil, fmt.Errorf("getMultipleAccounts returned %d accounts for %d keys", len(datas), len(chunk))
		}
		for i, d := range datas {
			if d != nil {
				out[chunk[i]] = d
			}
		}
	}
	return out, nil
}

// isNFTCandidate reports whether a balance looks like a position NFT: exactly 1 unit, 0 decimals.
func isNFTCandidate(t TokenBalance) bool {
	return t.Decimals == 0 && t.Amount != nil && t.Amount.Cmp(big.NewInt(1)) == 0
}

// nftMints returns the distinct mints of the wallet's NFT-like balances.
func nftMints(tokens []TokenBalance) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tokens {
		if isNFTCandidate(t) && !seen[t.Mint] {
			seen[t.Mint] = true
			out = append(out, t.Mint)
		}
	}
	return out
}

// ---- PDA derivation ----

// findProgramAddress derives a program derived address like
// solana_program::Pubkey::find_program_address: sha256(seeds || bump || programID ||
// "ProgramDerivedAddress") for bump 255 down to 1, returning the first hash that is NOT a valid
// ed25519 point. (A copy of solana.FindProgramAddress, kept here to avoid an import cycle.)
func findProgramAddress(seeds [][]byte, programID string) (string, error) {
	pid := base58.Decode(programID)
	if len(pid) != 32 {
		return "", fmt.Errorf("program id %q is not a 32-byte public key", programID)
	}
	if len(seeds) > 15 {
		return "", errors.New("too many seeds")
	}
	for _, s := range seeds {
		if len(s) > 32 {
			return "", errors.New("seed longer than 32 bytes")
		}
	}
	for b := 255; b >= 1; b-- {
		h := sha256.New()
		for _, s := range seeds {
			h.Write(s)
		}
		h.Write([]byte{byte(b)})
		h.Write(pid)
		h.Write([]byte("ProgramDerivedAddress"))
		sum := h.Sum(nil)
		if _, err := new(edwards25519.Point).SetBytes(sum); err != nil {
			return base58.Encode(sum), nil
		}
	}
	return "", errors.New("no viable bump seed")
}

// pubkeyBytes decodes a base58 public key, which must be 32 bytes.
func pubkeyBytes(s string) ([]byte, error) {
	b := base58.Decode(s)
	if len(b) != 32 {
		return nil, fmt.Errorf("%q is not a 32-byte public key", s)
	}
	return b, nil
}

// ---- Amount formatting ----

// formatUnits renders amount / 10^decimals as an exact decimal string with trailing zeros
// trimmed ("3.75", "42", "0", "-1.5").
func formatUnits(amount *big.Int, decimals int) string {
	if amount == nil {
		return "0"
	}
	neg := amount.Sign() < 0
	s := new(big.Int).Abs(amount).String()
	if decimals > 0 {
		if len(s) <= decimals {
			s = strings.Repeat("0", decimals-len(s)+1) + s
		}
		s = s[:len(s)-decimals] + "." + s[len(s)-decimals:]
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if neg && s != "0" {
		s = "-" + s
	}
	return s
}

// legFromRaw builds a leg from a raw integer amount.
func legFromRaw(mint string, raw *big.Int, decimals int) Leg {
	str := formatUnits(raw, decimals)
	f, _, _ := big.ParseFloat(str, 10, 64, big.ToNearestEven)
	amt, _ := f.Float64()
	return Leg{Mint: mint, Amount: amt, AmountStr: str, Decimals: decimals}
}

// legFromRat builds a leg from an exact rational amount (in whole tokens), truncated to the
// token's decimals.
func legFromRat(mint string, amount *big.Rat, decimals int) Leg {
	scaled := new(big.Rat).Mul(amount, new(big.Rat).SetInt(pow10(decimals)))
	raw := new(big.Int).Quo(scaled.Num(), scaled.Denom()) // truncates toward zero
	return legFromRaw(mint, raw, decimals)
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

func boolPtr(b bool) *bool { return &b }

// rangeLabel is the pair label of a concentrated-liquidity position.
func rangeLabel(symA, symB string, inRange bool) string {
	if inRange {
		return symA + "/" + symB + " · in range"
	}
	return symA + "/" + symB + " · out of range"
}

// clmmPosition assembles the Position for a decoded concentrated-liquidity position.
func clmmPosition(protocol, id, positionMint string, in Input, mintA, mintB string, decA, decB int,
	liquidity, sqrtPrice *big.Int, tickLower, tickUpper int32, tm TickMath) (Position, error) {
	if tickLower >= tickUpper {
		return Position{}, fmt.Errorf("position %s: invalid tick range [%d, %d)", id, tickLower, tickUpper)
	}
	sl, err := tm.SqrtPriceAtTick(tickLower)
	if err != nil {
		return Position{}, fmt.Errorf("position %s: %w", id, err)
	}
	su, err := tm.SqrtPriceAtTick(tickUpper)
	if err != nil {
		return Position{}, fmt.Errorf("position %s: %w", id, err)
	}
	a, b, status := AmountsFromLiquidity(liquidity, sqrtPrice, sl, su)
	inRange := status == InRange
	return Position{
		Protocol:     protocol,
		ID:           id,
		Kind:         KindLP,
		Label:        rangeLabel(in.symbol(mintA, ""), in.symbol(mintB, ""), inRange),
		InRange:      boolPtr(inRange),
		Legs:         []Leg{legFromRaw(mintA, a, decA), legFromRaw(mintB, b, decB)},
		ClaimedMints: []string{positionMint},
	}, nil
}
