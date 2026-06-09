package fault

// ScriptFaultConfig holds the configuration for a user-defined fault script.
// The script runs inside the guest VM via the guest agent when a script fault fires.
type ScriptFaultConfig struct {
	// Path is the path inside the guest VM to the fault script.
	Path string
	// Args are additional arguments to pass to the script.
	Args []string
	// TimeoutSeconds is the maximum duration to wait for the script. Default: 30s.
	TimeoutSeconds int
	// Rate is the probability [0.0, 1.0] per step of triggering this script fault.
	Rate float64
}
