# blockyard Architecture

This document describes the structural decisions — the "how" — behind
blockyard's design. For scope, milestones, and feature descriptions (the
"what" and "when"), see [roadmap.md](roadmap.md). For configuration
reference, see the Server Configuration section in the roadmap.

## Backend Interface

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

## HTTP Stack

The control plane API is built with `chi` on top of Go's `net/http`. The
proxy layer uses `net/http` and `coder/websocket` for connection upgrades,
WebSocket forwarding, and streaming. `chi` implements `http.Handler` so
everything composes naturally with the standard library.

## Session and Worker Routing

A session store maps session IDs to worker IDs; a worker registry maps worker
IDs to network addresses. Both are concrete in-memory structs for v0 (map +
`sync.RWMutex`). Load balancing (v1) sits on top — it picks a worker and
writes the mapping; the stores just hold the data.

When v2 needs PostgreSQL-backed implementations for multi-node HA, extracting
an interface in Go is a low-cost refactor — define the interface at the call
site and the existing struct already satisfies it.

## Task Store

An in-memory task store manages restore tasks. It provides a create/subscribe
pattern: background restore goroutines write log lines; HTTP handlers read
buffered output and optionally follow live lines via a channel. Same interface
extraction story as session/worker routing for v2 HA.

## Network Isolation

App containers execute arbitrary user-supplied R code and must be isolated from
each other, from the server's management API, and from host-level network
services. Internet egress is permitted.

**Docker:** each spawned container gets its own freshly-created user-defined
bridge network. When `max_sessions_per_worker = 1` the network is named
`blockyard-{session-id}`; when sessions share a worker it is named
`blockyard-{worker-id}`. The server joins each network (multi-homed) solely
to proxy traffic; no host port mapping is needed. The address is resolved by
inspecting the container's IP on its specific named network — not just any IP
the container has. One host-level iptables rule blocks `169.254.169.254`
(cloud instance metadata); the server verifies this at startup on
cloud-detected hosts.

**Why per-container bridges:** the requirements are that workers can reach
the internet and local network services (package repositories, external APIs,
OpenBao, IdP — which may themselves be containerized on the same host),
workers cannot reach each other, the server can reach each worker to proxy
traffic, and workers cannot reach the server's management API. The obvious
alternative — a single shared bridge with `--icc=false` (inter-container
communication disabled) — blocks *all* container-to-container traffic,
including server-to-worker, since the server is itself a container in the
Docker Compose deployment. Workarounds (server in `network_mode: host`,
two-network topologies, published ports to localhost) each re-introduce the
isolation problem in a different form and require compensating iptables rules
or bind-address management. Per-container bridges give strong isolation by
default without additional firewall rules. The operational overhead — network
proliferation, server multi-homing, cleanup — is manageable in practice:
Docker handles thousands of networks, Linux multi-homing is well-supported,
and label-based cleanup is a single API filter call.

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

**Container hardening:** `--cap-drop=ALL`, `--security-opt=no-new-privileges`,
`--read-only` with tmpfs at `/tmp`, no Docker socket mount, default seccomp
profile.

**Kubernetes (v2):** `NetworkPolicy` per Pod denying all ingress except from
the server, restricting egress to internet-only. Requires a CNI plugin that
enforces NetworkPolicy (Calico or Cilium).

## HTTP Hardening

The HTTP stack applies several defense-in-depth measures:

**Security headers** (all responses):
- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Strict-Transport-Security: max-age=63072000; includeSubDomains` (HTTPS only)

**Content-Security-Policy** (API responses only):
`default-src 'none'; frame-ancestors 'none'`. Intentionally not applied to
proxied Shiny apps, which require inline scripts and styles.

**Rate limiting** (per-IP, via `go-chi/httprate`):
| Endpoint group | Limit |
|---|---|
| Auth (`/login`, `/callback`, `/logout`) | 10 req/min |
| Credential exchange | 20 req/min |
| User token management | 20 req/min |
| General API (`/api/v1/*`) | 120 req/min |
| App proxy (`/app/{name}/*`) | 200 req/min |

Health probes (`/healthz`, `/readyz`) are not rate-limited.

**Request body limits**: 1 MiB default; bundle upload endpoint uses
`max_bundle_size` from config (default 100 MiB).

## Credential Trust Model

Shiny apps execute arbitrary R code. Users enroll credentials for external
services (AI providers, databases, S3, etc.) that must be delivered to
their sessions as plaintext. This is an inherent requirement — the R
process needs raw API keys and tokens to authenticate against third
parties. The architecture is designed so that **no single compromised
component can exfiltrate all user credentials at once.**

Three independent trust domains hold the credential pipeline:

| Domain | What it holds | Compromise alone yields |
|---|---|---|
| **IdP** | User identities, OIDC signing keys | Forge JWTs for any user → unlock any user's OpenBao namespace |
| **OpenBao** | Encrypted credential storage, per-user policy enforcement | Read all stored secrets directly |
| **Server** | OIDC client secret, in-memory session tokens, AppRole-issued vault token (write-scoped, renewable) | Secrets for users with active sessions only (see below) |

**Why the server alone is not sufficient to exfiltrate all credentials:**

The server authenticates to OpenBao via AppRole auth — a renewable,
scoped token that grants **write** (credential enrollment) and
**metadata** operations. It cannot read user secret values. The only
way to read a user's secrets is with a scoped OpenBao token obtained
via JWT login — which requires a valid IdP access token for that user.

The server obtains a user's access token only when the user actively
authenticates via the OIDC flow. Access tokens are held in memory (never
persisted to disk) and expire on a short TTL. The OIDC client secret
alone cannot produce user access tokens — it is used to exchange
authorization codes (which require interactive user login at the IdP)
and to refresh existing tokens (which requires a refresh token from a
prior login).

Therefore, a compromised server can exfiltrate credentials only for
users who log in during the window of compromise. Users who do not
authenticate during that window are unaffected. On detection, restarting
the server clears all in-memory tokens; revoking the server's vault
token, re-bootstrapping via AppRole with a new `secret_id`, and
rotating the OIDC client secret completes remediation.

**Why not store credentials in the server's own database?**

The alternative — encrypting credentials with a symmetric key in SQLite —
was evaluated and rejected. A symmetric encryption key is fundamentally
different from a write-scoped admin token: whoever holds the key can
decrypt every credential for every user who has ever enrolled, instantly,
regardless of session state. A compromised server with access to the key
and the database can exfiltrate the complete credential history in one
operation. There is no way to create a "write-only" encryption key.

The operational cost of running OpenBao (deploy, initialize, unseal,
monitor) is the price for this property. It is worth paying because
blockyard is unlikely to undergo a professional security audit in the
foreseeable future, and defense-in-depth through trust domain separation
is the most effective mitigation against an unaudited server codebase.

**Inherent limitation:** a compromised server will always be able to
intercept credentials for users with active sessions. This is
unavoidable — the server brokers credential delivery between the
browser-side OIDC flow and the container-side injection, and plaintext
must traverse it. The architecture minimizes this to the theoretical
minimum: active sessions only, no retroactive access.

## Authorization Model

Blockyard separates authentication from authorization. The IdP handles
authentication — proving who a user is via OIDC. Blockyard handles
authorization entirely — deciding what each user can do. IdP groups
play no role in blockyard's authorization model.

### System Roles

Every user has one system-wide role, assigned by a blockyard admin.
New users get `viewer` by default on first OIDC login.

| Role | Capabilities |
|---|---|
| `admin` | Full control: manage all apps, users, settings. Implicit owner-level access to all content. |
| `publisher` | Create and deploy apps. Full control over own apps. No access to other publishers' apps unless explicitly granted. |
| `viewer` | Access apps they have been explicitly granted access to (or apps with `logged_in`/`public` visibility). Cannot create or deploy. |

### Per-Content ACL

Each app has an owner (the user who created it) and may have additional
per-user access grants. Grants are user-to-resource — there are no
group-based grants.

| ACL level | Capabilities |
|---|---|
| `owner` | Full control: deploy bundles, change settings, delete app, manage access grants. Set at creation time; transferable by admins. |
| `collaborator` | Deploy bundles, change app settings. Cannot delete the app or manage its access grants. |
| `viewer` | Access and use the app. No management capabilities. |

System admins have implicit owner-level access to all apps.

### App Visibility

Each app has an `access_type` that controls who can reach it:

| `access_type` | Who can access |
|---|---|
| `acl` | Only users with an explicit ACL grant (owner, collaborator, or viewer). Default. |
| `logged_in` | Any authenticated user. No per-user grant required. |
| `public` | Anyone, including unauthenticated users. |

### Identity Injection

The proxy injects two headers on each request forwarded to a Shiny app:

- **`X-Shiny-User`** — the authenticated user's OIDC `sub`. Empty for
  anonymous access to public apps.
- **`X-Shiny-Access`** — the user's effective access level for the
  specific app being accessed. Derived at proxy time from the
  authorization model.

`X-Shiny-Access` values correspond to the per-content ACL levels:

| Condition | `X-Shiny-Access` |
|---|---|
| System admin, or app owner | `owner` |
| Has collaborator grant on this app | `collaborator` |
| Has viewer grant, or app is `logged_in` and user is authenticated | `viewer` |
| App is `public`, user is not authenticated | `anonymous` |

Apps read `session$request$HTTP_X_SHINY_ACCESS` and branch on it — one
value, no ambiguity about what the user can do.

### API Authentication

Two authentication mechanisms, tried in order:

1. **OIDC session cookie** — for browser-based access. Established via
   the `/login` → `/callback` OIDC flow.
2. **Personal Access Token (PAT)** — for CLI, CI/CD, and script access.
   `Authorization: Bearer by_...` header. Tokens are created via the
   web UI (session-only), hashed with SHA-256 for storage, and carry
   the owning user's current role.

When neither is present, the request is rejected with 401. The v0
static bearer token (`server.token` / `BLOCKYARD_SERVER_TOKEN`) has been
superseded by PATs and is no longer supported.

## Graceful Shutdown

On SIGTERM the server shuts down cleanly in this order:

1. **Stop management listener** — if `management_bind` is configured, shut
   down the management listener first so health probes start failing and load
   balancers stop sending traffic
2. **Stop accepting new connections** — close the main HTTP listener
3. **Drain in-flight requests** — wait up to `shutdown_timeout` (default `30s`)
   for in-flight HTTP and WebSocket requests to finish; remaining connections
   are dropped
4. **Cancel background goroutines** — stop health poller, autoscaler, log
   retention cleaner, vault token renewal, and audit writer
5. **Drain sessions and stop workers** — mark all apps as draining (no new
   sessions routed), wait up to half of `shutdown_timeout` for active sessions
   to end naturally, then force-evict remaining workers in parallel (15 s
   timeout per worker)
6. **Remove remaining resources** — clean up any leftover containers and
   networks (build containers, orphans); mark in-progress bundles as `failed`
7. **Flush and close** — flush structured logs and audit log, close the DB
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

Seven tables — everything else lives in OpenBao, the IdP, Docker, or in-memory.

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
    owner                   TEXT NOT NULL DEFAULT '',  -- OIDC sub of creator
    access_type             TEXT NOT NULL DEFAULT 'acl', -- acl | logged_in | public
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,                      -- max replicas; NULL = unlimited
    max_sessions_per_worker INTEGER NOT NULL DEFAULT 1,   -- max sessions per container
    memory_limit            TEXT,                  -- e.g. "512m"
    cpu_limit               REAL,                  -- fractional vCPUs
    title                   TEXT,                  -- human-readable title for catalog
    description             TEXT,                  -- catalog description
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);
-- No status column: app status (running/stopped) is inferred at runtime
-- from whether any workers exist. No need to persist dynamic state.

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,     -- UUID
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL,        -- pending | building | ready | failed
    uploaded_at TEXT NOT NULL
);
CREATE INDEX idx_bundles_app_id ON bundles(app_id);

-- v1 additions: user management, personal access tokens, ACL, tags

CREATE TABLE users (
    sub          TEXT PRIMARY KEY,
    email        TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    role         TEXT NOT NULL DEFAULT 'viewer',
    active       INTEGER NOT NULL DEFAULT 1,
    last_login   TEXT NOT NULL
);

CREATE TABLE personal_access_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   BLOB NOT NULL UNIQUE,
    user_sub     TEXT NOT NULL REFERENCES users(sub),
    name         TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_pat_token_hash ON personal_access_tokens(token_hash);
CREATE INDEX idx_pat_user_sub ON personal_access_tokens(user_sub);

CREATE TABLE app_access (
    app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal  TEXT NOT NULL,        -- user sub
    kind       TEXT NOT NULL DEFAULT 'user',
    role       TEXT NOT NULL,        -- viewer | collaborator
    granted_by TEXT NOT NULL DEFAULT '',
    granted_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (app_id, principal, kind)
);

CREATE TABLE tags (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE app_tags (
    app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, tag_id)
);
```

`users` tracks every OIDC-authenticated user. Roles are managed directly by
blockyard admins — not derived from IdP groups. `personal_access_tokens`
provides identity-aware API access, replacing the v0 static bearer token.
`app_access` stores per-content ACL grants (viewer or collaborator). `tags`
and `app_tags` support content discovery and catalog filtering.

`active_bundle` only ever references a `ready` bundle; enforced in application
logic. `max_workers_per_app` defaults to NULL (unlimited, capped by the global
`max_workers` ceiling). `max_sessions_per_worker` defaults to `1`; in v0 other
values are rejected.

**What lives elsewhere:**

| Concern | Where |
|---|---|
| Per-user credentials (OAuth tokens, API keys) | OpenBao (v1) |
| User identity (authentication) | IdP via OIDC (v1) |
| Session state (sub, access + refresh token) | Server-side session store (v1) |
| User roles and active status | blockyard `users` table (v1) |
| App status (running/stopped) | In-memory (inferred from worker existence) |
| Runtime worker state (container ID ↔ session) | In-memory |
| App logs | Docker log stream + persisted files |

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

### TLS

Not built-in for v0 — delegate to Caddy/nginx/Traefik. Evaluate built-in
TLS via Go's `crypto/tls` + `autocert` for v1.

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
