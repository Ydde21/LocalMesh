//go:build windows

package hardware

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
)

func psJSON(ctx context.Context, expr string, v any) error {
	ctx2, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// @(…) forces array JSON even for a single row — ConvertTo-Json
	// emits a bare object for one result, which would not unmarshal
	// into the callers' slices (Win32_ComputerSystem is always a
	// single row, so memoryTotal would silently return 0 forever).
	out, err := exec.CommandContext(ctx2, "powershell", "-NoProfile", "-NonInteractive",
		"-Command", "@("+expr+") | ConvertTo-Json -Compress").Output()
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

func cpuModel(ctx context.Context) string {
	var rows []struct {
		Name string `json:"Name"`
	}
	if err := psJSON(ctx, "Get-CimInstance Win32_Processor | Select-Object Name", &rows); err == nil && len(rows) > 0 {
		return strings.TrimSpace(rows[0].Name)
	}
	return "unknown"
}

func memoryTotal(ctx context.Context) uint64 {
	var rows []struct {
		TotalPhysicalMemory uint64 `json:"TotalPhysicalMemory"`
	}
	if err := psJSON(ctx, "Get-CimInstance Win32_ComputerSystem | Select-Object TotalPhysicalMemory", &rows); err == nil && len(rows) > 0 {
		return rows[0].TotalPhysicalMemory
	}
	return 0
}

func detectGPUs(ctx context.Context) []contracts.GPU {
	var rows []struct {
		Name          string `json:"Name"`
		AdapterRAM    uint64 `json:"AdapterRAM"`
		DriverVersion string `json:"DriverVersion"`
	}
	err := psJSON(ctx, "Get-CimInstance Win32_VideoController | Select-Object Name,AdapterRAM,DriverVersion", &rows)
	if err != nil {
		return nil
	}
	var gpus []contracts.GPU
	for _, r := range rows {
		if r.Name == "" {
			continue
		}
		vendor := ""
		up := strings.ToUpper(r.Name)
		switch {
		case strings.Contains(up, "NVIDIA"):
			vendor = "NVIDIA"
		case strings.Contains(up, "AMD"):
			vendor = "AMD"
		case strings.Contains(up, "INTEL"):
			vendor = "Intel"
		}
		g := contracts.GPU{Name: r.Name, Vendor: vendor, Driver: r.DriverVersion}
		// Win32 AdapterRAM overflows at 4GB on some drivers; clamp.
		if r.AdapterRAM < (1<<32)-1 {
			g.VRAMBytes = r.AdapterRAM
		}
		gpus = append(gpus, g)
	}
	// Prefer nvidia-smi for accurate VRAM.
	if nv := nvidiaVRAM(ctx); len(nv) > 0 {
		for i := range gpus {
			if gpus[i].Vendor == "NVIDIA" {
				if v, ok := nv[gpus[i].Name]; ok {
					gpus[i].VRAMBytes = v
				}
			}
		}
	}
	return gpus
}

func nvidiaVRAM(ctx context.Context) map[string]uint64 {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx2, "nvidia-smi",
		"--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	m := map[string]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, ", ")
		if len(parts) != 2 {
			continue
		}
		if mib, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
			m[strings.TrimSpace(parts[0])] = uint64(mib * (1 << 20))
		}
	}
	return m
}
