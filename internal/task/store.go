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
	status    Status
	createdAt time.Time
	buffer    []string     // all lines emitted so far
	ch        chan string   // live followers receive here
	done      chan struct{} // closed when task completes
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
		ch:        make(chan string, 64),
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
	return e.status, true
}

// Subscribe returns a snapshot of buffered lines and a channel for
// live lines. The caller must read the snapshot first, then follow
// the channel. The done channel is closed when the task completes.
func (s *Store) Subscribe(id string) (snapshot []string, live <-chan string, done <-chan struct{}, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, found := s.tasks[id]
	if !found {
		return nil, nil, nil, false
	}
	snap := make([]string, len(e.buffer))
	copy(snap, e.buffer)
	return snap, e.ch, e.done, true
}

// Sender writes log lines to a task. Not safe for concurrent use —
// one sender per task, owned by the restore goroutine.
type Sender struct {
	e *entry
}

func (s Sender) Write(line string) {
	s.e.buffer = append(s.e.buffer, line)
	// Non-blocking send — if nobody is following, drop the live line.
	// The buffer has it for later subscribers.
	select {
	case s.e.ch <- line:
	default:
	}
}

func (s Sender) Complete(status Status) {
	s.e.status = status
	close(s.e.ch)
	close(s.e.done)
}
