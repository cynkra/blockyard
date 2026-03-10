use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::response::Json;
use bytes::Bytes;

use super::error::{ApiError, bad_request, not_found, server_error};
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
) -> Result<(StatusCode, Json<UploadResponse>), ApiError> {
    // 1. Validate app exists
    let app = crate::db::sqlite::resolve_app(&state.db, &app_id)
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
    if let Err(e) = bundle::unpack_archive(&paths).await {
        bundle::delete_bundle_files(&paths).await;
        return Err(server_error(format!("unpack: {e}")));
    }

    // 6. Create library dir
    if let Err(e) = bundle::create_library_dir(&paths).await {
        bundle::delete_bundle_files(&paths).await;
        return Err(server_error(format!("create lib dir: {e}")));
    }

    // 7. Insert bundle row (status = pending)
    if let Err(e) = crate::db::sqlite::create_bundle(
        &state.db,
        &bundle_id,
        &app.id,
        paths.archive.to_str().unwrap(),
    )
    .await
    {
        bundle::delete_bundle_files(&paths).await;
        return Err(server_error(format!("create bundle row: {e}")));
    }

    // 8. Create task in TaskStore
    let task_sender = state.task_store.create(task_id.clone());

    // 9. Get image name
    let image = state
        .config
        .docker
        .as_ref()
        .map(|d| d.image.clone())
        .unwrap_or_else(|| crate::config::DEFAULT_IMAGE.into());

    // 10. Spawn async restore
    bundle::restore::spawn_restore(bundle::restore::RestoreParams {
        backend: state.backend.clone(),
        pool: state.db.clone(),
        task_store: state.task_store.clone(),
        task_sender,
        app_id: app.id,
        bundle_id: bundle_id.clone(),
        paths,
        image,
        retention: state.config.storage.bundle_retention,
        bundle_server_path: state.config.storage.bundle_server_path.clone(),
    });

    // 11. Return 202
    Ok((
        StatusCode::ACCEPTED,
        Json(UploadResponse { bundle_id, task_id }),
    ))
}

pub async fn list_bundles<B: Backend>(
    State(state): State<AppState<B>>,
    Path(app_id): Path<String>,
) -> Result<Json<Vec<crate::db::sqlite::BundleRow>>, ApiError> {
    let app = crate::db::sqlite::resolve_app(&state.db, &app_id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?
        .ok_or_else(|| not_found(format!("app {app_id} not found")))?;

    let bundles = crate::db::sqlite::list_bundles_by_app(&state.db, &app.id)
        .await
        .map_err(|e| server_error(format!("db error: {e}")))?;
    Ok(Json(bundles))
}
