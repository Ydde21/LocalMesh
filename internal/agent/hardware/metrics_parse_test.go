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
