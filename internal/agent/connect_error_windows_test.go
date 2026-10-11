//go:build windows

package agent

import (
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestClassifyWSAErrs: Windows socket failures arrive as raw WSA codes
// inside os.SyscallError (connectex on dial, wsarecv/wsasend on an
// established conn) — the invented unix constants never match them.
// Each must land in the same cause bucket its unix errno sibling does.
func TestClassifyWSAErrs(t *testing.T) {
	wrap := func(syscallName string, errno syscall.Errno) error {
		return &url.Error{Op: "Get", URL: "wss://cp.example/connect",
			Err: &net.OpError{Op: "dial", Net: "tcp",
				Err: &os.SyscallError{Syscall: syscallName, Err: errno}}}
	}
	cases := []struct {
		name string
		err  error
		want ConnectCause
	}{
		{"connectex refused", wrap("connectex", windows.WSAECONNREFUSED), CauseRefused},
		{"connectex timeout", wrap("connectex", windows.WSAETIMEDOUT), CauseTimeout},
		{"connectex host unreachable", wrap("connectex", windows.WSAEHOSTUNREACH), CauseUnreachable},
		{"connectex net unreachable", wrap("connectex", windows.WSAENETUNREACH), CauseUnreachable},
		{"connectex net down", wrap("connectex", windows.WSAENETDOWN), CauseUnreachable},
		{"lookup host not found", wrap("getaddrinfo", windows.WSAHOST_NOT_FOUND), CauseDNS},
		{"lookup try again", wrap("getaddrinfo", windows.WSATRY_AGAIN), CauseDNS},
		{"wsarecv reset", wrap("wsarecv", windows.WSAECONNRESET), CauseClosed},
		{"wsasend aborted", wrap("wsasend", windows.WSAECONNABORTED), CauseClosed},
		{"wsarecv shutdown", wrap("wsarecv", windows.WSAESHUTDOWN), CauseClosed},
		{"unmapped wsa", wrap("connectex", windows.WSAEACCES), CauseUnknown},
	}
	for _, tc := range cases {
		if got := ConnectCauseOf(tc.err); got != tc.want {
			t.Errorf("%s: cause = %q, want %q (err: %v)", tc.name, got, tc.want, tc.err)
		}
	}
}
