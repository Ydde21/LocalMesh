package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/coder/websocket"
)

// ConnectCause is a coarse failure category for reaching the control
// plane. It rides the "cause" field of reconnect log lines so an
// operator (and, later, `localmesh-agent status`) can tell why the
// agent cannot connect without decoding wrapped Go error chains.
type ConnectCause string

const (
	// CauseAuth — the node credential was rejected (HTTP 401/403):
	// bad token, node not approved, or revoked.
	CauseAuth ConnectCause = "auth"
	// CauseConfig — the local config cannot produce a dial target
	// (unparseable server_url).
	CauseConfig ConnectCause = "config"
	// CauseDNS — the server hostname does not resolve.
	CauseDNS ConnectCause = "dns"
	// CauseTLS — certificate or handshake problem: untrusted CA,
	// SAN/hostname mismatch, expired cert, remote TLS alert.
	CauseTLS ConnectCause = "tls"
	// CauseRefused — TCP connect refused: control plane down, wrong
	// port, or a firewall actively rejecting.
	CauseRefused ConnectCause = "refused"
	// CauseUnreachable — no route: host or network unreachable.
	CauseUnreachable ConnectCause = "unreachable"
	// CauseTimeout — a connect/handshake deadline fired.
	CauseTimeout ConnectCause = "timeout"
	// CauseHTTP — the upgrade was answered with a non-101 HTTP status
	// other than 401/403 (proxy, captive portal, wrong service).
	CauseHTTP ConnectCause = "http"
	// CauseProtocol — the post-upgrade handshake was rejected: protocol
	// version mismatch or an error frame from the server.
	CauseProtocol ConnectCause = "protocol"
	// CauseClosed — the socket closed or reset mid-session (clean
	// close, TCP reset, EOF).
	CauseClosed ConnectCause = "closed"
	// CauseUnknown — anything that escaped the buckets above.
	CauseUnknown ConnectCause = "unknown"
)

// ConnectError wraps a failed dial or handshake with its classified
// cause. Run logs the cause on every reconnect; callers can also
// errors.As a *ConnectError out of a session error.
type ConnectError struct {
	Cause ConnectCause
	Op    string // connect phase: "dial", "hello"
	Addr  string // redacted dial target, empty when not URL-bound
	Err   error
}

func (e *ConnectError) Error() string {
	if e.Addr != "" {
		return fmt.Sprintf("%s %s: %v", e.Op, e.Addr, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *ConnectError) Unwrap() error { return e.Err }

// ConnectCauseOf returns the failure category for an error from the
// connect/session path. Errors already carrying a cause (*ConnectError)
// report it verbatim; raw transport errors are classified on the fly so
// every reconnect log line carries a reason.
func ConnectCauseOf(err error) ConnectCause {
	var ce *ConnectError
	if errors.As(err, &ce) && ce.Cause != "" {
		return ce.Cause
	}
	return classifyConnectErr(err)
}

// classifyConnectErr buckets raw transport and handshake errors. Check
// order matters: a DNS timeout is a *net.DNSError before it is a
// net.Error, and typed x509 failures are checked before the "tls:"
// string fallback for unexported alert errors.
func classifyConnectErr(err error) ConnectCause {
	switch {
	case err == nil:
		return CauseUnknown
	case errors.Is(err, errAuthRejected):
		return CauseAuth
	case errors.Is(err, context.DeadlineExceeded):
		return CauseTimeout
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout {
			return CauseTimeout
		}
		return CauseDNS
	}

	// Certificate and handshake problems: typed x509 errors plus
	// record-layer failures.
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var certInv x509.CertificateInvalidError
	var insecureAlg x509.InsecureAlgorithmError
	var certVer *tls.CertificateVerificationError
	var recHdr tls.RecordHeaderError
	switch {
	case errors.As(err, &hostErr), errors.As(err, &authErr),
		errors.As(err, &certInv), errors.As(err, &insecureAlg),
		errors.As(err, &certVer), errors.As(err, &recHdr):
		return CauseTLS
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return CauseTimeout
	}

	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return CauseRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return CauseUnreachable
	}

	// Windows socket failures carry raw WSA codes (connectex et al.),
	// not the unix errno names above.
	if cause, ok := classifyPlatformErr(err); ok {
		return cause
	}

	// Closed sockets: clean close, mid-session reset, or the read side
	// hitting EOF after the peer went away.
	var closeErr websocket.CloseError
	switch {
	case errors.As(err, &closeErr),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, io.EOF),
		errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EPIPE):
		return CauseClosed
	}

	// Remote TLS alerts arrive as an unexported type; their message
	// still carries the "tls:" prefix.
	if strings.Contains(err.Error(), "tls:") {
		return CauseTLS
	}
	return CauseUnknown
}
