package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/discovery"
)

// ErrNoClusters reports that LAN discovery found no LocalMesh
// advertisements. It is not fatal — mDNS never crosses routed networks
// or VPNs — it just means the manual --server path is needed.
var ErrNoClusters = errors.New("no LocalMesh clusters found on this network — use `join --server <url>` for manual enrollment")

// DiscoverServers browses the local link for advertised LocalMesh
// control planes. An empty slice is a normal result, not an error.
func DiscoverServers(ctx context.Context, timeout time.Duration) ([]discovery.Cluster, error) {
	clusters, err := discovery.Browse(ctx, discovery.BrowseOptions{Timeout: timeout})
	if err != nil && errors.Is(err, discovery.ErrUnavailable) {
		return nil, fmt.Errorf("%w — use 'join --server <url>' for manual enrollment instead", err)
	}
	return clusters, err
}

// DiscoverJoinOptions controls `join -discover`: which cluster to pick
// and how to verify its advertised CA.
type DiscoverJoinOptions struct {
	// Timeout is the mDNS browse window (0 → discovery default).
	Timeout time.Duration
	// Instance selects a cluster by name when several advertise.
	Instance string
	// CAFinger is an out-of-band --ca-fingerprint pin. When set it is
	// authoritative: it wins over the advertised TXT ca_fp.
	CAFinger string
	// Stderr receives progress and security warnings (may be nil).
	Stderr io.Writer

	// browse and fetchFP are injected by tests; nil means live mDNS
	// browse and a real (TLS-insecure) ca.pem fetch.
	browse  func(ctx context.Context, timeout time.Duration) ([]discovery.Cluster, error)
	fetchFP func(ctx context.Context, base string) (string, error)
}

// DiscoveredServer is a discovered control plane whose served CA was
// verified against its advertisement (or an out-of-band pin).
type DiscoveredServer struct {
	// Cluster is the selected advertisement.
	Cluster discovery.Cluster
	// Server is the base URL to enroll against (https://host:port).
	Server string
	// CAFinger is the verified "sha256:..." fingerprint to pin during
	// enrollment — the enrollment flow re-checks it, so a spoofed
	// advertisement cannot plant a rogue CA.
	CAFinger string
}

// DiscoverServer finds a LocalMesh cluster on the LAN and verifies its
// advertised CA fingerprint. It only supplies a server URL and a
// verified fingerprint — enrollment still requires a valid token and
// admin approval, so discovery never authorizes a device.
func DiscoverServer(ctx context.Context, opts DiscoverJoinOptions) (*DiscoveredServer, error) {
	logf := func(format string, a ...any) {
		if opts.Stderr != nil {
			fmt.Fprintf(opts.Stderr, format+"\n", a...)
		}
	}
	browse := opts.browse
	if browse == nil {
		browse = DiscoverServers
	}
	fetch := opts.fetchFP
	if fetch == nil {
		fetch = fetchCAFingerprint
	}

	clusters, err := browse(ctx, opts.Timeout)
	if err != nil {
		return nil, err
	}
	c, err := SelectCluster(clusters, opts.Instance)
	if err != nil {
		return nil, err
	}
	logf("found cluster %q at %s", c.Instance, c.Endpoint())

	// Fetch the cluster CA over intentionally unverified TLS — safe at
	// this stage ONLY because the fetched certificate's fingerprint is
	// verified against the advertisement (or the out-of-band pin)
	// before anything trusts it. Try the advertised host name first,
	// then each advertised address.
	var base, fetchedFP string
	for _, cand := range candidateBases(c) {
		fp, err := fetch(ctx, cand)
		if err == nil {
			base, fetchedFP = cand, fp
			break
		}
	}
	if base == "" {
		return nil, fmt.Errorf("cluster %q advertised at %s but unreachable — could not fetch its CA; try join --server instead",
			c.Instance, strings.Join(candidateBases(c), ", "))
	}

	fp, warn, err := decideCAFingerprint(fetchedFP, c.CAFingerprint, opts.CAFinger)
	if warn != "" {
		logf("%s", warn)
	}
	if err != nil {
		return nil, err
	}
	logf("verified cluster CA fingerprint %s", fp)
	return &DiscoveredServer{Cluster: c, Server: base, CAFinger: fp}, nil
}

// SelectCluster picks one cluster from the discovered set: the only one
// when unambiguous, or the named one via -discover-instance.
func SelectCluster(clusters []discovery.Cluster, instance string) (discovery.Cluster, error) {
	if len(clusters) == 0 {
		return discovery.Cluster{}, ErrNoClusters
	}
	if instance != "" {
		for _, c := range clusters {
			if c.Instance == instance {
				return c, nil
			}
		}
		names := make([]string, 0, len(clusters))
		for _, c := range clusters {
			names = append(names, c.Instance)
		}
		return discovery.Cluster{}, fmt.Errorf("no discovered cluster named %q — advertised: %s",
			instance, strings.Join(names, ", "))
	}
	if len(clusters) == 1 {
		return clusters[0], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "multiple LocalMesh clusters discovered — re-run with -discover-instance <name>:")
	for i, c := range clusters {
		fmt.Fprintf(&b, "\n  %d. %s (%s)", i+1, c.Instance, c.Endpoint())
	}
	return discovery.Cluster{}, errors.New(b.String())
}

// candidateBases lists https base URLs to try for the selected cluster:
// the advertised host name first (canonical, stable across DHCP), then
// routable advertised addresses. Link-local, multicast, and unspecified
// addresses are skipped — they are undialable without a zone or never
// valid targets.
func candidateBases(c discovery.Cluster) []string {
	port := strconv.Itoa(c.Port)
	var bases []string
	if c.Host != "" {
		bases = append(bases, "https://"+net.JoinHostPort(c.Host, port))
	}
	for _, ip := range c.Addrs {
		if ip == nil || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		bases = append(bases, "https://"+net.JoinHostPort(ip.String(), port))
	}
	return bases
}

// decideCAFingerprint applies the trust policy for a discovered server:
// an out-of-band --ca-fingerprint pin wins over the advertised TXT; a
// fetched/served CA that disagrees aborts the join (a spoofed
// advertisement must not plant a rogue CA); a missing advertisement
// requires an explicit pin.
func decideCAFingerprint(fetched, advertised, pinned string) (fp string, warn string, err error) {
	switch {
	case pinned != "":
		if fetched != pinned {
			return "", "", fmt.Errorf("CA fingerprint mismatch: server presents %s but --ca-fingerprint pins %s — aborting (possible MITM or wrong cluster)",
				fetched, pinned)
		}
		if advertised != "" && advertised != pinned {
			return pinned, fmt.Sprintf("warning: advertised ca_fp %s differs from pinned %s — the advertisement may be spoofed; trusting the out-of-band pin",
				shortFP(advertised), shortFP(pinned)), nil
		}
		return pinned, "", nil
	case advertised == "":
		return "", "", errors.New("advertisement does not include a CA fingerprint (ca_fp) — re-run with --ca-fingerprint sha256:... to pin explicitly")
	case fetched != advertised:
		return "", "", fmt.Errorf("CA fingerprint MISMATCH: advertisement claims %s but the server presents %s — aborting; the advertisement may be spoofed, verify out-of-band with --ca-fingerprint",
			shortFP(advertised), shortFP(fetched))
	default:
		return fetched, "", nil
	}
}

// fetchCAFingerprint fetches /api/v1/ca.pem over intentionally
// unverified TLS and returns its sha256 fingerprint ("sha256:hex").
// InsecureSkipVerify is correct here: the cluster CA is not yet
// trusted, and the fingerprint check downstream is what authenticates
// it — identical to the bootstrap inside fetchCA.
func fetchCAFingerprint(ctx context.Context, base string) (string, error) {
	hc := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(base, "/")+"/api/v1/ca.pem", nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /api/v1/ca.pem → %s", resp.Status)
	}
	if !isPEMCert(body) {
		return "", errors.New("server did not return a CA certificate")
	}
	return "sha256:" + fingerprintOf(body), nil
}
