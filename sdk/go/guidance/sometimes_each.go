package guidance

// SometimesEach tracks whether each distinct value of a key has been observed
// at least once under label. On first observation of a new (label, key) pair,
// it records coverage novelty via the IJON Explore mechanism, steering the
// explorer toward states that produce new key values.
//
// The (label, key) pair is hashed to a stable int64 using FNV-1a 64-bit, then
// forwarded to Explore. This is exactly what IJON does for per-value coverage:
// each unique combination becomes its own "edge" in the state-space bitmap.
//
// Usage:
//
//	guidance.SometimesEach("leader_election_result", leaderID)
//	guidance.SometimesEach("error_code", fmt.Sprintf("%d", err.Code))
func SometimesEach(label, key string) {
	// FNV-1a 64-bit of "label\x00key".
	var h uint64 = 14695981039346656037
	for i := 0; i < len(label); i++ {
		h ^= uint64(label[i])
		h *= 1099511628211
	}
	h ^= uint64('\x00')
	h *= 1099511628211
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	Explore(label, int64(h))
}
