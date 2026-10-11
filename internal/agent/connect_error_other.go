//go:build !windows

package agent

// classifyPlatformErr is a no-op off Windows: unix socket failures
// already carry the errno values classifyConnectErr matches directly.
func classifyPlatformErr(error) (ConnectCause, bool) { return "", false }
