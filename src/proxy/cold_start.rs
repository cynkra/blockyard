use std::time::Duration;

use axum::http::StatusCode;

use crate::app::{ActiveWorker, AppState};
use crate::backend::Backend;
use crate::db::sqlite::AppRow;
use crate::ops;

/// Ensure a running, healthy worker exists for the given app.
/// Returns the worker ID.
///
/// If a worker already exists, returns it immediately.
/// If no worker exists, spawns one and polls health_check until ready.
pub async fn ensure_worker<B: Backend>(
    state: &AppState<B>,
    app: &AppRow,
    bundle_id: &str,
) -> Result<String, StatusCode> {
    // Check for an existing worker
    if let Some(entry) = state.workers.iter().find(|e| e.value().app_id == app.id) {
        return Ok(entry.key().clone());
    }

    // Check global worker limit
    if state.workers.len() >= state.config.proxy.max_workers as usize {
        return Err(StatusCode::SERVICE_UNAVAILABLE);
    }

    // Check per-app worker limit (if configured)
    if let Some(max) = app.max_workers_per_app {
        let app_worker_count = state
            .workers
            .iter()
            .filter(|e| e.value().app_id == app.id)
            .count();
        if app_worker_count >= max as usize {
            return Err(StatusCode::SERVICE_UNAVAILABLE);
        }
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
        labels: std::collections::HashMap::new(),
    };

    // Spawn the worker
    let handle = state.backend.spawn(&spec).await.map_err(|e| {
        tracing::error!(error = %e, app_id = %app.id, "failed to spawn worker");
        StatusCode::INTERNAL_SERVER_ERROR
    })?;

    // Resolve and cache the worker's address
    let addr = state.backend.addr(&handle).await.map_err(|e| {
        tracing::error!(error = %e, "failed to resolve worker address");
        StatusCode::INTERNAL_SERVER_ERROR
    })?;
    state.registry.insert(worker_id.clone(), addr);

    // Start log capture
    ops::spawn_log_capture(state, worker_id.clone(), app.id.clone(), handle.clone());

    // Track the worker
    state.workers.insert(
        worker_id.clone(),
        ActiveWorker {
            app_id: app.id.clone(),
            handle: handle.clone(),
            session_id: None,
        },
    );

    // Poll health_check until ready or timeout
    let timeout = state.config.proxy.worker_start_timeout;
    if !poll_healthy(state, &handle, timeout).await {
        tracing::warn!(
            worker_id = %worker_id,
            app_id = %app.id,
            "worker did not become healthy within timeout"
        );
        ops::evict_worker(state, &worker_id).await;
        return Err(StatusCode::GATEWAY_TIMEOUT);
    }

    tracing::info!(
        worker_id = %worker_id,
        app_id = %app.id,
        addr = %addr,
        "worker is healthy"
    );

    Ok(worker_id)
}

/// Poll backend.health_check until it returns true or the deadline
/// expires. Uses exponential backoff starting at 100ms, capped at 2s.
async fn poll_healthy<B: Backend>(
    state: &AppState<B>,
    handle: &B::Handle,
    timeout: Duration,
) -> bool {
    let start = tokio::time::Instant::now();
    let mut interval = Duration::from_millis(100);
    let max_interval = Duration::from_secs(2);

    loop {
        if state.backend.health_check(handle).await {
            return true;
        }

        if start.elapsed() >= timeout {
            return false;
        }

        tokio::time::sleep(interval).await;
        interval = (interval * 2).min(max_interval);
    }
}
