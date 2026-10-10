package update

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A Source abstracts "where a release lives" — a local directory or an
// HTTPS base URL carrying release-manifest.json, SHA256SUMS, their
// signatures, and the artifact binaries.
type Source interface {
	// Read returns a small metadata file (manifest, sums, signature).
	Read(name string) ([]byte, error)
	// Stream writes a potentially large artifact to w.
	Stream(name string, w io.Writer) error
	// Describe renders the origin for operator output.
	Describe() string
}

// errNotExist mirrors fs.ErrNotExist for "the release has no such file".
var errNotExist = fs.ErrNotExist

// Max sizes guard against hostile/truncated responses when the source
// is remote.
const (
	maxMetaBytes     = 8 << 20          // manifest / sums / signatures
	maxArtifactBytes = 4 << 30          // release binaries
	httpTimeout      = 10 * time.Minute // generous: artifacts may be tens of MB on slow links
)

// OpenSource resolves -release: an existing local directory, or an
// https:// base URL. Plain http:// is accepted only for loopback hosts
// (local mirrors/tests) — releases must otherwise travel over TLS, and
// even then the signature, not TLS, is what is trusted.
func OpenSource(release string, client *http.Client) (Source, error) {
	if release == "" {
		return nil, fmt.Errorf("empty -release")
	}
	if strings.HasPrefix(release, "https://") || strings.HasPrefix(release, "http://") {
		u, err := url.Parse(release)
		if err != nil {
			return nil, fmt.Errorf("bad release URL %q: %w", release, err)
		}
		if u.Scheme == "http" && !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("refusing plain-http release URL %q — use https (loopback exempt for local mirrors)", release)
		}
		if client == nil {
			client = &http.Client{Timeout: httpTimeout}
		}
		return &urlSource{base: strings.TrimRight(u.String(), "/"), c: client}, nil
	}
	st, err := os.Stat(release)
	if err != nil {
		return nil, fmt.Errorf("release %q is neither a directory nor an https URL: %w", release, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("release %q is not a directory", release)
	}
	return &dirSource{dir: release}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// --- local directory ------------------------------------------------

type dirSource struct{ dir string }

func (d *dirSource) Read(name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(d.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", name, errNotExist)
	}
	return b, err
}

func (d *dirSource) Stream(name string, w io.Writer) error {
	f, err := os.Open(filepath.Join(d.dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: %w", name, errNotExist)
		}
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (d *dirSource) Describe() string {
	abs, err := filepath.Abs(d.dir)
	if err != nil {
		return d.dir
	}
	return abs
}

// --- HTTPS base URL --------------------------------------------------

type urlSource struct {
	base string
	c    *http.Client
}

func (u *urlSource) get(name string, w io.Writer, limit int64) error {
	if strings.ContainsAny(name, "/\\") || name == ".." {
		return fmt.Errorf("bad release file name %q", name)
	}
	resp, err := u.c.Get(u.base + "/" + name)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", name, errNotExist)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s/%s: %s", u.base, name, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%s exceeds size limit (%d bytes)", name, limit)
	}
	return nil
}

func (u *urlSource) Read(name string) ([]byte, error) {
	var buf strings.Builder
	buf.Grow(64 << 10)
	if err := u.get(name, &buf, maxMetaBytes); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func (u *urlSource) Stream(name string, w io.Writer) error {
	return u.get(name, w, maxArtifactBytes)
}

func (u *urlSource) Describe() string { return u.base }
