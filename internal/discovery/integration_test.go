package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TestAnnounceBrowseLoopback is an end-to-end check: one Announcer and
// one Browse on the same host over multicast-capable interfaces
// (zeroconf's interface list includes loopback where the OS marks it
// multicast-capable — macOS lo0 qualifies). mDNS is inherently
// environment-dependent, so transport failures and empty results skip
// rather than fail; the deterministic logic is covered by unit tests.
func TestAnnounceBrowseLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Any free port works — Announce does not need the service up.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback TCP:", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	fp := "sha256:" + strings.Repeat("ab", 32)
	inst := fmt.Sprintf("lm-test-%d", time.Now().UnixNano()%1e6)
	ann, err := Announce(ctx, AnnounceOptions{
		Port:     port,
		Instance: inst,
		TXT:      map[string]string{TXTKeyCAFP: fp, TXTKeyProto: "1"},
	})
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("multicast unavailable on this host: %v", err)
	}
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	defer ann.Close()

	// Give the responder a moment to finish probing before querying.
	time.Sleep(500 * time.Millisecond)
	clusters, err := Browse(ctx, BrowseOptions{Timeout: 4 * time.Second})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			t.Skipf("multicast unavailable for browse: %v", err)
		}
		t.Fatalf("Browse: %v", err)
	}
	if len(clusters) == 0 {
		t.Skip("no mDNS responses on this host/network — loopback advertise/browse not supported here")
	}
	for _, c := range clusters {
		if c.Instance != inst {
			continue
		}
		if c.Port != port {
			t.Errorf("port = %d, want %d", c.Port, port)
		}
		if c.CAFingerprint != fp {
			t.Errorf("CAFingerprint = %q, want %q", c.CAFingerprint, fp)
		}
		if c.TXT[TXTKeyCluster] != inst {
			t.Errorf("cluster TXT = %q, want %q", c.TXT[TXTKeyCluster], inst)
		}
		if len(c.Addrs) == 0 {
			t.Error("no addresses advertised")
		}
		return
	}
	t.Skipf("our instance %q not seen among %d advertisements — mDNS loopback not functional here", inst, len(clusters))
}
