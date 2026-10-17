package koinly

import (
	"sort"
	"strings"

	"crypto-portfolio/internal/tax"
)

// Wallet is a CryptKeep source as the suggester sees it.
type Wallet struct {
	ID, Label, Type string
	Addresses       []string
}

// AssetInfo is an asset CryptKeep knows (snapshot rows + activity legs).
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

// SuggestWallets maps each Koinly wallet name to a CryptKeep source ID or tax.Untracked.
func SuggestWallets(txns []Txn, gains []tax.Disposal, wallets []Wallet) map[string]string {
	addrs := map[string]map[string]bool{}
	add := func(name, addr string) {
		if name == "" || addr == "" {
			return
		}
		if addrs[name] == nil {
			addrs[name] = map[string]bool{}
		}
		addrs[name][addr] = true
	}
	for _, t := range txns {
		add(t.RecvWallet, t.TxDest)
		add(t.SendWallet, t.TxSrc)
	}
	single := func(typ string) string {
		id, n := "", 0
		for _, w := range wallets {
			if w.Type == typ {
				id, n = w.ID, n+1
			}
		}
		if n == 1 {
			return id
		}
		return ""
	}
	out := map[string]string{}
	for _, name := range WalletNames(txns, gains) {
		out[name] = tax.Untracked
		found := false
		for _, w := range wallets {
			for _, a := range w.Addresses {
				if addrs[name][a] {
					out[name], found = w.ID, true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			continue
		}
		low := strings.ToLower(name)
		for _, typ := range []string{"kraken", "coinbase"} {
			if strings.Contains(low, typ) {
				if id := single(typ); id != "" {
					out[name] = id
				}
				break
			}
		}
	}
	return out
}

// SuggestAssets maps each holding's Asset string to a CryptKeep asset key ("" = skip).
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
