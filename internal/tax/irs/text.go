package irs

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"crypto-portfolio/internal/tax/report"
)

// latin1Replacements spell out common characters beyond Latin-1 that have no Latin-1 base
// letter under Unicode decomposition.
var latin1Replacements = map[rune]string{
	'…': "...", '‘': "'", '’': "'", '‚': ",", '‛': "'", '“': `"`, '”': `"`, '„': `"`, '‟': `"`,
	'‐': "-", '‑': "-", '‒': "-", '–': "-", '—': "-", '―': "-", '−': "-", '•': "*",
	'‹': "<", '›': ">", '€': "EUR", '™': "TM",
	'Ł': "L", 'ł': "l", 'Đ': "D", 'đ': "d", 'Ħ': "H", 'ħ': "h", 'ı': "i", 'Œ': "OE", 'œ': "oe",
	'Ŧ': "T", 'ŧ': "t", 'Ŋ': "N", 'ŋ': "n", 'ĸ': "k", 'ſ': "s", 'Ə': "E", 'ə': "e",
}

// latin1 converts s to text the forms' font can show (pdfcpu only fills Latin-1 without a
// user font): Latin-1 letters stay, accented letters beyond it lose the accent ("Ż" → "Z"),
// common punctuation is spelled out ("…" → "..."), controls and invisible marks are dropped
// (whitespace becomes a space), and anything else (emoji, CJK) becomes "?".
func latin1(s string) string {
	var b strings.Builder
	for _, r := range norm.NFC.String(s) {
		switch {
		case r == utf8.RuneError:
			b.WriteByte('?')
		case unicode.IsControl(r): // C0, DEL and C1 (0x80–0x9F print as other glyphs)
			if unicode.IsSpace(r) {
				b.WriteByte(' ')
			}
		case r <= 0xFF:
			b.WriteRune(r)
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf): // marks, ZWJ, variation selectors
		default:
			if rep, ok := latin1Replacements[r]; ok {
				b.WriteString(rep)
			} else if base, _ := utf8.DecodeRuneInString(norm.NFD.String(string(r))); base != r && base <= 0xFF && !unicode.IsControl(base) {
				b.WriteRune(base)
			} else if unicode.IsSpace(r) {
				b.WriteByte(' ')
			} else {
				b.WriteByte('?')
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// printable reports whether a Latin-1 text has anything left besides "?" and spaces.
func printable(s string) bool { return strings.Trim(s, "? ") != "" }

// shortKey is an asset key's short Latin-1 form: "spl:<mint>" becomes "ABCD...WXYZ".
func shortKey(key string) string {
	if mint, ok := strings.CutPrefix(key, "spl:"); ok {
		key = mint
		if rs := []rune(mint); len(rs) > 8 {
			key = string(rs[:4]) + "..." + string(rs[len(rs)-4:])
		}
	}
	if k := latin1(key); printable(k) {
		return k
	}
	return ""
}

// description is column (a) with its symbol in Latin-1; a symbol with nothing printable
// left (emoji only, CJK) is replaced by the asset key's short form.
func description(row report.Form8949Row) string {
	sym := latin1(row.Symbol)
	if !printable(sym) {
		if k := shortKey(row.AssetKey); k != "" {
			sym = k
		}
	}
	return strings.TrimSuffix(row.Description, row.Symbol) + sym
}

// ssnText formats a 9-digit SSN as NNN-NN-NNNN; anything else is kept as given.
func ssnText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 9 && strings.Trim(s, "0123456789") == "" {
		return s[:3] + "-" + s[3:5] + "-" + s[5:]
	}
	return s
}

// fit makes v fit a field's maxLen (0 = no limit): a MM/DD/YYYY date becomes MM/DD/YY;
// anything else (an amount, an SSN) is never cut, so a value that is too long is an error.
func fit(v string, maxLen int) (string, error) {
	if maxLen <= 0 || utf8.RuneCountInString(v) <= maxLen {
		return v, nil
	}
	if d, err := time.Parse("01/02/2006", v); err == nil {
		if s := d.Format("01/02/06"); len(s) <= maxLen {
			return s, nil
		}
	}
	return "", fmt.Errorf("%q is longer than the field's %d characters", v, maxLen)
}
