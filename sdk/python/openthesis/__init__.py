"""
OpenThesis Python SDK - open-source deterministic testing SDK.

All assertion and guidance messages are emitted as JSONL to
``$OPENTHESIS_OUTPUT_DIR/sdk.jsonl``, matching the wire format consumed by the OpenThesis guest agent.

Quick start::

    from openthesis import assert_, guidance, lifecycle, random as ot_random

    # Assertions
    assert_.always(x > 0, "x is positive", {"x": x})
    assert_.sometimes(leader != "", "a leader was elected")
    assert_.reachable("write path exercised")

    # Guidance
    guidance.maximize_int("writes_committed", count)
    guidance.track_state("leader", leader_id)

    # Lifecycle
    lifecycle.setup_complete({"nodes": 3})

    # Deterministic random
    rng = ot_random.Random()
    key = rng.choice(["a", "b", "c"])
"""

from openthesis import assert_
from openthesis import guidance
from openthesis import lifecycle
from openthesis import proptest
from openthesis import random

# Convenience re-exports for the most commonly used functions.
from openthesis.assert_ import (
    always,
    always_equal,
    always_greater_than,
    always_or_unreachable,
    ever_since,
    reachable,
    sometimes,
    sometimes_all,
    sometimes_each,
    sometimes_equal,
    sometimes_greater_than,
    unreachable,
)
from openthesis.guidance import (
    explore,
    explore_pair,
    maximize_int,
    track_counter,
    track_state,
)
from openthesis.lifecycle import (
    burst_done,
    prefork,
    send_event,
    setup_complete,
    teardown,
)
from openthesis.random import Random

__version__ = "0.1.0"
__all__ = [
    # sub-modules
    "assert_",
    "guidance",
    "lifecycle",
    "proptest",
    "random",
    # assert
    "always",
    "always_equal",
    "always_greater_than",
    "always_or_unreachable",
    "ever_since",
    "reachable",
    "sometimes",
    "sometimes_all",
    "sometimes_each",
    "sometimes_equal",
    "sometimes_greater_than",
    "unreachable",
    # guidance
    "explore",
    "explore_pair",
    "maximize_int",
    "track_counter",
    "track_state",
    # lifecycle
    "burst_done",
    "prefork",
    "send_event",
    "setup_complete",
    "teardown",
    # random
    "Random",
]
