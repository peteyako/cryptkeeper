package koinly

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"cryptkeeper/internal/tax"
)

// Wallet is a CryptKeeper source as the suggester sees it.
type Wallet struct {
	ID, Label, Type string
	Addresses       []string
}

// AssetInfo is an asset CryptKeeper knows (snapshot rows + activity legs).
type AssetInfo struct{ Key, Symbol, Name string }

// WalletNames returns every Koinly wallet name seen, sorted.
func WalletNames(txns []Txn, gains []tax.Disposal) []string {
	set := map[string]bool{}
	for _, t := range txns {
		set[t.SendWallet] = true
		set[t.RecvWallet] = true
	}
	for _, g := range gains {
		set[g.Wallet] = true
	}
	delete(set, "")
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Match is the CryptKeeper wallet a Koinly wallet was matched to (a source ID or
// tax.Untracked) and why, in words for the import page.
type Match struct{ ID, Why string }

// MatchWallets matches each Koinly wallet name to a CryptKeeper wallet, on the strongest
// evidence it finds:
//
//  1. its transactions' own addresses (TxDest of what it received, TxSrc of what it sent)
//     are a wallet's, the wallet used by the most transactions winning;
//  2. its name carries an address, whole or shortened as Koinly writes it
//     ("Bitcoin (BTC) - bc1q8w...dl"), of exactly one wallet;
//  3. its name contains exactly one wallet's label, among the wallets of the chain the name
//     names ("Solana (SOL) - K1S");
//  4. it names an exchange and there is exactly one source of that exchange.
//
// Anything else is tax.Untracked.
func MatchWallets(txns []Txn, gains []tax.Disposal, wallets []Wallet) map[string]Match {
	uses := map[string]map[string]int{} // Koinly wallet → its own address → transactions
	add := func(name, addr string) {
		if name == "" || addr == "" {
			return
		}
		if uses[name] == nil {
			uses[name] = map[string]int{}
		}
		uses[name][addr]++
	}
	for _, t := range txns {
		add(t.RecvWallet, t.TxDest)
		add(t.SendWallet, t.TxSrc)
	}
	out := map[string]Match{}
	for _, name := range WalletNames(txns, gains) {
		out[name] = matchWallet(name, uses[name], wallets)
	}
	return out
}

func matchWallet(name string, uses map[string]int, wallets []Wallet) Match {
	best, hits := -1, 0
	for i, w := range wallets {
		n := 0
		for _, a := range w.Addresses {
			n += uses[a]
		}
		if n > hits {
			best, hits = i, n
		}
	}
	if best >= 0 {
		return Match{wallets[best].ID, "Its transactions use " + wallets[best].Label + "'s address."}
	}
	if pre, suf, ok := nameAddress(name); ok {
		if i, ok := only(wallets, func(w Wallet) bool {
			return slices.ContainsFunc(w.Addresses, func(a string) bool {
				return strings.HasPrefix(a, pre) && strings.HasSuffix(a, suf)
			})
		}); ok {
			return Match{wallets[i].ID, pre + "…" + suf + " is one of " + wallets[i].Label + "'s addresses."}
		}
	}
	chain := nameChain(name)
	if i, ok := only(wallets, func(w Wallet) bool {
		return (chain == "" || w.Type == chain) && containsWord(name, w.Label)
	}); ok {
		return Match{wallets[i].ID, "Named after " + wallets[i].Label + "."}
	}
	low := strings.ToLower(name)
	for _, typ := range []string{"kraken", "coinbase"} {
		if strings.Contains(low, typ) {
			if i, ok := only(wallets, func(w Wallet) bool { return w.Type == typ }); ok {
				return Match{wallets[i].ID, "Your only " + strings.ToUpper(typ[:1]) + typ[1:] + " source."}
			}
			break
		}
	}
	return Match{tax.Untracked, "None of your wallets matches it."}
}

// only is the index of the one wallet that passes ok; false when none or several do.
func only(wallets []Wallet, ok func(Wallet) bool) (int, bool) {
	found := -1
	for i, w := range wallets {
		if ok(w) {
			if found >= 0 {
				return -1, false
			}
			found = i
		}
	}
	return found, found >= 0
}

// nameAddressRe finds an address in a Koinly wallet name: whole, or shortened to its start
// and end around "..." or "…". Koinly keeps six characters and two; with fewer, a start like
// "bc1q" would match half a wallet's addresses.
var nameAddressRe = regexp.MustCompile(`([A-Za-z0-9]{6,})(?:\.\.\.|…)([A-Za-z0-9]{2,})|([A-Za-z0-9]{26,})`)

// nameAddress is the start and end of the address in a wallet name ("bc1q8w", "dl"); a
// whole address is its own start.
func nameAddress(name string) (pre, suf string, ok bool) {
	m := nameAddressRe.FindStringSubmatch(name)
	switch {
	case m == nil:
		return "", "", false
	case m[3] != "":
		return m[3], "", true
	}
	return m[1], m[2], true
}

// nameChain is the source type a Koinly wallet name says it is on, or "".
func nameChain(name string) string {
	low := strings.ToLower(name)
	switch {
	case strings.Contains(low, "bitcoin") || strings.Contains(low, "(btc)"):
		return "bitcoin"
	case strings.Contains(low, "solana") || strings.Contains(low, "(sol)"):
		return "solana"
	}
	return ""
}

// containsWord reports whether name contains label as whole words, ignoring case: "K1S" is
// in "Solana (SOL) - K1S" but not in "Solana (SOL) - K1S2".
func containsWord(name, label string) bool {
	label = strings.TrimSpace(label)
	if label == "" {
		return false
	}
	re, err := regexp.Compile(`(?i)(^|[^A-Za-z0-9])` + regexp.QuoteMeta(label) + `($|[^A-Za-z0-9])`)
	return err == nil && re.MatchString(name)
}

// SuggestAssets maps each holding's Asset string to a CryptKeeper asset key ("" = skip).
// ambiguous lists the candidate keys for holdings it could not decide.
func SuggestAssets(holdings []Holding, known []AssetInfo) (m map[string]string, ambiguous map[string][]string) {
	m = map[string]string{}
	ambiguous = map[string][]string{}
	for _, h := range holdings {
		var cands []AssetInfo
		for _, k := range known {
			if strings.EqualFold(k.Symbol, h.Symbol) {
				cands = append(cands, k)
			}
		}
		switch {
		case len(cands) == 1:
			m[h.Asset] = cands[0].Key
		case len(cands) > 1:
			m[h.Asset] = ""
			for _, c := range cands {
				if c.Name == h.Name {
					m[h.Asset] = c.Key
					break
				}
			}
			if m[h.Asset] == "" {
				keys := make([]string, len(cands))
				for i, c := range cands {
					keys[i] = c.Key
				}
				sort.Strings(keys)
				ambiguous[h.Asset] = keys
			}
		case h.CostUSD == 0 && h.ValueUSD == 0:
			m[h.Asset] = ""
		default:
			m[h.Asset] = strings.ToUpper(h.Symbol)
		}
	}
	return m, ambiguous
}
