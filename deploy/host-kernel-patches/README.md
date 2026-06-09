# OpenThesis Host Kernel Patches

KVM patches that enable userspace RDTSC/RDTSCP interception
(`KVM_CAP_RDTSC_EXITING`) and immediate HLT exit to userspace
(`KVM_CAP_HLT_USER_EXIT`). Required for the Firecracker DST backend's
virtual clock and burst boundary detection. Works on both Intel VMX and AMD SVM.

## Versioned patch sets

```
deploy/host-kernel-patches/
  6.8.0/    Ubuntu 24.04  (linux-source-6.8.0)  RDTSC cap=236  HLT cap=237
  6.18/     Pop!_OS / System76 / vanilla 6.18.x  RDTSC cap=245  HLT cap=246
```

The `build-host-kernel.sh` script auto-detects `uname -r` and selects the
right subdirectory.  To add support for a new kernel version, copy the nearest
set into a new subdirectory, renumber constants in patches 0001 and 0006 to
avoid conflicts with that kernel's existing UAPI, and update `VERSION`.

## Number allocation

| Kernel | `KVM_EXIT_X86_RDTSC` | `KVM_CAP_RDTSC_EXITING` | `KVM_CAP_HLT_USER_EXIT` |
|--------|----------------------|------------------------|------------------------|
| 6.8.x  | 40 (free after 39)   | 236 (free after VM_TYPES=235) | 237 |
| 6.18.x | 41 (TDX=40)          | 245 (free after GUEST_MEMFD_FLAGS=244) | 246 |

Firecracker (patched, built for a specific host kernel) uses the cap numbers
above. The binary and kernel module are built together on the same machine by
`build-firecracker.sh` and `build-host-kernel.sh` respectively, so they always
use matching numbers.

## Building

```bash
# Local machine (auto-detects kernel version):
./deploy/server/build-host-kernel.sh --local

# Remote server:
./deploy/server/build-host-kernel.sh ubuntu@YOUR_SERVER
```

The script downloads kernel source if needed (6.18: vanilla kernel.org tar;
6.8: Ubuntu `linux-source-6.8.0` package), applies only the patches for the
detected CPU vendor (VMX for Intel, SVM for AMD), builds the KVM modules
in-tree, and hot-loads them without a reboot.

## Patch list

Each version set applies in series order:

| # | Patch | Target |
|---|-------|--------|
| 0001 | kvm-uapi: add `KVM_EXIT_X86_RDTSC` exit reason and `KVM_CAP_RDTSC_EXITING` | `include/uapi/linux/kvm.h` |
| 0002 | kvm-x86: add `rdtsc_exiting` flag to `kvm_arch` | `arch/x86/include/asm/kvm_host.h` |
| 0003 | kvm-x86: wire `KVM_CAP_RDTSC_EXITING` capability enable | `arch/x86/kvm/x86.c` |
| 0004 | kvm-vmx: RDTSC/RDTSCP VM-exit handler (Intel only) | `arch/x86/kvm/vmx/vmx.c` |
| 0005 | kvm-svm: RDTSC/RDTSCP intercept handler (AMD only) | `arch/x86/kvm/svm/svm.c` |
| 0006 | kvm-uapi: add `KVM_CAP_HLT_USER_EXIT` | `include/uapi/linux/kvm.h` |
| 0007 | kvm-x86: add `hlt_user_exit` flag to `kvm_arch` | `arch/x86/include/asm/kvm_host.h` |
| 0008 | kvm-x86: wire `KVM_CAP_HLT_USER_EXIT` + early return in `kvm_emulate_halt` | `arch/x86/kvm/x86.c` |

Patches 0004 and 0005 are vendor-exclusive; the build script skips each on
the wrong CPU vendor. Patches 0001-0003 and 0006-0008 are vendor-neutral (they
touch shared x86 code). HLT exits are already intercepted by default on both
VMX (`CPU_BASED_HLT_EXITING`) and SVM (`INTERCEPT_HLT`), so no additional
vendor-specific wiring is needed for patches 0006-0008.

## Protocol

```
RDTSC:
  1. VM creation: enable_cap(KVM_CAP_RDTSC_EXITING)
  2. Guest rdtsc -> VM exit -> kvm_run.exit_reason = KVM_EXIT_X86_RDTSC
  3. Firecracker: write virtual_tsc into kvm_run.rdtsc.tsc_value
  4. vcpu.run() -> kernel delivers EAX:EDX = tsc_value, skips rdtsc

HLT (burst boundary):
  1. VM creation: enable_cap(KVM_CAP_HLT_USER_EXIT)
  2. Guest executes HLT -> VM exit -> kvm_run.exit_reason = KVM_EXIT_HLT
  3. Firecracker DST: records burst boundary, drives next burst
  4. vcpu.run() resumes guest after HLT instruction
```
