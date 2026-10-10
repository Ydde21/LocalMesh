package metrics

import (
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/localmesh/localmesh/internal/server/events"
)

// subBuf is the broker subscription depth the collector drains. Events
// are observed with atomic increments only, so the consumer is orders
// of magnitude faster than any producer; the buffer absorbs bursts.
const subBuf = 1024

// dedupeCap bounds the per-id transition bookkeeping. Exceeding it
// resets the map — a >8192-rollout lifetime is far past any realistic
// deployment, and the alternative is an unbounded map.
const dedupeCap = 8192

// Gauge is one scrape-time gauge sample supplied by the integrator via
// SetGauges. Values are sampled on every scrape (live).
type Gauge struct {
	Name   string            // metric name; "localmesh_" is prepended when absent
	Help   string            // # HELP line text (optional but recommended)
	Value  float64           // NaN/±Inf render per exposition conventions
	Labels map[string]string // bounded, enum-like values only — never ids
}

// label is one metric label pair.
type label struct{ k, v string }

// series is one counter sample set: sorted labels → monotonic count.
type series struct {
	labels []label // sorted by k, canonicalized at inc time
	value  atomic.Uint64
}

// counterVec is a minimal labeled counter family: mutex-guarded series
// map, atomic values. Lock is held only for map get/create.
type counterVec struct {
	name string
	help string
	mu   sync.Mutex
	m    map[string]*series
}

func newCounterVec(name, help string) *counterVec {
	return &counterVec{name: name, help: help, m: map[string]*series{}}
}

// seriesKey encodes sorted label pairs unambiguously (\x00 cannot
// appear in label names; \x01 separators delimit pairs).
func seriesKey(ls []label) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.k)
		b.WriteByte(0)
		b.WriteString(l.v)
		b.WriteByte(1)
	}
	return b.String()
}

func (v *counterVec) inc(ls ...label) {
	// Sort so callers can pass labels in any order and the key/rendering
	// stay canonical. 1-label calls (the common case) skip the sort.
	if len(ls) > 1 {
		sort.Slice(ls, func(i, j int) bool { return ls[i].k < ls[j].k })
	}
	key := seriesKey(ls)
	v.mu.Lock()
	s, ok := v.m[key]
	if !ok {
		s = &series{labels: append([]label(nil), ls...)}
		v.m[key] = s
	}
	v.mu.Unlock()
	s.value.Add(1)
}

type sample struct {
	key    string
	labels []label
	value  uint64
}

// snapshot returns all series in deterministic (key-sorted) order.
func (v *counterVec) snapshot() []sample {
	v.mu.Lock()
	out := make([]sample, 0, len(v.m))
	for k, s := range v.m {
		out = append(out, sample{key: k, labels: s.labels, value: s.value.Load()})
	}
	v.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// Collector counts broker events and serves them as Prometheus text.
// Everything is in-memory: a process restart resets counters (gauges
// come from the integrator's provider, so they stay truthful). All
// methods are safe for concurrent use.
type Collector struct {
	log *slog.Logger

	eventsTotal     *counterVec // localmesh_events_total{event}
	workloadsQ      *counterVec // localmesh_workloads_queued_total
	workloadsS      *counterVec // localmesh_workloads_started_total
	workloadsF      *counterVec // localmesh_workloads_finished_total{status}
	modelOps        *counterVec // localmesh_model_operation_events_total{status}
	updatesFinished *counterVec // localmesh_agent_updates_finished_total{phase}
	rolloutTrans    *counterVec // localmesh_rollout_transitions_total{state}
	nodeConn        *counterVec // localmesh_node_connected_total
	nodeDisc        *counterVec // localmesh_node_disconnected_total
	nodeMaint       *counterVec // localmesh_node_maintenance_events_total{state}
	releasesReg     *counterVec // localmesh_releases_registered_total
	releasesRev     *counterVec // localmesh_releases_revoked_total
	releaseKeysReg  *counterVec // localmesh_release_keys_registered_total
	releaseKeysRev  *counterVec // localmesh_release_keys_revoked_total
	ownership       *counterVec // localmesh_ownership_transitions_total{event}

	// vecs is the fixed render order (declaration order above).
	vecs []*counterVec

	// dedupe guards rolloutState/updateDone.
	mu           sync.Mutex
	rolloutState map[string]string // rollout id → last counted state
	updateDone   map[string]bool   // update id → terminal already counted

	gaugeMu sync.RWMutex
	gaugeFn func() []Gauge

	subMu     sync.Mutex
	unsub     func()
	drainDone chan struct{}
}

// New builds an empty collector. log may be nil.
func New(log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	c := &Collector{
		log:             log.With("component", "metrics"),
		eventsTotal:     newCounterVec("localmesh_events_total", "Broker events observed, by event type."),
		workloadsQ:      newCounterVec("localmesh_workloads_queued_total", "Inference workloads accepted into the queue."),
		workloadsS:      newCounterVec("localmesh_workloads_started_total", "Inference workloads dispatched to a node."),
		workloadsF:      newCounterVec("localmesh_workloads_finished_total", "Inference workloads reaching a terminal status."),
		modelOps:        newCounterVec("localmesh_model_operation_events_total", "Model-operation events by reported status."),
		updatesFinished: newCounterVec("localmesh_agent_updates_finished_total", "Agent updates reaching a terminal phase (deduplicated per update)."),
		rolloutTrans:    newCounterVec("localmesh_rollout_transitions_total", "Rollout state transitions (deduplicated per rollout)."),
		nodeConn:        newCounterVec("localmesh_node_connected_total", "Node agent connections established."),
		nodeDisc:        newCounterVec("localmesh_node_disconnected_total", "Node agent connections closed."),
		nodeMaint:       newCounterVec("localmesh_node_maintenance_events_total", "Node maintenance-state events by target state."),
		releasesReg:     newCounterVec("localmesh_releases_registered_total", "Signed releases registered."),
		releasesRev:     newCounterVec("localmesh_releases_revoked_total", "Releases revoked."),
		releaseKeysReg:  newCounterVec("localmesh_release_keys_registered_total", "Release signing keys registered."),
		releaseKeysRev:  newCounterVec("localmesh_release_keys_revoked_total", "Release signing keys revoked."),
		ownership:       newCounterVec("localmesh_ownership_transitions_total", "Control-plane ownership transitions by event kind."),
		rolloutState:    map[string]string{},
		updateDone:      map[string]bool{},
	}
	c.vecs = []*counterVec{
		c.eventsTotal, c.workloadsQ, c.workloadsS, c.workloadsF,
		c.modelOps, c.updatesFinished, c.rolloutTrans,
		c.nodeConn, c.nodeDisc, c.nodeMaint,
		c.releasesReg, c.releasesRev, c.releaseKeysReg, c.releaseKeysRev,
		c.ownership,
	}
	return c
}

// Subscribe attaches a broker subscriber and drains events into
// Observe until Close. It is the recommended wiring: one subscription
// sees events from every producer (hub, gateway, adminapi,
// orchestrators) without touching their internals. If the broker
// refuses the subscription (subscriber cap), a warning is logged and
// nothing is drained. Re-Subscribe replaces the previous subscription.
func (c *Collector) Subscribe(ev *events.Broker) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.unsubLocked()
	ch, unsub := ev.Subscribe(subBuf)
	if ch == nil {
		c.log.Warn("metrics subscription refused by events broker (subscriber cap)")
		return
	}
	c.unsub = unsub
	done := make(chan struct{})
	c.drainDone = done
	go func() {
		defer close(done)
		for e := range ch {
			c.Observe(e.Type, e.Data)
		}
	}()
}

// Close detaches the broker subscription (if any) and waits for the
// drain goroutine to exit. Idempotent; the collector keeps counting
// direct Observe calls afterwards.
func (c *Collector) Close() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	c.unsubLocked()
}

func (c *Collector) unsubLocked() {
	if c.unsub != nil {
		c.unsub()
		c.unsub = nil
	}
	if c.drainDone != nil {
		<-c.drainDone
		c.drainDone = nil
	}
}

// Wrap returns a publish func that observes the event then forwards it
// — a drop-proof alternative for notifier-style paths:
//
//	modelOps.SetNotifier(mc.Wrap(s.ev.Publish))
//
// Do NOT combine Wrap on a publish path with Subscribe on the same
// broker: the same event is then counted twice.
func (c *Collector) Wrap(next func(event string, payload any)) func(event string, payload any) {
	return func(event string, payload any) {
		c.Observe(event, payload)
		if next != nil {
			next(event, payload)
		}
	}
}

// SetGauges installs the scrape-time gauge provider. fn is invoked on
// every scrape; a panicking provider contributes no gauges rather than
// failing the scrape (logged). Passing nil clears the provider.
func (c *Collector) SetGauges(fn func() []Gauge) {
	c.gaugeMu.Lock()
	c.gaugeFn = fn
	c.gaugeMu.Unlock()
}

func (c *Collector) gauges() (out []Gauge) {
	c.gaugeMu.RLock()
	fn := c.gaugeFn
	c.gaugeMu.RUnlock()
	if fn == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			c.log.Warn("gauge provider panicked", "err", r)
			out = nil
		}
	}()
	return fn()
}

// ---------------------------------------------------------------------------
// event → counter mapping
// ---------------------------------------------------------------------------

// Observe counts one event. Called by the Subscribe drain goroutine;
// also the direct entry point for tests and wrapped publishers.
func (c *Collector) Observe(event string, payload any) {
	c.eventsTotal.inc(label{"event", norm(event)})
	switch event {
	case "workload.queued":
		c.workloadsQ.inc()
	case "workload.started":
		c.workloadsS.inc()
	case "workload.finished":
		c.workloadsF.inc(label{"status", norm(strField(payload, "status"))})
	case "model.operation":
		c.modelOps.inc(label{"status", norm(strField(payload, "status"))})
	case "agent.update":
		c.observeUpdate(payload)
	case "rollout":
		c.observeRollout(payload)
	case "node.connected":
		c.nodeConn.inc()
	case "node.disconnected":
		c.nodeDisc.inc()
	case "node.maintenance":
		state := strField(payload, "state")
		if state == "" {
			state = "none" // MaintNone — cleared marker
		}
		c.nodeMaint.inc(label{"state", norm(state)})
	case "release":
		c.observeRelease(payload)
	case "control-plane":
		// Ownership transitions (acquired|released|lost|promoted|refused).
		// Payload shape is integrator-defined; accept the event kind
		// under any of the conventional field names.
		kind := strField(payload, "event", "kind", "type")
		c.ownership.inc(label{"event", norm(kind)})
	}
}

// observeUpdate counts an agent.update event only when its phase is
// terminal, deduplicated per update id (an update's terminal emit is
// store-guarded to fire once, but belt-and-suspenders is cheap).
func (c *Collector) observeUpdate(payload any) {
	phase := strField(payload, "phase")
	if !terminalPhase(phase) {
		return
	}
	id := strField(payload, "id")
	if id != "" {
		c.mu.Lock()
		if len(c.updateDone) >= dedupeCap {
			c.updateDone = map[string]bool{}
		}
		if c.updateDone[id] {
			c.mu.Unlock()
			return
		}
		c.updateDone[id] = true
		c.mu.Unlock()
	}
	c.updatesFinished.inc(label{"phase", norm(phase)})
}

// observeRollout counts a rollout event only when the state differs
// from the last counted state for that rollout — the orchestrator
// republishes the rollout row on every child-update event, so raw
// counting would credit "running" once per update, not once per
// transition.
func (c *Collector) observeRollout(payload any) {
	state := strField(payload, "state")
	id := strField(payload, "id")
	if id != "" {
		c.mu.Lock()
		if len(c.rolloutState) >= dedupeCap {
			c.rolloutState = map[string]string{}
		}
		if prev, ok := c.rolloutState[id]; ok && prev == state {
			c.mu.Unlock()
			return
		}
		c.rolloutState[id] = state
		c.mu.Unlock()
	}
	c.rolloutTrans.inc(label{"state", norm(state)})
}

// observeRelease maps the release registry's discriminated event:
// {kind: release|release_key|release_key_revoked, ...}. A release event
// whose nested release carries status "revoked" counts as a revocation;
// anything else under kind=release is a (re)registration.
func (c *Collector) observeRelease(payload any) {
	switch strField(payload, "kind") {
	case "release":
		if strField(subField(payload, "release"), "status") == "revoked" {
			c.releasesRev.inc()
		} else {
			c.releasesReg.inc()
		}
	case "release_key":
		c.releaseKeysReg.inc()
	case "release_key_revoked":
		c.releaseKeysRev.inc()
	}
}

// terminalPhase mirrors store.UpdTerminal without a store import.
func terminalPhase(phase string) bool {
	switch phase {
	case "succeeded", "failed", "rolled_back", "cancelled", "interrupted":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// payload field extraction
// ---------------------------------------------------------------------------
//
// Events arrive as whatever the producer published: map[string]string,
// map[string]any, or store structs (*store.Workload, *store.AgentUpdate,
// *store.Rollout, *store.OwnershipEvent). Field lookup is deliberately
// duck-typed so this package stays free of a store dependency.

// strField reads the first listed string field from a map or struct
// (struct match is case-insensitive: "status" hits Status). Returns ""
// when absent or not a string.
func strField(v any, names ...string) string {
	for _, n := range names {
		if s := field(v, n); s != "" {
			return s
		}
	}
	return ""
}

func field(v any, name string) string {
	if v == nil {
		return ""
	}
	switch m := v.(type) {
	case map[string]string:
		return m[name]
	case map[string]any:
		if s, ok := m[name].(string); ok {
			return s
		}
		return ""
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}
	f := rv.FieldByNameFunc(func(n string) bool { return strings.EqualFold(n, name) })
	if f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

// subField reads a nested payload field (e.g. the "release" object
// inside a release event map).
func subField(v any, name string) any {
	if m, ok := v.(map[string]any); ok {
		return m[name]
	}
	return nil
}

// norm canonicalizes a label value: empty → "unknown" (Prometheus
// prefers a named bucket over an empty one), trimmed otherwise. Label
// VALUES come only from bounded enums (status/phase/state/event) —
// ids and model names are never passed as labels anywhere in Observe.
func norm(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
