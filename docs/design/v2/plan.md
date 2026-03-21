# blockyard v2 Implementation Plan

This document is the build plan for v2 — single-node production completeness.
It covers new packages, dependency additions, build phases, key type
definitions, schema changes, and test strategy. The roadmap (`../roadmap.md`)
is the source of truth for *what* v2 includes; this document describes *how*
to build it.

v2 builds on v1's infrastructure (OIDC, RBAC, OpenBao, autoscaling, web UI)
and adds usability improvements, safety nets, and blockr-specific features:
dual-database support (SQLite + PostgreSQL), bundle rollback, soft-delete,
resource limit enforcement, scale-to-zero with pre-warming, a cold-start
loading page, board storage (PostgreSQL + PostgREST + vault Identity OIDC),
build pipeline modernization (rv → pak), a content-addressable package
store, web UI expansion, and a CLI tool.

## New Packages

v2 adds the following packages to the existing layout. Existing packages
are extended in place.

```
cmd/
├── blockyard/                  # (existing) server binary
└── by/                         # NEW: CLI binary
    └── main.go                 # subcommands: deploy, list, logs, start, stop, ...

internal/
├── db/
│   ├── db.go                   # refactored: sqlx + dialect abstraction
│   ├── dialect.go              # SQLite/PostgreSQL dialect helpers
│   └── ...
├── pkgstore/
│   ├── store.go                # package store: content-addressable directory
│   └── view.go                 # per-worker library views (hard-linked trees)
├── proxy/
│   ├── ... (existing)
│   └── loading.go              # cold-start loading page handler
├── ui/
│   ├── ... (existing)
│   └── templates/
│       ├── loading.html        # cold-start spinner page
│       ├── app_settings.html   # per-app settings panel
│       └── app_logs.html       # per-app log viewer

migrations/
├── sqlite/
│   ├── 001_initial.up.sql      # existing v1 schema
│   ├── 001_initial.down.sql
│   ├── 002_v2_soft_delete.up.sql
│   ├── 002_v2_soft_delete.down.sql
│   ├── 003_v2_pre_warming.up.sql
│   └── 003_v2_pre_warming.down.sql
└── postgres/
    ├── 001_initial.up.sql      # existing v1 schema (PostgreSQL dialect)
    ├── 001_initial.down.sql
    ├── 002_v2_soft_delete.up.sql
    ├── 002_v2_soft_delete.down.sql
    ├── 003_v2_pre_warming.up.sql
    ├── 003_v2_pre_warming.down.sql
    ├── 004_v2_boards.up.sql    # board storage (PostgreSQL only)
    └── 004_v2_boards.down.sql
```

## New Dependencies

```go
// go.mod additions — existing deps unchanged

// Database
require (
    github.com/jmoiron/sqlx             v1.x  // database abstraction, placeholder rebinding
    github.com/golang-migrate/migrate/v4 v4.x  // versioned schema migrations
    github.com/jackc/pgx/v5             v5.x  // PostgreSQL driver (stdlib adapter)
)

// CLI
require (
    github.com/spf13/cobra  v1.x  // CLI framework (subcommands, flags, help)
)
```

**Dependency rationale:**

- **sqlx** — thin layer over `database/sql`. Provides `Rebind()` for
  placeholder rewriting (`?` → `$1,$2,...`), struct scanning, and named
  parameters. Not an ORM — all existing SQL stays unchanged.
- **golang-migrate** — versioned migration files with up/down support.
  Embedded via `embed.FS` for single-binary distribution. Supports
  both SQLite and PostgreSQL drivers.
- **pgx** — the standard PostgreSQL driver for Go. Used via its
  `stdlib` adapter to register with `database/sql` (which sqlx wraps).
- **cobra** — standard Go CLI framework. Provides subcommands, flag
  parsing, and auto-generated help text.

## v2 Config Additions

```toml
[database]
# NEW: driver selection. "sqlite" (default) or "postgres".
driver = "sqlite"
path = "/data/db/blockyard.db"       # used when driver = "sqlite"
url = ""                             # PostgreSQL connection string; used when driver = "postgres"
                                     # use BLOCKYARD_DATABASE_URL env var for secrets

[board_storage]
# When set, enables board storage. Injected as POSTGREST_URL into worker
# containers. Requires driver = "postgres". Auth is handled by vault's
# Identity OIDC provider — the R app requests PostgREST JWTs from vault
# using its existing vault token.
postgrest_url = ""

[proxy]
# existing fields unchanged; new additions:
pre_warmed_seats = 0     # per-app default; 0 = scale-to-zero (no warm pool)
```

Per-app overrides for `pre_warmed_seats` are stored in the `apps` table
(see schema changes below).

## Schema Changes

### SQLite + PostgreSQL (shared)

**Migration 002: soft-delete**

```sql
ALTER TABLE apps ADD COLUMN deleted_at TEXT;
```

**Migration 003: pre-warming**

```sql
ALTER TABLE apps ADD COLUMN pre_warmed_seats INTEGER NOT NULL DEFAULT 0;
```

### PostgreSQL Only

**Migration 004: board storage**

```sql
-- PostgREST roles
CREATE ROLE blockr_user NOLOGIN;
GRANT USAGE ON SCHEMA public TO blockr_user;
CREATE ROLE anon NOLOGIN;

-- Board identity and metadata
CREATE TABLE boards (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub   TEXT NOT NULL,
    board_id    TEXT NOT NULL,
    acl_type    TEXT NOT NULL DEFAULT 'private'
                CHECK (acl_type IN ('private', 'public', 'restricted')),
    tags        TEXT[] DEFAULT '{}',
    created_at  TIMESTAMPTZ DEFAULT now(),
    updated_at  TIMESTAMPTZ DEFAULT now(),
    UNIQUE (owner_sub, board_id)
);

-- Versioned board data
CREATE TABLE board_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub   TEXT NOT NULL,
    board_id    TEXT NOT NULL,
    data        JSONB NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now(),
    FOREIGN KEY (owner_sub, board_id)
        REFERENCES boards(owner_sub, board_id) ON DELETE CASCADE
);

CREATE INDEX idx_board_versions_lookup
    ON board_versions(owner_sub, board_id, created_at DESC);

-- Sharing (for restricted ACL)
CREATE TABLE board_shares (
    owner_sub       TEXT NOT NULL,
    board_id        TEXT NOT NULL,
    shared_with_sub TEXT NOT NULL,
    created_at      TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (owner_sub, board_id, shared_with_sub),
    FOREIGN KEY (owner_sub, board_id)
        REFERENCES boards(owner_sub, board_id) ON DELETE CASCADE
);

-- Identity helper for RLS.
-- Reads idp_sub (custom claim) because vault's Identity OIDC
-- provider hardcodes the standard sub to the vault entity ID.
CREATE FUNCTION current_sub() RETURNS TEXT AS $$
    SELECT current_setting('request.jwt.claims', true)::json->>'idp_sub'
$$ LANGUAGE sql STABLE;

-- RLS: boards
ALTER TABLE boards ENABLE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON boards
    USING (owner_sub = current_sub())
    WITH CHECK (owner_sub = current_sub());

CREATE POLICY public_read ON boards FOR SELECT
    USING (acl_type = 'public');

CREATE POLICY restricted_read ON boards FOR SELECT
    USING (acl_type = 'restricted' AND EXISTS (
        SELECT 1 FROM board_shares
        WHERE board_shares.owner_sub = boards.owner_sub
        AND board_shares.board_id = boards.board_id
        AND board_shares.shared_with_sub = current_sub()
    ));

-- RLS: board_versions (inherits access from parent board)
ALTER TABLE board_versions ENABLE ROW LEVEL SECURITY;

CREATE POLICY version_owner ON board_versions
    USING (owner_sub = current_sub())
    WITH CHECK (owner_sub = current_sub());

CREATE POLICY version_public ON board_versions FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM boards
        WHERE boards.owner_sub = board_versions.owner_sub
        AND boards.board_id = board_versions.board_id
        AND boards.acl_type = 'public'
    ));

CREATE POLICY version_restricted ON board_versions FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM boards b
        JOIN board_shares bs
            ON b.owner_sub = bs.owner_sub AND b.board_id = bs.board_id
        WHERE b.owner_sub = board_versions.owner_sub
        AND b.board_id = board_versions.board_id
        AND b.acl_type = 'restricted'
        AND bs.shared_with_sub = current_sub()
    ));

-- RLS: board_shares
ALTER TABLE board_shares ENABLE ROW LEVEL SECURITY;

CREATE POLICY shares_owner ON board_shares
    USING (owner_sub = current_sub())
    WITH CHECK (owner_sub = current_sub());

CREATE POLICY shares_see_own ON board_shares FOR SELECT
    USING (shared_with_sub = current_sub());

-- Grant PostgREST access
GRANT SELECT, INSERT, UPDATE, DELETE ON boards, board_versions, board_shares
    TO blockr_user;
```

Security enforcement is entirely PostgreSQL's responsibility via RLS.
Blockyard provisions the schema and policies; PostgREST validates JWTs
against vault's JWKS endpoint (`/identity/oidc/.well-known/keys`);
PostgreSQL evaluates RLS policies on every query. Blockyard is not in
the data path at runtime.

## Build Phases

### Phase 2-1: Database Dual-Backend Foundation

Migrate the database layer from raw `database/sql` + inline DDL to
`sqlx` + `golang-migrate`. Support both SQLite and PostgreSQL as a
config option. This is the foundation — board storage requires
PostgreSQL, and v4 multi-node HA requires it for session/worker stores.

**Deliverables:**

1. Introduce `sqlx` — replace `*sql.DB` with `*sqlx.DB` in the `DB`
   struct. All existing queries get `sqlx.Rebind(dialect, query)` for
   placeholder portability. Struct scanning replaces manual `Scan()`
   calls where beneficial.
2. Versioned migrations via `golang-migrate` — extract the inline DDL
   from `db.go` into `migrations/sqlite/001_initial.up.sql` and
   `migrations/postgres/001_initial.up.sql`. Migrations are embedded
   via `embed.FS`.
3. PostgreSQL driver registration — register `pgx/v5/stdlib` alongside
   `modernc.org/sqlite`. The `[database] driver` config field selects
   which is used.
4. Dialect helpers — `internal/db/dialect.go`:
   - `IsUniqueConstraintError(err)` dispatches on driver type
     (SQLite: string match; PostgreSQL: `pq` error code `23505`)
   - Connection setup differs: SQLite gets `SetMaxOpenConns(1)`;
     PostgreSQL gets a connection pool.
5. Config validation — `driver = "postgres"` requires `url` to be set;
   `driver = "sqlite"` requires `path`.
6. Test infrastructure — integration tests run against both backends.
   SQLite tests use `:memory:`; PostgreSQL tests use a test container
   or are skipped when no PostgreSQL is available.

**Key type changes:**

```go
// internal/db/db.go

type Dialect int

const (
    DialectSQLite Dialect = iota
    DialectPostgres
)

type DB struct {
    *sqlx.DB
    dialect  Dialect
    tempPath string // non-empty when using a temp file for :memory:
}

func Open(cfg config.DatabaseConfig) (*DB, error) {
    switch cfg.Driver {
    case "sqlite":
        return openSQLite(cfg.Path)
    case "postgres":
        return openPostgres(cfg.URL)
    default:
        return nil, fmt.Errorf("unsupported database driver: %q", cfg.Driver)
    }
}
```

All existing `db.Exec("... ? ...", args...)` calls become
`db.Exec(sqlx.Rebind(db.BindType(), "... ? ..."), args...)` — or
equivalently, a helper method on `*DB` that rebinds automatically.
This is a mechanical transformation across all ~40 methods.

### Phase 2-2: Quick Wins

Three independent features that build on the existing schema and
infrastructure. Low risk, high usability impact.

**Deliverables:**

1. **Bundle rollback** — new endpoint
   `POST /api/v1/apps/{id}/rollback` with body `{ "bundle_id": "..." }`.
   Validates the target bundle exists, is `ready`, and belongs to the
   app. Drains active sessions (marks app as draining, waits for
   `shutdown_timeout / 2`, then force-evicts remaining workers), sets
   `active_bundle`, and allows new sessions against the rolled-back
   bundle.

   ```
   POST /api/v1/apps/{id}/rollback  { "bundle_id": "..." }
     1. Validate bundle is ready and belongs to app
     2. Mark app as draining (no new sessions routed)
     3. Wait up to drain_timeout for sessions to end
     4. Force-evict remaining workers
     5. Set active_bundle = target bundle
     6. Clear draining flag
     → 200 { app details with new active_bundle }
   ```

2. **Soft-delete for apps** — add `deleted_at TEXT` column. `DELETE
   /api/v1/apps/{id}` sets `deleted_at` instead of removing the row.
   All queries filter `WHERE deleted_at IS NULL`. A background sweeper
   goroutine runs periodically and purges soft-deleted apps older than
   a configurable retention period (default: 30 days) — stopping
   workers, removing files, deleting bundle rows, then the app row.

   New config field:

   ```toml
   [storage]
   soft_delete_retention = "720h"   # 30 days; 0 = immediate hard delete
   ```

   A restore endpoint allows undoing a soft-delete before purge:

   ```
   POST /api/v1/apps/{id}/restore  → clears deleted_at, returns 200
   ```

3. **Per-content resource limit enforcement** — wire the existing
   `memory_limit` and `cpu_limit` fields from `WorkerSpec` into Docker
   container creation. In `internal/backend/docker/docker.go`, the
   `Spawn` method sets `HostConfig.Resources`:

   ```go
   resources := container.Resources{}
   if spec.MemoryLimit != "" {
       mem, _ := units.RAMInBytes(spec.MemoryLimit)
       resources.Memory = mem
   }
   if spec.CPULimit > 0 {
       resources.NanoCPUs = int64(spec.CPULimit * 1e9)
   }
   ```

   No schema changes — the fields already exist.

### Phase 2-3: Worker Lifecycle (Scale-to-Zero, Pre-Warming, Loading Page)

Three features that share idle-detection and worker-lifecycle machinery.
Built together because they interact tightly.

**Deliverables:**

1. **Scale-to-zero** — the existing autoscaler already evicts idle
   workers after `idle_worker_timeout`. Scale-to-zero formalizes this:
   when all sessions disconnect and the idle timeout expires, the app
   has zero running workers. The next request triggers a cold start.
   No new machinery needed for the eviction side — the change is on
   the cold-start side (the loading page).

2. **Pre-warming** — a new per-app `pre_warmed_seats` field (default
   `0`, stored in `apps` table). When `> 0`, the autoscaler maintains
   a pool of standby workers with no assigned sessions. When a new
   session arrives, it claims a warm worker (zero cold-start latency)
   and the autoscaler immediately spawns a replacement to maintain
   the pool size.

   The autoscaler loop (which already runs on `health_interval`) gains
   a pre-warming check:

   ```
   for each app with pre_warmed_seats > 0:
       idle_count = count workers with zero sessions
       deficit = pre_warmed_seats - idle_count
       if deficit > 0:
           spawn deficit workers (respecting max_workers limits)
   ```

   Workers spawned for pre-warming are identical to on-demand workers —
   same image, same mounts, same health checks. The only difference is
   they have no assigned sessions until claimed.

3. **Cold-start loading page** — when a browser request (`Accept:
   text/html`) arrives for an app with no healthy workers, the proxy
   serves an HTML page with a spinner instead of holding the request
   open. The page polls a readiness endpoint to detect when the worker
   is ready, then redirects to the app.

   Non-browser requests (API calls, WebSocket upgrades) continue using
   the existing hold-until-healthy behavior.

   **New endpoint:**

   ```
   GET /app/{name}/__blockyard/ready
     → 200 { "ready": true }   when a healthy worker exists
     → 200 { "ready": false }  when still starting
     → 503                     on timeout / failure
   ```

   **Loading page behavior:**

   ```
   1. Browser requests /app/my-app/
   2. Proxy detects: no healthy workers, Accept includes text/html
   3. Spawn worker (if not already spawning)
   4. Serve loading.html (embedded template, blockyard-branded spinner)
   5. loading.html polls GET /app/my-app/__blockyard/ready every 2s
   6. On { "ready": true } → JavaScript redirects to /app/my-app/
   7. On timeout (worker_start_timeout) → show error message
   ```

   The `/__blockyard/` path prefix is reserved and never forwarded to
   the worker container. It's intercepted by the proxy before routing.

   The loading page is not customizable in v2 — branding and
   customization are v3 concerns.

### Phase 2-4: Board Storage

Add PostgreSQL-backed board storage with PostgREST as the API layer.
Blockyard owns the schema; vault's Identity OIDC provider issues JWTs;
PostgREST validates them; PostgreSQL enforces access control via RLS.

See [phase-2-4.md](phase-2-4.md) for the full implementation plan.

**Deliverables:**

1. **Board schema migration** — `migrations/postgres/004_v2_boards.up.sql`
   creates the `boards`, `board_versions`, `board_shares` tables, RLS
   policies, and PostgREST roles. PostgreSQL only — SQLite deployments
   skip this migration.

2. **Config addition** — `[board_storage]` section with `postgrest_url`.

3. **PostgREST URL injection** — when `[board_storage] postgrest_url`
   is configured, inject `POSTGREST_URL` as an environment variable
   into worker containers (alongside existing `VAULT_ADDR` and
   `BLOCKYARD_API_URL` injection in `WorkerSpec`).

4. **Vault Identity OIDC setup** — operator/init-container configures
   vault's Identity secrets engine to issue PostgREST-scoped JWTs
   containing the user's original IdP subject as a `idp_sub`
   custom claim.

5. **PostgREST board storage example** — docker-compose with
   PostgreSQL + PostgREST + vault Identity OIDC alongside the existing
   PocketBase example.

**Architecture:**

```
blockr (R app)
  │
  ├── 1. Vault token from X-Blockyard-Vault-Token (existing flow)
  ├── 2. GET /identity/oidc/token/postgrest → vault-signed JWT
  ├── 3. Authorization: Bearer <vault-issued JWT>
  │
  ▼
PostgREST ──JWKS──→ OpenBao (/identity/oidc/.well-known/keys)
  │
  ▼
PostgreSQL (RLS enforces per-user scoping + sharing)
```

Blockyard is not in the data path. The R app uses its existing vault
token to request PostgREST JWTs from vault on demand — no new header
injection, no token refresh concerns. The vault token is renewable by
the R app via `POST /auth/token/renew-self`.

**Board operations from R (via PostgREST):**

```
Save:    POST   /boards          { owner_sub, board_id }
                                 → creates board metadata
         POST   /board_versions  { owner_sub, board_id, data }
                                 → creates versioned snapshot
Load:    GET    /board_versions?owner_sub=eq.{sub}&board_id=eq.{id}
                                 &order=created_at.desc&limit=1
List:    GET    /boards          (RLS filters automatically)
Delete:  DELETE /boards?owner_sub=eq.{sub}&board_id=eq.{id}
Share:   POST   /board_shares   { owner_sub, board_id, shared_with_sub }
Tags:    PATCH  /boards?owner_sub=eq.{sub}&board_id=eq.{id}
                                 { tags: ["analysis", "demo"] }
```

### Phase 2-5: Build Pipeline — rv → pak

Replace rv with pak as the build-time dependency manager. Make lockfiles
optional. Support three build modes depending on what the bundle ships:
lockfile (`pkg.lock` → `pak::lockfile_install()`), DESCRIPTION
(`pak::local_install_deps()`), or bare scripts (`pak::scan_deps()` +
`pak::pkg_install()`). This simplifies the deployment workflow and
aligns blockyard with the standard R ecosystem.

**Deliverables:**

1. **pak cache** — download and cache pak's pre-built bundle on the
   server, replacing the rv binary cache (`internal/pakcache/`). pak
   vendors all 16 of its dependencies into a single self-contained
   package — one download, instant install.
2. **Build mode detection** — inspect bundle contents to select the
   resolution strategy: lockfile > DESCRIPTION > script scan.
3. **Build container command** — R script that loads pak (mounted
   read-only) and runs the appropriate strategy.
4. **BuildSpec extension** — add `Cmd` and `Mounts` fields to
   `BuildSpec` so the `Build` method supports flexible commands.
5. **Config changes** — replace `rv_version` with `pak_version`.
6. **Bundle validation** — relax lockfile requirement; only `app.R`
   is mandatory.
7. **Remove rv** — delete `internal/rvcache/`, `SetLibraryPath()`,
   `RvBinaryPath`, update examples.

### Phase 2-6: Package Store and Runtime Assembly

A server-level content-addressable package store populated during
builds and consumed at runtime. Every dependency restore catalogs its
installed packages into the store. Workers assemble additional packages
from store entries via hard links — near-instant, zero disk overhead.

**Deliverables:**

1. **Package store** (`internal/pkgstore/store.go`) — content-
   addressable directory keyed by `{package}/{version}-{source}`.
   Populated during builds via `Store.Ingest()`. Append-only.
2. **Build integration** — after a successful restore, hard-link all
   installed packages into the store.
3. **Per-worker library views** (`internal/pkgstore/view.go`) — flat
   directories populated with hard links from the store. Mounted
   read-only at `/extra-lib/` in worker containers.
4. **Worker lifecycle integration** — create view directories on spawn,
   mount them, clean up on eviction.
5. **Runtime assembly API** — `POST /api/v1/packages` endpoint that
   hard-links packages from the store into a running worker's view.
   Store hits are instant. Store misses are reported to the caller
   (no build containers at runtime).
6. **Worker authentication** — HMAC-based worker tokens for
   in-container API access.

### Phase 2-7: Web UI Expansion

Extends the v1 dashboard with per-app management and operational
visibility. Server-rendered HTML, no JavaScript framework.

**Deliverables:**

1. **Per-app settings panel** — accessible from the dashboard via a
   detail/edit link per app. Displays and allows editing of:
   - Name, title, description
   - Access type (acl / logged_in / public)
   - ACL management (grant/revoke user access)
   - Resource limits (memory, CPU)
   - Worker scaling (max_workers_per_app, max_sessions_per_worker)
   - Pre-warmed seats
   - Tags
   - Bundle list with rollback action
   - Soft-delete (with confirmation)

   Uses existing API endpoints — the UI is a form that POSTs to
   `PATCH /api/v1/apps/{id}`, `POST /api/v1/apps/{id}/rollback`, etc.

2. **Content filtering** — add search/filter controls to the dashboard
   app list. Filter by tag, search by name/title/description. Uses the
   existing `ListCatalog` endpoint with query parameters.

3. **Per-app log viewer** — accessible from the per-app settings panel.
   Streams logs via `GET /api/v1/apps/{id}/logs` using chunked transfer
   encoding. The UI uses `fetch()` with a `ReadableStream` reader to
   append log lines to a `<pre>` element in real time. Historical logs
   are loaded first, then live lines are appended.

No new navigation chrome (navbar, app switcher) — deferred to v3.

### Phase 2-8: CLI Tool

A dedicated Go binary (`cmd/by/`) for interacting with the server via
the REST API. Built last to target the stable, final v2 API surface.

**Authentication:** `BLOCKYARD_TOKEN` environment variable (a PAT).
No login command — users create PATs via the web UI and export the
env var. A `blockyard login` convenience command is a future addition.

**Server URL:** `BLOCKYARD_URL` environment variable (e.g.,
`https://blockyard.example.com`).

**Subcommands:**

```
by deploy <app-name> <path>    Upload a bundle (tar.gz the directory, POST to upload endpoint)
by list                        List apps (with status, active bundle, owner)
by get <app>                   Get app details
by start <app>                 Start an app
by stop <app>                  Stop an app
by rollback <app> <bundle-id>  Roll back to a previous bundle
by logs <app>                  Tail app logs (--follow for streaming)
by bundles <app>               List bundles for an app
by delete <app>                Soft-delete an app
by restore <app>               Restore a soft-deleted app
by config <app> [flags]        Update app config (--memory, --cpu, --access-type, etc.)
by users list                  List users (admin only)
by users update <sub> [flags]  Update user role/active status (admin only)
by tokens create <name>        Create a PAT (requires browser session — deferred)
```

All commands are thin wrappers around the REST API. The CLI bundles a
directory into a tar.gz archive for `deploy` — this is the primary
value over raw `curl`.

**Error handling:** the CLI prints the `message` field from error
responses, not the raw JSON. Non-zero exit codes on failure.

## Build Order and Dependency Graph

```
Phase 2-1: Database Dual-Backend Foundation
  └── prerequisite for: board storage (phase 2-4), everything else benefits

Phase 2-2: Quick Wins (Rollback, Soft-Delete, Resource Limits)
  └── depends on: phase 2-1 (migrations)
  └── independent of: phases 2-3 through 2-6

Phase 2-3: Worker Lifecycle (Scale-to-Zero, Pre-Warming, Loading Page)
  └── depends on: phase 2-1 (pre_warmed_seats column)
  └── independent of: phases 2-4 through 2-6

Phase 2-4: Board Storage
  └── depends on: phase 2-1 (PostgreSQL support)
  └── independent of: phases 2-3, 2-5, 2-6

Phase 2-5: Build Pipeline (rv → pak)
  └── independent of: phases 2-2 through 2-4
  └── can be developed in parallel with everything after 2-1

Phase 2-6: Package Store and Runtime Assembly
  └── depends on: phase 2-5 (pak-based builds populate the store)
  └── independent of: phases 2-3, 2-4

Phase 2-7: Web UI Expansion
  └── depends on: phases 2-2 (rollback, soft-delete UI),
      2-3 (pre-warming config), 2-4 (board storage optional)

Phase 2-8: CLI Tool
  └── depends on: all API-changing phases (2-2 through 2-6)
  └── built last to target final API surface
```

Phases 2-3, 2-4, and 2-5 are independent of each other and can be
developed in parallel after the foundation. Phase 2-6 requires 2-5.

## Test Strategy

### Unit tests

- **Database dialect tests:** verify all ~40 methods produce correct
  results on both SQLite and PostgreSQL. Run against `:memory:` SQLite
  and a test PostgreSQL instance (skipped when unavailable).
- **Migration tests:** verify up/down migrations on both dialects.
- **Package store tests:** store has/install/path operations using temp
  directories. Hard-link view creation and cleanup.
- **Loading page tests:** verify HTML template renders, readiness
  endpoint returns correct status.
- **CLI tests:** subcommand parsing, flag validation, output formatting.

### Integration tests

Start the full server (with mock backend) and exercise HTTP endpoints:

- **Rollback flow:** deploy two bundles → rollback to first → verify
  active bundle changed.
- **Soft-delete flow:** delete app → verify filtered from listings →
  restore → verify visible again → wait for purge.
- **Resource limits:** spawn worker with memory/CPU limits → verify
  Docker container has correct `HostConfig.Resources`.
- **Pre-warming:** configure app with `pre_warmed_seats = 1` → verify
  autoscaler maintains one idle worker → claim it with a session →
  verify replacement spawned.
- **Loading page:** request app with no workers → verify HTML loading
  page served (not held request) → verify `/__blockyard/ready` returns
  status → verify redirect on ready.
- **Package install:** POST to install endpoint → verify store entry
  created → verify hard-linked into worker view.
- **Board storage:** requires PostgreSQL test instance. Verify RLS
  policies: owner sees own boards, shared user sees restricted boards,
  public boards visible to all, private boards invisible to others.

### Docker integration tests

- Resource limit enforcement: spawn container with `memory_limit` and
  `cpu_limit`, verify Docker inspect shows correct values.
- Package install build container: run a real `pak::pkg_install()` in
  a build container, verify package appears in store.

## Design Decisions

1. **sqlx over raw database/sql.** The refactor is mechanical
   (rebind + struct scanning) and sqlx is the thinnest useful layer.
   It avoids both the fragility of hand-rolled placeholder rewriting
   and the complexity of a full ORM. All existing SQL is preserved.

2. **golang-migrate over inline DDL.** Versioned migrations with
   up/down support are essential for production PostgreSQL deployments.
   Embedded via `embed.FS` so the single-binary distribution is
   preserved. SQLite deployments also benefit from versioned schema
   evolution (vs. the current `CREATE TABLE IF NOT EXISTS` approach
   that cannot handle column additions).

3. **Separate migration tracks per dialect.** SQLite and PostgreSQL
   migration files are maintained independently. This avoids
   dialect-conditional SQL within migration files and allows
   PostgreSQL-only migrations (board storage).

4. **Board metadata separate from versions.** The `boards` table holds
   identity and access control (owner, board_id, acl_type, tags). The
   `board_versions` table holds the data snapshots. This ensures ACL
   settings and tags are per-board, not per-version — sharing a board
   means sharing all its versions.

5. **Hard links over bind mount propagation for package views.** Hard
   links require no special privileges, no Docker configuration changes,
   and no mount lifecycle management. The only constraint — store and
   views on the same filesystem — is naturally satisfied when both live
   under `bundle_server_path`.

6. **Open package installation (no allowlist).** The threat model is
   unchanged from the worker container — users already run arbitrary R
   code. Build containers carry the same isolation as workers. An
   allowlist would add operational friction without meaningful security
   improvement.

7. **Loading page for browsers only.** API clients and WebSocket
   connections continue using the hold-until-healthy pattern. The
   loading page is only served when the request's `Accept` header
   includes `text/html`. This avoids breaking programmatic clients
   that expect either a response or a timeout.

8. **CLI last.** The CLI targets the final v2 API surface, avoiding
   rework if endpoints change during development. Auth is a simple
   env var (`BLOCKYARD_TOKEN`); no device flow or browser-based login
   in v2.

9. **Pre-warming shares autoscaler machinery.** The pre-warming check
   runs inside the existing autoscaler loop (which already runs on
   `health_interval`). No new goroutine or timer. The autoscaler
   already handles idle eviction; pre-warming is the inverse — spawn
   when the idle pool is below the target.

10. **`/__blockyard/` reserved path prefix.** All blockyard-internal
    proxy endpoints live under this prefix, which is never forwarded to
    worker containers. This avoids collisions with app routes and
    provides a clean namespace for future proxy-level features.
