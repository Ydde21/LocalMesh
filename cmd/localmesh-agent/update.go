package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/agent/update"
)

// cmdUpdate implements `localmesh-agent update`: download/read a signed
// release, verify it end-to-end, and — only with --apply — atomically
// replace this binary. Dry-run is the default; nothing is fetched or
// touched beyond the release metadata without --apply.
func cmdUpdate() {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	releaseDir := fs.String("release", "", "release directory or https:// base URL containing release-manifest.json + SHA256SUMS")
	pubkey := fs.String("pubkey", "", "release public key file (minisign .pub or PEM) — required; never taken from the release itself")
	target := fs.String("target", "", "binary to replace (default: the running executable)")
	apply := fs.Bool("apply", false, "perform the swap (default: verify + dry-run only)")
	force := fs.Bool("force", false, "allow a release whose protocol exceeds this build (control plane must already be newer)")
	fs.Parse(os.Args[2:])

	if *releaseDir == "" || *pubkey == "" {
		fmt.Fprintln(os.Stderr, "usage: localmesh-agent update -release <dir-or-https-url> -pubkey <keyfile> [--apply] [-target path] [-force]")
		os.Exit(2)
	}
	pubData, err := os.ReadFile(*pubkey)
	if err != nil {
		fatal(fmt.Errorf("cannot read -pubkey: %w", err))
	}
	if err := update.Run(update.Options{
		Release: *releaseDir,
		PubKey:  pubData,
		Target:  *target,
		Apply:   *apply,
		Force:   *force,
	}); err != nil {
		fatal(err)
	}
}

// pinUpdateKeyFile reads a release public key file and pins it into
// cfg — shared by `join -update-pubkey` and `update-key`.
func pinUpdateKeyFile(cfg *agent.Config, keyfile string) (keyID string, err error) {
	pubData, err := os.ReadFile(keyfile)
	if err != nil {
		return "", fmt.Errorf("cannot read update pubkey %s: %w", keyfile, err)
	}
	keyID, err = agent.PinUpdateKey(cfg, pubData)
	if err != nil {
		return "", fmt.Errorf("%s is not a release public key (minisign .pub or PEM): %w", keyfile, err)
	}
	return keyID, nil
}

// cmdUpdateKey implements `localmesh-agent update-key <keyfile>`:
// provision the release signing key into agent.json. This key is the
// trust anchor for server-dispatched upgrades — the control plane
// never sends key material, so it must be provisioned locally.
func cmdUpdateKey() {
	fs := flag.NewFlagSet("update-key", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default ~/.localmesh/agent.json)")
	fs.Parse(os.Args[2:])
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: localmesh-agent update-key [-config path] <pubkey-file>")
		fmt.Fprintln(os.Stderr, "  pins the release signing key (minisign .pub or PEM) used to verify")
		fmt.Fprintln(os.Stderr, "  server-dispatched agent upgrades — the trust anchor the wire can")
		fmt.Fprintln(os.Stderr, "  never supply. Print the key id for the server's release policy.")
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
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot load config %s: %v\nRun 'localmesh-agent join' first (or pass -update-pubkey to join).\n", path, err)
		os.Exit(1)
	}
	keyID, err := pinUpdateKeyFile(cfg, fs.Arg(0))
	if err != nil {
		fatal(err)
	}
	if err := cfg.Save(path); err != nil {
		fatal(err)
	}
	fmt.Printf("update key pinned: %s\n", keyID)
	fmt.Printf("server-dispatched upgrades will require releases signed by this key (config %s)\n", path)
}

// cmdUpdateApply implements the hidden `update-apply` helper spawned
// by the agent itself when a running binary cannot replace itself
// (Windows) or when a rollback must happen after the process exits.
// It waits for the parent pid to die, performs the swap, and restarts
// the agent — nothing else. Not an operator command.
func cmdUpdateApply() {
	fs := flag.NewFlagSet("update-apply", flag.ExitOnError)
	target := fs.String("target", "", "binary to replace (or restore over, with -rollback)")
	staged := fs.String("staged", "", "verified <target>.new to install")
	pid := fs.Int("pid", 0, "parent pid to await before swapping")
	service := fs.Bool("service", false, "restart via the OS service manager afterwards")
	relaunch := fs.Bool("relaunch", false, "restart via detached `<target> run` afterwards")
	rollback := fs.Bool("rollback", false, "restore <target>.bak instead of installing -staged")
	cfgPath := fs.String("config", "", "config path forwarded to the relaunched `run`")
	wait := fs.Duration("wait", 60*time.Second, "bound on the pid wait + swap retries")
	fs.Parse(os.Args[2:])
	if err := agent.UpdateApply(context.Background(), agent.UpdateApplyOptions{
		Target:     *target,
		Staged:     *staged,
		ParentPID:  *pid,
		Rollback:   *rollback,
		Service:    *service,
		Relaunch:   *relaunch,
		ConfigPath: *cfgPath,
		WaitBound:  *wait,
		Out:        os.Stderr,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "update-apply:", err)
		os.Exit(1)
	}
}
