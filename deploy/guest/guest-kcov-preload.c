/*
 * guest-kcov-preload.c - LD_PRELOAD library for KCOV coverage in SUT processes.
 *
 * Enables Linux KCOV (kernel coverage tracing) in any dynamically-linked process.
 * Writes an AFL-style edge bitmap to /run/kcov_bitmap (shared with openthesis-init).
 *
 * Usage: LD_PRELOAD=/opt/openthesis/lib/libkcov_preload.so ./my-sut
 *
 * Build:
 *   gcc -shared -fPIC -O2 -o libkcov_preload.so guest-kcov-preload.c
 *
 * Flush model (deterministic):
 *   The library does NOT use a background timer thread. Flushing happens on:
 *   1. SIGUSR2 from openthesis-init at burst boundaries (deterministic, driven by
 *      instruction count via the DST ctrl socket)
 *   2. Process exit via __attribute__((destructor))
 *
 *   This eliminates the wall-clock non-determinism of the old 200ms polling thread.
 *   The Rust init sends SIGUSR2 to all SUT PIDs just before calling flush_and_send().
 *
 * Limitation: Only works with dynamically-linked binaries. Static Go (CGO_ENABLED=0)
 * ignores LD_PRELOAD. Build with CGO_ENABLED=1 to enable dynamic linking.
 */

#define _GNU_SOURCE
#include <fcntl.h>
#include <signal.h>
#include <stdint.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#define KCOV_INIT_TRACE _IOR('c', 1, uint64_t)
#define KCOV_ENABLE     _IO('c', 100)
#define KCOV_DISABLE    _IO('c', 101)
#define KCOV_TRACE_PC   0

#define KCOV_ENTRIES  65536
#define BITMAP_SIZE   (1 << 16)
#define BITMAP_PATH   "/run/kcov_bitmap"

static int        kcov_fd    = -1;
static uint64_t  *kcov_buf   = NULL;
static uint8_t   *bitmap     = NULL;
static int        bitmap_fd  = -1;

static void flush_to_bitmap(void) {
    if (!kcov_buf || !bitmap) return;

    uint64_t count = __atomic_exchange_n(kcov_buf, 0, __ATOMIC_SEQ_CST);
    if (count == 0) return;
    if (count > KCOV_ENTRIES) count = KCOV_ENTRIES;

    // PCs are at kcov_buf[1..count].  Process adjacent pairs as AFL-style edges:
    // slot = ((prev >> 1) ^ cur) % BITMAP_SIZE.
    // Start prev from kcov_buf[1] (not kcov_buf[0] which was just zeroed) to
    // avoid generating a spurious edge from the zeroed count word.
    if (count < 2) return;
    uint64_t prev = kcov_buf[1];
    for (uint64_t i = 2; i <= count; i++) {
        uint64_t cur  = kcov_buf[i];
        uint64_t slot = ((prev >> 1) ^ cur) % BITMAP_SIZE;
        uint8_t  old  = bitmap[slot];
        bitmap[slot]  = (old == 255) ? 255 : (old + 1);
        prev = cur;
    }
}

static void handle_sigusr2(int sig) {
    (void)sig;
    flush_to_bitmap();
}

__attribute__((constructor))
static void kcov_init(void) {
    kcov_fd = open("/sys/kernel/debug/kcov", O_RDWR);
    if (kcov_fd < 0) return;

    if (ioctl(kcov_fd, KCOV_INIT_TRACE, (uint64_t)KCOV_ENTRIES) != 0) {
        close(kcov_fd); kcov_fd = -1; return;
    }

    size_t buf_len = (KCOV_ENTRIES + 1) * sizeof(uint64_t);
    void *p = mmap(NULL, buf_len, PROT_READ | PROT_WRITE, MAP_SHARED, kcov_fd, 0);
    if (p == MAP_FAILED) {
        close(kcov_fd); kcov_fd = -1; return;
    }
    kcov_buf = (uint64_t *)p;

    if (ioctl(kcov_fd, KCOV_ENABLE, KCOV_TRACE_PC) != 0) {
        munmap(kcov_buf, buf_len);
        close(kcov_fd); kcov_fd = -1; kcov_buf = NULL; return;
    }

    bitmap_fd = open(BITMAP_PATH, O_RDWR | O_CREAT, 0666);
    if (bitmap_fd >= 0) {
        (void)ftruncate(bitmap_fd, BITMAP_SIZE);
        void *bm = mmap(NULL, BITMAP_SIZE, PROT_READ | PROT_WRITE,
                        MAP_SHARED, bitmap_fd, 0);
        if (bm != MAP_FAILED) bitmap = (uint8_t *)bm;
        else { close(bitmap_fd); bitmap_fd = -1; }
    }

    // Register SIGUSR2 as the flush trigger (sent by openthesis-init at burst boundaries).
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = handle_sigusr2;
    sa.sa_flags   = SA_RESTART;
    sigaction(SIGUSR2, &sa, NULL);
}

__attribute__((destructor))
static void kcov_fini(void) {
    flush_to_bitmap();
    if (kcov_fd >= 0) {
        ioctl(kcov_fd, KCOV_DISABLE, 0);
        munmap(kcov_buf, (KCOV_ENTRIES + 1) * sizeof(uint64_t));
        close(kcov_fd);
    }
    if (bitmap)    munmap(bitmap, BITMAP_SIZE);
    if (bitmap_fd >= 0) close(bitmap_fd);
}
