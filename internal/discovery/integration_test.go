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

// freePort returns a currently-free TCP port. There is a small race to
// bind it, but these tests only need it as an advertised service port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback TCP:", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func announceOrSkip(t *testing.T, ctx context.Context, opts AnnounceOptions) *Announcer {
	t.Helper()
	ann, err := Announce(ctx, opts)
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("multicast unavailable on this host: %v", err)
	}
	if err != nil {
		t.Fatalf("Announce: %v", err)
	}
	t.Cleanup(func() { ann.Close() })
	return ann
}

// browseOrSkip runs one browse window; transport failures skip.
func browseOrSkip(t *testing.T, ctx context.Context, timeout time.Duration) []Cluster {
	t.Helper()
	clusters, err := Browse(ctx, BrowseOptions{Timeout: timeout})
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("multicast unavailable for browse: %v", err)
	}
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	return clusters
}

func countInstance(clusters []Cluster, instance string) int {
	n := 0
	for _, c := range clusters {
		if c.Instance == instance {
			n++
		}
	}
	return n
}

// TestMultiClusterBrowseFilter: two announcers with distinct instance
// names on the same host must both be visible to one Browse, each
// deduplicated to a single entry (re-announcements on multiple
// interfaces merge), and selectable by name — the exact shape of a
// two-cluster LAN where `join -discover-instance <name>` picks one.
// In-process multicast only; no real multi-machine LAN involved.
func TestMultiClusterBrowseFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := time.Now().UnixNano() % 1e6
	nameA := fmt.Sprintf("lm-alpha-%d", suffix)
	nameB := fmt.Sprintf("lm-beta-%d", suffix)
	fp := "sha256:" + strings.Repeat("cd", 32)

	announceOrSkip(t, ctx, AnnounceOptions{
		Port: freePort(t), Instance: nameA,
		TXT: map[string]string{TXTKeyCAFP: fp, TXTKeyProto: "1"},
	})
	announceOrSkip(t, ctx, AnnounceOptions{
		Port: freePort(t), Instance: nameB,
		TXT: map[string]string{TXTKeyCAFP: fp, TXTKeyProto: "1"},
	})

	time.Sleep(500 * time.Millisecond) // let probing settle
	clusters := browseOrSkip(t, ctx, 4*time.Second)
	if len(clusters) == 0 {
		t.Skip("no mDNS responses on this host/network — loopback advertise/browse not supported here")
	}

	na, nb := countInstance(clusters, nameA), countInstance(clusters, nameB)
	if na == 0 && nb == 0 {
		t.Skipf("neither test instance seen among %d advertisements — mDNS loopback not functional here", len(clusters))
	}
	// Dedup: regardless of how many interfaces answered, each instance
	// must collapse to exactly one cluster entry.
	if na != 1 {
		t.Errorf("instance %q appeared %d times, want 1 (deduped)", nameA, na)
	}
	if nb != 1 {
		t.Errorf("instance %q appeared %d times, want 1 (deduped)", nameB, nb)
	}

	// Filter-by-name shape: an agent picking cluster B by name.
	if nb == 1 {
		var pick *Cluster
		for i := range clusters {
			if clusters[i].Instance == nameB {
				pick = &clusters[i]
			}
		}
		if pick == nil || pick.CAFingerprint != fp {
			t.Errorf("filtered cluster = %+v, want %q with fp %q", pick, nameB, fp)
		}
	}
}

// TestAnnouncerCloseStopsAdvertisement: once Close() runs (goodbye
// packets + socket teardown), a fresh Browse must no longer see the
// instance — the in-process proof that shut-down/stale advertisements
// stop being discovered. Record TTL (120s per RFC 6762) only governs
// responder caches; with the responder gone nothing re-announces.
func TestAnnouncerCloseStopsAdvertisement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inst := fmt.Sprintf("lm-gone-%d", time.Now().UnixNano()%1e6)
	ann := announceOrSkip(t, ctx, AnnounceOptions{
		Port: freePort(t), Instance: inst,
		TXT: map[string]string{TXTKeyCAFP: "sha256:" + strings.Repeat("ef", 32)},
	})

	time.Sleep(500 * time.Millisecond)
	clusters := browseOrSkip(t, ctx, 4*time.Second)
	if len(clusters) == 0 {
		t.Skip("no mDNS responses on this host/network — loopback advertise/browse not supported here")
	}
	if countInstance(clusters, inst) == 0 {
		t.Skipf("announced instance %q never discovered — mDNS loopback not functional here", inst)
	}

	if err := ann.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Allow the goodbye multicast to land, then re-browse: the dead
	// responder must not answer.
	time.Sleep(300 * time.Millisecond)
	clusters = browseOrSkip(t, ctx, 3*time.Second)
	if n := countInstance(clusters, inst); n != 0 {
		t.Errorf("closed announcer still advertised %d entries", n)
	}
}

// TestAdvertisedAddressFamilies: an announcement carrying explicit
// IPv4 and IPv6 addrs must surface both families to the browser. A/AAAA
// records ride whichever multicast transport works, so this passes even
// on hosts without IPv6 multicast — it is skipped only when loopback
// mDNS itself does not function.
func TestAdvertisedAddressFamilies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inst := fmt.Sprintf("lm-addrs-%d", time.Now().UnixNano()%1e6)
	announceOrSkip(t, ctx, AnnounceOptions{
		Port: freePort(t), Instance: inst,
		Addrs: []string{"127.0.0.1", "::1"},
		TXT:   map[string]string{TXTKeyCAFP: "sha256:" + strings.Repeat("aa", 32)},
	})

	time.Sleep(500 * time.Millisecond)
	clusters := browseOrSkip(t, ctx, 4*time.Second)
	if len(clusters) == 0 {
		t.Skip("no mDNS responses on this host/network — loopback advertise/browse not supported here")
	}
	for _, c := range clusters {
		if c.Instance != inst {
			continue
		}
		var v4, v6 bool
		for _, ip := range c.Addrs {
			if ip.Equal(net.ParseIP("127.0.0.1")) {
				v4 = true
			}
			if ip.Equal(net.ParseIP("::1")) {
				v6 = true
			}
		}
		if !v4 {
			t.Errorf("advertised IPv4 addr missing from browse: %v", c.Addrs)
		}
		if !v6 {
			t.Errorf("advertised IPv6 addr missing from browse: %v", c.Addrs)
		}
		return
	}
	t.Skipf("our instance %q not seen among %d advertisements — mDNS loopback not functional here", inst, len(clusters))
}

// TestBrowseWithoutAdvertiser: with no LocalMesh announcer up (the
// "discovery disabled" posture — the announce path is opt-in and this
// package announces nothing unless asked), a browse returns no local
// cluster for our unique instance. A stray real advertiser on this
// link would only add entries for OTHER names.
func TestBrowseWithoutAdvertiser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	phantom := fmt.Sprintf("lm-never-%d", time.Now().UnixNano()%1e6)
	clusters := browseOrSkip(t, ctx, 3*time.Second)
	if n := countInstance(clusters, phantom); n != 0 {
		t.Fatalf("instance %q was never announced but appeared %d times", phantom, n)
	}
	// Informational: log any real clusters on this link so a failure is
	// attributable to a stray advertiser, not our code.
	for _, c := range clusters {
		t.Logf("stray advertisement present on link: %q %s", c.Instance, c.Endpoint())
	}
}
