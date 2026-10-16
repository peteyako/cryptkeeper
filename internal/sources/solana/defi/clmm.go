package defi

import (
	"fmt"
	"math/big"
)

// Concentrated-liquidity math (Uniswap v3 style) in Q64.64 fixed point, as used by Orca
// Whirlpools and Raydium CLMM. All arithmetic is exact (math/big) and rounds down, matching
// what each protocol's SDK quotes for withdrawing a position's liquidity.

// Tick bounds shared by Orca and Raydium (1.0001^±443636 ≈ 2^±64).
const (
	MinTick = -443636
	MaxTick = 443636
)

// TickMath converts a tick index to its Q64.64 sqrt price. Orca and Raydium use different
// magic constants, so their results differ in the low bits; each protocol's positions must use
// its own implementation to reproduce on-chain values exactly.
type TickMath interface {
	SqrtPriceAtTick(tick int32) (*big.Int, error)
}

var (
	// OrcaTickMath ports sqrt_price_from_tick_index from the Whirlpool program:
	// https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/math/tick_math.rs
	OrcaTickMath TickMath = orcaTickMath{}
	// RaydiumTickMath ports get_sqrt_price_at_tick from the Raydium CLMM program:
	// https://github.com/raydium-io/raydium-clmm/blob/master/programs/amm/src/libraries/tick_math.rs
	RaydiumTickMath TickMath = raydiumTickMath{}
)

func mustBig(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 0)
	if !ok {
		panic("defi: bad constant " + s)
	}
	return n
}

var q64 = new(big.Int).Lsh(big.NewInt(1), 64)

func checkTick(tick int32) error {
	if tick < MinTick || tick > MaxTick {
		return fmt.Errorf("tick %d out of range [%d, %d]", tick, MinTick, MaxTick)
	}
	return nil
}

// ---- Orca ----

type orcaTickMath struct{}

// Orca positive ticks: Q32.96 factors 2^96·√1.0001^(2^i), combined with mul_shift_96, >> 32.
var orcaPositive = [...]*big.Int{
	mustBig("79232123823359799118286999567"), // bit 0 (initial value when set)
	mustBig("79236085330515764027303304731"),
	mustBig("79244008939048815603706035061"),
	mustBig("79259858533276714757314932305"),
	mustBig("79291567232598584799939703904"),
	mustBig("79355022692464371645785046466"),
	mustBig("79482085999252804386437311141"),
	mustBig("79736823300114093921829183326"),
	mustBig("80248749790819932309965073892"),
	mustBig("81282483887344747381513967011"),
	mustBig("83390072131320151908154831281"),
	mustBig("87770609709833776024991924138"),
	mustBig("97234110755111693312479820773"),
	mustBig("119332217159966728226237229890"),
	mustBig("179736315981702064433883588727"),
	mustBig("407748233172238350107850275304"),
	mustBig("2098478828474011932436660412517"),
	mustBig("55581415166113811149459800483533"),
	mustBig("38992368544603139932233054999993551"),
}

var orcaPositiveOne = mustBig("79228162514264337593543950336") // 2^96

// Orca negative ticks: Q64.64 factors 2^64/√1.0001^(2^i), combined with (ratio·f) >> 64.
var orcaNegative = [...]*big.Int{
	mustBig("18445821805675392311"), // bit 0 (initial value when set)
	mustBig("18444899583751176498"),
	mustBig("18443055278223354162"),
	mustBig("18439367220385604838"),
	mustBig("18431993317065449817"),
	mustBig("18417254355718160513"),
	mustBig("18387811781193591352"),
	mustBig("18329067761203520168"),
	mustBig("18212142134806087854"),
	mustBig("17980523815641551639"),
	mustBig("17526086738831147013"),
	mustBig("16651378430235024244"),
	mustBig("15030750278693429944"),
	mustBig("12247334978882834399"),
	mustBig("8131365268884726200"),
	mustBig("3584323654723342297"),
	mustBig("696457651847595233"),
	mustBig("26294789957452057"),
	mustBig("37481735321082"),
}

func (orcaTickMath) SqrtPriceAtTick(tick int32) (*big.Int, error) {
	if err := checkTick(tick); err != nil {
		return nil, err
	}
	if tick >= 0 {
		ratio := new(big.Int).Set(orcaPositiveOne)
		if tick&1 != 0 {
			ratio.Set(orcaPositive[0])
		}
		for i := 1; i < len(orcaPositive); i++ {
			if tick&(1<<i) != 0 {
				ratio.Mul(ratio, orcaPositive[i])
				ratio.Rsh(ratio, 96)
			}
		}
		return ratio.Rsh(ratio, 32), nil
	}
	abs := -tick
	ratio := new(big.Int).Set(q64)
	if abs&1 != 0 {
		ratio.Set(orcaNegative[0])
	}
	for i := 1; i < len(orcaNegative); i++ {
		if abs&(1<<i) != 0 {
			ratio.Mul(ratio, orcaNegative[i])
			ratio.Rsh(ratio, 64)
		}
	}
	return ratio, nil
}

// ---- Raydium ----

type raydiumTickMath struct{}

// Raydium factors: 2^64/√1.0001^(2^i) in Q64.64 (with truncated low bits), as hex.
var raydiumFactors = [...]*big.Int{
	mustBig("0xfffcb933bd6fb800"), // bit 0 (initial value when set)
	mustBig("0xfff97272373d4000"),
	mustBig("0xfff2e50f5f657000"),
	mustBig("0xffe5caca7e10f000"),
	mustBig("0xffcb9843d60f7000"),
	mustBig("0xff973b41fa98e800"),
	mustBig("0xff2ea16466c9b000"),
	mustBig("0xfe5dee046a9a3800"),
	mustBig("0xfcbe86c7900bb000"),
	mustBig("0xf987a7253ac65800"),
	mustBig("0xf3392b0822bb6000"),
	mustBig("0xe7159475a2caf000"),
	mustBig("0xd097f3bdfd2f2000"),
	mustBig("0xa9f746462d9f8000"),
	mustBig("0x70d869a156f31c00"),
	mustBig("0x31be135f97ed3200"),
	mustBig("0x9aa508b5b85a500"),
	mustBig("0x5d6af8dedc582c"),
	mustBig("0x2216e584f5fa"),
}

var u128Max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

func (raydiumTickMath) SqrtPriceAtTick(tick int32) (*big.Int, error) {
	if err := checkTick(tick); err != nil {
		return nil, err
	}
	abs := tick
	if abs < 0 {
		abs = -abs
	}
	ratio := new(big.Int).Set(q64)
	if abs&1 != 0 {
		ratio.Set(raydiumFactors[0])
	}
	for i := 1; i < len(raydiumFactors); i++ {
		if abs&(1<<i) != 0 {
			ratio.Mul(ratio, raydiumFactors[i])
			ratio.Rsh(ratio, 64)
		}
	}
	if tick > 0 {
		ratio.Quo(u128Max, ratio)
	}
	return ratio, nil
}

// ---- Amounts ----

// RangeStatus says where the pool price is relative to a position's range.
type RangeStatus int

const (
	BelowRange RangeStatus = iota // price ≤ lower bound: the position is all token A
	InRange                       // lower < price < upper: both tokens
	AboveRange                    // price ≥ upper bound: the position is all token B
)

func (s RangeStatus) String() string {
	switch s {
	case BelowRange:
		return "below range"
	case InRange:
		return "in range"
	default:
		return "above range"
	}
}

// AmountsFromLiquidity returns the token A and B amounts (raw integer units, rounded down) of
// liquidity L between sqrtLower and sqrtUpper at the current sqrt price (all Q64.64):
//
//	below range: A = L·(√Pu−√Pl)/(√Pl·√Pu), B = 0
//	in range:    A = L·(√Pu−√Pc)/(√Pc·√Pu), B = L·(√Pc−√Pl)
//	above range: A = 0,                     B = L·(√Pu−√Pl)
//
// The range test compares sqrt prices (not ticks) like the Orca and Raydium SDKs:
// √Pc ≤ √Pl is below, √Pc ≥ √Pu is above.
func AmountsFromLiquidity(liquidity, sqrtCurrent, sqrtLower, sqrtUpper *big.Int) (a, b *big.Int, status RangeStatus) {
	if sqrtLower.Cmp(sqrtUpper) > 0 {
		sqrtLower, sqrtUpper = sqrtUpper, sqrtLower
	}
	switch {
	case sqrtCurrent.Cmp(sqrtLower) <= 0:
		return deltaA(liquidity, sqrtLower, sqrtUpper), new(big.Int), BelowRange
	case sqrtCurrent.Cmp(sqrtUpper) < 0:
		return deltaA(liquidity, sqrtCurrent, sqrtUpper), deltaB(liquidity, sqrtLower, sqrtCurrent), InRange
	default:
		return new(big.Int), deltaB(liquidity, sqrtLower, sqrtUpper), AboveRange
	}
}

// deltaA = floor((L << 64)·(√Pu−√Pl) / (√Pl·√Pu)) — Orca get_amount_delta_a /
// Raydium get_delta_amount_0_unsigned with round_up = false.
func deltaA(l, lower, upper *big.Int) *big.Int {
	if lower.Sign() <= 0 || l.Sign() <= 0 {
		return new(big.Int)
	}
	num := new(big.Int).Lsh(l, 64)
	num.Mul(num, new(big.Int).Sub(upper, lower))
	den := new(big.Int).Mul(lower, upper)
	return num.Quo(num, den)
}

// deltaB = floor(L·(√Pu−√Pl) / 2^64) — Orca get_amount_delta_b /
// Raydium get_delta_amount_1_unsigned with round_up = false.
func deltaB(l, lower, upper *big.Int) *big.Int {
	if l.Sign() <= 0 {
		return new(big.Int)
	}
	n := new(big.Int).Mul(l, new(big.Int).Sub(upper, lower))
	return n.Rsh(n, 64)
}
