/*
 * libvmcall.so - OpenThesis per-edge coverage via VMCALL hypercall.
 *
 * Implements the LLVM SanitizerCoverage trace-pc-guard interface using a
 * VMCALL (hypercall) instruction instead of writing to a shared bitmap. The
 * VMM intercepts KVM_EXIT_VMCALL and updates its own coverage bitmap directly,
 * giving per-edge visibility without the vsock round-trip at burst boundaries.
 *
 * This is the P4 alternative to libcover.c (bitmap-over-vsock). Use it only
 * when the Firecracker VMCALL exit handler (deploy/firecracker-patches/0009-)
 * is applied. Without that patch, VMCALL faults the guest with #UD.
 *
 * Build:
 *   gcc -O2 -fPIC -shared -o libvmcall.so libvmcall.c
 *   x86_64-linux-musl-gcc -O2 -fPIC -shared -o libvmcall.so libvmcall.c
 *
 * Usage:
 *   LD_PRELOAD=/opt/openthesis/lib/libvmcall.so ./your-binary
 *   # or set in openthesis-init node config via ld_preload field
 *
 * Protocol: VMCALL with rax=0xD57C0000 ("DST COVER"), rbx=edge_slot.
 * The VMM reads rax as the command discriminator and rbx as the bitmap slot.
 * It increments bitmap[rbx % BITMAP_SIZE] atomically and returns via vmresume.
 *
 * Requires: Firecracker patch adding KVM_EXIT_VMCALL handler in vcpu.rs.
 * Status: aspirational - activate when patch 0009 is written and applied.
 */

#include <stdint.h>
#include <pthread.h>

#define BITMAP_SIZE (1 << 16)

/* VMCALL command: "DST COVER" discriminator for the VMM handler. */
#define DST_VMCALL_COVER 0xD57C0000UL

/* FNV-1a-64 hash to spread guard addresses across the bitmap. */
static inline uint64_t fnv1a64(uint64_t v) {
    uint64_t h = 14695981039346656037ULL;
    h ^= (v & 0xff); h *= 1099511628211ULL;
    h ^= ((v >>  8) & 0xff); h *= 1099511628211ULL;
    h ^= ((v >> 16) & 0xff); h *= 1099511628211ULL;
    h ^= ((v >> 24) & 0xff); h *= 1099511628211ULL;
    h ^= ((v >> 32) & 0xff); h *= 1099511628211ULL;
    h ^= ((v >> 40) & 0xff); h *= 1099511628211ULL;
    h ^= ((v >> 48) & 0xff); h *= 1099511628211ULL;
    h ^= ((v >> 56) & 0xff); h *= 1099511628211ULL;
    return h;
}

/* Emit a VMCALL with the given slot. The VMM increments bitmap[slot]. */
static inline void vmcall_cover(uint64_t slot) {
    __asm__ volatile (
        "vmcall"
        :
        : "a"(DST_VMCALL_COVER), "b"(slot)
        : "memory"
    );
}

void __sanitizer_cov_trace_pc_guard_init(uint32_t *start, uint32_t *stop) {
    (void)start; (void)stop;
}

void __sanitizer_cov_trace_pc_guard(uint32_t *guard) {
    if (!*guard) return;
    uint64_t slot = fnv1a64((uint64_t)(uintptr_t)guard) % BITMAP_SIZE;
    vmcall_cover(slot);
}
