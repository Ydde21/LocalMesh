package release

import (
	"testing"
	"time"
)

// TestCompareVersions: ordering contract for the anti-rollback floor —
// [v]X.Y.Z, v-prefix tolerated, pre/build suffixes ignored.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.10.0", "0.10.0", 0},
		{"v0.10.0", "0.10.0", 0},
		{"0.9.9", "0.10.0", -1},
		{"0.10.0", "0.9.9", 1},
		{"0.10.1", "0.10.0", 1},
		{"1.0.0", "0.99.9", 1},
		{"0.10.0", "0.10.10", -1},
		{"0.10.0-rc1", "0.10.0", 0},
		{"0.10.0+build5", "0.10.0", 0},
		{"2.0.0", "10.0.0", -1},
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil {
			t.Fatalf("CompareVersions(%q,%q) error: %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestCompareVersionsRejects: malformed versions are errors — the floor
// must fail closed on input it cannot order, never guess.
func TestCompareVersionsRejects(t *testing.T) {
	for _, bad := range []string{"", "1.2", "1.2.3.4", "a.b.c", "1..3", "1.2.-3", "latest", "0.x.0"} {
		if _, err := CompareVersions(bad, "1.0.0"); err == nil {
			t.Errorf("CompareVersions(%q, 1.0.0) = nil error, want rejection", bad)
		}
		if _, err := CompareVersions("1.0.0", bad); err == nil {
			t.Errorf("CompareVersions(1.0.0, %q) = nil error, want rejection", bad)
		}
	}
}

// TestVersionLTFailsClosed: when either side is unparseable the answer
// is "below floor" — a corrupt floor or target can only block a
// rollback path, never silently permit one.
func TestVersionLTFailsClosed(t *testing.T) {
	if !VersionLT("garbage", "0.10.0") {
		t.Error("unparseable target must report below-floor")
	}
	if !VersionLT("0.9.0", "0.10.0") {
		t.Error("0.9.0 < 0.10.0 must be true")
	}
	if VersionLT("0.10.0", "0.10.0") {
		t.Error("equal is not below")
	}
	if VersionLT("0.11.0", "0.10.0") {
		t.Error("0.11.0 is not below 0.10.0")
	}
}

// TestManifestValidateMinAgentVersion: the floor must parse, must not
// exceed the release's own version, and stays optional.
func TestManifestValidateMinAgentVersion(t *testing.T) {
	base := func() *Manifest {
		return &Manifest{
			Version: "0.10.0", ProtocolVersion: 1, MinProtocolVersion: 1,
			CreatedAt: time.Now().UTC(), Signer: Signer{Algorithm: "minisign"},
			Artifacts: []Artifact{{
				File: "localmesh-agent-0.10.0-linux-amd64", OS: "linux", Arch: "amd64",
				SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1,
			}},
		}
	}
	m := base()
	if err := m.Validate(); err != nil {
		t.Fatalf("no floor must be valid: %v", err)
	}
	m = base()
	m.MinAgentVersion = "0.10.0"
	if err := m.Validate(); err != nil {
		t.Fatalf("floor == version must be valid: %v", err)
	}
	m = base()
	m.MinAgentVersion = "0.9.0"
	if err := m.Validate(); err != nil {
		t.Fatalf("floor below version must be valid: %v", err)
	}
	m = base()
	m.MinAgentVersion = "v0.9.0"
	if err := m.Validate(); err != nil {
		t.Fatalf("v-prefixed floor must be valid: %v", err)
	}
	m = base()
	m.MinAgentVersion = "0.11.0"
	if err := m.Validate(); err == nil {
		t.Fatal("floor above version must be rejected — a release cannot floor above itself")
	}
	m = base()
	m.MinAgentVersion = "soon"
	if err := m.Validate(); err == nil {
		t.Fatal("unparseable floor must be rejected")
	}
}
