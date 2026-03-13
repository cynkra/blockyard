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

- **Isolation:** blockr apps execute arbitrary user-supplied R code. Container
  isolation is required — there is no bare-metal process backend. The default
  is one container per session (`max_sessions_per_worker = 1`). See the Worker
  Scaling feature entry for details.

**Milestones:** v0 is the core infrastructure — no user auth on the app
plane. v1 is the MVP: the minimum needed to host a real blockr app for real
users. v1 adds user auth (OIDC), identity injection, per-user credential
management (the integration system), and load balancing. Nothing beyond v1 is
required to call the product useful.

**The one deliberate exception to "no premature abstraction"** is the `Backend`
interface (Docker vs. Kubernetes). This abstraction is worth its complexity
because: (a) it affects every other layer of the architecture and retrofitting
it later is expensive, (b) its shape is well-validated across multiple prior
projects, and (c) we know we will need Kubernetes before the project reaches
scale. The `Backend` interface is validated in tests using a lightweight
in-process mock — no bare-metal process spawner is needed. Everything else —
language dispatch, content-type routing tables, version matching — gets built
when there is a second use case to validate the abstraction's shape.

## Server Configuration

The server is configured via a TOML file. Every value can be overridden by an
environment variable, which takes precedence — this is the recommended approach
for secrets in container deployments. The env var name is the config key path
uppercased with `BLOCKYARD_` prefix and dots replaced by underscores (e.g.
`[server].token` → `BLOCKYARD_SERVER_TOKEN`).

**Config file location:** the server looks for `blockyard.toml` in the current
working directory by default. Override with `--config <path>`.

**Startup validation:** the server validates the full config (including env var
overrides) at startup and refuses to start on any error — missing required
fields, unreachable Docker socket, unreadable storage path, etc. Config changes
require a restart; there is no hot reload.

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "..."   # bearer token for control plane auth (v0)
                            # use BLOCKYARD_SERVER_TOKEN env var in production
shutdown_timeout = "30s"   # drain window on SIGTERM

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
```

**v1 additions** (OIDC + OpenBao arrive together — neither is meaningful
without the other):

```toml
[server]
# v1 additions to [server]:
session_secret = "..."                              # HMAC key for cookie signing
                                                    # use BLOCKYARD_SERVER_SESSION_SECRET env var
                                                    # required when [oidc] is configured
external_url   = "https://blockyard.example.com"    # public URL for OIDC redirect_uri and
                                                    # cookie Secure flag; required behind a
                                                    # reverse proxy

[oidc]
issuer_url    = "https://auth.example.com/realms/myrealm"
client_id     = "blockyard"
client_secret = "..."    # use BLOCKYARD_OIDC_CLIENT_SECRET env var
groups_claim  = "groups" # optional, default: "groups"
cookie_max_age = "24h"   # optional, default: 24h

[openbao]
address     = "https://bao.example.com"
admin_token = "..."      # use BLOCKYARD_OPENBAO_ADMIN_TOKEN env var
                         # operator initializes and unseals OpenBao manually
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
  deployments and the reference implementation for the interface.

  **Networking:** each spawned container gets its own freshly-created
  user-defined bridge network. When `max_sessions_per_worker = 1` the network
  is named `blockyard-{session-id}`; when sessions share a worker it is named
  `blockyard-{worker-id}`. The server joins that network to proxy traffic;
  no host port mapping is needed. The address is resolved by inspecting the
  container's IP on its specific named network — not just any IP the container
  has.

  **Why per-container bridges (design decision):** the requirements are that
  workers can reach the internet and local network services (package
  repositories, external APIs, OpenBao, IdP — which may themselves be
  containerized on the same host), workers cannot reach each other, the server
  can reach each worker to proxy traffic, and workers cannot reach the server's
  management API. The obvious alternative — a single shared bridge with
  `--icc=false` (inter-container communication disabled) — blocks *all*
  container-to-container traffic, including server-to-worker, since the server
  is itself a container in the Docker Compose deployment. Workarounds (server
  in `network_mode: host`, two-network topologies, published ports to
  localhost) each re-introduce the isolation problem in a different form and
  require compensating iptables rules or bind-address management. Per-container
  bridges give strong isolation by default without additional firewall rules.
  The operational overhead — network proliferation, server multi-homing,
  cleanup — is manageable in practice: Docker handles thousands of networks,
  Linux multi-homing is well-supported, and label-based cleanup is a single
  API filter call.

  **Labels:** all containers and networks spawned by blockyard carry
  identifying labels:

  ```
  dev.blockyard/managed    = "true"
  dev.blockyard/app-id     = "{app-id}"
  dev.blockyard/worker-id  = "{worker-id}"
  ```

  These labels are used for orphan cleanup, log streaming, health polling, and
  lifecycle management. On startup, the server queries Docker for both
  containers and networks carrying `dev.blockyard/managed=true` and removes
  any it has no active record for.

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

- **Network isolation.** Each app container runs in its own isolated Docker
  bridge network (see Docker backend entry). Containers on different bridge
  networks cannot reach each other — Docker's bridge isolation enforces this
  without additional iptables rules. The server joins each container's network
  solely to proxy traffic; no host port mapping is used. The server's
  management API binds only on a separate host/management interface and is not
  reachable from within app containers. At startup the server verifies that the
  host has an iptables rule blocking app container traffic to
  `169.254.169.254` (the cloud instance metadata endpoint) and refuses to
  start if it is missing on a cloud-detected host. Every spawned container has
  all Linux capabilities dropped (`--cap-drop=ALL`), privilege escalation
  disabled (`--security-opt=no-new-privileges`), and a read-only filesystem
  with a tmpfs at `/tmp`. The Docker socket is never mounted into app
  containers.

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
  container on Docker/Podman, an init container or Job on Kubernetes. Each
  backend is responsible for ensuring `rv` is available in its build
  environment. A shared cache avoids re-downloading packages on every deploy.
  The restored library is written to `{bundle-id}_lib/` alongside the unpacked
  bundle and mounted read-only into app workers at `/blockyard-lib`.
  The R version is configured server-wide — no per-deployment version selection
  or version matching logic.

  Restore output (stdout/stderr from `rv`) is streamed to the caller via the
  task log endpoint. The task lifecycle is managed by an in-memory task store.

- **Content registry.** A SQLite database with two tables:

  - `apps` — name, UUID, resource limits (`max_workers_per_app`,
    `max_sessions_per_worker`, `memory_limit`, `cpu_limit`), and active
    bundle ID. No persisted status column — app status (running/stopped) is
    inferred at runtime from whether any workers exist for the app.
  - `bundles` — per-app bundle history: tar.gz path, upload timestamp, bundle
    status (`pending | building | ready | failed`)

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
  network-level access control is acceptable.

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
  URL, client credentials, and an optional groups claim name (default:
  `"groups"`). Any compliant IdP works without IdP-specific code: Keycloak,
  Authentik, Auth0, Okta, Azure AD, Google Workspace.

- **IdP client credentials.** Replaces static token for machine-to-machine
  auth via the OAuth 2.0 client credentials flow. A CI/CD pipeline
  authenticates with a `client_id` + `client_secret` against the IdP's token
  endpoint and receives a short-lived JWT, validated against the IdP's JWKS
  endpoint — the same path used for human OIDC sessions. No API key storage
  in the database; token rotation, revocation, and expiry are handled by the
  IdP.

- **User sessions.** After a successful OIDC callback, the server stores
  the user's groups, access token, and refresh token in a server-side
  session store (`sync.RWMutex`-protected `map[string]*UserSession` keyed
  by `sub`). The browser receives a signed cookie carrying only the user's
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
  `viewer`) mapped from IdP groups. Per-content ACLs grant individual users
  or groups `viewer` or `collaborator` access to specific apps. Apps have an
  `access_type` (`acl` or `public`): public apps are accessible without
  authentication (the Anonymous level in Posit Connect's four-level model).
  Identity headers are injected when the user is authenticated, absent for
  anonymous access.

- **Identity injection.** On each proxied request, inject the authenticated
  user's identity into the Shiny process via HTTP headers (`X-Shiny-User`,
  `X-Shiny-Groups`). The Shiny app reads these headers to personalise content
  without implementing its own auth. For public apps the headers are absent.

- **Integration system (per-user credentials).** Allows each user to register
  credentials for external services (AI providers, S3, databases, etc.) once;
  these are made available to their Shiny sessions at runtime in a
  cryptographically bounded way.

  **Requires:** IdP (OIDC) and OpenBao — both v1 dependencies.

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

  The server authenticates to OpenBao with a static admin token supplied via
  env var (`BLOCKYARD_OPENBAO_ADMIN_TOKEN`). AppRole auth is deferred.

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

### v2

- **Kubernetes backend.** Implement the `Backend` interface using
  `k8s.io/client-go` to create Deployments (long-lived apps) and Jobs (tasks).
  Involves pod specs, service creation for routing, PVC management for shared
  caches, and pod status polling.

- **Bundle rollback.** Activate a previous bundle for a content item. Drain
  active sessions gracefully before switching.

- **Per-content resource limit enforcement.** CPU/memory caps via Docker / K8s
  (fields carried in `WorkerSpec` from v0).

- **CLI tool.** A dedicated Go binary for interacting with the server: deploy
  apps, list content, tail logs, manage settings. Communicates via the REST
  API.

- **Web UI.** Admin dashboard, content browser, log viewer; credential
  enrollment UI.

- **Multiple execution environment images.** Per-app image selection; operators
  or app developers specify which image to use per deployment.

- **Scale-to-zero.** When an app has no active connections for a configurable
  idle period, stop its workers to free resources. On the next request, hold
  the connection and spin up a worker before forwarding. Pair with
  pre-warming.

- **Seat-based pre-warming.** Pre-start a pool of containers before users
  arrive, so the first request doesn't incur cold-start latency.

- **Runtime package installation.** Allow apps to install R packages at runtime
  (writable library mount); explore use cases such as user-driven package
  experimentation.

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

- **Rate limiting.** No rate limiting is built in. When `max_workers` is
  reached the server returns 503 immediately. Operators rate-limit at the
  network edge.

## Architecture

### Backend Interface

The central abstraction. All container runtimes implement this interface. The
interface methods accept a `context.Context` for cancellation and timeout
propagation, take a `WorkerSpec` or `BuildSpec` describing what to run, and
return a plain string ID identifying the managed resource. Each backend
maintains its own internal state (container metadata, network IDs, etc.) keyed
by that ID — callers only see the string.

The build method maps to the native run-to-completion primitive on each
backend: a container with auto-remove on Docker/Podman, a Job on Kubernetes.
In v0 it runs `rv restore`; later it could run container image builds. Build
containers carry the same labels as workers and are covered by the same orphan
cleanup.

| Backend             | Client                            | Handle    | Priority | Purpose                |
|---------------------|-----------------------------------|-----------|----------|------------------------|
| `Docker` / `Podman` | `github.com/docker/docker/client` | Container ID | v0  | Single-host production |
| `Kubernetes`        | `k8s.io/client-go`               | Pod/Job name | v2  | Multi-node production  |

**Podman** exposes a Docker-compatible socket via `podman system service`.
The Docker Go client connects to it unchanged; the Docker backend works
without modification. Rootless Podman is the recommended mode for operators
who choose it.

### HTTP Stack

The control plane API is built with `chi` on top of Go's `net/http`. The
proxy layer uses `net/http` and `coder/websocket` for connection upgrades,
WebSocket forwarding, and streaming. `chi` implements `http.Handler` so
everything composes naturally with the standard library.

### Session and Worker Routing

A session store maps session IDs to worker IDs; a worker registry maps worker
IDs to network addresses. Both are concrete in-memory structs for v0 (map +
`sync.RWMutex`). Load balancing (v1) sits on top — it picks a worker and
writes the mapping; the stores just hold the data.

When v2 needs PostgreSQL-backed implementations for multi-node HA, extracting
an interface in Go is a low-cost refactor — define the interface at the call
site and the existing struct already satisfies it.

### Task Store

An in-memory task store manages restore tasks. It provides a create/subscribe
pattern: background restore goroutines write log lines; HTTP handlers read
buffered output and optionally follow live lines via a channel. Same interface
extraction story as session/worker routing for v2 HA.

### Network Isolation

App containers execute arbitrary user-supplied R code and must be isolated from
each other, from the server's management API, and from host-level network
services. Internet egress is permitted.

**Docker:** per-container bridge network. The server joins each network
(multi-homed) solely to proxy traffic. One host-level iptables rule blocks
`169.254.169.254` (cloud instance metadata); the server verifies this at
startup on cloud-detected hosts.

**Container hardening:** `--cap-drop=ALL`, `--security-opt=no-new-privileges`,
`--read-only` with tmpfs at `/tmp`, no Docker socket mount, default seccomp
profile.

**Kubernetes (v2):** `NetworkPolicy` per Pod denying all ingress except from
the server, restricting egress to internet-only. Requires a CNI plugin that
enforces NetworkPolicy (Calico or Cilium).

## Graceful Shutdown

On SIGTERM the server shuts down cleanly in this order:

1. **Stop accepting new connections** — close the HTTP listener
2. **Drain in-flight requests** — wait up to `shutdown_timeout` (default `30s`)
   for in-flight HTTP and WebSocket requests to finish; remaining connections
   are dropped
3. **Stop all managed containers and networks** — stop and remove every
   container and bridge network carrying `dev.blockyard/managed=true`;
   steps 3 and 4 run in parallel
4. **Stop in-progress build containers** — stop any running dependency restore
   containers and mark their bundles as `failed` in the DB
5. **Flush and close** — flush structured logs and audit log, close the DB
   connection

All active user sessions are killed on shutdown. This is intentional — a
server restart is a rare, disruptive operational event, not a rolling update.

No hot reload. Config changes require a restart.

### State on Restart

The server makes no attempt to recover or reconnect to containers after a
restart — clean or otherwise.

**Clean shutdown:** all containers and networks are stopped and removed before
exit. Next startup begins with an empty slate.

**Unclean shutdown** (crash, OOM kill, power loss): containers may still be
running on the host. Orphan cleanup on startup removes them. End state is the
same as a clean shutdown.

In both cases all active user sessions are lost. Simplicity over resilience.

## Database Schema

Two tables — everything else lives in OpenBao, the IdP, Docker, or in-memory.

**Storage backend:** SQLite (`modernc.org/sqlite`, pure Go) for single-host
Docker deployments — zero operational overhead and sufficient for the write
load (deploys and config changes, not per-request writes). The Kubernetes
backend (v2) will likely require PostgreSQL for HA multi-node deployments.
The database layer should be abstracted behind an interface when that need
arises.

```sql
CREATE TABLE apps (
    id                      TEXT PRIMARY KEY,      -- UUID, system-generated
    name                    TEXT NOT NULL UNIQUE,  -- user-supplied slug
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,                      -- max replicas; NULL = unlimited
    max_sessions_per_worker INTEGER NOT NULL DEFAULT 1,   -- max sessions per container; v0: always 1
    memory_limit            TEXT,                  -- e.g. "512m"
    cpu_limit               REAL,                  -- fractional vCPUs
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);
-- No status column: app status (running/stopped) is inferred at runtime
-- from whether any workers exist. No need to persist dynamic state.

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,     -- UUID
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL,        -- pending | building | ready | failed
    path        TEXT NOT NULL,        -- path to tar.gz on disk
    uploaded_at TEXT NOT NULL
);
```

`active_bundle` only ever references a `ready` bundle; enforced in application
logic. `max_workers_per_app` defaults to NULL (unlimited, capped by the global
`max_workers` ceiling). `max_sessions_per_worker` defaults to `1`; in v0 other
values are rejected.

**What lives elsewhere:**

| Concern | Where |
|---|---|
| Per-user credentials (OAuth tokens, API keys) | OpenBao (v1) |
| User identity, groups, auth tokens | IdP (v1) |
| Session state (sub, groups, access + refresh token) | Signed cookie (v1) |
| App status (running/stopped) | In-memory (inferred from worker existence) |
| Runtime worker state (container ID ↔ session) | In-memory |
| App logs | Docker log stream + persisted files |

### Not Built-In (v0)

- **TLS termination** — delegate to Caddy/nginx/Traefik. Evaluate built-in
  TLS via Go's `crypto/tls` + `autocert` for v1.

## Deployment

### Distribution

Two artifacts are shipped:

- **Native binary** — a single statically-linked Go binary (`CGO_ENABLED=0`).
  Suitable for operators who prefer to manage the process directly (systemd
  unit, etc.) or for development. No runtime dependencies beyond Docker and
  the R image.
- **Docker image** — the recommended production deployment. Uses the
  Docker-out-of-Docker (DooD) pattern: the server container is given access to
  the host Docker daemon via a mounted socket. Containers spawned for Shiny
  apps are siblings on the host, not children of the server container.

### Networking

In both deployment modes, workers must be reachable from the server over TCP.
For Docker, each worker gets its own per-container bridge network (see Network
Isolation). The server joins each network to proxy traffic, resolving the
worker's address via the backend (container IP + Shiny port).

The external TLS-terminating proxy (Caddy, nginx, Traefik) connects to the
server over the host network or a dedicated Docker network. The server only
speaks plain HTTP.

### Bundle Storage

Bundle archives, unpacked app directories, and restored R libraries must be
accessible to both the server (for writing during deploy) and to workers
(read-only at runtime). Two config values control the paths:

- **`bundle_server_path`** — where the server reads and writes bundles
  (e.g. `/data/bundles`).
- **`bundle_worker_path`** — where each worker sees its own bundle
  (e.g. `/app`). Read-only.

How the same underlying storage appears at both paths is an operator concern:
a named Docker volume in Docker Compose, a PVC in Kubernetes, or a shared
host path when the server runs as a native binary.

Each worker gets two read-only mounts:
- App code → `{bundle_worker_path}/` (e.g. `/app`)
- R library → `/blockyard-lib` (fixed path, `R_LIBS=/blockyard-lib`)

### Reference Docker Compose

A minimal single-host setup with Caddy for TLS:

```yaml
services:
  blockyard:
    image: ghcr.io/cynkra/blockyard:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - blockyard-bundles:/data/bundles
      - blockyard-db:/data/db
    environment:
      BLOCKYARD_SERVER_TOKEN: "${BLOCKYARD_SERVER_TOKEN}"
    networks:
      - blockyard-net

  caddy:
    image: caddy:latest
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - caddy-data:/data
    networks:
      - blockyard-net

volumes:
  blockyard-bundles:
  blockyard-db:
  caddy-data:

networks:
  blockyard-net:
```

```
# Caddyfile
blockyard.example.com {
    reverse_proxy blockyard:8080
}
```

### Kubernetes Deployment (v2)

The k8s deployment is a v2 milestone. Several constraints differ meaningfully
from the single-host Docker case.

**Our server** runs as a k8s Deployment, talking to the cluster API via
`k8s.io/client-go`. No Docker socket — the Kubernetes backend creates Pods and
Services via the k8s API instead.

**Bundle storage** — the Docker named-volume approach does not translate to
k8s. Options, in order of preference:

- **ReadWriteMany PVC (default)** — a PersistentVolumeClaim with
  `ReadWriteMany` access mode (NFS, AWS EFS, CephFS, etc.) mounted into both
  the server Pod and each app Pod.
- **Object storage (alternative)** — bundles uploaded to S3/MinIO; app Pods
  pull the bundle at startup via an init container.
- **Image-baking (out of scope)** — build a container image per bundle at
  deploy time (Kaniko or similar).

**Database** — SQLite's single-writer model is incompatible with multi-replica
deployments. The k8s deployment uses PostgreSQL.

**In-memory state** — the worker map moves to PostgreSQL for HA, with a local
read-through cache.

**TLS and ingress** — cert-manager handles certificates. An Ingress resource
routes external traffic to the server's ClusterIP Service. The server proxies
onward to app Pod IPs resolved via the k8s API.

**Distribution** — a Helm chart is the primary artifact. Covers server
Deployment, RBAC, PVC, PostgreSQL dependency, and Ingress/cert-manager
integration.
