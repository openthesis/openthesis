"""
Deterministic random number generation for OpenThesis.

Inside the deterministic VM, the PRNG is seeded from OPENTHESIS_SEED.
Outside (no env var), falls back to a crypto-random seed - same fallback as Go SDK.
"""

import os
import random as _stdlib_random
import struct


class _SplitMix64:
    """SplitMix64 PRNG - same constants as pkg/prng and sdk/go/random."""

    __slots__ = ("_state",)

    def __init__(self, seed: int) -> None:
        self._state = int(seed) & 0xFFFFFFFFFFFFFFFF

    def uint64(self) -> int:
        self._state = (self._state + 0x9E3779B97F4A7C15) & 0xFFFFFFFFFFFFFFFF
        z = self._state
        z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & 0xFFFFFFFFFFFFFFFF
        z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & 0xFFFFFFFFFFFFFFFF
        return z ^ (z >> 31)


def _default_seed() -> int:
    """Return the seed from OPENTHESIS_SEED or a crypto-random fallback."""
    seed_str = os.environ.get("OPENTHESIS_SEED", "")
    if seed_str:
        try:
            return int(seed_str)
        except ValueError:
            pass
    # Fallback: seed from os.urandom (non-deterministic, only outside the VM).
    raw = os.urandom(8)
    return struct.unpack("<Q", raw)[0]


class _SplitMix64Random(_stdlib_random.Random):
    """stdlib.Random subclass backed by SplitMix64.

    Overrides random() to pull bits from SplitMix64 rather than Mersenne
    Twister, so all stdlib.Random methods (choice, shuffle, randint, …) are
    deterministic under the same seed.
    """

    def __init__(self, seed: int) -> None:
        super().__init__()
        self._sm64 = _SplitMix64(seed)

    def seed(self, a=None, version=2):  # type: ignore[override]
        # Re-seed the underlying SplitMix64 when explicitly asked.
        if a is not None:
            self._sm64 = _SplitMix64(int(a))

    def random(self) -> float:
        # Return a float in [0.0, 1.0) using 53 bits from SplitMix64.
        return (self._sm64.uint64() >> 11) * (1.0 / (1 << 53))

    def getrandbits(self, k: int) -> int:
        # Provide k bits assembled from SplitMix64 words.
        result = 0
        bits_needed = k
        while bits_needed > 0:
            word = self._sm64.uint64()
            take = min(bits_needed, 64)
            result = (result << take) | (word >> (64 - take))
            bits_needed -= take
        return result


class Random:
    """Deterministic random class - same API surface as Go sdk/go/random.

    Seed priority:
      1. Explicit ``seed`` argument to __init__.
      2. OPENTHESIS_SEED environment variable.
      3. os.urandom (non-deterministic fallback for use outside the VM).
    """

    def __init__(self, seed: int = None) -> None:
        actual_seed = seed if seed is not None else _default_seed()
        self._rng = _SplitMix64Random(actual_seed)

    def choice(self, seq):
        """Return a random element from seq (non-empty)."""
        if not seq:
            raise IndexError("choice from an empty sequence")
        return self._rng.choice(seq)

    def randint(self, a: int, b: int) -> int:
        """Return a random integer N such that a <= N <= b."""
        return self._rng.randint(a, b)

    def shuffle(self, lst: list) -> None:
        """Shuffle list in-place."""
        self._rng.shuffle(lst)

    def random(self) -> float:
        """Return a random float in [0.0, 1.0)."""
        return self._rng.random()

    def uint64(self) -> int:
        """Return a random uint64 - mirrors Go sdk/go/random.Uint64."""
        return self._rng._sm64.uint64()

    def get_random(self, n: int) -> bytes:
        """Return n bytes of deterministic entropy - mirrors Go sdk/go/random.GetRandom."""
        buf = bytearray(n)
        i = 0
        while i < n:
            val = self._rng._sm64.uint64()
            for j in range(8):
                if i >= n:
                    break
                buf[i] = (val >> (j * 8)) & 0xFF
                i += 1
        return bytes(buf)

    def sample(self, population, k: int):
        """Return a k-length list of unique elements from population."""
        return self._rng.sample(population, k)

    def uniform(self, a: float, b: float) -> float:
        """Return a random float N such that a <= N <= b."""
        return self._rng.uniform(a, b)


# Module-level convenience functions using a shared default instance.
_default: Random = None  # lazy init


def _get_default() -> Random:
    global _default
    if _default is None:
        _default = Random()
    return _default


def choice(seq):
    """Module-level choice using the shared default PRNG."""
    return _get_default().choice(seq)


def randint(a: int, b: int) -> int:
    """Module-level randint using the shared default PRNG."""
    return _get_default().randint(a, b)


def shuffle(lst: list) -> None:
    """Module-level shuffle using the shared default PRNG."""
    _get_default().shuffle(lst)


def uint64() -> int:
    """Module-level uint64 using the shared default PRNG."""
    return _get_default().uint64()


def get_random(n: int) -> bytes:
    """Module-level get_random using the shared default PRNG."""
    return _get_default().get_random(n)
