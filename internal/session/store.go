package session

import "sync"

type Store struct {
	mu       sync.RWMutex
	sessions map[string]string // session ID → worker ID
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]string)}
}

func (s *Store) Get(sessionID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	wid, ok := s.sessions[sessionID]
	return wid, ok
}

func (s *Store) Set(sessionID, workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = workerID
}

func (s *Store) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// DeleteByWorker removes all sessions mapped to the given worker.
// Linear scan — acceptable at max_workers = 100.
func (s *Store) DeleteByWorker(workerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for sid, wid := range s.sessions {
		if wid == workerID {
			delete(s.sessions, sid)
			n++
		}
	}
	return n
}

func (s *Store) CountForWorker(workerID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, wid := range s.sessions {
		if wid == workerID {
			n++
		}
	}
	return n
}
