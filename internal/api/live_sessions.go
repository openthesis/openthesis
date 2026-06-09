package api

import (
	"sync"

	"github.com/openthesis/openthesis/internal/orchestrator"
)

// liveSessionManager holds active LiveSession instances keyed by notebook ID.
type liveSessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*orchestrator.LiveSession
}

func newLiveSessionManager() *liveSessionManager {
	return &liveSessionManager{
		sessions: make(map[string]*orchestrator.LiveSession),
	}
}

func (m *liveSessionManager) set(notebookID string, ls *orchestrator.LiveSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.sessions[notebookID]; ok {
		_ = existing.Close()
	}
	m.sessions[notebookID] = ls
}

func (m *liveSessionManager) get(notebookID string) (*orchestrator.LiveSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ls, ok := m.sessions[notebookID]
	return ls, ok
}

func (m *liveSessionManager) delete(notebookID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ls, ok := m.sessions[notebookID]; ok {
		_ = ls.Close()
		delete(m.sessions, notebookID)
	}
}

func (m *liveSessionManager) alive(notebookID string) bool {
	ls, ok := m.get(notebookID)
	return ok && ls.IsAlive()
}
