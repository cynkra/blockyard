# blockyard v0 Implementation Plan

This document is the build plan for v0 — the first working technical milestone.
It covers project layout, dependency graph, build phases, key type definitions,
and test strategy. The roadmap (`../roadmap.md`) is the source of truth for
*what* v0 includes; this document describes *how* to build it.

## Project Layout

A single Go module. The `cmd/blockyard/` directory holds the entry point; all
domain logic lives under `internal/`.

```
blockyard/
├── go.mod
├── cmd/
│   └── blockyard/
│       ├── main.go                # wiring, signal handling, shutdown
│       └── backend_docker.go      # imports docker backend, provides newBackend()
├── internal/
│   ├── config/
│   │   └── config.go              # TOML parsing + env var overlay + validation
│   ├── backend/
│   │   ├── backend.go             # Backend interface, WorkerSpec, BuildSpec
│   │   ├── docker/
│   │   │   └── docker.go          # DockerBackend
│   │   └── mock/
│   │       └── mock.go            # MockBackend (imported only from _test.go)
│   ├── db/
│   │   └── db.go                  # SQLite setup, migrations, queries
│   ├── bundle/
│   │   └── bundle.go              # upload, storage, unpack, restore, retention
│   ├── session/
│   │   └── store.go               # SessionStore: session ID → worker ID
│   ├── registry/
│   │   └── registry.go            # WorkerRegistry: worker ID → "host:port"
│   ├── logstore/
│   │   └── store.go               # LogStore: per-worker buffer + broadcast
│   ├── task/
│   │   └── store.go               # in-memory task store for async restore jobs
│   ├── api/
│   │   ├── router.go              # chi router assembly + healthz
│   │   ├── auth.go                # bearer token middleware
│   │   ├── apps.go                # app CRUD + start/stop/logs endpoints
│   │   ├── bundles.go             # upload + list endpoints
│   │   ├── tasks.go               # task status + log streaming
│   │   └── error.go               # shared error response helpers
│   ├── proxy/
│   │   ├── proxy.go               # reverse proxy handler, routing
│   │   ├── forward.go             # HTTP forwarding, prefix stripping
│   │   ├── ws.go                  # WebSocket forwarding + cache
│   │   └── coldstart.go           # hold-until-healthy logic
│   ├── ops/
│   │   ├── ops.go                 # evict_worker, startup_cleanup, graceful_shutdown
│   │   ├── health.go              # health polling loop
│   │   └── logcapture.go          # per-worker log capture goroutine
│   └── server/
│       └── state.go               # Server struct: shared server state
├── migrations/
│   └── 001_initial.sql
├── blockyard.toml                 # reference config
├── docs/
└── .github/
    └── workflows/
        └── ci.yml
```

**Backend selection via build tags:** only Docker exists for now, so
`backend_docker.go` has no build tag. When a Kubernetes backend is added:

```
cmd/blockyard/backend_docker.go   # //go:build !k8s
cmd/blockyard/backend_k8s.go      # //go:build k8s
```

`go build` gives Docker (default). `go build -tags k8s` gives Kubernetes.

**Mock backend:** lives in `internal/backend/mock/` and is only imported from
`_test.go` files. Go's toolchain excludes test-only imports from production
builds — no build tags needed.

**Docker integration tests:** gated behind a `docker_test` build tag. Run
with `go test -tags docker_test ./internal/backend/docker/`. Regular
`go test ./...` skips them.

## Dependencies

```
go 1.24

require (
    github.com/go-chi/chi/v5      // HTTP router: lightweight, idiomatic http.Handler
    github.com/BurntSushi/toml     // TOML config: written by the spec co-creator
    github.com/coder/websocket     // WebSocket: context-aware, clean API
    github.com/docker/docker       // Docker client: official first-party
    github.com/google/uuid         // UUIDs: standard, well-tested
    modernc.org/sqlite             // SQLite: pure Go, no CGO needed
)
```

**Dependency rationale:**

- **chi** — routing, middleware, route groups for auth separation. Implements
  `http.Handler` so everything composes with the standard library. No
  framework lock-in.
- **modernc.org/sqlite** — pure Go SQLite. `CGO_ENABLED=0` builds work.
  Blockyard's SQLite usage (metadata CRUD, not per-request writes) won't
  hit the performance difference vs `mattn/go-sqlite3`.
- **coder/websocket** — context-aware cancellation, maintained by Coder.
  Simplifies WS cache TTL and disconnect tracking vs gorilla/websocket
  (archived).
- **docker/docker client** — official Go client for the Docker Engine API.
  Full coverage of container lifecycle, network management, log streaming,
  image pulling. Works with Podman's Docker-compatible socket unchanged.
- **BurntSushi/toml** — standard Go TOML library. Env var overlay is a
  manual layer on top (not a config framework).
- **log/slog** (stdlib) — structured logging. JSON and text output, log
  levels. Built into Go 1.21+.

**What comes from the standard library:**

- HTTP serving and reverse proxying (`net/http`, `net/http/httputil`)
- TLS (deferred to v1, but `crypto/tls` + `autocert` when needed)
- Concurrency (`sync`, `context`, goroutines, channels)
- Signal handling (`os/signal`)
- Timers and tickers (`time`)
- Tar/gzip handling (`archive/tar`, `compress/gzip`)
- File I/O, temp files, atomic rename (`os`, `io`)

## Build Phases

The dependency graph dictates build order. Each phase produces testable code.
Phases are sequential; items within a phase can be worked in parallel.

### Phase 1: Foundation ([detailed plan](phase-0-1.md))

Establish the project skeleton, core types, config parsing, and database
schema. Everything else builds on this.

**Deliverables:**

1. Go module with `cmd/blockyard/main.go` + `internal/` package structure
2. Config parsing (`internal/config/`) — TOML + env var overlay
3. `Backend` interface + `WorkerSpec` + `BuildSpec`
4. Mock backend implementation (for tests)
5. SQLite schema + migrations (`internal/db/`)
6. In-memory stores: `session.Store`, `registry.Registry`, `logstore.Store`, `task.Store`
7. `Server` struct that holds shared server state
8. Structured logging setup (`log/slog`)
9. GitHub Actions CI workflow

**Config structure:**

```go
type Config struct {
    Server   ServerConfig
    Docker   DockerConfig
    Storage  StorageConfig
    Database DatabaseConfig
    Proxy    ProxyConfig
}

type ServerConfig struct {
    Bind            string        // default: "0.0.0.0:8080"
    Token           string        // required, no default
    ShutdownTimeout time.Duration // default: 30s
}

type DockerConfig struct {
    Socket    string // default: "/var/run/docker.sock"
    Image     string // required, e.g. "ghcr.io/rocker-org/r-ver:4.4.3"
    ShinyPort int    // default: 3838
    RvVersion string // default: "latest"
}

type StorageConfig struct {
    BundleServerPath string // required, e.g. "/data/bundles"
    BundleWorkerPath string // default: "/app"
    BundleRetention  int    // default: 50
    MaxBundleSize    int64  // default: 104857600 (100 MiB)
}

type DatabaseConfig struct {
    Path string // required, e.g. "/data/db/blockyard.db"
}

type ProxyConfig struct {
    WsCacheTTL         time.Duration // default: 60s
    HealthInterval     time.Duration // default: 15s
    WorkerStartTimeout time.Duration // default: 60s
    MaxWorkers         int           // default: 100
    LogRetention       time.Duration // default: 1h
}
```

**Env var overlay:** every config field can be overridden by an env var. The
convention is `BLOCKYARD_` + section + `_` + field, uppercased:

| Config path | Env var |
|---|---|
| `Server.Bind` | `BLOCKYARD_SERVER_BIND` |
| `Docker.RvVersion` | `BLOCKYARD_DOCKER_RV_VERSION` |
| `Storage.BundleServerPath` | `BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH` |
| `Proxy.WsCacheTTL` | `BLOCKYARD_PROXY_WS_CACHE_TTL` |

Implementation: deserialize TOML first, then walk the struct via reflection
and override any field that has a matching env var set. The env var name is
derived from the `toml` struct tags — adding a config field automatically
gives it an env var. A test verifies no two fields produce the same env var
name.

**Loading:** `--config <path>` flag or `./blockyard.toml` by default. TOML
first, env var overlay second, validation third. The server refuses to start
on any validation error.

**Backend interface (full v0 surface):**

```go
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

Worker handles are plain strings. Each backend maintains its own internal
state (container metadata, network IDs, etc.) keyed by the worker ID
provided in the spec — callers only see the string.

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
    Kind ResourceKind // Container or Network
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

**Mock backend:**

An in-memory implementation for unit and integration tests. Does not start
real containers. Tracks spawned/stopped workers in memory and exposes them
for test assertions.

```go
type MockBackend struct {
    mu             sync.RWMutex
    workers        map[string]*mockWorker
    healthResponse atomic.Bool  // configurable: default true
    buildSuccess   atomic.Bool  // configurable: default true
}
```

The mock backend starts a lightweight `net/http/httptest` server per "worker"
that responds with 200. This lets proxy tests route real HTTP traffic through
the proxy to the mock worker without Docker.

**Server (shared server state):**

```go
type Server struct {
    Config   *config.Config
    Backend  backend.Backend
    DB       *db.DB
    Workers  *WorkerMap            // worker ID → ActiveWorker
    Sessions *session.Store        // session ID → worker ID
    Registry *registry.Registry    // worker ID → "host:port"
    Tasks    *task.Store           // async restore task tracking
    LogStore *logstore.Store       // per-worker log buffers
}
```

`Server` is a plain struct passed by pointer. The `Backend` field holds
an interface value — tests assign a `*mock.MockBackend`, production wires
in a `*docker.DockerBackend`.

All concurrent maps (in `session.Store`, `registry.Registry`, etc.) use
`sync.RWMutex` + `map`, not `sync.Map` (the typed-key ergonomics are
better and the access patterns don't benefit from `sync.Map`'s
optimization for disjoint key sets).

The in-memory stores (`session.Store`, `registry.Registry`,
`logstore.Store`, `task.Store`) live in their own packages. Each is
self-contained with no dependency on `server`. This keeps the import
graph acyclic — `server` imports the store packages, and consumer
packages (`api`, `proxy`, `ops`) import `server`.

### Phase 2: Docker Backend ([detailed plan](phase-0-2.md))

Implement the `Backend` interface for Docker using the Docker Go client.
This is the only production backend for v0.

**Deliverables:**

1. `DockerBackend` — full `Backend` interface implementation
2. Per-container bridge network creation and cleanup
3. Server multi-homing — join each worker's network
4. Container hardening (cap-drop ALL, read-only rootfs, no-new-privileges)
5. Image pulling (on demand, before build/spawn)
6. Label management (`dev.blockyard/*`)
7. Server container ID detection (for network joining)
8. Metadata endpoint protection (iptables rules blocking `169.254.169.254`)
9. Docker integration tests behind `docker_test` build tag

**DockerBackend internal state:**

```go
type DockerBackend struct {
    client            *client.Client
    serverID          string // own container ID, empty if native mode
    config            *config.DockerConfig
    metadataBlockMode metadataMode // cached after first spawn
}
```

The Docker backend maintains its own per-worker state (container ID, network
ID, network name) in an internal map keyed by the worker ID string provided
in the `WorkerSpec`. Callers never see these details — they just pass the
worker ID string back to `Stop`, `HealthCheck`, `Addr`, etc.

**Key operations:**

`Spawn` flow:
```
1. Ensure image exists locally (pull if not)
2. Create per-worker bridge network: blockyard-{worker-id}
   Labels: dev.blockyard/managed=true, app-id, worker-id
3. Block metadata endpoint for the network (iptables)
4. Create container:
   - Image from spec
   - Network: the bridge just created
   - Mounts: bundle → worker_mount (ro),
            library subdir → /blockyard-lib (ro, see library path resolution below)
   - Env: SHINY_PORT={port}, R_LIBS=/blockyard-lib
   - Tmpfs: /tmp
   - CapDrop: ALL, SecurityOpt: no-new-privileges, ReadonlyRootfs: true
   - Memory/CPU limits from spec
   - Labels: dev.blockyard/managed=true, app-id, worker-id, role=worker
   - No published ports
5. Join the server to the worker's network (if running in a container)
6. Start the container
```

`Build` flow:
```
1. Ensure image exists locally
2. Create container:
   - Same image as workers
   - Mounts: bundle → /app (ro), library output dir → /app/rv/library (rw)
   - Cmd: download rv from GitHub releases, run `rv sync`
   - Labels: dev.blockyard/managed=true, app-id, bundle-id, role=build
   - Tmpfs: /tmp, /root/.cache/rv
   - Rootfs NOT read-only (needs to install rv binary)
3. Start container
4. Wait for exit
5. Remove container
6. Return BuildResult{success, exit_code}
```

**Build log streaming:** `Build` is synchronous — it blocks until the
container exits and returns `BuildResult`. It does not stream build logs.
Phase 3 needs real-time build output for `GET /api/v1/tasks/{task_id}/logs`.
To support this, `BuildSpec` should gain a `LogWriter io.Writer` field.
When non-nil, `Build` streams the container's stdout/stderr to it while
waiting for exit. Phase 3 wires this to the task store.

**Library path resolution:** `rv sync` writes packages to
`rv/library/{R_VERSION}/{ARCH}/` inside the project directory. Since the
image is fixed server-wide, R version and architecture are constant across
all builds. After a successful build, the server discovers the package
directory by listing the single subdirectory under `{bundle-id}_lib/`
(e.g. `{bundle-id}_lib/4.4/x86_64/`). This resolved path is what gets
mounted into worker containers at `/blockyard-lib`. Workers set
`R_LIBS=/blockyard-lib` so R finds the packages directly — no rv needed
at runtime.

`Stop` flow:
```
1. Stop the container (with timeout)
2. Remove the container
3. Remove iptables metadata rule (by comment tag)
4. Disconnect server from the worker's network
5. Remove the network
Best-effort on each step — failures don't prevent subsequent steps.
```

**Server container ID detection:** the server needs its own container ID to
join worker networks. Detection order:

1. `BLOCKYARD_SERVER_ID` env var — explicit override
2. Parse `/proc/self/cgroup` — Docker writes the container ID in cgroup paths
3. Read hostname — Docker sets it to the short container ID by default
4. If all fail: native mode. Skip network joining; workers are reachable on
   the bridge gateway IP.

**Metadata endpoint protection:** cloud providers expose instance metadata at
`169.254.169.254`. Without protection, R code in a container can steal host
IAM credentials. On each `Spawn`, the Docker backend inserts an iptables rule
in the `DOCKER-USER` chain scoped to the worker network's subnet, tagged with
a `blockyard-{worker-id}` comment for cleanup. If iptables is unavailable
(no `CAP_NET_ADMIN`), the backend falls back to a live reachability check —
if `169.254.169.254` is already unreachable (operator-installed blanket rule),
spawn proceeds. If it's reachable and iptables can't block it, spawn fails.
The detection result is cached after the first spawn.

### Phase 3: Content Management ([detailed plan](phase-0-3.md))

Bundle upload, dependency restoration, content registry. The deployment
pipeline — from tar.gz to running app.

**Deliverables:**

1. Bundle storage — atomic writes, tar.gz unpacking, retention cleanup
2. Dependency restoration via `backend.Build()`
3. Async restore pipeline — wires task store, backend build, bundle status
4. Bundle status transitions (`pending → building → ready | failed`)
5. Bundle size limit enforcement (`MaxBundleSize`)

**Bundle upload flow:**

```
1. Validate app exists
2. Generate bundle_id (UUID) and task_id (UUID)
3. Enforce MaxBundleSize on the request body (reject with 413 if exceeded)
4. Write tar.gz to temp file in {BundleServerPath}/{app-id}/
5. Atomically rename to {bundle-id}.tar.gz
6. Unpack tar.gz to {bundle-id}/ directory
7. Create library output directory {bundle-id}_lib/
8. Insert bundle row: status = "pending"
9. Create task in TaskStore
10. Spawn async restore goroutine (does not block the response)
11. Return 202 with bundle_id and task_id
```

Async restore:
```
1. Update bundle status → "building"
2. Construct BuildSpec (image, rv_version, bundle path, library path)
3. Call backend.Build() — runs rv sync in a container
4. On success:
   - Update bundle status → "ready"
   - Set app.active_bundle → this bundle
   - Enforce retention (delete oldest non-active beyond limit)
5. On failure:
   - Update bundle status → "failed"
   - Leave app.active_bundle unchanged
6. On panic/crash:
   - Bundle stays "building"; startup_cleanup marks it "failed"
```

**Task store:**

```go
type Store struct {
    mu    sync.RWMutex
    tasks map[string]*entry
}

type entry struct {
    id        string
    status    Status // Running, Completed, Failed
    createdAt time.Time
    buffer    []string          // all lines emitted so far
    broadcast chan string       // live followers receive here
    done      chan struct{}     // closed when task completes
}
```

**Subscribe pattern (critical for correctness):** subscribe to the broadcast
channel first, then snapshot the buffer. Deliver the snapshot, then relay
live lines, skipping any that were already in the snapshot. This prevents
dropped or duplicate lines across the snapshot/live boundary.

Tasks are in-memory only — they do not survive a server restart.

**Storage layout on the server:**

```
{BundleServerPath}/
  {app-id}/
    {bundle-id}.tar.gz    # uploaded archive
    {bundle-id}/          # unpacked app code
      app.R
      rv.lock
      ...
    {bundle-id}_lib/      # R package library restored by rv
```

**BundlePaths** — a shared path constructor so all code agrees on the layout:

```go
type BundlePaths struct {
    Archive string // {base}/{app-id}/{bundle-id}.tar.gz
    Unpacked string // {base}/{app-id}/{bundle-id}/
    Library  string // {base}/{app-id}/{bundle-id}_lib/
}

func NewBundlePaths(base, appID, bundleID string) BundlePaths { ... }
```

### Phase 4: REST API + Auth ([detailed plan](phase-0-4.md))

The control plane HTTP API. All endpoints under `/api/v1/`. Protected by
static bearer token.

**Deliverables:**

1. chi router with all v0 endpoints
2. Bearer token middleware (from `Config.Server.Token`)
3. `/healthz` endpoint (unauthenticated)
4. App CRUD endpoints
5. App lifecycle endpoints (start/stop)
6. App log streaming endpoint
7. Bundle endpoints (upload, list)
8. Task endpoints (status, log streaming)
9. App name validation

**Router assembly:**

```go
func NewRouter(srv *server.Server) http.Handler {
    r := chi.NewRouter()

    r.Get("/healthz", healthz)

    r.Route("/api/v1", func(r chi.Router) {
        r.Use(bearerAuth(srv.Config.Server.Token))

        r.Post("/apps", createApp(srv))
        r.Get("/apps", listApps(srv))
        r.Get("/apps/{id}", getApp(srv))
        r.Patch("/apps/{id}", updateApp(srv))
        r.Delete("/apps/{id}", deleteApp(srv))

        r.Post("/apps/{id}/bundles", uploadBundle(srv))
        r.Get("/apps/{id}/bundles", listBundles(srv))

        r.Post("/apps/{id}/start", startApp(srv))
        r.Post("/apps/{id}/stop", stopApp(srv))
        r.Get("/apps/{id}/logs", appLogs(srv))

        r.Get("/tasks/{taskID}", getTask(srv))
        r.Get("/tasks/{taskID}/logs", taskLogs(srv))
    })

    return r
}
```

**Bearer auth middleware:**

```go
func bearerAuth(token string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            auth := r.Header.Get("Authorization")
            if !strings.HasPrefix(auth, "Bearer ") || auth[7:] != token {
                writeError(w, http.StatusUnauthorized, "unauthorized")
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

**Endpoint behaviors:**

| Endpoint | Method | Behavior |
|---|---|---|
| `/healthz` | GET | Returns 200 OK. No auth. No dependency checks. |
| `/api/v1/apps` | POST | Create app. Body: `{ "name": "..." }`. Validates name. Returns 201. |
| `/api/v1/apps` | GET | List all apps with derived status (running/stopped). |
| `/api/v1/apps/{id}` | GET | Get app by UUID or name (see below). 404 if neither matches. |
| `/api/v1/apps/{id}` | PATCH | Update app config (resource limits). Partial update. |
| `/api/v1/apps/{id}` | DELETE | Delete app. Stops workers, removes files, deletes DB rows. 204. |
| `/api/v1/apps/{id}/bundles` | POST | Upload bundle (tar.gz body). Returns 202 with bundle_id + task_id. |
| `/api/v1/apps/{id}/bundles` | GET | List bundles for app, newest first. |
| `/api/v1/apps/{id}/start` | POST | Start app. Spawns worker. No-op if already running. Requires active bundle. |
| `/api/v1/apps/{id}/stop` | POST | Stop app. Stops all workers (best-effort). Returns count. |
| `/api/v1/apps/{id}/logs` | GET | Stream worker logs. Optional `worker_id` query param. |
| `/api/v1/tasks/{taskID}` | GET | Get task status (running/completed/failed). |
| `/api/v1/tasks/{taskID}/logs` | GET | Stream restore output. Chunked text/plain. |

**App status:** not stored in the database. Derived at request time from
whether any workers exist for the app in `Server.Workers`. The API response
includes a `status` field ("running" or "stopped") computed from the worker
map.

**App resolution:** all `{id}` parameters across app endpoints (GET, PATCH,
DELETE, start, stop, logs, bundles) resolve by UUID first, then by name.
This is safe from collisions because names must start with a lowercase letter
while UUIDs start with a hex digit.

**App name validation:** 1–63 characters, lowercase ASCII letters, digits,
and hyphens. Must start with a letter. Must not end with a hyphen. Matches
DNS label rules.

**Error responses:** consistent JSON shape on all error paths:

```json
{ "error": "app_not_found", "message": "No app with ID a3f2c1..." }
```

`error` is a stable machine-readable code (for programmatic clients);
`message` is human-readable context. HTTP status codes: 400 (bad request /
validation), 401 (unauthorized), 404 (not found), 409 (conflict — duplicate
name, no active bundle), 413 (bundle too large), 503 (at capacity). 500 for
unexpected errors.

### Phase 5: Proxy Layer ([detailed plan](phase-0-5.md))

HTTP/WebSocket reverse proxy, session management, cold-start hold, WebSocket
session caching. This is the data plane — user browsers hit these routes.

**Deliverables:**

1. HTTP reverse proxy — forward requests to worker containers
2. WebSocket upgrade and bidirectional forwarding
3. Session cookie management (set on first request, read on subsequent)
4. Cold-start hold — block until worker healthy or timeout
5. WebSocket connection caching on client disconnect
6. Trailing-slash redirect (`/app/{name}` → `/app/{name}/`)
7. Prefix stripping (remove `/app/{name}` before forwarding)
8. Router composition — proxy routes alongside API routes

**Request flow — first visit to `/app/my-app/`:**

```
1. Extract app name from URL path
2. No session cookie → generate session_id (UUID)
3. Look up app in DB → get active_bundle
4. No active bundle → 503
5. Check worker limits (global max_workers, per-app max_workers_per_app)
6. Generate worker_id (UUID), backend.Spawn(WorkerSpec{WorkerID: worker_id, ...})
7. backend.Addr(worker_id) → "host:port"
8. Register in WorkerRegistry and Workers map
9. Start log capture goroutine
10. Pin session: SessionStore.Set(session_id, worker_id)
11. Cold-start hold: poll HealthCheck with exponential backoff
    (100ms initial, capped at 2s) until healthy or worker_start_timeout
12. If timeout → evict worker, return 503
13. Set session cookie (blockyard_session, path=/app/{name}/, HttpOnly, SameSite=Lax)
14. Forward request to worker (strip /app/{name} prefix)
```

**Request flow — subsequent visit (session cookie present):**

```
1. Extract session_id from cookie
2. SessionStore.Get(session_id) → worker_id
3. WorkerRegistry.Get(worker_id) → "host:port"
4. If worker gone → treat as new session (re-spawn)
5. Forward request to worker
```

**WebSocket upgrade flow:**

```
1. Detect upgrade headers
2. Route to worker via session (same as HTTP)
3. Check WsCache for existing backend connection (reconnect case)
4. If not cached: open new WS connection to backend
5. Bidirectional frame shuttling: two goroutines, shared context
6. On client disconnect:
   - Cache backend WS with TTL timer (ws_cache_ttl)
   - Do NOT stop the worker
7. On reconnect within TTL: resume from cache, cancel timer
8. On TTL expiry: close backend WS, evict worker if no remaining sessions
9. On backend disconnect: close client connection
```

**SessionStore** (`internal/session/`):

```go
type Store struct {
    mu       sync.RWMutex
    sessions map[string]string // session ID → worker ID
}

// Methods: Get, Set, Delete, DeleteByWorker, CountForWorker
```

`DeleteByWorker(workerID)` is a reverse lookup — scans all sessions and
removes any mapping to the given worker. Used by `evict_worker`. With
`max_workers = 100` this is a trivial scan.

**WorkerRegistry** (`internal/registry/`):

```go
type Registry struct {
    mu    sync.RWMutex
    addrs map[string]string // worker ID → "host:port"
}

// Methods: Get, Set, Delete
```

**Session cookie:** name `blockyard_session`, value is a UUID. Path scoped
to `/app/{name}/`. `HttpOnly`, `SameSite=Lax`. No `Secure` flag in v0 (TLS
is terminated externally).

### Phase 6: Operations ([detailed plan](phase-0-6.md))

Health polling, orphan cleanup, log capture, log retention, graceful
shutdown. Background goroutines and lifecycle hooks.

**Deliverables:**

1. `evict_worker` — shared helper that fully decommissions a worker
2. `startup_cleanup` — remove orphaned containers/networks, fail stale bundles
3. `graceful_shutdown` — stop all workers and builds on SIGTERM
4. Health polling goroutine
5. Per-worker log capture goroutine
6. Log retention cleanup goroutine
7. `main.go` wiring — startup, background tasks, signal handling, shutdown

**LogStore** (`internal/logstore/`):

```go
type Store struct {
    mu      sync.RWMutex
    entries map[string]*logEntry // keyed by worker ID
}

type logEntry struct {
    appID   string
    buffer  []string              // capped at maxLogLines (50,000)
    ch      chan string           // broadcast channel for live followers
    endedAt time.Time             // zero value if still active
}
```

Methods: `Create(workerID, appID) LogSender`, `Subscribe(workerID)`,
`WorkerIDsByApp(appID)`, `MarkEnded(workerID)`, `CleanupExpired(retention)`,
`HasActive(workerID)`.

Subscribe uses the same subscribe-then-snapshot pattern as the task store
to prevent dropped or duplicated lines.

Buffer capped at 50,000 lines per worker (~10 MB at 200 bytes/line).

**evict_worker:** the single codepath for decommissioning a worker.
Idempotent. Best-effort on each step.

```go
func evictWorker(srv *server.Server, workerID string) {
    // 1. Remove from Workers map
    // 2. backend.Stop(ctx, workerID)
    // 3. Remove from WorkerRegistry
    // 4. Remove all sessions for this worker (SessionStore.DeleteByWorker)
    // 5. Mark log stream ended (LogStore.MarkEnded)
}
```

Called by: health poller, stop endpoint, delete endpoint, WS cache TTL
expiry, graceful shutdown.

**Health polling:** a goroutine that runs every `HealthInterval` (default
15s). Each cycle: snapshot all worker IDs, health-check all concurrently
(one goroutine per worker), evict any that fail.

**Orphan cleanup:** runs once at startup, before the server accepts
connections:

1. Clean up orphaned iptables rules (scan `DOCKER-USER` for
   `blockyard-*` comments)
2. `backend.ListManaged()` → remove all (the server just started, every
   managed resource is an orphan from a previous crash)
3. `db.FailStaleBuilds()` → mark `building` bundles as `failed`

If the backend is unreachable at startup, the server refuses to start.

**Graceful shutdown:** on SIGTERM/SIGINT:

1. Stop accepting new connections (close the HTTP listener via
   `http.Server.Shutdown` with `ShutdownTimeout` context)
2. Cancel background goroutines (via a shared `context.Context`)
3. Evict all tracked workers concurrently (each with a 15s timeout)
4. Remove any remaining managed resources (build containers, networks)
5. Fail any in-progress builds (`db.FailStaleBuilds`)
6. Close the database

A second signal during shutdown forces immediate exit.

**Background goroutine cancellation:** all background goroutines (health
poller, log retention cleaner) take a `context.Context` derived from a
parent that is cancelled on shutdown signal. They `select` on `ctx.Done()`
to exit cooperatively.

## Graceful Shutdown

Detailed in phase 6 above and in the roadmap. Summary:

```
SIGTERM received
  → http.Server.Shutdown(ctx with ShutdownTimeout)
    → stops accepting, drains in-flight (up to 30s)
  → cancel background goroutine context
  → wait for health poller + log cleaner to exit
  → evict all tracked workers concurrently (15s each)
  → backend.ListManaged → remove remaining resources
  → db.FailStaleBuilds
  → db.Close
```

## Build Order and Dependency Graph

```
Phase 1: Foundation
  ├── Config parsing
  ├── Backend interface + mock
  ├── SQLite schema
  ├── In-memory stores (session, registry, logstore, task)
  └── Server state

Phase 2: Docker Backend
  └── depends on: Backend interface, Config

Phase 3: Content Management
  ├── Bundle upload + storage
  ├── Async restore pipeline (uses task store from phase 1)
  └── depends on: Backend (Build), SQLite (content registry)

Phase 4: REST API + Auth
  └── depends on: Content Management, Server state

Phase 5: Proxy Layer
  ├── Session routing
  ├── Cold-start + WS caching
  └── depends on: Backend (Spawn/Stop/Addr), Session/Worker stores

Phase 6: Operations
  ├── Health polling
  ├── Orphan cleanup
  ├── Log capture
  └── depends on: Backend, Worker tracking
```

Phases 3 and 5 are independent of each other — they can be developed in
parallel. Both depend on phase 2 only for Docker integration testing; their
unit tests use the mock backend from phase 1.

## Test Strategy

Three levels:

### Unit tests (in-package `_test.go` files)

- **Config tests:** TOML parsing, env var overlay, validation errors, env
  var name uniqueness.
- **Mock backend tests:** spawn/stop/health_check update internal state.
- **Database tests:** full CRUD for apps and bundles, name uniqueness, fail
  stale bundles, retention enforcement. Use in-memory SQLite (`:memory:`).
- **Session/worker store tests:** concurrent insert/get/remove.
- **Task store tests:** create, write logs, subscribe, complete. No dropped
  or duplicate lines.
- **Log store tests:** create, subscribe, mark ended, cleanup expired.
- **App name validation tests:** valid names, invalid names.
- **Bundle storage tests:** write/unpack/retention using temp dirs.

### Integration tests

Start the full server (with mock backend) via `httptest.NewServer` and
exercise HTTP endpoints with a real HTTP client:

- **API tests:** CRUD apps, upload bundles, start/stop, list — all via HTTP
  against the running server.
- **Proxy tests:** the mock backend's httptest servers act as workers. Tests
  verify correct routing, session cookies, WS upgrades, cold-start holding,
  prefix stripping.
- **Bundle flow tests:** upload → restore (mock build) → bundle ready → app
  startable.

### Docker integration tests (gated behind `docker_test` build tag)

Tests that exercise the real Docker backend. Require a running Docker daemon.
Run with `go test -tags docker_test ./...`. Skipped by default in
`go test ./...`.

- **Spawn and stop a container** — verify it appears in `docker ps`,
  responds to health checks, and is removed after stop.
- **Network isolation** — spawn two containers, verify they cannot reach
  each other.
- **Build (rv restore)** — run a restore with a minimal `rv.lock`.
- **Orphan cleanup** — create labeled resources externally, run cleanup,
  verify removed.
- **Metadata endpoint** — verify containers cannot reach `169.254.169.254`.

## CI

```yaml
name: CI
on: [push, pull_request]

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
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: go test -tags docker_test ./...
```

## Design Decisions

Resolved design decisions for v0:

1. **Session cookie format:** plain UUID in a `Set-Cookie` header. No HMAC
   signing in v0 — there is no user auth on the app plane, so the cookie
   only routes traffic to a worker, it doesn't carry identity. v1 switches
   to signed cookies when OIDC is added.

2. **Worker lifecycle on app stop:** immediate kill in v0. When
   `POST /apps/{id}/stop` is called, all workers are stopped without
   draining. Graceful drain is added in v1 alongside session sharing.

3. **No premature interface extraction.** SessionStore, WorkerRegistry,
   TaskStore, and LogStore start as concrete structs in their own packages.
   When v2 needs PostgreSQL or Redis backing for HA, interfaces are
   extracted at the consumer — any struct with matching methods already
   satisfies a Go interface.

4. **Bundle upload size limit:** 100 MiB default (`MaxBundleSize`),
   configurable. Enforced by reading at most `MaxBundleSize + 1` bytes from
   the request body — if the extra byte is read, reject with 413.

5. **Container startup command:** `WorkerSpec.Cmd` controls the container
   command. In v0, the server always sets this to a hardcoded default:
   `["R", "-e", "shiny::runApp('/app', port = as.integer(Sys.getenv('SHINY_PORT')))"]`.
   This works with stock rocker images that don't have a Shiny-specific
   entrypoint. `SHINY_PORT` is set from `DockerConfig.ShinyPort`. The mount
   point (`/app`) matches `BundleWorkerPath`. Making the command
   configurable is a potential future enhancement, not a v0 concern.

6. **rv installation in build containers:** the base image (e.g.
   `rocker/r-ver`) ships R but not rv. The build container downloads rv from
   GitHub releases as the first step of its command:
   `curl -sSL {url} -o /usr/local/bin/rv && chmod +x /usr/local/bin/rv && rv sync`.
   The `RvVersion` config field controls which release tag is used. Using
   the same base image for builds and workers guarantees matching R version,
   architecture, and system libraries.

7. **Library mount path:** `rv sync` writes packages under
   `rv/library/{R_VERSION}/{ARCH}/` inside the build container. After a
   successful build, the server discovers the exact subdirectory (e.g.
   `{bundle-id}_lib/4.4/x86_64/`) and mounts it into workers at
   `/blockyard-lib` (a fixed path outside the app directory) with
   `R_LIBS=/blockyard-lib`. This avoids conflicts with the read-only app
   mount at `/app` and lets R find packages directly without rv at runtime.

8. **App status is not persisted.** The `apps` table has no `status` column.
   Runtime status (running/stopped) is inferred from whether any workers
   exist for the app. This avoids staleness on crash/restart and eliminates
   synchronization between in-memory state and the DB.

9. **No `ON DELETE CASCADE` on `bundles.app_id`.** Deleting an app requires
   a multi-step teardown (stop workers, remove files, delete bundle rows,
   delete app row). The FK constraint prevents the DB from silently deleting
   bundles. The API handler orchestrates the full sequence; the FK enforces
   ordering.

10. **Circular FK between `apps` and `bundles`.** `apps.active_bundle`
    references `bundles.id`, and `bundles.app_id` references `apps.id`. This
    works because apps are created with `active_bundle = NULL` and the field
    is only set later when a bundle reaches `ready` status. No deferred
    constraints needed.
