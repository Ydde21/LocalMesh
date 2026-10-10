package events_test

import (
	"fmt"
	"testing"

	"github.com/localmesh/localmesh/internal/server/events"
)

// BenchmarkBrokerPublish measures the per-event fanout cost of
// Broker.Publish — RLock + subscriber-map iteration + one non-blocking
// channel send per subscriber — as the subscriber set grows. Every
// subscriber drains concurrently from a goroutine (the realistic SSE
// shape), so sends stay on the buffered fast path rather than
// degenerating into the drop path once buffers fill.
func BenchmarkBrokerPublish(b *testing.B) {
	for _, subs := range []int{0, 1, 8, 64, 200} {
		b.Run(fmt.Sprintf("subscribers=%d", subs), func(b *testing.B) {
			br := events.NewBroker()
			stop := make(chan struct{})
			var unsubs []func()
			for i := 0; i < subs; i++ {
				ch, unsub := br.Subscribe(256)
				unsubs = append(unsubs, unsub)
				go func() {
					for {
						select {
						case <-ch:
						case <-stop:
							return
						}
					}
				}()
			}
			b.Cleanup(func() {
				close(stop)
				for _, u := range unsubs {
					u()
				}
			})
			payload := map[string]any{"id": "node-1", "cpu": 42.5}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				br.Publish("node.metrics", payload)
			}
		})
	}
}

// BenchmarkBrokerSubscribe measures subscribe+unsubscribe churn — what
// each dashboard SSE connect/disconnect costs the broker.
func BenchmarkBrokerSubscribe(b *testing.B) {
	br := events.NewBroker()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, unsub := br.Subscribe(64)
		unsub()
	}
}
