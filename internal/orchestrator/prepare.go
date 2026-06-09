package orchestrator

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/openthesis/openthesis/internal/container"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// PrepareResult holds paths created during rootfs preparation.
type PrepareResult struct {
	RootFSPath   string
	InitrdPath   string
	BundlePath   string // OCI bundle directory for gVisor backend
	OutputPath   string // Host path to sdk.jsonl for gVisor backend
	ControlDir   string // Host path to file-based control channel for gVisor backend
	SUTImagePath string // optional ext4 image attached as /dev/vdb → /mnt/sut in VM
}

// prepareRootFS creates a CoW overlay of the base image and builds an initramfs
// with all node binaries, test scripts, and the boot init.
// firecrackerMode: if true, creates a raw ext4 disk instead of qcow2 (Firecracker
// does not support qcow2; all binaries are delivered via initramfs).
func prepareRootFS(cfg *testconfig.Config, initBinary, qemuBinary, stateDir, runID string, firecrackerMode bool) (*PrepareResult, error) {
	runDir := filepath.Join(stateDir, runID)
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return nil, fmt.Errorf("orchestrator prepare mkdir: %w", err)
	}

	// Build initramfs with injected files.
	initrdPath := filepath.Join(runDir, "initrd.cpio")
	if err := buildInitramfs(cfg, initBinary, initrdPath); err != nil {
		return nil, fmt.Errorf("orchestrator prepare initramfs: %w", err)
	}

	result := &PrepareResult{InitrdPath: initrdPath}

	if firecrackerMode {
		// Firecracker uses raw/ext4 block devices; qcow2 is not supported.
		// All node binaries are delivered via initramfs; the rootfs ext4 is
		// only needed as a writable block device (Firecracker requires one).
		// We always create a small scratch ext4 (256 MiB) rather than copying
		// the full base image (~2 GB); initramfs carries all SUT binaries and
		// the init script, so the root filesystem contents don't matter.
		scratch := filepath.Join(runDir, "scratch.ext4")
		if err := createScratchExt4(scratch, 1024); err != nil {
			return nil, fmt.Errorf("orchestrator prepare scratch ext4: %w", err)
		}
		result.RootFSPath = scratch
		// Pass the optional SUT data image through to the VM config.
		if cfg.SUTImage != "" {
			result.SUTImagePath = cfg.SUTImage
		}
		slog.Info("orchestrator: created scratch ext4 for firecracker", "path", scratch)
		return result, nil
	}

	// Resolve qemu-img from the same directory as the QEMU binary.
	qemuImg := "qemu-img"
	if qemuBinary != "" {
		candidate := filepath.Join(filepath.Dir(qemuBinary), "qemu-img")
		if _, err := os.Stat(candidate); err == nil {
			qemuImg = candidate
		}
	}

	if cfg.BaseImage != "" {
		// Create qcow2 overlay backed by the base image.
		absBase, err := filepath.Abs(cfg.BaseImage)
		if err != nil {
			return nil, fmt.Errorf("orchestrator prepare abs: %w", err)
		}
		overlay := filepath.Join(runDir, "rootfs.qcow2")
		cmd := exec.Command(qemuImg, "create", "-f", "qcow2",
			"-b", absBase, "-F", "raw", overlay)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("orchestrator prepare qcow2: %s: %w", out, err)
		}
		result.RootFSPath = overlay
	} else {
		// No base image specified; create a minimal scratch qcow2 disk.
		// QEMU's savevm/loadvm requires at least one writable qcow2 block
		// device to store VM snapshots. Without this, snapshots silently fail.
		scratch := filepath.Join(runDir, "scratch.qcow2")
		cmd := exec.Command(qemuImg, "create", "-f", "qcow2", scratch, "64M")
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("orchestrator prepare scratch disk: %s: %w", out, err)
		}
		result.RootFSPath = scratch
		slog.Info("orchestrator: created scratch disk for savevm", "path", scratch)
	}

	return result, nil
}

// CreateScratchExt4 creates an empty ext4 image of sizeMB megabytes at path.
// Exported so pool workers can create per-worker scratch disks.
func CreateScratchExt4(path string, sizeMB int) error {
	return createScratchExt4(path, sizeMB)
}

// createScratchExt4 creates an empty ext4 image of sizeMB megabytes at path.
func createScratchExt4(path string, sizeMB int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		return err
	}
	f.Close()
	out, err := exec.Command("mkfs.ext4", "-F", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4: %s: %w", out, err)
	}
	return nil
}

// bundleIptablesLegacy finds xtables-legacy-multi on the host, resolves its
// shared library dependencies via ldd, and writes all files into the cpio archive.
// This enables the inject_fault handler in openthesis-init to use iptables for
// DROP rules inside the FC guest (which has no full rootfs userspace).
func bundleIptablesLegacy(w *cpioWriter) error {
	// Find the xtables-legacy-multi binary (the canonical iptables-legacy on Debian/Ubuntu).
	candidates := []string{
		"/sbin/xtables-legacy-multi",
		"/usr/sbin/xtables-legacy-multi",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".openthesis", "bin", "iptables-legacy"))
	}
	var binaryPath string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			binaryPath = p
			break
		}
	}
	if binaryPath == "" {
		return fmt.Errorf("xtables-legacy-multi not found")
	}

	// Parse shared library dependencies via ldd.
	out, err := exec.Command("ldd", binaryPath).Output()
	if err != nil {
		return fmt.Errorf("ldd %s: %w", binaryPath, err)
	}
	// ldd output format: "        libfoo.so.N => /path/to/libfoo.so.N (0x...)"
	// Also: "        /lib64/ld-linux-x86-64.so.2 (0x...)" for the interpreter.
	var libPaths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// Interpreter line: "/lib64/ld-linux-x86-64.so.2 (...)"
		if strings.HasPrefix(line, "/") {
			if idx := strings.Index(line, " "); idx >= 0 {
				libPaths = append(libPaths, line[:idx])
			}
			continue
		}
		// Regular dep: "libfoo.so => /real/path (0x...)"
		if idx := strings.Index(line, " => "); idx >= 0 {
			rest := strings.TrimSpace(line[idx+4:])
			if spaceIdx := strings.Index(rest, " "); spaceIdx >= 0 {
				rest = rest[:spaceIdx]
			}
			if rest != "" && rest != "not" {
				libPaths = append(libPaths, rest)
			}
		}
	}

	// Write directories in sorted order so parent dirs come before children.
	// Build the full set of needed dirs first.
	dirSet := map[string]bool{"sbin": true}
	for _, p := range libPaths {
		for d := strings.TrimPrefix(filepath.Dir(p), "/"); d != "" && d != "."; d = strings.TrimPrefix(filepath.Dir(d), "/") {
			dirSet[d] = true
		}
	}
	sortedDirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		sortedDirs = append(sortedDirs, d)
	}
	// Sort by length then lexicographic so parents always precede children.
	for i := 1; i < len(sortedDirs); i++ {
		for j := i; j > 0 && (len(sortedDirs[j]) < len(sortedDirs[j-1]) ||
			(len(sortedDirs[j]) == len(sortedDirs[j-1]) && sortedDirs[j] < sortedDirs[j-1])); j-- {
			sortedDirs[j], sortedDirs[j-1] = sortedDirs[j-1], sortedDirs[j]
		}
	}
	for _, d := range sortedDirs {
		_ = w.writeDir(d)
	}

	// Write iptables binary as /sbin/iptables.
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", binaryPath, err)
	}
	if err := w.writeFile("sbin/iptables", data, 0o755); err != nil {
		return fmt.Errorf("write iptables: %w", err)
	}

	// Write each shared library at its canonical path.
	var written []string
	for _, libPath := range libPaths {
		libData, err := os.ReadFile(libPath)
		if err != nil {
			slog.Warn("bundleIptablesLegacy: skip unreadable lib", "path", libPath, "err", err)
			continue
		}
		rel := strings.TrimPrefix(libPath, "/")
		if err := w.writeFile(rel, libData, 0o755); err != nil {
			return fmt.Errorf("write lib %s: %w", rel, err)
		}
		written = append(written, filepath.Base(libPath))
	}

	// Bundle the xtables extension modules needed for our specific iptables commands:
	//   libxt_standard.so ; DROP/ACCEPT/RETURN verdict targets
	//   libxt_tcp.so      ; tcp --dport match
	// xtables-legacy-multi looks for extensions in /usr/lib/ARCH/xtables/ or /lib/xtables/.
	xtablesDirs := []string{
		"/usr/lib/x86_64-linux-gnu/xtables",
		"/usr/lib/xtables",
		"/lib/xtables",
		"/lib/x86_64-linux-gnu/xtables",
	}
	requiredExts := []string{"libxt_standard.so", "libxt_tcp.so"}
	for _, xtDir := range xtablesDirs {
		if _, err := os.Stat(xtDir); err != nil {
			continue
		}
		// Create ancestor directories in order (shallowest first) so the cpio
		// unpacker can create each dir after its parent already exists.
		relDir := strings.TrimPrefix(xtDir, "/")
		var ancestors []string
		for d := relDir; d != "" && d != "."; d = filepath.Dir(d) {
			ancestors = append(ancestors, d)
		}
		// Reverse: shallowest first.
		for i := len(ancestors) - 1; i >= 0; i-- {
			_ = w.writeDir(ancestors[i])
		}
		for _, ext := range requiredExts {
			extPath := filepath.Join(xtDir, ext)
			extData, err := os.ReadFile(extPath)
			if err != nil {
				slog.Warn("bundleIptablesLegacy: xtables ext not found", "ext", ext, "dir", xtDir)
				continue
			}
			rel := strings.TrimPrefix(extPath, "/")
			if err := w.writeFile(rel, extData, 0o755); err != nil {
				slog.Warn("bundleIptablesLegacy: write xtables ext failed", "ext", ext, "err", err)
				continue
			}
			written = append(written, ext)
		}
		break // found the xtables dir
	}

	slog.Info("buildInitramfs: bundled iptables-legacy", "files", written)
	return nil
}

// bundleTC finds the tc binary (iproute2) on the host, resolves its shared
// library dependencies via ldd, and writes all files into the cpio archive.
// This enables the inject_fault handler in openthesis-init to use tc netem
// for network delay injection inside the FC guest.
func bundleTC(w *cpioWriter) error {
	candidates := []string{
		"/sbin/tc",
		"/usr/sbin/tc",
		"/usr/bin/tc",
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".openthesis", "bin", "tc"))
	}
	var binaryPath string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			binaryPath = p
			break
		}
	}
	if binaryPath == "" {
		return fmt.Errorf("tc not found")
	}

	out, err := exec.Command("ldd", binaryPath).Output()
	if err != nil {
		return fmt.Errorf("ldd %s: %w", binaryPath, err)
	}
	var libPaths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/") {
			if idx := strings.Index(line, " "); idx >= 0 {
				libPaths = append(libPaths, line[:idx])
			}
			continue
		}
		if idx := strings.Index(line, " => "); idx >= 0 {
			rest := strings.TrimSpace(line[idx+4:])
			if spaceIdx := strings.Index(rest, " "); spaceIdx >= 0 {
				rest = rest[:spaceIdx]
			}
			if rest != "" && rest != "not" {
				libPaths = append(libPaths, rest)
			}
		}
	}

	dirSet := map[string]bool{"sbin": true}
	for _, p := range libPaths {
		for d := strings.TrimPrefix(filepath.Dir(p), "/"); d != "" && d != "."; d = strings.TrimPrefix(filepath.Dir(d), "/") {
			dirSet[d] = true
		}
	}
	sortedDirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		sortedDirs = append(sortedDirs, d)
	}
	for i := 1; i < len(sortedDirs); i++ {
		for j := i; j > 0 && (len(sortedDirs[j]) < len(sortedDirs[j-1]) ||
			(len(sortedDirs[j]) == len(sortedDirs[j-1]) && sortedDirs[j] < sortedDirs[j-1])); j-- {
			sortedDirs[j], sortedDirs[j-1] = sortedDirs[j-1], sortedDirs[j]
		}
	}
	for _, d := range sortedDirs {
		_ = w.writeDir(d)
	}

	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", binaryPath, err)
	}
	if err := w.writeFile("sbin/tc", data, 0o755); err != nil {
		return fmt.Errorf("write tc: %w", err)
	}

	var written []string
	for _, libPath := range libPaths {
		libData, err := os.ReadFile(libPath)
		if err != nil {
			slog.Warn("bundleTC: skip unreadable lib", "path", libPath, "err", err)
			continue
		}
		rel := strings.TrimPrefix(libPath, "/")
		if err := w.writeFile(rel, libData, 0o755); err != nil {
			return fmt.Errorf("write lib %s: %w", rel, err)
		}
		written = append(written, filepath.Base(libPath))
	}

	slog.Info("buildInitramfs: bundled tc", "files", written)
	return nil
}

// bundleSharedLibsForBinary runs ldd on binaryPath, parses the output, and
// writes each unique shared library into the cpio archive at its canonical path.
// seenPaths tracks already-written library paths to avoid duplicates across
// multiple binaries. Returns the basenames of libraries actually written.
// Silently succeeds (returns nil, nil) for static binaries where ldd errors.
func bundleSharedLibsForBinary(w *cpioWriter, binaryPath string, seenPaths map[string]bool) ([]string, error) {
	out, err := exec.Command("ldd", binaryPath).Output()
	if err != nil {
		// Static binary or ldd unavailable; not an error.
		return nil, nil
	}
	// ldd output: "        libfoo.so.N => /path/to/libfoo.so.N (0x...)"
	// or:         "        /lib64/ld-linux-x86-64.so.2 (0x...)"
	var libPaths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/") {
			if idx := strings.Index(line, " "); idx >= 0 {
				libPaths = append(libPaths, line[:idx])
			}
			continue
		}
		if idx := strings.Index(line, " => "); idx >= 0 {
			rest := strings.TrimSpace(line[idx+4:])
			if spaceIdx := strings.Index(rest, " "); spaceIdx >= 0 {
				rest = rest[:spaceIdx]
			}
			if rest != "" && rest != "not" {
				libPaths = append(libPaths, rest)
			}
		}
	}

	// Collect unique library directories so we can create them first.
	dirSet := map[string]bool{}
	for _, p := range libPaths {
		for d := strings.TrimPrefix(filepath.Dir(p), "/"); d != "" && d != "."; d = strings.TrimPrefix(filepath.Dir(d), "/") {
			dirSet[d] = true
		}
	}
	sortedDirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		sortedDirs = append(sortedDirs, d)
	}
	sort.Slice(sortedDirs, func(i, j int) bool {
		if len(sortedDirs[i]) != len(sortedDirs[j]) {
			return len(sortedDirs[i]) < len(sortedDirs[j])
		}
		return sortedDirs[i] < sortedDirs[j]
	})
	for _, d := range sortedDirs {
		_ = w.writeDir(d)
	}

	var written []string
	for _, libPath := range libPaths {
		if seenPaths[libPath] {
			continue
		}
		seenPaths[libPath] = true
		libData, err := os.ReadFile(libPath)
		if err != nil {
			slog.Debug("bundleSharedLibsForBinary: skip unreadable lib", "path", libPath, "err", err)
			continue
		}
		rel := strings.TrimPrefix(libPath, "/")
		if err := w.writeFile(rel, libData, 0o755); err != nil {
			return written, fmt.Errorf("write lib %s: %w", rel, err)
		}
		written = append(written, filepath.Base(libPath))
	}
	return written, nil
}

// buildInitramfs creates a cpio newc-format archive containing:
//   - /opt/openthesis/bin/<name> for each node binary
//   - /opt/openthesis/test/<script> for all test scripts
//   - /opt/openthesis/config.json with the runtime config
//   - /init boot script
func buildInitramfs(cfg *testconfig.Config, initBinary, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create initramfs: %w", err)
	}
	defer f.Close()

	w := newCPIOWriter(f)

	// Write directories first; the kernel initramfs unpacker needs parent
	// directories to exist before it can create files within them.
	dirs := []string{
		"dev", "proc", "sys", "tmp", "run",
		"bin", "usr", "usr/bin",
		"opt", "opt/openthesis", "opt/openthesis/bin",
		"opt/openthesis/test", "opt/openthesis/output",
	}
	for _, d := range dirs {
		if err := w.writeDir(d); err != nil {
			return fmt.Errorf("write dir %s: %w", d, err)
		}
	}

	// Write init binary (compiled Go binary, acts as PID 1).
	initData, err := os.ReadFile(initBinary)
	if err != nil {
		return fmt.Errorf("read init binary %s: %w", initBinary, err)
	}
	if err := w.writeFile("init", initData, 0o755); err != nil {
		return fmt.Errorf("write init: %w", err)
	}

	// Include a static busybox binary so that shell test scripts can run
	// inside the minimal initramfs environment (which has no system userspace).
	// Busybox provides sh, wget (used as curl substitute), and common utilities.
	busyboxPaths := []string{
		"/opt/openthesis/bin/busybox",
		"/usr/bin/busybox",
		"/bin/busybox",
	}
	var busyboxData []byte
	for _, p := range busyboxPaths {
		busyboxData, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if busyboxData != nil {
		if err := w.writeFile("bin/busybox", busyboxData, 0o755); err != nil {
			return fmt.Errorf("write busybox: %w", err)
		}
		// Symlinks so test scripts using #!/bin/sh, #!/bin/bash, curl, wget work.
		shellSymlinks := []struct{ name, target string }{
			{"bin/sh", "/bin/busybox"},
			{"bin/bash", "/bin/busybox"},
			{"usr/bin/curl", "/bin/busybox"},
			{"usr/bin/wget", "/bin/busybox"},
			{"usr/bin/awk", "/bin/busybox"},
			{"usr/bin/grep", "/bin/busybox"},
			{"usr/bin/sed", "/bin/busybox"},
		}
		for _, s := range shellSymlinks {
			if err := w.writeSymlink(s.name, s.target); err != nil {
				return fmt.Errorf("write symlink %s: %w", s.name, err)
			}
		}
	} else {
		slog.Warn("buildInitramfs: busybox not found; shell test scripts may fail")
	}

	// Include iptables-legacy for network fault injection (block_port faults).
	// The guest kernel has CONFIG_IP_NF_FILTER=y so iptables rules work inside FC.
	// iptables-legacy is dynamically linked; we bundle its shared library deps too.
	if err := bundleIptablesLegacy(w); err != nil {
		slog.Debug("buildInitramfs: iptables-legacy not bundled; block_port faults disabled", "err", err)
	}

	// Include tc (iproute2) for network delay injection (delay_port faults).
	// netem qdisc is built into tc itself; no loadable modules needed.
	if err := bundleTC(w); err != nil {
		slog.Debug("buildInitramfs: tc not bundled; delay_port faults disabled", "err", err)
	}

	// Bundle coverage and fault-injection libraries into the guest initramfs.
	// These are LD_PRELOAD'd into dynamically-linked SUT processes by openthesis-init.
	// Build on host:
	//   gcc -shared -fPIC -O2 -o /opt/openthesis/lib/libkcov_preload.so deploy/guest-kcov-preload.c
	//   gcc -shared -fPIC -O2 -o /opt/openthesis/lib/libcover.so deploy/libcover.c
	//   gcc -shared -fPIC -O2 -o /opt/openthesis/lib/libvoidstar.so deploy/libvoidstar.c
	_ = w.writeDir("opt/openthesis/lib")
	type guestLib struct {
		hostPaths []string // candidate host paths (first hit wins)
		guestPath string   // where to place it inside the initramfs
		logLabel  string
	}
	guestLibs := []guestLib{
		{
			hostPaths: []string{
				"/opt/openthesis/lib/libkcov_preload.so",
				"/usr/lib/libkcov_preload.so",
			},
			guestPath: "opt/openthesis/lib/libkcov_preload.so",
			logLabel:  "libkcov_preload.so (KCOV for dynamic SUT processes)",
		},
		{
			hostPaths: []string{
				"/opt/openthesis/lib/libcover.so",
				"/usr/lib/libcover.so",
			},
			guestPath: "opt/openthesis/lib/libcover.so",
			logLabel:  "libcover.so (SanitizerCoverage BB coverage for C/C++ SUTs)",
		},
		{
			hostPaths: []string{
				"/opt/openthesis/lib/libvoidstar.so",
				"/usr/lib/libvoidstar.so",
				"/usr/local/lib/libvoidstar.so",
			},
			guestPath: "opt/openthesis/lib/libvoidstar.so",
			logLabel:  "libvoidstar.so (SanitizerCoverage alias)",
		},
		{
			hostPaths: []string{
				"/opt/openthesis/lib/libfault.so",
				"/usr/lib/libfault.so",
			},
			guestPath: "opt/openthesis/lib/libfault.so",
			logLabel:  "libfault.so (storage fault injection: fsync/write EIO)",
		},
	}
	for _, gl := range guestLibs {
		for _, hp := range gl.hostPaths {
			data, err := os.ReadFile(hp)
			if err != nil {
				continue
			}
			if werr := w.writeFile(gl.guestPath, data, 0o755); werr == nil {
				slog.Info("buildInitramfs: bundled "+gl.logLabel, "src", hp)
			}
			break
		}
	}
	// Also bundle libvoidstar.so under usr/lib for binaries that rpath to /usr/lib.
	if data, err := os.ReadFile("/opt/openthesis/lib/libvoidstar.so"); err == nil {
		if err := w.writeDir("usr/lib"); err == nil {
			_ = w.writeFile("usr/lib/libvoidstar.so", data, 0o755)
		}
	}

	// Write node binaries and their shared library dependencies.
	seenBin := make(map[string]bool)
	seenLib := make(map[string]bool)
	for _, node := range cfg.Nodes {
		if seenBin[node.Binary] {
			continue
		}
		seenBin[node.Binary] = true

		data, err := os.ReadFile(node.Binary)
		if err != nil {
			return fmt.Errorf("read binary %s: %w", node.Binary, err)
		}

		name := filepath.Base(node.Binary)
		path := "opt/openthesis/bin/" + name
		if err := w.writeFile(path, data, 0o755); err != nil {
			return fmt.Errorf("write binary %s: %w", name, err)
		}

		// Bundle shared library dependencies so dynamically-linked C binaries
		// (e.g. redis-server, kafka) work inside the minimal initramfs.
		// Static Go binaries produce a "not a dynamic executable" error from
		// ldd which we swallow silently.
		if written, err := bundleSharedLibsForBinary(w, node.Binary, seenLib); err != nil {
			slog.Debug("buildInitramfs: skip ldd for binary", "binary", name, "err", err)
		} else if len(written) > 0 {
			slog.Info("buildInitramfs: bundled shared libs", "binary", name, "count", len(written))
		}
	}

	// Write test scripts.
	if cfg.TestDir != "" {
		entries, err := os.ReadDir(cfg.TestDir)
		if err != nil {
			return fmt.Errorf("read test dir %s: %w", cfg.TestDir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(cfg.TestDir, e.Name()))
			if err != nil {
				return fmt.Errorf("read test script %s: %w", e.Name(), err)
			}
			path := "opt/openthesis/test/" + e.Name()
			if err := w.writeFile(path, data, 0o755); err != nil {
				return fmt.Errorf("write test script %s: %w", e.Name(), err)
			}
		}
	}

	// Write runtime config.
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := w.writeFile("opt/openthesis/config.json", cfgJSON, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return w.writeTrailer()
}

// cpioWriter writes newc-format cpio archives.
type cpioWriter struct {
	w      io.Writer
	offset int
	ino    uint32
}

func newCPIOWriter(w io.Writer) *cpioWriter {
	return &cpioWriter{w: w, ino: 1}
}

func (c *cpioWriter) writeFile(name string, data []byte, mode uint32) error {
	return c.writeEntry(name, data, mode|0o100000) // S_IFREG
}

func (c *cpioWriter) writeDir(name string) error {
	return c.writeEntry(name, nil, 0o40755) // S_IFDIR
}

func (c *cpioWriter) writeSymlink(name, target string) error {
	return c.writeEntry(name, []byte(target), 0o120777) // S_IFLNK
}

func (c *cpioWriter) writeTrailer() error {
	return c.writeEntry("TRAILER!!!", nil, 0)
}

func (c *cpioWriter) writeEntry(name string, data []byte, mode uint32) error {
	nameLen := len(name) + 1 // include null terminator
	dataLen := len(data)

	c.ino++

	// newc header: 6 bytes magic + 13 x 8-byte hex fields = 110 bytes.
	hdr := fmt.Sprintf(
		"070701"+
			"%08X"+ // ino
			"%08X"+ // mode
			"%08X"+ // uid
			"%08X"+ // gid
			"%08X"+ // nlink
			"%08X"+ // mtime
			"%08X"+ // filesize
			"%08X"+ // devmajor
			"%08X"+ // devminor
			"%08X"+ // rdevmajor
			"%08X"+ // rdevminor
			"%08X"+ // namesize
			"%08X", // check
		c.ino, mode, 0, 0, 1, 0, dataLen,
		0, 0, 0, 0, nameLen, 0,
	)

	if _, err := io.WriteString(c.w, hdr); err != nil {
		return err
	}
	c.offset += len(hdr)

	// Name + null terminator.
	if _, err := io.WriteString(c.w, name+"\x00"); err != nil {
		return err
	}
	c.offset += nameLen

	// Pad to 4-byte boundary after header + name.
	if pad := align4(c.offset) - c.offset; pad > 0 {
		if _, err := c.w.Write(make([]byte, pad)); err != nil {
			return err
		}
		c.offset += pad
	}

	// Data.
	if dataLen > 0 {
		if _, err := c.w.Write(data); err != nil {
			return err
		}
		c.offset += dataLen

		// Pad data to 4-byte boundary.
		if pad := align4(c.offset) - c.offset; pad > 0 {
			if _, err := c.w.Write(make([]byte, pad)); err != nil {
				return err
			}
			c.offset += pad
		}
	}

	return nil
}

func align4(n int) int {
	return (n + 3) &^ 3
}

// prepareOCIBundle creates an OCI runtime bundle for the gVisor backend.
// The bundle contains a rootfs/ directory with node binaries, test scripts,
// and a config.json with the OCI runtime spec.
func prepareOCIBundle(cfg *testconfig.Config, initBinary, stateDir, runID string) (*PrepareResult, error) {
	bundleDir := filepath.Join(stateDir, runID, "bundle")
	rootfsDir := filepath.Join(bundleDir, "rootfs")
	hostOutputDir := filepath.Join(stateDir, runID, "output")
	hostControlDir := filepath.Join(stateDir, runID, "control")
	hostCommandDir := filepath.Join(hostControlDir, "commands")

	// Create the rootfs directory tree.
	dirs := []string{
		rootfsDir,
		filepath.Join(rootfsDir, "opt", "openthesis", "bin"),
		filepath.Join(rootfsDir, "opt", "openthesis", "test"),
		filepath.Join(rootfsDir, "opt", "openthesis", "output"),
		filepath.Join(rootfsDir, "opt", "openthesis", "control"),
		filepath.Join(rootfsDir, "dev"),
		filepath.Join(rootfsDir, "proc"),
		filepath.Join(rootfsDir, "sys"),
		filepath.Join(rootfsDir, "tmp"),
		hostOutputDir,
		hostControlDir,
		hostCommandDir,
	}
	for _, d := range dirs {
		mode := os.FileMode(0o755)
		if d == hostOutputDir {
			// gVisor may run container root with userns remapping; make the
			// host output bind mount world-writable so openthesis-init can
			// always append sdk.jsonl regardless of uid mapping.
			mode = 0o777
		}
		if err := os.MkdirAll(d, mode); err != nil {
			return nil, fmt.Errorf("orchestrator oci bundle mkdir %s: %w", d, err)
		}
		if d == hostOutputDir {
			if err := os.Chmod(d, 0o777); err != nil {
				return nil, fmt.Errorf("orchestrator oci bundle chmod output dir %s: %w", d, err)
			}
		}
	}
	// Ensure sdk output file exists with permissive mode before sandbox start.
	// This avoids setup hangs when container uid mapping cannot create files in
	// the bind-mounted output directory.
	hostSDKPath := filepath.Join(hostOutputDir, "sdk.jsonl")
	if f, err := os.OpenFile(hostSDKPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666); err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle create sdk output %s: %w", hostSDKPath, err)
	} else {
		_ = f.Close()
	}
	if err := os.Chmod(hostSDKPath, 0o666); err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle chmod sdk output %s: %w", hostSDKPath, err)
	}

	// Copy node binaries into rootfs.
	seen := make(map[string]bool)
	for _, node := range cfg.Nodes {
		if seen[node.Binary] {
			continue
		}
		seen[node.Binary] = true

		data, err := os.ReadFile(node.Binary)
		if err != nil {
			return nil, fmt.Errorf("orchestrator oci bundle read binary %s: %w", node.Binary, err)
		}
		dst := filepath.Join(rootfsDir, "opt", "openthesis", "bin", filepath.Base(node.Binary))
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return nil, fmt.Errorf("orchestrator oci bundle write binary %s: %w", dst, err)
		}
	}

	// Copy init binary into the bundle. For gVisor this is PID 1 and is
	// responsible for launching all nodes, waiting readiness, and executing
	// phase commands from the file-based control channel.
	initData, err := os.ReadFile(initBinary)
	if err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle read init binary %s: %w", initBinary, err)
	}
	initDst := filepath.Join(rootfsDir, "opt", "openthesis", "bin", "openthesis-init")
	if err := os.WriteFile(initDst, initData, 0o755); err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle write init binary %s: %w", initDst, err)
	}

	// Copy test scripts into rootfs.
	if cfg.TestDir != "" {
		entries, err := os.ReadDir(cfg.TestDir)
		if err != nil {
			return nil, fmt.Errorf("orchestrator oci bundle read test dir %s: %w", cfg.TestDir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(cfg.TestDir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("orchestrator oci bundle read test script %s: %w", e.Name(), err)
			}
			dst := filepath.Join(rootfsDir, "opt", "openthesis", "test", e.Name())
			if err := os.WriteFile(dst, data, 0o755); err != nil {
				return nil, fmt.Errorf("orchestrator oci bundle write test script %s: %w", e.Name(), err)
			}
		}
	}

	// Write runtime config into rootfs, with node binary paths rewritten to
	// their container-side location (/opt/openthesis/bin/<basename>).
	containerNodes := make([]testconfig.Node, len(cfg.Nodes))
	for i, n := range cfg.Nodes {
		containerNodes[i] = n
		containerNodes[i].Binary = "/opt/openthesis/bin/" + filepath.Base(n.Binary)
	}
	containerCfg := *cfg
	containerCfg.Nodes = containerNodes
	cfgJSON, err := json.Marshal(&containerCfg)
	if err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle marshal config: %w", err)
	}
	cfgPath := filepath.Join(rootfsDir, "opt", "openthesis", "config.json")
	if err := os.WriteFile(cfgPath, cfgJSON, 0o644); err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle write config: %w", err)
	}

	// Write OCI runtime spec (config.json at bundle root).
	spec := ociSpec(cfg, hostOutputDir, hostControlDir)
	specJSON, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle marshal spec: %w", err)
	}
	specPath := filepath.Join(bundleDir, "config.json")
	if err := os.WriteFile(specPath, specJSON, 0o644); err != nil {
		return nil, fmt.Errorf("orchestrator oci bundle write spec: %w", err)
	}

	return &PrepareResult{
		BundlePath: bundleDir,
		OutputPath: hostSDKPath,
		ControlDir: hostControlDir,
	}, nil
}

// prepareFromCompose imports Docker images from a compose file and builds
// an initramfs with the extracted rootfs layers.
func prepareFromCompose(cfg *testconfig.Config, initBinary, stateDir, runID string) (*PrepareResult, error) {
	composeCfg, err := container.ParseCompose(cfg.ComposeFile)
	if err != nil {
		return nil, fmt.Errorf("orchestrator compose parse: %w", err)
	}

	runDir := filepath.Join(stateDir, runID)

	// Import each service's Docker image.
	sorted := composeCfg.TopologicalSort()
	serviceConfigs := make(map[string]*container.ImageConfig, len(sorted))

	for _, svc := range sorted {
		if svc.Image == "" {
			continue
		}

		destDir := filepath.Join(runDir, "images", svc.Name)
		imgCfg, err := container.ImportImage(svc.Image, destDir)
		if err != nil {
			return nil, fmt.Errorf("orchestrator import image %s: %w", svc.Image, err)
		}
		serviceConfigs[svc.Name] = imgCfg

		slog.Info("orchestrator: imported container image",
			"service", svc.Name, "image", svc.Image)
	}

	// Generate a container manifest for the guest init system.
	// This tells openthesis-init which containers to start and in what order.
	type containerSpec struct {
		Name      string            `json:"name"`
		RootFS    string            `json:"rootfs"`
		Command   []string          `json:"command"`
		Env       map[string]string `json:"env"`
		DependsOn []string          `json:"depends_on,omitempty"`
		HealthCmd string            `json:"health_cmd,omitempty"`
	}

	var specs []containerSpec
	for _, svc := range sorted {
		imgCfg := serviceConfigs[svc.Name]
		if imgCfg == nil {
			continue
		}

		// Merge image env with compose env (compose overrides).
		env := make(map[string]string)
		for k, v := range imgCfg.Env {
			env[k] = v
		}
		for k, v := range svc.Env {
			env[k] = v
		}

		// Determine command.
		cmd := svc.Command
		if len(cmd) == 0 {
			cmd = imgCfg.Command()
		}

		specs = append(specs, containerSpec{
			Name:      svc.Name,
			RootFS:    "/opt/openthesis/containers/" + svc.Name,
			Command:   cmd,
			Env:       env,
			DependsOn: svc.DependsOn,
			HealthCmd: svc.HealthCmd,
		})
	}

	specData, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("orchestrator compose spec marshal: %w", err)
	}
	specPath := filepath.Join(runDir, "containers.json")
	if err := os.WriteFile(specPath, specData, 0o644); err != nil {
		return nil, fmt.Errorf("orchestrator compose spec write: %w", err)
	}

	// Build initramfs. For large images this won't scale; baking into rootfs is preferred.
	// This path creates a simplified initramfs with just the manifest.
	initrdPath := filepath.Join(runDir, "initrd.cpio")
	if err := buildComposeInitramfs(cfg, initBinary, specData, initrdPath); err != nil {
		return nil, fmt.Errorf("orchestrator compose initramfs: %w", err)
	}

	result := &PrepareResult{InitrdPath: initrdPath}
	if cfg.BaseImage != "" {
		overlay := filepath.Join(runDir, "rootfs.qcow2")
		absBase, _ := filepath.Abs(cfg.BaseImage)
		cmd := exec.Command("qemu-img", "create", "-f", "qcow2",
			"-b", absBase, "-F", "raw", overlay)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("orchestrator compose qcow2: %s: %w", out, err)
		}
		result.RootFSPath = overlay
	}

	return result, nil
}

func buildComposeInitramfs(cfg *testconfig.Config, initBinary string, specData []byte, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create compose initramfs: %w", err)
	}
	defer f.Close()

	w := newCPIOWriter(f)

	dirs := []string{
		"dev", "proc", "sys", "tmp", "run",
		"opt", "opt/openthesis", "opt/openthesis/bin",
		"opt/openthesis/test", "opt/openthesis/output",
		"opt/openthesis/containers",
	}
	for _, d := range dirs {
		if err := w.writeDir(d); err != nil {
			return fmt.Errorf("write dir %s: %w", d, err)
		}
	}

	// Write init binary.
	initData, err := os.ReadFile(initBinary)
	if err != nil {
		return fmt.Errorf("read init binary %s: %w", initBinary, err)
	}
	if err := w.writeFile("init", initData, 0o755); err != nil {
		return fmt.Errorf("write init: %w", err)
	}

	// Write containers manifest.
	if err := w.writeFile("opt/openthesis/containers.json", specData, 0o644); err != nil {
		return fmt.Errorf("write containers.json: %w", err)
	}

	// Write test scripts if available.
	if cfg.TestDir != "" {
		entries, _ := os.ReadDir(cfg.TestDir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(cfg.TestDir, e.Name()))
			if err != nil {
				continue
			}
			w.writeFile("opt/openthesis/test/"+e.Name(), data, 0o755)
		}
	}

	// Write runtime config.
	cfgJSON, _ := json.Marshal(cfg)
	w.writeFile("opt/openthesis/config.json", cfgJSON, 0o644)

	return w.writeTrailer()
}

// PrepareMultiOCIBundles creates one OCI bundle per service node. Each bundle
// has the service binary as PID 1, suitable for use with StartMulti. Unlike
// prepareOCIBundle (which embeds all nodes in one container with openthesis-init
// as PID 1), these bundles run each node directly for better isolation and
// per-container fault injection granularity.
//
// Returns a map from node name → bundle directory path.
func PrepareMultiOCIBundles(cfg *testconfig.Config, stateDir, runID string) (map[string]string, error) {
	bundles := make(map[string]string, len(cfg.Nodes))
	hostOutputDir := filepath.Join(stateDir, runID, "output")
	hostControlDir := filepath.Join(stateDir, runID, "control")

	// Shared output + control directories (all containers write to same output).
	for _, d := range []string{hostOutputDir, hostControlDir, filepath.Join(hostControlDir, "commands")} {
		mode := os.FileMode(0o755)
		if d == hostOutputDir {
			mode = 0o777
		}
		if err := os.MkdirAll(d, mode); err != nil {
			return nil, fmt.Errorf("orchestrator multi-bundle mkdir %s: %w", d, err)
		}
	}
	hostSDKPath := filepath.Join(hostOutputDir, "sdk.jsonl")
	if f, err := os.OpenFile(hostSDKPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666); err != nil {
		return nil, fmt.Errorf("orchestrator multi-bundle create sdk output: %w", err)
	} else {
		_ = f.Close()
	}
	_ = os.Chmod(hostSDKPath, 0o666)

	for _, node := range cfg.Nodes {
		bundleDir := filepath.Join(stateDir, runID, "bundles", node.Name)
		rootfsDir := filepath.Join(bundleDir, "rootfs")

		dirs := []string{
			rootfsDir,
			filepath.Join(rootfsDir, "opt", "openthesis", "bin"),
			filepath.Join(rootfsDir, "opt", "openthesis", "output"),
			filepath.Join(rootfsDir, "dev"),
			filepath.Join(rootfsDir, "proc"),
			filepath.Join(rootfsDir, "sys"),
			filepath.Join(rootfsDir, "tmp"),
		}
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return nil, fmt.Errorf("orchestrator multi-bundle mkdir %s: %w", d, err)
			}
		}

		// Copy the node binary as the container entrypoint.
		data, err := os.ReadFile(node.Binary)
		if err != nil {
			return nil, fmt.Errorf("orchestrator multi-bundle read binary %s: %w", node.Binary, err)
		}
		binName := filepath.Base(node.Binary)
		dst := filepath.Join(rootfsDir, "opt", "openthesis", "bin", binName)
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return nil, fmt.Errorf("orchestrator multi-bundle write binary %s: %w", dst, err)
		}

		// Build OCI spec with the service binary as PID 1.
		env := []string{
			"PATH=/opt/openthesis/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			// Disable async goroutine preemption (SIGURG-driven) for determinism.
			"GODEBUG=asyncpreemptoff=1",
			// Force single-OS-thread execution. With GOMAXPROCS=1 the guest Go
			// runtime creates exactly one worker M; all goroutines multiplex on
			// that single sentry Task. This eliminates cross-Task Go channel/mutex
			// dependencies that deadlocked the previous cooperative scheduler
			// attempt (which assumed GOMAXPROCS>1 produced multiple Tasks per
			// container). Without this, the DeterministicScheduler cannot safely
			// interleave Tasks from different containers.
			"GOMAXPROCS=1",
		}
		// Determinism: sort env keys before iterating. Map iteration order in
		// Go is randomized; without sort, two runs would produce env arrays in
		// different orders, leading to different bytes in the OCI spec → different
		// container hash → potentially different cgroup paths → different process
		// startup ordering observable to KCOV.
		envKeys := make([]string, 0, len(node.Env))
		for k := range node.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			env = append(env, k+"="+node.Env[k])
		}

		spec := map[string]any{
			"ociVersion": "1.0.2",
			"process": map[string]any{
				"terminal": false,
				"user":     map[string]any{"uid": 0, "gid": 0},
				"args":     []string{"/opt/openthesis/bin/" + binName},
				"env":      env,
				"cwd":      "/opt/openthesis",
			},
			"root": map[string]any{"path": "rootfs", "readonly": false},
			"mounts": []map[string]any{
				{"destination": "/proc", "type": "proc", "source": "proc"},
				{"destination": "/dev", "type": "tmpfs", "source": "tmpfs"},
				{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"ro"}},
				{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs"},
				{
					"destination": "/opt/openthesis/output",
					"type":        "bind",
					"source":      hostOutputDir,
					"options":     []string{"rbind", "rw"},
				},
			},
			"linux": map[string]any{
				"namespaces": []map[string]any{
					{"type": "pid"},
					{"type": "ipc"},
					{"type": "uts"},
					{"type": "mount"},
					{"type": "network"},
				},
			},
		}
		specJSON, err := json.MarshalIndent(spec, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("orchestrator multi-bundle marshal spec %s: %w", node.Name, err)
		}
		specPath := filepath.Join(bundleDir, "config.json")
		if err := os.WriteFile(specPath, specJSON, 0o644); err != nil {
			return nil, fmt.Errorf("orchestrator multi-bundle write spec %s: %w", node.Name, err)
		}

		bundles[node.Name] = bundleDir
	}

	return bundles, nil
}

// ociSpec builds a minimal OCI runtime specification for the test workload.
func ociSpec(cfg *testconfig.Config, hostOutputDir, hostControlDir string) map[string]any {
	// PID 1 in the OCI container must be openthesis-init so all daemon and
	// non-daemon nodes are started consistently across backends.
	processArgs := []string{"/opt/openthesis/bin/openthesis-init"}

	// Build environment variables.
	env := []string{
		"PATH=/opt/openthesis/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"OPENTHESIS_GVISOR=1",
		// Probe timeout per attempt: 5s virtual (~1.7s real at 3GHz TSC).
		// Setup timeout: 25s virtual (~8.3s real); enough for all nodes to start.
		"OPENTHESIS_PROBE_TIMEOUT=1s",
		"OPENTHESIS_SETUP_TIMEOUT=4s",
		// Disable asynchronous goroutine preemption in the gVisor sentry.
		// The sentry is a Go runtime process; async preemption is driven by
		// OS signals (SIGURG) whose delivery timing depends on the host
		// scheduler; making it a source of non-determinism. Setting
		// asyncpreemptoff=1 switches the sentry to cooperative preemption
		// only, so goroutine scheduling decisions are determined solely by
		// explicit yield points (channel ops, function calls, etc.), which
		// are deterministic given a fixed instruction stream.
		"GODEBUG=asyncpreemptoff=1",
		// Force single-OS-thread execution in the guest application. With
		// GOMAXPROCS=1 the guest Go runtime creates exactly one worker M;
		// all goroutines multiplex on that single sentry Task. This is the
		// prerequisite for re-enabling the cooperative DeterministicScheduler:
		// with GOMAXPROCS>1 different goroutines run on different OS threads
		// (different sentry Tasks), creating Go-level cross-Task dependencies
		// that deadlock the cooperative scheduler. With GOMAXPROCS=1 all
		// cross-goroutine communication is resolved within one Task by the
		// Go runtime's own scheduler; no cross-Task blocking possible.
		"GOMAXPROCS=1",
	}
	for _, node := range cfg.Nodes {
		// Determinism: sort keys, Go map iteration is randomized. Env order
		// is baked into the OCI spec JSON and eventually into execve envp;
		// different orderings produce different KCOV observations, diverging
		// the snapshot tree at the first step where env is consumed.
		envKeys := make([]string, 0, len(node.Env))
		for k := range node.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			env = append(env, k+"="+node.Env[k])
		}
	}

	return map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user": map[string]any{
				"uid": 0,
				"gid": 0,
			},
			"args": processArgs,
			"env":  env,
			"cwd":  "/opt/openthesis",
		},
		"root": map[string]any{
			"path":     "rootfs",
			"readonly": false,
		},
		"mounts": []map[string]any{
			{"destination": "/proc", "type": "proc", "source": "proc"},
			{"destination": "/dev", "type": "tmpfs", "source": "tmpfs"},
			{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"ro"}},
			{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs"},
			{
				"destination": "/opt/openthesis/output",
				"type":        "bind",
				"source":      hostOutputDir,
				"options":     []string{"rbind", "rw"},
			},
			{
				"destination": "/opt/openthesis/control",
				"type":        "bind",
				"source":      hostControlDir,
				"options":     []string{"rbind", "rw"},
			},
		},
		"linux": map[string]any{
			"namespaces": []map[string]any{
				{"type": "pid"},
				{"type": "ipc"},
				{"type": "uts"},
				{"type": "mount"},
				{"type": "network"},
			},
		},
	}
}
