use axum::{Router, middleware};

use crate::app::AppState;
use crate::backend::Backend;

pub mod apps;
pub mod auth;
pub mod bundles;
pub mod error;
pub mod tasks;

pub fn api_router<B: Backend + Clone>(state: AppState<B>) -> Router<AppState<B>> {
    let max_body = state.config.storage.max_bundle_size;

    let authed = Router::new()
        .route(
            "/apps",
            axum::routing::post(apps::create_app::<B>).get(apps::list_apps::<B>),
        )
        .route(
            "/apps/{id}",
            axum::routing::get(apps::get_app::<B>)
                .patch(apps::update_app::<B>)
                .delete(apps::delete_app::<B>),
        )
        .route(
            "/apps/{id}/bundles",
            axum::routing::post(bundles::upload_bundle::<B>).get(bundles::list_bundles::<B>),
        )
        .route(
            "/apps/{id}/start",
            axum::routing::post(apps::start_app::<B>),
        )
        .route("/apps/{id}/stop", axum::routing::post(apps::stop_app::<B>))
        .route("/apps/{id}/logs", axum::routing::get(apps::app_logs::<B>))
        .route(
            "/tasks/{task_id}/logs",
            axum::routing::get(tasks::task_logs::<B>),
        )
        .layer(axum::extract::DefaultBodyLimit::max(max_body))
        .layer(middleware::from_fn_with_state(
            state,
            auth::bearer_auth::<B>,
        ));

    Router::new()
        .nest("/api/v1", authed)
        .route("/healthz", axum::routing::get(healthz))
}

async fn healthz() -> &'static str {
    "ok"
}
