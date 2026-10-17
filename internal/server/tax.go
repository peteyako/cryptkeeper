package server

import (
	"context"
	"sync"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/tax"
)

// taxCache keeps the last tax-lot computation briefly: the overview's P&L column would
// otherwise replay all activity on every live push.
type taxCache struct {
	mu  sync.Mutex
	at  time.Time
	res tax.Result
	ok  bool
}

// invalidateTax drops the cached tax result, so the next read replays the activity.
func (s *Server) invalidateTax() {
	c := &s.taxes
	c.mu.Lock()
	c.ok = false
	c.mu.Unlock()
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
// computed before the first snapshot is not cached: its spam set (from the snapshot's
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
//   - From, Opening and OpeningParked come from the last closed year. With no closed year
//     (before a Koinly import) From is the zero time: every activity counts, with no
//     opening lots.
//   - Manual lots, tags and price overrides come from data/tax.yaml.
//   - Txs is all activity, without CSV rows the live API already synced.
//   - Spam is spamSet of the configured hidden mints and the latest snapshot.
//
// ready is false while there is no snapshot yet (the spam set is then incomplete).
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
	in = tax.Input{
		Loc:    loc,
		Manual: settings.ManualLots,
		Txs:    txs,
		Tags:   tagMap(settings.Tags),
		Prices: settings.Prices,
		Spam:   spamSet(s.Cfg.Solana.HideMints, snap),
	}
	if s.TaxYears != nil {
		last, ok := s.TaxYears.LastClosed()
		in.From = tax.OpenYearStart(last, ok, loc)
		in.Opening, in.OpeningParked = last.ClosingLots, last.ClosingParked
	}
	return in, ready, nil
}

// taxActivity is every stored activity entry, minus CSV-imported ones the live API already
// synced (see dedupeImported).
func (s *Server) taxActivity(ctx context.Context) ([]core.Tx, error) {
	txs, err := s.Activity.All(ctx, nil)
	if err != nil {
		return nil, err
	}
	typeOf := map[string]string{}
	for _, d := range s.Registry.Defs() {
		typeOf[d.ID] = d.Type
	}
	return dedupeImported(txs, typeOf), nil
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
// mint, plus the asset key of every hidden holding row in snap (spam airdrops, unpriced
// tokens). A key that some wallet also shows (snap.Assets) is not spam: a dust balance of
// a real token hidden in one wallet must not erase that token's trades everywhere.
func spamSet(hideMints []string, snap portfolio.Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, m := range hideMints {
		if m != "" {
			out[core.SPLKey(m)] = true
		}
	}
	shown := make(map[string]bool, len(snap.Assets))
	for _, a := range snap.Assets {
		shown[a.AssetKey] = true
	}
	for _, w := range snap.Wallets {
		for _, r := range w.HiddenRows {
			if !shown[r.AssetKey] {
				out[r.AssetKey] = true
			}
		}
	}
	return out
}
