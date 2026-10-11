// Command gen-manifest writes release-manifest.json for a dist
// directory of release artifacts. scripts/release.sh invokes it via
// `go run ./internal/release/cmd/gen-manifest`.
//
// Protocol versions default to the wire constants compiled in from
// internal/contracts (read, never modified) so the manifest always
// matches what the built binaries actually speak.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/release"
)

func main() {
	dist := flag.String("dist", "", "release directory to scan")
	version := flag.String("version", "", "release version (semver, no leading v)")
	signer := flag.String("signer", "", "signer algorithm: minisign|openssl|unsigned")
	keyID := flag.String("signer-key-id", "", "signer key id (minisign keynum hex / pubkey fingerprint)")
	dev := flag.Bool("dev", false, "mark the signer as a dev identity (release.sh --dev)")
	proto := flag.Int("protocol", contracts.ProtocolVersion, "wire protocol the artifacts speak")
	minProto := flag.Int("min-protocol", contracts.MinAgentProtocolVersion, "oldest agent protocol still accepted")
	minAgent := flag.String("min-agent-version", "", "anti-rollback floor: agents must not roll back below this version once this release is approved")
	out := flag.String("out", "", "output path (default <dist>/release-manifest.json)")
	flag.Parse()

	if *dist == "" || *version == "" || *signer == "" {
		fmt.Fprintln(os.Stderr, "usage: gen-manifest -dist DIR -version V -signer minisign|openssl|unsigned [-signer-key-id ID]")
		os.Exit(2)
	}
	created := time.Now().UTC()
	if epoch := os.Getenv("SOURCE_DATE_EPOCH"); epoch != "" {
		if secs, err := strconv.ParseInt(epoch, 10, 64); err == nil {
			created = time.Unix(secs, 0).UTC()
		}
	}
	m, err := build(*dist, *version, *signer, *keyID, *dev, *proto, *minProto, *minAgent, created)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-manifest:", err)
		os.Exit(1)
	}
	outPath := *out
	if outPath == "" {
		outPath = filepath.Join(*dist, release.ManifestFile)
	}
	data, err := m.Marshal()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-manifest:", err)
		os.Exit(1)
	}
	data = append(data, '\n')
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-manifest:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "gen-manifest: wrote %s (%d artifacts)\n", outPath, len(m.Artifacts))
}

func build(dist, version, signer, keyID string, dev bool, proto, minProto int, minAgent string, created time.Time) (*release.Manifest, error) {
	entries, err := os.ReadDir(dist)
	if err != nil {
		return nil, err
	}
	var arts []release.Artifact
	agentPrefix, serverPrefix := release.AgentPrefix+version+"-", release.ServerPrefix+version+"-"
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == release.SumsFile || name == release.ManifestFile ||
			strings.HasSuffix(name, release.MinisignSigExt) || strings.HasSuffix(name, release.OpenSSLSigExt) {
			continue
		}
		// Catalog only THIS release's artifacts — a shared dist dir can
		// hold stale binaries from other versions that must not enter
		// a signed manifest.
		if !strings.HasPrefix(name, agentPrefix) && !strings.HasPrefix(name, serverPrefix) {
			continue
		}
		if _, _, _, ok := release.ParseArtifactName(name); !ok {
			continue
		}
		full := filepath.Join(dist, name)
		sum, err := release.SumFile(full)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(full)
		if err != nil {
			return nil, err
		}
		_, goos, goarch, _ := release.ParseArtifactName(name)
		arts = append(arts, release.Artifact{
			File: name, OS: goos, Arch: goarch, SHA256: sum, Size: st.Size(),
		})
	}
	if len(arts) == 0 {
		return nil, fmt.Errorf("no localmesh-agent-*/localmesh-server-* artifacts found in %s", dist)
	}
	sort.Slice(arts, func(i, j int) bool { return arts[i].File < arts[j].File })
	m := &release.Manifest{
		Version:            version,
		ProtocolVersion:    proto,
		MinProtocolVersion: minProto,
		MinAgentVersion:    minAgent,
		CreatedAt:          created,
		Signer:             release.Signer{Algorithm: signer, KeyID: keyID, Dev: dev},
		Artifacts:          arts,
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}
