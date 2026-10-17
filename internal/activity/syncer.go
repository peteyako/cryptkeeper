package activity

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"crypto-portfolio/internal/core"
)

const (
	defaultBudget   = 30
	defaultInterval = 15 * time.Minute
	defaultSince    = 90 * 24 * time.Hour // backfill horizon when Since is unset
	activityTimeout = 60 * time.Second    // per source per tick (covers all of its Activity calls)
	maxConcurrent   = 4                   // sources synced at once
	backfillCalls   = 3                   // Activity calls per tick while a source is backfilling
	matchWindowDays = 90                  // how far back transfer matching looks
)

// Entry is one source that can list activity.
type Entry struct {
	ID, Label string
	Src       core.ActivitySource
}

// Valuer prices an asset on a past day. ok is false (with a nil error) when no price is known.
// The prices service implements it.
type Valuer interface {
	Historical(ctx context.Context, symbol string, day time.Time) (usd float64, ok bool, err error)
}

// MintValuer is an optional extension of Valuer: it prices a Solana SPL token by mint
// address. The syncer uses it for legs the symbol lookup cannot price.
type MintValuer interface {
	HistoricalMint(ctx context.Context, mint string, day time.Time) (usd float64, ok bool, err error)
}

// Syncer pulls activity from every source on a schedule, persists it, values the legs and
// links transfers between the user's sources. Fields must be set before Run/SyncOnce.
type Syncer struct {
	Store    *Store
	Sources  func() []Entry   // only sources that implement core.ActivitySource
	Since    func() time.Time // backfill horizon (now - backfillDays); default 90 days ago
	Budget   int              // per source per Activity call; default 30
	Interval time.Duration    // default 15 minutes
	Log      *slog.Logger
	// Valuer, when set, fills missing Leg.USD (and Fee.USD) from historical prices. Without
	// it legs are stored as the source reported them.
	Valuer Valuer
	// SkipMints are SPL mints never valued (the Solana hidden mints: spam the price source
	// does not know, whose lookups would only spend its rate limit).
	SkipMints []string
	// OnNew, when set, is called after each pass with the entries inserted during it (oldest
	// first, already carrying any transfer link made in the same pass). Backfilled history
	// counts as new, so consumers that alert should look at the entry time.
	OnNew func(txs []core.Tx)

	timeout time.Duration // test hook: overrides activityTimeout

	revalued atomic.Int64 // txs upserted by the last revalue pass (tests)

	once sync.Once
	kick chan struct{}
	sem  chan struct{}

	mu      sync.Mutex
	running map[string]bool            // sources being synced right now
	subs    map[chan struct{}]struct{} // Subscribe channels
}

func (s *Syncer) init() {
	s.once.Do(func() {
		s.kick = make(chan struct{}, 1)
		s.sem = make(chan struct{}, 1)
	})
}

func (s *Syncer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Syncer) callTimeout() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return activityTimeout
}

// Subscribe returns a channel that receives a signal whenever a source starts or finishes
// syncing (its status in the store has changed by then), and a cancel func that must be
// called when the subscriber goes away. Signals coalesce: a slow reader gets one, not a
// backlog.
func (s *Syncer) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan struct{}]struct{}{}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			s.mu.Unlock()
		})
	}
}

// Running returns the ids of the sources being synced right now.
func (s *Syncer) Running() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.running))
	for id := range s.running {
		out[id] = true
	}
	return out
}

// setRunning marks a source as syncing or not and signals the subscribers.
func (s *Syncer) setRunning(id string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		if s.running == nil {
			s.running = map[string]bool{}
		}
		s.running[id] = true
	} else {
		delete(s.running, id)
	}
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Kick asks Run for an early pass. It never blocks and coalesces repeated kicks.
func (s *Syncer) Kick() {
	s.init()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run syncs immediately, then every Interval or on Kick, until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	s.init()
	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.SyncOnce(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("activity sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// SyncOnce makes one pass over all sources (up to four at a time), then matches transfers
// over the last 90 days of stored activity. A failing source is recorded in its sync status
// (see Store.Statuses) and never affects the others, so the returned error covers only
// cancellation and storage failures. Concurrent calls run one after another.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	s.init()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	var entries []Entry
	if s.Sources != nil {
		entries = s.Sources()
	}
	since := time.Now().Add(-defaultSince)
	if s.Since != nil {
		since = s.Since()
	}
	vals := newValueCache(s.Valuer, s.log(), s.SkipMints)

	var (
		mu    sync.Mutex
		fresh []core.Tx
		seen  = map[string]bool{}
	)
	var g errgroup.Group
	g.SetLimit(maxConcurrent)
	for _, e := range entries {
		if e.ID == "" || e.Src == nil || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		g.Go(func() error {
			s.setRunning(e.ID, true)
			defer s.setRunning(e.ID, false) // after syncSource has saved the outcome
			got := s.syncSource(ctx, e, since, vals)
			mu.Lock()
			fresh = append(fresh, got...)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait() // goroutines never return errors
	if err := ctx.Err(); err != nil {
		return err
	}

	changed, err := s.matchTransfers(ctx)
	if err != nil {
		return fmt.Errorf("match transfers: %w", err)
	}
	if s.OnNew != nil && len(fresh) > 0 {
		linked := make(map[string]core.Tx, len(changed))
		for _, t := range changed {
			linked[t.ID] = t
		}
		for i, t := range fresh {
			if c, ok := linked[t.ID]; ok {
				fresh[i] = c
			}
		}
		sort.SliceStable(fresh, func(a, b int) bool {
			if !fresh[a].Time.Equal(fresh[b].Time) {
				return fresh[a].Time.Before(fresh[b].Time)
			}
			return fresh[a].ID < fresh[b].ID
		})
		s.callOnNew(fresh)
	}
	if err := s.revalue(ctx, since, vals); err != nil {
		// The sources synced fine; a failed backfill of missing values retries next pass.
		s.log().Warn("activity revalue failed", "err", err)
	}
	return nil
}

// fiatSymbols are currencies the price source does not value as assets (USD itself is pegged).
var fiatSymbols = map[string]bool{"EUR": true, "GBP": true, "CAD": true, "JPY": true, "AUD": true, "CHF": true}

// needsValue reports whether l has no USD value but could be priced; legs of the mints in skip
// never are.
func needsValue(l *core.Leg, skip map[string]bool) bool {
	if l.USD != nil || l.Amount == 0 || l.AssetKey == "" || (l.Mint != "" && skip[l.Mint]) {
		return false
	}
	if _, spl := core.IsSPLKey(l.AssetKey); spl {
		return l.Mint != ""
	}
	return !fiatSymbols[strings.ToUpper(l.AssetKey)]
}

// revalue fills legs stored without a USD value (history from before mint pricing, or from
// a price outage) and upserts only the txs that gained one. Legs that stay unpriceable are
// left alone, so a pass with nothing to gain writes nothing.
func (s *Syncer) revalue(ctx context.Context, since time.Time, vals *valueCache) error {
	s.revalued.Store(0)
	if s.Valuer == nil {
		return nil
	}
	recent, err := s.Store.since(ctx, since)
	if err != nil {
		return err
	}
	var changed []core.Tx
	for _, t := range recent {
		missing := t.Fee != nil && needsValue(t.Fee, vals.skip)
		for i := range t.Legs {
			missing = missing || needsValue(&t.Legs[i], vals.skip)
		}
		if !missing {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		valCtx, stop := context.WithTimeout(ctx, s.callTimeout())
		before := countValued(t)
		vals.value(valCtx, &t)
		stop()
		if countValued(t) > before {
			changed = append(changed, t)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if _, err := s.Store.Upsert(ctx, changed); err != nil {
		return err
	}
	s.revalued.Store(int64(len(changed)))
	s.log().Info("activity revalued", "txs", len(changed))
	return nil
}

func countValued(t core.Tx) int {
	n := 0
	for _, l := range t.Legs {
		if l.USD != nil {
			n++
		}
	}
	if t.Fee != nil && t.Fee.USD != nil {
		n++
	}
	return n
}

func (s *Syncer) callOnNew(txs []core.Tx) {
	defer func() {
		if r := recover(); r != nil {
			s.log().Error("activity OnNew panic", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	s.OnNew(txs)
}

// matchTransfers links own-source transfers in the recent window and persists the changes.
func (s *Syncer) matchTransfers(ctx context.Context) ([]core.Tx, error) {
	recent, err := s.Store.since(ctx, time.Now().AddDate(0, 0, -matchWindowDays))
	if err != nil {
		return nil, err
	}
	changed := MatchTransfers(recent)
	if len(changed) == 0 {
		return nil, nil
	}
	if _, err := s.Store.Upsert(ctx, changed); err != nil {
		return nil, err
	}
	return changed, nil
}

// syncSource runs one source's share of a pass and returns the entries it newly inserted.
// Whatever happens inside — errors, timeouts, panics — ends up in the source's last_error
// and the call returns normally.
func (s *Syncer) syncSource(ctx context.Context, e Entry, since time.Time, vals *valueCache) (fresh []core.Tx) {
	var err error
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			s.log().Error("activity source panic", "source", e.ID, "panic", r, "stack", string(debug.Stack()))
		}
		if err == nil || ctx.Err() != nil { // a cancelled pass is shutdown, not a source failure
			return
		}
		s.log().Warn("activity sync failed", "source", e.ID, "label", e.Label, "err", err)
		if serr := s.Store.saveError(ctx, e.ID, err.Error()); serr != nil {
			s.log().Error("activity save error", "source", e.ID, "err", serr)
		}
	}()

	state, done, err := s.Store.loadState(ctx, e.ID)
	if err != nil {
		return nil
	}
	budget := s.Budget
	if budget <= 0 {
		budget = defaultBudget
	}
	callCtx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()

	calls := 1
	if !done {
		calls = backfillCalls // history fills faster on the first ticks
	}
	for i := 0; i < calls; i++ {
		res, callErr := e.Src.Activity(callCtx, core.ActivityRequest{State: state, Since: since, Budget: budget})
		// A source may fail partway and still return what it fetched plus a State that
		// resumes after it (e.g. one slow Esplora page out of many). Keep that progress;
		// without a State nothing can be trusted, so drop the result.
		if callErr != nil && res.State == nil {
			err = callErr
			return fresh
		}
		valCtx, stopVal := context.WithTimeout(ctx, s.callTimeout()) // pricing gets its own budget
		txs := s.prepare(valCtx, e, res.Txs, vals)
		stopVal()
		var added []core.Tx
		if added, err = s.Store.upsert(ctx, txs); err != nil {
			return fresh
		}
		fresh = append(fresh, added...)
		if err = s.Store.saveProgress(ctx, e.ID, res.State, res.BackfillDone && callErr == nil, time.Now()); err != nil {
			return fresh
		}
		if callErr != nil {
			err = callErr // recorded as last_error by the deferred handler, after the progress
			return fresh
		}
		state, done = res.State, res.BackfillDone
		if done {
			break
		}
	}
	return fresh
}

// prepare fills in blank identity fields, drops entries that cannot be stored and values
// the legs. The returned entries do not share leg slices with the source's results.
func (s *Syncer) prepare(ctx context.Context, e Entry, in []core.Tx, vals *valueCache) []core.Tx {
	out := make([]core.Tx, 0, len(in))
	for _, t := range in {
		if t.SourceID == "" {
			t.SourceID = e.ID
		}
		if t.ID == "" && t.ExternalID != "" {
			t.ID = core.TxID(t.SourceID, t.ExternalID)
		}
		if t.ID == "" {
			s.log().Warn("activity entry without id dropped", "source", e.ID)
			continue
		}
		vals.value(ctx, &t)
		out = append(out, t)
	}
	return out
}

// valueKey identifies one price lookup within a pass.
type valueKey struct {
	symbol string
	day    time.Time
}

type valueResult struct {
	once sync.Once
	usd  float64
	ok   bool
}

// valueCache values legs for one pass, asking the Valuer at most once per (symbol, day).
type valueCache struct {
	valuer Valuer
	log    *slog.Logger
	skip   map[string]bool // mints never valued
	mu     sync.Mutex
	m      map[valueKey]*valueResult
}

func newValueCache(v Valuer, log *slog.Logger, skipMints []string) *valueCache {
	skip := make(map[string]bool, len(skipMints))
	for _, m := range skipMints {
		skip[m] = true
	}
	return &valueCache{valuer: v, log: log, skip: skip, m: map[valueKey]*valueResult{}}
}

// value sets USD on t's legs and fee where it is missing. Legs of the SPL tokens that the
// price source does not know ("spl:<mint>") are skipped; pegged stablecoins are worth their
// amount; the rest use the closing price of the UTC day of the entry. Valuation problems
// leave USD unset and never surface as errors. t.Legs is replaced by a copy before writing.
func (c *valueCache) value(ctx context.Context, t *core.Tx) {
	if c.valuer == nil {
		return
	}
	day := t.Time.UTC().Truncate(24 * time.Hour)
	if len(t.Legs) > 0 {
		legs := make([]core.Leg, len(t.Legs))
		copy(legs, t.Legs)
		for i := range legs {
			c.fill(ctx, &legs[i], day)
		}
		t.Legs = legs
	}
	if t.Fee != nil {
		fee := *t.Fee
		c.fill(ctx, &fee, day)
		t.Fee = &fee
	}
}

func (c *valueCache) fill(ctx context.Context, l *core.Leg, day time.Time) {
	if l.USD != nil || l.Amount == 0 || l.AssetKey == "" || (l.Mint != "" && c.skip[l.Mint]) {
		return
	}
	var (
		price float64
		ok    bool
	)
	if _, spl := core.IsSPLKey(l.AssetKey); !spl {
		if core.Pegged[strings.ToUpper(l.AssetKey)] {
			usd := l.Amount
			l.USD = &usd
			return
		}
		price, ok = c.price(ctx, l.AssetKey, day)
	}
	if !ok && l.Mint != "" {
		price, ok = c.mintPrice(ctx, l.Mint, day)
	}
	if !ok {
		return
	}
	usd := l.Amount * price
	l.USD = &usd
}

// mintPrice returns the cached price for (mint, day) from the valuer's MintValuer side, if any.
// Mint addresses are case-sensitive, so the key is not upper-cased.
func (c *valueCache) mintPrice(ctx context.Context, mint string, day time.Time) (float64, bool) {
	mv, isMint := c.valuer.(MintValuer)
	if !isMint {
		return 0, false
	}
	key := valueKey{"mint:" + mint, day}
	c.mu.Lock()
	r := c.m[key]
	if r == nil {
		r = &valueResult{}
		c.m[key] = r
	}
	c.mu.Unlock()
	r.once.Do(func() {
		defer func() {
			if p := recover(); p != nil {
				c.log.Warn("activity mint valuer panic", "mint", mint, "panic", p)
				r.ok = false
			}
		}()
		usd, ok, err := mv.HistoricalMint(ctx, mint, day)
		if err != nil {
			c.log.Debug("activity mint valuation failed", "mint", mint, "day", day.Format("2006-01-02"), "err", err)
			return
		}
		r.usd, r.ok = usd, ok
	})
	return r.usd, r.ok
}

// price returns the cached closing price for (symbol, day), asking the Valuer on first use.
func (c *valueCache) price(ctx context.Context, symbol string, day time.Time) (float64, bool) {
	key := valueKey{strings.ToUpper(symbol), day}
	c.mu.Lock()
	r := c.m[key]
	if r == nil {
		r = &valueResult{}
		c.m[key] = r
	}
	c.mu.Unlock()
	r.once.Do(func() {
		defer func() {
			if p := recover(); p != nil {
				c.log.Warn("activity valuer panic", "symbol", symbol, "panic", p)
				r.ok = false
			}
		}()
		usd, ok, err := c.valuer.Historical(ctx, symbol, day)
		if err != nil {
			c.log.Debug("activity valuation failed", "symbol", symbol, "day", day.Format("2006-01-02"), "err", err)
			return
		}
		r.usd, r.ok = usd, ok
	})
	return r.usd, r.ok
}
