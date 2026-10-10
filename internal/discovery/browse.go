package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// BrowseOptions configures a LAN browse for LocalMesh clusters.
type BrowseOptions struct {
	// Timeout is the browse window — how long to listen for
	// announcements. Defaults to DefaultBrowseTimeout.
	Timeout time.Duration
	// Ifaces restricts the query to these interfaces; nil means all
	// multicast-capable interfaces.
	Ifaces []net.Interface
}

// clusterKey deduplicates advertisements by instance+host+port.
type clusterKey struct {
	instance, host string
	port           int
}

// Browse listens on the local link for _localmesh._tcp announcements
// for opts.Timeout and returns the deduplicated clusters found.
//
// An empty slice is a normal result — no responders on this link, or
// multicast is filtered. mDNS never crosses routed networks/VPN
// boundaries, so callers must offer the manual "join --server <url>"
// path. Only genuine transport failures (no multicast-capable
// interface, resolver setup refused) produce an error, wrapped in
// ErrUnavailable.
func Browse(ctx context.Context, opts BrowseOptions) ([]Cluster, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultBrowseTimeout
	}
	resolver, err := newResolver(opts.Ifaces)
	if err != nil {
		return nil, err
	}

	// zeroconf closes entries when bctx ends, ending the receive loop.
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	if err := resolver.Browse(bctx, ServiceType, Domain, entries); err != nil {
		return nil, fmt.Errorf("%w: browse: %v", ErrUnavailable, err)
	}

	found := map[clusterKey]*Cluster{}
	var order []clusterKey
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case e, ok := <-entries:
			if !ok {
				return sortedClusters(found, order), nil
			}
			collectEntry(found, &order, e)
		}
	}
}

// collectEntry folds one advertised entry into the dedup map, merging
// addresses when the same instance+host+port is seen again (e.g. a
// re-announcement or a goodbye followed by a fresh announcement).
func collectEntry(found map[clusterKey]*Cluster, order *[]clusterKey, e *zeroconf.ServiceEntry) {
	if e == nil || e.Instance == "" || e.Port == 0 {
		return
	}
	host := strings.TrimSuffix(e.HostName, ".")
	key := clusterKey{instance: e.Instance, host: host, port: e.Port}
	c, ok := found[key]
	if !ok {
		c = &Cluster{Instance: e.Instance, Host: host, Port: e.Port, TXT: map[string]string{}}
		found[key] = c
		*order = append(*order, key)
	}
	for _, ip := range e.AddrIPv4 {
		if !containsIP(c.Addrs, ip) {
			c.Addrs = append(c.Addrs, ip)
		}
	}
	for _, ip := range e.AddrIPv6 {
		if !containsIP(c.Addrs, ip) {
			c.Addrs = append(c.Addrs, ip)
		}
	}
	if txt := parseTXT(e.Text); len(txt) > 0 {
		c.TXT = txt
		c.CAFingerprint = caFingerprint(txt)
	}
	c.SeenAt = time.Now()
}

func sortedClusters(found map[clusterKey]*Cluster, order []clusterKey) []Cluster {
	clusters := make([]Cluster, 0, len(order))
	for _, k := range order {
		clusters = append(clusters, *found[k])
	}
	sort.Slice(clusters, func(i, j int) bool {
		a, b := clusters[i], clusters[j]
		if a.Instance != b.Instance {
			return a.Instance < b.Instance
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Port < b.Port
	})
	return clusters
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, x := range list {
		if x.Equal(ip) {
			return true
		}
	}
	return false
}

// newResolver builds a dual-stack resolver, falling back to IPv4-only
// when the host lacks IPv6 multicast — common on VPN clients and
// minimal container networks, where the strict dual-stack join fails.
func newResolver(ifaces []net.Interface) (*zeroconf.Resolver, error) {
	opts := []zeroconf.ClientOption{}
	if len(ifaces) > 0 {
		opts = append(opts, zeroconf.SelectIfaces(ifaces))
	}
	resolver, err := zeroconf.NewResolver(opts...)
	if err != nil {
		resolver, err = zeroconf.NewResolver(
			append(opts, zeroconf.SelectIPTraffic(zeroconf.IPv4))...)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return resolver, nil
}
