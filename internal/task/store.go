package task

import (
	"sync"
	"time"
)

type Status int

const (
	Running Status = iota
	Completed
	Failed
)

type Store struct {
	mu    sync.RWMutex
	tasks map[string]*entry
}

type entry struct {
	mu          sync.Mutex
	status      Status
	createdAt   time.Time
	buffer      []string     // all lines emitted so far
	subscribers []chan string // per-subscriber channels
	done        chan struct{} // closed when task completes
}

func NewStore() *Store {
	return &Store{tasks: make(map[string]*entry)}
}

// Create registers a new running task. Returns a Sender for writing
// log lines.
func (s *Store) Create(id string) Sender {
	e := &entry{
		status:    Running,
		createdAt: time.Now(),
		done:      make(chan struct{}),
	}
	s.mu.Lock()
	s.tasks[id] = e
	s.mu.Unlock()
	return Sender{e: e}
}

// Status returns the task's current status. Returns false if not found.
func (s *Store) Status(id string) (Status, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tasks[id]
	if !ok {
		return 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, true
}

// CreatedAt returns the task's creation timestamp as an RFC3339 string.
// Returns empty string if the task is not found.
func (s *Store) CreatedAt(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tasks[id]
	if !ok {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createdAt.Format(time.RFC3339)
}

// Subscribe returns a snapshot of buffered lines and a channel for
// live lines written after the snapshot. The live channel only
// delivers new lines — no dedup needed by the caller. The done
// channel is closed when the task completes.
func (s *Store) Subscribe(id string) (snapshot []string, live <-chan string, done <-chan struct{}, ok bool) {
	s.mu.RLock()
	e, found := s.tasks[id]
	s.mu.RUnlock()
	if !found {
		return nil, nil, nil, false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	snap := make([]string, len(e.buffer))
	copy(snap, e.buffer)

	ch := make(chan string, 64)
	// Only register a subscriber if the task is still running.
	// If already done, close the channel immediately so the caller
	// can detect completion after draining the snapshot.
	if e.status == Running {
		e.subscribers = append(e.subscribers, ch)
	} else {
		close(ch)
	}

	return snap, ch, e.done, true
}

// Sender writes log lines to a task.
type Sender struct {
	e *entry
}

func (s Sender) Write(line string) {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()

	s.e.buffer = append(s.e.buffer, line)
	// Non-blocking send to all subscribers — if a subscriber's
	// channel is full, the line is dropped from live delivery
	// but retained in the buffer for future subscribers.
	for _, ch := range s.e.subscribers {
		select {
		case ch <- line:
		default:
		}
	}
}

func (s Sender) Complete(status Status) {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()

	s.e.status = status
	for _, ch := range s.e.subscribers {
		close(ch)
	}
	s.e.subscribers = nil
	close(s.e.done)
}
