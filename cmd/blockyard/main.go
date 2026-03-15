package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/cynkra/blockyard/internal/api"
	"github.com/cynkra/blockyard/internal/audit"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/integration"
	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/telemetry"
)

func main() {
	configPath := flag.String("config", "blockyard.toml", "path to config file")
	flag.Parse()

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

	// Initialize backend
	be, err := docker.New(context.Background(), &cfg.Docker, cfg.Storage.BundleServerPath)
	if err != nil {
		slog.Error("failed to create docker backend", "error", err)
		os.Exit(1)
	}

	// Initialize database
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	// Build shared state and router
	srv := server.NewServer(cfg, be, database)

	// Initialize OIDC if configured.
	if cfg.OIDC != nil {
		baseURL := cfg.Server.ExternalURL
		if baseURL == "" {
			baseURL = "http://" + cfg.Server.Bind
		}
		redirectURL := baseURL + "/callback"

		oidcClient, err := auth.Discover(
			context.Background(),
			cfg.OIDC.IssuerURL,
			cfg.OIDC.ClientID,
			cfg.OIDC.ClientSecret.Expose(),
			redirectURL,
		)
		if err != nil {
			slog.Error("OIDC discovery failed", "error", err)
			os.Exit(1)
		}

		srv.OIDCClient = oidcClient
		srv.SigningKey = auth.DeriveSigningKey(cfg.Server.SessionSecret.Expose())
		srv.SessionTokenKey = auth.DeriveSessionTokenKey(cfg.Server.SessionSecret.Expose())
		srv.UserSessions = auth.NewUserSessionStore()

	}

	// Initialize OpenBao if configured.
	if cfg.Openbao != nil {
		srv.VaultClient = integration.NewClient(
			cfg.Openbao.Address,
			cfg.Openbao.AdminToken.Expose,
		)
		srv.VaultTokenCache = integration.NewVaultTokenCache()

		if err := integration.Bootstrap(context.Background(), srv.VaultClient, cfg.Openbao.JWTAuthPath, cfg.Openbao.SkipPolicyScopeCheck); err != nil {
			slog.Error("OpenBao bootstrap failed", "error", err)
			os.Exit(1)
		}
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

	handler := api.NewRouter(srv)

	httpServer := &http.Server{
		Addr:    cfg.Server.Bind,
		Handler: handler,
	}

	// Management listener (optional, for /healthz, /readyz, /metrics).
	var mgmtServer *http.Server
	if cfg.Server.ManagementBind != "" {
		mgmtHandler := api.NewManagementRouter(srv)
		mgmtServer = &http.Server{
			Addr:    cfg.Server.ManagementBind,
			Handler: mgmtHandler,
		}
	}

	// Background goroutine context
	bgCtx, bgCancel := context.WithCancel(context.Background())
	var bgWg sync.WaitGroup

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
		tracingShutdown(context.Background())
	}

	slog.Info("shutdown complete")
}
