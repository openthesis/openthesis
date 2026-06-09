package hypervisor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// isolatedCPUPool tracks which isolated CPUs are available for pinning.
// CPUs are loaded from /sys/devices/system/cpu/isolated at startup and
// returned to the pool when a VM stops.
type isolatedCPUPool struct {
	mu   sync.Mutex
	cpus []int
	free []bool
	idx  int // round-robin index into cpus
}

// newIsolatedCPUPool reads /sys/devices/system/cpu/isolated and builds the pool.
// Returns an empty pool (not an error) if no CPUs are isolated.
func newIsolatedCPUPool() (*isolatedCPUPool, error) {
	data, err := os.ReadFile("/sys/devices/system/cpu/isolated")
	if err != nil {
		return &isolatedCPUPool{}, err
	}
	cpus, err := parseCPUList(strings.TrimSpace(string(data)))
	if err != nil {
		return &isolatedCPUPool{}, err
	}
	free := make([]bool, len(cpus))
	for i := range free {
		free[i] = true
	}
	return &isolatedCPUPool{cpus: cpus, free: free}, nil
}

// next returns the next free isolated CPU.
// Returns (cpu, true) if a free CPU is available, (0, false) if all are in use.
// The caller must not pin if ok is false; sharing an isolated core between
// two Firecracker processes breaks the PMC instruction-counter determinism
// guarantee (both processes preempt each other on the same core).
func (p *isolatedCPUPool) next() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range len(p.cpus) {
		slot := (p.idx + i) % len(p.cpus)
		if p.free[slot] {
			p.free[slot] = false
			p.idx = (slot + 1) % len(p.cpus)
			return p.cpus[slot], true
		}
	}
	return 0, false
}

// release marks a CPU as free.
func (p *isolatedCPUPool) release(cpu int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.cpus {
		if c == cpu {
			p.free[i] = true
			return
		}
	}
}

// parseCPUList parses a Linux CPU list string like "2,4-7,10" into a []int.
func parseCPUList(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var cpus []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dash := strings.Index(part, "-"); dash >= 0 {
			lo, err := strconv.Atoi(part[:dash])
			if err != nil {
				return nil, fmt.Errorf("cpu list parse %q: %w", part, err)
			}
			hi, err := strconv.Atoi(part[dash+1:])
			if err != nil {
				return nil, fmt.Errorf("cpu list parse %q: %w", part, err)
			}
			for c := lo; c <= hi; c++ {
				cpus = append(cpus, c)
			}
		} else {
			c, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("cpu list parse %q: %w", part, err)
			}
			cpus = append(cpus, c)
		}
	}
	return cpus, nil
}
