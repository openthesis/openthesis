/*
 * libcover.so - OpenThesis userspace coverage runtime for C/C++ binaries.
 *
 * Implements the LLVM SanitizerCoverage trace-pc-guard interface so that any
 * binary compiled with:
 *
 *   clang -fsanitize-coverage=trace-pc-guard -shared-libasan ...
 *   gcc   -fsanitize-coverage=trace-pc-guard ...
 *
 * and linked with -lcover automatically contributes basic-block hit counts
 * to the shared AFL-style edge bitmap at /run/kcov_bitmap.
 *
 * The bitmap file is created/opened by openthesis-init before any instrumented
 * process starts. Each 64-bit guard value is hashed (FNV-1a) to a bitmap slot;
 * the slot is incremented (saturating at 255) on each BB hit. The init process
 * reads the bitmap at burst boundaries and sends it to the host alongside KCOV
 * kernel-path data, giving the coverage-guided explorer visibility into every
 * basic block of every instrumented C/C++ binary.
 *
 * Build (Linux x86-64, static-pie for initramfs embedding):
 *
 *   gcc -O2 -fPIC -shared -o libcover.so libcover.c
 *   # or cross-compile from macOS:
 *   x86_64-linux-musl-gcc -O2 -fPIC -shared -o libcover.so libcover.c
 *
 * Install into the guest rootfs so binaries can find it at runtime:
 *
 *   sudo cp libcover.so /opt/openthesis/lib/libcover.so
 *   # The initramfs builder bundles /opt/openthesis/lib/ automatically.
 *
 * Usage for SUT authors (C/C++ binaries):
 *
 *   CFLAGS  += -fsanitize-coverage=trace-pc-guard
 *   LDFLAGS += -L/opt/openthesis/lib -lvoidstar -Wl,-rpath,/usr/lib
 *
 * The binary will then contribute BB coverage inside the OpenThesis VM without
 * any source changes beyond the compile flags.
 */

#define _GNU_SOURCE
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <pthread.h>

#define BITMAP_PATH "/run/kcov_bitmap"
#define BITMAP_SIZE (1 << 16)   /* 64 KB - matches gocover.go and kcov.go */

static uint8_t  *bitmap;
static int       bitmap_fd = -1;
static pthread_once_t init_once = PTHREAD_ONCE_INIT;

static void bitmap_init(void) {
    bitmap_fd = open(BITMAP_PATH, O_RDWR | O_CREAT, 0666);
    if (bitmap_fd < 0) {
        /* Bitmap file not available (not inside OpenThesis VM). Use anonymous
         * mapping so guard callbacks are still fast no-ops rather than crashing. */
        bitmap = (uint8_t *)mmap(NULL, BITMAP_SIZE,
                                 PROT_READ | PROT_WRITE,
                                 MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
        return;
    }
    /* Ensure the file is exactly BITMAP_SIZE bytes. */
    if (ftruncate(bitmap_fd, BITMAP_SIZE) != 0) {
        close(bitmap_fd);
        bitmap_fd = -1;
        bitmap = (uint8_t *)mmap(NULL, BITMAP_SIZE,
                                 PROT_READ | PROT_WRITE,
                                 MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
        return;
    }
    bitmap = (uint8_t *)mmap(NULL, BITMAP_SIZE,
                             PROT_READ | PROT_WRITE,
                             MAP_SHARED, bitmap_fd, 0);
    if (bitmap == MAP_FAILED) {
        close(bitmap_fd);
        bitmap_fd = -1;
        bitmap = (uint8_t *)mmap(NULL, BITMAP_SIZE,
                                 PROT_READ | PROT_WRITE,
                                 MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    }
}

/* AFL-style hash of a guard pointer to a bitmap slot.
 * Uses FNV-1a-64 to spread guard addresses across the 64KB bitmap. */
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

/*
 * __sanitizer_cov_trace_pc_guard_init is called once at program startup for
 * each translation unit. LLVM assigns monotonically increasing uint32_t
 * guard IDs to each basic block edge; start..stop is the range for this TU.
 * We don't need to do any per-TU initialization - the guard value itself is
 * used as the hash input in trace_pc_guard.
 */
void __sanitizer_cov_trace_pc_guard_init(uint32_t *start, uint32_t *stop) {
    pthread_once(&init_once, bitmap_init);
    (void)start; (void)stop;
}

/*
 * __sanitizer_cov_trace_pc_guard is called at every basic block edge.
 * *guard is the unique guard ID assigned by LLVM to this edge.
 * We hash it to a bitmap slot and increment (saturating at 255).
 */
void __sanitizer_cov_trace_pc_guard(uint32_t *guard) {
    if (!bitmap || !*guard) return;
    uint64_t slot = fnv1a64((uint64_t)(uintptr_t)guard) % BITMAP_SIZE;
    uint8_t v = bitmap[slot];
    if (v < 255) {
        bitmap[slot] = v + 1;
    }
}

