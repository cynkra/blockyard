package server

import (
	"sync"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/logstore"
	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/task"
)

// Server holds all shared state for the running server.
// Passed by pointer to API handlers, proxy, and background goroutines.
type Server struct {
	Config   *config.Config
	Backend  backend.Backend
	DB       *db.DB
	Workers  *WorkerMap
	Sessions *session.Store
	Registry *registry.Registry
	Tasks    *task.Store
	LogStore *logstore.Store
}

// NewServer creates a Server with all in-memory stores initialized.
func NewServer(cfg *config.Config, be backend.Backend, database *db.DB) *Server {
	return &Server{
		Config:   cfg,
		Backend:  be,
		DB:       database,
		Workers:  NewWorkerMap(),
		Sessions: session.NewStore(),
		Registry: registry.New(),
		Tasks:    task.NewStore(),
		LogStore: logstore.NewStore(),
	}
}

// ActiveWorker represents a running worker tracked by the server.
// The worker ID is the map key in WorkerMap, not stored here.
type ActiveWorker struct {
	AppID string
}

// WorkerMap is a concurrent map of worker ID → ActiveWorker.
type WorkerMap struct {
	mu      sync.Mutex
	workers map[string]ActiveWorker
}

func NewWorkerMap() *WorkerMap {
	return &WorkerMap{workers: make(map[string]ActiveWorker)}
}

func (m *WorkerMap) Get(id string) (ActiveWorker, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[id]
	return w, ok
}

func (m *WorkerMap) Set(id string, w ActiveWorker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers[id] = w
}

func (m *WorkerMap) Delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.workers, id)
}

func (m *WorkerMap) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.workers)
}

func (m *WorkerMap) CountForApp(appID string) int {
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
func (m *WorkerMap) All() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.workers))
	for id := range m.workers {
		ids = append(ids, id)
	}
	return ids
}

// ForApp returns all worker IDs for a given app.
func (m *WorkerMap) ForApp(appID string) []string {
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
