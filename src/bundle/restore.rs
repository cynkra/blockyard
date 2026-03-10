use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

use sqlx::SqlitePool;

use crate::backend::{Backend, BuildSpec};
use crate::bundle::BundlePaths;
use crate::db;
use crate::task::{InMemoryTaskStore, TaskSender};

/// Parameters for spawning a restore task.
pub struct RestoreParams<B: Backend> {
    pub backend: Arc<B>,
    pub pool: SqlitePool,
    pub task_store: Arc<InMemoryTaskStore>,
    pub task_sender: TaskSender,
    pub app_id: String,
    pub bundle_id: String,
    pub paths: BundlePaths,
    pub image: String,
    pub retention: u32,
    pub bundle_server_path: PathBuf,
}

/// Spawn the async restore task. Returns immediately — the restore runs
/// in a background tokio task.
pub fn spawn_restore<B: Backend>(params: RestoreParams<B>) {
    let RestoreParams {
        backend,
        pool,
        task_store,
        task_sender,
        app_id,
        bundle_id,
        paths,
        image,
        retention,
        bundle_server_path,
    } = params;

    tokio::spawn(async move {
        let result = run_restore(
            &*backend,
            &pool,
            &task_sender,
            &app_id,
            &bundle_id,
            &paths,
            &image,
        )
        .await;

        match result {
            Ok(()) => {
                task_sender.complete(&task_store, true).await;
                // Enforce retention after successful deploy
                crate::bundle::enforce_retention(
                    &pool,
                    &bundle_server_path,
                    &app_id,
                    Some(&bundle_id),
                    retention,
                )
                .await;
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

    task_sender
        .send("Starting dependency restoration...".into())
        .await;

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
    let build_result = backend
        .build(&spec)
        .await
        .map_err(|e| crate::bundle::BundleError::Restore(format!("build: {e}")))?;

    if !build_result.success {
        let msg = format!("Build failed with exit code {:?}", build_result.exit_code);
        task_sender.send(msg.clone()).await;
        db::sqlite::update_bundle_status(pool, bundle_id, "failed")
            .await
            .map_err(|e| crate::bundle::BundleError::Restore(format!("update status: {e}")))?;
        return Err(crate::bundle::BundleError::Restore(msg));
    }

    // 4. Mark bundle as ready and activate
    task_sender
        .send("Build succeeded. Activating bundle...".into())
        .await;

    db::sqlite::update_bundle_status(pool, bundle_id, "ready")
        .await
        .map_err(|e| crate::bundle::BundleError::Restore(format!("update status: {e}")))?;

    db::sqlite::set_active_bundle(pool, app_id, bundle_id)
        .await
        .map_err(|e| crate::bundle::BundleError::Restore(format!("set active bundle: {e}")))?;

    task_sender.send("Bundle activated.".into()).await;
    Ok(())
}
