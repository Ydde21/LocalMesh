package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/grandcat/zeroconf"
)

// AnnounceOptions configures a _localmesh._tcp service announcement.
type AnnounceOptions struct {
	// Port is the HTTPS port the control plane listens on (required).
	Port int
	// Instance is the cluster display name, advertised as the mDNS
	// service instance name and the "cluster" TXT key (required,
	// ≤ 63 bytes). Keep it human-friendly; it is not a secret.
	Instance string
	// TXT holds additional public TXT pairs. Recognized keys: "ca_fp"
	// (cluster CA fingerprint, "sha256:hex" or bare hex — normalized
	// to bare lowercase hex on the wire) and "proto" (protocol version
	// integer string). The package always sets "v" and defaults
	// "cluster" to Instance. Never put secrets or node names here.
	TXT map[string]string
	// Ifaces restricts announcement to these interfaces; nil means all
	// multicast-capable interfaces.
	Ifaces []net.Interface
	// Host optionally overrides the advertised host name (it is
	// normalized to an FQDN under .local); defaults to the machine
	// hostname.
	Host string
	// Addrs optionally overrides the advertised addresses; defaults to
	// the addresses of the selected interfaces.
	Addrs []string
}

// Announcer is a live mDNS service registration. It sends goodbye
// packets and releases the multicast sockets on Close, or automatically
// when the context passed to Announce is canceled. Note that the
// registration is bound to the interface set captured at Announce time —
// interfaces joining later are not picked up (a zeroconf limitation);
// re-announce on major network changes if needed.
type Announcer struct {
	srv      *zeroconf.Server
	instance string
	port     int
	txt      []string
	done     chan struct{} // closed by Close; releases the ctx watcher
	once     sync.Once
}

// Announce registers a _localmesh._tcp service on the LAN so agents can
// find this control plane. Multicast is best-effort: when no
// multicast-capable interface exists, Announce returns an error
// wrapping ErrUnavailable rather than panicking — the control plane
// should log it and keep running (manual --server enrollment still
// works).
func Announce(ctx context.Context, opts AnnounceOptions) (*Announcer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Instance == "" {
		return nil, errors.New("discovery: Instance is required")
	}
	if len(opts.Instance) > 63 {
		return nil, errors.New("discovery: Instance exceeds 63-byte DNS label limit")
	}
	if opts.Port <= 0 || opts.Port > 65535 {
		return nil, fmt.Errorf("discovery: invalid port %d", opts.Port)
	}
	txt, err := buildTXT(opts)
	if err != nil {
		return nil, err
	}
	host, err := advertiseHost(opts)
	if err != nil {
		return nil, err
	}
	addrs, err := advertiseAddrs(opts)
	if err != nil {
		return nil, err
	}

	// RegisterProxy is used unconditionally: zeroconf's Register
	// appends ".local." only when the hostname lacks the domain — but
	// macOS hostnames already end in ".local" without the trailing dot,
	// pass the check, and produce a non-FQDN SRV target that miekg/dns
	// refuses to pack (the announcement then silently never goes out).
	// Normalizing host + addrs here keeps the wire records correct.
	srv, err := zeroconf.RegisterProxy(opts.Instance, ServiceType, Domain,
		opts.Port, host, addrs, txt, opts.Ifaces)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	a := &Announcer{srv: srv, instance: opts.Instance, port: opts.Port, txt: txt,
		done: make(chan struct{})}
	// Tie the announcement's lifetime to the caller's context (skipped
	// for non-cancelable contexts like context.Background()). The done
	// channel keeps a manual Close from parking this goroutine forever.
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				a.Close()
			case <-a.done:
			}
		}()
	}
	return a, nil
}

// Instance returns the announced service instance name.
func (a *Announcer) Instance() string { return a.instance }

// Port returns the announced service port.
func (a *Announcer) Port() int { return a.port }

// TXT returns the announced wire-format TXT records.
func (a *Announcer) TXT() []string { return a.txt }

// Close unregisters the service (multicast goodbye) and releases the
// sockets. Safe to call more than once.
func (a *Announcer) Close() error {
	a.once.Do(func() {
		close(a.done)
		a.srv.Shutdown()
	})
	return nil
}

// advertiseHost resolves the advertised host name to an FQDN under the
// .local domain (mDNS names must live there). RegisterProxy keeps the
// name verbatim once it ends with the domain.
func advertiseHost(opts AnnounceOptions) (string, error) {
	h := opts.Host
	if h == "" {
		var err error
		h, err = os.Hostname()
		if err != nil {
			return "", fmt.Errorf("discovery: cannot determine hostname: %w", err)
		}
	}
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return "", errors.New("discovery: empty host name")
	}
	if !strings.HasSuffix(h, "."+Domain) {
		h += "." + Domain
	}
	return h + ".", nil
}

// advertiseAddrs returns the IP addresses to advertise: opts.Addrs when
// given, else the unicast addresses of the multicast interfaces — the
// same selection zeroconf's Register makes (loopback excluded,
// link-local v6 only when no global v6 exists). A loopback-only host
// advertises its loopback addresses so same-machine agents still
// resolve something.
func advertiseAddrs(opts AnnounceOptions) ([]string, error) {
	if len(opts.Addrs) > 0 {
		for _, s := range opts.Addrs {
			if net.ParseIP(s) == nil {
				return nil, fmt.Errorf("discovery: invalid advertised address %q", s)
			}
		}
		return opts.Addrs, nil
	}
	ifaces := opts.Ifaces
	if len(ifaces) == 0 {
		ifaces = multicastIfaces()
	}
	var out []string
	var loopback []string
	for i := range ifaces {
		v4, v6, lo := addrsForInterface(&ifaces[i])
		for _, ip := range v4 {
			out = append(out, ip.String())
		}
		for _, ip := range v6 {
			out = append(out, ip.String())
		}
		for _, ip := range lo {
			loopback = append(loopback, ip.String())
		}
	}
	if len(out) == 0 {
		out = loopback
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no usable addresses on multicast interfaces", ErrUnavailable)
	}
	return out, nil
}

// multicastIfaces lists up interfaces that support multicast — the same
// set zeroconf picks by default.
func multicastIfaces() []net.Interface {
	var out []net.Interface
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagMulticast != 0 {
			out = append(out, ifi)
		}
	}
	return out
}

// addrsForInterface splits an interface's addresses into IPv4, usable
// IPv6 (global unicast, or link-local when no global exists), and
// loopback.
func addrsForInterface(iface *net.Interface) (v4, v6, loopback []net.IP) {
	var v6local []net.IP
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipnet.IP.IsLoopback() {
			loopback = append(loopback, ipnet.IP)
			continue
		}
		if ipnet.IP.To4() != nil {
			v4 = append(v4, ipnet.IP)
			continue
		}
		switch ip := ipnet.IP.To16(); {
		case ip == nil:
		case ip.IsGlobalUnicast():
			v6 = append(v6, ipnet.IP)
		case ip.IsLinkLocalUnicast():
			v6local = append(v6local, ipnet.IP)
		}
	}
	if len(v6) == 0 {
		v6 = v6local
	}
	return v4, v6, loopback
}

// buildTXT merges caller TXT pairs with the mandatory format version
// and cluster name, normalizing the CA fingerprint to the bare-hex wire
// form.
func buildTXT(opts AnnounceOptions) ([]string, error) {
	m := make(map[string]string, len(opts.TXT)+3)
	for k, v := range opts.TXT {
		m[k] = v
	}
	if fp := m[TXTKeyCAFP]; fp != "" {
		norm, err := NormalizeCAFingerprint(fp)
		if err != nil {
			return nil, fmt.Errorf("discovery: invalid %s TXT value: %w", TXTKeyCAFP, err)
		}
		m[TXTKeyCAFP] = strings.TrimPrefix(norm, "sha256:")
	}
	if v, ok := m[TXTKeyFormat]; ok && v != FormatVersion {
		return nil, fmt.Errorf("discovery: unsupported %s=%q — this build announces %s=%s",
			TXTKeyFormat, v, TXTKeyFormat, FormatVersion)
	}
	m[TXTKeyFormat] = FormatVersion
	if _, ok := m[TXTKeyCluster]; !ok {
		m[TXTKeyCluster] = opts.Instance
	}
	return encodeTXT(m)
}
