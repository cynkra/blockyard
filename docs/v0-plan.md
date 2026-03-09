# blockr.cloud v0 Implementation Plan

This document is the build plan for v0 — the first working technical milestone.
It covers project scaffolding, crate layout, dependency graph, build phases,
key type definitions, and test strategy. The roadmap (`docs/roadmap.md`) is
the source of truth for *what* v0 includes; this document describes *how* to
build it.

## Crate Layout

A Cargo workspace with two crates.

```
blockr.cloud/
├── Cargo.toml              # workspace root
├── blockr-server/          # binary crate — main entry point
│   ├── Cargo.toml
│   └── src/
│       └── main.rs
├── blockr-cloud/           # library crate — all domain logic
│   ├── Cargo.toml
│   ├── src/
│   │   ├── lib.rs
│   │   ├── config.rs       # TOML config + env var overlay
│   │   ├── backend/
│   │   │   ├── mod.rs      # Backend trait, WorkerSpec, BuildSpec, handles
│   │   │   ├── docker.rs   # bollard-based Docker/Podman implementation
│   │   │   └── mock.rs     # in-process mock for tests
│   │   ├── db/
│   │   │   ├── mod.rs      # database trait
│   │   │   └── sqlite.rs   # SQLite implementation (sqlx)
│   │   ├── bundle/
│   │   │   ├── mod.rs      # bundle upload, storage layout, retention
│   │   │   └── restore.rs  # dependency restoration via backend.build()
│   │   ├── proxy/
│   │   │   ├── mod.rs      # HTTP/WS reverse proxy
│   │   │   ├── session.rs  # SessionStore trait + in-memory impl
│   │   │   ├── worker.rs   # WorkerRegistry trait + in-memory impl
│   │   │   ├── cold_start.rs   # hold-until-healthy logic
│   │   │   └── ws_cache.rs     # WS connection caching on disconnect
│   │   ├── api/
│   │   │   ├── mod.rs      # axum router assembly
│   │   │   ├── apps.rs     # CRUD + start/stop/logs endpoints
│   │   │   ├── bundles.rs  # upload + list endpoints
│   │   │   ├── tasks.rs    # task log streaming endpoint
│   │   │   └── auth.rs     # bearer token middleware
│   │   ├── health.rs       # /healthz handler + health polling loop
│   │   ├── task.rs         # TaskStore trait + in-memory impl
│   │   ├── cleanup.rs      # orphan cleanup on startup
│   │   └── app.rs          # AppState — shared server state
│   └── tests/              # integration tests
│       ├── api_test.rs
│       ├── proxy_test.rs
│       └── bundle_test.rs
├── blockr.toml             # example config
├── docs/
│   ├── roadmap.md
│   └── v0-plan.md          # this file
└── .github/
    └── workflows/
        └── ci.yml
```

**Why this split:**

- **`blockr-cloud`** is a library crate. All traits, implementations, and
  business logic live here. It can be tested with `cargo test` using the mock
  backend — no Docker required. Integration tests live in `blockr-cloud/tests/`
  — they start the full server with the mock backend and exercise the HTTP API
  end to end.
- **`blockr-server`** is a thin binary that wires `blockr-cloud` components
  together, parses CLI args, loads config, and starts the server. Almost no
  logic of its own.

## Dependencies

```toml
# blockr-cloud/Cargo.toml
[dependencies]
tokio       = { version = "1", features = ["full"] }
axum        = { version = "0.8", features = ["ws"] }
hyper       = { version = "1", features = ["full"] }
hyper-util  = "0.1"
http-body-util = "0.1"
tower       = { version = "0.5", features = ["util"] }
bollard     = "0.18"              # Docker API
sqlx        = { version = "0.8", features = ["runtime-tokio", "sqlite"] }
serde       = { version = "1", features = ["derive"] }
serde_json  = "1"
toml        = "0.8"
uuid        = { version = "1", features = ["v4"] }
tracing     = "0.1"
tracing-subscriber = { version = "0.3", features = ["env-filter", "json"] }
thiserror   = "2"
tokio-util  = { version = "0.7", features = ["io"] }
bytes       = "1"
dashmap     = "6"                 # concurrent maps for session/worker stores
tempfile    = "3"                 # atomic bundle writes

[dev-dependencies]
reqwest     = { version = "0.12", features = ["json", "cookies"] }
tokio-tungstenite = "0.26"        # WS client for proxy tests
assert_matches = "1"
```

**Dependency rationale:**

- **axum 0.8** — routing, middleware, extractors for the control plane API.
  Built on hyper and tower, so the proxy layer composes naturally.
- **hyper 1 + hyper-util** — direct HTTP client for proxying requests to
  workers. axum handles inbound routing; hyper handles outbound forwarding and
  WS upgrades.
- **bollard** — async Docker Engine API client. Handles container lifecycle,
  network management, log streaming, image pulling. Supports both Docker and
  Podman sockets.
- **sqlx** — compile-time checked SQL queries. SQLite for v0, PostgreSQL added
  in v2 by switching the connection pool type. No ORM.
- **dashmap** — lock-free concurrent `HashMap`. Used for `SessionStore` and
  `WorkerRegistry` in-memory implementations instead of `Arc<RwLock<HashMap>>`.
  Lower contention under concurrent proxy traffic.

## Build Phases

The dependency graph dictates build order. Each phase produces a testable
artifact. Phases are sequential; items within a phase can be worked in
parallel.

### Phase 1: Foundation

Establish the project skeleton, core types, config parsing, and database
schema. Everything else builds on this.

**Deliverables:**

1. Cargo workspace with `blockr-server` and `blockr-cloud`
2. Config parsing (`config.rs`) — TOML + env var overlay
3. `Backend` trait + `WorkerSpec` + `BuildSpec` + handle types
4. Mock backend implementation (for tests)
5. SQLite schema + migrations (`db/sqlite.rs`)
6. `AppState` struct that holds shared server state
7. Structured logging setup (`tracing` + `tracing-subscriber`)

**Config structure:**

```rust
#[derive(Debug, Deserialize)]
pub struct Config {
    pub server: ServerConfig,
    pub docker: DockerConfig,
    pub storage: StorageConfig,
    pub database: DatabaseConfig,
    pub proxy: ProxyConfig,
}

#[derive(Debug, Deserialize)]
pub struct ServerConfig {
    #[serde(default = "default_bind")]
    pub bind: SocketAddr,               // default: 0.0.0.0:8080
    pub token: String,                  // bearer token for control plane
    #[serde(default = "default_shutdown_timeout")]
    pub shutdown_timeout: Duration,     // default: 30s
}

#[derive(Debug, Deserialize)]
pub struct DockerConfig {
    #[serde(default = "default_socket")]
    pub socket: String,                 // default: /var/run/docker.sock
    pub image: String,                  // e.g. ghcr.io/blockr-org/blockr-r-base:latest
    #[serde(default = "default_shiny_port")]
    pub shiny_port: u16,                // default: 3838
}

#[derive(Debug, Deserialize)]
pub struct StorageConfig {
    pub bundle_server_path: PathBuf,    // where server reads/writes bundles
    #[serde(default = "default_worker_path")]
    pub bundle_worker_path: PathBuf,    // mount point inside workers; default: /app
    #[serde(default = "default_retention")]
    pub bundle_retention: u32,          // default: 50
}

#[derive(Debug, Deserialize)]
#[derive(Debug, Deserialize)]
pub struct DatabaseConfig {
    pub path: PathBuf,                  // e.g. /data/db/blockr.db
}

pub struct ProxyConfig {
    #[serde(default = "default_ws_cache_ttl")]
    pub ws_cache_ttl: Duration,         // default: 60s
    #[serde(default = "default_health_interval")]
    pub health_interval: Duration,      // default: 10s
    #[serde(default = "default_start_timeout")]
    pub worker_start_timeout: Duration, // default: 60s
    #[serde(default = "default_max_workers")]
    pub max_workers: u32,               // default: 100
}
```

**Env var overlay:** each config field can be overridden by an env var. The
convention is `BLOCKR_` + section + `_` + field, uppercased:

```
BLOCKR_SERVER_BIND, BLOCKR_SERVER_TOKEN, BLOCKR_DOCKER_IMAGE, etc.
```

Implementation: deserialize TOML first, then walk the struct and override any
field that has a matching env var set. Use a derive macro or a manual overlay
function — not a config framework. The overlay is explicit and testable.

**Backend trait (full v0 surface):**

```rust
use std::net::SocketAddr;

// Native async traits (Rust 1.75+) — no #[async_trait] macro needed.
pub trait Backend: Send + Sync + 'static {
    type Handle: WorkerHandle;

    /// Spawn a long-lived worker (Shiny app container).
    async fn spawn(&self, spec: &WorkerSpec) -> Result<Self::Handle>;

    /// Stop and remove a worker.
    async fn stop(&self, handle: &Self::Handle) -> Result<()>;

    /// TCP or HTTP health check against the worker.
    async fn health_check(&self, handle: &Self::Handle) -> bool;

    /// Stream stdout/stderr logs from the worker.
    async fn logs(&self, handle: &Self::Handle) -> Result<LogStream>;

    /// Resolve the worker's address (IP + Shiny port).
    async fn addr(&self, handle: &Self::Handle) -> Result<SocketAddr>;

    /// Run a build task to completion (dependency restore).
    /// Streams logs, returns success/failure, cleans up the build container.
    async fn build(&self, spec: &BuildSpec) -> Result<BuildResult>;

    /// List all managed resources (containers + networks) for orphan cleanup.
    async fn list_managed(&self) -> Result<Vec<ManagedResource>>;

    /// Remove an orphaned resource by ID.
    async fn remove_resource(&self, resource: &ManagedResource) -> Result<()>;
}

pub trait WorkerHandle: Send + Sync + Clone + std::fmt::Debug {
    fn id(&self) -> &str;
}
```

**WorkerSpec:**

```rust
pub struct WorkerSpec {
    pub app_id: String,
    pub worker_id: String,
    pub image: String,
    pub bundle_path: PathBuf,       // server-side path to unpacked bundle
    pub library_path: PathBuf,      // server-side path to restored R library
    pub worker_mount: PathBuf,      // in-container mount point (bundle_worker_path)
    pub shiny_port: u16,
    pub memory_limit: Option<String>,
    pub cpu_limit: Option<f64>,
    pub labels: HashMap<String, String>,
}
```

**BuildSpec:**

```rust
pub struct BuildSpec {
    pub app_id: String,
    pub bundle_id: String,
    pub image: String,
    pub bundle_path: PathBuf,       // server-side path to unpacked bundle
    pub library_path: PathBuf,      // server-side output path for restored library
    pub labels: HashMap<String, String>,
}

pub struct BuildResult {
    pub success: bool,
    pub exit_code: Option<i64>,
}
```

**Mock backend:**

```rust
/// In-process mock backend for unit and integration tests.
/// Does not start real containers. Tracks spawned/stopped workers
/// in memory and exposes them for test assertions.
pub struct MockBackend {
    workers: DashMap<String, MockWorker>,
    /// Configurable: should health_check return true or false?
    pub health_response: AtomicBool,
    /// Configurable: should build succeed or fail?
    pub build_success: AtomicBool,
    /// Bound port for the mock HTTP server each "worker" runs.
    next_port: AtomicU16,
}
```

The mock backend spawns a tiny `tokio::net::TcpListener` per "worker" that
responds with 200 on any request. This lets proxy tests route real HTTP
traffic through the proxy to the mock worker without Docker.

**AppState (shared server state):**

```rust
pub struct AppState<B: Backend> {
    pub config: Arc<Config>,
    pub backend: Arc<B>,
    pub db: SqlitePool,
    pub session_store: Arc<InMemorySessionStore>,
    pub worker_registry: Arc<InMemoryWorkerRegistry>,
    pub task_store: Arc<InMemoryTaskStore>,
    /// Currently running workers, keyed by worker_id.
    /// Maps worker_id → (app_id, handle, session_id).
    pub workers: Arc<DashMap<String, ActiveWorker<B::Handle>>>,
}
```

### Phase 2: Docker Backend + Network Isolation

Implement the `Backend` trait for Docker using `bollard`. This is the only
production backend for v0.

**Deliverables:**

1. `DockerBackend` — full `Backend` trait implementation
2. Per-container bridge network creation and cleanup
3. Server joins each worker's network (multi-homing)
4. Container hardening (cap-drop, read-only fs, no-new-privileges)
5. Image pulling (at startup and before each build/spawn)
6. Label management (`dev.blockr.cloud/*`)

**Docker backend — key operations:**

`spawn()` flow:
```
1. Create a user-defined bridge network: blockr-{session-id}
   Labels: dev.blockr.cloud/managed=true, app-id, worker-id
2. Create container:
   - Image: spec.image
   - Network: the bridge just created
   - Mounts: bundle → worker_mount (ro), library → worker_mount/lib (ro)
   - Tmpfs: /tmp
   - CapDrop: ALL
   - SecurityOpt: no-new-privileges
   - ReadonlyRootfs: true
   - Labels: dev.blockr.cloud/managed=true, app-id, worker-id
   - No published ports
3. Connect the server's own container to the new bridge network
   (bollard: network_connect with the server's container ID)
4. Start the container
5. Return DockerHandle { container_id, network_id }
```

`addr()` flow:
```
1. Inspect the container
2. Extract the IP address on the worker's named network
   (not just any IP — specifically the IP on blockr-{session-id})
3. Return SocketAddr(ip, spec.shiny_port)
```

`stop()` flow:
```
1. Stop the container (with timeout)
2. Remove the container
3. Disconnect the server from the worker's bridge network
4. Remove the bridge network
```

`build()` flow:
```
1. Create container with:
   - Image: spec.image
   - Cmd: ["rv", "restore"]
   - Working dir: /app
   - Mounts:
     - bundle → /app (ro)
     - library output dir → /app/lib (rw) — this is the only writable mount
   - Same hardening as workers, except library mount is rw
   - Labels: dev.blockr.cloud/managed=true, app-id, bundle-id
   - AutoRemove: true
2. Start container
3. Attach to container stdout/stderr, stream logs to TaskStore
4. Wait for container to exit
5. Return BuildResult { success, exit_code }
```

**Server container ID detection:** the server needs to know its own container
ID to join worker networks. Detection order:

1. Read `/proc/self/cgroup` — Docker writes the container ID in cgroup paths
2. Read hostname — Docker sets it to the short container ID by default
3. `BLOCKR_CONTAINER_ID` env var — explicit override for non-standard setups
4. If all fail and Docker socket is reachable: assume native binary mode
   (not running in a container); skip network joining, workers are reachable
   on the bridge gateway IP

### Phase 3: Content Management

Bundle upload, dependency restoration, content registry. These form the
deployment pipeline — the path from "user has a tar.gz" to "app is ready to
run."

**Deliverables:**

1. Bundle upload endpoint (`POST /api/v1/apps/{id}/bundles`)
2. Bundle storage layout — atomic writes, retention cleanup
3. Dependency restoration via `backend.build()`
4. `TaskStore` trait + in-memory implementation
5. Task log streaming endpoint (`GET /api/v1/tasks/{task_id}/logs`)
6. Content registry — SQLite CRUD for apps and bundles

**Bundle upload flow:**

```
POST /api/v1/apps/{id}/bundles
Authorization: Bearer <token>
Content-Type: application/octet-stream
Body: <tar.gz bytes>

→ 202 Accepted
{
  "bundle_id": "b1234...",
  "task_id": "t5678..."
}
```

Server-side:
```
1. Validate app exists
2. Generate bundle_id (UUID) and task_id (UUID)
3. Write tar.gz to temp file in bundle_server_path/{app-id}/
4. Atomically rename to {bundle-id}.tar.gz
5. Unpack tar.gz to {bundle-id}/ directory
6. Insert bundle row: status = "pending"
7. Create task in TaskStore
8. Spawn async restore task (does not block the response)
9. Return 202 with bundle_id and task_id
```

Async restore task:
```
1. Call backend.build(BuildSpec { ... })
2. Stream build logs to TaskStore
3. On success:
   - Update bundle status → "ready"
   - Set app.active_bundle → this bundle
4. On failure:
   - Update bundle status → "failed"
   - Leave app.active_bundle unchanged
```

**Task log streaming:**

```
GET /api/v1/tasks/{task_id}/logs
Authorization: Bearer <token>

→ 200 OK
Content-Type: text/plain
Transfer-Encoding: chunked

Installing packages...
  pak::pkg_install("dplyr")
  ...
```

Uses `axum::body::Body::from_stream()` with a `tokio::sync::broadcast`
channel. The build process writes log lines to the channel; the HTTP handler
reads from it. If the caller connects after the build started, they get
buffered output (stored in the `TaskStore`) followed by live streaming.

**TaskStore:**

```rust
pub trait TaskStore: Send + Sync + 'static {
    /// Create a new task. Returns a sender for writing log output.
    fn create(&self, task_id: TaskId) -> TaskSender;

    /// Get the current state of a task.
    async fn get(&self, task_id: &TaskId) -> Option<TaskState>;

    /// Get a stream of log output (buffered + live).
    async fn log_stream(&self, task_id: &TaskId) -> Option<LogStream>;

    /// Mark a task as completed (success or failure).
    async fn complete(&self, task_id: &TaskId, success: bool);
}

pub struct TaskState {
    pub id: TaskId,
    pub status: TaskStatus,     // running | completed | failed
    pub created_at: DateTime,
}

pub enum TaskStatus {
    Running,
    Completed,
    Failed,
}
```

In-memory implementation: a `DashMap<TaskId, TaskEntry>` where each entry
holds a `Vec<String>` of buffered log lines and a
`tokio::sync::broadcast::Sender<String>` for live subscribers.

### Phase 4: REST API + Auth

The control plane HTTP API. All endpoints under `/api/v1/`. Protected by
static bearer token.

**Deliverables:**

1. axum router with all v0 endpoints
2. Bearer token middleware (from `[server] token` config)
3. `/healthz` endpoint (unauthenticated)
4. App CRUD endpoints
5. Bundle endpoints
6. App lifecycle endpoints (start/stop)
7. App log streaming endpoint

**Router assembly:**

```rust
pub fn api_router<B: Backend>(state: AppState<B>) -> Router {
    let authed = Router::new()
        .route("/apps", post(create_app).get(list_apps))
        .route("/apps/{id}", get(get_app).patch(update_app).delete(delete_app))
        .route("/apps/{id}/bundles", post(upload_bundle).get(list_bundles))
        .route("/apps/{id}/start", post(start_app))
        .route("/apps/{id}/stop", post(stop_app))
        .route("/apps/{id}/logs", get(app_logs))
        .route("/tasks/{task_id}/logs", get(task_logs))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            bearer_auth,
        ));

    Router::new()
        .nest("/api/v1", authed)
        .route("/healthz", get(healthz))
        .with_state(state)
}
```

**Bearer auth middleware:**

```rust
async fn bearer_auth<B: Backend>(
    State(state): State<AppState<B>>,
    req: Request,
    next: Next,
) -> Result<Response, StatusCode> {
    let token = req.headers()
        .get(AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "));

    match token {
        Some(t) if t == state.config.server.token => Ok(next.run(req).await),
        _ => Err(StatusCode::UNAUTHORIZED),
    }
}
```

**Endpoint behaviors:**

| Endpoint | Method | Behavior |
|---|---|---|
| `/api/v1/apps` | POST | Create app. Body: `{ "name": "..." }`. Returns app object with generated UUID. Name must be unique, URL-safe slug. |
| `/api/v1/apps` | GET | List all apps. Returns array of app objects. |
| `/api/v1/apps/{id}` | GET | Get app details including active bundle, status, config. |
| `/api/v1/apps/{id}` | PATCH | Update app config (resource limits, worker scaling). Body: partial app object. |
| `/api/v1/apps/{id}` | DELETE | Delete app. Stops all workers, removes bundles from disk, deletes DB rows. |
| `/api/v1/apps/{id}/bundles` | POST | Upload bundle. Body: tar.gz bytes. Returns 202 with `bundle_id` + `task_id`. |
| `/api/v1/apps/{id}/bundles` | GET | List bundles for app. Returns array of bundle objects. |
| `/api/v1/apps/{id}/start` | POST | Start app. No-op if already running. Creates initial worker if not already spawned (workers are also started on-demand by the proxy). |
| `/api/v1/apps/{id}/stop` | POST | Stop app. Stops all workers, cleans up networks. |
| `/api/v1/apps/{id}/logs` | GET | Stream app logs. Query params: `worker_id` (optional), `follow` (bool). Uses chunked transfer encoding. |
| `/api/v1/tasks/{task_id}/logs` | GET | Stream task logs. Chunked plain text. |
| `/healthz` | GET | Returns 200 OK. No auth. No dependency checks. |

**Error responses** follow a consistent JSON shape:

```json
{
  "error": "app_not_found",
  "message": "No app with ID a3f2c1..."
}
```

HTTP status codes: 400 (bad request / validation), 401 (missing/invalid token),
404 (not found), 409 (conflict, e.g. duplicate app name), 503 (max workers
reached).

### Phase 5: Proxy Layer

The HTTP/WebSocket reverse proxy that serves Shiny apps to end users. This is
the data plane — separate from the control plane API but served by the same
HTTP listener.

**Deliverables:**

1. HTTP reverse proxy — forward requests to worker containers
2. WebSocket upgrade and forwarding
3. Session cookie management (set on first request, read on subsequent)
4. `SessionStore` + `WorkerRegistry` (in-memory implementations)
5. Cold-start hold — block until worker healthy or timeout
6. WebSocket connection caching on client disconnect
7. Trailing-slash redirect (`/app/{name}` → `/app/{name}/`)
8. Prefix stripping (remove `/app/{name}` before forwarding to worker)

**Request flow — first visit to `/app/my-app/`:**

```
1. Extract app name from URL path
2. No session cookie → generate session_id (UUID)
3. Look up app in DB → get active_bundle, status
4. Check global worker count < max_workers → 503 if at ceiling
5. Check app worker count < max_workers_per_app → 503 if at ceiling
6. Call backend.spawn(WorkerSpec { ... })
7. Register worker in WorkerRegistry
8. Pin session in SessionStore: session_id → worker_id
9. Poll backend.health_check(handle) every 500ms
   - If healthy within worker_start_timeout → continue
   - If timeout → stop worker, return 503
10. Get worker address via backend.addr(handle)
11. Set session cookie (session_id, path=/app/{name}/)
12. Forward request to worker (strip /app/{name} prefix)
13. Return response to client
```

**Request flow — subsequent visit (session cookie present):**

```
1. Extract session_id from cookie
2. SessionStore.get(session_id) → worker_id
3. WorkerRegistry.addr(worker_id) → SocketAddr
4. Forward request to worker
5. Return response
```

**WebSocket upgrade flow:**

```
1. Detect Upgrade: websocket + Connection: upgrade headers
2. Route to worker as above (session cookie lookup)
3. Establish backend WS connection to worker
4. Relay frames bidirectionally: client ↔ server ↔ worker
5. On client disconnect:
   - Do NOT close the backend WS connection
   - Start ws_cache_ttl timer (default 60s)
   - If client reconnects with same session_id → reattach
   - If timer expires → close backend WS, call backend.stop()
```

**SessionStore + WorkerRegistry:**

```rust
pub trait SessionStore: Send + Sync + 'static {
    async fn get(&self, session_id: &str) -> Option<WorkerId>;
    async fn insert(&self, session_id: &str, worker_id: WorkerId);
    async fn remove(&self, session_id: &str);
}

pub trait WorkerRegistry: Send + Sync + 'static {
    async fn addr(&self, worker_id: &WorkerId) -> Option<SocketAddr>;
    async fn insert(&self, worker_id: WorkerId, addr: SocketAddr);
    async fn remove(&self, worker_id: &WorkerId);
}

// In-memory implementations: DashMap wrappers. Trivial.
```

**Proxy handler integration with axum:**

The proxy is a fallback route in the main router — anything not matched by
`/api/v1/*` or `/healthz` falls through to the proxy:

```rust
pub fn server_router<B: Backend>(state: AppState<B>) -> Router {
    Router::new()
        .nest("/api/v1", api_router(state.clone()))
        .route("/healthz", get(healthz))
        .fallback(proxy_handler)
        .with_state(state)
}
```

The proxy handler extracts the app name from the path, validates the
`/app/{name}/` prefix, and handles routing. Any request outside `/app/*/` that
isn't an API or health route gets 404.

### Phase 6: Health Polling + Orphan Cleanup + Log Capture

Operational concerns that run alongside the main server.

**Deliverables:**

1. Background health polling loop
2. Orphan cleanup on startup
3. App log capture and streaming

**Health polling:**

A `tokio::spawn`ed loop that runs every `health_interval` (default 10s):

```rust
async fn health_poll_loop<B: Backend>(state: AppState<B>) {
    let mut interval = tokio::time::interval(state.config.proxy.health_interval);
    loop {
        interval.tick().await;
        let workers: Vec<_> = state.workers.iter()
            .map(|entry| (entry.key().clone(), entry.value().clone()))
            .collect();

        for (worker_id, active) in workers {
            let healthy = state.backend.health_check(&active.handle).await;
            if !healthy {
                tracing::warn!(worker_id, app_id = active.app_id, "worker unhealthy");
                // Stop the unhealthy worker
                let _ = state.backend.stop(&active.handle).await;
                state.workers.remove(&worker_id);
                state.worker_registry.remove(&WorkerId(worker_id)).await;
                // Session cleanup happens on next request — the proxy will
                // spawn a new worker when the session cookie hits a missing
                // worker.
            }
        }
    }
}
```

**Orphan cleanup:**

Runs once at server startup, before accepting connections:

```rust
async fn cleanup_orphans<B: Backend>(backend: &B) -> Result<()> {
    let managed = backend.list_managed().await?;
    for resource in managed {
        tracing::info!(id = resource.id, kind = ?resource.kind, "removing orphan");
        backend.remove_resource(&resource).await?;
    }
    Ok(())
}
```

`list_managed()` in the Docker backend queries for containers and networks
with `dev.blockr.cloud/managed=true` label. All are removed unconditionally —
the server starts with a clean slate.

**App log capture:**

Container logs are captured via `backend.logs(handle)`, which returns a
`LogStream` (a stream of log lines with timestamps). The proxy stores recent
logs in a bounded ring buffer per worker. The REST API endpoint
`GET /api/v1/apps/{id}/logs` reads from this buffer (historical) and
optionally follows live output via `backend.logs()`.

Log persistence beyond the buffer is deferred — in v0, logs are available
while the worker is running and for a short window after it stops. This is
sufficient for development; production log aggregation should use Docker's
native log drivers (json-file, journald, etc.).

## Graceful Shutdown

The `main.rs` wiring handles shutdown:

```rust
async fn main() {
    // ... config, state setup ...

    let listener = TcpListener::bind(&config.server.bind).await?;

    // Graceful shutdown signal
    let shutdown = async {
        tokio::signal::ctrl_c().await.ok();
        // or SIGTERM via tokio::signal::unix::signal(SignalKind::terminate())
    };

    // Start server with graceful shutdown
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown)
        .await?;

    // After listener closes: drain, stop workers, cleanup
    shutdown_workers(&state).await;
    // Flush logs, close DB
}
```

Shutdown sequence (from roadmap):
1. Stop accepting new connections (axum handles this)
2. Drain in-flight requests (up to `shutdown_timeout`)
3. Stop all managed containers and networks
4. Stop in-progress build containers, mark bundles as `failed`
5. Flush logs, close DB

## Build Order and Dependency Graph

```
Phase 1: Foundation
  ├── Config parsing
  ├── Backend trait + mock
  ├── SQLite schema
  └── AppState

Phase 2: Docker Backend
  └── depends on: Backend trait, Config

Phase 3: Content Management
  ├── Bundle upload + storage
  ├── TaskStore + restore pipeline
  └── depends on: Backend (build), SQLite (content registry)

Phase 4: REST API + Auth
  └── depends on: Content Management, AppState

Phase 5: Proxy Layer
  ├── Session routing
  ├── Cold-start + WS caching
  └── depends on: Backend (spawn/stop/addr), Session/Worker stores

Phase 6: Operations
  ├── Health polling
  ├── Orphan cleanup
  ├── Log capture
  └── depends on: Backend, Worker tracking
```

Phases 3 and 5 are independent of each other — they can be developed in
parallel. Both depend on Phase 2 only for integration testing; their unit
tests use the mock backend from Phase 1.

## Test Strategy

Three levels:

### Unit tests (in-crate `#[cfg(test)]` modules)

- **Mock backend tests:** verify that `spawn`, `stop`, `build` update internal
  state correctly. No I/O.
- **Config tests:** TOML parsing, env var overlay, validation errors.
- **Bundle storage tests:** write/unpack/retention logic using `tempdir`.
- **Session/worker store tests:** concurrent insert/get/remove.
- **Task store tests:** create, write logs, stream, complete.
- **Auth middleware tests:** valid token, missing token, wrong token.

### Integration tests (`blockr-cloud/tests/` directory)

These start the full server with the mock backend and exercise HTTP endpoints:

- **API tests:** CRUD apps, upload bundles, start/stop, list — all via
  `reqwest` against the running server.
- **Proxy tests:** the mock backend spawns tiny HTTP servers. Tests verify
  that requests to `/app/{name}/` are forwarded correctly, session cookies
  are set, WS upgrades work, cold-start holding works.
- **Bundle flow tests:** upload → restore (mock build) → bundle ready →
  app startable.

Test helper:

```rust
async fn spawn_test_server() -> (SocketAddr, AppState<MockBackend>) {
    let config = test_config();
    let backend = MockBackend::new();
    let db = SqlitePool::connect(":memory:").await.unwrap();
    run_migrations(&db).await.unwrap();
    let state = AppState::new(config, backend, db);
    let app = server_router(state.clone());
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(axum::serve(listener, app).into_future());
    (addr, state)
}
```

### Docker integration tests (gated behind `#[cfg(feature = "docker-tests")]`)

Tests that exercise the real Docker backend. Require a running Docker daemon.
Run in CI with Docker-in-Docker or on a dev machine with Docker installed.
Gated behind a feature flag so `cargo test` works without Docker.

- **Spawn and stop a container** — verify it appears in `docker ps`, responds
  to health checks, and is removed after stop.
- **Network isolation** — spawn two containers, verify they cannot reach each
  other.
- **Build (rv restore)** — run a real restore with a minimal `rv.lock`.
- **Orphan cleanup** — create labeled containers/networks externally, run
  cleanup, verify they are removed.

## CI

A single GitHub Actions workflow:

```yaml
name: CI
on: [push, pull_request]

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
        with:
          components: clippy, rustfmt
      - run: cargo fmt --check
      - run: cargo clippy -- -D warnings
      - run: cargo test

  docker-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
      - run: cargo test --features docker-tests
```

## Design Decisions

Resolved decisions that came up during planning:

1. **Session cookie format:** plain UUID in a `Set-Cookie` header. No HMAC
   signing in v0 — there is no user auth on the app plane, so the cookie
   only routes traffic to a worker, it doesn't carry identity. v1 switches
   to signed cookies when OIDC is added.

2. **Worker lifecycle on app stop:** immediate kill in v0. When
   `POST /apps/{id}/stop` is called, all workers are stopped without
   draining. Graceful drain is added in v1 alongside session sharing.

3. **Proxy concurrency model:** a single shared `hyper::Client` with
   connection pooling. One session per worker in v0 means there is no
   multiplexing benefit from per-worker clients. Revisit if v1 load
   balancing shows contention.

4. **Bundle upload size limit:** 100MB default, configurable via
   `[storage] max_bundle_size`. Large bundles with vendored packages
   could exceed this — operators can raise it.

5. **Container startup command:** the Docker image's entrypoint runs Shiny
   on a port specified by the `SHINY_PORT` env var. The server sets
   `SHINY_PORT` from `[docker] shiny_port` in the container spec. Image
   entrypoint: `R -e "shiny::runApp('/app', port = as.integer(Sys.getenv('SHINY_PORT')))"`.
   The mount point (`/app`) matches `bundle_worker_path`.
