package hardware

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/localmesh/localmesh/internal/contracts"
)

// cpuSampleInterval is the window for CPU busy% deltas — short enough to
// stay cheap on the heartbeat path, long enough for a stable sample.
const cpuSampleInterval = 200 * time.Millisecond

// commandContext is exec.CommandContext, swappable in tests so the
// nvidia-smi exec path (missing binary, non-zero exit, canned output)
// can be exercised without real hardware.
var commandContext = exec.CommandContext

// Metrics samples live utilization. It is cheap enough to run every few
// seconds. Each *_measured flag is set only when the platform produced a
// real measurement this cycle; unmeasured fields stay zero and must not
// be read as idle/free. diskDir picks the filesystem the disk fields
// describe — pass the agent state dir; "" walks up from the cwd.
func Metrics(ctx context.Context, active int, loaded []string, diskDir string) contracts.Metrics {
	m := contracts.Metrics{
		ActiveRequests: active,
		LoadedModels:   loaded,
		// A nil slice means the engine inventory was unreachable; only a
		// non-nil result is real engine state.
		LoadedMeasured: loaded != nil,
		UptimeSeconds:  int64(time.Since(bootTime).Seconds()),
	}
	m.CPUUtilPercent, m.CPUUtilMeasured = sampleCPU(ctx)
	var availOK bool
	m.MemoryUsedBytes, m.MemoryTotalBytes, m.MemoryAvailableBytes, availOK = sampleMemory(ctx)
	m.MemAvailMeasured = availOK
	m.GPUUtilPercent, m.GPUVRAMUsedBytes, m.GPUUtilMeasured, m.VRAMUsedMeasured = sampleGPU(ctx)
	m.DiskFreeBytes, m.DiskTotalBytes, m.DiskMeasured = sampleDisk(diskDir)
	return m
}

// sampleDisk reports free/total bytes on the filesystem holding dir,
// walking to the nearest existing ancestor first (a pre-join state dir
// may not exist yet). Fail-open: an unmeasurable filesystem just leaves
// the measured flag false.
func sampleDisk(dir string) (free, total uint64, ok bool) {
	if dir == "" {
		dir = "."
	}
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return 0, 0, false
		}
		dir = parent
	}
	u, err := disk.Usage(dir)
	if err != nil {
		return 0, 0, false
	}
	return u.Free, u.Total, true
}

// sampleGPU returns aggregate GPU util% and used VRAM via nvidia-smi.
// Without a working nvidia-smi (macOS, AMD/Intel GPUs, missing driver)
// nothing is measurable, so the flags stay false.
func sampleGPU(ctx context.Context) (util float64, vram uint64, utilOK, vramOK bool) {
	ctx2, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := commandContext(ctx2, "nvidia-smi",
		"--query-gpu=utilization.gpu,memory.used",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, false, false
	}
	return parseNvidiaSMILoad(string(out))
}
