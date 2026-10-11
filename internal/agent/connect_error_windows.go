//go:build windows

package agent

import (
	"errors"

	"golang.org/x/sys/windows"
)

// classifyPlatformErr maps the raw Winsock codes a Windows socket
// failure carries (connectex on dial, wsarecv/wsasend mid-session).
// The unix errno constants in classifyConnectErr are invented values
// on Windows — they never equal a real WSA error — so a refused dial
// or dead peer would otherwise fall through to "unknown".
func classifyPlatformErr(err error) (ConnectCause, bool) {
	switch {
	case errors.Is(err, windows.WSAECONNREFUSED):
		return CauseRefused, true
	case errors.Is(err, windows.WSAETIMEDOUT):
		return CauseTimeout, true
	case errors.Is(err, windows.WSAEHOSTUNREACH), errors.Is(err, windows.WSAENETUNREACH),
		errors.Is(err, windows.WSAENETDOWN):
		return CauseUnreachable, true
	case errors.Is(err, windows.WSAHOST_NOT_FOUND), errors.Is(err, windows.WSANO_DATA),
		errors.Is(err, windows.WSANO_RECOVERY), errors.Is(err, windows.WSATRY_AGAIN):
		return CauseDNS, true
	case errors.Is(err, windows.WSAECONNRESET),
		errors.Is(err, windows.WSAECONNABORTED),
		errors.Is(err, windows.WSAESHUTDOWN):
		return CauseClosed, true
	}
	return "", false
}
