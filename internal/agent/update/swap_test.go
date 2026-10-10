package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/localmesh/localmesh/internal/release"
)

// TestStageAndInstall exercises the full apply path: stage the artifact
// beside the target, verify its digest, atomically swap, and keep the
// previous binary as .bak.
func TestStageAndInstall(t *testing.T) {
	dir := t.TempDir()
	relDir := t.TempDir()
	content := []byte("new-binary-contents")
	if err := os.WriteFile(filepath.Join(relDir, "art"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "localmesh-agent")
	old := []byte("old-binary")
	if err := os.WriteFile(target, old, 0o755); err != nil {
		t.Fatal(err)
	}

	src, err := OpenSource(relDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := release.SumBytes(content)
	staged, err := StageArtifact(src, "art", target, sum)
	if err != nil {
		t.Fatal(err)
	}
	bak, err := InstallBinary(staged, target)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(content) {
		t.Fatal("target does not contain new binary")
	}
	bakBytes, _ := os.ReadFile(bak)
	if string(bakBytes) != string(old) {
		t.Fatal(".bak does not contain previous binary")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("staged file should have been renamed away")
	}
	// Executable bit preserved on unix.
	if info, _ := os.Stat(target); info.Mode()&0o111 == 0 {
		t.Fatal("installed target lost executable bit")
	}
}

func TestStageChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	relDir := t.TempDir()
	os.WriteFile(filepath.Join(relDir, "art"), []byte("x"), 0o644)
	target := filepath.Join(dir, "localmesh-agent")
	os.WriteFile(target, []byte("orig"), 0o755)

	src, _ := OpenSource(relDir, nil)
	_, err := StageArtifact(src, "art", target, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected sha256 mismatch, got %v", err)
	}
	if _, serr := os.Stat(target + stagedSuffix); !os.IsNotExist(serr) {
		t.Fatal("staged file should be removed on mismatch")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "orig" {
		t.Fatal("target was touched by a failed stage")
	}
}

func TestInstallIntoEmptyPath(t *testing.T) {
	// Fresh install: no existing target → no .bak, binary lands.
	dir := t.TempDir()
	relDir := t.TempDir()
	content := []byte("new")
	os.WriteFile(filepath.Join(relDir, "art"), content, 0o644)
	target := filepath.Join(dir, "localmesh-agent")

	src, _ := OpenSource(relDir, nil)
	staged, err := StageArtifact(src, "art", target, release.SumBytes(content))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallBinary(staged, target); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(content) {
		t.Fatal("target not installed")
	}
	if _, err := os.Stat(target + backupSuffix); !os.IsNotExist(err) {
		t.Fatal("no .bak should exist for a fresh install")
	}
}

// TestRollback verifies RollbackBinary restores .bak over target.
func TestRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "localmesh-agent")
	os.WriteFile(target, []byte("new-broken"), 0o755)
	os.WriteFile(target+backupSuffix, []byte("old-good"), 0o755)
	if err := RollbackBinary(target); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old-good" {
		t.Fatal("rollback did not restore .bak")
	}
	if _, err := os.Stat(target + backupSuffix); !os.IsNotExist(err) {
		t.Fatal(".bak should be consumed by rollback")
	}
}

func TestRollbackNoBak(t *testing.T) {
	dir := t.TempDir()
	if err := RollbackBinary(filepath.Join(dir, "localmesh-agent")); err == nil {
		t.Fatal("expected error when no .bak exists")
	}
}
