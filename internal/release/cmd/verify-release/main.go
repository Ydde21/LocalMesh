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
		fmt.Println("verify-release: FAIL")
		os.Exit(1)
	}
	fmt.Println("verify-release: PASS")
}
