package hypervisor

import "fmt"

// Compile-time interface satisfaction.
var (
	_ Hypervisor = (*QEMUHypervisor)(nil)
	_ Hypervisor = (*PatchedQEMUHypervisor)(nil)
	_ Hypervisor = (*GVisorHypervisor)(nil)
	_ Hypervisor = (*FirecrackerHypervisor)(nil)
)

// Backend selects the hypervisor implementation.
type Backend string

const (
	// BackendTCG uses stock QEMU with TCG emulation and icount-based
	// deterministic virtual time. No QEMU patches required. Slowest but
	// simplest; good for development and CI.
	BackendTCG Backend = "tcg"

	// BackendPatched uses a patched QEMU with copy-on-write snapshots,
	// hypercall interface, and full time/RNG/IO virtualization. Requires
	// building QEMU with the openthesis patch series. ~100x faster
	// snapshots than TCG mode.
	BackendPatched Backend = "patched"

	// BackendGVisor uses a patched gVisor (runsc) for container-level
	// deterministic execution. No hardware emulation; the Sentry kernel
	// handles all syscalls deterministically. Fastest boot and snapshot
	// times (~50ms boot, ~1ms snapshot).
	BackendGVisor Backend = "gvisor"

	// BackendFirecracker uses patched Firecracker v1.15.0 as the primary
	// deterministic backend. Firecracker is production-proven at AWS Lambda/Fargate
	// scale, has minimal device surface (no ACPI PM timer, no HPET, no watchdog,
	// no tokio async executor), and is patched for instruction-level PMC-based
	// full instruction-level PMC-based determinism.
	//
	// DST patch series (deploy/firecracker-patches/):
	//   0001: VirtualClock + InstructionCounter (PMC)
	//   0002: PMC wired into vCPU KVM_RUN loop
	//   0003: SplitMix64 replaces aws-lc-rs virtio-rng
	//   0004: RDRAND/RDSEED CPUID masked, invariant TSC forced, 1 GHz leaf
	//   0005: TSC set to 1 GHz synthetic, per-quantum TSC counter reset
	//   0006: DeterministicPacketQueue for virtio-net RX ordering
	//   0007: DST ctrl socket (get/set clock, RNG state, flush DPQ)
	//
	// Requires: KVM, patched binary from deploy/build-firecracker.sh,
	// kernel.perf_event_paranoid=0 (configured by deploy/provision.sh).
	BackendFirecracker Backend = "firecracker"
)

// ParseBackend converts a string to a Backend, returning an error for unknown values.
func ParseBackend(s string) (Backend, error) {
	switch s {
	case "tcg", "":
		return BackendTCG, nil
	case "patched":
		return BackendPatched, nil
	case "gvisor":
		return BackendGVisor, nil
	case "firecracker":
		return BackendFirecracker, nil
	default:
		return "", fmt.Errorf("hypervisor: unknown backend %q (want tcg, patched, gvisor, or firecracker)", s)
	}
}

// NewHypervisor creates a Hypervisor for the given backend.
func NewHypervisor(backend Backend, binary string, stateDir string) (Hypervisor, error) {
	switch backend {
	case BackendTCG:
		return NewQEMU(binary, stateDir), nil
	case BackendPatched:
		return NewPatchedQEMU(binary, stateDir), nil
	case BackendGVisor:
		return NewGVisor(binary, stateDir), nil
	case BackendFirecracker:
		return NewFirecracker(binary, stateDir), nil
	default:
		return nil, fmt.Errorf("hypervisor: unknown backend %q", backend)
	}
}
