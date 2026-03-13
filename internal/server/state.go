package server

import (
	"sync"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
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

	// Auth fields — nil when [oidc] is not configured (v0 compat).
	OIDCClient   *auth.OIDCClient
	SigningKey    *auth.SigningKey
	UserSessions *auth.UserSessionStore

	// Authorization — always initialized (used in both OIDC and static-token modes).
	RoleCache *auth.RoleMappingCache
	JWKSCache *auth.JWKSCache // nil when OIDC is not configured

	// Session token signing key — for credential exchange tokens.
	// Derived from session_secret with a different domain string.
	SessionTokenKey *auth.SigningKey

	// OpenBao — nil when [openbao] is not configured.
	VaultClient     *integration.Client
	VaultTokenCache *integration.VaultTokenCache

	// Draining tracks app IDs currently being drained (graceful stop).
	Draining *DrainSet
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
		Draining: NewDrainSet(),
	}
}

// AuthDeps returns an auth.Deps populated from this server's fields.
// Used by the router to wire auth handlers and middleware without a
// circular import.
func (s *Server) AuthDeps() *auth.Deps {
	return &auth.Deps{
		Config:       s.Config,
		OIDCClient:   s.OIDCClient,
		SigningKey:    s.SigningKey,
		UserSessions: s.UserSessions,
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

// AppIDs returns a deduplicated list of app IDs that have active workers.
func (m *WorkerMap) AppIDs() []string {
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

// DrainSet tracks app IDs that are currently being drained (graceful stop).
type DrainSet struct {
	mu  sync.Mutex
	set map[string]bool
}

func NewDrainSet() *DrainSet {
	return &DrainSet{set: make(map[string]bool)}
}

func (d *DrainSet) Add(appID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.set[appID] = true
}

func (d *DrainSet) Remove(appID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.set, appID)
}

func (d *DrainSet) Contains(appID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.set[appID]
}
