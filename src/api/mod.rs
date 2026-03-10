use axum::{Router, middleware};

use crate::app::AppState;
use crate::backend::Backend;

pub mod auth;
pub mod bundles;
pub mod tasks;

pub fn api_router<B: Backend + Clone>(state: AppState<B>) -> Router<AppState<B>> {
    let authed = Router::new()
        .route(
            "/apps/{id}/bundles",
            axum::routing::post(bundles::upload_bundle::<B>).get(bundles::list_bundles::<B>),
        )
        .route(
            "/tasks/{task_id}/logs",
            axum::routing::get(tasks::task_logs::<B>),
        )
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
