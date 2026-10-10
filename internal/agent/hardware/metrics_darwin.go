//go:build darwin

package hardware

import (
	"context"
	"os/exec"
	"strconv"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

// sampleCPU measures busy% over a short interval via host_processor_info
// (through gopsutil). A real interval delta is always produced when the
// call succeeds.
func sampleCPU(ctx context.Context) (float64, bool) {
	pct, err := cpu.PercentWithContext(ctx, cpuSampleInterval, false)
	if err != nil || len(pct) == 0 {
		return 0, false
	}
	return round2(pct[0]), true
}

// sampleMemory reads total RAM from sysctl and computes available memory
// as (free + inactive) * pageSize from vm_stat — the count a process can
// claim without swapping. GPU util is not measurable on macOS: Apple
// Silicon exposes no counter interface and nvidia-smi does not exist
// there, so sampleGPU reports flags=false rather than fabricating.
func sampleMemory(ctx context.Context) (used, total, avail uint64, availOK bool) {
	total, _ = strconv.ParseUint(sysctl(ctx, "hw.memsize"), 10, 64)
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "vm_stat").Output()
	if err != nil {
		return 0, total, 0, false
	}
	avail, ok := parseVMStat(string(out))
	if !ok {
		return 0, total, 0, false
	}
	if total > avail {
		used = total - avail
	}
	return used, total, avail, true
}
