//go:build darwin

package hardware

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

func sysctl(ctx context.Context, key string) string {
	out, err := exec.CommandContext(ctx, "sysctl", "-n", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func cpuModel(ctx context.Context) string {
	// Intel Macs expose machdep.cpu.brand_string; Apple Silicon exposes
	// the chip via `system_profiler` "Chip:". Try sysctl first.
	if v := sysctl(ctx, "machdep.cpu.brand_string"); v != "" {
		return v
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "system_profiler", "SPHardwareDataType").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Chip:") {
				return strings.TrimSpace(strings.TrimPrefix(line, "Chip:"))
			}
		}
	}
	if v := sysctl(ctx, "hw.model"); v != "" {
		return v
	}
	return "Apple Silicon"
}

func memoryTotal(ctx context.Context) uint64 {
	v, _ := strconv.ParseUint(sysctl(ctx, "hw.memsize"), 10, 64)
	return v
}

// detectGPUs on macOS uses system_profiler. Apple Silicon GPUs share
// unified memory; discrete AMD GPUs report real VRAM.
func detectGPUs(ctx context.Context) []contracts.GPU {
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "system_profiler", "SPDisplaysDataType").Output()
	if err != nil {
		return nil
	}
	var gpus []contracts.GPU
	var cur *contracts.GPU
	flush := func() {
		if cur != nil && cur.Name != "" {
			gpus = append(gpus, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Chipset Model:") {
			flush()
			cur = &contracts.GPU{Name: strings.TrimSpace(strings.TrimPrefix(line, "Chipset Model:"))}
		} else if cur != nil {
			switch {
			case strings.HasPrefix(line, "VRAM"):
				vram := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				if strings.HasSuffix(vram, "GB") {
					if f, err := strconv.ParseFloat(strings.TrimSuffix(vram, " GB"), 64); err == nil {
						cur.VRAMBytes = uint64(f * (1 << 30))
					}
				} else if strings.HasSuffix(vram, "MB") {
					if f, err := strconv.ParseFloat(strings.TrimSuffix(vram, " MB"), 64); err == nil {
						cur.VRAMBytes = uint64(f * (1 << 20))
					}
				}
			case strings.HasPrefix(line, "Vendor:"):
				cur.Vendor = strings.TrimSpace(strings.TrimPrefix(line, "Vendor:"))
			}
		}
	}
	flush()

	// Apple Silicon uses unified memory.
	if runtime_isARM64() {
		for i := range gpus {
			gpus[i].SharedVRAM = true
			if gpus[i].Vendor == "" {
				gpus[i].Vendor = "Apple"
			}
		}
		if len(gpus) == 0 {
			gpus = []contracts.GPU{{Name: cpuModel(ctx), Vendor: "Apple", SharedVRAM: true}}
		}
	}
	return gpus
}

func runtime_isARM64() bool {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "arm64"
}
