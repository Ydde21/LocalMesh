// Command verify-release performs every release-directory check
// natively in Go: signature over SHA256SUMS and the manifest, artifact
// checksums, manifest schema, manifest<->sums agreement, and the
// expected platform list. scripts/verify-release.sh shells out to it so
// verification works even without the minisign/openssl binaries.
//
// Exit code: 0 when every check passes (warnings allowed), 1 on any
// failure, 2 on usage errors.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/localmesh/localmesh/internal/release"
)

func main() {
	dir := flag.String("dir", "", "release directory to verify")
	pub := flag.String("pubkey", "", "public key file (minisign .pub or PEM)")
	allowUnsigned := flag.Bool("allow-unsigned", false, "permit missing signatures (checksums and schema only)")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: verify-release -dir DIST -pubkey PUBKEYFILE [--allow-unsigned]")
		os.Exit(2)
	}
	var pubData []byte
	if *pub != "" {
		var err error
		pubData, err = os.ReadFile(*pub)
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify-release: cannot read pubkey: %v\n", err)
			os.Exit(2)
		}
		if _, err := release.ParsePublicKey(pubData); err != nil {
			fmt.Fprintf(os.Stderr, "verify-release: %v\n", err)
			os.Exit(2)
		}
	}
	abs, _ := filepath.Abs(*dir)
	fmt.Printf("verifying release in %s\n", abs)
	rep, err := release.VerifyRelease(*dir, release.VerifyOptions{
		PubKey:           pubData,
		AllowUnsigned:    *allowUnsigned,
		RequirePlatforms: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-release:", err)
		os.Exit(1)
	}
	for _, c := range rep.Checks {
		fmt.Printf("%-4s  %-14s %s\n", c.Status, c.Name, c.Detail)
	}
	if !rep.OK() {
		printRemediations(rep)
		fmt.Println("verify-release: FAIL")
		os.Exit(1)
	}
	fmt.Println("verify-release: PASS")
}

// remediation maps a failed check name to the operator's next step.
// Keep each hint to one line — verify-release output is runbook input.
var remediation = map[string]string{
	"sums":          "release directory is incomplete — re-fetch SHA256SUMS from the release source (do not hand-edit it)",
	"manifest":      "re-fetch release-manifest.json; a corrupt or missing manifest means nothing else can be trusted",
	"signature":     "confirm --pubkey is the REAL release public key (compare its key id to the manifest's signer.key_id), then re-download the file + its .minisig/.sig together — a persistent failure means tampering or the wrong key",
	"checksums":     "re-download the named artifact(s); a checksum that still fails is a tampered or truncated download — do not install, report it",
	"manifest-sums": "the manifest and SHA256SUMS disagree — the release was repackaged inconsistently; do not install, report to the maintainers",
	"platforms":     "this release ships no artifact for every expected platform — check the manifest's artifacts list for your os/arch, or wait for a complete release",
	"signer":        "manifest signer metadata disagrees with the signature files — do not install, report to the maintainers",
}

func printRemediations(rep *release.Report) {
	seen := map[string]bool{}
	for _, c := range rep.Checks {
		if c.Status != "FAIL" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		if hint, ok := remediation[c.Name]; ok {
			fmt.Printf("  → %s: %s\n", c.Name, hint)
		}
	}
	if rep.Manifest != nil && rep.Manifest.Signer.KeyID != "" {
		fmt.Printf("  note: release declares signer.key_id %s (algorithm %s%s) — your --pubkey must resolve to the same id\n",
			rep.Manifest.Signer.KeyID, rep.Manifest.Signer.Algorithm,
			map[bool]string{true: ", DEV-signed", false: ""}[rep.Manifest.Signer.Dev])
	}
}
