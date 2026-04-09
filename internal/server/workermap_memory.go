package server

import (
	"sync"
	"time"
)

// MemoryWorkerMap is a concurrent in-memory map of worker ID → ActiveWorker.
type MemoryWorkerMap struct {
	mu      sync.Mutex
	workers map[string]ActiveWorker
}

func NewMemoryWorkerMap() *MemoryWorkerMap {
	return &MemoryWorkerMap{workers: make(map[string]ActiveWorker)}
}

func (m *MemoryWorkerMap) Get(id string) (ActiveWorker, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[id]
	return w, ok
}

func (m *MemoryWorkerMap) Set(id string, w ActiveWorker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers[id] = w
}

func (m *MemoryWorkerMap) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.workers, id)
}

func (m *MemoryWorkerMap) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.workers)
}

func (m *MemoryWorkerMap) CountForApp(appID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, w := range m.workers {
		if w.AppID == appID {
			n++
		}
	}
	return n
}

// All returns a snapshot of all worker IDs.
func (m *MemoryWorkerMap) All() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.workers))
	for id := range m.workers {
		ids = append(ids, id)
	}
	return ids
}

// WorkersForServer returns all worker IDs. In the memory
// implementation every worker belongs to "this" server — single-node
// deployments have one process — so the serverID filter is a no-op.
// The Redis variant does the real filtering.
func (m *MemoryWorkerMap) WorkersForServer(_ string) []string {
	return m.All()
}

// ForApp returns all worker IDs for a given app (including draining).
func (m *MemoryWorkerMap) ForApp(appID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, w := range m.workers {
		if w.AppID == appID {
			ids = append(ids, id)
		}
	}
	return ids
}

// ForAppAvailable returns worker IDs for an app that are not draining.
func (m *MemoryWorkerMap) ForAppAvailable(appID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, w := range m.workers {
		if w.AppID == appID && !w.Draining {
			ids = append(ids, id)
		}
	}
	return ids
}

// MarkDraining sets the draining flag on all workers for an app.
// Returns the list of affected worker IDs.
func (m *MemoryWorkerMap) MarkDraining(appID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, w := range m.workers {
		if w.AppID == appID {
			w.Draining = true
			m.workers[id] = w
			ids = append(ids, id)
		}
	}
	return ids
}

// SetDraining sets the draining flag on a single worker by ID.
// Used by refresh drain-and-replace to drain specific old workers
// while keeping newly spawned workers available.
func (m *MemoryWorkerMap) SetDraining(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[workerID]; ok {
		w.Draining = true
		m.workers[workerID] = w
	}
}

// SetIdleSince marks when a worker became idle (zero sessions).
// Called when the last session for a worker is removed.
func (m *MemoryWorkerMap) SetIdleSince(workerID string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[workerID]; ok {
		w.IdleSince = t
		m.workers[workerID] = w
	}
}

// SetIdleSinceIfZero marks when a worker became idle, but only if it
// isn't already marked. This avoids resetting the timer on repeated
// ticks while the worker remains idle.
func (m *MemoryWorkerMap) SetIdleSinceIfZero(workerID string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[workerID]; ok && w.IdleSince.IsZero() {
		w.IdleSince = t
		m.workers[workerID] = w
	}
}

// ClearIdleSince resets the idle timer (a new session was assigned).
// Returns true if the worker was idle before clearing.
func (m *MemoryWorkerMap) ClearIdleSince(workerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[workerID]; ok {
		wasIdle := !w.IdleSince.IsZero()
		w.IdleSince = time.Time{}
		m.workers[workerID] = w
		return wasIdle
	}
	return false
}

// IdleWorkers returns workers that have been idle longer than the
// given timeout, excluding draining workers (they have their own lifecycle).
func (m *MemoryWorkerMap) IdleWorkers(timeout time.Duration) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var idle []string
	for id, w := range m.workers {
		if w.IdleSince.IsZero() || w.Draining {
			continue
		}
		if now.Sub(w.IdleSince) < timeout {
			continue
		}
		idle = append(idle, id)
	}
	return idle
}

// AppIDs returns a deduplicated list of app IDs that have active workers.
func (m *MemoryWorkerMap) AppIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]bool)
	var ids []string
	for _, w := range m.workers {
		if !seen[w.AppID] {
			seen[w.AppID] = true
			ids = append(ids, w.AppID)
		}
	}
	return ids
}

// ClearDraining clears the draining flag on a single worker by ID.
func (m *MemoryWorkerMap) ClearDraining(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[workerID]; ok {
		w.Draining = false
		m.workers[workerID] = w
	}
}

// IsDraining returns true if any worker for the given app is draining.
func (m *MemoryWorkerMap) IsDraining(appID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workers {
		if w.AppID == appID && w.Draining {
			return true
		}
	}
	return false
}

var _ WorkerMap = (*MemoryWorkerMap)(nil)
