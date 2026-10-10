package hardware

import (
	"strconv"
	"strings"
)

// Pure-output parsers for the per-OS samplers. They never execute tools
// or read files, so fixtures can be tested on any platform.

// cpuTimes is one /proc/stat aggregate sample: busy and total jiffies.
type cpuTimes struct {
	busy, total uint64
}

// parseCPUStatLine parses the aggregate "cpu " line from /proc/stat.
// Fields: user nice system idle iowait irq softirq steal [guest
// guest_nice]. Guest jiffies are already folded into user/nice, so only
// the first eight counters are summed.
func parseCPUStatLine(line string) (cpuTimes, bool) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTimes{}, false
	}
	n := len(f) - 1
	if n > 8 {
		n = 8
	}
	var vals [8]uint64
	var total uint64
	for i := 0; i < n; i++ {
		v, err := strconv.ParseUint(f[i+1], 10, 64)
		if err != nil {
			return cpuTimes{}, false
		}
		vals[i] = v
		total += v
	}
	idle := vals[3]
	if n > 4 {
		idle += vals[4] // iowait
	}
	return cpuTimes{busy: total - idle, total: total}, true
}

// cpuDeltaPercent computes busy% between two samples. Returns ok=false
// when the counters did not advance (same read or counter reset).
func cpuDeltaPercent(a, b cpuTimes) (float64, bool) {
	if b.total <= a.total || b.busy < a.busy {
		return 0, false
	}
	busy := b.busy - a.busy
	if busy == 0 {
		return 0, true // genuinely idle
	}
	return round2(100 * float64(busy) / float64(b.total-a.total)), true
}

// parseMeminfo extracts MemTotal and MemAvailable from /proc/meminfo
// contents, in bytes. ok is false when either field is absent — on
// pre-3.14 kernels there is no honest "available" figure to report.
func parseMeminfo(out string) (total, avail uint64, ok bool) {
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, haveTotal = v*1024, true
		case "MemAvailable:":
			avail, haveAvail = v*1024, true
		}
	}
	return total, avail, haveTotal && haveAvail
}

// parseVMStat parses `vm_stat` output: the page size from the header and
// the free + inactive page counters. ok requires all three fields.
func parseVMStat(out string) (avail uint64, ok bool) {
	var pageSize, freePages, inactive uint64
	var haveSize, haveFree, haveInactive bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "page size of"):
			i := strings.Index(line, "page size of ") + len("page size of ")
			j := strings.Index(line[i:], " bytes")
			if j < 0 {
				continue
			}
			if v, err := strconv.ParseUint(line[i:i+j], 10, 64); err == nil {
				pageSize, haveSize = v, true
			}
		case strings.HasPrefix(line, "Pages free:"):
			if v, ok := trailingCounter(line); ok {
				freePages, haveFree = v, true
			}
		case strings.HasPrefix(line, "Pages inactive:"):
			if v, ok := trailingCounter(line); ok {
				inactive, haveInactive = v, true
			}
		}
	}
	if !haveSize || !haveFree || !haveInactive {
		return 0, false
	}
	return (freePages + inactive) * pageSize, true
}

// trailingCounter parses the number at the end of a "Key:   12345." line.
func trailingCounter(line string) (uint64, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(line[i+1:]), "."), 10, 64)
	return v, err == nil
}

// parseNvidiaSMILoad parses `nvidia-smi --query-gpu=utilization.gpu,
// memory.used --format=csv,noheader,nounits` output: one line per GPU,
// "util%, MiB". util is averaged across GPUs, vram summed. Each flag is
// true only when at least one GPU reported that field.
//
// Fields are range-checked: util must be 0-100%, vram 0 to 1 PiB (MiB).
// Bounds do double duty — they reject NaN/Inf/negatives (NaN fails every
// comparison) and keep the float→uint64 conversion in range. Without
// them, malformed output converts to platform-dependent garbage
// (0 on arm64, 2^63 on amd64) and gets reported as measured data.
func parseNvidiaSMILoad(out string) (util float64, vram uint64, utilOK, vramOK bool) {
	var nU, nV int
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			continue
		}
		if u, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err == nil && u >= 0 && u <= 100 {
			util += u
			nU++
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil && v >= 0 && v <= (1<<30) {
			vram += uint64(v * (1 << 20))
			nV++
		}
	}
	if nU > 0 {
		util /= float64(nU)
	}
	return round2(util), vram, nU > 0, nV > 0
}
