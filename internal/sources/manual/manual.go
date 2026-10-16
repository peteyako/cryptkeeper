// Package manual is a Source for holdings typed in by hand (coins held anywhere the app
// cannot read). They are priced like any other holding.
package manual

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/core"
)

// Holding is one hand-entered line.
type Holding struct {
	Asset  string  `yaml:"asset"`
	Amount float64 `yaml:"amount"`
	Note   string  `yaml:"note,omitempty"`
}

// Source serves a fixed list of holdings.
type Source struct {
	id       string
	holdings []Holding
}

func New(id string, holdings []Holding) *Source { return &Source{id: id, holdings: holdings} }

func (s *Source) ID() string { return s.id }

func (s *Source) Fetch(context.Context) (core.WalletResult, error) {
	res := core.WalletResult{WalletID: s.id, AsOf: time.Now()}
	for _, h := range s.holdings {
		key := NormalizeAsset(h.Asset)
		sym := key
		mint := ""
		if m, ok := core.IsSPLKey(key); ok {
			mint = m
			sym = m[:4] + "…" + m[len(m)-4:]
		}
		res.Holdings = append(res.Holdings, core.Holding{
			WalletID: s.id, AssetKey: key, Symbol: sym, Mint: mint,
			Amount: h.Amount, AmountStr: strconv.FormatFloat(h.Amount, 'f', -1, 64), Note: h.Note,
		})
	}
	res.Meta = []core.KV{{K: "Type", V: "Manual entries"}, {K: "Entries", V: strconv.Itoa(len(s.holdings))}}
	return res, nil
}

// NormalizeAsset upper-cases symbols and keeps "spl:<mint>" keys as given (mints are
// case-sensitive base58).
func NormalizeAsset(a string) string {
	a = strings.TrimSpace(a)
	if len(a) > 4 && strings.EqualFold(a[:4], "spl:") {
		return core.SPLKey(a[4:])
	}
	return strings.ToUpper(a)
}

// ParseLines parses the UI textarea format, one holding per line: "ASSET AMOUNT [note…]".
// Blank lines and lines starting with # are ignored. Errors name the line number.
func ParseLines(text string) ([]Holding, error) {
	var out []Holding
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: expected \"ASSET AMOUNT [note]\"", i+1)
		}
		amt, err := strconv.ParseFloat(strings.ReplaceAll(f[1], ",", ""), 64)
		if err != nil || amt < 0 {
			return nil, fmt.Errorf("line %d: %q is not a valid amount", i+1, f[1])
		}
		out = append(out, Holding{Asset: NormalizeAsset(f[0]), Amount: amt, Note: strings.Join(f[2:], " ")})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("add at least one holding, e.g. \"BTC 0.25\"")
	}
	return out, nil
}

// FormatLines is the inverse of ParseLines, for re-filling the edit form.
func FormatLines(hs []Holding) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(h.Asset)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(h.Amount, 'f', -1, 64))
		if h.Note != "" {
			b.WriteByte(' ')
			b.WriteString(h.Note)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
