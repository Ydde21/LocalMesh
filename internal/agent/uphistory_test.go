package agent

// uphistory_test.go covers the update-history file itself: round-trip,
// capping, corruption fail-open, the tripwire match rules, and the
// status/doctor summary lines.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

// historyTestAgent returns an agent whose update state dir is dir.
func historyTestAgent(t *testing.T, dir string) *Agent {
	t.Helper()
	a := &Agent{cfg: &Config{ServerURL: "http://x", NodeID: "n", NodeToken: "t"}, log: testLogger()}
	a.SetConfigPath(filepath.Join(dir, "agent.json"))
	return a
}

func TestUpdateHistoryRoundTrip(t *testing.T) {
	a := historyTestAgent(t, t.TempDir())
	path, err := a.updateHistoryPath()
	if err != nil {
		t.Fatal(err)
	}
	a.recordUpdateOutcome(updateHistoryEntry{
		OperationID: "op-1", ReleaseID: "r1", KeyID: "k1",
		ToVersion: "0.8.0", Outcome: upOutcomeFailed, Detail: "insufficient disk",
	})
	a.recordUpdateOutcome(updateHistoryEntry{
		OperationID: "op-2", ReleaseID: "r2", KeyID: "k1",
		ToVersion: "0.8.1", Outcome: upOutcomeRolledBack, Restarted: true,
	})
	h, err := readUpdateHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 {
		t.Fatalf("history len = %d, want 2", len(h))
	}
	if h[0].OperationID != "op-1" || h[0].Detail != "insufficient disk" || h[0].Restarted {
		t.Fatalf("entry[0] = %+v", h[0])
	}
	if h[1].OperationID != "op-2" || h[1].Outcome != upOutcomeRolledBack || !h[1].Restarted {
		t.Fatalf("entry[1] = %+v", h[1])
	}
	if h[0].FinishedAt.IsZero() {
		t.Fatal("FinishedAt was not defaulted")
	}
	if runtime.GOOS != "windows" {
		// Windows reports 0666 for writable files — no unix perm bits.
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("history file mode = %v, %v", st.Mode().Perm(), err)
		}
	}
}

func TestUpdateHistoryCapped(t *testing.T) {
	a := historyTestAgent(t, t.TempDir())
	path, _ := a.updateHistoryPath()
	for i := 0; i < maxUpdateHistoryEntries+5; i++ {
		a.recordUpdateOutcome(updateHistoryEntry{
			OperationID: fmt.Sprintf("op-%d", i),
			ToVersion:   "0.8.0", Outcome: upOutcomeFailed,
		})
	}
	h, err := readUpdateHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != maxUpdateHistoryEntries {
		t.Fatalf("history len = %d, want cap %d", len(h), maxUpdateHistoryEntries)
	}
	// Oldest entries were dropped: the first survivor is op-5.
	if h[0].OperationID != "op-5" {
		t.Fatalf("oldest surviving entry = %+v, want op-5", h[0])
	}
}

func TestUpdateHistoryCorruptFailsOpen(t *testing.T) {
	a := historyTestAgent(t, t.TempDir())
	path, _ := a.updateHistoryPath()
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readUpdateHistory(path); err == nil {
		t.Fatal("corrupt history should report an error")
	}
	// The tripwire and summary must fail open, never panic or block.
	u := contracts.AgentUpgrade{OperationID: "op", Version: "0.8.0", ReleaseID: "r1", KeyID: "k"}
	if _, blocked := a.blockedByHistory(u); blocked {
		t.Fatal("corrupt history must not block an update")
	}
	// Recording over the corruption starts a fresh file.
	a.recordUpdateOutcome(updateHistoryEntry{ToVersion: "0.8.0", Outcome: upOutcomeFailed})
	h, err := readUpdateHistory(path)
	if err != nil || len(h) != 1 {
		t.Fatalf("history after re-record = %v, %v", h, err)
	}
}

func TestHistoryTripwireRules(t *testing.T) {
	u := contracts.AgentUpgrade{OperationID: "op", Version: "0.8.0", ReleaseID: "r42", KeyID: "k1"}
	base := updateHistoryEntry{
		ReleaseID: "r42", KeyID: "k1", ToVersion: "0.8.0",
		Outcome: upOutcomeRolledBack, Restarted: true,
	}
	cases := []struct {
		name string
		mod  func(e *updateHistoryEntry)
		modU func(u *contracts.AgentUpgrade)
		want bool
	}{
		{"matching rolled_back blocks", nil, nil, true},
		{"restarted failed blocks", func(e *updateHistoryEntry) { e.Outcome = upOutcomeFailed }, nil, true},
		{"succeeded never blocks", func(e *updateHistoryEntry) { e.Outcome = upOutcomeSucceeded }, nil, false},
		{"pre-install failure never blocks", func(e *updateHistoryEntry) {
			e.Outcome = upOutcomeFailed
			e.Restarted = false
		}, nil, false},
		{"abort never blocks", func(e *updateHistoryEntry) { e.Outcome = upOutcomeAborted }, nil, false},
		{"version mismatch", func(e *updateHistoryEntry) { e.ToVersion = "0.8.1" }, nil, false},
		{"v-prefix versions equal", func(e *updateHistoryEntry) { e.ToVersion = "v0.8.0" }, nil, true},
		{"release mismatch", func(e *updateHistoryEntry) { e.ReleaseID = "r99" }, nil, false},
		{"key mismatch", func(e *updateHistoryEntry) { e.KeyID = "k2" }, nil, false},
		{"empty dispatch release matches any", nil, func(u *contracts.AgentUpgrade) { u.ReleaseID = "" }, true},
		{"empty dispatch key matches any", nil, func(u *contracts.AgentUpgrade) { u.KeyID = "" }, true},
	}
	for _, tc := range cases {
		e, uu := base, u
		if tc.mod != nil {
			tc.mod(&e)
		}
		if tc.modU != nil {
			tc.modU(&uu)
		}
		if got := historyTripwire(e, uu); got != tc.want {
			t.Fatalf("%s: tripwire = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestUpdateHistorySummary(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.json")
	if s := UpdateHistorySummary(cfgPath); s != "" {
		t.Fatalf("no history should give empty summary, got %q", s)
	}
	a := historyTestAgent(t, dir)
	a.recordUpdateOutcome(updateHistoryEntry{
		OperationID: "op-9", ReleaseID: "r9", ToVersion: "0.8.0",
		Outcome: upOutcomeRolledBack, Restarted: true,
		FinishedAt: time.Date(2026, 2, 16, 3, 4, 5, 0, time.UTC),
	})
	s := UpdateHistorySummary(cfgPath)
	if !strings.Contains(s, "0.8.0") || !strings.Contains(s, "rolled_back") ||
		!strings.Contains(s, "op-9") {
		t.Fatalf("summary = %q", s)
	}
}

// TestDoctorUpdateHistoryEmit: doctor warns on a failed update and
// passes a confirmed one; silence when no update has ever run.
func TestDoctorUpdateHistoryEmit(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.json")
	var lines []string
	emit := func(status, name, format string, a ...any) {
		lines = append(lines, status+" "+name+": "+fmt.Sprintf(format, a...))
	}
	doctorUpdateHistory(cfgPath, emit)
	if len(lines) != 0 {
		t.Fatalf("no history should emit nothing, got %v", lines)
	}
	ag := historyTestAgent(t, dir)
	ag.recordUpdateOutcome(updateHistoryEntry{
		OperationID: "op-bad", ToVersion: "0.8.0",
		Outcome: upOutcomeRolledBack, Restarted: true,
	})
	doctorUpdateHistory(cfgPath, emit)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN update-history") ||
		!strings.Contains(lines[0], "rolled_back") {
		t.Fatalf("rollback emit = %v", lines)
	}
	ag.recordUpdateOutcome(updateHistoryEntry{
		OperationID: "op-ok", ToVersion: "0.8.1",
		Outcome: upOutcomeSucceeded, Restarted: true,
	})
	doctorUpdateHistory(cfgPath, emit)
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "PASS update-history") ||
		!strings.Contains(lines[1], "0.8.1") {
		t.Fatalf("success emit = %v", lines)
	}
}
