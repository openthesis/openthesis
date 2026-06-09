// Package debugger provides multiverse time-travel debugging via snapshot trees.
package debugger

import (
	"fmt"
	"sync"
	"time"

	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// Session is a time-travel debugging cursor for a single completed run.
// It holds a pointer to the run's event store path and snapshot tree,
// and tracks a "current step"; the user's position in the execution timeline.
// All queries are scoped to events at or before CurrentStep.
// Mode is "cursor" (read-only event queries); replay mode is not yet implemented.
type Session struct {
	ID        string
	RunID     string
	ProjectID string
	TestID    string
	CreatedAt time.Time
	Mode      string // "cursor" (read-only); "replay" in future

	mu          sync.RWMutex
	currentStep uint64
	maxStep     uint64 // highest step seen in the event store
	evStorePath string
	treePath    string
	tree        *snapshot.Tree // loaded lazily, nil until first access
}

// CurrentStep returns the session's current position in the timeline.
func (s *Session) CurrentStep() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentStep
}

// MaxStep returns the highest step number recorded for this run.
func (s *Session) MaxStep() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxStep
}

// Advance moves the cursor forward by n steps (capped at MaxStep).
func (s *Session) Advance(n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentStep += n
	if s.currentStep > s.maxStep {
		s.currentStep = s.maxStep
	}
}

// Rewind moves the cursor backward by n steps (floored at 0).
func (s *Session) Rewind(n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= s.currentStep {
		s.currentStep = 0
	} else {
		s.currentStep -= n
	}
}

// JumpToStep sets the cursor to an absolute step position.
func (s *Session) JumpToStep(step uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if step > s.maxStep {
		step = s.maxStep
	}
	s.currentStep = step
}

// Events returns events from the run's event store scoped to [fromStep, currentStep].
// Applies the given extra filter on top of the step range.
func (s *Session) Events(extra eventstore.Filter) ([]eventstore.Event, error) {
	s.mu.RLock()
	step := s.currentStep
	s.mu.RUnlock()

	// Use step as inclusive upper bound by scanning all events and filtering post-read.
	// This is simpler than extending the Filter API for now.
	all, err := eventstore.Query(s.evStorePath, eventstore.Filter{
		Types:     extra.Types,
		Container: extra.Container,
		Limit:     extra.Limit,
	})
	if err != nil {
		return nil, err
	}
	var out []eventstore.Event
	for _, e := range all {
		if e.Step <= step {
			out = append(out, e)
		}
	}
	return out, nil
}

// Moments returns snapshot tree nodes up to the current step's virtual time.
// The virtual time is derived from the snapshot whose Step matches the cursor.
func (s *Session) Moments() ([]snapshot.NodeInfo, error) {
	s.mu.Lock()
	if s.tree == nil {
		tree, err := snapshot.LoadTree(s.treePath)
		if err != nil {
			// No tree yet (pre-snapshot-persistence build or run still active).
			s.mu.Unlock()
			return nil, nil
		}
		s.tree = tree
	}
	tree := s.tree
	s.mu.Unlock()

	return tree.History(), nil
}

// SessionState is the JSON-serializable summary of a session.
type SessionState struct {
	ID          string    `json:"id"`
	RunID       string    `json:"run_id"`
	ProjectID   string    `json:"project_id"`
	TestID      string    `json:"test_id"`
	Mode        string    `json:"mode"`
	CurrentStep uint64    `json:"current_step"`
	MaxStep     uint64    `json:"max_step"`
	CreatedAt   time.Time `json:"created_at"`
}

// EventStorePath returns the on-disk path to the event store for this session's run.
// Used by the shrink API to perform read-only oracle checks.
func (s *Session) EventStorePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.evStorePath
}

// State returns the serializable state of this session.
func (s *Session) State() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SessionState{
		ID:          s.ID,
		RunID:       s.RunID,
		ProjectID:   s.ProjectID,
		TestID:      s.TestID,
		Mode:        s.Mode,
		CurrentStep: s.currentStep,
		MaxStep:     s.maxStep,
		CreatedAt:   s.CreatedAt,
	}
}

// SessionManager manages all active debug sessions.
// Sessions are in-memory and lost on server restart (acceptable for debugging tools).
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewSessionManager creates an empty SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session)}
}

// Create makes a new session for the given run.
// evStorePath and treePath are the on-disk paths to the event store and snapshot tree.
func (m *SessionManager) Create(id, runID, pid, tid, evStorePath, treePath string) (*Session, error) {
	// Scan event store to find maxStep.
	events, err := eventstore.Query(evStorePath, eventstore.Filter{Limit: 0})
	if err != nil {
		// Empty or missing event store; session still usable, just no events.
		events = nil
	}
	var maxStep uint64
	for _, e := range events {
		if e.Step > maxStep {
			maxStep = e.Step
		}
	}

	sess := &Session{
		ID:          id,
		RunID:       runID,
		ProjectID:   pid,
		TestID:      tid,
		Mode:        "cursor",
		CreatedAt:   time.Now().UTC(),
		currentStep: maxStep, // default: start at end of run
		maxStep:     maxStep,
		evStorePath: evStorePath,
		treePath:    treePath,
	}

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	return sess, nil
}

// Get returns the session with the given ID.
func (m *SessionManager) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	return s, nil
}

// Delete removes a session.
func (m *SessionManager) Delete(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// List returns all active sessions as states.
func (m *SessionManager) List() []SessionState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionState, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.State())
	}
	return out
}
