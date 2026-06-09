# OpenThesis Host Kernel Patches

KVM patches for Linux 6.8.x that enable userspace-visible RDTSC/RDTSCP
interception.  Required for the Firecracker DST backend's virtual clock
(PMC -> VirtualClock).

Patches 0001-0004 cover Intel VMX.  Patch 0005 adds the AMD SVM equivalent
using the same KVM_CAP_RDTSC_EXITING protocol, enabling full intra-burst
time determinism on AMD EPYC without any other changes to the Firecracker
userspace path.

## What they do

| Patch | File | Change |
|-------|------|--------|
| 0001 | `include/uapi/linux/kvm.h` | Add `KVM_EXIT_X86_RDTSC = 40`, `KVM_CAP_RDTSC_EXITING = 236`, `kvm_run.rdtsc.tsc_value` |
| 0002 | `arch/x86/include/asm/kvm_host.h` | Add `kvm_arch.rdtsc_exiting` flag (shared between VMX and SVM paths) |
| 0003 | `arch/x86/kvm/x86.c` | Handle `KVM_CAP_RDTSC_EXITING` in check_extension + enable_cap |
| 0004 | `arch/x86/kvm/vmx/vmx.c` | Intel VMX: exec control + handle_rdtsc_exit + dispatch table entries |
| 0005 | `arch/x86/kvm/svm/svm.c` | AMD SVM: VMCB intercept bits + svm_handle_rdtsc_exit + dispatch table entries |

## Protocol (userspace ↔ kernel)

```
1. VM creation:  enable_cap(KVM_CAP_RDTSC_EXITING)
2. Guest rdtsc → VM exit → kvm_run.exit_reason = 40
3. Userspace: compute virtual_tsc, write to kvm_run.rdtsc.tsc_value
4. vcpu.run() → kernel reads tsc_value → injects EAX:EDX → skips rdtsc
```

## Firecracker DST changes needed after applying

In `src/vmm/src/dst/rdtsc.rs`:
- `KVM_EXIT_RDTSC = 37`  →  `KVM_EXIT_RDTSC = 40`
- `KVM_CAP_RDTSC_EXITING = 214` → `KVM_CAP_RDTSC_EXITING = 236`
- Implement `set_rdtsc_response()` using `kvm_run.rdtsc.tsc_value` at offset +32

In `src/vmm/src/vstate/vcpu.rs`:
- `exit_reason == 38 || exit_reason == 51` → `exit_reason == 40`
  (KVM_EXIT_LOONGARCH_IOCSR=38 conflicts; KVM_EXIT_X86_RDTSC=40 is the new number)

## Building (module-only, no full kernel rebuild)

Apply all patches from the `series` file in order.  On Intel hosts omit
patch 0005; on AMD hosts omit patch 0004.  Applying both is harmless since
each patch only modifies its respective `vmx.c` or `svm.c`.

### Intel (VMX) host

```bash
cd /usr/src/linux-source-6.8.0
PATCHES=/path/to/openthesis/deploy/host-kernel-patches

sudo patch -p1 < $PATCHES/0001-kvm-uapi-add-KVM_EXIT_X86_RDTSC-and-KVM_CAP_RDTSC_E.patch
sudo patch -p1 < $PATCHES/0002-kvm-x86-add-rdtsc_exiting-flag-to-kvm_arch.patch
sudo patch -p1 < $PATCHES/0003-kvm-x86-wire-KVM_CAP_RDTSC_EXITING-capability.patch
sudo patch -p1 < $PATCHES/0004-kvm-vmx-add-RDTSC-RDTSCP-userspace-exit-handler.patch

sudo cp /boot/config-$(uname -r) .config
sudo make olddefconfig
sudo make prepare scripts
sudo make -j$(nproc) M=arch/x86/kvm modules

sudo rmmod kvm_intel kvm
sudo insmod arch/x86/kvm/kvm.ko
sudo insmod arch/x86/kvm/vmx/kvm_intel.ko
```

### AMD (SVM) host

```bash
cd /usr/src/linux-source-6.8.0
PATCHES=/path/to/openthesis/deploy/host-kernel-patches

sudo patch -p1 < $PATCHES/0001-kvm-uapi-add-KVM_EXIT_X86_RDTSC-and-KVM_CAP_RDTSC_E.patch
sudo patch -p1 < $PATCHES/0002-kvm-x86-add-rdtsc_exiting-flag-to-kvm_arch.patch
sudo patch -p1 < $PATCHES/0003-kvm-x86-wire-KVM_CAP_RDTSC_EXITING-capability.patch
sudo patch -p1 < $PATCHES/0005-kvm-svm-add-RDTSC-RDTSCP-userspace-exit-handler.patch

sudo cp /boot/config-$(uname -r) .config
sudo make olddefconfig
sudo make prepare scripts
sudo make -j$(nproc) M=arch/x86/kvm modules

sudo rmmod kvm_amd kvm
sudo insmod arch/x86/kvm/kvm.ko
sudo insmod arch/x86/kvm/svm/kvm_amd.ko
```

## KVM exit reason numbering (Linux 6.8)

| Value | Name |
|-------|------|
| 37 | KVM_EXIT_NOTIFY |
| 38 | KVM_EXIT_LOONGARCH_IOCSR  ← conflicts with old Firecracker DST |
| 39 | KVM_EXIT_MEMORY_FAULT |
| **40** | **KVM_EXIT_X86_RDTSC** (new) |

## KVM cap numbering (Linux 6.8)

| Value | Name |
|-------|------|
| 234 | KVM_CAP_GUEST_MEMFD |
| 235 | KVM_CAP_VM_TYPES |
| **236** | **KVM_CAP_RDTSC_EXITING** (new) |
