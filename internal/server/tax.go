package server

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/tax/report"
)

// taxCache keeps the last tax-lot computation briefly: the overview's P&L column would
// otherwise replay all activity on every live push.
type taxCache struct {
	mu  sync.Mutex
	at  time.Time
	res tax.Result
	ok  bool

	// years caches the report of an earlier open year (computed up to its end): it is
	// expensive and, like res, only valid until the next invalidateTax or a minute.
	ymu   sync.Mutex
	years map[int]cachedYear
}

type cachedYear struct {
	at  time.Time
	rep report.YearReport
}

type activityKey struct{}

// activityHolder loads the stored activity once for the request that carries it.
type activityHolder struct {
	once sync.Once
	txs  []core.Tx
	err  error
}

// withActivity marks ctx so every allActivity under it shares one Activity.All read.
func (s *Server) withActivity(ctx context.Context) context.Context {
	if ctx.Value(activityKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, activityKey{}, &activityHolder{})
}

// allActivity is every stored activity entry; under a withActivity context it is read once.
// Callers must not modify the result.
func (s *Server) allActivity(ctx context.Context) ([]core.Tx, error) {
	h, ok := ctx.Value(activityKey{}).(*activityHolder)
	if !ok {
		return s.Activity.All(ctx, nil)
	}
	h.once.Do(func() { h.txs, h.err = s.Activity.All(ctx, nil) })
	return h.txs, h.err
}

// invalidateTax drops the cached tax result, so the next read replays the activity.
func (s *Server) invalidateTax() {
	c := &s.taxes
	c.mu.Lock()
	c.ok = false
	c.mu.Unlock()
	c.ymu.Lock()
	c.years = nil
	c.ymu.Unlock()
}

// taxLoc is the tax timezone; time.Local when none was wired.
func (s *Server) taxLoc() *time.Location {
	if s.TaxLoc == nil {
		return time.Local
	}
	return s.TaxLoc
}

// taxResult computes the open tax year's lots, disposals and income with tax.Compute, cached
// for a minute. ok is false when there is no activity store or it cannot be read. A result
// computed before the first snapshot is not cached: its quiet set (from the snapshot's
// hidden rows) is still incomplete.
func (s *Server) taxResult(ctx context.Context) (tax.Result, bool) {
	if s.Activity == nil {
		return tax.Result{}, false
	}
	c := &s.taxes
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && s.Now().Sub(c.at) < time.Minute {
		return c.res, true
	}
	in, ready, err := s.taxInput(ctx)
	if err != nil {
		s.Log.Warn("tax lots: load activity", "err", err)
		return tax.Result{}, false
	}
	c.res, c.at, c.ok = tax.Compute(in), s.Now(), ready
	return c.res, true
}

// taxInput assembles the engine's input:
//
//   - From, Opening, OpeningParked and PairedBeforeFrom (its CarriedPeers) come from the
//     last closed year. With no closed year (before a Koinly import) From is the zero
//     time: every activity counts, with no opening lots. A Koinly year carries no peers.
//   - Manual lots, tags and price overrides come from data/tax.yaml.
//   - Txs is taxActivity: all activity, without CSV rows the live API already synced, the
//     rest counted as their exchange's wallet when there is one.
//   - Spam is spamSet of the configured hidden mints; Quiet is quietSet of the latest
//     snapshot (the tokens hidden automatically).
//   - With a tax scope (a portfolio, see taxScope), only its wallets count: see
//     applyTaxScope.
//
// ready is false until the first live snapshot (before it, the quiet set is incomplete or the
// last session's).
func (s *Server) taxInput(ctx context.Context) (in tax.Input, ready bool, err error) {
	txs, err := s.taxActivity(ctx)
	if err != nil {
		return tax.Input{}, false, err
	}
	loc := s.taxLoc()
	var settings tax.Settings
	if s.TaxSettings != nil {
		settings = s.TaxSettings.Get()
	}
	snap, ready := s.latest()
	ready = ready && !snap.Warm // the last session's snapshot isn't enough to close a year on
	spam := spamSet(s.Cfg.Solana.HideMints)
	in = tax.Input{
		Loc:       loc,
		Manual:    settings.ManualLots,
		WriteOffs: settings.WriteOffs,
		Txs:       txs,
		Tags:      tagMap(settings.Tags),
		Prices:    settings.Prices,
		Spam:      spam,
		Quiet:     quietSet(snap, spam),
	}
	if s.TaxYears != nil {
		last, ok := s.TaxYears.LastClosed()
		in.From = tax.OpenYearStart(last, ok, loc)
		in.Opening, in.OpeningParked = s.csvOpening(last.ClosingLots, last.ClosingParked)
		if len(last.CarriedPeers) > 0 {
			in.PairedBeforeFrom = make(map[string]bool, len(last.CarriedPeers))
			for _, id := range last.CarriedPeers {
				in.PairedBeforeFrom[id] = true
			}
		}
	}
	if covers, _, scoped := s.taxScope(); scoped {
		applyTaxScope(&in, covers)
	}
	return in, ready, nil
}

// taxScope is the portfolio the tax reports cover (tax.yaml's scope): covers reports
// whether a wallet counts, and name is the portfolio's. scoped is false (every wallet counts)
// without a scope, when its portfolio no longer exists, or when it holds every wallet.
// Only its wallets matter, not an asset filter. Wallets that aren't sources (the untracked
// one, CSV pseudo-wallets) always count.
func (s *Server) taxScope() (covers func(wallet string) bool, name string, scoped bool) {
	if s.TaxSettings == nil || s.Portfolios == nil {
		return nil, "", false
	}
	id := s.TaxSettings.Get().Scope
	if id == "" {
		return nil, "", false
	}
	pf, ok := s.Portfolios.Get(id)
	if !ok || pf.Wallets.All {
		return nil, pf.Name, false
	}
	sources := map[string]bool{}
	for _, d := range s.Registry.Defs() {
		sources[d.ID] = true
	}
	return func(w string) bool { return !sources[w] || pf.Wallets.Has(w) }, pf.Name, true
}

// taxScoped is snap with only the wallets the tax scope covers (snap itself without one).
func (s *Server) taxScoped(snap portfolio.Snapshot) portfolio.Snapshot {
	covers, _, scoped := s.taxScope()
	if !scoped {
		return snap
	}
	var ids []string
	for _, w := range snap.Wallets {
		if covers(w.ID) {
			ids = append(ids, w.ID)
		}
	}
	return portfolios.Apply(portfolios.Portfolio{Wallets: portfolios.Selector{IDs: ids}, Assets: portfolios.AllSelector()}, snap)
}

// applyTaxScope leaves the wallets covers rejects out of the tax entirely: their activity,
// manual lots, write-offs and opening lots and positions. A transfer between a covered wallet
// and a left-out one keeps its covered half, tagged own-wallet unless the user tagged it:
// coins sent there move to the untracked wallet, and coins received from there draw on it,
// as a move between the user's own wallets rather than a sale or a purchase.
func applyTaxScope(in *tax.Input, covers func(string) bool) {
	in.Txs = slices.DeleteFunc(slices.Clone(in.Txs), func(tx core.Tx) bool { return !covers(tx.SourceID) })
	tags := maps.Clone(in.Tags)
	if tags == nil {
		tags = map[string]tax.Tag{}
	}
	for _, tx := range in.Txs {
		if tx.PeerID == "" || tx.PeerSource == "" || covers(tx.PeerSource) {
			continue
		}
		if _, tagged := tags[tx.ID]; !tagged {
			tags[tx.ID] = tax.TagOwnWallet
		}
	}
	in.Tags = tags
	in.Manual = slices.DeleteFunc(slices.Clone(in.Manual), func(l tax.Lot) bool { return !covers(l.Wallet) })
	in.WriteOffs = slices.DeleteFunc(slices.Clone(in.WriteOffs), func(w tax.WriteOff) bool { return !covers(w.Wallet) })
	in.Opening = slices.DeleteFunc(slices.Clone(in.Opening), func(l tax.Lot) bool { return !covers(l.Wallet) })
	in.OpeningParked = slices.DeleteFunc(slices.Clone(in.OpeningParked), func(p tax.Parked) bool { return !covers(p.Wallet) })
}

// taxActivity is every stored activity entry, minus CSV-imported ones the live API already
// synced (see dedupeImported). The remaining CSV rows count as the wallet of their exchange
// when exactly one live source has its type (see csvTargets): their SourceID becomes that
// source's (their ID is kept, so tags and price overrides still find them). The engine and
// the Koinly roll-back both read this, so CSV buys fund the live exchange's sales.
func (s *Server) taxActivity(ctx context.Context) ([]core.Tx, error) {
	txs, err := s.allActivity(ctx)
	if err != nil {
		return nil, err
	}
	typeOf := s.liveSourceTypes()
	out := dedupeImported(txs, typeOf) // a fresh slice: txs is shared and stays untouched
	targets := csvTargets(typeOf)
	if len(targets) == 0 {
		return out, nil
	}
	for i := range out {
		if w, ok := targets[out[i].SourceID]; ok {
			out[i].SourceID = w
		}
		if w, ok := targets[out[i].PeerSource]; ok {
			out[i].PeerSource = w
		}
	}
	return out, nil
}

// csvOpening is a closed year's closing lots and parked balances with the CSV pseudo-wallets
// moved to their live exchange wallet like the CSV txs (see taxActivity). The year's own
// slices are not modified.
func (s *Server) csvOpening(lots []tax.Lot, parked []tax.Parked) ([]tax.Lot, []tax.Parked) {
	targets := csvTargets(s.liveSourceTypes())
	if len(targets) == 0 {
		return lots, parked
	}
	lots, parked = slices.Clone(lots), slices.Clone(parked)
	for i := range lots {
		if w, ok := targets[lots[i].Wallet]; ok {
			lots[i].Wallet = w
		}
	}
	for i := range parked {
		if w, ok := targets[parked[i].Wallet]; ok {
			parked[i].Wallet = w
		}
	}
	return lots, parked
}

// liveSourceTypes is the type of every registered source, by ID.
func (s *Server) liveSourceTypes() map[string]string {
	typeOf := map[string]string{}
	for _, d := range s.Registry.Defs() {
		typeOf[d.ID] = d.Type
	}
	return typeOf
}

// csvTargets maps each CSV pseudo-source (coinbase-csv, kraken-csv) to the live source of
// its exchange type when there is exactly one; with none or several it stays apart.
func csvTargets(typeOf map[string]string) map[string]string {
	byType := map[string][]string{}
	for id, t := range typeOf {
		byType[t] = append(byType[t], id)
	}
	out := map[string]string{}
	for csv, t := range importedSourceType {
		if ids := byType[t]; len(ids) == 1 {
			out[csv] = ids[0]
		}
	}
	return out
}

// withoutSpam is txs as the engine sees them for the transaction history: txs tagged spam are
// left out, the configured hidden mints' legs and fee are dropped, and a tx left with
// neither legs nor fee is left out. A spam tag on a transfer half counts only while the half
// is unpaired, as in the engine (tagOf); on a paired half it is a leftover and is ignored.
// txs is not modified.
func (s *Server) withoutSpam(txs []core.Tx) []core.Tx {
	spam := spamSet(s.Cfg.Solana.HideMints)
	var tags map[string]tax.TagEntry
	if s.TaxSettings != nil {
		tags = s.TaxSettings.Get().Tags
	}
	byID := make(map[string]core.Tx, len(txs))
	for _, tx := range txs {
		if tx.PeerID != "" && tx.Status != core.TxFailed {
			byID[tx.ID] = tx
		}
	}
	// unpaired mirrors the engine: no usable peer in txs, or one that does not link back.
	unpaired := func(tx core.Tx) bool {
		peer, ok := byID[tx.PeerID]
		return !ok || peer.ID == tx.ID || peer.PeerID != tx.ID
	}
	out := make([]core.Tx, 0, len(txs))
	for _, tx := range txs {
		if tags[tx.ID].Tag == tax.TagSpam && (tx.PeerID == "" || (tx.Status != core.TxFailed && unpaired(tx))) {
			continue
		}
		if slices.ContainsFunc(tx.Legs, func(l core.Leg) bool { return spam[l.AssetKey] }) {
			tx.Legs = slices.DeleteFunc(slices.Clone(tx.Legs), func(l core.Leg) bool { return spam[l.AssetKey] })
		}
		if tx.Fee != nil && spam[tx.Fee.AssetKey] {
			tx.Fee = nil
		}
		if len(tx.Legs) == 0 && tx.Fee == nil {
			continue
		}
		out = append(out, tx)
	}
	return out
}

// tagMap is the engine's view of the tax.yaml tags: tx ID → tag.
func tagMap(entries map[string]tax.TagEntry) map[string]tax.Tag {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]tax.Tag, len(entries))
	for id, e := range entries {
		out[id] = e.Tag
	}
	return out
}

// spamSet is the asset keys the tax engine ignores: spl:<mint> for every configured hidden
// mint. Txs the user tagged spam are ignored by their tag; nothing else is dropped.
func spamSet(hideMints []string) map[string]bool {
	out := map[string]bool{}
	for _, m := range hideMints {
		if m != "" {
			out[core.SPLKey(m)] = true
		}
	}
	return out
}

// quietSet is the asset keys hidden automatically (unverified, unpriced or dust tokens): the
// key of every hidden holding row in snap that no wallet shows (snap.Assets) and that is not
// spam. The engine still processes them in full, but a worthless receive of one (an airdrop)
// raises no review item (tax.Input.Quiet). The set moves as balances change, which is why it
// may only ever silence issues, never drop a leg.
func quietSet(snap portfolio.Snapshot, spam map[string]bool) map[string]bool {
	shown := make(map[string]bool, len(snap.Assets))
	for _, a := range snap.Assets {
		shown[a.AssetKey] = true
	}
	out := map[string]bool{}
	for _, w := range snap.Wallets {
		for _, r := range w.HiddenRows {
			if !shown[r.AssetKey] && !spam[r.AssetKey] {
				out[r.AssetKey] = true
			}
		}
	}
	return out
}
