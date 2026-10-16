package solana

import (
	"math/big"
	"net/url"
	"strings"
	"unicode"
)

const (
	maxSymbolRunes = 24
	maxNameRunes   = 64
	maxURLLen      = 500
)

// shortKey shortens a public key for display: EPjFWdd5…Dt1v becomes "EPjF…Dt1v".
func shortKey(s string) string {
	if len(s) <= 9 {
		return s
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// cleanText sanitises token-supplied text: anyone can mint a token with any name, so strip
// padding NULs, control and invisible formatting characters (bidi overrides, zero-width), invalid
// UTF-8, surrounding space, and cap the length.
func cleanText(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if rs := []rune(out); len(rs) > maxRunes {
		out = strings.TrimSpace(string(rs[:maxRunes]))
	}
	return out
}

// safeHTTPURL returns s if it is an absolute http(s) URL, else "". Logo URLs end up in <img src>.
func safeHTTPURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxURLLen {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return s
}

// formatAmount renders an integer token amount with the given decimals as an exact decimal
// string with trailing zeros removed: (1500000, 6) -> "1.5".
func formatAmount(n *big.Int, decimals int) string {
	neg := n.Sign() < 0
	s := new(big.Int).Abs(n).String()
	if decimals > 0 {
		if len(s) <= decimals {
			s = strings.Repeat("0", decimals-len(s)+1) + s
		}
		intPart, frac := s[:len(s)-decimals], strings.TrimRight(s[len(s)-decimals:], "0")
		if frac != "" {
			s = intPart + "." + frac
		} else {
			s = intPart
		}
	}
	if neg {
		return "-" + s
	}
	return s
}
