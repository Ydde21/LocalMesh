package update

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// Atomic binary replacement. The new binary is first fully staged as
// <target>.new on the same filesystem, checksum-verified, then installed
// with two renames: <target> -> <target>.bak (rollback copy, always
// preserved) and <target>.new -> <target>. If the second rename fails
// the .bak is restored, so the install path never leaves the target
// missing.
//
// The new binary is never executed by this code path — a service
// restart (or the operator) starts it.

const (
	stagedSuffix = ".new"
	backupSuffix = ".bak"
)

// StagedPath is where StageArtifact places the verified incoming
// binary for a given target.
func StagedPath(target string) string { return target + stagedSuffix }

// BackupPath is where InstallBinary preserves the outgoing binary —
// the rollback copy. Callers that must name the backup before the swap
// happens (the wire-op pending-update marker) use this so the naming
// contract lives in one place.
func BackupPath(target string) string { return target + backupSuffix }

// StageArtifact streams the artifact from src to <target>.new, sets
// mode 0755, and verifies its SHA-256 against wantHex. A failed or
// partial stage never touches the live binary; the staged file is
// removed on error.
func StageArtifact(src Source, name, target, wantHex string) (staged string, err error) {
	staged = target + stagedSuffix
	f, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("cannot stage %s: %w", staged, err)
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	err = src.Stream(name, w)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("fetching %s: %w", name, err)
	}
	got := fmt.Sprintf("%x", h.Sum(nil))
	if got != wantHex {
		os.Remove(staged)
		return "", fmt.Errorf("staged %s sha256 mismatch (want %s, got %s) — refusing to install",
			name, wantHex, got)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(staged, 0o755); err != nil {
			os.Remove(staged)
			return "", err
		}
	}
	return staged, nil
}

// InstallBinary atomically replaces target with the already-verified
// staged file, keeping the previous binary as <target>.bak. The
// returned bak path is empty for a fresh install (nothing to back up).
// On Windows replacing a running executable fails — that error is
// detected and annotated rather than silently swallowed.
func InstallBinary(staged, target string) (bak string, err error) {
	bak = target + backupSuffix
	if _, err := os.Stat(target); err == nil {
		// Only the latest .bak is ever kept: a leftover backup from a
		// previous update is removed first — os.Rename will not replace
		// an existing destination on Windows (the helper's swaps run
		// there), and even on unix a stale backup is dead weight.
		os.Remove(bak)
		if err := os.Rename(target, bak); err != nil {
			return "", fmt.Errorf("cannot move %s aside%s: %w",
				target, windowsBusyHint(), err)
		}
	} else if os.IsNotExist(err) {
		bak = "" // fresh install — no rollback copy exists
	} else {
		return "", fmt.Errorf("cannot inspect %s: %w", target, err)
	}
	if err := os.Rename(staged, target); err != nil {
		// Roll back: the target is currently absent.
		rerr := os.Rename(bak, target)
		os.Remove(staged)
		if rerr != nil {
			return "", fmt.Errorf("install failed (%v) AND rollback failed (%v) — restore %s to %s manually",
				err, rerr, bak, target)
		}
		return "", fmt.Errorf("install failed, previous binary restored: %w", err)
	}
	return bak, nil
}

// RollbackBinary restores <target>.bak over <target> — the manual
// recovery path after a bad update.
func RollbackBinary(target string) error {
	bak := target + backupSuffix
	if _, err := os.Stat(bak); err != nil {
		return fmt.Errorf("no rollback copy at %s", bak)
	}
	// Replace via rename so a running (unix) binary isn't disturbed
	// mid-write.
	if err := os.Rename(bak, target); err != nil {
		return fmt.Errorf("cannot restore %s -> %s%s: %w", bak, target, windowsBusyHint(), err)
	}
	return nil
}

func windowsBusyHint() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	return " — the running executable cannot be replaced on Windows; stop the agent first (`localmesh-agent service stop` or `sc.exe stop LocalMeshAgent`)"
}

// DefaultTarget resolves the running binary's path, resolving symlinks
// so the real file — not a link — is replaced.
func DefaultTarget() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}
