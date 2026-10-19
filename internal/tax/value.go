package tax

import (
	"fmt"
	"math"

	"cryptkeeper/internal/core"
)

// PriceKey is the price-override key for leg number leg of a transaction.
func PriceKey(txID string, leg int) string { return fmt.Sprintf("%s#%d", txID, leg) }

// FeePriceKey is the price-override key for a transaction's fee.
func FeePriceKey(txID string) string { return txID + "#fee" }

// valued is a leg with its resolved absolute USD value.
type valued struct {
	core.Leg
	idx   int
	usd   float64
	known bool
}

// valueLeg returns the absolute USD value of a leg: |override| when finite (an override of
// 0 is a deliberate "worthless"), then |Leg.USD| when finite and non-zero, then |Amount|
// for pegged assets, else unknown.
func valueLeg(l core.Leg, key string, prices map[string]float64) (usd float64, known bool) {
	if p, ok := prices[key]; ok && finite(p) {
		return math.Abs(p), true
	}
	if usableUSD(l.USD) {
		return math.Abs(*l.USD), true
	}
	if core.Pegged[l.AssetKey] {
		return math.Abs(l.Amount), true
	}
	return 0, false
}

// usableUSD reports whether a leg's USD is a value: set, finite and non-zero.
func usableUSD(u *float64) bool { return u != nil && *u != 0 && finite(*u) }

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// fillTrade values a side's single unvalued leg from the other, fully valued, side.
// Adapted from the average-cost engine's sideValue / fillSide, removed in M3.
func fillTrade(neg, pos []*valued) {
	negTotal, negAll := sideValue(neg)
	posTotal, posAll := sideValue(pos)
	if !negAll && posAll {
		fillSide(neg, posTotal)
	} else if !posAll && negAll {
		fillSide(pos, negTotal)
	}
}

// sideValue sums the known values of a side and reports whether every leg has one.
func sideValue(legs []*valued) (total float64, all bool) {
	all = len(legs) > 0
	for _, l := range legs {
		if l.known {
			total += l.usd
		} else {
			all = false
		}
	}
	return total, all
}

// fillSide values the single unvalued leg of a side as total minus the side's known legs.
func fillSide(legs []*valued, total float64) {
	var unknown *valued
	known := 0.0
	for _, l := range legs {
		switch {
		case l.known:
			known += l.usd
		case unknown == nil:
			unknown = l
		default:
			return // two unvalued legs: cannot split the value
		}
	}
	if unknown != nil && total-known > 0 {
		unknown.usd, unknown.known = total-known, true
	}
}
