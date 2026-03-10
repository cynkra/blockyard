pub mod cold_start;
pub mod forward;
pub mod registry;
pub mod session;
pub mod ws_cache;

use axum::extract::ws::WebSocketUpgrade;
use axum::extract::{Path, Request, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Router;

use crate::app::AppState;
use crate::backend::Backend;

/// Build the full application router: API (control plane) + proxy (data plane).
pub fn full_router<B: Backend + Clone>(state: AppState<B>) -> Router {
    let api = crate::api::api_router(state.clone());

    Router::new()
        .merge(api)
        .route(
            "/app/{name}",
            axum::routing::get(trailing_slash_redirect),
        )
        // Two routes needed: {*rest} doesn't match an empty/root path
        .route(
            "/app/{name}/",
            axum::routing::any(proxy_handler_root::<B>),
        )
        .route(
            "/app/{name}/{*rest}",
            axum::routing::any(proxy_handler::<B>),
        )
        .with_state(state)
}

pub async fn trailing_slash_redirect(
    Path(name): Path<String>,
    req: Request,
) -> Response {
    let query = req
        .uri()
        .query()
        .map(|q| format!("?{q}"))
        .unwrap_or_default();
    let location = format!("/app/{name}/{query}");
    (
        StatusCode::MOVED_PERMANENTLY,
        [(axum::http::header::LOCATION, location)],
    )
        .into_response()
}

/// Handler for `/app/{name}/` (root path, no wildcard segment).
pub async fn proxy_handler_root<B: Backend + Clone>(
    Path(name): Path<String>,
    State(state): State<AppState<B>>,
    req: Request,
) -> Response {
    use axum::extract::FromRequestParts;
    let (mut parts, body) = req.into_parts();
    let ws = WebSocketUpgrade::from_request_parts(&mut parts, &state)
        .await
        .ok();
    let req = Request::from_parts(parts, body);

    match proxy_request(&name, &state, ws, req).await {
        Ok(resp) => resp,
        Err(status) => status.into_response(),
    }
}

pub async fn proxy_handler<B: Backend + Clone>(
    Path((name, _rest)): Path<(String, String)>,
    State(state): State<AppState<B>>,
    req: Request,
) -> Response {
    // Manually extract WebSocket upgrade from request parts.
    // This avoids axum's Option<WebSocketUpgrade> extractor which
    // causes Handler trait issues with the other extractors.
    use axum::extract::FromRequestParts;
    let (mut parts, body) = req.into_parts();
    let ws = WebSocketUpgrade::from_request_parts(&mut parts, &state)
        .await
        .ok();
    let req = Request::from_parts(parts, body);

    match proxy_request(&name, &state, ws, req).await {
        Ok(resp) => resp,
        Err(status) => status.into_response(),
    }
}

async fn proxy_request<B: Backend + Clone>(
    app_name: &str,
    state: &AppState<B>,
    ws: Option<WebSocketUpgrade>,
    req: Request,
) -> Result<Response, StatusCode> {
    // 1. Look up the app by name
    let app = crate::db::sqlite::get_app_by_name(&state.db, app_name)
        .await
        .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?
        .ok_or(StatusCode::NOT_FOUND)?;

    // 2. Must have an active bundle
    let bundle_id = app
        .active_bundle
        .as_ref()
        .ok_or(StatusCode::SERVICE_UNAVAILABLE)?;

    // 3. Check for existing session
    let existing_session = session::extract_session_id(req.headers());
    let (worker_id, is_new_session) = match existing_session
        .as_ref()
        .and_then(|sid| state.sessions.get(sid).map(|wid| (sid.clone(), wid)))
    {
        Some((sid, wid)) => {
            // Verify worker is still alive
            if state.registry.get(&wid).is_some() {
                (wid, false)
            } else {
                // Worker gone — clean up stale session, treat as new
                state.sessions.remove(&sid);
                let wid = cold_start::ensure_worker(state, &app, bundle_id).await?;
                (wid, true)
            }
        }
        None => {
            // New session — spawn or find a worker
            let wid = cold_start::ensure_worker(state, &app, bundle_id).await?;
            (wid, true)
        }
    };

    // 4. Generate session ID if new
    let session_id = if is_new_session {
        let sid = uuid::Uuid::new_v4().to_string();
        state.sessions.insert(sid.clone(), worker_id.clone());

        // Update the ActiveWorker's session_id
        if let Some(mut worker) = state.workers.get_mut(&worker_id) {
            worker.session_id = Some(sid.clone());
        }

        sid
    } else {
        existing_session.unwrap()
    };

    // 5. Resolve worker address
    let addr = state
        .registry
        .get(&worker_id)
        .ok_or(StatusCode::BAD_GATEWAY)?;

    // 6. Dispatch: WebSocket upgrade or HTTP forward
    if let Some(ws) = ws {
        let state: AppState<B> = (*state).clone();
        let app_name = app_name.to_string();
        let session_id_owned = session_id.clone();
        let path = strip_prefix(req.uri().path(), &app_name);

        let mut response = ws
            .on_upgrade(move |client_ws| async move {
                if let Err(e) = forward::shuttle_ws(
                    client_ws,
                    addr,
                    &path,
                    &session_id_owned,
                    &state,
                )
                .await
                {
                    tracing::debug!(
                        error = %e,
                        worker_id = %worker_id,
                        "websocket proxy ended"
                    );
                }
            })
            .into_response();

        if is_new_session {
            response.headers_mut().insert(
                axum::http::header::SET_COOKIE,
                session::session_cookie(&session_id, &app_name)
                    .parse()
                    .unwrap(),
            );
        }

        Ok(response)
    } else {
        let mut response = forward::forward_http(req, addr, app_name)
            .await
            .map_err(|_| StatusCode::BAD_GATEWAY)?;

        if is_new_session {
            response.headers_mut().insert(
                axum::http::header::SET_COOKIE,
                session::session_cookie(&session_id, app_name)
                    .parse()
                    .unwrap(),
            );
        }

        Ok(response)
    }
}

fn strip_prefix(path: &str, app_name: &str) -> String {
    let prefix = format!("/app/{app_name}");
    let stripped = path.strip_prefix(&prefix).unwrap_or(path);
    if stripped.is_empty() {
        "/".to_string()
    } else {
        stripped.to_string()
    }
}
