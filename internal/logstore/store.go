package logstore

import (
	"sync"
	"time"
)

const maxLogLines = 50_000

type Store struct {
	mu      sync.RWMutex
	entries map[string]*logEntry
}

type logEntry struct {
	appID   string
	buffer  []string
	ch      chan string
	endedAt time.Time // zero if still active
}

func NewStore() *Store {
	return &Store{entries: make(map[string]*logEntry)}
}

// Create registers a new log stream for a worker. Returns a Sender
// for writing log lines from the capture goroutine.
func (s *Store) Create(workerID, appID string) Sender {
	e := &logEntry{
		appID: appID,
		ch:    make(chan string, 64),
	}
	s.mu.Lock()
	s.entries[workerID] = e
	s.mu.Unlock()
	return Sender{e: e}
}

// Subscribe returns a snapshot and live channel for a worker's logs.
func (s *Store) Subscribe(workerID string) (snapshot []string, live <-chan string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, found := s.entries[workerID]
	if !found {
		return nil, nil, false
	}
	snap := make([]string, len(e.buffer))
	copy(snap, e.buffer)
	return snap, e.ch, true
}

// WorkerIDsByApp returns worker IDs for all workers of the given app.
func (s *Store) WorkerIDsByApp(appID string) (workerIDs []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for wid, e := range s.entries {
		if e.appID == appID {
			workerIDs = append(workerIDs, wid)
		}
	}
	return workerIDs
}

func (s *Store) MarkEnded(workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[workerID]; ok {
		e.endedAt = time.Now()
		close(e.ch)
	}
}

func (s *Store) HasActive(workerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[workerID]
	return ok && e.endedAt.IsZero()
}

// CleanupExpired removes log entries that ended more than `retention` ago.
func (s *Store) CleanupExpired(retention time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-retention)
	n := 0
	for wid, e := range s.entries {
		if !e.endedAt.IsZero() && e.endedAt.Before(cutoff) {
			delete(s.entries, wid)
			n++
		}
	}
	return n
}

type Sender struct {
	e *logEntry
}

func (s Sender) Write(line string) {
	if len(s.e.buffer) < maxLogLines {
		s.e.buffer = append(s.e.buffer, line)
	}
	select {
	case s.e.ch <- line:
	default:
	}
}
