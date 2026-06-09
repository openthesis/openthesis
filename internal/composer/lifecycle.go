package composer

// Phase represents a lifecycle stage of the test composer.
type Phase uint8

const (
	PhaseInit   Phase = iota // waiting for setup_complete
	PhaseSetup               // running setup commands
	PhaseRun                 // running workload commands with faults active
	PhaseSettle              // faults stopped, running settle commands
	PhaseVerify              // workloads completed, running verify commands
	PhaseDone
)

var phaseNames = [...]string{
	"init",
	"setup",
	"run",
	"settle",
	"verify",
	"done",
}

// String returns the human-readable phase name.
func (p Phase) String() string {
	if int(p) < len(phaseNames) {
		return phaseNames[p]
	}
	return "unknown"
}
