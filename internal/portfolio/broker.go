package portfolio

import "sync"

// Broker fans snapshots out to SSE subscribers. Each subscriber channel holds at most one
// pending snapshot: a slow client only ever gets the latest, never a backlog.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Snapshot]struct{}
}

func NewBroker() *Broker { return &Broker{subs: map[chan Snapshot]struct{}{}} }

// Subscribe returns a channel of snapshots and a cancel func that must be called when the
// subscriber goes away.
func (b *Broker) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
		})
	}
}

// Publish delivers s to every subscriber, replacing any undelivered older snapshot.
func (b *Broker) Publish(s Snapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case <-ch: // drop the stale pending one
		default:
		}
		select {
		case ch <- s:
		default:
		}
	}
}

// Subscribers returns the current subscriber count.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
