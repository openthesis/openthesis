//go:build linux

package main

// monitor.go - memory and health monitoring: periodic /proc/meminfo sampling
// and platform memory assertion emission.

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// startMemoryMonitor samples /proc/meminfo every 30 seconds and emits a
// platform assertion if memory usage exceeds 95%.
// No lockAndEnableKcov here: the ticker fires on virtual-clock time (icount),
// but the exact burst boundary at which it wakes is non-deterministic across
// runs with different SUT paths. Excluding its kernel paths from KCOV avoids
// cov_hash divergence from an unrelated goroutine.
func startMemoryMonitor(procs []nodeProc) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		checkMemoryUsage()
	}
}

// checkMemoryUsage reads /proc/meminfo and emits a platform memory assertion.
func checkMemoryUsage() {
	total, available := readMemInfo()
	if total == 0 {
		return
	}
	used := total - available
	fraction := float64(used) / float64(total)
	condition := fraction < 0.95
	details := map[string]any{
		"used_kb":  used,
		"total_kb": total,
		"fraction": fraction,
	}
	emitPlatformAssertion(true, condition, "peak memory below 95%", true, details)
}

// readMemInfo returns MemTotal and MemAvailable from /proc/meminfo in kB.
func readMemInfo() (total, available uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = val
		case "MemAvailable:":
			available = val
		}
	}
	return
}
