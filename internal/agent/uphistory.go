package agent

// uphistory.go is the agent's small durable record of update outcomes:
// update-history.json beside agent.json, capped to the last
// maxUpdateHistoryEntries terminal results. It exists for exactly two
// purposes:
//
//   - loop detection — a release that already failed post-install on
//     this node (rolled back, or exhausted its restart-cycle budget)
//     must not be re-installed on redispatch: the same deterministic
//     outcome would repeat. blockedByHistory is that tripwire.
//   - status/doctor surfacing — UpdateHistorySummary renders the latest
//     outcome for operators.
//
// It is NOT an audit log: pre-install refusals, aborts and successes
// are recorded for surfacing but only post-restart terminal failures
// arm the tripwire.
//
// Failure policy is fail-open: an unreadable/corrupt history disables
// the tripwire, it never blocks an update.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

const (
	updateHistoryFile = "update-history.json"

	// maxUpdateHistoryEntries caps the file — old outcomes age out so
	// a stale tripwire cannot block a legitimately fixed release
	// forever.
	maxUpdateHistoryEntries = 10
)

// Recorded outcomes. Post-restart resolutions mark Restarted — only
// those arm the redispatch tripwire.
const (
	upOutcomeSucceeded  = "succeeded"
	upOutcomeFailed     = "failed"
	upOutcomeRolledBack = "rolled_back"
	upOutcomeAborted    = "aborted"
)

// updateHistoryEntry is one terminal update outcome.
type updateHistoryEntry struct {
	OperationID string `json:"operation_id,omitempty"`
	ReleaseID   string `json:"release_id,omitempty"`
	KeyID       string `json:"key_id,omitempty"`
	ToVersion   string `json:"to_version"`
	Outcome     string `json:"outcome"` // succeeded|failed|rolled_back|aborted
	// Restarted marks outcomes decided by the post-restart marker
	// resolution — the install actually ran. Only restarted failures
	// trip the redispatch loop guard.
	Restarted  bool      `json:"restarted,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// readUpdateHistory loads the history file; absent → empty, corrupt →
// error (callers fail open).
func readUpdateHistory(path string) ([]updateHistoryEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var h []updateHistoryEntry
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", updateHistoryFile, err)
	}
	return h, nil
}

// recordUpdateOutcome appends one terminal outcome to the history
// file, keeping only the newest maxUpdateHistoryEntries. Fail-open:
// a write error is logged, never returned — history must not block
// updates.
func (a *Agent) recordUpdateOutcome(e updateHistoryEntry) {
	path, err := a.updateHistoryPath()
	if err != nil {
		return
	}
	if e.FinishedAt.IsZero() {
		e.FinishedAt = time.Now().UTC()
	}
	updateStateMu.Lock()
	defer updateStateMu.Unlock()
	h, err := readUpdateHistory(path)
	if err != nil {
		a.log.Warn("update history unreadable — starting fresh", "err", err)
		h = nil
	}
	h = append(h, e)
	if len(h) > maxUpdateHistoryEntries {
		h = h[len(h)-maxUpdateHistoryEntries:]
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		a.log.Warn("cannot create update state dir", "err", err)
		return
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		a.log.Warn("cannot write update history", "err", err)
	}
}

// historyTripwire reports whether entry e is a post-install terminal
// failure for the same release identity the dispatch wants to install
// again: same version (lenient compare), same release id when both
// sides name one, same signing key when both sides name one. A
// Restarted entry means the swap actually ran on this node and the
// outcome failed — re-running it is a loop, not a retry.
func historyTripwire(e updateHistoryEntry, u contracts.AgentUpgrade) bool {
	if !e.Restarted {
		return false
	}
	if e.Outcome != upOutcomeFailed && e.Outcome != upOutcomeRolledBack {
		return false
	}
	if !versionsEqual(e.ToVersion, u.Version) {
		return false
	}
	if e.ReleaseID != "" && u.ReleaseID != "" && e.ReleaseID != u.ReleaseID {
		return false
	}
	if e.KeyID != "" && u.KeyID != "" && e.KeyID != u.KeyID {
		return false
	}
	return true
}

// blockedByHistory scans newest-first for a tripwire entry matching
// the dispatch. Fail-open on any read problem.
func (a *Agent) blockedByHistory(u contracts.AgentUpgrade) (updateHistoryEntry, bool) {
	path, err := a.updateHistoryPath()
	if err != nil {
		return updateHistoryEntry{}, false
	}
	h, err := readUpdateHistory(path)
	if err != nil {
		a.log.Warn("update history unreadable — loop guard skipped", "err", err)
		return updateHistoryEntry{}, false
	}
	for i := len(h) - 1; i >= 0; i-- {
		if historyTripwire(h[i], u) {
			return h[i], true
		}
	}
	return updateHistoryEntry{}, false
}

// updateHistoryLine renders one entry for status/doctor output.
func updateHistoryLine(e updateHistoryEntry) string {
	s := fmt.Sprintf("update to %s %s at %s",
		e.ToVersion, e.Outcome, e.FinishedAt.UTC().Format(time.RFC3339))
	if e.OperationID != "" {
		s += fmt.Sprintf(" (op %s)", e.OperationID)
	}
	if e.Detail != "" {
		s += " — " + e.Detail
	}
	return s
}

// latestUpdateEntry returns the newest history entry beside the config.
func latestUpdateEntry(cfgPath string) (updateHistoryEntry, bool) {
	h, err := readUpdateHistory(filepath.Join(filepath.Dir(cfgPath), updateHistoryFile))
	if err != nil || len(h) == 0 {
		return updateHistoryEntry{}, false
	}
	return h[len(h)-1], true
}

// UpdateHistorySummary renders the latest update outcome for
// `localmesh-agent status` and doctor — empty when nothing has been
// recorded. It reads only the history file so it works without a
// config, like PendingUpdateSummary.
func UpdateHistorySummary(cfgPath string) string {
	e, ok := latestUpdateEntry(cfgPath)
	if !ok {
		return ""
	}
	return "last " + updateHistoryLine(e)
}
