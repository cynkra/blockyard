package logstore

import (
	"sync"
	"time"
)

const maxLogLines = 50_000

type Store struct {
	mu      sync.Mutex
	entries map[string]*logEntry
}

type logEntry struct {
	mu      sync.Mutex // protects buffer and ended
	appID   string
	buffer  []string
	ch      chan string
	ended   bool
	endedAt time.Time
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
	s.mu.Lock()
	e, found := s.entries[workerID]
	s.mu.Unlock()
	if !found {
		return nil, nil, false
	}
	e.mu.Lock()
	snap := make([]string, len(e.buffer))
	copy(snap, e.buffer)
	e.mu.Unlock()
	return snap, e.ch, true
}

// WorkerIDsByApp returns worker IDs for all workers of the given app.
func (s *Store) WorkerIDsByApp(appID string) (workerIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for wid, e := range s.entries {
		if e.appID == appID {
			workerIDs = append(workerIDs, wid)
		}
	}
	return workerIDs
}



// MarkEnded marks a worker's log stream as ended. Idempotent — safe to
// call multiple times or on nonexistent workers.
func (s *Store) MarkEnded(workerID string) {
	s.mu.Lock()
	e, ok := s.entries[workerID]
	s.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ended {
		return
	}
	e.ended = true
	e.endedAt = time.Now()
	close(e.ch)
}

func (s *Store) HasActive(workerID string) bool {
	s.mu.Lock()
	e, ok := s.entries[workerID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.ended
}

// IsEnded returns true if the worker's log stream has ended.
func (s *Store) IsEnded(workerID string) bool {
	s.mu.Lock()
	e, ok := s.entries[workerID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ended
}

// CleanupExpired removes log entries that ended more than `retention` ago.
func (s *Store) CleanupExpired(retention time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-retention)
	n := 0
	for wid, e := range s.entries {
		e.mu.Lock()
		expired := e.ended && e.endedAt.Before(cutoff)
		e.mu.Unlock()
		if expired {
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
	s.e.mu.Lock()
	if len(s.e.buffer) < maxLogLines {
		s.e.buffer = append(s.e.buffer, line)
	}
	s.e.mu.Unlock()
	select {
	case s.e.ch <- line:
	default:
	}
}
