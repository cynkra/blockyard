# blockyard Roadmap

## Scope

`blockyard` is focused on hosting **blockr Shiny applications**. This
deliberately narrows the scope relative to general-purpose platforms like Posit
Connect:

- **Content type:** Shiny apps only. Plumber APIs, static sites, rendered
  documents, scheduled tasks, and parameterized reports are out of scope for
  now. The code should be factored into clear packages so adding a new content
  type later means adding a new package, not refactoring existing ones.

- **Runtime:** A single R version, configured server-wide. Per-deployment
  version selection, multi-version side-by-side installations, and version
  matching strategies add meaningful complexity for a use case we don't yet
  have. Add when there is a concrete need.

- **Language:** R only. Python, Julia, and multi-language dependency restore
  pipelines are out of scope. The `Backend` interface is agnostic to what runs
  inside a container, so adding a new language later is a matter of adding a
  new deployment pipeline, not changing the core architecture.

- **Isolation:** blockr apps execute arbitrary user-supplied R code. The
  default Docker backend provides full container isolation; a lightweight
  process backend using bubblewrap is available as an alternative (v3).
  The default is one worker per session (`max_sessions_per_worker = 1`).
  See the Worker Scaling feature entry for details.

**Milestones:** v0 is the core infrastructure — no user auth on the app
plane. v1 is the MVP: the minimum needed to host a real blockr app for real
users. v1 adds user auth (OIDC), identity injection, per-user credential
management (the integration system), and load balancing. Nothing beyond v1 is
required to call the product useful. v2 adds single-node production polish
(CLI, scale-to-zero, pre-warming, runtime package install). v3 adds the
lightweight process backend. v4 adds Kubernetes for multi-node scaling.

**The one deliberate exception to "no premature abstraction"** is the `Backend`
interface (Docker vs. process vs. Kubernetes). This abstraction is worth its
complexity because: (a) it affects every other layer of the architecture and
retrofitting it later is expensive, (b) its shape is well-validated across
multiple prior projects, and (c) we know we will need alternative backends
(process for lightweight deployments, Kubernetes for scale). The `Backend`
interface is validated in tests using a lightweight in-process mock.
Everything else — language dispatch, content-type routing tables, version
matching — gets built when there is a second use case to validate the
abstraction's shape.

## Server Configuration

The server is configured via a TOML file. Every value can be overridden by an
environment variable, which takes precedence — this is the recommended approach
for secrets in container deployments. The env var name is the config key path
uppercased with `BLOCKYARD_` prefix and dots replaced by underscores (e.g.
`[server].session_secret` → `BLOCKYARD_SERVER_SESSION_SECRET`).

**Config file location:** the server looks for `blockyard.toml` in the current
working directory by default. Override with `--config <path>`.

**Startup validation:** the server validates the full config (including env var
overrides) at startup and refuses to start on any error — missing required
fields, unreachable Docker socket, unreadable storage path, etc. Config changes
require a restart; there is no hot reload.

```toml
[server]
bind             = "0.0.0.0:8080"
# token field removed — superseded by Personal Access Tokens (v1 wrap-up §2)
shutdown_timeout = "30s"   # drain window on SIGTERM
log_level        = "info"  # trace, debug, info, warn, error

[docker]
socket     = "/var/run/docker.sock"  # or Podman socket path
image      = "ghcr.io/rocker-org/r-ver:4.4.3"
shiny_port = 3838                    # internal port Shiny listens on
rv_version = "latest"                # rv release tag, e.g. "v0.18.0"

[storage]
bundle_server_path = "/data/bundles"   # where the server reads/writes bundles
bundle_worker_path = "/app"            # where each worker sees its bundle (read-only)
bundle_retention   = 50                # max bundles retained per app; oldest non-active deleted first
max_bundle_size    = 104857600         # 100 MiB; upload size limit

[database]
path = "/data/db/blockyard.db"

[proxy]
ws_cache_ttl            = "60s"   # how long to hold backend WS on client disconnect
health_interval         = "15s"   # how often to poll worker health
worker_start_timeout    = "60s"   # how long to hold a request while a worker starts
max_workers             = 100     # hard ceiling on total running workers across all apps
log_retention           = "1h"    # how long to keep logs after a worker exits
session_idle_ttl        = "1h"    # sweep sessions idle longer than this
idle_worker_timeout     = "5m"    # evict workers idle (zero sessions) longer than this
```

**v1 additions** (OIDC + OpenBao arrive together — neither is meaningful
without the other):

```toml
[server]
# v1 additions to [server]:
session_secret = "..."                              # HMAC key for cookie signing
                                                    # use BLOCKYARD_SERVER_SESSION_SECRET env var
                                                    # required when [oidc] is configured and
                                                    # [openbao] is not — auto-generated and
                                                    # persisted to vault when [openbao] is set
management_bind = "127.0.0.1:9100"                  # optional: separate listener for /healthz,
                                                    # /readyz, /metrics (no auth). Keeps ops
                                                    # endpoints off the public listener.
external_url   = "https://blockyard.example.com"    # public URL for OIDC redirect_uri and
                                                    # cookie Secure flag; required behind a
                                                    # reverse proxy

[oidc]
issuer_url    = "https://auth.example.com/realms/myrealm"
client_id     = "blockyard"
client_secret = "..."    # use BLOCKYARD_OIDC_CLIENT_SECRET env var or
                         # vault:secret/data/blockyard/oidc#client_secret
initial_admin  = "..."    # OIDC sub of the first admin user
                           # checked only on first login; use BLOCKYARD_OIDC_INITIAL_ADMIN env var
cookie_max_age = "24h"   # optional, default: 24h

[openbao]
address     = "https://bao.example.com"
role_id     = "blockyard-server"         # AppRole role ID (not secret — like a username)
# secret_id is NEVER in config — delivered once via BLOCKYARD_OPENBAO_SECRET_ID env var
# admin_token is deprecated — still accepted for migration, but cannot coexist with role_id
```

## Features

### v0: Core Infrastructure

One session per worker (`max_sessions_per_worker = 1`, enforced),
`max_workers_per_app` unlimited by default, global `max_workers` ceiling.
Docker backend, Shiny apps only. No user auth on the app plane. Control
plane protected by a single static bearer token in config.

---

- **Backend interface abstraction.** A pluggable interface that lets the server
  manage workers without knowing whether they are Docker containers or
  Kubernetes pods. This is the architectural foundation — every other feature
  depends on it. Includes the interface definition, `WorkerSpec` (what to run),
  and an opaque worker ID (string) returned by the backend. The interface is
  agnostic to what runs inside the worker — it deals only with *where* and
  *how* containers are launched, not *what* they run. Validated in tests using
  a lightweight in-process mock backend; no bare-metal process spawner is
  needed.

- **Docker / Podman backend.** Implement the `Backend` interface using the
  Docker Go client (`github.com/docker/docker/client`) to create and manage
  containers. Covers image pulling, container creation, health checking, log
  streaming, and cleanup. The only production backend for single-host
  deployments and the reference implementation for the interface. See
  [Network Isolation](architecture.md#network-isolation) for the per-container
  bridge design and container hardening.

- **Worker scaling.** Two per-app numeric parameters control how containers are
  shared across sessions — the same model as Posit Connect's `Max Processes`
  and `Max Connections Per Process`:

  - **`max_workers_per_app` (default: unlimited):** how many container replicas
    can run in parallel for this app. Unlimited by default — capped only by the
    global `max_workers` ceiling. When set explicitly, prevents one app from
    starving others. When `> 1`, the proxy load-balances incoming sessions
    across workers.
  - **`max_sessions_per_worker` (default 1):** how many sessions share one
    container. When `1` (the default), every session gets its own container —
    no cross-session interference is possible. When `> 1`, sessions share the
    same R process, global environment, `system()` calls, and file I/O.
    Session sharing is appropriate only for authenticated, mutually-trusting
    users where lower resource cost and absence of per-session cold starts are
    worthwhile.

  With the defaults (`max_workers_per_app = unlimited`,
  `max_sessions_per_worker = 1`), every session gets its own container and
  workers accumulate up to the global `max_workers` ceiling. This is the
  recommended configuration for any deployment with external or untrusted
  users.

  Both fields are present in the schema from v0. In v0,
  `max_sessions_per_worker` is always 1 (other values are rejected) —
  session-sharing logic is deferred to v1. `max_workers_per_app` is respected
  from v0 but defaults to unlimited.

- **HTTP / WebSocket reverse proxy.** Accept incoming HTTP and WebSocket
  connections and forward them to the correct backend worker based on URL
  routing. Must handle connection upgrades (HTTP → WS), set `X-Forwarded-*`
  headers, and support multiple apps on different URL prefixes.

  **URL scheme:**

  ```
  /api/v1/...      → control plane REST API
  /app/{name}/     → proxied Shiny app (name-based, v0)
  /app/{uuid}/     → proxied Shiny app (UUID-based, stable across renames, v1)
  ```

  Apps are routed by name. The proxy redirects `/app/{name}` (no trailing
  slash) to `/app/{name}/` and strips the prefix before forwarding to the
  container — Shiny requires a trailing-slash prefix for relative asset URLs
  to resolve correctly.

- **Session and worker routing.** Cookie-based session pinning. A session store
  maps session IDs to worker IDs; a worker registry maps worker IDs to network
  addresses. These start as concrete in-memory structs for v0. When v2 needs
  PostgreSQL-backed implementations for HA, extracting an interface is a
  low-cost refactor — define the interface at the call site and any struct with
  matching methods already satisfies it.

  On first request to `/app/{name}/`, the proxy sets a session cookie containing
  a generated session ID. Subsequent requests — including WebSocket reconnects
  during the `ws_cache_ttl` grace period — are routed via this cookie.

- **Cold-start UX.** When a new session requires a new container, the proxy
  holds the initial HTTP request open until the container passes its health
  check, then forwards it — the user sees the browser's native loading
  indicator while R starts. No custom loading page is served; the browser
  handles the wait. If the container does not become healthy within
  `worker_start_timeout` (default `60s`), the held request is released with
  a 503. A custom loading page is a v2 polish item.

- **WebSocket session caching.** When a browser briefly disconnects (page
  reload, network glitch), hold the backend WebSocket connection open for a
  grace period so the client can reconnect to the same session. Critical for
  Shiny apps where session state lives in the R process. When
  `max_sessions_per_worker = 1`, this also means keeping the container alive
  during the grace period — the container is not stopped until the grace period
  expires without a reconnect.

- **Active health polling.** After a worker starts, periodically poll its
  endpoint (TCP connect or lightweight HTTP probe) to detect hung processes.
  Shiny Server only probes at startup; a hung R process can hold a worker slot
  indefinitely. On failure, mark the worker unhealthy and stop it via the
  backend.

- **Network isolation.** App containers execute arbitrary user-supplied R code
  and must be isolated from each other, from the server's management API, and
  from host-level network services (e.g. cloud instance metadata). Internet
  egress is permitted. See [Network Isolation](architecture.md#network-isolation)
  for the mechanism.

- **Bundle upload and deployment.** Accept a tar.gz archive of app code via a
  REST endpoint, unpack it to a content directory, trigger dependency
  restoration, and register it in the content database. App name and all
  configuration are supplied via the API — there is no in-bundle manifest file.
  The conventional entrypoint is `app.R`; an `rv.lock` at the bundle root is
  required for dependency restoration. Each upload creates a new versioned
  bundle; previous bundles are retained up to a configurable limit, enabling
  rollback. Typical deploy flow:

  ```
  POST /api/v1/apps                       { "name": "my-app" }  →  { "id": "a3f2c1...", ... }
  POST /api/v1/apps/{id}/bundles          <tar.gz body>          →  202 { "bundle_id": "...", "task_id": "..." }
  GET  /api/v1/tasks/{task_id}/logs                              →  chunked restore output
  ```

  The upload endpoint returns immediately with `202 Accepted`. The bundle is
  created with status `pending` and dependency restoration runs asynchronously
  in a build container. While restoring, the bundle is `building`. On success
  it transitions to `ready` and is activated (set as `active_bundle` on the
  app). On failure it transitions to `failed` and the previous active bundle
  remains unchanged.
  The `active_bundle` foreign key only ever points at a `ready` bundle;
  this is enforced in application logic.

  Callers stream restore output via `GET /api/v1/tasks/{task_id}/logs` —
  chunked plain text, compatible with `curl -N`. The task is in-memory only;
  it does not survive a server restart. If the server is restarted while a
  restore is running, the bundle is marked `failed` during orphan cleanup and
  the caller must re-deploy.

  **Storage layout (on the server):**
  ```
  {bundle_server_path}/
    {app-id}/
      {bundle-id}.tar.gz    # uploaded archive
      {bundle-id}/          # unpacked app code
        app.R
        rv.lock
        ...
      {bundle-id}_lib/      # R package library restored by rv
  ```

  Archives are written to a temp path first and moved atomically into place on
  success — no partial state on failed uploads. Unpacking happens eagerly at
  upload time. When the number of bundles for an app exceeds
  `bundle_retention` (default 50), the oldest non-active bundles are deleted
  (archive + unpacked dir + library). The active bundle is never deleted
  automatically.

  **Container mounts:** each worker receives two read-only bind mounts:
  - The unpacked app directory → `{bundle_worker_path}` (read-only)
  - The restored R package library → `/blockyard-lib` (read-only)

  The library is mounted at a fixed path outside the app directory to avoid
  conflicts with the read-only app mount. The environment variable
  `R_LIBS=/blockyard-lib` tells R where to find installed packages. Both
  mounts are read-only — app code cannot modify its own source or installed
  packages at runtime.

- **Dependency restoration.** After uploading a bundle, restore R package
  dependencies from `rv.lock` using [`rv`](https://github.com/A2-ai/rv).
  `rv` is a hard runtime requirement. Restore runs via the backend's build
  method — how the build step executes is backend-specific: a run-to-completion
  container on Docker/Podman, an init container or Job on Kubernetes. The `rv`
  binary is downloaded from GitHub releases and cached locally in
  `{bundle_server_path}/.rv-cache/` (`internal/rvcache`). Pinned versions are
  cached indefinitely; `latest` is re-fetched when the cache is older than one
  hour. Downloads are serialized via a global mutex and written atomically to
  prevent partial-file corruption.
  A shared cache avoids re-downloading packages on every deploy.
  The restored library is written to `{bundle-id}_lib/` alongside the unpacked
  bundle and mounted read-only into app workers at `/blockyard-lib`.
  The R version is configured server-wide — no per-deployment version selection
  or version matching logic.

  Restore output (stdout/stderr from `rv`) is streamed to the caller via the
  task log endpoint. The task lifecycle is managed by an in-memory task store.

- **Content registry.** A SQLite database. Core v0 tables:

  - `apps` — name, UUID, owner, access type, resource limits
    (`max_workers_per_app`, `max_sessions_per_worker`, `memory_limit`,
    `cpu_limit`), catalog fields (`title`, `description`), and active bundle
    ID. No persisted status column — app status (running/stopped) is inferred
    at runtime from whether any workers exist for the app.
  - `bundles` — per-app bundle history: upload timestamp, bundle status
    (`pending | building | ready | failed`)

  v1 additions:
  - `users` — OIDC-authenticated users: sub, email, name, role, active status
  - `personal_access_tokens` — identity-aware API tokens (SHA-256 hash stored)
  - `app_access` — per-content ACL grants (viewer/collaborator per user)
  - `tags` / `app_tags` — admin-managed tags for content discovery

  See [Database Schema](architecture.md#database-schema) for the full DDL.
  Runtime worker state (container ID → session mapping) is in-memory. SQLite
  stores only what must survive a server restart.

- **REST API.** HTTP endpoints for all server operations: deploy apps, list
  apps, start/stop apps, manage settings, view logs. This is the primary
  interface — the CLI and (eventually) the web UI are clients of this API.

  All endpoints are prefixed with `/api/v1/`. The version is the API contract
  version, independent of the product milestone — it starts at `v1` from day
  one and increments only on breaking changes.

  Core v0 endpoints:
  ```
  POST   /api/v1/apps                    create an app
  GET    /api/v1/apps                    list apps
  GET    /api/v1/apps/{id}               get app details
  DELETE /api/v1/apps/{id}               delete an app
  POST   /api/v1/apps/{id}/bundles       upload a bundle (202; restore runs async)
  GET    /api/v1/apps/{id}/bundles       list bundles
  POST   /api/v1/apps/{id}/start         start an app
  POST   /api/v1/apps/{id}/stop          stop an app
  GET    /api/v1/apps/{id}/logs          stream or fetch logs
  PATCH  /api/v1/apps/{id}              update app config (resource limits, worker scaling)
  GET    /api/v1/tasks/{task_id}/logs    stream restore output (chunked plain text)
  ```

- **Control plane authentication (static token).** A single bearer token
  configured in the server config file. No database storage, no issuance
  logic. Sufficient for development and single-operator deployments where
  network-level access control is acceptable. Replaced by Personal Access
  Tokens in the v1 wrap-up.

- **App log capture.** Capture stdout/stderr from each container and make it
  available via the REST API (`GET /api/v1/apps/{id}/logs`). Logs must be
  persisted for a configurable period after a container exits so crashes can be
  diagnosed after the fact. Captured via Docker's log streaming API using the
  container's `dev.blockyard/app-id` and `dev.blockyard/worker-id` labels.

- **Orphan cleanup.** On startup, query Docker for containers and networks
  labeled `dev.blockyard/managed=true` and remove any the server has no
  active record for. Prevents resource leaks accumulating across server
  restarts. Orphaned containers are simply removed — there is no session
  state to resume.

- **`/healthz` endpoint.** Unauthenticated liveness check. Returns `200 OK`
  whenever the server process is running. No dependency checks.

- **Execution environment images.** A single server-wide Docker image
  configured in `[docker] image`. We maintain a Rocker-based image with R +
  required system libraries. The image is pulled on demand — via an
  `ensureImage()` check that pulls only if the image is not already present
  locally. Image selection and pinning are an operational concern managed
  centrally, not by the server or app developers.

- **Per-content resource limits (schema only).** CPU and memory limit fields
  are present in the content registry and carried in `WorkerSpec` from v0 so
  the schema does not change when enforcement is added. Actual enforcement is
  backend-specific and deferred.

### v1 / MVP: User-Facing Completeness

Adds everything needed to host a real blockr app for real users. Builds on v0
infrastructure.

---

- **Multi-worker and session sharing.** Enforce `max_workers_per_app` and
  `max_sessions_per_worker` when `> 1`. OpenBao credentials are injected
  per-request via HTTP headers — not per-worker at spawn time. With
  `max_sessions_per_worker = 1`, the proxy injects the raw vault token
  directly. With `max_sessions_per_worker > 1`, the proxy injects a
  signed session reference token that the app exchanges for the real
  credential via a server callback (see Integration system).

- **Load balancing.** Distribute incoming sessions across multiple workers when
  `max_workers_per_app > 1`. Shiny requires cookie-hash sticky sessions —
  sessions are stateful and tied to a specific R process; once a session is
  assigned to a worker it stays there. Comes as a package deal with
  auto-scaling.

- **Auto-scaling.** Monitor active connections or request rate per app and
  dynamically spawn or stop workers within the `max_workers_per_app` bound.
  Paired with load balancing.

- **OIDC authentication.** Delegate user authentication to an external identity
  provider via OpenID Connect. Implement against OIDC Discovery, which
  auto-discovers all endpoints. The only configuration required is the issuer
  URL and client credentials. Any compliant IdP works without IdP-specific code: Keycloak,
  Authentik, Auth0, Okta, Azure AD, Google Workspace.

- **Personal Access Tokens.** PATs replace the static bearer token for
  non-browser API access (CLI, CI/CD, scripts). Each PAT is identity-aware
  (tied to a user in the `users` table), per-user, and individually
  revocable. PATs are created via an OIDC session (browser login) and used
  as `Authorization: Bearer <token>` for API calls. The token format uses a
  `by_` prefix for secret scanning, and only the SHA-256 hash is stored.
  Deactivating a user immediately invalidates all their PATs. The static
  bearer token (`[server] token`) is removed.

- **User sessions.** After a successful OIDC callback, the server stores
  the user's access token and refresh token in a server-side session store
  (`sync.RWMutex`-protected `map[string]*UserSession` keyed by `sub`).
  Groups are not stored — they play no role in blockyard's authorization
  model. The browser receives a signed cookie carrying only the user's
  `sub` and `issued_at` (~100-150 bytes, HMAC-SHA256 signed). Access tokens
  have a short TTL (5–15 minutes, configured on the IdP). On each request,
  if the access token is near expiry the server transparently exchanges the
  refresh token for a new access token and updates the server-side session —
  the cookie is unchanged and the user never notices.

  Runtime state (which container belongs to which session) is kept in-memory.
  Logout deletes the server-side session entry and clears the cookie —
  session invalidation is immediate. Sessions are lost on server restart;
  users must re-authenticate. This matches all other in-memory state in v1
  (workers, proxy sessions, task store).

- **RBAC + per-content ACL.** Three system roles (`admin`, `publisher`,
  `viewer`) managed directly by blockyard admins on user records — not
  mapped from IdP groups. The IdP handles authentication (OIDC); blockyard
  handles authorization. Per-content ACLs are user-to-resource grants
  (no group-based grants): individual users are granted `owner`,
  `collaborator`, or `viewer` access to specific apps. Apps have an
  `access_type` that controls visibility: `acl` (only users with explicit
  grants), `logged_in` (any authenticated user), or `public` (anyone
  including anonymous/unauthenticated users).

- **Identity injection.** On each proxied request, inject the authenticated
  user's identity into the Shiny process via HTTP headers: `X-Shiny-User`
  (the user's OIDC `sub`) and `X-Shiny-Access` (the user's effective access
  level for the specific app: `owner`, `collaborator`, `viewer`, or
  `anonymous`). The access level is derived from blockyard's authorization
  model (system role + per-content ACL + app `access_type`). The Shiny app
  reads these headers to personalise content without implementing its own
  auth. For public apps accessed without authentication, `X-Shiny-User` is
  empty and `X-Shiny-Access` is `anonymous`.

- **Integration system (per-user credentials).** Allows each user to register
  credentials for external services (AI providers, S3, databases, etc.) once;
  these are made available to their Shiny sessions at runtime in a
  cryptographically bounded way.

  **Requires:** IdP (OIDC) and OpenBao — both v1 dependencies. See
  [Credential Trust Model](architecture.md#credential-trust-model) in the
  architecture doc for the security rationale behind these external
  dependencies.

  **Threat model:** Shiny apps run arbitrary R code. Any credential or token
  placed in the process space must be treated as potentially exfiltrable. The
  blast radius of a compromised session must be bounded to that user's secrets
  only — no path from the process to any other user's data or to the server's
  own DB credentials.

  **Mechanism — OpenBao + IdP JWT auth:**
  [OpenBao](https://openbao.org) (the open source Vault fork) is used as the
  secrets backend. OpenBao is bundled in the reference Docker Compose; operators
  who already run Vault or OpenBao can point the server at their own instance.
  OpenBao must be initialized and unsealed by the operator before the server
  starts — no auto-unseal; this is documented as a one-time setup step.

  The IdP and OpenBao are wired together via OpenBao's JWT auth method:
  OpenBao is configured with the IdP's JWKS endpoint once, after which any
  valid IdP JWT can be exchanged for a scoped OpenBao token. Per-user policies
  restrict each token to `read` on `secret/users/{sub}/*` only.

  **Session flow (single-tenant, `max_sessions_per_worker = 1`):**
  1. User authenticates via IdP → server receives their OIDC JWT
  2. On each proxied request, the **server** (not the R process) presents the
     user's access token to OpenBao's `/auth/jwt/login` endpoint (with
     in-memory caching keyed by `sub` to avoid per-request calls)
  3. OpenBao validates the JWT, maps the `sub` claim to a policy, and returns
     a short-lived token scoped to `secret/users/{sub}/*`
  4. The scoped OpenBao token is injected as the `X-Blockyard-Vault-Token`
     HTTP header on the proxied request. The OpenBao address is injected once
     at container startup as the `VAULT_ADDR` environment variable (recognized
     natively by Vault/OpenBao client libraries).
  5. The R process reads the token via `session$request` and calls OpenBao
     directly to read its credentials — it never touches the server's DB or
     decryption keys
  6. The server's OpenBao admin credentials (used for enrollment writes) never
     enter the process space

  **Session flow (shared containers, `max_sessions_per_worker > 1`):**

  Raw vault tokens in HTTP headers are not safe for shared containers —
  they could leak between co-tenant sessions if the app logs request
  headers or stores them in a shared variable. Instead, the proxy uses a
  two-phase exchange pattern (the same approach Posit Connect uses for
  OAuth Integrations):

  1. On each proxied request, the server injects
     `X-Blockyard-Session-Token` — a signed, short-lived token containing
     the user's `sub`, the app ID, and the worker ID.
  2. The R app exchanges the session token for a real vault credential
     by calling `POST /api/v1/credentials/vault` with the session token
     as a Bearer credential.
  3. The server validates the token (signature, expiry, worker existence),
     exchanges the user's identity for a scoped OpenBao token, and returns
     it to the app.

  The actual vault secret never crosses the proxy layer. The server also
  injects `BLOCKYARD_API_URL` as a container environment variable so the
  R process knows where to call the exchange endpoint.

  **Enrollment progression:**
  - **v1a (stopgap):** users write credentials directly via the OpenBao web UI
    at the correct paths (`secret/users/{sub}/apikeys/{service}` etc.).
  - **v1b:** `POST /users/me/credentials/{service}` on the blockyard REST
    API. Server validates identity via OIDC, writes to OpenBao on the user's
    behalf.
  - **v2:** blockyard web UI wraps the v1b API. Point-and-click credential
    management.

  The server authenticates to OpenBao via AppRole auth — a renewable,
  scoped token obtained at startup and maintained by a background renewal
  goroutine. The static `admin_token` is deprecated but still accepted
  for migration. See [v1 wrap-up §4](v1/wrap-up.md#4-secret-lifecycle)
  for the full design.

  **R interface:** no companion R package. A documented helper function
  transparently handles both deployment modes:
  - **Single-tenant** (`max_sessions_per_worker = 1`): reads the raw
    `X-Blockyard-Vault-Token` header directly.
  - **Shared containers** (`max_sessions_per_worker > 1`): reads
    `X-Blockyard-Session-Token`, exchanges it for a vault token via
    `POST /api/v1/credentials/vault`, and returns the token.

  The OpenBao address is available as the `VAULT_ADDR` environment
  variable (injected at container startup). App developers use the helper
  function and `httr2` to query OpenBao for their credentials.

- **Stable UUID URLs.** The proxy resolves `/app/{uuid}/` in addition to
  `/app/{name}/`, giving every app a stable URL that survives renames.
  Vanity URLs (top-level path aliases like `/sales-dashboard/`) were
  considered and dropped — app names are already human-readable, and the
  routing complexity wasn't justified for v1.

- **Content discovery.** A content catalog endpoint in the REST API listing all
  accessible items with metadata (title, type, owner, status, URL), a
  hierarchical tag system for organizing content (admin-managed), and basic
  search/filter support.

- **Audit logging.** Append-only log of all state-changing operations: who
  deployed what, when an app was started/stopped, config changes. JSON Lines
  format for easy ingestion into log aggregation tools.

- **Telemetry and observability.** Structured logging via `log/slog`,
  Prometheus-compatible metrics endpoint, and OpenTelemetry tracing. Metrics
  should cover active connections per app, request rates, worker lifecycle
  events (spawn, stop, crash), and health check results.

- **`/readyz` endpoint.** Readiness check. Returns `200 OK` only when all
  runtime dependencies are reachable — DB, Docker socket, IdP, OpenBao.
  Returns `503` with a JSON body listing which checks failed.

- **User-facing web UI (minimal).** Server-rendered HTML pages for browser
  users who navigate to the site directly. An unauthenticated landing page
  with a sign-in button (redirecting to `/login`), and an authenticated
  dashboard at `/` listing all apps the user has access to (consuming the
  same catalog queries as the content discovery API, filtered by RBAC).
  Rendered with Go's `html/template` and embedded via `embed.FS` — no
  JavaScript framework, no build step. Credential enrollment forms for
  operator-defined services (OpenBao) are inline on the dashboard. User
  management (admin UI for role assignment and activation/deactivation)
  and PAT management (create, list, and revoke tokens) are also provided.
  In v0 mode (no OIDC), the root page shows all deployed apps without auth.
  In-app navigation chrome (navbar, app switcher) is deferred to v2.

### v2: Single-Node Production Completeness

Usability improvements, safety nets, and blockr-specific features for the
existing Docker deployment. No Kubernetes dependency.

---

- **CLI tool.** A dedicated Go binary for interacting with the server: deploy
  apps, list content, tail logs, manage settings. Communicates via the REST
  API.

- **Bundle rollback.** Activate a previous bundle for a content item. Drain
  active sessions gracefully before switching.

- **Soft-delete for apps.** Mark apps as deleted instead of immediate removal;
  background cleanup purges after a retention period. Enables undo and audit
  trails.

- **Per-content resource limit enforcement.** CPU/memory caps via Docker
  `--memory` and `--cpus` flags (fields already carried in `WorkerSpec` from
  v0). Kubernetes enforcement comes for free in v3.

- **Scale-to-zero.** When an app has no active connections for a configurable
  idle period, stop its workers to free resources. On the next request, hold
  the connection and spin up a worker before forwarding.

- **Seat-based pre-warming.** Pre-start a standby worker per app so the first
  user doesn't incur cold-start latency. When a session claims the warm
  container, replace it with a fresh one.

- **Multiple execution environment images.** Per-app image selection; add an
  `image` field to app configuration that overrides the server-wide default.

- **Web UI expansion.** Content browser with search/filter and tag management,
  live-streaming log viewer per app. Lower priority — the API + CLI covers
  the same functionality.

- **Board storage.** Per-user board save/restore and sharing for blockr.
  The recommended backend is PostgreSQL with Row-Level Security, accessed
  via PostgREST using the user's existing OIDC JWT — no provisioning, no
  admin tokens, no blockyard involvement in the data path. Alternative
  backends (PocketBase, S3, Gitea) are supported via operator-provisioned
  credentials in OpenBao. See [board-storage.md](board-storage.md) for
  the full design.

- **Runtime package installation.** Allow users to install additional block
  packages during a live session via a server-level package store with
  per-worker hard-linked library views. See [v2 draft](v2/draft.md) for the
  full design.

### v3: Process Backend

A lightweight alternative to Docker for single-host deployments. Replaces
per-worker containers with bubblewrap-sandboxed processes — no daemon, no
socket, ~2ms startup overhead. See [backends.md](backends.md) for the full
design and isolation analysis.

---

- **Process backend implementation.** Implement the `Backend` interface
  using bubblewrap (`bwrap`) for process sandboxing. Workers are spawned
  as bwrap-sandboxed R processes with PID namespace isolation, filesystem
  isolation via bind mounts, seccomp filtering, and capability dropping.
  No per-worker network isolation or resource limits — these are deferred
  to the Docker and Kubernetes backends.

- **Containerized deployment mode.** A Docker image shipping blockyard, R,
  bwrap, and system libraries. Runs with a custom seccomp profile allowing
  user namespace creation — no Docker socket, no `CAP_SYS_ADMIN`. The
  recommended deployment mode for most users of the process backend.
  Provides rootfs containment: a bwrap sandbox escape lands in the outer
  container's filesystem, not the host.

- **Native deployment mode.** Documentation and tooling for running the
  process backend directly on a Linux host without an outer container.
  Operator provisions R, bwrap, and system libraries.

- **Custom seccomp profile.** A JSON seccomp profile based on Docker's
  default, adding `CLONE_NEWUSER` to the allowlist. Shipped alongside
  the Docker Compose configuration.

### v4: Kubernetes and Multi-Node

The Kubernetes backend and the architectural refactors it triggers.

---

- **Kubernetes backend.** Implement the `Backend` interface using
  `k8s.io/client-go` to create Deployments (long-lived apps) and Jobs (tasks).
  Involves pod specs, service creation for routing, PVC management for shared
  caches, and pod status polling.

- **Interface extraction.** `SessionStore`, `WorkerRegistry`, and `TaskStore`
  become interfaces with swappable implementations (in-memory for single-node,
  PostgreSQL-backed for multi-node HA).

- **Backend package extraction.** Extract separate Go packages for each backend
  implementation if build-tag sprawl warrants it.

- **Build image consolidation.** Kubernetes variant of the v2 rv-binary mount
  approach: init containers or shared volumes for build dependencies.

### Out of Scope

- **Task execution (run-to-completion).** Spawn an R script that runs once and
  exits. Not a Shiny use case. Add if a concrete need arises.

- **Cron scheduling.** Depends on task execution.

- **Static site serving.** Not a Shiny use case.

- **Git-backed deployment.** CI/CD via the REST API is more explicit and more
  flexible — set up a GitHub Actions workflow that calls
  `POST /api/v1/apps/{id}/bundles` on push.

- **Parameterized reports.** Not a Shiny use case.

- **Per-app environment variables.** Apps execute arbitrary user-supplied R
  code — any env var injected into the process is readable by that code, so
  per-app secrets do not provide a meaningful security boundary. Per-user
  credentials via OpenBao (v1) are the correct model.

- **Multi-language support.** The `Backend` interface is already
  runtime-agnostic, so adding a new language means adding a new deployment
  pipeline, not rearchitecting the core.

- **Rate limiting.** Per-IP rate limiting is applied at the router level
  using `go-chi/httprate`. Limits are grouped by endpoint sensitivity:
  auth endpoints (10 req/min), credential exchange (20 req/min), user
  token management (20 req/min), general API (120 req/min), and app
  proxy (200 req/min). Health probes (`/healthz`, `/readyz`) are
  unprotected. Operators may still apply additional limits at the network
  edge.

For architecture, deployment, database schema, and shutdown behavior, see
[architecture.md](architecture.md).
