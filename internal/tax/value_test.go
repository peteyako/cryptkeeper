package tax

import (
	"math"
	"testing"
)

func TestValueOverrideWins(t *testing.T) {
	key := PriceKey("w:a", 0)
	got, ok := valueLeg(lg("SOL", -1, usd(-200)), key, map[string]float64{key: 150})
	if !ok {
		t.Fatal("want known")
	}
	near(t, "usd", got, 150)
}

func TestValueAbsOfSignedUSD(t *testing.T) {
	got, ok := valueLeg(lg("SOL", -1, usd(-200)), PriceKey("w:a", 0), nil)
	if !ok {
		t.Fatal("want known")
	}
	near(t, "usd", got, 200)
}

func TestValuePegged(t *testing.T) {
	got, ok := valueLeg(lg("USDC", 25, nil), PriceKey("w:a", 0), nil)
	if !ok {
		t.Fatal("want known")
	}
	near(t, "usd", got, 25)
}

func TestValueUnknown(t *testing.T) {
	got, ok := valueLeg(lg("spl:X", 100, nil), PriceKey("w:a", 0), nil)
	if ok {
		t.Fatal("want unknown")
	}
	near(t, "usd", got, 0)
}

func TestKeys(t *testing.T) {
	if PriceKey("w:a", 2) != "w:a#2" || FeePriceKey("w:a") != "w:a#fee" {
		t.Fatal("bad keys")
	}
}

func mkValued(asset string, amount float64, u *float64) *valued {
	l := lg(asset, amount, u)
	v := &valued{Leg: l}
	v.usd, v.known = valueLeg(l, "k", nil)
	return v
}

func TestFillOneUnvaluedLeg(t *testing.T) {
	sol := mkValued("SOL", -1, nil)
	fillTrade([]*valued{sol}, []*valued{mkValued("USDC", 180, nil)})
	if !sol.known {
		t.Fatal("want SOL valued")
	}
	near(t, "usd", sol.usd, 180)
}

func TestFillTwoUnvaluedNoop(t *testing.T) {
	a, b := mkValued("spl:A", -1, nil), mkValued("spl:B", -2, nil)
	fillTrade([]*valued{a, b}, []*valued{mkValued("USDC", 50, nil)})
	if a.known || b.known {
		t.Fatal("both should stay unknown")
	}
}

func TestFillSubtractsKnownLegs(t *testing.T) {
	a, b := mkValued("SOL", 1, nil), mkValued("ETH", 1, usd(30))
	fillTrade([]*valued{mkValued("USDC", -100, nil)}, []*valued{a, b})
	near(t, "usd", a.usd, 70)
}

func TestValueOverrideAbsAndNonFinite(t *testing.T) {
	key := PriceKey("w:a", 0)
	got, ok := valueLeg(lg("SOL", -1, usd(-200)), key, map[string]float64{key: -150})
	if !ok {
		t.Fatal("want known")
	}
	near(t, "negative override", got, 150)

	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		got, ok := valueLeg(lg("SOL", -1, usd(-200)), key, map[string]float64{key: bad})
		if !ok {
			t.Fatalf("override %v: want the leg's USD", bad)
		}
		near(t, "fallback to Leg.USD", got, 200)
		if _, ok := valueLeg(lg("spl:X", 1, nil), key, map[string]float64{key: bad}); ok {
			t.Errorf("override %v on an unpriced leg: want unknown", bad)
		}
	}

	got, ok = valueLeg(lg("spl:X", 1, nil), key, map[string]float64{key: 0})
	if !ok || got != 0 {
		t.Errorf("zero override = %v, %v, want 0 known", got, ok)
	}
}
