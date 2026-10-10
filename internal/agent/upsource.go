package agent

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// proxySource is an update.Source that reads release files through the
// control plane's authenticated release proxy. A wire-dispatched
// AgentUpgrade with source "localmesh://releases/<id>" is resolved to
//
//	GET <server_url>/api/v1/agent-releases/<id>/files/<name>
//
// carrying the node credential and the same pinned-CA TLS config the
// agent socket uses — agents without outbound access to the release
// host still verify signatures end-to-end; the proxy is transport, not
// trust.
//
// Fetch policy mirrors update.urlSource: metadata reads are capped at
// 8MiB, artifact streams at 4GiB, file names are single path segments.
type proxySource struct {
	base  string // <server>/api/v1/agent-releases/<id>/files
	token string
	c     *http.Client
	ctx   context.Context // upgrade op ctx — aborts cancel in-flight fetches
}

// same caps as internal/agent/update/source.go (kept private there).
const (
	proxyMaxMetaBytes     = 8 << 20
	proxyMaxArtifactBytes = 4 << 30
	proxyHTTPTimeout      = 10 * time.Minute // artifacts may be tens of MB on slow links
)

// localmeshReleasePrefix is the wire scheme routing release fetches
// through the control plane.
const localmeshReleasePrefix = "localmesh://releases/"

// releaseProxy resolves a localmesh://releases/<id> source into a
// proxySource. The release id must be a single path segment.
func (a *Agent) releaseProxy(ctx context.Context, source string) (*proxySource, error) {
	id := strings.TrimPrefix(source, localmeshReleasePrefix)
	id = strings.TrimSuffix(id, "/")
	if id == "" || id == ".." || strings.ContainsAny(id, "/\\") {
		return nil, fmt.Errorf("bad localmesh release source %q — want %s<id>", source, localmeshReleasePrefix)
	}
	hc := &http.Client{Timeout: proxyHTTPTimeout}
	if tlsCfg := a.cfg.TLSConfig(); tlsCfg != nil {
		hc.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	base := strings.TrimRight(a.cfg.ServerURL, "/") +
		"/api/v1/agent-releases/" + id + "/files"
	return &proxySource{base: base, token: a.cfg.NodeToken, c: hc, ctx: ctx}, nil
}

func (p *proxySource) get(name string, w io.Writer, limit int64) error {
	if strings.ContainsAny(name, "/\\") || name == ".." {
		return fmt.Errorf("bad release file name %q", name)
	}
	req, err := http.NewRequestWithContext(p.ctx, http.MethodGet, p.base+"/"+name, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s/%s: %s", p.base, name, resp.Status)
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

func (p *proxySource) Read(name string) ([]byte, error) {
	var buf strings.Builder
	buf.Grow(64 << 10)
	if err := p.get(name, &buf, proxyMaxMetaBytes); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func (p *proxySource) Stream(name string, w io.Writer) error {
	return p.get(name, w, proxyMaxArtifactBytes)
}

func (p *proxySource) Describe() string {
	return strings.TrimSuffix(p.base, "/files") + " (control-plane release proxy)"
}
