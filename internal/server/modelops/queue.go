// Package modelops is the model-operation scheduling layer: a persistent,
// bounded-concurrency dispatch queue for pull/delete work on node agents.
// Ops are rows before they are wire frames — submissions survive restarts,
// dedupe cleanly, retry transient engine/transport failures with backoff,
// and never oversubscribe a node or the cluster.
//
// The store is the source of truth for concurrency accounting: the queue
// reads active counts each pass rather than tracking them in memory, so it
// stays correct even when op state is written by other code paths (the
// node hub's frame handling, admin cancellation, disconnect interruption).
package modelops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
)

// Config controls queue bounds and retry policy.
type Config struct {
	// GlobalConcurrent caps in-flight (dispatched+running) ops across the
	// cluster. Default 2.
	GlobalConcurrent int
	// PerNodeConcurrent caps in-flight ops per node. Default 1: a model
	// op is heavy I/O on the agent's engine — serialize them.
	PerNodeConcurrent int
	// QueueMax bounds the 'queued' backlog; Submit beyond it fails with
	// ErrQueueFull (caller → 429). Default 64.
	QueueMax int
	// MaxAttempts caps total attempts per op (initial + retries).
	// Default 3.
	MaxAttempts int
	// BaseBackoff is the first retry delay; each subsequent attempt
	// doubles it (base * 2^attempts-used), capped at 5 minutes with ±20%
	// jitter. Default 15s.
	BaseBackoff time.Duration
	// TickInterval is the dispatch loop cadence — also the backstop for
	// backoff expiry and connectivity changes. Default 1s.
	TickInterval time.Duration
}

func (c Config) withDefaults() Config {
	if c.GlobalConcurrent <= 0 {
		c.GlobalConcurrent = 2
	}
	if c.PerNodeConcurrent <= 0 {
		c.PerNodeConcurrent = 1
	}
	if c.QueueMax <= 0 {
		c.QueueMax = 64
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 15 * time.Second
	}
	if c.TickInterval <= 0 {
		c.TickInterval = time.Second
	}
	return c
}

// Dispatcher is the wire-facing half of the queue — implemented over the
// node hub in production, faked in tests. Keep it free of hub types so
// this package stays testable.
type Dispatcher interface {
	// Send delivers the op's pull/delete frame to the node.
	Send(ctx context.Context, nodeID string, op *store.ModelOperation) error
	// Abort asks the node to cancel an in-flight op (best effort).
	Abort(ctx context.Context, nodeID, opID string) error
	// Online reports whether the node has a live connection.
	Online(nodeID string) bool
}

// Notifier publishes lifecycle events (wired to the SSE broker by the
// integrator; nil-safe).
type Notifier func(event string, payload any)

var (
	// ErrQueueFull — the queued backlog is at QueueMax; caller → 429.
	ErrQueueFull = errors.New("model operation queue is full")
	// ErrDuplicate — an active op already exists for this node+model;
	// caller → 409. Matched via errors.Is; use *DuplicateError for detail.
	ErrDuplicate = errors.New("an operation is already active for this model on this node")
	// ErrNotActive — the op is already in a terminal state; caller → 409.
	ErrNotActive = errors.New("operation is not active")
)

// DuplicateError describes a dedupe conflict: OpID is the already-active
// op this submission would have duplicated.
type DuplicateError struct {
	OpID   string
	NodeID string
	Model  string
	Op     string
	Status string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("an operation is already active for model %q on node %s (op %s, status %s)",
		e.Model, e.NodeID, e.OpID, e.Status)
}

// Is lets errors.Is(err, ErrDuplicate) match.
func (e *DuplicateError) Is(err error) bool { return err == ErrDuplicate }

// SubmitRequest is one op submission.
type SubmitRequest struct {
	NodeID   string
	Model    string
	Op       string // "pull" | "delete"
	Priority int    // higher dispatches first; default 0
}

// Progress is one agent progress frame.
type Progress struct {
	Phase         string
	Completed     int64
	Total         int64
	RateBPS       int64
	Indeterminate bool
}

// Done is the agent's terminal frame for an op.
type Done struct {
	Success       bool
	Cancelled     bool
	Retryable     bool
	Error         string
	Completed     int64
	Total         int64
	Phase         string
	RateBPS       int64
	Indeterminate bool
}

// Queue owns the model-op dispatch loop.
type Queue struct {
	st     *store.DB
	disp   Dispatcher
	log    *slog.Logger
	notify Notifier

	mu     sync.Mutex
	cfg    Config
	online map[string]bool // last observed connectivity per node with work
	epoch  int64           // control-plane fencing token; -1 = unfenced

	wake chan struct{} // event-driven dispatch nudge (capacity 1)
}

// SetEpoch installs the control-plane ownership fencing token. Claims
// pass it to the store, which matches zero rows once a successor
// bumps the epoch — stale processes cannot dispatch work. -1 disables
// fencing (standalone/tests).
func (q *Queue) SetEpoch(epoch int64) {
	q.mu.Lock()
	q.epoch = epoch
	q.mu.Unlock()
}

// New builds a queue. Run drives it; Reconcile is invoked at Run start.
func New(st *store.DB, disp Dispatcher, cfg Config, log *slog.Logger) *Queue {
	if log == nil {
		log = slog.Default()
	}
	return &Queue{
		st:     st,
		disp:   disp,
		cfg:    cfg.withDefaults(),
		log:    log.With("component", "modelops"),
		online: map[string]bool{},
		epoch:  -1, // unfenced until SetEpoch (production always sets it)
		wake:   make(chan struct{}, 1),
	}
}

// SetNotifier installs the event publisher (typically events.Broker.Publish).
func (q *Queue) SetNotifier(n Notifier) { q.notify = n }

// UpdateConfig live-reloads bounds; zero fields keep their current value.
func (q *Queue) UpdateConfig(cfg Config) {
	q.mu.Lock()
	c := q.cfg
	if cfg.GlobalConcurrent > 0 {
		c.GlobalConcurrent = cfg.GlobalConcurrent
	}
	if cfg.PerNodeConcurrent > 0 {
		c.PerNodeConcurrent = cfg.PerNodeConcurrent
	}
	if cfg.QueueMax > 0 {
		c.QueueMax = cfg.QueueMax
	}
	if cfg.MaxAttempts > 0 {
		c.MaxAttempts = cfg.MaxAttempts
	}
	if cfg.BaseBackoff > 0 {
		c.BaseBackoff = cfg.BaseBackoff
	}
	q.cfg = c
	q.mu.Unlock()
	q.kick()
}

// Config returns the active configuration.
func (q *Queue) Config() Config {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cfg
}

func (q *Queue) config() Config { return q.Config() }

func (q *Queue) emit(event string, payload any) {
	if q.notify != nil {
		q.notify(event, payload)
	}
}

func (q *Queue) kick() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Submit persists an op in 'queued'. Offline nodes are accepted — the op
// waits until the node is connected. Dedupe: one active op per node+model
// (any op kind); conflicts return *DuplicateError.
func (q *Queue) Submit(ctx context.Context, req SubmitRequest) (*store.ModelOperation, error) {
	if req.Op != "pull" && req.Op != "delete" {
		return nil, fmt.Errorf("invalid op %q", req.Op)
	}
	if req.NodeID == "" || req.Model == "" {
		return nil, errors.New("node_id and model are required")
	}
	cfg := q.config()
	// Fast-path dedupe so a duplicate reports ErrDuplicate even when the
	// queue is also full; the insert below is the authoritative check.
	if existing, err := q.st.ActiveModelOpFor(ctx, req.NodeID, req.Model); err == nil {
		return nil, &DuplicateError{OpID: existing.ID, NodeID: req.NodeID,
			Model: req.Model, Op: existing.Op, Status: existing.Status}
	}
	queued, err := q.st.QueuedModelOpCount(ctx)
	if err != nil {
		return nil, err
	}
	if queued >= cfg.QueueMax {
		return nil, ErrQueueFull
	}
	op, err := q.st.EnqueueModelOp(ctx, req.NodeID, req.Model, req.Op, req.Priority, cfg.MaxAttempts)
	if err != nil {
		if errors.Is(err, store.ErrActiveModelOp) {
			de := &DuplicateError{NodeID: req.NodeID, Model: req.Model}
			if ex, gerr := q.st.ActiveModelOpFor(ctx, req.NodeID, req.Model); gerr == nil {
				de.OpID, de.Op, de.Status = ex.ID, ex.Op, ex.Status
			}
			return nil, de
		}
		return nil, err
	}
	q.log.Debug("op queued", "op", op.ID, "node", req.NodeID, "model", req.Model, "kind", req.Op)
	q.kick()
	return op, nil
}

// Run reconciles leftovers from a previous life, then dispatches until
// ctx ends. Call it in a goroutine.
func (q *Queue) Run(ctx context.Context) {
	if err := q.Reconcile(ctx); err != nil {
		q.log.Warn("reconcile failed", "err", err)
	}
	tick := q.config().TickInterval
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-q.wake:
		}
		q.dispatchOnce(ctx)
	}
}

// Reconcile recovers ops orphaned by a previous process: dispatched/
// running rows are interrupted (their true outcome is unknowable), then
// retryable pulls resume; deletes stay interrupted. Queued rows pick up
// naturally on the next pass.
func (q *Queue) Reconcile(ctx context.Context) error {
	n, err := q.st.InterruptActiveModelOps(ctx, "interrupted by control-plane restart")
	if err != nil {
		return err
	}
	if n > 0 {
		q.log.Warn("interrupted orphaned model ops", "count", n)
	}
	return q.resumeInterrupted(ctx, "")
}

// OnNodeConnected expedites a reconnected node's queued/resumed ops.
func (q *Queue) OnNodeConnected(nodeID string) {
	q.mu.Lock()
	q.online[nodeID] = true
	q.mu.Unlock()
	if err := q.st.DueNowNodeModelOps(context.Background(), nodeID); err != nil {
		q.log.Debug("expedite node ops", "err", err)
	}
	q.kick()
}

// OnNodeDisconnected interrupts the node's in-flight ops and requeues
// retryable pulls (deletes stay interrupted — their outcome is unknown).
// The hub calls this on conn drop; the dispatch loop also detects
// transitions itself, so a missed call heals within one tick.
func (q *Queue) OnNodeDisconnected(nodeID string) {
	q.mu.Lock()
	q.online[nodeID] = false
	q.mu.Unlock()
	q.handleDisconnect(nodeID)
	q.kick()
}

// OnProgress records an agent progress frame. Late frames on finished ops
// are dropped by the store's status guard.
func (q *Queue) OnProgress(ctx context.Context, opID string, p Progress) {
	if err := q.st.UpdateModelOpProgress(ctx, opID, p.Phase,
		p.Completed, p.Total, p.RateBPS, p.Indeterminate); err != nil {
		q.log.Debug("record progress", "op", opID, "err", err)
	}
}

// OnDone applies an agent terminal frame: success → 'succeeded' with the
// final counters, cancel → 'cancelled', failure → the retry policy in
// OnFail. Late frames on finished ops are dropped.
func (q *Queue) OnDone(ctx context.Context, opID string, d Done) {
	switch {
	case d.Cancelled:
		err := q.st.FinishModelOp(ctx, opID, "cancelled", firstNonEmpty(d.Error, "cancelled by administrator"),
			d.Completed, d.Total, d.RateBPS, d.Phase, d.Indeterminate)
		if err != nil {
			q.log.Debug("finish cancelled op", "op", opID, "err", err)
		}
	case d.Success:
		phase := d.Phase
		if phase == "" {
			phase = "complete"
		}
		err := q.st.FinishModelOp(ctx, opID, "succeeded", "",
			d.Completed, d.Total, d.RateBPS, phase, d.Indeterminate)
		if err != nil {
			q.log.Debug("finish succeeded op", "op", opID, "err", err)
		}
	default:
		errStr := d.Error
		if errStr == "" {
			errStr = "operation failed"
		}
		q.OnFail(ctx, opID, errStr, d.Retryable)
	}
	q.kick() // a slot may have freed
}

// OnFail records an agent-reported failure. Retryable pulls requeue with
// backoff until attempts run out; deletes and permanent failures go
// straight to 'failed'. Late failures on finished ops are dropped.
func (q *Queue) OnFail(ctx context.Context, opID, errStr string, retryable bool) {
	op, err := q.st.GetModelOpState(ctx, opID)
	if err != nil || !opInFlight(op.Status) {
		return // unknown, queued (never ran), or already terminal — drop
	}
	if retryable && op.Op == "pull" {
		next := time.Now().Add(q.backoff(op.Attempt))
		if ok, rerr := q.st.RequeueModelOp(ctx, opID, next, errStr); rerr == nil && ok {
			q.log.Info("op requeued after failure", "op", opID, "attempt", op.Attempt, "err", errStr)
			q.kick()
			return
		}
		// Attempts exhausted or raced terminal — check once.
		if st2, gerr := q.st.GetModelOpState(ctx, opID); gerr == nil && opActive(st2.Status) {
			q.st.FailModelOp(ctx, opID, errStr+" (attempts exhausted)")
		}
		return
	}
	if err := q.st.FailModelOp(ctx, opID, errStr); err != nil {
		q.log.Debug("fail op", "op", opID, "err", err)
	}
	q.kick()
}

// Cancel aborts an op: queued ops cancel in place (never dispatched);
// dispatched/running ops get an abort frame and are finalized cancelled —
// a late agent OpDone is then dropped by the status guard.
func (q *Queue) Cancel(ctx context.Context, opID string) error {
	op, err := q.st.GetModelOpState(ctx, opID)
	if err != nil {
		return err // ErrNotFound propagates
	}
	switch op.Status {
	case "queued", "interrupted":
		// Neither is executing on an agent — finalize locally. The
		// store's guard covers both; a miss means the op raced to
		// dispatched/running, so fall through to the abort path.
		if ok, err := q.st.CancelQueuedModelOp(ctx, opID); err != nil {
			return err
		} else if ok {
			q.kick()
			return nil
		}
		fallthrough
	case "dispatched", "running":
		if err := q.disp.Abort(ctx, op.NodeID, opID); err != nil {
			// Best effort: an unreachable node can't confirm, but the op
			// is dead either way — finalize locally.
			q.log.Debug("abort frame failed", "op", opID, "err", err)
		}
		if err := q.st.FinishModelOperationStatus(ctx, opID, "cancelled", "cancelled by administrator"); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Our abort raced the agent's own completion frame — the
				// op may already sit in a terminal state. That's the
				// outcome the caller wanted, not an error.
				if cur, rerr := q.st.GetModelOpState(ctx, opID); rerr == nil &&
					opTerminal(cur.Status) {
					q.kick()
					return nil
				}
				return ErrNotActive
			}
			return err
		}
		q.kick()
		return nil
	default:
		return ErrNotActive
	}
}

// ---------------------------------------------------------------------------
// dispatch loop
// ---------------------------------------------------------------------------

// dispatchOnce runs one scheduling pass: refresh connectivity (interrupt
// ops on freshly-dropped nodes), then claim and send due ops within the
// global and per-node caps.
func (q *Queue) dispatchOnce(ctx context.Context) {
	cfg := q.config()
	q.refreshConnectivity()

	counts, err := q.st.ModelOpsActiveByNode(ctx)
	if err != nil {
		q.log.Debug("active op counts", "err", err)
		return
	}
	global := 0
	for _, c := range counts {
		global += c
	}
	free := cfg.GlobalConcurrent - global
	if free <= 0 {
		return
	}
	q.mu.Lock()
	var eligible []string
	for id, on := range q.online {
		if on && counts[id] < cfg.PerNodeConcurrent {
			eligible = append(eligible, id)
		}
	}
	epoch := q.epoch
	q.mu.Unlock()
	if len(eligible) == 0 {
		return
	}
	claimed, err := q.st.ClaimDueModelOps(ctx, epoch, eligible, cfg.PerNodeConcurrent, free)
	if err != nil {
		q.log.Debug("claim due ops", "err", err)
		return
	}
	sent := map[string]int{}
	for _, op := range fairOrder(claimed) {
		if free <= 0 || counts[op.NodeID]+sent[op.NodeID] >= cfg.PerNodeConcurrent ||
			!q.disp.Online(op.NodeID) {
			// Claimed but not sendable this pass — hand it back without
			// consuming an attempt.
			q.st.ReleaseModelOpClaim(ctx, op.ID)
			continue
		}
		if q.sendOp(ctx, op) == nil {
			sent[op.NodeID]++
			free--
		}
	}
}

// refreshConnectivity diffs observed node connectivity against the last
// pass: online→offline interrupts+resumes, offline→online expedites.
// This makes the queue self-sufficient when hub hook calls are absent.
func (q *Queue) refreshConnectivity() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes, err := q.st.ModelOpWorkNodes(ctx)
	if err != nil {
		return
	}
	q.mu.Lock()
	prev, cur := q.online, map[string]bool{}
	for _, n := range nodes {
		cur[n] = q.disp.Online(n)
	}
	q.online = cur
	q.mu.Unlock()
	// Check transitions over the union of work nodes and previously known
	// nodes: a node whose last op was just interrupted drops out of the
	// work set, but its drop still needs handleDisconnect → resume.
	seen := make(map[string]bool, len(nodes)+len(prev))
	for _, n := range nodes {
		seen[n] = true
	}
	for n := range prev {
		seen[n] = true
	}
	for n := range seen {
		was, known := prev[n]
		switch {
		case known && was && !cur[n]:
			q.handleDisconnect(n)
		case known && !was && cur[n]:
			if err := q.st.DueNowNodeModelOps(ctx, n); err == nil {
				q.kick()
			}
		}
	}
}

// handleDisconnect interrupts a dropped node's in-flight ops, then
// resumes retryable pulls; deletes remain interrupted for manual retry.
func (q *Queue) handleDisconnect(nodeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := q.st.InterruptNodeModelOps(ctx, nodeID, "node disconnected mid-operation"); err != nil {
		q.log.Warn("interrupt ops on disconnect", "node", nodeID, "err", err)
	}
	if err := q.resumeInterrupted(ctx, nodeID); err != nil {
		q.log.Warn("resume interrupted pulls", "node", nodeID, "err", err)
	}
}

// resumeInterrupted requeues retryable interrupted pulls (empty nodeID =
// cluster-wide, used by Reconcile) and fails exhausted ones.
func (q *Queue) resumeInterrupted(ctx context.Context, nodeID string) error {
	resumed, err := q.st.ResumeInterruptedPulls(ctx, nodeID, q.config().BaseBackoff.Seconds())
	if err != nil {
		return err
	}
	for _, id := range resumed {
		q.log.Info("pull resumed after interruption", "op", id)
		q.emit("model.operation", map[string]any{
			"operation_id": id, "node_id": nodeID, "status": "queued",
		})
	}
	if n, err := q.st.FailExhaustedInterruptedPulls(ctx, nodeID); err != nil {
		return err
	} else if n > 0 {
		q.log.Warn("interrupted pulls exhausted retries", "count", n)
	}
	if len(resumed) > 0 {
		q.kick()
	}
	return nil
}

// sendOp delivers one claimed op. On send failure: pulls requeue with
// backoff (an unsent/restarted pull is idempotent); deletes go to
// 'interrupted' — the frame may have reached the agent, and a delete
// must never fire twice.
func (q *Queue) sendOp(ctx context.Context, op *store.OpState) error {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := q.disp.Send(sctx, op.NodeID, &op.ModelOperation)
	cancel()
	if err == nil {
		q.log.Debug("op dispatched", "op", op.ID, "node", op.NodeID, "kind", op.Op, "attempt", op.Attempt)
		q.emit("model.operation", map[string]any{
			"operation_id": op.ID, "node_id": op.NodeID,
			"model": op.Model, "status": "dispatched",
		})
		return nil
	}
	if op.Op == "delete" {
		q.st.MarkModelOpInterrupted(ctx, op.ID, "dispatch failed (outcome unknown): "+err.Error())
		return err
	}
	next := time.Now().Add(q.backoff(op.Attempt))
	if ok, rerr := q.st.RequeueModelOp(ctx, op.ID, next, err.Error()); rerr != nil {
		q.log.Debug("requeue after send failure", "op", op.ID, "err", rerr)
	} else if !ok {
		// Exhausted or raced — fail it if it's still ours to finish.
		if st2, gerr := q.st.GetModelOpState(ctx, op.ID); gerr == nil && opActive(st2.Status) {
			q.st.FailModelOp(ctx, op.ID, "dispatch failed after retries: "+err.Error())
		}
	}
	return err
}

// backoff is the delay before the next attempt given how many dispatches
// the op has already consumed: base * 2^attempt, capped at 5m, ±20%
// jitter. attempt is incremented at claim time, so it is always ≥1 for a
// failure retry — the first retry waits ~2*base.
func (q *Queue) backoff(attempt int) time.Duration {
	d := q.config().BaseBackoff << min(attempt, 8)
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d * time.Duration(80+rand.IntN(40)) / 100
}

// opInFlight reports whether an agent may be executing the op right now —
// the only states where progress/done/fail frames are meaningful.
func opInFlight(status string) bool {
	return status == "dispatched" || status == "running"
}

// opTerminal is the no-transitions-left set.
func opTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	}
	return false
}

// opActive also counts 'queued' — statuses eligible for fail/exhaustion
// transitions from queue-side bookkeeping.
func opActive(status string) bool {
	return opInFlight(status) || status == "queued"
}

func firstNonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// fairOrder interleaves claimed ops one-per-node per round so a node with
// a deep backlog can't drain the global cap while other nodes wait.
// Node order follows each node's best-ranked op (priority, then age).
func fairOrder(ops []*store.OpState) []*store.OpState {
	groups := map[string][]*store.OpState{}
	var nodes []string
	for _, op := range ops {
		if _, ok := groups[op.NodeID]; !ok {
			nodes = append(nodes, op.NodeID)
		}
		groups[op.NodeID] = append(groups[op.NodeID], op)
	}
	out := make([]*store.OpState, 0, len(ops))
	for round := 0; ; round++ {
		progressed := false
		for _, n := range nodes {
			if round < len(groups[n]) {
				out = append(out, groups[n][round])
				progressed = true
			}
		}
		if !progressed {
			return out
		}
	}
}
