package cli

// CmdAuto is an alias for `openthesis campaign --adaptive`.
// It exists for backward compatibility with existing scripts.
func CmdAuto(args []string) int {
	return CmdCampaign(append([]string{"--adaptive"}, args...))
}

func escalMultiplierFromLevel(level int) float64 {
	m := 1.0
	for range level {
		m *= 1.5
	}
	if m > 4.0 {
		m = 4.0
	}
	return m
}
