/*
 * libfault.so - Storage fault injection for OpenThesis.
 *
 * LD_PRELOAD library that intercepts fsync, fdatasync, and write() syscalls
 * and probabilistically returns EIO, simulating disk failures, WAL corruption,
 * and partial writes in storage-intensive SUTs (etcd, postgres, mysql, etc).
 *
 * Configuration (set via openthesis.json node env, or OPENTHESIS_FAULT_* vars):
 *   OPENTHESIS_FAULT_FSYNC_RATE   - probability [0,1] of fsync returning EIO
 *   OPENTHESIS_FAULT_WRITE_RATE   - probability [0,1] of write returning EIO
 *   OPENTHESIS_FAULT_TRUNCATE_RATE - probability [0,1] of truncation on write
 *
 * The fault RNG is seeded from OPENTHESIS_SEED so fault decisions are
 * deterministic across runs with the same seed (required for replay).
 *
 * Build (Linux x86-64):
 *   gcc -shared -fPIC -O2 -ldl -o libfault.so libfault.c
 *
 * Usage in openthesis.json:
 *   "env": {"LD_PRELOAD": "/opt/openthesis/lib/libfault.so",
 *           "OPENTHESIS_FAULT_FSYNC_RATE": "0.05",
 *           "OPENTHESIS_FAULT_WRITE_RATE": "0.01"}
 */

#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

/* SplitMix64 PRNG - same constants as pkg/prng for cross-language consistency. */
static uint64_t rng_state;

static uint64_t splitmix64(void) {
    rng_state += 0x9e3779b97f4a7c15ULL;
    uint64_t z = rng_state;
    z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ULL;
    z = (z ^ (z >> 27)) * 0x94d049bb133111ebULL;
    return z ^ (z >> 31);
}

/* Returns true with probability p (0.0–1.0). */
static int fault_fires(double p) {
    if (p <= 0.0) return 0;
    if (p >= 1.0) return 1;
    double r = (double)(splitmix64() >> 11) / (double)(1ULL << 53);
    return r < p;
}

static double fsync_rate  = 0.0;
static double write_rate  = 0.0;
static double trunc_rate  = 0.0;
static int    initialized = 0;

static void fault_init(void) {
    if (initialized) return;
    initialized = 1;

    /* Seed from OPENTHESIS_SEED for determinism; fall back to /dev/urandom. */
    const char *seed_str = getenv("OPENTHESIS_SEED");
    if (seed_str) {
        rng_state = (uint64_t)strtoull(seed_str, NULL, 10);
    } else {
        int fd = open("/dev/urandom", O_RDONLY);
        if (fd >= 0) {
            read(fd, &rng_state, sizeof(rng_state));
            close(fd);
        }
    }
    /* Mix in library address to make different processes diverge slightly. */
    rng_state ^= (uint64_t)(uintptr_t)&fault_init;

    const char *fs = getenv("OPENTHESIS_FAULT_FSYNC_RATE");
    const char *ws = getenv("OPENTHESIS_FAULT_WRITE_RATE");
    const char *ts = getenv("OPENTHESIS_FAULT_TRUNCATE_RATE");
    if (fs) fsync_rate = atof(fs);
    if (ws) write_rate = atof(ws);
    if (ts) trunc_rate = atof(ts);
}

/* Real function pointers resolved lazily. */
static int (*real_fsync)(int fd)       = NULL;
static int (*real_fdatasync)(int fd)   = NULL;
static ssize_t (*real_write)(int fd, const void *buf, size_t count) = NULL;

int fsync(int fd) {
    fault_init();
    if (!real_fsync) real_fsync = dlsym(RTLD_NEXT, "fsync");
    if (fault_fires(fsync_rate)) {
        errno = EIO;
        return -1;
    }
    return real_fsync(fd);
}

int fdatasync(int fd) {
    fault_init();
    if (!real_fdatasync) real_fdatasync = dlsym(RTLD_NEXT, "fdatasync");
    if (fault_fires(fsync_rate)) { /* same rate as fsync */
        errno = EIO;
        return -1;
    }
    return real_fdatasync(fd);
}

ssize_t write(int fd, const void *buf, size_t count) {
    fault_init();
    if (!real_write) real_write = dlsym(RTLD_NEXT, "write");
    if (fault_fires(write_rate)) {
        if (fault_fires(trunc_rate) && count > 1) {
            /* Partial write: write half the bytes then report error on "second call". */
            size_t partial = count / 2;
            ssize_t n = real_write(fd, buf, partial);
            if (n < 0) return n;
            errno = EIO;
            return n; /* report partial write without error (caller should check) */
        }
        errno = EIO;
        return -1;
    }
    return real_write(fd, buf, count);
}
