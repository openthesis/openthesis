use std::os::fd::RawFd;
use crate::config::{AF_VSOCK, VMADDR_CID_ANY, VSOCK_GUEST_PORT};

// sockaddr_vm matches Linux's struct sockaddr_vm (linux/vm_sockets.h).
#[repr(C)]
pub struct SockaddrVM {
    pub svm_family: u16,
    pub svm_reserved1: u16,
    pub svm_port: u32,
    pub svm_cid: u32,
    pub svm_flags: u8,
    pub svm_zero: [u8; 3],
}

// create_server binds an AF_VSOCK socket and starts listening.
pub fn create_server() -> Result<RawFd, String> {
    let fd = unsafe {
        libc::socket(
            AF_VSOCK as libc::c_int,
            libc::SOCK_STREAM | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if fd < 0 {
        return Err(format!("vsock socket(): errno {}", errno()));
    }

    let sa = SockaddrVM {
        svm_family: AF_VSOCK,
        svm_reserved1: 0,
        svm_port: VSOCK_GUEST_PORT,
        svm_cid: VMADDR_CID_ANY,
        svm_flags: 0,
        svm_zero: [0; 3],
    };

    let ret = unsafe {
        libc::bind(
            fd,
            &sa as *const SockaddrVM as *const libc::sockaddr,
            std::mem::size_of::<SockaddrVM>() as u32,
        )
    };
    if ret < 0 {
        unsafe { libc::close(fd) };
        return Err(format!("vsock bind(): errno {}", errno()));
    }

    if unsafe { libc::listen(fd, 4) } < 0 {
        unsafe { libc::close(fd) };
        return Err(format!("vsock listen(): errno {}", errno()));
    }

    crate::logf!("vsock: listening on port {}", VSOCK_GUEST_PORT);
    Ok(fd)
}

// accept_connection blocks until a host connection arrives on server_fd.
// Retries automatically on EINTR.
pub fn accept_connection(server_fd: RawFd) -> Result<RawFd, String> {
    loop {
        let conn_fd = unsafe {
            libc::accept(server_fd, std::ptr::null_mut(), std::ptr::null_mut())
        };
        if conn_fd >= 0 {
            crate::logf!("vsock: host connected");
            return Ok(conn_fd);
        }
        let e = errno();
        if e == libc::EINTR {
            continue; // signal interrupted accept; retry
        }
        return Err(format!("vsock accept(): errno {}", e));
    }
}

fn errno() -> i32 {
    unsafe { *libc::__errno_location() }
}
