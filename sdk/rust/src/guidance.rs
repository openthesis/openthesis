//! Exploration feedback primitives inspired by IJON (Aschermann et al., IEEE S&P 2020).
//!
//! These signals tell the exploration engine which states are "interesting,"
//! guiding it toward deeper, more diverse state-space coverage beyond what
//! edge coverage alone can achieve.
//!
//! Signals are written to `${OPENTHESIS_OUTPUT_DIR}/sdk.jsonl` as JSONL.

use serde_json::json;
use std::collections::HashMap;
use std::fs::{File, OpenOptions};
use std::io::Write;
use std::sync::{Mutex, OnceLock};

static WRITER: OnceLock<Mutex<Option<File>>> = OnceLock::new();
static COUNTERS: OnceLock<Mutex<HashMap<String, i64>>> = OnceLock::new();

fn writer() -> &'static Mutex<Option<File>> {
    WRITER.get_or_init(|| {
        let file = open_output_file();
        Mutex::new(file)
    })
}

fn counters() -> &'static Mutex<HashMap<String, i64>> {
    COUNTERS.get_or_init(|| Mutex::new(HashMap::new()))
}

fn open_output_file() -> Option<File> {
    let dir = std::env::var("OPENTHESIS_OUTPUT_DIR")
        .ok()
        .filter(|s| !s.is_empty())?;

    let path = if dir.ends_with('/') {
        format!("{}sdk.jsonl", dir)
    } else {
        format!("{}/sdk.jsonl", dir)
    };

    OpenOptions::new()
        .append(true)
        .create(true)
        .open(&path)
        .ok()
}

fn emit(guidance_type: &str, name: &str, value: i64) {
    let val = json!({
        "openthesis_guidance": {
            "guidance_type": guidance_type,
            "name": name,
            "value": value
        }
    });

    let mut guard = writer().lock().unwrap_or_else(|e| e.into_inner());
    if let Some(ref mut f) = *guard {
        let mut bytes = serde_json::to_vec(&val).unwrap_or_default();
        bytes.push(b'\n');
        let _ = f.write_all(&bytes);
    }
}

/// Report a value to maximize. The exploration engine prioritizes states where
/// this value reaches new highs. Equivalent to IJON_MAX.
pub fn maximize_int(name: &str, value: i64) {
    emit("maximize", name, value);
}

/// Report a state-space coordinate. Each unique value for a given name is
/// treated as new coverage. Equivalent to IJON_SET.
pub fn explore(name: &str, value: i64) {
    emit("explore", name, value);
}

/// Report a two-dimensional state-space coordinate by packing a and b into
/// a single i64 (lower 32 bits = a, upper 32 bits = b).
pub fn explore_pair(name: &str, a: i64, b: i64) {
    let combined = (a & 0xFFFF_FFFF) | ((b & 0xFFFF_FFFF) << 32);
    emit("explore", name, combined);
}

/// Report a named qualitative state. The string is hashed to a stable i64
/// bucket using FNV-1a 64-bit. Each unique value of `state` for a given
/// `name` is treated as new coverage.
pub fn track_state(name: &str, state: &str) {
    let h = fnv1a(state);
    emit("explore", name, h as i64);
}

/// Accumulate a named counter and report the running total to the exploration
/// engine. The engine prioritizes states where the total reaches new highs.
/// `delta` may be positive or negative.
pub fn track_counter(name: &str, delta: i64) {
    let total = {
        let mut c = counters().lock().unwrap_or_else(|e| e.into_inner());
        let entry = c.entry(name.to_owned()).or_insert(0);
        *entry += delta;
        *entry
    };
    emit("maximize", name, total);
}

/// Track whether each distinct value of `key` has been observed at least once
/// under `label`. Hashes the (label, key) pair with FNV-1a and forwards to
/// `explore`, steering the explorer toward states that produce new key values.
pub fn sometimes_each(label: &str, key: &str) {
    let mut h: u64 = 14_695_981_039_346_656_037;
    for b in label.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(1_099_511_628_211);
    }
    h ^= b'\0' as u64;
    h = h.wrapping_mul(1_099_511_628_211);
    for b in key.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(1_099_511_628_211);
    }
    emit("explore", label, h as i64);
}

fn fnv1a(s: &str) -> u64 {
    let mut h: u64 = 14_695_981_039_346_656_037;
    for b in s.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(1_099_511_628_211);
    }
    h
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_fnv1a_stable() {
        let h1 = fnv1a("hello");
        let h2 = fnv1a("hello");
        assert_eq!(h1, h2);
        assert_ne!(fnv1a("hello"), fnv1a("world"));
    }

    #[test]
    fn test_explore_pair_packing() {
        // Verify bit packing: a in low 32, b in high 32.
        let combined = (1i64 & 0xFFFF_FFFF) | ((2i64 & 0xFFFF_FFFF) << 32);
        assert_eq!(combined, 0x0000_0002_0000_0001i64);
    }

    #[test]
    fn test_no_panic_without_output_dir() {
        maximize_int("test_metric", 42);
        explore("test_state", 7);
        track_state("role", "leader");
        track_counter("ops", 1);
        track_counter("ops", 2);
    }
}
