# Server Update Design

This document describes how blockyard deployments are updated. Two paths
exist depending on infrastructure:

- **Basic:** `docker compose pull && docker compose up -d`. Workers die,
  sessions restart. No additional infrastructure required.
- **Rolling:** `by admin update`. Zero downtime, workers survive. Requires
  Redis and a reverse proxy with Docker service discovery.

Related issues: #70 (client-server version negotiation), #71 (update
notifications).

## Status Quo

The current design explicitly does not support rolling updates.
`StartupCleanup` force-removes every container labeled
`dev.blockyard/managed=true`. `GracefulShutdown` drains and evicts all
workers before exit. The worker token signing key is ephemeral — generated
from `crypto/rand` on every startup. In-memory session routing is not
persisted. A restart means all sessions die.

This was the right trade-off for v0–v1: simplicity over resilience. The
basic update path preserves this — operators who don't need rolling
updates don't pay for the complexity.

## Basic Update Path

For deployments without Redis or a service-discovery-capable proxy,
updates are manual:

```sh
docker compose pull
docker compose up -d
```

Workers are killed, sessions restart. The server boots clean. This is the
current behavior, documented rather than orchestrated.

One improvement applies to the basic path regardless: **health-check-aware
drain**. When the server receives SIGTERM, it immediately starts returning
503 on `/healthz` and `/readyz`, then drains in-flight requests before
exiting. Any health-check-aware proxy (even statically configured) will
stop routing new traffic to the server as soon as health checks fail,
making the restart window cleaner even without service discovery. This
is a small change to the existing shutdown path and benefits all
deployments.

`by admin update` detects the absence of Redis and prints the manual
compose commands instead of attempting a rolling update.

## Rolling Update Path

### Prerequisites

Rolling updates require two pieces of infrastructure beyond the server
itself:

**Redis** — replaces the in-memory session store, worker registry, and
worker map with shared state accessible to both old and new server
processes during the overlap. Without shared state, the overlap doesn't
work: the new server can't route requests to workers it didn't spawn,
and the old server can't hand off sessions it holds only in memory.

Redis is the right choice over PostgreSQL for this state because it's
ephemeral and high-frequency: session lookups on every proxied request,
worker health updates every few seconds, TTL-based expiration. This is
cache-shaped work, not relational data.

**Reverse proxy with Docker service discovery** — during a rolling
update, old and new server containers run simultaneously on the same
Docker network. The proxy must discover both containers and route traffic
based on health. Two capabilities are required:

1. **Discovery** — the proxy watches Docker for container events and
   adds/removes backends automatically. This is inherently
   proxy-specific: Traefik's Docker provider, caddy-docker-proxy,
   nginx-proxy each use their own label/env conventions. Blockyard does
   not interact with these labels — the operator configures them once
   when setting up the proxy, and both containers inherit the same
   labels during the overlap.

2. **Health-based routing** — the proxy checks backend health and stops
   routing to unhealthy containers. This is the cutover mechanism: when
   `by admin update` signals the old server to drain, it starts returning
   503 on `/healthz`. The proxy detects this and shifts all traffic to
   the new server. This part is vendor-agnostic — every proxy that does
   service discovery also does health checking.

The discovery requirement is what distinguishes the rolling path from the
basic path. A statically configured proxy (e.g. `reverse_proxy
blockyard:8080` in a Caddyfile) won't discover the new container. It
only knows about the old one, and the overlap provides no benefit.

### Why Redis Rather Than Reconstruction

The alternative — no Redis, reconstruct state from the database and
Docker labels on startup — was considered and rejected. It requires a
"frozen" mode on the old server (stop background goroutines but keep
proxying), hand-off marker files on the data volume, and careful
ordering to avoid both servers mutating worker state simultaneously.
The result is fragile, hard to test, and produces a second code path
through the entire server lifecycle. Redis eliminates all of this: both
servers read and write the same state, and the overlap is a non-event.

### Shared State in Redis

Three data structures move from in-memory maps to Redis:

**Session store** (`session.Store`) — maps session ID to worker ID and
user sub. Currently a Go map with `sync.RWMutex`. In Redis: hash per
session with TTL matching session expiry. Looked up on every proxied
request.

**Worker registry** (`registry.Registry`) — maps worker ID to network
address. Currently a Go map. In Redis: simple key-value with TTL. Looked
up on every proxied request (cache miss from session store, or direct
worker routing).

**Worker map** (`server.WorkerMap`) — maps worker ID to active worker
metadata (app ID, bundle ID, draining flag, idle-since timestamp). Used
by the autoscaler, health poller, and session routing. In Redis: hash
per worker.

The in-memory implementations remain as the default when Redis is not
configured. The server detects Redis availability at startup and selects
the appropriate backend. Both implement the same Go interface.

### Interface Extraction

The three stores are extracted behind interfaces defined at the call
sites:

```go
// Used by proxy, autoscaler, session management
type SessionStore interface {
    Get(sessionID string) (session.Entry, bool)
    Set(sessionID string, entry session.Entry)
    Delete(sessionID string)
    DeleteByWorker(workerID string)
    CountForWorkers(workerIDs []string) int
}

// Used by proxy for address resolution
type WorkerRegistry interface {
    Get(workerID string) (string, bool)
    Set(workerID string, addr string)
    Delete(workerID string)
}

// Used by autoscaler, health poller, proxy, ops
type WorkerMap interface {
    Get(workerID string) (server.ActiveWorker, bool)
    Set(workerID string, w server.ActiveWorker)
    Delete(workerID string)
    All() []string
    MarkDraining(appID string)
    // ...
}
```

This extraction is cheap — the existing concrete types already satisfy
these shapes. The Redis implementations use the same interface with
GET/SET/HSET operations and appropriate TTLs.

### v4 Clustering

This interface extraction and Redis integration directly serves the v4
Kubernetes milestone. Multi-replica server deployments need shared state
for worker routing; Redis is already in place. The difference between
"two servers during a rolling update" and "N replicas in a cluster" is
one of degree, not kind. The same shared state layer handles both.

### Worker Token Persistence

The HMAC signing key used for worker tokens is currently ephemeral. For
the overlap to work, both old and new servers must sign and verify tokens
with the same key.

**Primary path (OpenBao configured):** on first startup, generate the
key and store it at `secret/data/blockyard/worker-signing-key`. On
subsequent startups, read it back. OpenBao is already in the stack for
credential management; this is a natural extension.

**Fallback (no OpenBao):** write the key to the data volume at
`/data/db/.worker-key`. The signing key protects worker-to-server
communication on a local Docker network — storing it alongside the
database is acceptable given the threat model.

### Drain Mode

When the old server needs to yield to the new one, it enters drain mode.
This is triggered by `SIGUSR1` (sent by `by admin update`). `SIGTERM`
retains the current behavior (full shutdown with worker eviction), so
`docker stop` remains safe for operators who aren't doing rolling
updates.

**Drain sequence:**

1. `/healthz` and `/readyz` start returning 503 immediately. The proxy
   detects this via health checks and stops routing new traffic to the
   old server. This is the cutover signal — vendor-agnostic, no proxy
   API calls or label manipulation required.
2. In-flight requests drain (up to `shutdown_timeout`).
3. Background goroutines stop (health poller, autoscaler, token
   refreshers, audit writer).
4. Database connection closes.
5. Process exits.

**What does NOT happen:** no `EvictWorker`, no `RemoveResource`, no
`Backend.Stop`. Workers, networks, and token directories are left
intact. The new server already manages them via Redis.

### Background Goroutine Ownership

During the overlap, both servers can serve requests — they share routing
state via Redis and either can handle any incoming request. But
background goroutines that mutate state (autoscaler, health poller,
token refreshers) must not run on both servers simultaneously. Two
autoscalers making independent scaling decisions against the same shared
state would race — both could see "app X needs 1 more worker" and each
spawn one.

The new server starts in **passive mode**: it serves requests but does
not start the autoscaler, health poller, or other state-mutating
background loops. The old server continues running them normally. After
the old server exits, `by admin update` signals the new server to
activate its background goroutines (e.g., via an admin API call or a
second signal). At no point do two sets of background loops run
concurrently.

This is simpler than leader election and has no race window regardless
of overlap duration — the old server's autoscaler makes correct
decisions for as long as it runs, because it reads the same Redis state.

### Migration Safety

Database migrations run on startup via `golang-migrate`. During a
rolling update, the old server is still serving traffic when the new
server starts and runs migrations.

**Constraint: migrations must be backward-compatible with the previous
release.** The old server continues reading and writing the database
after migrations run. This follows the expand-and-contract pattern:

- **Expand** (this release): add new columns with defaults, new tables,
  new indexes. The old server ignores schema it doesn't know about.
- **Contract** (next release): drop old columns, remove deprecated
  tables, tighten constraints. Safe because no server running the
  previous code is still alive.

The compatibility window is N/N-1: each release's migrations must work
with the previous release's code. Cleanup of deprecated schema ships in
the following release.

#### CI Enforcement

A `migration-compat` CI job enforces backward compatibility mechanically
rather than relying on review discipline. When a PR touches migration
files, CI applies all migrations (including the new ones) to a fresh
database, then checks out the latest release tag and runs its database
tests against the migrated schema. If the old code's tests pass, the
migration is backward-compatible by definition.

```yaml
migration-compat:
  if: # PR touches internal/db/migrations/
  steps:
    - uses: actions/checkout@v4
    - name: Apply migrations from PR
      run: go test -run TestMigrateUp ./internal/db/...
    - name: Checkout previous release
      run: |
        PREV_TAG=$(git describe --tags --abbrev=0 HEAD~1 --match 'v*')
        git checkout "$PREV_TAG" -- internal/db/*_test.go internal/db/db.go
    - name: Run old tests against migrated schema
      run: go test ./internal/db/...
```

This catches issues no SQL linter would flag — a column rename that
breaks a hardcoded query, a `NOT NULL` addition without a default, a
dropped column still referenced in a `SELECT`. Conversely, a `DROP
COLUMN` that the old code never references passes cleanly.

**Complement: `atlas migrate lint`** runs as a fast pre-check before the
full compatibility test. It catches common mistakes (missing defaults,
destructive DDL, transaction gaps) in seconds, providing earlier feedback
while the heavier test runs. It supports golang-migrate format and both
PostgreSQL and SQLite.

**Pre-migration backup:** `by admin update` backs up the database before
starting the new container. For SQLite: file copy. For PostgreSQL:
`pg_dump` to a timestamped file. If the new server fails to start, the
backup and old image tag are available for manual recovery.

**Skip-version upgrades:** migrations are sequential and
`golang-migrate` runs all pending. Skipping from v1 to v3 applies
migrations in order. No special handling needed.

### Server-Worker API Compatibility

Rolling updates require that the new server can manage workers spawned
by the old server. The worker-facing surface is small:

| Surface | Contract |
|---|---|
| Token file | JWT at `{token_dir}/token`, read by worker on each API call |
| `POST /api/v1/packages` | Worker sends package install request, authed via token |
| Health probe | HTTP GET to worker's Shiny port, expects 200 |
| Mount layout | App code at `{bundle_worker_path}/`, library at `/blockyard-lib` |
| Environment variables | `BLOCKYARD_WORKER_ID`, `BLOCKYARD_APP_ID`, etc. |
| Network topology | Per-worker bridge, server joined to each |

**Within a major version:** this contract is stable. Changes must be
additive and backward-compatible. Given the small surface, this is not a
significant constraint.

**Across major versions:** rolling updates are not supported. Major
upgrades use the basic restart path.

## `by admin update` Command

The CLI command orchestrates the full flow. It requires access to the
Docker daemon (via socket) and to the blockyard API.

```
by admin update [--channel stable|main] [--yes]
```

### Precondition Check

Before doing anything, the command verifies:

1. A newer version is available (GitHub Releases API, respecting
   channel). If up to date, exit.
2. Redis is configured and reachable. If not, print the manual compose
   commands and exit.

The server does not attempt to detect whether the proxy supports service
discovery — that's a property of the proxy's configuration, not
something blockyard can reliably probe. If the proxy is statically
configured, the new container won't receive traffic and the `/readyz`
poll in step 4 will time out. The update fails safely: the old server
keeps running, nothing breaks. The documentation notes the proxy
requirement; the precondition check does not enforce it.

### Rolling Update Flow

```
by admin update
  1. Pull         → docker pull ghcr.io/cynkra/blockyard:<tag>
  2. Backup       → copy SQLite file or pg_dump
  3. Start new    → start new container (same network, same proxy labels)
  4. Wait ready   → poll /readyz on new container
  5. Drain old    → SIGUSR1 old server (health checks fail, proxy shifts)
  6. Wait exit    → old container drains and exits
  7. Cleanup      → remove old container, verify health
```

**Step 3–4:** the new server starts, connects to Redis (finds existing
worker state), runs any pending migrations, joins worker networks, and
starts its background goroutines (health poller, autoscaler, token
refreshers). It becomes ready when `/readyz` returns 200.

During the overlap between steps 3 and 5, the proxy load-balances across
both servers. This is safe because they share all routing state via
Redis. Both can serve any request.

**Step 5:** `by admin update` sends SIGUSR1 to the old server process.
The old server enters drain mode: health endpoints return 503
immediately, the proxy detects this and stops routing new requests to it.
No proxy-specific API call or label change is needed — the health check
is the universal cutover signal.

**Step 6:** the old server finishes draining in-flight requests and
exits. Workers remain running, managed by the new server via Redis.

### Failure Modes

- **Pull fails** → abort, nothing changed.
- **New server fails to start** → stop new container, old server still
  running. Print rollback instructions (backup location, old image tag).
- **New server never becomes ready** → timeout, stop new container, old
  server still serving. No user impact.
- **Old server won't drain** → after timeout, force-kill. Workers are
  safe — already managed by new server via Redis.

### Rollback

Not automated. The operator restores the database backup and runs the
old image. `by admin update` prints the exact commands needed.
Automated rollback is deferred — it requires deciding what to do with
migrations that already ran and workers that the new server may have
spawned.

## What This Does Not Cover

- **Automatic rollback** — manual for now, see above.
- **Config migration** — if `blockyard.toml` schema changes between
  versions, the operator updates the config manually. The server
  validates config on startup and fails with clear messages.
