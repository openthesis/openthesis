"""
Lifecycle event signaling matching the OpenThesis SDK protocol.

Wire formats:
  {"openthesis_setup_complete": {"status": "complete", "details": {...}}}
  {"openthesis_send_event":     {"event_name": "...", "details": {...}}}
  {"openthesis_teardown":       {"status": "complete", "details": {...}}}
  {"openthesis_prefork":        {"status": "ready",    "details": {...}}}
  {"openthesis_burst_done":     {"status": "complete", "details": {...}}}
"""

import json
import os
import threading

_lock = threading.Lock()
_writer = None
_writer_init = False


def _get_writer():
    global _writer, _writer_init
    if _writer_init:
        return _writer
    _writer_init = True

    output_dir = os.environ.get("OPENTHESIS_OUTPUT_DIR", "")
    if output_dir:
        path = os.path.join(output_dir, "sdk.jsonl")
        try:
            _writer = open(path, "a", buffering=1)
            return _writer
        except OSError:
            pass

    return None


def _write_jsonl(env: dict) -> None:
    with _lock:
        w = _get_writer()
        if w is None:
            return
        line = json.dumps(env, separators=(",", ":")) + "\n"
        w.write(line)


def setup_complete(details: dict = None) -> None:
    """Signal that the system under test is ready for testing."""
    _write_jsonl({"openthesis_setup_complete": {"status": "complete", "details": details or {}}})


def send_event(event_name: str, details: dict = None) -> None:
    """Send a custom lifecycle event."""
    _write_jsonl({"openthesis_send_event": {"event_name": event_name, "details": details or {}}})


def teardown(details: dict = None) -> None:
    """Signal graceful shutdown of the system under test."""
    _write_jsonl({"openthesis_teardown": {"status": "complete", "details": details or {}}})


def prefork(details: dict = None) -> None:
    """Signal that the SUT supports forkserver mode."""
    _write_jsonl({"openthesis_prefork": {"status": "ready", "details": details or {}}})


def burst_done(details: dict = None) -> None:
    """Signal that a burst of work is complete."""
    _write_jsonl({"openthesis_burst_done": {"status": "complete", "details": details or {}}})


def stop_faults(duration_seconds: float) -> None:
    """Request a quiet period during which the orchestrator suppresses fault injection."""
    _write_jsonl({"openthesis_stop_faults": {"duration_seconds": duration_seconds, "status": "requested"}})
