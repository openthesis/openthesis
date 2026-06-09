package orchestrator

// ProgressEventKind classifies a ProgressEvent.
type ProgressEventKind int

const (
	// ProgressUpdate carries the current exploration counters.
	ProgressUpdate ProgressEventKind = iota
	// ProgressViolation is sent when a new assertion violation is detected.
	ProgressViolation
	// ProgressDone is sent when the exploration loop finishes (before Run returns).
	ProgressDone
)

// ProgressFaultArm is a snapshot of one fault arm's statistics for the TUI.
type ProgressFaultArm struct {
	Kind    string
	Pulls   uint64
	AvgRate float64 // normalised activation rate 0-1 (AvgReward / maxReward)
}

// ProgressEvent is sent over RunConfig.ProgressCh during exploration.
// The orchestrator sends these non-blocking; a full channel drops the event.
type ProgressEvent struct {
	Kind ProgressEventKind

	// ProgressUpdate fields - set for ProgressUpdate events.
	States     uint64
	Edges      uint64
	Violations int
	FaultArms  []ProgressFaultArm

	// ProgressViolation fields - set for ProgressViolation events.
	ViolationStep     uint64
	ViolationProperty string

	// ProgressDone fields - set for ProgressDone, contains final counters.
	FinalStates     uint64
	FinalEdges      uint64
	FinalViolations int
}

// sendProgress delivers evt on ch without blocking. If ch is full the event
// is silently discarded so exploration is never stalled by a slow TUI.
func sendProgress(ch chan<- ProgressEvent, evt ProgressEvent) {
	if ch == nil {
		return
	}
	select {
	case ch <- evt:
	default:
	}
}
