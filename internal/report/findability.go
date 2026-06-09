package report

import (
	"math"
	"sort"
)

// RunsNeededFor94 computes how many clean (violation-free) runs are needed
// to reach 94% confidence that the violation is fixed, given the estimated
// per-run survival probability pSurvive.
//
// Formula: solve (1 - pSurvive)^n >= 0.94 for n:
//
//	n = ceil(log(0.06) / log(1 - pSurvive))
//
// Returns -1 if pSurvive >= 1.0 (bug appears every run, unfixable).
// Returns 1 if pSurvive <= 0.0 (bug never found, already fixed).
func RunsNeededFor94(pSurvive float64) int {
	if pSurvive <= 0 {
		return 1
	}
	if pSurvive >= 1.0 {
		return -1
	}
	n := math.Log(0.06) / math.Log(1.0-pSurvive)
	return int(math.Ceil(n))
}

// BugProbabilityPoint is a single data point in the bug probability timeline.
type BugProbabilityPoint struct {
	VTime          float64 `json:"vtime"`           // virtual time (in seconds)
	Probability    float64 `json:"probability"`     // Bayesian-smoothed probability (0-100)
	CILow          float64 `json:"ci_low"`          // 90% credible interval lower bound
	CIHigh         float64 `json:"ci_high"`         // 90% credible interval upper bound
	RawProbability float64 `json:"raw_probability"` // raw observed proportion (0-100)
	ChangePointP   float64 `json:"change_point_p"`  // probability of regime change (0-100)
}

// BugReport is a detailed analysis of a single bug's findability over time.
type BugReport struct {
	Property         string                `json:"property"`
	Message          string                `json:"message"`
	Timeline         []BugProbabilityPoint `json:"timeline"`
	Logs             []LogEntry            `json:"logs"`
	MeanTimeToBug    float64               `json:"mean_time_to_bug"` // expected vtime between occurrences
	PSurvival        float64               `json:"p_survival"`       // probability you'd see it again in same duration
	TotalOccurrences int                   `json:"total_occurrences"`
	TotalWindows     int                   `json:"total_windows"`
}

// LogEntry is a log line correlated to virtual time.
type LogEntry struct {
	VTime     float64 `json:"vtime"`
	Source    string  `json:"source"`
	Message   string  `json:"message"`
	Container string  `json:"container,omitempty"`
}

// BugObservation records whether a bug was observed in a given time window.
type BugObservation struct {
	VTime    float64 // virtual time of this window
	Observed bool    // was the bug seen in this window?
	LogLines []LogEntry
}

// ComputeBugReport builds a full bug probability timeline from observations.
//
// The algorithm:
//  1. Bucket observations into windows by vtime
//  2. For each window, compute raw probability = observed / total trials
//  3. Apply Beta-Binomial Bayesian smoothing with prior Beta(1, 1) (uniform)
//  4. Compute 90% credible interval from Beta posterior quantiles
//  5. Detect change points via windowed likelihood ratio
//  6. Compute Lomax findability statistics (mean time-to-bug, p_survival)
func ComputeBugReport(property, message string, observations []BugObservation, totalDuration float64) *BugReport {
	if len(observations) == 0 {
		return &BugReport{
			Property: property,
			Message:  message,
		}
	}

	// Sort by vtime.
	sort.Slice(observations, func(i, j int) bool {
		return observations[i].VTime < observations[j].VTime
	})

	// Determine windowing: divide total vtime range into buckets.
	minT := observations[0].VTime
	maxT := observations[len(observations)-1].VTime
	timeRange := maxT - minT
	if timeRange <= 0 {
		timeRange = 1.0
	}

	// Target ~100 windows for the chart, minimum window size 0.5s.
	windowSize := timeRange / 100.0
	if windowSize < 0.5 {
		windowSize = 0.5
	}
	numWindows := int(math.Ceil(timeRange / windowSize))
	if numWindows < 1 {
		numWindows = 1
	}

	// Bucket observations.
	type bucket struct {
		hits  int
		total int
		logs  []LogEntry
	}
	buckets := make([]bucket, numWindows)

	for _, obs := range observations {
		idx := int((obs.VTime - minT) / windowSize)
		if idx >= numWindows {
			idx = numWindows - 1
		}
		buckets[idx].total++
		if obs.Observed {
			buckets[idx].hits++
		}
		buckets[idx].logs = append(buckets[idx].logs, obs.LogLines...)
	}

	// Compute timeline.
	timeline := make([]BugProbabilityPoint, 0, numWindows)
	totalHits := 0
	totalTrials := 0

	// Running stats for change point detection.
	recentHits := 0
	recentTrials := 0
	windowLookback := 5 // compare recent 5 windows vs overall

	for i, b := range buckets {
		if b.total == 0 {
			continue
		}

		totalHits += b.hits
		totalTrials += b.total

		// Raw probability.
		rawP := float64(b.hits) / float64(b.total) * 100.0

		// Beta-Binomial posterior: Beta(alpha + hits, beta + misses)
		// Prior: Beta(0.5, 0.5) (Jeffreys prior; less biased than uniform for small samples)
		alpha := 0.5 + float64(totalHits)
		beta := 0.5 + float64(totalTrials-totalHits)

		// Posterior mean = alpha / (alpha + beta)
		smoothedP := alpha / (alpha + beta) * 100.0

		// 90% credible interval via Beta quantile approximation.
		// Use normal approximation for Beta distribution.
		ciLow, ciHigh := betaCredibleInterval(alpha, beta, 0.90)
		ciLow *= 100.0
		ciHigh *= 100.0

		// Change point detection: compare recent window rate vs overall rate.
		recentHits += b.hits
		recentTrials += b.total
		changeP := 0.0
		if i >= windowLookback && recentTrials > 0 {
			overallRate := float64(totalHits) / float64(totalTrials)
			recentRate := float64(recentHits) / float64(recentTrials)

			// Simple change point score: KL divergence between recent and overall.
			changeP = klDivergenceBernoulli(recentRate, overallRate) * 100.0
			if changeP > 100 {
				changeP = 100
			}

			// Slide the window: subtract oldest bucket from recent stats.
			if i >= windowLookback {
				old := buckets[i-windowLookback]
				recentHits -= old.hits
				recentTrials -= old.total
			}
		}

		vtime := minT + float64(i)*windowSize + windowSize/2.0

		timeline = append(timeline, BugProbabilityPoint{
			VTime:          vtime,
			Probability:    smoothedP,
			CILow:          ciLow,
			CIHigh:         ciHigh,
			RawProbability: rawP,
			ChangePointP:   changeP,
		})
	}

	// Collect all logs.
	var allLogs []LogEntry
	for _, b := range buckets {
		allLogs = append(allLogs, b.logs...)
	}

	// Findability statistics (Lomax distribution).
	// mean_time_to_bug = T / M
	// p_survival = probability of seeing the bug again in same duration.
	var meanTTB float64
	var pSurvival float64
	if totalHits > 0 {
		meanTTB = totalDuration / float64(totalHits)
		// Lomax survival: P(no bug in time T) = (1 + T/scale)^(-shape)
		// Simplified: use exponential approximation P = 1 - exp(-T/meanTTB)
		pSurvival = (1.0 - math.Exp(-totalDuration/meanTTB)) * 100.0
	}

	return &BugReport{
		Property:         property,
		Message:          message,
		Timeline:         timeline,
		Logs:             allLogs,
		MeanTimeToBug:    meanTTB,
		PSurvival:        pSurvival,
		TotalOccurrences: totalHits,
		TotalWindows:     totalTrials,
	}
}

// betaCredibleInterval computes the credible interval for Beta(alpha, beta)
// using the normal approximation. Returns (low, high) as proportions.
func betaCredibleInterval(alpha, beta, confidence float64) (float64, float64) {
	mean := alpha / (alpha + beta)
	variance := (alpha * beta) / ((alpha + beta) * (alpha + beta) * (alpha + beta + 1))
	sd := math.Sqrt(variance)

	// Z-score for 90% CI = 1.645
	z := 1.645
	if confidence >= 0.99 {
		z = 2.576
	} else if confidence >= 0.95 {
		z = 1.96
	}

	low := mean - z*sd
	high := mean + z*sd

	if low < 0 {
		low = 0
	}
	if high > 1 {
		high = 1
	}

	return low, high
}

// klDivergenceBernoulli computes KL divergence between two Bernoulli distributions.
// Higher values indicate more different distributions (likely change point).
func klDivergenceBernoulli(p, q float64) float64 {
	// Clamp to avoid log(0).
	const eps = 1e-10
	p = math.Max(eps, math.Min(1-eps, p))
	q = math.Max(eps, math.Min(1-eps, q))

	return p*math.Log(p/q) + (1-p)*math.Log((1-p)/(1-q))
}
