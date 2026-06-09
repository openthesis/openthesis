"""
Assertions matching the OpenThesis SDK protocol.

Each function emits JSONL to OPENTHESIS_OUTPUT_DIR/sdk.jsonl (or
OPENTHESIS_SDK_LOCAL_OUTPUT if set). The JSON format is identical to the Go SDK so the
guest agent's parseAssertionOutput parser accepts it without changes.

Wire format (two lines emitted per callsite, first hit only):
  {"openthesis_assert": {"hit": false, "condition": false, "message": "...",
                         "assert_type": "always", "must_hit": true,
                         "id": "<sha256[:4]>",
                         "location": {"file": "...", "line": N}}}
  {"openthesis_assert": {"hit": true, "condition": <bool>, "message": "...",
                         "assert_type": "always", "must_hit": true,
                         "id": "<sha256[:4]>", "details": {...},
                         "location": {"file": "...", "line": N}}}
"""

import hashlib
import inspect
import json
import os
import sys
import threading

_lock = threading.Lock()
_writer = None
_writer_init = False
_declared: set = set()  # assertType + ":" + message keys already catalog-declared
_ever_since_armed: set = set()  # "ever_since:"+message keys that have been armed


def _get_writer():
    global _writer, _writer_init
    if _writer_init:
        return _writer
    _writer_init = True

    output_dir = os.environ.get("OPENTHESIS_OUTPUT_DIR", "")
    if output_dir:
        path = os.path.join(output_dir, "sdk.jsonl")
        try:
            _writer = open(path, "a", buffering=1)  # line-buffered
            return _writer
        except OSError:
            pass

    local_output = os.environ.get("OPENTHESIS_SDK_LOCAL_OUTPUT", "")
    if local_output:
        try:
            _writer = open(local_output, "a", buffering=1)
            return _writer
        except OSError:
            pass

    return None


def _callsite_id(file: str, line: int) -> str:
    """SHA-256 of "file:line", first 4 bytes as hex - matches Go callsiteID."""
    raw = f"{file}:{line}".encode()
    digest = hashlib.sha256(raw).digest()
    return digest[:4].hex()


def _write_envelope(w, env: dict) -> None:
    line = json.dumps(env, separators=(",", ":")) + "\n"
    w.write(line)


def _emit(condition: bool, message: str, assert_type: str, must_hit: bool,
          details: dict, caller_skip: int) -> None:
    """Core emission logic - mirrors Go assert.emit()."""
    frame = sys._getframe(caller_skip)
    file = frame.f_code.co_filename
    line = frame.f_lineno

    assertion_id = _callsite_id(file, line)

    with _lock:
        w = _get_writer()
        if w is None:
            return

        # Emit hit:false catalog declaration on first encounter.
        decl_key = f"{assert_type}:{message}"
        if decl_key not in _declared:
            _declared.add(decl_key)
            decl = {
                "openthesis_assert": {
                    "hit": False,
                    "condition": False,
                    "message": message,
                    "assert_type": assert_type,
                    "must_hit": must_hit,
                    "id": assertion_id,
                    "location": {"file": file, "line": line},
                }
            }
            _write_envelope(w, decl)

        # Emit the actual evaluation.
        body: dict = {
            "hit": True,
            "condition": condition,
            "message": message,
            "assert_type": assert_type,
            "must_hit": must_hit,
            "id": assertion_id,
            "location": {"file": file, "line": line},
        }
        if details:
            body["details"] = details

        _write_envelope(w, {"openthesis_assert": body})


def always(condition: bool, message: str, details: dict = None) -> None:
    """Assert condition is true every time this is called.

    A single False evaluation fails the property.
    """
    _emit(condition, message, "always", True, details, 2)


def sometimes(condition: bool, message: str, details: dict = None) -> None:
    """Assert condition is true at least once across the entire run."""
    _emit(condition, message, "sometimes", True, details, 2)


def reachable(message: str, details: dict = None) -> None:
    """Assert this code path is reached at least once."""
    _emit(True, message, "reachable", True, details, 2)


def always_or_unreachable(condition: bool, message: str, details: dict = None) -> None:
    """Assert condition is true every time reached, but also passes if never reached."""
    _emit(condition, message, "always_or_unreachable", False, details, 2)


def unreachable(message: str, details: dict = None) -> None:
    """Assert this code path is never reached."""
    _emit(False, message, "unreachable", False, details, 2)


def always_greater_than(left: int, right: int, message: str, details: dict = None) -> None:
    d = dict(details) if details else {}
    d["left_value"] = left
    d["right_value"] = right
    _emit(left > right, message, "always", True, d, 2)


def sometimes_greater_than(left: int, right: int, message: str, details: dict = None) -> None:
    d = dict(details) if details else {}
    d["left_value"] = left
    d["right_value"] = right
    _emit(left > right, message, "sometimes", True, d, 2)


def always_equal(left: int, right: int, message: str, details: dict = None) -> None:
    d = dict(details) if details else {}
    d["left_value"] = left
    d["right_value"] = right
    _emit(left == right, message, "always", True, d, 2)


def sometimes_equal(left: int, right: int, message: str, details: dict = None) -> None:
    d = dict(details) if details else {}
    d["left_value"] = left
    d["right_value"] = right
    _emit(left == right, message, "sometimes", True, d, 2)


def ever_since(condition: bool, message: str, details: dict = None) -> None:
    """Assert that once condition becomes true, it stays true forever.

    The first call where condition is True arms the assertion; subsequent
    calls where condition is False are violations.
    """
    key = "ever_since:" + message
    with _lock:
        if condition:
            _ever_since_armed.add(key)
        armed = key in _ever_since_armed
        effective_condition = condition or not armed
    _emit(effective_condition, message, "ever_since", True, details, 2)


def sometimes_all(message: str, named_bools: dict, details: dict = None) -> None:
    """Assert that all named conditions are simultaneously true at least once.

    named_bools: dict mapping condition names to bool values.
    The engine explores from states achieving the most sub-goals.
    """
    satisfied = sum(1 for v in named_bools.values() if v)
    total = len(named_bools)
    all_true = satisfied == total and total > 0

    d = dict(details) if details else {}
    d["sub_goals"] = named_bools
    d["satisfied_count"] = satisfied
    d["total_count"] = total

    _emit(all_true, message, "sometimes_all", True, d, 2)


def sometimes_each(label: str, key: str, details: dict = None) -> None:
    """Assert that each distinct key is observed at least once under label.

    Call this each time an event with a particular key occurs; the engine
    checks that every expected key was reached at least once.
    """
    _emit(True, f"{label}:{key}", "sometimes", True, details, 2)
