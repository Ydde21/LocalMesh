package hardware

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Fixture-driven coverage of the nvidia-smi telemetry path. Two layers:
// parseNvidiaSMILoad table tests (pure output → values + measured flags)
// and exec-path tests that swap commandContext for a helper process so
// missing-binary / non-zero-exit cases run on any CI runner — none of
// this file needs a real GPU.
//
// Contract under test: a *_measured flag is true only when at least one
// GPU really reported that field this cycle. Unknown is not zero.

func TestParseNvidiaSMILoadTable(t *testing.T) {
	const mib = uint64(1 << 20)
	cases := []struct {
		name       string
		out        string
		wantUtil   float64
		wantVRAM   uint64
		wantUtilOK bool
		wantVRAMOK bool
	}{
		{"single GPU", "37, 512\n", 37, 512 * mib, true, true},
		{"multi GPU", "37, 512\n12, 1024\n", 24.5, 1536 * mib, true, true},
		{"four GPUs", "100, 24576\n99, 24576\n98, 24576\n87, 24576\n",
			96, 98304 * mib, true, true},
		// A GPU idling at 0% with 0 MiB used is a real measurement —
		// flags must stay true. Unknown ≠ zero.
		{"zero util and vram", "0, 0\n", 0, 0, true, true},
		{"zero across two GPUs", "0, 0\n0, 0\n", 0, 0, true, true},
		// 80 GiB-class card (H100/A100-80G): no overflow, no clamping.
		{"large vram", "45, 81920\n", 45, 81920 * mib, true, true},
		{"fractional values", "12.5, 1536.5\n", 12.5, uint64(1536.5 * float64(mib)), true, true},
		{"crlf line endings", "37, 512\r\n12, 1024\r\n", 24.5, 1536 * mib, true, true},
		{"extra whitespace", "  61 ,  2048  \n", 61, 2048 * mib, true, true},
		// Per-field degradation: a field nvidia-smi could not read comes
		// back "N/A" — that field's flag drops, the other still counts.
		{"util only", "55, N/A\n", 55, 0, true, false},
		{"vram only", "N/A, 512\n", 0, 512 * mib, false, true},
		{"neither field", "N/A, N/A\n", 0, 0, false, false},
		{"mixed healthy and partial", "55, N/A\n12, 1024\n", 33.5, 1024 * mib, true, true},
		{"util from one of two GPUs", "N/A, 512\n40, N/A\n", 40, 512 * mib, true, true},
		// Malformed / absent output — everything unmeasured.
		{"empty", "", 0, 0, false, false},
		{"blank line", "\n", 0, 0, false, false},
		{"driver error text", "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n",
			0, 0, false, false},
		{"one column", "37\n", 0, 0, false, false},
		{"three columns", "37, 512, extra\n", 0, 0, false, false},
		{"trailing comma", "37,\n", 37, 0, true, false}, // empty vram field → unmeasured
		{"non-numeric", "a, b\n", 0, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			util, vram, utilOK, vramOK := parseNvidiaSMILoad(tc.out)
			if util != tc.wantUtil || vram != tc.wantVRAM ||
				utilOK != tc.wantUtilOK || vramOK != tc.wantVRAMOK {
				t.Errorf("parseNvidiaSMILoad(%q) = (%v, %v, %v, %v), want (%v, %v, %v, %v)",
					tc.out, util, vram, utilOK, vramOK,
					tc.wantUtil, tc.wantVRAM, tc.wantUtilOK, tc.wantVRAMOK)
			}
		})
	}
}

// --- exec path -------------------------------------------------------------

// fakeSMIEnv marks the helper subprocess; TestHelperProcess exits
// immediately unless it is set.
const fakeSMIEnv = "GO_LOCALMESH_FAKE_SMI"

// TestHelperProcess re-executes the test binary as a stand-in for
// nvidia-smi: it prints FAKE_SMI_STDOUT/STDERR and exits FAKE_SMI_EXIT.
// (Standard os/exec test pattern — see src/os/exec/exec_test.go.)
func TestHelperProcess(t *testing.T) {
	if os.Getenv(fakeSMIEnv) != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv("FAKE_SMI_STDOUT"))
	fmt.Fprint(os.Stderr, os.Getenv("FAKE_SMI_STDERR"))
	code, _ := strconv.Atoi(os.Getenv("FAKE_SMI_EXIT"))
	os.Exit(code)
}

// stubCommandContext swaps the exec seam for the duration of a test.
// stdout=="" and exit==0 still produce a runnable command; pass
// missingBinary to simulate ENOENT instead.
func stubCommandContext(t *testing.T, stdout, stderr string, exitCode int, missingBinary bool) {
	t.Helper()
	old := commandContext
	if missingBinary {
		commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			// A name that can never resolve — exercises the real
			// exec.LookPath ENOENT path inside Output().
			return exec.CommandContext(ctx, "localmesh-nonexistent-nvidia-smi")
		}
	} else {
		commandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			cs := append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)
			cmd := exec.CommandContext(ctx, os.Args[0], cs...)
			cmd.Env = append(os.Environ(),
				fakeSMIEnv+"=1",
				"FAKE_SMI_STDOUT="+stdout,
				"FAKE_SMI_STDERR="+stderr,
				"FAKE_SMI_EXIT="+strconv.Itoa(exitCode),
			)
			return cmd
		}
	}
	t.Cleanup(func() { commandContext = old })
}

func TestSampleGPUMissingBinary(t *testing.T) {
	// nvidia-smi not on PATH: every flag false, no values — a machine
	// without NVIDIA telemetry is unmeasured, not idle.
	stubCommandContext(t, "", "", 0, true)
	util, vram, utilOK, vramOK := sampleGPU(t.Context())
	if util != 0 || vram != 0 || utilOK || vramOK {
		t.Fatalf("got (%v,%v,%v,%v), want all zero/false", util, vram, utilOK, vramOK)
	}
}

func TestSampleGPUNonZeroExit(t *testing.T) {
	// Driver present but not loaded (or GPU wedged): nvidia-smi exits 9
	// with a diagnostic on stderr. Output() returns ExitError → unmeasured.
	stubCommandContext(t, "",
		"NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.", 9, false)
	util, vram, utilOK, vramOK := sampleGPU(t.Context())
	if util != 0 || vram != 0 || utilOK || vramOK {
		t.Fatalf("got (%v,%v,%v,%v), want all zero/false", util, vram, utilOK, vramOK)
	}
}

func TestSampleGPUValidOutput(t *testing.T) {
	stubCommandContext(t, "61, 2048\n23, 4096\n", "", 0, false)
	util, vram, utilOK, vramOK := sampleGPU(t.Context())
	if !utilOK || !vramOK {
		t.Fatal("expected both measured")
	}
	if util != 42 {
		t.Errorf("util = %v, want 42", util)
	}
	if vram != 6144*(1<<20) {
		t.Errorf("vram = %v", vram)
	}
}

func TestMetricsGPUFlagPropagation(t *testing.T) {
	// End-to-end through Metrics(): fake nvidia-smi output must surface
	// as gpu_util_measured/vram_used_measured with real values.
	stubCommandContext(t, "61, 2048\n", "", 0, false)
	m := Metrics(t.Context(), 0, nil)
	if !m.GPUUtilMeasured || !m.VRAMUsedMeasured {
		t.Fatalf("flags not propagated: %+v", m)
	}
	if m.GPUUtilPercent != 61 {
		t.Errorf("gpu util = %v", m.GPUUtilPercent)
	}
	if m.GPUVRAMUsedBytes != 2048*(1<<20) {
		t.Errorf("vram = %v", m.GPUVRAMUsedBytes)
	}
}

func TestMetricsGPUUnknownIsNotZero(t *testing.T) {
	// nvidia-smi runs but reports N/A for both fields: the Metrics
	// contract is flags=false — consumers must not read the zeros.
	stubCommandContext(t, "N/A, N/A\n", "", 0, false)
	m := Metrics(t.Context(), 0, nil)
	if m.GPUUtilMeasured || m.VRAMUsedMeasured {
		t.Fatal("N/A fields must not be marked measured")
	}
	if m.GPUUtilPercent != 0 || m.GPUVRAMUsedBytes != 0 {
		t.Errorf("unmeasured fields should stay zero: %+v", m)
	}
}
