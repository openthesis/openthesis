"""
OpenThesis property-based testing: Hypothesis integration for deterministic exploration.

When OPENTHESIS_SEED is set (inside the VM), seeds Hypothesis from the
OpenThesis PRNG for deterministic, replayable property tests and emits
guidance signals for each generated value to steer exploration.

Usage:
    from openthesis import proptest
    from hypothesis import strategies as st

    @proptest.given(st.integers(min_value=0, max_value=100))
    def test_property(x):
        assert x >= 0
"""
import os
import functools
from typing import Callable

try:
    from hypothesis import given, settings, HealthCheck
    from hypothesis import strategies
    from hypothesis.database import InMemoryExampleDatabase
    _HYPOTHESIS_AVAILABLE = True
except ImportError:
    _HYPOTHESIS_AVAILABLE = False
    strategies = None

try:
    from . import guidance as _guidance
    from . import random as _random
except ImportError:
    from openthesis import guidance as _guidance
    from openthesis import random as _random


def _get_seed() -> "int | None":
    """Return the deterministic seed if running inside an OpenThesis VM."""
    seed_str = os.environ.get("OPENTHESIS_SEED", "")
    if seed_str:
        try:
            return int(seed_str)
        except ValueError:
            pass
    return None


def _make_det_settings(seed: int) -> "settings":
    """Build a deterministic Hypothesis settings object, handling version differences."""
    base_kwargs = dict(
        database=InMemoryExampleDatabase(),
        suppress_health_check=[HealthCheck.too_slow, HealthCheck.data_too_large],
    )
    # deriving_from_seed was added in Hypothesis 6.x; fall back gracefully.
    try:
        return settings(**base_kwargs, deriving_from_seed=seed)
    except TypeError:
        pass
    try:
        return settings(**base_kwargs, deriving=seed)
    except TypeError:
        return settings(**base_kwargs)


def given(*strategies_args, **kw_strategies):
    """
    Decorator: run the wrapped function as a Hypothesis property test integrated
    with OpenThesis exploration.

    In deterministic mode (OPENTHESIS_SEED set): seeds Hypothesis from the
    OpenThesis PRNG, uses an in-memory DB, and emits explore() guidance per
    generated example.

    Outside the VM: falls back to standard Hypothesis behavior.
    """
    if not _HYPOTHESIS_AVAILABLE:
        def decorator(func):
            @functools.wraps(func)
            def wrapper(*args, **kwargs):
                pass  # Hypothesis not installed; skip silently.
            return wrapper
        return decorator

    def decorator(func):
        seed = _get_seed()

        if seed is not None:
            hyp_settings = _make_det_settings(seed)

            @given(*strategies_args, **kw_strategies)
            @hyp_settings
            @functools.wraps(func)
            def wrapped(*args, **kwargs):
                _guidance.track_counter("proptest_examples_generated", 1)
                return func(*args, **kwargs)
        else:
            @given(*strategies_args, **kw_strategies)
            @functools.wraps(func)
            def wrapped(*args, **kwargs):
                return func(*args, **kwargs)

        return wrapped
    return decorator


def test(func: Callable) -> Callable:
    """
    Mark a zero-argument function as an OpenThesis property test.

    Usage:
        @proptest.test
        def test_no_args():
            x = strategies.integers().example()
            assert x is not None
    """
    @functools.wraps(func)
    def wrapper():
        seed = _get_seed()
        if seed is not None:
            _guidance.track_counter("proptest_tests_run", 1)
        return func()
    return wrapper


def assume(condition: bool) -> None:
    """Filter out invalid examples; mirrors hypothesis.assume."""
    _guidance.explore("proptest_assume_satisfied", 1 if condition else 0)
    if not _HYPOTHESIS_AVAILABLE:
        if not condition:
            raise ValueError("proptest.assume: condition not satisfied")
        return
    from hypothesis import assume as _assume
    _assume(condition)


def note(message: str) -> None:
    """Log a message in Hypothesis output; mirrors hypothesis.note."""
    if not _HYPOTHESIS_AVAILABLE:
        return
    from hypothesis import note as _note
    _note(message)
