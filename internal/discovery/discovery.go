// Package discovery implements zero-configuration LAN discovery for
// LocalMesh control planes using mDNS / DNS-SD (RFC 6762 and RFC 6763),
// built on github.com/grandcat/zeroconf.
//
// A control plane calls Announce to register a _localmesh._tcp service
// on the local link; agents call Browse to find advertised clusters.
//
// Scope and limits: mDNS is link-local multicast. It does not cross
// routed networks, VLAN boundaries, VPN tunnels, or most cloud/overlay
// networks, and some networks block multicast entirely. Browse
// returning an empty slice is a normal outcome on those networks —
// callers must surface the manual "join --server <url>" fallback
// rather than treating it as a failure.
//
// Security invariants:
//   - Discovery NEVER authorizes a device. Enrollment tokens and admin
//     approval are untouched; discovery only supplies a server URL and
//     a verifiable CA fingerprint.
//   - Advertised TXT records contain only public information: the CA
//     fingerprint is a SHA-256 hash of a public certificate. Never put
//     secrets, tokens, or node names in TXT.
//   - Announcements are unauthenticated by design (mDNS has no
//     signatures); consumers MUST verify ca_fp against the served CA
//     before trusting an endpoint, which the agent join flow does.
package discovery

import (
	"errors"
	"net"
	"strconv"
	"time"
)

const (
	// ServiceType is the DNS-SD service type LocalMesh control planes
	// announce and agents browse for.
	ServiceType = "_localmesh._tcp"
	// Domain is the mDNS domain — the link-local "local" domain only.
	Domain = "local"
	// FormatVersion is the discovery TXT format version ("v" key).
	FormatVersion = "1"
)

// Well-known TXT record keys. Keep values small and public; each
// "key=value" pair must fit in one 255-byte DNS TXT string.
const (
	// TXTKeyFormat ("v") carries FormatVersion.
	TXTKeyFormat = "v"
	// TXTKeyCAFP ("ca_fp") carries the cluster CA SHA-256 fingerprint as
	// lowercase hex without a "sha256:" prefix on the wire.
	TXTKeyCAFP = "ca_fp"
	// TXTKeyCluster ("cluster") carries the cluster display name.
	TXTKeyCluster = "cluster"
	// TXTKeyProto ("proto") carries the control-plane protocol version
	// as a decimal integer string.
	TXTKeyProto = "proto"
)

// ErrUnavailable reports that mDNS could not be used on this host or
// network — no multicast-capable interface, or the OS/network refused
// the multicast group join (some VPNs and container networks). Announce
// and Browse wrap it: errors.Is(err, discovery.ErrUnavailable) lets
// callers degrade gracefully to manual configuration. It is a sentinel
// error; the package never panics on multicast failure.
var ErrUnavailable = errors.New("mdns unavailable on this network")

// DefaultBrowseTimeout is the browse window used when
// BrowseOptions.Timeout is unset.
const DefaultBrowseTimeout = 5 * time.Second

// Cluster is a LocalMesh control plane discovered on the LAN.
type Cluster struct {
	// Instance is the mDNS service instance name — the cluster display
	// name passed to Announce.
	Instance string `json:"instance"`
	// Host is the advertised host name, typically "<hostname>.local".
	Host string `json:"host"`
	// Port is the advertised TCP port of the HTTPS control plane.
	Port int `json:"port"`
	// Addrs are the advertised IPv4 and IPv6 addresses.
	Addrs []net.IP `json:"addrs"`
	// TXT is the advertised TXT record as a key/value map.
	TXT map[string]string `json:"txt"`
	// CAFingerprint is the advertised cluster CA fingerprint,
	// normalized to "sha256:<hex>" — empty when the advertisement
	// carried no (or an invalid) ca_fp value.
	CAFingerprint string `json:"ca_fingerprint"`
	// SeenAt is when this advertisement was last received.
	SeenAt time.Time `json:"seen_at"`
}

// Endpoint returns "host:port" for display and dialing, preferring the
// advertised host name and falling back to the first advertised
// address when the host name is empty.
func (c Cluster) Endpoint() string {
	host := c.Host
	if host == "" && len(c.Addrs) > 0 {
		host = c.Addrs[0].String()
	}
	return net.JoinHostPort(host, strconv.Itoa(c.Port))
}
