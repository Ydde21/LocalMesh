// Package release defines the signed-release manifest, checksum, and
// signature formats produced by scripts/release.sh and consumed by
// scripts/verify-release.sh and `localmesh-agent update`.
//
// Trust model: an update is applied only when the release signature
// verifies against a public key the operator supplies out-of-band —
// never a key shipped inside the release itself. HTTPS transport alone
// is not trusted; the minisign (Ed25519) or openssl signature over
// SHA256SUMS and the manifest is what proves provenance.
//
// This package is deliberately dependency-free (stdlib only) so the
// agent can verify releases on every supported platform without the
// minisign or openssl binaries installed.
package release

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Manifest describes one signed release. It is emitted as
// release-manifest.json by scripts/release.sh (via
// internal/release/cmd/gen-manifest) and signed alongside SHA256SUMS.
//
// Protocol semantics: ProtocolVersion is the wire protocol the binaries
// in this release speak (contracts.ProtocolVersion at build time, baked
// in by gen-manifest — internal/contracts is never parsed). An agent
// speaking agentProto is inside this release's supported window when
// MinProtocolVersion <= agentProto <= ProtocolVersion.
type Manifest struct {
	Version            string `json:"version"`              // semver without leading v, e.g. "0.6.0"
	ProtocolVersion    int    `json:"protocol_version"`     // wire protocol the artifacts speak
	MinProtocolVersion int    `json:"min_protocol_version"` // oldest agent proto still accepted
	// MinAgentVersion is the anti-rollback floor declared by this
	// release: once it is installed or targeted, the fleet may not
	// roll back to an agent older than this version. Optional —
	// absent means the release ratchets no floor. Set it (via
	// gen-manifest -min-agent-version / LOCALMESH_MIN_AGENT_VERSION)
	// when a release closes an agent-side vulnerability or a wire
	// behaviour older builds must not resume.
	MinAgentVersion string     `json:"min_agent_version,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	Signer          Signer     `json:"signer"`
	Artifacts       []Artifact `json:"artifacts"`
}

// Signer identifies who signed the release. KeyID is the minisign
// keynum as le64 %016X (the id `minisign` prints) or the SHA-256
// fingerprint of the PEM public key. It is
// informational only — verification always uses the operator-supplied
// public key, never a key carried inside the release.
type Signer struct {
	Algorithm string `json:"algorithm"` // "minisign", "openssl", or "unsigned"
	KeyID     string `json:"key_id,omitempty"`
	// Dev marks a release signed with a development key
	// (scripts/release.sh --dev). Verifiers warn on it; operators must
	// never pin a dev key id as a fleet trust anchor.
	Dev bool `json:"dev,omitempty"`
}

// Artifact is one downloadable binary in the release.
type Artifact struct {
	File   string `json:"file"`   // basename inside the release directory
	OS     string `json:"os"`     // darwin, linux, windows
	Arch   string `json:"arch"`   // amd64, arm64
	SHA256 string `json:"sha256"` // lowercase hex
	Size   int64  `json:"size"`   // bytes
}

// File names every signed release directory carries.
const (
	SumsFile       = "SHA256SUMS"
	ManifestFile   = "release-manifest.json"
	MinisignSigExt = ".minisig" // appended to the signed file's name
	OpenSSLSigExt  = ".sig"     // appended to the signed file's name
	AgentPrefix    = "localmesh-agent-"
	ServerPrefix   = "localmesh-server-"
)

// ExpectedAgentPlatforms is the platform list a complete release must
// ship; verify-release fails when any is missing.
var ExpectedAgentPlatforms = [][2]string{
	{"darwin", "arm64"},
	{"darwin", "amd64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"windows", "amd64"},
}

var (
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	validOS   = map[string]bool{"darwin": true, "linux": true, "windows": true}
	validArch = map[string]bool{"amd64": true, "arm64": true}
)

// ParseManifest decodes and validates a release-manifest.json document.
// Unknown fields are tolerated (forward compatibility) but every
// required field is strictly checked.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid manifest JSON: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks all required fields and internal consistency.
func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("manifest: missing version")
	}
	if m.ProtocolVersion < 1 {
		return fmt.Errorf("manifest: protocol_version must be >= 1 (got %d)", m.ProtocolVersion)
	}
	if m.MinProtocolVersion < 1 {
		return fmt.Errorf("manifest: min_protocol_version must be >= 1 (got %d)", m.MinProtocolVersion)
	}
	if m.MinProtocolVersion > m.ProtocolVersion {
		return fmt.Errorf("manifest: min_protocol_version %d exceeds protocol_version %d — corrupt manifest",
			m.MinProtocolVersion, m.ProtocolVersion)
	}
	if m.MinAgentVersion != "" {
		// The floor is security-relevant: it must parse and can never
		// exceed the release's own version (a release cannot demand
		// the fleet already be newer than itself).
		if _, err := parseVersion(m.MinAgentVersion); err != nil {
			return fmt.Errorf("manifest: min_agent_version %q unparseable: %w", m.MinAgentVersion, err)
		}
		if c, err := CompareVersions(m.MinAgentVersion, m.Version); err != nil {
			return fmt.Errorf("manifest: version %q unparseable: %w", m.Version, err)
		} else if c > 0 {
			return fmt.Errorf("manifest: min_agent_version %s exceeds version %s — a release cannot floor above itself",
				m.MinAgentVersion, m.Version)
		}
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("manifest: missing created_at")
	}
	switch m.Signer.Algorithm {
	case "minisign", "openssl", "unsigned":
	default:
		return fmt.Errorf("manifest: unknown signer algorithm %q", m.Signer.Algorithm)
	}
	if len(m.Artifacts) == 0 {
		return fmt.Errorf("manifest: no artifacts")
	}
	seen := make(map[string]bool)
	for i, a := range m.Artifacts {
		where := fmt.Sprintf("manifest: artifact %d (%q)", i, a.File)
		if a.File == "" || strings.ContainsAny(a.File, "/\\") {
			return fmt.Errorf("%s: bad file name", where)
		}
		if !validOS[a.OS] {
			return fmt.Errorf("%s: unsupported os %q", where, a.OS)
		}
		if !validArch[a.Arch] {
			return fmt.Errorf("%s: unsupported arch %q", where, a.Arch)
		}
		if !sha256Re.MatchString(a.SHA256) {
			return fmt.Errorf("%s: sha256 must be 64 lowercase hex chars", where)
		}
		if a.Size <= 0 {
			return fmt.Errorf("%s: size must be positive", where)
		}
		if seen[a.File] {
			return fmt.Errorf("manifest: duplicate artifact %q", a.File)
		}
		seen[a.File] = true
	}
	return nil
}

// Marshal serializes the manifest in its canonical on-disk form.
func (m *Manifest) Marshal() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// FindArtifact returns the artifact built for the given platform, or an
// error naming the platforms the release actually ships.
func (m *Manifest) FindArtifact(goos, goarch string) (*Artifact, error) {
	for i := range m.Artifacts {
		if m.Artifacts[i].OS == goos && m.Artifacts[i].Arch == goarch &&
			strings.HasPrefix(m.Artifacts[i].File, AgentPrefix) {
			return &m.Artifacts[i], nil
		}
	}
	var have []string
	for _, a := range m.Artifacts {
		if strings.HasPrefix(a.File, AgentPrefix) {
			have = append(have, a.OS+"/"+a.Arch)
		}
	}
	return nil, fmt.Errorf("no agent artifact for %s/%s in this release (ships: %s)",
		goos, goarch, strings.Join(have, ", "))
}

// CompatibleProtocol reports whether an agent speaking agentProto is
// inside this release's declared compatibility window
// [MinProtocolVersion, ProtocolVersion]. The wire rule is that the
// server accepts agents from its minimum up to its own version.
func (m *Manifest) CompatibleProtocol(agentProto int) bool {
	return agentProto >= m.MinProtocolVersion && agentProto <= m.ProtocolVersion
}

// MissingAgentPlatforms lists which of ExpectedAgentPlatforms have no
// artifact in the manifest.
func (m *Manifest) MissingAgentPlatforms() [][2]string {
	var missing [][2]string
	for _, p := range ExpectedAgentPlatforms {
		found := false
		for _, a := range m.Artifacts {
			if a.OS == p[0] && a.Arch == p[1] && strings.HasPrefix(a.File, AgentPrefix) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, p)
		}
	}
	return missing
}

// ArtifactName returns the canonical release artifact name:
// localmesh-agent-0.6.0-linux-amd64 (with .exe on windows).
func ArtifactName(prefix, version, goos, goarch string) string {
	name := prefix + version + "-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// ParseArtifactName recovers (prefix, os, arch) from an artifact file
// name produced by ArtifactName. The version segment may itself contain
// dashes (e.g. 0.0.0-dev+sha) — the last two dash-separated tokens are
// always os and arch.
func ParseArtifactName(file string) (prefix, goos, goarch string, ok bool) {
	for _, p := range []string{AgentPrefix, ServerPrefix} {
		if !strings.HasPrefix(file, p) {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(file, p), ".exe")
		parts := strings.Split(rest, "-")
		if len(parts) < 3 {
			return "", "", "", false
		}
		goos, goarch = parts[len(parts)-2], parts[len(parts)-1]
		if !validOS[goos] || !validArch[goarch] {
			return "", "", "", false
		}
		return p, goos, goarch, true
	}
	return "", "", "", false
}
