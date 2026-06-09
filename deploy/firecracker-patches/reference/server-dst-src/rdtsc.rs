// KVM_EXIT_X86_RDTSC: exit reason after host kernel patch (Linux 6.8).
// Exit 38 = KVM_EXIT_LOONGARCH_IOCSR (conflict with old numbering).
// Exit 40 = KVM_EXIT_X86_RDTSC (newly added by openthesis host-kernel-patches).
pub const KVM_EXIT_RDTSC: u32 = 40;

// KVM_CAP_RDTSC_EXITING = 236 (next after KVM_CAP_VM_TYPES=235 in Linux 6.8).
pub const KVM_CAP_RDTSC_EXITING: u32 = 236;

/// Write the virtual TSC value into kvm_run so the kernel injects EAX:EDX.
///
/// kvm_run union layout: the `rdtsc` field (struct { tsc_value: u64 }) is the
/// first member of the union, sitting at offset +32 from the start of kvm_run.
pub fn set_rdtsc_response(vcpu_fd: &mut kvm_ioctls::VcpuFd, value: u64) {
    let kvm_run = vcpu_fd.get_kvm_run();
    unsafe {
        let ptr = kvm_run as *const _ as *mut u8;
        (ptr.add(32) as *mut u64).write_unaligned(value);
    }
}
