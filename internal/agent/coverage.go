package agent

import (
	"fmt"
	"os"
)

// CoverageSource provides edge coverage data from an instrumented process.
type CoverageSource interface {
	Name() string
	Collect() ([]byte, error) // returns edge bitmap
}

// SharedMemCoverage reads coverage from a shared memory region (like AFL).
type SharedMemCoverage struct {
	name    string
	shmPath string
	size    int
}

// NewSharedMemCoverage creates a coverage source that reads from a shared memory file.
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
