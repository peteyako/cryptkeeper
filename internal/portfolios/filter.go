package portfolios

import (
	"slices"

	"cryptkeeper/internal/portfolio"
)

// Apply returns snap restricted to portfolio p. Each wallet keeps only the rows whose
// (wallet, asset) pair p includes, and HiddenRows are filtered the same way. A wallet left
// with no rows is dropped unless p lists it explicitly (then it stays, empty or erroring, so
// its status stays visible) or p is unfiltered (wallets and assets both "all"). Totals,
// assets, shares and the stale flag are recomputed with portfolio.FromWallets. snap is not
// modified.
func Apply(p Portfolio, snap portfolio.Snapshot) portfolio.Snapshot {
	inc := Include(p)
	unfiltered := p.Wallets.All && p.Assets.All
	views := make([]portfolio.WalletView, 0, len(snap.Wallets))
	for _, w := range snap.Wallets {
		if !p.Wallets.Has(w.ID) {
			continue
		}
		w.Rows = filterRows(w.ID, w.Rows, inc)
		w.HiddenRows = filterRows(w.ID, w.HiddenRows, inc)
		if len(w.Rows) == 0 && !unfiltered && !p.Wallets.explicit(w.ID) {
			continue
		}
		views = append(views, w)
	}
	return portfolio.FromWallets(views, snap.Prices, snap.UpdatedAt)
}

// filterRows returns a new slice of the rows inc accepts (nil if none).
func filterRows(walletID string, rows []portfolio.HoldingRow, inc portfolio.Include) []portfolio.HoldingRow {
	var out []portfolio.HoldingRow
	for _, r := range rows {
		if inc(walletID, r.AssetKey) {
			out = append(out, r)
		}
	}
	return out
}

// Include reports whether a (wallet, asset) pair counts toward p, for History.Series.
func Include(p Portfolio) portfolio.Include {
	p = p.clone() // the closure must not see later edits to the caller's slices
	return func(walletID, assetKey string) bool {
		return p.Wallets.Has(walletID) && p.Assets.Has(assetKey)
	}
}

// NetWorthShare is the filtered snapshot's share of the unfiltered total, or 0 when the
// unfiltered total is 0.
func NetWorthShare(filtered, all portfolio.Snapshot) float64 {
	if all.TotalUSD == 0 {
		return 0
	}
	return filtered.TotalUSD / all.TotalUSD
}

// MissingWallets lists the wallet ids p names explicitly that are not in known (typically
// deleted sources), in p's order.
func MissingWallets(p Portfolio, known []string) []string {
	var missing []string
	for _, id := range p.Wallets.IDs {
		if !p.Wallets.All && !slices.Contains(known, id) {
			missing = append(missing, id)
		}
	}
	return missing
}

// WatchKeys returns p's watch-list asset keys (already normalized), as a copy.
func WatchKeys(p Portfolio) []string { return slices.Clone(p.Watch) }
