"""
Exploration guidance primitives matching the OpenThesis SDK protocol.

Signals are written to OPENTHESIS_OUTPUT_DIR/sdk.jsonl as JSONL.

Wire format:
  {"openthesis_guidance": {"guidance_type": "maximize", "name": "...", "value": N}}
  {"openthesis_guidance": {"guidance_type": "explore",  "name": "...", "value": N}}
"""

import os
import json
import threading
from typing import Union

_lock = threading.Lock()
_writer = None
_writer_init = False

# Running totals for track_counter().
_counter_lock = threading.Lock()
_counters: dict = {}


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


def _emit(guidance_type: str, name: str, value: int) -> None:
    with _lock:
        w = _get_writer()
        if w is None:
            return
        env = {
            "openthesis_guidance": {
                "guidance_type": guidance_type,
                "name": name,
                "value": value,
            }
        }
        line = json.dumps(env, separators=(",", ":")) + "\n"
        w.write(line)


def maximize_int(label: str, value: int) -> None:
    """Report a value to maximize.

    The exploration engine prioritizes states where this value reaches new highs.
    Equivalent to IJON_MAX / Go's guidance.MaximizeInt.
    """
    _emit("maximize", label, int(value))


def explore(label: str, data: Union[int, bytes]) -> None:
    """Report a state-space coordinate.

    Each unique value for a given label is treated as new coverage, driving
    exploration toward diverse states. Equivalent to IJON_SET / Go's guidance.Explore.

    data may be an int or bytes; bytes are FNV-1a hashed to a stable int64.
    """
    if isinstance(data, (bytes, bytearray)):
        value = _fnv1a(data)
    else:
        value = int(data)
    _emit("explore", label, value)


def explore_pair(label: str, a: int, b: int) -> None:
    """Report a two-dimensional state-space coordinate.

    Matches Go's guidance.ExplorePair: combines two 32-bit values into one int64.
    """
    combined = (int(a) & 0xFFFFFFFF) | ((int(b) & 0xFFFFFFFF) << 32)
    # Sign-extend to int64 range.
    if combined >= (1 << 63):
        combined -= (1 << 64)
    _emit("explore", label, combined)


def track_state(label: str, state: str) -> None:
    """Report a named qualitative state.

    The string is FNV-1a hashed to a stable int64 bucket.
    Matches Go's guidance.TrackState.
    """
    h = _fnv1a(state.encode())
    _emit("explore", label, h)


def track_counter(label: str, delta: int) -> None:
    """Accumulate a named counter and report the running total.

    delta may be positive or negative. Matches Go's guidance.TrackCounter.
    """
    with _counter_lock:
        _counters[label] = _counters.get(label, 0) + int(delta)
        total = _counters[label]
    _emit("maximize", label, total)


def _fnv1a(data: bytes) -> int:
    """FNV-1a 64-bit hash - matches Go's TrackState implementation."""
    h = 14695981039346656037  # FNV offset basis
    for byte in data:
        h ^= byte
        h = (h * 1099511628211) & 0xFFFFFFFFFFFFFFFF
    # Interpret as signed int64.
    if h >= (1 << 63):
        h -= (1 << 64)
    return h
