package orchestrator

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain verifies that no goroutines leak after orchestrator tests complete.
// Workers must exit cleanly when their context is cancelled.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
