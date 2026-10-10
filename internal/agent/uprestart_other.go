//go:build !unix && !windows

package agent

import (
	"errors"
	"os"
	"os/exec"
)

// Fallback platform primitives for OSes that are neither unix nor
// Windows (plan9, js): no service detection, no exec-self, no reliable
// pid watch. The centralized update still verifies and stages but the
// restart handoff reports its limits plainly.
func serviceManaged() bool { return false }

func execSelfPlatform(string) error {
	return errors.New("exec-self is not supported on this platform")
}

func pidAlivePlatform(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p != nil
}

func spawnDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process == nil {
		return errors.New("spawn: no process handle")
	}
	return cmd.Process.Release()
}
