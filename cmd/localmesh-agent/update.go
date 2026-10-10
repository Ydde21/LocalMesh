package main

import (
	"flag"
	"fmt"
	"os"

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
