package agent

import (
	"runtime"
	"strings"
	"testing"

	"github.com/localmesh/localmesh/internal/agent/hardware"
	"github.com/localmesh/localmesh/internal/contracts"
)

func TestCurrent(t *testing.T) {
	hardware.SetVersion("v9.9.9-test")
	t.Cleanup(func() { hardware.SetVersion("dev") })
	i := Current()
	if i.Version != "v9.9.9-test" {
		t.Errorf("Version = %q", i.Version)
	}
	if i.Protocol != contracts.ProtocolVersion {
		t.Errorf("Protocol = %d, want %d", i.Protocol, contracts.ProtocolVersion)
	}
	if i.GoVersion != runtime.Version() || i.OS != runtime.GOOS || i.Arch != runtime.GOARCH {
		t.Errorf("runtime fields wrong: %+v", i)
	}
}

func TestInfoString(t *testing.T) {
	i := Info{Version: "v0.5.0", Protocol: 1, GoVersion: "go1.25", OS: "linux", Arch: "amd64"}
	want := "localmesh-agent v0.5.0 proto=1 linux/amd64 go1.25"
	if got := i.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestInfoStringParseRoundTrip(t *testing.T) {
	in := Info{Version: "v0.5.0", Protocol: 1, GoVersion: "go1.25.3", OS: "darwin", Arch: "arm64"}
	out, err := Parse(in.String())
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip: %+v != %+v", out, in)
	}
}

func TestParse(t *testing.T) {
	i, err := Parse("localmesh-agent dev proto=2 windows/amd64 go1.25")
	if err != nil {
		t.Fatal(err)
	}
	if i.Version != "dev" || i.Protocol != 2 || i.OS != "windows" ||
		i.Arch != "amd64" || i.GoVersion != "go1.25" {
		t.Fatalf("parsed = %+v", i)
	}
}

func TestParseBad(t *testing.T) {
	for _, s := range []string{
		"",
		"localmesh-agent",
		"localmesh-agent v0.5.0 proto=1 linux/amd64",      // missing go version
		"localmesh-agent v0.5.0 proto=1 linux/amd64 go x", // too many fields
		"localmesh-agent v0.5.0 proto=x linux/amd64 go1.25",
		"localmesh-agent v0.5.0 1 linux/amd64 go1.25",      // no proto= prefix
		"localmesh-agent v0.5.0 proto=1 linuxamd64 go1.25", // no os/arch slash
		"localmesh-agent v0.5.0 proto=1 linux/ go1.25",     // empty arch
		"localmesh-agent v0.5.0 proto=1 /amd64 go1.25",     // empty os
		"localmesh-agent v0.5.0 proto=1 linux/amd64 1.25",  // no go prefix
		"other-binary v0.5.0 proto=1 linux/amd64 go1.25",
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) should fail", s)
		}
	}
}

func TestCompatible(t *testing.T) {
	ours := contracts.ProtocolVersion
	cases := []struct {
		name        string
		serverProto int
		minProto    int
		wantOK      bool
		wantSubstr  string
	}{
		{"same version", ours, ours, true, "compatible"},
		{"server newer, accepts us", ours + 2, ours, true, "compatible"},
		{"server newer, min below us", ours + 2, ours - 1, true, "compatible"},
		{"agent older than server minimum", ours + 1, ours + 1, false, "older than"},
		{"agent far older than minimum", ours + 2, ours + 2, false, "older than"},
		{"agent newer than server", ours - 1, ours - 1, false, "newer than"},
		{"agent newer than server, min satisfied", ours - 1, ours - 2, false, "newer than"},
		{"zero-valued server proto", 0, 0, false, "newer than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, msg := Compatible(tc.serverProto, tc.minProto)
			if ok != tc.wantOK {
				t.Fatalf("Compatible(%d, %d) ok=%v, want %v (msg %q)",
					tc.serverProto, tc.minProto, ok, tc.wantOK, msg)
			}
			if !strings.Contains(msg, tc.wantSubstr) {
				t.Errorf("msg %q should mention %q", msg, tc.wantSubstr)
			}
			// The message should always carry the protocol numbers so an
			// operator can act on it.
			if !strings.Contains(msg, "v") {
				t.Errorf("msg %q should name a protocol version", msg)
			}
		})
	}
}
