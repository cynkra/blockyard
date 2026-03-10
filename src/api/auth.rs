use axum::extract::State;
use axum::http::StatusCode;
use axum::middleware::Next;
use axum::response::Response;

use crate::app::AppState;
use crate::backend::Backend;

pub async fn bearer_auth<B: Backend>(
    State(state): State<AppState<B>>,
    req: axum::http::Request<axum::body::Body>,
    next: Next,
) -> Result<Response, StatusCode> {
    let token = req
        .headers()
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "));

    match token {
        Some(t) if t == state.config.server.token => Ok(next.run(req).await),
        _ => Err(StatusCode::UNAUTHORIZED),
    }
}
