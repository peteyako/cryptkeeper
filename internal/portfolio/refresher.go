package portfolio

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrTooSoon is returned by a forced refresh within MinForceInterval of the previous one.
var ErrTooSoon = errors.New("refreshed moments ago")

// Refresher keeps the latest Snapshot fresh in the background and publishes every new
// one to the broker, including a refresh's progress as wallets finish (Warm snapshots), so
// one slow wallet doesn't hold back the others. Page loads never wait on slow APIs.
type Refresher struct {
	Agg      *Aggregator
	Broker   *Broker
	History  *History // optional
	Interval time.Duration
	// PriceInterval, when shorter than Interval, reprices the last balances in between
	// (Aggregator.Reprice), so prices refresh more often than the wallets are fetched. Set it
	// before Run; SetPriceInterval changes it after.
	PriceInterval    time.Duration
	MinForceInterval time.Duration // default 30s
	Log              *slog.Logger
	// OnSnapshot runs after each refresh and reprice (the alerts engine hooks in here).
	OnSnapshot func(Snapshot)

	sf          singleflight.Group
	mu          sync.RWMutex
	latest      *Snapshot
	lastForced  time.Time
	lastSuccess time.Time
	kick        chan struct{}
	priceSet    chan struct{}      // SetPriceInterval tells Run to restart its price ticker
	gen         uint64             // bumped by Kick: a build publishes only while it is the newest
	cancelBuild context.CancelFunc // stops the build in progress when a kick replaces it
}

// Run refreshes immediately and then every Interval until ctx is done, repricing every
// PriceInterval in between. Kick() triggers an early refresh (e.g. after a source is added).
// Until the first refresh finishes, the last session's balances (Aggregator.Warm) stand in,
// so pages don't wait on slow APIs after a restart; that snapshot isn't recorded, alerted on
// or counted as a sync.
func (r *Refresher) Run(ctx context.Context) {
	r.mu.Lock()
	if r.kick == nil {
		r.kick = make(chan struct{}, 1)
	}
	kick := r.kick
	r.priceSet = make(chan struct{}, 1)
	priceSet := r.priceSet
	r.mu.Unlock()
	if s, ok := r.Agg.Warm(ctx); ok {
		r.mu.Lock()
		if r.latest == nil {
			r.latest = &s
		}
		r.mu.Unlock()
		if r.Broker != nil {
			r.Broker.Publish(s)
		}
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	var (
		pt        *time.Ticker
		priceTick <-chan time.Time // nil: prices refresh only with the wallets
	)
	startPrices := func() {
		if pt != nil {
			pt.Stop()
			pt, priceTick = nil, nil
		}
		if every := r.PriceEvery(); every < r.Interval {
			pt = time.NewTicker(every)
			priceTick = pt.C
		}
	}
	startPrices()
	defer func() {
		if pt != nil {
			pt.Stop()
		}
	}()
	r.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refresh(ctx)
		case <-kick:
			r.refresh(ctx)
		case <-priceTick:
			go r.reprice(ctx, false) // never delays a kick
		case <-priceSet:
			startPrices()
		}
	}
}

// PriceEvery is how often prices refresh: PriceInterval, but at least with every refresh.
func (r *Refresher) PriceEvery() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.priceEvery()
}

// priceEvery is PriceEvery for a caller holding r.mu.
func (r *Refresher) priceEvery() time.Duration {
	if r.PriceInterval > 0 && r.PriceInterval < r.Interval {
		return r.PriceInterval
	}
	return r.Interval
}

// SetPriceInterval changes PriceInterval (Settings); a running Run restarts its price ticker.
func (r *Refresher) SetPriceInterval(d time.Duration) {
	r.mu.Lock()
	r.PriceInterval = d
	ch := r.priceSet
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Kick schedules a refresh as soon as possible. A refresh in progress is out of date (a
// source was added, changed or removed, or a backup restored): it stops without publishing
// anything more, and the new one starts at once instead of waiting for its slow wallets.
func (r *Refresher) Kick() {
	r.mu.Lock()
	if r.kick == nil {
		r.kick = make(chan struct{}, 1)
	}
	k := r.kick
	r.gen++
	if r.cancelBuild != nil {
		r.cancelBuild()
	}
	r.mu.Unlock()
	select {
	case k <- struct{}{}:
	default:
	}
}

// Latest returns the most recent snapshot, if any refresh has completed.
func (r *Refresher) Latest() (Snapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.latest == nil {
		return Snapshot{}, false
	}
	return *r.latest, true
}

// LastSuccess is the time of the last refresh in which at least one wallet synced.
func (r *Refresher) LastSuccess() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastSuccess
}

// Force runs a refresh now unless one ran within MinForceInterval.
func (r *Refresher) Force(ctx context.Context) (Snapshot, error) {
	min := r.MinForceInterval
	if min <= 0 {
		min = 30 * time.Second
	}
	r.mu.Lock()
	if time.Since(r.lastForced) < min {
		r.mu.Unlock()
		s, _ := r.Latest()
		return s, ErrTooSoon
	}
	r.lastForced = time.Now()
	r.mu.Unlock()
	return r.refresh(ctx), nil
}

// Rebuild runs a refresh now, whatever MinForceInterval says: after a reset the old
// snapshot must not be served again.
func (r *Refresher) Rebuild(ctx context.Context) Snapshot { return r.refresh(ctx) }

// RepriceNow reprices the last balances at once, however recent the last prices are (the
// header's prices chip). ok is false when it published nothing: no refresh has finished
// yet, or one is running, which prices too.
func (r *Refresher) RepriceNow(ctx context.Context) (Snapshot, bool) { return r.reprice(ctx, true) }

// reprice publishes the last balances at new prices, between refreshes. A refresh prices
// too, so a reprice gives way to one: it doesn't start while one runs or (unless forced)
// within half a PriceEvery of the last prices, and it publishes nothing if anything else
// was published, or a kick came, while it priced. It isn't counted as a sync.
func (r *Refresher) reprice(ctx context.Context, force bool) (Snapshot, bool) {
	v, _, _ := r.sf.Do("reprice", func() (any, error) {
		r.mu.Lock()
		prev, gen := r.latest, r.gen
		busy := r.cancelBuild != nil || prev == nil || prev.Warm || (!force && time.Since(prev.PricedAt) < r.priceEvery()/2)
		r.mu.Unlock()
		if busy {
			return nil, nil
		}
		pctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		s, ok := r.Agg.Reprice(pctx)
		if !ok {
			return nil, nil
		}
		r.mu.Lock()
		if r.latest != prev || r.gen != gen || r.cancelBuild != nil {
			r.mu.Unlock()
			return nil, nil
		}
		r.latest = &s
		if r.Broker != nil {
			r.Broker.Publish(s)
		}
		r.mu.Unlock()
		if r.History != nil {
			if err := r.History.Record(pctx, s); err != nil && r.Log != nil {
				r.Log.Warn("history record failed", "err", err)
			}
		}
		if r.OnSnapshot != nil {
			r.OnSnapshot(s)
		}
		return s, nil
	})
	s, ok := v.(Snapshot)
	return s, ok
}

// refresh builds a snapshot, publishing its progress; concurrent callers share one build.
// A build a kick replaced returns the latest snapshot instead of its own.
func (r *Refresher) refresh(ctx context.Context) Snapshot {
	v, _, _ := r.sf.Do("refresh", func() (any, error) {
		// Detach from a request context so a closed browser tab doesn't cancel the build.
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		r.mu.Lock()
		gen := r.gen
		r.cancelBuild = cancel
		r.mu.Unlock()
		start := time.Now()
		s := r.Agg.BuildProgress(bctx, func(p Snapshot) {
			r.mu.Lock() // held while publishing, so a replaced build can't publish after a kick
			defer r.mu.Unlock()
			if r.gen == gen {
				r.latest = &p
				if r.Broker != nil {
					r.Broker.Publish(p)
				}
			}
		})
		ok := 0
		for _, w := range s.Wallets {
			if w.Status == StatusSynced {
				ok++
			}
		}
		r.mu.Lock()
		r.cancelBuild = nil
		if r.gen != gen {
			latest := r.latest
			r.mu.Unlock()
			if latest == nil {
				return Snapshot{}, nil
			}
			return *latest, nil
		}
		r.latest = &s
		if ok > 0 || len(s.Wallets) == 0 {
			r.lastSuccess = s.UpdatedAt
		}
		r.mu.Unlock()
		if r.Log != nil {
			r.Log.Info("refreshed", "wallets", len(s.Wallets), "synced", ok, "total_usd", int64(s.TotalUSD), "took", time.Since(start).Round(time.Millisecond))
		}
		if r.History != nil {
			if err := r.History.Record(bctx, s); err != nil && r.Log != nil {
				r.Log.Warn("history record failed", "err", err)
			}
		}
		if r.Broker != nil {
			r.Broker.Publish(s)
		}
		if r.OnSnapshot != nil {
			r.OnSnapshot(s)
		}
		return s, nil
	})
	return v.(Snapshot)
}
