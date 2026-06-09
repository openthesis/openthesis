# gVisor Patches for Deterministic Simulation Testing

This document specifies the patches applied to [gVisor](https://github.com/google/gvisor)
to turn its Sentry kernel into a fully deterministic simulation runtime for OpenThesis.

The patched `runsc` replaces QEMU+guest-kernel as the execution backend when full
hardware emulation is not required. Because gVisor already interposes on every
syscall (via its KVM or ptrace platform), it is a natural fit for deterministic
replay: we control every source of non-determinism at the Sentry level rather
than at the instruction level.

## Design Principles

1. **Total determinism** -- Given the same seed and the same container image, every
   execution produces the same sequence of observable events (syscall results,
   network traffic, file contents, scheduling order).

2. **Instruction-counted time** -- Instead of advancing virtual time only at coarse
   scheduling points, we count retired instructions via `perf_event` / `SIGPROF`
   and advance the virtual clock proportionally. This gives sub-syscall time
   resolution while remaining fully reproducible (instruction counts are
   deterministic when scheduling is deterministic).

3. **Cooperative multi-sandbox** -- OpenThesis runs distributed systems where each
   node is a separate gVisor sandbox. The control plane coordinates atomic
   checkpoints across all sandboxes so the entire distributed state can be
   snapshotted and restored as a unit.

4. **Memory-mapped coverage** -- The Sentry shares a coverage bitmap with the host
   process via a `memfd` + `mmap`. The explorer reads coverage without any IPC
   or serialization overhead.

5. **Syscall-level fault injection** -- In addition to network faults, individual
   syscalls can be made to fail with configurable errno values (`EIO`, `EAGAIN`,
   `ENOMEM`, `ECONNRESET`, `ETIMEDOUT`) based on a fault schedule provided by the
   control plane.

---

## Patch Overview

| # | Patch file | Subsystem | Summary |
|---|-----------|-----------|---------|
| 1 | `0001-virtual-time.patch` | `pkg/sentry/time` | Replace `CalibratedClock` with deterministic `VirtualClock` |
| 2 | `0002-deterministic-rng.patch` | `pkg/sentry/kernel` | Seed-based ChaCha20 PRNG for all entropy sources |
| 3 | `0003-deterministic-scheduler.patch` | `pkg/sentry/kernel` | TID-ordered cooperative task scheduling |
| 4 | `0004-deterministic-network.patch` | `pkg/tcpip` | Ordered packet delivery with fault injection |
| 5 | `0005-deterministic-filesystem.patch` | `pkg/sentry/vfs` | Deterministic inodes, sorted readdir, fixed timestamps |
| 6 | `0006-fast-checkpoint.patch` | `pkg/sentry/kernel` | sfork-based CoW snapshots |
| 7 | `0007-control-socket.patch` | `runsc/sandbox` | Unix socket for pause/resume/checkpoint/inject commands |
| 8 | `0008-coverage-bitmap.patch` | `pkg/sentry/kernel` | Shared-memory coverage collection via memfd |
| 9 | `0009-syscall-fault-injection.patch` | `pkg/sentry/kernel` | Per-syscall errno injection |
| 10 | `0010-instruction-counter.patch` | `pkg/sentry/platform` | Instruction counting for accurate virtual time |
| 11 | `0011-dst-wiring.patch` | `runsc/boot` | Wire all DST components into loader and kernel |
| 12 | `0012-seccomp-fixes.patch` | `runsc/boot/filter`, `runsc/fsgofer/filter`, `pkg/sentry/kernel` | Seccomp fixes for Go 1.22+ accept4/getsockname + scheduler idle advance |
| 13 | `0013-additional-rand-sources.patch` | `pkg/rand`, `pkg/sentry/inet`, `pkg/sentry/mm`, `pkg/sentry/socket/netlink`, `pkg/sentry/kernel` | Round 2 audit: route all remaining math/rand sources through dstAwareReader (netlink port, abstract socket autobind, stack ASLR, syslog, CPU clock ticker) |
| 14 | `0014-kvm-determinism.patch` | `pkg/sentry/platform/kvm`, `pkg/sentry/platform` | KVM platform: RDRAND/RDSEED CPUID masking, virtual TSC via KVM_SET_MSRS per SwitchToUser, DSTClocker interface, seccomp filter for 19 KVM ioctls |
| 15 | `0015-syscall-time-signal-fixes.patch` | `pkg/sentry/syscalls/linux` | gettimeofday(tz) returns UTC in DST mode; SIGURG/SIGPROF/SIGVTALRM dropped in DST mode |
| 16 | `0016-dst-seed-flag.patch` | `runsc/config` | `--openthesis-dst-seed` flag for reproducible seed injection |
| 17 | `0017-loader-kvm-wiring.patch` | `runsc/boot` | Wire DSTClocker (virtual TSC) into KVM platform after InitDST(); use DSTSeed from config |
| 18 | `0018-tcpip-restore-fix.patch` | `pkg/tcpip/stack` | tcpip Stack.afterLoad seeds insecureRNG from DST-aware reader instead of wall clock |
| 19 | `0019-dst-kernel-and-filters.patch` | `pkg/sentry/kernel`, `runsc/dst`, `runsc/boot/filter`, `runsc/fsgofer/filter` | SetDeterministicSource in InitDST; scheduler TaskBlocking/currentBlocking; accept4+getsockname seccomp allowlist |
| 20 | `0020-virtual-clock-updates.patch` | `pkg/sentry/time` | VirtualClock AdvanceQuantum/AdvanceInstructions/Freeze/Thaw/MonotonicNS updates |

---

## Patch 1: Virtual Time

**Files modified:**
- `pkg/sentry/time/calibrated_clock.go` -- replaced
- `pkg/sentry/time/virtual_clock.go` -- new
- `pkg/sentry/time/parameters.go` -- updated (vDSO parameter page)
- `pkg/sentry/time/vdso.go` -- updated (vDSO clock mapping)
- `pkg/sentry/kernel/timekeeper.go` -- updated
- `runsc/boot/loader.go` -- updated (create VirtualClock when DST enabled)

### Concept

All time sources visible to the guest (`clock_gettime` with `CLOCK_REALTIME`,
`CLOCK_MONOTONIC`, `CLOCK_BOOTTIME`; `gettimeofday`; `time()`) return values
from a virtual clock. The virtual clock starts at a configurable epoch and
advances only when the scheduler explicitly ticks it.

Time advances in two ways:

1. **Scheduling quanta** -- Each time the scheduler selects a task, the virtual
   clock advances by a fixed quantum (default 10ms). This provides coarse-grained
   deterministic time.

2. **Instruction counting** -- When the instruction counter (patch 10) is active,
   the clock additionally advances by `instructions_retired / ips_rate` where
   `ips_rate` is a configurable instructions-per-second rate (default 1 GHz).
   This gives fine-grained time within a single scheduling quantum.

### Key Types

```go
// pkg/sentry/time/virtual_clock.go

// VirtualClock provides deterministic time for the Sentry.
// It replaces CalibratedClock in deterministic mode.
type VirtualClock struct {
    mu sync.Mutex

    // epoch is the base time (configurable, default 2024-01-01T00:00:00Z).
    epoch time.Time

    // monotonicNS tracks elapsed nanoseconds since boot.
    monotonicNS int64

    // quantumNS is the time added per scheduling quantum.
    quantumNS int64

    // ipsRate is the instructions-per-nanosecond rate for instruction-counted time.
    ipsRate float64

    // frozen is true when the clock is paused (during checkpoint).
    frozen bool
}

// GetTime returns the current virtual time for the given clock ID.
func (vc *VirtualClock) GetTime(clockID int32) (int64, error) {
    vc.mu.Lock()
    defer vc.mu.Unlock()

    switch clockID {
    case linux.CLOCK_REALTIME:
        return vc.epoch.UnixNano() + vc.monotonicNS, nil
    case linux.CLOCK_MONOTONIC, linux.CLOCK_MONOTONIC_RAW,
         linux.CLOCK_MONOTONIC_COARSE, linux.CLOCK_BOOTTIME:
        return vc.monotonicNS, nil
    default:
        return 0, linuxerr.EINVAL
    }
}

// AdvanceQuantum advances the clock by one scheduling quantum.
func (vc *VirtualClock) AdvanceQuantum() {
    vc.mu.Lock()
    defer vc.mu.Unlock()
    if !vc.frozen {
        vc.monotonicNS += vc.quantumNS
    }
}

// AdvanceInstructions advances the clock by the given instruction count.
func (vc *VirtualClock) AdvanceInstructions(count uint64) {
    vc.mu.Lock()
    defer vc.mu.Unlock()
    if !vc.frozen {
        vc.monotonicNS += int64(float64(count) / vc.ipsRate)
    }
}

// Freeze stops the clock (used during checkpointing).
func (vc *VirtualClock) Freeze() {
    vc.mu.Lock()
    defer vc.mu.Unlock()
    vc.frozen = true
}

// Thaw resumes the clock.
func (vc *VirtualClock) Thaw() {
    vc.mu.Lock()
    defer vc.mu.Unlock()
    vc.frozen = false
}
```

### vDSO Parameter Page

The vDSO parameter page is updated to reflect virtual time so that userspace
`clock_gettime` calls (which bypass the syscall path via the vDSO) also return
deterministic values. After each clock advance, `updateVDSOParams` writes the
current monotonic and realtime offsets into the shared page:

```go
// pkg/sentry/time/parameters.go

func (vc *VirtualClock) updateVDSOParams(p *vdsoParams) {
    mono := vc.monotonicNS
    real := vc.epoch.UnixNano() + mono

    // Set both clocks to return fixed values by setting:
    //   mult = 0  (no scaling from TSC)
    //   shift = 0
    //   baseCycles = 0
    //   baseNS = the current virtual time
    // This makes the vDSO formula: time = baseNS + (cycles * 0) = baseNS
    p.monoParams = vdsoClockParams{
        baseCycles: 0,
        baseNS:     mono,
        mult:       0,
        shift:      0,
    }
    p.realParams = vdsoClockParams{
        baseCycles: 0,
        baseNS:     real,
        mult:       0,
        shift:      0,
    }
    atomic.AddUint32(&p.seq, 1) // seqcount update
}
```

---

## Patch 2: Deterministic RNG

**Files modified:**
- `pkg/sentry/kernel/rng.go` -- new
- `pkg/sentry/kernel/kernel.go` -- updated (wire DeterministicRNG)
- `pkg/sentry/syscalls/linux/sys_random.go` -- updated (use DeterministicRNG)
- `pkg/sentry/fsimpl/devtmpfs/devtmpfs.go` -- updated (wire /dev/random, /dev/urandom)

### Concept

All entropy sources in the guest are replaced by a deterministic ChaCha20-based
PRNG seeded from the simulation seed. This covers:

- `getrandom(2)` syscall
- `/dev/random` and `/dev/urandom` device reads
- Internal `rand.Read()` calls within the Sentry

The PRNG state is included in checkpoint/restore so that restored execution
continues with the same random stream.

### Key Types

```go
// pkg/sentry/kernel/rng.go

// DeterministicRNG provides reproducible randomness for the entire sandbox.
// It uses ChaCha20 in counter mode, seeded from the simulation seed.
type DeterministicRNG struct {
    mu    sync.Mutex
    state [32]byte  // ChaCha20 key
    ctr   uint64    // block counter
    buf   [64]byte  // current block
    pos   int       // position within current block
}

// NewDeterministicRNG creates a new PRNG from a 64-bit simulation seed.
// The seed is expanded into a 256-bit ChaCha20 key via SplitMix64.
func NewDeterministicRNG(seed uint64) *DeterministicRNG {
    rng := &DeterministicRNG{}
    // Expand seed into 32-byte key using SplitMix64.
    s := seed
    for i := 0; i < 4; i++ {
        s += 0x9e3779b97f4a7c15
        z := s
        z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
        z = (z ^ (z >> 27)) * 0x94d049bb133111eb
        z = z ^ (z >> 31)
        binary.LittleEndian.PutUint64(rng.state[i*8:(i+1)*8], z)
    }
    rng.pos = len(rng.buf) // force refill on first read
    return rng
}

// Read fills p with deterministic random bytes.
func (r *DeterministicRNG) Read(p []byte) (int, error) {
    r.mu.Lock()
    defer r.mu.Unlock()

    n := 0
    for n < len(p) {
        if r.pos >= len(r.buf) {
            r.refill()
        }
        copied := copy(p[n:], r.buf[r.pos:])
        r.pos += copied
        n += copied
    }
    return n, nil
}

// refill generates the next ChaCha20 block.
func (r *DeterministicRNG) refill() {
    var nonce [12]byte
    binary.LittleEndian.PutUint64(nonce[4:], r.ctr)
    cipher, _ := chacha20.NewUnauthenticatedCipher(r.state[:], nonce[:])
    // Zero buf, then encrypt zeros to get keystream.
    for i := range r.buf {
        r.buf[i] = 0
    }
    cipher.XORKeyStream(r.buf[:], r.buf[:])
    r.ctr++
    r.pos = 0
}

// StateFields implements state.Savable for checkpoint/restore.
func (r *DeterministicRNG) StateFields() []string {
    return []string{"state", "ctr", "pos"}
}
```

### Syscall Integration

The `getrandom(2)` handler is modified to call `DeterministicRNG.Read()`:

```go
// pkg/sentry/syscalls/linux/sys_random.go

func Getrandom(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
    buf := args[0].Pointer()
    length := args[1].SizeT()
    flags := args[2].Int()

    if length > maxRandomRead {
        length = maxRandomRead
    }

    b := make([]byte, length)
    // In deterministic mode, use the seeded PRNG instead of host entropy.
    if t.Kernel().DeterministicMode() {
        t.Kernel().DeterministicRNG().Read(b)
    } else {
        rand.Read(b)
    }

    n, err := t.CopyOutBytes(buf, b)
    return uintptr(n), nil, err
}
```

---

## Patch 3: Deterministic Scheduler

**Files modified:**
- `pkg/sentry/kernel/task_run.go` -- updated
- `pkg/sentry/kernel/task_sched.go` -- updated
- `pkg/sentry/kernel/kernel.go` -- updated (add DeterministicScheduler)
- `pkg/sentry/kernel/dst_scheduler.go` -- new

### Concept

The default gVisor scheduler runs tasks on a Go goroutine pool with no ordering
guarantees. For determinism, we replace this with a cooperative scheduler that
enforces TID-ordered execution:

1. All runnable tasks are placed in a priority queue ordered by TID (ascending).
2. The scheduler selects the lowest-TID runnable task and runs it.
3. A task runs until it completes a syscall, blocks, or exhausts its instruction
   quantum.
4. When a task yields, the scheduler advances the virtual clock and selects the
   next task.

This eliminates all scheduling non-determinism, including from the Go runtime.

### Key Types

```go
// pkg/sentry/kernel/dst_scheduler.go

// DeterministicScheduler enforces TID-ordered cooperative scheduling.
type DeterministicScheduler struct {
    mu sync.Mutex

    // runnable is a min-heap of tasks ordered by TID.
    runnable taskHeap

    // current is the currently executing task (nil if idle).
    current *Task

    // clock is the virtual clock to advance on each quantum.
    clock *time.VirtualClock

    // instrQuanta is the max instructions per scheduling slice.
    instrQuanta uint64

    // wakeup signals the scheduler loop that a new task is runnable.
    wakeup chan struct{}

    // paused is true when the scheduler is frozen (for checkpoint).
    paused bool
}

// taskHeap implements heap.Interface for TID-ordered scheduling.
type taskHeap []*Task

func (h taskHeap) Len() int            { return len(h) }
func (h taskHeap) Less(i, j int) bool  { return h[i].ThreadID() < h[j].ThreadID() }
func (h taskHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *taskHeap) Push(x interface{}) { *h = append(*h, x.(*Task)) }
func (h *taskHeap) Pop() interface{} {
    old := *h
    n := len(old)
    t := old[n-1]
    *h = old[:n-1]
    return t
}

// Schedule runs the main scheduling loop. It never returns.
func (ds *DeterministicScheduler) Schedule() {
    for {
        ds.mu.Lock()
        for ds.runnable.Len() == 0 || ds.paused {
            ds.mu.Unlock()
            <-ds.wakeup
            ds.mu.Lock()
        }

        // Pick the lowest-TID runnable task.
        t := heap.Pop(&ds.runnable).(*Task)
        ds.current = t
        ds.mu.Unlock()

        // Advance the virtual clock by one quantum.
        ds.clock.AdvanceQuantum()

        // Let the task run until it yields (syscall completion, block, or
        // instruction quantum exhausted).
        t.deterministicRun(ds.instrQuanta)

        ds.mu.Lock()
        ds.current = nil

        // If the task is still runnable, re-add it.
        if t.State() == TaskStateRunnable {
            heap.Push(&ds.runnable, t)
        }
        ds.mu.Unlock()
    }
}

// MakeRunnable adds a task to the run queue.
func (ds *DeterministicScheduler) MakeRunnable(t *Task) {
    ds.mu.Lock()
    defer ds.mu.Unlock()
    heap.Push(&ds.runnable, t)
    select {
    case ds.wakeup <- struct{}{}:
    default:
    }
}

// Pause freezes the scheduler (for checkpoint).
func (ds *DeterministicScheduler) Pause() {
    ds.mu.Lock()
    defer ds.mu.Unlock()
    ds.paused = true
    ds.clock.Freeze()
}

// Resume unfreezes the scheduler.
func (ds *DeterministicScheduler) Resume() {
    ds.mu.Lock()
    ds.paused = false
    ds.clock.Thaw()
    ds.mu.Unlock()
    select {
    case ds.wakeup <- struct{}{}:
    default:
    }
}
```

### Task Yield Points

Each task's syscall handler is wrapped to yield after completion:

```go
// pkg/sentry/kernel/task_run.go (modified)

func (t *Task) doSyscall() taskRunState {
    // ... existing syscall dispatch ...
    result := t.executeSyscall(sysno, args)

    // In deterministic mode, yield after every syscall so the scheduler
    // can pick the next task in TID order.
    if t.k.DeterministicMode() {
        t.yield()
    }

    return result
}

// yield returns control to the deterministic scheduler.
func (t *Task) yield() {
    t.k.DeterministicScheduler().taskYielded(t)
    // Block on the task's resume channel until the scheduler picks us again.
    <-t.deterministicResume
}
```

---

## Patch 4: Deterministic Network

**Files modified:**
- `pkg/tcpip/stack/stack.go` -- updated
- `pkg/tcpip/link/deterministic/deterministic.go` -- new
- `pkg/tcpip/network/ipv4/ipv4.go` -- updated (ordered delivery)
- `pkg/tcpip/transport/tcp/connect.go` -- updated (deterministic initial sequence numbers)

### Concept

The netstack network stack is modified for deterministic packet delivery:

1. Each packet is assigned a virtual-time delivery timestamp when it enters the
   stack.
2. Packets are buffered in a priority queue sorted by `(delivery_time, packet_id)`
   where `packet_id` is a monotonically increasing counter.
3. Packets are delivered only when the virtual clock reaches their delivery time.
4. TCP initial sequence numbers are derived from the deterministic RNG.

### Fault Injection

The deterministic link layer supports these fault modes:

| Fault | Description |
|-------|-------------|
| `packet_loss` | Drop packets with probability P (seeded PRNG) |
| `reorder` | Add random delay to delivery timestamp |
| `delay` | Add fixed delay to all packets |
| `partition` | Drop all packets between specified endpoints |

### Key Types

```go
// pkg/tcpip/link/deterministic/deterministic.go

// DeterministicLinkEndpoint wraps a link endpoint with ordered delivery
// and fault injection.
type DeterministicLinkEndpoint struct {
    mu sync.Mutex

    // inner is the wrapped link endpoint.
    inner stack.LinkEndpoint

    // queue is a priority queue of pending packets.
    queue packetQueue

    // nextID is the monotonic packet ID counter.
    nextID uint64

    // clock is the virtual clock for delivery scheduling.
    clock *time.VirtualClock

    // rng is the deterministic RNG for fault injection decisions.
    rng *kernel.DeterministicRNG

    // faults is the current fault injection configuration.
    faults FaultConfig
}

// FaultConfig describes active network faults.
type FaultConfig struct {
    // LossRate is the probability [0.0, 1.0] that a packet is dropped.
    LossRate float64

    // ReorderMaxNS is the maximum reorder delay in nanoseconds.
    ReorderMaxNS int64

    // DelayNS is the fixed delay added to all packets in nanoseconds.
    DelayNS int64

    // Partitions is a set of (src, dst) pairs that are fully partitioned.
    Partitions map[partitionKey]bool
}

type pendingPacket struct {
    deliveryTime int64
    packetID     uint64
    pkt          *stack.PacketBuffer
}

// packetQueue implements a min-heap ordered by (deliveryTime, packetID).
type packetQueue []pendingPacket

func (q packetQueue) Less(i, j int) bool {
    if q[i].deliveryTime != q[j].deliveryTime {
        return q[i].deliveryTime < q[j].deliveryTime
    }
    return q[i].packetID < q[j].packetID
}

// DeliverNetworkPacket enqueues a packet with a deterministic delivery time.
func (d *DeterministicLinkEndpoint) DeliverNetworkPacket(pkt *stack.PacketBuffer) {
    d.mu.Lock()
    defer d.mu.Unlock()

    // Check partition.
    if d.isPartitioned(pkt) {
        pkt.DecRef()
        return
    }

    // Check loss.
    if d.faults.LossRate > 0 {
        var b [8]byte
        d.rng.Read(b[:])
        r := float64(binary.LittleEndian.Uint64(b[:])) / float64(math.MaxUint64)
        if r < d.faults.LossRate {
            pkt.DecRef()
            return
        }
    }

    // Compute delivery time.
    now, _ := d.clock.GetTime(linux.CLOCK_MONOTONIC)
    deliveryTime := now + d.faults.DelayNS

    // Add reorder jitter.
    if d.faults.ReorderMaxNS > 0 {
        var b [8]byte
        d.rng.Read(b[:])
        jitter := int64(binary.LittleEndian.Uint64(b[:]) % uint64(d.faults.ReorderMaxNS))
        deliveryTime += jitter
    }

    d.nextID++
    heap.Push(&d.queue, pendingPacket{
        deliveryTime: deliveryTime,
        packetID:     d.nextID,
        pkt:          pkt,
    })
}

// DrainReady delivers all packets whose delivery time has passed.
func (d *DeterministicLinkEndpoint) DrainReady() {
    d.mu.Lock()
    defer d.mu.Unlock()

    now, _ := d.clock.GetTime(linux.CLOCK_MONOTONIC)
    for d.queue.Len() > 0 && d.queue[0].deliveryTime <= now {
        p := heap.Pop(&d.queue).(pendingPacket)
        d.inner.DeliverNetworkPacket(p.pkt)
    }
}
```

---

## Patch 5: Deterministic Filesystem

**Files modified:**
- `pkg/sentry/fsimpl/tmpfs/tmpfs.go` -- updated (sorted readdir, sequential inodes)
- `pkg/sentry/kernel/timekeeper.go` -- updated (fixed timestamps)
- `pkg/sentry/fsimpl/gofer/gofer.go` -- updated (sorted readdir, fixed timestamps)

### Concept

Three aspects of the filesystem are made deterministic:

1. **Inode allocation** -- Inode numbers are assigned sequentially from a seed-based
   starting point. No host inode numbers leak into the guest.

2. **Directory iteration** -- `getdents(2)` / `readdir(3)` always returns entries
   in lexicographic order, regardless of the underlying filesystem's natural order.

3. **Timestamps** -- All file timestamps (`atime`, `mtime`, `ctime`) are derived
   from the virtual clock. No wall-clock times are visible.

### Key Types

```go
// pkg/sentry/fsimpl/tmpfs/tmpfs.go (new type added)

// DeterministicInodeAllocator provides sequential inode numbers.
type DeterministicInodeAllocator struct {
    next atomic.Uint64
}

// NewDeterministicInodeAllocator creates an allocator starting from a
// seed-derived base.
func NewDeterministicInodeAllocator(seed uint64) *DeterministicInodeAllocator {
    a := &DeterministicInodeAllocator{}
    // Start from a seed-derived base to detect inode-dependent bugs.
    base := (seed * 0x9e3779b97f4a7c15) & 0x00FFFFFFFFFFFFFF
    if base < 2 {
        base = 2 // inode 1 is reserved for root
    }
    a.next.Store(base)
    return a
}

// Allocate returns the next deterministic inode number.
func (a *DeterministicInodeAllocator) Allocate() uint64 {
    return a.next.Add(1) - 1
}
```

### Sorted Directory Iteration

```go
// pkg/sentry/fsimpl/tmpfs/tmpfs.go (modified)

// IterDirents returns directory entries in lexicographic order.
func (d *directory) IterDirents(ctx context.Context, cb vfs.IterDirentsCallback, offset int64) (int64, error) {
    // Collect all children into a sorted slice.
    d.mu.RLock()
    entries := make([]vfs.Dirent, 0, len(d.childMap))
    for name, child := range d.childMap {
        entries = append(entries, vfs.Dirent{
            Name:    name,
            Type:    child.Type(),
            Ino:     child.InodeNumber(),
            NextOff: 0, // filled below
        })
    }
    d.mu.RUnlock()

    sort.Slice(entries, func(i, j int) bool {
        return entries[i].Name < entries[j].Name
    })

    // Assign sequential offsets and emit.
    for i := range entries {
        entries[i].NextOff = int64(i) + 1
    }

    for i := int64(0); i < int64(len(entries)); i++ {
        if i < offset {
            continue
        }
        if err := cb.Handle(entries[i]); err != nil {
            return i, err
        }
    }
    return int64(len(entries)), nil
}
```

---

## Patch 6: Fast Checkpoint/Restore

**Files modified:**
- `pkg/sentry/kernel/kernel.go` -- updated (`SaveTo`/`LoadFrom` for sfork support)
- `pkg/sentry/kernel/dst_snapshot.go` -- new (snapshot tree)
- `runsc/sandbox/sandbox.go` -- updated (checkpoint command)

### Concept

Standard gVisor checkpoint serializes the entire Sentry state to a file and
restores it by deserializing. This is too slow for simulation testing where
thousands of checkpoints may be taken per run.

We implement **sfork-based snapshotting**: instead of serializing, we `fork(2)`
the Sentry process. The child becomes the snapshot. Because of copy-on-write
(CoW) pages, this is near-instantaneous regardless of memory size.

### How It Works

1. **Pause** -- The deterministic scheduler pauses all tasks and freezes the clock.

2. **sfork** -- The Sentry calls `fork()`. The child process is the snapshot.
   The child immediately enters a sleep state, waiting for a restore signal.

3. **Resume parent** -- The parent (current execution) resumes. Any memory writes
   trigger CoW page faults, so the child retains the snapshot state.

4. **Restore** -- To restore from a snapshot: signal the snapshot child to wake up,
   terminate the current Sentry process, and let the child take over. The child
   re-attaches to the control socket and I/O channels.

5. **Shadow page tables** -- On the KVM platform, we maintain shadow page tables
   so that the guest physical-to-host virtual mapping survives the fork without
   re-creating the KVM VM. The ptrace platform works naturally with fork.

### Key Types

```go
// pkg/sentry/kernel/dst_snapshot.go

// Snapshot represents a point-in-time snapshot of the entire Sentry state,
// held as a forked child process with CoW pages.
type Snapshot struct {
    // PID is the OS PID of the snapshot child process.
    PID int

    // ID is the unique snapshot identifier.
    ID string

    // VirtualTimeNS is the virtual clock value at snapshot time.
    VirtualTimeNS int64

    // ParentID is the ID of the parent snapshot (empty for root).
    ParentID string

    // Created is the wall-clock time the snapshot was taken (for bookkeeping only).
    Created time.Time
}

// SnapshotTree manages a tree of sfork-based snapshots.
type SnapshotTree struct {
    mu        sync.Mutex
    snapshots map[string]*Snapshot
    current   string
}

// Take creates a new snapshot by forking the Sentry process.
func (st *SnapshotTree) Take(k *Kernel, id string) (*Snapshot, error) {
    // 1. Pause the scheduler and freeze the clock.
    k.DeterministicScheduler().Pause()
    defer k.DeterministicScheduler().Resume()

    // 2. Flush all pending I/O.
    k.FlushPendingIO()

    // 3. Fork.
    pid, _, err := syscall.Syscall(syscall.SYS_FORK, 0, 0, 0)
    if err != 0 {
        return nil, fmt.Errorf("sfork failed: %w", err)
    }

    if pid == 0 {
        // Child: this is the snapshot. Sleep until signaled.
        snapshotChildWait()
        // If woken up, we are being restored. Re-init I/O.
        k.ReconnectIO()
        return nil, errRestored
    }

    // Parent: record the snapshot.
    vt, _ := k.VirtualClock().GetTime(linux.CLOCK_MONOTONIC)
    snap := &Snapshot{
        PID:           int(pid),
        ID:            id,
        VirtualTimeNS: vt,
        ParentID:      st.current,
        Created:       time.Now(),
    }
    st.mu.Lock()
    st.snapshots[id] = snap
    st.mu.Unlock()

    return snap, nil
}

// Restore activates a previous snapshot. This kills the current Sentry
// and wakes the snapshot child.
func (st *SnapshotTree) Restore(id string) error {
    st.mu.Lock()
    snap, ok := st.snapshots[id]
    st.mu.Unlock()
    if !ok {
        return fmt.Errorf("snapshot %q not found", id)
    }

    // Signal the snapshot child to wake up.
    syscall.Kill(snap.PID, syscall.SIGUSR1)

    // Terminate ourselves. The snapshot child takes over.
    os.Exit(0)

    return nil // unreachable
}
```

### Memory Overhead

Each snapshot shares all memory pages with the parent via CoW. Only pages
modified after the snapshot are duplicated. For a typical 256MB sandbox, the
incremental cost of a snapshot is roughly proportional to the number of dirty
pages since the previous snapshot -- typically under 1MB for a checkpoint taken
between syscalls.

---

## Patch 7: Control Socket

**Files modified:**
- `runsc/dst/ctrl.go` -- new (control socket server)
- `runsc/boot/loader.go` -- updated (start control socket)
- `runsc/cmd/run.go` -- updated (accept `--openthesis-control` flag)

### Concept

The patched `runsc` exposes a Unix domain socket for runtime control by the
OpenThesis control plane. The socket path is specified via the
`--openthesis-control=/path/to/sock` flag.

### Wire Protocol

The protocol is line-delimited JSON. Each request is a single JSON object;
each response is a single JSON object.

#### Commands

| Command | Request | Response |
|---------|---------|----------|
| `pause` | `{"cmd":"pause"}` | `{"ok":true}` |
| `resume` | `{"cmd":"resume"}` | `{"ok":true}` |
| `checkpoint` | `{"cmd":"checkpoint","id":"snap-1"}` | `{"ok":true,"snapshot_id":"snap-1","virtual_time_ns":1000000}` |
| `restore` | `{"cmd":"restore","id":"snap-1"}` | Connection closes (new Sentry reconnects) |
| `set-time` | `{"cmd":"set-time","ns":5000000000}` | `{"ok":true}` |
| `set-seed` | `{"cmd":"set-seed","seed":12345}` | `{"ok":true}` |
| `inject-fault` | `{"cmd":"inject-fault","fault":{...}}` | `{"ok":true}` |
| `inject-syscall-fault` | `{"cmd":"inject-syscall-fault","tid":5,"sysno":0,"errno":5}` | `{"ok":true}` |
| `status` | `{"cmd":"status"}` | `{"ok":true,"state":"running","tasks":3,"virtual_time_ns":1000000}` |
| `coverage` | `{"cmd":"coverage"}` | `{"ok":true,"bitmap_fd":"/proc/self/fd/7","size":65536}` |

#### Fault Object

```json
{
    "type": "network",
    "config": {
        "loss_rate": 0.1,
        "delay_ns": 5000000,
        "reorder_max_ns": 1000000,
        "partitions": [
            {"src": "10.0.0.1", "dst": "10.0.0.2"}
        ]
    }
}
```

#### Syscall Fault Object

```json
{
    "cmd": "inject-syscall-fault",
    "tid": 5,
    "sysno": 1,
    "errno": 5,
    "count": 3,
    "probability": 1.0
}
```

This causes TID 5's next 3 `write(2)` calls to fail with `EIO` (errno 5).

### Key Types

```go
// runsc/dst/ctrl.go

// ControlSocket handles OpenThesis control commands.
type ControlSocket struct {
    path     string
    listener net.Listener
    sandbox  *Sandbox
}

// Serve accepts connections and dispatches commands.
func (cs *ControlSocket) Serve() error {
    for {
        conn, err := cs.listener.Accept()
        if err != nil {
            return err
        }
        go cs.handleConn(conn)
    }
}

func (cs *ControlSocket) handleConn(conn net.Conn) {
    defer conn.Close()
    scanner := bufio.NewScanner(conn)
    encoder := json.NewEncoder(conn)

    for scanner.Scan() {
        var req controlRequest
        if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
            encoder.Encode(controlResponse{OK: false, Error: err.Error()})
            continue
        }

        resp := cs.dispatch(req)
        encoder.Encode(resp)
    }
}

func (cs *ControlSocket) dispatch(req controlRequest) controlResponse {
    switch req.Cmd {
    case "pause":
        cs.sandbox.Kernel().DeterministicScheduler().Pause()
        return controlResponse{OK: true}
    case "resume":
        cs.sandbox.Kernel().DeterministicScheduler().Resume()
        return controlResponse{OK: true}
    case "checkpoint":
        snap, err := cs.sandbox.Kernel().SnapshotTree().Take(
            cs.sandbox.Kernel(), req.ID,
        )
        if err != nil {
            return controlResponse{OK: false, Error: err.Error()}
        }
        return controlResponse{
            OK:            true,
            SnapshotID:    snap.ID,
            VirtualTimeNS: snap.VirtualTimeNS,
        }
    case "restore":
        err := cs.sandbox.Kernel().SnapshotTree().Restore(req.ID)
        return controlResponse{OK: false, Error: err.Error()} // unreachable on success
    case "status":
        vt, _ := cs.sandbox.Kernel().VirtualClock().GetTime(linux.CLOCK_MONOTONIC)
        return controlResponse{
            OK:            true,
            State:         cs.sandbox.State(),
            Tasks:         cs.sandbox.Kernel().TaskCount(),
            VirtualTimeNS: vt,
        }
    case "inject-fault":
        return cs.handleFaultInjection(req)
    case "inject-syscall-fault":
        return cs.handleSyscallFaultInjection(req)
    case "coverage":
        return cs.handleCoverage()
    default:
        return controlResponse{OK: false, Error: fmt.Sprintf("unknown command: %s", req.Cmd)}
    }
}
```

---

## Patch 8: Coverage Bitmap

**Files modified:**
- `pkg/sentry/kernel/coverage.go` -- new
- `pkg/sentry/kernel/kernel.go` -- updated (initialize coverage)
- `pkg/sentry/kernel/task_syscall.go` -- updated (record coverage)

### Concept

Coverage is collected via a shared memory bitmap. The Sentry creates a `memfd`,
maps it, and records a bit for each unique (PC, syscall) pair observed. The host
reads the bitmap directly via the memfd file descriptor -- no IPC, no
serialization.

The bitmap is sized at 64KB (512K bits) by default, using modular hashing to
map PCs to bit positions. This matches the AFL-style coverage bitmap approach.

### Key Types

```go
// pkg/sentry/kernel/coverage.go

// CoverageBitmap provides shared-memory code coverage collection.
type CoverageBitmap struct {
    // fd is the memfd file descriptor.
    fd int

    // bitmap is the mmap'd coverage bitmap.
    bitmap []byte

    // size is the bitmap size in bytes.
    size int

    // prevPC tracks the previous PC for edge coverage.
    prevPC uint64
}

// NewCoverageBitmap creates a new coverage bitmap backed by a memfd.
func NewCoverageBitmap(size int) (*CoverageBitmap, error) {
    fd, err := unix.MemfdCreate("openthesis-coverage", 0)
    if err != nil {
        return nil, fmt.Errorf("memfd_create: %w", err)
    }

    if err := unix.Ftruncate(fd, int64(size)); err != nil {
        unix.Close(fd)
        return nil, fmt.Errorf("ftruncate: %w", err)
    }

    bitmap, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
    if err != nil {
        unix.Close(fd)
        return nil, fmt.Errorf("mmap: %w", err)
    }

    return &CoverageBitmap{
        fd:     fd,
        bitmap: bitmap,
        size:   size,
    }, nil
}

// RecordEdge records a control flow edge from prevPC to pc.
func (cb *CoverageBitmap) RecordEdge(pc uint64) {
    // XOR with previous PC for edge coverage (AFL-style).
    hash := (pc ^ cb.prevPC) % uint64(cb.size*8)
    byteIdx := hash / 8
    bitIdx := hash % 8
    cb.bitmap[byteIdx] |= 1 << bitIdx
    cb.prevPC = pc >> 1
}

// FD returns the memfd file descriptor for sharing with the host.
func (cb *CoverageBitmap) FD() int {
    return cb.fd
}

// Count returns the number of edges seen.
func (cb *CoverageBitmap) Count() int {
    count := 0
    for _, b := range cb.bitmap {
        count += bits.OnesCount8(b)
    }
    return count
}

// Reset clears the bitmap.
func (cb *CoverageBitmap) Reset() {
    for i := range cb.bitmap {
        cb.bitmap[i] = 0
    }
    cb.prevPC = 0
}
```

---

## Patch 9: Syscall-Level Fault Injection

**Files modified:**
- `pkg/sentry/kernel/syscall_fault.go` -- new
- `pkg/sentry/kernel/task_syscall.go` -- updated (check fault before dispatch)
- `pkg/sentry/kernel/kernel.go` -- updated (SyscallFaultTable)

### Concept

Individual syscalls can be made to fail with specific errno values. Faults are
configured per-TID and per-sysno, with an optional count (how many times to
fail) and probability (for stochastic faults using the deterministic RNG).

This enables testing of error handling paths that are difficult to trigger
in normal execution: `ENOMEM` from `mmap`, `EIO` from `read`, `ECONNRESET`
from `recvmsg`, etc.

### Key Types

```go
// pkg/sentry/kernel/syscall_fault.go

// SyscallFault describes a fault to inject for a specific syscall.
type SyscallFault struct {
    // TID is the target task's thread ID. 0 means all tasks.
    TID int32

    // Sysno is the syscall number to fault. -1 means all syscalls.
    Sysno int64

    // Errno is the error to return.
    Errno int32

    // Count is the remaining number of times to inject this fault.
    // -1 means infinite.
    Count int32

    // Probability is the chance [0.0, 1.0] of injecting per invocation.
    Probability float64
}

// SyscallFaultTable manages active syscall faults.
type SyscallFaultTable struct {
    mu     sync.Mutex
    faults []SyscallFault
    rng    *DeterministicRNG
}

// Check tests whether the given syscall should be faulted.
// Returns (errno, true) if the syscall should fail, (0, false) otherwise.
func (sft *SyscallFaultTable) Check(tid int32, sysno uintptr) (int32, bool) {
    sft.mu.Lock()
    defer sft.mu.Unlock()

    for i := range sft.faults {
        f := &sft.faults[i]

        // Match TID (0 = wildcard).
        if f.TID != 0 && f.TID != tid {
            continue
        }

        // Match sysno (-1 = wildcard).
        if f.Sysno >= 0 && f.Sysno != int64(sysno) {
            continue
        }

        // Check remaining count.
        if f.Count == 0 {
            continue
        }

        // Check probability.
        if f.Probability < 1.0 {
            var b [8]byte
            sft.rng.Read(b[:])
            r := float64(binary.LittleEndian.Uint64(b[:])) / float64(math.MaxUint64)
            if r >= f.Probability {
                continue
            }
        }

        // Inject the fault.
        if f.Count > 0 {
            f.Count--
        }

        // Remove exhausted faults.
        if f.Count == 0 {
            sft.faults = append(sft.faults[:i], sft.faults[i+1:]...)
        }

        return f.Errno, true
    }

    return 0, false
}

// Add adds a new syscall fault.
func (sft *SyscallFaultTable) Add(fault SyscallFault) {
    sft.mu.Lock()
    defer sft.mu.Unlock()
    sft.faults = append(sft.faults, fault)
}
```

### Syscall Dispatch Integration

```go
// pkg/sentry/kernel/task_syscall.go (modified)

func (t *Task) executeSyscall(sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
    // Check for injected faults before dispatching.
    if t.k.DeterministicMode() {
        if errno, faulted := t.k.SyscallFaultTable().Check(t.ThreadID(), sysno); faulted {
            return 0, linuxerr.ErrorFromErrno(errno)
        }
    }

    // Normal dispatch.
    return t.doSyscallDispatch(sysno, args)
}
```

---

## Patch 10: Instruction Counter

**Files modified:**
- `pkg/sentry/platform/kvm/machine.go` -- updated
- `pkg/sentry/platform/ptrace/ptrace.go` -- updated
- `pkg/sentry/platform/dst_counter.go` -- new

### Concept

For accurate virtual time, we count the number of instructions retired by each
task. On the KVM platform, this uses `perf_event_open` with
`PERF_COUNT_HW_INSTRUCTIONS`. On the ptrace platform, we use `PTRACE_PEEKUSER`
to read the performance counters, or fall back to `SIGPROF`-based sampling.

The instruction count is fed into the virtual clock (patch 1) to advance time
proportionally.

### Key Types

```go
// pkg/sentry/platform/dst_counter.go

// InstructionCounter tracks retired instructions for a task.
type InstructionCounter struct {
    // fd is the perf_event file descriptor (-1 if unavailable).
    fd int

    // total is the cumulative instruction count.
    total uint64

    // available indicates whether hardware counters are available.
    available bool
}

// NewInstructionCounter creates a counter for the given OS thread.
func NewInstructionCounter(tid int) *InstructionCounter {
    attr := unix.PerfEventAttr{
        Type:   unix.PERF_TYPE_HARDWARE,
        Config: unix.PERF_COUNT_HW_INSTRUCTIONS,
        Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
        Bits:   unix.PerfBitDisabled | unix.PerfBitExcludeKernel,
    }

    fd, err := unix.PerfEventOpen(&attr, tid, -1, -1, unix.PERF_FLAG_FD_CLOEXEC)
    if err != nil {
        return &InstructionCounter{fd: -1, available: false}
    }

    return &InstructionCounter{fd: fd, available: true}
}

// Enable starts counting instructions.
func (ic *InstructionCounter) Enable() {
    if ic.available {
        unix.IoctlSetInt(ic.fd, unix.PERF_EVENT_IOC_ENABLE, 0)
    }
}

// Disable stops counting and returns instructions since last read.
func (ic *InstructionCounter) Disable() uint64 {
    if !ic.available {
        return 0
    }

    unix.IoctlSetInt(ic.fd, unix.PERF_EVENT_IOC_DISABLE, 0)

    var count uint64
    buf := make([]byte, 8)
    unix.Read(ic.fd, buf)
    count = binary.LittleEndian.Uint64(buf)

    delta := count - ic.total
    ic.total = count

    // Reset the counter for the next slice.
    unix.IoctlSetInt(ic.fd, unix.PERF_EVENT_IOC_RESET, 0)

    return delta
}

// Close releases the perf_event file descriptor.
func (ic *InstructionCounter) Close() {
    if ic.available {
        unix.Close(ic.fd)
    }
}
```

---

## Build Requirements

The patched gVisor requires:

- Go 1.22+ (gVisor's minimum)
- Bazel 7.x (gVisor's build system)
- Linux x86_64 host with KVM support (for the KVM platform)
- `perf_event_open` access for instruction counting (may require `perf_event_paranoid <= 1`)

See `deploy/build-gvisor.sh` for the full build procedure.

## Relationship to QEMU Patches

The gVisor patches provide an alternative execution backend to the patched QEMU.
Both backends expose the same control socket protocol and coverage bitmap
interface, so the OpenThesis control plane works identically with either.

| Feature | QEMU backend | gVisor backend |
|---------|-------------|----------------|
| Isolation level | Full hardware VM | Syscall interception |
| Determinism granularity | Instruction-level | Syscall-level |
| Checkpoint speed | ~100ms (QEMU snapshot) | ~1ms (sfork CoW) |
| Memory overhead per snapshot | Full memory copy | CoW pages only |
| Startup time | ~1s (VM boot) | ~50ms (sandbox start) |
| Kernel compatibility | Any guest kernel | Linux syscall ABI only |
| Multi-node coordination | Separate QEMU instances | Separate sandboxes (same host) |

For most distributed system testing, the gVisor backend is preferred because of
its dramatically faster checkpoint/restore and lower per-node overhead. The QEMU
backend is used when kernel-level determinism is required (e.g., testing kernel
modules or drivers).
