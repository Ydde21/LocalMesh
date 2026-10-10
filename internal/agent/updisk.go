package agent

// updisk.go gates an upgrade on filesystem headroom: the staged
// <target>.new, the live binary, and the <target>.bak rollback copy
// must coexist during the swap, so the artifact's filesystem needs
// roughly 2× the artifact free before any byte is fetched. Refusing
// here beats discovering ENOSPC mid-download or — worse — mid-rename.
//
// Fail-open on measurement problems: a filesystem gopsutil can't read
// reports WARN and proceeds (doctor already surfaces that), matching
// doctor.go's disk check rather than bricking updates on an exotic
// mount.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/shirou/gopsutil/v4/disk"
)

// updateDiskMargin — free space must be at least this multiple of the
// artifact: staged .new + live binary + .bak coexist during the swap.
const updateDiskMargin = 2

// diskFreeFn reports free bytes on the filesystem holding dir —
// injectable like osExitFn/spawnApplyFn so tests can force refusals.
var diskFreeFn = func(dir string) (uint64, error) {
	u, err := disk.Usage(dir)
	if err != nil {
		return 0, err
	}
	return u.Free, nil
}

// checkUpdateDisk returns an "insufficient disk" error when the
// filesystem holding the install target lacks margin× the artifact.
// artBytes < 0 is treated as unknown (no gate).
func (a *Agent) checkUpdateDisk(target string, artBytes int64) error {
	if artBytes < 0 {
		return nil
	}
	dir := filepath.Dir(target)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			a.log.Warn("cannot locate filesystem for update target — disk check skipped",
				"target", target)
			return nil
		}
		dir = parent
	}
	free, err := diskFreeFn(dir)
	if err != nil {
		a.log.Warn("cannot measure free space for update — disk check skipped",
			"dir", dir, "err", err)
		return nil
	}
	need := uint64(artBytes) * updateDiskMargin
	if free < need {
		return fmt.Errorf("insufficient disk space on %s: %.1f MiB free, need %.1f MiB (2× the %.1f MiB artifact) — free space before retrying",
			dir, mib(free), mib(need), mib(uint64(artBytes)))
	}
	return nil
}

func mib(n uint64) float64 { return float64(n) / (1 << 20) }
