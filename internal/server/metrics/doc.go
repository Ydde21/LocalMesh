// Package metrics renders LocalMesh internals in the Prometheus text
// exposition format (v0.0.4) using only the standard library.
//
// ---------------------------------------------------------------------------
// INTEGRATOR WIRING (server.go)
// ---------------------------------------------------------------------------
//
//	mc := metrics.New(log)
//	mc.Subscribe(s.ev)          // drains the events broker; Close() detaches
//	mc.SetGauges(func() []metrics.Gauge {
//	    snap, _ := s.hub.Snapshot(ctx)
//	    online := 0
//	    for _, v := range snap { if v.Online { online++ } }
//	    return []metrics.Gauge{
//	        {Name: "nodes_online", Help: "Nodes currently connected.", Value: float64(online)},
//	        {Name: "nodes_total",  Help: "Nodes known to the cluster.", Value: float64(len(snap))},
//	        {Name: "control_plane_epoch", Help: "Ownership fencing epoch.", Value: float64(s.owner.Epoch())},
//	        {Name: "control_plane_active", Help: "1 when this instance holds ownership.", Value: 1},
//	    }
//	})
//	mux.Handle("GET /metrics", mwRead(mc.Handler()))   // or unauthenticated, your call
//
// Subscribe vs Observe/Wrap: Subscribe is the recommended (and only
// complete) wiring — the hub, gateway, adminapi and orchestrators all
// publish through the SAME broker, so one subscription sees every
// event. Observe is the synchronous counting entry point (Subscribe's
// drain goroutine calls it) and is public so tests or bespoke wiring
// can feed it directly. Wrap is offered for the SetNotifier-style
// publish paths (modelops.SetNotifier(mc.Wrap(s.ev.Publish))) when
// synchronous, drop-free counting is wanted — but pick ONE wiring:
// an event that flows through a wrapped publish AND the subscription
// is counted twice.
//
// ---------------------------------------------------------------------------
// SEMANTICS
// ---------------------------------------------------------------------------
//
//   - Counters are monotonic, process-local and reset on restart. That
//     is correct: live state (nodes online, epoch, role, live update
//     phases) belongs to the gauge provider, not counters.
//   - The broker drops to slow subscribers silently; the drain
//     goroutine only does atomic increments against a 1024-deep buffer,
//     so drops are effectively impossible — but possible in principle.
//     If a drop-proof path is required, use Wrap on the notifier path
//     and accept that hub/gateway internals are not wrappable.
//   - Label cardinality is bounded by construction: only enum-valued
//     fields are ever used as labels (status/phase/state/event). Node
//     ids, model names, release ids and workload ids NEVER become
//     labels. Unknown/empty enum values normalize to "unknown".
//   - "rollout" events are republished on every child update, so the
//     transition counter dedupes per rollout id — a state is counted
//     only when it differs from the last counted state for that id.
//     Same for terminal agent updates: one terminal count per update
//     id. Dedupe state is capped (8192 ids) and resets wholesale.
//   - v0.10 adds histograms (fixed buckets declared at construction)
//     and two more capped id-keyed maps: nodeSeen (reconnect detection)
//     and opStarted (model-op duration timers — 'interrupted' keeps the
//     timer so the sample spans retries; a terminal with no observed
//     start records nothing).
//   - Histogram series appear only after their first observation —
//     localmesh_inference_ttft_seconds stays empty until a producer
//     publishes finished workloads carrying ttft_ms (the collector
//     reads it whenever it arrives).
//   - SSE subscriber count is NOT exposed: the broker keeps its
//     subscriber set private. Surface it via SetGauges if a broker
//     accessor is ever added.
package metrics
