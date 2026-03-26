package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
)

func TestWorkerMapCountForApp(t *testing.T) {
	m := NewWorkerMap()
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
	m := NewWorkerMap()
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
	m := NewWorkerMap()

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
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-b"})

	all := m.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(all))
	}
}

func TestIdleWorkersScaleToZero(t *testing.T) {
	m := NewWorkerMap()
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
	m := NewWorkerMap()
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
	m := NewWorkerMap()
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
	m := NewWorkerMap()
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
	wm := NewWorkerMap()
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
	wm := NewWorkerMap()
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
			Openbao: &config.OpenbaoConfig{
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

func TestWorkerEnv_WithBoardStorage(t *testing.T) {
	srv := &Server{
		Config: &config.Config{
			Server: config.ServerConfig{Bind: ":8080"},
			BoardStorage: &config.BoardStorageConfig{
				PostgrestURL: "http://postgrest:3000",
			},
		},
	}
	env := WorkerEnv(srv)
	if env["POSTGREST_URL"] != "http://postgrest:3000" {
		t.Errorf("POSTGREST_URL = %q, want %q", env["POSTGREST_URL"], "http://postgrest:3000")
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
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-b"})
	m.Set("w3", ActiveWorker{AppID: "app-a"})

	ids := m.AppIDs()
	if len(ids) != 2 {
		t.Errorf("expected 2 distinct app IDs, got %d", len(ids))
	}
}

func TestForAppAvailable(t *testing.T) {
	m := NewWorkerMap()
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
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})

	ts := time.Now().Add(-5 * time.Minute)
	m.SetIdleSince("w1", ts)

	w, _ := m.Get("w1")
	if !w.IdleSince.Equal(ts) {
		t.Errorf("IdleSince = %v, want %v", w.IdleSince, ts)
	}
}

func TestSetIdleSinceIfZero(t *testing.T) {
	m := NewWorkerMap()
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

func TestMarkDraining(t *testing.T) {
	m := NewWorkerMap()
	m.Set("w1", ActiveWorker{AppID: "app-a"})
	m.Set("w2", ActiveWorker{AppID: "app-a"})

	m.MarkDraining("app-a")

	w1, _ := m.Get("w1")
	w2, _ := m.Get("w2")
	if !w1.Draining || !w2.Draining {
		t.Error("expected all workers for app to be draining after MarkDraining")
	}
}
