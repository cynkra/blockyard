# blockyard v3 Implementation Plan

This document is the build plan for v3 — operations and runtime. It covers
new packages, dependency additions, config changes, schema changes, build
phases, key type definitions, and test strategy. The roadmap
(`../roadmap.md`) is the source of truth for *what* v3 includes; the draft
(`draft.md`) and update design (`../update.md`) describe the *why* and
design rationale; this document describes *how* to build it.

v3 has two tracks. The **operations track** adds rolling updates with zero
downtime: interface extraction for session/worker stores, Redis-backed
shared state, worker token persistence, drain mode, and the `by admin
update` CLI command. The **runtime track** adds the process backend
(bubblewrap sandboxing), the zygote worker model for Docker, and
per-app container configuration (data mounts, multiple execution images,
dynamic resource limits, per-app OCI runtime selection).

The operations track runs first — once v2 is deployed, low-friction
updates are immediately needed, and the shared state layer directly serves
v4 clustering.

## New Packages

v3 adds the following packages. Existing packages are extended in place.

```
cmd/
├── blockyard/                     # (existing) server binary — drain mode, passive mode
├── by/                            # (existing) CLI binary — new admin subcommand group
│   └── admin.go                   # NEW: by admin update
└── by-builder/                    # (existing) unchanged

internal/
├── backend/
│   ├── docker/                    # (existing) — zygote worker model additions
│   └── process/                   # NEW: bubblewrap process backend
│       ├── process.go             # Backend interface implementation
│       ├── bwrap.go               # bubblewrap command construction
│       └── seccomp.go             # seccomp profile loading and validation
├── session/
│   ├── store.go                   # (existing) → renamed to memory.go, implements SessionStore
│   ├── iface.go                   # NEW: SessionStore interface definition
│   └── redis.go                   # NEW: Redis-backed SessionStore implementation
├── registry/
│   ├── registry.go                # (existing) → renamed to memory.go, implements WorkerRegistry
│   ├── iface.go                   # NEW: WorkerRegistry interface definition
│   └── redis.go                   # NEW: Redis-backed WorkerRegistry implementation
├── server/
│   ├── state.go                   # (existing) — WorkerMap → interface + memory impl
│   ├── workermap_iface.go         # NEW: WorkerMap interface definition
│   ├── workermap_memory.go        # NEW: in-memory WorkerMap (extracted from state.go)
│   └── workermap_redis.go         # NEW: Redis-backed WorkerMap implementation
├── redisstate/                    # NEW: shared Redis connection management
│   └── redisstate.go              # Redis client setup, health check, availability detection
├── drain/                         # NEW: drain mode orchestration
│   └── drain.go                   # SIGUSR1 handler, drain sequence, passive→active transition
└── zygote/                        # NEW: zygote worker model
    ├── control.go                 # TCP control protocol client (phase 3-9)
    ├── manager.go                 # session↔child bookkeeping + sweep loop
    ├── secret.go                  # per-worker control secret
    ├── zygote.R                   # embedded R script (socketSelect polling)
    ├── zygote_helper.c            # KSM helper (prctl wrapper)
    ├── embed.go                   # //go:embed declarations
    └── sandbox.go                 # NEW in phase 3-10: post-fork sandboxing
```

## New Dependencies

```go
// go.mod additions — existing deps unchanged

// Redis
require (
    github.com/redis/go-redis/v9  v9.x  // Redis client; used for shared state
)

// Process backend (no Go deps — bwrap is an external binary)
// Seccomp profile is a JSON file, parsed with encoding/json (stdlib)
```

**Dependency rationale:**

- **go-redis** — the standard Go Redis client. Supports all Redis data
  structures needed (strings, hashes, key TTLs, pub/sub for future use).
  No connection pool tuning needed for the expected load (hundreds of
  ops/sec, not thousands). The `v9` API uses `context.Context` throughout.

## v3 Config Additions

```toml
[server]
# NEW: drain mode timeout. How long to wait for in-flight requests to
# complete after entering drain mode (SIGUSR1). Separate from
# shutdown_timeout (SIGTERM) because drain mode leaves workers alive.
drain_timeout = "30s"

[redis]
# NEW: optional. When set, enables Redis-backed shared state for
# session routing, worker registry, and worker map. Required for
# rolling updates.
url = ""               # Redis connection string; use BLOCKYARD_REDIS_URL env var
# key_prefix allows multiple blockyard instances to share a Redis.
# Default: "blockyard:". Most deployments leave this alone.
key_prefix = "blockyard:"

[docker]
# NEW: per-app override stored in DB; server-wide default unchanged.
# runtime field selects OCI runtime (e.g., "kata-runtime", "runc").
runtime = ""           # empty = Docker daemon default

[process]
# NEW: process backend configuration. Only used when backend = "process".
bwrap_path = "/usr/bin/bwrap"       # path to bubblewrap binary
seccomp_profile = ""                # path to custom seccomp profile; empty = built-in default
r_path = "/usr/bin/R"               # path to R binary
```

The `[server] backend` field (currently always `"docker"`) gains
`"process"` as a valid value. The backend selection determines which
`Backend` implementation is instantiated at startup.

Per-app fields stored in the `apps` table (not in TOML):

- `image` — per-app Docker image override (default: server-wide
  `[docker] image`)
- `runtime` — per-app OCI runtime override (default: server-wide
  `[docker] runtime`)
- `data_mounts` — JSON array of mount specifications (default: `[]`)

## Schema Changes

### SQLite + PostgreSQL (shared)

**Migration 007: v3 app features**

```sql
ALTER TABLE apps ADD COLUMN image TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN runtime TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN data_mounts TEXT NOT NULL DEFAULT '[]';
```

`image` and `runtime` are empty-string-means-use-server-default, matching
the existing `memory_limit` / `cpu_limit` pattern. `data_mounts` is a JSON
array stored as TEXT, validated at the application layer.

No Redis-related schema changes — Redis state is ephemeral by design. No
migration for the worker token key — it's stored in OpenBao or a file, not
the database.

### Admin-Defined Mount Sources (config only)

```toml
[[storage.data_mounts]]
name = "models"
path = "/data/shared-models"

[[storage.data_mounts]]
name = "scratch"
path = "/data/scratch"
```

These are config-only — no schema changes. App-level mount specifications
reference sources by name and are validated against this whitelist at the
API boundary.

## Build Phases

### Phase 3-1: Migration Discipline

Establish the rules, documentation, and CI enforcement for
backward-compatible schema migrations. Rolling updates (phase 3-5)
require the old server to continue reading and writing the database
after the new server's migrations have run. Every migration must be
backward-compatible with the previous release (N/N-1 compatibility
window). This phase lands first — it protects every subsequent
migration, including the v3 ones.

**Deliverables:**

1. **Migration authoring guide** (`docs/design/migrations.md`) —
   the canonical reference for writing blockyard migrations. Covers:

   **The expand-and-contract pattern:**
   - **Expand** (this release): additive changes only. The old server
     must be able to read and write the database after these run.
   - **Contract** (next release): remove deprecated schema. Safe
     because no server running the previous code is still alive.

   **Allowed operations (expand phase):**
   - `ADD COLUMN` with a `DEFAULT` value (or nullable)
   - `CREATE TABLE`
   - `CREATE INDEX` (non-unique, or unique on new tables only)
   - `CREATE INDEX CONCURRENTLY` (PostgreSQL; avoids table locks)
   - `ADD CHECK` constraint with `NOT VALID` (PostgreSQL; deferred
     validation)

   **Prohibited operations (without a paired contract in the next
   release):**
   - `DROP COLUMN` — old server may SELECT or INSERT it
   - `RENAME COLUMN` — old server references the old name
   - `ALTER COLUMN ... TYPE` — old server assumes the old type
   - `DROP TABLE` — unless created in the same migration batch
   - `ALTER TABLE ... ADD ... NOT NULL` without `DEFAULT` — old server
     INSERTs will fail
   - `RENAME TABLE` — old server references the old name
   - `DROP INDEX` on an index the old server relies on for performance

   **Migration file conventions:**
   - Sequential numbering: `NNN_description.up.sql` /
     `NNN_description.down.sql`
   - Both up and down files must exist. Down migrations are a
     production path (`by admin rollback`), not just a dev tool.
     Irreversible migrations (e.g., data backfills) must be explicitly
     marked `-- irreversible: <reason>` — this blocks automated
     rollback past that point and is flagged in CI.
   - Both SQLite and PostgreSQL tracks must have matching migration
     numbers and equivalent intent
   - One logical change per migration number — don't bundle unrelated
     DDL
   - Comments explaining *why* for non-obvious choices

   **Contract phase procedure:**
   - The release notes for the expand phase document what will be
     contracted in the next release
   - The contract migration references the expand migration number:
     `-- contracts: 007 (v3.0)`
   - Before merging a contract, verify no deployed server runs the
     expand-phase code (one full release cycle must have passed)

2. **SQL DDL linter** (`internal/db/lint_test.go`) — a Go test that
   parses migration `.up.sql` files and rejects prohibited patterns.
   This is the fastest feedback loop — runs in seconds with no database.

   The linter parses SQL statements (regex-based, not a full parser —
   sufficient for DDL) and checks:
   - `DROP COLUMN` → error
   - `RENAME COLUMN` / `ALTER COLUMN ... RENAME` → error
   - `ALTER COLUMN ... TYPE` → error
   - `DROP TABLE` → error (unless table was `CREATE`d in the same file)
   - `NOT NULL` on `ALTER TABLE ... ADD COLUMN` without `DEFAULT` → error
   - `RENAME TABLE` → error

   Each rule can be suppressed with a comment on the preceding line:
   `-- lint:allow <rule> — <reason>`. The suppression itself is logged
   in CI output so it's visible during review.

   ```go
   func TestMigrationSafety(t *testing.T) {
       for _, dialect := range []string{"sqlite", "postgres"} {
           files := glob(migrationsDir(dialect), "*.up.sql")
           for _, f := range files {
               violations := lintMigration(f)
               for _, v := range violations {
                   t.Errorf("%s:%d: %s", f, v.Line, v.Message)
               }
           }
       }
   }
   ```

3. **`atlas migrate lint` integration** — runs as a CI step before the
   heavier compatibility test. Atlas supports golang-migrate format and
   both SQLite and PostgreSQL. Catches issues the regex linter misses:
   missing transaction boundaries, data-dependent DDL, implicit lock
   escalation.

   ```yaml
   - name: Atlas lint (SQLite)
     run: atlas migrate lint --dir file://internal/db/migrations/sqlite
       --dev-url "sqlite://dev?mode=memory" --latest 1
   - name: Atlas lint (PostgreSQL)
     run: atlas migrate lint --dir file://internal/db/migrations/postgres
       --dev-url "postgres://..." --latest 1
   ```

4. **`migration-compat` CI job** — the definitive backward-compatibility
   check. When a PR touches migration files:

   a. Apply all migrations (including the new ones) to fresh databases
      (both SQLite and PostgreSQL).
   b. Check out the latest release tag's database test files.
   c. Run the old code's database tests against the migrated schema.
   d. If old tests pass → migration is backward-compatible.

   ```yaml
   migration-compat:
     if: contains(github.event.pull_request.changed_files, 'internal/db/migrations/')
     strategy:
       matrix:
         dialect: [sqlite, postgres]
     steps:
       - uses: actions/checkout@v4
       - name: Apply migrations from PR branch
         run: go test -run TestMigrateUp -tags ${{ matrix.dialect }}_test ./internal/db/...
       - name: Checkout previous release test code
         run: |
           PREV_TAG=$(git describe --tags --abbrev=0 HEAD~1 --match 'v*')
           git checkout "$PREV_TAG" -- internal/db/*_test.go internal/db/db.go
       - name: Run old tests against migrated schema
         run: go test -tags ${{ matrix.dialect }}_test ./internal/db/...
   ```

   This catches issues no static linter can: a column rename that
   breaks a hardcoded query, a new default value that changes old
   code's expected behavior, a constraint that rejects old code's
   INSERTs.

5. **Up-down-up roundtrip test** (`TestMigrateRoundtrip`) — applies all
   migrations up, then all down, then all up again. Verifies that down
   migrations are correct inverses and the schema is stable after a
   round trip. Runs on both dialects.

   ```go
   func TestMigrateRoundtrip(t *testing.T) {
       db := openTestDB(t)
       migrateUp(t, db)
       schemaAfterUp := dumpSchema(t, db)
       migrateDown(t, db)
       migrateUp(t, db)
       schemaAfterRoundtrip := dumpSchema(t, db)
       if schemaAfterUp != schemaAfterRoundtrip {
           t.Errorf("schema differs after up-down-up roundtrip")
       }
   }
   ```

6. **Migration file convention check** (CI step) — a shell script or
   Go test that verifies:
   - Every `.up.sql` has a matching `.down.sql` (and vice versa)
   - Migration numbers are sequential with no gaps
   - SQLite and PostgreSQL tracks have matching migration numbers
   - No migration file is empty (must contain at least one statement
     or an explicit `-- no-op: <reason>` comment)

7. **Pre-migration backup utility** (`internal/db/backup.go`) — used by
   `by admin update` (phase 3-5) before starting the new server:
   - SQLite: atomic file copy to `{path}.backup.{RFC3339 timestamp}`
   - PostgreSQL: `pg_dump --format=custom` to timestamped file
   - Returns the backup path so the caller can print it

**Enforcement layers (fastest to slowest):**

| Layer | What it catches | When it runs |
|---|---|---|
| SQL DDL linter | Prohibited DDL patterns | `go test`, <1s |
| File convention check | Missing pairs, numbering gaps | CI, <1s |
| `atlas migrate lint` | Missing defaults, lock issues, transactions | CI, ~5s |
| Up-down-up roundtrip | Broken down migrations, schema drift | CI, ~10s |
| `migration-compat` | Actual N-1 code breakage | CI, ~60s |

### Phase 3-2: Interface Extraction & Token Persistence

Two prerequisites for shared state: extract interfaces so Redis can
implement the same contracts, and persist the worker token signing key
so both servers verify the same tokens during a rolling update.

**Deliverables:**

1. **SessionStore interface** (`internal/session/iface.go`) — extract
   from the existing `Store` type:

   ```go
   type SessionStore interface {
       Get(sessionID string) (Entry, bool)
       Set(sessionID string, entry Entry)
       Touch(sessionID string) bool
       Delete(sessionID string)
       DeleteByWorker(workerID string) int
       CountForWorker(workerID string) int
       CountForWorkers(workerIDs []string) int
       RerouteWorker(oldWorkerID, newWorkerID string) int
       EntriesForWorker(workerID string) map[string]Entry
       SweepIdle(maxAge time.Duration) int
   }
   ```

   The existing `Store` struct is renamed to `MemoryStore` and satisfies
   this interface. The constructor becomes `NewMemoryStore()`.

2. **WorkerRegistry interface** (`internal/registry/iface.go`) — extract
   from the existing `Registry` type:

   ```go
   type WorkerRegistry interface {
       Get(workerID string) (string, bool)
       Set(workerID string, addr string)
       Delete(workerID string)
   }
   ```

   The existing `Registry` struct is renamed to `MemoryRegistry`. The
   constructor becomes `NewMemoryRegistry()`.

3. **WorkerMap interface** (`internal/server/workermap_iface.go`) —
   extract from the existing `WorkerMap` type:

   ```go
   type WorkerMap interface {
       Get(id string) (ActiveWorker, bool)
       Set(id string, w ActiveWorker)
       Delete(id string)
       Count() int
       CountForApp(appID string) int
       All() []string
       ForApp(appID string) []string
       ForAppAvailable(appID string) []string
       MarkDraining(appID string) []string
       SetDraining(workerID string)
       SetIdleSince(workerID string, t time.Time)
       SetIdleSinceIfZero(workerID string, t time.Time)
       ClearIdleSince(workerID string) bool
       IdleWorkers(timeout time.Duration) []string
       AppIDs() []string
       IsDraining(appID string) bool
   }
   ```

   The existing struct moves to `MemoryWorkerMap` in
   `workermap_memory.go`. Constructor becomes `NewMemoryWorkerMap()`.

4. **Server struct update** — change field types from concrete to
   interface:

   ```go
   type Server struct {
       // ...
       Workers  WorkerMap                  // was *WorkerMap
       Sessions session.SessionStore       // was *session.Store
       Registry registry.WorkerRegistry    // was *registry.Registry
       // ...
   }
   ```

5. **Call site updates** — mechanical: every `srv.Sessions.X()`,
   `srv.Workers.X()`, `srv.Registry.X()` call already uses the method
   set defined by the interfaces. Compilation verifies completeness.

6. **Worker token: OpenBao storage** — when `[openbao]` is configured,
   on first startup generate the key and store it at
   `secret/data/blockyard/worker-signing-key`. On subsequent startups,
   read it back. Uses the existing `integration.Client`.

7. **Worker token: file-based fallback** — when OpenBao is not
   configured, write the key to `{storage.bundle_server_path}/.worker-key`.
   Read on startup if the file exists; generate and write if it doesn't.
   File permissions: `0600`.

8. **Startup flow change** — in `cmd/blockyard/main.go`, replace the
   current `crypto/rand` key generation:

   ```go
   // Before (v2):
   workerKey := auth.NewSigningKey(randomBytes)

   // After (v3):
   workerKey, err := loadOrCreateWorkerKey(srv.VaultClient, cfg)
   ```

   The helper tries OpenBao first, falls back to file, falls back to
   generating a new key (and persisting it via the available path).

9. **Tests** — interface compliance tests
   (`var _ SessionStore = (*MemoryStore)(nil)`, etc.). Worker token
   round-trip test (write → read → verify same key) for both OpenBao
   and file paths. Integration test with mock vault client.

### Phase 3-3: Redis Shared State

Implement Redis-backed versions of all three interfaces. Add Redis
connection management. The server detects Redis availability at startup
and selects the appropriate backend.

**Deliverables:**

1. **Redis client package** (`internal/redisstate/`) — manages the
   shared `*redis.Client`, health check, and key prefix. Exposes a
   `New(ctx, cfg *config.RedisConfig) (*Client, error)` constructor
   (ctx bounds the initial PING) and a `Ping(ctx) error` health check.
   The config struct:

   ```go
   type RedisConfig struct {
       URL       string `toml:"url"`
       KeyPrefix string `toml:"key_prefix"`
   }
   ```

2. **Redis SessionStore** (`internal/session/redis.go`) — implements
   `SessionStore` using Redis hashes. Each session is a hash at
   `{prefix}session:{sessionID}` with fields `worker_id`, `user_sub`,
   `last_access`. TTL matches `session_idle_ttl`. `DeleteByWorker` uses
   a Lua script for atomicity (scan + delete). `SweepIdle` is handled
   by Redis TTL expiry — the method becomes a no-op (TTLs do the work).

3. **Redis WorkerRegistry** (`internal/registry/redis.go`) — implements
   `WorkerRegistry` using simple key-value pairs at
   `{prefix}registry:{workerID}`. TTL-based — entries expire if not
   refreshed (heartbeat from health poller).

4. **Redis WorkerMap** (`internal/server/workermap_redis.go`) —
   implements `WorkerMap` using Redis hashes. Each worker is a hash at
   `{prefix}worker:{workerID}` with fields `app_id`, `bundle_id`,
   `draining`, `idle_since`, `started_at`, `server_id` (hostname of
   the owning server — useful for debugging during rolling updates).
   List operations (`All`, `ForApp`, etc.) use `SCAN` with pattern
   matching. `CancelToken` is not serializable — it remains local to
   the owning server instance.

5. **Server startup selection** — in `cmd/blockyard/main.go`.
   `NewServer()` initialises memory stores by default; when Redis is
   configured the stores are overwritten:

   ```go
   if cfg.Redis != nil {
       rc, err := redisstate.New(ctx, cfg.Redis)
       srv.RedisClient = rc
       srv.Sessions = session.NewRedisStore(rc, cfg.Proxy.SessionIdleTTL.Duration)
       srv.Registry = registry.NewRedisRegistry(rc, 3*cfg.Proxy.HealthInterval.Duration)
       hostname, _ := os.Hostname()
       srv.Workers = server.NewRedisWorkerMap(rc, hostname)
   }
   ```

6. **`/readyz` integration** — when Redis is configured, include Redis
   connectivity in the readiness check.

7. **Redis network isolation** — workers must not reach Redis.

   Workers are already isolated by construction: each gets its own
   per-worker bridge network and can only reach containers explicitly
   connected to it (service containers, the server). Redis is never
   connected to any worker network, so workers cannot reach it — unless
   the operator puts Redis on the `ServiceNetwork`.

   **Preflight check:** `checkRedisOnServiceNetwork` (in
   `internal/preflight/docker_checks.go`) inspects the service network
   at startup and rejects if any container matching the Redis hostname
   or IP is found on it.

   **Deployment guidance:** use a separate Docker network for Redis:

   ```yaml
   services:
     blockyard:
       networks: [state, default]
     redis:
       networks: [state]
   networks:
     state:
       internal: true
   ```

   **Defense in depth:** Redis AUTH recommended — startup logs a warning
   when the connection has no password. No Redis data in worker-visible
   surfaces.

8. **Tests** — Redis integration tests (tagged `redis_test`) against a
   real Redis service container in CI. Unit tests use miniredis for each
   store method. Concurrent access tests (race-detected) for all three
   stores verify Lua script atomicity and absence of data races.

### Phase 3-4: Drain Mode & Server Handoff

Add `SIGUSR1` drain mode for rolling updates and improve `SIGTERM`
shutdown with health-check-aware draining.

**Deliverables:**

1. **Drain package** (`internal/drain/`) — orchestrates the drain
   sequence:

   ```go
   type Drainer struct {
       MainServer *http.Server
       MgmtServer *http.Server   // may be nil
       BGCancel   context.CancelFunc
       BGWait     *sync.WaitGroup
       DB         io.Closer
       Timeout    time.Duration
   }

   func (d *Drainer) Drain(ctx context.Context)    // SIGUSR1: workers survive
   func (d *Drainer) Shutdown(ctx context.Context, srv *server.Server) // SIGTERM: full stop
   ```

2. **Signal handling** — in `main.go`, distinguish `SIGUSR1` (drain)
   from `SIGTERM`/`SIGINT` (shutdown).

3. **Health endpoint gating** — `atomic.Bool` draining flag on the
   server. When set, `/healthz` and `/readyz` return 503. Set at the
   start of both drain and shutdown, before HTTP server shutdown begins.

4. **Passive mode** — `BLOCKYARD_PASSIVE=1` env var. Server serves
   requests but does not start background goroutines.
   `POST /api/v1/admin/activate` (admin auth, returns 200/409/500)
   starts them. Prevents two sets of background loops during overlap.

5. **Tests** — drain sequence ordering, passive mode verification,
   activation endpoint.

### Phase 3-5: Rolling Update Command

The CLI commands that orchestrate rolling updates and rollbacks.

**Deliverables:**

1. **`by admin` subcommand group** — new cobra command group.

2. **`by admin update`** — full rolling update flow:

   ```
   by admin update [--channel stable|main] [--yes] [--watch=5m]
     1. Check for newer version (GitHub Releases API)
     2. Verify Redis is configured and reachable
     3. Pull new image
     4. Back up database + record migration version in metadata
     5. Start new container (passive mode, same network/labels)
     6. Poll /readyz on new container until 200
     7. SIGUSR1 old server (drain mode)
     8. Wait for old container to exit
     9. POST /api/v1/admin/activate on new server
    10. Remove old container, verify health
   ```

3. **`by admin rollback`** — N-1 rollback via the same infrastructure:

   ```
   by admin rollback [--yes]
     1. Read backup metadata (image tag, migration version)
     2. Verify Redis is configured and reachable
     3. Pull old image, start old container (passive)
     4. Run down migrations to recorded version
     5. Poll /readyz, drain current, activate old, cleanup
   ```

4. **Docker interaction** — Docker SDK for containers, signals, health
   polling. Container ID from labels or `--container` flag.

5. **Failure handling** — each step has a defined fallback. Down
   migration failures abort with backup location for manual restore.

6. **`--watch` flag** — post-update health monitoring with automatic
   rollback on failure.

7. **Scheduled auto-updates** — server-side config:

   ```toml
   [update]
   schedule = ""          # cron expression; empty = disabled
   channel = "stable"
   watch_period = "5m"
   ```

   Server orchestrates the update internally, then transitions to
   watchdog mode (HTTP stopped, orchestration goroutine alive) to
   monitor the new server and rollback on failure.

8. **Tests** — update, rollback, and watch-triggered-rollback sequences
   with mock Docker client. Scheduled update trigger conditions.

### Phase 3-6: Data Mounts & Per-App Configuration

Per-app container configuration: data mounts, execution environment
images, OCI runtime selection, and dynamic resource limit updates.
These all follow the same pattern: per-app field in the DB, field in
`WorkerSpec`, backend reads it at spawn time.

**Deliverables:**

1. **Admin-defined mount sources** — parse `[[storage.data_mounts]]`
   from TOML. Validate names are unique, paths are absolute.

2. **App-level mount specification** — `app_data_mounts` table with
   `(app_id, target)` PK and FK to `apps(id)`:

   ```go
   type DataMount struct {
       Source   string `json:"source"`   // "models" or "models/v2"
       Target   string `json:"target"`   // "/data/models"
       ReadOnly *bool  `json:"readonly"` // default true
   }
   ```

3. **Mount validation** — source exists in admin whitelist, no `..` in
   subpath, target doesn't collide with reserved paths, no duplicates.

4. **Mount backend integration** — `WorkerSpec.DataMounts` field. Docker
   backend: direct bind-mount strings (host paths bypass `MountConfig`
   translation). Process backend: bwrap `--bind` / `--ro-bind`.

5. **Multiple execution environment images** — `image` field on app
   config (migration 002). `PATCH /api/v1/apps/{id}` accepts `image`.
   Docker backend reads `WorkerSpec.Image`, falls back to server-wide
   default. `by update --image <ref>` CLI support.

6. **Per-app OCI runtime selection** — `runtime` field on app config
   (migration 002). Docker backend sets `HostConfig.Runtime` when set.
   Runtime changes require admin (the runtime controls the container
   isolation boundary — runc vs kata vs sysbox — so it is a
   security-sensitive setting).

   **Runtime fallback chain** — the effective runtime for a worker is
   resolved by `AppRuntime()` in order:

   1. Per-app `runtime` column (admin-set override via PATCH or
      `by scale --runtime`).
   2. `docker.runtime_defaults` — a TOML map from access type to
      runtime, e.g. `public = "kata-runtime"`. This lets operators
      enforce stricter isolation for public-facing apps without
      touching each app individually.
   3. `docker.runtime` — server-wide default (empty = Docker daemon
      default).

   `runtime_defaults` keys are validated against the set of access
   types (`acl`, `logged_in`, `public`).

7. **Dynamic resource limit updates** — new Backend interface method:

   ```go
   UpdateResources(ctx context.Context, id string, mem int64, nanoCPUs int64) error
   ```

   Docker backend: `client.ContainerUpdate()`. Process backend: returns
   `ErrNotSupported`. In `UpdateApp`, when limits change, call
   `UpdateResources` for each running worker (best-effort — persisted
   in DB regardless).

8. **Tests** — mount validation (path traversal, reserved paths).
   Docker integration: verify mounts, image override, runtime override,
   and live resource limit changes in container inspect.

### Phase 3-7: Process Backend Core

Implement the `Backend` interface using bubblewrap. This phase covers
the core implementation; packaging and deployment are phase 3-8.
Also decouples the codebase from Docker-specific assumptions to
prepare for future backends (k8s).

**Deliverables:**

1. **Process backend** (`internal/backend/process/`) — implements all
   `Backend` methods:

   ```go
   type ProcessBackend struct {
       cfg     config.ProcessConfig
       seccomp string
   }

   func (b *ProcessBackend) Spawn(ctx, spec) error
   func (b *ProcessBackend) Stop(ctx, id) error
   func (b *ProcessBackend) HealthCheck(ctx, id) bool
   func (b *ProcessBackend) Logs(ctx, id) (LogStream, error)
   func (b *ProcessBackend) Addr(ctx, id) (string, error)
   func (b *ProcessBackend) Build(ctx, spec) (BuildResult, error)
   func (b *ProcessBackend) ListManaged(ctx) ([]ManagedResource, error)
   func (b *ProcessBackend) RemoveResource(ctx, r) error
   func (b *ProcessBackend) WorkerResourceUsage(ctx, id) (*WorkerResourceUsageResult, error)
   func (b *ProcessBackend) CleanupOrphanResources(ctx) error
   func (b *ProcessBackend) Preflight(ctx) (*preflight.Report, error)
   ```

2. **bwrap command construction** — PID namespace (`--unshare-pid`),
   filesystem isolation (`--ro-bind` app + library, `--tmpfs /tmp`),
   `--new-session`, `--die-with-parent`, seccomp via `--seccomp`,
   capability dropping.

3. **Worker lifecycle** — child processes tracked by PID. Health check:
   HTTP GET to `localhost:{port}`. Logs: stdout/stderr to managed files.
   Stop: `SIGTERM` → `SIGKILL` after timeout.

4. **Build support** — bwrap-sandboxed builds with write access to build
   directory and pak cache. Same BuildSpec → R script → pak flow.

5. **Backend selection** — `cmd/blockyard/main.go` instantiates
   `ProcessBackend` when `[server] backend = "process"`.

6. **Backend interface decoupling** — rename `ContainerStats` →
   `WorkerResourceUsage`; add `CleanupOrphanResources()` and
   `Preflight()` to `Backend` interface; move `ParseMemoryLimit` to
   `internal/units`; move `default_memory_limit`, `default_cpu_limit`
   to `[server]` config; move `store_retention` to `[storage]` config;
   rename `skip_docker_preflight` → `skip_preflight`. Goal: no code
   outside `internal/backend/docker/` imports that package.

7. **Tests** — unit tests for bwrap argument construction. Integration
   tests (tagged `process_test`): spawn worker, verify health check,
   verify filesystem isolation, verify cleanup. Skipped when bwrap is
   unavailable.

### Phase 3-8: Process Backend Packaging & Deployment

Deployment artifacts and documentation for the process backend.

**Deliverables:**

1. **Custom seccomp profiles** — two JSON files based on Docker's
   default. An outer-container profile at
   `internal/seccomp/blockyard-outer.json` relaxes `clone`/`clone3`/
   `unshare`/`setns` so bwrap's `--unshare-user` works under Docker's
   seccomp default without `CAP_SYS_ADMIN`. A bwrap-internal profile
   at `internal/seccomp/blockyard-bwrap.json` is compiled to BPF at
   build time and loaded by bwrap via `--seccomp <fd>` to sandbox
   the worker R process itself. Both are generated from
   vendored-upstream + overlay via `make regen-seccomp`. The outer
   profile is shared with the zygote model (phase 3-10).

2. **Variant Docker images (three, not one)** — `blockyard-docker`
   (slim, docker backend only), `blockyard-process` (R + bwrap +
   compiled BPF, process backend only), and `blockyard` (everything:
   R + bwrap + iptables, both backends). The process and everything
   variants apply the outer seccomp profile at deploy time via
   `--security-opt seccomp=...`. No Docker socket, no
   `CAP_SYS_ADMIN`. Published alongside each other; `:latest`
   points at the everything variant.

3. **Native deployment mode** — documentation for bare Linux hosts.
   Prerequisites checklist: R, bwrap, system libraries. Resource limit
   guidance (process backend has no per-worker cgroups — relies on outer
   container or system limits).

4. **Multi-arch process-backend image** — the new process-backend
   Docker image (deliverable #2) is published for `linux/amd64` and
   `linux/arm64`, matching the existing Docker-backend image. The
   process backend is Linux-only (bwrap is unavailable on macOS), so
   no darwin server binaries are added — operators who want to run
   blockyard on a Mac use the Docker backend image via Docker Desktop.

5. **Process backend rolling updates** — orchestrator variant for
   process-backend deployments. The old server fork+execs a new
   blockyard process from the same binary with `BLOCKYARD_PASSIVE=1`
   and an alternate bind address (two-port parallel servers, not
   single-port handoff — this is what gives zero-interruption cutover).
   Both servers share state via Redis. An operator-run reverse proxy
   fronts both bind ports and routes by health, same shape as the
   Docker rolling update. After the new server passes `/readyz` and
   is activated, the old server drains: it stops accepting new
   sessions while existing ones complete naturally, then exits.
   Workers spawned by the old server die with it (Pdeathsig) but by
   that point no sessions are left on them; the new server's
   autoscaler rebuilds the pool from new traffic. Requires no
   external supervisor — `by admin update` is the single entry point
   for both Docker and process backends.

### Phase 3-9: Zygote Worker Model

Long-lived **zygote** R process (one per worker) that pre-loads a
bundle's packages, then forks per-session children on demand via
`parallel::mcfork`. Works on both Docker and process backends via an
optional `backend.Forking` capability interface. Ships the two
unconditional benefits — startup-latency elimination and per-session
isolation. Opt-in KSM memory sharing and post-fork sandboxing land
together in phase 3-10. **See `phase-3-9.md` for the full design and
wire protocol.**

**High-level deliverables (summary only; details live in `phase-3-9.md`):**

1. **`zygote` column + `backend.Forking` capability interface** — per-app
   opt-in, validated against the backend, guarded behind a server-wide
   `experimental.zygote` config flag.
2. **`internal/zygotectl/` + `internal/zygote/` packages** — control
   protocol client (`Client`, `Info`, `ChildExitMsg`, embedded
   `zygote.R`) and backend-agnostic `Manager` with session↔child
   bookkeeping, sweep loop, and exit-event handling. Split across two
   packages to break a `backend` ↔ control-client import cycle.
3. **Docker and process backend `Forking` implementations** — zygote
   spawn with control port, control client, control-connection watcher
   for unexpected disconnect, idempotent `Stop` with synthesised
   `ChildExit` events.
4. **Session-addressed routing** — `session.Entry.Addr` holds the
   per-child shiny address; proxy reads it directly, registry path
   unchanged for non-zygote sessions.
5. **Cleanup convergence** — three paths (child exit, worker stop,
   sweep loop) converge on the same manager bookkeeping cleanup.
6. **Tests** — control protocol unit tests, `INFO` round-trip, Docker
   and process integration tests (spawn → fork → kill → crash
   detection → control-loss eviction).

### Phase 3-10: Zygote Hardening & KSM

Two companion hardening tracks layered on top of the phase 3-9
mechanism: **post-fork sandboxing** (isolation) and **opt-in kernel
same-page merging** (memory sharing). They land together because
KSM's RSS-spike failure mode needs sandbox-level containment and
KSM's threat model shares the multi-tenant audit story with
sandboxing. See `phase-3-10-draft.md` for the full design, KSM
rationale, and observability.

**Post-fork sandboxing deliverables:**

1. **Per-child isolation** — each child applies isolation before
   starting Shiny:
   - `unshare(CLONE_NEWUSER | CLONE_NEWNS)` — private mount namespace
   - Private tmpfs at `/tmp`
   - seccomp-bpf filter (must allow `PR_SET_MEMORY_MERGE` for KSM)
   - `capset()` — drop all capabilities
   - `setrlimit()` — `RLIMIT_AS`, `RLIMIT_CPU`, `RLIMIT_NPROC`

2. **Docker security options** — container create call adds
   `--security-opt seccomp=blockyard-seccomp.json` (same profile as
   process backend, phase 3-8) and `--security-opt apparmor=unconfined`
   (Ubuntu 23.10+ only) when the zygote model is enabled.

3. **Environment variable hardening** — `OMP_NUM_THREADS=1` and
   `MKL_NUM_THREADS=1` in the template process before forking.

4. **Package compatibility documentation** — document the three
   categories: safe to pre-load (shiny, ggplot2, dplyr), dangerous to
   pre-load (arrow, torch, rJava — load in each child), and safe if
   not used before fork (DBI, RPostgres).

**KSM opt-in deliverables:**

5. **`ksm` column + `experimental.ksm` server flag** — second
   two-level opt-in, independent of `experimental.zygote`. API
   rejects `apps.ksm = true` without `apps.zygote = true` on the
   same app and without `experimental.ksm = true` in server config.
6. **`zygote_helper.c` + per-arch precompiled `.so`** — tiny
   dependency-free C helper loaded via `dyn.load` from `zygote.R`
   to call `prctl(PR_SET_MEMORY_MERGE)`. Embedded per-architecture
   via build-tag-guarded `//go:embed`.
7. **`STATS` control command + observability** — new command on the
   control protocol returning dynamic KSM merge counts from
   `/proc/<pid>/ksm_stat`. `zygote.Manager` gains a metrics-poll
   goroutine that updates labeled Prometheus gauges
   (`blockyard_zygote_ksm_merging_pages{app_id, worker_id}`) plus a
   host-global `blockyard_host_ksm_pages_sharing` scraper.
8. **KSM preflight checks** — each backend's `Preflight()` reads
   `/sys/kernel/mm/ksm/run` and `/sys/kernel/mm/ksm/pages_to_scan`,
   warns when ksmd is off or scanning at the desktop default. Gated
   on both server and per-app opt-in so operators who haven't opted
   into KSM see no preflight noise.
9. **Up-front bundle byte-compilation** — `zygote.R` compiles
   `global.R` / `app.R` via `compiler::cmpfile` before the fork so
   bundle closures are `BCODESXP` from birth. Prevents the JIT from
   dirtying shared closure pages post-fork (the dominant source of
   page divergence for user code).
10. **Children pin themselves at `oom_score_adj=1000`** — the kernel
    OOM killer reaps a child (one session, recoverable via the 307
    fallback) instead of the zygote (entire family) under the RSS
    spike that a coordinated GC burst can produce before ksmd
    catches up. Self-write, unprivileged, no capability coupling.

**Tests** — `/tmp` isolation between children, seccomp profile
active, `CLONE_NEWUSER` works inside the container, `prctl(PR_SET_MEMORY_MERGE)`
allowed by seccomp, KSM helper fallback on mocked `prctl` failure,
KSM-effectiveness integration test (fork two children, force
`gc(full=TRUE)`, poll `STATS` until `ksm_merging_pages_total > 0`,
skip on `/sys/kernel/mm/ksm/run == 0`).

## Build Order and Dependency Graph

```
Phase 3-1: Migration Discipline
  └── lands first — protects every subsequent migration

Phase 3-2: Interface Extraction & Token Persistence
  └── prerequisite for: phase 3-3 (Redis implements the interfaces)

Phase 3-3: Redis Shared State
  └── depends on: phase 3-2
  └── prerequisite for: phase 3-4 (passive mode), phase 3-5 (update cmd)

Phase 3-4: Drain Mode & Server Handoff
  └── depends on: phase 3-3 (passive mode needs Redis at startup)
  └── prerequisite for: phase 3-5 (update cmd sends SIGUSR1)

Phase 3-5: Rolling Update Command
  └── depends on: phases 3-2, 3-3, 3-4
  └── completes the operations track

Phase 3-6: Data Mounts & Per-App Configuration
  └── independent of: operations track
  └── can be developed in parallel after 3-1

Phase 3-7: Process Backend Core
  └── independent of: operations track
  └── can be developed in parallel after 3-1

Phase 3-8: Process Backend Packaging & Deployment
  └── depends on: phase 3-7 (needs the backend implementation)

Phase 3-9: Zygote Worker Model
  └── independent of: process backend (enhances Docker backend)
  └── can be developed in parallel with anything after 3-1

Phase 3-10: Zygote Hardening & KSM
  └── depends on: phase 3-9 (mechanism must exist)
  └── depends on: phase 3-8 (shares seccomp profile)
```

**Recommended order:**

1. Phase 3-1 (migration discipline) — land first
2. Phase 3-2 → 3-3 → 3-4 → 3-5 (operations track, sequential)
3. Phase 3-6 (per-app config) — in parallel with operations track
4. Phase 3-7 → 3-8 (process backend, sequential)
5. Phase 3-9 → 3-10 (zygote, sequential)

Phases 3-6, 3-7, and 3-9 are independent of each other and of the
operations track. They can be developed in parallel.

## Test Strategy

### Unit tests

- **Migration safety** — DDL linter, up-down-up roundtrip, file
  convention checks.
- **Interface compliance** — `var _ SessionStore = (*MemoryStore)(nil)`
  and equivalent for all pairs.
- **Redis stores** (tagged `redis_test`) — behavioral equivalence with
  memory stores, TTL expiry, Lua script atomicity, concurrent access.
- **Worker token persistence** — round-trip for OpenBao and file paths.
- **Drain sequence** — health returns 503 before HTTP shutdown. Passive
  mode skips goroutines. Activation starts them.
- **Data mount validation** — path traversal, reserved paths, unknown
  sources, duplicates.
- **bwrap command construction** — argument lists for various configs.

### Integration tests

- **Migration compatibility** (CI) — new migrations + old code's tests.
- **Redis shared state** (tagged `redis_test`) — cross-instance reads,
  TTL-based expiry.
- **Rolling update simulation** — two instances sharing Redis, SIGUSR1
  drain, traffic continuity.
- **Worker token persistence** — restart, verify tokens still valid.
- **Data mounts** (tagged `docker_test`) — mounts in Docker inspect.
- **Per-app image/runtime** (tagged `docker_test`) — correct image and
  runtime in Docker inspect.
- **Dynamic resource limits** (tagged `docker_test`) — ContainerUpdate
  reflected in inspect.
- **Process backend** (tagged `process_test`) — spawn, health, fs
  isolation, cleanup. Skipped without bwrap.
- **Zygote** (tagged `docker_test`) — zygote, fork, independent
  health checks, `/tmp` isolation, child cleanup.
- **Redis network isolation** (tagged `docker_test`) — worker cannot
  connect to Redis.

## Design Decisions

1. **Multi-layered migration enforcement.** Five layers from fastest to
   slowest: DDL linter (<1s), convention check (<1s), atlas lint (~5s),
   roundtrip (~10s), migration-compat (~60s). Each catches different
   classes of issues.

2. **Redis isolation via network topology, not just AUTH.** Workers
   execute arbitrary code — AUTH alone is insufficient. Primary defense:
   no network route from worker to Redis (Docker: dedicated internal
   network; process: Unix socket outside bwrap sandbox). AUTH is defense
   in depth.

3. **go-redis over raw RESP protocol.** Standard, well-maintained Go
   Redis client. No benefit to rolling our own.

4. **Lua scripts for atomic multi-key Redis operations.** Operations
   like `DeleteByWorker` must be atomic during the rolling update
   overlap.

5. **TTL-based session expiry in Redis.** Idiomatic Redis — `Touch`
   refreshes TTL, no sweep goroutine needed.

6. **Passive mode via environment variable.** `BLOCKYARD_PASSIVE=1` set
   by `by admin update` when starting the new container. Simpler than
   modifying the compose file or adding CLI flags.

7. **HTTP endpoint for activation.** `POST /api/v1/admin/activate`
   confirms success and surfaces errors. Auth is free — CLI already has
   a PAT and the new server's address from `/readyz` polling.

8. **CancelToken not serialized to Redis.** Go closure — can't be
   stored. The owning server manages token lifecycle locally.

9. **Process backend without per-worker cgroups.** Requires root or
   cgroupfs delegation. Process backend targets lightweight deployments
   where the outer container or system provides resource limits.

10. **Control channel for the zygote model.** TCP on the per-worker
    Docker bridge (or process-backend loopback), line-delimited
    protocol, pre-shared secret AUTH, `base::socketSelect`-driven
    poll loop on the R side. Unix sockets and `docker exec` were both
    rejected — see `phase-3-9.md` decision #4 for the reasoning
    (socket file permissions conflict with per-worker UIDs; `docker
    exec` has 50-200ms overhead per fork; `httpuv` is fork-unsafe).

11. **Interface extraction and token persistence as one phase.** Both
    are prerequisites for shared state with the same conceptual goal:
    enabling two server instances to coexist. Neither is large enough
    to warrant its own phase.

12. **Per-app config clustered with data mounts.** Multiple images,
    runtime selection, and dynamic resource limits all follow the same
    pattern: DB column → WorkerSpec field → backend reads at spawn.
    Grouping them avoids four tiny phases.
