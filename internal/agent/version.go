package agent

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/localmesh/localmesh/internal/agent/hardware"
	"github.com/localmesh/localmesh/internal/contracts"
)

// Info describes the running agent build: the stamped release version,
// the wire protocol it speaks, and the toolchain/platform it was built
// for. Version is "dev" for unstamped builds — the release version is
// injected via `-ldflags -X main.version=...` → hardware.SetVersion.
type Info struct {
	Version   string // build stamp, e.g. "v0.5.0" (or "dev")
	Protocol  int    // wire protocol this agent speaks
	GoVersion string // runtime.Version(), e.g. "go1.25.3"
	OS        string // runtime.GOOS
	Arch      string // runtime.GOARCH
}

// Current returns the build info for this binary.
func Current() Info {
	return Info{
		Version:   hardware.Version(),
		Protocol:  contracts.ProtocolVersion,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String renders the one-line form `localmesh-agent version` prints:
//
//	localmesh-agent v0.5.0 proto=1 linux/amd64 go1.25
func (i Info) String() string {
	return fmt.Sprintf("localmesh-agent %s proto=%d %s/%s %s",
		i.Version, i.Protocol, i.OS, i.Arch, i.GoVersion)
}

// Parse reads the output of Info.String back into an Info — used by
// fleet tooling that compares `localmesh-agent version` output across
// machines.
func Parse(s string) (Info, error) {
	f := strings.Fields(s)
	if len(f) != 5 || f[0] != "localmesh-agent" {
		return Info{}, fmt.Errorf("unrecognized version line %q", s)
	}
	if !strings.HasPrefix(f[2], "proto=") {
		return Info{}, fmt.Errorf("bad proto field in %q", s)
	}
	proto, err := strconv.Atoi(strings.TrimPrefix(f[2], "proto="))
	if err != nil {
		return Info{}, fmt.Errorf("bad proto field in %q", s)
	}
	osArch := strings.SplitN(f[3], "/", 2)
	if len(osArch) != 2 || osArch[0] == "" || osArch[1] == "" {
		return Info{}, fmt.Errorf("bad os/arch field in %q", s)
	}
	if !strings.HasPrefix(f[4], "go") {
		return Info{}, fmt.Errorf("bad go-version field in %q", s)
	}
	return Info{
		Version:   f[1],
		Protocol:  proto,
		OS:        osArch[0],
		Arch:      osArch[1],
		GoVersion: f[4],
	}, nil
}

// Compatible checks this agent's protocol against what the server
// advertised in HelloAck. It is the agent-side friendly message only —
// the server enforces MinAgentProtocolVersion authoritatively; this
// exists so the agent logs an actionable upgrade hint instead of a bare
// disconnect.
//
//	ours < minProto    → agent too old: upgrade the agent
//	ours > serverProto → agent too new: upgrade the control plane
//	otherwise          → compatible
func Compatible(serverProto, minProto int) (ok bool, msg string) {
	ours := contracts.ProtocolVersion
	switch {
	case ours < minProto:
		return false, fmt.Sprintf(
			"agent protocol v%d is older than this server's minimum v%d — upgrade the agent (see docs/agent-updates.md)",
			ours, minProto)
	case ours > serverProto:
		return false, fmt.Sprintf(
			"agent protocol v%d is newer than this server's v%d — upgrade the control plane, or downgrade the agent",
			ours, serverProto)
	default:
		return true, fmt.Sprintf(
			"protocol v%d compatible with server v%d (server accepts v%d+)",
			ours, serverProto, minProto)
	}
}
