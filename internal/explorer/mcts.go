package explorer

import (
	"log/slog"
	"math"
	"sync"

	"github.com/openthesis/openthesis/internal/prng"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// MCTSNode augments a snapshot with MCTS statistics.
type MCTSNode struct {
	SnapshotID  snapshot.ID
	ParentID    snapshot.ID
	Children    []snapshot.ID
	Visits      uint64
	TotalReward float64
	Depth       uint32
}

// MCTSSelector implements UCB1-based Monte Carlo Tree Search over the
// snapshot tree. The snapshot tree IS the MCTS tree.
// Alphuzz (ACSAC 2022): https://dl.acm.org/doi/epdf/10.1145/3564625.3564660
//
// UCB1 scores each child as:
//
//   score = x_i + C * sqrt(ln(N) / n_i)
//
//   x_i  = average reward for child i (exploitation)
//   n_i  = visit count for child i
//   N    = visit count of parent
//   C    = exploration constant (default sqrt(2))
//
// Tree structure:
//
//   root (N=50)
//   +-- A (n=30, x=0.6)   <- UCB1 selects best child at each level
//   |   +-- A1 (n=10)
//   |   +-- A2 (n=0)      <- unvisited: selected before UCB1 applies
//   +-- B (n=20, x=0.3)
//
// Select: walk root -> leaf via UCB1; unvisited children take priority.
// Backpropagate: leaf -> root, visits++ and totalReward += reward at each node.
type MCTSSelector struct {
	mu    sync.RWMutex
	nodes map[snapshot.ID]*MCTSNode
	root  snapshot.ID
	c     float64 // exploration constant, default sqrt(2)
}

// NewMCTSSelector creates a selector with the given exploration constant.
// c = sqrt(2) is the theoretical optimum for UCB1.
func NewMCTSSelector(rootID snapshot.ID, c float64) *MCTSSelector {
	if c <= 0 {
		c = math.Sqrt2
	}
	m := &MCTSSelector{
		nodes: make(map[snapshot.ID]*MCTSNode),
		root:  rootID,
		c:     c,
	}
	// Register root node.
	m.nodes[rootID] = &MCTSNode{
		SnapshotID: rootID,
		ParentID:   rootID,
		Depth:      0,
	}
	return m
}

// RegisterNode adds a snapshot to the MCTS tree. Called when a new
// snapshot is created during exploration.
func (m *MCTSSelector) RegisterNode(id snapshot.ID, parentID snapshot.ID, depth uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.nodes[id]; exists {
		return
	}

	m.nodes[id] = &MCTSNode{
		SnapshotID: id,
		ParentID:   parentID,
		Depth:      depth,
	}

	// Add as child of parent.
	if parent, ok := m.nodes[parentID]; ok {
		parent.Children = append(parent.Children, id)
	}
}

// Select walks from root to a leaf using UCB1 at each branch point.
// Unvisited children are selected first. Ties broken by PRNG.
func (m *MCTSSelector) Select(rng *prng.Source) snapshot.ID {
	m.mu.RLock()
	defer m.mu.RUnlock()

	current := m.root
	for {
		node, ok := m.nodes[current]
		if !ok || len(node.Children) == 0 {
			return current
		}

		// Find unvisited children first.
		var unvisited []snapshot.ID
		for _, childID := range node.Children {
			if child, ok := m.nodes[childID]; ok && child.Visits == 0 {
				unvisited = append(unvisited, childID)
			}
		}
		if len(unvisited) > 0 {
			// Pick random unvisited child.
			idx := rng.Uint64() % uint64(len(unvisited))
			return unvisited[idx]
		}

		// All children visited; select by UCB1.
		bestScore := math.Inf(-1)
		bestChild := node.Children[0]
		parentVisits := node.Visits
		if parentVisits == 0 {
			parentVisits = 1
		}
		logParent := math.Log(float64(parentVisits))

		for _, childID := range node.Children {
			child, ok := m.nodes[childID]
			if !ok {
				continue
			}
			score := m.ucb1(child, logParent)
			if score > bestScore {
				bestScore = score
				bestChild = childID
			}
		}
		current = bestChild
	}
}

// ucb1 computes the UCB1 score for a node. Must hold at least RLock.
func (m *MCTSSelector) ucb1(node *MCTSNode, logParentVisits float64) float64 {
	if node.Visits == 0 {
		return math.Inf(1)
	}
	exploitation := node.TotalReward / float64(node.Visits)
	exploration := m.c * math.Sqrt(logParentVisits/float64(node.Visits))
	return exploitation + exploration
}

// Backpropagate walks from id to root, incrementing visits and adding
// reward at each ancestor. This credits the path that led to a discovery.
func (m *MCTSSelector) Backpropagate(id snapshot.ID, reward float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current := id
	for {
		node, ok := m.nodes[current]
		if !ok {
			break
		}
		node.Visits++
		node.TotalReward += reward
		if current == m.root {
			break
		}
		current = node.ParentID
	}

	if reward > 0 {
		slog.Debug("explorer: mcts backpropagate", "from", id, "reward", reward)
	}
}

// NodeCount returns the number of nodes in the MCTS tree.
func (m *MCTSSelector) NodeCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.nodes)
}

// UCB1Score returns the UCB1 score for a given node (for debugging/display).
func (m *MCTSSelector) UCB1Score(id snapshot.ID) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	node, ok := m.nodes[id]
	if !ok || node.Visits == 0 {
		return math.Inf(1)
	}

	parent, ok := m.nodes[node.ParentID]
	if !ok {
		return node.TotalReward / float64(node.Visits)
	}

	parentVisits := parent.Visits
	if parentVisits == 0 {
		parentVisits = 1
	}
	return m.ucb1(node, math.Log(float64(parentVisits)))
}

// Stats returns visits and average reward for a node.
func (m *MCTSSelector) Stats(id snapshot.ID) (visits uint64, avgReward float64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	node, ok := m.nodes[id]
	if !ok {
		return 0, 0
	}
	if node.Visits == 0 {
		return 0, 0
	}
	return node.Visits, node.TotalReward / float64(node.Visits)
}
