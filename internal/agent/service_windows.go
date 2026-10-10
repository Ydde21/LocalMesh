//go:build windows

package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
)

const serviceName = "LocalMeshAgent"

func serviceAction(action, cfgPath string, out io.Writer) error {
	switch action {
	case "install":
		return scInstall(cfgPath, out)
	case "uninstall":
		return scUninstall(out)
	case "start":
		_, err := run(out, "sc.exe", "start", serviceName)
		return err
	case "stop":
		_, err := run(out, "sc.exe", "stop", serviceName)
		return err
	case "status":
		return scStatus(out)
	}
	return nil
}

func scInstall(cfgPath string, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	binPath := fmt.Sprintf(`"%s" run -config "%s"`, exe, cfgPath)
	// sc.exe grammar: each option is "name=" followed by the value as the
	// next argument.
	if _, err := run(out, "sc.exe", "create", serviceName,
		"binPath=", binPath, "start=", "auto"); err != nil {
		return fmt.Errorf("%w (requires an elevated shell)", err)
	}
	// Restart on failure after a short delay.
	run(out, "sc.exe", "failure", serviceName, "reset=", "86400",
		"actions=", "restart/10000/restart/10000/restart/10000")
	fmt.Fprintf(out, "installed service %s → %s\n", serviceName, binPath)
	fmt.Fprintln(out, "start it now with: localmesh-agent service start")
	return nil
}

func scUninstall(out io.Writer) error {
	if _, err := run(out, "sc.exe", "stop", serviceName); err != nil {
		fmt.Fprintln(out, "stop: service not running (continuing)")
	}
	if _, err := run(out, "sc.exe", "delete", serviceName); err != nil {
		return fmt.Errorf("%w (requires an elevated shell)", err)
	}
	fmt.Fprintln(out, "uninstalled")
	return nil
}

func scStatus(out io.Writer) error {
	res, err := run(out, "sc.exe", "query", serviceName)
	if err != nil {
		fmt.Fprintln(out, "status: not installed")
		return nil
	}
	state := "unknown"
	for _, line := range strings.Split(res, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "STATE") {
			if i := strings.LastIndex(line, " "); i >= 0 {
				state = line[i+1:]
			}
		}
	}
	fmt.Fprintf(out, "status: %s\n", state)
	return nil
}

// runUnderService runs the agent under the Windows Service Control
// Manager when the process was launched as a service; interactive
// invocations return handled=false so `run` stays a foreground command.
func runUnderService(ctx context.Context, cfg *Config, log *slog.Logger, cfgPath string) (bool, error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false, err
	}
	return true, svc.Run(serviceName, &winService{ctx: ctx, cfg: cfg, log: log, cfgPath: cfgPath})
}

type winService struct {
	ctx     context.Context
	cfg     *Config
	log     *slog.Logger
	cfgPath string
}

func (s *winService) Execute(_ []string, r <-chan svc.ChangeRequest, schan chan<- svc.Status) (bool, uint32) {
	schan <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		ag := New(s.cfg, s.log)
		ag.SetConfigPath(s.cfgPath)
		errCh <- ag.Run(ctx)
	}()

	schan <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	s.log.Info("running as Windows service", "name", serviceName)
	for {
		select {
		case err := <-errCh:
			if err != nil {
				s.log.Error("agent exited", "err", err)
			}
			schan <- svc.Status{State: svc.Stopped}
			return false, 0
		case cr := <-r:
			switch cr.Cmd {
			case svc.Interrogate:
				schan <- cr.CurrentStatus
			case svc.Stop, svc.Shutdown:
				schan <- svc.Status{State: svc.StopPending, WaitHint: uint32((10 * time.Second).Milliseconds())}
				cancel()
				<-errCh
				schan <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		}
	}
}
