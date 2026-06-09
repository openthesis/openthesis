package main

// cmdCausality implements `openthesis causality --artifact <dir>`.
//
// Causality analysis rewinds to different points in a violation's timeline,
// re-runs forward many times with varied fault schedules, and plots a
// probability-over-time curve. Sharp jumps on the curve are the inception
// point - the moment the bug became likely.
//
// All flag parsing and computation is handled by cmdInvestigate with
// --likelihood pre-set so the probability curve is always computed.
func cmdCausality(args []string) int {
	// Rewrite "causality" in the binary name so --help shows the right command.
	// os.Args[0] is the binary, os.Args[1] would be "causality" already.
	return cmdInvestigate(append([]string{"--likelihood"}, args...))
}
