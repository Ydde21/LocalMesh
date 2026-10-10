//go:build windows

package hardware

import (
	"context"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

// sampleCPU measures busy% over a short interval via GetSystemTimes
// (through gopsutil).
func sampleCPU(ctx context.Context) (float64, bool) {
	pct, err := cpu.PercentWithContext(ctx, cpuSampleInterval, false)
	if err != nil || len(pct) == 0 {
		return 0, false
	}
	return round2(pct[0]), true
}

// sampleMemory reports GlobalMemoryStatusEx's ullAvailPhys as the
// available figure.
func sampleMemory(ctx context.Context) (used, total, avail uint64, availOK bool) {
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return 0, 0, 0, false
	}
	return vm.Used, vm.Total, vm.Available, true
}
