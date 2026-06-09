package orchestrator

import "sync"

// SaturationDetector tracks coverage velocity over a sliding window of bursts
// and declares saturation when marginal new-edge rate drops below a threshold.
// Used by StrategyEvolver to decide when to shift exploration strategy.
type SaturationDetector struct {
	mu        sync.Mutex
	window    []int // ring buffer of per-burst new-edge counts
	size      int
	pos       int
	filled    bool
	threshold float64 // avg edges/burst below which we declare saturation
	minBursts int     // require at least this many bursts before declaring saturation
}

// NewSaturationDetector returns a detector with the given sliding-window size
// and saturation threshold (avg new edges per burst).
// Typical: size=100, threshold=0.5 (fewer than 1 new edge every 2 bursts).
func NewSaturationDetector(size int, threshold float64) *SaturationDetector {
	if size <= 0 {
		size = 100
	}
	if threshold <= 0 {
		threshold = 0.5
	}
	return &SaturationDetector{
		window:    make([]int, size),
		size:      size,
		threshold: threshold,
		minBursts: size / 2,
	}
}

// Record adds the new-edge count from one burst to the window.
func (d *SaturationDetector) Record(newEdges int) {
	d.mu.Lock()
	d.window[d.pos%d.size] = newEdges
	d.pos++
	if d.pos >= d.size {
		d.filled = true
	}
	d.mu.Unlock()
}

// Saturated returns true when the rolling average of new edges per burst
// has dropped below the threshold for a full window.
func (d *SaturationDetector) Saturated() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pos < d.minBursts {
		return false
	}
	n := d.size
	if !d.filled {
		n = d.pos
	}
	total := 0
	for i := 0; i < n; i++ {
		total += d.window[i]
	}
	return float64(total)/float64(n) < d.threshold
}

// Rate returns the current rolling average of new edges per burst.
func (d *SaturationDetector) Rate() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pos == 0 {
		return 1.0
	}
	n := d.size
	if !d.filled {
		n = d.pos
	}
	total := 0
	for i := 0; i < n; i++ {
		total += d.window[i]
	}
	return float64(total) / float64(n)
}

// Reset clears all recorded observations, allowing a fresh saturation measurement.
// Called when the strategy evolves or escalation occurs.
func (d *SaturationDetector) Reset() {
	d.mu.Lock()
	d.pos = 0
	d.filled = false
	for i := range d.window {
		d.window[i] = 0
	}
	d.mu.Unlock()
}
