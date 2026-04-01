package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"syscall"

	"github.com/cynkra/blockyard/internal/api"
	_ "github.com/cynkra/blockyard/internal/api/docs"
	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/buildercache"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/pkgstore"
	"github.com/cynkra/blockyard/internal/preflight"
	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
	"github.com/cynkra/blockyard/internal/update"
)

var version = "dev"

func main() {
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

	// Reconfigure log level from config (server.log_level / BLOCKYARD_SERVER_LOG_LEVEL).
	logLevel := config.ParseLogLevel(cfg.Server.LogLevel)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	})))
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

	// Initialize backend
	be, err := docker.New(context.Background(), &cfg.Docker, cfg.Storage.BundleServerPath)
	if err != nil {
		slog.Error("failed to create docker backend", "error", err)
		os.Exit(1)
	}

	// Initialize database
	database, err := db.Open(cfg.Database)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	// Build shared state and router
	srv := server.NewServer(cfg, be, database)
	srv.Version = version

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

	// ── Preflight: Docker-dependent checks ──
	if !cfg.Server.SkipDockerPreflight {
		builderBin, builderErr := buildercache.EnsureCached(
			filepath.Join(cfg.Storage.BundleServerPath, ".by-builder-cache"), version)
		if builderErr != nil {
			slog.Warn("preflight: could not cache by-builder, skipping builder check",
				"error", builderErr)
			builderBin = ""
		}

		preflightCtx, preflightCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		dockerReport := preflight.RunDockerChecks(preflightCtx, preflight.DockerDeps{
			Client:     be.Client(),
			ServerID:   be.ServerID(),
			MountCfg:   be.MountCfg(),
			Config:     &cfg.Docker,
			StorePath:  storePath,
			BuilderBin: builderBin,
		})
		preflightCancel()
		dockerReport.Log()
		if dockerReport.HasErrors() {
			slog.Error("preflight Docker checks failed")
			os.Exit(1)
		}
	}

	// Generate ephemeral HMAC key for worker tokens.
	workerKeyBytes := make([]byte, 32)
	if _, err := rand.Read(workerKeyBytes); err != nil {
		slog.Error("failed to generate worker token key", "error", err)
		os.Exit(1)
	}
	srv.WorkerTokenKey = auth.NewSigningKey(workerKeyBytes)

	// Set operation hooks to avoid import cycles.
	srv.EvictWorkerFn = ops.EvictWorker
	srv.SpawnLogCaptureFn = ops.SpawnLogCapture

	// Background goroutine context — used for vault token renewal and others.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	var bgWg sync.WaitGroup

	// ── Initialize OpenBao (must happen before OIDC for vault reference resolution) ──

	if cfg.Openbao != nil {
		tokenFilePath := filepath.Join(filepath.Dir(cfg.Database.Path), ".vault-token")

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
		srv.AuditLog = audit.New(cfg.Audit.Path)
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

	// Startup cleanup — must complete before accepting traffic.
	if err := ops.StartupCleanup(context.Background(), srv); err != nil {
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

	handler := api.NewRouter(srv)

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

	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		update.SpawnChecker(bgCtx, version, srv)
	}()

	// Store eviction sweeper.
	if cfg.Docker.StoreRetention.Duration > 0 {
		pkgstore.SpawnEvictionSweeper(bgCtx, srv.PkgStore, cfg.Docker.StoreRetention.Duration)
	}

	// Refresh scheduler.
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		srv.RunRefreshScheduler(bgCtx)
	}()

	// Start audit log background writer.
	if srv.AuditLog != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			srv.AuditLog.Run(bgCtx, cfg.Audit.Path)
		}()
	}

	// Graceful shutdown on SIGTERM / SIGINT
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer stop()

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

	<-ctx.Done()
	slog.Info("shutdown signal received")

	// 1. Drain management listener first (health probes fail, LB stops
	//    sending traffic), then drain the main listener.
	shutdownCtx, cancel := context.WithTimeout(context.Background(),
		cfg.Server.ShutdownTimeout.Duration)
	defer cancel()

	if mgmtServer != nil {
		if err := mgmtServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("management server shutdown error", "error", err)
		}
	}

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}

	// 2. Cancel background goroutines and wait
	bgCancel()
	bgWg.Wait()

	// 3. Stop all workers and clean up
	ops.GracefulShutdown(context.Background(), srv)

	// 4. Flush tracing spans
	if tracingShutdown != nil {
		tracingShutdown(context.Background()) //nolint:errcheck // best-effort flush during shutdown
	}

	slog.Info("shutdown complete")
}

