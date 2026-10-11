package agent

// uprestart.go is the restart half of the wire-op upgrade: the
// platform primitives live in uprestart_{unix,windows,other}.go, this
// file holds the handoff request, the injectable seams, and the
// detached `update-apply` helper logic that Windows uses to swap a
// binary its own process still has mapped.
//
// update-apply does exactly three things: wait for the parent pid to
// die, perform the rename dance (forward: InstallBinary; rollback:
// RollbackBinary), and restart the agent (service manager or detached
// `run`). It has no network and parses nothing beyond flags.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/localmesh/localmesh/internal/agent/update"
)

// Injectable process seams — like the dial/handshake timeout vars, tests
// swap these to observe exits without dying. serviceManagedFn is here
// too: supervisor markers (INVOCATION_ID, the SCM session check) leak
// into test processes on CI runners, so the restart path must be
// forced deterministic like the others.
var (
	osExitFn         = os.Exit
	execSelfFn       = execSelfPlatform
	spawnApplyFn     = spawnUpdateApply
	serviceManagedFn = serviceManaged
)

// inLocalMeshUnit reports whether a /proc/self/cgroup dump places the
// process inside the localmesh-agent systemd unit — the only proof a
// supervisor will restart us. INVOCATION_ID alone can't provide it:
// anything that is itself a unit (CI runners, systemd-run wrappers)
// leaks it to every child without supervising them.
func inLocalMeshUnit(cgroup []byte) bool {
	return bytes.Contains(cgroup, []byte("localmesh-agent.service"))
}

// applyRequest describes one update-apply handoff — the parent builds
// it and spawnUpdateApply turns it into argv.
type applyRequest struct {
	Target    string // live binary to replace (or restore over)
	Staged    string // verified <target>.new; unused for Rollback
	ParentPID int    // pid the child waits on before touching Target
	Service   bool   // restart via the OS service manager
	Relaunch  bool   // restart via detached `<target> run`
	Rollback  bool   // restore Target.bak instead of installing Staged
	// ConfigPath is forwarded to the relaunched `<target> run` so a
	// custom -config location survives the restart (service-managed
	// restarts carry it in the installed binPath already).
	ConfigPath string
}

// spawnUpdateApply launches this same binary as a detached
// `update-apply` child.
func spawnUpdateApply(req applyRequest) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"update-apply",
		"-target", req.Target,
		"-pid", strconv.Itoa(req.ParentPID),
	}
	if req.Staged != "" {
		args = append(args, "-staged", req.Staged)
	}
	if req.Rollback {
		args = append(args, "-rollback")
	}
	if req.ConfigPath != "" {
		args = append(args, "-config", req.ConfigPath)
	}
	// The restart mode is orthogonal to the swap direction — a rollback
	// still needs its relaunch/service-start instruction.
	switch {
	case req.Service:
		args = append(args, "-service")
	case req.Relaunch:
		args = append(args, "-relaunch")
	}
	return spawnDetached(exe, args...)
}

// UpdateApplyOptions controls the update-apply helper. It is exported
// for the hidden `localmesh-agent update-apply` subcommand.
type UpdateApplyOptions struct {
	Target     string        // binary to replace/restore (required)
	Staged     string        // <target>.new to install (forward mode; required unless Rollback)
	ParentPID  int           // pid to await before swapping (0 = no wait)
	Rollback   bool          // restore Target.bak rather than installing Staged
	Service    bool          // afterwards: `service start`
	Relaunch   bool          // afterwards: detached `<target> run`
	ConfigPath string        // forwarded to the relaunched `run` as -config
	WaitBound  time.Duration // total bound on pid-wait + swap retries (default 60s)
	PollEvery  time.Duration // poll interval (default 250ms)
	Out        io.Writer     // progress; nil → io.Discard

	// Test seams (nil → platform defaults).
	pidAlive   func(pid int) bool
	relaunchFn func(target, cfgPath string) error
	serviceFn  func(out io.Writer) error
}

// UpdateApply is the detached-helper entry point: wait for the parent
// to exit, swap the binary, restart the agent.
func UpdateApply(ctx context.Context, opts UpdateApplyOptions) error {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	if opts.Target == "" {
		return errors.New("update-apply: -target is required")
	}
	if !opts.Rollback && opts.Staged == "" {
		return errors.New("update-apply: -staged is required (or pass -rollback)")
	}
	bound := opts.WaitBound
	if bound <= 0 {
		bound = 60 * time.Second
	}
	poll := opts.PollEvery
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	alive := opts.pidAlive
	if alive == nil {
		alive = pidAlivePlatform
	}

	// 1. Wait for the parent to release the target binary. On Windows
	// the exe stays locked while the process lives; elsewhere this is
	// instant.
	if opts.ParentPID > 0 {
		deadline := time.Now().Add(bound)
		for alive(opts.ParentPID) {
			if time.Now().After(deadline) {
				return fmt.Errorf("update-apply: parent pid %d still alive after %s", opts.ParentPID, bound)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(poll):
			}
		}
	}

	// 2. Swap, retrying until the bound — the OS can keep the file
	// locked briefly after the process dies (AV scanners, indexing).
	deadline := time.Now().Add(bound)
	var swapErr error
	for {
		if opts.Rollback {
			swapErr = update.RollbackBinary(opts.Target)
		} else {
			_, swapErr = update.InstallBinary(opts.Staged, opts.Target)
		}
		if swapErr == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("update-apply: swap failed for %s: %w", bound, swapErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	if opts.Rollback {
		fmt.Fprintf(out, "rolled back %s\n", opts.Target)
	} else {
		fmt.Fprintf(out, "installed %s\n", opts.Target)
	}

	// 3. Restart the agent.
	if opts.Service {
		fn := opts.serviceFn
		if fn == nil {
			fn = func(o io.Writer) error { return serviceAction("start", "", o) }
		}
		return fn(out)
	}
	if opts.Relaunch {
		fn := opts.relaunchFn
		if fn == nil {
			fn = relaunchAgent
		}
		return fn(opts.Target, opts.ConfigPath)
	}
	return nil
}

// relaunchAgent starts `<target> run` detached — the foreground-agent
// restart path after update-apply swaps the binary. The config path is
// forwarded so an agent started with a custom -config comes back on
// the same config instead of the default path (which may not exist).
func relaunchAgent(target, cfgPath string) error {
	args := []string{"run"}
	if cfgPath != "" {
		args = append(args, "-config", cfgPath)
	}
	return spawnDetached(target, args...)
}
