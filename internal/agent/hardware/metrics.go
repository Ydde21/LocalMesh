package hardware

import (
	"context"
	"os/exec"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

// cpuSampleInterval is the window for CPU busy% deltas — short enough to
// stay cheap on the heartbeat path, long enough for a stable sample.
const cpuSampleInterval = 200 * time.Millisecond

// Metrics samples live utilization. It is cheap enough to run every few
// seconds. Each *_measured flag is set only when the platform produced a
// real measurement this cycle; unmeasured fields stay zero and must not
// be read as idle/free.
func Metrics(ctx context.Context, active int, loaded []string) contracts.Metrics {
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
	return m
}

// sampleGPU returns aggregate GPU util% and used VRAM via nvidia-smi.
// Without a working nvidia-smi (macOS, AMD/Intel GPUs, missing driver)
// nothing is measurable, so the flags stay false.
func sampleGPU(ctx context.Context) (util float64, vram uint64, utilOK, vramOK bool) {
	ctx2, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "nvidia-smi",
		"--query-gpu=utilization.gpu,memory.used",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, false, false
	}
	return parseNvidiaSMILoad(string(out))
}
