# Phase 0-3: Content Management

Bundle upload, dependency restoration, content registry. These form the
deployment pipeline — the path from "user has a tar.gz" to "app is ready to
run."

## Deliverables

1. Bundle upload endpoint (`POST /api/v1/apps/{id}/bundles`)
2. Bundle storage — atomic writes, tar.gz unpacking, retention cleanup
3. Dependency restoration via `backend.build()`
4. `TaskStore` trait + in-memory implementation
5. Task log streaming endpoint (`GET /api/v1/tasks/{task_id}/logs`)
6. Content registry — SQLite queries for app and bundle lifecycle
7. Image pulling — `DockerBackend::ensure_image()` called before build/spawn
8. Bundle size limit — `max_bundle_size` config field + enforcement via
   axum `DefaultBodyLimit` + 413 response on oversized uploads
9. Minimal axum router — enough to serve the bundle upload, task logs, and
   healthz endpoints (full API wiring is phase 0-4, but we need something
   runnable to test the deployment pipeline end-to-end)

## Step-by-step

### Step 1: `TaskStore` trait + in-memory implementation

The task store tracks async background jobs (currently just dependency
restoration). It holds buffered log output and a broadcast channel for live
subscribers. This is the foundation for the restore pipeline — build it first
so the restore step has somewhere to write logs.

`src/task.rs`:

```rust
use std::sync::Arc;
use dashmap::DashMap;
use tokio::sync::broadcast;

pub type TaskId = String;

#[derive(Debug, Clone, serde::Serialize)]
pub struct TaskState {
    pub id: TaskId,
    pub status: TaskStatus,
    pub created_at: String,
}

#[derive(Debug, Clone, PartialEq, serde::Serialize)]
#[serde(rename_all = "lowercase")]
pub enum TaskStatus {
    Running,
    Completed,
    Failed,
}

/// Sender handle returned by TaskStore::create(). Used to write log lines
/// and mark the task complete.
pub struct TaskSender {
    id: TaskId,
    tx: broadcast::Sender<String>,
    buffer: Arc<tokio::sync::Mutex<Vec<String>>>,
}

impl TaskSender {
    /// Append a log line. Buffered for late subscribers and broadcast to
    /// any current live subscribers.
    pub async fn send(&self, line: String) {
        self.buffer.lock().await.push(line.clone());
        let _ = self.tx.send(line); // ignore if no receivers
    }

    /// Mark the task as completed or failed.
    pub async fn complete(self, store: &InMemoryTaskStore, success: bool) {
        let status = if success { TaskStatus::Completed } else { TaskStatus::Failed };
        if let Some(mut entry) = store.tasks.get_mut(&self.id) {
            entry.status = status;
        }
    }
}

struct TaskEntry {
    status: TaskStatus,
    created_at: String,
    buffer: Arc<tokio::sync::Mutex<Vec<String>>>,
    tx: broadcast::Sender<String>,
}

pub struct InMemoryTaskStore {
    tasks: DashMap<TaskId, TaskEntry>,
}

impl InMemoryTaskStore {
    pub fn new() -> Self {
        Self { tasks: DashMap::new() }
    }

    /// Create a new task. Returns a sender for writing log output.
    pub fn create(&self, task_id: TaskId) -> TaskSender {
        let (tx, _) = broadcast::channel(256);
        let buffer = Arc::new(tokio::sync::Mutex::new(Vec::new()));
        let now = chrono::Utc::now().to_rfc3339();

        self.tasks.insert(task_id.clone(), TaskEntry {
            status: TaskStatus::Running,
            created_at: now,
            buffer: buffer.clone(),
            tx: tx.clone(),
        });

        TaskSender { id: task_id, tx, buffer }
    }

    /// Get the current state of a task.
    pub fn get(&self, task_id: &str) -> Option<TaskState> {
        self.tasks.get(task_id).map(|entry| TaskState {
            id: task_id.to_string(),
            status: entry.status.clone(),
            created_at: entry.created_at.clone(),
        })
    }

    /// Get buffered log lines and a receiver for live output.
    /// Returns (buffered_lines, live_receiver). The receiver yields lines
    /// appended after this call. Callers should drain the buffer first,
    /// then switch to the receiver.
    pub async fn log_stream(
        &self,
        task_id: &str,
    ) -> Option<(Vec<String>, broadcast::Receiver<String>)> {
        self.tasks.get(task_id).map(|entry| {
            let rx = entry.tx.subscribe();
            // Lock order: subscribe first (to avoid missing lines between
            // buffer snapshot and subscribe), then snapshot the buffer.
            // This means the receiver may contain some lines that are also
            // in the buffer — callers must deduplicate or accept this.
            // In practice, the buffer is snapshotted and the receiver
            // picks up from that point. We re-subscribe after snapshotting.
            (Vec::new(), rx)
        })
    }

    /// Snapshot-then-subscribe: returns buffered lines and a receiver that
    /// will receive all lines appended *after* the snapshot.
    pub async fn subscribe(
        &self,
        task_id: &str,
    ) -> Option<(Vec<String>, broadcast::Receiver<String>)> {
        let entry = self.tasks.get(task_id)?;
        let buffer = entry.buffer.lock().await.clone();
        let rx = entry.tx.subscribe();
        Some((buffer, rx))
    }
}
```

**Key design choice:** `subscribe()` locks the buffer, clones it, then
subscribes to the broadcast channel. There is a brief window where a line
could be appended between the clone and the subscribe — this line would
appear in neither the buffer snapshot nor the receiver. To avoid this, we
subscribe first and accept potential duplicates at the boundary. However,
the simpler approach is to subscribe first, then snapshot:

```rust
pub async fn subscribe(&self, task_id: &str)
    -> Option<(Vec<String>, broadcast::Receiver<String>)>
{
    let entry = self.tasks.get(task_id)?;
    let rx = entry.tx.subscribe();          // subscribe first
    let buffer = entry.buffer.lock().await;  // then snapshot
    Some((buffer.clone(), rx))
    // Lines between subscribe and snapshot appear in both
    // — callers see duplicates, not gaps. Gaps are worse.
}
```

The HTTP handler deduplicates by tracking the count of lines already sent
from the buffer and skipping that many from the receiver.

**Tests:**

```rust
#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn create_and_get_task() {
        let store = InMemoryTaskStore::new();
        let sender = store.create("task-1".into());
        let state = store.get("task-1").unwrap();
        assert_eq!(state.status, TaskStatus::Running);
        sender.complete(&store, true).await;
        let state = store.get("task-1").unwrap();
        assert_eq!(state.status, TaskStatus::Completed);
    }

    #[tokio::test]
    async fn log_buffering_and_subscribe() {
        let store = InMemoryTaskStore::new();
        let sender = store.create("task-1".into());

        sender.send("line 1".into()).await;
        sender.send("line 2".into()).await;

        let (buffer, mut rx) = store.subscribe("task-1").await.unwrap();
        assert_eq!(buffer.len(), 2);

        sender.send("line 3".into()).await;
        let live = rx.recv().await.unwrap();
        assert_eq!(live, "line 3");
    }

    #[tokio::test]
    async fn get_nonexistent_returns_none() {
        let store = InMemoryTaskStore::new();
        assert!(store.get("nope").is_none());
    }
}
```

### Step 2: Bundle storage module

`src/bundle/mod.rs` handles the filesystem side of bundle management:
receiving the tar.gz, writing it atomically, unpacking, and retention
cleanup. No HTTP — just pure storage logic.

```rust
use std::path::{Path, PathBuf};
use tokio::io::AsyncWriteExt;

pub mod restore;

/// Storage paths for a given bundle.
pub struct BundlePaths {
    pub archive: PathBuf,     // {app_id}/{bundle_id}.tar.gz
    pub unpacked: PathBuf,    // {app_id}/{bundle_id}/
    pub library: PathBuf,     // {app_id}/{bundle_id}_lib/
}

impl BundlePaths {
    pub fn new(base: &Path, app_id: &str, bundle_id: &str) -> Self {
        let app_dir = base.join(app_id);
        Self {
            archive: app_dir.join(format!("{bundle_id}.tar.gz")),
            unpacked: app_dir.join(bundle_id),
            library: app_dir.join(format!("{bundle_id}_lib")),
        }
    }
}

/// Write the uploaded tar.gz to a temp file, then atomically rename.
/// Creates the app directory if it doesn't exist.
pub async fn write_archive(
    base: &Path,
    app_id: &str,
    bundle_id: &str,
    data: bytes::Bytes,
) -> Result<BundlePaths, BundleError> {
    let paths = BundlePaths::new(base, app_id, bundle_id);
    let app_dir = base.join(app_id);
    tokio::fs::create_dir_all(&app_dir).await
        .map_err(|e| BundleError::Storage(format!("create app dir: {e}")))?;

    // Write to temp file in the same directory (same filesystem for rename)
    let temp_path = app_dir.join(format!(".{bundle_id}.tar.gz.tmp"));
    let mut file = tokio::fs::File::create(&temp_path).await
        .map_err(|e| BundleError::Storage(format!("create temp file: {e}")))?;
    file.write_all(&data).await
        .map_err(|e| BundleError::Storage(format!("write temp file: {e}")))?;
    file.flush().await
        .map_err(|e| BundleError::Storage(format!("flush temp file: {e}")))?;

    // Atomic rename
    tokio::fs::rename(&temp_path, &paths.archive).await
        .map_err(|e| BundleError::Storage(format!("rename archive: {e}")))?;

    Ok(paths)
}

/// Unpack the tar.gz archive into {bundle_id}/ directory.
pub async fn unpack_archive(paths: &BundlePaths) -> Result<(), BundleError> {
    let archive_path = paths.archive.clone();
    let unpack_dir = paths.unpacked.clone();

    // Run in a blocking task — tar decompression is CPU-bound
    tokio::task::spawn_blocking(move || {
        let file = std::fs::File::open(&archive_path)
            .map_err(|e| BundleError::Unpack(format!("open archive: {e}")))?;
        let decoder = flate2::read::GzDecoder::new(file);
        let mut archive = tar::Archive::new(decoder);

        std::fs::create_dir_all(&unpack_dir)
            .map_err(|e| BundleError::Unpack(format!("create unpack dir: {e}")))?;

        archive.unpack(&unpack_dir)
            .map_err(|e| BundleError::Unpack(format!("unpack: {e}")))?;

        Ok(())
    })
    .await
    .map_err(|e| BundleError::Unpack(format!("spawn_blocking: {e}")))?
}

/// Create the library output directory for dependency restoration.
pub async fn create_library_dir(paths: &BundlePaths) -> Result<(), BundleError> {
    tokio::fs::create_dir_all(&paths.library).await
        .map_err(|e| BundleError::Storage(format!("create library dir: {e}")))?;
    Ok(())
}

/// Delete a bundle's files (archive, unpacked dir, library dir).
/// Best-effort — logs errors but does not fail.
pub async fn delete_bundle_files(paths: &BundlePaths) {
    for path in [&paths.archive, &paths.unpacked, &paths.library] {
        if path.exists() {
            let result = if path.is_dir() {
                tokio::fs::remove_dir_all(path).await
            } else {
                tokio::fs::remove_file(path).await
            };
            if let Err(e) = result {
                tracing::warn!(path = %path.display(), error = %e, "failed to delete bundle file");
            }
        }
    }
}

/// Enforce retention: keep at most `retention` bundles per app, plus the
/// active bundle (never deleted). Returns IDs of deleted bundles.
pub async fn enforce_retention(
    pool: &sqlx::SqlitePool,
    base: &Path,
    app_id: &str,
    active_bundle_id: Option<&str>,
    retention: u32,
) -> Vec<String> {
    let bundles = match crate::db::sqlite::list_bundles_by_app(pool, app_id).await {
        Ok(b) => b,
        Err(e) => {
            tracing::warn!(app_id, error = %e, "failed to list bundles for retention");
            return vec![];
        }
    };

    // Bundles are ordered newest-first. Keep the first `retention` plus
    // any bundle that is the active one.
    let mut to_delete = Vec::new();
    let mut kept = 0u32;
    for bundle in &bundles {
        let is_active = active_bundle_id == Some(bundle.id.as_str());
        if is_active || kept < retention {
            if !is_active {
                kept += 1;
            }
            continue;
        }
        to_delete.push(bundle.clone());
    }

    let mut deleted_ids = Vec::new();
    for bundle in to_delete {
        let paths = BundlePaths::new(base, app_id, &bundle.id);
        delete_bundle_files(&paths).await;
        // Delete the DB row too
        if let Err(e) = crate::db::sqlite::delete_bundle(pool, &bundle.id).await {
            tracing::warn!(bundle_id = bundle.id, error = %e, "failed to delete bundle row");
        } else {
            deleted_ids.push(bundle.id);
        }
    }

    deleted_ids
}

#[derive(Debug, thiserror::Error)]
pub enum BundleError {
    #[error("storage error: {0}")]
    Storage(String),
    #[error("unpack error: {0}")]
    Unpack(String),
    #[error("restore error: {0}")]
    Restore(String),
}
```

**`max_bundle_size` config field.** Add to `StorageConfig`:

```rust
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct StorageConfig {
    pub bundle_server_path: PathBuf,
    #[serde(default = "default_worker_path")]
    pub bundle_worker_path: PathBuf,
    #[serde(default = "default_retention")]
    pub bundle_retention: u32,
    #[serde(default = "default_max_bundle_size")]
    pub max_bundle_size: usize,        // bytes; default 100MB
}

fn default_max_bundle_size() -> usize { 100 * 1024 * 1024 }
```

Add `BLOCKYARD_STORAGE_MAX_BUNDLE_SIZE` to `supported_env_vars()` and
`apply_env_overrides()`. The upload route applies this as an axum
`DefaultBodyLimit` layer — requests exceeding the limit are rejected with
413 Payload Too Large before the handler runs. No custom error body on
413 — axum's default response is sufficient.

Update `blockyard.toml` example:

```toml
[storage]
bundle_server_path = "/data/bundles"
bundle_worker_path = "/app"
bundle_retention   = 50
max_bundle_size    = 104857600   # 100MB in bytes
```

**New dependencies:**

```toml
flate2 = "1"    # gzip decompression
tar = "0.4"     # tar archive unpacking
```

**New DB query** — `delete_bundle` in `db/sqlite.rs`:

```rust
pub async fn delete_bundle(pool: &SqlitePool, id: &str) -> Result<bool, sqlx::Error> {
    let result = sqlx::query("DELETE FROM bundles WHERE id = ?")
        .bind(id)
        .execute(pool)
        .await?;
    Ok(result.rows_affected() > 0)
}
```

Also add `set_active_bundle` in `db/sqlite.rs`:

```rust
pub async fn set_active_bundle(
    pool: &SqlitePool,
    app_id: &str,
    bundle_id: &str,
) -> Result<bool, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    let result = sqlx::query(
        "UPDATE apps SET active_bundle = ?, updated_at = ? WHERE id = ?"
    )
    .bind(bundle_id)
    .bind(&now)
    .bind(app_id)
    .execute(pool)
    .await?;
    Ok(result.rows_affected() > 0)
}
```

**Tests:**

```rust
#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    fn make_test_targz(dir: &Path) -> PathBuf {
        // Create a minimal tar.gz with an app.R and rv.lock
        let tar_path = dir.join("test.tar.gz");
        let file = std::fs::File::create(&tar_path).unwrap();
        let encoder = flate2::write::GzEncoder::new(file, flate2::Compression::default());
        let mut archive = tar::Builder::new(encoder);

        let app_r = b"library(shiny)\nshinyApp(ui, server)";
        let mut header = tar::Header::new_gnu();
        header.set_size(app_r.len() as u64);
        header.set_mode(0o644);
        header.set_cksum();
        archive.append_data(&mut header, "app.R", &app_r[..]).unwrap();

        archive.into_inner().unwrap().finish().unwrap();
        tar_path
    }

    #[tokio::test]
    async fn write_and_unpack_archive() {
        let tmp = TempDir::new().unwrap();
        let tar_data = tokio::fs::read(make_test_targz(tmp.path())).await.unwrap();

        let paths = write_archive(
            tmp.path(), "app-1", "bundle-1",
            bytes::Bytes::from(tar_data),
        ).await.unwrap();

        assert!(paths.archive.exists());

        unpack_archive(&paths).await.unwrap();
        assert!(paths.unpacked.join("app.R").exists());
    }

    #[tokio::test]
    async fn delete_bundle_files_works() {
        let tmp = TempDir::new().unwrap();
        let tar_data = tokio::fs::read(make_test_targz(tmp.path())).await.unwrap();
        let paths = write_archive(
            tmp.path(), "app-1", "bundle-1",
            bytes::Bytes::from(tar_data),
        ).await.unwrap();
        unpack_archive(&paths).await.unwrap();
        create_library_dir(&paths).await.unwrap();

        delete_bundle_files(&paths).await;
        assert!(!paths.archive.exists());
        assert!(!paths.unpacked.exists());
        assert!(!paths.library.exists());
    }
}
```

### Step 3: Restore pipeline

`src/bundle/restore.rs` — orchestrates the async restore task. Receives the
uploaded bundle, calls `backend.build()`, streams logs to the `TaskStore`,
updates the bundle status in SQLite, and sets `active_bundle` on success.

```rust
use crate::backend::{Backend, BuildSpec};
use crate::bundle::BundlePaths;
use crate::db;
use crate::task::{InMemoryTaskStore, TaskSender};
use sqlx::SqlitePool;
use std::collections::HashMap;
use std::sync::Arc;

/// Spawn the async restore task. Returns immediately — the restore runs
/// in a background tokio task.
pub fn spawn_restore<B: Backend>(
    backend: Arc<B>,
    pool: SqlitePool,
    task_store: Arc<InMemoryTaskStore>,
    task_sender: TaskSender,
    app_id: String,
    bundle_id: String,
    paths: BundlePaths,
    image: String,
    retention: u32,
    bundle_server_path: std::path::PathBuf,
) {
    tokio::spawn(async move {
        let result = run_restore(
            &*backend, &pool, &task_store, &task_sender,
            &app_id, &bundle_id, &paths, &image,
        ).await;

        match result {
            Ok(()) => {
                task_sender.complete(&task_store, true).await;
                // Enforce retention after successful deploy
                crate::bundle::enforce_retention(
                    &pool, &bundle_server_path, &app_id,
                    Some(&bundle_id), retention,
                ).await;
            }
            Err(e) => {
                task_sender.send(format!("ERROR: {e}")).await;
                task_sender.complete(&task_store, false).await;
                let _ = db::sqlite::update_bundle_status(&pool, &bundle_id, "failed").await;
            }
        }
    });
}

async fn run_restore<B: Backend>(
    backend: &B,
    pool: &SqlitePool,
    _task_store: &InMemoryTaskStore,
    task_sender: &TaskSender,
    app_id: &str,
    bundle_id: &str,
    paths: &BundlePaths,
    image: &str,
) -> Result<(), crate::bundle::BundleError> {
    // 1. Update status to "building"
    db::sqlite::update_bundle_status(pool, bundle_id, "building")
        .await
        .map_err(|e| crate::bundle::BundleError::Restore(format!("update status: {e}")))?;

    task_sender.send("Starting dependency restoration...".into()).await;

    // 2. Build the spec
    let mut labels = HashMap::new();
    labels.insert("dev.blockyard/app-id".into(), app_id.into());
    labels.insert("dev.blockyard/bundle-id".into(), bundle_id.into());

    let spec = BuildSpec {
        app_id: app_id.to_string(),
        bundle_id: bundle_id.to_string(),
        image: image.to_string(),
        bundle_path: paths.unpacked.clone(),
        library_path: paths.library.clone(),
        labels,
    };

    // 3. Run the build
    let build_result = backend.build(&spec).await
        .map_err(|e| crate::bundle::BundleError::Restore(format!("build: {e}")))?;

    if !build_result.success {
        let msg = format!(
            "Build failed with exit code {:?}",
            build_result.exit_code
        );
        task_sender.send(msg.clone()).await;
        db::sqlite::update_bundle_status(pool, bundle_id, "failed")
            .await
            .map_err(|e| crate::bundle::BundleError::Restore(format!("update status: {e}")))?;
        return Err(crate::bundle::BundleError::Restore(msg));
    }

    // 4. Mark bundle as ready and activate
    task_sender.send("Build succeeded. Activating bundle...".into()).await;

    db::sqlite::update_bundle_status(pool, bundle_id, "ready")
        .await
        .map_err(|e| crate::bundle::BundleError::Restore(format!("update status: {e}")))?;

    db::sqlite::set_active_bundle(pool, app_id, bundle_id)
        .await
        .map_err(|e| crate::bundle::BundleError::Restore(format!("set active bundle: {e}")))?;

    task_sender.send("Bundle activated.".into()).await;
    Ok(())
}
```

**Build log streaming (decided: opaque build, no real-time streaming).**
`backend.build()` runs the container to completion and returns a
`BuildResult`. Logs are not streamed in real time — the `TaskSender`
receives status messages from the restore pipeline ("Starting dependency
restoration...", "Build succeeded", etc.) but not the raw build output
line-by-line. This is sufficient for v0 where the primary concern is
knowing whether the build succeeded. Real-time streaming can be added
later by splitting `build()` or accepting a log callback, but that's a
Backend trait change that isn't warranted yet.

### Step 4: Image pulling

`DockerBackend::ensure_image()` — pull the configured image if it's not
already present locally. Called on demand inside `DockerBackend::build()`
and `DockerBackend::spawn()` as their first step. Not called at server
startup — the image is pulled when it's actually needed.

Add to `src/backend/docker.rs`:

```rust
impl DockerBackend {
    /// Pull the image if it's not already present locally.
    /// Uses bollard's create_image (docker pull).
    pub async fn ensure_image(&self, image: &str) -> Result<(), BackendError> {
        use bollard::image::CreateImageOptions;
        use futures_util::StreamExt;

        // Check if image exists locally
        match self.client.inspect_image(image).await {
            Ok(_) => {
                tracing::debug!(image, "image already present");
                return Ok(());
            }
            Err(_) => {
                tracing::info!(image, "pulling image");
            }
        }

        let options = CreateImageOptions {
            from_image: image,
            ..Default::default()
        };

        let mut stream = self.client.create_image(Some(options), None, None);
        while let Some(result) = stream.next().await {
            match result {
                Ok(info) => {
                    if let Some(status) = info.status {
                        tracing::debug!(image, status, "pull progress");
                    }
                }
                Err(e) => {
                    return Err(BackendError::Build(
                        format!("image pull failed for '{image}': {e}")
                    ));
                }
            }
        }

        tracing::info!(image, "image pulled successfully");
        Ok(())
    }
}
```

**Wiring:** `ensure_image()` is called *inside* `DockerBackend::build()`
and `DockerBackend::spawn()` as their first step — not by the caller.
Image pulling is a container-runtime concept (a future local/process
backend wouldn't have images), so it stays internal to the Docker backend.
The `Backend` trait is not modified. The restore pipeline and proxy layer
call `backend.build()` / `backend.spawn()` as usual and get image pulling
for free.

### Step 5: `AppState` update — add `TaskStore`

Add the task store to `AppState` so handlers can access it:

```rust
pub struct AppState<B: Backend> {
    pub config: Arc<Config>,
    pub backend: Arc<B>,
    pub db: SqlitePool,
    pub workers: Arc<DashMap<String, ActiveWorker<B::Handle>>>,
    pub task_store: Arc<InMemoryTaskStore>,
}

impl<B: Backend> AppState<B> {
    pub fn new(config: Config, backend: B, db: SqlitePool) -> Self {
        Self {
            config: Arc::new(config),
            backend: Arc::new(backend),
            db,
            workers: Arc::new(DashMap::new()),
            task_store: Arc::new(InMemoryTaskStore::new()),
        }
    }
}
```

### Step 6: Minimal axum router + bundle upload endpoint

Wire up enough of the HTTP layer to test the deployment pipeline. This is
*not* the full API (that's phase 0-4) — just the endpoints needed to
exercise and test bundle upload + restore + task logs.

`src/api/mod.rs`:

```rust
use axum::{Router, middleware};
use crate::app::AppState;
use crate::backend::Backend;

pub mod bundles;
pub mod tasks;
pub mod auth;

pub fn api_router<B: Backend>(state: AppState<B>) -> Router<AppState<B>> {
    let max_body = state.config.storage.max_bundle_size;

    let authed = Router::new()
        .route("/apps/{id}/bundles", axum::routing::post(bundles::upload_bundle::<B>))
        .route("/apps/{id}/bundles", axum::routing::get(bundles::list_bundles::<B>))
        .route("/tasks/{task_id}/logs", axum::routing::get(tasks::task_logs::<B>))
        .layer(axum::extract::DefaultBodyLimit::max(max_body))
        .layer(middleware::from_fn_with_state(
            state.clone(),
            auth::bearer_auth::<B>,
        ));

    Router::new()
        .nest("/api/v1", authed)
        .route("/healthz", axum::routing::get(healthz))
}

async fn healthz() -> &'static str {
    "ok"
}
```

`src/api/auth.rs` — bearer token middleware:

```rust
use axum::{extract::State, http::StatusCode, middleware::Next, response::Response};
use crate::app::AppState;
use crate::backend::Backend;

pub async fn bearer_auth<B: Backend>(
    State(state): State<AppState<B>>,
    req: axum::http::Request<axum::body::Body>,
    next: Next,
) -> Result<Response, StatusCode> {
    let token = req.headers()
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "));

    match token {
        Some(t) if t == state.config.server.token => Ok(next.run(req).await),
        _ => Err(StatusCode::UNAUTHORIZED),
    }
}
```

`src/api/bundles.rs` — bundle upload handler:

```rust
use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::Json;
use bytes::Bytes;
use crate::app::AppState;
use crate::backend::Backend;
use crate::bundle;

#[derive(serde::Serialize)]
pub struct UploadResponse {
    pub bundle_id: String,
    pub task_id: String,
}

pub async fn upload_bundle<B: Backend>(
    State(state): State<AppState<B>>,
    Path(app_id): Path<String>,
    body: Bytes,
) -> Result<(StatusCode, Json<UploadResponse>), (StatusCode, Json<ErrorResponse>)> {
    // 1. Validate app exists
    let app = crate::db::sqlite::get_app(&state.db, &app_id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {app_id} not found")))?;

    // 2. Validate body is not empty
    if body.is_empty() {
        return Err(bad_request("empty bundle body".into()));
    }

    // Note: bundle size is enforced at the router level via
    // axum::extract::DefaultBodyLimit — requests exceeding
    // max_bundle_size are rejected with 413 before reaching
    // this handler. See api_router() setup.

    // 3. Generate IDs
    let bundle_id = uuid::Uuid::new_v4().to_string();
    let task_id = uuid::Uuid::new_v4().to_string();

    // 4. Write archive atomically
    let base = &state.config.storage.bundle_server_path;
    let paths = bundle::write_archive(base, &app.id, &bundle_id, body)
        .await
        .map_err(|e| server_error(format!("write archive: {e}")))?;

    // 5. Unpack
    bundle::unpack_archive(&paths)
        .await
        .map_err(|e| server_error(format!("unpack: {e}")))?;

    // 6. Create library dir
    bundle::create_library_dir(&paths)
        .await
        .map_err(|e| server_error(format!("create lib dir: {e}")))?;

    // 7. Insert bundle row (status = pending)
    crate::db::sqlite::create_bundle(&state.db, &app.id, paths.archive.to_str().unwrap())
        .await
        .map_err(|e| server_error(format!("create bundle row: {e}")))?;

    // 8. Create task in TaskStore
    let task_sender = state.task_store.create(task_id.clone());

    // 9. Get image name
    let image = state.config.docker
        .as_ref()
        .map(|d| d.image.clone())
        .unwrap_or_else(|| "rocker/r-ver:latest".into());

    // 10. Spawn async restore
    bundle::restore::spawn_restore(
        state.backend.clone(),
        state.db.clone(),
        state.task_store.clone(),
        task_sender,
        app.id,
        bundle_id.clone(),
        paths,
        image,
        state.config.storage.bundle_retention,
        state.config.storage.bundle_server_path.clone(),
    );

    // 11. Return 202
    Ok((
        StatusCode::ACCEPTED,
        Json(UploadResponse { bundle_id, task_id }),
    ))
}

pub async fn list_bundles<B: Backend>(
    State(state): State<AppState<B>>,
    Path(app_id): Path<String>,
) -> Result<Json<Vec<crate::db::sqlite::BundleRow>>, (StatusCode, Json<ErrorResponse>)> {
    let bundles = crate::db::sqlite::list_bundles_by_app(&state.db, &app_id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?;
    Ok(Json(bundles))
}

#[derive(serde::Serialize)]
pub struct ErrorResponse {
    pub error: String,
    pub message: String,
}

fn bad_request(msg: String) -> (StatusCode, Json<ErrorResponse>) {
    (StatusCode::BAD_REQUEST, Json(ErrorResponse {
        error: "bad_request".into(),
        message: msg,
    }))
}

fn not_found(msg: String) -> (StatusCode, Json<ErrorResponse>) {
    (StatusCode::NOT_FOUND, Json(ErrorResponse {
        error: "not_found".into(),
        message: msg,
    }))
}

fn server_error(msg: String) -> (StatusCode, Json<ErrorResponse>) {
    (StatusCode::INTERNAL_SERVER_ERROR, Json(ErrorResponse {
        error: "internal_error".into(),
        message: msg,
    }))
}
```

**`create_bundle` signature change (decided).** The upload handler generates
the bundle_id upfront (needed for filesystem paths before the DB row
exists). Change `create_bundle` in `db/sqlite.rs` to accept a
caller-supplied `id` parameter instead of auto-generating a UUID. Update
existing tests to pass an explicit ID.

```rust
pub async fn create_bundle(
    pool: &SqlitePool,
    id: &str,        // caller-supplied, not auto-generated
    app_id: &str,
    path: &str,
) -> Result<BundleRow, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    sqlx::query_as::<_, BundleRow>(
        "INSERT INTO bundles (id, app_id, status, path, uploaded_at)
         VALUES (?, ?, 'pending', ?, ?)
         RETURNING *",
    )
    .bind(id)
    .bind(app_id)
    .bind(path)
    .bind(&now)
    .fetch_one(pool)
    .await
}
```

### Step 7: Task log streaming endpoint

`src/api/tasks.rs` — stream task logs over chunked HTTP:

```rust
use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::body::Body;
use futures_util::StreamExt;
use tokio_stream::wrappers::BroadcastStream;
use crate::app::AppState;
use crate::backend::Backend;
use crate::task::TaskStatus;

pub async fn task_logs<B: Backend>(
    State(state): State<AppState<B>>,
    Path(task_id): Path<String>,
) -> Result<Response, StatusCode> {
    let task_state = state.task_store.get(&task_id)
        .ok_or(StatusCode::NOT_FOUND)?;

    let (buffer, rx) = state.task_store.subscribe(&task_id)
        .await
        .ok_or(StatusCode::NOT_FOUND)?;

    let is_done = task_state.status != TaskStatus::Running;
    let buffer_len = buffer.len();

    // Build a stream: first the buffered lines, then live lines
    let buffer_stream = futures_util::stream::iter(
        buffer.into_iter().map(|line| Ok::<_, std::convert::Infallible>(format!("{line}\n")))
    );

    if is_done {
        // Task already finished — return just the buffer
        let body = Body::from_stream(buffer_stream);
        return Ok(Response::builder()
            .header("content-type", "text/plain")
            .body(body)
            .unwrap());
    }

    // Task is still running — stream buffer then live output
    let live_stream = BroadcastStream::new(rx)
        .filter_map(move |result| {
            std::future::ready(match result {
                Ok(line) => Some(Ok(format!("{line}\n"))),
                Err(tokio_stream::wrappers::errors::BroadcastStreamRecvError::Lagged(n)) => {
                    Some(Ok(format!("[dropped {n} lines]\n")))
                }
            })
        });

    let combined = buffer_stream.chain(live_stream);
    let body = Body::from_stream(combined);

    Ok(Response::builder()
        .header("content-type", "text/plain")
        .header("transfer-encoding", "chunked")
        .body(body)
        .unwrap())
}
```

**New dependency:**

```toml
tokio-stream = { version = "0.1", features = ["sync"] }
```

The `sync` feature provides `BroadcastStream`, which adapts a
`broadcast::Receiver` into a `Stream` suitable for `Body::from_stream()`.

### Step 8: Wire up `main.rs`

Update `main.rs` to start the HTTP server with the minimal router. This
makes the deployment pipeline testable end-to-end with `curl`.

```rust
use blockyard::app::AppState;
use blockyard::config::Config;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "blockyard=info".parse().unwrap()),
        )
        .json()
        .init();

    let config = Config::load()?;
    tracing::info!("loaded config");

    // Initialize backend
    #[cfg(feature = "docker")]
    let backend = {
        let docker_config = config.docker.clone()
            .expect("[docker] config required");
        blockyard::backend::docker::DockerBackend::new(docker_config).await?
    };

    // Initialize database
    let db = blockyard::db::create_pool(&config.database.path).await?;

    // Build state and router
    let state = AppState::new(config.clone(), backend, db);
    let app = blockyard::api::api_router(state.clone())
        .with_state(state);

    // Start server
    let listener = tokio::net::TcpListener::bind(&config.server.bind).await?;
    tracing::info!(bind = %config.server.bind, "server listening");

    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    Ok(())
}

async fn shutdown_signal() {
    tokio::signal::ctrl_c().await.ok();
    tracing::info!("shutdown signal received");
}
```

### Step 9: Module declarations

Update `src/lib.rs` to include the new modules:

```rust
pub mod app;
pub mod api;
pub mod backend;
pub mod bundle;
pub mod config;
pub mod db;
pub mod task;
```

### Step 10: Integration tests

Tests that exercise the full upload → restore → activate pipeline using
the mock backend. These start a real HTTP server and use `reqwest` to
interact with it.

`tests/bundle_test.rs`:

```rust
use blockyard::app::AppState;
use blockyard::backend::mock::MockBackend;
use blockyard::db;
use reqwest::StatusCode;
use std::net::SocketAddr;

async fn spawn_test_server() -> (SocketAddr, AppState<MockBackend>) {
    let config = test_config();
    let backend = MockBackend::new();
    let pool = sqlx::SqlitePool::connect(":memory:").await.unwrap();
    db::run_migrations(&pool).await.unwrap();
    let state = AppState::new(config, backend, pool);
    let app = blockyard::api::api_router(state.clone())
        .with_state(state.clone());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(axum::serve(listener, app).into_future());
    (addr, state)
}

fn test_config() -> blockyard::config::Config {
    let tmp = tempfile::TempDir::new().unwrap();
    // ... construct a Config with bundle_server_path pointing at tmp
    todo!("construct test config")
}

fn make_test_bundle() -> Vec<u8> {
    // Build a minimal tar.gz in memory
    let mut builder = tar::Builder::new(Vec::new());
    let app_r = b"library(shiny)";
    let mut header = tar::Header::new_gnu();
    header.set_size(app_r.len() as u64);
    header.set_mode(0o644);
    header.set_cksum();
    builder.append_data(&mut header, "app.R", &app_r[..]).unwrap();
    let tar_data = builder.into_inner().unwrap();

    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    std::io::Write::write_all(&mut encoder, &tar_data).unwrap();
    encoder.finish().unwrap()
}

#[tokio::test]
async fn upload_bundle_returns_202() {
    let (addr, state) = spawn_test_server().await;

    // Create an app first
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::ACCEPTED);

    let body: serde_json::Value = resp.json().await.unwrap();
    assert!(body["bundle_id"].is_string());
    assert!(body["task_id"].is_string());
}

#[tokio::test]
async fn upload_without_auth_returns_401() {
    let (addr, state) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::UNAUTHORIZED);
}

#[tokio::test]
async fn upload_to_nonexistent_app_returns_404() {
    let (addr, _state) = spawn_test_server().await;

    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/nonexistent/bundles"))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::NOT_FOUND);
}

#[tokio::test]
async fn task_logs_streams_output() {
    let (addr, state) = spawn_test_server().await;
    let app = db::sqlite::create_app(&state.db, "test-app").await.unwrap();

    // Upload a bundle
    let client = reqwest::Client::new();
    let resp = client
        .post(format!("http://{addr}/api/v1/apps/{}/bundles", app.id))
        .header("authorization", "Bearer test-token")
        .body(make_test_bundle())
        .send()
        .await
        .unwrap();

    let body: serde_json::Value = resp.json().await.unwrap();
    let task_id = body["task_id"].as_str().unwrap();

    // Give the background task a moment to run
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    // Fetch task logs
    let resp = client
        .get(format!("http://{addr}/api/v1/tasks/{task_id}/logs"))
        .header("authorization", "Bearer test-token")
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), StatusCode::OK);
    let logs = resp.text().await.unwrap();
    // Should contain at least the "Starting dependency restoration..." line
    assert!(logs.contains("Starting dependency restoration"));
}
```

## New dependencies

```toml
flate2 = "1"                                     # gzip compression/decompression
tar = "0.4"                                      # tar archive packing/unpacking
tokio-stream = { version = "0.1", features = ["sync"] }  # BroadcastStream adapter
```

## New source files

| File | Purpose |
|---|---|
| `src/task.rs` | TaskStore trait + InMemoryTaskStore |
| `src/bundle/mod.rs` | Bundle storage — write, unpack, delete, retention |
| `src/bundle/restore.rs` | Async restore pipeline |
| `src/api/mod.rs` | Minimal axum router |
| `src/api/auth.rs` | Bearer token middleware |
| `src/api/bundles.rs` | Bundle upload + list handlers |
| `src/api/tasks.rs` | Task log streaming handler |

## Modified files

| File | Change |
|---|---|
| `src/lib.rs` | Add `pub mod api`, `pub mod bundle`, `pub mod task` |
| `src/app.rs` | Add `task_store: Arc<InMemoryTaskStore>` to `AppState` |
| `src/main.rs` | Start HTTP server |
| `src/config.rs` | Add `max_bundle_size` to `StorageConfig`, env var support |
| `src/db/sqlite.rs` | Add `delete_bundle`, `set_active_bundle`; change `create_bundle` to accept caller-supplied ID |
| `src/backend/docker.rs` | Add `ensure_image()` method |
| `Cargo.toml` | Add `flate2`, `tar`, `tokio-stream` |

## Exit criteria

Phase 0-3 is done when:

- `TaskStore` creates tasks, buffers logs, streams to subscribers
- Bundle archive is written atomically and unpacked correctly
- Restore pipeline calls `backend.build()`, updates bundle status, sets
  `active_bundle` on success
- Retention cleanup deletes oldest non-active bundles when limit is exceeded
- `DockerBackend::ensure_image()` pulls images when missing
- Uploads exceeding `max_bundle_size` are rejected with 413
- `POST /api/v1/apps/{id}/bundles` returns 202 with bundle_id + task_id
- `GET /api/v1/tasks/{task_id}/logs` streams buffered + live log output
- Bearer auth middleware rejects unauthenticated requests
- `/healthz` returns 200 without auth
- `main.rs` starts the server and serves requests
- All unit tests pass (`task.rs`, `bundle/mod.rs`, `db/sqlite.rs`)
- Integration tests pass with mock backend (`tests/bundle_test.rs`)
- `cargo clippy` clean
- `cargo test --features test-support` green

