package hypervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// guestArchFromBinary returns "arm64" when the QEMU binary name contains
// "aarch64", and "x86_64" otherwise. Used to select machine type and
// kernel cmdline console device automatically.
func guestArchFromBinary(binary string) string {
	base := filepath.Base(binary)
	if strings.Contains(base, "aarch64") {
		return "arm64"
	}
	return "x86_64"
}

// DefaultVMConfig returns a VMConfig with deterministic defaults.
// VCPUs is always 1 because multi-core introduces non-deterministic scheduling.
// RequirePMC is true so the Firecracker backend hard-fails on a host that
// cannot provide PMC-based virtual time rather than silently degrading.
func DefaultVMConfig(name string, seed uint64) VMConfig {
	stateDir := func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return "/var/lib/openthesis"
		}
		return filepath.Join(home, ".openthesis")
	}()
	return VMConfig{
		Name:       name,
		MemoryMB:   512,
		VCPUs:      1,
		Seed:       seed,
		StateDir:   stateDir,
		RequirePMC: true,
	}
}

// QEMUArgs builds the QEMU command-line arguments from a VMConfig.
// The flags disable every source of host-dependent non-determinism:
// single-threaded TCG, no hardware RNG, VM-locked RTC, single vCPU,
// and instruction-counted virtual time.
//
// The -icount flag is the critical piece for deterministic simulation:
//
//	shift=7: virtual_time = instructions_executed * 128ns (deterministic)
//	sleep=off: when the guest executes HLT (any sleep/idle), the virtual
//	           clock instantly jumps to the next timer event; zero wall
//	           time passes. This is how "sleep is free" works.
//	align=off: don't try to match host wall clock
//
// This means guest time.Sleep(500ms) completes in microseconds of real
// time. The workload can complete hundreds of iterations per burst.
// The qemuBinary path is used to detect the guest architecture: a binary
// named qemu-system-aarch64 produces arm64 machine flags, anything else
// produces x86_64 flags. Pass "" to default to x86_64.
func QEMUArgs(cfg VMConfig, qmpSocket, serialSocket, consoleLog string) []string {
	return qemuArgsForBinary("", cfg, qmpSocket, serialSocket, consoleLog)
}

// QEMUArgsForBinary is like QEMUArgs but derives machine type from the binary.
func QEMUArgsForBinary(binary string, cfg VMConfig, qmpSocket, serialSocket, consoleLog string) []string {
	return qemuArgsForBinary(binary, cfg, qmpSocket, serialSocket, consoleLog)
}

func qemuArgsForBinary(binary string, cfg VMConfig, qmpSocket, serialSocket, consoleLog string) []string {
	arch := guestArchFromBinary(binary)

	icountFlags := "shift=7,sleep=off,align=off"
	if cfg.RecordReplay == "record" || cfg.RecordReplay == "replay" {
		icountFlags += fmt.Sprintf(",rr=%s,rrfile=%s", cfg.RecordReplay, cfg.ReplayFile)
		if cfg.RecordReplay == "record" {
			icountFlags += ",rrsnapshot=init"
		}
	}

	var args []string
	if arch == "arm64" {
		args = []string{
			"-machine", "virt",
			"-accel", "tcg,thread=single",
			"-cpu", "cortex-a57",
			"-icount", icountFlags,
			"-smp", "1",
			"-m", fmt.Sprintf("%d", cfg.MemoryMB),
			"-nographic",
			"-no-reboot",
		}
	} else {
		args = []string{
			"-accel", "tcg,thread=single",
			"-cpu", "qemu64,-rdrand,-rdseed",
			"-rtc", "base=2024-01-01T00:00:00,clock=vm,driftfix=none",
			"-icount", icountFlags,
			"-smp", "1",
			"-m", fmt.Sprintf("%d", cfg.MemoryMB),
			"-nographic",
			"-no-reboot",
		}
	}

	if cfg.KernelPath != "" {
		args = append(args, "-kernel", cfg.KernelPath)
		args = append(args, "-append", KernelCmdlineForArch(cfg.Seed, arch))
	}

	if cfg.InitrdPath != "" {
		args = append(args, "-initrd", cfg.InitrdPath)
	}

	if cfg.RootFSPath != "" {
		format := "raw"
		if strings.HasSuffix(cfg.RootFSPath, ".qcow2") {
			format = "qcow2"
		}
		if cfg.RecordReplay != "" {
			args = append(args,
				"-drive", fmt.Sprintf("file=%s,format=%s,if=none,id=img-direct,snapshot=on", cfg.RootFSPath, format),
				"-drive", "driver=blkreplay,if=none,image=img-direct,id=img-blkreplay",
				"-device", "virtio-blk-pci,drive=img-blkreplay",
			)
		} else {
			args = append(args,
				"-drive", fmt.Sprintf("file=%s,format=%s,if=virtio,snapshot=on", cfg.RootFSPath, format),
			)
		}
	}

	args = append(args,
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", qmpSocket),
	)

	args = append(args,
		"-serial", fmt.Sprintf("file:%s", consoleLog),
		"-chardev", fmt.Sprintf("socket,id=virtserial0,path=%s,server=on,wait=off", serialSocket),
		"-device", "virtio-serial",
		"-device", "virtserialport,chardev=virtserial0,name=agent.0",
	)

	args = append(args,
		"-netdev", "user,id=net0",
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", DeterministicMAC(cfg.Seed)),
	)
	if cfg.RecordReplay != "" {
		args = append(args, "-object", "filter-replay,id=replay,netdev=net0")
	}

	if cfg.GDBPort > 0 && cfg.RecordReplay == "replay" {
		args = append(args, "-gdb", fmt.Sprintf("tcp::%d", cfg.GDBPort), "-S")
	}

	return args
}

// PatchedQEMUArgs builds QEMU command-line arguments for a patched binary.
// The patched build enables CoW snapshots, hypercalls, and deterministic I/O
// ordering via CONFIG_OPENTHESIS (compile-time). Memory is backed by a shared
// tmpfs region (/dev/shm) so the host can map the coverage bitmap directly.
//
// Compared to QEMUArgs:
//   - Uses memory-backend-file on /dev/shm instead of -m for shared memory
//   - Uses -numa node,memdev=mem to attach the shared backend
func PatchedQEMUArgs(cfg VMConfig, qmpSocket, serialSocket, consoleLog string) []string {
	icountFlags := "shift=7,sleep=off,align=off"
	if cfg.RecordReplay == "record" || cfg.RecordReplay == "replay" {
		icountFlags += fmt.Sprintf(",rr=%s,rrfile=%s", cfg.RecordReplay, cfg.ReplayFile)
		if cfg.RecordReplay == "record" {
			icountFlags += ",rrsnapshot=init"
		}
	}

	args := []string{
		"-accel", "tcg,thread=single",
		"-cpu", "qemu64,-rdrand,-rdseed",
		"-rtc", "base=2024-01-01T00:00:00,clock=vm,driftfix=none",
		"-icount", icountFlags,
		"-smp", "1",
		"-m", fmt.Sprintf("%d", cfg.MemoryMB),
		"-object", fmt.Sprintf("memory-backend-file,id=mem,size=%dM,mem-path=/dev/shm/openthesis-%s,share=on", cfg.MemoryMB, cfg.Name),
		"-numa", "node,memdev=mem",
		"-nographic",
		"-no-reboot",
	}

	if cfg.KernelPath != "" {
		args = append(args, "-kernel", cfg.KernelPath)
		args = append(args, "-append", KernelCmdline(cfg.Seed))
	}

	if cfg.InitrdPath != "" {
		args = append(args, "-initrd", cfg.InitrdPath)
	}

	if cfg.RootFSPath != "" {
		format := "raw"
		if strings.HasSuffix(cfg.RootFSPath, ".qcow2") {
			format = "qcow2"
		}
		if cfg.RecordReplay != "" {
			args = append(args,
				"-drive", fmt.Sprintf("file=%s,format=%s,if=none,id=img-direct,snapshot=on", cfg.RootFSPath, format),
				"-drive", "driver=blkreplay,if=none,image=img-direct,id=img-blkreplay",
				"-device", "virtio-blk-pci,drive=img-blkreplay",
			)
		} else {
			args = append(args,
				"-drive", fmt.Sprintf("file=%s,format=%s,if=virtio,snapshot=on", cfg.RootFSPath, format),
			)
		}
	}

	args = append(args,
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", qmpSocket),
	)

	args = append(args,
		"-serial", fmt.Sprintf("file:%s", consoleLog),
		"-chardev", fmt.Sprintf("socket,id=virtserial0,path=%s,server=on,wait=off", serialSocket),
		"-device", "virtio-serial",
		"-device", "virtserialport,chardev=virtserial0,name=agent.0",
	)

	args = append(args,
		"-netdev", "user,id=net0",
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", DeterministicMAC(cfg.Seed)),
	)
	if cfg.RecordReplay != "" {
		args = append(args, "-object", "filter-replay,id=replay,netdev=net0")
	}

	if cfg.GDBPort > 0 && cfg.RecordReplay == "replay" {
		args = append(args, "-gdb", fmt.Sprintf("tcp::%d", cfg.GDBPort), "-S")
	}

	return args
}

// KernelCmdline returns the kernel command line for x86_64 deterministic boot.
// Each flag eliminates a source of non-determinism:
//   - nokaslr: disables KASLR so code addresses are fixed
//   - random.trust_cpu=off: prevents seeding from hardware RDRAND
//   - random.trust_bootloader=on: seeds CRNG from a deterministic bootloader value
//   - intel_pstate=disable: uses simple frequency governor
//   - nohz=off, highres=off: disable tickless and high-res timers
//   - clocksource=tsc, tsc=reliable: use TSC locked to VM virtual clock
//   - no_timer_check: skip timer sanity checks (needed with icount)
//   - preempt=none: cooperative kernel scheduler; eliminates involuntary context-switch
//     non-determinism (valid because guest kernel has PREEMPT_NONE=y)
//   - idle=halt, cpuidle.off=1: force HLT-based idle; QEMU icount sleep=off fast-forwards
//     virtual time through HLT at zero wall cost, eliminating idle-loop instruction variance
//   - GORANDSEED: Linux passes unrecognised KEY=VALUE cmdline args to PID 1 as env vars.
//     The Go runtime reads GORANDSEED in runtime.randinit() to seed the global PRNG
//     deterministically, making map iteration, select, math/rand, and the goroutine
//     scheduler reproducible across runs with the same seed.
func KernelCmdline(seed uint64) string {
	return KernelCmdlineForArch(seed, "x86_64")
}

// KernelCmdlineForArch returns the kernel command line for the given guest arch.
// arm64 uses ttyAMA0 as the serial console and omits x86-specific flags.
func KernelCmdlineForArch(seed uint64, arch string) string {
	if arch == "arm64" {
		return fmt.Sprintf(
			"console=ttyAMA0 nokaslr "+
				"random.trust_cpu=off random.trust_bootloader=on "+
				"nohz=off highres=off "+
				"preempt=none "+
				"GORANDSEED=openthesis-%d "+
				"openthesis.seed=%d",
			seed, seed,
		)
	}
	return fmt.Sprintf(
		"console=ttyS0 nokaslr "+
			"random.trust_cpu=off random.trust_bootloader=on "+
			"intel_pstate=disable nohz=off highres=off "+
			"clocksource=tsc tsc=reliable no_timer_check "+
			"preempt=none "+
			"GORANDSEED=openthesis-%d "+
			"openthesis.seed=%d",
		seed, seed,
	)
}

// DeterministicMAC returns a MAC address derived deterministically from the seed.
// The locally-administered bit is set (02:xx:xx:xx:xx:xx) to avoid collisions
// with real hardware OUIs.
func DeterministicMAC(seed uint64) string {
	z := seed + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= (z >> 31)

	b := [6]byte{
		0x02,
		byte(z >> 8),
		byte(z >> 16),
		byte(z >> 24),
		byte(z >> 32),
		byte(z >> 40),
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}
