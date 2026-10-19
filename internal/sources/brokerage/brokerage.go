// Package brokerage is a Source for a brokerage account typed in by hand: stocks, ETFs and
// funds by ticker, and cash. CryptKeeper never connects to the broker; prices come from the
// price service ("stock:<TICKER>" keys) and names from a ticker lookup.
package brokerage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/sources/manual"
)

// Cash is how a holding of cash is stored: dollars, priced at one like any "USD" balance.
// The tickers USD (an ETF) and CASH (a bank stock) are stored as stock:USD and stock:CASH.
const Cash = "USD"

// cashWords start a cash line: "USD 1500", "CASH $1,500".
var cashWords = map[string]bool{"USD": true, "CASH": true}

// ticker is the ticker a stored asset names, or cash for Cash.
func ticker(asset string) (t string, cash bool) {
	if asset == Cash {
		return "", true
	}
	if t, ok := core.IsStockKey(asset); ok {
		return t, false
	}
	return asset, false
}

// ValidAsset reports whether a stored holding's asset is Cash, a ticker as
// core.NormalizeTicker writes it, or "stock:<TICKER>".
func ValidAsset(asset string) bool {
	if asset == Cash {
		return true
	}
	t, _ := ticker(asset)
	n, ok := core.NormalizeTicker(t)
	return ok && n == t
}

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
		if t, cash := ticker(h.Asset); !cash {
			tickers = append(tickers, t)
		}
	}
	var secs map[string]Security
	var lookupErr error
	if s.lookup != nil && len(tickers) > 0 {
		secs, lookupErr = s.lookup(ctx, tickers)
		if lookupErr != nil {
			res.Warnings = append(res.Warnings, "Couldn't reach Yahoo Finance to check the tickers, so prices may be out of date.")
		}
	}
	warned := map[string]bool{}
	for _, h := range s.holdings {
		amount := strconv.FormatFloat(h.Amount, 'f', -1, 64)
		t, cash := ticker(h.Asset)
		if cash {
			res.Holdings = append(res.Holdings, core.Holding{
				WalletID: s.id, AssetKey: Cash, Symbol: Cash, Name: "Cash", Amount: h.Amount, AmountStr: amount, Note: h.Note,
			})
			continue
		}
		sec, known := secs[t]
		name := sec.Name
		if name == "" {
			name = t
		}
		res.Holdings = append(res.Holdings, core.Holding{
			WalletID: s.id, AssetKey: core.StockKey(t), Symbol: t, Name: name,
			Amount: h.Amount, AmountStr: amount, Note: h.Note,
		})
		if warned[t] || s.lookup == nil || lookupErr != nil {
			continue
		}
		switch {
		case !known:
			res.Warnings = append(res.Warnings, fmt.Sprintf("Yahoo Finance doesn't know %s. Check the ticker.", t))
			warned[t] = true
		case sec.Currency != "USD":
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s trades in %s. CryptKeeper only prices US-dollar listings.", t, sec.Currency))
			warned[t] = true
		}
	}
	res.Meta = []core.KV{{K: "Prices", V: "Yahoo Finance"}, {K: "Holdings", V: strconv.Itoa(len(s.holdings))}}
	return res, nil
}

// ParseLines parses the form's holdings, one per line: "TICKER SHARES [note…]", or for cash
// "USD AMOUNT" or "CASH AMOUNT" (the amount may start with $). The tickers USD and CASH are
// written stock:USD and stock:CASH. Blank lines and lines starting with # are ignored.
// Errors name the line.
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
		asset := Cash
		if !cashWords[strings.ToUpper(f[0])] {
			raw, explicit := f[0], false
			if len(raw) > 6 && strings.EqualFold(raw[:6], "stock:") {
				raw, explicit = raw[6:], true
			}
			t, ok := core.NormalizeTicker(raw)
			if !ok {
				return nil, fmt.Errorf("line %d: %q isn't a ticker", i+1, f[0])
			}
			asset = t
			if explicit && cashWords[t] {
				asset = core.StockKey(t)
			}
		}
		amount, err := strconv.ParseFloat(strings.TrimPrefix(strings.ReplaceAll(f[1], ",", ""), "$"), 64)
		if err != nil || amount < 0 {
			return nil, fmt.Errorf("line %d: %q is not a valid amount", i+1, f[1])
		}
		out = append(out, manual.Holding{Asset: asset, Amount: amount, Note: strings.Join(f[2:], " ")})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("add at least one holding, e.g. \"VTI 40\"")
	}
	return out, nil
}
