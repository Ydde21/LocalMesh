package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/localmesh/localmesh/internal/agent/update"
)

// pendingUpdate is the durable record of an in-flight self-update,
// written BEFORE the binary swap so the post-restart process knows it
// must confirm (or roll back). It lives next to agent.json — a stale
// copy means a restart landed on an unexpected binary and the update
// needs resolution, not silent success.
type pendingUpdate struct {
	OperationID string    `json:"operation_id"`
	ReleaseID   string    `json:"release_id,omitempty"`
	KeyID       string    `json:"key_id,omitempty"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	BakPath     string    `json:"bak_path"` // update.BackupPath(target) — also yields target by trimming .bak
	StagedAt    time.Time `json:"staged_at"`
	// Attempts counts cycles that failed to prove this update healthy:
	// boots that evaluated the marker AND sessions that died inside the
	// post-restart settle window. Capped at maxUpdateAttempts — a marker
	// still present after that many cycles is a failed update, rolled
	// back when a .bak exists and reported rather than retried forever.
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

// updateStateMu serializes read-modify-write access to the update
// state files (update-pending.json, update-history.json): the attempt
// bump in the boot check and the settle-window failure counter both
// run off the session goroutine and could otherwise lose updates.
var updateStateMu sync.Mutex

// updateStateDir returns the directory holding the agent's update
// state files (update-pending.json, update-history.json). SetConfigPath
// points it at a custom -config location; otherwise it follows
// DefaultDir (LOCALMESH_AGENT_DIR or ~/.localmesh).
func (a *Agent) updateStateDir() (string, error) {
	if a.cfgPath != "" {
		return filepath.Dir(a.cfgPath), nil
	}
	return DefaultDir()
}

// pendingMarkerPath returns the marker path beside the agent config.
func (a *Agent) pendingMarkerPath() (string, error) {
	dir, err := a.updateStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pendingUpdateFile), nil
}

// updateHistoryPath returns the update-history path beside the config.
func (a *Agent) updateHistoryPath() (string, error) {
	dir, err := a.updateStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, updateHistoryFile), nil
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

// clearPendingMarker removes the marker under updateStateMu so a
// clear can never interleave inside another goroutine's
// read-modify-write section (which would resurrect a resolved marker).
// Callers must not hold the mutex. Windows makes a single Remove
// unreliable — a transient indexer/AV lock fails it once and the
// marker would survive to re-report next boot — so removal retries
// briefly before giving up.
func clearPendingMarker(path string) error {
	updateStateMu.Lock()
	defer updateStateMu.Unlock()
	var err error
	for i := 0; i < 5; i++ {
		err = os.Remove(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
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

// staleUpdateFileGrace bounds how young a staging leftover must be to
// survive the boot sweep. An update-apply helper a just-exited parent
// spawned works inside a bounded window and may still legitimately
// need <target>.new, so only files older than this are reclaimed.
var staleUpdateFileGrace = 10 * time.Minute

// sweepStaleUpdateFiles reclaims leftover staging/temp files a crashed
// or abandoned update could leave behind: the staged <target>.new
// beside the binary, and *.new/*.staged/*.tmp/*.bak files in the agent
// state dir. The live rollback copy and any .bak a pending marker
// still references are always kept; files younger than
// staleUpdateFileGrace are left alone. Runs once at boot — the update
// exit paths already remove their own artifacts, so anything stale
// enough to sweep is crash debris by definition.
func (a *Agent) sweepStaleUpdateFiles() {
	dir, err := a.updateStateDir()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleUpdateFileGrace)

	// Paths that must survive regardless of age: the live target's
	// rollback copy and the .bak a pending marker still needs.
	keep := map[string]bool{}
	target := a.updTarget
	if target == "" {
		if t, err := update.DefaultTarget(); err == nil {
			target = t
		}
	}
	if target != "" {
		if abs, err := filepath.Abs(update.BackupPath(target)); err == nil {
			keep[abs] = true
		}
	}
	if mkPath, err := a.pendingMarkerPath(); err == nil {
		if m, ok, err := readPendingMarker(mkPath); err == nil && ok && m.BakPath != "" {
			if abs, err := filepath.Abs(m.BakPath); err == nil {
				keep[abs] = true
			}
		}
	}

	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			switch {
			case strings.HasSuffix(name, ".new"),
				strings.HasSuffix(name, ".staged"),
				strings.HasSuffix(name, ".tmp"):
				// update-owned staging/temp leftovers
			case strings.HasSuffix(name, ".bak") &&
				!strings.Contains(strings.TrimSuffix(name, ".bak"), "."):
				// binary-style backups only — a dotted stem means an
				// operator backup like agent.json.bak, not update debris
			default:
				continue
			}
			a.removeIfStale(filepath.Join(dir, name), keep, cutoff)
		}
	}
	if target != "" {
		a.removeIfStale(update.StagedPath(target), keep, cutoff)
	}
}

func (a *Agent) removeIfStale(path string, keep map[string]bool, cutoff time.Time) {
	if abs, err := filepath.Abs(path); err == nil && keep[abs] {
		return
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.ModTime().After(cutoff) {
		return
	}
	if err := os.Remove(path); err != nil {
		a.log.Warn("cannot remove stale update file", "path", path, "err", err)
		return
	}
	a.log.Info("removed stale update file", "path", path)
}
