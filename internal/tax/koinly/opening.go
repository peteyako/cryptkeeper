package koinly

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"crypto-portfolio/internal/tax"
)

// dust is the unit amount below which a quantity counts as zero (as in package tax).
const dust = 1e-12

// costTol is the dollar amount below which a cost difference is float residue.
const costTol = 0.005

// acquisitionTypes are the Koinly row types whose received side brings units into a wallet
// ("buy" is fiat → crypto).
var acquisitionTypes = map[string]bool{"crypto_deposit": true, "exchange": true, "transfer": true, "buy": true}

// OpeningInput is everything BuildOpening reconciles: Koinly's closing state for Year,
// the user's mappings, and what CryptKeeper's wallets held at the cutover.
type OpeningInput struct {
	Year     int            // the imported year (2025)
	Loc      *time.Location // tax timezone; nil means time.Local
	Holdings []Holding
	Txns     []Txn
	// WalletMap maps a Koinly wallet name → source ID | tax.Untracked. Being keyed by
	// name, it cannot tell apart Koinly wallets that share a display name.
	WalletMap map[string]string
	AssetMap  map[string]string             // Koinly "SYM (Name)" → asset key | ""
	Balances  map[string]map[string]float64 // RollBack result: source ID → asset key → qty at cutover
	// ExtraBasis is asset key → the total USD basis the user entered for all of that
	// asset's CryptKeeper-excess units, not a per-unit price. With a positive basis those
	// lots carry it as a real cost; with none (0) they are flagged tax.FlagMissingBasis.
	ExtraBasis map[string]float64
	Parked     []tax.Parked // RollBackParked result
}

// ReconRow is one mapped asset's reconciliation between Koinly and CryptKeeper.
type ReconRow struct {
	KoinlyAsset, AssetKey                 string
	KoinlyQty, KoinlyCost, CryptKeeperQty float64
	Untracked, MissingBasis               float64 // units sent to tax.Untracked / units with no Koinly basis
	Dated, Various                        float64 // units in dated lots / in "before Year" lots
	Lots                                  []tax.Lot
}

// Opening is the reconciled opening position for the year after Year.
type Opening struct {
	Rows     []ReconRow
	Lots     []tax.Lot
	Parked   []tax.Parked // OpeningInput.Parked passed through, for the closed year's ClosingParked
	Problems []string
}

// BuildOpening turns Koinly's year-end holdings into dated FIFO opening lots per wallet.
// Per mapped asset, each wallet's quantity at the cutover is dated from its newest
// acquisitions in the transaction history; what Koinly holds beyond CryptKeeper goes to
// tax.Untracked, dated from its own wallets' rows and then from any rows (or parts of
// rows) the other wallets left unused; what CryptKeeper holds beyond Koinly becomes
// missing-basis lots; the rest is one "before Year" lot per wallet carrying Koinly's
// remaining cost (with none, a positive remainder is spread over the dated lots, so the
// total still matches Koinly).
// Holdings that share an asset key are reconciled together. Rows are by Koinly cost,
// highest first; lots are by wallet, asset and acquisition time.
func BuildOpening(in OpeningInput) Opening {
	if in.Loc == nil {
		in.Loc = time.Local
	}
	var out Opening
	byKey := map[string][]Holding{}
	unmapped := map[string]bool{}
	for _, h := range in.Holdings {
		k, ok := in.AssetMap[h.Asset]
		if !ok && !unmapped[h.Asset] {
			unmapped[h.Asset] = true
			out.Problems = append(out.Problems, h.Asset+": no asset mapping; skipped")
		}
		if k != "" {
			byKey[k] = append(byKey[k], h)
		}
	}
	sym, shared := symbolKeys(in.Holdings, in.AssetMap, in.Year)
	for _, k := range slices.Sorted(maps.Keys(byKey)) {
		row, problems := reconcile(in, k, byKey[k], sym)
		out.Rows = append(out.Rows, row)
		out.Lots = append(out.Lots, row.Lots...)
		out.Problems = append(out.Problems, shared[k]...)
		out.Problems = append(out.Problems, problems...)
	}
	slices.SortStableFunc(out.Rows, func(a, b ReconRow) int { return cmp.Compare(b.KoinlyCost, a.KoinlyCost) })
	slices.SortStableFunc(out.Lots, lotOrder)
	out.Parked = slices.Clone(in.Parked)
	slices.SortStableFunc(out.Parked, func(a, b tax.Parked) int {
		return cmp.Or(strings.Compare(a.Wallet, b.Wallet), strings.Compare(a.Asset, b.Asset))
	})
	return out
}

func lotOrder(a, b tax.Lot) int {
	return cmp.Or(strings.Compare(a.Wallet, b.Wallet), strings.Compare(a.Asset, b.Asset), a.Acquired.Compare(b.Acquired))
}

// symbolKeys maps a transaction-history currency symbol to the asset key of the mapped
// holding with that symbol, preferring the larger cost when several match. shared holds,
// per asset key that lost its symbol to another key, the problem saying so.
func symbolKeys(holdings []Holding, assetMap map[string]string, year int) (keys map[string]string, shared map[string][]string) {
	keys, best := map[string]string{}, map[string]float64{}
	all := map[string][]string{} // symbol → its distinct keys
	for _, h := range holdings {
		k := assetMap[h.Asset]
		if k == "" {
			continue
		}
		if !slices.Contains(all[h.Symbol], k) {
			all[h.Symbol] = append(all[h.Symbol], k)
		}
		if c, seen := best[h.Symbol]; !seen || h.CostUSD > c {
			keys[h.Symbol], best[h.Symbol] = k, h.CostUSD
		}
	}
	shared = map[string][]string{}
	for _, s := range slices.Sorted(maps.Keys(all)) {
		for _, k := range all[s] {
			if k == keys[s] {
				continue
			}
			others := slices.DeleteFunc(slices.Clone(all[s]), func(o string) bool { return o == k })
			slices.Sort(others)
			shared[k] = append(shared[k], fmt.Sprintf("%s: shares symbol %s with %s; its %d rows date %s",
				k, s, strings.Join(others, ", "), year, keys[s]))
		}
	}
	return keys, shared
}

// walletOf maps a Koinly wallet name to its source ID; an unmapped wallet is untracked.
func walletOf(walletMap map[string]string, name string) string {
	if w := walletMap[name]; w != "" {
		return w
	}
	return tax.Untracked
}

// acquisition is a transaction-history row that brought units of an asset into a wallet.
type acquisition struct {
	Txn
	wallet, id string
}

// acquisitions returns the rows before the cutover that brought asset k into a wallet,
// newest first (equal times keep file order). A transfer between two Koinly wallets
// mapped to the same wallet moves nothing into it, so it is left out. Each
// carries a traceable lot TxID: "koinly:<TxHash>", or "koinly:row<N>" (N counting
// Txns from 1) when the row has no hash.
func acquisitions(in OpeningInput, k string, sym map[string]string, cutover time.Time) []acquisition {
	var out []acquisition
	for i, t := range in.Txns {
		if !acquisitionTypes[t.Type] || !(t.RecvAmount > 0) || sym[t.RecvCurrency] != k || !t.Time.Before(cutover) {
			continue
		}
		w := walletOf(in.WalletMap, t.RecvWallet)
		if t.Type == "transfer" && walletOf(in.WalletMap, t.SendWallet) == w {
			continue
		}
		id := "koinly:" + t.TxHash
		if t.TxHash == "" {
			id = fmt.Sprintf("koinly:row%d", i+1)
		}
		out = append(out, acquisition{Txn: t, wallet: w, id: id})
	}
	slices.SortStableFunc(out, func(a, b acquisition) int { return b.Time.Compare(a.Time) })
	return out
}

// reconcile builds asset k's row and lots from its Koinly holdings hs.
func reconcile(in OpeningInput, k string, hs []Holding, sym map[string]string) (ReconRow, []string) {
	row := ReconRow{AssetKey: k}
	var names, problems []string
	note := func(format string, args ...any) { problems = append(problems, k+": "+fmt.Sprintf(format, args...)) }
	for _, h := range hs {
		row.KoinlyQty += h.Quantity
		row.KoinlyCost += h.CostUSD
		if !slices.Contains(names, h.Asset) {
			names = append(names, h.Asset)
		}
	}
	row.KoinlyAsset = strings.Join(names, ", ")
	q, c := row.KoinlyQty, row.KoinlyCost
	if q < 0 {
		note("Koinly quantity %s is negative; treated as 0", qty(q))
		q = 0
	}
	tol := math.Max(dust, 1e-9*q)
	cutover := time.Date(in.Year+1, 1, 1, 0, 0, 0, 0, in.Loc)

	// 1. What each wallet holds at the cutover; Koinly's excess is untracked.
	held := map[string]float64{}
	for _, w := range slices.Sorted(maps.Keys(in.Balances)) {
		switch v := in.Balances[w][k]; {
		case v > dust:
			held[w] = v
			row.CryptKeeperQty += v
		case v < -dust:
			note("%s rolls back to %s; treated as 0", w, qty(v))
		}
	}
	ck := row.CryptKeeperQty
	if math.Abs(q-ck) > math.Max(dust, 1e-6*q) {
		note("Koinly %s vs CryptKeeper %s", qty(q), qty(ck))
	}
	if q > ck+tol {
		row.Untracked = q - ck
		held[tax.Untracked] += row.Untracked
	}

	// 2–3. Date units from the newest acquisitions, at most Koinly's quantity in total.
	// First each wallet takes its own rows, newest first across wallets (which only
	// matters once that cap binds). Then the untracked excess takes whatever rows, or
	// parts of rows, are still unused: Koinly's extra units most likely came from those
	// recent acquisitions, and leaving them "before Year" would make them long-term.
	rows := acquisitions(in, k, sym, cutover)
	used, spent := make([]float64, len(rows)), make([]float64, len(rows))
	var lots []tax.Lot
	dated := map[string]float64{}
	datedCost := 0.0
	date := func(i int, w string) {
		r := rows[i]
		take := min(held[w]-dated[w], q-row.Dated, r.RecvAmount-used[i])
		if take <= tol {
			return
		}
		cost := r.RecvCost * take / r.RecvAmount
		if used[i]+take >= r.RecvAmount { // the row's last units carry the rest of its cost exactly
			cost = r.RecvCost - spent[i]
		}
		l := tax.Lot{Wallet: w, Asset: k, Amount: take, CostUSD: cost, Acquired: r.Time.In(in.Loc), TxID: r.id}
		if r.Type == "transfer" {
			l.Flag = tax.FlagTransferDate
		}
		lots = append(lots, l)
		used[i] += take
		spent[i] += cost
		dated[w] += take
		row.Dated += take
		datedCost += cost
	}
	for i, r := range rows {
		date(i, r.wallet)
	}
	for i := range rows {
		date(i, tax.Untracked)
	}
	nDated := len(lots)

	// 4. What is left undated in each wallet: CryptKeeper's excess over Koinly first
	// becomes missing-basis lots (or lots at the user's ExtraBasis), pro-rata over the
	// real wallets' remainders.
	rest := map[string]float64{}
	realRest := 0.0
	wallets := slices.Sorted(maps.Keys(held))
	for _, w := range wallets {
		rest[w] = math.Max(0, held[w]-dated[w])
		if w != tax.Untracked && rest[w] > tol {
			realRest += rest[w]
		}
	}
	if excess := ck - q; excess > tol {
		// Remainders within tol are float residue: they take no share (no sliver lots).
		if realRest < excess-tol*float64(len(held)+1) {
			note("only %s of CryptKeeper's %s extra units are undated; the rest keep dated lots", qty(realRest), qty(excess))
			excess = realRest
		}
		extra := math.Max(0, in.ExtraBasis[k])
		flag := tax.FlagMissingBasis
		if extra > 0 { // the user supplied their basis: a real cost, not a missing one
			flag = ""
		}
		for _, w := range wallets {
			if w == tax.Untracked || rest[w] <= tol {
				continue
			}
			share := excess * rest[w] / realRest
			lots = append(lots, tax.Lot{Wallet: w, Asset: k, Amount: share, CostUSD: extra * share / excess,
				Acquired: cutover, Flag: flag})
			rest[w] -= share
			row.MissingBasis += share
		}
	}

	// 4–5. One "before Year" lot per wallet, sharing Koinly's undated cost by quantity.
	various := len(lots)
	before := time.Date(in.Year, 1, 1, 0, 0, 0, 0, in.Loc).Add(-time.Second)
	for _, w := range wallets {
		if rest[w] > tol {
			lots = append(lots, tax.Lot{Wallet: w, Asset: k, Amount: rest[w], Acquired: before, Various: true})
			row.Various += rest[w]
		}
	}
	left := c - datedCost
	if left < 0 {
		if left < -costTol {
			outcome := "older lots set to $0"
			if row.Various == 0 {
				outcome = "dated lots kept at their cost"
			}
			note("dated %d cost exceeds Koinly's total cost; %s", in.Year, outcome)
		}
		left = 0
	}
	switch {
	case row.Various > 0:
		for i := various; i < len(lots); i++ {
			lots[i].CostUSD = left * lots[i].Amount / row.Various
		}
	case left > costTol && row.Dated > 0:
		// No older lots: the dated lots carry all of Koinly's cost, scaled by their cost
		// (by amount when they cost nothing). Missing-basis lots keep ExtraBasis.
		note("Koinly's cost exceeds the dated %d lots by $%.2f; spread across them", in.Year, left)
		for i := range lots[:nDated] {
			if datedCost > 0 {
				lots[i].CostUSD *= c / datedCost
			} else {
				lots[i].CostUSD = c * lots[i].Amount / row.Dated
			}
		}
	case left > costTol:
		note("$%.2f of Koinly's cost has no lots to carry it", left)
	}

	slices.SortStableFunc(lots, lotOrder)
	row.Lots = lots
	return row, problems
}

// qty formats a quantity for a problem message, to 9 decimal places without trailing zeros.
func qty(v float64) string {
	return strconv.FormatFloat(math.Round(v*1e9)/1e9, 'f', -1, 64)
}
