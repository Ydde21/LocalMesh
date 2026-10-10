//go:build linux

package hardware

import (
	"context"
	"os"
	"strings"
	"time"
)

// sampleCPU measures busy% from two /proc/stat reads over a short
// interval, so every successful call is a real interval delta.
func sampleCPU(ctx context.Context) (float64, bool) {
	t1, ok := readCPUTimes()
	if !ok {
		return 0, false
	}
	select {
	case <-ctx.Done():
		return 0, false
	case <-time.After(cpuSampleInterval):
	}
	t2, ok := readCPUTimes()
	if !ok {
		return 0, false
	}
	return cpuDeltaPercent(t1, t2)
}

func readCPUTimes() (cpuTimes, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return parseCPUStatLine(line)
}

// sampleMemory reports the kernel's own MemAvailable estimate; older
// kernels without the field yield flags=false rather than a guess.
func sampleMemory(ctx context.Context) (used, total, avail uint64, availOK bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, false
	}
	total, avail, ok := parseMeminfo(string(b))
	if !ok {
		return 0, total, 0, false
	}
	if total > avail {
		used = total - avail
	}
	return used, total, avail, true
}
