//go:build linux

package main

// KCOV-based kernel coverage collection for the Firecracker backend.
// https://docs.kernel.org/dev-tools/kcov.html
//
// Three coverage modes are supported:
//
//   init-process KCOV  - traces kernel paths taken by PID 1: vsock I/O,
//                        process management, /proc reads. No binary changes.
//   LD_PRELOAD KCOV    - deploy/guest-kcov-preload.c enables KCOV in each
//                        SUT process. Requires dynamically-linked binaries.
//   Intel PT (future)  - traces all processes at hardware speed. Requires a
//                        Firecracker patch to expose /dev/intel_pt per-VM.
//
// The shared AFL-style bitmap lives at /run/kcov_bitmap (64 KB, tmpfs).
// Any process with libkcov_preload.so loaded writes to it; init reads it.
// init sends the merged bitmap to the host as a "coverage" message on each
// burst-boundary flush.

import (
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	// KCOV ioctl numbers on Linux x86_64 (from linux/kcov.h)
	// KCOV_INIT_TRACE = _IOR('c', 1, unsigned long)
	// = (2<<30) | (8<<16) | ('c'<<8) | 1 = 0x80086301
	ioctlKcovInitTrace = 0x80086301
	// KCOV_ENABLE = _IO('c', 100) = ('c'<<8)|100 = 0x6364
	ioctlKcovEnable = 0x6364
	// KCOV_DISABLE = _IO('c', 101) = 0x6365
	ioctlKcovDisable = 0x6365
	// KCOV_REMOTE_ENABLE = _IOW('c', 102, struct kcov_remote_arg)
	// struct kcov_remote_arg { u32 trace_mode; u32 num_handles; u64 common_handle; }
	// sizeof = 16, _IOW = (1<<30)|(16<<16)|('c'<<8)|102 = 0x40106366
	ioctlKcovRemoteEnable = 0x40106366

	kcovTracePc = 0 // KCOV_TRACE_PC mode: collect (PC) entries

	// kcovEntries is the trace buffer size in 8-byte entries.
	// Entry [0] = count of PCs written; entries [1..N] = raw PC values.
	//
	// Sized for ~5M-instruction bursts. With CONFIG_KCOV_INSTRUMENT_ALL, every
	// basic block produces a PC. Worst case: ~10 BBs per 1000 insns = ~50K PCs
	// per 5M-insn burst. 524288 entries gives 8x headroom and prevents overflow
	// between burst-boundary flushes. There is no wall-clock ticker; the buffer
	// must hold a full burst. 524288 * 8 bytes = 4 MiB per init process.
	kcovEntries = 524288

	// bitmapSize is the AFL-style edge bitmap size in bytes.
	// Each slot counts transitions: edge = (prevPC>>1) XOR curPC, folded to 64K.
	bitmapSize = 1 << 16

	// bitmapPath is a shared tmpfs file all KCOV-instrumented processes write to.
	// init also writes its own coverage here; the LD_PRELOAD library appends to it.
	bitmapPath = "/run/kcov_bitmap"

	// KCOV is flushed only on demand at burst boundaries via handleFlushCoverage().
	// Timer-interrupt-driven flushing caused cov_hash divergence across runs with
	// identical instruction sequences; virtual-time-driven flushing eliminates that.
)

// extraKcov holds per-OS-thread KCOV trace buffers opened by long-lived
// goroutines via enableKcovForCurrentThread(). KCOV is per-task in the
// kernel; only the OS thread that called KCOV_ENABLE has its kernel paths
// recorded. With GOMAXPROCS unconstrained, the Go runtime may spawn
// transient Ms for blocked syscalls, and any kernel paths those threads
// take vanish from coverage. Each long-lived goroutine calls
// runtime.LockOSThread() and registers its OS thread here so flushAndSend()
// can drain every per-thread buffer into the shared bitmap.
//
// The slice is append-only. Iteration under the read-lock is safe even while
// a new thread registers concurrently.
var (
	extraKcovMu sync.RWMutex
	extraKcov   []*kcovThreadState
)

// kcovThreadState is a minimal per-thread KCOV trace buffer. It has no
// bitmap and no lastPC of its own. flushAndSend() folds entries from every
// per-thread buffer into the primary kcovState's shared bitmap so AFL-style
// edges remain consistent.
type kcovThreadState struct {
	fd  int
	buf []uint64 // mmap of /sys/kernel/debug/kcov for this OS thread
}

// enableKcovForCurrentThread opens a fresh KCOV fd, issues KCOV_INIT_TRACE,
// mmaps the trace buffer, and calls KCOV_ENABLE for the calling OS thread.
// The caller must have already called runtime.LockOSThread() so the Go
// runtime keeps the goroutine pinned to that thread for its lifetime.
//
// Failures are logged and swallowed. This is purely additive coverage and
// must not break the init process when KCOV is unavailable.
func enableKcovForCurrentThread(label string) {
	fd, err := syscall.Open("/sys/kernel/debug/kcov", syscall.O_RDWR, 0)
	if err != nil {
		logf("kcov[%s]: open failed: %v", label, err)
		return
	}
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), ioctlKcovInitTrace, uintptr(kcovEntries),
	); errno != 0 {
		logf("kcov[%s]: KCOV_INIT_TRACE failed: %v", label, errno)
		syscall.Close(fd)
		return
	}
	bufLen := kcovEntries * 8
	raw, err := syscall.Mmap(fd, 0, bufLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		logf("kcov[%s]: mmap failed: %v", label, err)
		syscall.Close(fd)
		return
	}
	buf := unsafe.Slice((*uint64)(unsafe.Pointer(&raw[0])), kcovEntries)
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), ioctlKcovEnable, uintptr(kcovTracePc),
	); errno != 0 {
		logf("kcov[%s]: KCOV_ENABLE failed: %v", label, errno)
		syscall.Munmap(raw) //nolint:errcheck
		syscall.Close(fd)
		return
	}
	ts := &kcovThreadState{fd: fd, buf: buf}
	extraKcovMu.Lock()
	extraKcov = append(extraKcov, ts)
	extraKcovMu.Unlock()
	logf("kcov[%s]: enabled on OS thread (fd=%d, total threads=%d)", label, fd, len(extraKcov))
}

// lockAndEnableKcov is a one-call helper for goroutine entry points:
//
//	go func() { lockAndEnableKcov("vsock"); ... }()
//
// Pins the goroutine to its current OS thread so the Go runtime cannot
// migrate it after a blocking syscall, then registers a fresh KCOV fd for
// that thread. Safe to call when KCOV is unavailable; failures are logged.
func lockAndEnableKcov(label string) {
	runtime.LockOSThread()
	enableKcovForCurrentThread(label)
}

// kcovResetCh is signalled by handleResetCoverage to zero the coverage
// bitmap before the root snapshot is taken. This ensures the first cov_hash
// is identical across runs regardless of boot-time coverage variance.
var kcovResetCh = make(chan chan struct{}, 1)

// kcovFlushCh is signalled by handleFlushCoverage to trigger an immediate
// synchronous KCOV flush at burst boundaries. Replaces wall-clock
// ticker-driven flushes so cov_hash is captured at a deterministic
// virtual-time point (end of burst), not a non-deterministic wall-clock
// offset within the burst.
var kcovFlushCh = make(chan chan struct{}, 1)

// KCOV ring buffer layout (mmap'd from /sys/kernel/debug/kcov):
//
//   offset   size     field
//   -------  -------  --------------------------------------------------
//   [0]      uint64   write cursor  (kernel increments after each PC)
//   [1]      uint64   PC[1]        \
//   [2]      uint64   PC[2]         > up to kcovEntries-1 raw PCs
//   ...      ...      ...          /
//   [N]      uint64   PC[N]         (N = AtomicSwap(buf[0], 0))
//
// Guest-to-vsock flow at each burst boundary:
//
//   Guest kernel              openthesis-init (collect goroutine)
//   ----------------------    -------------------------------------------
//   KCOV writes PCs           flush_coverage vsock cmd arrives
//   to buf[1..N]              |
//   buf[0] = count    ------> AtomicSwap(&buf[0], 0) -> N
//                             prev = s.lastPC
//                             for i in 1..N:
//                               slot = ((prev>>1) ^ buf[i]) % 64K
//                               bitmap[slot]++
//                               prev = buf[i]
//                             s.lastPC = buf[N]   <- bridge to next burst
//                             if bitmap != prevBitmap: send over vsock
//
// kcovState holds the live KCOV buffer and the shared edge bitmap.
type kcovState struct {
	fd         int
	buf        []uint64 // mmap of /sys/kernel/debug/kcov; buf[0]=count, buf[1..]=PCs
	bitmapFile *os.File
	bitmap     []byte           // MAP_SHARED mmap of bitmapPath; written by all instrumented procs
	prevBitmap [bitmapSize]byte // snapshot from last send; used to detect new edges
	lastPC     uint64           // last PC from previous tick; bridges cross-tick edge pairs
	lastGen    uint64           // incremented on every non-empty collect
}

// startKcov opens KCOV for the current (init) process and launches a collect
// goroutine that merges the trace buffer into the shared bitmap and forwards
// it to the host. Returns immediately if KCOV is unavailable (kernel not
// built with CONFIG_KCOV).
//
// Must be called from the main goroutine, which is locked to M0 via
// runtime.LockOSThread() at the top of main. KCOV is per-task in the kernel;
// it traces only the OS thread that called KCOV_ENABLE. With GOMAXPROCS=1
// and main locked to M0, every goroutine in init runs on M0, so KCOV_ENABLE
// on M0 captures every kernel path the init process executes. Same instruction
// sequence -> same kernel paths -> same PCs in the trace buffer.
//
// The collect goroutine reads the shared trace buffer that M0 writes to. With
// GOMAXPROCS=1 it also runs on M0, but that is not required for correctness.
func startKcov(stopCh <-chan struct{}) {
	s, err := openKcov()
	if err != nil {
		logf("kcov: not available (%v); no kernel coverage", err)
		return
	}
	logf("kcov: enabled (init-process KCOV on M0; bitmap at %s)", bitmapPath)
	go s.collect(stopCh)
}

// kcovRemoteArg is the Go equivalent of Linux's struct kcov_remote_arg.
// Must match the kernel ABI exactly: { u32 trace_mode; u32 num_handles; u64 common_handle; }.
type kcovRemoteArg struct {
	traceMode    uint32
	numHandles   uint32
	commonHandle uint64
}

// tryKcovRemoteEnable registers the KCOV fd for remote coverage from global
// kernel subsystems (softirq, workqueues). On kernels that support it
// (CONFIG_KCOV + kernel >= 5.3), the per-task trace buffer also receives PCs
// from interrupt context handlers associated with this process. Non-fatal:
// logs and returns on EINVAL (old kernel or feature not built in).
func tryKcovRemoteEnable(fd int) {
	// KCOV_SUBSYSTEM_GLOBAL = 1 (from linux/kcov.h). The common_handle encodes
	// the subsystem in the top byte: (subsystem << 56) | instance_id.
	// Instance 1 is arbitrary; we use it so parallel VMs don't collide.
	const kcovSubsysGlobal = uint64(1) << 56
	arg := kcovRemoteArg{
		traceMode:    kcovTracePc,
		numHandles:   0,
		commonHandle: kcovSubsysGlobal | 1,
	}
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), ioctlKcovRemoteEnable,
		uintptr(unsafe.Pointer(&arg)),
	); errno != 0 {
		logf("kcov: KCOV_REMOTE_ENABLE not supported (errno=%v); softirq coverage excluded", errno)
		return
	}
	logf("kcov: KCOV_REMOTE_ENABLE active - softirq/workqueue kernel paths captured")
}

func openKcov() (*kcovState, error) {
	fd, err := syscall.Open("/sys/kernel/debug/kcov", syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /sys/kernel/debug/kcov: %w", err)
	}

	// Allocate the trace buffer in the kernel.
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), ioctlKcovInitTrace, uintptr(kcovEntries),
	); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("KCOV_INIT_TRACE(entries=%d): %w", kcovEntries, errno)
	}

	// mmap the trace buffer. The kernel allocates exactly kcovEntries uint64s.
	// buf[0] = count of PCs written, buf[1..kcovEntries-1] = raw PC values.
	// kcovEntries already includes the count word; do not add 1.
	bufLen := kcovEntries * 8
	raw, err := syscall.Mmap(fd, 0, bufLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("mmap kcov trace buffer (len=%d): %w", bufLen, err)
	}
	buf := unsafe.Slice((*uint64)(unsafe.Pointer(&raw[0])), kcovEntries)

	// Enable KCOV_TRACE_PC for this goroutine's OS thread. KCOV is per-task
	// in the kernel; runtime.LockOSThread() keeps the goroutine on one OS
	// thread so KCOV keeps tracing it.
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, uintptr(fd), ioctlKcovEnable, uintptr(kcovTracePc),
	); errno != 0 {
		syscall.Munmap(raw)
		syscall.Close(fd)
		return nil, fmt.Errorf("KCOV_ENABLE(mode=%d): %w", kcovTracePc, errno)
	}

	// Attempt to enable remote coverage for softirq/workqueue kernel paths.
	// These run in interrupt context on behalf of this process (vsock packet
	// processing, network RX softirqs) and are invisible to per-task KCOV.
	// Remote coverage uses the same trace buffer; the kernel merges remote
	// PCs alongside per-task PCs when the softirq runs. This reduces the
	// ~1% KCOV edge variance from interrupt-driven paths.
	tryKcovRemoteEnable(fd)

	// Open or create the shared bitmap file so LD_PRELOAD processes can write to it.
	bitmapFile, err := os.OpenFile(bitmapPath, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		logf("kcov: cannot open bitmap file %s: %v (continuing without shared bitmap)", bitmapPath, err)
	} else {
		_ = bitmapFile.Truncate(bitmapSize)
	}

	var bitmap []byte
	if bitmapFile != nil {
		bitmap, err = syscall.Mmap(int(bitmapFile.Fd()), 0, bitmapSize,
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			logf("kcov: mmap bitmap failed: %v (continuing)", err)
			bitmapFile.Close()
			bitmapFile = nil
		}
	}

	return &kcovState{
		fd:         fd,
		buf:        buf,
		bitmapFile: bitmapFile,
		bitmap:     bitmap,
	}, nil
}

func (s *kcovState) collect(stopCh <-chan struct{}) {
	defer func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(s.fd), ioctlKcovDisable, 0) //nolint:errcheck
		if s.bitmap != nil {
			syscall.Munmap(s.bitmap) //nolint:errcheck
		}
		if s.bitmapFile != nil {
			s.bitmapFile.Close()
		}
		syscall.Close(s.fd)
	}()

	for {
		select {
		case <-stopCh:
			return
		case done := <-kcovFlushCh:
			// Burst-boundary flush. flush_coverage is the only KCOV sampling
			// trigger; there is no wall-clock ticker.
			s.flushAndSend()
			close(done)
		case done := <-kcovResetCh:
			// Zero the KCOV trace buffer, shared bitmap, prevBitmap, and lastPC
			// so the next snapshot's cov_hash reflects only post-reset coverage.
			// lastPC must be cleared; a stale PC from before the reset would
			// corrupt the first cross-burst edge of the next run.
			atomic.StoreUint64(&s.buf[0], 0)
			extraKcovMu.RLock()
			for _, ts := range extraKcov {
				atomic.StoreUint64(&ts.buf[0], 0)
			}
			extraKcovMu.RUnlock()
			if s.bitmap != nil {
				for i := range s.bitmap {
					s.bitmap[i] = 0
				}
			}
			s.prevBitmap = [bitmapSize]byte{}
			s.lastPC = 0
			close(done)
		}
	}
}

// handleFlushCoverage triggers an immediate synchronous KCOV bitmap flush.
// Called from the vsock command listener when the host sends "flush_coverage"
// at the end of each burst. cov_hash is computed at the burst boundary, not
// at a non-deterministic wall-clock offset within the burst.
func handleFlushCoverage() {
	done := make(chan struct{})
	select {
	case kcovFlushCh <- done:
		<-done
	default:
		// KCOV not running; nothing to flush.
	}
}

// handleResetCoverage zeros all coverage state synchronously. Called from
// the vsock command listener when the host sends reset_coverage. Blocks
// until the collect goroutine has completed the reset; the host must know
// the bitmap is clean before triggering the root snapshot.
func handleResetCoverage() {
	done := make(chan struct{})
	select {
	case kcovResetCh <- done:
		<-done
		logf("kcov: bitmap reset (init burst coverage zeroed)")
	default:
		// KCOV not running (no kernel support); nothing to reset.
	}
}

// flushAndSend reads the KCOV trace buffer, folds PC pairs into the shared
// bitmap, merges uprobe and Go-cover data, then sends the bitmap to the host
// if it changed since the last send.
func (s *kcovState) flushAndSend() {
	// Drain every per-OS-thread KCOV buffer registered by long-lived goroutines
	// via enableKcovForCurrentThread(). Each buffer is folded into the same
	// shared bitmap using AFL-style edge encoding. Per-thread buffers do not
	// share s.lastPC; each thread's PC stream is independent, so prev is kept
	// local per drain.
	extraKcovMu.RLock()
	threads := extraKcov
	extraKcovMu.RUnlock()
	for _, ts := range threads {
		s.foldThreadBuffer(ts)
	}

	// Atomically read and reset the count in buf[0].
	count := atomic.SwapUint64(&s.buf[0], 0)
	if count == 0 && s.bitmap == nil {
		return
	}
	// buf has kcovEntries slots; buf[0] is the count word, so the max valid
	// PC index is kcovEntries-1.
	if count > kcovEntries-1 {
		count = kcovEntries - 1
	}

	// Fold (prevPC, curPC) pairs into the bitmap using AFL-style edge encoding:
	//   edge_slot = ((prevPC >> 1) XOR curPC) mod bitmapSize
	//
	// Kernel KCOV trace: buf[0]=count, buf[1..count]=PCs. After AtomicSwap,
	// buf[0]=0 and PCs are at buf[1..count].
	//
	// Use s.lastPC as the initial prev so the edge between the last PC of the
	// previous burst and the first PC of this burst is not lost. Without this,
	// different burst-boundary positions cause different cross-boundary edges to
	// be skipped, making cov_hash diverge even across identical instruction
	// sequences.
	if count >= 1 {
		target := s.bitmap
		if target == nil {
			// No shared file; fall back to prevBitmap so we still send something.
			target = s.prevBitmap[:]
		}
		prev := s.lastPC
		for i := uint64(1); i <= count; i++ { // PCs are at buf[1..count]
			cur := s.buf[i]
			if prev != 0 { // skip if no prior context (start of trace after reset)
				slot := ((prev >> 1) ^ cur) % bitmapSize
				old := target[slot]
				if old < 255 {
					target[slot] = old + 1
				}
			}
			prev = cur
		}
		s.lastPC = s.buf[count] // save last PC to bridge the next burst
	}

	// Merge uprobe (userspace) hit counts into the shared bitmap so that
	// application-level code paths (Raft consensus, leader election) are
	// visible to the coverage-guided explorer alongside kernel edges.
	// globalUprobe is nil-safe; if uprobes are disabled mergeIntoSlice is
	// never called.
	if globalUprobe != nil {
		target := s.bitmap
		if target == nil {
			target = s.prevBitmap[:]
		}
		globalUprobe.mergeIntoSlice(target)
	}

	// Merge Go -cover block-level counters into the shared bitmap.
	// globalGoCover is nil-safe; when GOCOVERDIR is not set the collector is
	// disabled and mergeIntoSlice returns immediately.
	if globalGoCover != nil {
		target := s.bitmap
		if target == nil {
			target = s.prevBitmap[:]
		}
		globalGoCover.mergeIntoSlice(target)
	}

	// Take a snapshot to send. Use the shared file if available, otherwise
	// fall back to prevBitmap.
	var sendBuf [bitmapSize]byte
	if s.bitmap != nil {
		copy(sendBuf[:], s.bitmap)
	} else {
		sendBuf = s.prevBitmap
	}

	// Only send if new edges appeared. prevBitmap tracks the last sent state;
	// skipping unchanged bitmaps prevents saturating the vsock with redundant
	// 64 KB messages on quiet bursts.
	if sendBuf == s.prevBitmap {
		return
	}

	s.prevBitmap = sendBuf // update before send to avoid double-send on error
	atomic.AddUint64(&s.lastGen, 1)
	sendCoverageToHost(sendBuf[:])
}

// foldThreadBuffer drains a per-OS-thread KCOV trace buffer and folds its
// (prevPC, curPC) pairs into the shared bitmap. Each thread's PC stream is
// independent; prev is local to this drain and is not bridged across threads.
func (s *kcovState) foldThreadBuffer(ts *kcovThreadState) {
	count := atomic.SwapUint64(&ts.buf[0], 0)
	if count == 0 {
		return
	}
	if count > kcovEntries-1 {
		count = kcovEntries - 1
	}
	target := s.bitmap
	if target == nil {
		target = s.prevBitmap[:]
	}
	var prev uint64
	for i := uint64(1); i <= count; i++ {
		cur := ts.buf[i]
		if prev != 0 {
			slot := ((prev >> 1) ^ cur) % bitmapSize
			if old := target[slot]; old < 255 {
				target[slot] = old + 1
			}
		}
		prev = cur
	}
}

// sendCoverageToHost sends an AFL-style edge bitmap to the host over the
// virtio-serial / vsock channel.
func sendCoverageToHost(bitmap []byte) {
	type coveragePayload struct {
		Source string `json:"source"`
		Data   string `json:"data"` // base64-encoded bitmap
	}
	sendToHost("coverage", coveragePayload{
		Source: "kcov-kernel",
		Data:   base64.StdEncoding.EncodeToString(bitmap),
	})
}
