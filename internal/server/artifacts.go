package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/localmesh/localmesh/internal/release"
	"github.com/localmesh/localmesh/internal/server/store"
)

// ---------------------------------------------------------------------------
// Release artifact store — the localmesh:// proxy backend.
//
// Release rows carry verified metadata (manifest, sums, signatures); the
// BINARY payloads live on the control-plane filesystem under
// <DataDir>/releases/<release-id>/<filename>. Admins upload artifacts
// after registration; agents on air-gapped networks fetch them through
// GET /api/v1/agent-releases/<id>/files/<name> with node credentials.
//
// The store is transport, not trust: every Put verifies the bytes'
// sha256 against the release's signed SHA256SUMS before committing, and
// every Open serves only names the signed manifest declares — a
// poisoned upload can't reach an agent, and an agent re-verifies the
// release signature itself regardless.
// ---------------------------------------------------------------------------

type ArtifactStore struct{ root string }

// NewArtifactStore roots the store at <dataDir>/releases.
func NewArtifactStore(dataDir string) *ArtifactStore {
	return &ArtifactStore{root: filepath.Join(dataDir, "releases")}
}

// validReleaseFileName allows single path segments only — the same rule
// the agent's proxySource enforces, applied symmetrically here.
func validReleaseFileName(name string) bool {
	return name != "" && name != ".." && !strings.ContainsAny(name, `/\`)
}

// Put validates name against the release's signed manifest, streams the
// body to a temp file, verifies sha256 against the manifest artifact
// digest (itself cross-checked against signed SHA256SUMS at
// registration), then renames into place. Returns the stored
// digest+size.
func (as *ArtifactStore) Put(ctx context.Context, rel *store.Release, name string, r io.Reader) (shaHex string, size int64, err error) {
	if !validReleaseFileName(name) {
		return "", 0, fmt.Errorf("bad artifact name %q", name)
	}
	wantSum := ""
	man, err := release.ParseManifest(rel.Manifest)
	if err != nil {
		return "", 0, fmt.Errorf("stored manifest corrupt: %w", err)
	}
	for _, a := range man.Artifacts {
		if a.File == name {
			wantSum = a.SHA256
			break
		}
	}
	if wantSum == "" {
		return "", 0, fmt.Errorf("%s is not a manifest artifact of release %s", name, rel.Version)
	}

	dir := filepath.Join(as.root, rel.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after rename

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, 5<<30)) // 5GiB sanity cap
	if err != nil {
		tmp.Close()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSum != "" && sum != wantSum {
		return "", 0, fmt.Errorf("%s sha256 mismatch: got %s, manifest declares %s", name, sum[:16], wantSum[:16])
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return "", 0, err
	}
	return sum, n, nil
}

// PutBytes stores the release metadata files verbatim (manifest, sums,
// signature files) — written at registration time after verification;
// the agent re-verifies signatures on fetch anyway.
func (as *ArtifactStore) PutBytes(rel *store.Release, name string, data []byte) error {
	if !validReleaseFileName(name) {
		return fmt.Errorf("bad artifact name %q", name)
	}
	dir := filepath.Join(as.root, rel.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// Open serves a stored file. The caller enforces manifest membership —
// this layer only guards path traversal.
func (as *ArtifactStore) Open(rel *store.Release, name string) (io.ReadSeekCloser, int64, error) {
	if !validReleaseFileName(name) {
		return nil, 0, fs.ErrNotExist
	}
	f, err := os.Open(filepath.Join(as.root, rel.ID, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, fs.ErrNotExist
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}
