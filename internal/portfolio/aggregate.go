package portfolio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/store"
)

// PriceReq asks for one asset's price. Mint is set for SPL tokens (mapped or not) so the
// pricer can fall back to Jupiter.
type PriceReq struct{ Key, Mint string }

// PriceFunc returns prices keyed by PriceReq.Key; a missing key means no price.
type PriceFunc func(ctx context.Context, reqs []PriceReq) map[string]core.Price

// Wallet is one source to fetch, with the display facts the snapshot needs.
type Wallet struct {
	ID, Label, Type string
	Description     string // what the wallet is for, as the user wrote it
	Kind            core.Kind
	Source          core.Source // nil when the source failed to build
	BuildErr        error
}

// Aggregator fetches every wallet, prices holdings and builds a Snapshot.
type Aggregator struct {
	Wallets func() []Wallet
	Prices  PriceFunc
	// CachedPrices prices a warm start from stored prices, without network calls; optional.
	CachedPrices PriceFunc
	DB           *store.DB // last-good wallet cache; may be nil in tests
	StaleMax     time.Duration
	FetchTimeout time.Duration
	// HideUnpriced moves Solana tokens without a price, or worth less than MinUSD, into Hidden.
	HideUnpriced bool
	MinUSD       float64
	Now          func() time.Time

	mu         sync.Mutex
	last       map[string]walletState // each wallet as the last finished build left it
	lastPrices map[string]core.Price  // the live prices of the last finished build
}

// walletState is one wallet's result and status in a build.
type walletState struct {
	res    core.WalletResult
	status Status
}

func (a *Aggregator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Build runs one full refresh.
func (a *Aggregator) Build(ctx context.Context) Snapshot { return a.BuildProgress(ctx, nil) }

// BuildProgress runs one full refresh like Build, and calls progress (when set) with what is
// known so far: once at the start, then whenever wallets finish while others still fetch, so
// one slow wallet doesn't hold back the rest. A wallet not done yet shows what it showed
// before (see known); these snapshots are priced without network calls and are Warm. The
// returned snapshot is the finished one, priced live. A build cancelled partway (a newer one
// replaced it) is not remembered and not priced: its result is dropped.
func (a *Aggregator) BuildProgress(ctx context.Context, progress func(Snapshot)) Snapshot {
	wallets := a.Wallets()
	results := make([]core.WalletResult, len(wallets))
	status := make([]Status, len(wallets))
	for i, w := range wallets {
		results[i], status[i] = a.known(ctx, w.ID)
	}
	type fetched struct {
		i int
		walletState
	}
	done := make(chan fetched, len(wallets))
	for i, w := range wallets {
		go func() {
			res, st := a.fetchOne(ctx, w)
			done <- fetched{i, walletState{res, st}}
		}()
	}
	if progress != nil {
		progress(a.inProgress(ctx, wallets, results, status))
	}
	left := len(wallets)
	take := func(f fetched) {
		results[f.i], status[f.i] = f.res, f.status
		left--
	}
	for left > 0 {
		take(<-done)
		for more := true; more && left > 0; { // take the others that finished meanwhile too
			select {
			case f := <-done:
				take(f)
			default:
				more = false
			}
		}
		if progress != nil && left > 0 {
			progress(a.inProgress(ctx, wallets, results, status))
		}
	}

	prices := map[string]core.Price{}
	if errors.Is(ctx.Err(), context.Canceled) {
		return assemble(wallets, results, status, prices, a.HideUnpriced, a.MinUSD, a.now())
	}
	if reqs := PriceRequests(results); len(reqs) > 0 && a.Prices != nil {
		prices = a.Prices(ctx, reqs)
	}
	a.remember(wallets, results, status, prices)
	return assemble(wallets, results, status, prices, a.HideUnpriced, a.MinUSD, a.now())
}

// known is what a wallet showed before this build: its result and status from the last
// finished build, else its last good result (pending), else nothing yet (pending).
func (a *Aggregator) known(ctx context.Context, id string) (core.WalletResult, Status) {
	a.mu.Lock()
	st, ok := a.last[id]
	a.mu.Unlock()
	if ok {
		return st.res, st.status
	}
	if lg, ok := a.loadLastGood(ctx, id); ok && a.now().Sub(lg.AsOf) <= a.StaleMax {
		return lg, StatusPending
	}
	return core.WalletResult{WalletID: id}, StatusPending
}

// inProgress is the Warm snapshot of a build still running, priced without network calls:
// the last finished build's prices, then CachedPrices for the rest.
func (a *Aggregator) inProgress(ctx context.Context, wallets []Wallet, results []core.WalletResult, status []Status) Snapshot {
	prices := map[string]core.Price{}
	var missing []PriceReq
	a.mu.Lock()
	for _, req := range PriceRequests(results) {
		if p, ok := a.lastPrices[req.Key]; ok {
			prices[req.Key] = p
		} else {
			missing = append(missing, req)
		}
	}
	a.mu.Unlock()
	if len(missing) > 0 && a.CachedPrices != nil {
		for k, p := range a.CachedPrices(ctx, missing) {
			prices[k] = p
		}
	}
	s := assemble(wallets, results, status, prices, a.HideUnpriced, a.MinUSD, a.now())
	s.Warm = true
	return s
}

// remember keeps a finished build's wallets and prices for the next build's progress.
func (a *Aggregator) remember(wallets []Wallet, results []core.WalletResult, status []Status, prices map[string]core.Price) {
	last := make(map[string]walletState, len(wallets))
	for i, w := range wallets {
		last[w.ID] = walletState{results[i], status[i]}
	}
	a.mu.Lock()
	a.last, a.lastPrices = last, prices
	a.mu.Unlock()
}

// Warm builds a snapshot from the last session, without fetching anything: each wallet's
// last good result (within StaleMax) priced by CachedPrices. After a restart, pages show
// balances at once from it while the first live refresh runs. Its wallets are pending and
// its time is the newest result's. ok is false when no wallet has a usable last result.
func (a *Aggregator) Warm(ctx context.Context) (s Snapshot, ok bool) {
	wallets := a.Wallets()
	results := make([]core.WalletResult, len(wallets))
	status := make([]Status, len(wallets))
	var newest time.Time
	for i, w := range wallets {
		results[i], status[i] = core.WalletResult{WalletID: w.ID}, StatusPending
		if lg, ok := a.loadLastGood(ctx, w.ID); ok && a.now().Sub(lg.AsOf) <= a.StaleMax {
			results[i] = lg
			if lg.AsOf.After(newest) {
				newest = lg.AsOf
			}
		}
	}
	if newest.IsZero() {
		return Snapshot{}, false
	}
	prices := map[string]core.Price{}
	if reqs := PriceRequests(results); len(reqs) > 0 && a.CachedPrices != nil {
		prices = a.CachedPrices(ctx, reqs)
	}
	s = assemble(wallets, results, status, prices, a.HideUnpriced, a.MinUSD, newest)
	s.Warm = true
	return s, true
}

func (a *Aggregator) fetchOne(ctx context.Context, w Wallet) (core.WalletResult, Status) {
	if w.Source == nil {
		msg := "source not configured"
		if w.BuildErr != nil {
			msg = w.BuildErr.Error()
		}
		return core.WalletResult{WalletID: w.ID, Err: msg, AsOf: a.now()}, StatusError
	}
	timeout := a.FetchTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := safeFetch(fctx, w.Source)
	res.WalletID = w.ID
	if err == nil {
		if res.AsOf.IsZero() {
			res.AsOf = a.now()
		}
		a.saveLastGood(ctx, res)
		return res, StatusSynced
	}
	if lg, ok := a.loadLastGood(ctx, w.ID); ok && a.now().Sub(lg.AsOf) <= a.StaleMax {
		lg.Stale = true
		lg.Err = err.Error()
		return lg, StatusStale
	}
	return core.WalletResult{WalletID: w.ID, Err: err.Error(), AsOf: a.now()}, StatusError
}

// safeFetch turns a panicking source into an error so one bad source can't kill refreshes.
func safeFetch(ctx context.Context, s core.Source) (res core.WalletResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return s.Fetch(ctx)
}

func (a *Aggregator) saveLastGood(ctx context.Context, res core.WalletResult) {
	if a.DB == nil {
		return
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return
	}
	a.DB.ExecContext(ctx, `INSERT INTO lastgood_wallet (wallet_id, json, as_of) VALUES (?, ?, ?)
		ON CONFLICT(wallet_id) DO UPDATE SET json = excluded.json, as_of = excluded.as_of`, res.WalletID, string(raw), res.AsOf.Unix())
}

func (a *Aggregator) loadLastGood(ctx context.Context, id string) (core.WalletResult, bool) {
	if a.DB == nil {
		return core.WalletResult{}, false
	}
	var raw string
	err := a.DB.QueryRowContext(ctx, `SELECT json FROM lastgood_wallet WHERE wallet_id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return core.WalletResult{}, false
	}
	var res core.WalletResult
	if json.Unmarshal([]byte(raw), &res) != nil {
		return core.WalletResult{}, false
	}
	return res, true
}

// PriceRequests lists the distinct assets in results, most important first.
func PriceRequests(results []core.WalletResult) []PriceReq {
	idx := map[string]int{}
	unnamed := map[string]bool{}
	var out []PriceReq
	add := func(h core.Holding) {
		if h.AssetKey == "" {
			return
		}
		if strings.Contains(h.Symbol, "…") {
			unnamed[h.AssetKey] = true
		}
		if i, ok := idx[h.AssetKey]; ok {
			if out[i].Mint == "" {
				out[i].Mint = h.Mint // a Solana holding of the same asset supplies the mint
			}
			return
		}
		idx[h.AssetKey] = len(out)
		out = append(out, PriceReq{Key: h.AssetKey, Mint: h.Mint})
	}
	for _, r := range results {
		for _, h := range r.Holdings {
			add(h)
		}
		for _, h := range r.Hidden {
			add(h)
		}
	}
	// Price what matters first: exchange symbols, then named SPL tokens, then unnamed mints
	// (spam airdrops show a shortened mint "AbCd…wXyZ"). Pricers cap work per call.
	rank := func(r PriceReq) int {
		_, spl := core.IsSPLKey(r.Key)
		switch {
		case !spl:
			return 0
		case !unnamed[r.Key]:
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// assemble is pure: wallets + results + prices → Snapshot.
func assemble(wallets []Wallet, results []core.WalletResult, status []Status, prices map[string]core.Price, hideUnpriced bool, minUSD float64, now time.Time) Snapshot {
	views := make([]WalletView, 0, len(wallets))
	for i, w := range wallets {
		res := results[i]
		wv := WalletView{
			ID: w.ID, Label: w.Label, Description: w.Description, Type: w.Type, Kind: w.Kind,
			NFTCount: res.NFTCount, Warnings: res.Warnings, Meta: res.Meta,
			Status: status[i], Err: res.Err, AsOf: res.AsOf,
		}
		for _, h := range res.Hidden {
			wv.HiddenRows = append(wv.HiddenRows, PriceRow(h, prices))
		}
		for _, h := range res.Holdings {
			row := PriceRow(h, prices)
			if hideUnpriced && w.Type == "solana" && isSpamCandidate(row, minUSD) {
				wv.HiddenRows = append(wv.HiddenRows, row)
				continue
			}
			wv.Rows = append(wv.Rows, row)
		}
		sort.SliceStable(wv.Rows, func(a, b int) bool { return wv.Rows[a].ValueUSD > wv.Rows[b].ValueUSD })
		sort.SliceStable(wv.HiddenRows, func(a, b int) bool { return wv.HiddenRows[a].ValueUSD > wv.HiddenRows[b].ValueUSD })
		views = append(views, wv)
	}
	return FromWallets(views, prices, now)
}

// FromWallets builds a Snapshot from wallet views whose Rows are already priced: wallet
// values, the total, assets merged across wallets with their shares, and the stale flag.
// The aggregator uses it for "all holdings"; portfolio filters use it on a subset of rows.
func FromWallets(views []WalletView, prices map[string]core.Price, now time.Time) Snapshot {
	snap := Snapshot{UpdatedAt: now, Prices: prices}
	assets := map[string]*AssetRow{}
	var order []string
	for _, wv := range views {
		if wv.Status == StatusStale {
			snap.HasStale = true
		}
		wv.ValueUSD = 0
		for _, row := range wv.Rows {
			if row.Price != nil && row.Price.Stale {
				snap.HasStale = true
			}
			wv.ValueUSD += row.ValueUSD
			ar, ok := assets[row.AssetKey]
			if !ok {
				ar = &AssetRow{AssetKey: row.AssetKey, Symbol: row.Symbol, Name: row.Name, Mint: row.Mint, LogoURL: row.LogoURL}
				if p, ok := prices[row.AssetKey]; ok {
					pc := p
					ar.Price = &pc
				}
				assets[row.AssetKey] = ar
				order = append(order, row.AssetKey)
			}
			if ar.Name == "" {
				ar.Name = row.Name
			}
			ar.Amount += row.Amount
			if row.Staked {
				ar.StakedAmt += row.Amount
			}
			ar.ValueUSD += row.ValueUSD
			ar.Priced = ar.Priced || row.Priced
			if !slices.Contains(ar.HeldIn, wv.Label) {
				ar.HeldIn = append(ar.HeldIn, wv.Label)
			}
		}
		snap.TotalUSD += wv.ValueUSD
		snap.Wallets = append(snap.Wallets, wv)
	}
	for _, k := range order {
		ar := assets[k]
		if snap.TotalUSD > 0 {
			ar.Share = ar.ValueUSD / snap.TotalUSD
		}
		snap.Assets = append(snap.Assets, *ar)
	}
	sort.SliceStable(snap.Assets, func(i, j int) bool {
		if snap.Assets[i].Priced != snap.Assets[j].Priced {
			return snap.Assets[i].Priced
		}
		return snap.Assets[i].ValueUSD > snap.Assets[j].ValueUSD
	})
	sort.SliceStable(snap.Wallets, func(i, j int) bool { return snap.Wallets[i].ValueUSD > snap.Wallets[j].ValueUSD })
	return snap
}

// PriceRow applies a price (if any) to a holding.
func PriceRow(h core.Holding, prices map[string]core.Price) HoldingRow {
	row := HoldingRow{Holding: h}
	if p, ok := prices[h.AssetKey]; ok {
		pc := p
		row.Price = &pc
		row.Priced = true
		row.ValueUSD = h.Amount * p.USD
	}
	return row
}

// isSpamCandidate reports whether a Solana token row belongs in the collapsed "hidden
// tokens" list: unverified (scam airdrops often carry a fake price), unpriced, or dust.
// SOL itself and DeFi position legs are never hidden.
func isSpamCandidate(row HoldingRow, minUSD float64) bool {
	if row.Mint == "" || row.AssetKey == "SOL" || row.Protocol != "" {
		return false
	}
	return !row.Verified || !row.Priced || row.ValueUSD < minUSD
}
