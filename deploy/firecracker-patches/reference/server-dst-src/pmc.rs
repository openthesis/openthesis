use std::io;

#[derive(Debug)]
pub struct InstructionCounter { fd: i32 }

const PERF_TYPE_HARDWARE: u32 = 0;
const PERF_COUNT_HW_INSTRUCTIONS: u64 = 1;

#[repr(C)]
struct PerfEventAttr {
    type_: u32, size: u32, config: u64,
    sample_period_or_freq: u64, sample_type: u64, read_format: u64,
    flags: u64, wakeup_events_or_watermark: u32, bp_type: u32,
    config1_or_bp_addr: u64, config2_or_bp_len: u64, branch_sample_type: u64,
    sample_regs_user: u64, sample_stack_user: u32, clock_id: i32,
    sample_regs_intr: u64, aux_watermark: u32, sample_max_stack: u16,
    reserved_2: u16, aux_sample_size: u32, reserved_3: u32,
    sig_data: u64, config3: u64,
}

impl InstructionCounter {
    pub fn new() -> Result<Self, io::Error> {
        let mut attr: PerfEventAttr = unsafe { std::mem::zeroed() };
        attr.type_ = PERF_TYPE_HARDWARE;
        attr.size = std::mem::size_of::<PerfEventAttr>() as u32;
        attr.config = PERF_COUNT_HW_INSTRUCTIONS;
        attr.flags = 1; // disabled
        attr.flags |= 1 << 19; // exclude_host (bit 19): count ONLY guest instructions
        let fd = unsafe {
            libc::syscall(libc::SYS_perf_event_open, &attr as *const _, 0i32, -1i32, -1i32, 0u64)
        } as i32;
        if fd < 0 { return Err(io::Error::last_os_error()); }
        Ok(Self { fd })
    }

    pub fn is_available() -> bool { Self::new().is_ok() }

    pub fn read(&self) -> u64 {
        let mut val: u64 = 0;
        unsafe { libc::read(self.fd, &mut val as *mut _ as *mut libc::c_void, 8); }
        val
    }

    pub fn enable(&self) {
        unsafe { libc::ioctl(self.fd, 0x2400 /* PERF_EVENT_IOC_ENABLE */, 0); }
    }

    pub fn disable(&self) {
        unsafe { libc::ioctl(self.fd, 0x2401 /* PERF_EVENT_IOC_DISABLE */, 0); }
    }

    pub fn reset(&self) {
        unsafe { libc::ioctl(self.fd, 0x2403 /* PERF_EVENT_IOC_RESET */, 0); }
    }
}

impl Drop for InstructionCounter {
    fn drop(&mut self) { unsafe { libc::close(self.fd); } }
}
