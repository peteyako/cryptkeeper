package templates

import (
	"slices"
	"strconv"
)

// The display decisions of the Koinly import wizard.

// koinlyID is the id of row i's control in a mapping or reconciliation table: "kw-0"
// (wallet), "ka-3" (asset), "kx-1" (extra basis). Koinly's names can hold any character, so
// they never go into an id.
func koinlyID(prefix string, i int) string { return prefix + "-" + strconv.Itoa(i) }

// koinlyLoaded lists what the draft holds, one entry per report: "Holdings as at Dec 31,
// 2025: 3 assets", "Transaction history: 1 transaction", "Capital gains: not uploaded".
func koinlyLoaded(w KoinlyWizard) []string {
	count := func(kind string, n int, word string) string {
		if !slices.Contains(w.Kinds, kind) {
			return "not uploaded"
		}
		return Plural(n, word)
	}
	holdings := "Holdings: not uploaded (needed to accept)"
	if slices.Contains(w.Kinds, "holdings") {
		holdings = "Holdings as at " + w.AsOf.Format("Jan 2, 2006") + ": " + Plural(w.Holdings, "asset")
	}
	return []string{
		holdings,
		"Transaction history: " + count("transactions", w.Txns, "transaction"),
		"Capital gains: " + count("gains", w.Gains, "row"),
		"Income: " + count("income", w.Income, "row"),
	}
}

// koinlyAcceptLabel is the Accept button's text: "Accept and close 2025".
func koinlyAcceptLabel(w KoinlyWizard) string {
	return "Accept and close " + strconv.Itoa(w.Year)
}

// koinlyExtraValue is the extra-basis input's value: empty for none.
func koinlyExtraValue(v float64) string {
	if !finite(v) || v <= 0 {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// koinlyExtraLabel names the extra-basis input for screen readers: "Your cost for the 1.5
// SOL Koinly has no cost for, in US dollars".
func koinlyExtraLabel(r KoinlyReconRow) string {
	return "Your cost for the " + Coin(r.MissingBasis, r.Symbol) + " Koinly has no cost for, in US dollars"
}

// KoinlyWalletLabel is a wallet option of the mapping select: "Phantom (Solana)".
func KoinlyWalletLabel(label, sourceType string) string {
	if t := typeName(sourceType); t != "" {
		return label + " (" + t + ")"
	}
	return label
}
