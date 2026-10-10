//go:build linux

package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const systemdUnit = "localmesh-agent.service"

func systemdUnitPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "systemd", "user", systemdUnit), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnit), nil
}

func serviceAction(action, cfgPath string, out io.Writer) error {
	switch action {
	case "install":
		return systemdInstall(cfgPath, out)
	case "uninstall":
		return systemdUninstall(out)
	case "start":
		_, err := run(out, "systemctl", "--user", "start", systemdUnit)
		return err
	case "stop":
		_, err := run(out, "systemctl", "--user", "stop", systemdUnit)
		return err
	case "status":
		return systemdStatus(out)
	}
	return nil
}

func runUnderService(context.Context, *Config, *slog.Logger, string) (bool, error) {
	return false, nil
}

func systemdInstall(cfgPath string, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	unit, err := systemdUnitPath()
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`[Unit]
Description=LocalMesh node agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run -config %s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, systemdQuote(exe), systemdQuote(cfgPath))
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(out, "writing %s\n", unit)
	if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
		return err
	}
	if _, err := run(out, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := run(out, "systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
		return err
	}
	fmt.Fprintln(out, "installed and started (user unit)")
	fmt.Fprintln(out, "note: `loginctl enable-linger $USER` is required for the")
	fmt.Fprintln(out, "agent to start at boot without an active login session")
	return nil
}

func systemdUninstall(out io.Writer) error {
	if _, err := run(out, "systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
		fmt.Fprintln(out, "disable: unit not loaded/enabled (continuing)")
	}
	unit, err := systemdUnitPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "removing %s\n", unit)
	if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
		return err
	}
	run(out, "systemctl", "--user", "daemon-reload")
	fmt.Fprintln(out, "uninstalled")
	return nil
}

func systemdStatus(out io.Writer) error {
	unit, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(unit); err != nil {
		fmt.Fprintln(out, "not installed (no unit file)")
	} else {
		fmt.Fprintf(out, "unit: %s\n", unit)
	}
	res, err := run(out, "systemctl", "--user", "is-active", systemdUnit)
	if err != nil {
		fmt.Fprintf(out, "status: %s\n", strings.TrimSpace(res))
		return nil
	}
	fmt.Fprintf(out, "status: %s\n", strings.TrimSpace(res))
	res, err = run(out, "systemctl", "--user", "is-enabled", systemdUnit)
	if err == nil {
		fmt.Fprintf(out, "enabled: %s\n", strings.TrimSpace(res))
	}
	return nil
}

// systemdQuote wraps an ExecStart path in quotes when it contains spaces.
func systemdQuote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}
