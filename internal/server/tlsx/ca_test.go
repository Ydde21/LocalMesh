package tlsx

import (
	"crypto/x509"
	"net"
	"testing"
)

func leafSANs(t *testing.T, dir string, names []string) (*x509.Certificate, func()) {
	ca, err := LoadOrCreate(dir, names, nil)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	leaf, err := x509.ParseCertificate(ca.Server.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf, func() {}
}

func TestIPLiteralGoesToIPSAN(t *testing.T) {
	dir := t.TempDir()
	leaf, _ := leafSANs(t, dir, []string{"myserver.local", "192.168.1.50", "::1"})

	hasIP := map[string]bool{}
	for _, ip := range leaf.IPAddresses {
		hasIP[ip.String()] = true
	}
	if !hasIP["192.168.1.50"] {
		t.Fatalf("IP literal must land in IPAddresses, got %v", leaf.IPAddresses)
	}
	for _, n := range leaf.DNSNames {
		if net.ParseIP(n) != nil {
			t.Fatalf("IP literal %q must not appear in DNSNames", n)
		}
	}
	hasDNS := map[string]bool{}
	for _, n := range leaf.DNSNames {
		hasDNS[n] = true
	}
	if !hasDNS["myserver.local"] || !hasDNS["localhost"] {
		t.Fatalf("missing DNS SANs: %v", leaf.DNSNames)
	}
	if !hasIP["127.0.0.1"] {
		t.Fatal("loopback IP SAN must always be present")
	}
}

func TestCertReissuedWhenSANsChange(t *testing.T) {
	dir := t.TempDir()
	leaf, _ := leafSANs(t, dir, []string{"first.local"})
	has := func(n string, l *x509.Certificate) bool {
		for _, d := range l.DNSNames {
			if d == n {
				return true
			}
		}
		return false
	}
	if !has("first.local", leaf) {
		t.Fatal("first cert lacks first.local")
	}

	// Re-open with an additional hostname — cert must be reissued.
	ca, err := LoadOrCreate(dir, []string{"first.local", "second.local"}, nil)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	leaf2, _ := x509.ParseCertificate(ca.Server.Certificate[0])
	if !has("second.local", leaf2) {
		t.Fatal("reopened CA did not reissue cert covering new SAN")
	}
}

func TestCertKeptWhenSANsUnchanged(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreate(dir, []string{"same.local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	serial1 := ca1.Server.Certificate[0]

	ca2, err := LoadOrCreate(dir, []string{"same.local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(serial1) != string(ca2.Server.Certificate[0]) {
		t.Fatal("cert must be reused when SANs are unchanged")
	}
}

func TestCovers(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreate(dir, []string{"a.local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(ca.Server.Certificate[0])
	if !covers(leaf, []string{"a.local"}, nil) {
		t.Fatal("leaf must cover its own SANs")
	}
	if covers(leaf, []string{"b.local"}, nil) {
		t.Fatal("leaf must not cover unissued names")
	}
	if covers(leaf, nil, []net.IP{net.ParseIP("10.0.0.5")}) {
		t.Fatal("leaf must not cover unissued IPs")
	}
}
