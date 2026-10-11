// Command localmesh-agent runs the LocalMesh node agent.
//
// Usage:
//
//	localmesh-agent join --server https://host:8443 --token lme_... [--name ...] [--ca-fingerprint sha256:...] [--update-pubkey keyfile] [-force]
//	localmesh-agent join -discover --token lme_... [-discover-instance name] [--ca-fingerprint sha256:...]
//	localmesh-agent discover [-timeout 5s] [-json]
//	localmesh-agent run [--config path]
//	localmesh-agent doctor [--config path] [--ca-file ca.pem]
//	localmesh-agent service install|uninstall|start|stop|status [--config path]
//	localmesh-agent status
//	localmesh-agent update -release <dir-or-url> -pubkey <keyfile> [--apply]
//	localmesh-agent update-key [--config path] <keyfile>
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
	case "report":
		os.Exit(cmdReport())
	case "service":
		cmdService()
	case "status":
		cmdStatus()
	case "update":
		cmdUpdate()
	case "update-key":
		cmdUpdateKey()
	case "update-apply":
		// Hidden: the self-update restart helper spawned by the agent
		// itself after a server-dispatched upgrade. Not for operators.
		cmdUpdateApply()
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
  report    redacted machine-shareable diagnostic bundle (-json, -self-test)
  service   install|uninstall|start|stop|status the OS service
  status    show local agent configuration
  update    verify and install a signed agent release (opt-in, dry-run default)
  update-key  pin the release signing key for server-dispatched upgrades

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
	updatePubkey := fs.String("update-pubkey", "", "release signing public key file (minisign .pub or PEM) — pins the trust anchor for server-dispatched upgrades; the control plane NEVER sends key material")
	foreground := fs.Bool("run", true, "start serving workloads after approval")
	force := fs.Bool("force", false, "re-enroll and replace existing node credentials (requires new admin approval)")
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

	// Re-running join mints a fresh node credential and silently
	// replaces the persisted one on approval — the old node goes
	// offline forever while a new pending request queues for the
	// admin. Refuse on an enrolled config unless the operator asks.
	if existing, err := agent.LoadConfig(path); err == nil && existing.NodeID != "" && !*force {
		fmt.Fprintf(os.Stderr, "error: already enrolled as node %s (server %s, config %s)\nre-running join replaces those credentials and requires new admin approval — re-run with -force to proceed, or delete %s\n",
			existing.NodeID, existing.ServerURL, path, path)
		os.Exit(1)
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
	if *updatePubkey != "" {
		keyID, err := pinUpdateKeyFile(res.Config, *updatePubkey)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stderr, "release update key pinned: %s\n", keyID)
	}
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
		ag.SetConfigPath(path)
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
	handled, err := agent.RunUnderService(ctx, cfg, log, path)
	if err != nil {
		fatal(err)
	}
	if handled {
		return
	}
	ag := agent.New(cfg, log)
	ag.SetConfigPath(path)
	if err := ag.Run(ctx); err != nil {
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

// cmdReport emits the redacted fleet-validation diagnostic bundle —
// safe to paste into a bug report or validation runbook. It never
// prints credentials, tokens, prompts, or completions.
func cmdReport() int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default ~/.localmesh/agent.json)")
	caFile := fs.String("ca-file", "", "pinned CA PEM file (overrides the config's embedded CA)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	selfTest := fs.Bool("self-test", false, "run one bounded inference through the local engine")
	fs.Parse(os.Args[2:])

	path := *cfgPath
	if path == "" {
		var err error
		if path, err = agent.DefaultPath(); err != nil {
			fatal(err)
		}
	}
	cfg, _ := agent.LoadConfig(path) // nil is fine — the report records "not enrolled"
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.WriteFleetReport(ctx, os.Stdout, cfg, agent.FleetReportOpts{
		ConfigPath: path, CAFile: *caFile, JSON: *asJSON, SelfTest: *selfTest,
	}); err != nil {
		fatal(err)
	}
	return 0 // findings are not exit codes
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
	path, err := agent.DefaultPath()
	if err != nil {
		fatal(err)
	}
	cfg, err := agent.LoadConfig(path)
	switch {
	case err == nil:
		fmt.Printf("server:  %s\nnode id: %s\nname:    %s\nconfig:  %s\n",
			cfg.ServerURL, cfg.NodeID, cfg.Name, path)
		if s := agent.PendingUpdateSummary(path); s != "" {
			fmt.Printf("update:  %s\n", s)
		}
		if s := agent.UpdateHistorySummary(path); s != "" {
			fmt.Printf("update:  %s\n", s)
		}
	case os.IsNotExist(err):
		fmt.Printf("not enrolled — no config at %s (run 'localmesh-agent join' to enroll this machine)\n", path)
	default:
		// A config that exists but won't parse is a broken
		// enrollment, not a fresh machine — say so.
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
