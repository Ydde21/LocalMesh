//go:build unix

package agent

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

// serviceManaged reports whether this process runs under a service
// supervisor that will restart it on exit: systemd sets INVOCATION_ID
// for service-spawned processes, launchd sets XPC_SERVICE_NAME to the
// job label (ours is net.localmesh.agent).
func serviceManaged() bool {
	switch runtime.GOOS {
	case "linux":
		return os.Getenv("INVOCATION_ID") != ""
	case "darwin":
		return strings.HasPrefix(os.Getenv("XPC_SERVICE_NAME"), "net.localmesh")
	}
	return false
}

// execSelfPlatform re-execs the (already swapped) binary in place —
// the running image is replaced by the new on-disk binary with the
// same argv and environment.
func execSelfPlatform(target string) error {
	return syscall.Exec(target, os.Args, os.Environ())
}

// pidAlivePlatform probes a pid with signal 0 — the update-apply
// helper's parent-death watch.
func pidAlivePlatform(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// spawnDetached starts a child in its own session with no attached
// stdio — it survives this process exiting.
func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process == nil {
		return errors.New("spawn: no process handle")
	}
	return cmd.Process.Release()
}
