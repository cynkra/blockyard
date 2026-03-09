# Phase 0-1: Foundation

Establish the project skeleton, core types, config parsing, and database
schema. Everything else builds on this. This phase produces a crate that
compiles and passes tests but does not start a server or talk to Docker.

## Deliverables

1. Crate skeleton with `src/lib.rs` + `src/main.rs`, feature flags
2. Config parsing (`config.rs`) — TOML + env var overlay
3. `Backend` trait + `WorkerSpec` + `BuildSpec` + handle types
4. Mock backend implementation (`backend/mock.rs`, behind `test-support`)
5. SQLite schema + migrations (`db/sqlite.rs`)
6. `AppState` struct that holds shared server state
7. Structured logging setup (`tracing` + `tracing-subscriber`)
8. GitHub Actions CI workflow

## Step-by-step

### Step 1: Crate skeleton

Create the project with `cargo init --name blockyard`. Set up `Cargo.toml`
with feature flags and dependencies. The crate has both `src/lib.rs` (public
API) and `src/main.rs` (entry point stub).

```toml
[package]
name = "blockyard"
version = "0.1.0"
edition = "2024"

[features]
default = ["docker"]
docker = ["dep:bollard"]
test-support = []

[dependencies]
tokio       = { version = "1", features = ["full"] }
axum        = { version = "0.8", features = ["ws"] }
hyper       = { version = "1", features = ["full"] }
hyper-util  = "0.1"
http-body-util = "0.1"
tower       = { version = "0.5", features = ["util"] }
bollard     = { version = "0.18", optional = true }
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
dashmap     = "6"
tempfile    = "3"

[dev-dependencies]
blockyard = { path = ".", features = ["test-support"] }
reqwest      = { version = "0.12", features = ["json", "cookies"] }
tokio-tungstenite = "0.26"
assert_matches = "1"
```

`src/lib.rs` — module declarations only:

```rust
pub mod config;
pub mod backend;
pub mod db;
pub mod app;
```

`src/main.rs` — placeholder:

```rust
use blockyard::config::Config;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .json()
        .init();

    let config = Config::load()?;
    tracing::info!("loaded config");

    // Server wiring comes in later phases.
    Ok(())
}
```

Also add `anyhow` to dependencies for `main.rs` error handling:

```toml
anyhow = "1"
```

### Step 2: Config parsing

`src/config.rs` — TOML deserialization with env var overlay.

```rust
use serde::Deserialize;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::time::Duration;

#[derive(Debug, Clone, Deserialize)]
pub struct Config {
    pub server: ServerConfig,
    #[serde(default)]
    pub docker: Option<DockerConfig>,   // required only when feature = "docker"
    pub storage: StorageConfig,
    pub database: DatabaseConfig,
    pub proxy: ProxyConfig,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ServerConfig {
    #[serde(default = "default_bind")]
    pub bind: SocketAddr,
    pub token: String,
    #[serde(default = "default_shutdown_timeout", with = "humantime_serde")]
    pub shutdown_timeout: Duration,
}

#[derive(Debug, Clone, Deserialize)]
pub struct DockerConfig {
    #[serde(default = "default_socket")]
    pub socket: String,
    pub image: String,
    #[serde(default = "default_shiny_port")]
    pub shiny_port: u16,
}

#[derive(Debug, Clone, Deserialize)]
pub struct StorageConfig {
    pub bundle_server_path: PathBuf,
    #[serde(default = "default_worker_path")]
    pub bundle_worker_path: PathBuf,
    #[serde(default = "default_retention")]
    pub bundle_retention: u32,
}

#[derive(Debug, Clone, Deserialize)]
pub struct DatabaseConfig {
    pub path: PathBuf,
}

#[derive(Debug, Clone, Deserialize)]
pub struct ProxyConfig {
    #[serde(default = "default_ws_cache_ttl", with = "humantime_serde")]
    pub ws_cache_ttl: Duration,
    #[serde(default = "default_health_interval", with = "humantime_serde")]
    pub health_interval: Duration,
    #[serde(default = "default_start_timeout", with = "humantime_serde")]
    pub worker_start_timeout: Duration,
    #[serde(default = "default_max_workers")]
    pub max_workers: u32,
}

// --- defaults ---

fn default_bind() -> SocketAddr { "0.0.0.0:8080".parse().unwrap() }
fn default_shutdown_timeout() -> Duration { Duration::from_secs(30) }
fn default_socket() -> String { "/var/run/docker.sock".into() }
fn default_shiny_port() -> u16 { 3838 }
fn default_worker_path() -> PathBuf { PathBuf::from("/app") }
fn default_retention() -> u32 { 50 }
fn default_ws_cache_ttl() -> Duration { Duration::from_secs(60) }
fn default_health_interval() -> Duration { Duration::from_secs(10) }
fn default_start_timeout() -> Duration { Duration::from_secs(60) }
fn default_max_workers() -> u32 { 100 }
```

**Duration handling:** the TOML config uses human-readable duration strings
(`"30s"`, `"60s"`, `"10s"`). The `humantime-serde` crate bridges serde and
`std::time::Duration` with human-readable parsing. Add to dependencies:

```toml
humantime-serde = "1"
```

**Loading and env var overlay:**

**Naming rule:** every config field maps to exactly one env var. The name is
derived mechanically: `BLOCKYARD_` + section name + `_` + field name, all
uppercased. Examples:

| Config path | Env var |
|---|---|
| `server.bind` | `BLOCKYARD_SERVER_BIND` |
| `storage.bundle_server_path` | `BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH` |
| `proxy.ws_cache_ttl` | `BLOCKYARD_PROXY_WS_CACHE_TTL` |

The mapping is deterministic and one-to-one. Two tests enforce this at
compile time:

- **Coverage:** every leaf field in `Config` (discovered via serde
  serialization) must have a corresponding entry in `supported_env_vars()`.
  Adding a config field without its env var entry fails this test.
- **Uniqueness:** no two entries in `supported_env_vars()` may be the same.
  Catches the case where different field paths collapse to the same env var
  name (e.g. a nested subsection whose path happens to match a flat field).

```rust
/// Returns the set of env var names that apply_env_overrides handles.
/// Used by the completeness test to verify no field is missing.
pub fn supported_env_vars() -> &'static [&'static str] {
    &[
        "BLOCKYARD_SERVER_BIND",
        "BLOCKYARD_SERVER_TOKEN",
        "BLOCKYARD_SERVER_SHUTDOWN_TIMEOUT",
        "BLOCKYARD_DOCKER_SOCKET",
        "BLOCKYARD_DOCKER_IMAGE",
        "BLOCKYARD_DOCKER_SHINY_PORT",
        "BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH",
        "BLOCKYARD_STORAGE_BUNDLE_WORKER_PATH",
        "BLOCKYARD_STORAGE_BUNDLE_RETENTION",
        "BLOCKYARD_DATABASE_PATH",
        "BLOCKYARD_PROXY_WS_CACHE_TTL",
        "BLOCKYARD_PROXY_HEALTH_INTERVAL",
        "BLOCKYARD_PROXY_WORKER_START_TIMEOUT",
        "BLOCKYARD_PROXY_MAX_WORKERS",
    ]
}

impl Config {
    /// Load config from file + env var overrides.
    /// File path: --config CLI arg, or ./blockyard.toml by default.
    pub fn load() -> Result<Self, ConfigError> {
        let path = std::env::args()
            .skip_while(|a| a != "--config")
            .nth(1)
            .unwrap_or_else(|| "blockyard.toml".into());

        let text = std::fs::read_to_string(&path)
            .map_err(|e| ConfigError::ReadFile(path.clone(), e))?;
        let mut config: Config = toml::from_str(&text)
            .map_err(ConfigError::Parse)?;

        config.apply_env_overrides();
        config.validate()?;
        Ok(config)
    }

    /// Override individual fields from BLOCKYARD_* env vars.
    fn apply_env_overrides(&mut self) {
        if let Ok(v) = std::env::var("BLOCKYARD_SERVER_BIND") {
            if let Ok(addr) = v.parse() { self.server.bind = addr; }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_SERVER_TOKEN") {
            self.server.token = v;
        }
        if let Ok(v) = std::env::var("BLOCKYARD_SERVER_SHUTDOWN_TIMEOUT") {
            if let Ok(d) = v.parse::<humantime::Duration>() { self.server.shutdown_timeout = d.into(); }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DOCKER_SOCKET") {
            if let Some(docker) = &mut self.docker { docker.socket = v; }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DOCKER_IMAGE") {
            if let Some(docker) = &mut self.docker { docker.image = v; }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DOCKER_SHINY_PORT") {
            if let (Some(docker), Ok(p)) = (&mut self.docker, v.parse()) { docker.shiny_port = p; }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH") {
            self.storage.bundle_server_path = PathBuf::from(v);
        }
        if let Ok(v) = std::env::var("BLOCKYARD_STORAGE_BUNDLE_WORKER_PATH") {
            self.storage.bundle_worker_path = PathBuf::from(v);
        }
        if let Ok(v) = std::env::var("BLOCKYARD_STORAGE_BUNDLE_RETENTION") {
            if let Ok(n) = v.parse() { self.storage.bundle_retention = n; }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_DATABASE_PATH") {
            self.database.path = PathBuf::from(v);
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_WS_CACHE_TTL") {
            if let Ok(d) = v.parse::<humantime::Duration>() { self.proxy.ws_cache_ttl = d.into(); }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_HEALTH_INTERVAL") {
            if let Ok(d) = v.parse::<humantime::Duration>() { self.proxy.health_interval = d.into(); }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_WORKER_START_TIMEOUT") {
            if let Ok(d) = v.parse::<humantime::Duration>() { self.proxy.worker_start_timeout = d.into(); }
        }
        if let Ok(v) = std::env::var("BLOCKYARD_PROXY_MAX_WORKERS") {
            if let Ok(n) = v.parse() { self.proxy.max_workers = n; }
        }
    }

    /// Validate config after all overrides are applied.
    fn validate(&self) -> Result<(), ConfigError> {
        if self.server.token.is_empty() {
            return Err(ConfigError::Validation(
                "server.token must not be empty".into()
            ));
        }
        #[cfg(feature = "docker")]
        {
            let docker = self.docker.as_ref().ok_or_else(|| {
                ConfigError::Validation("[docker] section required when docker feature is enabled".into())
            })?;
            if docker.image.is_empty() {
                return Err(ConfigError::Validation(
                    "docker.image must not be empty".into()
                ));
            }
        }
        Ok(())
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("failed to read config file '{0}': {1}")]
    ReadFile(String, std::io::Error),
    #[error("failed to parse config: {0}")]
    Parse(toml::de::Error),
    #[error("config validation failed: {0}")]
    Validation(String),
}
```

Add `humantime` (for parsing in overlay) alongside `humantime-serde` (for
serde deserialization):

```toml
humantime = "2"
```

**Tests (in `config.rs`):**

```rust
#[cfg(test)]
mod tests {
    use super::*;

    fn minimal_toml() -> &'static str {
        r#"
        [server]
        token = "test-token"

        [docker]
        image = "ghcr.io/blockr-org/blockr-r-base:latest"

        [storage]
        bundle_server_path = "/tmp/bundles"

        [database]
        path = "/tmp/blockyard.db"

        [proxy]
        "#
    }

    #[test]
    fn parse_minimal_config() {
        let config: Config = toml::from_str(minimal_toml()).unwrap();
        assert_eq!(config.server.bind, "0.0.0.0:8080".parse().unwrap());
        assert_eq!(config.server.token, "test-token");
        assert_eq!(config.proxy.max_workers, 100);
    }

    #[test]
    fn env_var_overrides_token() {
        let mut config: Config = toml::from_str(minimal_toml()).unwrap();
        std::env::set_var("BLOCKYARD_SERVER_TOKEN", "override-token");
        config.apply_env_overrides();
        std::env::remove_var("BLOCKYARD_SERVER_TOKEN");
        assert_eq!(config.server.token, "override-token");
    }

    #[test]
    fn validation_rejects_empty_token() {
        let mut config: Config = toml::from_str(minimal_toml()).unwrap();
        config.server.token = String::new();
        assert!(config.validate().is_err());
    }

    /// Verify every leaf field in Config has a corresponding BLOCKYARD_* env var.
    ///
    /// Serializes Config to a JSON value, recursively collects all leaf
    /// field paths (e.g. "server.bind", "proxy.max_workers"), converts
    /// each to the expected env var name (BLOCKYARD_SERVER_BIND, etc.), and
    /// asserts it appears in supported_env_vars().
    ///
    /// If you add a config field but forget to add its env var override,
    /// this test fails with a message telling you which env var is missing.
    #[test]
    fn env_var_coverage_complete() {
        let config: Config = toml::from_str(minimal_toml()).unwrap();
        let value = serde_json::to_value(&config).unwrap();

        let mut field_paths = Vec::new();
        collect_leaf_paths(&value, "", &mut field_paths);

        let supported: std::collections::HashSet<&str> =
            supported_env_vars().iter().copied().collect();

        let mut missing = Vec::new();
        for path in &field_paths {
            let env_var = format!("BLOCKYARD_{}", path.to_uppercase().replace('.', "_"));
            if !supported.contains(env_var.as_str()) {
                missing.push(format!("{env_var} (for config field '{path}')"));
            }
        }

        assert!(
            missing.is_empty(),
            "Config fields without env var support:\n  {}",
            missing.join("\n  ")
        );
    }

    /// Verify no two config fields map to the same env var name.
    #[test]
    fn env_var_names_unique() {
        let vars = supported_env_vars();
        let mut seen = std::collections::HashSet::new();
        let mut dupes = Vec::new();
        for var in vars {
            if !seen.insert(var) {
                dupes.push(*var);
            }
        }
        assert!(
            dupes.is_empty(),
            "Duplicate env var names:\n  {}",
            dupes.join("\n  ")
        );
    }

    /// Recursively collect dotted paths to all leaf (non-object) fields.
    fn collect_leaf_paths(
        value: &serde_json::Value,
        prefix: &str,
        out: &mut Vec<String>,
    ) {
        match value {
            serde_json::Value::Object(map) => {
                for (key, val) in map {
                    let path = if prefix.is_empty() {
                        key.clone()
                    } else {
                        format!("{prefix}.{key}")
                    };
                    collect_leaf_paths(val, &path, out);
                }
            }
            _ => {
                out.push(prefix.to_string());
            }
        }
    }
}
```

### Step 3: Backend trait

`src/backend/mod.rs` — trait definitions and associated types. No
implementation here; this is pure interface.

```rust
use std::net::SocketAddr;
use std::path::PathBuf;
use std::collections::HashMap;

pub mod docker;  // #[cfg(feature = "docker")]
pub mod mock;    // #[cfg(feature = "test-support")]

/// Pluggable container runtime. Docker/Podman for v0, Kubernetes for v2.
pub trait Backend: Send + Sync + 'static {
    type Handle: WorkerHandle;

    /// Spawn a long-lived worker (Shiny app container).
    async fn spawn(&self, spec: &WorkerSpec) -> Result<Self::Handle, BackendError>;

    /// Stop and remove a worker.
    async fn stop(&self, handle: &Self::Handle) -> Result<(), BackendError>;

    /// TCP or HTTP health check against the worker.
    async fn health_check(&self, handle: &Self::Handle) -> bool;

    /// Stream stdout/stderr logs from the worker.
    async fn logs(&self, handle: &Self::Handle) -> Result<LogStream, BackendError>;

    /// Resolve the worker's address (IP + Shiny port).
    async fn addr(&self, handle: &Self::Handle) -> Result<SocketAddr, BackendError>;

    /// Run a build task to completion (dependency restore).
    async fn build(&self, spec: &BuildSpec) -> Result<BuildResult, BackendError>;

    /// List all managed resources (containers + networks) for orphan cleanup.
    async fn list_managed(&self) -> Result<Vec<ManagedResource>, BackendError>;

    /// Remove an orphaned resource.
    async fn remove_resource(&self, resource: &ManagedResource) -> Result<(), BackendError>;
}

pub trait WorkerHandle: Send + Sync + Clone + std::fmt::Debug {
    fn id(&self) -> &str;
}

/// Everything a backend needs to launch a worker.
#[derive(Debug, Clone)]
pub struct WorkerSpec {
    pub app_id: String,
    pub worker_id: String,
    pub image: String,
    pub bundle_path: PathBuf,
    pub library_path: PathBuf,
    pub worker_mount: PathBuf,
    pub shiny_port: u16,
    pub memory_limit: Option<String>,
    pub cpu_limit: Option<f64>,
    pub labels: HashMap<String, String>,
}

/// Everything a backend needs to run a build task (dependency restore).
#[derive(Debug, Clone)]
pub struct BuildSpec {
    pub app_id: String,
    pub bundle_id: String,
    pub image: String,
    pub bundle_path: PathBuf,
    pub library_path: PathBuf,
    pub labels: HashMap<String, String>,
}

#[derive(Debug)]
pub struct BuildResult {
    pub success: bool,
    pub exit_code: Option<i64>,
}

/// A managed resource discovered during orphan cleanup.
#[derive(Debug)]
pub struct ManagedResource {
    pub id: String,
    pub kind: ResourceKind,
}

#[derive(Debug)]
pub enum ResourceKind {
    Container,
    Network,
}

/// A stream of log lines. Type alias — exact type TBD during implementation
/// (likely `Pin<Box<dyn Stream<Item = String> + Send>>`).
pub type LogStream = tokio::sync::mpsc::Receiver<String>;

#[derive(Debug, thiserror::Error)]
pub enum BackendError {
    #[error("spawn failed: {0}")]
    Spawn(String),
    #[error("stop failed: {0}")]
    Stop(String),
    #[error("build failed: {0}")]
    Build(String),
    #[error("address resolution failed: {0}")]
    Addr(String),
    #[error("log streaming failed: {0}")]
    Logs(String),
    #[error("cleanup failed: {0}")]
    Cleanup(String),
}
```

`src/backend/docker.rs` — stub behind feature flag:

```rust
//! Docker/Podman backend implementation.
//! Full implementation in phase 0-2.

#[cfg(feature = "docker")]
pub struct DockerBackend {
    // Fields added in phase 0-2
}
```

### Step 4: Mock backend

`src/backend/mock.rs` — behind `test-support` feature. Implements the full
`Backend` trait with configurable behavior for tests.

```rust
#[cfg(feature = "test-support")]
use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, AtomicU16, Ordering};
use dashmap::DashMap;
use crate::backend::*;

pub struct MockBackend {
    workers: DashMap<String, MockWorker>,
    pub health_response: AtomicBool,
    pub build_success: AtomicBool,
    next_port: AtomicU16,
}

#[derive(Debug, Clone)]
pub struct MockHandle {
    pub id: String,
    pub addr: SocketAddr,
}

impl WorkerHandle for MockHandle {
    fn id(&self) -> &str { &self.id }
}

struct MockWorker {
    handle: MockHandle,
    _listener: tokio::net::TcpListener,  // keeps the port open
}

impl MockBackend {
    pub fn new() -> Self {
        Self {
            workers: DashMap::new(),
            health_response: AtomicBool::new(true),
            build_success: AtomicBool::new(true),
            next_port: AtomicU16::new(19000),
        }
    }

    pub fn worker_count(&self) -> usize {
        self.workers.len()
    }

    pub fn has_worker(&self, id: &str) -> bool {
        self.workers.contains_key(id)
    }
}
```

The `Backend` impl spawns a `TcpListener` on localhost for each "worker",
allowing proxy tests to route real HTTP traffic end to end. `health_check`
returns the value of `health_response`. `build` succeeds or fails based on
`build_success`. `list_managed` and `remove_resource` are no-ops (mock has
no external state to leak).

**Tests for mock backend:**

```rust
#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn spawn_and_stop() {
        let backend = MockBackend::new();
        let spec = test_worker_spec("app-1", "worker-1");
        let handle = backend.spawn(&spec).await.unwrap();
        assert_eq!(backend.worker_count(), 1);
        assert!(backend.has_worker("worker-1"));

        backend.stop(&handle).await.unwrap();
        assert_eq!(backend.worker_count(), 0);
    }

    #[tokio::test]
    async fn health_check_configurable() {
        let backend = MockBackend::new();
        let spec = test_worker_spec("app-1", "worker-1");
        let handle = backend.spawn(&spec).await.unwrap();

        assert!(backend.health_check(&handle).await);

        backend.health_response.store(false, Ordering::SeqCst);
        assert!(!backend.health_check(&handle).await);
    }

    fn test_worker_spec(app_id: &str, worker_id: &str) -> WorkerSpec {
        WorkerSpec {
            app_id: app_id.into(),
            worker_id: worker_id.into(),
            image: "test:latest".into(),
            bundle_path: "/tmp/bundle".into(),
            library_path: "/tmp/lib".into(),
            worker_mount: "/app".into(),
            shiny_port: 3838,
            memory_limit: None,
            cpu_limit: None,
            labels: Default::default(),
        }
    }
}
```

### Step 5: SQLite schema + migrations

`src/db/mod.rs` — database setup and migration:

```rust
use sqlx::SqlitePool;

pub mod sqlite;

pub async fn create_pool(path: &std::path::Path) -> Result<SqlitePool, sqlx::Error> {
    // Create parent directory if it doesn't exist
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).ok();
    }
    let url = format!("sqlite://{}?mode=rwc", path.display());
    let opts = sqlx::sqlite::SqliteConnectOptions::from_str(&url)?
        .pragma("foreign_keys", "ON");
    let pool = SqlitePool::connect_with(opts).await?;
    run_migrations(&pool).await?;
    Ok(pool)
}

pub async fn run_migrations(pool: &SqlitePool) -> Result<(), sqlx::Error> {
    sqlx::migrate!().run(pool).await?;  // looks for ./migrations/ at crate root
    Ok(())
}
```

`migrations/001_initial.sql` (at crate root — `sqlx` convention):

```sql
CREATE TABLE IF NOT EXISTS apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    status                  TEXT NOT NULL DEFAULT 'stopped',
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

CREATE INDEX idx_bundles_app_id ON bundles(app_id);
```

`src/db/sqlite.rs` — query functions used by the API layer:

```rust
use sqlx::SqlitePool;
use uuid::Uuid;

/// App record as stored in SQLite.
#[derive(Debug, Clone, sqlx::FromRow, serde::Serialize)]
pub struct AppRow {
    pub id: String,
    pub name: String,
    pub status: String,
    pub active_bundle: Option<String>,
    pub max_workers_per_app: Option<i64>,
    pub max_sessions_per_worker: i64,
    pub memory_limit: Option<String>,
    pub cpu_limit: Option<f64>,
    pub created_at: String,
    pub updated_at: String,
}

/// Bundle record as stored in SQLite.
#[derive(Debug, Clone, sqlx::FromRow, serde::Serialize)]
pub struct BundleRow {
    pub id: String,
    pub app_id: String,
    pub status: String,
    pub path: String,
    pub uploaded_at: String,
}

pub async fn create_app(pool: &SqlitePool, name: &str) -> Result<AppRow, sqlx::Error> {
    let id = Uuid::new_v4().to_string();
    let now = chrono::Utc::now().to_rfc3339();
    sqlx::query_as::<_, AppRow>(
        "INSERT INTO apps (id, name, status, max_sessions_per_worker, created_at, updated_at)
         VALUES (?, ?, 'stopped', 1, ?, ?)
         RETURNING *"
    )
    .bind(&id)
    .bind(name)
    .bind(&now)
    .bind(&now)
    .fetch_one(pool)
    .await
}

pub async fn get_app(pool: &SqlitePool, id: &str) -> Result<Option<AppRow>, sqlx::Error> {
    sqlx::query_as::<_, AppRow>("SELECT * FROM apps WHERE id = ?")
        .bind(id)
        .fetch_optional(pool)
        .await
}

pub async fn get_app_by_name(pool: &SqlitePool, name: &str) -> Result<Option<AppRow>, sqlx::Error> {
    sqlx::query_as::<_, AppRow>("SELECT * FROM apps WHERE name = ?")
        .bind(name)
        .fetch_optional(pool)
        .await
}

pub async fn list_apps(pool: &SqlitePool) -> Result<Vec<AppRow>, sqlx::Error> {
    sqlx::query_as::<_, AppRow>("SELECT * FROM apps ORDER BY created_at DESC")
        .fetch_all(pool)
        .await
}

pub async fn delete_app(pool: &SqlitePool, id: &str) -> Result<bool, sqlx::Error> {
    let result = sqlx::query("DELETE FROM apps WHERE id = ?")
        .bind(id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected() > 0)
}

// Bundle queries follow the same pattern — insert, list by app, update status.
```

Add `chrono` to dependencies:

```toml
chrono = { version = "0.4", features = ["serde"] }
```

**Tests:**

```rust
#[cfg(test)]
mod tests {
    use super::*;

    async fn test_pool() -> SqlitePool {
        let pool = SqlitePool::connect(":memory:").await.unwrap();
        crate::db::run_migrations(&pool).await.unwrap();
        pool
    }

    #[tokio::test]
    async fn create_and_get_app() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        assert_eq!(app.name, "my-app");
        assert_eq!(app.status, "stopped");

        let fetched = get_app(&pool, &app.id).await.unwrap().unwrap();
        assert_eq!(fetched.id, app.id);
    }

    #[tokio::test]
    async fn duplicate_name_fails() {
        let pool = test_pool().await;
        create_app(&pool, "my-app").await.unwrap();
        assert!(create_app(&pool, "my-app").await.is_err());
    }

    #[tokio::test]
    async fn delete_app_removes_row() {
        let pool = test_pool().await;
        let app = create_app(&pool, "my-app").await.unwrap();
        assert!(delete_app(&pool, &app.id).await.unwrap());
        assert!(get_app(&pool, &app.id).await.unwrap().is_none());
    }
}
```

### Step 6: AppState

`src/app.rs` — shared server state, threaded through axum handlers via
`State`.

```rust
use std::sync::Arc;
use dashmap::DashMap;
use sqlx::SqlitePool;
use crate::backend::Backend;
use crate::config::Config;

/// Shared server state. Cloneable (all fields behind Arc).
/// Generic over the backend so tests can use MockBackend.
#[derive(Clone)]
pub struct AppState<B: Backend> {
    pub config: Arc<Config>,
    pub backend: Arc<B>,
    pub db: SqlitePool,
    /// Currently running workers, keyed by worker_id.
    pub workers: Arc<DashMap<String, ActiveWorker<B::Handle>>>,
}

/// A running worker tracked by the server.
#[derive(Debug, Clone)]
pub struct ActiveWorker<H: Clone> {
    pub app_id: String,
    pub handle: H,
    pub session_id: String,
}

impl<B: Backend> AppState<B> {
    pub fn new(config: Config, backend: B, db: SqlitePool) -> Self {
        Self {
            config: Arc::new(config),
            backend: Arc::new(backend),
            db,
            workers: Arc::new(DashMap::new()),
        }
    }
}
```

`AppState` intentionally does not hold `SessionStore`, `WorkerRegistry`, or
`TaskStore` yet — those are added in phases 0-3 and 0-5 when they are needed.
Adding fields early that nothing uses makes the struct harder to construct in
tests.

### Step 7: Structured logging

In `main.rs`, already shown in step 1. The setup is:

```rust
tracing_subscriber::fmt()
    .with_env_filter(
        tracing_subscriber::EnvFilter::try_from_default_env()
            .unwrap_or_else(|_| "blockyard=info".parse().unwrap())
    )
    .json()
    .init();
```

- **Default level:** `info` for `blockyard`, `warn` for everything else
- **Override:** set `RUST_LOG=blockyard=debug` for debug output
- **Format:** JSON lines (structured, parseable by log aggregators)

No additional work beyond what's in `main.rs`. The `tracing` crate is used
throughout the codebase via `tracing::info!`, `tracing::warn!`, etc.

## Example blockyard.toml

Ship this as the example config at the crate root:

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "change-me-in-production"
shutdown_timeout = "30s"

[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/blockr-org/blockr-r-base:latest"
shiny_port = 3838

[storage]
bundle_server_path = "/data/bundles"
bundle_worker_path = "/app"
bundle_retention   = 50

[database]
path = "/data/db/blockyard.db"

[proxy]
ws_cache_ttl         = "60s"
health_interval      = "10s"
worker_start_timeout = "60s"
max_workers          = 100
```

## Implementation notes

Things to keep in mind during implementation:

- **Circular FK between `apps` and `bundles`.** `apps.active_bundle`
  references `bundles.id`, and `bundles.app_id` references `apps.id`. This
  works because apps are created with `active_bundle = NULL` and the field
  is only set later when a bundle reaches `ready` status. No deferred
  constraints needed — the insert order (app first, bundle second, then
  update `active_bundle`) avoids the cycle naturally.

- **`create_pool` needs `use std::str::FromStr`** for
  `SqliteConnectOptions::from_str`. The code snippets in this doc are
  illustrative, not copy-paste complete — missing imports, error type
  conversions, etc. will be resolved during implementation.

- **Unused dependencies in phase 0-1.** The Cargo.toml lists dependencies
  for later phases (axum, hyper, tower, etc.) that nothing in this phase
  uses. This is intentional — one stable Cargo.toml across all phases avoids
  churn. Expect unused-import warnings during this phase; they go away as
  later phases land.

### Step 8: GitHub Actions CI

`.github/workflows/ci.yml` — runs on every push and pull request. Two jobs:
lint/test (always) and Docker integration tests (when the Docker backend
matters, starting from phase 0-2).

```yaml
name: CI
on:
  push:
    branches: [main]
  pull_request:

env:
  CARGO_TERM_COLOR: always
  RUSTFLAGS: "-D warnings"

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
        with:
          components: clippy, rustfmt
      - uses: Swatinem/rust-cache@v2
      - run: cargo fmt --check
      - run: cargo clippy --all-targets --features test-support
      - run: cargo test --features test-support

  docker-tests:
    runs-on: ubuntu-latest
    if: false  # enabled in phase 0-2 when Docker backend is implemented
    steps:
      - uses: actions/checkout@v4
      - uses: dtolnay/rust-toolchain@stable
      - uses: Swatinem/rust-cache@v2
      - run: cargo test --features docker-tests
```

**Notes:**

- **`Swatinem/rust-cache`** — caches `target/` and the cargo registry between
  runs. Cuts CI time significantly after the first run.
- **`RUSTFLAGS: -D warnings`** — treats all warnings as errors in CI. Keeps
  the codebase clean. This is set globally rather than per-command so clippy
  and test builds share the same flag.
- **`--all-targets`** on clippy — also lints tests and benchmarks, not just
  `src/`.
- **`docker-tests` job** — disabled (`if: false`) until phase 0-2. Flip to
  `if: true` or remove the condition when the Docker backend lands.
- **No caching of SQLite** — test DBs are in-memory (`:memory:`), nothing
  to cache.

## Exit criteria

Phase 0-1 is done when:

- `cargo build` succeeds (with default features)
- `cargo build --no-default-features --features test-support` succeeds
- `cargo test --features test-support` passes:
  - Config parsing + env var overlay + validation tests
  - Env var coverage + uniqueness tests
  - Mock backend spawn/stop/health_check tests
  - SQLite create/get/list/delete app tests
- `src/main.rs` loads config and initializes logging (does not start a server)
- The example `blockyard.toml` is valid and parseable
- CI passes on GitHub Actions (lint + test)
