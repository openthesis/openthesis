package fault

import (
	"time"

	"github.com/openthesis/openthesis/internal/prng"
)

// NetworkConfig controls probabilistic network fault injection.
type NetworkConfig struct {
	DropRate  float64       // [0,1] probability of dropping a packet
	DelayMin  time.Duration // minimum added delay
	DelayMax  time.Duration // maximum added delay
	CrashRate float64       // [0,1] probability of node crash per check

	// PartitionRate is the probability per (src,dst) pair of generating a
	// partition during a step. When >0, partitions are chosen independently
	// from drops and support a directional component (see PartitionDirection).
	PartitionRate float64

	// DirectedPartitionRatio is the probability that a partition is directional
	// (outbound-only or inbound-only) rather than symmetric. 0.0 means every
	// partition is bidirectional; 1.0 means every partition is one-way. The
	// direction (outbound vs inbound) is chosen uniformly at random when the
	// partition is directional.
	DirectedPartitionRatio float64

	// ThrottleRate is the probability per (src,dst) pair of generating a
	// bandwidth throttle during a step.
	ThrottleRate float64
	// ThrottleMinKbps is the minimum bandwidth cap applied when throttling.
	ThrottleMinKbps int
	// ThrottleMaxKbps is the maximum bandwidth cap applied when throttling.
	ThrottleMaxKbps int

	// ReorderRate is the probability per (src,dst) pair of reordering packets
	// during a step. Uses tc netem reorder with a fixed correlation.
	ReorderRate float64
	// ReorderCorrelation is the tc netem reorder correlation percentage [0,100].
	// Controls how often consecutive packets are reordered together.
	// Defaults to 25 if zero.
	ReorderCorrelation int

	// NWayPartitionRate is the probability per step of triggering an N-way
	// split-brain partition that divides all nodes into independent groups.
	// Unlike the per-pair PartitionRate (which can create overlapping partitions),
	// NWayPartition assigns every node to exactly one group. All inter-group
	// traffic is blocked symmetrically.
	NWayPartitionRate float64
	// NWayPartitionMaxGroups is the maximum number of partition groups to create.
	// Actual group count is chosen uniformly in [2, NWayPartitionMaxGroups].
	// Defaults to 3 if zero.
	NWayPartitionMaxGroups int
}

// PartitionDirection describes which traffic legs of a partition are blocked.
type PartitionDirection uint8

const (
	// PartitionBoth blocks traffic in both directions (symmetric partition).
	PartitionBoth PartitionDirection = iota
	// PartitionOutbound blocks only src → dst (dst → src replies still flow).
	PartitionOutbound
	// PartitionInbound blocks only dst → src (src → dst requests still flow).
	PartitionInbound
)

// String returns the lowercase canonical name for the direction, suitable
// for wire encoding and log output.
func (d PartitionDirection) String() string {
	switch d {
	case PartitionOutbound:
		return "outbound"
	case PartitionInbound:
		return "inbound"
	default:
		return "both"
	}
}

// Partition defines a network partition between two groups of nodes.
type Partition struct {
	GroupA   []NodeID
	GroupB   []NodeID
	Directed bool // if true, only A->B is blocked
}

// NetworkInjector injects probabilistic network faults.
type NetworkInjector struct {
	cfg        NetworkConfig
	partitions []Partition
}

// NewNetwork returns a NetworkInjector with the given configuration.
func NewNetwork(cfg NetworkConfig) *NetworkInjector {
	return &NetworkInjector{cfg: cfg}
}

// AddPartition adds a network partition.
func (n *NetworkInjector) AddPartition(p Partition) {
	n.partitions = append(n.partitions, p)
}

// ClearPartitions removes all network partitions.
func (n *NetworkInjector) ClearPartitions() {
	n.partitions = n.partitions[:0]
}

// ShouldDrop returns true if a message from src to dst should be dropped.
func (n *NetworkInjector) ShouldDrop(src, dst NodeID, rng *prng.Source) bool {
	if n.isPartitioned(src, dst) {
		return true
	}
	return rng.Float64() < n.cfg.DropRate
}

// Delay returns the additional latency to impose on a message from src to dst.
func (n *NetworkInjector) Delay(src, dst NodeID, rng *prng.Source) time.Duration {
	span := n.cfg.DelayMax - n.cfg.DelayMin
	if span <= 0 {
		return n.cfg.DelayMin
	}
	return n.cfg.DelayMin + time.Duration(rng.Float64()*float64(span))
}

// ShouldCrash returns true if the given node should be crashed.
func (n *NetworkInjector) ShouldCrash(node NodeID, rng *prng.Source) bool {
	return rng.Float64() < n.cfg.CrashRate
}

func (n *NetworkInjector) isPartitioned(src, dst NodeID) bool {
	for _, p := range n.partitions {
		if inGroup(src, p.GroupA) && inGroup(dst, p.GroupB) {
			return true
		}
		if !p.Directed && inGroup(src, p.GroupB) && inGroup(dst, p.GroupA) {
			return true
		}
	}
	return false
}

func inGroup(node NodeID, group []NodeID) bool {
	for _, n := range group {
		if n == node {
			return true
		}
	}
	return false
}

// ShouldPartition decides whether the (src, dst) pair should be partitioned
// during this step, and if so returns the direction of the partition.
//
// The decision draws exactly two Float64 values from rng (PRNG consumption
// is fixed regardless of outcome) so callers can reason about determinism
// without knowing whether PartitionRate happened to fire.
func (n *NetworkInjector) ShouldPartition(src, dst NodeID, rng *prng.Source) (bool, PartitionDirection) {
	roll := rng.Float64()
	dirRoll := rng.Float64()
	if n.cfg.PartitionRate <= 0 || roll >= n.cfg.PartitionRate {
		return false, PartitionBoth
	}
	if dirRoll >= n.cfg.DirectedPartitionRatio {
		return true, PartitionBoth
	}
	// Split the upper half of [0,1) evenly between outbound and inbound so
	// both one-way variants are exercised with equal probability.
	if dirRoll < n.cfg.DirectedPartitionRatio/2 {
		return true, PartitionOutbound
	}
	return true, PartitionInbound
}

// ShouldNWayPartition decides whether to trigger an N-way split-brain this step
// and, if so, returns the node groups. Every node is assigned to exactly one
// group; all traffic between groups is blocked symmetrically.
//
// The decision draws exactly two values from rng: one for the rate roll and one
// to choose the group count, keeping PRNG progression deterministic.
func (n *NetworkInjector) ShouldNWayPartition(nodes []NodeID, rng *prng.Source) (bool, [][]NodeID) {
	roll := rng.Float64()
	groupRoll := rng.Float64()
	if n.cfg.NWayPartitionRate <= 0 || roll >= n.cfg.NWayPartitionRate {
		return false, nil
	}
	if len(nodes) < 2 {
		return false, nil
	}
	maxGroups := n.cfg.NWayPartitionMaxGroups
	if maxGroups < 2 {
		maxGroups = 3
	}
	if maxGroups > len(nodes) {
		maxGroups = len(nodes)
	}
	// Choose group count in [2, maxGroups] uniformly.
	numGroups := 2 + int(groupRoll*float64(maxGroups-1))
	if numGroups > maxGroups {
		numGroups = maxGroups
	}

	// Assign nodes round-robin (after implicit PRNG shuffle via order).
	// Round-robin is deterministic and ensures each group gets at least one node.
	groups := make([][]NodeID, numGroups)
	for i, node := range nodes {
		groups[i%numGroups] = append(groups[i%numGroups], node)
	}

	// Drop empty groups (can happen if numGroups > len(nodes)).
	result := groups[:0]
	for _, g := range groups {
		if len(g) > 0 {
			result = append(result, g)
		}
	}
	if len(result) < 2 {
		return false, nil
	}
	return true, result
}

// ShouldReorder decides whether to reorder packets on the (src, dst) pair and,
// if so, returns the reorder correlation percentage. The correlation value is
// used as the tc netem reorder gap: 1/correlation packets bypass the netem
// delay and are sent immediately, causing reordering. Draws exactly two Float64
// values so callers can reason about deterministic PRNG progression.
func (n *NetworkInjector) ShouldReorder(src, dst NodeID, rng *prng.Source) (bool, int) {
	roll := rng.Float64()
	corrRoll := rng.Float64()
	if n.cfg.ReorderRate <= 0 || roll >= n.cfg.ReorderRate {
		return false, 0
	}
	corr := n.cfg.ReorderCorrelation
	if corr <= 0 {
		corr = 25
	}
	if corr > 100 {
		corr = 100
	}
	// Vary correlation slightly: [corr/2, corr].
	corr = corr/2 + int(corrRoll*float64(corr/2+1))
	return true, corr
}

// ShouldThrottle decides whether to bandwidth-limit the (src, dst) pair and,
// if so, returns the cap in kilobits per second. A return value of (false, 0)
// means the pair is not throttled this step. The rate is chosen uniformly in
// [ThrottleMinKbps, ThrottleMaxKbps].
//
// Like ShouldPartition the function draws exactly two Float64 values so
// callers can reason about PRNG progression without branching.
func (n *NetworkInjector) ShouldThrottle(src, dst NodeID, rng *prng.Source) (bool, int) {
	roll := rng.Float64()
	rateRoll := rng.Float64()
	if n.cfg.ThrottleRate <= 0 || roll >= n.cfg.ThrottleRate {
		return false, 0
	}
	lo, hi := n.cfg.ThrottleMinKbps, n.cfg.ThrottleMaxKbps
	if lo <= 0 {
		lo = 64
	}
	if hi <= lo {
		return true, lo
	}
	return true, lo + int(rateRoll*float64(hi-lo))
}
