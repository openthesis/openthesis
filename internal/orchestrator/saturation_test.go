package orchestrator

import (
	"testing"

	"pgregory.net/rapid"
)

// Property: Rate() equals the arithmetic mean of what was recorded.
func TestSaturationDetectorRateIsArithmeticMean(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := rapid.IntRange(2, 64).Draw(t, "size")
		threshold := rapid.Float64Range(0.1, 10.0).Draw(t, "threshold")
		d := NewSaturationDetector(size, threshold)

		values := rapid.SliceOfN(rapid.IntRange(0, 20), 1, size*2).Draw(t, "values")
		for _, v := range values {
			d.Record(v)
		}

		n := len(values)
		if n > size {
			// Only last `size` values are in the window.
			values = values[len(values)-size:]
			n = size
		}
		sum := 0
		for _, v := range values {
			sum += v
		}
		want := float64(sum) / float64(n)
		got := d.Rate()
		if abs(got-want) > 1e-9 {
			t.Fatalf("Rate() = %f, want %f (values=%v)", got, want, values)
		}
	})
}

// Property: Saturated() is false until minBursts = size/2 records are in.
func TestSaturationDetectorRequiresMinBursts(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := rapid.IntRange(4, 64).Draw(t, "size")
		d := NewSaturationDetector(size, 100.0) // threshold far above zero

		// Record fewer than minBursts entries with zero edges (below threshold).
		minBursts := size / 2
		for i := 0; i < minBursts-1; i++ {
			d.Record(0)
		}
		if d.Saturated() {
			t.Fatalf("declared saturation with only %d records, minBursts=%d", minBursts-1, minBursts)
		}
	})
}

// Property: after filling with values > threshold, Saturated() is false.
func TestSaturationDetectorNotSaturatedAboveThreshold(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := rapid.IntRange(2, 32).Draw(t, "size")
		threshold := rapid.Float64Range(0.5, 5.0).Draw(t, "threshold")
		d := NewSaturationDetector(size, threshold)

		// Fill with values well above threshold.
		for i := 0; i < size*2; i++ {
			d.Record(int(threshold) + 10)
		}
		if d.Saturated() {
			t.Fatalf("Saturated()=true but all values were above threshold")
		}
	})
}

// Property: Reset returns Rate() to 1.0 (empty state sentinel).
func TestSaturationDetectorResetClearsState(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		size := rapid.IntRange(2, 32).Draw(t, "size")
		d := NewSaturationDetector(size, 0.5)

		for i := 0; i < size; i++ {
			d.Record(5)
		}
		d.Reset()

		if d.Rate() != 1.0 {
			t.Fatalf("Rate() after Reset = %f, want 1.0", d.Rate())
		}
		if d.Saturated() {
			t.Fatalf("Saturated()=true after Reset")
		}
	})
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
