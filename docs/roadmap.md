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
  isolation mode is **per-session** (one container per user session). See the
  Isolation Mode feature entry for details.

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
bind  = "0.0.0.0:8080"
token = "..."           # bearer token for control plane auth (v0)
                        # use BLOCKR_SERVER_TOKEN env var in production

[docker]
socket     = "/var/run/docker.sock"  # or Podman socket path
image      = "ghcr.io/blockr-org/blockr-r-base:latest"
shiny_port = 3838                    # internal port Shiny listens on

[storage]
# Named Docker volume (recommended when server runs in a container):
bundle_volume = "blockr-bundles"
# Or host bind mount (native binary or explicit config):
# bundle_host_path      = "/opt/blockr/bundles"
# bundle_container_path = "/bundles"

[database]
path = "/data/db/blockr.db"

[proxy]
ws_cache_ttl    = "60s"   # how long to hold backend WS on client disconnect
health_interval = "10s"   # how often to poll worker health
queue_depth     = 128     # max queued requests before returning 503
```

**v1 additions** (OIDC + OpenBao):

```toml
[oidc]
issuer_url    = "https://auth.example.com/realms/myrealm"
client_id     = "blockr-cloud"
client_secret = "..."    # use BLOCKR_OIDC_CLIENT_SECRET env var
groups_claim  = "groups" # optional, default: "groups"

[openbao]
address       = "https://bao.example.com"
admin_token   = "..."    # use BLOCKR_OPENBAO_ADMIN_TOKEN env var
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
  user-defined bridge network named `blockr-{session-id}` (per-session) or
  `blockr-{app-id}` (per-app). The server joins that network to proxy traffic;
  no host port mapping is needed. `backend.addr(handle)` returns the
  container's bridge IP and the Shiny port (configured as `[docker] shiny_port`,
  default `3838`).

  **Labels:** all containers and networks spawned by blockr.cloud carry
  identifying labels:

  ```
  dev.blockr.cloud/managed    = "true"
  dev.blockr.cloud/app-id     = "{app-id}"
  dev.blockr.cloud/session-id = "{session-id}"  # per-session mode only
  ```

  These labels are used for orphan cleanup, log streaming, health polling, and
  lifecycle management. On startup, the server queries Docker for both
  containers and networks carrying `dev.blockr.cloud/managed=true` and removes
  any it has no active record for.
  **Priority: v0.** Required — there is no other production backend.

- **Isolation mode.** Controls the granularity at which containers are spawned
  per app. Two modes:

  - **`per-session` (default):** A new container is spawned for each incoming
    user session and torn down when the session ends. No cross-session
    interference is possible — each user has their own process, filesystem, and
    memory space. Required for public or untrusted users. Higher resource cost
    and cold-start latency per session.

  - **`per-app`:** One container (or a pool) is shared across all sessions for
    an app. Users within the app share the same R process — global environment,
    `system()` calls, and file I/O are not isolated between sessions.
    Appropriate only for authenticated, mutually-trusting users (e.g. an
    internal team) where the lower resource cost and absence of per-session
    cold starts are worthwhile.

  blockr apps execute arbitrary user-supplied R code, so `per-session` is the
  default and the recommended mode for any deployment with external or untrusted
  users. `per-app` is opt-in per app.

  Isolation mode is stored in the content registry and carried in `WorkerSpec`.
  In `per-session` mode, the proxy spawns a worker on each new WebSocket
  connection and routes that connection exclusively to it; in `per-app` mode,
  the proxy routes connections to shared workers and applies load balancing.
  This affects both the proxy routing layer and the backend lifecycle logic.

  OpenBao tokens are injected at worker spawn time in both modes; in
  `per-session` mode each container gets its own scoped token, which is
  automatically invalidated when the container is destroyed.
  **Priority: v0.** Must be designed into the proxy and backend trait from the
  start — retrofitting session-scoped worker lifecycle later is expensive.

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
  **Priority: v0.** Can't serve apps without it.

- **Request queuing.** Hold incoming requests in a bounded queue rather than
  immediately returning 503 when the server cannot serve them immediately.
  The trigger differs by isolation mode: in `per-app` mode it is "all workers
  for this app are at capacity"; in `per-session` mode it is "the host is at
  resource limits and cannot spawn another container right now." Once capacity
  is available, dequeue and forward. Only return 503 when the queue itself is
  full. Shiny Server's immediate-503-at-capacity behaviour is a known pain
  point we should fix from the start.
  **Priority: v0.** Immediate 503 under load is poor UX.

- **Active health polling.** After a worker starts, periodically poll its
  endpoint (TCP connect or lightweight HTTP probe) to detect hung processes.
  Shiny Server only probes at startup; a hung R process can hold a worker slot
  indefinitely. On failure, mark the worker unhealthy, stop it via the backend,
  and (if auto-scaling is enabled) spawn a replacement.
  **Priority: v0.** Without this, hung processes silently swallow traffic.

- **Load balancing.** Distribute requests across multiple workers for the same
  app. Only applicable to `per-app` mode — in `per-session` mode each session
  already has a dedicated container and there is nothing to balance. Shiny
  requires cookie-hash sticky sessions — sessions are stateful and tied to a
  specific R process. faucet's `LoadBalancingStrategy` trait is a good model.
  **Priority: v1 / MVP.** v0 runs single-worker-per-app. Load balancing
  and auto-scaling are a package deal added together.

- **WebSocket session caching.** When a browser briefly disconnects (page
  reload, network glitch), hold the backend WebSocket connection open for a
  grace period so the client can reconnect to the same session. faucet
  implements this with a 60s cache. Critical for Shiny apps where session state
  lives in the R process. In `per-session` mode this also means keeping the
  container alive during the grace period — `backend.stop()` is not called
  until the grace period expires without a reconnect.
  **Priority: v0.** Session loss on reload is unacceptable for Shiny users.

- **Auto-scaling.** Monitor active connections or request rate per app and
  dynamically call `backend.spawn()` / `backend.stop()` to add or remove
  workers. Only applicable to `per-app` mode — in `per-session` mode the
  worker count tracks the session count one-to-one and there is nothing to
  scale independently. Configurable min/max instances per app. faucet's
  RPS-based autoscaler and ShinyProxy's seat-based scaler are references.
  **Priority: v1 / MVP.** Comes alongside load balancing — they are a
  package deal.

- **Scale-to-zero.** When a `per-app` mode app has no active connections for a
  configurable idle period, stop its workers to free resources. On the next
  incoming request, hold the connection and spin up a worker before forwarding.
  Idle detection should use reference-counted connection tracking (HTTP
  connections, WebSocket connections, and pending connections counted
  separately) — inspired by Shiny Server's `httpConn` / `sockConn` /
  `pendingConn` model. Only meaningful for `per-app` mode; in `per-session`
  mode containers already die with their session.
  **Priority: v2.** Depends on `per-app` mode; pair with pre-warming.

- **Bundle upload and deployment.** Accept a tar.gz archive of app code via a
  REST endpoint, unpack it to a content directory, trigger dependency
  installation, and register it in the content database. App name and all
  configuration are supplied via the API — there is no in-bundle manifest file.
  The conventional entrypoint is `app.R`; a `rv.lock` at the bundle root is
  required for dependency restoration. Each upload creates a new versioned
  bundle; previous bundles are retained up to a configurable limit, enabling
  rollback. Typical deploy flow:

  ```
  POST /apps                       { "name": "my-app" }  →  { "id": "a3f2c1...", ... }
  POST /apps/{id}/bundles          <tar.gz body>          →  activates immediately
  ```
  **Priority: v0.** Core deployment mechanism.

- **Bundle rollback.** Activate a previous bundle for a content item. Drain
  active sessions gracefully before switching — don't kill users mid-session.
  Posit Connect's bundle activation model is the reference.
  **Priority: v2.** In v0 a bad deploy can be fixed by redeploying the
  previous code, which goes through the same path as a fresh deploy.

- **Dependency restoration.** After uploading a bundle, restore R package
  dependencies from `rv.lock` using [`rv`](https://github.com/A2-ai/rv).
  Restore runs inside a build container with a shared cache volume so packages
  aren't re-downloaded on every deploy. The R version is configured
  server-wide — no per-deployment version selection or version matching logic.
  **Priority: v0.** Tightly coupled with bundle upload — can't deploy without
  restoring deps.

- **Content registry.** A SQLite database with two tables:

  - `apps` — name, UUID, status (running/stopped/failed), isolation mode,
    resource limits (`max_processes`, `memory_limit`, `cpu_limit`), active
    bundle ID, and encrypted environment variables
  - `bundles` — per-app bundle history: tar.gz path, upload timestamp, which
    bundle is currently active

  Credentials are in OpenBao, user identity is in the IdP, runtime worker
  state (container ID → session mapping) is in-memory, and session state lives
  in signed cookies. SQLite stores only what must survive a server restart.
  **Priority: v0.** Need to track what's deployed and its state.

- **REST API.** HTTP endpoints for all server operations: deploy apps, list
  apps, start/stop apps, manage settings, view logs. This is the primary
  interface — the CLI and (eventually) the web UI are clients of this API.
  Think of it as the control plane.
  **Priority: v0.** Primary server interface.

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
  `POST /apps/{id}/bundles` on push. The server has no need to poll
  repositories.

- **Per-content resource limits.** Enforce CPU and memory limits per content
  item (`max_processes`, `memory_limit`, `cpu_limit`). In the Docker backend,
  these map to container resource constraints. In the Kubernetes backend, they
  map to pod resource requests/limits. Resource limits are stored in the content
  registry and passed through `WorkerSpec` from the start, even if enforcement
  is backend-specific.
  **Priority: design now, enforce later.** `WorkerSpec` carries the fields from
  v1; actual enforcement added when Docker/K8s backends land.

- **Parameterized reports.** Support Quarto and R Markdown documents with
  user-supplied parameters, named variants, and per-variant schedules.
  **Priority: out of scope.** Not a Shiny use case.

- **Execution environment images.** Pre-built Docker images with R + system
  libraries installed, used as the base for running apps and tasks. Defines
  what's available at runtime (R version, system deps like GDAL, GEOS, etc.).
  ricochet maintains `r-ubuntu`, `r-alpine`, `r-alma` variants.
  **Priority: v2.** Start with user-provided images, offer maintained base
  images eventually.

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

  Runtime state (which container belongs to which session) is kept in-memory
  and reconstructed implicitly as sessions reconnect after a server restart.
  Explicit logout is handled via an in-memory revocation list of `jti` claim
  values; revocations are lost on restart, meaning a revoked token remains
  valid until its natural expiry — acceptable for the single-host deployment
  model.
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

  **Threat model:** Shiny apps run arbitrary R code. Any credential or token
  placed in the process space must be treated as potentially exfiltrable. The
  blast radius of a compromised session must be bounded to that user's secrets
  only — no path from the process to any other user's data or to the server's
  own DB credentials.

  **Mechanism — Vault + IdP JWT auth:**
  [OpenBao](https://openbao.org) (the open source Vault fork) is used as the
  secrets backend. The IdP and OpenBao are wired together via OpenBao's JWT
  auth method: OpenBao is configured with the IdP's JWKS endpoint once, after
  which any valid IdP JWT can be exchanged for a scoped OpenBao token. Per-user
  policies restrict each token to `read` on `secret/users/{sub}/*` only.

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

  **Enrollment:** Two credential types — OAuth delegation (user authorises via
  provider flow; server stores refresh token in OpenBao at
  `secret/users/{sub}/oauth/{provider}`) and API key / secret (user enters key
  via UI; server writes to `secret/users/{sub}/apikeys/{service}`). Enrollment
  is handled by the server with its admin OpenBao token; the R process has no
  write access.

  **Token TTL and renewal:** OpenBao session tokens are issued with a short
  TTL. The R process renews its token before expiry using OpenBao's standard
  token renewal API — idiomatic OpenBao usage. The companion R package handles
  renewal transparently before each credential read, so app code never deals
  with token lifecycle.

  **R interface:** A companion R package (`blockr.cloud` or similar) wraps the
  OpenBao API behind a simple call: `blockr_secret("openai")` reads
  `secret/users/{sub}/apikeys/openai` using the injected token (renewing it if
  needed) and returns the key. The package is the only integration point the
  app developer needs to know about.

  **Priority: v1 / MVP.** Without this, blockr apps cannot securely integrate
  with external services on a per-user basis.

- **Vanity URLs.** Allow publishers to assign a custom URL path (e.g.
  `/sales-dashboard`) to a content item, in addition to its system-assigned
  ID-based URL. The router resolves vanity paths before falling back to
  ID-based routing. Requires collision detection and a reserved-prefix blocklist
  (e.g. `/__`, `/api`, `/login`).
  **Priority: v1 / MVP.** Low implementation cost, high discoverability
  value.

- **Content discovery.** A way for users to find and navigate to deployed
  content: a content catalog endpoint in the REST API listing all accessible
  items with metadata (title, type, owner, status, URL), a hierarchical tag
  system for organizing content (admin-managed), and basic search/filter
  support. Needed even before a web UI — the API consumer (CLI or UI) needs
  something to work with.
  **Priority: v1 / MVP.** Without discovery, the platform is a black box.

- **Environment variable management.** Store per-app environment variables
  (database credentials, API keys, etc.) encrypted at rest, inject them into
  the container/process at startup. Avoids putting secrets in code or config
  files.
  **Priority: v0.** Apps need secrets from day one.

- **App log capture.** Capture stdout/stderr from each container and make it
  available via the REST API (`GET /apps/{id}/logs`). With `per-session`
  containers, logs must be persisted for a configurable period after the
  container exits so crashes can be diagnosed after the fact. Captured via
  Docker's log streaming API using the container's `dev.blockr.cloud/app-id`
  and `dev.blockr.cloud/session-id` labels.
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
  restarts. With `per-session` containers there is nothing to resume — orphans
  are simply removed.
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
  startup times but adds resource cost. Only meaningful for `per-app` mode.
  **Priority: v2.** Pair with scale-to-zero.

- **Telemetry and observability.** Structured logging, Prometheus-compatible
  metrics endpoint, and OpenTelemetry tracing. Metrics should cover active
  connections per app, request rates, worker lifecycle events (spawn, stop,
  crash), queue depth, and health check results. Posit Connect ships Prometheus
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

## Proposed Architecture

### Backend Trait

The central abstraction. All container runtimes implement this trait:

```rust
#[async_trait]
trait Backend: Send + Sync {
    type Handle: WorkerHandle;

    async fn spawn(&self, spec: &WorkerSpec) -> Result<Self::Handle>;
    async fn stop(&self, handle: &Self::Handle) -> Result<()>;
    async fn health_check(&self, handle: &Self::Handle) -> bool;
    async fn logs(&self, handle: &Self::Handle) -> Result<LogStream>;
    async fn addr(&self, handle: &Self::Handle) -> SocketAddr;
}

trait WorkerHandle: Send + Sync {
    fn id(&self) -> &str;
}
```

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
directory, the startup command, environment variables (decrypted at spawn
time), resource limits (`max_memory`, `max_cpu`), and isolation mode
(`per-session` or `per-app`). There is no language or runtime version field —
the server runs a single configured R version. Resource limit enforcement is
backend-specific (Docker container constraints, K8s pod limits), but the
fields are present from v0 so the schema does not need to change when
enforcement is added.

The proxy layer uses the isolation mode from `WorkerSpec` to decide worker
lifecycle: in `per-session` mode it calls `backend.spawn()` on each new
WebSocket connection and `backend.stop()` on disconnect; in `per-app` mode it
manages a shared pool and applies load balancing across workers.

The load-balancing and autoscaling layers operate on `SocketAddr` returned by
`backend.addr(handle)` and are completely backend-agnostic.

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

Single-worker-per-app, Docker backend, Shiny apps only. No user auth on the
app plane. Control plane protected by a single static bearer token in config.

1. **`Backend` trait** — Docker implementation; mock backend for tests;
   `WorkerSpec` includes isolation mode from the start
2. **Isolation mode** — `per-session` (default) and `per-app`; proxy and
   backend lifecycle wired accordingly
3. **HTTP/WS reverse proxy** — route requests to the correct worker, handle WS
   upgrades
4. **WebSocket session caching** — hold backend WS connections on client
   disconnect
5. **Request queuing** — queue requests at capacity rather than returning
   immediate 503
6. **Active health polling** — periodic health checks on running workers; detect
   and replace hung processes
7. **Bundle upload** — accept tar.gz via REST, unpack, register; name supplied
   via API; version every upload
8. **Dependency restoration** — restore R packages from `rv.lock` using `rv`
9. **Content registry** — SQLite database tracking deployed apps, bundle
   history, resource limits, isolation mode, and state
10. **REST API** — deploy, list, start/stop, view logs
11. **Static bearer token** — single token in server config for control plane
    access; no database storage
12. **Environment variable management** — encrypted at rest, injected at startup
13. **App log capture** — stream and persist container stdout/stderr; expose
    via REST API
14. **Orphan container cleanup** — remove unlabeled/untracked containers on
    startup

### v1 / MVP: User-Facing Completeness

Adds everything needed to host a real blockr app for real users. Builds on v0
infrastructure.

12. **OIDC authentication** — enterprise SSO; establishes user identity
13. **IdP client credentials** — replaces static token; machine auth via
    OAuth 2.0 client credentials flow; same JWT validation path as human auth
14. **User sessions** — server-side session tracking for authenticated users
15. **RBAC + per-content ACL** — roles and per-app access control
16. **Identity injection** — user identity and groups injected as HTTP headers
    into each Shiny session
17. **Integration system** — OpenBao as secrets backend; IdP JWT → scoped
    OpenBao token at session start; token injected into R process; R process
    reads secrets directly from OpenBao; companion R package (`blockr_secret()`)
18. **Audit logging** — append-only JSON Lines of all state-changing operations
19. **Vanity URLs** — per-content custom URL paths
20. **Content discovery** — catalog API, tag system, search/filter
21. **Load balancing** — cookie-hash sticky sessions for Shiny
22. **Auto-scaling** — connection-based, paired with load balancing
23. **Telemetry and observability** — Prometheus metrics endpoint,
    OpenTelemetry tracing

### v2

23. **Kubernetes backend** — Deployments for apps, Jobs for tasks
24. **Bundle rollback** — activate a previous bundle; drain sessions gracefully
25. **Per-content resource limit enforcement** — CPU/memory caps via Docker /
    K8s (fields carried in `WorkerSpec` from v0)
27. **CLI tool** — dedicated Rust binary for deployment and management
28. **Web UI** — admin dashboard, content browser, log viewer
29. **Execution environment images** — maintained base images with R + system
    libs
30. **Scale-to-zero** — idle shutdown for `per-app` mode; pair with
    pre-warming
31. **Seat-based pre-warming** — pre-started container pools; pair with
    scale-to-zero

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
    id             TEXT PRIMARY KEY,  -- UUID, system-generated
    name           TEXT NOT NULL UNIQUE,  -- user-supplied slug
    status         TEXT NOT NULL,     -- running | stopped | failed
    isolation_mode TEXT NOT NULL,     -- per-session | per-app
    active_bundle  TEXT REFERENCES bundles(id),
    max_processes  INTEGER,
    memory_limit   TEXT,              -- e.g. "512m"
    cpu_limit      REAL,              -- fractional vCPUs
    env_vars       BLOB,              -- encrypted JSON
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,     -- UUID
    app_id      TEXT NOT NULL REFERENCES apps(id),
    path        TEXT NOT NULL,        -- path to tar.gz on disk
    uploaded_at TEXT NOT NULL
);
```

**What lives elsewhere:**

| Concern | Where |
|---|---|
| Per-user credentials (OAuth tokens, API keys) | OpenBao |
| User identity, groups, auth tokens | IdP |
| Session state (sub, groups, access + refresh token) | Signed cookie |
| Runtime worker state (container ID ↔ session) | In-memory |
| App logs | Docker log stream + persisted files |
| Revoked token list | In-memory (`jti` blocklist) |

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

In both deployment modes, app containers must be reachable from the server
over TCP. For Docker, all containers (server and app containers) are placed on
a shared Docker network created at startup. The server resolves each container's
address via `backend.addr(handle)` (container IP + Shiny port) and proxies
traffic to it.

The external TLS-terminating proxy (Caddy, nginx, Traefik) connects to our
server over the host network or a shared Docker network. Our server only speaks
plain HTTP.

### Bundle Storage and Path Translation

Bundle archives and unpacked app directories must be accessible to both the
server (for reading) and to app containers (bind-mounted at startup). This
creates a path constraint: the path given to Docker when spawning an app
container must be the **host-side path**, not the path as seen from inside the
server container.

Two approaches, both supported:

**Named Docker volume (recommended for containerized server)**

```toml
[storage]
bundle_volume = "blockr-bundles"   # Docker named volume
bundle_mount  = "/bundles"         # mount point inside app containers
```

The named volume is mounted into the server container at `/data/bundles` and
into each app container at `/bundles`. Docker resolves the volume on both sides
— no host path translation needed.

**Host bind mount (native binary or explicitly configured)**

```toml
[storage]
bundle_host_path      = "/opt/blockr/bundles"  # path on the Docker host
bundle_container_path = "/bundles"             # mount point inside app containers
```

When the server runs as a native binary, `bundle_host_path` is also its local
path to bundle files. When the server runs in a container, `bundle_host_path`
is the host-side path of whatever is mounted into the server container — the
operator must configure this explicitly to match their bind mount.

`WorkerSpec` carries the resolved host-side path or volume name so that the
backend can construct the correct bind mount spec for each app container
regardless of how the server itself is deployed.

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
      BLOCKR_CONTROL_TOKEN: "${BLOCKR_CONTROL_TOKEN}"
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

Two in-memory stores exist today: the worker map (session → Pod address) and
the `jti` revocation list. With a single server replica these are fine. For
HA (multiple server replicas), both move to PostgreSQL:

- Worker map → a `workers` table (session ID, pod IP, port, app ID, created at)
  with the server holding a local read-through cache
- `jti` blocklist → a `revoked_tokens` table with TTL-based cleanup via a
  background task

The in-memory implementations and the PostgreSQL-backed implementations share
an interface defined from v0, so the swap is additive rather than a rewrite.

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
