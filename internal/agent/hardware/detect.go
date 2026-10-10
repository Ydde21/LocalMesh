// Package hardware discovers CPU/GPU/RAM capabilities and samples live
// utilization, using per-OS adapters with graceful degradation.
package hardware

import (
	"context"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

var version = "dev"

// SetVersion stamps the agent build version into hardware reports.
func SetVersion(v string) { version = v }

// Version returns the stamped build version ("dev" when unstamped).
func Version() string { return version }

// Detect returns the node's static hardware information.
func Detect(ctx context.Context) contracts.HardwareInfo {
	hw := contracts.HardwareInfo{
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		CPUCores:     runtime.NumCPU(),
		AgentVersion: version,
	}
	if h, err := os.Hostname(); err == nil {
		hw.Hostname = h
	}
	hw.CPUModel = cpuModel(ctx)
	hw.MemoryBytes = memoryTotal(ctx)
	hw.GPUs = detectGPUs(ctx)
	return hw
}

// firstNonEmpty returns the first non-empty trimmed string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func round2(f float64) float64 {
	return float64(int(f*100)) / 100
}

var bootTime = time.Now()
