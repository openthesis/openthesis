package main

// cmdCausality implements `openthesis causality --artifact <dir>`.
// Alias for cmdInvestigate with --likelihood pre-set.
func cmdCausality(args []string) int {
	return cmdInvestigate(append([]string{"--likelihood"}, args...))
}
