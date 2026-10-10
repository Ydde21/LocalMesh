package agent

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/discovery"
)

func cluster(instance, host string, port int, fp string, addrs ...net.IP) discovery.Cluster {
	return discovery.Cluster{
		Instance: instance, Host: host, Port: port, Addrs: addrs,
		TXT:           map[string]string{"v": "1"},
		CAFingerprint: fp,
		SeenAt:        time.Now(),
	}
}

var fpA = "sha256:" + strings.Repeat("aa", 32)
var fpB = "sha256:" + strings.Repeat("bb", 32)

func TestSelectCluster(t *testing.T) {
	if _, err := SelectCluster(nil, ""); !errors.Is(err, ErrNoClusters) {
		t.Fatalf("empty set should return ErrNoClusters, got %v", err)
	}

	one := cluster("alpha", "a.local", 8443, fpA)
	got, err := SelectCluster([]discovery.Cluster{one}, "")
	if err != nil || got.Instance != "alpha" {
		t.Fatalf("single cluster should auto-select: %v %v", got, err)
	}

	two := []discovery.Cluster{one, cluster("beta", "b.local", 8443, fpB)}
	_, err = SelectCluster(two, "")
	if err == nil || !strings.Contains(err.Error(), "-discover-instance") {
		t.Fatalf("ambiguous set should demand -discover-instance, got %v", err)
	}
	if !strings.Contains(err.Error(), "1. alpha") || !strings.Contains(err.Error(), "2. beta") {
		t.Fatalf("ambiguous error should list candidates, got %v", err)
	}

	got, err = SelectCluster(two, "beta")
	if err != nil || got.Instance != "beta" {
		t.Fatalf("named selection failed: %v %v", got, err)
	}

	_, err = SelectCluster(two, "gamma")
	if err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("unknown name should list advertised names, got %v", err)
	}
}

func TestCandidateBases(t *testing.T) {
	c := discovery.Cluster{
		Host: "srv.local", Port: 8443,
		Addrs: []net.IP{
			net.ParseIP("192.168.1.10"),
			net.ParseIP("fd00::1"),     // global v6: kept
			net.ParseIP("fe80::1"),     // link-local v6: skipped (no zone)
			net.ParseIP("169.254.1.1"), // link-local v4: skipped
			nil,                        // malformed entry: skipped
			net.ParseIP("0.0.0.0"),     // unspecified: skipped
		},
	}
	bases := candidateBases(c)
	want := []string{
		"https://srv.local:8443",
		"https://192.168.1.10:8443",
		"https://[fd00::1]:8443",
	}
	if len(bases) != len(want) {
		t.Fatalf("bases = %v, want %v", bases, want)
	}
	for i := range want {
		if bases[i] != want[i] {
			t.Errorf("bases[%d] = %q, want %q", i, bases[i], want[i])
		}
	}
}

func TestDecideCAFingerprint(t *testing.T) {
	fetched := fpA

	// Advertised fingerprint matches the served CA → trust it.
	fp, warn, err := decideCAFingerprint(fetched, fpA, "")
	if err != nil || warn != "" || fp != fpA {
		t.Fatalf("match case: fp=%q warn=%q err=%v", fp, warn, err)
	}

	// Advertised vs served mismatch → abort (spoofing must not plant a
	// rogue CA).
	_, _, err = decideCAFingerprint(fetched, fpB, "")
	if err == nil || !strings.Contains(err.Error(), "MISMATCH") {
		t.Fatalf("spoofed advertisement should abort loudly, got %v", err)
	}

	// Missing advertised fingerprint requires an explicit pin.
	_, _, err = decideCAFingerprint(fetched, "", "")
	if err == nil || !strings.Contains(err.Error(), "--ca-fingerprint") {
		t.Fatalf("missing ca_fp should require --ca-fingerprint, got %v", err)
	}

	// Out-of-band pin wins over TXT: matching pin proceeds…
	fp, _, err = decideCAFingerprint(fetched, fpA, fpA)
	if err != nil || fp != fpA {
		t.Fatalf("pinned match should proceed: %v", err)
	}
	// …even when the advertisement disagrees (with a loud warning).
	fp, warn, err = decideCAFingerprint(fetched, fpB, fpA)
	if err != nil || fp != fpA || warn == "" {
		t.Fatalf("pin should win over conflicting TXT with warning: fp=%q warn=%q err=%v", fp, warn, err)
	}
	// …but a pin that doesn't match the served CA aborts.
	_, _, err = decideCAFingerprint(fetched, fpA, fpB)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("pin mismatch should abort, got %v", err)
	}
}

func TestFetchCAFingerprint(t *testing.T) {
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("test-der-bytes")})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ca.pem" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(caPEM)
	}))
	defer srv.Close()

	got, err := fetchCAFingerprint(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchCAFingerprint: %v", err)
	}
	want := "sha256:" + fingerprintOf(caPEM)
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Non-PEM body and non-200 are errors, not fingerprints.
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not a cert"))
	}))
	defer bad.Close()
	if _, err := fetchCAFingerprint(context.Background(), bad.URL); err == nil {
		t.Error("expected error for non-PEM body")
	}
}

func TestDiscoverServer(t *testing.T) {
	ip := net.ParseIP("192.168.1.50")
	stubFetch := func(fps map[string]string) func(context.Context, string) (string, error) {
		return func(_ context.Context, base string) (string, error) {
			if fp, ok := fps[base]; ok {
				return fp, nil
			}
			return "", fmt.Errorf("unreachable: %s", base)
		}
	}
	stubBrowse := func(cs []discovery.Cluster, err error) func(context.Context, time.Duration) ([]discovery.Cluster, error) {
		return func(context.Context, time.Duration) ([]discovery.Cluster, error) {
			return cs, err
		}
	}

	// Happy path: single cluster, hostname base serves the advertised CA.
	c := cluster("alpha", "srv.local", 8443, fpA)
	ds, err := DiscoverServer(context.Background(), DiscoverJoinOptions{
		browse:  stubBrowse([]discovery.Cluster{c}, nil),
		fetchFP: stubFetch(map[string]string{"https://srv.local:8443": fpA}),
	})
	if err != nil {
		t.Fatalf("DiscoverServer: %v", err)
	}
	if ds.Server != "https://srv.local:8443" || ds.CAFinger != fpA {
		t.Fatalf("unexpected result: %+v", ds)
	}

	// Hostname undialable → falls back to the advertised IP.
	ds, err = DiscoverServer(context.Background(), DiscoverJoinOptions{
		browse:  stubBrowse([]discovery.Cluster{{Instance: "alpha", Host: "srv.local", Port: 8443, Addrs: []net.IP{ip}, CAFingerprint: fpA}}, nil),
		fetchFP: stubFetch(map[string]string{"https://192.168.1.50:8443": fpA}),
	})
	if err != nil || ds.Server != "https://192.168.1.50:8443" {
		t.Fatalf("IP fallback failed: %v %+v", err, ds)
	}

	// Served CA disagrees with the advertisement → abort (anti-spoof).
	_, err = DiscoverServer(context.Background(), DiscoverJoinOptions{
		browse:  stubBrowse([]discovery.Cluster{c}, nil),
		fetchFP: stubFetch(map[string]string{"https://srv.local:8443": fpB}),
	})
	if err == nil || !strings.Contains(err.Error(), "MISMATCH") {
		t.Fatalf("spoofed CA should abort, got %v", err)
	}

	// Nothing discovered → ErrNoClusters.
	_, err = DiscoverServer(context.Background(), DiscoverJoinOptions{
		browse: stubBrowse(nil, nil),
	})
	if !errors.Is(err, ErrNoClusters) {
		t.Fatalf("empty browse should return ErrNoClusters, got %v", err)
	}

	// Advertised but unreachable anywhere → actionable error.
	_, err = DiscoverServer(context.Background(), DiscoverJoinOptions{
		browse:  stubBrowse([]discovery.Cluster{c}, nil),
		fetchFP: stubFetch(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable advertiser should error, got %v", err)
	}
}
