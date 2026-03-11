# blockyard v1 Implementation Plan

This document is the build plan for v1 — the MVP milestone. It covers new
modules, dependency additions, build phases, key type definitions, schema
changes, and test strategy. The roadmap (`../roadmap.md`) is the source of
truth for *what* v1 includes; this document describes *how* to build it.

v1 builds on v0's infrastructure (Docker backend, proxy layer, bundle
pipeline, REST API) and adds everything needed to host a real blockr app for
real users: user authentication (OIDC), authorization (RBAC), per-user
credential management (OpenBao), multi-worker scaling, content discovery,
and operational observability.

## New Modules

v1 adds the following modules to the existing crate layout. Existing modules
(`api/`, `proxy/`, `backend/`, `bundle/`, `db/`, `config.rs`, `app.rs`,
`task.rs`, `ops.rs`) are extended in place.

```
src/
├── auth/
│   ├── mod.rs              # shared JWT validation, token types
│   ├── oidc.rs             # OIDC discovery, authorization code flow
│   ├── session.rs          # signed session cookie (encode/decode/refresh)
│   └── client_credentials.rs  # OAuth 2.0 client credentials (machine auth)
├── authz/
│   ├── mod.rs              # permission checks, role definitions
│   └── acl.rs              # per-content access control lists
├── integration/
│   ├── mod.rs              # OpenBao client, JWT auth setup
│   └── enrollment.rs       # credential enrollment API handlers
├── vanity.rs               # vanity URL resolution + collision detection
├── catalog.rs              # content discovery: catalog, tags, search
├── audit.rs                # append-only audit log writer
├── telemetry.rs            # Prometheus metrics + OpenTelemetry tracing
├── api/
│   ├── ... (existing)
│   ├── users.rs            # /users/me, credential enrollment endpoints
│   ├── catalog.rs          # catalog + tag endpoints
│   └── vanity.rs           # vanity URL management endpoints
├── proxy/
│   ├── ... (existing)
│   ├── identity.rs         # X-Shiny-User / X-Shiny-Groups injection
│   ├── load_balancer.rs    # sticky session assignment across workers
│   └── autoscaler.rs       # connection-based auto-scaling
└── db/
    ├── ... (existing)
    └── sqlite.rs           # extended: roles, ACLs, tags, vanity URLs, audit
```

## New Dependencies

```toml
[dependencies]
# --- existing (unchanged) ---
# tokio, axum, hyper, hyper-util, http-body-util, tower, bollard, sqlx,
# serde, serde_json, toml, uuid, tracing, tracing-subscriber, thiserror,
# tokio-util, bytes, dashmap, tempfile

# --- v1 additions ---
# OIDC / JWT
openidconnect   = "4"           # OIDC discovery, authorization code flow
jsonwebtoken    = "9"           # JWT decode + JWKS validation (phase 1-2)

# Session cookie signing
hmac            = "0.12"
sha2            = "0.10"
base64          = "0.22"

# OpenBao / Vault client
reqwest         = { version = "0.12", features = ["json", "rustls-tls"] }
# (reqwest already in dev-dependencies; promoted to regular dep for OpenBao client)

# Telemetry
metrics             = "0.24"
metrics-exporter-prometheus = "0.16"
tracing-opentelemetry       = "0.28"
opentelemetry               = "0.27"
opentelemetry-otlp          = "0.27"
opentelemetry_sdk           = "0.27"

# Audit logging
tokio = { version = "1", features = ["full", "fs"] }  # fs for async file writes
```

**Dependency rationale:**

- **openidconnect** — full OIDC Relying Party implementation. Handles
  discovery, JWKS fetching, authorization URL generation, and token exchange.
  Built on `oauth2` crate.
- **jsonwebtoken** — lightweight JWT validation for the control plane (client
  credentials flow, phase 1-2). Shares the JWKS fetched by `openidconnect`.
- **hmac + sha2 + base64** — HMAC-SHA256 cookie signing and key derivation.
- **reqwest** — HTTP client for OpenBao API calls. Already used in
  dev-dependencies; promoted to regular dependency.
- **metrics + prometheus exporter** — Prometheus-compatible metrics. The
  `metrics` facade allows instrumentation without coupling to the exporter.
- **tracing-opentelemetry + opentelemetry** — connects the existing `tracing`
  instrumentation to an OTel collector. Zero-cost when no collector is
  configured.

## Build Phases

### Phase 1-1: OIDC Authentication + User Sessions ([detailed plan](phase-1-1.md))

Establish user identity. This is the foundation — RBAC, identity injection,
and OpenBao integration all require a logged-in user.

**Deliverables:**

1. OIDC discovery client — fetch provider metadata from
   `{issuer_url}/.well-known/openid-configuration`
2. Authorization code flow endpoints: `GET /login`, `GET /callback`,
   `POST /logout`
3. Minimal signed session cookie — HMAC-SHA256 signed, carries only `sub` +
   `issued_at` (~100-150 bytes)
4. Server-side session store — `DashMap<String, UserSession>` keyed by `sub`,
   holds groups, access token, refresh token
5. Transparent access token refresh — on each request, if access token is
   near expiry, exchange refresh token and update server-side session
6. Auth middleware for the app plane — protect `/app/` routes; redirect
   unauthenticated users to `/login`
7. Config additions: `[oidc]` section
8. Cookie signing key derivation from a configured secret

**Config additions:**

```rust
#[derive(Debug, Deserialize)]
pub struct OidcConfig {
    pub issuer_url: String,          // OIDC issuer URL
    pub client_id: String,           // registered client ID
    pub client_secret: String,       // client secret (use env var)
    #[serde(default = "default_groups_claim")]
    pub groups_claim: String,        // default: "groups"
    #[serde(default = "default_cookie_max_age")]
    pub cookie_max_age: Duration,    // cookie max-age; default: 24h
}
```

**Note on cookie lifetime vs. session lifetime:** `cookie_max_age` controls how
long the browser retains the session cookie. The *effective* session duration is
`min(cookie_max_age, refresh_token_lifetime)` — if the IdP's refresh token
expires before the cookie, the session ends at refresh token expiry regardless
of the cookie's max-age. Conversely, a long-lived refresh token is bounded by
the cookie max-age. Operators should align both values.

**Session architecture: minimal cookie + server-side store.**

The cookie carries only `sub` and `issued_at`, signed with HMAC-SHA256. All
sensitive/bulky data (groups, access token, refresh token) lives server-side
in a `DashMap<String, UserSession>`. This avoids cookie size issues (IdP
JWTs can be 1-2KB, easily exceeding the 4KB browser limit), eliminates the
need for refresh token encryption, and enables immediate session invalidation
on logout.

```rust
/// Minimal payload in the session cookie.
#[derive(Serialize, Deserialize)]
pub struct CookiePayload {
    pub sub: String,    // IdP subject identifier
    pub issued_at: i64, // cookie issue time
}

/// Server-side session data, keyed by sub.
pub struct UserSession {
    pub groups: Vec<String>,     // group memberships from groups_claim
    pub access_token: String,    // short-lived IdP access token
    pub refresh_token: String,   // long-lived refresh token (plaintext, never leaves server)
    pub expires_at: i64,         // access token expiry (unix timestamp)
}
```

Trade-off: sessions are lost on server restart — users must re-authenticate.
This is the same failure mode as workers, proxy sessions, and task store
(all in-memory in v1). v2's PostgreSQL migration would naturally extend to
session state.

**Authorization code flow:**

```
GET /login
  → 302 to IdP authorize endpoint with:
    response_type=code, client_id, redirect_uri=/callback,
    scope=openid+groups, state=random+return_url

GET /callback?code=...&state=...
  → Exchange code for tokens at IdP token endpoint
  → Validate ID token signature against JWKS
  → Extract sub + groups from ID token claims
  → Store UserSession server-side, set signed cookie (sub + issued_at)
  → 302 to return_url from state (default: /)

POST /logout
  → Remove server-side session, clear cookie
  → 302 to / (or IdP end_session_endpoint if available)
```

**Auth middleware integration:**

The v0 proxy serves apps without authentication. v1 adds a middleware layer
that verifies the signed session cookie, looks up the server-side session,
and inserts `AuthenticatedUser` into request extensions. The control plane
API continues to use bearer token auth (upgraded to JWT in phase 1-2).

```rust
/// App-plane auth middleware. Inserted into the proxy router.
/// Redirects unauthenticated users to /login?return_url={current_path}.
async fn app_auth_middleware(
    State(state): State<AppState<B>>,
    mut req: Request,
    next: Next,
) -> Result<Response, Response> {
    let cookie = verify_session_cookie(&state, &req)?;
    let session = state.user_sessions.get(&cookie.sub)?;
    refresh_if_needed(&state, &cookie.sub).await?;
    req.extensions_mut().insert(AuthenticatedUser { ... });
    Ok(next.run(req).await)
}
```

### Phase 1-2: IdP Client Credentials + RBAC + Per-Content ACL

Replace the static bearer token with JWT-based machine auth. Add role-based
access control and per-content permissions.

**Deliverables:**

1. JWT validation middleware for the control plane — validate bearer tokens
   as JWTs against the IdP's JWKS (same keys as OIDC)
2. OAuth 2.0 client credentials support — clients authenticate with
   `client_id` + `client_secret` at the IdP's token endpoint
3. Role system — three roles: `admin`, `publisher`, `viewer`
4. Role assignment — mapped from IdP groups claim
5. Per-content ACL — owners and explicit viewer/collaborator grants per app
6. Authorization checks on all API and proxy endpoints
7. Schema additions: roles and ACL tables

**Roles and permissions:**

| Permission | admin | publisher | viewer |
|---|---|---|---|
| Create apps | yes | yes | no |
| Deploy bundles | yes | own apps | no |
| Start/stop apps | yes | own apps | no |
| Update app config | yes | own apps | no |
| Delete apps | yes | own apps | no |
| View all apps | yes | no | no |
| View accessible apps | yes | yes | yes |
| Access app (proxy) | yes | own + granted | granted only |
| Manage users/roles | yes | no | no |

"Own apps" = apps where the user is the `owner` (set at creation time to the
authenticated user's `sub`).

**Per-content ACL:**

```sql
CREATE TABLE app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,    -- user sub or group name
    kind        TEXT NOT NULL,    -- 'user' | 'group'
    role        TEXT NOT NULL,    -- 'viewer' | 'collaborator'
    granted_by  TEXT NOT NULL,    -- sub of the granting user
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);
```

Access evaluation order: admin overrides all → owner has full access →
explicit ACL grants → deny.

**ACL conflict resolution:** a principal may have multiple grants for the same
app (e.g., a direct `viewer` grant and a `collaborator` grant via group
membership). The effective role is the highest-privilege grant across all
matching entries. `Role` implements `Ord` in Rust (`collaborator > viewer`),
and the access check collects all grants for the user's `sub` (kind=user)
plus all their group names (kind=group), then takes the max.

**ACL enforcement on active sessions:** ACL checks run on HTTP requests only,
not on individual WebSocket frames. When a user's access is revoked, it takes
effect on their next HTTP request or WebSocket reconnect — active WS
connections continue until the next reconnect. This avoids per-frame database
or cache lookups on the hot path. If per-request ACL checks become a
performance concern, an in-memory ACL cache with short TTL (30–60s) can be
added as an optimization.

**JWT validation for control plane:**

```rust
/// Replaces the v0 static bearer token check.
/// Validates JWT signature against JWKS, checks expiry and issuer.
/// Falls back to static token if [oidc] is not configured (dev mode).
async fn api_auth_middleware<B: Backend>(
    State(state): State<AppState<B>>,
    req: Request,
    next: Next,
) -> Result<Response, StatusCode> {
    let token = extract_bearer_token(&req)?;

    if let Some(oidc) = &state.config.oidc {
        let claims = validate_jwt(token, &state.jwks_cache, oidc)?;
        // Insert claims into request extensions for downstream handlers
        // Role is derived from groups claim
    } else {
        // Fallback: static token comparison (v0 compat / dev mode)
        if token != state.config.server.token {
            return Err(StatusCode::UNAUTHORIZED);
        }
    }

    Ok(next.run(req).await)
}
```

**Schema additions:**

```sql
-- Add owner to apps table (NOT NULL — no ownerless apps)
-- Uses table-rebuild pattern since SQLite doesn't support ADD COLUMN NOT NULL
-- without a default. Pre-release migration consolidation (see below) means
-- no existing rows need migration.
ALTER TABLE apps ADD COLUMN owner TEXT NOT NULL;

-- Per-content access grants
CREATE TABLE app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('user', 'group')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

-- Role-to-group mapping (admin-managed)
CREATE TABLE role_mappings (
    group_name  TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'publisher', 'viewer')),
    PRIMARY KEY (group_name)
);
```

### Phase 1-3: Identity Injection + OpenBao Integration

Deliver authenticated user identity and per-user credentials to Shiny apps
at runtime.

**Deliverables:**

1. Identity header injection — `X-Shiny-User` and `X-Shiny-Groups` headers
   on every proxied request
2. OpenBao client — HTTP client for OpenBao's KV v2 and auth APIs
3. OpenBao JWT auth setup — on server startup, configure OpenBao's JWT auth
   method with the IdP's JWKS URI
4. Per-request credential injection — exchange user's JWT for a scoped
   OpenBao token, inject as `X-Blockyard-Vault-Token` and
   `X-Blockyard-Vault-Addr` headers on every proxied request
5. Token cache — in-memory cache keyed by user `sub` to avoid per-request
   OpenBao calls
6. Credential enrollment API — `POST /api/v1/users/me/credentials/{service}`
7. Config additions: `[openbao]` section
8. OpenBao health check for `/readyz` (wired in phase 1-6)

**Identity injection (proxy middleware):**

```rust
/// Injected after auth middleware, before forwarding to the worker.
/// Reads SessionPayload from request extensions (set by app_auth_middleware).
fn inject_identity_headers(req: &mut Request, session: &SessionPayload) {
    let headers = req.headers_mut();
    headers.insert("X-Shiny-User", session.sub.parse().unwrap());
    headers.insert(
        "X-Shiny-Groups",
        session.groups.join(",").parse().unwrap(),
    );
    // Remove any client-supplied values to prevent spoofing
}
```

Headers are injected by the proxy, never by the client. The proxy strips
any client-supplied `X-Shiny-User` or `X-Shiny-Groups` headers before
forwarding.

**Per-request credential injection via HTTP headers:**

OpenBao credentials are injected per-request as HTTP headers, not per-container
as env vars. This is the same model Posit Connect uses for OAuth Integrations
— per-user credentials are part of the Shiny session context, not the process
environment. This design supports `max_sessions_per_worker > 1` safely: each
user's request carries their own scoped token, even on shared workers.

```
Per-request flow (on every proxied HTTP request):
1. Auth middleware extracts SessionPayload from session cookie
2. Look up cached OpenBao token for this user's `sub`
   - Cache hit (token not expired): use cached token
   - Cache miss: POST {openbao_addr}/v1/auth/jwt/login
     Body: { "role": "blockyard-user", "jwt": "{access_token}" }
     OpenBao validates JWT against IdP JWKS, returns scoped token
     Cache the token (keyed by sub, TTL = token_ttl)
3. Inject headers:
   - X-Blockyard-Vault-Token: {scoped_token}
   - X-Blockyard-Vault-Addr: {openbao_address}
4. R app reads via session$request$HTTP_X_BLOCKYARD_VAULT_TOKEN
```

**Token cache:**

```rust
/// In-memory cache of OpenBao tokens keyed by user sub.
/// Avoids calling OpenBao's JWT login endpoint on every request.
pub struct VaultTokenCache {
    tokens: DashMap<String, CachedToken>,
}

struct CachedToken {
    token: String,
    expires_at: Instant,
}
```

The cache TTL matches `[openbao] token_ttl` (default 1h). On cache miss, the
proxy exchanges the user's access token for a scoped OpenBao token and caches
the result. No `WorkerSpec` changes are needed — credentials never enter the
container environment.

**Note:** `WorkerSpec` does not carry `vault_token` or `vault_addr`. These are
injected per-request via headers, not per-container via env vars.

> **Open research item:** Investigate how Posit Connect and ShinyProxy handle
> per-user credentials on long-lived WebSocket connections. Headers are only
> present on the initial WS upgrade request. For Shiny, the R code reads
> `session$request` at session init time, which captures the upgrade headers.
> This is likely sufficient (the token is available when the session starts),
> but needs verification against real Shiny app patterns. If mid-session token
> refresh is needed, the R code can call OpenBao's renewal API directly using
> the token from `session$request`.

**Config additions:**

```rust
#[derive(Debug, Deserialize)]
pub struct OpenbaoConfig {
    pub address: String,             // e.g. https://bao.example.com
    pub admin_token: String,         // use BLOCKYARD_OPENBAO_ADMIN_TOKEN env var
    #[serde(default = "default_token_ttl")]
    pub token_ttl: Duration,         // default: 1h
    #[serde(default = "default_jwt_auth_path")]
    pub jwt_auth_path: String,       // default: "jwt"
}
```

**Credential enrollment API:**

```
POST /api/v1/users/me/credentials/{service}
Authorization: Bearer <jwt>
Content-Type: application/json
Body: { "api_key": "sk-..." }

→ 204 No Content
```

Server-side:
1. Validate user identity from JWT
2. Write to OpenBao: `PUT /v1/secret/data/users/{sub}/apikeys/{service}`
   using the server's admin token
3. The user's session-scoped token can then read this secret

**OpenBao bootstrap (server startup):**

When `[openbao]` is configured, the server verifies on startup that:
1. OpenBao is reachable and unsealed
2. The JWT auth method is enabled at the configured path
3. The `blockyard-user` role exists with the correct policy
4. The KV v2 secrets engine is mounted at `secret/`

If any check fails, the server logs a warning but starts anyway — OpenBao
may be configured later. The `/readyz` endpoint (phase 1-6) will report
OpenBao as unhealthy.

### Phase 1-4: Session Sharing + Load Balancing + Auto-scaling

Unlock multi-worker operation. This phase changes the proxy's core routing
model from "one session = one worker" to "sessions distributed across a
worker pool."

**Deliverables:**

1. Unlock `max_sessions_per_worker > 1` — remove the v0 validation rejection
2. Session sharing — multiple sessions routed to the same worker
3. Cookie-hash sticky sessions — deterministic worker assignment
4. Load balancer — distribute new sessions across workers with available
   capacity
5. Connection-based auto-scaling — spawn/stop workers based on active
   session count
6. Graceful drain on app stop — wait for sessions to end before killing
   workers
7. Proxy concurrency model — per-worker `hyper::Client` if contention
   appears

**Load balancing strategy:**

```rust
/// Assigns a session to a worker. Called when a new session arrives
/// and the app has max_workers_per_app > 1.
pub struct LoadBalancer {
    // No persistent state — decisions are based on current worker map
}

impl LoadBalancer {
    /// Pick a worker for a new session.
    /// 1. Find workers with available capacity (sessions < max_sessions_per_worker)
    /// 2. Among those, pick the one with fewest sessions (least-loaded)
    /// 3. If none have capacity and max_workers_per_app not reached, return None
    ///    (caller spawns a new worker)
    /// 4. If none have capacity and at max_workers_per_app, return error (503)
    pub fn assign(
        &self,
        app_id: &str,
        workers: &DashMap<String, ActiveWorker<B::Handle>>,
        sessions: &SessionStore,
        max_sessions: u32,
        max_workers: Option<u32>,
    ) -> Result<Option<WorkerId>, LoadBalancerError>;
}
```

Sticky sessions: once assigned, a session stays pinned to its worker via
`SessionStore` (unchanged from v0). The load balancer only runs on new
session creation.

**Auto-scaling:**

```rust
/// Runs as a background loop alongside health polling.
/// Checks each app's worker count against demand.
pub async fn autoscale_loop<B: Backend>(state: AppState<B>) {
    let mut interval = tokio::time::interval(state.config.proxy.health_interval);
    loop {
        interval.tick().await;
        for app in list_running_apps(&state).await {
            let worker_count = count_workers_for_app(&state, &app.id);
            let session_count = count_sessions_for_app(&state, &app.id);
            let max_sessions = app.max_sessions_per_worker;

            // Scale up: if all workers are at capacity and below max_workers
            if all_at_capacity(worker_count, session_count, max_sessions) {
                if can_scale_up(&app, worker_count, &state) {
                    spawn_worker_for_app(&state, &app).await;
                }
            }

            // Scale down: if a worker has 0 sessions and others have capacity
            if has_idle_workers(worker_count, session_count, max_sessions) {
                drain_idle_worker(&state, &app).await;
            }
        }
    }
}
```

Scale-up is eager (spawn when all workers are full); scale-down is
conservative (only remove workers with zero sessions). Scale-to-zero is
deferred to v2.

**Graceful drain on stop:**

```rust
/// v0 kills workers immediately. v1 drains sessions first.
async fn stop_app_graceful<B: Backend>(state: &AppState<B>, app_id: &str) {
    let workers = get_workers_for_app(state, app_id);

    // 1. Stop routing new sessions to this app
    mark_app_draining(state, app_id).await;

    // 2. Wait for existing sessions to end (up to shutdown_timeout)
    let deadline = Instant::now() + state.config.server.shutdown_timeout;
    loop {
        let remaining = count_sessions_for_workers(state, &workers);
        if remaining == 0 || Instant::now() >= deadline {
            break;
        }
        tokio::time::sleep(Duration::from_secs(1)).await;
    }

    // 3. Force-stop remaining workers
    for worker in workers {
        evict_worker(state, &worker.id).await;
    }
}
```

### Phase 1-5: Vanity URLs + Content Discovery

User-facing features for navigating and accessing deployed content.

**Deliverables:**

1. Vanity URL assignment — `PATCH /api/v1/apps/{id}` with `vanity_url` field
2. Vanity URL routing — resolve `/{vanity}/` to the target app before
   name-based routing
3. Collision detection — reject vanity URLs that collide with reserved
   prefixes or existing vanity URLs
4. Catalog API — `GET /api/v1/catalog` listing accessible apps with metadata
5. Tag system — admin-managed tags attached to apps
6. Search/filter — query params on the catalog endpoint

**Vanity URL routing:**

```rust
/// Extended router. Vanity routes are checked before the /app/{name}/ routes.
pub fn full_router<B: Backend + Clone>(state: AppState<B>) -> Router {
    let api = api_router(state.clone());

    Router::new()
        .merge(api)
        // Auth endpoints
        .route("/login", get(login_handler))
        .route("/callback", get(callback_handler))
        .route("/logout", post(logout_handler))
        // Vanity URL catch-all — checked before /app/{name}/
        // The handler resolves the vanity path to an app and proxies.
        // Returns 404 if no vanity URL matches (does not fall through
        // to avoid shadowing legitimate 404s).
        .route("/{vanity}", get(trailing_slash_redirect_vanity))
        .route("/{vanity}/", any(vanity_proxy_handler_root::<B>))
        .route("/{vanity}/{*rest}", any(vanity_proxy_handler::<B>))
        // Standard app routes
        .route("/app/{name}", get(trailing_slash_redirect))
        .route("/app/{name}/", any(proxy_handler_root::<B>))
        .route("/app/{name}/{*rest}", any(proxy_handler::<B>))
        .with_state(state)
}
```

**Reserved prefix blocklist:**

```rust
const RESERVED_PREFIXES: &[&str] = &[
    "api", "app", "login", "callback", "logout", "healthz", "readyz",
    "metrics", "static", "assets", "admin",
];
```

Vanity URLs are validated against this list and against existing vanity URLs
on assignment. The vanity URL is stored in the `apps` table.

**Schema additions:**

```sql
-- Add vanity URL to apps table
ALTER TABLE apps ADD COLUMN vanity_url TEXT UNIQUE;

-- Tags
CREATE TABLE tags (
    id      TEXT PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE app_tags (
    app_id  TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    tag_id  TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, tag_id)
);
```

**Catalog API:**

```
GET /api/v1/catalog?tag=finance&search=dashboard&page=1&per_page=20
Authorization: Bearer <token>

→ 200 OK
{
  "items": [
    {
      "id": "a3f2c1...",
      "name": "sales-dashboard",
      "title": "Sales Dashboard",
      "owner": "user-sub",
      "vanity_url": "/sales-dashboard",
      "tags": ["finance", "reporting"],
      "status": "running",
      "url": "/app/sales-dashboard/",
      "updated_at": "2026-03-10T12:00:00Z"
    }
  ],
  "total": 42,
  "page": 1,
  "per_page": 20
}
```

The catalog respects RBAC — users only see apps they have access to. Admins
see everything.

### Phase 1-6: Audit Logging + Telemetry + /readyz

Operational completeness. Audit trail for compliance, metrics for monitoring,
readiness checks for orchestration.

**Deliverables:**

1. Audit log writer — append-only JSON Lines file
2. Audit middleware — capture all state-changing operations
3. Prometheus metrics endpoint — `GET /metrics`
4. OpenTelemetry tracing — export spans to an OTel collector
5. `/readyz` endpoint — dependency health checks
6. Key metrics: active workers, active sessions, request rates, worker
   lifecycle events, health check results

**Audit log format:**

```json
{
  "ts": "2026-03-10T14:32:01.123Z",
  "action": "app.create",
  "actor": "user-sub-123",
  "target": "app-id-456",
  "detail": {"name": "sales-dashboard"},
  "source_ip": "10.0.1.42"
}
```

Actions: `app.create`, `app.update`, `app.delete`, `app.start`, `app.stop`,
`bundle.upload`, `bundle.restore.success`, `bundle.restore.fail`,
`access.grant`, `access.revoke`, `credential.enroll`, `user.login`,
`user.logout`.

**Audit log writer:**

```rust
/// Append-only audit log. Writes are buffered and flushed periodically
/// or on shutdown. Thread-safe via an mpsc channel.
pub struct AuditLog {
    sender: mpsc::UnboundedSender<AuditEntry>,
}

impl AuditLog {
    pub fn log(&self, entry: AuditEntry) {
        let _ = self.sender.send(entry);
    }
}

/// Background task: receives entries and appends to the log file.
async fn audit_writer(
    mut receiver: mpsc::UnboundedReceiver<AuditEntry>,
    path: PathBuf,
) {
    let mut file = OpenOptions::new()
        .create(true).append(true).open(&path).await.unwrap();

    while let Some(entry) = receiver.recv().await {
        let line = serde_json::to_string(&entry).unwrap();
        file.write_all(line.as_bytes()).await.ok();
        file.write_all(b"\n").await.ok();
    }
    file.flush().await.ok();
}
```

**Prometheus metrics:**

```rust
/// Key metrics registered at startup.
fn register_metrics() {
    // Gauges
    describe_gauge!("blockyard_workers_active", "Currently running workers");
    describe_gauge!("blockyard_sessions_active", "Active proxy sessions");

    // Counters
    describe_counter!("blockyard_workers_spawned_total", "Total workers spawned");
    describe_counter!("blockyard_workers_stopped_total", "Total workers stopped");
    describe_counter!("blockyard_bundles_uploaded_total", "Total bundles uploaded");
    describe_counter!("blockyard_restores_total", "Total dependency restores");
    describe_counter!("blockyard_proxy_requests_total", "Total proxied requests");
    describe_counter!("blockyard_health_checks_failed_total", "Failed health checks");

    // Histograms
    describe_histogram!("blockyard_cold_start_seconds", "Worker cold-start duration");
    describe_histogram!("blockyard_proxy_request_seconds", "Proxy request duration");
}
```

The `/metrics` endpoint is unauthenticated (same as `/healthz`). Operators
can restrict access at the network level if needed.

**`/readyz` endpoint:**

```rust
async fn readyz<B: Backend>(State(state): State<AppState<B>>) -> Response {
    let mut checks = Vec::new();

    // Database
    checks.push(("database", check_db(&state.db).await));

    // Docker socket
    checks.push(("docker", state.backend.health_check_self().await));

    // IdP (OIDC discovery endpoint)
    if let Some(oidc) = &state.config.oidc {
        checks.push(("idp", check_idp(oidc).await));
    }

    // OpenBao
    if let Some(bao) = &state.config.openbao {
        checks.push(("openbao", check_openbao(bao).await));
    }

    let all_ok = checks.iter().all(|(_, ok)| *ok);
    let status = if all_ok { StatusCode::OK } else { StatusCode::SERVICE_UNAVAILABLE };

    let body = serde_json::json!({
        "status": if all_ok { "ready" } else { "not_ready" },
        "checks": checks.into_iter()
            .map(|(name, ok)| (name, if ok { "pass" } else { "fail" }))
            .collect::<HashMap<_, _>>()
    });

    (status, Json(body)).into_response()
}
```

**Config additions:**

```rust
#[derive(Debug, Deserialize)]
pub struct TelemetryConfig {
    #[serde(default)]
    pub metrics_enabled: bool,           // default: false
    #[serde(default)]
    pub otlp_endpoint: Option<String>,   // e.g. http://otel-collector:4317
}

#[derive(Debug, Deserialize)]
pub struct AuditConfig {
    pub path: PathBuf,                   // e.g. /data/audit/blockyard.jsonl
}
```

## Config Summary

Full v1 config structure (v0 fields unchanged):

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "..."               # static token (v0 compat / dev mode)
shutdown_timeout = "30s"
session_secret   = "..."               # HMAC key for cookie signing
                                       # use BLOCKYARD_SERVER_SESSION_SECRET env var
external_url     = "https://blockyard.example.com"  # public URL (for redirects, cookie Secure flag)

[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/rocker-org/r-ver:latest"
shiny_port = 3838

[storage]
bundle_server_path = "/data/bundles"
bundle_worker_path = "/app"
bundle_retention   = 50

[database]
path = "/data/db/blockyard.db"

[proxy]
ws_cache_ttl            = "60s"
health_interval         = "15s"
worker_start_timeout    = "60s"
max_workers             = 100

# --- v1 additions ---

[oidc]
issuer_url    = "https://auth.example.com/realms/myrealm"
client_id     = "blockyard"
client_secret = "..."                  # use BLOCKYARD_OIDC_CLIENT_SECRET env var
groups_claim  = "groups"
cookie_max_age = "24h"

[openbao]
address     = "https://bao.example.com"
admin_token = "..."                    # use BLOCKYARD_OPENBAO_ADMIN_TOKEN env var
token_ttl   = "1h"

[telemetry]
metrics_enabled = true
otlp_endpoint   = "http://otel-collector:4317"

[audit]
path = "/data/audit/blockyard.jsonl"
```

All v1 config sections (`[oidc]`, `[openbao]`, `[telemetry]`, `[audit]`)
are optional. When omitted, the server runs in v0-compatible mode: static
bearer token, no user auth on the app plane, no credential management, no
metrics export, no audit log.

**Environment variable mappings for v1 config fields:**

The v0 env var overlay pattern (`BLOCKYARD_SECTION_FIELD`) extends to all
new sections. Each field must be added to `supported_env_vars()` and
`apply_env_overrides()` — the existing `env_var_coverage_complete` test
enforces this.

```
BLOCKYARD_SERVER_SESSION_SECRET
BLOCKYARD_SERVER_EXTERNAL_URL
BLOCKYARD_OIDC_ISSUER_URL
BLOCKYARD_OIDC_CLIENT_ID
BLOCKYARD_OIDC_CLIENT_SECRET
BLOCKYARD_OIDC_GROUPS_CLAIM
BLOCKYARD_OIDC_COOKIE_MAX_AGE
BLOCKYARD_OPENBAO_ADDRESS
BLOCKYARD_OPENBAO_ADMIN_TOKEN
BLOCKYARD_OPENBAO_TOKEN_TTL
BLOCKYARD_OPENBAO_JWT_AUTH_PATH
BLOCKYARD_TELEMETRY_METRICS_ENABLED
BLOCKYARD_TELEMETRY_OTLP_ENDPOINT
BLOCKYARD_AUDIT_PATH
```

**Auto-construction of optional sections from env vars:** the v1 config
sections are `Option<T>` (absent when not in the TOML file). Setting an env
var like `BLOCKYARD_OIDC_CLIENT_ID` when no `[oidc]` section exists in the
TOML would silently do nothing under the v0 overlay pattern (the
`if let Some(oidc) = &mut self.oidc` guard fails). To support env-var-only
configuration (common in Docker Compose deployments where secrets come
entirely from env vars), `apply_env_overrides()` should auto-construct a
default struct when any env var in the section's prefix is set:

```rust
// Before applying individual overrides:
if self.oidc.is_none() && env_prefix_exists("BLOCKYARD_OIDC_") {
    self.oidc = Some(OidcConfig::default());
}
// Repeat for openbao, telemetry, audit
```

Required fields without meaningful defaults (e.g. `issuer_url`,
`client_secret`) start as empty strings and are caught by
`config.validate()` — same error path as a TOML section with missing
fields.

## Schema Migrations

**Pre-release consolidation:** before v0.1.0, the existing v0 migrations
(`001_initial.sql` and `002_remove_app_status.sql`) should be collapsed into a
single `001_initial.sql`. Since no external consumers have run these migrations,
there is no upgrade path to maintain. After v0.1.0, migrations are append-only
and immutable. Migration numbers below are relative to the v0.1.0 baseline and
will be assigned final numbers at implementation time.

v1 adds three migrations:

```sql
-- 002_add_owner_and_vanity.sql
-- owner is NOT NULL — table rebuild required for SQLite compatibility.
-- Since v0 migrations are consolidated pre-release, no existing rows
-- need migration.
ALTER TABLE apps ADD COLUMN owner TEXT NOT NULL;
ALTER TABLE apps ADD COLUMN vanity_url TEXT UNIQUE;
ALTER TABLE apps ADD COLUMN title TEXT;

-- 003_access_control.sql
CREATE TABLE app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('user', 'group')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

CREATE TABLE role_mappings (
    group_name  TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'publisher', 'viewer')),
    PRIMARY KEY (group_name)
);

-- 004_tags.sql
CREATE TABLE tags (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL
);

CREATE TABLE app_tags (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    tag_id      TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, tag_id)
);
```

## Build Order and Dependency Graph

```
Phase 1-1: OIDC + User Sessions
  ├── OIDC discovery client
  ├── Authorization code flow (/login, /callback, /logout)
  ├── Signed session cookies
  ├── Token refresh middleware
  └── App-plane auth middleware

Phase 1-2: IdP Client Credentials + RBAC
  ├── JWT validation (control plane)
  ├── Role system + group mapping
  ├── Per-content ACL
  └── depends on: Phase 1-1 (JWKS cache, JWT validation infra)

Phase 1-3: Identity Injection + OpenBao
  ├── X-Shiny-User / X-Shiny-Groups headers
  ├── OpenBao client + JWT auth
  ├── Session-scoped token issuance
  ├── Credential enrollment API
  └── depends on: Phase 1-1 (user sessions, access tokens)

Phase 1-4: Session Sharing + Load Balancing + Auto-scaling
  ├── Unlock max_sessions_per_worker
  ├── Least-loaded worker assignment
  ├── Connection-based auto-scaling
  ├── Graceful drain
  └── depends on: Phase 1-2 (RBAC for per-app worker limits)

Phase 1-5: Vanity URLs + Content Discovery
  ├── Vanity URL routing
  ├── Catalog API + tags
  └── depends on: Phase 1-2 (RBAC for catalog visibility)

Phase 1-6: Audit Logging + Telemetry + /readyz
  ├── Audit log writer
  ├── Prometheus metrics
  ├── OpenTelemetry tracing
  ├── /readyz endpoint
  └── depends on: Phase 1-3 (OpenBao health check)
```

Phases 1-5 and 1-6 are independent of each other and can be developed in
parallel. Phase 1-4 is independent of 1-5 and 1-6. The critical path is
1-1 → 1-2 → 1-3 → 1-4.

## Test Strategy

### Unit tests

- **OIDC tests:** mock IdP discovery response, verify JWKS parsing, token
  validation with known keys, expired token rejection, wrong issuer
  rejection.
- **Session cookie tests:** sign/verify round-trip, tampered cookie
  rejection, expired cookie handling, refresh token encryption/decryption.
- **RBAC tests:** role derivation from groups, permission checks for each
  role, ACL evaluation with user grants, group grants, owner override.
- **Load balancer tests:** least-loaded assignment, capacity exhaustion
  (503), scale-up trigger, scale-down with idle workers.
- **Vanity URL tests:** collision detection against reserved prefixes,
  duplicate rejection, resolution to correct app.
- **Audit log tests:** entry serialization, write ordering.

### Integration tests

These extend the v0 integration test infrastructure (mock backend + test
server):

- **Auth flow tests:** full login → callback → session cookie → authenticated
  request → logout cycle. Uses a mock IdP (static JWKS + token endpoint).
- **RBAC integration tests:** create app as publisher, verify viewer cannot
  deploy, verify admin can access all, verify ACL grants work.
- **Proxy auth tests:** unauthenticated request redirects to `/login`,
  authenticated request is proxied, identity headers are injected.
- **Load balancing tests:** configure `max_sessions_per_worker = 2`, send 3
  sessions, verify 2 workers spawned, sessions distributed correctly.
- **Auto-scaling tests:** saturate workers, verify scale-up; disconnect
  sessions, verify scale-down.
- **Vanity URL tests:** assign vanity URL, request via vanity path, verify
  proxied to correct app.
- **Catalog tests:** create apps with tags, query catalog with filters,
  verify RBAC-filtered results.

### Mock IdP

Tests require a mock identity provider. Implemented as a test helper:

```rust
/// Starts a minimal OIDC-compliant mock IdP for integration tests.
/// Serves /.well-known/openid-configuration, /jwks, /token, /authorize.
/// Issues JWTs signed with a test RSA key.
struct MockIdp {
    addr: SocketAddr,
    signing_key: RsaPrivateKey,
}

impl MockIdp {
    async fn start() -> Self;
    fn issue_token(&self, sub: &str, groups: &[&str]) -> String;
}
```

### Docker integration tests

Extended with:

- **OpenBao integration:** start OpenBao dev server, verify JWT auth setup,
  token issuance, secret read.
- **Identity headers:** verify `X-Shiny-User` reaches the container.

## Design Decisions

1. **Minimal cookie + server-side session store.** The signed cookie carries
   only `sub` and `issued_at` (~100-150 bytes). All sensitive/bulky data
   (groups, access token, refresh token) lives server-side in a
   `DashMap<String, UserSession>` keyed by `sub`. This avoids cookie size
   issues (IdP JWT access tokens are 1-2KB; combined with groups and
   encrypted refresh tokens the cookie easily exceeds the 4KB browser
   limit), removes the need for AES-GCM encryption, and enables immediate
   session invalidation on logout. Trade-off: sessions are lost on server
   restart, but this matches all other in-memory state in v1 (workers,
   proxy sessions, task store). v2's PostgreSQL migration extends naturally
   to session state.

2. **Static token fallback when OIDC is not configured.** The `[oidc]`
   section is optional. When absent, the server runs in v0-compatible mode
   with static bearer token auth and no app-plane authentication. This
   supports development and single-operator deployments without requiring an
   IdP.

3. **Roles mapped from IdP groups, not stored in blockyard's DB.** Role
   assignment is driven by the `groups` claim in the IdP token. A
   `role_mappings` table maps group names to blockyard roles. This means
   role changes happen in the IdP (group membership) and are reflected on
   next login — no sync protocol needed. The trade-off: operators must
   manage group-to-role mappings.

4. **Least-loaded (not round-robin) load balancing.** Shiny sessions are
   long-lived and vary in resource consumption. Round-robin could overload
   one worker while others are idle. Least-loaded distributes more evenly.
   The assignment is sticky — once pinned, a session stays with its worker.

5. **Audit log as JSON Lines file, not a database table.** Append-only file
   writes are simpler, faster, and don't compete with the main database for
   SQLite's single writer lock. Operators ingest the file into their log
   aggregation system (ELK, Loki, etc.). The file is rotated by standard
   tools (logrotate). A future database-backed audit store can be added
   behind a trait if needed.

6. **OpenBao bootstrap is best-effort on startup.** The server does not fail
   to start if OpenBao is unreachable — it logs a warning and reports
   unhealthy via `/readyz`. This allows the server and OpenBao to start in
   any order in Docker Compose. Credential-dependent features are
   unavailable until OpenBao is healthy.

7. **Vanity URLs are a column on the apps table, not a separate routing
   table.** Each app can have at most one vanity URL. This is simpler than
   a many-to-many routing table and sufficient for v1. If multiple aliases
   per app are needed later, extract to a separate table.

8. **Per-request credential injection, not per-container env vars.** OpenBao
   tokens are injected as HTTP headers (`X-Blockyard-Vault-Token`) on each
   proxied request, not as env vars at container spawn time. This supports
   `max_sessions_per_worker > 1` safely — each user's session gets their
   own scoped token. The R app reads the token via `session$request`, the
   standard Shiny mechanism for proxy-injected headers. This matches how
   Posit Connect handles OAuth Integrations.

9. **Metadata endpoint protection unchanged from v0.** v0's per-network
   iptables rules (or host-level rule fallback) blocking `169.254.169.254`
   remain sufficient for v1. No changes needed — the protection applies to
   all worker containers regardless of authentication or credential
   injection model.

10. **Catalog `status` field is derived from local in-memory state.** The
    catalog API's `status` field is computed from the workers DashMap, which
    is node-local. This is accurate for v1 (single server). For v2
    multi-node deployments, `status` will need to come from shared state
    (PostgreSQL-backed worker registry) or be documented as approximate.

11. **`SessionStore`, `WorkerRegistry`, and `TaskStore` remain concrete
    structs, not traits.** The roadmap describes these as traits with
    swappable implementations, but v0 implemented them as concrete
    `DashMap`-backed structs (a deliberate simplification documented in
    phase 0-5). v1 does not require distributed state, so trait extraction
    is deferred to v2 when PostgreSQL-backed implementations are needed for
    multi-node deployments. See `docs/design/v2/draft.md`.
