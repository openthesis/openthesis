//! Lifecycle event signaling matching the OpenThesis SDK wire format.

use serde_json::{json, Value};
use std::fs::{File, OpenOptions};
use std::io::Write;
use std::sync::{Mutex, OnceLock};

static WRITER: OnceLock<Mutex<Option<File>>> = OnceLock::new();

fn writer() -> &'static Mutex<Option<File>> {
    WRITER.get_or_init(|| {
        let file = open_output_file();
        Mutex::new(file)
    })
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

fn write_jsonl(val: &Value) {
    let mut guard = writer().lock().unwrap_or_else(|e| e.into_inner());
    if let Some(ref mut f) = *guard {
        let mut bytes = serde_json::to_vec(val).unwrap_or_default();
        bytes.push(b'\n');
        let _ = f.write_all(&bytes);
    }
}

/// Signal that the system under test is ready. The platform begins test
/// execution after receiving this signal.
///
/// Wire format: `{"openthesis_setup_complete": {"status": "complete", "details": {...}}}`
pub fn setup_complete(details: Value) {
    write_jsonl(&json!({
        "openthesis_setup_complete": {
            "status": "complete",
            "details": details
        }
    }));
}

/// Send a custom lifecycle event.
///
/// Wire format: `{"openthesis_send_event": {"event_name": "...", "details": {...}}}`
pub fn send_event(name: &str, details: Value) {
    write_jsonl(&json!({
        "openthesis_send_event": {
            "event_name": name,
            "details": details
        }
    }));
}

/// Signal graceful shutdown of the system under test.
///
/// Wire format: `{"openthesis_teardown": {"status": "complete", "details": {...}}}`
pub fn teardown(details: Value) {
    write_jsonl(&json!({
        "openthesis_teardown": {
            "status": "complete",
            "details": details
        }
    }));
}

/// Signal that fault injection should pause for `duration_seconds`.
///
/// Wire format: `{"openthesis_stop_faults": {"duration_seconds": 5.0}}`
pub fn stop_faults(duration_seconds: f64) {
    write_jsonl(&json!({
        "openthesis_stop_faults": {
            "duration_seconds": duration_seconds
        }
    }));
}

/// Emit the pre-fork ready signal. Called by the platform before forking
/// the deterministic snapshot.
///
/// Wire format: `{"openthesis_prefork": {"status": "ready", "details": {...}}}`
pub fn prefork(details: Value) {
    write_jsonl(&json!({
        "openthesis_prefork": {
            "status": "ready",
            "details": details
        }
    }));
}

/// Emit the burst-done signal. Called after a burst of execution completes.
///
/// Wire format: `{"openthesis_burst_done": {"status": "complete", "details": {...}}}`
pub fn burst_done(details: Value) {
    write_jsonl(&json!({
        "openthesis_burst_done": {
            "status": "complete",
            "details": details
        }
    }));
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn test_no_panic_without_output_dir() {
        setup_complete(json!({"version": "1.0"}));
        send_event("test_started", json!({"seed": 42}));
        teardown(json!({}));
        stop_faults(5.0);
    }
}
