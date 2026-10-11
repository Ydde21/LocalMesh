package updateplane

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/localmesh/localmesh/internal/release"
	"github.com/localmesh/localmesh/internal/server/store"
)

// ---------------------------------------------------------------------------
// Release registry — all signature/trust logic lives HERE; the store is
// dumb persistence (a release row can only reference a live release_keys
// key_id via FK, and registration never writes a row it didn't verify).
//
// Trust model recap: RegisterRelease verifies the detached signature over
// BOTH the manifest and SHA256SUMS bytes against a pre-registered
// release_keys public key, cross-checks every manifest artifact digest
// against the sums file, and derives the dispatch metadata (platforms,
// protocol bounds, signer key id) the wire frame later carries.
// ---------------------------------------------------------------------------

// RegisterInput is everything registration needs. Signatures maps the
// signature FILE names as shipped in the release directory — e.g.
// "SHA256SUMS.minisig" / "release-manifest.json.minisig" (minisign) or
// ".sig" (openssl) — to the signature file's verbatim contents.
type RegisterInput struct {
	ManifestJSON []byte            // verbatim release-manifest.json
	SumsJSON     []byte            // verbatim SHA256SUMS
	Signatures   map[string]string // "<file>.minisig"|"<file>.sig" → contents
	BaseURL      string            // https:// artifact host; "" = control-plane proxy
	Channel      string            // default "stable"
	Notes        string
	CreatedBy    string
}

// RegisterRelease validates and registers a signed release. Unsigned
// manifests are refused (ErrUnsignedRelease → 400); unknown/revoked
// signer keys, bad signatures, manifest↔sums digest disagreements,
// missing agent artifacts, and non-https/non-loopback base URLs are
// refused as validation errors (→ 400). store.ErrDuplicateRelease → 409.
func (m *Manager) RegisterRelease(ctx context.Context, in RegisterInput) (*store.Release, error) {
	man, err := release.ParseManifest(in.ManifestJSON)
	if err != nil {
		return nil, err
	}
	if man.Signer.Algorithm == "unsigned" {
		return nil, ErrUnsignedRelease
	}
	// The signing key must be registered and unrevoked — the registry
	// never trusts a key carried inside the release itself.
	key, err := m.st.GetReleaseKey(ctx, man.Signer.KeyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("signing key %q is not registered or is revoked: %w",
				man.Signer.KeyID, err)
		}
		return nil, err
	}
	sums, err := release.ParseSums(in.SumsJSON)
	if err != nil {
		return nil, err
	}
	// Every manifest artifact must appear in SHA256SUMS with the same
	// digest — a manifest that disagrees with its sums file is tampered
	// or truncated either way.
	for _, a := range man.Artifacts {
		want, ok := sums[a.File]
		if !ok {
			return nil, fmt.Errorf("manifest artifact %s missing from %s", a.File, release.SumsFile)
		}
		if want != a.SHA256 {
			return nil, fmt.Errorf("%s: digest for %s disagrees with the manifest", release.SumsFile, a.File)
		}
	}
	// Require at least one agent platform artifact and derive the
	// platforms column ("agent/<os>/<arch>", "server/<os>/<arch>").
	platforms := []string{}
	hasAgent := false
	for _, a := range man.Artifacts {
		var kind string
		switch {
		case strings.HasPrefix(a.File, release.AgentPrefix):
			kind, hasAgent = "agent", true
		case strings.HasPrefix(a.File, release.ServerPrefix):
			kind = "server"
		default:
			continue
		}
		platforms = append(platforms, kind+"/"+a.OS+"/"+a.Arch)
	}
	if !hasAgent {
		return nil, fmt.Errorf("%w: release carries no %s* artifact", ErrInvalidRollout, release.AgentPrefix)
	}
	// Signature format follows the declared signer algorithm; both the
	// sums file and the manifest must verify against the stored key.
	var ext string
	switch man.Signer.Algorithm {
	case "minisign":
		ext = release.MinisignSigExt
	case "openssl":
		ext = release.OpenSSLSigExt
	default:
		return nil, fmt.Errorf("unsupported signer algorithm %q", man.Signer.Algorithm)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{release.SumsFile, in.SumsJSON},
		{release.ManifestFile, in.ManifestJSON},
	} {
		sig, ok := in.Signatures[f.name+ext]
		if !ok {
			return nil, fmt.Errorf("missing signature %s%s", f.name, ext)
		}
		if err := release.VerifySignature([]byte(key.PublicKey), f.data, []byte(sig)); err != nil {
			return nil, fmt.Errorf("%s%s: %w", f.name, ext, err)
		}
	}
	if err := validateBaseURL(in.BaseURL); err != nil {
		return nil, err
	}
	channel := in.Channel
	if channel == "" {
		channel = "stable"
	}
	rec := &store.Release{
		Version:          man.Version,
		Channel:          channel,
		BaseURL:          in.BaseURL,
		Manifest:         json.RawMessage(in.ManifestJSON),
		ManifestSHA256:   release.SumBytes(in.ManifestJSON),
		SumsSHA256:       release.SumBytes(in.SumsJSON),
		Signatures:       in.Signatures,
		SignerKeyID:      man.Signer.KeyID,
		ProtocolVersion:  man.ProtocolVersion,
		MinAgentProtocol: man.MinProtocolVersion,
		Platforms:        platforms,
		Notes:            in.Notes,
		CreatedBy:        in.CreatedBy,
	}
	out, err := m.st.RegisterRelease(ctx, rec)
	if err != nil {
		return nil, err
	}
	m.emit("release", map[string]any{"kind": "release", "release": out})
	return out, nil
}

// validateBaseURL enforces the release-source policy: https:// always;
// http:// only when the host is loopback (local mirrors); empty means
// "serve through the control-plane proxy" (localmesh://releases/<id>).
func validateBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid base_url %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if u.Hostname() == "localhost" {
			return nil
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("base_url %q: http is only allowed for loopback mirrors", raw)
	default:
		return fmt.Errorf("base_url %q: scheme must be https (http only on loopback)", raw)
	}
}

// ---------------------------------------------------------------------------
// release keys
// ---------------------------------------------------------------------------

// RegisterKey validates a public key file (minisign .pub or PEM), derives
// the signer key_id the same way scripts/release.sh does — the minisign
// keynum as le64 %016X (the id `minisign` prints) or
// "sha256:<32 hex of DER-fingerprint>" — and
// stores it. store.ErrDuplicateKey → 409.
func (m *Manager) RegisterKey(ctx context.Context, pub []byte, comment string) (*store.ReleaseKey, error) {
	pk, err := release.ParsePublicKey(pub)
	if err != nil {
		return nil, err
	}
	var keyID, kind string
	if pk.Kind == "minisign" {
		keyID = pk.Minisign.KeyIDHex
		kind = "minisign"
	} else {
		der, err := x509.MarshalPKIXPublicKey(pk.Key)
		if err != nil {
			return nil, fmt.Errorf("cannot derive key id: %w", err)
		}
		sum := sha256.Sum256(der)
		keyID = "sha256:" + hex.EncodeToString(sum[:])[:32]
		kind = "openssl"
	}
	k, err := m.st.AddReleaseKey(ctx, keyID, kind, string(pub), comment)
	if err != nil {
		return nil, err
	}
	m.emit("release", map[string]any{"kind": "release_key", "release_key": k})
	return k, nil
}

// ListReleaseKeys returns all signing keys (including revoked — the
// admin view needs the audit trail).
func (m *Manager) ListReleaseKeys(ctx context.Context) ([]*store.ReleaseKey, error) {
	return m.st.ListReleaseKeys(ctx)
}

// RevokeReleaseKey retires a key: already-registered releases stay valid
// (they were verified at registration) but nothing new can be signed by
// it. store.ErrNotFound → 404.
func (m *Manager) RevokeReleaseKey(ctx context.Context, keyID string) error {
	if err := m.st.RevokeReleaseKey(ctx, keyID); err != nil {
		return err
	}
	m.emit("release", map[string]any{"kind": "release_key_revoked", "key_id": keyID})
	return nil
}

// ---------------------------------------------------------------------------
// releases (read/admin pass-throughs for the API layer)
// ---------------------------------------------------------------------------

// ListReleases returns all registered releases, newest first.
func (m *Manager) ListReleases(ctx context.Context) ([]*store.Release, error) {
	return m.st.ListReleases(ctx)
}

// GetRelease returns one release row.
func (m *Manager) GetRelease(ctx context.Context, id string) (*store.Release, error) {
	return m.st.GetRelease(ctx, id)
}

// RevokeRelease marks a release unusable for NEW rollouts; in-flight
// updates already dispatched keep their verified payloads.
// store.ErrNotFound → 404.
func (m *Manager) RevokeRelease(ctx context.Context, id string) error {
	rel, err := m.st.GetRelease(ctx, id)
	if err != nil {
		return err
	}
	if err := m.st.RevokeRelease(ctx, id); err != nil {
		return err
	}
	rel.Status = "revoked"
	m.emit("release", map[string]any{"kind": "release", "release": rel})
	return nil
}
