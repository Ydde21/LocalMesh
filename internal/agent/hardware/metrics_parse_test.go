package hardware

import "testing"

const meminfoFixture = `MemTotal:       16384000 kB
MemFree:         512000 kB
MemAvailable:    8192000 kB
Buffers:         102400 kB
Cached:         6144000 kB
SwapCached:            0 kB
Active:         4000000 kB
Inactive:       3000000 kB
`

func TestParseMeminfo(t *testing.T) {
	total, avail, ok := parseMeminfo(meminfoFixture)
	if !ok {
		t.Fatal("expected ok")
	}
	if total != 16384000*1024 {
		t.Errorf("total = %d", total)
	}
	if avail != 8192000*1024 {
		t.Errorf("avail = %d", avail)
	}
}

func TestParseMeminfoNoAvailable(t *testing.T) {
	// Pre-3.14 kernel: no MemAvailable field — must report not-measured.
	_, _, ok := parseMeminfo("MemTotal:       16384000 kB\nMemFree:         512000 kB\n")
	if ok {
		t.Fatal("expected ok=false without MemAvailable")
	}
}

func TestParseMeminfoGarbage(t *testing.T) {
	for _, s := range []string{"", "garbage\n", "MemTotal: nope\nMemAvailable: also-nope\n"} {
		if _, _, ok := parseMeminfo(s); ok {
			t.Fatalf("expected ok=false for %q", s)
		}
	}
}

// Memory-pressure boundaries: "available" is a *measurement*, so the
// parser must report extreme values honestly — including zero, which is
// a real "fully exhausted" reading, not a missing one.
func TestParseMeminfoPressure(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantTotal uint64
		wantAvail uint64
		wantOK    bool
	}{
		// The kernel reports 0 available under maximum reclaim
		// pressure — a valid, schedulable signal: the node has no
		// headroom. Distinct from unmeasured.
		{"zero available", "MemTotal:       16384000 kB\nMemAvailable:          0 kB\n",
			16384000 * 1024, 0, true},
		{"one kB left", "MemTotal:       16384000 kB\nMemAvailable:          1 kB\n",
			16384000 * 1024, 1024, true},
		// Weird-but-consistent: kernels under heavy cgroup pressure can
		// report avail > total momentarily on old versions; the parser
		// passes values through — classification lives in the sampler.
		{"avail exceeds total", "MemTotal:       16384000 kB\nMemAvailable:   20000000 kB\n",
			16384000 * 1024, 20000000 * 1024, true},
		// 512 GiB+ nodes: no truncation.
		{"huge total", "MemTotal:       536870912 kB\nMemAvailable:   268435456 kB\n",
			536870912 * 1024, 268435456 * 1024, true},
		// Later field wins — matches the sequential-scan semantics.
		{"duplicate fields last-wins", "MemTotal:       16384000 kB\nMemAvailable:    8192000 kB\nMemAvailable:    4096000 kB\n",
			16384000 * 1024, 4096000 * 1024, true},
		{"missing unit still parses", "MemTotal:       16384000\nMemAvailable:    8192000 kB\n",
			16384000 * 1024, 8192000 * 1024, true},
		// Partial reads still return what parsed; ok=false is what the
		// caller must gate on.
		{"avail without total", "MemAvailable:    8192000 kB\n",
			0, 8192000 * 1024, false},
		{"negative field rejected", "MemTotal:       -1024 kB\nMemAvailable:    8192000 kB\n",
			0, 8192000 * 1024, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, avail, ok := parseMeminfo(tc.in)
			if ok != tc.wantOK || total != tc.wantTotal || avail != tc.wantAvail {
				t.Errorf("parseMeminfo(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.in, total, avail, ok, tc.wantTotal, tc.wantAvail, tc.wantOK)
			}
		})
	}
}

const procStatFixture = "cpu  4705 356 5844 369900 1966 0 234 0 0 0\ncpu0 1000 100 1500 90000 500 0 60 0 0 0\n"

func TestParseCPUStatLine(t *testing.T) {
	ct, ok := parseCPUStatLine("cpu  4705 356 5844 369900 1966 0 234 0 0 0")
	if !ok {
		t.Fatal("expected ok")
	}
	wantTotal := uint64(4705 + 356 + 5844 + 369900 + 1966 + 0 + 234)
	if ct.total != wantTotal {
		t.Errorf("total = %d, want %d", ct.total, wantTotal)
	}
	if ct.busy != wantTotal-369900-1966 {
		t.Errorf("busy = %d", ct.busy)
	}
}

func TestParseCPUStatLineBad(t *testing.T) {
	for _, s := range []string{
		"", "cpu0 1 2 3 4", "cpu  1 2 x 4", "intr 123", "cpu",
	} {
		if _, ok := parseCPUStatLine(s); ok {
			t.Fatalf("expected ok=false for %q", s)
		}
	}
}

func TestCPUDeltaPercent(t *testing.T) {
	a, _ := parseCPUStatLine("cpu  100 0 100 700 100 0 0 0")
	b, _ := parseCPUStatLine("cpu  150 0 150 800 100 0 0 0")
	p, ok := cpuDeltaPercent(a, b)
	if !ok {
		t.Fatal("expected ok")
	}
	// 100 busy of 200 total jiffies elapsed → 50%.
	if p != 50 {
		t.Errorf("pct = %v, want 50", p)
	}
	if _, ok := cpuDeltaPercent(b, a); ok {
		t.Fatal("expected ok=false for non-advancing counters")
	}
	if _, ok := cpuDeltaPercent(a, a); ok {
		t.Fatal("expected ok=false for identical samples")
	}
}

func TestCPUDeltaPercentEdge(t *testing.T) {
	// Total advanced but busy counter reset (jiffies wrap/CPU
	// hot-unplug): cannot produce an honest delta → unmeasured.
	a, _ := parseCPUStatLine("cpu  100 0 100 700 100 0 0 0")
	wrap, _ := parseCPUStatLine("cpu  10 0 10 800 100 0 0 0")
	if _, ok := cpuDeltaPercent(a, wrap); ok {
		t.Fatal("expected ok=false when busy counter regressed")
	}
	// 100% busy window: every elapsed jiffy was busy → 100, not a clamp.
	busy, _ := parseCPUStatLine("cpu  300 0 300 700 100 0 0 0")
	if p, ok := cpuDeltaPercent(a, busy); !ok || p != 100 {
		t.Fatalf("full busy = %v,%v want 100,true", p, ok)
	}
	// Idle window: total advanced, busy did not → a genuine 0%.
	idle, _ := parseCPUStatLine("cpu  100 0 100 900 100 0 0 0")
	if p, ok := cpuDeltaPercent(a, idle); !ok || p != 0 {
		t.Fatalf("idle = %v,%v want 0,true", p, ok)
	}
}

const nvidiaSMIFixture = `37, 512
12, 1024
`

func TestParseNvidiaSMILoad(t *testing.T) {
	util, vram, utilOK, vramOK := parseNvidiaSMILoad(nvidiaSMIFixture)
	if !utilOK || !vramOK {
		t.Fatal("expected both flags")
	}
	if util != 24.5 {
		t.Errorf("util = %v, want 24.5", util)
	}
	if vram != uint64(1536)*(1<<20) {
		t.Errorf("vram = %d", vram)
	}
}

func TestParseNvidiaSMILoadPartial(t *testing.T) {
	// Util column parses, VRAM column does not → only util measured.
	util, _, utilOK, vramOK := parseNvidiaSMILoad("55, N/A\n")
	if !utilOK || vramOK {
		t.Fatalf("utilOK=%v vramOK=%v", utilOK, vramOK)
	}
	if util != 55 {
		t.Errorf("util = %v", util)
	}
}

func TestParseNvidiaSMILoadGarbage(t *testing.T) {
	for _, s := range []string{"", "\n", "no gpus here\n", "a, b, c\n"} {
		util, vram, uOK, vOK := parseNvidiaSMILoad(s)
		if uOK || vOK || util != 0 || vram != 0 {
			t.Fatalf("expected all-false/zero for %q, got %v %v %v %v", s, util, vram, uOK, vOK)
		}
	}
}

const vmStatFixture = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                     4243.
Pages active:                                 183586.
Pages inactive:                               182172.
Pages speculative:                               546.
Pages wired down:                             159765.
`

func TestParseVMStat(t *testing.T) {
	avail, ok := parseVMStat(vmStatFixture)
	if !ok {
		t.Fatal("expected ok")
	}
	if avail != uint64(4243+182172)*16384 {
		t.Errorf("avail = %d", avail)
	}
}

func TestParseVMStatBad(t *testing.T) {
	for _, s := range []string{
		"",
		"Pages free: 100.\nPages inactive: 200.\n",                 // no page size
		"Mach Virtual Memory Statistics: (page size of x bytes)\n", // bad size
		"Pages free: nope.\n",                                      // bad counter
	} {
		if _, ok := parseVMStat(s); ok {
			t.Fatalf("expected ok=false for %q", s)
		}
	}
}

func TestParseVMStatPressure(t *testing.T) {
	// Zero free+inactive is a REAL reading under full memory pressure
	// (macOS compresses/swaps rather than reporting nothing) — ok must
	// stay true: 0 available is schedulable signal, not "unmeasured".
	avail, ok := parseVMStat("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free: 0.\nPages inactive: 0.\n")
	if !ok || avail != 0 {
		t.Fatalf("full pressure = (%d, %v), want (0, true)", avail, ok)
	}
	// Intel-Mac 4 KiB page size path (vs the 16 KiB fixture).
	avail, ok = parseVMStat("Mach Virtual Memory Statistics: (page size of 4096 bytes)\n" +
		"Pages free: 1000.\nPages inactive: 500.\n")
	if !ok || avail != 1500*4096 {
		t.Fatalf("4k pages = (%d, %v), want (%d, true)", avail, ok, 1500*4096)
	}
	// Page size present but no free counter → unmeasured, not zero.
	if _, ok = parseVMStat("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages inactive: 100.\n"); ok {
		t.Fatal("expected ok=false without Pages free")
	}
}

func TestMetricsFlagContract(t *testing.T) {
	m := Metrics(t.Context(), 0, nil)
	if m.LoadedMeasured {
		t.Fatal("nil loaded must not be marked measured")
	}
	m = Metrics(t.Context(), 1, []string{})
	if !m.LoadedMeasured {
		// empty non-nil slice still counts as observed engine state
		t.Fatal("non-nil loaded should mark measured")
	}
	m = Metrics(t.Context(), 1, []string{"llama3.1:8b"})
	if !m.LoadedMeasured {
		t.Fatal("expected loaded_measured")
	}
	if m.ActiveRequests != 1 {
		t.Errorf("active = %d", m.ActiveRequests)
	}
}
