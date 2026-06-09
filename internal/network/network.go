// Package network provides a simulated message-passing network for containers.
package network

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/openthesis/openthesis/internal/clock"
	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/prng"
)

// ErrNodeNotFound indicates the specified node is not registered.
var ErrNodeNotFound = errors.New("network: node not found")

// Message is a unit of data sent between nodes.
type Message struct {
	ID        uint64
	Src       fault.NodeID
	Dst       fault.NodeID
	Payload   []byte
	SentAt    time.Time
	DeliverAt time.Time
	Dropped   bool
}

// Network is a simulated message-passing network.
// Messages are delivered deterministically based on virtual time and fault injection.
type Network struct {
	mu         sync.RWMutex
	injector   fault.Injector
	rng        *prng.Source
	clock      *clock.Virtual
	partitions *PartitionManager
	pending    []Message
	delivered  []Message
	nextMsgID  uint64
	nodes      map[fault.NodeID]bool
}

// New creates a Network with the given fault injector, PRNG source, and virtual clock.
func New(injector fault.Injector, rng *prng.Source, clk *clock.Virtual) *Network {
	return &Network{
		injector:   injector,
		rng:        rng,
		clock:      clk,
		partitions: NewPartitionManager(),
		nodes:      make(map[fault.NodeID]bool),
	}
}

// AddNode registers a node in the network.
func (n *Network) AddNode(id fault.NodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes[id] = true
}

// RemoveNode unregisters a node from the network.
func (n *Network) RemoveNode(id fault.NodeID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.nodes, id)
}

// Send enqueues a message from src to dst. Returns the message ID,
// whether it was dropped, and any error.
func (n *Network) Send(src, dst fault.NodeID, payload []byte) (uint64, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.nodes[src] {
		return 0, false, ErrNodeNotFound
	}
	if !n.nodes[dst] {
		return 0, false, ErrNodeNotFound
	}

	now := n.clock.Now()
	id := n.nextMsgID
	n.nextMsgID++

	msg := Message{
		ID:      id,
		Src:     src,
		Dst:     dst,
		Payload: copyBytes(payload),
		SentAt:  now,
	}

	// Check partitions first.
	if n.partitions.IsPartitioned(src, dst) {
		msg.Dropped = true
		msg.DeliverAt = now
		n.delivered = append(n.delivered, msg)
		return id, true, nil
	}

	// Check fault injection for drops.
	if n.injector.ShouldDrop(src, dst, n.rng) {
		msg.Dropped = true
		msg.DeliverAt = now
		n.delivered = append(n.delivered, msg)
		return id, true, nil
	}

	// Apply delay.
	delay := n.injector.Delay(src, dst, n.rng)
	msg.DeliverAt = now.Add(delay)

	n.pending = append(n.pending, msg)
	return id, false, nil
}

// Deliver returns all messages for dst deliverable at or before now.
// Messages are sorted by (DeliverAt, ID) for determinism.
func (n *Network) Deliver(dst fault.NodeID) []Message {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.clock.Now()
	var ready, remaining []Message

	for _, msg := range n.pending {
		if msg.Dst == dst && !msg.DeliverAt.After(now) {
			ready = append(ready, msg)
		} else {
			remaining = append(remaining, msg)
		}
	}
	n.pending = remaining

	sort.Slice(ready, func(i, j int) bool {
		if ready[i].DeliverAt.Equal(ready[j].DeliverAt) {
			return ready[i].ID < ready[j].ID
		}
		return ready[i].DeliverAt.Before(ready[j].DeliverAt)
	})

	n.delivered = append(n.delivered, ready...)
	return ready
}

// Pending returns the count of in-flight messages.
func (n *Network) Pending() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.pending)
}

// Partitions returns the partition manager for this network.
func (n *Network) Partitions() *PartitionManager {
	return n.partitions
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}
