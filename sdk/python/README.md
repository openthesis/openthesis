# OpenThesis Python SDK

Python SDK for deterministic testing with OpenThesis.

## Installation

```bash
pip install .
```

Or for local development:

```bash
pip install -e .
```

## Requirements

- Python 3.8+
- No external dependencies.

## Usage

### Assertions

```python
from openthesis import assert_

# Assert condition is always true. A single False fails the property.
assert_.always(x > 0, "x is positive", {"x": x})

# Assert condition is true at least once across the run.
assert_.sometimes(leader != "", "a leader was elected")

# Assert this code path is always reached.
assert_.reachable("write path exercised")

# Assert this code path is never reached.
assert_.unreachable("invalid state reached")

# Assert condition is true every time reached, or never reached at all.
assert_.always_or_unreachable(val >= 0, "value is non-negative")

# Assert all named conditions are simultaneously true at least once.
assert_.sometimes_all("quorum with writes", {
    "has_leader": leader != "",
    "writes_committed": writes > 0,
})
```

### Guidance

```python
from openthesis import guidance

# Tell the explorer to maximize this value (hill-climbing).
guidance.maximize_int("writes_committed", commit_count)

# Treat each unique int value as new coverage.
guidance.explore("raft_term", term)

# Track a string-valued state dimension (hashed to int64).
guidance.track_state("leader_id", leader)

# Accumulate a counter and report the running total.
guidance.track_counter("bytes_written", len(payload))

# Two-dimensional coordinate.
guidance.explore_pair("node_state", node_id, state_enum)
```

### Lifecycle

```python
from openthesis import lifecycle

# Signal that the SUT is ready for testing.
lifecycle.setup_complete({"nodes": 3, "version": "1.0"})

# Send a custom event.
lifecycle.send_event("leader_elected", {"leader": node_id})

# Signal graceful shutdown.
lifecycle.teardown({"reason": "test complete"})
```

### Deterministic Random

```python
from openthesis import Random

# Seeded from OPENTHESIS_SEED env var, or explicit seed.
rng = Random(seed=42)

key = rng.choice(["alice", "bob", "carol"])
n   = rng.randint(1, 100)
rng.shuffle(items)
```

## Wire Format

All functions emit JSONL to `$OPENTHESIS_OUTPUT_DIR/sdk.jsonl` (or `$OPENTHESIS_SDK_LOCAL_OUTPUT`).

### Assertions

```json
{"openthesis_assert": {"hit": false, "condition": false, "message": "x is positive", "assert_type": "always", "must_hit": true, "id": "a1b2c3d4", "location": {"file": "driver.py", "line": 12}}}
{"openthesis_assert": {"hit": true, "condition": true, "message": "x is positive", "assert_type": "always", "must_hit": true, "id": "a1b2c3d4", "details": {"x": 5}, "location": {"file": "driver.py", "line": 12}}}
```

### Guidance

```json
{"openthesis_guidance": {"guidance_type": "maximize", "name": "writes_committed", "value": 42}}
{"openthesis_guidance": {"guidance_type": "explore",  "name": "raft_term", "value": 3}}
```

The format is identical to the Go SDK, so the OpenThesis guest agent's assertion and guidance parsers accept it without changes.
