package server

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/preflight"
	"github.com/cynkra/blockyard/internal/bundle"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/logstore"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/redisstate"
	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/telemetry"

	"github.com/prometheus/client_golang/prometheus"
)

// Server holds all shared state for the running server.
// Passed by pointer to API handlers, proxy, and background goroutines.
type Server struct {
	Config   *config.Config
	Backend  backend.Backend
	DB       *db.DB
	Workers  WorkerMap
	Sessions session.Store
	Registry registry.WorkerRegistry
	Tasks    *task.Store
	LogStore *logstore.Store
	Metrics  *telemetry.Metrics

	// Auth fields — nil when [oidc] is not configured (v0 compat).
	OIDCClient   *auth.OIDCClient
	SigningKey    *auth.SigningKey
	UserSessions *auth.UserSessionStore
	// Session token signing key — for credential exchange tokens.
	// Derived from session_secret with a different domain string.
	SessionTokenKey *auth.SigningKey

	// OpenBao — nil when [openbao] is not configured.
	VaultClient     *integration.Client
	VaultTokenCache *integration.VaultTokenCache

	// VaultTokenHealthy reports whether the vault token is valid.
	// Non-nil only when AppRole auth is used (token renewal active).
	VaultTokenHealthy func() bool

	// Redis client — nil when [redis] is not configured.
	RedisClient *redisstate.Client

	// Audit log — nil when [audit] is not configured.
	AuditLog *audit.Log

	// System checks — populated during startup, used by the system page
	// and API endpoints. Nil until Init is called.
	Checker *preflight.Checker

	// Package store — nil when not available (no builds yet).
	PkgStore *pkgstore.Store

	// HMAC key for worker tokens. Persisted via OpenBao or file-based
	// fallback so both servers verify the same tokens during a rolling
	// update. Independent of SessionSecret and OIDC.
	WorkerTokenKey *auth.SigningKey

	// Per-worker mutex for runtime package installs. Serializes
	// installs to the same worker to avoid races on .packages.json,
	// library state, and conflict detection.
	installMus sync.Map // workerID → *sync.Mutex

	// Tracks workers that have a container transfer in progress.
	// Prevents a second install from starting a parallel transfer.
	transferring sync.Map // workerID → bool

	// Process-local cancel functions for worker token refresher goroutines.
	// Kept on Server (not in ActiveWorker) because func() cannot be
	// serialized to Redis in phase 3-3.
	cancelTokens sync.Map // workerID → func()

	// Draining is set when the server enters drain mode (SIGUSR1) or
	// shutdown (SIGTERM). Health endpoints return 503 when set.
	Draining atomic.Bool

	// Passive is true when BLOCKYARD_PASSIVE=1 is set. Background
	// goroutines are deferred until POST /api/v1/admin/activate.
	Passive atomic.Bool

	// Bootstrap token state — one-time token that can be exchanged for
	// a real PAT via POST /api/v1/bootstrap. Hash is set at startup;
	// Redeemed is flipped to true on first successful exchange.
	BootstrapTokenHash []byte
	BootstrapRedeemed  atomic.Bool

	// Version is the server version string, set at build time.
	Version string

	// UpdateAvailable stores the latest available version string when
	// a newer release is detected. Nil means no update or not yet checked.
	UpdateAvailable atomic.Pointer[string]

	// RestoreWG is used in tests to wait for background restore goroutines
	// to complete before cleanup. Nil in production.
	RestoreWG *sync.WaitGroup

	// Hooks for operations that would cause import cycles if called
	// directly from server. Set during initialization in main().
	EvictWorkerFn    func(ctx context.Context, srv *Server, workerID, reason string)
	SpawnLogCaptureFn func(ctx context.Context, srv *Server, workerID, appID string)
}

// SetUpdateAvailable records that a newer version is available.
func (srv *Server) SetUpdateAvailable(v string) {
	srv.UpdateAvailable.Store(&v)
}

// GetVersion returns the running server version.
func (srv *Server) GetVersion() string {
	return srv.Version
}

// workerInstallMu returns a per-worker mutex, creating one if needed.
func (srv *Server) workerInstallMu(workerID string) *sync.Mutex {
	v, _ := srv.installMus.LoadOrStore(workerID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// CleanupInstallMu removes the per-worker mutex (called on eviction).
func (srv *Server) CleanupInstallMu(workerID string) {
	srv.installMus.Delete(workerID)
}

// SetCancelToken registers a cancel function for a worker's token refresher.
func (srv *Server) SetCancelToken(workerID string, cancel func()) {
	if cancel != nil {
		srv.cancelTokens.Store(workerID, cancel)
	}
}

// CancelTokenRefresher calls and removes the cancel function for a worker.
// No-op if the worker has no registered cancel function.
func (srv *Server) CancelTokenRefresher(workerID string) {
	if v, ok := srv.cancelTokens.LoadAndDelete(workerID); ok {
		v.(func())()
	}
}

// IsTransferring returns true if a container transfer is in progress
// for the given worker.
func (srv *Server) IsTransferring(workerID string) bool {
	_, ok := srv.transferring.Load(workerID)
	return ok
}

// SetTransferring marks a worker as having a transfer in progress.
func (srv *Server) SetTransferring(workerID string) {
	srv.transferring.Store(workerID, true)
}

// ClearTransferring removes the transfer-in-progress flag for a worker.
func (srv *Server) ClearTransferring(workerID string) {
	srv.transferring.Delete(workerID)
}

// BundlePaths returns the filesystem paths for a bundle.
func (srv *Server) BundlePaths(appID, bundleID string) bundle.Paths {
	return bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, appID, bundleID)
}

// InternalAPIURL returns the URL workers should use to reach this server.
func (srv *Server) InternalAPIURL() string {
	if srv.Config.Docker.ServiceNetwork != "" {
		_, port, _ := net.SplitHostPort(srv.Config.Server.Bind)
		if port == "" {
			port = "8080"
		}
		return "http://blockyard:" + port
	}
	if srv.Config.Server.ExternalURL != "" {
		return srv.Config.Server.ExternalURL
	}
	return "http://host.docker.internal" + srv.Config.Server.Bind
}

// NewServer creates a Server with all in-memory stores initialized.
// The returned Server owns a fresh per-instance [telemetry.Metrics]
// registered against a private prometheus registry, so tests that
// construct a Server never contend on a shared counter state. Production
// callers should replace [Server.Metrics] with one registered against
// [prometheus.DefaultRegisterer] so the /metrics HTTP handler can scrape
// it — see [NewServerWithDefaultMetrics].
func NewServer(cfg *config.Config, be backend.Backend, database *db.DB) *Server {
	return &Server{
		Config:   cfg,
		Backend:  be,
		DB:       database,
		Workers:  NewMemoryWorkerMap(),
		Sessions: session.NewMemoryStore(),
		Registry: registry.NewMemoryRegistry(),
		Tasks:    task.NewStore(),
		LogStore: logstore.NewStore(),
		Metrics:  telemetry.NewMetrics(prometheus.NewRegistry()),
	}
}

// NewServerWithDefaultMetrics is the production constructor. It creates
// a Server whose metrics are registered with [prometheus.DefaultRegisterer]
// so the promhttp /metrics endpoint can scrape them.
func NewServerWithDefaultMetrics(cfg *config.Config, be backend.Backend, database *db.DB) *Server {
	srv := NewServer(cfg, be, database)
	srv.Metrics = telemetry.NewMetrics(prometheus.DefaultRegisterer)
	return srv
}

// AuthDeps returns an auth.Deps populated from this server's fields.
// Used by the router to wire auth handlers and middleware without a
// circular import.
func (srv *Server) AuthDeps() *auth.Deps {
	return &auth.Deps{
		Config:       srv.Config,
		OIDCClient:   srv.OIDCClient,
		SigningKey:    srv.SigningKey,
		UserSessions: srv.UserSessions,
		AuditLog:     srv.AuditLog,
		DB:           srv.DB,
	}
}

// ActiveWorker represents a running worker tracked by the server.
// The worker ID is the map key in WorkerMap, not stored here.
type ActiveWorker struct {
	AppID     string
	BundleID  string    // bundle active at spawn time; runtime installs resolve against this
	Draining  bool      // set by graceful drain; no new sessions routed
	IdleSince time.Time // zero value = not idle; set when session count hits 0
	StartedAt time.Time // when the worker was spawned
}
