package release

import (
	"fmt"
	"strconv"
	"strings"
)

// CompareVersions orders two release version strings. It returns -1, 0,
// or +1 as a < b, a == b, or a > b. Both inputs are parsed as
// [v]MAJOR.MINOR.PATCH — a leading "v" is tolerated, "-pre" and "+build"
// suffixes are ignored (prerelease ordering is not a LocalMesh concept;
// versions never ship with them). This is deliberately NOT the wire
// protocol window (manifest protocol_version) — it orders agent binary
// versions for the min_agent_version anti-rollback floor.
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", a, err)
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", b, err)
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

// VersionLT reports a < b under CompareVersions. Unparseable inputs are
// conservative: an unparseable floor or target fails closed (returns
// true) so a corrupt manifest can only block, never silently permit.
func VersionLT(a, b string) bool {
	c, err := CompareVersions(a, b)
	return err != nil || c < 0
}

// parseVersion splits "[v]X.Y.Z" into numeric components.
func parseVersion(s string) ([3]int, error) {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("want X.Y.Z, got %d components", len(parts))
	}
	for i, p := range parts {
		if p == "" {
			return v, fmt.Errorf("empty component %d", i)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("component %q is not a non-negative integer", p)
		}
		v[i] = n
	}
	return v, nil
}
