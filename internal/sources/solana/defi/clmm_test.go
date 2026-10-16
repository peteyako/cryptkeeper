package defi

import (
	"math/big"
	"testing"
)

// Orca test_exact_bit_values: (tick, sqrt(tick), sqrt(-tick)) for every bit of the tick.
// https://github.com/orca-so/whirlpools/blob/main/programs/whirlpool/src/math/tick_math.rs
var orcaBitVectors = []struct {
	tick     int32
	pos, neg string
}{
	{0, "18446744073709551616", "18446744073709551616"},
	{1, "18447666387855959850", "18445821805675392311"},
	{2, "18448588748116922571", "18444899583751176498"},
	{4, "18450433606991734263", "18443055278223354162"},
	{8, "18454123878217468680", "18439367220385604838"},
	{16, "18461506635090006701", "18431993317065449817"},
	{32, "18476281010653910144", "18417254355718160513"},
	{64, "18505865242158250041", "18387811781193591352"},
	{128, "18565175891880433522", "18329067761203520168"},
	{256, "18684368066214940582", "18212142134806087854"},
	{512, "18925053041275764671", "17980523815641551639"},
	{1024, "19415764168677886926", "17526086738831147013"},
	{2048, "20435687552633177494", "16651378430235024244"},
	{4096, "22639080592224303007", "15030750278693429944"},
	{8192, "27784196929998399742", "12247334978882834399"},
	{16384, "41848122137994986128", "8131365268884726200"},
	{32768, "94936283578220370716", "3584323654723342297"},
	{65536, "488590176327622479860", "696457651847595233"},
	{131072, "12941056668319229769860", "26294789957452057"},
	{262144, "9078618265828848800676189", "37481735321082"},
}

func TestOrcaSqrtPriceAtTickBitVectors(t *testing.T) {
	for _, v := range orcaBitVectors {
		got, err := OrcaTickMath.SqrtPriceAtTick(v.tick)
		if err != nil || got.String() != v.pos {
			t.Errorf("orca sqrt(%d) = %v, %v; want %s", v.tick, got, err, v.pos)
		}
		got, err = OrcaTickMath.SqrtPriceAtTick(-v.tick)
		if err != nil || got.String() != v.neg {
			t.Errorf("orca sqrt(%d) = %v, %v; want %s", -v.tick, got, err, v.neg)
		}
	}
}

// Extra Orca ticks computed with the official @orca-so/whirlpools-core 3.1.1 tickIndexToSqrtPrice.
func TestOrcaSqrtPriceAtTickSDK(t *testing.T) {
	cases := map[int32]string{
		MinTick: "4295048016",                    // MIN_SQRT_PRICE_X64
		MaxTick: "79226673515401279992447579055", // MAX_SQRT_PRICE_X64
		-22710:  "5926564205805866318",
		22710:   "57416465105969178972",
		100000:  "2737055259406582257880",
		-100000: "124324258982887573",
		12345:   "34195943348800206620",
		-12345:  "9950957148631419635",
	}
	for tick, want := range cases {
		got, err := OrcaTickMath.SqrtPriceAtTick(tick)
		if err != nil || got.String() != want {
			t.Errorf("orca sqrt(%d) = %v, %v; want %s", tick, got, err, want)
		}
	}
}

// Raydium: MIN/MAX from the program's tick_math tests, the rest computed with the official
// @raydium-io/raydium-sdk-v2 0.2.74-alpha TickUtil.getSqrtPriceAtTick (same algorithm; for
// tick > 0 the SDK's 2^128/ratio equals the program's u128::MAX/ratio since ratio is never a
// power of two there).
func TestRaydiumSqrtPriceAtTick(t *testing.T) {
	cases := []struct {
		tick     int32
		pos, neg string
	}{
		{443636, "79226673521066979257578248091", "4295048016"},
		{22712, "57422206752458937802", "5925971608647152312"},
		{1, "18447666387855957090", "18445821805675395072"},
		{2, "18448588748116922877", "18444899583751176192"},
		{4, "18450433606991732521", "18443055278223355904"},
		{8, "18454123878217465836", "18439367220385607680"},
		{16, "18461506635090002945", "18431993317065453568"},
		{32, "18476281010653900001", "18417254355718170624"},
		{64, "18505865242158232063", "18387811781193609216"},
		{128, "18565175891880394798", "18329067761203558400"},
		{256, "18684368066214863021", "18212142134806163456"},
		{512, "18925053041275608146", "17980523815641700352"},
		{1024, "19415764168677569298", "17526086738831433728"},
		{2048, "20435687552632507490", "16651378430235570176"},
		{4096, "22639080592222823416", "15030750278694412288"},
		{8192, "27784196929994766438", "12247334978884435968"},
		{12345, "34195943348793463849", "9950957148633381772"},
		{16384, "41848122137984032019", "8131365268886854656"},
		{32768, "94936283578170668259", "3584323654725218816"},
		{65536, "488590176327110977113", "696457651848324352"},
		{100000, "2737055259402209284734", "124324258983086206"},
		{131072, "12941056668292132521558", "26294789957507116"},
		{262144, "9078618265828877810339005", "37481735321082"},
	}
	for _, c := range cases {
		got, err := RaydiumTickMath.SqrtPriceAtTick(c.tick)
		if err != nil || got.String() != c.pos {
			t.Errorf("raydium sqrt(%d) = %v, %v; want %s", c.tick, got, err, c.pos)
		}
		got, err = RaydiumTickMath.SqrtPriceAtTick(-c.tick)
		if err != nil || got.String() != c.neg {
			t.Errorf("raydium sqrt(%d) = %v, %v; want %s", -c.tick, got, err, c.neg)
		}
	}
	if got, _ := RaydiumTickMath.SqrtPriceAtTick(0); got.Cmp(q64) != 0 {
		t.Errorf("raydium sqrt(0) = %v", got)
	}
}

func TestSqrtPriceAtTickBounds(t *testing.T) {
	for _, tm := range []TickMath{OrcaTickMath, RaydiumTickMath} {
		for _, tick := range []int32{MinTick - 1, MaxTick + 1, -1 << 31, 1<<31 - 1} {
			if _, err := tm.SqrtPriceAtTick(tick); err == nil {
				t.Errorf("%T: tick %d accepted", tm, tick)
			}
		}
		// Strictly increasing across a sample of ticks, including both sign branches.
		prev, _ := tm.SqrtPriceAtTick(MinTick)
		for tick := int32(MinTick + 1); tick <= MaxTick; tick += 997 {
			cur, err := tm.SqrtPriceAtTick(tick)
			if err != nil || cur.Cmp(prev) <= 0 {
				t.Fatalf("%T: sqrt(%d) = %v not > previous %v", tm, tick, cur, prev)
			}
			prev = cur
		}
	}
}

// The captured pools' current sqrt price must lie in [sqrt(tick_current), sqrt(tick_current+1)):
// an end-to-end check of each protocol's tick math against live on-chain state.
func TestTickMathMatchesLivePools(t *testing.T) {
	accts, _ := loadAccounts(t, "orca_accounts.json", "raydium_clmm_accounts.json")
	check := func(name string, tm TickMath, sqrt *big.Int, tick int32) {
		lo, _ := tm.SqrtPriceAtTick(tick)
		hi, _ := tm.SqrtPriceAtTick(tick + 1)
		if sqrt.Cmp(lo) < 0 || sqrt.Cmp(hi) >= 0 {
			t.Errorf("%s: sqrt price %v not in [%v, %v) for tick %d", name, sqrt, lo, hi, tick)
		}
	}
	wp := accts["Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"]
	check("orca SOL/USDC", OrcaTickMath, u128At(wp, orcaPoolSqrtPrice), i32At(wp, 81))
	rp := accts["3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"]
	check("raydium SOL/USDC", RaydiumTickMath, u128At(rp, rayPoolSqrtPrice), i32At(rp, 269))
}

// Orca SDK test_decrease_liquidity_quote (L = 1,000,000, ticks -10..10, slippage ignored).
// https://github.com/orca-so/whirlpools/blob/main/rust-sdk/core/src/quote/liquidity.rs
func TestAmountsFromLiquidityOrcaSDKVectors(t *testing.T) {
	sl, _ := OrcaTickMath.SqrtPriceAtTick(-10)
	su, _ := OrcaTickMath.SqrtPriceAtTick(10)
	l := big.NewInt(1000000)
	cases := []struct {
		sqrt   string
		a, b   int64
		status RangeStatus
	}{
		{"18354745142194483561", 999, 0, BelowRange},
		{"18446744073709551616", 499, 499, InRange},
		{"18539204128674405812", 0, 999, AboveRange},
	}
	for _, c := range cases {
		sqrt, _ := new(big.Int).SetString(c.sqrt, 10)
		a, b, st := AmountsFromLiquidity(l, sqrt, sl, su)
		if a.Int64() != c.a || b.Int64() != c.b || st != c.status {
			t.Errorf("sqrt %s: got (%v, %v, %v), want (%d, %d, %v)", c.sqrt, a, b, st, c.a, c.b, c.status)
		}
	}
	// Zero liquidity → zero amounts.
	if a, b, _ := AmountsFromLiquidity(new(big.Int), q64, sl, su); a.Sign() != 0 || b.Sign() != 0 {
		t.Errorf("zero liquidity gave %v, %v", a, b)
	}
}

// Orca SDK test_position_status around ticks -100..100: the boundary is sqrt ≤ lower.
func TestRangeStatusBoundaries(t *testing.T) {
	sl, _ := OrcaTickMath.SqrtPriceAtTick(-100)
	su, _ := OrcaTickMath.SqrtPriceAtTick(100)
	if sl.String() != "18354745142194483561" {
		t.Fatalf("sqrt(-100) = %v", sl)
	}
	l := big.NewInt(1)
	for _, c := range []struct {
		sqrt *big.Int
		want RangeStatus
	}{
		{new(big.Int).Sub(sl, big.NewInt(1)), BelowRange},
		{sl, BelowRange},
		{new(big.Int).Add(sl, big.NewInt(1)), InRange},
		{new(big.Int).Sub(su, big.NewInt(1)), InRange},
		{su, AboveRange},
		{new(big.Int).Add(su, big.NewInt(1)), AboveRange},
	} {
		if _, _, st := AmountsFromLiquidity(l, c.sqrt, sl, su); st != c.want {
			t.Errorf("sqrt %v: status %v, want %v", c.sqrt, st, c.want)
		}
	}
}

// Orca token_math test_get_amount_delta: sqrt prices 2 and 4 (Q64.64), L = 4 → A = 1, B = 8.
func TestDeltaAmountsSmall(t *testing.T) {
	two := new(big.Int).Lsh(big.NewInt(2), 64)
	four := new(big.Int).Lsh(big.NewInt(4), 64)
	if got := deltaA(big.NewInt(4), two, four); got.Int64() != 1 {
		t.Errorf("deltaA = %v, want 1", got)
	}
	if got := deltaB(big.NewInt(4), two, four); got.Int64() != 8 {
		t.Errorf("deltaB = %v, want 8", got)
	}
	if got := deltaA(big.NewInt(4), four, four); got.Sign() != 0 {
		t.Errorf("deltaA equal prices = %v", got)
	}
	if got := deltaA(big.NewInt(4), new(big.Int), four); got.Sign() != 0 {
		t.Errorf("deltaA zero lower = %v (must not divide by zero)", got)
	}
	// Swapped bounds are normalized.
	a1, b1, _ := AmountsFromLiquidity(big.NewInt(1e12), q64, two, four)
	a2, b2, _ := AmountsFromLiquidity(big.NewInt(1e12), q64, four, two)
	if a1.Cmp(a2) != 0 || b1.Cmp(b2) != 0 {
		t.Errorf("bound order changed result")
	}
}
