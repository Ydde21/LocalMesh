package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// classifyConnectErr is exercised on realistic wrapped chains the way
// websocket.Dial surfaces them (url.Error > net.OpError > leaf). These
// are synthetic in-process errors — no real network is involved except
// the loopback refused-dial case below.
func TestClassifyConnectErr(t *testing.T) {
	wrap := func(inner error) error {
		return &url.Error{Op: "Get", URL: "wss://cp.example/connect",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: inner}}
	}

	cases := []struct {
		name string
		err  error
		want ConnectCause
	}{
		{"dns nxdomain", wrap(&net.DNSError{Err: "no such host", IsNotFound: true}), CauseDNS},
		{"dns timeout", wrap(&net.DNSError{Err: "i/o timeout", IsTimeout: true}), CauseTimeout},
		{"refused", wrap(&os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}), CauseRefused},
		{"host unreachable", wrap(&os.SyscallError{Syscall: "connect", Err: syscall.EHOSTUNREACH}), CauseUnreachable},
		{"net unreachable", wrap(&os.SyscallError{Syscall: "connect", Err: syscall.ENETUNREACH}), CauseUnreachable},
		{"syscall timeout", wrap(&os.SyscallError{Syscall: "connect", Err: syscall.ETIMEDOUT}), CauseTimeout},
		{"conn reset", wrap(&os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}), CauseClosed},
		{"broken pipe", wrap(&os.SyscallError{Syscall: "write", Err: syscall.EPIPE}), CauseClosed},
		{"x509 unknown authority", wrap(x509.UnknownAuthorityError{Cert: &x509.Certificate{}}), CauseTLS},
		{"x509 hostname", wrap(x509.HostnameError{Host: "cp.example"}), CauseTLS},
		{"x509 invalid cert", wrap(x509.CertificateInvalidError{Cert: &x509.Certificate{}}), CauseTLS},
		{"tls verify error", wrap(&tls.CertificateVerificationError{Err: errors.New("expired")}), CauseTLS},
		{"tls record header", wrap(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), CauseTLS},
		{"deadline", fmt.Errorf("dial: %w", context.DeadlineExceeded), CauseTimeout},
		{"ws close", websocket.CloseError{Code: websocket.StatusNormalClosure, Reason: "bye"}, CauseClosed},
		{"net closed", net.ErrClosed, CauseClosed},
		{"eof", fmt.Errorf("read: %w", io.EOF), CauseClosed},
		{"auth sentinel", fmt.Errorf("handshake: %w", errAuthRejected), CauseAuth},
		{"remote tls alert string", errors.New("remote error: tls: bad certificate"), CauseTLS},
		{"unknown", errors.New("something else entirely"), CauseUnknown},
		{"nil", nil, CauseUnknown},
	}
	for _, tc := range cases {
		if got := ConnectCauseOf(tc.err); got != tc.want {
			t.Errorf("%s: cause = %q, want %q (err: %v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// TestClassifyRealRefusedDial drives an actual loopback dial to a dead
// port — proves the classifier handles the real error chain produced by
// net/http + crypto/tls + net, not just hand-built ones.
func TestClassifyRealRefusedDial(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	addr := l.Addr().String()
	l.Close()

	_, err = (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), "tcp", addr)
	if err == nil {
		t.Fatal("dial to closed port unexpectedly succeeded")
	}
	if got := ConnectCauseOf(err); got != CauseRefused {
		t.Fatalf("real refused dial classified %q, want %q (err: %v)", got, CauseRefused, err)
	}
}

// TestConnectErrorWrap: ConnectError renders op+addr+err, unwraps, and
// ConnectCauseOf returns its stored cause verbatim.
func TestConnectErrorWrap(t *testing.T) {
	inner := errors.New("boom")
	ce := &ConnectError{Cause: CauseHTTP, Op: "dial", Addr: "wss://h:8443/x", Err: inner}
	if ce.Error() != "dial wss://h:8443/x: boom" {
		t.Fatalf("Error() = %q", ce.Error())
	}
	if !errors.Is(ce, inner) {
		t.Fatal("Unwrap should expose the inner error")
	}
	if got := ConnectCauseOf(fmt.Errorf("outer: %w", ce)); got != CauseHTTP {
		t.Fatalf("cause = %q, want %q", got, CauseHTTP)
	}
	// Empty Addr renders without a stray space.
	ce2 := &ConnectError{Cause: CauseProtocol, Op: "hello", Err: inner}
	if ce2.Error() != "hello: boom" {
		t.Fatalf("Error() = %q", ce2.Error())
	}
}
