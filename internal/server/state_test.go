package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/update"
)

func TestWorkerMapCountForApp(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-a"})
	m.Set("w3", ActiveWorker{AppID: "app-b"})

	if got := m.CountForApp("app-a"); got != 2 {
		t.Errorf("expected 2 for app-a, got %d", got)
	}
	if got := m.CountForApp("app-b"); got != 1 {
		t.Errorf("expected 1 for app-b, got %d", got)
	}
	if got := m.CountForApp("app-c"); got != 0 {
		t.Errorf("expected 0 for app-c, got %d", got)
	}
}

func TestWorkerMapForApp(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-b"})
	m.Set("w3", ActiveWorker{AppID: "app-a"})

	ids := m.ForApp("app-a")
	if len(ids) != 2 {
		t.Fatalf("expected 2 workers for app-a, got %d", len(ids))
	}

	ids = m.ForApp("app-c")
	if len(ids) != 0 {
		t.Fatalf("expected 0 workers for app-c, got %d", len(ids))
	}
}

func TestWorkerMapCRUD(t *testing.T) {
	m := NewMemoryWorkerMap()

	if m.Count() != 0 {
		t.Fatalf("expected empty map, got %d", m.Count())
	}

	m.Set("w1", ActiveWorker{AppID: "app-a"})
	w, ok := m.Get("w1")
	if !ok || w.AppID != "app-a" {
		t.Fatal("expected to get worker w1")
	}

	_, ok = m.Get("nonexistent")
	if ok {
		t.Fatal("expected false for nonexistent worker")
	}

	m.Delete("w1")
	if m.Count() != 0 {
		t.Fatalf("expected 0 after delete, got %d", m.Count())
	}
}

func TestWorkerMapAll(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-b"})

	all := m.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(all))
	}
}

func TestIdleWorkersScaleToZero(t *testing.T) {
	m := NewMemoryWorkerMap()
	// Single worker for app, idle beyond timeout — should be returned.
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		IdleSince: time.Now().Add(-10 * time.Minute),
	})

	idle := m.IdleWorkers(5 * time.Minute)
	if len(idle) != 1 {
		t.Errorf("expected 1 idle worker (scale to zero), got %d", len(idle))
	}
}

func TestIdleWorkersExcludesDraining(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		Draining:  true,
		IdleSince: time.Now().Add(-10 * time.Minute),
	})

	idle := m.IdleWorkers(5 * time.Minute)
	if len(idle) != 0 {
		t.Errorf("expected 0 idle workers (draining excluded), got %d", len(idle))
	}
}

func TestIdleWorkersExcludesNotYetIdle(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		IdleSince: time.Now().Add(-1 * time.Minute),
	})

	idle := m.IdleWorkers(5 * time.Minute)
	if len(idle) != 0 {
		t.Errorf("expected 0 idle workers (not yet idle enough), got %d", len(idle))
	}
}

func TestClearIdleSinceReturnsBool(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{
		AppID:     "app-a",
		IdleSince: time.Now().Add(-5 * time.Minute),
	})
	m.Set("w2", ActiveWorker{AppID: "app-b"}) // not idle

	// Clearing an idle worker returns true.
	if wasIdle := m.ClearIdleSince("w1"); !wasIdle {
		t.Error("expected ClearIdleSince to return true for idle worker")
	}

	// Clearing a non-idle worker returns false.
	if wasIdle := m.ClearIdleSince("w2"); wasIdle {
		t.Error("expected ClearIdleSince to return false for non-idle worker")
	}

	// Clearing nonexistent worker returns false.
	if wasIdle := m.ClearIdleSince("nonexistent"); wasIdle {
		t.Error("expected ClearIdleSince to return false for nonexistent worker")
	}

	// After clearing, the worker is no longer idle.
	if wasIdle := m.ClearIdleSince("w1"); wasIdle {
		t.Error("expected ClearIdleSince to return false after already cleared")
	}
}

func TestWorkerInstallMu(t *testing.T) {
	srv := &Server{}
	mu1 := srv.workerInstallMu("w-1")
	mu2 := srv.workerInstallMu("w-1")
	if mu1 != mu2 {
		t.Error("expected same mutex for same worker ID")
	}

	mu3 := srv.workerInstallMu("w-2")
	if mu1 == mu3 {
		t.Error("expected different mutex for different worker ID")
	}
}

func TestCleanupInstallMu(t *testing.T) {
	srv := &Server{}
	mu1 := srv.workerInstallMu("w-1")
	srv.CleanupInstallMu("w-1")

	mu2 := srv.workerInstallMu("w-1")
	if mu1 == mu2 {
		t.Error("expected new mutex after cleanup")
	}
}

func TestBundlePaths(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Storage: config.StorageConfig{
				BundleServerPath: "/data/bundles",
			},
		},
	}

	paths := srv.BundlePaths("app-1", "bundle-abc")
	if paths.Base == "" {
		t.Error("expected non-empty base path")
	}
	if !filepath.IsAbs(paths.Base) {
		t.Errorf("expected absolute path, got %q", paths.Base)
	}
}

func TestSetDraining(t *testing.T) {
	wm := NewMemoryWorkerMap()
	wm.Set("w-1", ActiveWorker{AppID: "app-1"})

	wm.SetDraining("w-1")
	w, ok := wm.Get("w-1")
	if !ok {
		t.Fatal("expected worker to exist")
	}
	if !w.Draining {
		t.Error("expected Draining=true after SetDraining")
	}
	if !wm.IsDraining("app-1") {
		t.Error("expected IsDraining=true for app-1")
	}
}

func TestSetDraining_NonExistent(t *testing.T) {
	wm := NewMemoryWorkerMap()
	// Should not panic for non-existent worker.
	wm.SetDraining("w-missing")
}

func TestCleanupTokenDir(t *testing.T) {
	dir := t.TempDir()
	workerID := "w-1"

	tokDir := filepath.Join(dir, ".worker-tokens", workerID)
	os.MkdirAll(tokDir, 0o700)
	os.WriteFile(filepath.Join(tokDir, "token"), []byte("secret"), 0o600)

	CleanupTokenDir(dir, workerID)

	if _, err := os.Stat(tokDir); !os.IsNotExist(err) {
		t.Error("expected token directory to be removed")
	}
}

func TestCleanupTokenDir_Nonexistent(t *testing.T) {
	CleanupTokenDir(t.TempDir(), "w-missing")
}

func TestWorkerEnv_BasicAPIURL(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
		},
	}
	env := WorkerEnv(srv)
	if env["BLOCKYARD_API_URL"] == "" {
		t.Error("expected BLOCKYARD_API_URL to be set")
	}
}

func TestWorkerEnv_WithOpenbao(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
			Vault: &config.VaultConfig{
				Address: "http://vault:8200",
				Services: []config.ServiceConfig{
					{ID: "mydb"},
				},
			},
		},
	}
	env := WorkerEnv(srv)
	if env["VAULT_ADDR"] != "http://vault:8200" {
		t.Errorf("VAULT_ADDR = %q, want %q", env["VAULT_ADDR"], "http://vault:8200")
	}
	if env["BLOCKYARD_VAULT_SERVICES"] == "" {
		t.Error("expected BLOCKYARD_VAULT_SERVICES to be set")
	}
}

func TestInternalAPIURL_ServiceNetwork(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":9090"},
			Docker: config.DockerConfig{ServiceNetwork: "mynet"},
		},
	}
	if got := srv.InternalAPIURL(); got != "http://blockyard:9090" {
		t.Errorf("got %q, want %q", got, "http://blockyard:9090")
	}
}

func TestInternalAPIURL_ExternalURL(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{
				Bind:        ":8080",
				ExternalURL: "https://example.com",
			},
		},
	}
	if got := srv.InternalAPIURL(); got != "https://example.com" {
		t.Errorf("got %q, want %q", got, "https://example.com")
	}
}

func TestInternalAPIURL_HostDocker(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
		},
	}
	if got := srv.InternalAPIURL(); got != "http://host.docker.internal:8080" {
		t.Errorf("got %q, want %q", got, "http://host.docker.internal:8080")
	}
}

func TestNewServer(t *testing.T) {
	be := mock.New()
	cfg := &config.Config{}
	srv := NewServer(cfg, be, nil)
	if srv.Config != cfg {
		t.Error("Config not set")
	}
	if srv.Workers == nil || srv.Sessions == nil || srv.Registry == nil || srv.Tasks == nil {
		t.Error("expected all stores initialized")
	}
}

func TestAuthDeps(t *testing.T) {
	key := auth.NewSigningKey([]byte("test-key-32-bytes-long!!!!!!!!!!"))
	srv := &Server{
		Config:    &config.Config{},
		SigningKey: key,
	}
	deps := srv.AuthDeps()
	if deps.Config != srv.Config {
		t.Error("expected Config in deps")
	}
	if deps.SigningKey != key {
		t.Error("expected SigningKey in deps")
	}
}

func TestAppIDs(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-b"})
	m.Set("w3", ActiveWorker{AppID: "app-a"})

	ids := m.AppIDs()
	if len(ids) != 2 {
		t.Errorf("expected 2 distinct app IDs, got %d", len(ids))
	}
}

func TestForAppAvailable(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-a", Draining: true})
	m.Set("w3", ActiveWorker{AppID: "app-a"})

	avail := m.ForAppAvailable("app-a")
	if len(avail) != 2 {
		t.Errorf("expected 2 available (non-draining) workers, got %d", len(avail))
	}
	for _, id := range avail {
		if id == "w2" {
			t.Error("draining worker should not be returned by ForAppAvailable")
		}
	}
}

func TestSetIdleSince(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})

	ts := time.Now().Add(-5 * time.Minute)
	m.SetIdleSince("w1", ts)

	w, _ := m.Get("w1")
	if !w.IdleSince.Equal(ts) {
		t.Errorf("IdleSince = %v, want %v", w.IdleSince, ts)
	}
}

func TestSetIdleSinceIfZero(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})

	ts1 := time.Now().Add(-5 * time.Minute)
	m.SetIdleSinceIfZero("w1", ts1)

	w, _ := m.Get("w1")
	if !w.IdleSince.Equal(ts1) {
		t.Errorf("first SetIdleSinceIfZero: IdleSince = %v, want %v", w.IdleSince, ts1)
	}

	// Second call should be a no-op.
	ts2 := time.Now()
	m.SetIdleSinceIfZero("w1", ts2)

	w, _ = m.Get("w1")
	if !w.IdleSince.Equal(ts1) {
		t.Error("second SetIdleSinceIfZero should not overwrite existing value")
	}
}

func TestSetUpdateStatus(t *testing.T) {
	srv := &Server{}
	srv.SetUpdateStatus(&update.Result{
		State:          update.StateUpdateAvailable,
		CurrentVersion: "1.0.0",
		LatestVersion:  "2.0.0",
	})
	got := srv.UpdateStatus.Load()
	if got == nil || got.LatestVersion != "2.0.0" {
		t.Errorf("expected LatestVersion=2.0.0, got %+v", got)
	}
	if srv.UpdateLastChecked.Load() == nil {
		t.Error("expected UpdateLastChecked to be stamped")
	}
}

func TestUpdateAvailableVersion(t *testing.T) {
	srv := &Server{}
	if v := srv.UpdateAvailableVersion(); v != "" {
		t.Errorf("expected empty before any check, got %q", v)
	}
	srv.SetUpdateStatus(&update.Result{State: update.StateUpToDate, LatestVersion: "1.0.0"})
	if v := srv.UpdateAvailableVersion(); v != "" {
		t.Errorf("up_to_date should not surface an available version, got %q", v)
	}
	srv.SetUpdateStatus(&update.Result{State: update.StateUpdateAvailable, LatestVersion: "2.0.0"})
	if v := srv.UpdateAvailableVersion(); v != "2.0.0" {
		t.Errorf("expected 2.0.0, got %q", v)
	}
}

func TestGetVersion(t *testing.T) {
	srv := &Server{Version: "1.5.0"}
	if got := srv.GetVersion(); got != "1.5.0" {
		t.Errorf("got %q, want %q", got, "1.5.0")
	}
}

func TestSetCancelToken(t *testing.T) {
	srv := &Server{}

	// nil cancel should be a no-op (no store).
	srv.SetCancelToken("w-1", nil)
	srv.CancelTokenRefresher("w-1") // should not panic

	// Set a real cancel function.
	called := false
	srv.SetCancelToken("w-2", func() { called = true })
	srv.CancelTokenRefresher("w-2")
	if !called {
		t.Error("expected cancel function to be called")
	}

	// Calling again should be a no-op (already deleted).
	srv.CancelTokenRefresher("w-2")
}

func TestTransferringState(t *testing.T) {
	srv := &Server{}

	if srv.IsTransferring("w-1") {
		t.Error("expected not transferring initially")
	}

	srv.SetTransferring("w-1")
	if !srv.IsTransferring("w-1") {
		t.Error("expected transferring after Set")
	}

	// Another worker should not be affected.
	if srv.IsTransferring("w-2") {
		t.Error("expected w-2 not transferring")
	}

	srv.ClearTransferring("w-1")
	if srv.IsTransferring("w-1") {
		t.Error("expected not transferring after Clear")
	}
}

func TestInternalAPIURL_ServiceNetworkNoBind(t *testing.T) {
	// Bind without port — should fall back to 8080.
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: "invalid-no-port"},
			Docker: config.DockerConfig{ServiceNetwork: "mynet"},
		},
	}
	if got := srv.InternalAPIURL(); got != "http://blockyard:8080" {
		t.Errorf("got %q, want fallback port 8080", got)
	}
}

func TestWorkerEnv_OpenbaoNoServices(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
			Vault: &config.VaultConfig{
				Address: "https://vault:8200",
			},
		},
	}
	env := WorkerEnv(srv)
	if env["VAULT_ADDR"] != "https://vault:8200" {
		t.Errorf("VAULT_ADDR = %q", env["VAULT_ADDR"])
	}
	if _, ok := env["BLOCKYARD_VAULT_SERVICES"]; ok {
		t.Error("should not set BLOCKYARD_VAULT_SERVICES when no services")
	}
}

func TestWorkerEnv_BoardStorageDBMount(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
			Database: config.DatabaseConfig{
				Driver:       "postgres",
				VaultMount:   "db-engine-42",
				BoardStorage: true,
			},
		},
	}
	env := WorkerEnv(srv)
	if env["BLOCKYARD_VAULT_DB_MOUNT"] != "db-engine-42" {
		t.Errorf("BLOCKYARD_VAULT_DB_MOUNT = %q, want %q",
			env["BLOCKYARD_VAULT_DB_MOUNT"], "db-engine-42")
	}
}

func TestWorkerEnv_BoardStorageDisabledNoDBMount(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
			Database: config.DatabaseConfig{
				Driver:     "postgres",
				VaultMount: "database",
				// BoardStorage is false
			},
		},
	}
	env := WorkerEnv(srv)
	if _, ok := env["BLOCKYARD_VAULT_DB_MOUNT"]; ok {
		t.Errorf("BLOCKYARD_VAULT_DB_MOUNT set with board_storage disabled: %q",
			env["BLOCKYARD_VAULT_DB_MOUNT"])
	}
}

func TestWorkerEnv_ShinyHostDocker(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080", Backend: "docker"},
		},
	}
	env := WorkerEnv(srv)
	if env["SHINY_HOST"] != "0.0.0.0" {
		t.Errorf("docker backend: SHINY_HOST = %q, want 0.0.0.0", env["SHINY_HOST"])
	}
}

func TestWorkerEnv_ShinyHostProcess(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080", Backend: "process"},
		},
	}
	env := WorkerEnv(srv)
	if env["SHINY_HOST"] != "127.0.0.1" {
		t.Errorf("process backend: SHINY_HOST = %q, want 127.0.0.1", env["SHINY_HOST"])
	}
}

func TestWorkerEnv_ShinyHostDefault(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
		},
	}
	env := WorkerEnv(srv)
	if env["SHINY_HOST"] != "0.0.0.0" {
		t.Errorf("default backend: SHINY_HOST = %q, want 0.0.0.0", env["SHINY_HOST"])
	}
}

func TestWorkerEnv_UserWorkerEnvPassthrough(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{
				Bind: ":8080",
				WorkerEnv: map[string]string{
					"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317",
					"TEAM":                        "data",
				},
			},
		},
	}
	env := WorkerEnv(srv)
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://alloy:4317" {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
	if env["TEAM"] != "data" {
		t.Errorf("TEAM = %q", env["TEAM"])
	}
}

func TestWorkerEnv_BlockyardKeysWinOverWorkerEnv(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{
				Bind: ":8080",
				WorkerEnv: map[string]string{
					"BLOCKYARD_API_URL": "http://evil",
					"SHINY_HOST":        "1.2.3.4",
				},
			},
		},
	}
	env := WorkerEnv(srv)
	if env["BLOCKYARD_API_URL"] == "http://evil" {
		t.Error("user worker_env must not override BLOCKYARD_API_URL")
	}
	if env["SHINY_HOST"] == "1.2.3.4" {
		t.Error("user worker_env must not override SHINY_HOST")
	}
}

func TestInjectOTELIdentity_NoEndpoint(t *testing.T) {
	env := map[string]string{"FOO": "bar"}
	injectOTELIdentity(env, "myapp", "w-123")
	if _, ok := env["OTEL_SERVICE_NAME"]; ok {
		t.Error("should not inject OTEL_SERVICE_NAME when endpoint is unset")
	}
	if _, ok := env["OTEL_RESOURCE_ATTRIBUTES"]; ok {
		t.Error("should not inject OTEL_RESOURCE_ATTRIBUTES when endpoint is unset")
	}
}

func TestInjectOTELIdentity_WithEndpoint(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317",
	}
	injectOTELIdentity(env, "myapp", "w-123")
	if env["OTEL_SERVICE_NAME"] != "myapp" {
		t.Errorf("OTEL_SERVICE_NAME = %q, want %q", env["OTEL_SERVICE_NAME"], "myapp")
	}
	want := "blockyard.app=myapp,blockyard.worker_id=w-123"
	if env["OTEL_RESOURCE_ATTRIBUTES"] != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q", env["OTEL_RESOURCE_ATTRIBUTES"], want)
	}
}

func TestInjectOTELIdentity_MergesUserResourceAttrs(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317",
		"OTEL_RESOURCE_ATTRIBUTES":    "team=data,env=prod",
	}
	injectOTELIdentity(env, "myapp", "w-123")
	want := "team=data,env=prod,blockyard.app=myapp,blockyard.worker_id=w-123"
	if env["OTEL_RESOURCE_ATTRIBUTES"] != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q", env["OTEL_RESOURCE_ATTRIBUTES"], want)
	}
}

func TestInjectOTELIdentity_UserServiceNameWins(t *testing.T) {
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317",
		"OTEL_SERVICE_NAME":           "custom-name",
	}
	injectOTELIdentity(env, "myapp", "w-123")
	if env["OTEL_SERVICE_NAME"] != "custom-name" {
		t.Errorf("user-set OTEL_SERVICE_NAME overwritten: %q", env["OTEL_SERVICE_NAME"])
	}
}

func TestMarkDraining(t *testing.T) {
	m := NewMemoryWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-a"})

	m.MarkDraining("app-a")

	w1, _ := m.Get("w1")
	w2, _ := m.Get("w2")
	if !w1.Draining || !w2.Draining {
		t.Error("expected all workers for app to be draining after MarkDraining")
	}
}
