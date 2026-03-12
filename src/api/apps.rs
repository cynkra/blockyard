use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::response::Json;
use futures_util::StreamExt;

use super::error::{ApiError, bad_request, conflict, not_found, server_error, service_unavailable};
use crate::app::AppState;
use crate::backend::Backend;
use crate::db;
use crate::db::sqlite::AppRow;
use crate::ops;

#[derive(serde::Serialize)]
pub struct AppResponse {
    #[serde(flatten)]
    pub app: AppRow,
    pub status: String,
}

fn app_status<B: Backend>(state: &AppState<B>, app_id: &str) -> String {
    let has_workers = state
        .workers
        .iter()
        .any(|entry| entry.value().app_id == app_id);
    if has_workers {
        "running".into()
    } else {
        "stopped".into()
    }
}

fn validate_app_name(name: &str) -> Result<(), String> {
    if name.is_empty() || name.len() > 63 {
        return Err("name must be 1-63 characters".into());
    }
    if !name
        .chars()
        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
    {
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

// --- CRUD ---

#[derive(serde::Deserialize)]
pub struct CreateAppRequest {
    pub name: String,
}

pub async fn create_app<B: Backend>(
    State(state): State<AppState<B>>,
    Json(body): Json<CreateAppRequest>,
) -> Result<(StatusCode, Json<AppResponse>), ApiError> {
    validate_app_name(&body.name).map_err(bad_request)?;

    let existing = db::sqlite::get_app_by_name(&state.db, &body.name)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?;
    if existing.is_some() {
        return Err(conflict(format!("app name '{}' already exists", body.name)));
    }

    let app = db::sqlite::create_app(&state.db, &body.name)
        .await
        .map_err(|e| server_error(format!("create app: {e}")))?;

    Ok((
        StatusCode::CREATED,
        Json(AppResponse {
            app,
            status: "stopped".into(),
        }),
    ))
}

pub async fn list_apps<B: Backend>(
    State(state): State<AppState<B>>,
) -> Result<Json<Vec<AppResponse>>, ApiError> {
    let apps = db::sqlite::list_apps(&state.db)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?;
    let responses = apps
        .into_iter()
        .map(|app| {
            let status = app_status(&state, &app.id);
            AppResponse { app, status }
        })
        .collect();
    Ok(Json(responses))
}

pub async fn get_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<Json<AppResponse>, ApiError> {
    let app = db::sqlite::resolve_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;
    let status = app_status(&state, &app.id);
    Ok(Json(AppResponse { app, status }))
}

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
    if let Some(v) = body.max_sessions_per_worker
        && v != 1
    {
        return Err(bad_request(
            "max_sessions_per_worker must be 1 in this version".to_string(),
        ));
    }

    // Verify app exists and resolve name → ID
    let existing = db::sqlite::resolve_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    let app = db::sqlite::update_app(
        &state.db,
        &existing.id,
        body.max_workers_per_app.map(Some),
        body.max_sessions_per_worker,
        body.memory_limit.map(Some),
        body.cpu_limit.map(Some),
    )
    .await
    .map_err(|e| server_error(format!("update app: {e}")))?;

    let status = app_status(&state, &app.id);
    Ok(Json(AppResponse { app, status }))
}

pub async fn delete_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<StatusCode, ApiError> {
    let app = db::sqlite::resolve_app(&state.db, &id)
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

// --- Lifecycle ---

#[derive(serde::Serialize)]
pub struct StartResponse {
    pub worker_id: String,
    pub status: String,
}

pub async fn start_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<Json<StartResponse>, ApiError> {
    let app = db::sqlite::resolve_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    // Already running — return existing state
    let existing_worker = state
        .workers
        .iter()
        .find(|entry| entry.value().app_id == app.id)
        .map(|entry| entry.key().clone());
    if let Some(worker_id) = existing_worker {
        return Ok(Json(StartResponse {
            worker_id,
            status: "running".into(),
        }));
    }

    // Must have an active bundle
    let bundle_id = app.active_bundle.as_ref().ok_or_else(|| {
        conflict("app has no active bundle — upload and build a bundle first".into())
    })?;

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

    let image = state
        .config
        .docker
        .as_ref()
        .map(|d| d.image.clone())
        .unwrap_or_else(|| crate::config::DEFAULT_IMAGE.into());

    let shiny_port = state
        .config
        .docker
        .as_ref()
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
        cmd: Some(vec![
            "Rscript".into(),
            format!(
                "{}/app.R",
                state.config.storage.bundle_worker_path.display()
            ),
        ]),
        memory_limit: app.memory_limit.clone(),
        cpu_limit: app.cpu_limit,
        labels,
    };

    // Spawn worker
    let handle = state
        .backend
        .spawn(&spec)
        .await
        .map_err(|e| server_error(format!("spawn worker: {e}")))?;

    // Start log capture
    ops::spawn_log_capture(&state, worker_id.clone(), app.id.clone(), handle.clone());

    // Track worker — no session yet, assigned by proxy in phase 0-5
    state.workers.insert(
        worker_id.clone(),
        crate::app::ActiveWorker {
            app_id: app.id.clone(),
            handle,
            session_id: None,
        },
    );

    Ok(Json(StartResponse {
        worker_id,
        status: "running".into(),
    }))
}

pub async fn stop_app<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
) -> Result<Json<serde_json::Value>, ApiError> {
    let app = db::sqlite::resolve_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    let stopped = stop_app_workers(&state, &app.id).await;

    Ok(Json(serde_json::json!({
        "status": "stopped",
        "workers_stopped": stopped,
    })))
}

/// Stop all workers belonging to the given app. Returns the count of
/// workers stopped. Errors from individual worker stops are logged
/// but do not fail the operation — best-effort cleanup.
async fn stop_app_workers<B: Backend>(state: &AppState<B>, app_id: &str) -> usize {
    let worker_ids: Vec<String> = state
        .workers
        .iter()
        .filter(|entry| entry.value().app_id == app_id)
        .map(|entry| entry.key().clone())
        .collect();

    let count = worker_ids.len();
    for worker_id in &worker_ids {
        ops::evict_worker(state, worker_id).await;
    }

    count
}

// --- Logs ---

#[derive(serde::Deserialize)]
pub struct LogsQuery {
    pub worker_id: Option<String>,
}

pub async fn app_logs<B: Backend>(
    State(state): State<AppState<B>>,
    Path(id): Path<String>,
    Query(query): Query<LogsQuery>,
) -> Result<axum::response::Response, ApiError> {
    let app = db::sqlite::resolve_app(&state.db, &id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {id} not found")))?;

    // Subscribe from log store
    let sub = if let Some(ref wid) = query.worker_id {
        // Verify the worker belongs to this app if it's still tracked
        if let Some(w) = state.workers.get(wid)
            && w.value().app_id != app.id
        {
            return Err(not_found(format!(
                "worker {wid} does not belong to app {id}"
            )));
        }
        state
            .log_store
            .subscribe(wid)
            .await
            .ok_or_else(|| not_found(format!("no logs for worker {wid}")))?
    } else {
        state
            .log_store
            .subscribe_by_app(&app.id)
            .await
            .map(|(_, sub)| sub)
            .ok_or_else(|| not_found("no logs available for this app".into()))?
    };

    if sub.ended {
        // Worker has exited — return buffered lines as a complete response
        let body = sub.lines.join("\n");
        let body = if body.is_empty() { body } else { body + "\n" };
        let response = axum::response::Response::builder()
            .header("content-type", "text/plain")
            .body(axum::body::Body::from(body))
            .unwrap();
        return Ok(response);
    }

    // Stream buffered lines + live broadcast
    let skip = sub.lines.len();
    let buffered = futures_util::stream::iter(
        sub.lines
            .into_iter()
            .map(|line| Ok::<_, std::io::Error>(format!("{line}\n"))),
    );

    let live = tokio_stream::wrappers::BroadcastStream::new(sub.rx)
        .skip(skip)
        .filter_map(|result| async move {
            match result {
                Ok(line) => Some(Ok(format!("{line}\n"))),
                Err(_) => None, // lagged or closed
            }
        });

    let stream = buffered.chain(live);
    let body = axum::body::Body::from_stream(stream);
    let response = axum::response::Response::builder()
        .header("content-type", "text/plain")
        .header("transfer-encoding", "chunked")
        .body(body)
        .unwrap();

    Ok(response)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn valid_app_names() {
        assert!(validate_app_name("a").is_ok());
        assert!(validate_app_name("my-app").is_ok());
        assert!(validate_app_name("app123").is_ok());
        assert!(validate_app_name("a1-b2-c3").is_ok());
    }

    #[test]
    fn invalid_app_names() {
        // empty
        assert!(validate_app_name("").is_err());
        // too long
        assert!(validate_app_name(&"a".repeat(64)).is_err());
        // uppercase
        assert!(validate_app_name("My-App").is_err());
        // starts with digit
        assert!(validate_app_name("1app").is_err());
        // starts with hyphen
        assert!(validate_app_name("-app").is_err());
        // ends with hyphen
        assert!(validate_app_name("app-").is_err());
        // special chars
        assert!(validate_app_name("app_name").is_err());
        assert!(validate_app_name("app.name").is_err());
    }
}
