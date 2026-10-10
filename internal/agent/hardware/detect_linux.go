//go:build linux

package hardware

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

func cpuModel(ctx context.Context) string {
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					return strings.TrimSpace(line[i+1:])
				}
			}
		}
	}
	// ARM devices may only have Hardware/Machine fields.
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Hardware") {
				if i := strings.Index(line, ":"); i >= 0 {
					return strings.TrimSpace(line[i+1:])
				}
			}
		}
	}
	return runtimeArch()
}

func memoryTotal(ctx context.Context) uint64 {
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					kb, _ := strconv.ParseUint(f[1], 10, 64)
					return kb * 1024
				}
			}
		}
	}
	return 0
}

// detectGPUs prefers nvidia-smi; falls back to lspci for names only.
func detectGPUs(ctx context.Context) []contracts.GPU {
	if gpus := nvidiaGPUs(ctx); len(gpus) > 0 {
		return gpus
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "lspci").Output()
	if err != nil {
		return nil
	}
	var gpus []contracts.GPU
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "VGA") || strings.Contains(line, "3D controller") {
			name := line
			if i := strings.Index(line, ": "); i >= 0 {
				name = line[i+2:]
			}
			vendor := ""
			switch {
			case strings.Contains(strings.ToUpper(name), "NVIDIA"):
				vendor = "NVIDIA"
			case strings.Contains(strings.ToUpper(name), "AMD") || strings.Contains(strings.ToUpper(name), "ATI"):
				vendor = "AMD"
			case strings.Contains(strings.ToUpper(name), "INTEL"):
				vendor = "Intel"
			}
			gpus = append(gpus, contracts.GPU{Name: strings.TrimSpace(name), Vendor: vendor})
		}
	}
	return gpus
}

// nvidiaGPUs queries nvidia-smi when present.
func nvidiaGPUs(ctx context.Context) []contracts.GPU {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "nvidia-smi",
		"--query-gpu=name,memory.total,driver_version",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var gpus []contracts.GPU
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ", ")
		if len(parts) < 2 {
			continue
		}
		g := contracts.GPU{Name: strings.TrimSpace(parts[0]), Vendor: "NVIDIA"}
		if mib, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
			g.VRAMBytes = uint64(mib * (1 << 20))
		}
		if len(parts) >= 3 {
			g.Driver = strings.TrimSpace(parts[2])
		}
		gpus = append(gpus, g)
	}
	return gpus
}

func runtimeArch() string {
	out, _ := exec.Command("uname", "-m").Output()
	return strings.TrimSpace(string(out))
}
