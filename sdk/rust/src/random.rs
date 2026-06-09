//! Deterministic random functions for use inside the OpenThesis VM.
//!
//! Seeded from `$OPENTHESIS_SEED` (u64, decimal). Falls back to
//! `std::time::SystemTime` when the variable is absent - this path is
//! only hit outside the deterministic VM and is intentionally
//! non-deterministic.

use std::sync::{Mutex, OnceLock};

static PRNG: OnceLock<Mutex<SplitMix64>> = OnceLock::new();

fn prng() -> &'static Mutex<SplitMix64> {
    PRNG.get_or_init(|| Mutex::new(SplitMix64::from_env()))
}

/// SplitMix64 PRNG. Same constants as pkg/prng in the Go codebase.
pub(crate) struct SplitMix64 {
    state: u64,
}

impl SplitMix64 {
    pub fn new(seed: u64) -> Self {
        Self { state: seed }
    }

    fn from_env() -> Self {
        if let Ok(s) = std::env::var("OPENTHESIS_SEED") {
            if let Ok(seed) = s.trim().parse::<u64>() {
                return Self::new(seed);
            }
        }
        // Fallback: seed from wall clock (non-deterministic, outside VM only).
        let seed = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos() as u64)
            .unwrap_or(0x9e37_79b9_7f4a_7c15);
        Self::new(seed)
    }

    pub fn next_u64(&mut self) -> u64 {
        self.state = self.state.wrapping_add(0x9e37_79b9_7f4a_7c15);
        let mut z = self.state;
        z = (z ^ (z >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
        z ^ (z >> 31)
    }
}

/// Return `n` bytes of deterministic entropy from the seeded PRNG.
pub fn get_random(n: usize) -> Vec<u8> {
    let mut rng = prng().lock().unwrap_or_else(|e| e.into_inner());
    let mut buf = vec![0u8; n];
    let mut i = 0;
    while i < n {
        let val = rng.next_u64();
        for j in 0..8 {
            if i >= n {
                break;
            }
            buf[i] = ((val >> (j * 8)) & 0xFF) as u8;
            i += 1;
        }
    }
    buf
}

/// Return a deterministic random u64.
pub fn uint64() -> u64 {
    prng().lock().unwrap_or_else(|e| e.into_inner()).next_u64()
}

/// Return a deterministic random element from `choices`.
///
/// Panics if `choices` is empty.
pub fn choose<T: Clone>(choices: &[T]) -> T {
    assert!(!choices.is_empty(), "random::choose called with empty slice");
    if choices.len() == 1 {
        return choices[0].clone();
    }
    let idx = (uint64() % choices.len() as u64) as usize;
    choices[idx].clone()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_splitmix64_deterministic() {
        let mut a = SplitMix64::new(42);
        let mut b = SplitMix64::new(42);
        for _ in 0..100 {
            assert_eq!(a.next_u64(), b.next_u64());
        }
    }

    #[test]
    fn test_splitmix64_different_seeds() {
        let mut a = SplitMix64::new(42);
        let mut b = SplitMix64::new(43);
        // Extremely unlikely to collide on first output.
        assert_ne!(a.next_u64(), b.next_u64());
    }

    #[test]
    fn test_get_random_length() {
        for n in [0, 1, 7, 8, 9, 100] {
            assert_eq!(get_random(n).len(), n);
        }
    }

    #[test]
    fn test_choose_single() {
        assert_eq!(choose(&[99u32]), 99);
    }

    #[test]
    fn test_choose_in_bounds() {
        let choices = vec![10u32, 20, 30, 40, 50];
        for _ in 0..1000 {
            let v = choose(&choices);
            assert!(choices.contains(&v));
        }
    }

    #[test]
    #[should_panic(expected = "empty slice")]
    fn test_choose_empty_panics() {
        choose::<u32>(&[]);
    }
}
