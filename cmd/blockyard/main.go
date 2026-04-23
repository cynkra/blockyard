package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cynkra/blockyard/internal/api"
	_ "github.com/cynkra/blockyard/internal/api/docs"
	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/drain"
	"github.com/cynkra/blockyard/internal/errorlog"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/preflight"
	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/redisstate"
	"github.com/cynkra/blockyard/internal/registry"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/session"
	"github.com/cynkra/blockyard/internal/telemetry"
	"github.com/cynkra/blockyard/internal/update"
)

var version = "dev"

// runProbe is the `blockyard probe --tcp host:port --timeout dur` mode
// used by the process backend's worker-egress preflight check. Spawned
// inside a bwrap sandbox configured exactly like a real worker; exits
// 0 on TCP connect success, 1 on failure. Uses a fresh FlagSet so the
// probe-specific flags don't clash with main's `-config`/`-version`.
func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	addr := fs.String("tcp", "", "host:port to TCP-connect")
	timeout := fs.Duration("timeout", 2*time.Second, "connect timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" {
		return fmt.Errorf("probe: --tcp host:port is required")
	}
	conn, err := net.DialTimeout("tcp", *addr, *timeout)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func main() {
	// `blockyard probe ...` short-circuits before flag.Parse() so its
	// own FlagSet handles the args. Used by process.checkWorkerEgress.
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		if err := runProbe(os.Args[2:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	configPath := flag.String("config", "blockyard.toml", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	// Process orchestrator re-execs blockyard with the same config file
	// during rolling updates; store the path so clone_process.go can
	// reach it without threading a second argument through the
	// orchestrator factory.
	cfg.ConfigPath = *configPath

	// Reconfigure log level from config (server.log_level / BLOCKYARD_SERVER_LOG_LEVEL).
	// Compose the JSON stderr handler with an errorlog capture handler so
	// WARN+ records flow into the in-memory ring buffer that backs the
	// admin UI "recent errors" panel. Stderr output is unchanged.
	logLevel := config.ParseLogLevel(cfg.Server.LogLevel)
	errLog := errorlog.NewStore(errorlog.DefaultCapacity)
	slog.SetDefault(slog.New(errorlog.NewHandler(
		slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}),
		errLog,
		slog.LevelWarn,
	)))
	logAttrs := []any{"bind", cfg.Server.Bind, "log_level", logLevel.String()}
	if cfg.Server.ManagementBind != "" {
		logAttrs = append(logAttrs, "management_bind", cfg.Server.ManagementBind)
	}
	slog.Info("loaded config", logAttrs...)

	// ── Preflight: config-only checks ──
	configReport := preflight.RunConfigChecks(cfg)
	configReport.Log()
	if configReport.HasErrors() {
		slog.Error("preflight config checks failed")
		os.Exit(1)
	}

	// ── Redis shared state (optional) ──
	//
	// Redis init happens BEFORE backend construction so the process
	// backend can share main.go's redisstate.Client for its Redis-
	// backed port and UID allocators. The Docker backend ignores the
	// client (it only reads cfg.Redis.URL as a string for its
	// preflight check), so the reorder is safe for both variants.
	//
	// A single connection pool also means one `defer rc.Close()` on
	// shutdown covers both the server's and the backend's usage, so
	// no additional Close() is needed on the Backend interface.
	var rc *redisstate.Client
	if cfg.Redis != nil {
		var err error
		rc, err = redisstate.New(context.Background(), cfg.Redis)
		if err != nil {
			slog.Error("failed to connect to redis", "error", err)
			os.Exit(1)
		}
		defer rc.Close()
	}

	// Database init happens BEFORE backend construction for the same
	// reason as Redis above: the process backend's Postgres-primary
	// port / UID allocators (#288) need the *sqlx.DB at construction
	// time. The Docker backend factory ignores the database argument.
	database, err := db.Open(cfg.Database)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}

	// Initialize backend via the tag-gated factory map.
	factory, ok := backendFactories[cfg.Server.Backend]
	if !ok {
		slog.Error("backend not available in this build",
			"backend", cfg.Server.Backend,
			"available", availableBackends())
		os.Exit(1)
	}
	be, err := factory(context.Background(), cfg, rc, database.DB, version)
	if err != nil {
		slog.Error("failed to create backend",
			"backend", cfg.Server.Backend, "error", err)
		os.Exit(1)
	}
	// Build shared state and router. Use NewServerWithDefaultMetrics so
	// prometheus counters are registered with DefaultRegisterer and the
	// /metrics HTTP endpoint (served by promhttp.Handler) can scrape them.
	srv := server.NewServerWithDefaultMetrics(cfg, be, database)
	srv.Version = version
	// Point the server at the same ErrorLog store the slog handler has
	// been feeding since config load, so the admin panel sees startup
	// warnings too (e.g. preflight errors).
	srv.ErrorLog = errLog

	// Initialize package store.
	storePath := filepath.Join(cfg.Storage.BundleServerPath, ".pkg-store")
	if err := os.MkdirAll(storePath, 0o755); err != nil { //nolint:gosec // G301: package store dir, not secrets
		slog.Error("failed to create package store", "error", err)
		os.Exit(1)
	}
	srv.PkgStore = pkgstore.NewStore(storePath)
	if platform := pkgstore.RecoverPlatform(storePath); platform != "" {
		srv.PkgStore.SetPlatform(platform)
	}

	// ── Preflight: backend-specific checks ──
	var backendReport *preflight.Report
	if !cfg.Server.SkipPreflight {
		preflightCtx, preflightCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		var pErr error
		backendReport, pErr = be.Preflight(preflightCtx)
		preflightCancel()
		if pErr != nil {
			slog.Error("preflight checks errored", "error", pErr)
			os.Exit(1)
		}
		backendReport.Log()
		if backendReport.HasErrors() {
			slog.Error("preflight backend checks failed")
			os.Exit(1)
		}
	}

	// Set operation hooks to avoid import cycles.
	srv.EvictWorkerFn = ops.EvictWorker
	srv.SpawnLogCaptureFn = ops.SpawnLogCapture

	// Background goroutine context — used for vault token renewal and others.
	bgCtx, bgCancel := context.WithCancel(context.Background()) //nolint:gosec // G118: bgCancel is called by Drainer.Finish/Shutdown, not defer
	var bgWg sync.WaitGroup

	// ── Initialize OpenBao (must happen before OIDC for vault reference resolution) ──

	if cfg.Openbao != nil {
		tokenFilePath := cfg.Openbao.TokenFile

		var adminTokenFunc func() string

		if cfg.Openbao.RoleID != "" {
			// AppRole auth flow.
			token, ttl, err := integration.InitAppRole(context.Background(), cfg.Openbao.Address, cfg.Openbao.RoleID, tokenFilePath)
			if err != nil {
				slog.Error("vault authentication failed", "error", err)
				os.Exit(1)
			}

			// Start token renewal goroutine.
			renewer := integration.NewTokenRenewer(cfg.Openbao.Address, token, tokenFilePath)
			adminTokenFunc = renewer.Token
			srv.VaultTokenHealthy = renewer.Healthy

			bgWg.Add(1)
			go func() {
				defer bgWg.Done()
				renewer.Run(bgCtx, ttl)
			}()
		} else {
			// Deprecated static admin_token.
			adminTokenFunc = cfg.Openbao.AdminToken.MustExpose
		}

		srv.VaultClient = integration.NewClient(cfg.Openbao.Address, adminTokenFunc)
		srv.VaultTokenCache = integration.NewVaultTokenCache()

		// Resolve vault references in config (e.g. "vault:path#key").
		if err := config.ResolveSecrets(context.Background(), cfg, srv.VaultClient); err != nil {
			slog.Error("failed to resolve vault references in config", "error", err)
			os.Exit(1)
		}

		// Auto-generate session_secret if empty and vault is available.
		if cfg.OIDC != nil && (cfg.Server.SessionSecret == nil || cfg.Server.SessionSecret.IsEmpty()) {
			secret, err := integration.ResolveSessionSecret(context.Background(), srv.VaultClient)
			if err != nil {
				slog.Error("failed to resolve session_secret", "error", err)
				os.Exit(1)
			}
			s := config.NewSecret(secret)
			cfg.Server.SessionSecret = &s
		}

		// Bootstrap verification.
		if err := integration.Bootstrap(context.Background(), srv.VaultClient, cfg.Openbao.JWTAuthPath, cfg.Openbao.SkipPolicyScopeCheck); err != nil {
			slog.Error("OpenBao bootstrap failed", "error", err)
			os.Exit(1)
		}
	}

	// Resolve worker signing key (vault if available, file fallback otherwise).
	workerKey, err := server.LoadOrCreateWorkerKey(context.Background(), srv.VaultClient, cfg)
	if err != nil {
		slog.Error("failed to load or create worker signing key", "error", err)
		os.Exit(1)
	}
	srv.WorkerTokenKey = workerKey

	// Per-process server ID. Phase 3-3 used bare hostname, which
	// breaks the process-backend rolling update: old and new blockyard
	// processes on the same host share a hostname, so the workermap
	// cannot distinguish their workers. A per-process 8-byte nonce
	// disambiguates concurrent peers; Docker rolling updates (where
	// each container already has a distinct hostname) keep working.
	//
	// The port/UID allocators use plain hostname as their crash-
	// recovery owner identifier — "distinguish concurrent peers" and
	// "recover my own crashed state" need different identifiers.
	hostname, _ := os.Hostname()
	serverID := hostname + "-" + randomNonceHex(8)

	// Shared-state backend selection — see #287, #286, parent #262.
	// Postgres is the source of truth; Redis is an optional read-through
	// cache so restarts don't cause session / worker loss. The same mode
	// drives all three stores (registry, worker map, session store)
	// because their durability requirements are identical — operators
	// rarely want asymmetric modes.
	mode := config.ResolveSessionStoreMode(cfg)
	registryTTL := 3 * cfg.Proxy.HealthInterval.Duration
	idleTTL := cfg.Proxy.SessionIdleTTL.Duration
	var pgSessions *session.PostgresStore
	switch mode {
	case config.SessionStoreMemory:
		srv.Registry = registry.NewMemoryRegistry()
		srv.Workers = server.NewMemoryWorkerMap()
		srv.Sessions = session.NewMemoryStore()
	case config.SessionStoreRedis:
		srv.Registry = registry.NewRedisRegistry(rc, registryTTL)
		srv.Workers = server.NewRedisWorkerMap(rc, serverID)
		srv.Sessions = session.NewRedisStore(rc, idleTTL)
	case config.SessionStorePostgres:
		srv.Registry = registry.NewPostgresRegistry(database.DB, registryTTL)
		srv.Workers = server.NewPostgresWorkerMap(database.DB, serverID)
		pgSessions = session.NewPostgresStore(database.DB, idleTTL)
		srv.Sessions = pgSessions
	case config.SessionStoreLayered:
		srv.Registry = registry.NewLayeredRegistry(
			registry.NewPostgresRegistry(database.DB, registryTTL),
			registry.NewRedisRegistry(rc, registryTTL),
		)
		srv.Workers = server.NewLayeredWorkerMap(
			server.NewPostgresWorkerMap(database.DB, serverID),
			server.NewRedisWorkerMap(rc, serverID),
		)
		pgSessions = session.NewPostgresStore(database.DB, idleTTL)
		srv.Sessions = session.NewLayeredStore(
			pgSessions, session.NewRedisStore(rc, idleTTL),
		)
	}
	if rc != nil {
		srv.RedisClient = rc
		slog.Info("using redis for shared state",
			"url", maskRedisPassword(cfg.Redis.URL),
			"prefix", cfg.Redis.KeyPrefix,
			"server_id", serverID)
	}
	if pgSessions != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			pgSessions.RunExpiry(bgCtx, time.Minute)
		}()
	}
	slog.Info("shared state store selected", "mode", mode)

	// Deferred validation: session_secret must be present if OIDC is configured.
	if cfg.OIDC != nil {
		if cfg.Server.SessionSecret == nil || cfg.Server.SessionSecret.IsEmpty() {
			slog.Error("config: server.session_secret is required when [oidc] is configured")
			os.Exit(1)
		}
	}

	// Initialize OIDC if configured.
	if cfg.OIDC != nil {
		baseURL := cfg.Server.ExternalURL
		if baseURL == "" {
			baseURL = "http://" + cfg.Server.Bind
		}
		redirectURL := baseURL + "/callback"

		clientSecret, err := cfg.OIDC.ClientSecret.Expose()
		if err != nil {
			slog.Error("failed to read OIDC client secret", "error", err)
			os.Exit(1)
		}

		oidcClient, err := auth.Discover(
			context.Background(),
			cfg.OIDC.IssuerURL,
			cfg.OIDC.IssuerDiscoveryURL,
			cfg.OIDC.ClientID,
			clientSecret,
			redirectURL,
		)
		if err != nil {
			slog.Error("OIDC discovery failed", "error", err)
			os.Exit(1)
		}

		srv.OIDCClient = oidcClient
		sessionSecret, err := cfg.Server.SessionSecret.Expose()
		if err != nil {
			slog.Error("failed to read session secret", "error", err)
			os.Exit(1)
		}
		srv.SigningKey = auth.DeriveSigningKey(sessionSecret)
		srv.SessionTokenKey = auth.DeriveSessionTokenKey(sessionSecret)
		srv.UserSessions = auth.NewUserSessionStore()
	}

	// Initialize audit log if configured.
	if cfg.Audit != nil {
		srv.AuditLog = audit.New(cfg.Audit.Path, srv.Metrics)
	}

	// Initialize OpenTelemetry tracing if configured.
	var tracingShutdown func(context.Context) error
	if cfg.Telemetry != nil && cfg.Telemetry.OTLPEndpoint != "" {
		shutdown, err := telemetry.InitTracing(context.Background(), cfg.Telemetry.OTLPEndpoint)
		if err != nil {
			slog.Error("failed to init tracing", "error", err)
			os.Exit(1)
		}
		tracingShutdown = shutdown
	}

	// Passive mode — set by `by admin update` when starting a new server
	// alongside the old one during a rolling update.
	passive := os.Getenv("BLOCKYARD_PASSIVE") == "1"
	if passive && (cfg.Redis == nil || cfg.Redis.URL == "") {
		slog.Error("BLOCKYARD_PASSIVE=1 requires [redis] to be configured")
		os.Exit(1)
	}
	if passive {
		slog.Info("starting in passive mode (background goroutines deferred)")
	}

	// Startup cleanup — must complete before accepting traffic.
	if err := ops.StartupCleanup(context.Background(), srv, passive); err != nil {
		slog.Error("startup cleanup failed", "error", err)
		os.Exit(1)
	}

	// Bootstrap token — a one-time token that can be exchanged for a real
	// PAT via POST /api/v1/bootstrap. The token itself never grants API
	// access; it can only be traded once for a properly scoped PAT.
	if token := cfg.Server.BootstrapToken; token != "" {
		if cfg.OIDC == nil || cfg.OIDC.InitialAdmin == "" {
			slog.Error("bootstrap_token requires oidc.initial_admin to be set")
			os.Exit(1)
		}
		hash := auth.HashPAT(token)
		if database.PATHashExists(hash) {
			slog.Info("bootstrap token already redeemed")
			// Ensure the sentinel is marked revoked (back-compat for
			// deployments that created it before we set revoked = 1).
			database.RevokePAT("bootstrap-redeemed", cfg.OIDC.InitialAdmin) //nolint:errcheck
		} else {
			srv.BootstrapTokenHash = hash
			slog.Warn("bootstrap token active — exchange via POST /api/v1/bootstrap")
		}
	}

	// Clean up orphaned worker library directories from previous runs.
	if srv.PkgStore != nil {
		workersDir := filepath.Join(srv.PkgStore.Root(), ".workers")
		entries, _ := os.ReadDir(workersDir)
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if _, found := srv.Workers.Get(e.Name()); !found {
				_ = os.RemoveAll(filepath.Join(workersDir, e.Name()))
			}
		}
	}

	// Extract background goroutine spawning into a function so it can be
	// called directly (active mode) or deferred (passive mode, triggered
	// by POST /api/v1/admin/activate).
	startBG := func() {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			ops.SpawnHealthPoller(bgCtx, srv)
		}()

		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			ops.SpawnLogRetentionCleaner(bgCtx, srv)
		}()

		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			proxy.RunAutoscaler(bgCtx, srv)
		}()

		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			ops.SpawnSoftDeleteSweeper(bgCtx, srv)
		}()

		if cfg.Update != nil && cfg.Update.Repo != "" {
			update.SetRepo(cfg.Update.Repo)
		}
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			update.SpawnChecker(bgCtx, version, srv)
		}()

		// Store eviction sweeper.
		if cfg.Storage.StoreRetention.Duration > 0 {
			pkgstore.SpawnEvictionSweeper(bgCtx, srv.PkgStore, cfg.Storage.StoreRetention.Duration)
		}

		// Refresh scheduler.
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			srv.RunRefreshScheduler(bgCtx)
		}()
	}

	// Audit log writer runs unconditionally — even in passive mode the
	// server serves requests that produce audit entries. Without the
	// writer draining the buffered channel, it fills and blocks request
	// goroutines.
	if srv.AuditLog != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			srv.AuditLog.Run(bgCtx, cfg.Audit.Path)
		}()
	}

	if !passive {
		startBG()
	} else {
		srv.Passive.Store(true)
	}

	// ── System checker (re-runnable checks + cached startup results) ──
	checkerDeps := preflight.RuntimeDeps{
		StorePath:     storePath,
		ServerVersion: version,
		DBPing:        database.Ping,
		BackendPing: func(ctx context.Context) error {
			_, err := be.ListManaged(ctx)
			return err
		},
		UpdateAvailable: srv.UpdateAvailableVersion,
	}
	if srv.Config.OIDC != nil {
		checkerDeps.IDPCheck = func(ctx context.Context) error {
			return api.CheckIDP(ctx, srv)
		}
	}
	if srv.VaultClient != nil {
		checkerDeps.VaultCheck = func(ctx context.Context) error {
			return srv.VaultClient.Health(ctx)
		}
	}
	if srv.RedisClient != nil {
		checkerDeps.RedisPing = srv.RedisClient.Ping
	}
	if srv.VaultTokenHealthy != nil {
		checkerDeps.VaultTokenOK = srv.VaultTokenHealthy
	}
	srv.Checker = preflight.NewChecker(checkerDeps)
	srv.Checker.Init(context.Background(), configReport, backendReport)

	// Late-binding drain closures — drainer is nil here, assigned below.
	var drainer *drain.Drainer

	drainFn := func() { drainer.Drain() }
	undrainFn := func() { drainer.Undrain() }

	// Exit signal — the scheduled updater (a bgWg goroutine) cannot
	// call Finish directly (deadlock), so both the API handler
	// goroutine and RunScheduled use this channel to wake main.
	doneCh := make(chan struct{}, 1)
	exitFn := func() {
		select {
		case doneCh <- struct{}{}:
		default:
		}
	}

	// Orchestrator — resolved via the tag-gated factory candidates.
	// Returns nil in containerized mode (PID 1) or when no candidate
	// matches the active backend, in which case /api/v1/admin/update
	// and /rollback return 501.
	var orch *orchestrator.Orchestrator
	if fac := newServerFactory(srv, cfg, be); fac != nil {
		orch = orchestrator.New(
			fac, srv.DB, cfg, srv.Version,
			srv.Tasks, &orchestrator.DefaultChecker{}, slog.Default(),
			drainFn, undrainFn, exitFn,
		)
	}

	handler := api.NewRouter(srv, startBG, orch, bgCtx)

	httpServer := &http.Server{
		Addr:              cfg.Server.Bind,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Management listener (optional, for /healthz, /readyz, /metrics).
	var mgmtServer *http.Server
	if cfg.Server.ManagementBind != "" {
		mgmtHandler := api.NewManagementRouter(srv)
		mgmtServer = &http.Server{
			Addr:              cfg.Server.ManagementBind,
			Handler:           mgmtHandler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
	}

	// Now assign the drainer — closures become safe to call.
	// FinishIdleWait is set to cfg.Update.DrainIdleWait for the
	// process backend (so Finish waits for local sessions to end
	// before tearing down workers), zero for Docker (which cuts over
	// hard and relies on the reverse proxy to drain the last
	// requests).
	drainer = &drain.Drainer{
		Srv:             srv,
		MainServer:      httpServer,
		MgmtServer:      mgmtServer,
		BGCancel:        bgCancel,
		BGWait:          &bgWg,
		TracingShutdown: tracingShutdown,
		FinishIdleWait:  finishIdleWaitForBackend(be, cfg),
		ServerID:        serverID,
	}

	// Scheduled auto-updates (not in passive mode — prevents the
	// newly deployed replacement from immediately trying to update).
	if !passive && orch != nil && cfg.Update != nil && cfg.Update.Schedule != "" {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			orch.RunScheduled(bgCtx, cfg.Update.Schedule, cfg.Update.Channel)
		}()
	}

	// Set up signal channels.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

	// forceExitOnSecondSignal spawns a goroutine that force-exits if a
	// second signal arrives during graceful drain/shutdown.
	forceExitOnSecondSignal := func() {
		go func() {
			s := <-sigCh
			slog.Warn("second signal received, forcing exit", "signal", s)
			os.Exit(1)
		}()
	}

	go func() {
		slog.Info("server listening", "bind", cfg.Server.Bind)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	if mgmtServer != nil {
		go func() {
			slog.Info("management listener started", "bind", cfg.Server.ManagementBind)
			if err := mgmtServer.ListenAndServe(); err != http.ErrServerClosed {
				slog.Error("management server error", "error", err)
				os.Exit(1)
			}
		}()
	}

	// Wait for signal or update-complete notification.
	for {
		select {
		case sig := <-sigCh:
			// SIGUSR1 during an active orchestrator operation must be
			// ignored — the orchestrator owns the drain/exit lifecycle
			// and Finish() would shut down the HTTP server it's using
			// to communicate with the new instance.
			if sig == syscall.SIGUSR1 && orch != nil && orch.State() != "idle" {
				slog.Warn("SIGUSR1 ignored: orchestrator operation in progress",
					"state", orch.State())
				continue
			}
			forceExitOnSecondSignal()
			switch sig {
			case syscall.SIGUSR1:
				drainer.Drain()
				drainer.Finish(cfg.Server.DrainTimeout.Duration)
			default:
				// SIGTERM, SIGINT → full shutdown.
				drainer.Shutdown(cfg.Server.ShutdownTimeout.Duration)
			}
			return
		case <-doneCh:
			drainer.Finish(cfg.Server.DrainTimeout.Duration)
			return
		}
	}
}

// randomNonceHex returns n random bytes as a hex string. Used for the
// per-process server ID suffix. Falls back to a deterministic value
// on crypto/rand failure; collision risk on fallback is accepted
// because rand.Read failure is effectively impossible on Linux.
func randomNonceHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b)
}

// maskRedisPassword replaces the password in a Redis URL with "***".
func maskRedisPassword(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if _, hasPwd := u.User.Password(); hasPwd {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

// finishIdleWaitForBackend returns the Drainer.FinishIdleWait value
// for the given resolved backend and config. The process backend
// needs a non-zero idle-wait because its workers are killed by
// Pdeathsig when the old server exits, and sessions on those workers
// end abruptly unless the idle-wait lets them finish first. Docker
// returns zero — it cuts over hard and relies on the reverse proxy
// to drain in-flight requests.
//
// This is the one place in main.go that branches on the concrete
// backend type; pushing the choice down into Drainer call sites
// would scatter variant-awareness across the codebase, and pushing
// it up into the orchestrator factory would create an orchestrator→
// drain cross-package dependency that otherwise doesn't exist.
func finishIdleWaitForBackend(be backend.Backend, cfg *config.Config) time.Duration {
	// Type assertion lives in backend_process.go (tag-gated) so this
	// file stays compilable in the docker-only variant. Variant that
	// doesn't include process_backend returns zero unconditionally.
	if dur, ok := finishIdleWaitForProcess(be, cfg); ok {
		return dur
	}
	return 0
}
