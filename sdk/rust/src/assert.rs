use serde_json::{json, Value};
use std::collections::HashMap;
use std::fs::{File, OpenOptions};
use std::io::Write;
use std::sync::{Mutex, OnceLock};

static WRITER: OnceLock<Mutex<Option<File>>> = OnceLock::new();
static DECLARED: OnceLock<Mutex<HashMap<String, bool>>> = OnceLock::new();

// ever_since armed state: key → still_holding
static EVER_SINCE: OnceLock<Mutex<HashMap<String, bool>>> = OnceLock::new();

fn writer() -> &'static Mutex<Option<File>> {
    WRITER.get_or_init(|| {
        let file = open_output_file();
        Mutex::new(file)
    })
}

fn declared() -> &'static Mutex<HashMap<String, bool>> {
    DECLARED.get_or_init(|| Mutex::new(HashMap::new()))
}

fn ever_since_state() -> &'static Mutex<HashMap<String, bool>> {
    EVER_SINCE.get_or_init(|| Mutex::new(HashMap::new()))
}

fn open_output_file() -> Option<File> {
    let dir = std::env::var("OPENTHESIS_OUTPUT_DIR")
        .ok()
        .filter(|s| !s.is_empty())
        .or_else(|| {
            std::env::var("OPENTHESIS_SDK_LOCAL_OUTPUT")
                .ok()
                .filter(|s| !s.is_empty())
        })?;

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

fn callsite_id(file: &str, line: u32) -> String {
    use std::collections::hash_map::DefaultHasher;
    use std::hash::{Hash, Hasher};
    let mut h = DefaultHasher::new();
    format!("{}:{}", file, line).hash(&mut h);
    format!("{:08x}", h.finish() & 0xFFFFFFFF)
}

fn write_json(val: &Value) {
    let mut guard = writer().lock().unwrap_or_else(|e| e.into_inner());
    if let Some(ref mut f) = *guard {
        let mut bytes = serde_json::to_vec(val).unwrap_or_default();
        bytes.push(b'\n');
        let _ = f.write_all(&bytes);
    }
}

pub fn emit(
    condition: bool,
    message: &str,
    assert_type: &str,
    must_hit: bool,
    details: Option<Value>,
    file: &str,
    line: u32,
) {
    let id = callsite_id(file, line);
    let decl_key = format!("{}:{}", assert_type, message);

    // Emit declaration on first encounter.
    {
        let mut decl = declared().lock().unwrap_or_else(|e| e.into_inner());
        if !decl.contains_key(&decl_key) {
            decl.insert(decl_key.clone(), true);
            let decl_val = json!({
                "openthesis_assert": {
                    "hit": false,
                    "condition": false,
                    "message": message,
                    "assert_type": assert_type,
                    "must_hit": must_hit,
                    "id": id,
                    "location": { "file": file, "line": line }
                }
            });
            write_json(&decl_val);
        }
    }

    let mut body = json!({
        "openthesis_assert": {
            "hit": true,
            "condition": condition,
            "message": message,
            "assert_type": assert_type,
            "must_hit": must_hit,
            "id": id,
            "location": { "file": file, "line": line }
        }
    });

    if let Some(d) = details {
        body["openthesis_assert"]["details"] = d;
    }

    write_json(&body);
}

// ever_since: armed on first call; once condition becomes false, stays false.
pub fn emit_ever_since(
    condition: bool,
    message: &str,
    details: Option<Value>,
    file: &str,
    line: u32,
) {
    let key = format!("{}:{}", file, line);
    let mut state = ever_since_state().lock().unwrap_or_else(|e| e.into_inner());
    let holding = state.entry(key).or_insert(true);
    if *holding && !condition {
        *holding = false;
    }
    let effective = *holding;
    drop(state);

    emit(effective, message, "always", true, details, file, line);
}

/// Assert that `condition` is true every time this is called.
/// A single false evaluation is a violation.
#[macro_export]
macro_rules! always {
    ($condition:expr, $message:expr) => {
        $crate::assert::emit($condition, $message, "always", true, None, file!(), line!())
    };
    ($condition:expr, $message:expr, $details:expr) => {
        $crate::assert::emit(
            $condition,
            $message,
            "always",
            true,
            Some($details),
            file!(),
            line!(),
        )
    };
}

/// Assert that `condition` is true whenever this code is reached,
/// but also passes if never reached.
#[macro_export]
macro_rules! always_or_unreachable {
    ($condition:expr, $message:expr) => {
        $crate::assert::emit(
            $condition,
            $message,
            "always_or_unreachable",
            false,
            None,
            file!(),
            line!(),
        )
    };
    ($condition:expr, $message:expr, $details:expr) => {
        $crate::assert::emit(
            $condition,
            $message,
            "always_or_unreachable",
            false,
            Some($details),
            file!(),
            line!(),
        )
    };
}

/// Assert that `condition` is true at least once across the entire run.
#[macro_export]
macro_rules! sometimes {
    ($condition:expr, $message:expr) => {
        $crate::assert::emit($condition, $message, "sometimes", true, None, file!(), line!())
    };
    ($condition:expr, $message:expr, $details:expr) => {
        $crate::assert::emit(
            $condition,
            $message,
            "sometimes",
            true,
            Some($details),
            file!(),
            line!(),
        )
    };
}

/// Assert that this code path is reached at least once.
#[macro_export]
macro_rules! reachable {
    ($message:expr) => {
        $crate::assert::emit(true, $message, "reachable", true, None, file!(), line!())
    };
    ($message:expr, $details:expr) => {
        $crate::assert::emit(
            true,
            $message,
            "reachable",
            true,
            Some($details),
            file!(),
            line!(),
        )
    };
}

/// Assert that this code path is never reached.
#[macro_export]
macro_rules! unreachable_assert {
    ($message:expr) => {
        $crate::assert::emit(false, $message, "unreachable", false, None, file!(), line!())
    };
    ($message:expr, $details:expr) => {
        $crate::assert::emit(
            false,
            $message,
            "unreachable",
            false,
            Some($details),
            file!(),
            line!(),
        )
    };
}

/// Assert that once `condition` becomes true it stays true for the rest of the run.
/// Tracks armed state per call site.
#[macro_export]
macro_rules! ever_since {
    ($condition:expr, $message:expr) => {
        $crate::assert::emit_ever_since($condition, $message, None, file!(), line!())
    };
    ($condition:expr, $message:expr, $details:expr) => {
        $crate::assert::emit_ever_since($condition, $message, Some($details), file!(), line!())
    };
}

/// Assert that `left > right` every time called.
/// The violation details include both operand values.
#[macro_export]
macro_rules! always_greater_than {
    ($left:expr, $right:expr, $message:expr) => {{
        let l = $left;
        let r = $right;
        $crate::assert::emit(
            l > r,
            $message,
            "always",
            true,
            Some(serde_json::json!({"left": l, "right": r, "op": ">"})),
            file!(),
            line!(),
        )
    }};
}

/// Assert that `left > right` at least once across the run.
#[macro_export]
macro_rules! sometimes_greater_than {
    ($left:expr, $right:expr, $message:expr) => {{
        let l = $left;
        let r = $right;
        $crate::assert::emit(
            l > r,
            $message,
            "sometimes",
            true,
            Some(serde_json::json!({"left": l, "right": r, "op": ">"})),
            file!(),
            line!(),
        )
    }};
}

/// Assert that `left == right` every time called.
#[macro_export]
macro_rules! always_equal {
    ($left:expr, $right:expr, $message:expr) => {{
        let l = $left;
        let r = $right;
        $crate::assert::emit(
            l == r,
            $message,
            "always",
            true,
            Some(serde_json::json!({"left": l, "right": r, "op": "=="})),
            file!(),
            line!(),
        )
    }};
}

/// Assert that `left == right` at least once across the run.
#[macro_export]
macro_rules! sometimes_equal {
    ($left:expr, $right:expr, $message:expr) => {{
        let l = $left;
        let r = $right;
        $crate::assert::emit(
            l == r,
            $message,
            "sometimes",
            true,
            Some(serde_json::json!({"left": l, "right": r, "op": "=="})),
            file!(),
            line!(),
        )
    }};
}

/// Assert that each distinct key under label is observed at least once.
/// Call with the same label and a different key each time.
#[macro_export]
macro_rules! sometimes_each {
    ($label:expr, $key:expr) => {
        $crate::assert::emit(
            true,
            &format!("{}:{}", $label, $key),
            "sometimes",
            true,
            None,
            file!(),
            line!(),
        )
    };
    ($label:expr, $key:expr, $details:expr) => {
        $crate::assert::emit(
            true,
            &format!("{}:{}", $label, $key),
            "sometimes",
            true,
            Some($details),
            file!(),
            line!(),
        )
    };
}

/// Assert that all named conditions are simultaneously true at least once.
/// Pass a HashMap<&str, bool> as the named conditions.
#[macro_export]
macro_rules! sometimes_all {
    ($message:expr, $named_bools:expr) => {{
        let satisfied: usize = $named_bools.values().filter(|&&v| v).count();
        let total: usize = $named_bools.len();
        let details = serde_json::json!({
            "satisfied_count": satisfied,
            "total_count": total,
            "sub_goals": $named_bools,
        });
        $crate::assert::emit(
            satisfied == total,
            $message,
            "sometimes_all",
            true,
            Some(details),
            file!(),
            line!(),
        )
    }};
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn test_emit_no_output_dir() {
        // Should not panic when OPENTHESIS_OUTPUT_DIR is not set.
        emit(true, "test message", "always", true, None, "test.rs", 1);
    }

    #[test]
    fn test_emit_with_details() {
        emit(
            false,
            "violation message",
            "always",
            true,
            Some(json!({"key": "value"})),
            "test.rs",
            2,
        );
    }

    #[test]
    fn test_ever_since_basic() {
        emit_ever_since(true, "ever_since test", None, "test.rs", 100);
        emit_ever_since(false, "ever_since test", None, "test.rs", 100);
        // After going false, subsequent calls should remain false.
        emit_ever_since(true, "ever_since test", None, "test.rs", 100);
    }

    #[test]
    fn test_callsite_id_stable() {
        let id1 = callsite_id("foo.rs", 42);
        let id2 = callsite_id("foo.rs", 42);
        assert_eq!(id1, id2);
        let id3 = callsite_id("foo.rs", 43);
        assert_ne!(id1, id3);
    }
}
