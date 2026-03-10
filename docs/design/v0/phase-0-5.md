# Phase 0-5: HTTP/WebSocket Reverse Proxy

Route user traffic to Shiny app containers. This is the layer that makes
deployed apps accessible — without it, the server can manage containers
but nobody can use them. Covers session routing, HTTP/WS forwarding,
cold-start holding, and WebSocket session caching.

## Deliverables

1. `SessionStore` — in-memory session-to-worker mapping
2. `WorkerRegistry` — in-memory worker-to-address mapping
3. Session cookie middleware — set and read `blockyard_session` cookie
4. Proxy router — catch-all for `/app/{name}/` routes
5. HTTP reverse proxy — forward requests to the correct worker
6. WebSocket reverse proxy — upgrade handling and bidirectional forwarding
7. Trailing-slash redirect — `/app/{name}` → `/app/{name}/`
8. Path prefix stripping — remove `/app/{name}` before forwarding
9. Cold-start holding — hold initial request while worker starts, poll
   health until ready or `worker_start_timeout` expires
10. WebSocket session caching — hold backend WS connection for
    `ws_cache_ttl` on client disconnect
11. `AppState` additions — `SessionStore`, `WorkerRegistry`, and `WsCache` fields
12. Router composition — proxy routes alongside API routes in `main.rs`
13. Integration tests — end-to-end HTTP and WebSocket proxying through
    mock backend

## What's already done

Phase 0-4 delivered:

- App CRUD endpoints (create, list, get, update, delete)
- App lifecycle endpoints (start, stop)
- App log streaming
- `ActiveWorker` with `session_id: Option<String>`
- `BundlePaths::for_bundle` shared path constructor
- Bearer token auth middleware
- `workers: DashMap<String, ActiveWorker<B::Handle>>` on `AppState`
- All bundle management (upload, restore, retention)

Phase 0-2 delivered:

- `Backend` trait with `spawn`, `stop`, `health_check`, `addr`
- Docker backend with per-worker bridge networks
- Mock backend with real TCP listeners

## Architecture overview

The proxy sits alongside the API router. The API handles control plane
operations (`/api/v1/...`); the proxy handles data plane traffic
(`/app/{name}/...`). Both share `AppState`.

```
                    ┌─────────────────────────────────────┐
                    │           axum Router                │
                    │                                     │
                    │  /api/v1/*    → API handlers         │
                    │  /healthz    → liveness check        │
                    │  /app/{name} → trailing-slash redir   │
                    │  /app/{name}/* → proxy handler        │
                    └──────────┬──────────────────────────┘
                               │
              ┌────────────────┼────────────────┐
              │                │                │
      ┌───────▼──────┐  ┌─────▼──────┐  ┌──────▼──────┐
      │ SessionStore │  │  Worker    │  │  Backend    │
      │ (DashMap)    │  │  Registry  │  │  (Docker)   │
      │ session_id → │  │  (DashMap) │  │             │
      │  worker_id   │  │ worker_id →│  │ spawn/stop/ │
      │              │  │  SocketAddr│  │ health/addr │
      └──────────────┘  └────────────┘  └─────────────┘
```

**Request flow for an existing session:**

1. Request arrives at `/app/my-app/some/path`
2. Proxy reads `blockyard_session` cookie → session ID
3. `SessionStore::get(session_id)` → worker ID
4. `WorkerRegistry::addr(worker_id)` → `SocketAddr`
5. Strip `/app/my-app` prefix, forward to `http://{addr}/some/path`
6. Return response to client

**Request flow for a new session (cold start):**

1. Request arrives at `/app/my-app/` with no session cookie
2. Look up app by name in DB → get app record + active bundle
3. Check if an existing worker has capacity (v0: no, always spawn new)
4. `backend.spawn(spec)` → handle
5. Register in `WorkerRegistry` and `workers` DashMap
6. Poll `backend.health_check(handle)` until healthy or timeout
7. `SessionStore::insert(session_id, worker_id)`
8. Set `blockyard_session` cookie on response
9. Forward request to worker

## Step-by-step

### Step 1: SessionStore

`src/proxy/session.rs` — maps session IDs to worker IDs.

```rust
use dashmap::DashMap;

pub type SessionId = String;
pub type WorkerId = String;

/// Maps session IDs to the worker handling that session.
/// In v0, this is a 1:1 mapping (one session per worker).
/// In v1, many sessions may map to the same worker when
/// max_sessions_per_worker > 1.
pub struct SessionStore {
    sessions: DashMap<SessionId, WorkerId>,
}

impl SessionStore {
    pub fn new() -> Self {
        Self {
            sessions: DashMap::new(),
        }
    }

    pub fn get(&self, session_id: &str) -> Option<WorkerId> {
        self.sessions.get(session_id).map(|v| v.clone())
    }

    pub fn insert(&self, session_id: SessionId, worker_id: WorkerId) {
        self.sessions.insert(session_id, worker_id);
    }

    pub fn remove(&self, session_id: &str) -> Option<WorkerId> {
        self.sessions.remove(session_id).map(|(_, v)| v)
    }

    /// Remove all sessions pointing to the given worker.
    /// Used when a worker is stopped or crashes.
    pub fn remove_by_worker(&self, worker_id: &str) {
        self.sessions.retain(|_, v| v != worker_id);
    }

    /// Count sessions assigned to a specific worker.
    pub fn count_for_worker(&self, worker_id: &str) -> usize {
        self.sessions.iter().filter(|e| e.value() == worker_id).count()
    }
}
```

The `SessionStore` is intentionally a concrete struct, not a trait. The
roadmap defines it as a trait for future PostgreSQL-backed HA
implementations, but in v0 the only implementation is in-memory. Extract
the trait when the second implementation arrives (v2/k8s). This avoids
carrying trait object indirection through the entire proxy stack when
there is only one implementation.

### Step 2: WorkerRegistry

`src/proxy/registry.rs` — maps worker IDs to resolved socket addresses.

```rust
use std::net::SocketAddr;
use dashmap::DashMap;

pub type WorkerId = String;

/// Caches resolved worker addresses. The backend resolves the address
/// on spawn; the registry caches it so the proxy doesn't call
/// backend.addr() on every request.
pub struct WorkerRegistry {
    addrs: DashMap<WorkerId, SocketAddr>,
}

impl WorkerRegistry {
    pub fn new() -> Self {
        Self {
            addrs: DashMap::new(),
        }
    }

    pub fn get(&self, worker_id: &str) -> Option<SocketAddr> {
        self.addrs.get(worker_id).map(|v| *v)
    }

    pub fn insert(&self, worker_id: WorkerId, addr: SocketAddr) {
        self.addrs.insert(worker_id, addr);
    }

    pub fn remove(&self, worker_id: &str) -> Option<SocketAddr> {
        self.addrs.remove(worker_id).map(|(_, v)| v)
    }
}
```

Like `SessionStore`, this is a concrete struct. The resolved address
is cached at spawn time — `backend.addr(handle)` is called once after
`spawn()` completes, not on every request. Container IPs don't change
during a container's lifetime, so the cache never goes stale.

### Step 3: AppState additions

Add `SessionStore`, `WorkerRegistry`, and `WsCache` to `AppState`:

```rust
use crate::proxy::session::SessionStore;
use crate::proxy::registry::WorkerRegistry;
use crate::proxy::ws_cache::WsCache;

#[derive(Clone)]
pub struct AppState<B: Backend> {
    pub config: Arc<Config>,
    pub backend: Arc<B>,
    pub db: SqlitePool,
    pub workers: Arc<DashMap<String, ActiveWorker<B::Handle>>>,
    pub task_store: Arc<InMemoryTaskStore>,
    pub sessions: Arc<SessionStore>,
    pub registry: Arc<WorkerRegistry>,
    pub ws_cache: Arc<WsCache>,
}
```

Update `AppState::new()` to initialize all three:

```rust
impl<B: Backend> AppState<B> {
    pub fn new(config: Config, backend: B, db: SqlitePool) -> Self {
        let ws_cache_ttl = config.proxy.ws_cache_ttl;
        Self {
            config: Arc::new(config),
            backend: Arc::new(backend),
            db,
            workers: Arc::new(DashMap::new()),
            task_store: Arc::new(InMemoryTaskStore::new()),
            sessions: Arc::new(SessionStore::new()),
            registry: Arc::new(WorkerRegistry::new()),
            ws_cache: Arc::new(WsCache::new(ws_cache_ttl)),
        }
    }
}
```

### Step 4: Proxy module structure

```
src/proxy/
    mod.rs       — proxy router and handler entry point
    session.rs   — SessionStore
    registry.rs  — WorkerRegistry
    forward.rs   — HTTP and WebSocket forwarding
    cold_start.rs — spawn + health-poll logic
    ws_cache.rs  — WebSocket connection caching
```

`src/lib.rs` addition:

```rust
pub mod proxy;
```

### Step 5: Session cookie middleware

The proxy needs to read and set a `blockyard_session` cookie. This is
not a middleware layer — the proxy handler reads the cookie from the
request headers and sets it on the response directly.

Cookie properties:
- **Name:** `blockyard_session`
- **Value:** UUID v4 (generated on first visit)
- **Path:** `/app/{name}/` (scoped to the specific app)
- **HttpOnly:** yes
- **SameSite:** Lax
- **Secure:** not set in v0 (no TLS); set when behind HTTPS proxy (v1)
- **Max-Age:** not set (session cookie — expires when browser closes)

```rust
use axum::http::HeaderMap;

const SESSION_COOKIE_NAME: &str = "blockyard_session";

/// Extract the session ID from the blockyard_session cookie.
fn extract_session_id(headers: &HeaderMap) -> Option<String> {
    headers
        .get_all(axum::http::header::COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .flat_map(|s| s.split(';'))
        .map(|s| s.trim())
        .find_map(|cookie| {
            let (name, value) = cookie.split_once('=')?;
            if name.trim() == SESSION_COOKIE_NAME {
                Some(value.trim().to_string())
            } else {
                None
            }
        })
}

/// Build the Set-Cookie header value for a new session.
fn session_cookie(session_id: &str, app_name: &str) -> String {
    format!(
        "{SESSION_COOKIE_NAME}={session_id}; Path=/app/{app_name}/; HttpOnly; SameSite=Lax"
    )
}
```

### Step 6: Trailing-slash redirect

`GET /app/{name}` (no trailing slash) → `301 /app/{name}/`

Shiny requires a trailing-slash prefix for relative asset URLs to
resolve correctly. Without this redirect, `<script src="shared/shiny.js">`
resolves to `/app/shared/shiny.js` instead of `/app/my-app/shared/shiny.js`.

```rust
pub async fn trailing_slash_redirect(
    Path(name): Path<String>,
    req: axum::extract::Request,
) -> axum::response::Response {
    let query = req.uri().query().map(|q| format!("?{q}")).unwrap_or_default();
    axum::response::Redirect::permanent(&format!("/app/{name}/{query}")).into_response()
}
```

### Step 7: Proxy handler — entry point

`src/proxy/mod.rs` — the main proxy handler that dispatches HTTP and
WebSocket requests.

The handler uses `Option<WebSocketUpgrade>` to handle both HTTP and
WebSocket requests in a single route. `WebSocketUpgrade` implements
`FromRequestParts` (not `FromRequest`), so it doesn't consume the
request body and can coexist with other extractors. When wrapped in
`Option`, it returns `None` for non-upgrade requests instead of
rejecting them — no route splitting required.

```rust
pub mod session;
pub mod registry;
pub mod forward;
pub mod cold_start;
pub mod ws_cache;

use axum::extract::{Path, State, Request};
use axum::extract::ws::WebSocketUpgrade;
use axum::response::{IntoResponse, Response};
use axum::http::StatusCode;
use crate::app::AppState;
use crate::backend::Backend;

pub async fn proxy_handler<B: Backend>(
    Path(name): Path<String>,
    State(state): State<AppState<B>>,
    ws: Option<WebSocketUpgrade>,
    req: Request,
) -> Response {
    match proxy_request(&name, &state, ws, req).await {
        Ok(resp) => resp,
        Err(status) => status.into_response(),
    }
}

async fn proxy_request<B: Backend>(
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
    let session_id = session::extract_session_id(req.headers());
    let (worker_id, is_new_session) = match session_id.as_ref().and_then(|sid| {
        state.sessions.get(sid).map(|wid| (sid.clone(), wid))
    }) {
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
        session_id.unwrap()
    };

    // 5. Resolve worker address
    let addr = state.registry.get(&worker_id)
        .ok_or(StatusCode::BAD_GATEWAY)?;

    // 6. Dispatch: WebSocket upgrade or HTTP forward
    if let Some(ws) = ws {
        // WebSocket path — axum handles the 101 upgrade handshake.
        // We connect to the backend WS inside on_upgrade, after the
        // client connection is fully upgraded.
        let state = state.clone();
        let app_name = app_name.to_string();
        let session_id_owned = session_id.clone();

        let mut response = ws.on_upgrade(move |client_ws| async move {
            let stripped_path = strip_prefix(req.uri().path(), &app_name);
            if let Err(e) = forward::shuttle_ws(
                client_ws, addr, &stripped_path, &session_id_owned, &state,
            ).await {
                tracing::debug!(
                    error = %e,
                    worker_id = %worker_id,
                    "websocket proxy ended"
                );
            }
        }).into_response();

        // Set session cookie on new sessions
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
        // HTTP path — forward via hyper
        let mut response = forward::forward_http(req, addr, app_name).await
            .map_err(|_| StatusCode::BAD_GATEWAY)?;

        // Set session cookie on new sessions
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

fn strip_prefix<'a>(path: &'a str, app_name: &str) -> String {
    let prefix = format!("/app/{app_name}");
    let stripped = path.strip_prefix(&prefix).unwrap_or(path);
    if stripped.is_empty() { "/".to_string() } else { stripped.to_string() }
}
```

**Why `Option<WebSocketUpgrade>` instead of separate routes:**

Axum's `WebSocketUpgrade` extractor validates the upgrade headers
(Connection, Upgrade, Sec-WebSocket-Key, Sec-WebSocket-Version). When
wrapped in `Option`, a non-upgrade request returns `None` rather than
rejecting the handler — so a single `any()` route handles both HTTP
and WebSocket traffic. This avoids duplicating the session resolution
and cold-start logic across two handlers.

### Step 8: Cold-start — spawn worker and wait for healthy

`src/proxy/cold_start.rs` — spawns a worker if none exists for the
app, then polls `health_check` until the worker is ready or the
timeout expires.

```rust
use std::time::Duration;
use axum::http::StatusCode;
use crate::app::{AppState, ActiveWorker};
use crate::backend::Backend;
use crate::db::sqlite::AppRow;

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
        labels: std::collections::HashMap::new(),
    };

    // Spawn the worker
    let handle = state.backend.spawn(&spec).await
        .map_err(|e| {
            tracing::error!(error = %e, app_id = %app.id, "failed to spawn worker");
            StatusCode::INTERNAL_SERVER_ERROR
        })?;

    // Resolve and cache the worker's address
    let addr = state.backend.addr(&handle).await
        .map_err(|e| {
            tracing::error!(error = %e, "failed to resolve worker address");
            StatusCode::INTERNAL_SERVER_ERROR
        })?;
    state.registry.insert(worker_id.clone(), addr);

    // Track the worker
    state.workers.insert(worker_id.clone(), ActiveWorker {
        app_id: app.id.clone(),
        handle: handle.clone(),
        session_id: None,
    });

    // Poll health_check until ready or timeout
    let timeout = state.config.proxy.worker_start_timeout;
    if !poll_healthy(state, &handle, timeout).await {
        // Worker failed to become healthy — clean up
        tracing::warn!(
            worker_id = %worker_id,
            app_id = %app.id,
            "worker did not become healthy within timeout"
        );
        state.workers.remove(&worker_id);
        state.registry.remove(&worker_id);
        let _ = state.backend.stop(&handle).await;
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
```

**Key decisions:**

- **Spawn is synchronous from the caller's perspective.** The proxy
  holds the initial HTTP request open while the worker starts. The user
  sees the browser's loading spinner. No custom loading page.

- **Exponential backoff on health polling.** Starts at 100ms, doubles
  each attempt, caps at 2s. This avoids hammering the backend while
  still detecting readiness quickly.

- **Worker cleanup on timeout.** If the worker doesn't become healthy,
  it's stopped and removed from all registries. The client gets a 504.

- **Race condition on concurrent first requests.** If two requests
  arrive simultaneously for the same app with no running worker, both
  may try to spawn. The first one to insert into `workers` wins; the
  second one finds the existing worker on the `workers.iter()` check.
  However, there's a brief window where both calls reach `spawn()`.
  This is acceptable in v0 — the extra worker is tracked and cleaned
  up normally. In v1, a per-app mutex or `DashMap::entry` API should
  prevent duplicate spawns.

### Step 9: HTTP forwarding

`src/proxy/forward.rs` — forward HTTP requests to the worker.
WebSocket forwarding (`shuttle_ws`) also lives in this module.

```rust
use std::net::SocketAddr;
use axum::extract::Request;
use axum::response::Response;
use hyper_util::rt::TokioIo;

/// Forward an HTTP request to the worker, stripping the /app/{name}
/// prefix.
pub async fn forward_http(
    req: Request,
    addr: SocketAddr,
    app_name: &str,
) -> Result<Response, ForwardError> {
    let (mut parts, body) = req.into_parts();

    // Strip /app/{name} prefix from the URI
    let original_path = parts.uri.path();
    let prefix = format!("/app/{app_name}");
    let new_path = original_path
        .strip_prefix(&prefix)
        .unwrap_or(original_path);
    let new_path = if new_path.is_empty() { "/" } else { new_path };

    // Reconstruct URI with query string
    let new_uri = if let Some(query) = parts.uri.query() {
        format!("{new_path}?{query}")
    } else {
        new_path.to_string()
    };
    parts.uri = new_uri.parse().map_err(|_| ForwardError::InvalidUri)?;

    // Set forwarding headers
    parts.headers.insert(
        "x-forwarded-for",
        // In v0, no upstream proxy — use peer addr if available, or omit
        "127.0.0.1".parse().unwrap(),
    );
    parts.headers.insert(
        "x-forwarded-proto",
        "http".parse().unwrap(),
    );

    let req = Request::from_parts(parts, body);

    // Connect to the worker and send the request
    let stream = tokio::net::TcpStream::connect(addr).await
        .map_err(ForwardError::Connect)?;

    let (mut sender, conn) = hyper::client::conn::http1::handshake(
        TokioIo::new(stream),
    ).await.map_err(ForwardError::Handshake)?;

    // Spawn the connection driver
    tokio::spawn(async move {
        if let Err(e) = conn.await {
            tracing::debug!(error = %e, "connection driver error");
        }
    });

    let resp = sender.send_request(req).await
        .map_err(ForwardError::Send)?;

    Ok(resp.map(axum::body::Body::new))
}

#[derive(Debug, thiserror::Error)]
pub enum ForwardError {
    #[error("invalid URI")]
    InvalidUri,
    #[error("connect: {0}")]
    Connect(std::io::Error),
    #[error("handshake: {0}")]
    Handshake(hyper::Error),
    #[error("send: {0}")]
    Send(hyper::Error),
    #[error("websocket: {0}")]
    WebSocket(String),
}
```

**Why hyper directly, not reqwest:**

The proxy uses `hyper::client::conn::http1` for HTTP request forwarding
rather than a connection-pooling client like `reqwest`. Reasons:

1. **One connection per request** is fine for v0. Each worker serves
   one session, and Shiny apps make infrequent HTTP requests (initial
   page load, then WebSocket). A connection pool adds complexity for
   no measurable benefit.

2. **Request forwarding needs raw control.** The proxy must forward
   the exact method, headers, and body without reqwest re-encoding
   them. `hyper::client::conn` gives this.

In v1, if load balancing across multiple workers shows contention,
revisit with a pooled client per worker.

### Step 10: WebSocket forwarding

WebSocket proxying uses axum's `WebSocketUpgrade` for the client side
and `tokio-tungstenite` for the backend connection. The upgrade
handshake is handled entirely by axum — no manual `101` response
construction or `Sec-WebSocket-Accept` computation.

The client-side `WebSocket` (axum) and backend-side `WebSocketStream`
(tungstenite) use different message types. Two conversion functions
bridge them.

```rust
use std::net::SocketAddr;
use axum::extract::ws::{WebSocket, Message as AxumMessage};
use tokio_tungstenite::tungstenite::protocol::Message;
use futures_util::{SinkExt, StreamExt};

/// Bidirectional WebSocket frame shuttling between client and backend.
/// Called from the proxy handler's `ws.on_upgrade()` callback after
/// axum has completed the HTTP upgrade handshake with the client.
pub async fn shuttle_ws<B: Backend>(
    client_ws: WebSocket,
    addr: SocketAddr,
    path: &str,
    session_id: &str,
    state: &AppState<B>,
) -> Result<(), ForwardError> {
    // Check WS cache for an existing backend connection (reconnect case)
    let backend_ws = if let Some(cached) = state.ws_cache.take(session_id).await {
        cached
    } else {
        // Connect to the backend WebSocket
        let backend_url = format!("ws://{addr}{path}");
        let (ws, _) = tokio_tungstenite::connect_async(&backend_url).await
            .map_err(|e| ForwardError::WebSocket(format!("backend connect: {e}")))?;
        ws
    };

    let (mut client_tx, mut client_rx) = client_ws.split();
    let (mut backend_tx, mut backend_rx) = backend_ws.split();

    // Client → Backend
    let c2b = async {
        while let Some(Ok(msg)) = client_rx.next().await {
            if backend_tx.send(axum_to_tungstenite(msg)).await.is_err() {
                break;
            }
        }
    };

    // Backend → Client
    let b2c = async {
        while let Some(Ok(msg)) = backend_rx.next().await {
            if client_tx.send(tungstenite_to_axum(msg)).await.is_err() {
                break;
            }
        }
    };

    // Run both directions concurrently — when either ends, the other
    // is cancelled.
    tokio::select! {
        _ = c2b => {},
        _ = b2c => {},
    }

    // Client disconnected — cache the backend WS for potential reconnect.
    // Reunite the split halves before caching.
    let backend_ws = backend_tx.reunite(backend_rx)
        .map_err(|e| ForwardError::WebSocket(format!("reunite: {e}")))?;
    state.ws_cache.cache(session_id, backend_ws);

    Ok(())
}

fn axum_to_tungstenite(msg: AxumMessage) -> Message {
    match msg {
        AxumMessage::Text(t) => Message::Text(t.to_string()),
        AxumMessage::Binary(b) => Message::Binary(b.to_vec()),
        AxumMessage::Ping(p) => Message::Ping(p.to_vec()),
        AxumMessage::Pong(p) => Message::Pong(p.to_vec()),
        AxumMessage::Close(c) => Message::Close(c.map(|cf| {
            tokio_tungstenite::tungstenite::protocol::CloseFrame {
                code: cf.code.into(),
                reason: cf.reason.to_string().into(),
            }
        })),
    }
}

fn tungstenite_to_axum(msg: Message) -> AxumMessage {
    match msg {
        Message::Text(t) => AxumMessage::Text(t.to_string().into()),
        Message::Binary(b) => AxumMessage::Binary(b.into()),
        Message::Ping(p) => AxumMessage::Ping(p.into()),
        Message::Pong(p) => AxumMessage::Pong(p.into()),
        Message::Close(c) => AxumMessage::Close(c.map(|cf| {
            axum::extract::ws::CloseFrame {
                code: cf.code.into(),
                reason: cf.reason.to_string().into(),
            }
        })),
        Message::Frame(_) => AxumMessage::Binary(vec![].into()),
    }
}
```

**How the pieces connect:**

1. Client sends a WS upgrade request to `/app/my-app/websocket/`
2. `proxy_handler` extracts `Some(WebSocketUpgrade)` via `Option<WebSocketUpgrade>`
3. Proxy resolves the session → worker → address (same as HTTP path)
4. `ws.on_upgrade(|client_ws| ...)` tells axum to complete the upgrade
   handshake and call the closure with the upgraded `WebSocket`
5. Inside the closure, `shuttle_ws` connects to the backend and
   shuttles frames bidirectionally
6. When the client disconnects, the backend half is cached for
   `ws_cache_ttl` (step 11)

**Shiny WebSocket path:** Shiny opens its WebSocket connection at
`/websocket/` relative to the app's base URL. After prefix stripping,
the proxy forwards to `ws://{worker_addr}/websocket/`. The path is
passed through unchanged after stripping.

### Step 11: WebSocket session caching

`src/proxy/ws_cache.rs` — hold the backend WebSocket connection open
for `ws_cache_ttl` after the client disconnects, allowing reconnection
to the same session.

When a Shiny user reloads the page or briefly loses network
connectivity, the browser drops the WebSocket connection. Without
caching, the R session and all its state are lost. The WS cache holds
the backend connection open for a grace period, during which a
reconnecting client can be reconnected to the same backend WS.

```rust
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;
use dashmap::DashMap;
use tokio::sync::Mutex;
use tokio_tungstenite::WebSocketStream;
use tokio_tungstenite::MaybeTlsStream;

type BackendWs = WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;

/// A cached backend WebSocket connection.
struct CachedConnection {
    ws: Mutex<Option<BackendWs>>,
    expires_at: tokio::time::Instant,
}

pub struct WsCache {
    entries: DashMap<String, Arc<CachedConnection>>,
    ttl: Duration,
}

impl WsCache {
    pub fn new(ttl: Duration) -> Self {
        Self {
            entries: DashMap::new(),
            ttl,
        }
    }

    /// Store a backend WS connection when the client disconnects.
    /// The connection is held for `ttl` and then dropped.
    pub fn cache(&self, session_id: &str, ws: BackendWs) {
        let entry = Arc::new(CachedConnection {
            ws: Mutex::new(Some(ws)),
            expires_at: tokio::time::Instant::now() + self.ttl,
        });

        let entry_clone = entry.clone();
        let session_id = session_id.to_string();
        let entries = self.entries.clone();

        self.entries.insert(session_id.clone(), entry);

        // Spawn a cleanup task
        tokio::spawn(async move {
            tokio::time::sleep_until(entry_clone.expires_at).await;
            // Only remove if this is still the same entry (not replaced
            // by a reconnect-then-disconnect cycle)
            entries.remove_if(&session_id, |_, v| Arc::ptr_eq(v, &entry_clone));
        });
    }

    /// Attempt to reclaim a cached backend WS for this session.
    /// Returns None if no cached connection exists or it has expired.
    pub async fn take(&self, session_id: &str) -> Option<BackendWs> {
        let entry = self.entries.remove(session_id)?;
        let (_, entry) = entry;
        if tokio::time::Instant::now() >= entry.expires_at {
            return None;
        }
        entry.ws.lock().await.take()
    }
}
```

**Integration with the WS proxy handler:**

When the client connects, the proxy first checks the WS cache for an
existing backend connection:

1. Client reconnects with the same session cookie
2. `ws_cache.take(session_id)` → cached backend WS
3. If found: resume shuttling with the cached backend WS
4. If not found: establish a new backend WS connection

When the client disconnects (one side of `shuttle_ws` ends):

1. `ws_cache.cache(session_id, backend_ws)` — cache the backend half
2. The cleanup task drops the backend WS after `ws_cache_ttl`
3. If the client reconnects within the TTL, the backend WS is reclaimed

**What "dropping the backend WS" means in v0:**

When the cached backend WS is dropped (TTL expired, no reconnect), the
WebSocket connection to the Shiny container closes. With
`max_sessions_per_worker = 1`, this means the worker has no more
sessions. The worker should then be stopped and cleaned up. This
cleanup is handled by the health polling loop (phase 0-6) — it
detects workers with no active sessions and stops them.

In v0, the simplest approach: when the WS cache entry expires, also
remove the session from `SessionStore` and stop the worker. This avoids
depending on the health polling loop for session teardown.

The `WsCache` cleanup task needs access to `AppState` to perform this
cleanup. Rather than making `WsCache` generic over `Backend`, the
cleanup is triggered by a callback. `WsCache::cache` accepts an
optional `on_expire` closure that runs when the TTL fires without a
reconnect:

```rust
    /// Store a backend WS connection when the client disconnects.
    /// `on_expire` is called if the TTL fires without a reconnect.
    pub fn cache<F>(&self, session_id: &str, ws: BackendWs, on_expire: F)
    where
        F: FnOnce() + Send + 'static,
    {
        // ... same as before, but the cleanup task calls on_expire
        // after removing the entry:
        tokio::spawn(async move {
            tokio::time::sleep_until(entry_clone.expires_at).await;
            if entries.remove_if(&session_id, |_, v| Arc::ptr_eq(v, &entry_clone)).is_some() {
                on_expire();
            }
        });
    }
```

The proxy handler passes a closure that performs session and worker
cleanup:

```rust
// In shuttle_ws, after tokio::select! completes:
let state = state.clone();
let session_id = session_id.to_string();
state.ws_cache.cache(&session_id, backend_ws, move || {
    // Runs if client doesn't reconnect within ws_cache_ttl
    tokio::spawn(async move {
        if let Some(worker_id) = state.sessions.remove(&session_id) {
            if state.sessions.count_for_worker(&worker_id) == 0 {
                if let Some((_, worker)) = state.workers.remove(&worker_id) {
                    state.registry.remove(&worker_id);
                    if let Err(e) = state.backend.stop(&worker.handle).await {
                        tracing::warn!(
                            worker_id = %worker_id,
                            error = %e,
                            "failed to stop worker on session expire"
                        );
                    }
                }
            }
        }
    });
});
```

This keeps `WsCache` decoupled from `AppState` while ensuring cleanup
happens reliably.

### Step 12: Router composition

Wire the proxy routes alongside the API routes in `main.rs` (or the
router construction):

```rust
use axum::Router;

pub fn app_router<B: Backend + Clone>(state: AppState<B>) -> Router {
    let api = crate::api::api_router(state.clone());

    Router::new()
        // API routes (control plane)
        .merge(api)
        // Proxy routes (data plane) — no auth (user-facing)
        .route("/app/{name}", axum::routing::get(proxy::trailing_slash_redirect))
        .route("/app/{name}/{*rest}", axum::routing::any(proxy::proxy_handler::<B>))
        .with_state(state)
}
```

**Route ordering:** axum matches routes in registration order. The
`/api/v1/*` routes are registered first, so they take priority over
the `/app/*` catch-all. The `/healthz` route is also registered
before the proxy routes.

**No auth on proxy routes.** The proxy serves user-facing Shiny apps.
In v0, there is no user authentication — apps are publicly accessible.
v1 adds OIDC authentication as a middleware layer on the proxy routes.

### Step 13: Path prefix stripping — implementation detail

The proxy must strip the `/app/{name}` prefix before forwarding to the
worker. Shiny expects to be the root of its URL space — it serves
assets at `/shared/shiny.js`, not `/app/my-app/shared/shiny.js`.

The stripping happens in `forward_http` (step 9). For WebSocket
requests, the path after stripping is typically `/websocket/` — Shiny's
SockJS endpoint.

**Edge cases:**

- `/app/my-app/` → forward as `/`
- `/app/my-app/websocket/` → forward as `/websocket/`
- `/app/my-app/shared/shiny.js` → forward as `/shared/shiny.js`
- `/app/my-app` → redirected to `/app/my-app/` (step 6), never reaches
  the proxy handler

### Step 14: Integration tests

All tests use the existing test infrastructure: `spawn_test_server()`
with `MockBackend`. The mock backend's `TcpListener` workers can accept
HTTP and WebSocket connections for realistic end-to-end testing.

**Extending MockBackend for proxy tests:**

The mock backend already spawns a TCP listener per worker (from phase
0-1). For proxy tests, upgrade these listeners to serve actual HTTP
responses, so the proxy can forward real traffic end-to-end:

```rust
/// Spawn a mock HTTP server that echoes request details.
/// Returns the TcpListener's local address.
async fn spawn_echo_server() -> (tokio::net::TcpListener, SocketAddr) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap();
    let addr = listener.local_addr().unwrap();

    let listener_clone = listener.try_clone().unwrap();
    tokio::spawn(async move {
        // Accept connections and respond with a simple echo
        // Uses hyper to serve HTTP responses
    });

    (listener, addr)
}
```

Alternatively, the mock backend can be extended to run a tiny axum
server per worker that echoes requests. This is more robust than raw
TCP handling.

**Test cases:**

```rust
#[tokio::test]
async fn proxy_returns_404_for_unknown_app() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    let resp = client.get(format!("http://{addr}/app/nonexistent/"))
        .send().await.unwrap();
    assert_eq!(resp.status(), 404);
}

#[tokio::test]
async fn proxy_redirects_missing_trailing_slash() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap();

    let resp = client.get(format!("http://{addr}/app/my-app"))
        .send().await.unwrap();
    assert_eq!(resp.status(), 301);
    assert!(resp.headers().get("location").unwrap()
        .to_str().unwrap()
        .ends_with("/app/my-app/"));
}

#[tokio::test]
async fn proxy_sets_session_cookie_on_first_request() {
    let (addr, state) = spawn_test_server().await;
    create_app_with_bundle(&state, "my-app").await;

    let client = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap();

    let resp = client.get(format!("http://{addr}/app/my-app/"))
        .send().await.unwrap();

    // Should have set the session cookie
    let cookie = resp.headers().get("set-cookie").unwrap().to_str().unwrap();
    assert!(cookie.contains("blockyard_session="));
    assert!(cookie.contains("Path=/app/my-app/"));
    assert!(cookie.contains("HttpOnly"));
}

#[tokio::test]
async fn proxy_reuses_session_on_subsequent_requests() {
    let (addr, state) = spawn_test_server().await;
    create_app_with_bundle(&state, "my-app").await;

    let client = reqwest::Client::builder()
        .cookie_store(true)
        .build()
        .unwrap();

    // First request — spawns a worker
    client.get(format!("http://{addr}/app/my-app/"))
        .send().await.unwrap();
    assert_eq!(state.workers.len(), 1);

    // Second request — reuses the same worker
    client.get(format!("http://{addr}/app/my-app/"))
        .send().await.unwrap();
    assert_eq!(state.workers.len(), 1); // still 1
}

#[tokio::test]
async fn proxy_returns_503_when_no_active_bundle() {
    let (addr, _state) = spawn_test_server().await;
    let client = reqwest::Client::new();

    // Create app without uploading a bundle
    client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth("test-token")
        .json(&serde_json::json!({ "name": "my-app" }))
        .send().await.unwrap();

    let resp = client.get(format!("http://{addr}/app/my-app/"))
        .send().await.unwrap();
    assert_eq!(resp.status(), 503);
}

#[tokio::test]
async fn proxy_returns_503_at_max_workers() {
    let (addr, state) = spawn_test_server_with_config(|config| {
        config.proxy.max_workers = 1;
    }).await;

    // Create two apps with bundles
    create_app_with_bundle(&state, "app-a").await;
    create_app_with_bundle(&state, "app-b").await;

    let client = reqwest::Client::new();

    // First request — fills the one worker slot
    let resp = client.get(format!("http://{addr}/app/app-a/"))
        .send().await.unwrap();
    assert_eq!(resp.status(), 200);

    // Second request for different app — should 503
    let resp = client.get(format!("http://{addr}/app/app-b/"))
        .send().await.unwrap();
    assert_eq!(resp.status(), 503);
}

#[tokio::test]
async fn proxy_strips_prefix_before_forwarding() {
    let (addr, state) = spawn_test_server().await;
    create_app_with_bundle(&state, "my-app").await;

    let client = reqwest::Client::builder()
        .cookie_store(true)
        .build()
        .unwrap();

    // Request a subpath — worker should receive the path without
    // the /app/my-app prefix
    let resp = client.get(format!("http://{addr}/app/my-app/shared/shiny.js"))
        .send().await.unwrap();

    // The mock echo server returns the received path in the body.
    // Verify it received /shared/shiny.js, not /app/my-app/shared/shiny.js.
    // (Exact assertion depends on mock server implementation.)
}
```

**WebSocket test:**

```rust
#[tokio::test]
async fn proxy_websocket_upgrade() {
    let (addr, state) = spawn_test_server().await;
    create_app_with_bundle(&state, "my-app").await;

    // First, make an HTTP request to establish a session
    let client = reqwest::Client::builder()
        .cookie_store(true)
        .build()
        .unwrap();
    let resp = client.get(format!("http://{addr}/app/my-app/"))
        .send().await.unwrap();
    let session_cookie = resp.headers().get("set-cookie")
        .unwrap().to_str().unwrap().to_string();

    // Extract session ID from cookie
    let session_id = session_cookie
        .split('=').nth(1).unwrap()
        .split(';').next().unwrap();

    // Connect WebSocket with the session cookie
    let ws_url = format!("ws://{addr}/app/my-app/websocket/");
    let mut ws_request = tokio_tungstenite::tungstenite::http::Request::builder()
        .uri(&ws_url)
        .header("Cookie", format!("blockyard_session={session_id}"))
        .body(())
        .unwrap();

    let (ws, _) = tokio_tungstenite::connect_async(ws_request)
        .await
        .unwrap();

    // WebSocket should be connected
    // Send a message and verify echo (depends on mock implementation)
}
```

**Test helper:**

```rust
/// Create an app with a ready bundle, suitable for proxy tests.
async fn create_app_with_bundle<B: Backend>(state: &AppState<B>, name: &str) {
    let app = crate::db::sqlite::create_app(&state.db, name).await.unwrap();
    let bundle = crate::db::sqlite::create_bundle(
        &state.db,
        &app.id,
        "/tmp/test-bundle.tar.gz",
    ).await.unwrap();
    crate::db::sqlite::update_bundle_status(&state.db, &bundle.id, "ready")
        .await.unwrap();
    crate::db::sqlite::set_active_bundle(&state.db, &app.id, &bundle.id)
        .await.unwrap();

    // Create the unpacked directory and library directory so WorkerSpec
    // paths exist
    let paths = crate::bundle::BundlePaths::for_bundle(
        &state.config.storage.bundle_server_path,
        &app.id,
        &bundle.id,
    );
    tokio::fs::create_dir_all(&paths.unpacked).await.ok();
    tokio::fs::create_dir_all(&paths.library).await.ok();
}
```

## New dependency

No new dependencies. All required crates are already in `Cargo.toml`:

- `hyper` (1.x) + `hyper-util` — HTTP client for forwarding
- `axum` with `ws` feature — WebSocket upgrade handling
- `tokio-tungstenite` (dev-dependency) — WebSocket client for tests,
  and backend WS connections in the proxy
- `futures-util` — `StreamExt` and `SinkExt` for WS frame shuttling
- `dashmap` — concurrent maps for `SessionStore` and `WorkerRegistry`

**Note:** `tokio-tungstenite` is currently a dev-dependency. It needs
to be promoted to a regular dependency since the proxy uses it for
backend WebSocket connections at runtime, not just in tests.

```toml
# Move from [dev-dependencies] to [dependencies]:
tokio-tungstenite = "0.26"
```

## Exit criteria

Phase 0-5 is done when:

- `SessionStore` maps session IDs to worker IDs (insert, get, remove,
  remove_by_worker)
- `WorkerRegistry` maps worker IDs to socket addresses (insert, get,
  remove)
- `AppState` holds `SessionStore`, `WorkerRegistry`, and `WsCache`
- `GET /app/{name}` redirects to `/app/{name}/` with 301
- `GET /app/{name}/` with no session cookie spawns a worker, sets a
  `blockyard_session` cookie, and forwards the request to the worker
- Subsequent requests with the same cookie route to the same worker
  without spawning a new one
- The `/app/{name}` prefix is stripped before forwarding — the worker
  receives requests rooted at `/`
- WebSocket upgrade requests at `/app/{name}/websocket/` are forwarded
  to the backend and frames are shuttled bidirectionally
- Cold-start holding: the proxy holds the initial request while the
  worker starts and polls `health_check` with exponential backoff
- If the worker doesn't become healthy within `worker_start_timeout`,
  the proxy returns 504 and cleans up the worker
- WebSocket session caching: on client disconnect, the backend WS is
  held for `ws_cache_ttl`; a reconnecting client reclaims it
- When the WS cache entry expires, the session is cleaned up and the
  worker is stopped (if no other sessions remain)
- Requests for unknown apps return 404
- Requests for apps without an active bundle return 503
- Requests when `max_workers` is reached return 503
- API routes (`/api/v1/*`) and health check (`/healthz`) still work
  unchanged alongside the proxy routes
- `tokio-tungstenite` promoted from dev-dependency to dependency
- All existing tests pass (phases 0-1 through 0-4)
- All new proxy integration tests pass
- `cargo clippy` clean

## Implementation notes

- **No connection pooling.** v0 opens a new TCP connection to the
  worker for every HTTP request. This is fine because Shiny apps
  serve one initial page load (a few HTTP requests for HTML, JS, CSS),
  then switch to WebSocket for all subsequent communication. The WS
  connection is long-lived — it's not reopened per message.

- **No `X-Forwarded-Host`.** In v0, there is no upstream proxy
  providing the original host. When Caddy/nginx sits in front of
  blockyard (typical deployment), it adds forwarding headers before
  blockyard sees the request. blockyard adds `X-Forwarded-For` and
  `X-Forwarded-Proto` but doesn't overwrite them if already present.
  Trust-upstream logic is a v1 concern.

- **Session cookie scope.** The cookie's `Path` is set to
  `/app/{name}/`, scoping it to the specific app. A user visiting
  two different apps gets two independent sessions. This prevents
  session confusion between apps.

- **Proxy handler is `any()`.** It accepts all HTTP methods (GET, POST,
  PUT, etc.) because Shiny apps may use XHR uploads and other methods.
  The proxy doesn't filter methods — it forwards everything.

- **No request body size limit on proxy routes.** The `DefaultBodyLimit`
  from phase 0-3 applies to the API routes (bundle uploads). Proxy
  routes don't use axum's body extraction, so the limit doesn't apply.
  The body is streamed through to the worker as-is.

- **Race condition on spawn.** Two concurrent first-requests for the
  same app may both trigger `backend.spawn()`. In v0 this is
  tolerated — both workers are tracked and serve their respective
  sessions. The extra resource cost is bounded by `max_workers`. In
  v1, a per-app spawn lock prevents this.

- **Mock backend for proxy tests.** The mock backend needs to serve
  actual HTTP responses for proxy integration tests to work
  end-to-end. Two options: (a) extend `MockBackend` to run a tiny
  HTTP echo server per worker, or (b) test against the real Docker
  backend with `docker-tests`. Option (a) is better for CI speed;
  option (b) is better for confidence. Do both — mock tests for the
  smoke suite, Docker tests for the full end-to-end.

- **`forward_http` uses `hyper::client::conn::http1`.** This creates
  one TCP connection per HTTP request — no pooling. Shiny's traffic
  pattern (few HTTP requests, then long-lived WS) makes pooling
  unnecessary. The `conn` future must be spawned to drive the
  connection to completion.

- **Shiny SockJS paths.** Shiny uses SockJS for WebSocket transport.
  The URL pattern is `/websocket/` (or with session and transport IDs
  like `/{session_id}/{transport}/websocket`). The proxy passes all
  paths through after prefix stripping — it doesn't need to understand
  SockJS internals.

- **WS cache cleanup triggers worker stop.** When a WS cache entry
  expires (client didn't reconnect within `ws_cache_ttl`), the proxy
  cleans up the session and stops the worker. This is the normal
  lifecycle termination path in v0 — users close the browser tab,
  the WS cache grace period passes, the worker is stopped. The
  explicit stop endpoint (`POST /apps/{id}/stop`) is for admin use;
  the proxy handles the organic lifecycle.
