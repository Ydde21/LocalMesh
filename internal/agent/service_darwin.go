//go:build darwin

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const launchdLabel = "net.localmesh.agent"

func launchdDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func serviceAction(action, cfgPath string, out io.Writer) error {
	switch action {
	case "install":
		return launchdInstall(cfgPath, out)
	case "uninstall":
		return launchdUninstall(out)
	case "start":
		return launchdStart(out)
	case "stop":
		return launchdStop(out)
	case "status":
		return launchdStatus(out)
	}
	return nil
}

func runUnderService(context.Context, *Config, *slog.Logger) (bool, error) {
	return false, nil
}

func launchdInstall(cfgPath string, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	plist, err := launchdPlistPath()
	if err != nil {
		return err
	}
	dir, err := DefaultDir()
	if err != nil {
		return err
	}
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return err
	}
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
		<string>-config</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchdLabel, xmlEsc(exe), xmlEsc(cfgPath),
		xmlEsc(filepath.Join(logs, "agent.log")), xmlEsc(filepath.Join(logs, "agent.err.log")))
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(out, "writing %s\n", plist)
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		return err
	}

	domain := launchdDomain()
	// Idempotent: drop any previously loaded job first.
	if _, err := run(out, "launchctl", "bootout", domain, plist); err == nil {
		fmt.Fprintln(out, "unloaded previous launchd job")
	}
	if _, err := run(out, "launchctl", "bootstrap", domain, plist); err != nil {
		fmt.Fprintln(out, "launchctl bootstrap failed; trying deprecated `launchctl load`")
		if _, err2 := run(out, "launchctl", "load", plist); err2 != nil {
			return errors.Join(err, err2)
		}
	}
	fmt.Fprintf(out, "installed and started %s (logs: %s)\n", launchdLabel, logs)
	return nil
}

func launchdUninstall(out io.Writer) error {
	plist, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if _, err := run(out, "launchctl", "bootout", launchdDomain(), plist); err != nil {
		fmt.Fprintln(out, "launchctl bootout: job not loaded (continuing)")
	}
	fmt.Fprintf(out, "removing %s\n", plist)
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(out, "uninstalled")
	return nil
}

// launchdStart bootstraps the written plist; if the job is already loaded
// it kicks it so a stopped instance runs again.
func launchdStart(out io.Writer) error {
	plist, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); err != nil {
		return fmt.Errorf("no plist at %s — run `localmesh-agent service install` first", plist)
	}
	if launchdLoaded() {
		_, err = run(out, "launchctl", "kickstart", launchdDomain()+"/"+launchdLabel)
		return err
	}
	_, err = run(out, "launchctl", "bootstrap", launchdDomain(), plist)
	return err
}

// launchdStop unloads the job. The plist stays on disk so `service start`
// can re-bootstrap it.
func launchdStop(out io.Writer) error {
	plist, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if _, err := run(out, "launchctl", "bootout", launchdDomain(), plist); err != nil {
		return fmt.Errorf("service not running (or not installed)")
	}
	fmt.Fprintln(out, "stopped")
	return nil
}

func launchdStatus(out io.Writer) error {
	plist, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plist); err != nil {
		fmt.Fprintln(out, "not installed (no plist)")
	} else {
		fmt.Fprintf(out, "plist: %s\n", plist)
	}
	res, err := run(out, "launchctl", "print", launchdDomain()+"/"+launchdLabel)
	if err != nil {
		fmt.Fprintln(out, "status: not running")
		return nil
	}
	state, pid := "unknown", ""
	for _, line := range strings.Split(res, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state =") {
			state = strings.TrimSpace(strings.TrimPrefix(line, "state ="))
		}
		if strings.HasPrefix(line, "pid =") {
			pid = strings.TrimSpace(strings.TrimPrefix(line, "pid ="))
		}
	}
	if pid != "" {
		fmt.Fprintf(out, "status: %s (pid %s)\n", state, pid)
	} else {
		fmt.Fprintf(out, "status: %s\n", state)
	}
	return nil
}

func launchdLoaded() bool {
	return exec.Command("launchctl", "print", launchdDomain()+"/"+launchdLabel).Run() == nil
}

func xmlEsc(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;",
		`"`, "&quot;", "'", "&apos;",
	).Replace(s)
}
