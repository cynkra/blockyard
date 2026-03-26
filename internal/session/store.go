package session

import (
	"log/slog"
	"sync"
	"time"
)

// Entry holds the state for a single session.
type Entry struct {
	WorkerID   string
	UserSub    string    // bound user identity; empty when OIDC is not configured
	LastAccess time.Time // updated on every proxy request; used for idle sweep
}

type Store struct {
	mu       sync.Mutex
	sessions map[string]Entry // session ID → entry
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]Entry)}
}

// Get returns the entry for the given session ID.
func (s *Store) Get(sessionID string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[sessionID]
	return e, ok
}

// Set creates or replaces a session entry.
func (s *Store) Set(sessionID string, entry Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.sessions[sessionID]
	s.sessions[sessionID] = entry
	if !exists {
		slog.Debug("session: created",
			"session_id", sessionID, "worker_id", entry.WorkerID,
			"user_sub", entry.UserSub)
	}
}

// Touch updates the LastAccess timestamp for an existing session.
// Returns false if the session does not exist.
func (s *Store) Touch(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[sessionID]
	if !ok {
		return false
	}
	e.LastAccess = time.Now()
	s.sessions[sessionID] = e
	return true
}

func (s *Store) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.sessions[sessionID]; ok {
		slog.Debug("session: deleted",
			"session_id", sessionID, "worker_id", e.WorkerID)
	}
	delete(s.sessions, sessionID)
}

// DeleteByWorker removes all sessions mapped to the given worker.
// Linear scan — acceptable at max_workers = 100.
func (s *Store) DeleteByWorker(workerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for sid, e := range s.sessions {
		if e.WorkerID == workerID {
			delete(s.sessions, sid)
			n++
		}
	}
	if n > 0 {
		slog.Debug("session: deleted all for worker",
			"worker_id", workerID, "count", n)
	}
	return n
}

func (s *Store) CountForWorker(workerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.sessions {
		if e.WorkerID == workerID {
			n++
		}
	}
	return n
}

// CountForWorkers returns the total session count across the given worker IDs.
func (s *Store) CountForWorkers(workerIDs []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]bool, len(workerIDs))
	for _, id := range workerIDs {
		set[id] = true
	}
	n := 0
	for _, e := range s.sessions {
		if set[e.WorkerID] {
			n++
		}
	}
	return n
}

// RerouteWorker reassigns all sessions from oldWorkerID to newWorkerID.
// Used by container transfer to migrate sessions to the new worker.
func (s *Store) RerouteWorker(oldWorkerID, newWorkerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for sid, e := range s.sessions {
		if e.WorkerID == oldWorkerID {
			e.WorkerID = newWorkerID
			s.sessions[sid] = e
			n++
		}
	}
	if n > 0 {
		slog.Debug("session: rerouted worker",
			"old_worker_id", oldWorkerID, "new_worker_id", newWorkerID,
			"count", n)
	}
	return n
}

// SweepIdle removes sessions whose LastAccess is older than maxAge.
// Returns the number of sessions removed.
func (s *Store) SweepIdle(maxAge time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	n := 0
	for sid, e := range s.sessions {
		if e.LastAccess.Before(cutoff) {
			slog.Debug("session: sweeping idle",
				"session_id", sid, "worker_id", e.WorkerID,
				"idle_since", e.LastAccess)
			delete(s.sessions, sid)
			n++
		}
	}
	return n
}
