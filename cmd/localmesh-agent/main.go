// Command localmesh-agent runs the LocalMesh node agent.
//
// Usage:
//
//	localmesh-agent join --server https://host:8443 --token lme_... [--name ...] [--ca-fingerprint sha256:...]
//	localmesh-agent run [--config path]
//	localmesh-agent doctor [--config path] [--ca-file ca.pem]
//	localmesh-agent service install|uninstall|start|stop|status [--config path]
//	localmesh-agent status
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/agent/hardware"
)

var version = "dev"

func main() {
	hardware.SetVersion(version)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch os.Args[1] {
	case "join":
		cmdJoin(log)
	case "run":
		cmdRun(log)
	case "doctor":
		os.Exit(cmdDoctor())
	case "service":
		cmdService()
	case "status":
		cmdStatus()
	case "version":
		fmt.Println("localmesh-agent", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `localmesh-agent — LocalMesh node agent

Commands:
  join     enroll this machine with a control plane
  run      connect and serve workloads (default when configured)
  doctor   check connectivity, credentials, engine, and telemetry
  service  install|uninstall|start|stop|status the OS service
  status   show local agent configuration

Run "localmesh-agent join -h" for options.
`)
}

func cmdJoin(log *slog.Logger) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	server := fs.String("server", "", "control plane URL, e.g. https://host:8443")
	token := fs.String("token", "", "enrollment token (lme_...)")
	name := fs.String("name", "", "node display name (default: hostname)")
	fingerprint := fs.String("ca-fingerprint", "", "pin cluster CA fingerprint (sha256:...)")
	wait := fs.Duration("wait", 30*time.Minute, "max time to await admin approval")
	cfgPath := fs.String("config", "", "config file path (default ~/.localmesh/agent.json)")
	ollama := fs.String("ollama-url", "", "Ollama endpoint (default http://127.0.0.1:11434)")
	foreground := fs.Bool("run", true, "start serving workloads after approval")
	fs.Parse(os.Args[2:])

	if *server == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "join requires --server and --token")
		os.Exit(2)
	}
	path := *cfgPath
	if path == "" {
		var err error
		path, err = agent.DefaultPath()
		if err != nil {
			fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := agent.Enroll(ctx, agent.EnrollOptions{
		Server: *server, Token: *token, Name: *name,
		CAFinger: *fingerprint, Wait: *wait, Stderr: os.Stderr,
	})
	if err != nil {
		fatal(err)
	}
	res.Config.OllamaURL = *ollama
	if err := res.Config.Save(path); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "enrolled as node %s — credentials in %s\n", res.Config.NodeID, path)

	if *foreground {
		ag := agent.New(res.Config, log)
		if err := ag.Run(ctx); err != nil {
			fatal(err)
		}
	}
}

func cmdRun(log *slog.Logger) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default ~/.localmesh/agent.json)")
	fs.Parse(os.Args[2:])

	path := *cfgPath
	if path == "" {
		var err error
		path, err = agent.DefaultPath()
		if err != nil {
			fatal(err)
		}
	}
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot load config %s: %v\nRun 'localmesh-agent join' first.\n", path, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// When launched by the Windows SCM this hands the process to the
	// service control loop instead of running interactively.
	handled, err := agent.RunUnderService(ctx, cfg, log)
	if err != nil {
		fatal(err)
	}
	if handled {
		return
	}
	if err := agent.New(cfg, log).Run(ctx); err != nil {
		fatal(err)
	}
}

func cmdDoctor() int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default ~/.localmesh/agent.json)")
	caFile := fs.String("ca-file", "", "pinned CA PEM file (overrides the config's embedded CA)")
	fs.Parse(os.Args[2:])

	path := *cfgPath
	if path == "" {
		var err error
		path, err = agent.DefaultPath()
		if err != nil {
			fatal(err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agent.Doctor(ctx, path, *caFile, os.Stdout)
}

func cmdService() {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default ~/.localmesh/agent.json)")
	fs.Parse(os.Args[2:])
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: localmesh-agent service install|uninstall|start|stop|status [-config path]")
		os.Exit(2)
	}
	path := *cfgPath
	if path == "" {
		var err error
		path, err = agent.DefaultPath()
		if err != nil {
			fatal(err)
		}
	}
	if err := agent.Service(fs.Arg(0), path, os.Stdout); err != nil {
		fatal(err)
	}
}

func cmdStatus() {
	path, _ := agent.DefaultPath()
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		fmt.Println("not enrolled (no config)")
		return
	}
	fmt.Printf("server:  %s\nnode id: %s\nname:    %s\nconfig:  %s\n",
		cfg.ServerURL, cfg.NodeID, cfg.Name, path)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
