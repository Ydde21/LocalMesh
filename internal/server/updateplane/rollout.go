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
// "Halt" cancels every still-cancellable live update (installing+ are
// left to report naturally) and lands the rollout 'failed'.
// ---------------------------------------------------------------------------

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
// rollouts.
func (m *Manager) advanceRollouts(ctx context.Context) {
	rollouts, err := m.st.ListRollouts(ctx, 200)
	if err != nil {
		m.log.Debug("list rollouts", "err", err)
		return
	}
	for _, r := range rollouts {
		switch r.State {
		case store.RolloutPending, store.RolloutRunning, store.RolloutAwaitingApproval:
			m.advanceRollout(ctx, r)
		}
	}
}

func (m *Manager) advanceRollout(ctx context.Context, r *store.Rollout) {
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
		if !m.isCanaryApproved(r.ID) && started <= r.CanaryCount {
			// Canary stage: the cohort is the first canary_count updates
			// ever started (a finished slot is NOT refilled — the whole
			// cohort must land before any post-canary work). When all
			// started updates are terminal and more nodes wait, park for
			// operator approval. canaryOK (set by ApproveNextBatch) keeps
			// an approved rollout from re-parking while its post-canary
			// batch is still being claimed.
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

func (m *Manager) setRolloutState(ctx context.Context, id, state, errStr string) {
	ok, err := m.st.SetRolloutState(ctx, id, state, errStr)
	if err != nil {
		m.log.Debug("set rollout state", "rollout", id, "state", state, "err", err)
		return
	}
	if ok {
		m.log.Info("rollout state", "rollout", id, "state", state, "error", errStr)
		m.emitRollout(ctx, id)
		if state == store.RolloutSucceeded || state == store.RolloutFailed ||
			state == store.RolloutCancelled {
			m.mu.Lock()
			delete(m.canaryOK, id)
			m.mu.Unlock()
		}
		m.kick()
	}
}

// isCanaryApproved reports whether a canary rollout was released past
// its approval gate by ApproveNextBatch (in-memory; see Manager.canaryOK).
func (m *Manager) isCanaryApproved(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canaryOK[id]
}

// ---------------------------------------------------------------------------
// admin-facing rollout operations
// ---------------------------------------------------------------------------

// ApproveNextBatch releases a canary rollout parked in
// awaiting_approval: remaining pending updates proceed batch_size at a
// time.
func (m *Manager) ApproveNextBatch(ctx context.Context, rolloutID string) (*store.Rollout, error) {
	r, err := m.st.GetRollout(ctx, rolloutID)
	if err != nil {
		return nil, err // ErrNotFound → 404
	}
	if r.State != store.RolloutAwaitingApproval {
		return nil, fmt.Errorf("rollout is %s: %w", r.State, ErrNotAwaitingApproval)
	}
	m.mu.Lock()
	m.canaryOK[r.ID] = true
	m.mu.Unlock()
	m.setRolloutState(ctx, r.ID, store.RolloutRunning, "")
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
