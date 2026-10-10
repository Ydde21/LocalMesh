package updateplane

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/store"
)

// ---------------------------------------------------------------------------
// rollout engine — campaigns group agent_updates rows into an
// administrable unit. Strategy semantics:
//
//	single     — exactly one node (batch ignored, treated as 1)
//	selected   — explicit list, batch_size concurrent, continues past
//	             failures and reports failed at the end
//	sequential — batch=1 auto-advance, halts on the first failure
//	canary     — first canary_count nodes run together; when all canary
//	             updates are terminal the rollout parks in
//	             awaiting_approval until ApproveNextBatch; remaining
//	             nodes then run batch_size at a time. Halts on failure.
//	parallel   — batch_size concurrent, continues past failures and
//	             reports failed at the end
//
// The canary gate is durable: ApproveNextBatch stamps
// rollouts.approved_by/approved_at in the same guarded UPDATE that
// flips the rollout back to 'running', so a control-plane restart or
// standby promotion sees an approved canary — never a re-parked one.
// approved_at IS NULL is the authoritative "still gated" bit.
//
// RolloutPaused ('paused') freezes a campaign: no claims, no re-park,
// no auto-halt, not even the all-terminal tally — ResumeRollout hands
// it back to the machine. In-flight updates still report and finish
// under their own timeouts: pausing gates NEW dispatch, it does not
// abort work already on the nodes.
//
// "Halt" cancels every still-cancellable live update (installing+ are
// left to report naturally) and lands the rollout 'failed'.
// ---------------------------------------------------------------------------

// RolloutPaused is the updateplane-local 'paused' rollout state. It is
// declared here rather than in store on purpose: the store keeps the
// state column generic (SetRolloutState guards only the terminal
// states), while the meaning of 'paused' — frozen advancement, live
// updates untouched — is engine semantics.
const RolloutPaused = "paused"

// CreateRollout validates and persists a campaign plus one pending
// update row per target node. Validation happens up-front so a bad
// target fails the whole request before anything is created.
//
// Errors map for the admin API: store.ErrNotFound → 404 (release or
// node), ErrReleaseNotApproved → 409, ErrInvalidRollout → 400,
// store.ErrActiveUpdate → 409, ErrQueueFull → 429.
func (m *Manager) CreateRollout(ctx context.Context, releaseID, strategy string, nodeIDs []string, canaryCount, batchSize int, createdBy string) (*store.Rollout, error) {
	cfg := m.config()
	rel, err := m.st.GetRelease(ctx, releaseID)
	if err != nil {
		return nil, err // ErrNotFound → 404
	}
	if rel.Status != "approved" {
		return nil, fmt.Errorf("%w: release %s is %s", ErrReleaseNotApproved, releaseID, rel.Status)
	}
	switch strategy {
	case StrategySingle, StrategySelected, StrategySequential, StrategyCanary, StrategyParallel:
	default:
		return nil, fmt.Errorf("%w: unknown strategy %q", ErrInvalidRollout, strategy)
	}
	if len(nodeIDs) == 0 {
		return nil, fmt.Errorf("%w: node_ids is required", ErrInvalidRollout)
	}
	seen := map[string]bool{}
	for _, id := range nodeIDs {
		if seen[id] {
			return nil, fmt.Errorf("%w: duplicate node_id %s", ErrInvalidRollout, id)
		}
		seen[id] = true
	}
	if strategy == StrategySingle && len(nodeIDs) != 1 {
		return nil, fmt.Errorf("%w: strategy single requires exactly one node", ErrInvalidRollout)
	}
	if canaryCount < 0 || canaryCount > cfg.MaxBatchSize {
		return nil, fmt.Errorf("%w: canary_count must be in [0,%d]", ErrInvalidRollout, cfg.MaxBatchSize)
	}
	if batchSize < 0 || batchSize > cfg.MaxBatchSize {
		return nil, fmt.Errorf("%w: batch_size must be in [0,%d]", ErrInvalidRollout, cfg.MaxBatchSize)
	}
	switch strategy {
	case StrategySingle, StrategySequential:
		batchSize = 1
	case StrategyCanary:
		if canaryCount == 0 {
			return nil, fmt.Errorf("%w: canary requires canary_count >= 1", ErrInvalidRollout)
		}
		if canaryCount >= len(nodeIDs) {
			return nil, fmt.Errorf("%w: canary_count %d covers the whole target set — use selected or parallel to update all nodes ungated",
				ErrInvalidRollout, canaryCount)
		}
		if batchSize == 0 {
			batchSize = min(len(nodeIDs), cfg.MaxBatchSize)
		}
	default: // selected, parallel
		if batchSize == 0 {
			batchSize = min(len(nodeIDs), cfg.MaxBatchSize)
		}
	}
	// Backlog bound: live updates cluster-wide + this rollout's rows.
	live, err := m.st.LiveUpdatesByPhase(ctx, livePhases...)
	if err != nil {
		return nil, err
	}
	if len(live)+len(nodeIDs) > cfg.QueueMax {
		return nil, ErrQueueFull
	}
	// Validate every target before creating anything.
	skip := map[string]bool{}
	for _, id := range nodeIDs {
		s, err := m.validateUpdateTarget(ctx, rel, id)
		if err != nil {
			return nil, err
		}
		skip[id] = s
	}
	// Hold the advance sweep for the whole cohort insert — a tick
	// running between row commits must never evaluate a rollout whose
	// updates don't all exist yet.
	m.createMu.Lock()
	defer m.createMu.Unlock()
	r, err := m.st.CreateRollout(ctx, releaseID, strategy, canaryCount, batchSize, createdBy)
	if err != nil {
		return nil, err
	}
	var created []string
	for _, id := range nodeIDs {
		u, err := m.st.EnqueueAgentUpdate(ctx, r.ID, releaseID, id, m.disp.AgentVersion(id))
		if err != nil {
			// Unwind: cancel the rows this call created and mark the
			// rollout dead — no half-created campaign.
			for _, cid := range created {
				m.st.FinishUpdate(ctx, cid, store.UpdCancelled, err.Error(),
					"rollout creation aborted")
			}
			m.st.SetRolloutState(ctx, r.ID, store.RolloutCancelled, err.Error())
			return nil, err // store.ErrActiveUpdate → 409
		}
		created = append(created, u.ID)
		if skip[id] {
			m.finishUpdate(ctx, u.ID, store.UpdSucceeded, "",
				"already running target version")
		} else {
			m.emitUpdate(ctx, u.ID)
		}
	}
	m.emitRollout(ctx, r.ID)
	m.kick()
	return m.st.GetRollout(ctx, r.ID)
}

// validateUpdateTarget checks one rollout target: the node must exist
// and be approved, and must not already carry a live update. A node
// already reporting the release version is a skip — its update row is
// still created (keeps rollout counts honest) and finishes succeeded
// immediately.
func (m *Manager) validateUpdateTarget(ctx context.Context, rel *store.Release, nodeID string) (skip bool, err error) {
	n, err := m.st.GetNode(ctx, nodeID)
	if err != nil {
		return false, err // ErrNotFound → 404
	}
	if n.Status != contracts.NodeStatusApproved {
		return false, fmt.Errorf("%w: node %s is %s", ErrInvalidRollout, n.Name, n.Status)
	}
	if v := m.disp.AgentVersion(nodeID); v != "" && v == rel.Version {
		return true, nil
	}
	if _, err := m.st.LiveUpdateForNode(ctx, nodeID); err == nil {
		return false, fmt.Errorf("node %s: %w", n.Name, store.ErrActiveUpdate)
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// advancement (called every tick)
// ---------------------------------------------------------------------------

// advanceRollouts walks every non-terminal rollout and pushes it one
// step: claim newly-eligible pending updates, park finished canaries on
// approval, halt halt-family rollouts on failure, and land fully-terminal
// rollouts. Paused rollouts are swept too — advanceRollout's early
// return is what freezes them.
func (m *Manager) advanceRollouts(ctx context.Context) {
	m.createMu.Lock()
	defer m.createMu.Unlock()
	rollouts, err := m.st.ListRollouts(ctx, 200)
	if err != nil {
		m.log.Debug("list rollouts", "err", err)
		return
	}
	for _, r := range rollouts {
		switch r.State {
		case store.RolloutPending, store.RolloutRunning,
			store.RolloutAwaitingApproval, RolloutPaused:
			m.advanceRollout(ctx, r)
		}
	}
}

func (m *Manager) advanceRollout(ctx context.Context, r *store.Rollout) {
	if r.State == RolloutPaused {
		return // frozen: no claims, no state changes, no auto-halt
	}
	updates, err := m.st.ListAgentUpdates(ctx, r.ID, "", "", 500)
	if err != nil {
		m.log.Debug("list rollout updates", "rollout", r.ID, "err", err)
		return
	}
	// Oldest first — creation order IS the cohort order.
	sort.Slice(updates, func(i, j int) bool { return updates[i].CreatedAt.Before(updates[j].CreatedAt) })

	var active, pend, failed []*store.AgentUpdate
	for _, u := range updates {
		switch {
		case store.UpdTerminal(u.Phase):
			if u.Phase != store.UpdSucceeded {
				failed = append(failed, u)
			}
		case u.Phase == store.UpdPending:
			pend = append(pend, u)
		default:
			active = append(active, u)
		}
	}

	// single/sequential/canary halt on the first failed update;
	// selected/parallel continue and only report failed at the end.
	haltFamily := r.Strategy == StrategySingle ||
		r.Strategy == StrategySequential || r.Strategy == StrategyCanary
	if haltFamily && len(failed) > 0 {
		m.haltRollout(ctx, r, updates, failed[0])
		return
	}

	if len(active)+len(pend) == 0 {
		// All terminal.
		switch {
		case len(failed) > 0:
			c := failed[0]
			m.setRolloutState(ctx, r.ID, store.RolloutFailed,
				fmt.Sprintf("update %s on node %s ended %s (%s)", c.ID, c.NodeID, c.Phase, c.Error))
		default:
			m.setRolloutState(ctx, r.ID, store.RolloutSucceeded, "")
		}
		return
	}

	if r.State == store.RolloutAwaitingApproval {
		return // parked until ApproveNextBatch
	}
	if r.State == store.RolloutPending {
		m.setRolloutState(ctx, r.ID, store.RolloutRunning, "")
	}

	limit := r.BatchSize
	if limit <= 0 {
		limit = 1
	}
	free := limit - len(active)
	if r.Strategy == StrategyCanary {
		started := len(updates) - len(pend) // active + terminal
		if r.ApprovedAt == nil && started <= r.CanaryCount {
			// Canary stage: the cohort is the first canary_count updates
			// ever started (a finished slot is NOT refilled — the whole
			// cohort must land before any post-canary work). When all
			// started updates are terminal and more nodes wait, park for
			// operator approval. approved_at IS NULL is the durable
			// "still gated" bit — once ApproveNextBatch stamps it the
			// rollout runs plain batch semantics forever, across
			// restarts and standby promotions.
			if started == r.CanaryCount && len(active) == 0 && len(pend) > 0 {
				m.setRolloutState(ctx, r.ID, store.RolloutAwaitingApproval, "")
				return
			}
			free = r.CanaryCount - started
		}
		// Approved (or overshot) → plain batch semantics.
	}
	for free > 0 && len(pend) > 0 {
		u := pend[0]
		pend = pend[1:]
		if m.claim(ctx, u) {
			free--
		}
	}
}

// haltRollout stops a halt-family rollout on first failure: cancellable
// live updates are aborted, then the rollout lands 'failed'. installing+
// updates are left to report naturally — a swapped binary can't be
// un-swapped by a message.
func (m *Manager) haltRollout(ctx context.Context, r *store.Rollout, updates []*store.AgentUpdate, cause *store.AgentUpdate) {
	for _, u := range updates {
		if !store.UpdTerminal(u.Phase) {
			m.abortUpdate(ctx, u, "rollout halted after failure")
		}
	}
	m.setRolloutState(ctx, r.ID, store.RolloutFailed,
		fmt.Sprintf("halted: update %s on node %s ended %s (%s)",
			cause.ID, cause.NodeID, cause.Phase, cause.Error))
}

// setRolloutState writes one guarded rollout transition, then logs,
// emits the rollout event, and kicks the loop. Returns whether the
// write landed (false = the row went terminal behind our back).
func (m *Manager) setRolloutState(ctx context.Context, id, state, errStr string) bool {
	ok, err := m.st.SetRolloutState(ctx, id, state, errStr)
	if err != nil {
		m.log.Debug("set rollout state", "rollout", id, "state", state, "err", err)
		return false
	}
	if ok {
		m.log.Info("rollout state", "rollout", id, "state", state, "error", errStr)
		m.emitRollout(ctx, id)
		m.kick()
	}
	return ok
}

// ---------------------------------------------------------------------------
// admin-facing rollout operations
// ---------------------------------------------------------------------------

// ApproveNextBatch releases a canary rollout parked at its approval
// gate: rollouts.approved_by/approved_at are stamped in the same
// guarded UPDATE that flips the rollout back to 'running'
// (store.ApproveRollout), so the decision survives restarts and
// standby promotions — an approved canary never re-parks. The actor
// is the approving admin's principal name, recorded in approved_by.
// Remaining pending updates then proceed batch_size at a time.
//
// A PAUSED rollout may also be approved — approval persists and the
// rollout stays paused until ResumeRollout (see approvePaused).
//
// store.ErrNotFound → 404; ErrNotAwaitingApproval → 409 when the
// rollout isn't at the gate (running, terminal, or a second approve
// that raced the first).
func (m *Manager) ApproveNextBatch(ctx context.Context, rolloutID, actor string) (*store.Rollout, error) {
	r, err := m.st.GetRollout(ctx, rolloutID)
	if err != nil {
		return nil, err // ErrNotFound → 404
	}
	if r.State == RolloutPaused {
		return m.approvePaused(ctx, r, actor)
	}
	if r.State != store.RolloutAwaitingApproval {
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotAwaitingApproval)
	}
	ok, err := m.st.ApproveRollout(ctx, rolloutID, actor)
	if err != nil {
		return nil, err
	}
	if !ok {
		// The row left awaiting_approval between the read and the
		// guarded write — a second approval, or a cancel/terminal
		// landing won the race.
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotAwaitingApproval)
	}
	m.log.Info("rollout approved", "rollout", r.ID, "actor", actor)
	m.emitRollout(ctx, r.ID)
	m.kick()
	return m.st.GetRollout(ctx, r.ID)
}

// approvePaused records a canary approval while the rollout stays
// paused — "approval persists, paused stays paused". ResumeRollout
// then sends it straight to 'running' (approved_at IS NOT NULL skips
// the gate for good).
//
// The store's atomic approver only fires from awaiting_approval and
// lands the row 'running', so the paused path round-trips the state
// through guarded writes:
//
//	paused → awaiting_approval → [ApproveRollout: stamp + running] → paused
//
// Every hop is guarded, so a crash mid-sequence still lands in a
// consistent state — parked-unapproved (approve again) or
// running-approved (the pause is lost but the approval holds; never
// approved-but-unpersisted). The committed 'running' window between
// the last two writes is the width of two UPDATEs — we don't emit or
// kick until 'paused' lands again, and the worst a tick landing inside
// the window can do is claim one pending update, which then rides out
// the pause as ordinary in-flight work.
//
// Only an unapproved canary has a gate to release from; anything else
// (non-canary, or already approved) is ErrNotAwaitingApproval. A
// canary paused BEFORE reaching the gate may be approved early — the
// stamp simply means the gate never engages once the cohort lands.
func (m *Manager) approvePaused(ctx context.Context, r *store.Rollout, actor string) (*store.Rollout, error) {
	if r.Strategy != StrategyCanary || r.ApprovedAt != nil {
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotAwaitingApproval)
	}
	if ok, err := m.st.SetRolloutState(ctx, r.ID, store.RolloutAwaitingApproval, ""); err != nil {
		return nil, err
	} else if !ok {
		// Went terminal between the read and the write.
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotAwaitingApproval)
	}
	ok, err := m.st.ApproveRollout(ctx, r.ID, actor)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Another writer moved it out of awaiting_approval mid-
		// sequence (concurrent approve, resume, or cancel). Restore
		// the pause best-effort; the approval, if it landed at all,
		// was recorded by the racer.
		m.repause(ctx, r.ID)
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotAwaitingApproval)
	}
	m.repause(ctx, r.ID)
	m.log.Info("rollout approved while paused", "rollout", r.ID, "actor", actor)
	m.emitRollout(ctx, r.ID)
	m.kick()
	return m.st.GetRollout(ctx, r.ID)
}

// repause best-effort returns a rollout to 'paused' — the terminal
// guard in SetRolloutState makes it a no-op if the row went terminal.
func (m *Manager) repause(ctx context.Context, id string) {
	if _, err := m.st.SetRolloutState(ctx, id, RolloutPaused, ""); err != nil {
		m.log.Debug("re-pause after paused-approve", "rollout", id, "err", err)
	}
}

// PauseRollout freezes a live rollout: pending updates stop being
// claimed and the engine makes no further state changes — no
// advancement, no re-park, no auto-halt, no final tally — until
// ResumeRollout. Pausing does NOT abort in-flight work: updates
// already claimed or dispatched to agents keep running and report
// normally; a pending update inside a paused rollout simply waits.
//
// Pausable states are 'running' and 'awaiting_approval'. Terminal
// rollouts, already-paused rollouts, and still-'pending' (not yet
// evaluated) ones refuse ErrNotPausable → 409.
func (m *Manager) PauseRollout(ctx context.Context, rolloutID string) (*store.Rollout, error) {
	r, err := m.st.GetRollout(ctx, rolloutID)
	if err != nil {
		return nil, err // ErrNotFound → 404
	}
	switch r.State {
	case store.RolloutRunning, store.RolloutAwaitingApproval:
		// pausable
	default:
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotPausable)
	}
	if !m.setRolloutState(ctx, r.ID, RolloutPaused, "") {
		// Raced a terminal landing between the read and the write.
		fresh, gerr := m.st.GetRollout(ctx, r.ID)
		if gerr != nil {
			return nil, gerr
		}
		return nil, fmt.Errorf("rollout is %s: %w", fresh.State, ErrNotPausable)
	}
	return m.st.GetRollout(ctx, r.ID)
}

// ResumeRollout unfreezes a paused rollout back to 'running'. The
// correct resting state is recomputed by the next advance pass rather
// than remembered: an unapproved canary whose cohort has landed
// re-parks at awaiting_approval (approved_at IS NULL keeps the gate
// live), a canary still mid-cohort just keeps claiming its cohort
// slots, and everything else runs plain batch semantics. A rollout
// that isn't paused refuses ErrNotPaused → 409.
func (m *Manager) ResumeRollout(ctx context.Context, rolloutID string) (*store.Rollout, error) {
	r, err := m.st.GetRollout(ctx, rolloutID)
	if err != nil {
		return nil, err // ErrNotFound → 404
	}
	if r.State != RolloutPaused {
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotPaused)
	}
	if !m.setRolloutState(ctx, r.ID, store.RolloutRunning, "") {
		// Raced a terminal landing between the read and the write.
		fresh, gerr := m.st.GetRollout(ctx, r.ID)
		if gerr != nil {
			return nil, gerr
		}
		return nil, fmt.Errorf("rollout is %s: %w", fresh.State, ErrNotPaused)
	}
	return m.st.GetRollout(ctx, r.ID)
}

// CancelRollout aborts a live rollout: every still-cancellable update is
// cancelled (pre-install rows get an abort frame; installing+ are left
// to report naturally) and the rollout lands 'cancelled'. Terminal
// rollouts refuse ErrNotCancellable → 409.
func (m *Manager) CancelRollout(ctx context.Context, rolloutID string) error {
	r, err := m.st.GetRollout(ctx, rolloutID)
	if err != nil {
		return err // ErrNotFound → 404
	}
	switch r.State {
	case store.RolloutSucceeded, store.RolloutFailed, store.RolloutCancelled:
		return fmt.Errorf("rollout already %s: %w", r.State, ErrNotCancellable)
	}
	// Land 'cancelled' BEFORE aborting updates: each abort kicks the
	// loop, and advanceRollouts only sweeps non-terminal rollouts — so
	// parking first keeps an all-terminal tally from marking the rollout
	// 'failed' ahead of (or over) our cancel.
	m.setRolloutState(ctx, r.ID, store.RolloutCancelled, "")
	updates, err := m.st.ListAgentUpdates(ctx, r.ID, "", "", 500)
	if err != nil {
		return err
	}
	for _, u := range updates {
		if !store.UpdTerminal(u.Phase) {
			m.abortUpdate(ctx, u, "rollout cancelled by administrator")
		}
	}
	return nil
}

// GetRollout returns one rollout with live update counts.
func (m *Manager) GetRollout(ctx context.Context, id string) (*store.Rollout, error) {
	return m.st.GetRollout(ctx, id)
}

// ListRollouts returns campaigns, newest first, with update counts.
func (m *Manager) ListRollouts(ctx context.Context, limit int) ([]*store.Rollout, error) {
	return m.st.ListRollouts(ctx, limit)
}
