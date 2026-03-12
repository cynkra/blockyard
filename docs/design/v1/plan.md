# blockyard v1 Implementation Plan

This document is the build plan for v1 — the MVP milestone. It covers new
packages, dependency additions, build phases, key type definitions, schema
changes, and test strategy. The roadmap (`../roadmap.md`) is the source of
truth for *what* v1 includes; this document describes *how* to build it.

v1 builds on v0's infrastructure (Docker backend, proxy layer, bundle
pipeline, REST API) and adds everything needed to host a real blockr app for
real users: user authentication (OIDC), authorization (RBAC), per-user
credential management (OpenBao), multi-worker scaling, content discovery,
and operational observability.

## New Packages

v1 adds the following packages to the existing layout. Existing packages
(`api/`, `proxy/`, `backend/`, `bundle/`, `db/`, `config/`, `server/`,
`task/`, `ops/`, `session/`, `registry/`, `logstore/`) are extended in
place.

```
internal/
├── auth/
│   ├── oidc.go              # OIDC discovery, authorization code flow
│   ├── session.go           # signed session cookie (encode/decode), server-side store
│   ├── jwt.go               # JWT validation against JWKS (control plane)
│   └── middleware.go         # app-plane + control-plane auth middleware
├── authz/
│   ├── rbac.go              # role definitions, permission checks
│   └── acl.go               # per-content access control lists
├── integration/
│   ├── openbao.go           # OpenBao client, JWT auth setup, bootstrap
│   └── enrollment.go        # credential enrollment logic
├── vanity/
│   ├── vanity.go            # vanity URL resolution + collision detection
│   └── reserved.go          # reserved prefix blocklist
├── catalog/
│   ├── catalog.go           # content listing, search, filtering
│   └── tags.go              # tag CRUD, app-tag associations
├── audit/
│   └── audit.go             # append-only JSON Lines audit log writer
├── telemetry/
│   └── telemetry.go         # Prometheus metrics + OpenTelemetry tracing setup
├── api/
│   ├── ... (existing)
│   ├── users.go             # /users/me, credential enrollment endpoints
│   ├── catalog.go           # catalog + tag endpoints
│   └── vanity.go            # vanity URL management endpoints
├── proxy/
│   ├── ... (existing)
│   ├── identity.go          # X-Shiny-User / X-Shiny-Groups injection
│   ├── loadbalancer.go      # least-loaded worker assignment
│   └── autoscaler.go        # connection-based auto-scaling loop
└── db/
    └── ... (existing, extended with roles, ACLs, tags, vanity URLs)
```

## New Dependencies

```go
// go.mod additions — existing deps unchanged
// (chi, docker/client, modernc.org/sqlite, coder/websocket, etc.)

// OIDC / JWT
require (
    github.com/coreos/go-oidc/v3   v3.x  // OIDC discovery, ID token verification
    golang.org/x/oauth2            v0.x  // OAuth 2.0 flows (authorization code, client credentials)
    github.com/go-jose/go-jose/v4  v4.x  // JWKS fetching, JWT parsing (used by go-oidc internally)
)

// Telemetry
require (
    github.com/prometheus/client_golang  v1.x  // Prometheus metrics + /metrics handler
    go.opentelemetry.io/otel             v1.x  // OpenTelemetry API
    go.opentelemetry.io/otel/sdk         v1.x  // OTel SDK (trace provider)
    go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc  v1.x  // OTLP gRPC exporter
)
```

**Dependency rationale:**

- **go-oidc** — full OIDC Relying Party implementation. Handles discovery,
  JWKS fetching, and ID token verification. Built on `golang.org/x/oauth2`.
- **golang.org/x/oauth2** — OAuth 2.0 authorization code flow and client
  credentials flow. Manages token exchange and refresh.
- **go-jose** — low-level JWKS and JWT operations. Used by `go-oidc`
  internally; exposed for control-plane JWT validation in phase 1-2.
- **prometheus/client_golang** — Prometheus-compatible metrics registry and
  HTTP handler. The standard Go metrics library.
- **opentelemetry** — connects `log/slog` structured logging to an OTel
  collector. Zero-cost when no collector is configured.

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
4. Server-side session store — `sync.RWMutex`-protected
   `map[string]*UserSession` keyed by `sub`, holds groups, access token,
   refresh token
5. Transparent access token refresh — on each request, if access token is
   near expiry, exchange refresh token and update server-side session
6. Auth middleware for the app plane — protect `/app/` routes; redirect
   unauthenticated users to `/login`
7. Config additions: `[oidc]` section
8. Cookie signing key derivation from a configured secret

**Config additions:**

```go
type OIDCConfig struct {
    IssuerURL    string        `toml:"issuer_url"`
    ClientID     string        `toml:"client_id"`
    ClientSecret string        `toml:"client_secret"`
    GroupsClaim  string        `toml:"groups_claim"`  // default: "groups"
    CookieMaxAge time.Duration `toml:"cookie_max_age"` // default: 24h
}
```

**Note on cookie lifetime vs. session lifetime:** `CookieMaxAge` controls how
long the browser retains the session cookie. The *effective* session duration is
`min(CookieMaxAge, refresh_token_lifetime)` — if the IdP's refresh token
expires before the cookie, the session ends at refresh token expiry regardless
of the cookie's max-age. Conversely, a long-lived refresh token is bounded by
the cookie max-age. Operators should align both values.

**Session architecture: minimal cookie + server-side store.**

The cookie carries only `sub` and `issued_at`, signed with HMAC-SHA256. All
sensitive/bulky data (groups, access token, refresh token) lives server-side
in a mutex-protected map. This avoids cookie size issues (IdP JWTs can be
1-2KB, easily exceeding the 4KB browser limit), eliminates the need for
refresh token encryption, and enables immediate session invalidation on logout.

```go
// CookiePayload is the minimal payload in the session cookie.
type CookiePayload struct {
    Sub      string `json:"sub"`
    IssuedAt int64  `json:"iat"`
}

// UserSession is server-side session data, keyed by sub.
type UserSession struct {
    Groups       []string
    AccessToken  string
    RefreshToken string
    ExpiresAt    time.Time
}

// SessionStore holds all active user sessions.
type SessionStore struct {
    mu       sync.RWMutex
    sessions map[string]*UserSession // keyed by sub
    secret   []byte                   // HMAC-SHA256 signing key
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
  → Validate ID token signature against JWKS (via go-oidc verifier)
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
and stores the authenticated user in the request context. The control plane
API continues to use bearer token auth (upgraded to JWT in phase 1-2).

```go
// AppAuthMiddleware protects /app/ proxy routes.
// Redirects unauthenticated users to /login?return_url={current_path}.
func AppAuthMiddleware(store *SessionStore) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            payload, err := store.VerifyCookie(r)
            if err != nil {
                http.Redirect(w, r, "/login?return_url="+r.URL.Path, http.StatusFound)
                return
            }
            session := store.Get(payload.Sub)
            if session == nil {
                http.Redirect(w, r, "/login?return_url="+r.URL.Path, http.StatusFound)
                return
            }
            store.RefreshIfNeeded(r.Context(), payload.Sub)
            ctx := WithAuthenticatedUser(r.Context(), payload, session)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

### Phase 1-2: IdP Client Credentials + RBAC + Per-Content ACL ([detailed plan](phase-1-2.md))

Replace the static bearer token with JWT-based machine auth. Add role-based
access control, per-content permissions, and public (anonymous) app access.

**Deliverables:**

1. JWT validation middleware for the control plane — validate bearer tokens
   as JWTs against the IdP's JWKS (same keys as OIDC)
2. OAuth 2.0 client credentials support — clients authenticate with
   `client_id` + `client_secret` at the IdP's token endpoint
3. Role system — three roles: `admin`, `publisher`, `viewer`
4. Role assignment — mapped from IdP groups claim
5. Per-content ACL — owners and explicit viewer/collaborator grants per app
6. Public (anonymous) access — apps with `access_type = 'public'` are
   accessible without authentication; identity headers injected when the
   user happens to be logged in, absent otherwise
7. Authorization checks on all API and proxy endpoints
8. Schema additions: roles, ACL tables, `access_type` column on apps

**Roles and permissions:**

| Permission | admin | publisher | viewer | anonymous |
|---|---|---|---|---|
| Create apps | yes | yes | no | no |
| Deploy bundles | yes | own apps | no | no |
| Start/stop apps | yes | own apps | no | no |
| Update app config | yes | own apps | no | no |
| Delete apps | yes | own apps | no | no |
| View all apps | yes | no | no | no |
| View accessible apps | yes | yes | yes | public only |
| Access app (proxy) | yes | own + granted | granted only | public only |
| Manage users/roles | yes | no | no | no |

"Own apps" = apps where the user is the `owner` (set at creation time to the
authenticated user's `sub`). "Public only" = apps with `access_type = 'public'`.

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

Access evaluation order: public app + unauthenticated → anonymous access;
admin overrides all → owner has full access → explicit ACL grants →
public app + authenticated → anonymous access (with identity headers) →
deny.

**ACL conflict resolution:** a principal may have multiple grants for the same
app (e.g., a direct `viewer` grant and a `collaborator` grant via group
membership). The effective role is the highest-privilege grant across all
matching entries. The access check collects all grants for the user's `sub`
(kind=user) plus all their group names (kind=group), then takes the max.
Role ordering is defined as a simple constant mapping (`collaborator > viewer`).

**ACL enforcement on active sessions:** ACL checks run on HTTP requests only,
not on individual WebSocket frames. When a user's access is revoked, it takes
effect on their next HTTP request or WebSocket reconnect — active WS
connections continue until the next reconnect. This avoids per-frame database
or cache lookups on the hot path. If per-request ACL checks become a
performance concern, an in-memory ACL cache with short TTL (30–60s) can be
added as an optimization.

**JWT validation for control plane:**

```go
// APIAuthMiddleware replaces the v0 static bearer token check.
// Validates JWT signature against JWKS, checks expiry and issuer.
// Falls back to static token if [oidc] is not configured (dev mode).
func APIAuthMiddleware(srv *server.Server) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            token := extractBearerToken(r)

            if srv.Config.OIDC != nil {
                claims, err := validateJWT(r.Context(), token, srv.JWKSKeySet, srv.Config.OIDC)
                if err != nil {
                    http.Error(w, "unauthorized", http.StatusUnauthorized)
                    return
                }
                ctx := WithCallerIdentity(r.Context(), claims)
                next.ServeHTTP(w, r.WithContext(ctx))
            } else {
                // Fallback: static token comparison (v0 compat / dev mode)
                if token != srv.Config.Server.Token {
                    http.Error(w, "unauthorized", http.StatusUnauthorized)
                    return
                }
                next.ServeHTTP(w, r)
            }
        })
    }
}
```

**Schema additions:**

```sql
-- Add owner and access_type to apps table.
-- Pre-release migration consolidation means no existing rows need migration.
ALTER TABLE apps ADD COLUMN owner TEXT NOT NULL;
ALTER TABLE apps ADD COLUMN access_type TEXT NOT NULL DEFAULT 'acl' CHECK (access_type IN ('acl', 'public'));

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

```go
// InjectIdentityHeaders adds X-Shiny-User and X-Shiny-Groups to the
// outgoing request. Strips any client-supplied values to prevent spoofing.
func InjectIdentityHeaders(r *http.Request, user *AuthenticatedUser) {
    r.Header.Del("X-Shiny-User")
    r.Header.Del("X-Shiny-Groups")
    r.Header.Set("X-Shiny-User", user.Sub)
    r.Header.Set("X-Shiny-Groups", strings.Join(user.Groups, ","))
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
1. Auth middleware extracts AuthenticatedUser from request context
2. Look up cached OpenBao token for this user's sub
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

```go
// VaultTokenCache caches OpenBao tokens keyed by user sub.
// Avoids calling OpenBao's JWT login endpoint on every request.
type VaultTokenCache struct {
    mu     sync.RWMutex
    tokens map[string]*cachedToken
}

type cachedToken struct {
    Token     string
    ExpiresAt time.Time
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

```go
type OpenbaoConfig struct {
    Address     string        `toml:"address"`       // e.g. https://bao.example.com
    AdminToken  Secret        `toml:"admin_token"`   // use BLOCKYARD_OPENBAO_ADMIN_TOKEN env var
    TokenTTL    time.Duration `toml:"token_ttl"`     // default: 1h
    JWTAuthPath string        `toml:"jwt_auth_path"` // default: "jwt"
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

**Load balancing strategy:**

```go
// LoadBalancer assigns sessions to workers. Stateless — decisions are
// based on current worker and session state.
type LoadBalancer struct{}

// Assign picks a worker for a new session.
// 1. Find workers with available capacity (sessions < maxSessionsPerWorker)
// 2. Among those, pick the one with fewest sessions (least-loaded)
// 3. If none have capacity and maxWorkersPerApp not reached, return "",nil
//    (caller spawns a new worker)
// 4. If none have capacity and at maxWorkersPerApp, return ErrCapacityExhausted
func (lb *LoadBalancer) Assign(
    appID string,
    workers *server.WorkerMap,
    sessions *session.Store,
    maxSessions int,
    maxWorkers *int,
) (string, error)
```

Sticky sessions: once assigned, a session stays pinned to its worker via
the session store (unchanged from v0). The load balancer only runs on new
session creation.

**Auto-scaling:**

```go
// RunAutoscaler runs as a background goroutine alongside health polling.
// Checks each app's worker count against demand.
func RunAutoscaler(ctx context.Context, srv *server.Server) {
    ticker := time.NewTicker(srv.Config.Proxy.HealthInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            for _, app := range listRunningApps(srv) {
                workerCount := countWorkersForApp(srv, app.ID)
                sessionCount := countSessionsForApp(srv, app.ID)
                maxSessions := app.MaxSessionsPerWorker

                // Scale up: if all workers are at capacity and below max_workers
                if allAtCapacity(workerCount, sessionCount, maxSessions) {
                    if canScaleUp(app, workerCount, srv) {
                        spawnWorkerForApp(ctx, srv, app)
                    }
                }

                // Scale down: if a worker has 0 sessions and others have capacity
                if hasIdleWorkers(workerCount, sessionCount, maxSessions) {
                    drainIdleWorker(ctx, srv, app)
                }
            }
        }
    }
}
```

Scale-up is eager (spawn when all workers are full); scale-down is
conservative (only remove workers with zero sessions). Scale-to-zero is
deferred to v2.

**Graceful drain on stop:**

```go
// stopAppGraceful drains sessions before killing workers.
// v0 kills workers immediately; v1 waits for sessions to end.
func stopAppGraceful(ctx context.Context, srv *server.Server, appID string) {
    workers := getWorkersForApp(srv, appID)

    // 1. Stop routing new sessions to this app
    markAppDraining(srv, appID)

    // 2. Wait for existing sessions to end (up to shutdown_timeout)
    deadline := time.Now().Add(srv.Config.Server.ShutdownTimeout)
    for {
        remaining := countSessionsForWorkers(srv, workers)
        if remaining == 0 || time.Now().After(deadline) {
            break
        }
        time.Sleep(time.Second)
    }

    // 3. Force-stop remaining workers
    for _, w := range workers {
        ops.EvictWorker(ctx, srv, w.ID)
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

```go
// Router setup. Vanity routes are checked before /app/{name}/ routes.
func NewRouter(srv *server.Server) *chi.Mux {
    r := chi.NewRouter()

    // API routes (existing)
    r.Route("/api/v1", func(r chi.Router) { /* ... */ })

    // Auth endpoints
    r.Get("/login", loginHandler(srv))
    r.Get("/callback", callbackHandler(srv))
    r.Post("/logout", logoutHandler(srv))

    // Vanity URL catch-all — checked before /app/{name}/
    // Returns 404 if no vanity URL matches.
    r.Get("/{vanity}", trailingSlashRedirectVanity)
    r.HandleFunc("/{vanity}/", vanityProxyHandler(srv))
    r.HandleFunc("/{vanity}/*", vanityProxyHandler(srv))

    // Standard app routes
    r.Get("/app/{name}", trailingSlashRedirect)
    r.HandleFunc("/app/{name}/", proxyHandler(srv))
    r.HandleFunc("/app/{name}/*", proxyHandler(srv))

    return r
}
```

**Reserved prefix blocklist:**

```go
var reservedPrefixes = []string{
    "api", "app", "login", "callback", "logout", "healthz", "readyz",
    "metrics", "static", "assets", "admin",
}
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

```go
// AuditLog is an append-only audit log backed by a JSON Lines file.
// Writes are buffered via a channel and flushed by a background goroutine.
type AuditLog struct {
    entries chan AuditEntry
}

// Log sends an entry to the background writer. Non-blocking.
func (a *AuditLog) Log(entry AuditEntry) {
    select {
    case a.entries <- entry:
    default:
        slog.Warn("audit log buffer full, dropping entry", "action", entry.Action)
    }
}

// runWriter is the background goroutine that appends entries to the log file.
func runWriter(ctx context.Context, entries <-chan AuditEntry, path string) {
    f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
    if err != nil {
        slog.Error("failed to open audit log", "path", path, "err", err)
        return
    }
    defer f.Close()

    enc := json.NewEncoder(f)
    for {
        select {
        case <-ctx.Done():
            // Drain remaining entries before exit
            for {
                select {
                case entry := <-entries:
                    enc.Encode(entry)
                default:
                    return
                }
            }
        case entry := <-entries:
            enc.Encode(entry)
        }
    }
}
```

**Prometheus metrics:**

```go
// Registered at startup.
var (
    // Gauges
    workersActive  = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "blockyard_workers_active", Help: "Currently running workers",
    })
    sessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "blockyard_sessions_active", Help: "Active proxy sessions",
    })

    // Counters
    workersSpawned = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_workers_spawned_total", Help: "Total workers spawned",
    })
    workersStopped = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_workers_stopped_total", Help: "Total workers stopped",
    })
    bundlesUploaded = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_bundles_uploaded_total", Help: "Total bundles uploaded",
    })
    proxyRequests = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_proxy_requests_total", Help: "Total proxied requests",
    })
    healthChecksFailed = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_health_checks_failed_total", Help: "Failed health checks",
    })

    // Histograms
    coldStartDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Name: "blockyard_cold_start_seconds", Help: "Worker cold-start duration",
    })
    proxyRequestDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Name: "blockyard_proxy_request_seconds", Help: "Proxy request duration",
    })
)
```

The `/metrics` endpoint is unauthenticated (same as `/healthz`). Operators
can restrict access at the network level if needed.

**`/readyz` endpoint:**

```go
func readyzHandler(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        checks := make(map[string]string)

        // Database
        if err := srv.DB.Ping(r.Context()); err != nil {
            checks["database"] = "fail"
        } else {
            checks["database"] = "pass"
        }

        // Docker socket
        if _, err := srv.Backend.ListManaged(r.Context()); err != nil {
            checks["docker"] = "fail"
        } else {
            checks["docker"] = "pass"
        }

        // IdP (OIDC discovery endpoint)
        if srv.Config.OIDC != nil {
            if err := checkIDP(r.Context(), srv.Config.OIDC); err != nil {
                checks["idp"] = "fail"
            } else {
                checks["idp"] = "pass"
            }
        }

        // OpenBao
        if srv.Config.Openbao != nil {
            if err := checkOpenbao(r.Context(), srv.Config.Openbao); err != nil {
                checks["openbao"] = "fail"
            } else {
                checks["openbao"] = "pass"
            }
        }

        allOK := true
        for _, v := range checks {
            if v == "fail" {
                allOK = false
                break
            }
        }

        status := "ready"
        httpStatus := http.StatusOK
        if !allOK {
            status = "not_ready"
            httpStatus = http.StatusServiceUnavailable
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(httpStatus)
        json.NewEncoder(w).Encode(map[string]any{
            "status": status,
            "checks": checks,
        })
    }
}
```

**Config additions:**

```go
type TelemetryConfig struct {
    MetricsEnabled bool   `toml:"metrics_enabled"` // default: false
    OTLPEndpoint   string `toml:"otlp_endpoint"`   // e.g. http://otel-collector:4317
}

type AuditConfig struct {
    Path string `toml:"path"` // e.g. /data/audit/blockyard.jsonl
}
```

## Config Summary

v1 config additions alongside v0 fields (v0 fields shown for context;
`log_retention`, `rv_version`, and `max_bundle_size` are omitted — see
roadmap for the complete v0 config):

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
new sections. Each field must be added to `supportedEnvVars()` and handled
in `applyEnvOverrides()` — the existing `TestEnvVarCoverageComplete` test
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
sections are pointer types (`*OIDCConfig`, etc.) — nil when not in the TOML
file. Setting an env var like `BLOCKYARD_OIDC_CLIENT_ID` when no `[oidc]`
section exists in the TOML would silently do nothing under the v0 overlay
pattern (the nil pointer check skips the section). To support env-var-only
configuration (common in Docker Compose deployments where secrets come
entirely from env vars), `applyEnvOverrides()` should auto-construct a
default struct when any env var in the section's prefix is set:

```go
// Before applying individual overrides:
if cfg.OIDC == nil && envPrefixExists("BLOCKYARD_OIDC_") {
    cfg.OIDC = &OIDCConfig{GroupsClaim: "groups", CookieMaxAge: 24 * time.Hour}
}
// Repeat for Openbao, Telemetry, Audit
```

Required fields without meaningful defaults (e.g. `IssuerURL`,
`ClientSecret`) start as zero values and are caught by `config.Validate()`
— same error path as a TOML section with missing fields.

## Schema Migrations

**Pre-release consolidation:** before v0.1.0, the existing v0 migrations
(`001_initial.sql` and `002_remove_app_status.sql`) should be collapsed into a
single `001_initial.sql`. Since no external consumers have run these migrations,
there is no upgrade path to maintain. After v0.1.0, migrations are append-only
and immutable. Migration numbers below are relative to the v0.1.0 baseline and
will be assigned final numbers at implementation time.

v1 adds three migrations:

```sql
-- 002_add_owner_vanity_access_type.sql
-- owner is NOT NULL — table rebuild required for SQLite compatibility.
-- Since v0 migrations are consolidated pre-release, no existing rows
-- need migration.
ALTER TABLE apps ADD COLUMN owner TEXT NOT NULL;
ALTER TABLE apps ADD COLUMN access_type TEXT NOT NULL DEFAULT 'acl' CHECK (access_type IN ('acl', 'public'));
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
  rejection, expired cookie handling.
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

Tests require a mock identity provider. Implemented as a test helper using
`net/http/httptest`:

```go
// MockIdP starts a minimal OIDC-compliant mock IdP for integration tests.
// Serves /.well-known/openid-configuration, /jwks, /token, /authorize.
// Issues JWTs signed with a test RSA key.
type MockIdP struct {
    Server     *httptest.Server
    SigningKey  *rsa.PrivateKey
    IssuerURL  string
}

func NewMockIdP() *MockIdP
func (m *MockIdP) IssueToken(sub string, groups []string) string
func (m *MockIdP) Close()
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
   mutex-protected `map[string]*UserSession` keyed by `sub`. This avoids
   cookie size issues (IdP JWT access tokens are 1-2KB; combined with groups
   and encrypted refresh tokens the cookie easily exceeds the 4KB browser
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
   behind an interface if needed.

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
    catalog API's `status` field is computed from the worker map, which is
    node-local. This is accurate for v1 (single server). For v2 multi-node
    deployments, `status` will need to come from shared state
    (PostgreSQL-backed worker registry) or be documented as approximate.

11. **Session store, worker registry, and task store remain concrete structs,
    not interfaces.** The roadmap describes these as swappable, but v0
    implemented them as concrete mutex-protected map structs (a deliberate
    simplification). v1 does not require distributed state, so interface
    extraction is deferred to v2 when PostgreSQL-backed implementations are
    needed for multi-node deployments.

12. **stdlib over external dependencies where possible.** Go's standard
    library covers HMAC signing (`crypto/hmac`), HTTP clients (`net/http`),
    JSON handling (`encoding/json`), and concurrent file I/O (goroutines +
    `os`). External dependencies are added only where the stdlib has no
    viable equivalent: OIDC discovery, Prometheus metrics, OpenTelemetry
    tracing.
