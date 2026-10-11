package updateplane

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/release"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

// fakeDisp is a Dispatcher test double: connectivity/drain state and the
// agent's reported version are scripted per node; sends/aborts record.
type fakeDisp struct {
	mu       sync.Mutex
	online   map[string]bool
	drained  map[string]bool
	versions map[string]string
	sent     []*contracts.AgentUpgrade
	aborted  []string
	failNext int
}

func newFakeDisp() *fakeDisp {
	return &fakeDisp{
		online:   map[string]bool{},
		drained:  map[string]bool{},
		versions: map[string]string{},
	}
}

func (f *fakeDisp) Online(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[id]
}

func (f *fakeDisp) NodeDrained(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drained[id]
}

func (f *fakeDisp) AgentVersion(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.versions[id]
}

func (f *fakeDisp) SendUpgrade(ctx context.Context, nodeID string, u *contracts.AgentUpgrade) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *u
	f.sent = append(f.sent, &cp)
	if f.failNext > 0 {
		f.failNext--
		return errors.New("connection reset")
	}
	return nil
}

func (f *fakeDisp) AbortUpgrade(ctx context.Context, nodeID, opID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = append(f.aborted, opID)
	return nil
}

func (f *fakeDisp) set(nodeID string, online, drained bool, ver string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online[nodeID] = online
	f.drained[nodeID] = drained
	f.versions[nodeID] = ver
}

func (f *fakeDisp) sentFor(opID string) *contracts.AgentUpgrade {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		if s.OperationID == opID {
			return s
		}
	}
	return nil
}

func (f *fakeDisp) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeDisp) abortedFor(opID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.aborted {
		if a == opID {
			return true
		}
	}
	return false
}

// env bundles a real store, the manager, and the fake dispatcher.
// cancel stops the Run loop early — restart tests kill the "process"
// before standing up a fresh Manager over the same DB.
type env struct {
	st      *store.DB
	m       *Manager
	disp    *fakeDisp
	ctx     context.Context
	cancel  context.CancelFunc
	notifMu sync.Mutex
	events  []string
}

func testLog() *slog.Logger {
	if os.Getenv("LM_TEST_LOGS") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fastCfg() Config {
	return Config{
		DrainTimeout:        300 * time.Millisecond,
		HealthVerifyTimeout: 900 * time.Millisecond,
		ReconnectTimeout:    500 * time.Millisecond,
		TickInterval:        15 * time.Millisecond,
		MaxBatchSize:        8,
		QueueMax:            64,
	}
}

// newEnv provisions a migrated test DB and a manager whose fencing epoch
// matches a bumped control_plane_lock (mirroring production ownership).
// run=false leaves the loop stopped for synchronous, race-free asserts.
func newEnv(t *testing.T, cfg Config, run bool) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Connect(ctx, testutil.PostgresDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := store.Migrate(ctx, st); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE control_plane_lock SET epoch=7 WHERE id=1`); err != nil {
		t.Fatalf("bump epoch: %v", err)
	}
	e := &env{st: st, disp: newFakeDisp(), ctx: ctx}
	e.m = New(st, e.disp, cfg, testLog())
	e.m.SetEpoch(7)
	e.m.SetNotifier(func(event string, payload any) {
		e.notifMu.Lock()
		e.events = append(e.events, event)
		e.notifMu.Unlock()
	})
	if run {
		rctx, cancel := context.WithCancel(ctx)
		e.cancel = cancel
		go e.m.Run(rctx)
		t.Cleanup(cancel)
	}
	return e
}

// stopLoop kills the manager's Run goroutine and lets any in-flight
// tick settle — simulating a control-plane crash. The store and the
// scripted dispatcher survive; a fresh Manager can then be stood up
// over them (see promote).
func (e *env) stopLoop() {
	e.cancel()
	time.Sleep(120 * time.Millisecond)
}

// promote stands up a NEW Manager over the same DB under a bumped
// fencing epoch — the standby-promotion path. The returned manager is
// running; its loop stops at test cleanup.
func (e *env) promote(t *testing.T, epoch int64) *Manager {
	t.Helper()
	if _, err := e.st.Pool.Exec(e.ctx,
		`UPDATE control_plane_lock SET epoch=$1 WHERE id=1`, epoch); err != nil {
		t.Fatalf("bump epoch: %v", err)
	}
	m2 := New(e.st, e.disp, fastCfg(), testLog())
	m2.SetEpoch(epoch)
	rctx, cancel := context.WithCancel(e.ctx)
	t.Cleanup(cancel)
	go m2.Run(rctx)
	return m2
}

// node creates an approved node row and scripts its dispatcher state.
func (e *env) node(t *testing.T, name, version string, online, drained bool) string {
	t.Helper()
	n, err := e.st.CreateNode(e.ctx, name, "hash-"+name, contracts.HardwareInfo{}, 2)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := e.st.SetNodeStatus(e.ctx, n.ID, contracts.NodeStatusApproved); err != nil {
		t.Fatalf("approve node: %v", err)
	}
	e.disp.set(n.ID, online, drained, version)
	return n.ID
}

// release registers a release row directly (bypassing signature
// validation — that path is covered by TestRegisterRelease*).
func (e *env) release(t *testing.T, version string) *store.Release {
	t.Helper()
	keyID := "TESTKEY-" + version
	if _, err := e.st.AddReleaseKey(e.ctx, keyID, "minisign", "pub", ""); err != nil {
		t.Fatalf("add key: %v", err)
	}
	r, err := e.st.RegisterRelease(e.ctx, &store.Release{
		Version: version, Channel: "stable",
		Manifest:       []byte(`{"version":"` + version + `"}`),
		ManifestSHA256: "m", SumsSHA256: "s",
		Signatures: map[string]string{}, SignerKeyID: keyID,
		ProtocolVersion: 1, MinAgentProtocol: 1,
		Platforms: []string{"agent/linux/amd64"},
	})
	if err != nil {
		t.Fatalf("register release: %v", err)
	}
	return r
}

func (e *env) getUpdate(t *testing.T, id string) *store.AgentUpdate {
	t.Helper()
	u, err := e.st.GetAgentUpdate(e.ctx, id)
	if err != nil {
		t.Fatalf("get update %s: %v", id, err)
	}
	return u
}

func (e *env) getRollout(t *testing.T, id string) *store.Rollout {
	t.Helper()
	r, err := e.st.GetRollout(e.ctx, id)
	if err != nil {
		t.Fatalf("get rollout %s: %v", id, err)
	}
	return r
}

func (e *env) maintenance(t *testing.T, nodeID string) (string, string) {
	t.Helper()
	n, err := e.st.GetNode(e.ctx, nodeID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	return n.Maintenance, n.MaintenanceReason
}

// waitMaintenanceCleared polls until the node leaves 'updating'.
// finishUpdate commits the terminal phase and releases the maintenance
// marker as separate store calls — a one-shot read taken the instant
// waitPhase sees the phase can land inside that gap under CPU load, so
// the real predicate ("marker released") is polled, not peeked once.
func (e *env) waitMaintenanceCleared(t *testing.T, nodeID string) {
	t.Helper()
	var st string
	waitFor(t, 8*time.Second, func() bool {
		st, _ = e.maintenance(t, nodeID)
		return st != store.MaintUpdating
	}, func() string {
		return "node " + nodeID + " updating marker released (still: " + st + ")"
	})
}

func waitFor(t *testing.T, d time.Duration, f func() bool, msg func() string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg())
}

func (e *env) waitPhase(t *testing.T, id string, want ...string) string {
	t.Helper()
	var got string
	waitFor(t, 8*time.Second, func() bool {
		u, err := e.st.GetAgentUpdate(e.ctx, id)
		if err != nil {
			return false
		}
		got = u.Phase
		for _, w := range want {
			if got == w {
				return true
			}
		}
		return false
	}, func() string {
		return "update " + id + " to reach " + want[0] + " (last: " + got + ")"
	})
	return got
}

func (e *env) waitRolloutState(t *testing.T, id string, want ...string) string {
	t.Helper()
	var got string
	waitFor(t, 8*time.Second, func() bool {
		r, err := e.st.GetRollout(e.ctx, id)
		if err != nil {
			return false
		}
		got = r.State
		for _, w := range want {
			if got == w {
				return true
			}
		}
		return false
	}, func() string {
		return "rollout " + id + " to reach " + want[0] + " (last: " + got + ")"
	})
	return got
}

// updatesFor returns the rollout's updates in creation order.
func (e *env) updatesFor(t *testing.T, rolloutID string) []*store.AgentUpdate {
	t.Helper()
	us, err := e.st.ListAgentUpdates(e.ctx, rolloutID, "", "", 50)
	if err != nil {
		t.Fatalf("list updates: %v", err)
	}
	// List is newest-first; flip to creation order.
	for i, j := 0, len(us)-1; i < j; i, j = i+1, j-1 {
		us[i], us[j] = us[j], us[i]
	}
	return us
}

// updateForNode finds a rollout's update row for one node.
func (e *env) updateForNode(t *testing.T, rolloutID, nodeID string) *store.AgentUpdate {
	t.Helper()
	for _, u := range e.updatesFor(t, rolloutID) {
		if u.NodeID == nodeID {
			return u
		}
	}
	t.Fatalf("no update for node %s in rollout %s", nodeID, rolloutID)
	return nil
}

// inFlight returns the rollout's started-but-live updates (past pending,
// not yet terminal) — created_at ties make "first created" ambiguous, so
// strategy tests key on in-flight counts rather than row position.
func (e *env) inFlight(t *testing.T, rolloutID string) []*store.AgentUpdate {
	t.Helper()
	var out []*store.AgentUpdate
	for _, u := range e.updatesFor(t, rolloutID) {
		if u.Phase != store.UpdPending && !store.UpdTerminal(u.Phase) {
			out = append(out, u)
		}
	}
	return out
}

// waitInFlight polls until n updates are started-but-live.
func (e *env) waitInFlight(t *testing.T, rolloutID string, n int) []*store.AgentUpdate {
	t.Helper()
	var out []*store.AgentUpdate
	waitFor(t, 8*time.Second, func() bool {
		out = e.inFlight(t, rolloutID)
		return len(out) >= n
	}, func() string {
		return fmt.Sprintf("%d in-flight updates in rollout %s", n, rolloutID)
	})
	return out
}

// driveDispatched creates a single-node rollout and waits for the
// upgrade frame. Requires the node online+drained.
func (e *env) driveDispatched(t *testing.T, rel *store.Release, nodeID string) (*store.Rollout, *store.AgentUpdate) {
	t.Helper()
	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{nodeID}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create rollout: %v", err)
	}
	u := e.updatesFor(t, r.ID)[0]
	e.waitPhase(t, u.ID, store.UpdDispatched)
	if e.disp.sentFor(u.ID) == nil {
		t.Fatal("no upgrade frame sent")
	}
	return r, u
}

// feed sends agent progress frames for the update.
func (e *env) feed(nodeID, opID string, phases ...string) {
	for _, p := range phases {
		e.m.OnUpgradeState(e.ctx, nodeID, contracts.UpgradeState{OperationID: opID, Phase: p})
	}
}

// reconnect simulates a conn drop + re-hello at the given version.
func (e *env) reconnect(nodeID, ver string) {
	e.disp.set(nodeID, false, false, ver)
	e.m.OnNodeDisconnected(nodeID)
	e.disp.set(nodeID, true, true, ver)
	e.m.OnNodeConnected(nodeID, ver)
}

func (e *env) sawEvent(name string) bool {
	e.notifMu.Lock()
	defer e.notifMu.Unlock()
	for _, ev := range e.events {
		if ev == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// update phase machine
// ---------------------------------------------------------------------------

// TestSingleLifecycle drives the whole phase machine: pending → draining
// → dispatched → agent frames → restarting → reconnect at the target
// version → verify_health → succeeded; rollout lands succeeded and the
// maintenance marker is released.
func TestSingleLifecycle(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	r, u := e.driveDispatched(t, rel, n)
	frame := e.disp.sentFor(u.ID)
	if frame.ReleaseID != rel.ID || frame.Version != "0.7.0" ||
		frame.Source != "localmesh://releases/"+rel.ID ||
		frame.KeyID != "TESTKEY-0.7.0" ||
		frame.ProtocolVersion != 1 || frame.MinAgentProtocol != 1 ||
		frame.AllowInsecureHTTP {
		t.Fatalf("frame = %+v", frame)
	}
	// While live, the node is fenced with our marker.
	st, reason := e.maintenance(t, n)
	if st != store.MaintUpdating || reason != maintReasonPrefix+u.ID {
		t.Fatalf("maintenance = %q %q, want updating/update:<id>", st, reason)
	}
	// Agent progress frames move the row.
	e.feed(n, u.ID, store.UpdVerifying, store.UpdDownloading, store.UpdStaging, store.UpdInstalling)
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdInstalling {
		t.Fatalf("phase = %s, want installing", got)
	}
	// Install completes → agent reports restarting → conn drops.
	e.feed(n, u.ID, store.UpdRestarting)
	// Reconnect at the OLD version mid-restart is tolerated (it may be a
	// pre-upgrade reconnect) — no promotion, no failure.
	e.disp.set(n, false, false, "0.6.0")
	e.m.OnNodeDisconnected(n)
	e.disp.set(n, true, true, "0.6.0")
	e.m.OnNodeConnected(n, "0.6.0")
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdRestarting {
		t.Fatalf("old-version reconnect changed phase to %s", got)
	}
	// Now the upgraded agent arrives.
	e.disp.set(n, false, false, "0.7.0")
	e.m.OnNodeDisconnected(n)
	e.disp.set(n, true, true, "0.7.0")
	e.m.OnNodeConnected(n, "0.7.0")
	e.waitPhase(t, u.ID, store.UpdSucceeded)
	if got := e.waitRolloutState(t, r.ID, store.RolloutSucceeded); got != store.RolloutSucceeded {
		t.Fatalf("rollout = %s", got)
	}
	e.waitMaintenanceCleared(t, n)
	st, _ = e.maintenance(t, n)
	if st != store.MaintNone {
		t.Fatalf("maintenance = %q after success, want cleared", st)
	}
	if !e.sawEvent("agent.update") || !e.sawEvent("rollout") {
		t.Fatal("expected agent.update and rollout events")
	}
}

// TestDrainTimeout: a node that never drains fails the update inside the
// drain budget and releases the maintenance marker.
func TestDrainTimeout(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, false) // online but never drained

	r, u := e.driveDispatchedWait(n, rel, t)
	_ = r
	got := e.waitPhase(t, u.ID, store.UpdFailed)
	if got != store.UpdFailed {
		t.Fatalf("phase = %s", got)
	}
	if !strings.Contains(e.getUpdate(t, u.ID).Error, "drain timeout") {
		t.Fatalf("error = %q", e.getUpdate(t, u.ID).Error)
	}
	e.waitMaintenanceCleared(t, n)
	st, _ := e.maintenance(t, n)
	if st != store.MaintNone {
		t.Fatalf("maintenance = %q after drain-timeout failure", st)
	}
	if e.disp.sentCount() != 0 {
		t.Fatal("upgrade frame sent to an undrained node")
	}
}

// driveDispatchedWait creates a rollout but does NOT wait for dispatch —
// used when dispatch is expected never to come.
func (e *env) driveDispatchedWait(nodeID string, rel *store.Release, t *testing.T) (*store.Rollout, *store.AgentUpdate) {
	t.Helper()
	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{nodeID}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create rollout: %v", err)
	}
	u := e.updatesFor(t, r.ID)[0]
	e.waitPhase(t, u.ID, store.UpdDraining)
	return r, u
}

// TestDrainThenDispatch: drain completing before the timeout dispatches.
func TestDrainThenDispatch(t *testing.T) {
	// Long drain budget: the test asserts the drain-completes path, not
	// the timeout — fastCfg's 300ms clock could legitimately fire first
	// when waitPhase polls late under load.
	cfg := fastCfg()
	cfg.DrainTimeout = time.Minute
	e := newEnv(t, cfg, true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, false)

	_, u := e.driveDispatchedWait(n, rel, t)
	time.Sleep(80 * time.Millisecond)
	e.disp.set(n, true, true, "0.6.0")
	e.waitPhase(t, u.ID, store.UpdDispatched)
}

// TestDisconnectPreInstall: dropping the conn in a pre-install phase
// interrupts the update (frame outcome unknowable).
func TestDisconnectPreInstall(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	r, u := e.driveDispatched(t, rel, n)
	e.disp.set(n, false, false, "")
	e.m.OnNodeDisconnected(n)
	if got := e.waitPhase(t, u.ID, store.UpdInterrupted); got != store.UpdInterrupted {
		t.Fatalf("phase = %s", got)
	}
	if got := e.waitRolloutState(t, r.ID, store.RolloutFailed); got != store.RolloutFailed {
		t.Fatalf("rollout = %s", got)
	}
	e.waitMaintenanceCleared(t, n)
	st, _ := e.maintenance(t, n)
	if st != store.MaintNone {
		t.Fatalf("maintenance = %q after interrupt", st)
	}
}

// TestReconnectWrongVersion: a node coming back at a version matching
// neither from_ nor to_ fails the update.
func TestReconnectWrongVersion(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	_, u := e.driveDispatched(t, rel, n)
	e.feed(n, u.ID, store.UpdInstalling, store.UpdRestarting)
	e.reconnect(n, "9.9.9-bogus")
	if got := e.waitPhase(t, u.ID, store.UpdFailed); got != store.UpdFailed {
		t.Fatalf("phase = %s", got)
	}
	if !strings.Contains(e.getUpdate(t, u.ID).Error, "unexpected agent version") {
		t.Fatalf("error = %q", e.getUpdate(t, u.ID).Error)
	}
}

// TestReconnectTimeout: an agent that never comes back fails once the
// reconnect budget expires.
func TestReconnectTimeout(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	_, u := e.driveDispatched(t, rel, n)
	e.feed(n, u.ID, store.UpdRestarting)
	e.disp.set(n, false, false, "")
	e.m.OnNodeDisconnected(n)
	got := e.waitPhase(t, u.ID, store.UpdFailed)
	if !strings.Contains(e.getUpdate(t, u.ID).Error, "did not reconnect") {
		t.Fatalf("error = %q", e.getUpdate(t, u.ID).Error)
	}
	_ = got
}

// TestCancelPending: a pending update cancels in place; no frame flies.
func TestCancelPending(t *testing.T) {
	e := newEnv(t, fastCfg(), false) // loop stopped → stays pending
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create rollout: %v", err)
	}
	u := e.updatesFor(t, r.ID)[0]
	if u.Phase != store.UpdPending {
		t.Fatalf("phase = %s, want pending", u.Phase)
	}
	if err := e.m.CancelUpdate(e.ctx, u.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdCancelled {
		t.Fatalf("phase = %s, want cancelled", got)
	}
	if e.disp.sentCount() != 0 {
		t.Fatal("cancelled update was dispatched")
	}
}

// TestCancelDrainingClearsMaintenance: cancel mid-drain releases the node.
func TestCancelDrainingClearsMaintenance(t *testing.T) {
	// Long drain budget: the test exercises cancel-from-draining, not
	// the drain timeout — a 300ms clock could race CancelUpdate itself
	// under load (a late waitPhase poll leaves <300ms of budget).
	cfg := fastCfg()
	cfg.DrainTimeout = time.Minute
	e := newEnv(t, cfg, true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, false)

	_, u := e.driveDispatchedWait(n, rel, t)
	// phase='draining' commits one statement before the maintenance
	// marker in claim() — wait for the marker rather than asserting in
	// the gap (the gap is real, just sub-ms under normal scheduling).
	deadline := time.Now().Add(10 * time.Second)
	var st string
	for {
		st, _ = e.maintenance(t, n)
		if st == store.MaintUpdating || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st != store.MaintUpdating {
		t.Fatalf("maintenance = %q during drain", st)
	}
	if err := e.m.CancelUpdate(e.ctx, u.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdCancelled {
		t.Fatalf("phase = %s", got)
	}
	st, _ = e.maintenance(t, n)
	if st != store.MaintNone {
		t.Fatalf("maintenance = %q after cancel", st)
	}
}

// TestCancelPreInstallSendsAbort: dispatched-phase cancel sends the
// abort frame then finishes cancelled; a late agent frame can't revive it.
func TestCancelPreInstallSendsAbort(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	_, u := e.driveDispatched(t, rel, n)
	e.feed(n, u.ID, store.UpdDownloading)
	if err := e.m.CancelUpdate(e.ctx, u.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !e.disp.abortedFor(u.ID) {
		t.Fatal("no abort frame sent")
	}
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdCancelled {
		t.Fatalf("phase = %s", got)
	}
	// Late frame is dropped.
	e.feed(n, u.ID, store.UpdInstalling)
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdCancelled {
		t.Fatalf("late frame mutated phase to %s", got)
	}
}

// TestCancelInstallingRefused: once the binary may be swapped, cancel
// refuses ErrNotCancellable.
func TestCancelInstallingRefused(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	_, u := e.driveDispatched(t, rel, n)
	e.feed(n, u.ID, store.UpdInstalling)
	if err := e.m.CancelUpdate(e.ctx, u.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("cancel installing = %v, want ErrNotCancellable", err)
	}
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdInstalling {
		t.Fatalf("phase = %s, want still installing", got)
	}
	if err := e.m.CancelUpdate(e.ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel unknown id = %v, want ErrNotFound", err)
	}
}

// TestRolledBack: agent rollback lands the update rolled_back; a
// halt-family rollout fails on it.
func TestRolledBack(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	r, u := e.driveDispatched(t, rel, n)
	e.feed(n, u.ID, store.UpdInstalling)
	e.m.OnUpgradeState(e.ctx, n, contracts.UpgradeState{
		OperationID: u.ID, Phase: "rolled_back", Detail: "health check failed"})
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdRolledBack {
		t.Fatalf("phase = %s, want rolled_back", got)
	}
	if got := e.waitRolloutState(t, r.ID, store.RolloutFailed); got != store.RolloutFailed {
		t.Fatalf("rollout = %s", got)
	}
	st, _ := e.maintenance(t, n)
	if st != store.MaintNone {
		t.Fatalf("maintenance = %q after rollback", st)
	}
}

// TestAlreadyOnVersion: a node already reporting the release version
// finishes succeeded immediately with the skip detail.
func TestAlreadyOnVersion(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.7.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	u := e.updatesFor(t, r.ID)[0]
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdSucceeded {
		t.Fatalf("phase = %s, want immediate succeeded", got)
	}
	if e.getUpdate(t, u.ID).Detail != "already running target version" {
		t.Fatalf("detail = %q", e.getUpdate(t, u.ID).Detail)
	}
	e.waitRolloutState(t, r.ID, store.RolloutSucceeded)
	if e.disp.sentCount() != 0 {
		t.Fatal("frame sent to an already-current node")
	}
}

// TestReconcileBoot: pre-install rows on offline nodes are interrupted;
// restarting rows persist for the agent to come back.
func TestReconcileBoot(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	rel := e.release(t, "0.7.0")
	online := e.node(t, "n-online", "0.6.0", true, true)
	offline := e.node(t, "n-offline", "0.6.0", false, false)
	restarting := e.node(t, "n-restart", "0.6.0", false, false)

	// Seed rows as a crashed process left them.
	r1, _ := e.m.CreateRollout(e.ctx, rel.ID, StrategySelected, []string{online, offline}, 0, 0, "test")
	us := e.updatesFor(t, r1.ID)
	for _, u := range us {
		if _, err := e.st.Pool.Exec(e.ctx,
			`UPDATE agent_updates SET phase='dispatched', updated_at=now() WHERE id=$1`, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	rst, err := e.st.EnqueueAgentUpdate(e.ctx, "", rel.ID, restarting, "0.6.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Pool.Exec(e.ctx,
		`UPDATE agent_updates SET phase='restarting', updated_at=now() WHERE id=$1`, rst.ID); err != nil {
		t.Fatal(err)
	}

	if err := e.m.Reconcile(e.ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Offline dispatched → interrupted; online dispatched survives.
	if got := e.getUpdate(t, e.updateForNode(t, r1.ID, online).ID).Phase; got != store.UpdDispatched {
		t.Fatalf("online row = %s, want still dispatched", got)
	}
	if got := e.getUpdate(t, e.updateForNode(t, r1.ID, offline).ID).Phase; got != store.UpdInterrupted {
		t.Fatalf("offline row = %s, want interrupted", got)
	}
	if got := e.getUpdate(t, rst.ID).Phase; got != store.UpdRestarting {
		t.Fatalf("restarting row = %s, want untouched", got)
	}
}

// TestEpochFence: claims only succeed under the live fencing epoch.
func TestEpochFence(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)

	u, err := e.st.EnqueueAgentUpdate(e.ctx, "", rel.ID, n, "0.6.0")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.st.ClaimPendingUpdate(e.ctx, u.ID, 999); err != nil || ok {
		t.Fatalf("stale-epoch claim ok=%v err=%v, want false", ok, err)
	}
	if ok, err := e.st.ClaimPendingUpdate(e.ctx, u.ID, 7); err != nil || !ok {
		t.Fatalf("live-epoch claim ok=%v err=%v, want true", ok, err)
	}
	if got := e.getUpdate(t, u.ID).Phase; got != store.UpdDraining {
		t.Fatalf("phase = %s, want draining", got)
	}
}

// ---------------------------------------------------------------------------
// rollout strategies
// ---------------------------------------------------------------------------

// TestCanaryGate: first canary_count nodes update; the rollout parks in
// awaiting_approval; ApproveNextBatch releases the rest at batch_size.
func TestCanaryGate(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)
	n3 := e.node(t, "n3", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyCanary, []string{n1, n2, n3}, 1, 2, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	inFlight := e.waitInFlight(t, r.ID, 1)
	// The canary runs alone; followers stay pending.
	time.Sleep(150 * time.Millisecond)
	for _, u := range e.updatesFor(t, r.ID) {
		if u.ID == inFlight[0].ID {
			continue
		}
		if u.Phase != store.UpdPending {
			t.Fatalf("non-canary update phase = %s, want pending", u.Phase)
		}
	}
	// Finish the canary → awaiting_approval.
	e.finishOK(t, inFlight[0].NodeID, inFlight[0].ID, "0.7.0")
	e.waitRolloutState(t, r.ID, store.RolloutAwaitingApproval)
	// Approving releases the remaining nodes (batch_size 2 → both) and
	// stamps the durable approval (approved_by/approved_at).
	r2, err := e.m.ApproveNextBatch(e.ctx, r.ID, "admin-1")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if r2.State != store.RolloutRunning {
		t.Fatalf("post-approve state = %s", r2.State)
	}
	if r2.ApprovedBy != "admin-1" || r2.ApprovedAt == nil {
		t.Fatalf("approval not stamped: by=%q at=%v", r2.ApprovedBy, r2.ApprovedAt)
	}
	if got := e.getRollout(t, r.ID); got.ApprovedBy != "admin-1" || got.ApprovedAt == nil {
		t.Fatalf("persisted approval by=%q at=%v", got.ApprovedBy, got.ApprovedAt)
	}
	// A second approve is a clean no-op — the rollout is running now.
	if _, err := e.m.ApproveNextBatch(e.ctx, r.ID, "admin-2"); !errors.Is(err, ErrNotAwaitingApproval) {
		t.Fatalf("double approve = %v, want ErrNotAwaitingApproval", err)
	}
	rest := e.waitInFlight(t, r.ID, 2)
	for _, u := range rest {
		e.finishOK(t, u.NodeID, u.ID, "0.7.0")
	}
	e.waitRolloutState(t, r.ID, store.RolloutSucceeded)
}

// finishOK walks an update through agent frames to a verified success.
// It waits for 'dispatched' first: 'draining' also counts as in-flight,
// and feeding install frames then would skip the upgrade frame entirely
// (the store accepts install phases from any live row) — the test would
// pass without ever exercising dispatch.
func (e *env) finishOK(t *testing.T, nodeID, updateID, toVersion string) {
	t.Helper()
	e.waitPhase(t, updateID, store.UpdDispatched)
	e.feed(nodeID, updateID, store.UpdInstalling, store.UpdRestarting)
	e.disp.set(nodeID, false, false, toVersion)
	e.m.OnNodeDisconnected(nodeID)
	e.disp.set(nodeID, true, true, toVersion)
	e.m.OnNodeConnected(nodeID, toVersion)
	e.waitPhase(t, updateID, store.UpdSucceeded)
}

// TestSequentialAdvance: batch=1 — the second node starts only after the
// first reaches a terminal phase.
func TestSequentialAdvance(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySequential, []string{n1, n2}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first := e.waitInFlight(t, r.ID, 1)[0]
	time.Sleep(150 * time.Millisecond)
	if got := len(e.inFlight(t, r.ID)); got != 1 {
		t.Fatalf("sequential ran ahead: %d in flight, want 1", got)
	}
	follower := e.updateForNode(t, r.ID, n2)
	if first.NodeID == n2 {
		follower = e.updateForNode(t, r.ID, n1)
	}
	if got := e.getUpdate(t, follower.ID).Phase; got != store.UpdPending {
		t.Fatalf("follower update = %s while first in flight", got)
	}
	e.finishOK(t, first.NodeID, first.ID, "0.7.0")
	e.waitPhase(t, follower.ID, store.UpdDispatched)
	e.finishOK(t, follower.NodeID, follower.ID, "0.7.0")
	e.waitRolloutState(t, r.ID, store.RolloutSucceeded)
}

// TestSequentialHaltsOnFailure: sequential stops advancing after a
// failure and cancels the remaining updates.
func TestSequentialHaltsOnFailure(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySequential, []string{n1, n2}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	first := e.waitInFlight(t, r.ID, 1)[0]
	e.m.OnUpgradeState(e.ctx, first.NodeID, contracts.UpgradeState{
		OperationID: first.ID, Phase: "failed", Error: "signature mismatch"})
	e.waitPhase(t, first.ID, store.UpdFailed)
	e.waitRolloutState(t, r.ID, store.RolloutFailed)
	for _, u := range e.updatesFor(t, r.ID) {
		if u.ID == first.ID {
			continue
		}
		if u.Phase != store.UpdCancelled {
			t.Fatalf("remaining update = %s, want cancelled on halt", u.Phase)
		}
	}
}

// TestParallelBounded: batch_size bounds in-flight updates; failures
// don't halt — the rollout reports failed only at the end.
func TestParallelBounded(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)
	n3 := e.node(t, "n3", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyParallel, []string{n1, n2, n3}, 0, 2, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pair := e.waitInFlight(t, r.ID, 2)
	time.Sleep(150 * time.Millisecond)
	if got := len(e.inFlight(t, r.ID)); got != 2 {
		t.Fatalf("parallel overran batch: %d in flight, want 2", got)
	}
	// One fails; the third still advances (no halt for parallel).
	e.m.OnUpgradeState(e.ctx, pair[0].NodeID, contracts.UpgradeState{
		OperationID: pair[0].ID, Phase: "failed", Error: "download 404"})
	e.waitPhase(t, pair[0].ID, store.UpdFailed)
	third := e.waitInFlight(t, r.ID, 2) // survivor + replacement
	e.finishOK(t, pair[1].NodeID, pair[1].ID, "0.7.0")
	for _, u := range third {
		if u.ID != pair[1].ID {
			e.finishOK(t, u.NodeID, u.ID, "0.7.0")
		}
	}
	// All terminal with one failure → rollout failed, not halted early.
	if got := e.waitRolloutState(t, r.ID, store.RolloutFailed); got != store.RolloutFailed {
		t.Fatalf("rollout = %s", got)
	}
}

// TestCancelRollout: live updates are cancelled and the rollout lands
// cancelled; terminal rollouts refuse.
func TestCancelRollout(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyParallel, []string{n1, n2}, 0, 2, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	us := e.updatesFor(t, r.ID)
	e.waitPhase(t, us[0].ID, store.UpdDispatched)
	e.waitPhase(t, us[1].ID, store.UpdDispatched)
	if err := e.m.CancelRollout(e.ctx, r.ID); err != nil {
		t.Fatalf("cancel rollout: %v", err)
	}
	for _, u := range us {
		if got := e.getUpdate(t, u.ID).Phase; got != store.UpdCancelled {
			t.Fatalf("update phase = %s, want cancelled", got)
		}
	}
	if got := e.getRollout(t, r.ID).State; got != store.RolloutCancelled {
		t.Fatalf("rollout = %s", got)
	}
	if err := e.m.CancelRollout(e.ctx, r.ID); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("second cancel = %v, want ErrNotCancellable", err)
	}
}

// TestApproveWrongState: approving a rollout that isn't parked refuses.
func TestApproveWrongState(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", false, false)
	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySequential, []string{n}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := e.m.ApproveNextBatch(e.ctx, r.ID, "admin"); !errors.Is(err, ErrNotAwaitingApproval) {
		t.Fatalf("approve = %v, want ErrNotAwaitingApproval", err)
	}
}

// ---------------------------------------------------------------------------
// durable canary approval (restart/promotion safety)
// ---------------------------------------------------------------------------

// TestCanaryApprovalSurvivesRestart is the durability contract: a
// canary approved just before a control-plane crash resumes under a
// fresh Manager (restart/standby promotion) WITHOUT a second approval
// and WITHOUT re-parking at awaiting_approval. The old in-memory
// canaryOK bit lost this exact window — approved_at IS NOT NULL is now
// the gate.
func TestCanaryApprovalSurvivesRestart(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)
	n3 := e.node(t, "n3", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyCanary, []string{n1, n2, n3}, 1, 2, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	canary := e.waitInFlight(t, r.ID, 1)[0]
	e.finishOK(t, canary.NodeID, canary.ID, "0.7.0")
	e.waitRolloutState(t, r.ID, store.RolloutAwaitingApproval)

	// Crash the control plane, THEN land the approval: the commit
	// flips state to 'running' and stamps approved_*, but no advance
	// pass runs before the process is gone — the exact window that
	// used to re-park.
	e.stopLoop()
	if _, err := e.m.ApproveNextBatch(e.ctx, r.ID, "admin-1"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := e.getRollout(t, r.ID); got.State != store.RolloutRunning ||
		got.ApprovedBy != "admin-1" || got.ApprovedAt == nil {
		t.Fatalf("post-approve row = %+v", got)
	}
	for _, u := range e.updatesFor(t, r.ID) {
		if u.ID != canary.ID && u.Phase != store.UpdPending {
			t.Fatalf("follower %s already %s before restart", u.ID, u.Phase)
		}
	}

	// Standby promotes: new Manager, same DB, bumped epoch.
	e.promote(t, 8)

	// No second approval: the followers must be claimed and the rollout
	// must never return to awaiting_approval.
	rest := e.waitInFlight(t, r.ID, 2)
	if got := e.getRollout(t, r.ID); got.State == store.RolloutAwaitingApproval {
		t.Fatal("approved canary re-parked after restart")
	}
	if got := e.getRollout(t, r.ID); got.ApprovedBy != "admin-1" || got.ApprovedAt == nil {
		t.Fatalf("approval lost across restart: %+v", got)
	}
	for _, u := range rest {
		e.finishOK(t, u.NodeID, u.ID, "0.7.0")
	}
	e.waitRolloutState(t, r.ID, store.RolloutSucceeded)
}

// TestCanaryUnapprovedRestartStaysParked: a canary parked at the gate
// without approval stays parked through a restart — no claims, no
// implicit release. approved_at IS NULL keeps the gate live.
func TestCanaryUnapprovedRestartStaysParked(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)
	n3 := e.node(t, "n3", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyCanary, []string{n1, n2, n3}, 1, 2, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	canary := e.waitInFlight(t, r.ID, 1)[0]
	// 'draining' counts as in-flight — wait for the actual dispatch so
	// finishOK's frames can't skip the sent frame under -race timing.
	e.waitPhase(t, canary.ID, store.UpdDispatched)
	e.finishOK(t, canary.NodeID, canary.ID, "0.7.0")
	e.waitRolloutState(t, r.ID, store.RolloutAwaitingApproval)

	e.stopLoop()
	e.promote(t, 8)

	// Give the promoted manager many ticks: nothing may advance.
	time.Sleep(300 * time.Millisecond)
	if got := e.getRollout(t, r.ID).State; got != store.RolloutAwaitingApproval {
		t.Fatalf("unapproved canary after restart = %s, want awaiting_approval", got)
	}
	for _, u := range e.updatesFor(t, r.ID) {
		if u.ID != canary.ID && u.Phase != store.UpdPending {
			t.Fatalf("post-canary update %s = %s under unapproved canary", u.ID, u.Phase)
		}
	}
	if e.disp.sentCount() != 1 {
		t.Fatalf("sends = %d, want exactly the canary frame", e.disp.sentCount())
	}
}

// ---------------------------------------------------------------------------
// pause / resume
// ---------------------------------------------------------------------------

// TestPauseResumeRollout: pausing gates NEW dispatch — pendings wait
// unclaimed — while an update already on a node keeps reporting and
// finishing under the pause. Resume restarts dispatch.
func TestPauseResumeRollout(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", false, false) // offline: stays pending

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySelected, []string{n1, n2}, 0, 2, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	u1 := e.waitInFlight(t, r.ID, 1)[0]
	e.waitPhase(t, u1.ID, store.UpdDispatched)

	p, err := e.m.PauseRollout(e.ctx, r.ID)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if p.State != RolloutPaused {
		t.Fatalf("paused state = %s", p.State)
	}
	// Already-paused refuses again.
	if _, err := e.m.PauseRollout(e.ctx, r.ID); !errors.Is(err, ErrNotPausable) {
		t.Fatalf("second pause = %v, want ErrNotPausable", err)
	}
	// In-flight work is NOT aborted: the dispatched update finishes
	// under the pause...
	e.finishOK(t, u1.NodeID, u1.ID, "0.7.0")
	// ...but the still-pending sibling is never claimed, even with the
	// node now online.
	e.disp.set(n2, true, true, "0.6.0")
	time.Sleep(250 * time.Millisecond) // many ticks
	if got := e.getUpdate(t, e.updateForNode(t, r.ID, n2).ID).Phase; got != store.UpdPending {
		t.Fatalf("pending update advanced to %s under pause", got)
	}
	if got := e.getRollout(t, r.ID).State; got != RolloutPaused {
		t.Fatalf("rollout state = %s under pause", got)
	}
	if e.disp.sentCount() != 1 {
		t.Fatalf("sends = %d while paused, want 1", e.disp.sentCount())
	}

	// Resume → the sibling dispatches and the rollout lands.
	res, err := e.m.ResumeRollout(e.ctx, r.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.State != store.RolloutRunning {
		t.Fatalf("resumed state = %s", res.State)
	}
	u2 := e.updateForNode(t, r.ID, n2)
	e.waitPhase(t, u2.ID, store.UpdDispatched)
	e.finishOK(t, u2.NodeID, u2.ID, "0.7.0")
	e.waitRolloutState(t, r.ID, store.RolloutSucceeded)
	// Terminal now: resume refuses.
	if _, err := e.m.ResumeRollout(e.ctx, r.ID); !errors.Is(err, ErrNotPaused) {
		t.Fatalf("resume terminal = %v, want ErrNotPaused", err)
	}
}

// TestApprovePausedRollout: approving a canary that was paused at the
// gate records the approval but keeps the rollout paused ("approval
// persists, paused stays paused"); resuming then runs it straight
// through — approved_at IS NOT NULL, the gate never re-engages.
func TestApprovePausedRollout(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)
	n3 := e.node(t, "n3", "0.6.0", true, true)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyCanary, []string{n1, n2, n3}, 1, 2, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	canary := e.waitInFlight(t, r.ID, 1)[0]
	e.finishOK(t, canary.NodeID, canary.ID, "0.7.0")
	e.waitRolloutState(t, r.ID, store.RolloutAwaitingApproval)

	if _, err := e.m.PauseRollout(e.ctx, r.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Kill the loop so the approve's internal running-window can't be
	// ticked on — the assert below stays deterministic.
	e.stopLoop()

	ap, err := e.m.ApproveNextBatch(e.ctx, r.ID, "admin-1")
	if err != nil {
		t.Fatalf("approve paused rollout: %v", err)
	}
	if ap.State != RolloutPaused {
		t.Fatalf("approve unpaused the rollout: %s", ap.State)
	}
	if ap.ApprovedBy != "admin-1" || ap.ApprovedAt == nil {
		t.Fatalf("approval not stamped while paused: %+v", ap)
	}
	for _, u := range e.updatesFor(t, r.ID) {
		if u.ID != canary.ID && u.Phase != store.UpdPending {
			t.Fatalf("follower %s advanced to %s while paused", u.ID, u.Phase)
		}
	}
	// Approved-but-paused is not at the gate: a second approve refuses.
	if _, err := e.m.ApproveNextBatch(e.ctx, r.ID, "admin-2"); !errors.Is(err, ErrNotAwaitingApproval) {
		t.Fatalf("second approve = %v, want ErrNotAwaitingApproval", err)
	}

	// Restart: paused + approved survives promotion untouched.
	m2 := e.promote(t, 8)
	time.Sleep(200 * time.Millisecond)
	if got := e.getRollout(t, r.ID).State; got != RolloutPaused {
		t.Fatalf("paused rollout after restart = %s", got)
	}

	// Resume under the new manager → straight to running, followers
	// claim, no re-park at the gate.
	if _, err := m2.ResumeRollout(e.ctx, r.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	rest := e.waitInFlight(t, r.ID, 2)
	for _, u := range rest {
		e.finishOK(t, u.NodeID, u.ID, "0.7.0")
	}
	e.waitRolloutState(t, r.ID, store.RolloutSucceeded)
}

// TestPauseResumeGuards: pending, paused, and terminal rollouts refuse
// pause; anything not paused refuses resume; unknown ids 404.
func TestPauseResumeGuards(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", false, false)

	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySequential, []string{n}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Still 'pending' (loop stopped): not yet evaluated → not pausable.
	if _, err := e.m.PauseRollout(e.ctx, r.ID); !errors.Is(err, ErrNotPausable) {
		t.Fatalf("pause pending = %v, want ErrNotPausable", err)
	}
	if _, err := e.m.ResumeRollout(e.ctx, r.ID); !errors.Is(err, ErrNotPaused) {
		t.Fatalf("resume pending = %v, want ErrNotPaused", err)
	}
	// Terminal: refuses both.
	if err := e.m.CancelRollout(e.ctx, r.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := e.m.PauseRollout(e.ctx, r.ID); !errors.Is(err, ErrNotPausable) {
		t.Fatalf("pause cancelled = %v, want ErrNotPausable", err)
	}
	if _, err := e.m.ResumeRollout(e.ctx, r.ID); !errors.Is(err, ErrNotPaused) {
		t.Fatalf("resume cancelled = %v, want ErrNotPaused", err)
	}
	// Unknown id → ErrNotFound.
	missing := "00000000-0000-0000-0000-000000000000"
	if _, err := e.m.PauseRollout(e.ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pause missing = %v, want ErrNotFound", err)
	}
	if _, err := e.m.ResumeRollout(e.ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resume missing = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// rollout validation
// ---------------------------------------------------------------------------

func TestCreateRolloutValidation(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", true, true)
	n2 := e.node(t, "n2", "0.6.0", true, true)

	// Bad strategy / empty targets / single arity.
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, "bogus", []string{n1}, 0, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("bad strategy = %v", err)
	}
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySelected, nil, 0, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("empty nodes = %v", err)
	}
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n1, n2}, 0, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("single with 2 nodes = %v", err)
	}
	// Batch bounds.
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyCanary, []string{n1, n2}, 99, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("canary_count over cap = %v", err)
	}
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyCanary, []string{n1, n2}, 2, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("canary covering all = %v", err)
	}
	// Unknown node → ErrNotFound; unapproved node → ErrInvalidRollout.
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySelected,
		[]string{"00000000-0000-0000-0000-000000000000"}, 0, 0, "test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown node = %v", err)
	}
	pend, err := e.st.CreateNode(e.ctx, "pending-node", "hash-pending", contracts.HardwareInfo{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySelected, []string{pend.ID}, 0, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("unapproved node = %v", err)
	}
	// Missing/revoked release.
	if _, err := e.m.CreateRollout(e.ctx, "00000000-0000-0000-0000-000000000000",
		StrategySingle, []string{n1}, 0, 0, "test"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing release = %v", err)
	}
	if err := e.st.RevokeRelease(e.ctx, rel.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n1}, 0, 0, "test"); !errors.Is(err, ErrReleaseNotApproved) {
		t.Fatalf("revoked release = %v", err)
	}
}

// TestActiveUpdateDedupe: a second rollout targeting a node with a live
// update is refused with store.ErrActiveUpdate.
func TestActiveUpdateDedupe(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", false, false)

	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n}, 0, 0, "test"); err != nil {
		t.Fatalf("first rollout: %v", err)
	}
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n}, 0, 0, "test"); !errors.Is(err, store.ErrActiveUpdate) {
		t.Fatalf("second rollout = %v, want ErrActiveUpdate", err)
	}
}

// TestQueueFullBound: QueueMax bounds total live updates.
func TestQueueFullBound(t *testing.T) {
	cfg := fastCfg()
	cfg.QueueMax = 2
	e := newEnv(t, cfg, false)
	rel := e.release(t, "0.7.0")
	n1 := e.node(t, "n1", "0.6.0", false, false)
	n2 := e.node(t, "n2", "0.6.0", false, false)
	n3 := e.node(t, "n3", "0.6.0", false, false)

	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySelected, []string{n1, n2}, 0, 0, "test"); err != nil {
		t.Fatalf("first rollout: %v", err)
	}
	if _, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n3}, 0, 0, "test"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("over-limit rollout = %v, want ErrQueueFull", err)
	}
}

// ---------------------------------------------------------------------------
// release registry
// ---------------------------------------------------------------------------

// signedFixture builds a real manifest+sums+signatures with a fresh
// minisign key.
type signedFixture struct {
	priv       ed25519.PrivateKey
	pub        *release.MinisignPublicKey
	pubBytes   []byte
	manifest   []byte
	sums       []byte
	signatures map[string]string
}

func makeFixture(t *testing.T) *signedFixture {
	t.Helper()
	priv, pub, pubBytes, err := release.GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("fake-agent-binary-0.7.0")
	sum := release.SumBytes(content)
	man := &release.Manifest{
		Version:            "0.7.0",
		ProtocolVersion:    1,
		MinProtocolVersion: 1,
		CreatedAt:          time.Now().UTC(),
		Signer:             release.Signer{Algorithm: "minisign", KeyID: pub.KeyIDHex},
		Artifacts: []release.Artifact{
			{File: "localmesh-agent-0.7.0-linux-amd64", OS: "linux", Arch: "amd64", SHA256: sum, Size: int64(len(content))},
			{File: "localmesh-server-0.7.0-linux-amd64", OS: "linux", Arch: "amd64", SHA256: release.SumBytes([]byte("srv")), Size: 3},
		},
	}
	manBytes, err := man.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	sums := release.Sums{
		"localmesh-agent-0.7.0-linux-amd64":  sum,
		"localmesh-server-0.7.0-linux-amd64": release.SumBytes([]byte("srv")),
	}
	sumsBytes := []byte(sums.Format())
	return &signedFixture{
		priv: priv, pub: pub, pubBytes: pubBytes,
		manifest: manBytes,
		sums:     sumsBytes,
		signatures: map[string]string{
			release.SumsFile + release.MinisignSigExt:     string(release.SignMinisign(priv, pub.KeyNum, sumsBytes, "test sums")),
			release.ManifestFile + release.MinisignSigExt: string(release.SignMinisign(priv, pub.KeyNum, manBytes, "test manifest")),
		},
	}
}

func (f *signedFixture) input() RegisterInput {
	return RegisterInput{
		ManifestJSON: f.manifest,
		SumsJSON:     f.sums,
		Signatures:   f.signatures,
		BaseURL:      "https://releases.example.com/0.7.0",
		Channel:      "stable",
		CreatedBy:    "test",
	}
}

// TestRegisterReleaseGood: a fully-signed release lands with derived
// platforms, hashes, signer key id, and protocol bounds.
func TestRegisterReleaseGood(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t)
	k, err := e.m.RegisterKey(e.ctx, fx.pubBytes, "ci key")
	if err != nil {
		t.Fatalf("register key: %v", err)
	}
	if k.KeyID != fx.pub.KeyIDHex || k.Kind != "minisign" {
		t.Fatalf("key row = %+v", k)
	}
	r, err := e.m.RegisterRelease(e.ctx, fx.input())
	if err != nil {
		t.Fatalf("register release: %v", err)
	}
	if r.Version != "0.7.0" || r.SignerKeyID != fx.pub.KeyIDHex ||
		r.ProtocolVersion != 1 || r.MinAgentProtocol != 1 || r.Status != "approved" {
		t.Fatalf("release = %+v", r)
	}
	want := map[string]bool{"agent/linux/amd64": true, "server/linux/amd64": true}
	if len(r.Platforms) != 2 {
		t.Fatalf("platforms = %v", r.Platforms)
	}
	for _, p := range r.Platforms {
		if !want[p] {
			t.Fatalf("unexpected platform %q", p)
		}
	}
	if !e.sawEvent("release") {
		t.Fatal("no release event")
	}
}

// TestRegisterReleaseUnsigned: signer "unsigned" is refused outright.
func TestRegisterReleaseUnsigned(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t)
	man := &release.Manifest{
		Version: "0.7.0", ProtocolVersion: 1, MinProtocolVersion: 1,
		CreatedAt: time.Now().UTC(),
		Signer:    release.Signer{Algorithm: "unsigned"},
		Artifacts: []release.Artifact{
			{File: "localmesh-agent-0.7.0-linux-amd64", OS: "linux", Arch: "amd64",
				SHA256: strings.Repeat("a", 64), Size: 3},
		},
	}
	mb, _ := man.Marshal()
	in := fx.input()
	in.ManifestJSON = mb
	_, err := e.m.RegisterRelease(e.ctx, in)
	if !errors.Is(err, ErrUnsignedRelease) {
		t.Fatalf("unsigned manifest = %v, want ErrUnsignedRelease", err)
	}
}

// TestRegisterReleaseUnknownKey: a manifest signed by an unregistered
// key_id is refused naming the key.
func TestRegisterReleaseUnknownKey(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t) // never registered
	_, err := e.m.RegisterRelease(e.ctx, fx.input())
	if err == nil || !strings.Contains(err.Error(), fx.pub.KeyIDHex) {
		t.Fatalf("unknown key err = %v, want one naming %s", err, fx.pub.KeyIDHex)
	}
}

// TestRegisterReleaseRevokedKey: a revoked key can't sign new releases.
func TestRegisterReleaseRevokedKey(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t)
	if _, err := e.m.RegisterKey(e.ctx, fx.pubBytes, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.m.RevokeReleaseKey(e.ctx, fx.pub.KeyIDHex); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.RegisterRelease(e.ctx, fx.input()); err == nil {
		t.Fatal("revoked key registered a release")
	}
}

// TestRegisterReleaseTamperedSums: sums that disagree with the manifest
// are refused before a row exists.
func TestRegisterReleaseTamperedSums(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t)
	if _, err := e.m.RegisterKey(e.ctx, fx.pubBytes, ""); err != nil {
		t.Fatal(err)
	}
	bad := release.Sums{
		"localmesh-agent-0.7.0-linux-amd64":  strings.Repeat("0", 64),
		"localmesh-server-0.7.0-linux-amd64": release.SumBytes([]byte("srv")),
	}
	badBytes := []byte(bad.Format())
	in := fx.input()
	in.SumsJSON = badBytes
	// Re-sign the tampered sums so it isn't the signature that fails.
	in.Signatures[release.SumsFile+release.MinisignSigExt] =
		string(release.SignMinisign(fx.priv, fx.pub.KeyNum, badBytes, "t"))
	_, err := e.m.RegisterRelease(e.ctx, in)
	if err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("tampered sums err = %v", err)
	}
}

// TestRegisterReleaseBadSignature: a signature made by another key fails.
func TestRegisterReleaseBadSignature(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t)
	if _, err := e.m.RegisterKey(e.ctx, fx.pubBytes, ""); err != nil {
		t.Fatal(err)
	}
	// Sign with a different key under the same keynum bytes — the sig
	// verifies structure but not against the stored key.
	priv2, _, _, err := release.GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	in := fx.input()
	in.Signatures[release.SumsFile+release.MinisignSigExt] =
		string(release.SignMinisign(priv2, fx.pub.KeyNum, fx.sums, "t"))
	if _, err := e.m.RegisterRelease(e.ctx, in); err == nil {
		t.Fatal("bad signature accepted")
	}
}

// TestRegisterReleaseBaseURL: http is refused except on loopback.
func TestRegisterReleaseBaseURL(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t)
	if _, err := e.m.RegisterKey(e.ctx, fx.pubBytes, ""); err != nil {
		t.Fatal(err)
	}
	in := fx.input()
	in.BaseURL = "http://releases.example.com"
	if _, err := e.m.RegisterRelease(e.ctx, in); err == nil {
		t.Fatal("plain http base_url accepted")
	}
	in.BaseURL = "http://127.0.0.1:9000/rel"
	if _, err := e.m.RegisterRelease(e.ctx, in); err != nil {
		t.Fatalf("loopback http refused: %v", err)
	}
	// Duplicate (version, channel) is a conflict.
	in2 := fx.input()
	in2.BaseURL = "http://localhost:9000/rel"
	if _, err := e.m.RegisterRelease(e.ctx, in2); !errors.Is(err, store.ErrDuplicateRelease) {
		t.Fatalf("dup release = %v, want ErrDuplicateRelease", err)
	}
}

// TestRegisterKeyDuplicate: the same key twice is a conflict.
func TestRegisterKeyDuplicate(t *testing.T) {
	e := newEnv(t, fastCfg(), false)
	fx := makeFixture(t)
	if _, err := e.m.RegisterKey(e.ctx, fx.pubBytes, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.RegisterKey(e.ctx, fx.pubBytes, ""); !errors.Is(err, store.ErrDuplicateKey) {
		t.Fatalf("dup key = %v, want ErrDuplicateKey", err)
	}
	if _, err := e.m.RegisterKey(e.ctx, []byte("not a key"), ""); err == nil {
		t.Fatal("garbage key accepted")
	}
}

// ---------------------------------------------------------------------------
// node maintenance guards
// ---------------------------------------------------------------------------

// TestMaintenanceGuard: admin maintenance writes are refused while a
// live update owns the marker; resume works after the update ends.
func TestMaintenanceGuard(t *testing.T) {
	// Long drain budget: the update must stay live across the guard
	// asserts — fastCfg's 300ms DrainTimeout can fire between waitPhase
	// and SetNodeMaintenance under -race, turning ErrActiveUpdate nil.
	cfg := fastCfg()
	cfg.DrainTimeout = time.Minute
	e := newEnv(t, cfg, true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, false)

	_, u := e.driveDispatchedWait(n, rel, t)
	if err := e.m.SetNodeMaintenance(e.ctx, n, store.MaintMode, "admin"); !errors.Is(err, store.ErrActiveUpdate) {
		t.Fatalf("admin set during update = %v, want ErrActiveUpdate", err)
	}
	if err := e.m.ResumeNode(e.ctx, n); !errors.Is(err, store.ErrActiveUpdate) {
		t.Fatalf("resume during update = %v, want ErrActiveUpdate", err)
	}
	if err := e.m.CancelUpdate(e.ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.m.ResumeNode(e.ctx, n); err != nil {
		t.Fatalf("resume after update ended: %v", err)
	}
	if err := e.m.SetNodeMaintenance(e.ctx, n, store.MaintDraining, "admin drain"); err != nil {
		t.Fatalf("admin drain: %v", err)
	}
	st, reason := e.maintenance(t, n)
	if st != store.MaintDraining || reason != "admin drain" {
		t.Fatalf("maintenance = %q %q", st, reason)
	}
	if err := e.m.SetNodeMaintenance(e.ctx, n, store.MaintUpdating, "x"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("admin setting 'updating' = %v", err)
	}
}

// TestSendFailureInterrupted: a failed SendUpgrade lands interrupted —
// the frame may have arrived, so no blind retry.
func TestSendFailureInterrupted(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "0.7.0")
	n := e.node(t, "n1", "0.6.0", true, true)
	e.disp.failNext = 1

	// The node is already drained, so 'draining' is a sub-tick transient —
	// wait straight for the interrupted landing rather than the hop.
	r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategySingle, []string{n}, 0, 0, "test")
	if err != nil {
		t.Fatalf("create rollout: %v", err)
	}
	u := e.updatesFor(t, r.ID)[0]
	if got := e.waitPhase(t, u.ID, store.UpdInterrupted); got != store.UpdInterrupted {
		t.Fatalf("phase = %s", got)
	}
	if e.disp.sentCount() != 1 {
		t.Fatalf("sends = %d, want exactly 1 (no retry)", e.disp.sentCount())
	}
}

// TestCreateRolloutAdvanceRace: a kick fired by a skip-finish mid-create
// must never let an advance tick evaluate a half-inserted cohort.
// Regression for the v0.9 chaos finding — before createMu, a parallel
// advance could see only the terminal rows committed so far and land the
// rollout 'succeeded' with a stranded pending update. Skip-first ordering
// is the sharpest probe: finishing node A fires exactly that kick while
// B..F are not yet inserted.
func TestCreateRolloutAdvanceRace(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	rel := e.release(t, "9.9.9")

	for i := 0; i < 12; i++ {
		skipID := e.node(t, fmt.Sprintf("skip-%d", i), "9.9.9", true, false)
		var ids []string
		ids = append(ids, skipID)
		for j := 0; j < 5; j++ {
			ids = append(ids, e.node(t, fmt.Sprintf("p-%d-%d", i, j), "1.0.0", true, false))
		}
		r, err := e.m.CreateRollout(e.ctx, rel.ID, StrategyParallel, ids, 0, 0, "test")
		if err != nil {
			t.Fatalf("create rollout %d: %v", i, err)
		}
		// Let any queued advance pass run, then assert the invariant that
		// used to break: a terminal rollout may never carry live rows.
		time.Sleep(30 * time.Millisecond)
		state := e.getRollout(t, r.ID).State
		switch state {
		case store.RolloutSucceeded, store.RolloutFailed, store.RolloutCancelled:
			for _, u := range e.updatesFor(t, r.ID) {
				if u.FinishedAt == nil {
					t.Fatalf("iter %d: rollout %s carries live update %s (%s)", i, state, u.ID, u.Phase)
				}
			}
		}
	}
}

// releaseFloor registers an approved release whose manifest declares a
// min_agent_version anti-rollback floor.
func (e *env) releaseFloor(t *testing.T, version, minAgent string) *store.Release {
	t.Helper()
	keyID := "TESTKEY-F-" + version
	if _, err := e.st.AddReleaseKey(e.ctx, keyID, "minisign", "pub", ""); err != nil {
		t.Fatalf("add key: %v", err)
	}
	r, err := e.st.RegisterRelease(e.ctx, &store.Release{
		Version: version, Channel: "stable",
		Manifest: []byte(`{"version":"` + version +
			`","min_agent_version":"` + minAgent + `"}`),
		ManifestSHA256: "m", SumsSHA256: "s",
		Signatures: map[string]string{}, SignerKeyID: keyID,
		ProtocolVersion: 1, MinAgentProtocol: 1,
		Platforms: []string{"agent/linux/amd64"},
	})
	if err != nil {
		t.Fatalf("register floor release: %v", err)
	}
	return r
}

// TestRolloutFloorBlocksRollback: once an approved release declares
// min_agent_version, no rollout may target an older agent version —
// the floor survives across releases because it is computed over every
// approved manifest, not the target alone.
func TestRolloutFloorBlocksRollback(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	n := e.node(t, "floor-node", "0.9.0", true, false)

	fr := e.releaseFloor(t, "0.10.0", "0.9.5") // fleet floor: 0.9.5
	old := e.release(t, "0.8.0")               // below floor

	if _, err := e.m.CreateRollout(e.ctx, old.ID, StrategySingle, []string{n}, 0, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("below-floor rollout: got %v, want ErrInvalidRollout", err)
	}
	// The floor-bearing release itself stays rollout-able.
	if _, err := e.m.CreateRollout(e.ctx, fr.ID, StrategySingle, []string{n}, 0, 0, "test"); err != nil {
		t.Fatalf("at-floor rollout: %v", err)
	}
}

// TestRolloutFloorRevocationLifts: revoking the floor-bearing release
// is the deliberate escape hatch — with it revoked, the floor drops and
// the older release rolls out again.
func TestRolloutFloorRevocationLifts(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	n := e.node(t, "floor-node2", "0.9.0", true, false)

	fr := e.releaseFloor(t, "0.10.0", "0.9.5")
	old := e.release(t, "0.8.0")

	if _, err := e.m.CreateRollout(e.ctx, old.ID, StrategySingle, []string{n}, 0, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("below-floor rollout must refuse while floor release is approved: %v", err)
	}
	if err := e.st.RevokeRelease(e.ctx, fr.ID); err != nil {
		t.Fatalf("revoke floor release: %v", err)
	}
	if _, err := e.m.CreateRollout(e.ctx, old.ID, StrategySingle, []string{n}, 0, 0, "test"); err != nil {
		t.Fatalf("rollback must be permitted once floor release is revoked: %v", err)
	}
}

// TestRolloutFloorStrongestWins: multiple floor-bearing releases ratchet
// to the strongest declared floor.
func TestRolloutFloorStrongestWins(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	n := e.node(t, "floor-node3", "0.9.0", true, false)

	e.releaseFloor(t, "0.10.0", "0.9.0")
	e.releaseFloor(t, "0.10.1", "0.9.9") // stronger floor wins
	mid := e.release(t, "0.9.5")         // above 0.9.0, below 0.9.9

	if _, err := e.m.CreateRollout(e.ctx, mid.ID, StrategySingle, []string{n}, 0, 0, "test"); !errors.Is(err, ErrInvalidRollout) {
		t.Fatalf("rollout below strongest floor: got %v, want ErrInvalidRollout", err)
	}
}

// TestRolloutNoFloorUnchanged: with no floor declared anywhere, version
// ordering imposes no new constraint — legacy behaviour preserved.
func TestRolloutNoFloorUnchanged(t *testing.T) {
	e := newEnv(t, fastCfg(), true)
	n := e.node(t, "floor-node4", "0.9.0", true, false)

	old := e.release(t, "0.8.0")
	if _, err := e.m.CreateRollout(e.ctx, old.ID, StrategySingle, []string{n}, 0, 0, "test"); err != nil {
		t.Fatalf("unfloored rollback must remain possible: %v", err)
	}
}
