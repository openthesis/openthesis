use serde::{Deserialize, Serialize};
use serde_json::Value;

// Wire message types matching the Go agent's JSON protocol.

#[derive(Debug, Deserialize)]
pub struct IncomingMessage {
    #[serde(rename = "type")]
    pub kind: String,
    pub payload: Value,
}

#[derive(Debug, Deserialize)]
pub struct RunCommandPayload {
    pub name: String,
    pub path: String,
}

#[derive(Debug, Deserialize)]
pub struct FaultPayload {
    pub kind: String,
    #[serde(default)]
    pub port: String,
    #[serde(default)]
    pub delay_ms: i64,
    #[serde(default)]
    pub src_port: String,
    #[serde(default)]
    pub dst_port: String,
    #[serde(default)]
    pub direction: String,
    #[serde(default)]
    pub rate_kbps: i32,
    #[serde(default)]
    pub node: String,
    #[serde(default)]
    pub duration_ms: i64,
    #[serde(default)]
    pub duration_ns: i64,
    #[serde(default)]
    pub correlation: i32,
    #[serde(default)]
    pub cpu_pct: i32,
    #[serde(default)]
    pub bps: i64,
    #[serde(default)]
    pub target_free_bytes: i64,
    #[serde(default)]
    pub data_dir: String,
    #[serde(default)]
    pub script_path: String,
    #[serde(default)]
    pub script_args: Vec<String>,
    #[serde(default)]
    pub script_timeout_seconds: i32,
    #[serde(default)]
    pub disk_flakey_interval_secs: i32,
    #[serde(default)]
    pub disk_flakey_duration_secs: i32,
}

#[derive(Debug, Deserialize)]
pub struct ExecPayload {
    pub cmd: String,
    #[serde(default)]
    pub timeout_seconds: i32,
}

#[derive(Debug, Deserialize)]
pub struct ChoiceOverridesPayload {
    pub overrides: Vec<i32>,
}

#[derive(Debug, Serialize)]
struct OutgoingMessage<'a, T: Serialize> {
    #[serde(rename = "type")]
    kind: &'a str,
    payload: T,
}

// send_message serializes and writes a JSON-line message to fd.
// Retries on partial writes so the full message is always delivered.
pub fn send_message<T: Serialize>(fd: libc::c_int, kind: &str, payload: T) {
    if fd < 0 { return; }
    let msg = OutgoingMessage { kind, payload };
    let s = match serde_json::to_string(&msg) {
        Ok(s) => s,
        Err(e) => { crate::logf!("send_message: marshal {}: {}", kind, e); return; }
    };
    write_all(fd, s.as_bytes());
    write_all(fd, b"\n");
}

// write_all writes all bytes to fd, retrying on EINTR and partial writes.
pub fn write_all(fd: libc::c_int, buf: &[u8]) {
    if fd < 0 || buf.is_empty() { return; }
    let mut offset = 0usize;
    while offset < buf.len() {
        let n = unsafe {
            libc::write(
                fd,
                buf[offset..].as_ptr() as *const libc::c_void,
                buf.len() - offset,
            )
        };
        if n > 0 {
            offset += n as usize;
        } else if n == 0 {
            break; // closed
        } else {
            let e = unsafe { *libc::__errno_location() };
            if e == libc::EINTR { continue; }
            break; // real error
        }
    }
}

#[derive(Serialize)]
pub struct LifecyclePayload<'a> {
    pub event: &'a str,
    pub details: Value,
}

#[derive(Serialize)]
pub struct OutputPayload {
    pub container: &'static str,
    pub filename: String,
    pub data: String,
    pub exit_code: i32,
}

#[derive(Serialize)]
pub struct CoveragePayload {
    pub source: &'static str,
    pub data: String,
}

#[derive(Serialize)]
pub struct ExecResultPayload {
    pub stdout: String,
    pub stderr: String,
    pub exit_code: i32,
}
