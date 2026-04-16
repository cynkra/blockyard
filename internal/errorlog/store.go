// Package errorlog captures recent WARN/ERROR slog records into a bounded
// in-memory ring buffer for display in the admin UI. It is intentionally
// narrow: no persistence, no search, no time-range queries. Deeper
// investigation belongs in an external log aggregator.
package errorlog

import (
	"log/slog"
	"sync"
	"time"
)

// Entry is a captured slog record flattened for UI rendering.
type Entry struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   []Attr
}

// Attr is a key/value pair from the record (or surrounding handler scope).
// Values are pre-rendered to strings so the UI does not need to know about
// slog.Value kinds.
type Attr struct {
	Key   string
	Value string
}

// Store is a bounded ring buffer of captured entries. Writes are
// non-blocking; the oldest entry is overwritten when the buffer is full.
//
// Store is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	cap     int
	entries []Entry
	start   int // index of oldest entry when count==cap
	count   int // number of valid entries (<= cap)
}

// DefaultCapacity is the ring-buffer size used when NewStore is called
// with capacity <= 0.
const DefaultCapacity = 1000

// NewStore returns a ring buffer sized to capacity. Non-positive values
// fall back to DefaultCapacity.
func NewStore(capacity int) *Store {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Store{cap: capacity, entries: make([]Entry, capacity)}
}

// Append stores an entry, evicting the oldest when the buffer is full.
func (s *Store) Append(e Entry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count < s.cap {
		s.entries[(s.start+s.count)%s.cap] = e
		s.count++
		return
	}
	s.entries[s.start] = e
	s.start = (s.start + 1) % s.cap
}

// Snapshot returns entries ordered newest-first.
func (s *Store) Snapshot() []Entry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, s.count)
	for i := 0; i < s.count; i++ {
		idx := (s.start + s.count - 1 - i + s.cap) % s.cap
		out[i] = s.entries[idx]
	}
	return out
}

// Len returns the number of entries currently in the buffer.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// Cap returns the ring buffer capacity.
func (s *Store) Cap() int {
	if s == nil {
		return 0
	}
	return s.cap
}
