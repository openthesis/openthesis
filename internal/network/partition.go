package network

import (
	"sync"

	"github.com/openthesis/openthesis/internal/fault"
)

// PartitionManager tracks active network partitions.
type PartitionManager struct {
	mu         sync.RWMutex
	partitions []fault.Partition
}

// NewPartitionManager creates an empty PartitionManager.
func NewPartitionManager() *PartitionManager {
	return &PartitionManager{}
}

// Add registers a new partition.
func (pm *PartitionManager) Add(p fault.Partition) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.partitions = append(pm.partitions, p)
}

// Clear removes all active partitions.
func (pm *PartitionManager) Clear() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.partitions = nil
}

// IsPartitioned reports whether any active partition blocks traffic from src to dst.
// For directed partitions, only GroupA->GroupB is blocked.
// For undirected partitions, both directions are blocked.
func (pm *PartitionManager) IsPartitioned(src, dst fault.NodeID) bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	for _, p := range pm.partitions {
		if inGroup(src, p.GroupA) && inGroup(dst, p.GroupB) {
			return true
		}
		if !p.Directed && inGroup(src, p.GroupB) && inGroup(dst, p.GroupA) {
			return true
		}
	}
	return false
}

// Active returns a copy of the active partitions.
func (pm *PartitionManager) Active() []fault.Partition {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	out := make([]fault.Partition, len(pm.partitions))
	copy(out, pm.partitions)
	return out
}

func inGroup(id fault.NodeID, group []fault.NodeID) bool {
	for _, g := range group {
		if g == id {
			return true
		}
	}
	return false
}
