// Package prng provides a deterministic SplitMix64 pseudo-random number generator.
// Unlike math/rand, the output sequence is stable across all Go versions and platforms,
// which is essential for reproducible simulation runs.
// https://prng.di.unimi.it/splitmix64.c
package prng

// Source is a deterministic pseudo-random number generator using SplitMix64.
// Same seed produces identical sequences across all platforms and Go versions.
type Source struct {
	state uint64
}

// New returns a Source seeded with the given value.
func New(seed uint64) *Source {
	return &Source{state: seed}
}

// Uint64 advances the generator by one SplitMix64 step and returns the result.
func (s *Source) Uint64() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Intn returns a uniformly distributed int in [0, n). It panics if n <= 0.
func (s *Source) Intn(n int) int {
	if n <= 0 {
		panic("prng: invalid argument to Intn")
	}
	// Rejection sampling to avoid modulo bias.
	bound := uint64(n)
	threshold := -bound % bound // equivalent to (2^64 - bound) % bound
	for {
		v := s.Uint64()
		if v >= threshold {
			return int(v % bound)
		}
	}
}

// Float64 returns a uniformly distributed float64 in [0.0, 1.0).
func (s *Source) Float64() float64 {
	// Use the upper 53 bits for a full-precision double mantissa.
	return float64(s.Uint64()>>11) / float64(1<<53)
}

// Bool returns a pseudo-random boolean.
func (s *Source) Bool() bool {
	return s.Uint64()&1 == 1
}

// Shuffle randomizes the order of n elements using the provided swap function.
// The algorithm is Fisher-Yates, iterating from i = n-1 down to 1.
func (s *Source) Shuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		j := s.Intn(i + 1)
		swap(i, j)
	}
}

// Fork derives a new independent Source by mixing the current state with a salt.
// Useful for giving each subsystem its own deterministic stream without correlation.
func (s *Source) Fork(salt uint64) *Source {
	return New(s.state ^ salt)
}

// State returns the raw internal state for checkpointing.
func (s *Source) State() uint64 {
	return s.state
}

// SetState restores the internal state, typically from a checkpoint.
func (s *Source) SetState(st uint64) {
	s.state = st
}
