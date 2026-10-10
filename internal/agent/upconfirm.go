package agent

// upconfirm.go is the post-restart half of a wire-op upgrade. The
// marker written before the swap makes the next boot accountable:
//
//   - hello succeeds + running version == marker.to_version +
//     capability report sent + session holds for the settle window
//     → report verify_health, delete marker (the SERVER marks the op
//     succeeded after its health checks — the agent only reports).
//   - the session dies INSIDE the settle window → the install is not
//     proven healthy: the failure counts against the marker's attempts
//     budget, and on exhaustion the .bak is restored (the same
//     convergence a crash loop gets).
//   - hello succeeds + version mismatch (old binary somehow ran)
//     → report failed, keep marker.
//   - hello REJECTED with a protocol error + marker present
//     → the new binary can't speak to this server: restore the .bak,
//     stamp marker.rolled_back, and exit/re-exec so the old binary
//     reconnects and reports rolled_back on a healthy conn (the
//     best-effort report on the just-rejected conn likely never lands).
//   - marker survives maxUpdateAttempts failed cycles
//     → roll back when a .bak exists (else clear + fail): the node
//     converges on the known-good binary rather than staying broken.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/localmesh/localmesh/internal/agent/update"
	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// upgradeHealthSettle is how long the post-restart session must stay
// up before the agent reports verify_health — connected, authenticated
// and inventory-sent is not enough if the new binary can't hold the
// session. Bounded well under the server's health-verify window. Var
// so tests can shrink it, like the dial/handshake timeouts.
var upgradeHealthSettle = 4 * time.Second

// upgradeBootCheck runs once at process start (Run, before any
// connect): it sweeps stale update artifacts, increments the marker's
// attempt counter so crash loops still converge, and resolves the
// marker when the restart-cycle budget is exhausted — restoring the
// .bak when one exists so the node ends on a known-good binary, or
// clearing + queueing a failed report when there is nothing to roll
// back to. The report itself is queued for the first successful
// session — at boot there is no conn yet.
func (a *Agent) upgradeBootCheck() {
	a.sweepStaleUpdateFiles()
	path, err := a.pendingMarkerPath()
	if err != nil {
		return
	}
	updateStateMu.Lock()
	m, ok, err := readPendingMarker(path)
	if err != nil {
		updateStateMu.Unlock()
		a.log.Warn("pending-update marker unreadable — leaving it for inspection", "err", err)
		return
	}
	if ok && !m.RolledBack {
		m.Attempts++
		err = writePendingMarker(path, m)
	}
	updateStateMu.Unlock()
	if err != nil {
		a.log.Warn("cannot update pending-update attempts", "err", err)
	}
	if !ok || m.RolledBack {
		// No marker, or already resolved on disk — a RolledBack marker
		// only owes the first healthy session its report; no
		// restart-cycle budget applies.
		return
	}
	if m.Attempts < maxUpdateAttempts {
		return
	}
	a.log.Warn("update exhausted its restart-cycle budget — rolling back",
		"op", m.OperationID, "attempts", m.Attempts)
	if a.rollbackMarkedUpdate(nil, path, m) {
		return // restarting into the restored binary
	}
	a.log.Warn("no rollback possible — update fails permanently",
		"op", m.OperationID)
	clearPendingMarker(path)
	a.recordUpdateOutcome(updateHistoryEntry{
		OperationID: m.OperationID, ReleaseID: m.ReleaseID, KeyID: m.KeyID,
		ToVersion: m.ToVersion, Outcome: upOutcomeFailed, Restarted: true,
		Detail: fmt.Sprintf("update to %s failed to confirm after %d restart cycles",
			m.ToVersion, m.Attempts),
	})
	a.bootReport = &contracts.UpgradeState{
		OperationID: m.OperationID,
		Phase:       upPhaseFailed,
		Error: fmt.Sprintf("update to %s failed to confirm after %d restart cycles",
			m.ToVersion, m.Attempts),
		FromVersion: m.FromVersion,
		ToVersion:   m.ToVersion,
	}
}

// postHelloUpgradeCheck runs once per session after hello_ack: it
// flushes any queued boot-time failure report, then evaluates the
// pending marker against the version this process is actually running.
// capOK tells it the capability report actually landed — connected and
// authenticated is only half of "healthy"; a node whose inventory
// never reached the server is not a confirmed update either.
func (a *Agent) postHelloUpgradeCheck(ctx context.Context, c *wsjson.Conn, capOK bool) {
	if a.bootReport != nil {
		a.reportUpgrade(c, *a.bootReport)
		a.bootReport = nil
	}
	path, err := a.pendingMarkerPath()
	if err != nil {
		return
	}
	m, ok, err := readPendingMarker(path)
	if err != nil {
		a.log.Warn("pending-update marker unreadable", "err", err)
		return
	}
	if !ok {
		return
	}
	if m.RolledBack {
		// The .bak is already restored (a prior boot's rollback) —
		// deliver the rolled_back the doomed conn could not, then
		// clear only after the report actually lands.
		werr := a.reportUpgrade(c, contracts.UpgradeState{
			OperationID: m.OperationID,
			Phase:       upPhaseRolledBack,
			Detail:      fmt.Sprintf("restored %s after the update to %s failed post-restart", Current().Version, m.ToVersion),
			FromVersion: m.FromVersion,
			ToVersion:   m.ToVersion,
		})
		if werr == nil {
			if err := clearPendingMarker(path); err != nil {
				a.log.Warn("cannot clear pending-update marker", "err", err)
			}
		}
		return
	}
	cur := Current().Version
	if versionsEqual(cur, m.ToVersion) {
		if !capOK {
			// The capability report never landed — the conn is already
			// broken and the session is on its way out; the marker
			// stays so the next session retries the health proof.
			return
		}
		// Connected + authenticated + inventory sent. Healthy also
		// means the session SURVIVES: the confirm runs asynchronously
		// so the read/heartbeat loops proceed while the settle window
		// elapses.
		go a.confirmUpgradeHealth(ctx, c, path, m.OperationID)
		return
	}
	// The marker names a version this process isn't: the restart never
	// landed on the new binary (stale exec path, restored .bak). Keep
	// the marker — the attempt budget decides when to give up.
	a.reportUpgrade(c, contracts.UpgradeState{
		OperationID: m.OperationID,
		Phase:       upPhaseFailed,
		Error:       fmt.Sprintf("expected %s after restart but %s is running", m.ToVersion, cur),
		Detail:      "marker kept — the update did not land on the new binary",
		FromVersion: m.FromVersion,
		ToVersion:   m.ToVersion,
	})
}

// confirmUpgradeHealth is the settle window between "the new binary
// got through hello" and "the update is healthy": if the session is
// still alive upgradeHealthSettle later, verify_health is reported and
// the marker cleared. If the session dies first the failure is counted
// against the marker's attempts budget — an install that cannot hold
// a session is a failed install, retried only within the budget and
// then rolled back.
func (a *Agent) confirmUpgradeHealth(ctx context.Context, c *wsjson.Conn, path, opID string) {
	t := time.NewTimer(upgradeHealthSettle)
	defer t.Stop()
	select {
	case <-ctx.Done():
		a.noteConfirmFailure(path, opID, c)
		return
	case <-t.C:
	}
	// Re-read under the state lock: the marker may have been resolved
	// (or superseded by another op) while the window elapsed.
	updateStateMu.Lock()
	m, ok, err := readPendingMarker(path)
	updateStateMu.Unlock()
	if err != nil || !ok || m.RolledBack || m.OperationID != opID {
		return
	}
	werr := a.reportUpgrade(c, contracts.UpgradeState{
		OperationID: m.OperationID,
		Phase:       upPhaseVerifyHealth,
		Detail: fmt.Sprintf("restarted into %s and held the session %s — awaiting server health confirmation",
			Current().Version, upgradeHealthSettle),
		FromVersion: m.FromVersion,
		ToVersion:   m.ToVersion,
	})
	if werr != nil {
		// Keep the marker: the next session retries verify_health —
		// a dropped frame must not read as a confirmed update.
		a.log.Warn("verify_health report undelivered; marker kept for retry", "op", m.OperationID)
		return
	}
	if err := clearPendingMarker(path); err != nil {
		a.log.Warn("cannot clear pending-update marker", "err", err)
	}
	a.recordUpdateOutcome(updateHistoryEntry{
		OperationID: m.OperationID, ReleaseID: m.ReleaseID, KeyID: m.KeyID,
		ToVersion: m.ToVersion, Outcome: upOutcomeSucceeded, Restarted: true,
		Detail: "restarted into " + Current().Version + " and held the session",
	})
}

// noteConfirmFailure counts a session that died inside the settle
// window against the marker's attempts budget — the same bound a
// crash loop consumes. On exhaustion it rolls the install back rather
// than letting a flapping session retry forever: each connect attempt
// would otherwise burn bandwidth forever while the update neither
// succeeds nor fails.
func (a *Agent) noteConfirmFailure(path, opID string, c *wsjson.Conn) {
	updateStateMu.Lock()
	m, ok, err := readPendingMarker(path)
	if err == nil && ok && !m.RolledBack && m.OperationID == opID {
		m.Attempts++
		err = writePendingMarker(path, m)
	}
	updateStateMu.Unlock()
	if err != nil {
		a.log.Warn("cannot update pending-update attempts", "err", err)
		return
	}
	if !ok || m.RolledBack || m.OperationID != opID || m.Attempts < maxUpdateAttempts {
		return
	}
	a.log.Warn("update never held a healthy session — rolling back",
		"op", m.OperationID, "attempts", m.Attempts)
	if !a.rollbackMarkedUpdate(c, path, m) {
		// Nothing to roll back to — leave the marker; the boot check
		// resolves it on the next start.
		a.log.Warn("rollback impossible after settle failures", "op", m.OperationID)
	}
}

// rollbackPendingUpdate inspects a failed hello: when the server
// rejected us on protocol grounds AND a pending update marker exists,
// the freshly-installed binary is (probably) the reason — restore the
// .bak and restart into it. Returns immediately for any other failure.
func (a *Agent) rollbackPendingUpdate(c *wsjson.Conn, sessErr error) {
	var ce *ConnectError
	if !errors.As(sessErr, &ce) || ce.Cause != CauseProtocol {
		return
	}
	path, err := a.pendingMarkerPath()
	if err != nil {
		return
	}
	m, ok, err := readPendingMarker(path)
	if err != nil || !ok || m.RolledBack {
		return
	}
	a.log.Warn("hello rejected after update — rolling back",
		"op", m.OperationID, "to_version", m.ToVersion,
		"restoring", m.FromVersion, "err", sessErr)
	if !a.rollbackMarkedUpdate(c, path, m) {
		// The restore failed — the marker stays and the attempt budget
		// bounds retries. Exit so a supervisor restarts us (a flapping
		// new binary beats a wedged broken one).
		osExitFn(1)
	}
}

// rollbackMarkedUpdate stamps the marker RolledBack, restores the
// .bak, and restarts into it — the shared convergence path for the
// protocol-reject, settle-exhaustion and boot-budget-exhaustion cases.
// The terminal report rides c best-effort (nil when there is no conn);
// the restored binary's first healthy session re-delivers it from the
// marker, which is the authoritative copy. Returns false when no
// restore could even be attempted (missing/unrestorable .bak) — the
// caller then decides the fallback (clear+fail at boot, exit in a
// session).
func (a *Agent) rollbackMarkedUpdate(c *wsjson.Conn, path string, m pendingUpdate) bool {
	target := targetFromBackup(m.BakPath)
	state := func(phase, detail, errStr string) contracts.UpgradeState {
		return contracts.UpgradeState{
			OperationID: m.OperationID, Phase: phase,
			Detail: detail, Error: errStr,
			FromVersion: m.FromVersion, ToVersion: m.ToVersion,
		}
	}
	record := func() {
		a.recordUpdateOutcome(updateHistoryEntry{
			OperationID: m.OperationID, ReleaseID: m.ReleaseID, KeyID: m.KeyID,
			ToVersion: m.ToVersion, Outcome: upOutcomeRolledBack, Restarted: true,
			Detail: "restored " + m.FromVersion + " after the new binary failed post-restart",
		})
	}

	// Mark the rollback before restarting: the restored binary's first
	// session reports rolled_back authoritatively — the one written
	// below rides a conn the server may have just rejected.
	updateStateMu.Lock()
	m.RolledBack = true
	err := writePendingMarker(path, m)
	updateStateMu.Unlock()
	if err != nil {
		a.log.Warn("cannot update pending marker for rollback", "err", err)
	}
	if runtime.GOOS == "windows" {
		// The running exe still holds the target — the update-apply
		// helper restores the .bak once we exit, then restarts the agent.
		if err := spawnApplyFn(applyRequest{
			Target: target, ParentPID: os.Getpid(), Rollback: true,
			Service: serviceManaged(), Relaunch: !serviceManaged(),
		}); err != nil {
			a.unstampRolledBack(path, m)
			a.reportUpgrade(c,
				state(upPhaseFailed, "", "rollback helper failed to spawn: "+err.Error()))
			return false
		}
		record()
		a.reportUpgrade(c,
			state(upPhaseRolledBack, "restored "+m.FromVersion, ""))
		osExitFn(0)
		return true
	}
	os.Remove(update.StagedPath(target)) // a leftover staged copy is never needed again
	if err := update.RollbackBinary(target); err != nil {
		a.unstampRolledBack(path, m)
		a.reportUpgrade(c,
			state(upPhaseFailed, "", "rollback failed: "+err.Error()))
		return false
	}
	record()
	a.reportUpgrade(c,
		state(upPhaseRolledBack, "restored "+m.FromVersion, ""))
	if err := execSelfFn(target); err != nil {
		// Restored on disk but re-exec failed — exit so a supervisor
		// (or the operator) starts the old binary normally.
		osExitFn(0)
	}
	return true
}

// unstampRolledBack reverts the marker's RolledBack flag when the
// restore could not run — without it the next session would happily
// report a rollback that never happened and clear the marker while the
// new (broken) binary keeps running.
func (a *Agent) unstampRolledBack(path string, m pendingUpdate) {
	m.RolledBack = false
	updateStateMu.Lock()
	if err := writePendingMarker(path, m); err != nil {
		a.log.Warn("cannot unstamp rollback marker", "err", err)
	}
	updateStateMu.Unlock()
}
