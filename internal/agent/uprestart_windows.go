//go:build windows

package agent

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// serviceManaged reports whether the SCM owns this process — the
// update handoff then exits and lets `sc start` bring the new binary
// up rather than re-launching directly.
func serviceManaged() bool {
	isSvc, err := svc.IsWindowsService()
	return err == nil && isSvc
}

// execSelfPlatform is unsupported: Windows cannot exec in place, and a
// running exe cannot be replaced anyway — the update-apply helper does
// the swap after this process exits.
func execSelfPlatform(string) error {
	return errors.New("exec-self is not supported on windows")
}

// pidAlivePlatform probes the parent pid for the update-apply helper.
// OpenProcess failing means the process is gone; a successful open
// followed by a non-signaled wait means it still runs.
func pidAlivePlatform(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	// Signaled (WAIT_OBJECT_0) means the process has exited; anything
	// else without an error means it still runs.
	event, err := windows.WaitForSingleObject(h, 0)
	return err == nil && event != windows.WAIT_OBJECT_0
}

const (
	winDetachedProcess       = 0x00000008 // DETACHED_PROCESS — no console
	winCreateNewProcessGroup = 0x00000200 // CREATE_NEW_PROCESS_GROUP
)

// spawnDetached starts a child with no console and its own process
// group so it survives this process exiting.
func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winDetachedProcess | winCreateNewProcessGroup,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process == nil {
		return errors.New("spawn: no process handle")
	}
	return cmd.Process.Release()
}
