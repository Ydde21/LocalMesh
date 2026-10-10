// Command localmesh-agent runs the LocalMesh node agent.
//
// Usage:
//
//	localmesh-agent join --server https://host:8443 --token lme_... [--name ...] [--ca-fingerprint sha256:...]
//	localmesh-agent join -discover --token lme_... [-discover-instance name] [--ca-fingerprint sha256:...]
//	localmesh-agent discover [-timeout 5s] [-json]
//	localmesh-agent run [--config path]
//	localmesh-agent doctor [--config path] [--ca-file ca.pem]
//	localmesh-agent service install|uninstall|start|stop|status [--config path]
//	localmesh-agent status
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/agent/hardware"
	"github.com/localmesh/localmesh/internal/discovery"
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
	case "discover":
		cmdDiscover()
	case "run":
		cmdRun(log)
	case "doctor":
		os.Exit(cmdDoctor())
	case "service":
		cmdService()
	case "status":
		cmdStatus()
	case "version":
		fmt.Println(agent.Current())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `localmesh-agent — LocalMesh node agent

Commands:
  join      enroll this machine with a control plane
  discover  find LocalMesh clusters on this network (mDNS)
  run       connect and serve workloads (default when configured)
  doctor    check connectivity, credentials, engine, and telemetry
  service   install|uninstall|start|stop|status the OS service
  status    show local agent configuration

Run "localmesh-agent join -h" or "localmesh-agent discover -h" for options.
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
	discover := fs.Bool("discover", false, "find the control plane via mDNS instead of --server")
	discInstance := fs.String("discover-instance", "", "cluster name to pick when several are discovered")
	discTimeout := fs.Duration("discover-timeout", 5*time.Second, "mDNS browse window for -discover")
	fs.Parse(os.Args[2:])

	switch {
	case *discover && *server != "":
		fmt.Fprintln(os.Stderr, "use either --server or --discover, not both")
		os.Exit(2)
	case !*discover && *server == "":
		fmt.Fprintln(os.Stderr, "join requires --server (or --discover)")
		os.Exit(2)
	case *token == "":
		fmt.Fprintln(os.Stderr, "join requires --token")
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

	// Discovery supplies only the server URL and a verified CA
	// fingerprint — the enrollment token and admin approval are
	// unchanged, so discovery never authorizes a device.
	serverURL := *server
	caFinger := *fingerprint
	var ds *agent.DiscoveredServer
	if *discover {
		var err error
		ds, err = agent.DiscoverServer(ctx, agent.DiscoverJoinOptions{
			Timeout: *discTimeout, Instance: *discInstance,
			CAFinger: caFinger, Stderr: os.Stderr,
		})
		if err != nil {
			fatal(err)
		}
		serverURL = ds.Server
		caFinger = ds.CAFinger
	}

	res, err := agent.Enroll(ctx, agent.EnrollOptions{
		Server: serverURL, Token: *token, Name: *name,
		CAFinger: caFinger, Wait: *wait, Stderr: os.Stderr,
	})
	if err != nil {
		fatal(err)
	}
	res.Config.OllamaURL = *ollama
	if ds != nil {
		res.Config.DiscoveryEnabled = true
		res.Config.DiscoveredCluster = ds.Cluster.Instance
		res.Config.DiscoveredAt = time.Now().UTC()
	}
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

func cmdDiscover() {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	timeout := fs.Duration("timeout", discovery.DefaultBrowseTimeout, "mDNS browse window")
	asJSON := fs.Bool("json", false, "emit the raw cluster list as JSON for scripting")
	fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	clusters, err := agent.DiscoverServers(ctx, *timeout)
	if err != nil {
		fatal(err)
	}
	if *asJSON {
		if clusters == nil {
			clusters = []discovery.Cluster{}
		}
		json.NewEncoder(os.Stdout).Encode(clusters)
		return
	}
	// Empty is not an error: mDNS is link-local multicast and does not
	// cross routed networks, VLANs, or VPNs — exit 0 with guidance.
	if len(clusters) == 0 {
		fmt.Println("no LocalMesh clusters found on this network — use `join --server <url>` for manual enrollment")
		fmt.Println("note: discovery works only on the local network segment (not across routers, VLANs, or VPNs)")
		return
	}
	fmt.Printf("%-24s  %-30s  %s\n", "NAME", "HOST:PORT", "CA FINGERPRINT")
	for _, c := range clusters {
		fp := c.CAFingerprint
		if fp == "" {
			fp = "(none advertised — join needs --ca-fingerprint)"
		} else if len(fp) > 23 {
			fp = fp[:23] + "…"
		}
		fmt.Printf("%-24s  %-30s  %s\n", c.Instance, c.Endpoint(), fp)
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
