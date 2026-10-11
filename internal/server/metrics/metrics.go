package metrics

import (
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// CertDaysRemaining parses a DER leaf certificate and returns days
// until expiry — the value behind the control_plane_cert_days_remaining
// gauge the integrator wires from the serving chain (s.ca.Server.
// Certificate[0]); ok=false when the bytes aren't a cert (dev-insecure
// mode simply emits no series).
func CertDaysRemaining(leafDER []byte) (days float64, ok bool) {
	cert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return 0, false
	}
	return time.Until(cert.NotAfter).Hours() / 24, true
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

// ---------------------------------------------------------------------------
// histograms
// ---------------------------------------------------------------------------

// histSeries is one labeled histogram series: per-bucket counts (last
// cell catches >last-bucket, i.e. le="+Inf"), running sum and total.
type histSeries struct {
	labels []label
	counts []uint64 // len(buckets)+1 — final cell is the +Inf bucket
	sum    float64
	total  uint64
}

// histSample is a point-in-time copy of one histogram series.
type histSample struct {
	key    string
	labels []label
	counts []uint64 // per-bucket (non-cumulative), +Inf last
	sum    float64
	total  uint64
}

// histogramVec is a minimal labeled histogram family: fixed buckets
// declared at construction, mutex-guarded series map. Observations are
// event-rate (never hot enough to need per-bucket atomics).
type histogramVec struct {
	name    string
	help    string
	buckets []float64 // sorted ascending; +Inf is implicit
	mu      sync.Mutex
	m       map[string]*histSeries
}

func newHistogramVec(name, help string, buckets []float64) *histogramVec {
	return &histogramVec{
		name: name, help: help,
		buckets: buckets,
		m:       map[string]*histSeries{},
	}
}

// observe adds one sample to the labeled series.
func (v *histogramVec) observe(ls []label, val float64) {
	if len(ls) > 1 {
		sort.Slice(ls, func(i, j int) bool { return ls[i].k < ls[j].k })
	}
	key := seriesKey(ls)
	v.mu.Lock()
	s, ok := v.m[key]
	if !ok {
		s = &histSeries{
			labels: append([]label(nil), ls...),
			counts: make([]uint64, len(v.buckets)+1),
		}
		v.m[key] = s
	}
	idx := sort.SearchFloat64s(v.buckets, val) // first bucket >= val
	s.counts[idx]++
	s.sum += val
	s.total++
	v.mu.Unlock()
}

// snapshot returns all series in deterministic (key-sorted) order,
// with counts copied so rendering holds no lock.
func (v *histogramVec) snapshot() []histSample {
	v.mu.Lock()
	out := make([]histSample, 0, len(v.m))
	for k, s := range v.m {
		out = append(out, histSample{
			key:    k,
			labels: s.labels,
			counts: append([]uint64(nil), s.counts...),
			sum:    s.sum,
			total:  s.total,
		})
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
	inferenceReq    *counterVec // localmesh_inference_requests_total{status}
	modelOps        *counterVec // localmesh_model_operation_events_total{status}
	updatesFinished *counterVec // localmesh_agent_updates_finished_total{phase}
	updatesRB       *counterVec // localmesh_agent_updates_rolled_back_total
	rolloutTrans    *counterVec // localmesh_rollout_transitions_total{state}
	rolloutsFail    *counterVec // localmesh_rollouts_failed_total
	nodeConn        *counterVec // localmesh_node_connected_total
	nodeDisc        *counterVec // localmesh_node_disconnected_total
	nodeReconn      *counterVec // localmesh_node_reconnects_total
	nodeMaint       *counterVec // localmesh_node_maintenance_events_total{state}
	releasesReg     *counterVec // localmesh_releases_registered_total
	releasesRev     *counterVec // localmesh_releases_revoked_total
	releaseKeysReg  *counterVec // localmesh_release_keys_registered_total
	releaseKeysRev  *counterVec // localmesh_release_keys_revoked_total
	ownership       *counterVec // localmesh_ownership_transitions_total{event}

	// vecs is the fixed render order (declaration order above).
	vecs []*counterVec

	queueSec *histogramVec // localmesh_inference_queue_seconds
	ttftSec  *histogramVec // localmesh_inference_ttft_seconds{status}
	totalSec *histogramVec // localmesh_inference_duration_seconds{status}
	opDurSec *histogramVec // localmesh_model_operation_duration_seconds{status}

	// hvecs is the fixed histogram render order (declaration order).
	hvecs []*histogramVec

	// dedupe guards rolloutState/updateDone/nodeSeen/opStarted.
	mu           sync.Mutex
	rolloutState map[string]string    // rollout id → last counted state
	updateDone   map[string]bool      // update id → terminal already counted
	nodeSeen     map[string]bool      // node id → connected this process lifetime
	opStarted    map[string]time.Time // op id → first dispatch observed

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
		inferenceReq:    newCounterVec("localmesh_inference_requests_total", "Inference requests reaching a terminal outcome (normalized: succeeded|failed|cancelled)."),
		modelOps:        newCounterVec("localmesh_model_operation_events_total", "Model-operation events by reported status."),
		updatesFinished: newCounterVec("localmesh_agent_updates_finished_total", "Agent updates reaching a terminal phase (deduplicated per update)."),
		updatesRB:       newCounterVec("localmesh_agent_updates_rolled_back_total", "Agent updates that terminated in a rollback (deduplicated per update)."),
		rolloutTrans:    newCounterVec("localmesh_rollout_transitions_total", "Rollout state transitions (deduplicated per rollout)."),
		rolloutsFail:    newCounterVec("localmesh_rollouts_failed_total", "Rollout campaigns that reached the failed state (deduplicated per rollout)."),
		nodeConn:        newCounterVec("localmesh_node_connected_total", "Node agent connections established."),
		nodeDisc:        newCounterVec("localmesh_node_disconnected_total", "Node agent connections closed."),
		nodeReconn:      newCounterVec("localmesh_node_reconnects_total", "Node connections for an id already connected this process lifetime (churn signal)."),
		nodeMaint:       newCounterVec("localmesh_node_maintenance_events_total", "Node maintenance-state events by target state."),
		releasesReg:     newCounterVec("localmesh_releases_registered_total", "Signed releases registered."),
		releasesRev:     newCounterVec("localmesh_releases_revoked_total", "Releases revoked."),
		releaseKeysReg:  newCounterVec("localmesh_release_keys_registered_total", "Release signing keys registered."),
		releaseKeysRev:  newCounterVec("localmesh_release_keys_revoked_total", "Release signing keys revoked."),
		ownership:       newCounterVec("localmesh_ownership_transitions_total", "Control-plane ownership transitions by event kind."),
		queueSec: newHistogramVec("localmesh_inference_queue_seconds",
			"Seconds an inference workload waited between acceptance and dispatch (decision.queued_ms).",
			[]float64{0.05, 0.25, 1, 5, 30, 120, 300}),
		ttftSec: newHistogramVec("localmesh_inference_ttft_seconds",
			"Seconds to first token for finished inference workloads, by outcome.",
			[]float64{0.25, 0.5, 1, 2.5, 5, 15, 60}),
		totalSec: newHistogramVec("localmesh_inference_duration_seconds",
			"End-to-end seconds for finished inference workloads, by outcome.",
			[]float64{1, 5, 15, 60, 180, 600}),
		opDurSec: newHistogramVec("localmesh_model_operation_duration_seconds",
			"Seconds from a model op's first observed dispatch to its terminal status, by outcome. Interrupted ops keep the timer across retries; ops predating a restart are undercounted.",
			[]float64{5, 30, 120, 600, 1800, 7200}),
		rolloutState: map[string]string{},
		updateDone:   map[string]bool{},
		nodeSeen:     map[string]bool{},
		opStarted:    map[string]time.Time{},
	}
	c.vecs = []*counterVec{
		c.eventsTotal, c.workloadsQ, c.workloadsS, c.workloadsF,
		c.inferenceReq, c.modelOps, c.updatesFinished, c.updatesRB,
		c.rolloutTrans, c.rolloutsFail,
		c.nodeConn, c.nodeDisc, c.nodeReconn, c.nodeMaint,
		c.releasesReg, c.releasesRev, c.releaseKeysReg, c.releaseKeysRev,
		c.ownership,
	}
	c.hvecs = []*histogramVec{c.queueSec, c.ttftSec, c.totalSec, c.opDurSec}
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
		// Queue wait: recorded at dispatch because that's when the wait
		// actually ends (the decision carries the scheduler-measured
		// queued_ms). Workloads that die in the queue never emit started
		// and contribute no sample — this is a scheduling-wait profile,
		// not a per-request guarantee.
		if ms, ok := numField(anyField(payload, "decision"), "queued_ms"); ok && ms >= 0 {
			c.queueSec.observe(nil, ms/1000)
		}
	case "workload.finished":
		status := norm(strField(payload, "status"))
		c.workloadsF.inc(label{"status", status})
		c.inferenceReq.inc(label{"status", normOutcome(status)})
		// Timings land when the row carries them — a failed request's
		// duration is still a real duration (time-to-fail), so no
		// outcome filter; absent fields contribute no sample.
		if ms, ok := numField(payload, "ttft_ms"); ok && ms > 0 {
			c.ttftSec.observe([]label{{"status", status}}, ms/1000)
		}
		if sec, ok := workloadDuration(payload); ok {
			c.totalSec.observe([]label{{"status", status}}, sec)
		}
	case "model.operation":
		status := norm(strField(payload, "status"))
		c.modelOps.inc(label{"status", status})
		c.observeModelOp(strField(payload, "operation_id"), status)
	case "agent.update":
		c.observeUpdate(payload)
	case "rollout":
		c.observeRollout(payload)
	case "node.connected":
		c.nodeConn.inc()
		c.observeNodeConnect(payload)
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
	// Rollbacks are the update outcome operators most need at a glance —
	// a dedicated counter keeps that signal out of the phase table.
	if phase == "rolled_back" {
		c.updatesRB.inc()
	}
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
	if state == "failed" {
		c.rolloutsFail.inc()
	}
}

// observeNodeConnect counts a node.connected as a "reconnect" when the
// node id has already connected during this process's lifetime. The
// map is process-local: a first connect after a restart always reads
// as a fresh connect — honest, since pre-restart history is unknowable.
func (c *Collector) observeNodeConnect(payload any) {
	id := strField(payload, "id")
	if id == "" {
		return
	}
	c.mu.Lock()
	if len(c.nodeSeen) >= dedupeCap && !c.nodeSeen[id] {
		c.nodeSeen = map[string]bool{}
	}
	reconnect := c.nodeSeen[id]
	c.nodeSeen[id] = true
	c.mu.Unlock()
	if reconnect {
		c.nodeReconn.inc()
	}
}

// observeModelOp tracks model-op execution wall time for the duration
// histogram. The timer starts on the first 'dispatched' (or 'running'
// progress frame — the first signal an agent is actually working) and
// closes on the terminal emit; 'interrupted' deliberately does NOT end
// it — retryable pulls resume under the same operation id, so the
// sample spans retries, which is the duration an operator cares about.
// Ops whose terminal arrives with no observed start (process restarted
// mid-op) contribute no sample rather than a fabricated duration.
func (c *Collector) observeModelOp(opID, status string) {
	if opID == "" {
		return
	}
	switch status {
	case "dispatched", "running":
		c.mu.Lock()
		if len(c.opStarted) >= dedupeCap {
			c.opStarted = map[string]time.Time{}
		}
		if _, ok := c.opStarted[opID]; !ok {
			c.opStarted[opID] = time.Now()
		}
		c.mu.Unlock()
	case "succeeded", "failed", "cancelled":
		c.mu.Lock()
		start, ok := c.opStarted[opID]
		if ok {
			delete(c.opStarted, opID)
		}
		c.mu.Unlock()
		if ok {
			c.opDurSec.observe([]label{{"status", status}}, time.Since(start).Seconds())
		}
	}
}

// workloadDuration resolves the end-to-end seconds of a finished
// workload. Preferred source is the stored total_ms (exact, survives
// republish); producers that publish the row without it (the current
// gateway emits workload.finished before the struct carries the
// timings) fall back to timestamps: finished_at−started_at on
// republished rows, or started_at→observe-time on the live emit — the
// broker hop is sub-millisecond, well inside any bucket. queued_at is
// the deeper fallback for rows that never reached 'running'. Durations
// beyond a day are rejected as clock junk.
func workloadDuration(payload any) (float64, bool) {
	if ms, ok := numField(payload, "total_ms"); ok && ms > 0 {
		return ms / 1000, true
	}
	start, ok := timeField(payload, "started_at", "queued_at")
	if !ok {
		return 0, false
	}
	end := time.Now()
	if fin, ok := timeField(payload, "finished_at"); ok {
		end = fin
	}
	d := end.Sub(start).Seconds()
	if d < 0 || d > 86400 {
		return 0, false
	}
	return d, true
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

// strField reads the first listed string field from a map or struct.
// Returns "" when absent or not a string.
func strField(v any, names ...string) string {
	for _, n := range names {
		if s, ok := anyField(v, n).(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// numField reads the first listed field as a number — every integer
// and float kind map/struct producers emit, plus json.Number. ok=false
// when the field is absent or non-numeric.
func numField(v any, names ...string) (float64, bool) {
	for _, n := range names {
		switch x := anyField(v, n).(type) {
		case int:
			return float64(x), true
		case int8:
			return float64(x), true
		case int16:
			return float64(x), true
		case int32:
			return float64(x), true
		case int64:
			return float64(x), true
		case uint:
			return float64(x), true
		case uint8:
			return float64(x), true
		case uint16:
			return float64(x), true
		case uint32:
			return float64(x), true
		case uint64:
			return float64(x), true
		case float32:
			return float64(x), true
		case float64:
			return x, true
		case json.Number:
			if f, err := x.Float64(); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// anyField reads a field from a map or struct payload. For structs the
// match is by JSON wire name first (e.g. "ttft_ms" reaches
// Workload.TTFTms via its tag — the name producers actually emit),
// falling back to a case-insensitive Go field-name match when the
// field carries no tag. Pointer fields are dereferenced; nil pointers
// read as absent.
func anyField(v any, name string) any {
	if v == nil {
		return nil
	}
	switch m := v.(type) {
	case map[string]string:
		return m[name]
	case map[string]any:
		return m[name]
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		jn, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if jn == "-" {
			continue
		}
		if jn == name || (jn == "" && strings.EqualFold(f.Name, name)) {
			fv := rv.Field(i)
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					return nil
				}
				fv = fv.Elem()
			}
			return fv.Interface()
		}
	}
	return nil
}

// subField reads a nested payload field (e.g. the "release" object
// inside a release event map).
func subField(v any, name string) any {
	if m, ok := v.(map[string]any); ok {
		return m[name]
	}
	return nil
}

// timeField reads the first listed field as a timestamp — time.Time
// struct fields (dereferenced) or RFC3339 strings from map payloads.
// ok=false when absent, unparseable, or the zero time.
func timeField(v any, names ...string) (time.Time, bool) {
	for _, n := range names {
		switch x := anyField(v, n).(type) {
		case time.Time:
			if !x.IsZero() {
				return x, true
			}
		case string:
			if t, err := time.Parse(time.RFC3339Nano, x); err == nil && !t.IsZero() {
				return t, true
			}
		}
	}
	return time.Time{}, false
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

// normOutcome maps the workload terminal-status vocabulary onto the
// three-bucket outcome enum inference_requests_total exposes
// (succeeded|failed|cancelled): the store spells success "completed",
// dashboards ask "did it work". workloads_finished_total keeps the raw
// vocabulary — this is a read-optimized alias, not a second truth.
func normOutcome(status string) string {
	if status == "completed" {
		return "succeeded"
	}
	return status
}
