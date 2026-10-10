package agent

// upconfirm.go is the post-restart half of a wire-op upgrade. The
// marker written before the swap makes the next boot accountable:
//
//   - hello succeeds + running version == marker.to_version
//     → report verify_health, delete marker (the SERVER marks the op
//     succeeded after its health checks — the agent only reports).
//   - hello succeeds + version mismatch (old binary somehow ran)
//     → report failed, keep marker.
//   - hello REJECTED with a protocol error + marker present
//     → the new binary can't speak to this server: restore the .bak,
//     stamp marker.rolled_back, and exit/re-exec so the old binary
//     reconnects and reports rolled_back on a healthy conn (the
//     best-effort report on the just-rejected conn likely never lands).
//   - marker survives maxUpdateAttempts boots
//     → clear it and report failed — the loop guard.

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/localmesh/localmesh/internal/agent/update"
	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// upgradeBootCheck runs once at process start (Run, before any
// connect): it increments the marker's attempt counter so crash loops
// still converge, and clears+reports the marker when the restart-cycle
// budget is exhausted. The report itself is queued for the first
// successful session — at boot there is no conn yet.
func (a *Agent) upgradeBootCheck() {
	path, err := a.pendingMarkerPath()
	if err != nil {
		return
	}
	m, ok, err := readPendingMarker(path)
	if err != nil {
		a.log.Warn("pending-update marker unreadable — leaving it for inspection", "err", err)
		return
	}
	if !ok {
		return
	}
	if m.RolledBack {
		// Already resolved on disk — this marker only owes the first
		// healthy session its rolled_back report; no restart-cycle
		// budget applies.
		return
	}
	m.Attempts++
	if err := writePendingMarker(path, m); err != nil {
		a.log.Warn("cannot update pending-update attempts", "err", err)
	}
	if m.Attempts >= maxUpdateAttempts {
		a.log.Warn("update exceeded its restart-cycle budget — clearing marker",
			"op", m.OperationID, "attempts", m.Attempts)
		clearPendingMarker(path)
		a.bootReport = &contracts.UpgradeState{
			OperationID: m.OperationID,
			Phase:       upPhaseFailed,
			Error: fmt.Sprintf("update to %s failed to confirm after %d restart cycles",
				m.ToVersion, m.Attempts),
			FromVersion: m.FromVersion,
			ToVersion:   m.ToVersion,
		}
	}
}

// postHelloUpgradeCheck runs once per session after hello_ack: it
// flushes any queued boot-time failure report, then evaluates the
// pending marker against the version this process is actually running.
func (a *Agent) postHelloUpgradeCheck(c *wsjson.Conn) {
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
			Detail:      fmt.Sprintf("restored %s after the server rejected %s at hello", Current().Version, m.ToVersion),
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
		werr := a.reportUpgrade(c, contracts.UpgradeState{
			OperationID: m.OperationID,
			Phase:       upPhaseVerifyHealth,
			Detail:      fmt.Sprintf("restarted into %s — awaiting server health confirmation", cur),
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
	if err != nil || !ok {
		return
	}
	target := targetFromBackup(m.BakPath)
	a.log.Warn("hello rejected after update — rolling back",
		"op", m.OperationID, "to_version", m.ToVersion,
		"restoring", m.FromVersion, "err", sessErr)
	// Mark the rollback before restarting: the restored binary's first
	// session reports rolled_back authoritatively — the one we write
	// below rides a conn the server just rejected.
	m.RolledBack = true
	if err := writePendingMarker(path, m); err != nil {
		a.log.Warn("cannot update pending marker for rollback", "err", err)
	}
	state := func(phase, detail, errStr string) contracts.UpgradeState {
		return contracts.UpgradeState{
			OperationID: m.OperationID, Phase: phase,
			Detail: detail, Error: errStr,
			FromVersion: m.FromVersion, ToVersion: m.ToVersion,
		}
	}
	if runtime.GOOS == "windows" {
		// The running (rejected) exe still holds the target — the
		// update-apply helper restores the .bak once we exit, then
		// restarts the agent.
		if err := spawnApplyFn(applyRequest{
			Target: target, ParentPID: os.Getpid(), Rollback: true,
			Service: serviceManaged(), Relaunch: !serviceManaged(),
		}); err != nil {
			a.reportUpgrade(c,
				state(upPhaseFailed, "", "rollback helper failed to spawn: "+err.Error()))
			osExitFn(1)
			return
		}
		a.reportUpgrade(c,
			state(upPhaseRolledBack, "restored "+m.FromVersion, ""))
		osExitFn(0)
		return
	}
	if err := update.RollbackBinary(target); err != nil {
		a.reportUpgrade(c,
			state(upPhaseFailed, "", "rollback failed: "+err.Error()))
		osExitFn(1) // marker stays; the attempt budget bounds retries
		return
	}
	a.reportUpgrade(c,
		state(upPhaseRolledBack, "restored "+m.FromVersion, ""))
	if err := execSelfFn(target); err != nil {
		// Restored on disk but re-exec failed — exit so a supervisor
		// (or the operator) starts the old binary normally.
		osExitFn(0)
	}
}
