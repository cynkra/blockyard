package server

import (
	"sync"
	"time"

	"github.com/cynkra/blockyard/internal/audit"
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

	// Audit log — nil when [audit] is not configured.
	AuditLog *audit.Log
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

// AuthDeps returns an auth.Deps populated from this server's fields.
// Used by the router to wire auth handlers and middleware without a
// circular import.
func (s *Server) AuthDeps() *auth.Deps {
	return &auth.Deps{
		Config:       s.Config,
		OIDCClient:   s.OIDCClient,
		SigningKey:    s.SigningKey,
		UserSessions: s.UserSessions,
		AuditLog:     s.AuditLog,
	}
}

// ActiveWorker represents a running worker tracked by the server.
// The worker ID is the map key in WorkerMap, not stored here.
type ActiveWorker struct {
	AppID    string
	Draining bool      // set by graceful drain; no new sessions routed
	IdleSince time.Time // zero value = not idle; set when session count hits 0
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

// ForApp returns all worker IDs for a given app (including draining).
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

// ForAppAvailable returns worker IDs for an app that are not draining.
func (m *WorkerMap) ForAppAvailable(appID string) []string {
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
func (m *WorkerMap) MarkDraining(appID string) []string {
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

// SetIdleSince marks when a worker became idle (zero sessions).
// Called when the last session for a worker is removed.
func (m *WorkerMap) SetIdleSince(workerID string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[workerID]; ok {
		w.IdleSince = t
		m.workers[workerID] = w
	}
}

// ClearIdleSince resets the idle timer (a new session was assigned).
func (m *WorkerMap) ClearIdleSince(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[workerID]; ok {
		w.IdleSince = time.Time{}
		m.workers[workerID] = w
	}
}

// IdleWorkers returns workers that have been idle longer than the
// given timeout, excluding the last worker for each app (don't scale
// to zero) and excluding draining workers (they have their own lifecycle).
func (m *WorkerMap) IdleWorkers(timeout time.Duration) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	// Count non-draining workers per app.
	appWorkerCount := make(map[string]int)
	for _, w := range m.workers {
		if !w.Draining {
			appWorkerCount[w.AppID]++
		}
	}

	var idle []string
	for id, w := range m.workers {
		if w.IdleSince.IsZero() || w.Draining {
			continue
		}
		if now.Sub(w.IdleSince) < timeout {
			continue
		}
		// Don't remove the last worker for an app (no scale-to-zero).
		if appWorkerCount[w.AppID] <= 1 {
			continue
		}
		idle = append(idle, id)
	}
	return idle
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

// IsDraining returns true if any worker for the given app is draining.
func (m *WorkerMap) IsDraining(appID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workers {
		if w.AppID == appID && w.Draining {
			return true
		}
	}
	return false
}
