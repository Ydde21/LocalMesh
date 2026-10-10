package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pendingUpdate is the durable record of an in-flight self-update,
// written BEFORE the binary swap so the post-restart process knows it
// must confirm (or roll back). It lives next to agent.json — a stale
// copy means a restart landed on an unexpected binary and the update
// needs resolution, not silent success.
type pendingUpdate struct {
	OperationID string    `json:"operation_id"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	BakPath     string    `json:"bak_path"` // update.BackupPath(target) — also yields target by trimming .bak
	StagedAt    time.Time `json:"staged_at"`
	// Attempts counts boots that have evaluated this marker. Capped at
	// maxUpdateAttempts: a marker still present after that many cycles
	// is a failed update, cleared and reported rather than retried
	// forever.
	Attempts int `json:"attempts"`
	// RolledBack is stamped when the .bak was restored — the restored
	// binary's first healthy session owes the server the rolled_back
	// report (the pre-restart one rode a conn the server had just
	// rejected, so it almost certainly never landed).
	RolledBack bool `json:"rolled_back,omitempty"`
}

const pendingUpdateFile = "update-pending.json"

// maxUpdateAttempts bounds how many restart cycles a pending update
// may consume before the marker is dropped and the op reported failed
// — the loop guard for pathological rollback↔reject ping-pong.
const maxUpdateAttempts = 3

// pendingMarkerPath returns the marker path beside the agent config.
// SetConfigPath points it at a custom -config location; otherwise it
// follows DefaultDir (LOCALMESH_AGENT_DIR or ~/.localmesh).
func (a *Agent) pendingMarkerPath() (string, error) {
	dir := ""
	if a.cfgPath != "" {
		dir = filepath.Dir(a.cfgPath)
	} else {
		d, err := DefaultDir()
		if err != nil {
			return "", err
		}
		dir = d
	}
	return filepath.Join(dir, pendingUpdateFile), nil
}

// SetConfigPath records where agent.json lives so update state files
// (update-pending.json) are written beside it.
func (a *Agent) SetConfigPath(p string) { a.cfgPath = p }

func writePendingMarker(path string, m pendingUpdate) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// readPendingMarker loads the marker; ok=false when absent or
// unreadable-for-absence. A corrupt marker is reported as an error —
// callers decide whether to treat it as fatal to the boot check.
func readPendingMarker(path string) (pendingUpdate, bool, error) {
	var m pendingUpdate
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return m, false, nil
		}
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("corrupt %s: %w", pendingUpdateFile, err)
	}
	return m, true, nil
}

func clearPendingMarker(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// targetFromBackup recovers the live binary path a marker's backup
// copy would restore — target and .bak always sit side by side.
func targetFromBackup(bak string) string {
	return strings.TrimSuffix(bak, ".bak")
}

// PendingUpdateSummary renders the one-line pending-update note that
// `localmesh-agent status` and `doctor` surface; empty when no marker
// is present. It reads only the marker file — no config required —
// so it works for the un-enrolled-but-mid-update case too.
func PendingUpdateSummary(cfgPath string) string {
	path := filepath.Join(filepath.Dir(cfgPath), pendingUpdateFile)
	m, ok, err := readPendingMarker(path)
	if err != nil || !ok {
		return ""
	}
	return fmt.Sprintf("update to %s pending verification (op %s, from %s)",
		m.ToVersion, m.OperationID, m.FromVersion)
}

// versionsEqual compares build stamps leniently: the wire may say
// "0.7.0" while the binary stamp says "v0.7.0" — that is a match, not
// a failed restart.
func versionsEqual(a, b string) bool {
	na := strings.TrimPrefix(strings.TrimSpace(a), "v")
	nb := strings.TrimPrefix(strings.TrimSpace(b), "v")
	return na != "" && na == nb
}
