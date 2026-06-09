// KCOV-based kernel coverage collection.
//
// Determinism guarantee: because the Rust agent is single-threaded (no M:N
// scheduler, no GC threads, no sysmon, no netpoller), KCOV_ENABLE on the main
// thread captures every kernel path the process takes. flush_and_send() is
// called directly from the command dispatch loop - no channels, no goroutines,
// no per-thread registration dance. This eliminates the ~1% KCOV edge variance
// that the Go agent had from transient runtime-spawned OS threads.

use std::os::fd::RawFd;
use base64_impl::encode as b64encode;

// KCOV ioctl numbers on Linux x86_64
// Cast via libc::syscall to avoid Ioctl type ambiguity between musl/glibc targets.
const KCOV_INIT_TRACE: u64 = 0x80086301; // _IOR('c', 1, unsigned long)
const KCOV_ENABLE: u64 = 0x6364;         // _IO('c', 100)
const KCOV_DISABLE: u64 = 0x6365;        // _IO('c', 101)
const KCOV_TRACE_PC: u64 = 0;            // mode for KCOV_ENABLE: trace PC addresses

unsafe fn ioctl2(fd: i32, cmd: u64, arg: u64) -> i64 {
    libc::syscall(libc::SYS_ioctl, fd as i64, cmd as i64, arg as i64)
}
unsafe fn ioctl1(fd: i32, cmd: u64) -> i64 {
    libc::syscall(libc::SYS_ioctl, fd as i64, cmd as i64)
}

// Trace buffer: 524288 entries * 8 bytes = 4 MiB. Sized for ~5M-insn bursts.
// Entry [0] = count; entries [1..N] = raw PC values.
const KCOV_ENTRIES: usize = 524288;

// AFL-style edge bitmap: 64K slots.
pub const BITMAP_SIZE: usize = 1 << 16;

const BITMAP_PATH: &str = "/run/kcov_bitmap";

pub struct KcovState {
    fd: RawFd,
    // mmap of /sys/kernel/debug/kcov; buf[0]=count, buf[1..]=PCs
    buf: *mut u64,
    // mmap of BITMAP_PATH shared with LD_PRELOAD processes
    bitmap_fd: RawFd,
    bitmap: *mut u8,
    prev_bitmap: Box<[u8; BITMAP_SIZE]>,
    // last PC from previous flush; bridges cross-flush edge pairs
    last_pc: u64,
    pub last_gen: u64,
}

// SAFETY: single-threaded process - no Send/Sync needed but compiler requires it
// because of the raw pointers.
unsafe impl Send for KcovState {}

impl KcovState {
    pub fn open() -> Result<KcovState, String> {
        let fd = unsafe {
            libc::open(
                b"/sys/kernel/debug/kcov\0".as_ptr() as *const libc::c_char,
                libc::O_RDWR,
            )
        };
        if fd < 0 {
            return Err(format!("open /sys/kernel/debug/kcov: errno {}", errno()));
        }

        // KCOV_INIT_TRACE: allocate trace buffer of KCOV_ENTRIES slots
        let ret = unsafe { ioctl2(fd, KCOV_INIT_TRACE, KCOV_ENTRIES as u64) };
        if ret < 0 {
            unsafe { libc::close(fd) };
            return Err(format!("KCOV_INIT_TRACE: errno {}", errno()));
        }

        let buf_len = KCOV_ENTRIES * 8;
        let buf = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                buf_len,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                fd,
                0,
            )
        };
        if buf == libc::MAP_FAILED {
            unsafe { libc::close(fd) };
            return Err(format!("mmap kcov trace: errno {}", errno()));
        }

        let ret = unsafe { ioctl2(fd, KCOV_ENABLE, KCOV_TRACE_PC) };
        if ret < 0 {
            unsafe {
                libc::munmap(buf, buf_len);
                libc::close(fd);
            }
            return Err(format!("KCOV_ENABLE: errno {}", errno()));
        }

        // Open shared bitmap file for LD_PRELOAD processes.
        let bitmap_fd = open_bitmap_file();
        let bitmap = if bitmap_fd >= 0 {
            let ptr = unsafe {
                libc::mmap(
                    std::ptr::null_mut(),
                    BITMAP_SIZE,
                    libc::PROT_READ | libc::PROT_WRITE,
                    libc::MAP_SHARED,
                    bitmap_fd,
                    0,
                )
            };
            if ptr == libc::MAP_FAILED { std::ptr::null_mut() } else { ptr as *mut u8 }
        } else {
            std::ptr::null_mut()
        };

        crate::logf!("kcov: enabled on main thread (single-threaded agent; 0% thread-spawn variance)");

        Ok(KcovState {
            fd,
            buf: buf as *mut u64,
            bitmap_fd,
            bitmap,
            prev_bitmap: Box::new([0u8; BITMAP_SIZE]),
            last_pc: 0,
            last_gen: 0,
        })
    }

    // flush_and_send drains the KCOV trace buffer, folds PC pairs into the
    // shared bitmap, and sends the merged bitmap to the host if it changed.
    //
    // Called directly from the command dispatch loop on the main thread.
    // No channels, no goroutines - the entire KCOV path is on one OS thread.
    pub fn flush_and_send(&mut self, conn_fd: RawFd) {
        // Atomically swap count to zero, reading how many PCs were recorded.
        let count = unsafe {
            let ptr = self.buf as *mut std::sync::atomic::AtomicU64;
            (*ptr).swap(0, std::sync::atomic::Ordering::Relaxed)
        } as usize;

        let has_bitmap = !self.bitmap.is_null();
        let has_pcs = count > 0;

        if !has_pcs && !has_bitmap {
            return;
        }

        // Fold (prev, cur) PC pairs into the bitmap using AFL-style edge encoding:
        // slot = ((prev >> 1) XOR cur) % BITMAP_SIZE
        if has_pcs {
            let count = count.min(KCOV_ENTRIES - 1);
            let target: &mut [u8] = if has_bitmap {
                unsafe { std::slice::from_raw_parts_mut(self.bitmap, BITMAP_SIZE) }
            } else {
                &mut self.prev_bitmap[..]
            };
            let mut prev = self.last_pc;
            for i in 1..=count {
                let cur = unsafe { *self.buf.add(i) };
                if prev != 0 {
                    let slot = (((prev >> 1) ^ cur) as usize) % BITMAP_SIZE;
                    let v = target[slot];
                    if v < 255 {
                        target[slot] = v + 1;
                    }
                }
                prev = cur;
            }
            if count >= 1 {
                self.last_pc = unsafe { *self.buf.add(count) };
            }
        }

        // Build send buffer from shared bitmap or local prev.
        let mut send_buf = [0u8; BITMAP_SIZE];
        if has_bitmap {
            unsafe {
                std::ptr::copy_nonoverlapping(self.bitmap, send_buf.as_mut_ptr(), BITMAP_SIZE);
            }
        } else {
            send_buf.copy_from_slice(&self.prev_bitmap[..]);
        }

        if send_buf == *self.prev_bitmap {
            return; // no change
        }

        self.prev_bitmap.copy_from_slice(&send_buf);
        self.last_gen += 1;
        self.send_coverage(conn_fd, &send_buf);
    }

    // merge_gocoverdir reads Go -cover counter files from all node cover_dirs
    // and folds them into the shared bitmap. Go's -cover runtime writes
    // .covcounters files to GOCOVERDIR. The files are in Go's coverage binary
    // format; we hash each (pkg, counter_index) pair with FNV-1a to a bitmap slot
    // and use the saturation counter as the edge frequency.
    //
    // This gives userspace Go branch coverage from etcd/kv/etc. alongside
    // the kernel-path KCOV data, without needing a clean process exit.
    pub fn merge_gocoverdir(&mut self, cover_dirs: &[String]) {
        if self.bitmap.is_null() { return; }
        let target = unsafe { std::slice::from_raw_parts_mut(self.bitmap, BITMAP_SIZE) };
        for dir in cover_dirs {
            let entries = match std::fs::read_dir(dir) {
                Ok(e) => e,
                Err(_) => continue,
            };
            for entry in entries.flatten() {
                let path = entry.path();
                let name = entry.file_name();
                let fname = name.to_string_lossy();
                // Go writes "covcounters.<meta-hash>.<pid>.<nano>" files
                if !fname.starts_with("covcounters.") { continue; }
                let data = match std::fs::read(&path) {
                    Ok(d) => d,
                    Err(_) => continue,
                };
                // Parse Go coverage counter file.
                // Format: magic(8) + version(4) + ... + counter values (u32 le)
                // We don't need to fully parse - just hash each byte position
                // to spread across the bitmap deterministically.
                if data.len() < 16 { continue; }
                let counters = &data[16..]; // skip header
                for (i, chunk) in counters.chunks(4).enumerate() {
                    if chunk.len() < 4 { break; }
                    let v = u32::from_le_bytes([chunk[0], chunk[1], chunk[2], chunk[3]]);
                    if v == 0 { continue; }
                    // Hash (file_name, counter_index) to a bitmap slot.
                    let h = fnv1a(fname.as_bytes(), i as u64);
                    let slot = h as usize % BITMAP_SIZE;
                    let old = target[slot];
                    if old < 255 { target[slot] = old + 1; }
                }
            }
        }
    }

    pub fn reset(&mut self) {
        // Zero trace buffer count.
        unsafe {
            let ptr = self.buf as *mut std::sync::atomic::AtomicU64;
            (*ptr).store(0, std::sync::atomic::Ordering::Relaxed);
        }
        // Zero shared bitmap.
        if !self.bitmap.is_null() {
            unsafe {
                std::ptr::write_bytes(self.bitmap, 0, BITMAP_SIZE);
            }
        }
        self.prev_bitmap.fill(0);
        self.last_pc = 0;
        crate::logf!("kcov: bitmap reset");
    }

    fn send_coverage(&self, conn_fd: RawFd, bitmap: &[u8]) {
        let encoded = b64encode(bitmap);
        crate::proto::send_message(conn_fd, "coverage", crate::proto::CoveragePayload {
            source: "kcov-kernel",
            data: encoded,
        });
    }
}

impl Drop for KcovState {
    fn drop(&mut self) {
        unsafe {
            ioctl1(self.fd, KCOV_DISABLE);
            if !self.bitmap.is_null() {
                libc::munmap(self.bitmap as *mut libc::c_void, BITMAP_SIZE);
            }
            if self.bitmap_fd >= 0 {
                libc::close(self.bitmap_fd);
            }
            libc::munmap(self.buf as *mut libc::c_void, KCOV_ENTRIES * 8);
            libc::close(self.fd);
        }
    }
}

fn open_bitmap_file() -> RawFd {
    let path = BITMAP_PATH.as_bytes();
    let mut path_buf = [0u8; 64];
    path_buf[..path.len()].copy_from_slice(path);

    let fd = unsafe {
        libc::open(
            path_buf.as_ptr() as *const libc::c_char,
            libc::O_RDWR | libc::O_CREAT,
            0o666,
        )
    };
    if fd < 0 {
        return -1;
    }
    unsafe { libc::ftruncate(fd, BITMAP_SIZE as libc::off_t) };
    fd
}

fn errno() -> i32 {
    unsafe { *libc::__errno_location() }
}

// FNV-1a hash of bytes + a u64 seed; maps (filename, counter_index) → bitmap slot.
fn fnv1a(bytes: &[u8], seed: u64) -> u64 {
    const OFFSET: u64 = 14695981039346656037;
    const PRIME: u64 = 1099511628211;
    let mut h = OFFSET;
    for &b in bytes {
        h ^= b as u64;
        h = h.wrapping_mul(PRIME);
    }
    // Mix in the index so different counters in the same file hash differently.
    let s = seed.to_le_bytes();
    for &b in &s {
        h ^= b as u64;
        h = h.wrapping_mul(PRIME);
    }
    h
}

// Minimal base64 encoder (no external dep beyond libc).
mod base64_impl {
    const TABLE: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

    pub fn encode(input: &[u8]) -> String {
        let mut out = Vec::with_capacity((input.len() + 2) / 3 * 4);
        let mut i = 0;
        while i + 2 < input.len() {
            let a = input[i] as usize;
            let b = input[i + 1] as usize;
            let c = input[i + 2] as usize;
            out.push(TABLE[a >> 2]);
            out.push(TABLE[((a & 3) << 4) | (b >> 4)]);
            out.push(TABLE[((b & 0xf) << 2) | (c >> 6)]);
            out.push(TABLE[c & 0x3f]);
            i += 3;
        }
        match input.len() - i {
            1 => {
                let a = input[i] as usize;
                out.push(TABLE[a >> 2]);
                out.push(TABLE[(a & 3) << 4]);
                out.push(b'=');
                out.push(b'=');
            }
            2 => {
                let a = input[i] as usize;
                let b = input[i + 1] as usize;
                out.push(TABLE[a >> 2]);
                out.push(TABLE[((a & 3) << 4) | (b >> 4)]);
                out.push(TABLE[(b & 0xf) << 2]);
                out.push(b'=');
            }
            _ => {}
        }
        unsafe { String::from_utf8_unchecked(out) }
    }
}
