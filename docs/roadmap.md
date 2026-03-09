# blockr.cloud Roadmap

## Scope

`blockr.cloud` is focused on hosting **blockr Shiny applications**. This
deliberately narrows the scope relative to general-purpose platforms like Posit
Connect:

- **Content type:** Shiny apps only. Plumber APIs, static sites, rendered
  documents, scheduled tasks, and parameterized reports are out of scope for
  now. The code should be factored into clear modules so adding a new content
  type later means adding a new module, not refactoring existing ones.

- **Runtime:** A single R version, configured server-wide. Per-deployment
  version selection, multi-version side-by-side installations, and version
  matching strategies add meaningful complexity for a use case we don't yet
  have. Add when there is a concrete need.

- **Language:** R only. Python, Julia, and multi-language dependency restore
  pipelines are out of scope. The `Backend` trait is agnostic to what runs
  inside a container, so adding a new language later is a matter of adding a
  new deployment pipeline, not changing the core architecture.

- **Isolation:** blockr apps execute arbitrary user-supplied R code. Container
  isolation is required — there is no bare-metal process backend. The default
  is one container per session (`max_sessions_per_worker = 1`). See the Worker
  Scaling feature entry for details.

**Milestones:** v0 is the first working technical milestone — core
infrastructure, no user auth on the app plane. v1 is the MVP: the minimum
needed to host a real blockr app for real users. v1 adds user auth (OIDC),
identity injection, per-user credential management (the integration system),
and load balancing. Nothing in "later" or beyond is required to call the
product useful.

**The one deliberate exception to "no premature abstraction"** is the `Backend`
trait (Docker vs. Kubernetes). This abstraction is worth its complexity
because: (a) it affects every other layer of the architecture and retrofitting
it later is expensive, (b) its shape is well-validated across multiple prior
projects, and (c) we know we will need Kubernetes before the project reaches
scale. The `Backend` trait is validated in tests using a lightweight in-process
mock — no bare-metal process spawner is needed. Everything else — language
dispatch, content-type routing tables, version matching — gets built when there
is a second use case to validate the abstraction's shape.

## Server Configuration

The server is configured via a TOML file. Every value can be overridden by an
environment variable, which takes precedence — this is the recommended approach
for secrets in container deployments. The env var name is the config key path
uppercased with `BLOCKR_` prefix and dots replaced by underscores (e.g.
`[server].token` → `BLOCKR_SERVER_TOKEN`).

**Config file location:** the server looks for `blockr.toml` in the current
working directory by default. Override with `--config <path>`.

**Startup validation:** the server validates the full config (including env var
overrides) at startup and refuses to start on any error — missing required
fields, unreachable Docker socket, unreadable storage path, etc. Config changes
require a restart; there is no hot reload.

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "..."   # bearer token for control plane auth (v0)
                            # use BLOCKR_SERVER_TOKEN env var in production
shutdown_timeout = "30s"   # drain window on SIGTERM

[docker]
socket     = "/var/run/docker.sock"  # or Podman socket path
image      = "ghcr.io/blockr-org/blockr-r-base:latest"
shiny_port = 3838                    # internal port Shiny listens on

[storage]
bundle_server_path = "/data/bundles"   # where the server reads/writes bundles
bundle_worker_path = "/app"            # where each worker sees its bundle (read-only)
bundle_retention   = 50                # max bundles retained per app; oldest non-active deleted first

[database]
path = "/data/db/blockr.db"

[proxy]
ws_cache_ttl            = "60s"   # how long to hold backend WS on client disconnect
health_interval         = "10s"   # how often to poll worker health
worker_start_timeout    = "60s"   # how long to hold a request while a worker starts
max_workers             = 100     # hard ceiling on total running workers across all apps
```

**v1 additions** (OIDC + OpenBao arrive together — neither is meaningful
without the other):

```toml
[oidc]
issuer_url    = "https://auth.example.com/realms/myrealm"
client_id     = "blockr-cloud"
client_secret = "..."    # use BLOCKR_OIDC_CLIENT_SECRET env var
groups_claim  = "groups" # optional, default: "groups"

[openbao]
address     = "https://bao.example.com"
admin_token = "..."      # use BLOCKR_OPENBAO_ADMIN_TOKEN env var
                         # operator initializes and unseals OpenBao manually
```

## Feature Inventory

Each feature is described below with a priority annotation:

- **v0** — must-have for the first working version (core infrastructure)
- **v1 / MVP** — required to host a real blockr app for real users
- **v2** — important but not near-term
- **out of scope** — not planned; add if a concrete use case arises

---

- **Backend trait abstraction.** A pluggable interface (`Backend` trait) that
  lets the server manage workers without knowing whether they are Docker
  containers or Kubernetes pods. This is the architectural foundation — every
  other feature depends on it. Includes the trait definition, `WorkerSpec`
  (what to run), and `WorkerHandle` (opaque reference to a running worker). The
  trait is agnostic to what runs inside the worker — it deals only with *where*
  and *how* containers are launched, not *what* they run. Validated in tests
  using a lightweight in-process mock backend; no bare-metal process spawner is
  needed.
  **Priority: v0.** Foundational — build first.

- **Docker / Podman backend.** Implement the `Backend` trait using the `bollard`
  crate to create and manage containers. Covers image pulling, container
  creation, health checking, log streaming, and cleanup. The only production
  backend for single-host deployments and the reference implementation for the
  trait.

  **Networking:** each spawned container gets its own freshly-created
  user-defined bridge network. When `max_sessions_per_worker = 1` the network
  is named `blockr-{session-id}`; when sessions share a worker it is named
  `blockr-{worker-id}`. The server joins that network to proxy traffic;
  no host port mapping is needed. `backend.addr(handle)` returns the
  container's bridge IP and the Shiny port (configured as `[docker] shiny_port`,
  default `3838`). The address is resolved by inspecting the container's IP on
  its specific named network — not just any IP the container has.

  **Why per-container bridges (design decision):** the requirements are that
  workers can reach the internet and local network services (package
  repositories, external APIs, OpenBao, IdP — which may themselves be
  containerized on the same host), workers cannot reach each other, the server
  can reach each worker to
  proxy traffic, and workers cannot reach the server's management API. The
  obvious alternative — a single shared bridge with `--icc=false`
  (inter-container communication disabled) — blocks *all* container-to-container
  traffic, including server-to-worker, since the server is itself a container in
  the Docker Compose deployment. Workarounds (server in `network_mode: host`,
  two-network topologies, published ports to localhost) each re-introduce the
  isolation problem in a different form and require compensating iptables rules
  or bind-address management. Per-container bridges give strong isolation by
  default without additional firewall rules. The operational overhead — network
  proliferation, server multi-homing, cleanup — is manageable in practice:
  Docker handles thousands of networks, Linux multi-homing is well-supported,
  and label-based cleanup is a single API filter call.

  **Labels:** all containers and networks spawned by blockr.cloud carry
  identifying labels:

  ```
  dev.blockr.cloud/managed    = "true"
  dev.blockr.cloud/app-id     = "{app-id}"
  dev.blockr.cloud/worker-id  = "{worker-id}"
  ```

  These labels are used for orphan cleanup, log streaming, health polling, and
  lifecycle management. On startup, the server queries Docker for both
  containers and networks carrying `dev.blockr.cloud/managed=true` and removes
  any it has no active record for.
  **Priority: v0.** Required — there is no other production backend.

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
  from v0 but defaults to unlimited. In v1, OpenBao tokens are injected per
  worker at spawn time; with `max_sessions_per_worker = 1` each container gets
  its own scoped token, automatically invalidated when the container is
  destroyed.
  **Priority: v0** (schema + 1 session per worker) **/ v1** (session sharing,
  load balancing, auto-scaling).

- **Kubernetes backend.** Implement the `Backend` trait using `kube-rs` to
  create Deployments (long-lived apps) and Jobs (tasks). Involves pod specs,
  service creation for routing, PVC management for shared caches, and pod
  status polling. Production backend for multi-node deployments.
  **Priority: v2.** Once Docker backend is stable, K8s is a separate
  milestone.

- **HTTP / WebSocket reverse proxy.** Accept incoming HTTP and WebSocket
  connections and forward them to the correct backend worker based on URL
  routing. Must handle connection upgrades (HTTP → WS), set `X-Forwarded-*`
  headers, and support multiple apps on different URL prefixes. faucet's
  `pool.rs` and `websockets.rs` are direct references.

  **URL scheme:**

  ```
  /api/v1/...      → control plane REST API
  /app/{name}/     → proxied Shiny app (name-based, v0)
  /{vanity}/       → vanity URL alias resolving to an app (v1)
  ```

  Apps are routed by name. The proxy redirects `/app/{name}` (no trailing
  slash) to `/app/{name}/` and strips the prefix before forwarding to the
  container — Shiny requires a trailing-slash prefix for relative asset URLs
  to resolve correctly.
  **Priority: v0.** Can't serve apps without it.

- **Rate limiting.** No rate limiting is built into blockr.cloud. When
  `max_workers` is reached the server returns 503 immediately — queuing
  requests only delays the inevitable for users who are better served by a
  fast error. Operators are responsible for rate limiting at the network edge
  (Caddy, nginx, Cloudflare, etc.) — this is the correct place for it in a
  deployment that already delegates TLS termination upstream. In v1, when apps
  become user-facing with OIDC, a per-user connection rate limit on the proxy
  is worth revisiting.
  **Priority: out of scope for v0; revisit for v1.**

- **Cold-start UX.** When a new session requires a new container (i.e. no
  existing worker has capacity), the proxy holds the initial HTTP request open
  until the container passes its health check, then forwards it — the user sees
  the browser's native loading indicator while R starts. No custom loading page
  is served; the browser handles the wait. If the container does not become
  healthy within `worker_start_timeout` (default `60s`), the held request is
  released with a 503. A custom loading page is a v2 polish item.
  **Priority: v0.** Behaviour must be defined from the start; the alternative
  (timing out immediately) is unacceptable.

- **Active health polling.** After a worker starts, periodically poll its
  endpoint (TCP connect or lightweight HTTP probe) to detect hung processes.
  Shiny Server only probes at startup; a hung R process can hold a worker slot
  indefinitely. On failure, mark the worker unhealthy, stop it via the backend,
  and (if auto-scaling is enabled) spawn a replacement.
  **Priority: v0.** Without this, hung processes silently swallow traffic.

- **Load balancing.** Distribute incoming sessions across multiple workers when
  `max_workers_per_app > 1`. Shiny requires cookie-hash sticky sessions —
  sessions are stateful and tied to a specific R process; once a session is
  assigned to a worker it stays there. faucet's `LoadBalancingStrategy` trait
  is a good model.
  **Priority: v1 / MVP.** In v0, `max_sessions_per_worker = 1` means each
  session has its own worker — no load balancing needed. Load balancing and
  auto-scaling are a package deal added together in v1.

- **WebSocket session caching.** When a browser briefly disconnects (page
  reload, network glitch), hold the backend WebSocket connection open for a
  grace period so the client can reconnect to the same session. faucet
  implements this with a 60s cache. Critical for Shiny apps where session state
  lives in the R process. When `max_sessions_per_worker = 1`, this also means
  keeping the container alive during the grace period — `backend.stop()` is
  not called until the grace period expires without a reconnect.
  **Priority: v0.** Session loss on reload is unacceptable for Shiny users.

- **Auto-scaling.** Monitor active connections or request rate per app and
  dynamically call `backend.spawn()` / `backend.stop()` to add or remove
  workers within the `max_workers_per_app` bound. faucet's RPS-based
  autoscaler and ShinyProxy's seat-based scaler are references.
  **Priority: v1 / MVP.** Comes alongside load balancing — they are a
  package deal.

- **Scale-to-zero.** When an app has no active connections for a configurable
  idle period, stop its workers to free resources. On the next incoming
  request, hold the connection and spin up a worker before forwarding. Idle
  detection should use reference-counted connection tracking (HTTP connections,
  WebSocket connections, and pending connections counted separately) — inspired
  by Shiny Server's `httpConn` / `sockConn` / `pendingConn` model.
  **Priority: v2.** Pair with pre-warming.

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
  in a build container. On success the bundle transitions to `ready` and is
  activated (set as `active_bundle` on the app). On failure the bundle
  transitions to `failed` and the previous active bundle remains unchanged.
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
  - The restored R package library → `{bundle_worker_path}/lib` (read-only)

  Both are mounted read-only — app code cannot modify its own source or
  installed packages at runtime.

  **Priority: v0.** Core deployment mechanism.

- **Bundle rollback.** Activate a previous bundle for a content item. Drain
  active sessions gracefully before switching — don't kill users mid-session.
  Posit Connect's bundle activation model is the reference.
  **Priority: v2.** In v0 a bad deploy can be fixed by redeploying the
  previous code, which goes through the same path as a fresh deploy.

- **Dependency restoration.** After uploading a bundle, restore R package
  dependencies from `rv.lock` using [`rv`](https://github.com/A2-ai/rv).
  `rv` is a hard runtime requirement. Restore runs via `backend.build()` —
  how the build step executes is backend-specific: a run-to-completion
  container on Docker/Podman, an init container or Job on Kubernetes, a local
  process when the server runs as a native binary. Each backend is responsible
  for ensuring `rv` is available in its build environment. A shared cache
  avoids re-downloading packages on every deploy. The restored library is
  written to `{bundle-id}_lib/` alongside the unpacked bundle and mounted
  read-only into app workers at `{bundle_worker_path}/lib`. The R version is
  configured server-wide — no per-deployment version selection or version
  matching logic.

  Restore output (stdout/stderr from `rv`) is streamed to the caller via the
  task log endpoint. The task lifecycle is managed by a `TaskStore` — an
  in-memory implementation for v0, with a PostgreSQL-backed implementation
  available for HA k8s deployments (v2). Both implement the same `TaskStore`
  trait so the task and API logic are unaffected by the swap.
  **Priority: v0.** Tightly coupled with bundle upload — can't deploy without
  restoring deps.

- **Content registry.** A SQLite database with two tables:

  - `apps` — name, UUID, status (running/stopped/failed), resource limits
    (`max_workers_per_app`, `max_sessions_per_worker`, `memory_limit`,
    `cpu_limit`), and active bundle ID
  - `bundles` — per-app bundle history: tar.gz path, upload timestamp, bundle
    status (`pending | ready | failed`)

  Runtime worker state (container ID → session mapping) is in-memory. SQLite
  stores only what must survive a server restart. Credentials (v1), user
  identity (v1), and session state (v1) live outside the DB — see the "What
  lives elsewhere" table in the Database Schema section.
  **Priority: v0.** Need to track what's deployed and its state.

- **REST API.** HTTP endpoints for all server operations: deploy apps, list
  apps, start/stop apps, manage settings, view logs. This is the primary
  interface — the CLI and (eventually) the web UI are clients of this API.
  Think of it as the control plane.

  All endpoints are prefixed with `/api/v1/`. The version is the API contract
  version, independent of the product milestone — it starts at `v1` from day
  one and increments only on breaking changes. No version negotiation; the
  version is in the path.

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
  **Priority: v0.** Primary server interface.

- **Health endpoints.** Unauthenticated endpoints for process monitoring:

  - **`GET /healthz` (liveness, v0):** returns `200 OK` whenever the server
    process is running. No dependency checks. Used for Docker health checks
    and simple uptime monitoring.
  - **`GET /readyz` (readiness, v1):** returns `200 OK` only when all runtime
    dependencies are reachable — DB, Docker socket, IdP, OpenBao. Returns
    `503` with a JSON body listing which checks failed. Useful for Kubernetes
    readiness probes and signalling that the server is not yet ready to serve
    traffic after startup.

  Both endpoints are excluded from bearer token authentication.
  **Priority: v0** (`/healthz`) **/ v1** (`/readyz`).

- **Task execution (run-to-completion).** Spawn an R script that runs once and
  exits. Capture stdout/stderr and exit code, store results. Used for ETL jobs,
  report rendering, data processing.
  **Priority: out of scope.** Shiny apps are the only content type for now.

- **Cron scheduling.** Trigger task execution on a schedule.
  **Priority: out of scope.** Depends on task execution, which is itself out of
  scope.

- **Static site serving.** Serve rendered Rmd/Quarto output as static HTML.
  **Priority: out of scope.** Not a Shiny use case.

- **Git-backed deployment.** Out of scope. CI/CD via the REST API is more
  explicit and more flexible — set up a GitHub Actions workflow that calls
  `POST /api/v1/apps/{id}/bundles` on push. The server has no need to poll
  repositories.

- **Per-content resource limits.** Enforce CPU and memory limits per content
  item (`max_workers_per_app`, `memory_limit`, `cpu_limit`). In the Docker backend,
  these map to container resource constraints. In the Kubernetes backend, they
  map to pod resource requests/limits. Resource limits are stored in the content
  registry and carried in `WorkerSpec` from v0 so the schema does not change
  when enforcement is added; actual enforcement is backend-specific and added
  when Docker/K8s backends are stable.
  **Priority: design now, enforce later.**

- **Parameterized reports.** Support Quarto and R Markdown documents with
  user-supplied parameters, named variants, and per-variant schedules.
  **Priority: out of scope.** Not a Shiny use case.

- **Execution environment images.** A single server-wide Docker image
  configured in `[docker] image`. We maintain a Rocker-based image with R +
  required system libraries. The image is pulled on server startup and on every
  bundle deploy — never at container spawn time (which would add latency to
  every session start). Image selection and pinning are an operational concern
  managed centrally, not by the server or app developers.
  **Priority: v0.** The server ships with a reference image; maintaining and
  publishing it is a separate but parallel concern.

- **Control plane authentication.** Two mechanisms, by milestone:

  **v0 — static token:** A single bearer token configured in the server config
  file. No database storage, no issuance logic. Sufficient for development and
  single-operator deployments where network-level access control is acceptable.

  **v1 / MVP — IdP client credentials:** Machine-to-machine auth via the OAuth
  2.0 client credentials flow. A CI/CD pipeline or automation script
  authenticates with a `client_id` + `client_secret` against the IdP's token
  endpoint and receives a short-lived JWT. Tokens have a short TTL; clients
  are responsible for re-authenticating before expiry — standard practice for
  any OAuth2 client library. The JWT is presented as a Bearer token on the
  REST API and validated against the IdP's JWKS endpoint — the same path used
  for human OIDC sessions. No API key storage in the database; token
  rotation, revocation, and expiry are handled by the IdP.
  **Priority: v0** (static token) **/ v1 / MVP** (IdP client credentials).

- **OIDC authentication.** Delegate user authentication to an external identity
  provider via OpenID Connect. The interface is fully standardized — we
  implement against OIDC Discovery (`{issuer}/.well-known/openid-configuration`
  ), which auto-discovers all endpoints (authorization, token, userinfo, JWKS,
  logout). The only configuration required is the issuer URL, client
  credentials, and an optional groups claim name (the one thing that varies
  across IdPs; default: `"groups"`). Any compliant IdP works without
  IdP-specific code: Keycloak, Authentik, Auth0, Okta, Azure AD, Google
  Workspace. OpenBao's JWT auth method is wired to the same JWKS URI from the
  discovery document, so IdP swapping also requires no OpenBao reconfiguration
  beyond the issuer URL.

  Minimal configuration:
  ```toml
  [oidc]
  issuer_url    = "https://auth.example.com/realms/myrealm"
  client_id     = "blockr-cloud"
  client_secret = "..."
  groups_claim  = "groups"  # optional, default: "groups"
  ```
  **Priority: v1 / MVP.**

- **Role-based access control (RBAC).** Define roles (e.g. admin, developer,
  viewer) with different permissions. Optionally, per-content ACLs so specific
  apps or tasks are visible only to certain users/groups. ShinyProxy does this
  with `access-groups` and SpEL expressions. Posit Connect's four-level model
  (Anonymous, Viewer, Publisher, Administrator) with per-content Viewer /
  Collaborator grants is the reference.
  **Priority: v1 / MVP.** Comes alongside OIDC — once there are multiple
  authenticated users, you need roles and per-content access.

- **User sessions.** After a successful OIDC callback, the server issues a
  signed cookie containing the user's `sub`, groups, access token, and
  encrypted refresh token. Access tokens have a short TTL (5–15 minutes,
  configured on the IdP). On each request, if the access token is near expiry
  the server transparently exchanges the refresh token for a new access token
  and re-issues the cookie — the user never notices. The cookie carries
  everything; no database lookup is required.

  Runtime state (which container belongs to which session) is kept in-memory.
  Logout deletes the cookie client-side; the access token remains valid until
  its natural expiry (5–15 minutes, configured on the IdP). No server-side
  token revocation list is maintained — the short TTL makes it unnecessary.
  **Priority: v1 / MVP.** Prerequisite for everything user-aware.

- **Identity injection.** On each proxied request, inject the authenticated
  user's identity into the Shiny process via HTTP headers (`X-Shiny-User`,
  `X-Shiny-Groups`). The Shiny app reads these headers to personalise content
  without implementing its own auth. For public apps the headers are absent.
  **Priority: v1 / MVP.** Required for user-aware blockr apps.

- **Integration system (per-user credentials).** Allows each user to register
  credentials for external services (AI providers, S3, databases, etc.) once;
  these are made available to their Shiny sessions at runtime in a
  cryptographically bounded way.

  **Requires:** IdP (OIDC) and OpenBao — both v1 dependencies. Not present in
  v0.

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

  **Session flow:**
  1. User authenticates via IdP → server receives their OIDC JWT
  2. At session start, the **server** (not the R process) presents the JWT to
     OpenBao's `/auth/jwt/login` endpoint
  3. OpenBao validates the JWT, maps the `sub` claim to a policy, and returns
     a short-lived token scoped to `secret/users/{sub}/*`
  4. The scoped OpenBao token is injected into the Shiny process as an
     environment variable
  5. The R process calls OpenBao directly to read its credentials — it never
     touches the server's DB or decryption keys
  6. The server's OpenBao admin credentials (used for enrollment writes) never
     enter the process space

  **Enrollment progression:**
  - **v1a (stopgap):** users write credentials directly via the OpenBao web UI
    at the correct paths (`secret/users/{sub}/apikeys/{service}` etc.). Rough
    but functional for early adopters; requires users to know their `sub`.
  - **v1b:** `POST /users/me/credentials/{service}` on the blockr.cloud REST
    API. Server validates identity via OIDC, writes to OpenBao on the user's
    behalf. Two credential types: API key/secret and OAuth delegation (server
    stores refresh token after provider OAuth flow).
  - **v2:** blockr.cloud web UI wraps the v1b API. Point-and-click credential
    management.

  The server authenticates to OpenBao with a static admin token supplied via
  env var (`BLOCKR_OPENBAO_ADMIN_TOKEN`). AppRole auth is deferred to later.

  **Token TTL and renewal:** OpenBao session tokens are issued with a short
  TTL. The R process renews its token before expiry using OpenBao's standard
  token renewal API — a plain HTTP call app developers make directly.

  **R interface:** no companion R package. App developers query OpenBao
  directly using `httr2` (or similar). The server injects two env vars into
  every session container:
  - `BLOCKR_VAULT_TOKEN` — the scoped OpenBao token for this session
  - `BLOCKR_VAULT_ADDR` — the OpenBao address reachable from inside the
    container

  Reading a secret is a standard OpenBao KV v2 GET request to
  `{BLOCKR_VAULT_ADDR}/v1/secret/data/users/{sub}/apikeys/{service}` with
  `Authorization: Bearer {BLOCKR_VAULT_TOKEN}`. This is documented as a
  how-to; no package abstraction is needed.

  **Priority: v1 / MVP.** Without this, blockr apps cannot securely integrate
  with external services on a per-user basis.

- **Vanity URLs.** Allow publishers to assign a custom URL path (e.g.
  `/sales-dashboard`) to a content item, in addition to its base `/app/{name}/`
  URL. The router resolves vanity paths before falling back to name-based
  routing. Requires collision detection and a reserved-prefix blocklist
  (e.g. `/api`, `/app`, `/login`).
  **Priority: v1 / MVP.** Low implementation cost, high discoverability
  value.

- **Content discovery.** A way for users to find and navigate to deployed
  content: a content catalog endpoint in the REST API listing all accessible
  items with metadata (title, type, owner, status, URL), a hierarchical tag
  system for organizing content (admin-managed), and basic search/filter
  support. Needed even before a web UI — the API consumer (CLI or UI) needs
  something to work with.
  **Priority: v1 / MVP.** Without discovery, the platform is a black box.

- **Per-app environment variables.** Out of scope. Apps execute arbitrary
  user-supplied R code — any env var injected into the process is readable by
  that code, so per-app secrets do not provide a meaningful security boundary.
  Per-user credentials via OpenBao (v1) are the correct model: each user's
  secrets are cryptographically scoped to their session only.

- **App log capture.** Capture stdout/stderr from each container and make it
  available via the REST API (`GET /api/v1/apps/{id}/logs`). Logs must be persisted
  for a configurable period after a container exits so crashes can be diagnosed
  after the fact. Captured via
  Docker's log streaming API using the container's `dev.blockr.cloud/app-id`
  and `dev.blockr.cloud/worker-id` labels.
  **Priority: v0.** Required to debug anything during development and
  operation.

- **Audit logging.** Append-only log of all state-changing operations: who
  deployed what, when an app was started/stopped, config changes. JSON Lines
  format for easy ingestion into log aggregation tools.
  **Priority: v1 / MVP.** Standard server logs suffice for v0; audit trail
  becomes important once real users are deploying and accessing apps.

- **Orphan cleanup.** On startup, query Docker for containers and networks
  labeled `dev.blockr.cloud/managed=true` and remove any the server has no
  active record for. Prevents resource leaks accumulating across server
  restarts. Orphaned containers are simply removed — there is no session
  state to resume.
  **Priority: v0.** Without this, restarts leak containers and networks.

- **Network isolation.** Each app container runs in its own isolated Docker
  bridge network (see Docker backend entry for naming and labeling). Containers
  on different bridge networks cannot reach each other — Docker's bridge
  isolation enforces this without additional iptables rules. The server joins
  each container's network solely to proxy traffic; no host port mapping is
  used. The server's management API binds only on a separate host/management
  interface and is not reachable from within app containers. At startup the
  server verifies that the host has an iptables rule blocking app container
  traffic to `169.254.169.254` (the cloud instance metadata endpoint) and
  refuses to start if it is missing on a cloud-detected host. Every spawned
  container has all Linux capabilities dropped (`--cap-drop=ALL`), privilege
  escalation disabled (`--security-opt=no-new-privileges`), and a read-only
  filesystem with a tmpfs at `/tmp`. The Docker socket is never mounted into
  app containers. In Kubernetes, a `NetworkPolicy` per Pod enforces the
  equivalent rules; a CNI plugin that supports NetworkPolicy (Calico or Cilium)
  is required.
  **Priority: v0.** Arbitrary user code runs in these containers — isolation
  must be correct from the first deployment.

- **CLI tool.** A separate command-line binary (Rust) for interacting with the
  server: deploy apps, list content, tail logs, invoke tasks, manage settings.
  Communicates with the server via the REST API. ricochet's CLI is the closest
  reference.
  **Priority: v2.** curl/httpie against the REST API is sufficient initially.

- **Web UI.** A browser-based interface for browsing deployed content, viewing
  logs, managing settings, and (for admins) managing users. Lower priority than
  the API/CLI but important for discoverability and non-developer users.
  **Priority: v2.** Developers are the primary users initially.

- **Multi-language support.** Python, Julia, or any runtime beyond R. The
  `Backend` trait is already runtime-agnostic, so adding a new language means
  adding a new deployment pipeline, not rearchitecting the core.
  **Priority: out of scope.** Add if a concrete use case arises.

- **Seat-based pre-warming.** Pre-start a pool of containers before users
  arrive, so the first request doesn't incur cold-start latency. ShinyProxy
  supports this with `minimum-seats-available`. Useful for apps with slow
  startup times but adds resource cost.
  **Priority: v2.** Pair with scale-to-zero.

- **Telemetry and observability.** Structured logging, Prometheus-compatible
  metrics endpoint, and OpenTelemetry tracing. Metrics should cover active
  connections per app, request rates, worker lifecycle events (spawn, stop,
  crash), and health check results. Posit Connect ships Prometheus
  + OTel as first-class features (adding OTel in 2026); retrofitting
  observability onto a production system is painful. The `tracing` and
  `metrics` crates in the Rust ecosystem make this relatively cheap to add
  early.
  **Priority: v1 / MVP.** Cheap to instrument early, expensive to retrofit.

- **TLS termination.** Serving HTTPS. Either built-in (via `rustls` + ACME) or
  delegated to an external reverse proxy (Caddy, nginx, Traefik). The external
  proxy model is standard in container deployments and avoids maintaining TLS
  code in the server itself.
  **Priority: external proxy, not built-in.** Delegate TLS to Caddy/nginx/
  Traefik. The server only speaks HTTP. No built-in TLS planned.

## Graceful Shutdown

On SIGTERM the server shuts down cleanly in this order:

1. **Stop accepting new connections** — close the HTTP listener
2. **Drain in-flight requests** — wait up to `shutdown_timeout` (default `30s`,
   configurable via `[server] shutdown_timeout`) for in-flight HTTP and
   WebSocket requests to finish; remaining connections are dropped
3. **Stop all managed containers and networks** — stop and remove every
   container and bridge network carrying `dev.blockr.cloud/managed=true`;
   steps 3 and 4 run in parallel
4. **Stop in-progress build containers** — stop any running dependency restore
   containers and mark their bundles as `failed` in the DB; a re-deploy will
   restart the restore from scratch
5. **Flush and close** — flush structured logs and audit log, close the DB
   connection

All active user sessions are killed on shutdown. This is intentional — a
server restart is a rare, disruptive operational event, not a rolling update.
The clean shutdown means the next startup begins with no orphaned containers
or networks.

No hot reload. Config changes require a restart.

### State on Restart

The server makes no attempt to recover or reconnect to containers after a
restart — clean or otherwise.

**Clean shutdown:** all containers and networks are stopped and removed before
exit. Next startup begins with an empty slate.

**Unclean shutdown** (crash, OOM kill, power loss): containers may still be
running on the host. Orphan cleanup on startup removes them. End state is the
same as a clean shutdown.

In both cases all active user sessions are lost. This is intentional —
simplicity over resilience. A crashed server is already broken from the user's
perspective; attempting partial session recovery adds complexity with little
real benefit.

**Token revocation:** not implemented. Logout deletes the cookie; the access
token expires naturally within one TTL (5–15 minutes). No revocation list.

## Proposed Architecture

### Backend Trait

The central abstraction. All container runtimes implement this trait:

```rust
#[async_trait]
trait Backend: Send + Sync {
    type Handle: WorkerHandle;

    // Long-lived workers (Shiny apps): start, proxy traffic, health-check.
    async fn spawn(&self, spec: &WorkerSpec) -> Result<Self::Handle>;
    async fn stop(&self, handle: &Self::Handle) -> Result<()>;
    async fn health_check(&self, handle: &Self::Handle) -> bool;
    async fn logs(&self, handle: &Self::Handle) -> Result<LogStream>;
    async fn addr(&self, handle: &Self::Handle) -> SocketAddr;

    // Run-to-completion tasks (dependency restore, image builds):
    // streams logs, returns success/failure, cleans up on completion.
    async fn build(&self, spec: &BuildSpec) -> Result<BuildResult>;
}

trait WorkerHandle: Send + Sync {
    fn id(&self) -> &str;
}
```

`build()` maps to the native run-to-completion primitive on each backend:
a container with auto-remove on Docker/Podman, a Job on Kubernetes, a batch
task on Nomad. In v0 it runs `rv restore`; later it could run container image
builds (Kaniko, buildah, etc.). Build containers carry the same labels as
workers and are covered by the same orphan cleanup.

**Planned implementations:**

| Backend             | Crate     | Handle type        | Priority | Purpose                |
|---------------------|-----------|--------------------|----------|------------------------|
| `Docker` / `Podman` | `bollard` | Container ID       | v0       | Single-host production |
| `Kubernetes`        | `kube-rs` | Pod/Job name       | v2       | Multi-node production  |

**Runtime and orchestrator notes:**

- **Podman** — Podman exposes a Docker-compatible socket via `podman system
  service`. `bollard` connects to it unchanged; the `DockerBackend`
  implementation works without modification. Configure the socket path in
  server config and Podman is supported. Rootless Podman (containers without a
  root daemon) is a meaningful security improvement over Docker's default
  daemon-as-root model and is the recommended mode for operators who choose it.
  Considered supported alongside Docker, not a separate backend.

- **containerd / CRI-O** — both are low-level runtimes used underneath Docker,
  Podman, and k8s. Nobody uses them directly as a deployment runtime; they are
  always mediated by one of the above. Invisible behind the trait.

- **Nomad** — HashiCorp's workload scheduler. Simpler than k8s, clean REST
  API, supports Docker task drivers. The `Backend` trait would accommodate a
  `NomadBackend` without changes to the interface. Parked for two reasons:
  (1) a third runtime to maintain with no confirmed user demand, (2)
  HashiCorp's 2023 BSL relicense complicates its use in OSS projects. Revisit
  if there is concrete demand.

- **Docker Swarm / Mesos** — effectively deprecated; not planned.

`WorkerSpec` carries everything a backend needs to launch a worker: the app
directory, the startup command, and resource limits (`max_memory`, `max_cpu`).
In v1, scoped OpenBao tokens are also carried for injection into the container
at spawn time. There is no language or runtime version field — the server runs
a single configured R version. Resource limit enforcement is backend-specific
(Docker container constraints, K8s pod limits), but the fields are present from
v0 so the schema does not need to change when enforcement is added.

The proxy layer uses `max_sessions_per_worker` and `max_workers_per_app` from
the app config to decide worker lifecycle: when `max_sessions_per_worker = 1`
(the v0 default and only allowed value) it calls `backend.spawn()` for each
new session and `backend.stop()` on disconnect, up to the global
`max_workers` ceiling. When `max_sessions_per_worker > 1` (v1), it routes new
sessions to workers with available capacity and manages a shared pool.

The load-balancing and autoscaling layers operate on `SocketAddr` returned by
`backend.addr(handle)` and are completely backend-agnostic.

### HTTP Stack

The control plane API is built with [`axum`](https://github.com/tokio-rs/axum)
(routing, middleware, request/response handling). The proxy layer uses
[`hyper`](https://github.com/hyperium/hyper) directly for connection upgrades,
WebSocket forwarding, and streaming. They compose naturally — `axum` is built
on `hyper` and `tower`.

### Session and Worker Routing

Session routing is split across two traits to keep concerns separate:

```rust
trait SessionStore: Send + Sync {
    async fn get(&self, session_id: &str) -> Option<WorkerId>;
    async fn insert(&self, session_id: &str, worker_id: WorkerId);
    async fn remove(&self, session_id: &str);
}

trait WorkerRegistry: Send + Sync {
    async fn addr(&self, worker_id: &WorkerId) -> Option<SocketAddr>;
    async fn insert(&self, worker_id: WorkerId, addr: SocketAddr);
    async fn remove(&self, worker_id: &WorkerId);
}
```

`SessionStore` pins sessions to workers (1:1 when `max_sessions_per_worker = 1`;
many:1 when `> 1`). `WorkerRegistry` resolves worker IDs to addresses. Load
balancing sits on top — it picks a worker and calls `SessionStore::insert`;
the stores just hold the mappings.

Both traits have in-memory implementations for v0 (`HashMap` behind
`Arc<RwLock<...>>`). A PostgreSQL-backed implementation is added for k8s HA
deployments (v2) without touching the proxy or routing logic.

On first request to `/app/{name}/`, the proxy sets a session cookie containing
a generated session ID. Subsequent requests — including WebSocket reconnects
during the `ws_cache_ttl` grace period — are routed via this cookie.

### Task Store

Restore tasks are managed via a `TaskStore` trait:

```rust
trait TaskStore: Send + Sync {
    async fn create(&self, task_id: TaskId) -> TaskHandle;
    async fn get(&self, task_id: &TaskId) -> Option<TaskState>;
    async fn log_stream(&self, task_id: &TaskId) -> Option<LogStream>;
}
```

In-memory for v0. PostgreSQL-backed implementation added for k8s HA (v2). Both
implement the same trait — no task or API logic changes on swap.

### Network Isolation

App containers execute arbitrary user-supplied R code and must be isolated from
each other, from the server's management API, and from host-level network
services. Internet egress is permitted — R packages, external APIs, and user
code all have legitimate reasons to make outbound requests.

**Docker: per-container bridge network**

Each spawned container gets its own freshly-created user-defined bridge
network. The server joins that network (multi-homed) solely to proxy traffic
to the container. Containers on different bridge networks cannot reach each
other — Docker's bridge isolation enforces this without additional iptables
rules. Internet egress works via NAT as normal.

The server's management API binds only on the host/management interface, not
on the per-container bridge networks, so app containers have no route to it.

One host-level iptables rule is required at setup time to block app containers
from reaching the cloud instance metadata endpoint at `169.254.169.254`. This
address is provided by cloud platforms (AWS, GCP, Azure) on every VM and
returns the instance's cloud credentials (IAM tokens, service account keys) to
anyone who asks. Docker bridge containers can reach it via the host network
stack. Without this rule, arbitrary user code could retrieve the host VM's
cloud credentials and use them against the cloud provider's API. The rule is a
one-time host configuration step; `blockr.cloud` verifies it is in place at
startup and refuses to start if it is missing on a cloud-detected host.

**Container hardening applied to every app container via `WorkerSpec`:**

- `--cap-drop=ALL` — all Linux capabilities dropped; a Shiny process needs none
- `--security-opt=no-new-privileges` — blocks privilege escalation via setuid
  binaries
- `--read-only` with a tmpfs at `/tmp` — container filesystem is immutable;
  app code cannot modify it
- No Docker socket mount
- Default seccomp profile enforced (never disabled)

**Kubernetes: NetworkPolicy**

Each app Pod gets a `NetworkPolicy` that denies all ingress except from the
server Pod, and restricts egress: internet-bound traffic is allowed, traffic
to cluster-internal CIDRs and to `169.254.169.254` is denied. This requires a
CNI plugin that enforces NetworkPolicy (Calico or Cilium; Flannel alone does
not suffice). The Helm chart documents this requirement and optionally installs
Calico as a sub-chart.

**`WorkerSpec` carries network config** so that each backend constructs the
appropriate isolation primitives at spawn time: a named bridge network for
Docker, a `NetworkPolicy` manifest for Kubernetes.

### v0: Core Infrastructure

One session per worker (`max_sessions_per_worker = 1`, enforced),
`max_workers_per_app` unlimited by default, global `max_workers` ceiling.
Docker backend, Shiny apps only. No user auth on the app plane. Control
plane protected by a single static bearer token in config.

1. **`Backend` trait** — Docker implementation (`spawn` + `build`); mock
   backend for tests
2. **Worker scaling** — proxy spawns one container per session and tears it
   down on disconnect; `max_sessions_per_worker` locked to `1` (other values
   rejected); `max_workers_per_app` defaults to unlimited (capped by global
   `max_workers`); session-sharing and load balancing deferred to v1
3. **Session and worker routing** — cookie-based session pinning; `SessionStore`
   and `WorkerRegistry` traits with in-memory implementations; designed to
   generalize to multi-worker and HA without interface changes
4. **Network isolation** — per-container bridge networks; container hardening
   (`--cap-drop=ALL`, read-only fs, no socket mount); metadata endpoint block
5. **HTTP/WS reverse proxy** — `axum` control plane + `hyper` proxy layer;
   route by app name (`/app/{name}/`); handle WS upgrades; trailing-slash
   redirect
6. **Cold-start UX** — hold initial request until container healthy; 503 after
   `worker_start_timeout`
7. **WebSocket session caching** — hold backend WS connections on client
   disconnect for `ws_cache_ttl`
8. **Active health polling** — periodic health checks on running workers;
   detect and replace hung processes
9. **Bundle upload** — accept tar.gz via REST; return 202 with `bundle_id` +
    `task_id`; unpack eagerly; atomic write; bundle status `pending → ready |
    failed`
10. **Dependency restoration** — restore R packages from `rv.lock` via
    `backend.build()`; shared cache; stream output via task log endpoint;
    `TaskStore` trait with in-memory implementation
11. **Content registry** — SQLite database tracking deployed apps, bundle
    history (`pending | ready | failed`), and resource limits
12. **REST API** — `/api/v1/` prefix; deploy, list, start/stop, view logs,
    stream task logs
13. **Static bearer token** — single token in server config; env var override
14. **App log capture** — stream and persist container stdout/stderr; expose
    via REST API
15. **Orphan cleanup** — remove untracked containers and networks on startup
16. **`/healthz` endpoint** — unauthenticated liveness check

### v1 / MVP: User-Facing Completeness

Adds everything needed to host a real blockr app for real users. Builds on v0
infrastructure.

17. **Multi-worker and session sharing** — enforce `max_workers_per_app` and
    `max_sessions_per_worker` when `> 1`; load balancing and auto-scaling
    wired in
18. **OIDC authentication** — enterprise SSO; establishes user identity
19. **IdP client credentials** — replaces static token; machine auth via
    OAuth 2.0 client credentials flow; same JWT validation path as human auth
20. **User sessions** — cookie-based; transparent access token refresh
21. **RBAC + per-content ACL** — roles and per-app access control
22. **Identity injection** — user identity and groups injected as HTTP headers
    into each Shiny session
23. **Integration system** — OpenBao as secrets backend; IdP JWT → scoped
    OpenBao token at session start; token injected into R process; R process
    reads secrets directly from OpenBao via `httr2`; no companion package
24. **Audit logging** — append-only JSON Lines of all state-changing operations
25. **Vanity URLs** — per-content custom URL paths
26. **Content discovery** — catalog API, tag system, search/filter
27. **Load balancing** — cookie-hash sticky sessions for Shiny; active when
    `max_workers_per_app > 1`
28. **Auto-scaling** — connection-based, paired with load balancing; active
    when `max_workers_per_app > 1`
29. **Telemetry and observability** — Prometheus metrics endpoint,
    OpenTelemetry tracing
30. **`/readyz` endpoint** — readiness check against all runtime dependencies

### v2

31. **Kubernetes backend** — Deployments for apps, Jobs for tasks
32. **Bundle rollback** — activate a previous bundle; drain sessions gracefully
33. **Per-content resource limit enforcement** — CPU/memory caps via Docker /
    K8s (fields carried in `WorkerSpec` from v0)
34. **CLI tool** — dedicated Rust binary for deployment and management
35. **Web UI** — admin dashboard, content browser, log viewer; credential
    enrollment UI
36. **Multiple execution environment images** — per-app image selection;
    operators or app developers specify which image to use per deployment
37. **Scale-to-zero** — idle shutdown; pair with pre-warming
38. **Seat-based pre-warming** — pre-started container pools; pair with
    scale-to-zero
39. **Runtime package installation** — allow apps to install R packages at
    runtime (writable library mount); explore use cases such as user-driven
    package experimentation and dynamic dependency loading

## Database Schema

Two tables — everything else lives in OpenBao, the IdP, Docker, or in-memory.

**Storage backend:** SQLite for single-host Docker deployments — zero
operational overhead and sufficient for the write load (deploys and config
changes, not per-request writes). The Kubernetes backend (v2) will likely
require PostgreSQL for HA multi-node deployments where SQLite's single-writer
model breaks down.

The database layer should be abstracted behind a trait from the start so that
swapping SQLite for PostgreSQL is a matter of adding a new implementation
rather than touching query code throughout the codebase. Use
[`sqlx`](https://github.com/launchbakery/sqlx) with compile-time checked
queries against a generic connection type — it supports both SQLite and
PostgreSQL with the same query syntax.

```sql
CREATE TABLE apps (
    id                      TEXT PRIMARY KEY,      -- UUID, system-generated
    name                    TEXT NOT NULL UNIQUE,  -- user-supplied slug
    status                  TEXT NOT NULL,         -- running | stopped | failed
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,                      -- max replicas; NULL = unlimited
    max_sessions_per_worker INTEGER NOT NULL DEFAULT 1,   -- max sessions per container; v0: always 1
    memory_limit            TEXT,                  -- e.g. "512m"
    cpu_limit               REAL,                  -- fractional vCPUs
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,     -- UUID
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL,        -- pending | ready | failed
    path        TEXT NOT NULL,        -- path to tar.gz on disk
    uploaded_at TEXT NOT NULL
);
```

`active_bundle` only ever references a `ready` bundle; enforced in application
logic (SQLite cannot express this constraint natively). `max_workers_per_app`
defaults to NULL (unlimited, capped by the global `max_workers` ceiling).
`max_sessions_per_worker` defaults to `1`; in v0 other values are rejected.
Session-sharing logic is deferred to v1.

**What lives elsewhere:**

| Concern | Where |
|---|---|
| Per-user credentials (OAuth tokens, API keys) | OpenBao (v1) |
| User identity, groups, auth tokens | IdP (v1) |
| Session state (sub, groups, access + refresh token) | Signed cookie (v1) |
| Runtime worker state (container ID ↔ session) | In-memory |
| App logs | Docker log stream + persisted files |

### Not Built-In

- **TLS termination** — delegate to Caddy/nginx/Traefik. The server speaks HTTP
  only.

## Deployment

### Distribution

Two artifacts are shipped:

- **Native binary** — a single statically-linked Rust binary. Suitable for
  operators who prefer to manage the process directly (systemd unit, etc.) or
  for development. No runtime dependencies beyond Docker and the R image.
- **Docker image** — the recommended production deployment. Uses the
  Docker-out-of-Docker (DooD) pattern: the server container is given access to
  the host Docker daemon via a mounted socket. Containers spawned for Shiny
  apps are siblings on the host, not children of the server container.

### Networking

In both deployment modes, workers must be reachable from the server over TCP.
For Docker, each worker gets its own per-container bridge network (see Network
Isolation). The server joins each network to proxy traffic, resolving the
worker's address via `backend.addr(handle)` (container IP + Shiny port).

The external TLS-terminating proxy (Caddy, nginx, Traefik) connects to the
server over the host network or a dedicated Docker network. The server only
speaks plain HTTP.

### Bundle Storage

Bundle archives, unpacked app directories, and restored R libraries must be
accessible to both the server (for writing during deploy) and to workers
(read-only at runtime). Two config values control the paths:

- **`bundle_server_path`** — where the server reads and writes bundles
  (e.g. `/data/bundles`). The full layout lives here: archives, unpacked
  app directories, and restored R libraries for all apps.
- **`bundle_worker_path`** — where each worker sees its own bundle
  (e.g. `/app`). Read-only. The worker only sees its specific bundle, not
  the full storage tree.

How the same underlying storage appears at both paths is an operator concern:
a named Docker volume in Docker Compose, a PVC in Kubernetes, or a shared
host path when the server runs as a native binary. The server config does not
name volumes or host paths — it only declares the mount points.

Each worker gets two read-only mounts:
- App code → `{bundle_worker_path}/` (e.g. `/app`)
- R library → `{bundle_worker_path}/lib` (e.g. `/app/lib`)

`WorkerSpec` carries the resolved paths so the backend can construct the
correct mount spec.

### Reference Docker Compose

A minimal single-host setup with Caddy for TLS:

```yaml
services:
  blockr:
    image: ghcr.io/blockr-org/blockr.cloud:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - blockr-bundles:/data/bundles
      - blockr-db:/data/db
    environment:
      BLOCKR_SERVER_TOKEN: "${BLOCKR_SERVER_TOKEN}"
    networks:
      - blockr-net

  caddy:
    image: caddy:latest
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - caddy-data:/data
    networks:
      - blockr-net

volumes:
  blockr-bundles:
  blockr-db:
  caddy-data:

networks:
  blockr-net:
```

```
# Caddyfile
blockr.example.com {
    reverse_proxy blockr:8080
}
```

### Kubernetes Deployment (v2)

The k8s deployment is a v2 milestone. Several constraints differ meaningfully
from the single-host Docker case.

**Our server** runs as a k8s Deployment, talking to the cluster API via
`kube-rs`. No Docker socket — the Kubernetes `Backend` trait implementation
creates Pods and Services via the k8s API instead.

**Bundle storage**

The Docker named-volume approach does not translate to k8s. Options, in order
of preference:

- **ReadWriteMany PVC (default)** — a PersistentVolumeClaim with `ReadWriteMany`
  access mode (NFS, AWS EFS, CephFS, etc.) mounted into both the server Pod and
  each app Pod. Available on most managed clusters; adds a storage class
  dependency.
- **Object storage (alternative)** — bundles uploaded to S3/MinIO; app Pods
  pull the bundle at startup via an init container. No RWX requirement, works
  on any cluster. Adds a cold-start download penalty and an object store
  dependency.
- **Image-baking (out of scope)** — build a container image per bundle at
  deploy time (Kaniko or similar); reference the image in the Pod spec. Zero
  runtime bundle access, but requires a full container build pipeline.

`WorkerSpec` carries a bundle reference that the backend interprets: a volume
mount spec for PVC mode, a pre-signed URL for object storage mode. The server
config declares which mode is active.

**Database**

SQLite's single-writer model is incompatible with multi-replica deployments.
The k8s deployment uses PostgreSQL. The database trait introduced for SQLite/
PostgreSQL portability (see Database Schema) means no query code changes —
only the connection pool type switches.

**In-memory state**

The worker map (session → Pod address) is kept in-memory. With a single server
replica this is fine. For HA (multiple server replicas), it moves to
PostgreSQL: a `workers` table (session ID, pod IP, port, app ID, created at)
with the server holding a local read-through cache. The in-memory and
PostgreSQL-backed implementations share an interface defined from v0, so the
swap is additive rather than a rewrite.

**TLS and ingress**

cert-manager handles certificate provisioning (Let's Encrypt or internal CA).
An Ingress resource (or HTTPRoute via Gateway API) routes external traffic to
our server's ClusterIP Service. Our server proxies onward to app Pod IPs
resolved via the k8s API — no Ingress rule per app needed.

**Distribution**

A Helm chart is the primary distribution artifact for k8s. Kustomize bases
are provided as an alternative. The chart covers: server Deployment, RBAC for
k8s API access, PVC (or object storage Secret), PostgreSQL dependency
(sub-chart or external), Ingress/cert-manager integration, and a
`values.yaml` with sensible defaults.
