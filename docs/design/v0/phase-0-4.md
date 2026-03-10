# Phase 0-4: REST API + Auth

The control plane HTTP API. All endpoints under `/api/v1/`, protected by
static bearer token. Phase 0-3 delivered the bundle upload pipeline and a
minimal router to support it. This phase expands that router to the full
v0 API surface: app CRUD, app lifecycle (start/stop), and log streaming.

## Deliverables

1. Remove `status` column from `apps` table — runtime state is derived
   from the workers DashMap, not stored in the DB
2. App CRUD endpoints — create, list, get, update, delete
3. App name validation — URL-safe slugs only
4. App lifecycle endpoints — start and stop
5. App log streaming endpoint
6. `ActiveWorker.session_id` → `Option<String>`
7. `BundlePaths::for_bundle` — shared path constructor for bundle dirs
8. DB additions — `update_app`, `clear_active_bundle`
9. Shared error response helpers — extract from `bundles.rs`, reuse
10. Router expansion — wire all endpoints into `api_router`
11. Integration tests for all new endpoints

## What's already done

Phase 0-3 delivered:

- Bearer token auth middleware (`api/auth.rs`)
- `/healthz` endpoint (unauthenticated)
- `POST /api/v1/apps/{id}/bundles` — upload bundle
- `GET /api/v1/apps/{id}/bundles` — list bundles
- `GET /api/v1/tasks/{task_id}/logs` — stream task logs
- Error response helpers (`bad_request`, `not_found`, `server_error`)
- DB queries: `create_app`, `get_app`, `get_app_by_name`, `list_apps`,
  `delete_app`, bundle CRUD, `set_active_bundle`, `update_bundle_status`
- `AppState` with `workers: DashMap<String, ActiveWorker<B::Handle>>`

## Step-by-step

### Step 1: Remove `status` column from `apps` table

The `status` column stores runtime state ("running" / "stopped"), but
this information is inherently ephemeral — it becomes stale on server
crash, restart, or worker failure. The workers DashMap already *is* the
source of truth for which workers are alive. Storing a copy in SQLite
creates a synchronization obligation with no upside.

**Migration `002_remove_app_status.sql`:**

```sql
-- SQLite doesn't support ALTER TABLE DROP COLUMN before 3.35.0.
-- Use the table-rebuild pattern for broad compatibility.
CREATE TABLE apps_new (
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

INSERT INTO apps_new SELECT
    id, name, active_bundle, max_workers_per_app,
    max_sessions_per_worker, memory_limit, cpu_limit,
    created_at, updated_at
FROM apps;

DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
```

**Code changes:**

- Remove `pub status: String` from `AppRow`
- Remove `'stopped'` from the `create_app` INSERT
- Remove the `update_app_status` function (never add it)
- Update `db::sqlite` tests that assert `app.status`

**Deriving status at read time:**

The `GET /apps/{id}` handler computes status from the workers DashMap
before returning the response. A lightweight wrapper serializes the
app row plus the derived status:

```rust
#[derive(serde::Serialize)]
pub struct AppResponse {
    #[serde(flatten)]
    pub app: AppRow,
    pub status: String,
}

fn app_status<B: Backend>(state: &AppState<B>, app_id: &str) -> String {
    let has_workers = state.workers.iter()
        .any(|entry| entry.value().app_id == app_id);
    if has_workers { "running".into() } else { "stopped".into() }
}
```

`AppResponse` flattens the DB row and adds the computed `status` field.
This keeps the DB layer clean (no runtime state) while the API layer
presents a complete picture. `list_apps` uses the same pattern —
iterate the DB rows and annotate each with its derived status.

### Step 2: `ActiveWorker.session_id` → `Option<String>`

The `session_id` field on `ActiveWorker` is currently a `String`, and
the start endpoint sets it to `""` because no session exists yet.
Change it to `Option<String>`:

```rust
pub struct ActiveWorker<H: Clone> {
    pub app_id: String,
    pub handle: H,
    pub session_id: Option<String>,
}
```

The start endpoint sets `session_id: None`. The proxy layer (phase 0-5)
sets it to `Some(session_id)` when a session is assigned.

### Step 3: `BundlePaths::for_bundle` — shared path constructor

Both the bundle upload flow (`write_archive`) and the start endpoint
need to construct paths for a bundle's archive, unpacked directory, and
library directory. Currently the start endpoint duplicates the path
conventions inline. Extract a shared constructor:

```rust
impl BundlePaths {
    /// Construct paths for a bundle given the base storage dir, app ID,
    /// and bundle ID. This is the single source of truth for the on-disk
    /// layout.
    pub fn for_bundle(base: &Path, app_id: &str, bundle_id: &str) -> Self {
        let app_dir = base.join(app_id);
        Self {
            archive: app_dir.join(format!("{bundle_id}.tar.gz")),
            unpacked: app_dir.join(bundle_id),
            library: app_dir.join(format!("{bundle_id}-lib")),
        }
    }
}
```

Update `write_archive` to use `BundlePaths::for_bundle` instead of
constructing paths inline. The start endpoint also uses it to locate
the bundle and library directories.

### Step 4: Extract shared error response helpers

The `ErrorResponse` struct and helpers (`bad_request`, `not_found`,
`server_error`) currently live in `api/bundles.rs`. Move them to a shared
location so all API modules can use them, and add the two new variants
needed for this phase.

`src/api/error.rs`:

```rust
use axum::http::StatusCode;
use axum::response::Json;

#[derive(serde::Serialize)]
pub struct ErrorResponse {
    pub error: String,
    pub message: String,
}

/// Convenience type for handler return values.
pub type ApiError = (StatusCode, Json<ErrorResponse>);

pub fn bad_request(msg: String) -> ApiError {
    (
        StatusCode::BAD_REQUEST,
        Json(ErrorResponse {
            error: "bad_request".into(),
            message: msg,
        }),
    )
}

pub fn not_found(msg: String) -> ApiError {
    (
        StatusCode::NOT_FOUND,
        Json(ErrorResponse {
            error: "not_found".into(),
            message: msg,
        }),
    )
}

pub fn conflict(msg: String) -> ApiError {
    (
        StatusCode::CONFLICT,
        Json(ErrorResponse {
            error: "conflict".into(),
            message: msg,
        }),
    )
}

pub fn service_unavailable(msg: String) -> ApiError {
    (
        StatusCode::SERVICE_UNAVAILABLE,
        Json(ErrorResponse {
            error: "service_unavailable".into(),
            message: msg,
        }),
    )
}

pub fn server_error(msg: String) -> ApiError {
    (
        StatusCode::INTERNAL_SERVER_ERROR,
        Json(ErrorResponse {
            error: "internal_error".into(),
            message: msg,
        }),
    )
}
```

Update `api/bundles.rs` to import from `api/error.rs` instead of defining
its own helpers. The `ErrorResponse` struct, `bad_request`, `not_found`,
and `server_error` functions are deleted from `bundles.rs` and replaced
with `use super::error::*`.

### Step 5: App name validation

App names are used in proxy URLs (`/app/{name}/`), so they must be
URL-safe slugs. Validation rules:

- Lowercase ASCII letters, digits, and hyphens only
- Must start with a letter
- Must not end with a hyphen
- Length: 1–63 characters
- Regex: `^[a-z][a-z0-9-]*[a-z0-9]$` (or `^[a-z]$` for single char)

Add a validation function in the apps module:

```rust
fn validate_app_name(name: &str) -> Result<(), String> {
    if name.is_empty() || name.len() > 63 {
        return Err("name must be 1-63 characters".into());
    }
    if !name.chars().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-') {
        return Err("name must contain only lowercase letters, digits, and hyphens".into());
    }
    if !name.starts_with(|c: char| c.is_ascii_lowercase()) {
        return Err("name must start with a lowercase letter".into());
    }
    if name.ends_with('-') {
        return Err("name must not end with a hyphen".into());
    }
    Ok(())
}
```

### Step 6: App CRUD endpoints

`src/api/apps.rs` — all app management endpoints.

**Create app:**

```rust
#[derive(serde::Deserialize)]
pub struct CreateAppRequest {
    pub name: String,
}

pub async fn create_app<B: Backend>(
    State(state): State<AppState<B>>,
    Json(body): Json<CreateAppRequest>,
) -> Result<(StatusCode, Json<AppResponse>), ApiError> {
    validate_app_name(&body.name).map_err(bad_request)?;

    // Check for duplicate name
    let existing = db::sqlite::get_app_by_name(&state.db, &body.name)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?;
    if existing.is_some() {
        return Err(conflict(format!("app name '{}' already exists", body.name)));
    }

    let app = db::sqlite::create_app(&state.db, &body.name)
        .await
        .map_err(|e| server_error(format!("create app: {e}")))?;

    // Newly created app has no workers → status is "stopped"
    Ok((StatusCode::CREATED, Json(AppResponse {
        app,
        status: "stopped".into(),
    })))
}
```

We check for duplicate names explicitly before the INSERT rather than
relying on the UNIQUE constraint error. This produces a clear 409
response instead of a generic 500 from a sqlx error. The DB constraint
is still there as a safety net.

**List apps:**

```rust
pub async fn list_apps<B: Backend>(
    State(state): State<AppState<B>>,
) -> Result<Json<Vec<AppResponse>>, ApiError> {
    let apps = db::sqlite::list_apps(&state.db)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?;
    let responses = apps.into_iter()
        .map(|app| {
            let status = app_status(&state, &app.id);
            AppResponse { app, status }
        })
        .collect();
    Ok(Json(responses))
}
```

**Get app:**

```rust
pub async fn get_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<Json<AppResponse>, ApiError> {
    let app = db::sqlite::get_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;
    let status = app_status(&state, &app.id);
    Ok(Json(AppResponse { app, status }))
}
```

**Update app:**

```rust
#[derive(serde::Deserialize)]
pub struct UpdateAppRequest {
    pub max_workers_per_app: Option<i64>,
    pub max_sessions_per_worker: Option<i64>,
    pub memory_limit: Option<String>,
    pub cpu_limit: Option<f64>,
}

pub async fn update_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
    Json(body): Json<UpdateAppRequest>,
) -> Result<Json<AppResponse>, ApiError> {
    // v0: max_sessions_per_worker is locked to 1; session-sharing is deferred to v1
    if let Some(v) = body.max_sessions_per_worker {
        if v != 1 {
            return Err(bad_request(
                "max_sessions_per_worker must be 1 in this version".to_string(),
            ));
        }
    }

    let app = db::sqlite::get_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    let app = db::sqlite::update_app(&state.db, &app.id, &body)
        .await
        .map_err(|e| server_error(format!("update app: {e}")))?;

    let status = app_status(&state, &app.id);
    Ok(Json(AppResponse { app, status }))
}
```

The updatable fields are resource limits and worker scaling — things an
operator adjusts without redeploying. Name and active_bundle are not
mutable via PATCH. Name is immutable because it appears in proxy URLs.
Active bundle is set by the restore pipeline, not by manual update.

**Delete app:**

```rust
pub async fn delete_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<StatusCode, ApiError> {
    let app = db::sqlite::get_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    // 1. Stop all workers for this app
    stop_app_workers(&state, &app.id).await;

    // 2. Delete bundle files from disk
    let bundles = db::sqlite::list_bundles_by_app(&state.db, &app.id)
        .await
        .map_err(|e| server_error(format!("list bundles: {e}")))?;
    for bundle in &bundles {
        let paths = crate::bundle::BundlePaths::for_bundle(
            &state.config.storage.bundle_server_path,
            &app.id,
            &bundle.id,
        );
        crate::bundle::delete_bundle_files(&paths).await;
    }

    // 3. Clear active_bundle FK before deleting bundles
    db::sqlite::clear_active_bundle(&state.db, &app.id)
        .await
        .map_err(|e| server_error(format!("clear active bundle: {e}")))?;

    // 4. Delete bundle rows
    for bundle in &bundles {
        let _ = db::sqlite::delete_bundle(&state.db, &bundle.id).await;
    }

    // 5. Delete app row
    db::sqlite::delete_app(&state.db, &app.id)
        .await
        .map_err(|e| server_error(format!("delete app: {e}")))?;

    Ok(StatusCode::NO_CONTENT)
}
```

Delete is the most complex endpoint because of the multi-step teardown.
The ordering matters: stop workers first (so nothing is using the bundle
files), then delete files from disk, then clear the `active_bundle` FK
(so the app row no longer references any bundle), then delete bundle
rows, then delete the app row. The FK constraint on `bundles.app_id`
enforces that bundles are deleted before the app.

### Step 7: DB additions

`src/db/sqlite.rs` — new queries for update and status management.

**update_app:**

```rust
pub async fn update_app(
    pool: &SqlitePool,
    id: &str,
    update: &crate::api::apps::UpdateAppRequest,
) -> Result<AppRow, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();

    // Build SET clause dynamically — only update fields that are present
    // in the request. This avoids nullifying fields the caller didn't send.
    //
    // sqlx doesn't support optional binds in a static query, so we use
    // separate queries per combination. With 4 optional fields this is
    // manageable. Alternatively, fetch-modify-write:
    let mut app = get_app(pool, id)
        .await?
        .ok_or_else(|| sqlx::Error::RowNotFound)?;

    if let Some(v) = update.max_workers_per_app {
        app.max_workers_per_app = Some(v);
    }
    if let Some(v) = update.max_sessions_per_worker {
        app.max_sessions_per_worker = v;
    }
    if let Some(ref v) = update.memory_limit {
        app.memory_limit = Some(v.clone());
    }
    if let Some(v) = update.cpu_limit {
        app.cpu_limit = Some(v);
    }

    sqlx::query_as::<_, AppRow>(
        "UPDATE apps SET
             max_workers_per_app = ?,
             max_sessions_per_worker = ?,
             memory_limit = ?,
             cpu_limit = ?,
             updated_at = ?
         WHERE id = ?
         RETURNING *"
    )
    .bind(app.max_workers_per_app)
    .bind(app.max_sessions_per_worker)
    .bind(&app.memory_limit)
    .bind(app.cpu_limit)
    .bind(&now)
    .bind(id)
    .fetch_one(pool)
    .await
}
```

The fetch-modify-write pattern is fine here because updates are rare
admin operations, not high-frequency paths. No concurrent update
contention expected.

**clear_active_bundle:**

```rust
pub async fn clear_active_bundle(
    pool: &SqlitePool,
    app_id: &str,
) -> Result<bool, sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    let result = sqlx::query(
        "UPDATE apps SET active_bundle = NULL, updated_at = ? WHERE id = ?"
    )
    .bind(&now)
    .bind(app_id)
    .execute(pool)
    .await?;
    Ok(result.rows_affected() > 0)
}
```

### Step 8: App lifecycle — start

`POST /api/v1/apps/{id}/start` — start an app by spawning a worker.

```rust
#[derive(serde::Serialize)]
pub struct StartResponse {
    pub worker_id: String,
    pub status: String,
}

pub async fn start_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<Json<StartResponse>, ApiError> {
    let app = db::sqlite::get_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    // Already running — return existing state
    let existing_worker = state.workers.iter()
        .find(|entry| entry.value().app_id == id)
        .map(|entry| entry.key().clone());
    if let Some(worker_id) = existing_worker {
        return Ok(Json(StartResponse {
            worker_id,
            status: "running".into(),
        }));
    }

    // Must have an active bundle
    let bundle_id = app.active_bundle.as_ref()
        .ok_or_else(|| bad_request(
            "app has no active bundle — upload and build a bundle first".into()
        ))?;

    // Check global worker limit
    if state.workers.len() >= state.config.proxy.max_workers as usize {
        return Err(service_unavailable("max workers reached".into()));
    }

    // Build WorkerSpec
    let worker_id = uuid::Uuid::new_v4().to_string();
    let paths = crate::bundle::BundlePaths::for_bundle(
        &state.config.storage.bundle_server_path,
        &app.id,
        bundle_id,
    );

    let image = state.config.docker.as_ref()
        .map(|d| d.image.clone())
        .unwrap_or_else(|| "rocker/r-ver:latest".into());

    let shiny_port = state.config.docker.as_ref()
        .map(|d| d.shiny_port)
        .unwrap_or(3838);

    let mut labels = std::collections::HashMap::new();
    labels.insert("dev.blockyard/app-id".into(), app.id.clone());
    labels.insert("dev.blockyard/worker-id".into(), worker_id.clone());

    let spec = crate::backend::WorkerSpec {
        app_id: app.id.clone(),
        worker_id: worker_id.clone(),
        image,
        bundle_path: paths.unpacked,
        library_path: paths.library,
        worker_mount: state.config.storage.bundle_worker_path.clone(),
        shiny_port,
        memory_limit: app.memory_limit.clone(),
        cpu_limit: app.cpu_limit,
        labels,
    };

    // Spawn worker
    let handle = state.backend.spawn(&spec)
        .await
        .map_err(|e| server_error(format!("spawn worker: {e}")))?;

    // Track worker — no session yet, assigned by proxy in phase 0-5
    state.workers.insert(worker_id.clone(), crate::app::ActiveWorker {
        app_id: app.id.clone(),
        handle,
        session_id: None,
    });

    Ok(Json(StartResponse {
        worker_id,
        status: "running".into(),
    }))
}
```

The start endpoint spawns a single worker. In v0, the proxy (phase 0-5)
will also spawn workers on-demand. The start endpoint is for explicit
pre-warming — e.g. start the app before the first user hits it.

The "already running" check uses the workers DashMap, not a stored DB
status. If any worker exists for this app, the app is running.

`session_id` is set to `None` because the start endpoint creates a
worker without a session. The proxy (phase 0-5) sets it to
`Some(session_id)` when a user session is assigned to this worker.

### Step 9: App lifecycle — stop

`POST /api/v1/apps/{id}/stop` — stop all workers for an app.

```rust
pub async fn stop_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<Json<serde_json::Value>, ApiError> {
    let app = db::sqlite::get_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    let stopped = stop_app_workers(&state, &app.id).await;

    Ok(Json(serde_json::json!({
        "status": "stopped",
        "workers_stopped": stopped,
    })))
}
```

**Shared helper — stop all workers for an app:**

```rust
/// Stop all workers belonging to the given app. Returns the count of
/// workers stopped. Errors from individual worker stops are logged
/// but do not fail the operation — best-effort cleanup.
async fn stop_app_workers<B: Backend>(state: &AppState<B>, app_id: &str) -> usize {
    let worker_ids: Vec<String> = state.workers.iter()
        .filter(|entry| entry.value().app_id == app_id)
        .map(|entry| entry.key().clone())
        .collect();

    let mut stopped = 0;
    for worker_id in &worker_ids {
        if let Some((_, worker)) = state.workers.remove(worker_id) {
            if let Err(e) = state.backend.stop(&worker.handle).await {
                tracing::warn!(
                    worker_id,
                    app_id,
                    error = %e,
                    "failed to stop worker"
                );
            }
            stopped += 1;
        }
    }

    stopped
}
```

`stop_app_workers` is used by both `stop_app` and `delete_app`. It
removes workers from the DashMap first, then stops them via the backend.
This ordering means that concurrent requests won't try to route to a
worker that is being torn down.

### Step 10: App log streaming

`GET /api/v1/apps/{id}/logs` — stream logs from a running worker.

Query parameters:
- `worker_id` (optional) — stream logs from a specific worker. If
  omitted, pick the first worker found for this app.
- `follow` (optional, bool, default true) — whether to follow live
  output or return buffered logs and close.

```rust
#[derive(serde::Deserialize)]
pub struct LogsQuery {
    pub worker_id: Option<String>,
    #[serde(default = "default_true")]
    pub follow: bool,
}

fn default_true() -> bool { true }

pub async fn app_logs<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
    Query(query): Query<LogsQuery>,
) -> Result<axum::response::Response, ApiError> {
    let _app = db::sqlite::get_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    // Find the worker
    let (worker_id, worker) = if let Some(ref wid) = query.worker_id {
        let w = state.workers.get(wid)
            .ok_or_else(|| not_found(format!("worker {wid} not found")))?;
        (wid.clone(), w.clone())
    } else {
        // Find any worker for this app
        state.workers.iter()
            .find(|entry| entry.value().app_id == id)
            .map(|entry| (entry.key().clone(), entry.value().clone()))
            .ok_or_else(|| not_found("no running workers for this app".into()))?
    };

    // Verify the worker belongs to this app
    if worker.app_id != id {
        return Err(not_found(format!(
            "worker {worker_id} does not belong to app {id}"
        )));
    }

    // Get log stream from backend
    let rx = state.backend.logs(&worker.handle)
        .await
        .map_err(|e| server_error(format!("log stream: {e}")))?;

    // Convert to streaming response
    let stream = tokio_stream::wrappers::ReceiverStream::new(rx)
        .map(|line| Ok::<_, std::io::Error>(format!("{line}\n")));

    let body = axum::body::Body::from_stream(stream);
    let response = axum::response::Response::builder()
        .header("content-type", "text/plain")
        .header("transfer-encoding", "chunked")
        .body(body)
        .unwrap();

    Ok(response)
}
```

The log endpoint streams lines from `backend.logs()`, which returns a
`tokio::sync::mpsc::Receiver<String>`. The stream stays open as long as
the backend produces output (follow behavior). If `follow=false`, the
backend implementation should close the stream after returning buffered
output — but this is a backend concern, not the API layer's. In v0
the Docker backend's `logs()` always follows; a non-following mode can
be added later if needed.

### Step 11: Router expansion

Update `api_router` in `src/api/mod.rs` to wire all endpoints:

```rust
pub mod apps;
pub mod auth;
pub mod bundles;
pub mod error;
pub mod tasks;

pub fn api_router<B: Backend + Clone>(state: AppState<B>) -> Router<AppState<B>> {
    let max_body = state.config.storage.max_bundle_size;

    let authed = Router::new()
        .route("/apps", post(apps::create_app::<B>).get(apps::list_apps::<B>))
        .route(
            "/apps/{id}",
            get(apps::get_app::<B>)
                .patch(apps::update_app::<B>)
                .delete(apps::delete_app::<B>),
        )
        .route(
            "/apps/{id}/bundles",
            post(bundles::upload_bundle::<B>).get(bundles::list_bundles::<B>),
        )
        .route("/apps/{id}/start", post(apps::start_app::<B>))
        .route("/apps/{id}/stop", post(apps::stop_app::<B>))
        .route("/apps/{id}/logs", get(apps::app_logs::<B>))
        .route("/tasks/{task_id}/logs", get(tasks::task_logs::<B>))
        .layer(axum::extract::DefaultBodyLimit::max(max_body))
        .layer(middleware::from_fn_with_state(
            state,
            auth::bearer_auth::<B>,
        ));

    Router::new()
        .nest("/api/v1", authed)
        .route("/healthz", axum::routing::get(healthz))
}
```

### Step 12: New dependency

```toml
tokio-stream = "0.1"   # ReceiverStream wrapper for log streaming
```

`tokio-stream` is used in the app logs endpoint to convert
`mpsc::Receiver` into a `Stream` that `Body::from_stream` can consume.
If this dependency is already pulled in transitively, make it explicit.

### Step 13: Integration tests

Extend `tests/bundle_test.rs` (or create `tests/api_test.rs`) with
tests for the new endpoints. All tests use the existing
`spawn_test_server()` helper with `MockBackend`.

**App CRUD tests:**

```rust
#[tokio::test]
async fn create_app_returns_201() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send()
        .await
        .unwrap();

    assert_eq!(resp.status(), 201);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["name"], "my-app");
    assert_eq!(body["status"], "stopped");
}

#[tokio::test]
async fn create_app_rejects_invalid_name() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    // Uppercase
    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "My-App" }))
        .send().await.unwrap();
    assert_eq!(resp.status(), 400);

    // Leading hyphen
    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "-app" }))
        .send().await.unwrap();
    assert_eq!(resp.status(), 400);
}

#[tokio::test]
async fn create_duplicate_name_returns_409() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();

    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();

    assert_eq!(resp.status(), 409);
}

#[tokio::test]
async fn list_apps_returns_all() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    for name in ["app-a", "app-b"] {
        client.post(format!("http://{addr}/api/v1/apps"))
            .bearer_auth("test-token")
            .json(&serde_json::json!({ "name": name }))
            .send().await.unwrap();
    }

    let resp = client.get(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .send().await.unwrap();

    assert_eq!(resp.status(), 200);
    let body: Vec<serde_json::Value> = resp.json().await.unwrap();
    assert_eq!(body.len(), 2);
}

#[tokio::test]
async fn get_app_returns_details() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client.get(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["name"], "my-app");
}

#[tokio::test]
async fn get_nonexistent_app_returns_404() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    let resp = client.get(format!("http://{addr}/api/v1/apps/nonexistent"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 404);
}

#[tokio::test]
async fn update_app_modifies_fields() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client.patch(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "memory_limit": "512m" }))
        .send().await.unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["memory_limit"], "512m");
}

#[tokio::test]
async fn delete_app_returns_204() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client.delete(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 204);

    let resp = client.get(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 404);
}
```

**App lifecycle tests:**

```rust
#[tokio::test]
async fn start_app_without_bundle_returns_400() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let resp = client.post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 400);
}

#[tokio::test]
async fn start_and_stop_app() {
    let (addr, state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    // Create app
    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    // Upload bundle and wait for restore
    let bundle_bytes = create_test_bundle();
    client.post(format!("http://{addr}/api/v1/apps/{id}/bundles"))
        .bearer_auth("test-token")
        .body(bundle_bytes)
        .send().await.unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;

    // Start
    let resp = client.post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["status"], "running");
    assert!(!body["worker_id"].as_str().unwrap().is_empty());

    // Verify worker was spawned in mock backend
    assert_eq!(state.workers.len(), 1);

    // Start again — should be no-op
    let resp = client.post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 200);
    assert_eq!(state.workers.len(), 1); // still 1

    // Stop
    let resp = client.post(format!("http://{addr}/api/v1/apps/{id}/stop"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 200);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["status"], "stopped");
    assert_eq!(body["workers_stopped"], 1);

    assert_eq!(state.workers.len(), 0);
}
```

**Delete with running workers:**

```rust
#[tokio::test]
async fn delete_app_stops_workers_first() {
    let (addr, state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    // Create app, upload bundle, start
    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();
    let created: serde_json::Value = resp.json().await.unwrap();
    let id = created["id"].as_str().unwrap();

    let bundle_bytes = create_test_bundle();
    client.post(format!("http://{addr}/api/v1/apps/{id}/bundles"))
        .bearer_auth("test-token")
        .body(bundle_bytes)
        .send().await.unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;

    client.post(format!("http://{addr}/api/v1/apps/{id}/start"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(state.workers.len(), 1);

    // Delete — should stop workers and clean up
    let resp = client.delete(format!("http://{addr}/api/v1/apps/{id}"))
        .bearer_auth("test-token")
        .send().await.unwrap();
    assert_eq!(resp.status(), 204);
    assert_eq!(state.workers.len(), 0);
}
```

## Complete endpoint table

| Endpoint | Method | Status | Behavior |
|---|---|---|---|
| `/api/v1/apps` | POST | **new** | Create app. Body: `{ "name": "..." }`. Returns 201 with app object. |
| `/api/v1/apps` | GET | **new** | List all apps. Returns array. |
| `/api/v1/apps/{id}` | GET | **new** | Get app details. |
| `/api/v1/apps/{id}` | PATCH | **new** | Update app config. Body: partial fields. |
| `/api/v1/apps/{id}` | DELETE | **new** | Delete app. Stops workers, removes files, deletes DB rows. Returns 204. |
| `/api/v1/apps/{id}/bundles` | POST | 0-3 | Upload bundle. Returns 202. |
| `/api/v1/apps/{id}/bundles` | GET | 0-3 | List bundles. |
| `/api/v1/apps/{id}/start` | POST | **new** | Start app. Spawns worker. No-op if running. |
| `/api/v1/apps/{id}/stop` | POST | **new** | Stop app. Stops all workers. |
| `/api/v1/apps/{id}/logs` | GET | **new** | Stream worker logs. Chunked text/plain. |
| `/api/v1/tasks/{task_id}/logs` | GET | 0-3 | Stream task logs. |
| `/healthz` | GET | 0-3 | Returns 200. No auth. |

## Implementation notes

- **App status is derived, not stored.** The `status` column is removed
  from the `apps` table. Runtime state ("running" / "stopped") is
  computed from the workers DashMap at request time. This avoids
  staleness on server crash/restart and eliminates the sync obligation
  between in-memory state and the DB. The API returns `AppResponse`
  (which wraps `AppRow` + derived `status`) so callers still see a
  `status` field in the JSON. If a persistent "admin intent" field is
  needed later (e.g. "disabled"), add a new column with a clear name.

- **`stop_app_workers` is best-effort.** Individual worker stop failures
  are logged but don't block the operation. If a container is already
  gone (e.g. it crashed), the stop call fails but the worker is still
  removed from the DashMap. The Docker backend already handles
  idempotent stop/remove (ignores 404/304/409 errors from the Docker
  API).

- **No drain on stop.** When `POST /apps/{id}/stop` is called, workers
  are stopped immediately without waiting for in-flight requests to
  complete. Graceful drain is a v1 feature alongside session sharing.

- **Worker lookup by app_id scans the DashMap.** `state.workers` is
  keyed by `worker_id`, not `app_id`. Finding workers for an app
  requires iterating all entries. With `max_workers = 100`, this is a
  trivial scan. If v2 needs faster lookup, add a secondary index
  (`DashMap<String, Vec<String>>` mapping app_id → worker_ids).

- **Start does not health-check.** The start endpoint spawns the worker
  and returns immediately. It does not wait for the worker to become
  healthy. The proxy layer (phase 0-5) handles cold-start holding —
  when a request arrives for a starting worker, the proxy polls
  `health_check` until the worker is ready or the timeout expires.
  The start endpoint's job is just to pre-warm the container.

- **Log streaming and `follow` parameter.** The `follow` query param is
  accepted but the Docker backend's `logs()` always follows. A
  non-following mode (return buffered output and close) is not needed
  for v0 — callers can simply close the connection when they've read
  enough. If non-following is needed later, it's a backend-level
  change, not an API-level one.

- **Test helper `create_test_bundle`.** The integration tests need a
  function that creates a minimal valid tar.gz for upload. This already
  exists in the phase 0-3 test suite — reuse it.

## Exit criteria

Phase 0-4 is done when:

- `status` column removed from `apps` table via migration
- App status is derived from workers DashMap, not stored
- `AppResponse` includes computed `status` field in all app endpoints
- `ActiveWorker.session_id` is `Option<String>`
- `BundlePaths::for_bundle` is the single source of truth for bundle
  paths, used by both bundle upload and worker start
- `POST /api/v1/apps` creates an app with a validated name, returns 201
- `GET /api/v1/apps` lists all apps with derived status
- `GET /api/v1/apps/{id}` returns app details or 404
- `PATCH /api/v1/apps/{id}` updates resource limits, returns updated app
- `DELETE /api/v1/apps/{id}` stops workers, cleans up files, returns 204
- `POST /api/v1/apps/{id}/start` spawns a worker, returns worker_id
- `POST /api/v1/apps/{id}/stop` stops all workers, returns count
- `GET /api/v1/apps/{id}/logs` streams worker logs as chunked text
- Invalid app names are rejected with 400
- Duplicate app names are rejected with 409
- Starting without an active bundle returns 400
- Starting at max_workers limit returns 503
- Error responses follow the `{ "error": "...", "message": "..." }` shape
- All existing phase 0-3 tests still pass
- All new integration tests pass
- `cargo clippy` clean
- `cargo test --features test-support` green
