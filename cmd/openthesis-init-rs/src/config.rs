use serde::{Deserialize, Deserializer};
use std::collections::HashMap;
use std::fs;

fn null_as_default<'de, D, T>(de: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(de)?.unwrap_or_default())
}

pub const CONFIG_PATH: &str = "/opt/openthesis/config.json";
pub const OUTPUT_DIR: &str = "/opt/openthesis/output";
pub const SDK_OUTPUT_FILE: &str = "/opt/openthesis/output/sdk.jsonl";
pub const COMMAND_DIR: &str = "/opt/openthesis/control/commands";
pub const BIN_DIR: &str = "/opt/openthesis/bin";
pub const TEST_DIR: &str = "/opt/openthesis/test";

pub const VSOCK_GUEST_PORT: u32 = 1234;
pub const AF_VSOCK: libc::sa_family_t = 40;
pub const VMADDR_CID_ANY: u32 = !0u32;

pub const KCOV_PRELOAD_PATH: &str = "/opt/openthesis/lib/libkcov_preload.so";
pub const VOIDSTAR_PATH: &str = "/opt/openthesis/lib/libvoidstar.so";
pub const LIBFAULT_PATH: &str = "/opt/openthesis/lib/libfault.so";
pub const CHOICE_OVERRIDES_PATH: &str = "/run/openthesis_choice_overrides";

// Burst-boundary synchronization with driver processes.
// At each flush_coverage, init increments OT_BURST_SEQ_PATH then waits up to
// BURST_ACK_TIMEOUT_MS for OT_BURST_ACK_PATH to appear before proceeding with
// coverage flush. This lets driver goroutines run assertion checks at exact
// burst boundaries rather than on wall-clock timers, enabling step-exact replay.
pub const OT_BURST_SEQ_PATH: &str  = "/run/ot_burst_seq";
pub const OT_BURST_ACK_PATH: &str  = "/run/ot_burst_ack";
pub const BURST_ACK_TIMEOUT_MS: u64 = 200;

#[derive(Debug, Deserialize, Clone)]
pub struct Config {
    pub nodes: Vec<Node>,
    #[serde(default)]
    pub setup_timeout: String,
}

#[derive(Debug, Deserialize, Clone)]
pub struct Node {
    pub name: String,
    pub binary: String,
    #[serde(default, deserialize_with = "null_as_default")]
    pub args: Vec<String>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub env: HashMap<String, String>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub ready_probe: String,
    pub daemon: Option<bool>,
    #[serde(default, deserialize_with = "null_as_default")]
    pub cover_dir: String,
    #[serde(default)]
    pub storage_fault_fsync_rate: f64,
    #[serde(default)]
    pub storage_fault_write_rate: f64,
}

impl Node {
    pub fn is_daemon(&self) -> bool {
        self.daemon.unwrap_or(true)
    }
}

impl Config {
    pub fn load() -> Result<Config, String> {
        let data = fs::read(CONFIG_PATH)
            .map_err(|e| format!("read {}: {}", CONFIG_PATH, e))?;
        serde_json::from_slice(&data)
            .map_err(|e| format!("parse config: {}", e))
    }

    pub fn setup_timeout_secs(&self) -> u64 {
        if let Some(s) = self.setup_timeout.strip_suffix('s') {
            if let Ok(n) = s.parse::<u64>() { return n; }
        }
        if let Some(s) = self.setup_timeout.strip_suffix("m") {
            if let Ok(n) = s.parse::<u64>() { return n * 60; }
        }
        90 // default 90s
    }
}

pub fn read_seed() -> u64 {
    let data = match fs::read("/proc/cmdline") {
        Ok(d) => d,
        Err(_) => return 42,
    };
    let cmdline = String::from_utf8_lossy(&data);
    for field in cmdline.split_whitespace() {
        if let Some(rest) = field.strip_prefix("openthesis.seed=") {
            if let Ok(seed) = rest.parse::<u64>() {
                if seed != 0 { return seed; }
            }
        }
    }
    42
}

pub fn is_firecracker_mode() -> bool {
    let data = match fs::read("/proc/cmdline") {
        Ok(d) => d,
        Err(_) => return false,
    };
    let cmdline = String::from_utf8_lossy(&data);
    cmdline.split_whitespace().any(|f| f == "openthesis.firecracker=1")
}
