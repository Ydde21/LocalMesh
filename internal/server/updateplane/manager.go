// Package updateplane is the server-side orchestration engine for v0.7
// centralized agent upgrades. It mirrors the modelops queue design:
// agent_updates rows are durable state before they are wire frames —
// pending updates are claimed under the control-plane fencing epoch,
// drain their node's workloads under maintenance='updating', receive an
// AgentUpgrade frame, progress through agent-reported phases, and are
// proven complete by the version the agent reports in its post-restart
// hello. Everything survives control-plane restarts.
//
// ---------------------------------------------------------------------------
// INTEGRATOR WIRING (this package owns no production wiring itself)
// ---------------------------------------------------------------------------
//
//	server.go:
//	  mgr := updateplane.New(st, nodehub.NewUpgradeDispatcher(hub, st),
//	      updateplane.Config{…}, log)
//	  mgr.SetEpoch(ownership.Epoch())      // REQUIRED — claims are fenced
//	  mgr.SetNotifier(ev.Publish)          // SSE fan-out
//	  go mgr.Run(ctx)
//
//	nodehub (new adapter, mirroring OpDispatcher):
//	  SendUpgrade — write Envelope{Type: MsgAgentUpgrade, ID: u.OperationID,
//	      Payload: u} on the node conn; ErrNodeOffline when absent.
//	  AbortUpgrade — Envelope{Type: MsgUpgradeAbort,
//	      Payload: UpgradeAbort{OperationID: opID}}.
//	  Online — hub.Online(nodeID).
//	  NodeDrained — hub.InFlight(nodeID)==0 AND no dispatched/running
//	      model ops for the node (st.ModelOpsActiveByNode).
//	  AgentVersion — the conn's hello-reported version (nc.agentVer);
//	      "" when offline.
//
//	nodehub.HandleConnection must call mgr.OnNodeConnected(node.ID,
//	  hello.AgentVersion) after register() and mgr.OnNodeDisconnected(node.ID)
//	  in unregister() — alongside the existing opQueue hooks. handleFrame
//	  routes contracts.MsgUpgradeState → mgr.OnUpgradeState(ctx, nodeID, st).
//
//	adminapi handlers delegate to mgr for release-keys, releases,
//	  rollouts, agent-updates, and node maintenance per
//	  docs/update-orchestration-api.md.
//
// ---------------------------------------------------------------------------
// PHASE MODEL (store/updates.go is authoritative)
// ---------------------------------------------------------------------------
//
//	pending → draining → dispatched → verifying → downloading → staging
//	  → installing → restarting → verify_health → succeeded
//	  terminal: succeeded | failed | rolled_back | cancelled | interrupted
//
// The agent reports progress via upgrade_state frames; success is proven
// not by a frame but by the agent reconnecting with hello.agent_version
// == update.to_version (verify_health), then staying connected (health).
package updateplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/store"
)

// Config tunes the orchestrator's bounds and timeouts.
type Config struct {
	// DrainTimeout bounds how long an update waits in 'draining' for the
	// node's in-flight inference and model ops to finish before the
	// upgrade frame is sent. Default 120s; expiry fails the update.
	DrainTimeout time.Duration
	// HealthVerifyTimeout bounds the post-install window (installing/
	// restarting/verify_health) in which the agent must reconnect AND
	// report the target version. Default 5m.
	HealthVerifyTimeout time.Duration
	// ReconnectTimeout bounds how long an update in a reboot phase
	// tolerates its node being offline before failing "did not
	// reconnect". Default 3m.
	ReconnectTimeout time.Duration
	// TickInterval is the orchestration loop cadence. Default 1s.
	TickInterval time.Duration
	// MaxBatchSize is the hard cap on rollout batch_size and
	// canary_count. Default 8.
	MaxBatchSize int
	// QueueMax bounds total live (unfinished) agent updates; creating a
	// rollout that would exceed it fails ErrQueueFull (caller → 429).
	// Default 256.
	QueueMax int
}

func (c Config) withDefaults() Config {
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 120 * time.Second
	}
	if c.HealthVerifyTimeout <= 0 {
		c.HealthVerifyTimeout = 5 * time.Minute
	}
	if c.ReconnectTimeout <= 0 {
		c.ReconnectTimeout = 3 * time.Minute
	}
	if c.TickInterval <= 0 {
		c.TickInterval = time.Second
	}
	if c.MaxBatchSize <= 0 {
		c.MaxBatchSize = 8
	}
	if c.QueueMax <= 0 {
		c.QueueMax = 256
	}
	return c
}

// Dispatcher is the wire-facing half of the orchestrator — implemented
// over nodehub in production (see package doc), faked in tests.
type Dispatcher interface {
	// SendUpgrade delivers the MsgAgentUpgrade frame to the node.
	SendUpgrade(ctx context.Context, nodeID string, u *contracts.AgentUpgrade) error
	// AbortUpgrade asks the node to cancel a not-yet-installed upgrade
	// (MsgUpgradeAbort). Best effort — an unreachable node can't confirm.
	AbortUpgrade(ctx context.Context, nodeID, opID string) error
	// Online reports whether the node has a live connection.
	Online(nodeID string) bool
	// NodeDrained reports whether the node has zero in-flight inference
	// requests AND zero dispatched/running model ops — the precondition
	// for dispatching an upgrade.
	NodeDrained(nodeID string) bool
	// AgentVersion returns the hello-reported agent version for the
	// node's live connection, or "" when offline/unknown.
	AgentVersion(nodeID string) string
}

// Notifier publishes lifecycle events (wired to events.Broker.Publish by
// the integrator; nil-safe). Event names: "agent.update" (AgentUpdate
// JSON), "rollout" (Rollout JSON), "release" ({kind, release|release_key}).
type Notifier func(event string, payload any)

var (
	// ErrQueueFull — the live-update backlog is at QueueMax; caller → 429.
	ErrQueueFull = errors.New("too many live agent updates")
	// ErrNotCancellable — the update/rollout can no longer be cancelled
	// (binary may already be swapped) or is already finished; caller → 409.
	ErrNotCancellable = errors.New("agent update can no longer be cancelled")
	// ErrUnsignedRelease — the manifest declares signer "unsigned";
	// registration refuses it; caller → 400.
	ErrUnsignedRelease = errors.New("release manifest is unsigned")
	// ErrReleaseNotApproved — the release is revoked (or otherwise not
	// approved); new rollouts are refused; caller → 409.
	ErrReleaseNotApproved = errors.New("release is not approved")
	// ErrInvalidRollout — strategy/target/bounds validation failed;
	// caller → 400. Wrap with fmt.Errorf("%w: detail").
	ErrInvalidRollout = errors.New("invalid rollout")
	// ErrNotAwaitingApproval — ApproveNextBatch on a rollout not in
	// awaiting_approval; caller → 409.
	ErrNotAwaitingApproval = errors.New("rollout is not awaiting approval")
)

// maintReasonPrefix marks node maintenance_reason values this
// orchestrator wrote — "update:<update-id>". clearUpdating only clears
// markers it can attribute to the finished update, so admin maintenance
// states are never clobbered.
const maintReasonPrefix = "update:"

// Rollout strategies.
const (
	StrategySingle     = "single"     // exactly one node
	StrategySelected   = "selected"   // explicit list, batch_size concurrent
	StrategySequential = "sequential" // one at a time, auto-advance, halt on failure
	StrategyCanary     = "canary"     // first canary_count nodes, then await approval
	StrategyParallel   = "parallel"   // batch_size concurrent, continue past failures
)

// livePhases is every non-terminal update phase, in lifecycle order.
var livePhases = []string{
	store.UpdPending, store.UpdDraining, store.UpdDispatched,
	store.UpdVerifying, store.UpdDownloading, store.UpdStaging,
	store.UpdInstalling, store.UpdRestarting, store.UpdVerifyHealth,
}

// preInstallPhases are dispatched-but-not-installed: the frame may or
// may not have arrived, and a conn drop here kills the upgrade with it —
// a disconnect or a pre-upgrade reconnect lands the row 'interrupted'.
var preInstallPhases = map[string]bool{
	store.UpdDispatched: true, store.UpdVerifying: true,
	store.UpdDownloading: true, store.UpdStaging: true,
}

// rebootPhases tolerate conn loss: the agent is (about to be) rebooting.
// The verdict comes from the next hello's agent_version, bounded by
// ReconnectTimeout/HealthVerifyTimeout.
var rebootPhases = map[string]bool{
	store.UpdInstalling: true, store.UpdRestarting: true, store.UpdVerifyHealth: true,
}

// Manager owns the update orchestration loop.
type Manager struct {
	st     *store.DB
	disp   Dispatcher
	log    *slog.Logger
	notify Notifier

	mu     sync.Mutex
	cfg    Config
	online map[string]bool // last observed connectivity per node with live updates
	epoch  int64           // control-plane fencing token; claims need it

	// canaryOK records canary rollouts released by ApproveNextBatch so a
	// 'running' rollout isn't re-parked at awaiting_approval while its
	// post-canary batch is still claiming. In-memory only: a control-
	// plane restart inside the approved-but-unclaimed window re-parks
	// the rollout — conservative (no unapproved work dispatches) and the
	// operator simply approves again.
	canaryOK map[string]bool

	wake chan struct{} // event-driven nudge (capacity 1)
}

// New builds a Manager. Run drives it; Reconcile is invoked at Run start.
// Claims require the fencing epoch: production MUST call SetEpoch with
// the ownership epoch before Run (tests may use epoch 0 — the migration
// default of control_plane_lock).
func New(st *store.DB, disp Dispatcher, cfg Config, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		st:       st,
		disp:     disp,
		cfg:      cfg.withDefaults(),
		log:      log.With("component", "updateplane"),
		online:   map[string]bool{},
		canaryOK: map[string]bool{},
		epoch:    -1, // unfenced until SetEpoch — claims will simply miss
		wake:     make(chan struct{}, 1),
	}
}

// SetEpoch installs the control-plane ownership fencing token. Claims
// match zero rows once a successor bumps control_plane_lock.epoch — a
// stale process cannot dispatch upgrades.
func (m *Manager) SetEpoch(epoch int64) {
	m.mu.Lock()
	m.epoch = epoch
	m.mu.Unlock()
}

// SetNotifier installs the event publisher.
func (m *Manager) SetNotifier(n Notifier) { m.notify = n }

// UpdateConfig live-reloads bounds/timeouts; zero fields keep their
// current value.
func (m *Manager) UpdateConfig(cfg Config) {
	m.mu.Lock()
	c := m.cfg
	if cfg.DrainTimeout > 0 {
		c.DrainTimeout = cfg.DrainTimeout
	}
	if cfg.HealthVerifyTimeout > 0 {
		c.HealthVerifyTimeout = cfg.HealthVerifyTimeout
	}
	if cfg.ReconnectTimeout > 0 {
		c.ReconnectTimeout = cfg.ReconnectTimeout
	}
	if cfg.TickInterval > 0 {
		c.TickInterval = cfg.TickInterval
	}
	if cfg.MaxBatchSize > 0 {
		c.MaxBatchSize = cfg.MaxBatchSize
	}
	if cfg.QueueMax > 0 {
		c.QueueMax = cfg.QueueMax
	}
	m.cfg = c
	m.mu.Unlock()
	m.kick()
}

// Config returns the active configuration.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *Manager) config() Config { return m.Config() }

func (m *Manager) getEpoch() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epoch
}

func (m *Manager) emit(event string, payload any) {
	if m.notify != nil {
		m.notify(event, payload)
	}
}

func (m *Manager) kick() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// emitUpdate publishes the freshest copy of one update row.
func (m *Manager) emitUpdate(ctx context.Context, id string) {
	if m.notify == nil {
		return
	}
	if u, err := m.st.GetAgentUpdate(ctx, id); err == nil {
		m.notify("agent.update", u)
	}
}

// emitRollout publishes the freshest rollout row (with live counts).
func (m *Manager) emitRollout(ctx context.Context, id string) {
	if m.notify == nil || id == "" {
		return
	}
	if r, err := m.st.GetRollout(ctx, id); err == nil {
		m.notify("rollout", r)
	}
}

// ---------------------------------------------------------------------------
// run loop
// ---------------------------------------------------------------------------

// Run reconciles leftovers from a previous life, then orchestrates until
// ctx ends. Call it in a goroutine.
func (m *Manager) Run(ctx context.Context) {
	if err := m.Reconcile(ctx); err != nil {
		m.log.Warn("reconcile failed", "err", err)
	}
	t := time.NewTicker(m.config().TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
		}
		m.tick(ctx)
	}
}

// Reconcile recovers rows a previous process left behind:
//   - dispatched..installing rows whose node is offline → interrupted
//     (the frame's outcome is unknowable);
//   - restarting/verify_health rows persist — the agent may still come
//     back at the target version; reconnect/health timeouts bound the wait;
//   - draining rows keep their phase and resume drain checks next tick
//     (dispatching immediately when the node is already drained);
//   - pending rows claim naturally.
func (m *Manager) Reconcile(ctx context.Context) error {
	live, err := m.st.LiveUpdatesByPhase(ctx, livePhases...)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, u := range live {
		m.online[u.NodeID] = m.disp.Online(u.NodeID)
	}
	m.mu.Unlock()
	interrupted := 0
	for _, u := range live {
		if u.Phase == store.UpdPending || u.Phase == store.UpdDraining ||
			u.Phase == store.UpdRestarting || u.Phase == store.UpdVerifyHealth {
			continue
		}
		if !m.disp.Online(u.NodeID) {
			m.finishUpdate(ctx, u.ID, store.UpdInterrupted,
				"control plane restarted; dispatch outcome unknown", "")
			interrupted++
		}
	}
	if interrupted > 0 {
		m.log.Warn("interrupted orphaned agent updates", "count", interrupted)
	}
	return nil
}

// tick runs one orchestration pass.
func (m *Manager) tick(ctx context.Context) {
	m.refreshConnectivity(ctx)
	m.advanceRollouts(ctx)
	m.claimStandalone(ctx)
	m.processDraining(ctx)
	m.processRebooting(ctx)
}

// ---------------------------------------------------------------------------
// node lifecycle hooks (wired to nodehub register/unregister/frame loop)
// ---------------------------------------------------------------------------

// OnNodeConnected is the promotion/verification path: the hub calls it
// after register() with the hello-reported agent version.
func (m *Manager) OnNodeConnected(nodeID, agentVersion string) {
	m.mu.Lock()
	m.online[nodeID] = true
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m.verifyConnected(ctx, nodeID, agentVersion)
	m.kick()
}

// OnNodeDisconnected handles conn drops: pre-install updates interrupt
// (the frame may not have arrived); reboot-phase updates persist (the
// agent is likely bouncing); pending/draining keep waiting.
func (m *Manager) OnNodeDisconnected(nodeID string) {
	m.mu.Lock()
	m.online[nodeID] = false
	m.mu.Unlock()
	m.handleDisconnect(nodeID)
	m.kick()
}

// verifyConnected applies the version-verdict rules for a node that just
// (re)connected while holding a live update.
func (m *Manager) verifyConnected(ctx context.Context, nodeID, agentVersion string) {
	u, err := m.st.LiveUpdateForNode(ctx, nodeID)
	if err != nil {
		return // no live update — nothing to verify
	}
	switch {
	case rebootPhases[u.Phase]:
		m.checkVersion(ctx, u, agentVersion)
	case preInstallPhases[u.Phase]:
		// A reconnect during a pre-install phase means the previous conn
		// (which carried the upgrade) is gone — outcome unknowable.
		m.finishUpdate(ctx, u.ID, store.UpdInterrupted,
			"node reconnected during "+u.Phase+"; prior dispatch outcome unknown", "")
	default:
		// pending/draining: the drain check proceeds next tick.
	}
}

// checkVersion is the promotion decision shared by OnNodeConnected and
// the self-healing tick: a reboot-phase update whose node reports the
// target version promotes to verify_health; a node still on from_version
// keeps waiting (it may be a pre-upgrade reconnect); anything else is an
// unexpected version and fails the update.
func (m *Manager) checkVersion(ctx context.Context, u *store.AgentUpdate, ver string) {
	switch {
	case u.ToVersion != "" && ver == u.ToVersion:
		if u.Phase != store.UpdVerifyHealth {
			m.setPhase(ctx, u.ID, store.UpdVerifyHealth,
				"agent reconnected reporting "+ver, "")
		}
	case ver == "", u.FromVersion != "" && ver == u.FromVersion, u.FromVersion == "":
		// Old binary (or unknown starting version — can't distinguish a
		// pre-upgrade reconnect from a wrong version, so wait for the
		// health-verify deadline rather than fail an innocent reconnect).
	default:
		m.finishUpdate(ctx, u.ID, store.UpdFailed,
			fmt.Sprintf("unexpected agent version %s after upgrade (wanted %s)", ver, u.ToVersion), "")
	}
}

// handleDisconnect interrupts a dropped node's pre-install updates;
// reboot phases and pending/draining are left to their timeouts.
func (m *Manager) handleDisconnect(nodeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u, err := m.st.LiveUpdateForNode(ctx, nodeID)
	if err != nil {
		return
	}
	if preInstallPhases[u.Phase] {
		m.finishUpdate(ctx, u.ID, store.UpdInterrupted,
			"node disconnected mid-update; frame outcome unknown", "")
	}
}

// OnUpgradeState applies an agent upgrade_state frame. Progress phases
// move the row; terminal frames finish it. Frames for a different node
// or a terminal row are dropped — the store's finished_at guard is the
// last word either way.
func (m *Manager) OnUpgradeState(ctx context.Context, nodeID string, st contracts.UpgradeState) {
	u, err := m.st.GetAgentUpdate(ctx, st.OperationID)
	if err != nil {
		m.log.Debug("upgrade_state for unknown update", "op", st.OperationID, "err", err)
		return
	}
	if u.NodeID != nodeID {
		m.log.Warn("upgrade_state from wrong node", "op", st.OperationID,
			"update_node", u.NodeID, "frame_node", nodeID)
		return
	}
	if store.UpdTerminal(u.Phase) {
		return // late frame on a finished row
	}
	switch st.Phase {
	case store.UpdVerifying, store.UpdDownloading, store.UpdStaging,
		store.UpdInstalling, store.UpdRestarting, store.UpdVerifyHealth:
		m.setPhase(ctx, u.ID, st.Phase, st.Detail, st.Error)
	case store.UpdFailed:
		m.finishUpdate(ctx, u.ID, store.UpdFailed,
			firstNonEmpty(st.Error, "agent reported failure"), st.Detail)
	case store.UpdRolledBack:
		m.finishUpdate(ctx, u.ID, store.UpdRolledBack,
			firstNonEmpty(st.Error, "agent rolled back"), st.Detail)
	case "aborted":
		m.finishUpdate(ctx, u.ID, store.UpdCancelled,
			firstNonEmpty(st.Error, "aborted by agent"), st.Detail)
	case store.UpdSucceeded:
		// A frame claiming success is accepted as far as verify_health —
		// the authoritative proof is the next hello's version match.
		m.setPhase(ctx, u.ID, store.UpdVerifyHealth,
			firstNonEmpty(st.Detail, "agent reports success; awaiting version check"), "")
	default:
		m.log.Debug("unknown upgrade_state phase", "op", st.OperationID, "phase", st.Phase)
	}
	m.kick()
}

// ---------------------------------------------------------------------------
// connectivity diff (self-healing when hub hooks are absent/late)
// ---------------------------------------------------------------------------

// refreshConnectivity diffs observed online state across nodes holding
// live updates: online→offline interrupts pre-install rows,
// offline→online runs the version check.
func (m *Manager) refreshConnectivity(ctx context.Context) {
	live, err := m.st.LiveUpdatesByPhase(ctx, livePhases...)
	if err != nil {
		m.log.Debug("list live updates", "err", err)
		return
	}
	cur := map[string]bool{}
	for _, u := range live {
		cur[u.NodeID] = m.disp.Online(u.NodeID)
	}
	m.mu.Lock()
	prev := m.online
	m.online = cur
	m.mu.Unlock()
	seen := make(map[string]bool, len(cur)+len(prev))
	for n := range cur {
		seen[n] = true
	}
	for n := range prev {
		seen[n] = true
	}
	for n := range seen {
		was, known := prev[n]
		switch {
		case known && was && !cur[n]:
			m.handleDisconnect(n)
		case known && !was && cur[n]:
			m.verifyConnected(ctx, n, m.disp.AgentVersion(n))
		}
	}
}

// ---------------------------------------------------------------------------
// dispatch machinery
// ---------------------------------------------------------------------------

// claim moves a pending update to 'draining' under the fencing epoch and
// fences the node off from new work (maintenance='updating'). Offline
// nodes are not claimed — pending waits for the node, so a host that is
// down doesn't burn its drain budget.
func (m *Manager) claim(ctx context.Context, u *store.AgentUpdate) bool {
	if !m.disp.Online(u.NodeID) {
		return false
	}
	ok, err := m.st.ClaimPendingUpdate(ctx, u.ID, m.getEpoch())
	if err != nil {
		m.log.Debug("claim pending update", "update", u.ID, "err", err)
		return false
	}
	if !ok {
		return false // raced or stale epoch
	}
	if err := m.markUpdating(ctx, u.NodeID, u.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			m.finishUpdate(ctx, u.ID, store.UpdFailed, "node record not found", "")
			return false
		}
		m.log.Warn("set updating maintenance", "node", u.NodeID, "err", err)
	}
	m.log.Debug("update claimed, draining", "update", u.ID, "node", u.NodeID)
	m.emitUpdate(ctx, u.ID)
	return true
}

// claimStandalone claims pending updates that carry no rollout id —
// defensive coverage for rows created outside the rollout engine.
func (m *Manager) claimStandalone(ctx context.Context) {
	pend, err := m.st.LiveUpdatesByPhase(ctx, store.UpdPending)
	if err != nil {
		return
	}
	for _, u := range pend {
		if u.RolloutID == "" {
			m.claim(ctx, u)
		}
	}
}

// processDraining advances claimed updates: drained nodes get the
// upgrade frame; nodes still busy wait until DrainTimeout.
func (m *Manager) processDraining(ctx context.Context) {
	rows, err := m.st.LiveUpdatesByPhase(ctx, store.UpdDraining)
	if err != nil {
		return
	}
	cfg := m.config()
	now := time.Now()
	for _, u := range rows {
		switch {
		case now.Sub(u.UpdatedAt) > cfg.DrainTimeout:
			m.finishUpdate(ctx, u.ID, store.UpdFailed,
				fmt.Sprintf("drain timeout: node still busy after %s", cfg.DrainTimeout), "")
		case !m.disp.Online(u.NodeID):
			// Node dropped mid-drain — keep waiting inside the same
			// drain budget (dispatch needs the conn anyway).
		case m.disp.NodeDrained(u.NodeID):
			m.dispatchUpgrade(ctx, u)
		}
	}
}

// dispatchUpgrade builds the wire frame and sends it. Source selection:
// release.base_url verbatim when set (the agent fetches artifacts from
// the release host directly); otherwise "localmesh://releases/<id>" —
// the control-plane release proxy for agents without outbound access.
// A send failure lands the row 'interrupted': the frame may have
// arrived, so the same op is never blindly re-sent.
func (m *Manager) dispatchUpgrade(ctx context.Context, u *store.AgentUpdate) {
	rel, err := m.st.GetRelease(ctx, u.ReleaseID)
	if err != nil {
		m.finishUpdate(ctx, u.ID, store.UpdFailed,
			"release no longer readable: "+err.Error(), "")
		return
	}
	src := rel.BaseURL
	if src == "" {
		src = "localmesh://releases/" + rel.ID
	}
	frame := &contracts.AgentUpgrade{
		OperationID:       u.ID,
		ReleaseID:         rel.ID,
		Version:           rel.Version,
		Source:            src,
		KeyID:             rel.SignerKeyID,
		ProtocolVersion:   rel.ProtocolVersion,
		MinAgentProtocol:  rel.MinAgentProtocol,
		AllowInsecureHTTP: strings.HasPrefix(src, "http://"),
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = m.disp.SendUpgrade(sctx, u.NodeID, frame)
	cancel()
	if err != nil {
		m.finishUpdate(ctx, u.ID, store.UpdInterrupted,
			"dispatch failed (outcome unknown): "+err.Error(), "")
		return
	}
	m.log.Debug("upgrade dispatched", "update", u.ID, "node", u.NodeID, "version", rel.Version)
	m.setPhase(ctx, u.ID, store.UpdDispatched, "upgrade frame sent", "")
}

// processRebooting drives the post-install window: version-match
// promotion for nodes that came back, success for verified+healthy
// nodes, and the reconnect/health timeouts.
func (m *Manager) processRebooting(ctx context.Context) {
	rows, err := m.st.LiveUpdatesByPhase(ctx,
		store.UpdInstalling, store.UpdRestarting, store.UpdVerifyHealth)
	if err != nil {
		return
	}
	cfg := m.config()
	now := time.Now()
	for _, u := range rows {
		age := now.Sub(u.UpdatedAt)
		if !m.disp.Online(u.NodeID) {
			if age > cfg.ReconnectTimeout {
				m.finishUpdate(ctx, u.ID, store.UpdFailed,
					"agent did not reconnect after install", "")
			}
			continue
		}
		ver := m.disp.AgentVersion(u.NodeID)
		m.checkVersion(ctx, u, ver) // self-healing version check
		if u.Phase == store.UpdVerifyHealth && u.ToVersion != "" && ver == u.ToVersion {
			// Health gate: the agent reconnected at the target version
			// and holds a live conn — the upgrade is proven.
			m.finishUpdate(ctx, u.ID, store.UpdSucceeded, "",
				"agent reconnected at "+ver+" and is healthy")
			continue
		}
		if age > cfg.HealthVerifyTimeout {
			m.finishUpdate(ctx, u.ID, store.UpdFailed,
				"agent did not confirm new version", "")
		}
	}
}

// ---------------------------------------------------------------------------
// phase-change + maintenance helpers
// ---------------------------------------------------------------------------

// setPhase moves a live update row and emits its event.
func (m *Manager) setPhase(ctx context.Context, id, phase, detail, errStr string) bool {
	ok, err := m.st.SetUpdatePhase(ctx, id, phase, detail, errStr)
	if err != nil {
		m.log.Debug("set update phase", "update", id, "phase", phase, "err", err)
		return false
	}
	if ok {
		m.emitUpdate(ctx, id)
	}
	return ok
}

// finishUpdate lands an update in a terminal phase, releases the
// maintenance marker it owned, and emits update + rollout events.
func (m *Manager) finishUpdate(ctx context.Context, id, phase, errStr, detail string) bool {
	ok, err := m.st.FinishUpdate(ctx, id, phase, errStr, detail)
	if err != nil {
		m.log.Debug("finish update", "update", id, "phase", phase, "err", err)
		return false
	}
	if !ok {
		return false // raced to terminal already
	}
	u, gerr := m.st.GetAgentUpdate(ctx, id)
	if gerr == nil {
		m.clearUpdating(ctx, u)
		m.log.Info("agent update finished", "update", id, "node", u.NodeID,
			"phase", phase, "error", errStr)
		m.emitUpdate(ctx, id)
		m.emitRollout(ctx, u.RolloutID)
	}
	m.kick()
	return true
}

// markUpdating fences a node off from new work for the duration of the
// update. A node already in an admin maintenance state is left as-is —
// it is already fenced, and its state is not ours to clear later.
func (m *Manager) markUpdating(ctx context.Context, nodeID, updateID string) error {
	n, err := m.st.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if n.Maintenance != store.MaintNone {
		return nil
	}
	return m.st.SetMaintenance(ctx, nodeID, store.MaintUpdating, maintReasonPrefix+updateID)
}

// clearUpdating releases the maintenance marker — only when the node
// carries the 'updating' state with THIS update's reason marker.
func (m *Manager) clearUpdating(ctx context.Context, u *store.AgentUpdate) {
	n, err := m.st.GetNode(ctx, u.NodeID)
	if err != nil {
		return
	}
	if n.Maintenance == store.MaintUpdating &&
		n.MaintenanceReason == maintReasonPrefix+u.ID {
		if err := m.st.SetMaintenance(ctx, u.NodeID, store.MaintNone, ""); err != nil {
			m.log.Debug("clear updating maintenance", "node", u.NodeID, "err", err)
		}
	}
}

// abortUpdate cancels one live update best-effort: pending/draining rows
// finish in place; pre-install rows get an abort frame first;
// installing+ are left — the binary may already be swapped and only the
// agent's own report (or rollback) can settle them.
func (m *Manager) abortUpdate(ctx context.Context, u *store.AgentUpdate, reason string) {
	switch u.Phase {
	case store.UpdPending, store.UpdDraining:
		m.finishUpdate(ctx, u.ID, store.UpdCancelled, "", reason)
	case store.UpdDispatched, store.UpdVerifying, store.UpdDownloading, store.UpdStaging:
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := m.disp.AbortUpgrade(actx, u.NodeID, u.ID); err != nil {
			m.log.Debug("abort frame failed", "update", u.ID, "err", err)
		}
		cancel()
		m.finishUpdate(ctx, u.ID, store.UpdCancelled, "", reason)
	default:
		// installing/restarting/verify_health — past the point of no return.
	}
}

// ---------------------------------------------------------------------------
// admin-facing update operations
// ---------------------------------------------------------------------------

// CancelUpdate aborts a live update. pending/draining finish cancelled in
// place; dispatched..staging get an UpgradeAbort frame then finish
// cancelled; installing and later refuse ErrNotCancellable — a binary
// already swapped cannot be un-swapped by a message.
func (m *Manager) CancelUpdate(ctx context.Context, id string) error {
	u, err := m.st.GetAgentUpdate(ctx, id)
	if err != nil {
		return err // store.ErrNotFound → 404
	}
	switch u.Phase {
	case store.UpdPending, store.UpdDraining,
		store.UpdDispatched, store.UpdVerifying, store.UpdDownloading, store.UpdStaging:
		m.abortUpdate(ctx, u, "cancelled by administrator")
		m.emitRollout(ctx, u.RolloutID)
		return nil
	default:
		return fmt.Errorf("update in phase %q: %w", u.Phase, ErrNotCancellable)
	}
}

// GetUpdate returns one update row.
func (m *Manager) GetUpdate(ctx context.Context, id string) (*store.AgentUpdate, error) {
	return m.st.GetAgentUpdate(ctx, id)
}

// ListUpdates lists update rows, newest first; optional rollout,
// node, and phase filters (limit clamps to [1,500]).
func (m *Manager) ListUpdates(ctx context.Context, rolloutID, nodeID, phase string, limit int) ([]*store.AgentUpdate, error) {
	return m.st.ListAgentUpdates(ctx, rolloutID, nodeID, phase, limit)
}

// LiveUpdateForNode returns the node's unfinished update, or
// store.ErrNotFound.
func (m *Manager) LiveUpdateForNode(ctx context.Context, nodeID string) (*store.AgentUpdate, error) {
	return m.st.LiveUpdateForNode(ctx, nodeID)
}

// ---------------------------------------------------------------------------
// node maintenance (admin API delegates here so the marker rules live in
// exactly one place)
// ---------------------------------------------------------------------------

// SetNodeMaintenance applies an admin drain ('draining') or maintenance
// ('maintenance') state. 'updating' is orchestrator-owned — an admin
// cannot set it, and a node fenced by a live update refuses admin state
// changes (cancel the update first). reason is recorded verbatim.
func (m *Manager) SetNodeMaintenance(ctx context.Context, nodeID, state, reason string) error {
	if state != store.MaintDraining && state != store.MaintMode {
		return fmt.Errorf("state must be %q or %q: %w",
			store.MaintDraining, store.MaintMode, ErrInvalidRollout)
	}
	if err := m.guardUpdateMaintenance(ctx, nodeID); err != nil {
		return err
	}
	return m.st.SetMaintenance(ctx, nodeID, state, reason)
}

// ResumeNode clears admin maintenance ('draining'/'maintenance' → ”).
// A node fenced by a live update refuses — the update owns that marker;
// a stale marker whose update is already terminal is cleared as repair.
func (m *Manager) ResumeNode(ctx context.Context, nodeID string) error {
	if err := m.guardUpdateMaintenance(ctx, nodeID); err != nil {
		return err
	}
	return m.st.SetMaintenance(ctx, nodeID, store.MaintNone, "")
}

// guardUpdateMaintenance refuses admin maintenance writes while a live
// update owns the node's 'updating' marker.
func (m *Manager) guardUpdateMaintenance(ctx context.Context, nodeID string) error {
	n, err := m.st.GetNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if n.Maintenance == store.MaintUpdating &&
		strings.HasPrefix(n.MaintenanceReason, maintReasonPrefix) {
		if _, lerr := m.st.LiveUpdateForNode(ctx, nodeID); lerr == nil {
			return fmt.Errorf("node is mid-update; cancel the update first: %w",
				store.ErrActiveUpdate)
		}
	}
	return nil
}

func firstNonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
