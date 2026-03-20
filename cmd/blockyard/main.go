package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

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

	// Background goroutine context — used for vault token renewal and others.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	var bgWg sync.WaitGroup

	// ── Initialize OpenBao (must happen before OIDC for vault reference resolution) ──

	if cfg.Openbao != nil {
		tokenFilePath := filepath.Join(filepath.Dir(cfg.Database.Path), ".vault-token")

		var adminTokenFunc func() string

		if cfg.Openbao.RoleID != "" {
			// AppRole auth flow.
			token, ttl, err := initVaultAppRole(cfg.Openbao.Address, cfg.Openbao.RoleID, tokenFilePath)
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
			secret, err := resolveSessionSecret(srv.VaultClient)
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
		tracingShutdown(context.Background()) //nolint:errcheck // best-effort flush during shutdown
	}

	slog.Info("shutdown complete")
}

// initVaultAppRole authenticates to vault using AppRole. It first tries
// a persisted token (renew-self), then falls back to AppRole login with
// secret_id from the environment.
func initVaultAppRole(addr, roleID, tokenFile string) (token string, ttl time.Duration, err error) {
	httpClient := &http.Client{}

	// 1. Try persisted token.
	persisted, err := integration.ReadTokenFile(tokenFile)
	if err != nil {
		slog.Warn("failed to read persisted vault token", "error", err)
	}
	if persisted != "" {
		renewTTL, err := integration.RenewSelf(context.Background(), httpClient, addr, persisted)
		if err == nil {
			slog.Info("reusing persisted vault token")
			return persisted, renewTTL, nil
		}
		slog.Warn("persisted vault token renewal failed, trying AppRole login", "error", err)
	}

	// 2. AppRole login with secret_id from env.
	secretID := os.Getenv("BLOCKYARD_OPENBAO_SECRET_ID")
	if secretID == "" {
		return "", 0, fmt.Errorf("vault bootstrap required: set BLOCKYARD_OPENBAO_SECRET_ID")
	}

	token, ttl, err = integration.AppRoleLogin(context.Background(), httpClient, addr, roleID, secretID)
	if err != nil {
		return "", 0, fmt.Errorf("AppRole login failed: %w", err)
	}

	// Persist the token for restart reuse.
	if writeErr := integration.WriteTokenFile(tokenFile, token); writeErr != nil {
		slog.Warn("failed to persist vault token", "error", writeErr)
	}

	slog.Info("vault AppRole authentication successful")
	return token, ttl, nil
}

// resolveSessionSecret reads or generates session_secret from vault.
// If the key exists at secret/data/blockyard/server-secrets, it's used.
// Otherwise, a new 32-byte random value is generated, stored, and returned.
func resolveSessionSecret(client *integration.Client) (string, error) {
	const kvPath = "blockyard/server-secrets"

	// Try reading existing.
	data, err := client.KVRead(context.Background(), kvPath, client.AdminToken())
	if err == nil {
		if v, ok := data["session_secret"]; ok {
			if s, ok := v.(string); ok && s != "" {
				slog.Info("session_secret loaded from vault")
				return s, nil
			}
		}
	}

	// Generate new.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session_secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)

	// Store in vault.
	if err := client.KVWrite(context.Background(), kvPath, map[string]any{
		"session_secret": secret,
	}); err != nil {
		return "", fmt.Errorf("store session_secret in vault: %w", err)
	}

	slog.Info("auto-generated session_secret (stored in vault)")
	return secret, nil
}
