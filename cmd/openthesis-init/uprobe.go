//go:build linux

package main

// uprobeCollector installs Linux uprobes on SUT binaries found under
// /opt/openthesis/bin and reads per-event hit counts from tracefs at every
// burst-boundary flush_coverage call.  Each (probe-name, hit-bucket) pair is
// hashed into the shared AFL-style edge bitmap alongside the KCOV kernel edges,
// making userspace code paths (Raft consensus, leader election, etc.) visible
// to the coverage-guided explorer.
//
// Design constraints:
//   - Fully non-fatal: any setup failure just logs a warning and disables the
//     collector.  The init process must never crash because of uprobe support.
//   - No background goroutines: hits are read synchronously inside flushAndSend,
//     which already runs in the collect goroutine.  Adding a goroutine would
//     require LockOSThread / KCOV wiring and complicate the determinism story.
//   - Hit-count bucketing: instead of recording the raw 64-bit hit count we
//     fold it into a 4-bit saturating bucket (0,1,2-3,4-7,8-15,16-31,32-63,64+)
//     matching AFL's hit-count bucketing.  This means the bitmap slot for a
//     probe changes only when its hit count crosses a bucket boundary, keeping
//     cov_hash changes meaningful rather than noisy.
//
// Tracefs layout used:
//
//	/sys/kernel/debug/tracing/uprobe_events           - write "p:<group>/<name> <bin>:0x<off>"
//	/sys/kernel/debug/tracing/events/<group>/<name>/enable - write "1" to arm
//	/sys/kernel/debug/tracing/events/<group>/<name>/id     - numeric event ID (unused here)
//	/sys/kernel/debug/tracing/events/<group>/<name>/format - describes fields (unused)
//	/sys/kernel/debug/tracing/per_cpu/cpu0/stats           - per-cpu event counts (unused)
//
// Hit counts are obtained from the per-event `hits` file that the Linux
// tracing subsystem exposes since kernel 4.1:
//
//	/sys/kernel/debug/tracing/events/openthesis/<name>/hits   (not always present)
//
// If the `hits` file is absent we fall back to reading the global
// `trace_stat/uprobe_profile` file which lists one line per probe:
//
//	<binary>  <probe-name>  <total-hits>
//
// Both paths are tried; whichever yields a non-zero result is used.

import (
	"bufio"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// maxProbesPerBinary is the maximum number of function-entry uprobes to
// install per binary.  Each probe consumes ~1μs per hit in kernel overhead.
// 50 probes per binary × 2 binaries = 100 probes total - well within the
// ~1,000-probe practical limit before overhead becomes measurable.
const maxProbesPerBinary = 50

const uprobeGroup = "openthesis"

// uprobeCollector is the singleton instance wired into flushAndSend.
// It is initialised once from main() before startKcov(); after that it is
// read-only (fields set in setup()) so there is no need for a mutex.
var globalUprobe *uprobeCollector

// uprobeCollector tracks the uprobes registered in tracefs and exposes
// collectHits() for use inside kcovState.flushAndSend().
type uprobeCollector struct {
	// probeNames is the ordered list of event names registered under the
	// openthesis tracefs group, e.g. ["openthesis_0", "openthesis_1", ...].
	// Index i corresponds to the i-th binary in binaryPaths.
	probeNames  []string
	binaryPaths []string
	traceFSPath string
}

// newUprobeCollector returns a new, unconfigured collector.
func newUprobeCollector() *uprobeCollector {
	return &uprobeCollector{
		traceFSPath: "/sys/kernel/debug/tracing",
	}
}

// setup discovers ELF binaries in /opt/openthesis/bin and registers one uprobe
// per binary (at the ELF entry point offset 0x0).  It is safe to call when
// tracefs is not mounted or KCOV is unavailable; every error path logs a
// warning and returns without setting enabled state.
func (u *uprobeCollector) setup() {
	// tracefs must already be mounted (mountFS() mounts debugfs on
	// /sys/kernel/debug before we are called).
	if _, err := os.Stat(u.traceFSPath); err != nil {
		logf("uprobe: tracefs not available at %s, userspace coverage disabled", u.traceFSPath)
		return
	}

	// Collect ELF binaries to instrument.
	dirs := []string{"/opt/openthesis/bin", "/usr/local/bin"}
	var binaries []string
	seen := make(map[string]bool)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			// Resolve symlinks so we don't double-instrument the same binary.
			absPath := filepath.Join(dir, e.Name())
			real, err := filepath.EvalSymlinks(absPath)
			if err != nil {
				real = absPath
			}
			if seen[real] {
				continue
			}
			if !isELF(absPath) {
				continue
			}
			seen[real] = true
			binaries = append(binaries, absPath)
		}
	}
	if len(binaries) == 0 {
		logf("uprobe: no ELF binaries found to instrument")
		return
	}

	// Clear any stale openthesis uprobes from a previous init run (e.g. after
	// snapshot restore that didn't kill the process tree).
	u.clearUprobes()

	// Enable tracing globally.
	_ = os.WriteFile(u.traceFSPath+"/tracing_on", []byte("1\n"), 0o644)

	// Open uprobe_events for appending.
	uprobeEventsPath := u.traceFSPath + "/uprobe_events"
	ef, err := os.OpenFile(uprobeEventsPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		logf("uprobe: cannot open uprobe_events: %v; userspace coverage disabled", err)
		return
	}
	defer ef.Close()

	var registered []string
	var registeredBins []string
	probeIdx := 0

	for _, bin := range binaries {
		// Collect function offsets from ELF symbol table (up to maxProbesPerBinary).
		offsets := elfFuncOffsets(bin, maxProbesPerBinary)
		if len(offsets) == 0 {
			// Fall back to entry point 0x0 if symbol table is absent/stripped.
			offsets = []uint64{0}
		}

		for _, off := range offsets {
			name := fmt.Sprintf("ot%d", probeIdx)
			probeIdx++

			event := fmt.Sprintf("p:%s/%s %s:0x%x\n", uprobeGroup, name, bin, off)
			if _, err := ef.WriteString(event); err != nil {
				logf("uprobe: register %s on %s@0x%x: %v (skipping)", name, bin, off, err)
				probeIdx-- // reclaim the index so names stay dense
				continue
			}

			// Arm the event so the kernel actually counts hits.
			enablePath := fmt.Sprintf("%s/events/%s/%s/enable", u.traceFSPath, uprobeGroup, name)
			if err := os.WriteFile(enablePath, []byte("1\n"), 0o644); err != nil {
				logf("uprobe: enable %s: %v (probe registered but not armed)", name, err)
			}

			registered = append(registered, name)
			registeredBins = append(registeredBins, bin)
		}
	}

	if len(registered) == 0 {
		logf("uprobe: no uprobes successfully registered")
		return
	}

	u.probeNames = registered
	u.binaryPaths = registeredBins
	logf("uprobe: registered %d uprobes (group=%s)", len(registered), uprobeGroup)
}

// clearUprobes removes all events in the openthesis group from uprobe_events.
// Each event is deleted by writing "-:<group>/<name>" to uprobe_events.
func (u *uprobeCollector) clearUprobes() {
	groupDir := fmt.Sprintf("%s/events/%s", u.traceFSPath, uprobeGroup)
	entries, err := os.ReadDir(groupDir)
	if err != nil {
		return // group doesn't exist yet; nothing to clear
	}
	ef, err := os.OpenFile(u.traceFSPath+"/uprobe_events", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	defer ef.Close()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		line := fmt.Sprintf("-%s/%s\n", uprobeGroup, e.Name())
		_, _ = ef.WriteString(line)
	}
}

// collectHits returns a map from probe name to saturating-bucketed hit count.
// It first attempts to read per-event hit counts from
//
//	events/<group>/<name>/hits   (fast path, kernel ≥ 4.1)
//
// and falls back to parsing trace_stat/uprobe_profile for older kernels.
// A return value of nil means the collector is disabled or no probes fired.
func (u *uprobeCollector) collectHits() map[string]uint8 {
	if len(u.probeNames) == 0 {
		return nil
	}

	hits := make(map[string]uint8, len(u.probeNames))

	for _, name := range u.probeNames {
		raw := u.readProbeHitsFast(name)
		if raw == 0 {
			raw = u.readProbeHitsSlow(name)
		}
		hits[name] = aflBucket(raw)
	}
	return hits
}

// readProbeHitsFast reads the hit count for a single probe from the per-event
// `hits` sysfs file (available since Linux 4.1).  Returns 0 on any error.
func (u *uprobeCollector) readProbeHitsFast(name string) uint64 {
	path := fmt.Sprintf("%s/events/%s/%s/hits", u.traceFSPath, uprobeGroup, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return val
}

// readProbeHitsSlow parses trace_stat/uprobe_profile to find the hit count for
// name.  The file has lines of the form:
//
//	<binary>  <group>/<name>  <total-hits>  <miss-hits>
//
// This is slower (reads the whole file) but works on kernels without per-event
// hits files.
func (u *uprobeCollector) readProbeHitsSlow(name string) uint64 {
	path := u.traceFSPath + "/trace_stat/uprobe_profile"
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	target := uprobeGroup + "/" + name
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		// Expected layout: binary  group/name  hits  miss
		if len(parts) < 3 {
			continue
		}
		if parts[1] != target {
			continue
		}
		val, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil {
			continue
		}
		return val
	}
	return 0
}

// mergeIntoSlice folds the uprobe hit map into a coverage bitmap slice using
// FNV-1a hashing.  For each probe with a non-zero bucketed hit count we set
// bitmap[hash(name, bucket) % len(bitmap)] = bucket, saturating at 255.
// This is called from kcovState.flushAndSend() after the KCOV trace buffer
// has been folded.
func (u *uprobeCollector) mergeIntoSlice(bitmap []byte) {
	hits := u.collectHits()
	if hits == nil {
		return
	}
	for name, bucket := range hits {
		if bucket == 0 {
			continue
		}
		idx := hashUprobeEdge(name, uint64(bucket)) % uint64(len(bitmap))
		existing := bitmap[idx]
		if existing < bucket {
			bitmap[idx] = bucket
		}
	}
}

// hashUprobeEdge maps (probe name, bucket) to a bitmap slot using FNV-1a.
// FNV-1a distributes short strings well across the 64KB bitmap with no
// external dependencies and constant time per probe.
func hashUprobeEdge(name string, bucket uint64) uint64 {
	// FNV-1a 64-bit basis and prime.
	const (
		basis uint64 = 14695981039346656037
		prime uint64 = 1099511628211
	)
	h := basis
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= prime
	}
	// Mix in bucket so different call counts for the same probe produce
	// different (adjacent) bitmap slots rather than colliding.
	h ^= bucket
	h *= prime
	return h
}

// aflBucket converts a raw hit count into a saturating 4-bit AFL-style bucket
// value in the range [0, 8]:
//
//	0        → 0
//	1        → 1
//	2-3      → 2
//	4-7      → 3
//	8-15     → 4
//	16-31    → 5
//	32-63    → 6
//	64+      → 7
//
// Storing the bucket index (1 byte) rather than the raw count means the bitmap
// slot changes only when the count crosses a boundary, keeping cov_hash
// transitions meaningful for the explorer.
func aflBucket(n uint64) uint8 {
	switch {
	case n == 0:
		return 0
	case n == 1:
		return 1
	case n < 4:
		return 2
	case n < 8:
		return 3
	case n < 16:
		return 4
	case n < 32:
		return 5
	case n < 64:
		return 6
	default:
		return 7
	}
}

// elfFuncOffsets reads the ELF symbol table from path and returns the file
// offsets of the up-to-limit largest functions. Largest functions are chosen
// because they typically contain the most distinct code paths, maximising the
// coverage signal per uprobe. Returns nil if the symbol table is absent.
//
// The file offset (not the virtual address) is required by the Linux uprobe
// infrastructure; it maps the probe to a page of the ELF file, not to a
// virtual address at load time.  For non-PIE executables and shared libs the
// file offset matches the on-disk position of the .text section.
func elfFuncOffsets(path string, limit int) []uint64 {
	f, err := elf.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	syms, err := f.Symbols()
	if err != nil {
		// Stripped binary - fall back to entry point in caller.
		return nil
	}

	type funcSym struct {
		off  uint64
		size uint64
	}
	var funcs []funcSym

	for _, s := range syms {
		if elf.ST_TYPE(s.Info) != elf.STT_FUNC {
			continue
		}
		if s.Value == 0 || s.Size == 0 {
			continue
		}
		// Convert virtual address to file offset using the section header.
		// Skip special section indices (SHN_UNDEF=0, SHN_ABS=0xfff1, etc).
		si := int(s.Section)
		if si <= 0 || si >= len(f.Sections) {
			continue
		}
		sect := f.Sections[si]
		if sect == nil || sect.Type == elf.SHT_NULL {
			continue
		}
		// file_offset = s.Value - sect.Addr + sect.Offset
		off := s.Value - sect.Addr + sect.Offset
		funcs = append(funcs, funcSym{off: off, size: s.Size})
	}

	if len(funcs) == 0 {
		return nil
	}

	// Sort largest functions first for maximum coverage per probe.
	sort.Slice(funcs, func(i, j int) bool { return funcs[i].size > funcs[j].size })

	if len(funcs) > limit {
		funcs = funcs[:limit]
	}

	offsets := make([]uint64, len(funcs))
	for i, fn := range funcs {
		offsets[i] = fn.off
	}
	return offsets
}

// isELF returns true if the file at path starts with the ELF magic bytes.
func isELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil {
		return false
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}
