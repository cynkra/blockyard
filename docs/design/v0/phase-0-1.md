# Phase 0-1: Foundation

Establish the Go module, core types, config parsing, database schema, and
shared server state. Everything else builds on this. This phase produces a
module that compiles and passes tests but does not start a server or talk to
Docker.

## Deliverables

1. Go module with `cmd/blockyard/main.go` + `internal/` package structure
2. Config parsing (`internal/config/`) — TOML + env var overlay + validation
3. `Backend` interface + `WorkerSpec` + `BuildSpec` + supporting types
4. Mock backend implementation (`internal/backend/mock/`)
5. SQLite schema (`internal/db/`)
6. In-memory stores: `session.Store`, `registry.Registry`, `logstore.Store`, `task.Store`
7. `Server` struct that holds shared server state (`internal/server/`)
8. Structured logging setup (`log/slog`)
9. GitHub Actions CI workflow

## Step-by-step

### Step 1: Go module and project skeleton

Initialize the module and create the directory structure. No build tags
needed yet — the mock backend is only imported from `_test.go` files, and
Go's toolchain excludes test-only imports from production builds.

```
go mod init github.com/cynkra/blockyard
```

Directory layout for this phase:

```
blockyard/
├── go.mod
├── cmd/
│   └── blockyard/
│       └── main.go                # config loading, slog setup, stub
├── internal/
│   ├── config/
│   │   └── config.go              # TOML parsing + env var overlay + validation
│   ├── backend/
│   │   ├── backend.go             # Backend interface, WorkerSpec, BuildSpec
│   │   └── mock/
│   │       └── mock.go            # MockBackend (imported only from _test.go)
│   ├── db/
│   │   └── db.go                  # SQLite setup, schema, queries
│   ├── session/
│   │   └── store.go               # session ID → worker ID mapping
│   ├── registry/
│   │   └── registry.go            # worker ID → "host:port" mapping
│   ├── logstore/
│   │   └── store.go               # per-worker log buffer + broadcast
│   ├── task/
│   │   └── store.go               # async task tracking + log streaming
│   └── server/
│       └── state.go               # Server struct: shared server state
├── blockyard.toml                 # reference config
└── .github/
    └── workflows/
        └── ci.yml
```

`cmd/blockyard/main.go` — entry point stub:

```go
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/cynkra/blockyard/internal/config"
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

	slog.Info("loaded config", "bind", cfg.Server.Bind)

	// Server wiring comes in later phases.
}
```

Initial dependencies (`go.mod`):

```
go 1.24

require (
    github.com/BurntSushi/toml v1.4.0
    github.com/coder/websocket v1.8.12
    github.com/docker/docker v27.5.1+incompatible
    github.com/go-chi/chi/v5 v5.2.1
    github.com/google/uuid v1.6.0
    modernc.org/sqlite v1.34.5
)
```

Dependencies used in later phases (chi, websocket, docker) are listed in
`go.mod` from the start. Unlike Rust, Go does not warn on declared-but-
unused module dependencies — only unused *imports* in source files are
errors. Declaring them upfront avoids repeated `go get` churn across phases.

### Step 2: Config parsing

`internal/config/config.go` — TOML deserialization with env var overlay
and startup validation.

```go
package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server   ServerConfig   `toml:"server"`
	Docker   DockerConfig   `toml:"docker"`
	Storage  StorageConfig  `toml:"storage"`
	Database DatabaseConfig `toml:"database"`
	Proxy    ProxyConfig    `toml:"proxy"`
}

type ServerConfig struct {
	Bind            string   `toml:"bind"`
	Token           string   `toml:"token"`
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
}

type DockerConfig struct {
	Socket    string `toml:"socket"`
	Image     string `toml:"image"`
	ShinyPort int    `toml:"shiny_port"`
	RvVersion string `toml:"rv_version"`
}

type StorageConfig struct {
	BundleServerPath string `toml:"bundle_server_path"`
	BundleWorkerPath string `toml:"bundle_worker_path"`
	BundleRetention  int    `toml:"bundle_retention"`
	MaxBundleSize    int64  `toml:"max_bundle_size"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type ProxyConfig struct {
	WsCacheTTL         Duration `toml:"ws_cache_ttl"`
	HealthInterval     Duration `toml:"health_interval"`
	WorkerStartTimeout Duration `toml:"worker_start_timeout"`
	MaxWorkers         int      `toml:"max_workers"`
	LogRetention       Duration `toml:"log_retention"`
}
```

**Duration type:** TOML does not have a native duration type. The config
uses human-readable strings (`"30s"`, `"1h"`). A custom `Duration` type
wraps `time.Duration` with TOML and string unmarshalling:

```go
// Duration wraps time.Duration for TOML deserialization from strings
// like "30s", "5m", "1h".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}
```

**Defaults:** applied after TOML parsing by filling zero-valued fields:

```go
func applyDefaults(cfg *Config) {
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "0.0.0.0:8080"
	}
	if cfg.Server.ShutdownTimeout.Duration == 0 {
		cfg.Server.ShutdownTimeout.Duration = 30 * time.Second
	}
	if cfg.Docker.Socket == "" {
		cfg.Docker.Socket = "/var/run/docker.sock"
	}
	if cfg.Docker.ShinyPort == 0 {
		cfg.Docker.ShinyPort = 3838
	}
	if cfg.Docker.RvVersion == "" {
		cfg.Docker.RvVersion = "latest"
	}
	if cfg.Storage.BundleWorkerPath == "" {
		cfg.Storage.BundleWorkerPath = "/app"
	}
	if cfg.Storage.BundleRetention == 0 {
		cfg.Storage.BundleRetention = 50
	}
	if cfg.Storage.MaxBundleSize == 0 {
		cfg.Storage.MaxBundleSize = 104857600 // 100 MiB
	}
	if cfg.Proxy.WsCacheTTL.Duration == 0 {
		cfg.Proxy.WsCacheTTL.Duration = 60 * time.Second
	}
	if cfg.Proxy.HealthInterval.Duration == 0 {
		cfg.Proxy.HealthInterval.Duration = 15 * time.Second
	}
	if cfg.Proxy.WorkerStartTimeout.Duration == 0 {
		cfg.Proxy.WorkerStartTimeout.Duration = 60 * time.Second
	}
	if cfg.Proxy.MaxWorkers == 0 {
		cfg.Proxy.MaxWorkers = 100
	}
	if cfg.Proxy.LogRetention.Duration == 0 {
		cfg.Proxy.LogRetention.Duration = 1 * time.Hour
	}
}
```

**Env var overlay:**

Every config field can be overridden by an env var. The convention is
`BLOCKYARD_` + section + `_` + field, all uppercased:

| Config path | Env var |
|---|---|
| `Server.Bind` | `BLOCKYARD_SERVER_BIND` |
| `Docker.RvVersion` | `BLOCKYARD_DOCKER_RV_VERSION` |
| `Storage.BundleServerPath` | `BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH` |
| `Proxy.WsCacheTTL` | `BLOCKYARD_PROXY_WS_CACHE_TTL` |

Implementation: a single reflection-driven function walks the `Config`
struct, derives the env var name from each field's `toml` struct tag, and
sets the value by type. Adding a config field automatically gives it an
env var — no manual mapping to maintain.

```go
// applyEnvOverrides walks cfg via reflection, deriving the env var name
// from toml struct tags (BLOCKYARD_ + section + _ + field, uppercased).
// Supported field types: string, int, int64, float64, Duration.
func applyEnvOverrides(cfg *Config) {
	applyEnvToStruct(reflect.ValueOf(cfg).Elem(), "BLOCKYARD")
}

func applyEnvToStruct(v reflect.Value, prefix string) {
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		envName := prefix + "_" + strings.ToUpper(tag)
		fv := v.Field(i)

		// Recurse into nested config sections (but not Duration,
		// which is a struct wrapper around time.Duration).
		if field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeOf(Duration{}) {
			applyEnvToStruct(fv, envName)
			continue
		}

		val, ok := os.LookupEnv(envName)
		if !ok {
			continue
		}

		switch fv.Type() {
		case reflect.TypeOf(Duration{}):
			if d, err := time.ParseDuration(val); err == nil {
				fv.Set(reflect.ValueOf(Duration{d}))
			}
		default:
			switch fv.Kind() {
			case reflect.String:
				fv.SetString(val)
			case reflect.Int, reflect.Int64:
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					fv.SetInt(n)
				}
			case reflect.Float64:
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					fv.SetFloat(f)
				}
			}
		}
	}
}
```

No hand-maintained env var list. No per-field setter functions. The
`toml` struct tags are the single source of truth for both TOML parsing
and env var naming.

**Loading:**

```go
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&cfg)
	applyEnvOverrides(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
```

**Validation:**

```go
func validate(cfg *Config) error {
	if cfg.Server.Token == "" {
		return fmt.Errorf("config: server.token must not be empty")
	}
	if cfg.Docker.Image == "" {
		return fmt.Errorf("config: docker.image must not be empty")
	}
	if cfg.Storage.BundleServerPath == "" {
		return fmt.Errorf("config: storage.bundle_server_path must not be empty")
	}
	if cfg.Database.Path == "" {
		return fmt.Errorf("config: database.path must not be empty")
	}

	if err := ensureDirWritable(cfg.Storage.BundleServerPath, "storage.bundle_server_path"); err != nil {
		return err
	}
	dbDir := filepath.Dir(cfg.Database.Path)
	if err := ensureDirWritable(dbDir, "database.path parent directory"); err != nil {
		return err
	}

	return nil
}

func ensureDirWritable(path, label string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("config: %s: cannot create directory %q: %w", label, path, err)
	}
	testFile := filepath.Join(path, ".blockyard-write-test")
	if err := os.WriteFile(testFile, nil, 0o644); err != nil {
		return fmt.Errorf("config: %s: directory %q is not writable: %w", label, path, err)
	}
	os.Remove(testFile)
	return nil
}
```

**Tests (`internal/config/config_test.go`):**

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

const minimalTOML = `
[server]
token = "test-token"

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`

func loadFromString(t *testing.T, content string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestParseMinimalConfig(t *testing.T) {
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.Bind != "0.0.0.0:8080" {
		t.Errorf("expected default bind, got %q", cfg.Server.Bind)
	}
	if cfg.Server.Token != "test-token" {
		t.Errorf("expected test-token, got %q", cfg.Server.Token)
	}
	if cfg.Proxy.MaxWorkers != 100 {
		t.Errorf("expected default max_workers 100, got %d", cfg.Proxy.MaxWorkers)
	}
}

func TestEnvVarOverridesToken(t *testing.T) {
	t.Setenv("BLOCKYARD_SERVER_TOKEN", "override-token")
	cfg := loadFromString(t, minimalTOML)
	if cfg.Server.Token != "override-token" {
		t.Errorf("expected override-token, got %q", cfg.Server.Token)
	}
}

func TestValidationRejectsEmptyToken(t *testing.T) {
	tomlContent := `
[server]
token = ""

[docker]
image = "ghcr.io/rocker-org/r-ver:latest"

[storage]
bundle_server_path = "/tmp/blockyard-test/bundles"

[database]
path = "/tmp/blockyard-test/db/blockyard.db"

[proxy]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "blockyard.toml")
	os.WriteFile(path, []byte(tomlContent), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Error("expected validation error for empty token")
	}
}
```

**Env var uniqueness test** — verifies no two config fields produce the
same env var name (catches the case where different `toml` tags in
different sections collapse to the same uppercased path):

```go
// collectEnvVarNames walks Config struct tags and returns all derived
// env var names. Used by tests only.
func collectEnvVarNames(t reflect.Type, prefix string) []string {
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		envName := prefix + "_" + strings.ToUpper(tag)
		if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeOf(Duration{}) {
			names = append(names, collectEnvVarNames(f.Type, envName)...)
		} else {
			names = append(names, envName)
		}
	}
	return names
}

func TestEnvVarNamesUnique(t *testing.T) {
	names := collectEnvVarNames(reflect.TypeOf(Config{}), "BLOCKYARD")
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			t.Errorf("duplicate env var name: %s", name)
		}
		seen[name] = true
	}
}
```

No coverage test is needed — the reflection-driven overlay handles every
field with a `toml` tag by construction.

### Step 3: Backend interface

`internal/backend/backend.go` — interface definition and associated types.
No implementation here; this is pure interface.

Worker handles are plain strings. Each backend maintains its own internal
state (container metadata, network IDs, etc.) keyed by the string ID —
callers only see the string.

```go
package backend

import "context"

// Backend is the pluggable container runtime abstraction.
// Docker/Podman for v0, Kubernetes for v2.
type Backend interface {
	// Spawn starts a long-lived worker. The caller provides the worker ID
	// in spec.WorkerID; the backend uses it as its internal key.
	Spawn(ctx context.Context, spec WorkerSpec) error

	// Stop stops and removes a worker by ID.
	Stop(ctx context.Context, id string) error

	// HealthCheck probes whether a worker is responsive.
	HealthCheck(ctx context.Context, id string) bool

	// Logs streams stdout/stderr from a worker.
	Logs(ctx context.Context, id string) (LogStream, error)

	// Addr resolves the worker's network address (host:port).
	Addr(ctx context.Context, id string) (string, error)

	// Build runs a build task to completion (dependency restore).
	Build(ctx context.Context, spec BuildSpec) (BuildResult, error)

	// ListManaged lists all resources carrying blockyard labels.
	ListManaged(ctx context.Context) ([]ManagedResource, error)

	// RemoveResource removes an orphaned resource.
	RemoveResource(ctx context.Context, r ManagedResource) error
}
```

**WorkerSpec:**

```go
type WorkerSpec struct {
	AppID       string
	WorkerID    string
	Image       string
	Cmd         []string          // container command; nil = use image entrypoint
	BundlePath  string            // server-side path to unpacked bundle
	LibraryPath string            // server-side path to restored R library
	WorkerMount string            // in-container mount point (BundleWorkerPath)
	ShinyPort   int
	MemoryLimit string            // e.g. "512m", "" if unset
	CPULimit    float64           // fractional vCPUs, 0 if unset
	Labels      map[string]string
}
```

**BuildSpec:**

```go
type BuildSpec struct {
	AppID       string
	BundleID    string
	Image       string
	RvVersion   string            // rv release tag, e.g. "latest" or "v0.18.0"
	BundlePath  string            // server-side path to unpacked bundle
	LibraryPath string            // server-side output path for restored library
	Labels      map[string]string
}
```

**BuildResult:**

```go
type BuildResult struct {
	Success  bool
	ExitCode int
}
```

**ManagedResource:**

```go
type ManagedResource struct {
	ID   string
	Kind ResourceKind
}

type ResourceKind int

const (
	ResourceContainer ResourceKind = iota
	ResourceNetwork
)
```

**LogStream:**

```go
// LogStream delivers log lines as they arrive.
// Read from Lines until the channel is closed (container exited).
type LogStream struct {
	Lines <-chan string
	// Close cancels the underlying log follow.
	Close func()
}
```

### Step 4: Mock backend

`internal/backend/mock/mock.go` — an in-memory implementation for unit and
integration tests. Does not start real containers. Tracks spawned/stopped
workers in memory and exposes them for test assertions.

The mock backend starts a lightweight `net/http/httptest` server per
"worker" that responds with 200. This lets proxy tests (phase 5) route
real HTTP traffic through the proxy to the mock worker without Docker.

```go
package mock

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"

	"github.com/cynkra/blockyard/internal/backend"
)

type MockBackend struct {
	mu           sync.RWMutex
	workers      map[string]*mockWorker
	HealthOK     atomic.Bool // configurable: default true
	BuildSuccess atomic.Bool // configurable: default true
}

type mockWorker struct {
	id     string
	spec   backend.WorkerSpec
	server *httptest.Server
}

func New() *MockBackend {
	b := &MockBackend{
		workers: make(map[string]*mockWorker),
	}
	b.HealthOK.Store(true)
	b.BuildSuccess.Store(true)
	return b
}

func (b *MockBackend) WorkerCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.workers)
}

func (b *MockBackend) HasWorker(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.workers[id]
	return ok
}

func (b *MockBackend) Spawn(_ context.Context, spec backend.WorkerSpec) error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	b.mu.Lock()
	defer b.mu.Unlock()
	b.workers[spec.WorkerID] = &mockWorker{
		id:     spec.WorkerID,
		spec:   spec,
		server: srv,
	}
	return nil
}

func (b *MockBackend) Stop(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.workers[id]
	if !ok {
		return fmt.Errorf("worker %q not found", id)
	}
	w.server.Close()
	delete(b.workers, id)
	return nil
}

func (b *MockBackend) HealthCheck(_ context.Context, id string) bool {
	b.mu.RLock()
	_, ok := b.workers[id]
	b.mu.RUnlock()
	if !ok {
		return false
	}
	return b.HealthOK.Load()
}

func (b *MockBackend) Logs(_ context.Context, _ string) (backend.LogStream, error) {
	ch := make(chan string)
	close(ch)
	return backend.LogStream{Lines: ch, Close: func() {}}, nil
}

func (b *MockBackend) Addr(_ context.Context, id string) (string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	w, ok := b.workers[id]
	if !ok {
		return "", fmt.Errorf("worker %q not found", id)
	}
	// httptest.Server.Listener.Addr() gives the listener's net.Addr.
	// URL is "http://127.0.0.1:<port>"; extract host:port from it.
	return w.server.Listener.Addr().String(), nil
}

func (b *MockBackend) Build(_ context.Context, _ backend.BuildSpec) (backend.BuildResult, error) {
	if b.BuildSuccess.Load() {
		return backend.BuildResult{Success: true, ExitCode: 0}, nil
	}
	return backend.BuildResult{Success: false, ExitCode: 1}, nil
}

func (b *MockBackend) ListManaged(_ context.Context) ([]backend.ManagedResource, error) {
	return nil, nil // mock has no external state to leak
}

func (b *MockBackend) RemoveResource(_ context.Context, _ backend.ManagedResource) error {
	return nil
}
```

**Tests (`internal/backend/mock/mock_test.go`):**

```go
package mock

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/backend"
)

func testWorkerSpec(appID, workerID string) backend.WorkerSpec {
	return backend.WorkerSpec{
		AppID:       appID,
		WorkerID:    workerID,
		Image:       "test:latest",
		BundlePath:  "/tmp/bundle",
		LibraryPath: "/tmp/lib",
		WorkerMount: "/app",
		ShinyPort:   3838,
	}
}

func TestSpawnAndStop(t *testing.T) {
	b := New()
	ctx := context.Background()

	spec := testWorkerSpec("app-1", "worker-1")
	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if b.WorkerCount() != 1 {
		t.Errorf("expected 1 worker, got %d", b.WorkerCount())
	}
	if !b.HasWorker("worker-1") {
		t.Error("expected HasWorker to return true")
	}

	if err := b.Stop(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if b.WorkerCount() != 0 {
		t.Errorf("expected 0 workers, got %d", b.WorkerCount())
	}
}

func TestHealthCheckConfigurable(t *testing.T) {
	b := New()
	ctx := context.Background()

	spec := testWorkerSpec("app-1", "worker-1")
	b.Spawn(ctx, spec)

	if !b.HealthCheck(ctx, "worker-1") {
		t.Error("expected healthy")
	}

	b.HealthOK.Store(false)
	if b.HealthCheck(ctx, "worker-1") {
		t.Error("expected unhealthy")
	}
}

func TestAddr(t *testing.T) {
	b := New()
	ctx := context.Background()

	spec := testWorkerSpec("app-1", "worker-1")
	b.Spawn(ctx, spec)

	addr, err := b.Addr(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		t.Error("expected non-empty address")
	}
}

func TestBuildConfigurable(t *testing.T) {
	b := New()
	ctx := context.Background()

	result, _ := b.Build(ctx, backend.BuildSpec{})
	if !result.Success {
		t.Error("expected success")
	}

	b.BuildSuccess.Store(false)
	result, _ = b.Build(ctx, backend.BuildSpec{})
	if result.Success {
		t.Error("expected failure")
	}
}

func TestStopNonexistent(t *testing.T) {
	b := New()
	err := b.Stop(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error stopping nonexistent worker")
	}
}
```

### Step 5: SQLite schema

`internal/db/db.go` — database setup and queries. Uses
`modernc.org/sqlite` (pure Go, no CGO).

The v0 schema is a Go string constant — no migration files, no embed, no
migration framework. Proper versioned migrations are a post-v0 concern;
for now the schema is small enough to live inline. `CREATE TABLE IF NOT
EXISTS` makes `Open` idempotent.

```go
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER NOT NULL DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    path        TEXT NOT NULL,
    uploaded_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_bundles_app_id ON bundles(app_id);
`

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	sqlDB, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &DB{sqlDB}, nil
}
```

**Row types and query functions:**

```go
type AppRow struct {
	ID                   string
	Name                 string
	ActiveBundle         *string
	MaxWorkersPerApp     *int
	MaxSessionsPerWorker int
	MemoryLimit          *string
	CPULimit             *float64
	CreatedAt            string
	UpdatedAt            string
}

type BundleRow struct {
	ID         string
	AppID      string
	Status     string
	Path       string
	UploadedAt string
}

func (db *DB) CreateApp(name string) (*AppRow, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := db.Exec(
		`INSERT INTO apps (id, name, max_sessions_per_worker, created_at, updated_at)
		 VALUES (?, ?, 1, ?, ?)`,
		id, name, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert app: %w", err)
	}

	return db.GetApp(id)
}

func (db *DB) GetApp(id string) (*AppRow, error) {
	row := db.QueryRow(`SELECT id, name, active_bundle, max_workers_per_app,
		max_sessions_per_worker, memory_limit, cpu_limit, created_at, updated_at
		FROM apps WHERE id = ?`, id)
	return scanApp(row)
}

func (db *DB) GetAppByName(name string) (*AppRow, error) {
	row := db.QueryRow(`SELECT id, name, active_bundle, max_workers_per_app,
		max_sessions_per_worker, memory_limit, cpu_limit, created_at, updated_at
		FROM apps WHERE name = ?`, name)
	return scanApp(row)
}

func (db *DB) ListApps() ([]AppRow, error) {
	rows, err := db.Query(`SELECT id, name, active_bundle, max_workers_per_app,
		max_sessions_per_worker, memory_limit, cpu_limit, created_at, updated_at
		FROM apps ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []AppRow
	for rows.Next() {
		var app AppRow
		if err := rows.Scan(&app.ID, &app.Name, &app.ActiveBundle,
			&app.MaxWorkersPerApp, &app.MaxSessionsPerWorker,
			&app.MemoryLimit, &app.CPULimit,
			&app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

func (db *DB) DeleteApp(id string) (bool, error) {
	result, err := db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (db *DB) FailStaleBuilds() (int64, error) {
	result, err := db.Exec(
		`UPDATE bundles SET status = 'failed' WHERE status = 'building'`,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func scanApp(row *sql.Row) (*AppRow, error) {
	var app AppRow
	err := row.Scan(&app.ID, &app.Name, &app.ActiveBundle,
		&app.MaxWorkersPerApp, &app.MaxSessionsPerWorker,
		&app.MemoryLimit, &app.CPULimit,
		&app.CreatedAt, &app.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &app, nil
}
```

**Tests (`internal/db/db_test.go`):**

Tests use in-memory SQLite (`:memory:`) — no file I/O, no cleanup.

```go
package db

import "testing"

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateAndGetApp(t *testing.T) {
	db := testDB(t)

	app, err := db.CreateApp("my-app")
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "my-app" {
		t.Errorf("expected my-app, got %q", app.Name)
	}
	if app.ID == "" {
		t.Error("expected non-empty ID")
	}

	fetched, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != app.ID {
		t.Errorf("expected %q, got %q", app.ID, fetched.ID)
	}
}

func TestGetAppByName(t *testing.T) {
	db := testDB(t)

	app, _ := db.CreateApp("my-app")

	fetched, err := db.GetAppByName("my-app")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != app.ID {
		t.Errorf("expected %q, got %q", app.ID, fetched.ID)
	}

	missing, err := db.GetAppByName("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Error("expected nil for nonexistent app")
	}
}

func TestDuplicateNameFails(t *testing.T) {
	db := testDB(t)

	_, err := db.CreateApp("my-app")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateApp("my-app")
	if err == nil {
		t.Error("expected error on duplicate name")
	}
}

func TestDeleteApp(t *testing.T) {
	db := testDB(t)

	app, _ := db.CreateApp("my-app")
	deleted, err := db.DeleteApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected deletion")
	}

	fetched, _ := db.GetApp(app.ID)
	if fetched != nil {
		t.Error("expected nil after deletion")
	}
}

func TestListApps(t *testing.T) {
	db := testDB(t)

	db.CreateApp("app-a")
	db.CreateApp("app-b")

	apps, err := db.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Errorf("expected 2 apps, got %d", len(apps))
	}
}

func TestFailStaleBuilds(t *testing.T) {
	db := testDB(t)

	app, _ := db.CreateApp("my-app")

	// Insert a bundle in "building" state
	_, err := db.Exec(
		`INSERT INTO bundles (id, app_id, status, path, uploaded_at)
		 VALUES ('b1', ?, 'building', '/tmp/b1.tar.gz', '2024-01-01T00:00:00Z')`,
		app.ID,
	)
	if err != nil {
		t.Fatal(err)
	}

	n, err := db.FailStaleBuilds()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 stale build marked failed, got %d", n)
	}

	// Verify status changed
	var status string
	db.QueryRow(`SELECT status FROM bundles WHERE id = 'b1'`).Scan(&status)
	if status != "failed" {
		t.Errorf("expected 'failed', got %q", status)
	}
}
```

### Step 6: In-memory stores

Four concurrent data structures used by the proxy, API, and operations
layers. All are simple `sync.RWMutex` + `map` types — no external
dependencies. Building them in phase 1 means later phases can use them
immediately without modifying `Server`.

**SessionStore** (`internal/session/store.go`):

Maps session IDs to worker IDs. Cookie-based session pinning for the
proxy layer.

```go
package session

import "sync"

type Store struct {
	mu       sync.RWMutex
	sessions map[string]string // session ID → worker ID
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]string)}
}

func (s *Store) Get(sessionID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	wid, ok := s.sessions[sessionID]
	return wid, ok
}

func (s *Store) Set(sessionID, workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = workerID
}

func (s *Store) Delete(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// DeleteByWorker removes all sessions mapped to the given worker.
// Linear scan — acceptable at max_workers = 100.
func (s *Store) DeleteByWorker(workerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for sid, wid := range s.sessions {
		if wid == workerID {
			delete(s.sessions, sid)
			n++
		}
	}
	return n
}

func (s *Store) CountForWorker(workerID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, wid := range s.sessions {
		if wid == workerID {
			n++
		}
	}
	return n
}
```

**WorkerRegistry** (`internal/registry/registry.go`):

Maps worker IDs to network addresses. The proxy looks up where to
forward traffic.

```go
package registry

import "sync"

type Registry struct {
	mu    sync.RWMutex
	addrs map[string]string // worker ID → "host:port"
}

func New() *Registry {
	return &Registry{addrs: make(map[string]string)}
}

func (r *Registry) Get(workerID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	addr, ok := r.addrs[workerID]
	return addr, ok
}

func (r *Registry) Set(workerID, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs[workerID] = addr
}

func (r *Registry) Delete(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.addrs, workerID)
}
```

**TaskStore** (`internal/task/store.go`):

Tracks async restore tasks. Provides a create/subscribe pattern:
background goroutines write log lines; HTTP handlers read buffered
output and optionally follow live lines.

```go
package task

import (
	"sync"
	"time"
)

type Status int

const (
	Running Status = iota
	Completed
	Failed
)

type Store struct {
	mu    sync.RWMutex
	tasks map[string]*entry
}

type entry struct {
	status    Status
	createdAt time.Time
	buffer    []string     // all lines emitted so far
	ch        chan string   // live followers receive here
	done      chan struct{} // closed when task completes
}

func NewStore() *Store {
	return &Store{tasks: make(map[string]*entry)}
}

// Create registers a new running task. Returns a Sender for writing
// log lines.
func (s *Store) Create(id string) Sender {
	e := &entry{
		status:    Running,
		createdAt: time.Now(),
		ch:        make(chan string, 64),
		done:      make(chan struct{}),
	}
	s.mu.Lock()
	s.tasks[id] = e
	s.mu.Unlock()
	return Sender{e: e}
}

// Status returns the task's current status. Returns false if not found.
func (s *Store) Status(id string) (Status, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tasks[id]
	if !ok {
		return 0, false
	}
	return e.status, true
}

// Subscribe returns a snapshot of buffered lines and a channel for
// live lines. The caller must read the snapshot first, then follow
// the channel. The done channel is closed when the task completes.
func (s *Store) Subscribe(id string) (snapshot []string, live <-chan string, done <-chan struct{}, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, found := s.tasks[id]
	if !found {
		return nil, nil, nil, false
	}
	// Snapshot under read lock — the sender appends under write lock
	// on the entry, but the store lock protects the map lookup.
	snap := make([]string, len(e.buffer))
	copy(snap, e.buffer)
	return snap, e.ch, e.done, true
}

// Sender writes log lines to a task. Not safe for concurrent use —
// one sender per task, owned by the restore goroutine.
type Sender struct {
	e *entry
}

func (s Sender) Write(line string) {
	s.e.buffer = append(s.e.buffer, line)
	// Non-blocking send — if nobody is following, drop the live line.
	// The buffer has it for later subscribers.
	select {
	case s.e.ch <- line:
	default:
	}
}

func (s Sender) Complete(status Status) {
	s.e.status = status
	close(s.e.ch)
	close(s.e.done)
}
```

**Subscribe pattern (critical for correctness):** callers subscribe to
the broadcast channel first (via `Subscribe`), then read the snapshot.
After delivering snapshot lines, they relay live lines from the channel,
skipping any that overlap with the snapshot. This prevents dropped or
duplicate lines across the snapshot/live boundary.

**LogStore** (`internal/logstore/store.go`):

Per-worker log buffer with broadcast for live followers. Same
subscribe-then-snapshot pattern as the task store.

```go
package logstore

import (
	"sync"
	"time"
)

const maxLogLines = 50_000

type Store struct {
	mu      sync.RWMutex
	entries map[string]*logEntry
}

type logEntry struct {
	appID   string
	buffer  []string
	ch      chan string
	endedAt time.Time // zero if still active
}

func NewStore() *Store {
	return &Store{entries: make(map[string]*logEntry)}
}

// Create registers a new log stream for a worker. Returns a Sender
// for writing log lines from the capture goroutine.
func (s *Store) Create(workerID, appID string) Sender {
	e := &logEntry{
		appID: appID,
		ch:    make(chan string, 64),
	}
	s.mu.Lock()
	s.entries[workerID] = e
	s.mu.Unlock()
	return Sender{e: e}
}

// Subscribe returns a snapshot and live channel for a worker's logs.
func (s *Store) Subscribe(workerID string) (snapshot []string, live <-chan string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, found := s.entries[workerID]
	if !found {
		return nil, nil, false
	}
	snap := make([]string, len(e.buffer))
	copy(snap, e.buffer)
	return snap, e.ch, true
}

// WorkerIDsByApp returns worker IDs for all workers of the given app.
func (s *Store) WorkerIDsByApp(appID string) (workerIDs []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for wid, e := range s.entries {
		if e.appID == appID {
			workerIDs = append(workerIDs, wid)
		}
	}
	return workerIDs
}

func (s *Store) MarkEnded(workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[workerID]; ok {
		e.endedAt = time.Now()
		close(e.ch)
	}
}

func (s *Store) HasActive(workerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[workerID]
	return ok && e.endedAt.IsZero()
}

// CleanupExpired removes log entries that ended more than `retention` ago.
func (s *Store) CleanupExpired(retention time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-retention)
	n := 0
	for wid, e := range s.entries {
		if !e.endedAt.IsZero() && e.endedAt.Before(cutoff) {
			delete(s.entries, wid)
			n++
		}
	}
	return n
}

type Sender struct {
	e *logEntry
}

func (s Sender) Write(line string) {
	if len(s.e.buffer) < maxLogLines {
		s.e.buffer = append(s.e.buffer, line)
	}
	select {
	case s.e.ch <- line:
	default:
	}
}
```

Buffer capped at 50,000 lines per worker (~10 MB at 200 bytes/line).

**Tests for stores:**

Each store gets in-package tests covering the core operations. Key
scenarios:

- **Session store:** set/get/delete, `DeleteByWorker` removes all
  sessions for a worker and returns count, `CountForWorker` reflects
  current state.
- **Registry:** set/get/delete, missing key returns `false`.
- **Task store:** create sets status to `Running`, `Subscribe` returns
  snapshot + live channel, `Sender.Write` appends to buffer and sends
  to channel, `Sender.Complete` closes channels and updates status.
  Verify no dropped lines across snapshot/live boundary.
- **Log store:** create/subscribe/mark-ended, `CleanupExpired` removes
  only entries older than retention, `HasActive` returns false after
  `MarkEnded`, buffer cap is enforced.

### Step 7: Server struct

`internal/server/state.go` — shared server state, passed by pointer to
handlers and background goroutines.

`Server` is a plain struct, not a generic type. The `Backend` interface
provides polymorphism without generics — tests assign a
`*mock.MockBackend` to the `Backend` field.

```go
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

// ActiveWorker represents a running worker tracked by the server.
// The worker ID is the map key in WorkerMap, not stored here.
type ActiveWorker struct {
	AppID string
}

// WorkerMap is a concurrent map of worker ID → ActiveWorker.
type WorkerMap struct {
	mu      sync.RWMutex
	workers map[string]ActiveWorker
}

func NewWorkerMap() *WorkerMap {
	return &WorkerMap{workers: make(map[string]ActiveWorker)}
}

func (m *WorkerMap) Get(id string) (ActiveWorker, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.workers)
}

func (m *WorkerMap) CountForApp(appID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.workers))
	for id := range m.workers {
		ids = append(ids, id)
	}
	return ids
}

// ForApp returns all worker IDs for a given app.
func (m *WorkerMap) ForApp(appID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ids []string
	for id, w := range m.workers {
		if w.AppID == appID {
			ids = append(ids, id)
		}
	}
	return ids
}
```

### Step 8: Structured logging

Already shown in step 1. The setup is:

```go
slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelInfo,
})))
```

- **Default level:** `Info`
- **Override:** set a custom handler or level via env var (handled in later
  phases if needed)
- **Format:** JSON lines (structured, parseable by log aggregators)

No additional work beyond what's in `main.go`. `log/slog` is used
throughout the codebase via `slog.Info()`, `slog.Warn()`, `slog.Error()`.

### Step 9: GitHub Actions CI

`.github/workflows/ci.yml` — runs on every push and pull request. Two
jobs: vet/test (always) and Docker integration tests (starting from
phase 2).

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: go vet ./...
      - run: go test ./...

  docker-tests:
    runs-on: ubuntu-latest
    if: false  # enabled in phase 2 when Docker backend is implemented
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: go test -tags docker_test ./...
```

**Notes:**

- **Go module cache** — `actions/setup-go@v5` caches the module cache
  automatically. No additional caching configuration needed.
- **`go vet`** — catches common mistakes. Runs on all packages.
- **`docker-tests` job** — disabled (`if: false`) until phase 2. Flip to
  `if: true` or remove the condition when the Docker backend lands.
- **No special test flags** — `go test ./...` runs all tests in all
  packages. The mock backend lives in `internal/backend/mock/` and is
  included in regular test runs (its `_test.go` files are just Go test
  files, no build tags needed).

## Example blockyard.toml

Ship this as the reference config at the repo root:

```toml
[server]
bind             = "0.0.0.0:8080"
# token is required — pass via BLOCKYARD_SERVER_TOKEN env var
shutdown_timeout = "30s"

[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/rocker-org/r-ver:latest"
shiny_port = 3838
rv_version = "latest"

[storage]
bundle_server_path = "/data/bundles"
bundle_worker_path = "/app"
bundle_retention   = 50
max_bundle_size    = 104857600

[database]
path = "/data/db/blockyard.db"

[proxy]
ws_cache_ttl         = "60s"
health_interval      = "15s"
worker_start_timeout = "60s"
max_workers          = 100
log_retention        = "1h"
```

## Implementation notes

Things to keep in mind during implementation:

- **No `ON DELETE CASCADE` on `bundles.app_id`.** This is intentional.
  Deleting an app requires a multi-step teardown (stop workers, remove
  bundle files from disk, delete bundle rows, then delete the app row).
  The FK constraint prevents the DB layer from silently deleting an app
  while leaving orphaned bundle rows or files on disk. The API handler
  (phase 4) orchestrates the full sequence; the FK enforces ordering.

- **Circular FK between `apps` and `bundles`.** `apps.active_bundle`
  references `bundles.id`, and `bundles.app_id` references `apps.id`. This
  works because apps are created with `active_bundle = NULL` and the field
  is only set later when a bundle reaches `ready` status. No deferred
  constraints needed — the insert order (app first, bundle second, then
  update `active_bundle`) avoids the cycle naturally.

- **`database/sql` nullable columns.** Columns that can be NULL
  (`active_bundle`, `max_workers_per_app`, `memory_limit`, `cpu_limit`)
  are represented as pointer types (`*string`, `*int`, `*float64`) in
  `AppRow`. `database/sql`'s `Scan` handles `nil` → `*T` mapping
  natively.

- **No `status` column on `apps`.** App status (running/stopped) is
  inferred at runtime from whether any workers exist for the app in
  `Server.Workers`. This avoids staleness on crash/restart and eliminates
  synchronization between in-memory state and the DB.

## Exit criteria

Phase 1 is done when:

- `go build ./...` succeeds
- `go vet ./...` is clean
- `go test ./...` passes:
  - Config parsing + env var overlay + validation tests
  - Env var uniqueness test
  - Mock backend spawn/stop/health_check/addr/build tests
  - SQLite create/get/list/delete app tests
  - SQLite fail stale builds test
  - Session store set/get/delete/delete-by-worker tests
  - Registry set/get/delete tests
  - Task store create/subscribe/write/complete tests
  - Log store create/subscribe/mark-ended/cleanup tests
- `cmd/blockyard/main.go` loads config and initializes logging (does not
  start a server)
- The example `blockyard.toml` is valid and parseable
- CI passes on GitHub Actions (vet + test)
