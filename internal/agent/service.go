package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
)

// Service runs an explicit service-lifecycle action on behalf of the
// `localmesh-agent service` subcommand. Nothing here runs implicitly —
// install only happens when the operator asks for it. Actions print the
// exact paths and commands they use.
func Service(action, cfgPath string, out io.Writer) error {
	switch action {
	case "install", "uninstall", "start", "stop", "status":
	default:
		return fmt.Errorf("unknown service action %q (want install|uninstall|start|stop|status)", action)
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	// Installing before enrollment registers a unit whose `run` exits
	// immediately — on every platform that is a restart loop. Require a
	// loadable config up front so the failure message is actionable.
	if action == "install" {
		if _, err := LoadConfig(abs); err != nil {
			return fmt.Errorf("cannot install service: %v — run 'localmesh-agent join' first", err)
		}
	}
	return serviceAction(action, abs, out)
}

// RunUnderService hands the process to the OS service manager when the
// binary was launched as a service (Windows SCM). handled=true means the
// call owns the process lifecycle; on other platforms it returns false
// immediately. cfgPath is forwarded so update state files sit beside
// the config the service was started with.
func RunUnderService(ctx context.Context, cfg *Config, log *slog.Logger, cfgPath string) (handled bool, err error) {
	return runUnderService(ctx, cfg, log, cfgPath)
}

// run prints the command being executed and returns its combined output —
// every service mutation stays auditable.
func run(out io.Writer, name string, args ...string) (string, error) {
	fmt.Fprintf(out, "$ %s %s\n", name, strings.Join(args, " "))
	b, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}
