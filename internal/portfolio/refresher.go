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
// one to the broker. Page loads never wait on slow APIs.
type Refresher struct {
	Agg              *Aggregator
	Broker           *Broker
	History          *History // optional
	Interval         time.Duration
	MinForceInterval time.Duration // default 30s
	Log              *slog.Logger
	// OnSnapshot runs after each refresh (alerts engine hooks in here later).
	OnSnapshot func(Snapshot)

	sf          singleflight.Group
	mu          sync.RWMutex
	latest      *Snapshot
	lastForced  time.Time
	lastSuccess time.Time
	kick        chan struct{}
}

// Run refreshes immediately and then every Interval until ctx is done. Kick() triggers an
// early refresh (e.g. after a source is added). Until the first refresh finishes, the last
// session's balances (Aggregator.Warm) stand in, so pages don't wait on slow APIs after a
// restart; that snapshot isn't recorded, alerted on or counted as a sync.
func (r *Refresher) Run(ctx context.Context) {
	r.mu.Lock()
	if r.kick == nil {
		r.kick = make(chan struct{}, 1)
	}
	kick := r.kick
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
	for {
		r.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// Kick schedules a refresh as soon as possible.
func (r *Refresher) Kick() {
	r.mu.Lock()
	if r.kick == nil {
		r.kick = make(chan struct{}, 1)
	}
	k := r.kick
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

// refresh builds a snapshot; concurrent callers share one build.
func (r *Refresher) refresh(ctx context.Context) Snapshot {
	v, _, _ := r.sf.Do("refresh", func() (any, error) {
		// Detach from a request context so a closed browser tab doesn't cancel the build.
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		start := time.Now()
		s := r.Agg.Build(bctx)
		ok := 0
		for _, w := range s.Wallets {
			if w.Status == StatusSynced {
				ok++
			}
		}
		r.mu.Lock()
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
