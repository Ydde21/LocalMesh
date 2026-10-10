package discovery

import (
	"net"
	"strings"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestParseTXT(t *testing.T) {
	got := parseTXT([]string{"v=1", "ca_fp=abc", "bare", "empty=", "a=b=c", "=bad"})
	want := map[string]string{
		"v": "1", "ca_fp": "abc", "bare": "", "empty": "", "a": "b=c",
	}
	if len(got) != len(want) {
		t.Fatalf("parseTXT returned %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseTXT[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestEncodeTXT(t *testing.T) {
	recs, err := encodeTXT(map[string]string{"proto": "1", "cluster": "demo", "v": "1"})
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic, sorted output.
	want := []string{"cluster=demo", "proto=1", "v=1"}
	if strings.Join(recs, ",") != strings.Join(want, ",") {
		t.Fatalf("encodeTXT = %v, want %v", recs, want)
	}

	if _, err := encodeTXT(map[string]string{"bad=key": "x"}); err == nil {
		t.Error("expected error for '=' in key")
	}
	if _, err := encodeTXT(map[string]string{"": "x"}); err == nil {
		t.Error("expected error for empty key")
	}
	long := map[string]string{"k": strings.Repeat("x", 255)}
	if _, err := encodeTXT(long); err == nil {
		t.Error("expected error for >255-byte record")
	}
}

func TestNormalizeCAFingerprint(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	for _, in := range []string{
		"sha256:" + hex64,
		strings.ToUpper(hex64),
		"sha256:" + strings.ToUpper(hex64),
		"  " + hex64 + "  ",
	} {
		got, err := NormalizeCAFingerprint(in)
		if err != nil {
			t.Errorf("NormalizeCAFingerprint(%q) error: %v", in, err)
			continue
		}
		if got != "sha256:"+hex64 {
			t.Errorf("NormalizeCAFingerprint(%q) = %q", in, got)
		}
	}
	for _, in := range []string{"", "sha256:short", "zz" + hex64[2:], "sha256:", "md5:" + hex64} {
		if _, err := NormalizeCAFingerprint(in); err == nil {
			t.Errorf("NormalizeCAFingerprint(%q) expected error", in)
		}
	}
}

func TestBuildTXT(t *testing.T) {
	hex64 := strings.Repeat("cd", 32)
	recs, err := buildTXT(AnnounceOptions{
		Instance: "demo",
		Port:     8443,
		TXT:      map[string]string{TXTKeyCAFP: "sha256:" + hex64, TXTKeyProto: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := parseTXT(recs)
	if m[TXTKeyFormat] != "1" {
		t.Errorf("v = %q, want 1", m[TXTKeyFormat])
	}
	if m[TXTKeyCluster] != "demo" {
		t.Errorf("cluster = %q, want demo", m[TXTKeyCluster])
	}
	if m[TXTKeyCAFP] != hex64 {
		t.Errorf("ca_fp = %q, want bare hex %q", m[TXTKeyCAFP], hex64)
	}
	if m[TXTKeyProto] != "1" {
		t.Errorf("proto = %q, want 1", m[TXTKeyProto])
	}

	// Caller may override the display name but not the format version.
	if _, err := buildTXT(AnnounceOptions{Instance: "x", Port: 1,
		TXT: map[string]string{TXTKeyFormat: "2"}}); err == nil {
		t.Error("expected error for conflicting v key")
	}
	if _, err := buildTXT(AnnounceOptions{Instance: "x", Port: 1,
		TXT: map[string]string{TXTKeyCAFP: "not-a-fingerprint"}}); err == nil {
		t.Error("expected error for invalid ca_fp")
	}
}

func entry(instance, host string, port int, text []string, v4, v6 []net.IP) *zeroconf.ServiceEntry {
	return &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: instance},
		HostName:      host,
		Port:          port,
		Text:          text,
		AddrIPv4:      v4,
		AddrIPv6:      v6,
	}
}

func TestCollectEntryDedup(t *testing.T) {
	found := map[clusterKey]*Cluster{}
	var order []clusterKey
	ip1 := net.ParseIP("192.168.1.10")
	ip2 := net.ParseIP("192.168.1.11")
	ip6 := net.ParseIP("fd00::1")

	collectEntry(found, &order, entry("alpha", "srv.local.", 8443,
		[]string{"ca_fp=" + strings.Repeat("ab", 32)}, []net.IP{ip1}, nil))
	// Same instance+host+port re-announced with an extra address: merge.
	collectEntry(found, &order, entry("alpha", "srv.local.", 8443,
		[]string{"ca_fp=" + strings.Repeat("ab", 32)}, []net.IP{ip1, ip2}, []net.IP{ip6}))
	// Different port → distinct cluster.
	collectEntry(found, &order, entry("alpha", "srv.local.", 9443, nil, []net.IP{ip1}, nil))
	// Missing instance → ignored.
	collectEntry(found, &order, entry("", "srv.local.", 8443, nil, []net.IP{ip1}, nil))
	// Missing port → ignored.
	collectEntry(found, &order, entry("beta", "srv.local.", 0, nil, []net.IP{ip1}, nil))

	clusters := sortedClusters(found, order)
	if len(clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(clusters))
	}
	primary := clusters[0]
	if primary.Instance != "alpha" || primary.Port != 8443 {
		t.Fatalf("unexpected first cluster: %+v", primary)
	}
	if primary.Host != "srv.local" {
		t.Errorf("Host = %q, want srv.local (trailing dot trimmed)", primary.Host)
	}
	if len(primary.Addrs) != 3 {
		t.Errorf("Addrs = %v, want 3 merged addresses", primary.Addrs)
	}
	wantFP := "sha256:" + strings.Repeat("ab", 32)
	if primary.CAFingerprint != wantFP {
		t.Errorf("CAFingerprint = %q, want %q", primary.CAFingerprint, wantFP)
	}
	if primary.SeenAt.IsZero() {
		t.Error("SeenAt not set")
	}
	if primary.TXT["ca_fp"] != strings.Repeat("ab", 32) {
		t.Errorf("TXT ca_fp = %q", primary.TXT["ca_fp"])
	}
}

func TestCollectEntryBadCAFP(t *testing.T) {
	found := map[clusterKey]*Cluster{}
	var order []clusterKey
	collectEntry(found, &order, entry("x", "h.local.", 1, []string{"ca_fp=bogus"}, nil, nil))
	c := found[clusterKey{"x", "h.local", 1}]
	if c.CAFingerprint != "" {
		t.Errorf("invalid ca_fp should yield empty CAFingerprint, got %q", c.CAFingerprint)
	}
	if c.TXT["ca_fp"] != "bogus" {
		t.Error("raw TXT should still be preserved")
	}
}

func TestEndpoint(t *testing.T) {
	c := Cluster{Host: "srv.local", Port: 8443}
	if got := c.Endpoint(); got != "srv.local:8443" {
		t.Errorf("Endpoint = %q", got)
	}
	c = Cluster{Port: 8443, Addrs: []net.IP{net.ParseIP("192.168.1.5")}}
	if got := c.Endpoint(); got != "192.168.1.5:8443" {
		t.Errorf("Endpoint fallback = %q", got)
	}
	c = Cluster{Port: 8443, Addrs: []net.IP{net.ParseIP("fd00::5")}}
	if got := c.Endpoint(); got != "[fd00::5]:8443" {
		t.Errorf("Endpoint v6 = %q", got)
	}
}

func TestAnnounceValidation(t *testing.T) {
	if _, err := Announce(t.Context(), AnnounceOptions{Port: 8443}); err == nil {
		t.Error("missing Instance should error")
	}
	if _, err := Announce(t.Context(), AnnounceOptions{Instance: "x", Port: 0}); err == nil {
		t.Error("port 0 should error")
	}
	if _, err := Announce(t.Context(), AnnounceOptions{Instance: "x", Port: 70000}); err == nil {
		t.Error("port >65535 should error")
	}
	if _, err := Announce(t.Context(), AnnounceOptions{Instance: strings.Repeat("x", 64), Port: 8443}); err == nil {
		t.Error("oversize instance should error")
	}
	if _, err := Announce(t.Context(), AnnounceOptions{Instance: "x", Port: 8443,
		TXT: map[string]string{TXTKeyCAFP: "bogus"}}); err == nil {
		t.Error("invalid ca_fp should error")
	}
}

func TestSecurityInvariants(t *testing.T) {
	// The wire format carries only public data: format version, cluster
	// display name, protocol version, and the CA fingerprint (a SHA-256
	// hash of a public certificate — safe to disclose).
	recs, err := buildTXT(AnnounceOptions{
		Instance: "demo", Port: 8443,
		TXT: map[string]string{TXTKeyCAFP: strings.Repeat("ef", 32), TXTKeyProto: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if strings.Contains(strings.ToLower(r), "token") || strings.Contains(strings.ToLower(r), "secret") {
			t.Fatalf("TXT must never carry secrets: %q", r)
		}
	}
	// An advertisement fingerprint that can't be parsed can never
	// become a valid pin — caFingerprint only returns normalized
	// sha256:hex or "".
	if got := caFingerprint(map[string]string{TXTKeyCAFP: "sha256:short"}); got != "" {
		t.Fatalf("bad fp should yield empty, got %q", got)
	}
}
