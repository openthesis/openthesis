package agent

import (
	"fmt"
	"os"
)

// Full coverage pipeline (guest -> host):
//
//   KCOV (kernel paths)        SHM bitmap (libvoidstar)    uprobe hits
//   -------------------------  --------------------------  ----------------------
//   /sys/kernel/debug/kcov     /run/kcov_bitmap (64K)      /sys/kernel/
//   buf[0]=count, buf[1..N]=PC AFL hit counts, MAP_SHARED  debug/tracing
//         |                          |                           |
//         v fold (prevPC>>1)^curPC   v copy on flush             v mergeIntoSlice
//   shared bitmap[64K] <-----------+-----------------------------------------+
//         |
//         v diff against prevBitmap
//   delta bitmap (only sent when new edges appear)
//         |
//         v base64 JSON over vsock
//   host orchestrator: kcovState.flushAndSend -> sendCoverageToHost

// CoverageSource provides edge coverage data from an instrumented process.
type CoverageSource interface {
	Name() string
	Collect() ([]byte, error) // returns edge bitmap
}

// SharedMemCoverage reads a flat AFL-style bitmap from a shared memory file.
type SharedMemCoverage struct {
	name    string
	shmPath string
	size    int
}

// NewSharedMemCoverage creates a coverage source backed by a shared memory file.
func NewSharedMemCoverage(name, shmPath string, size int) *SharedMemCoverage {
	return &SharedMemCoverage{
		name:    name,
		shmPath: shmPath,
		size:    size,
	}
}

// Name returns the identifier for this coverage source.
func (s *SharedMemCoverage) Name() string {
	return s.name
}

// Collect reads the current edge bitmap from shared memory.
func (s *SharedMemCoverage) Collect() ([]byte, error) {
	f, err := os.Open(s.shmPath)
	if err != nil {
		return nil, fmt.Errorf("coverage: open %s: %w", s.shmPath, err)
	}
	defer f.Close()

	buf := make([]byte, s.size)
	n, err := f.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("coverage: read %s: %w", s.shmPath, err)
	}
	return buf[:n], nil
}
