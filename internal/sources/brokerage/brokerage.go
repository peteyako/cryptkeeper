// Package brokerage is a Source for a brokerage account typed in by hand: stocks, ETFs and
// funds by ticker. CryptKeep never connects to the broker; prices come from the price
// service ("stock:<TICKER>" keys) and names from a ticker lookup.
package brokerage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/sources/manual"
)

// Security is what the ticker lookup knows about a ticker.
type Security struct {
	Name     string
	Currency string // only US-dollar listings are priced
}

// Lookup describes tickers. A ticker it doesn't know is absent from the result; err means it
// couldn't be asked at all.
type Lookup func(ctx context.Context, tickers []string) (map[string]Security, error)

// Source serves a fixed list of holdings, named by Lookup.
type Source struct {
	id       string
	holdings []manual.Holding
	lookup   Lookup
}

// New returns the account; lookup may be nil (holdings are then named by ticker).
func New(id string, holdings []manual.Holding, lookup Lookup) *Source {
	return &Source{id: id, holdings: holdings, lookup: lookup}
}

func (s *Source) ID() string { return s.id }

// Fetch lists the holdings. A ticker the lookup doesn't know, or one listed in another
// currency, comes with a warning; a lookup that fails only says the names may be missing.
func (s *Source) Fetch(ctx context.Context) (core.WalletResult, error) {
	res := core.WalletResult{WalletID: s.id, AsOf: time.Now()}
	tickers := make([]string, 0, len(s.holdings))
	for _, h := range s.holdings {
		tickers = append(tickers, h.Asset)
	}
	var secs map[string]Security
	var lookupErr error
	if s.lookup != nil {
		secs, lookupErr = s.lookup(ctx, tickers)
		if lookupErr != nil {
			res.Warnings = append(res.Warnings, "Couldn't reach Yahoo Finance to check the tickers, so prices may be out of date.")
		}
	}
	warned := map[string]bool{}
	for _, h := range s.holdings {
		t := h.Asset
		sec, known := secs[t]
		name := sec.Name
		if name == "" {
			name = t
		}
		res.Holdings = append(res.Holdings, core.Holding{
			WalletID: s.id, AssetKey: core.StockKey(t), Symbol: t, Name: name,
			Amount: h.Amount, AmountStr: strconv.FormatFloat(h.Amount, 'f', -1, 64), Note: h.Note,
		})
		if warned[t] || s.lookup == nil || lookupErr != nil {
			continue
		}
		switch {
		case !known:
			res.Warnings = append(res.Warnings, fmt.Sprintf("Yahoo Finance doesn't know %s. Check the ticker.", t))
			warned[t] = true
		case sec.Currency != "USD":
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s trades in %s. CryptKeep only prices US-dollar listings.", t, sec.Currency))
			warned[t] = true
		}
	}
	res.Meta = []core.KV{{K: "Prices", V: "Yahoo Finance"}, {K: "Holdings", V: strconv.Itoa(len(s.holdings))}}
	return res, nil
}

// ParseLines parses the form's holdings, one per line: "TICKER SHARES [note…]". Blank lines
// and lines starting with # are ignored. Errors name the line.
func ParseLines(text string) ([]manual.Holding, error) {
	var out []manual.Holding
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: expected \"TICKER SHARES [note]\"", i+1)
		}
		t, ok := core.NormalizeTicker(f[0])
		if !ok {
			return nil, fmt.Errorf("line %d: %q isn't a ticker", i+1, f[0])
		}
		shares, err := strconv.ParseFloat(strings.ReplaceAll(f[1], ",", ""), 64)
		if err != nil || shares < 0 {
			return nil, fmt.Errorf("line %d: %q is not a valid number of shares", i+1, f[1])
		}
		out = append(out, manual.Holding{Asset: t, Amount: shares, Note: strings.Join(f[2:], " ")})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("add at least one holding, e.g. \"VTI 40\"")
	}
	return out, nil
}
