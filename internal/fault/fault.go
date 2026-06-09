// Package fault defines fault injection types and deterministic injectors.
package fault

import (
	"time"

	"github.com/openthesis/openthesis/internal/prng"
)

// NodeID identifies a node in the distributed system under test.
type NodeID = string

// Kind classifies a fault for dispatch to the correct subsystem.
type Kind string

const (
	KindDrop          Kind = "drop"
	KindDelay         Kind = "delay"
	KindPartition     Kind = "partition"
	KindThrottle      Kind = "throttle"
	KindHang          Kind = "hang"
	KindTerminate     Kind = "terminate"
	KindClockJitter   Kind = "clock_jitter"
	KindThreadPause   Kind = "thread_pause"
	KindReorder       Kind = "reorder"
	KindCPUThrottle   Kind = "cpu_throttle"
	KindNWayPartition Kind = "nway_partition"
	KindDiskSlow      Kind = "disk_slow"    // cgroupv2 io.max throttle
	KindDiskFull      Kind = "disk_full"    // fill disk with fallocate
	KindDiskCorrupt   Kind = "disk_corrupt" // write garbage to node data files
	KindMemPressure   Kind = "mem_pressure" // cgroupv2 memory.max cap
	KindClear         Kind = "clear"        // clears all active faults
	KindScript        Kind = "script"       // run a user shell script inside guest
	KindCPUModulate   Kind = "cpu_modulate" // simulate different clock speeds via cgroupv2 cpu.max
	KindDiskFlakey    Kind = "disk_flakey"  // block-level I/O error injection via dm-flakey
)

// KindMask is a bitmask representation of fault.Kind values for compact storage
// in FrontierEntry and snapshot.Node. Allows scoring to distinguish states reached
// under different fault combinations without string allocation.
type KindMask uint16

const (
	MaskDrop          KindMask = 1 << 0
	MaskDelay         KindMask = 1 << 1
	MaskPartition     KindMask = 1 << 2
	MaskThrottle      KindMask = 1 << 3
	MaskHang          KindMask = 1 << 4
	MaskTerminate     KindMask = 1 << 5
	MaskClockJitter   KindMask = 1 << 6
	MaskThreadPause   KindMask = 1 << 7
	MaskReorder       KindMask = 1 << 8
	MaskCPUThrottle   KindMask = 1 << 9
	MaskNWayPartition KindMask = 1 << 10
	MaskDiskSlow      KindMask = 1 << 11
	MaskDiskFull      KindMask = 1 << 12
	MaskDiskCorrupt   KindMask = 1 << 13
	MaskMemPressure   KindMask = 1 << 14
	MaskScript        KindMask = 1 << 15
)

// KindToMask returns the bitmask bit for a given Kind. Returns 0 for unknown kinds.
func KindToMask(k Kind) KindMask {
	switch k {
	case KindDrop:
		return MaskDrop
	case KindDelay:
		return MaskDelay
	case KindPartition:
		return MaskPartition
	case KindThrottle:
		return MaskThrottle
	case KindHang:
		return MaskHang
	case KindTerminate:
		return MaskTerminate
	case KindClockJitter:
		return MaskClockJitter
	case KindThreadPause:
		return MaskThreadPause
	case KindReorder:
		return MaskReorder
	case KindCPUThrottle:
		return MaskCPUThrottle
	case KindNWayPartition:
		return MaskNWayPartition
	case KindDiskSlow:
		return MaskDiskSlow
	case KindDiskFull:
		return MaskDiskFull
	case KindDiskCorrupt:
		return MaskDiskCorrupt
	case KindMemPressure:
		return MaskMemPressure
	case KindScript:
		return MaskScript
	default:
		return 0
	}
}

// MaskToKinds returns the list of Kind values set in the given KindMask.
func MaskToKinds(m KindMask) []string {
	pairs := []struct {
		mask KindMask
		kind string
	}{
		{MaskDrop, string(KindDrop)},
		{MaskDelay, string(KindDelay)},
		{MaskPartition, string(KindPartition)},
		{MaskThrottle, string(KindThrottle)},
		{MaskHang, string(KindHang)},
		{MaskTerminate, string(KindTerminate)},
		{MaskClockJitter, string(KindClockJitter)},
		{MaskThreadPause, string(KindThreadPause)},
		{MaskReorder, string(KindReorder)},
		{MaskCPUThrottle, string(KindCPUThrottle)},
		{MaskNWayPartition, string(KindNWayPartition)},
		{MaskDiskSlow, string(KindDiskSlow)},
		{MaskDiskFull, string(KindDiskFull)},
		{MaskDiskCorrupt, string(KindDiskCorrupt)},
		{MaskMemPressure, string(KindMemPressure)},
		{MaskScript, string(KindScript)},
	}
	var kinds []string
	for _, p := range pairs {
		if m&p.mask != 0 {
			kinds = append(kinds, p.kind)
		}
	}
	return kinds
}

// Fault describes a single injectable failure condition.
type Fault struct {
	Kind    Kind
	Targets []NodeID
	Params  map[string]any
}

// Injector decides whether to inject faults based on deterministic PRNG.
type Injector interface {
	ShouldDrop(src, dst NodeID, rng *prng.Source) bool
	Delay(src, dst NodeID, rng *prng.Source) time.Duration
	ShouldCrash(n NodeID, rng *prng.Source) bool
}

// NopInjector implements Injector and never injects any faults.
type NopInjector struct{}

func (NopInjector) ShouldDrop(_, _ NodeID, _ *prng.Source) bool     { return false }
func (NopInjector) Delay(_, _ NodeID, _ *prng.Source) time.Duration { return 0 }
func (NopInjector) ShouldCrash(_ NodeID, _ *prng.Source) bool       { return false }
