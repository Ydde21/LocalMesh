// Package events is a tiny in-process pub/sub that fans cluster events
// out to SSE subscribers (the dashboard). Events are fire-and-forget;
// subscribers that fall behind are dropped rather than blocking
// producers.
package events

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// Event is one published occurrence.
type Event struct {
	Type string `json:"type"` // e.g. "node.connected", "workload.updated"
	Data any    `json:"data"`
}

type subscriber struct {
	ch    chan Event
	id    int64
	owner string
}

type Broker struct {
	mu   sync.RWMutex
	subs map[int64]*subscriber
	next int64
}

func NewBroker() *Broker {
	return &Broker{subs: map[int64]*subscriber{}}
}

// Publish sends an event to all subscribers. Non-blocking.
func (b *Broker) Publish(typ string, data any) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ev := Event{Type: typ, Data: data}
	for _, s := range b.subs {
		select {
		case s.ch <- ev:
		default: // subscriber lagging; drop rather than stall the pipeline
		}
	}
}

// Subscribe returns a channel and an unsubscribe func.
func (b *Broker) Subscribe(buf int) (<-chan Event, func()) {
	ch, unsub, _ := b.SubscribeFor("", buf, 0)
	return ch, unsub
}

// SubscribeFor registers a subscriber under an owner key (e.g. the
// principal ID) with per-owner and total caps so a single client or a
// subscriber flood cannot grow the subscriber set unboundedly.
// ok=false when a cap rejects the subscription; ch is nil then.
func (b *Broker) SubscribeFor(owner string, buf, perOwner int) (<-chan Event, func(), bool) {
	const maxTotal = 256
	b.mu.RLock()
	total := len(b.subs)
	owned := 0
	if owner != "" && perOwner > 0 {
		for _, s := range b.subs {
			if s.owner == owner {
				owned++
			}
		}
	}
	b.mu.RUnlock()
	if total >= maxTotal || (owner != "" && perOwner > 0 && owned >= perOwner) {
		return nil, func() {}, false
	}

	id := atomic.AddInt64(&b.next, 1)
	s := &subscriber{ch: make(chan Event, buf), id: id, owner: owner}
	b.mu.Lock()
	// Re-check under the write lock — the RLock check above races.
	if len(b.subs) >= maxTotal {
		b.mu.Unlock()
		return nil, func() {}, false
	}
	if owner != "" && perOwner > 0 {
		owned = 0
		for _, x := range b.subs {
			if x.owner == owner {
				owned++
			}
		}
		if owned >= perOwner {
			b.mu.Unlock()
			return nil, func() {}, false
		}
	}
	b.subs[id] = s
	b.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(s.ch)
		})
	}, true
}

// Marshal serializes event data for SSE.
func (e Event) Marshal() []byte {
	b, _ := json.Marshal(e)
	return b
}
