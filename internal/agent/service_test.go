package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The install guard must fire before any service-manager call — the
// failure paths below are the only ones a unit test can exercise
// without registering a real launchd/systemd/SCM service.
func TestServiceInstallRequiresEnrolledConfig(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "agent.json")
	if err := Service("install", missing, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "join") {
		t.Fatalf("install without config should fail with a join hint, got %v", err)
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	os.WriteFile(corrupt, []byte("{nope"), 0o600)
	if err := Service("install", corrupt, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "join") {
		t.Fatalf("install with corrupt config should fail with a join hint, got %v", err)
	}
}

func TestServiceUnknownAction(t *testing.T) {
	err := Service("bogus", filepath.Join(t.TempDir(), "agent.json"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown service action") {
		t.Fatalf("bad action should be rejected before anything else, got %v", err)
	}
}
