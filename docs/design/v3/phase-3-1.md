# Phase 3-1: Migration Discipline

Establish rules, documentation, and CI enforcement for backward-compatible
schema migrations. Rolling updates (phase 3-5) require the old server to
continue reading and writing the database after the new server's migrations
have run. Every migration must be backward-compatible with the previous
release (N/N-1 compatibility window). This phase lands first — it protects
every subsequent migration, including the v3 ones.

There are no existing persistent installations — we consolidate the eight
existing migrations into a single `001_initial` per dialect and start clean
with the new rules from day one.

## Deliverables

1. **Migration consolidation** — replace the eight existing migration pairs
   per dialect with a single `001_initial` that produces the same schema.
   Eliminates the SQLite-missing-004 gap and gives a clean baseline.
2. **Migration authoring guide** (`docs/design/migrations.md`) — the
   canonical reference for writing blockyard migrations. Covers the
   expand-and-contract pattern, allowed and prohibited operations, file
   conventions, and the contract phase procedure.
3. **Atlas Community lint** — CI step using the free Atlas Community
   binary (Apache 2.0). Catches destructive changes, missing defaults, and
   structural issues that convention checks cannot.
4. **File convention check** (`internal/db/migrate_test.go`) — a Go test
   that verifies migration file pairing, sequential numbering, dialect
   parity, and non-empty content.
5. **Up-down-up roundtrip test** (`internal/db/migrate_test.go`) — applies
   all migrations up, then all down, then all up again. Verifies that down
   migrations are correct inverses and the schema is stable after a round
   trip.
6. **Pre-migration backup utility** (`internal/db/backup.go`) — used by
   `by admin update` (phase 3-5) before applying migrations to a new
   release. SQLite file copy, PostgreSQL `pg_dump`.

**Enforcement layers (fastest to slowest):**

| Layer | What it catches | When it runs |
|---|---|---|
| File convention check | Missing pairs, numbering gaps, dialect mismatch, phase tags | `go test`, <1s |
| Atlas Community lint | Destructive DDL, missing defaults, lock issues | CI, ~5s |
| Up-down-up roundtrip | Broken down migrations, schema drift | `go test`, ~10s |

The `migration-compat` CI job (running old code's tests against new schema)
is deferred to phase 3-5 when rolling updates land and there is a real N-1
release to test against.

---

## Step-by-step

### Step 1: Consolidate migrations

Delete all existing migration files and replace with a single
`001_initial` pair per dialect. The consolidated schema is the result of
applying all eight original migrations in sequence.

#### `migrations/released.txt`

Tracks which migration numbers have shipped in a release. Managed by
the release process — when a release is tagged, any new migration
numbers are appended. The convention check reads this file to enforce
that contract migrations only reference expands that have shipped.

```
# Managed by the release process. Do not edit manually.
# Format: NNN vX.Y.Z
```

Initially empty (no releases yet after consolidation). After the first
release containing `001_initial`:

```
001 v0.1.0
```

This file lives at `internal/db/migrations/released.txt` and is
embedded alongside the dialect-specific migration directories.

#### Delete old files

```
internal/db/migrations/sqlite/
  002_v2_soft_delete.{up,down}.sql
  003_v2_pre_warming.{up,down}.sql
  005_v2_refresh.{up,down}.sql
  006_v2_backend_prereqs.{up,down}.sql
  007_v2_app_aliases.{up,down}.sql
  008_v2_bundle_logs.{up,down}.sql

internal/db/migrations/postgres/
  002_v2_soft_delete.{up,down}.sql
  003_v2_pre_warming.{up,down}.sql
  004_v2_boards.{up,down}.sql
  005_v2_refresh.{up,down}.sql
  006_v2_backend_prereqs.{up,down}.sql
  007_v2_app_aliases.{up,down}.sql
  008_v2_bundle_logs.{up,down}.sql
```

#### `migrations/sqlite/001_initial.up.sql`

The full SQLite schema after all eight original migrations. Key
differences from the original 001: `name` uses a partial unique index
instead of a column-level UNIQUE constraint (from 002), and columns
added by 003/005/006 are present from the start.

```sql
-- phase: expand
CREATE TABLE apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    access_type             TEXT NOT NULL DEFAULT 'acl'
                            CHECK (access_type IN ('acl', 'logged_in', 'public')),
    active_bundle           TEXT REFERENCES bundles(id) ON DELETE SET NULL,
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    title                   TEXT,
    description             TEXT,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL,
    deleted_at              TEXT,
    pre_warmed_sessions     INTEGER NOT NULL DEFAULT 0,
    refresh_schedule        TEXT NOT NULL DEFAULT '',
    last_refresh_at         TEXT,
    enabled                 INTEGER NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    uploaded_at TEXT NOT NULL,
    deployed_by TEXT,
    deployed_at TEXT,
    pinned      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_bundles_app_id ON bundles(app_id);

CREATE TABLE app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('user')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

CREATE TABLE users (
    sub        TEXT PRIMARY KEY,
    email      TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    role       TEXT NOT NULL DEFAULT 'viewer',
    active     INTEGER NOT NULL DEFAULT 1,
    last_login TEXT NOT NULL
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

CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    worker_id   TEXT NOT NULL,
    user_sub    TEXT,
    started_at  TEXT NOT NULL,
    ended_at    TEXT,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'ended', 'crashed'))
);

CREATE INDEX idx_sessions_app_started ON sessions(app_id, started_at DESC);
CREATE INDEX idx_sessions_user ON sessions(user_sub, app_id, started_at DESC);
CREATE INDEX idx_sessions_worker ON sessions(worker_id, started_at DESC);
CREATE INDEX idx_sessions_status ON sessions(status);

CREATE TABLE app_aliases (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name        TEXT NOT NULL UNIQUE,
    phase       TEXT NOT NULL CHECK (phase IN ('alias', 'redirect')),
    expires_at  TEXT NOT NULL
);

CREATE INDEX idx_app_aliases_app_id ON app_aliases(app_id);

CREATE TABLE bundle_logs (
    bundle_id   TEXT PRIMARY KEY REFERENCES bundles(id) ON DELETE CASCADE,
    output      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
```

#### `migrations/sqlite/001_initial.down.sql`

```sql
DROP TABLE IF EXISTS bundle_logs;
DROP TABLE IF EXISTS app_aliases;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS app_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS personal_access_tokens;
DROP TABLE IF EXISTS app_access;
DROP TABLE IF EXISTS bundles;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS users;
```

#### `migrations/postgres/001_initial.up.sql`

Same core schema as SQLite, plus the boards tables, RLS policies,
triggers, and PostgREST roles from the original migration 004.

```sql
-- phase: expand
CREATE TABLE apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    access_type             TEXT NOT NULL DEFAULT 'acl'
                            CHECK (access_type IN ('acl', 'logged_in', 'public')),
    active_bundle           TEXT,
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               DOUBLE PRECISION,
    title                   TEXT,
    description             TEXT,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL,
    deleted_at              TEXT,
    pre_warmed_sessions     INTEGER NOT NULL DEFAULT 0,
    refresh_schedule        TEXT NOT NULL DEFAULT '',
    last_refresh_at         TEXT,
    enabled                 INTEGER NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    uploaded_at TEXT NOT NULL,
    deployed_by TEXT,
    deployed_at TEXT,
    pinned      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_bundles_app_id ON bundles(app_id);

ALTER TABLE apps ADD CONSTRAINT fk_apps_active_bundle
    FOREIGN KEY (active_bundle) REFERENCES bundles(id) ON DELETE SET NULL;

CREATE TABLE app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('user')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

CREATE TABLE users (
    sub        TEXT PRIMARY KEY,
    email      TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    role       TEXT NOT NULL DEFAULT 'viewer',
    active     INTEGER NOT NULL DEFAULT 1,
    last_login TEXT NOT NULL
);

CREATE TABLE personal_access_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   BYTEA NOT NULL UNIQUE,
    user_sub     TEXT NOT NULL REFERENCES users(sub),
    name         TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_pat_token_hash ON personal_access_tokens(token_hash);
CREATE INDEX idx_pat_user_sub ON personal_access_tokens(user_sub);

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

CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    worker_id   TEXT NOT NULL,
    user_sub    TEXT,
    started_at  TEXT NOT NULL,
    ended_at    TEXT,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'ended', 'crashed'))
);

CREATE INDEX idx_sessions_app_started ON sessions(app_id, started_at DESC);
CREATE INDEX idx_sessions_user ON sessions(user_sub, app_id, started_at DESC);
CREATE INDEX idx_sessions_worker ON sessions(worker_id, started_at DESC);
CREATE INDEX idx_sessions_status ON sessions(status);

CREATE TABLE app_aliases (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name        TEXT NOT NULL UNIQUE,
    phase       TEXT NOT NULL CHECK (phase IN ('alias', 'redirect')),
    expires_at  TEXT NOT NULL
);

CREATE INDEX idx_app_aliases_app_id ON app_aliases(app_id);

CREATE TABLE bundle_logs (
    bundle_id   TEXT PRIMARY KEY REFERENCES bundles(id) ON DELETE CASCADE,
    output      TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

-- Board storage: PostgreSQL only.
-- Boards use native TIMESTAMPTZ (not TEXT) because they are never
-- shared with SQLite and benefit from timezone-aware comparison.

DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'blockr_user') THEN
        CREATE ROLE blockr_user NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'anon') THEN
        CREATE ROLE anon NOLOGIN;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO blockr_user;

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

CREATE TABLE board_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub   TEXT NOT NULL,
    board_id    TEXT NOT NULL,
    data        JSONB NOT NULL,
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ DEFAULT now(),
    FOREIGN KEY (owner_sub, board_id)
        REFERENCES boards(owner_sub, board_id) ON DELETE CASCADE
);

CREATE INDEX idx_board_versions_lookup
    ON board_versions(owner_sub, board_id, created_at DESC);

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

-- RLS: board_versions
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

-- Auto-update boards.updated_at on row modification.
CREATE FUNCTION update_boards_timestamp() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER boards_updated_at
    BEFORE UPDATE ON boards
    FOR EACH ROW EXECUTE FUNCTION update_boards_timestamp();

-- Auto-update boards.updated_at when a new version is inserted.
CREATE FUNCTION update_board_on_version() RETURNS TRIGGER AS $$
BEGIN
    UPDATE boards SET updated_at = now()
    WHERE owner_sub = NEW.owner_sub AND board_id = NEW.board_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER version_updates_board
    AFTER INSERT ON board_versions
    FOR EACH ROW EXECUTE FUNCTION update_board_on_version();

GRANT SELECT, INSERT, UPDATE, DELETE ON boards, board_versions, board_shares
    TO blockr_user;
GRANT SELECT ON users TO blockr_user;
```

#### `migrations/postgres/001_initial.down.sql`

```sql
REVOKE SELECT ON users FROM blockr_user;

DROP TRIGGER IF EXISTS version_updates_board ON board_versions;
DROP TRIGGER IF EXISTS boards_updated_at ON boards;
DROP FUNCTION IF EXISTS update_board_on_version();
DROP FUNCTION IF EXISTS update_boards_timestamp();

DROP TABLE IF EXISTS board_shares CASCADE;
DROP TABLE IF EXISTS board_versions CASCADE;
DROP TABLE IF EXISTS boards CASCADE;
DROP FUNCTION IF EXISTS current_sub();

-- Do not drop blockr_user/anon roles — they may be used by other
-- services. Role cleanup is an operator concern.

DROP TABLE IF EXISTS bundle_logs;
DROP TABLE IF EXISTS app_aliases;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS app_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS personal_access_tokens;
DROP TABLE IF EXISTS app_access;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS fk_apps_active_bundle;
DROP TABLE IF EXISTS bundles;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS users;
```

### Step 2: Migration authoring guide

New file: `docs/design/migrations.md`

This is the canonical reference for anyone writing a blockyard migration.
The content is derived from the v3 plan's phase 3-1 specification.

#### The expand-and-contract pattern

Every migration must be backward-compatible with the previous release.
The old server must be able to read and write the database after new
migrations run. This is enforced via a two-phase pattern:

- **Expand** (this release): additive changes only. The old server
  must be able to read and write the database after these run.
- **Contract** (next release): remove deprecated schema. Safe because
  no server running the previous code is still alive.

#### Migrations must be self-contained

**This is not mechanically enforced. Read carefully.**

Every migration must carry its own data transformations in SQL. Never
rely on application code running between an expand and its contract to
move, backfill, or transform data. Migrations run sequentially by
number — if a user upgrades from v1.0 to v3.0, the application code
from v2.0 never executes. Any data transformation that lived only in
v2.0's application code is skipped, and the contract migration will
destroy data that was never migrated.

Correct — backfill in the migration:
```sql
-- phase: expand
ALTER TABLE users ADD COLUMN email_normalized TEXT NOT NULL DEFAULT '';
UPDATE users SET email_normalized = lower(email);
```

Wrong — backfill deferred to application code:
```sql
-- phase: expand
ALTER TABLE users ADD COLUMN email_normalized TEXT NOT NULL DEFAULT '';
-- Application code will backfill this over the next few days...
```

If a backfill is too large to run in a single migration, it must still
be expressed as SQL in the migration file (batched `UPDATE` with a
loop, or a temporary trigger that populates on read). The migration
may be slow, but it will be correct regardless of which versions the
user skipped.

#### Allowed operations (expand phase)

- `ADD COLUMN` with a `DEFAULT` value (or nullable)
- `CREATE TABLE`
- `CREATE INDEX` (non-unique, or unique on new tables only)
- `CREATE INDEX CONCURRENTLY` (PostgreSQL; avoids table locks)
- `ADD CHECK` constraint with `NOT VALID` (PostgreSQL; deferred
  validation)

#### Prohibited operations (without a paired contract in the next release)

- `DROP COLUMN` — old server may SELECT or INSERT it
- `RENAME COLUMN` — old server references the old name
- `ALTER COLUMN ... TYPE` — old server assumes the old type
- `DROP TABLE` — unless created in the same migration batch
- `ALTER TABLE ... ADD ... NOT NULL` without `DEFAULT` — old server
  INSERTs will fail
- `RENAME TABLE` — old server references the old name
- `DROP INDEX` on an index the old server relies on for performance

#### Migration file conventions

- Sequential numbering: `NNN_description.up.sql` /
  `NNN_description.down.sql`
- Both up and down files must exist. Down migrations are a production
  path (`by admin rollback`), not just a dev tool. Irreversible
  migrations (e.g., data backfills) must be explicitly marked
  `-- irreversible: <reason>` — this blocks automated rollback past
  that point.
- Both SQLite and PostgreSQL tracks must have matching migration
  numbers. Use `-- no-op: <reason>` for dialect-specific migrations
  that don't apply to the other dialect.
- One logical change per migration number — don't bundle unrelated DDL.
- Comments explaining *why* for non-obvious choices.

#### Phase tags

Every `.up.sql` file must begin with a phase tag on its first line:

```sql
-- phase: expand
```

or

```sql
-- phase: contract
-- contracts: 002
```

Rules enforced by the convention check:

1. Every `.up.sql` has exactly one `-- phase:` tag (first line), with
   value `expand` or `contract`.
2. Every `contract` migration has a `-- contracts: NNN` line
   referencing one or more expand migration numbers (comma-separated
   for multi-expand contracts, e.g. `-- contracts: 002, 005`).
3. Referenced migration numbers must exist and be lower than the
   current migration number.
4. Referenced migrations must themselves be tagged `expand` (you
   don't contract a contract).
5. Referenced expand migrations must appear in `released.txt` — i.e.,
   they must have shipped in a prior release.

Phase tags are only required on `.up.sql` files — the `.down.sql`
inherits the phase from its `.up.sql` pair.

A release can contain any mix of expands and contracts.

#### Contract phase procedure

- The release notes for the expand phase document what will be
  contracted in the next release.
- The contract migration references the expand migration number via
  its `-- contracts: NNN` tag:
  ```sql
  -- phase: contract
  -- contracts: 002
  ```
- The convention check verifies that every referenced expand appears
  in `released.txt`. If the expand hasn't shipped yet, the check
  fails — no judgment call required.
- The release process appends newly-shipped migration numbers to
  `released.txt` when tagging a release.

### Step 3: Atlas Community lint

Add `atlas migrate lint` as a CI step using the Atlas Community binary
(Apache 2.0). Atlas parses SQL properly (not regex) and catches
destructive changes, missing defaults, and lock-related issues.

The Atlas Pro binary paywalled `migrate lint` in v0.38 (October 2025),
but the Community binary (built from the open-source repository)
retains the lint engine with generic analyzers. The PostgreSQL-specific
lock-duration rules (PG301-PG311) are Pro-only, but destructive change
detection — the primary concern for backward compatibility — is covered.

#### Install Atlas Community in devcontainer

Add to `.devcontainer/Dockerfile`, after the Go installation:

```dockerfile
# Atlas Community CLI (migration linting) — pin version, -latest is a canary.
# Checksum published at release.ariga.io alongside the binary.
ARG ATLAS_VERSION=v1.1.0
RUN curl -fsSL "https://release.ariga.io/atlas/atlas-community-linux-amd64-${ATLAS_VERSION}" \
      -o /usr/local/bin/atlas \
    && curl -fsSL "https://release.ariga.io/atlas/atlas-community-linux-amd64-${ATLAS_VERSION}.sha256" \
      -o /tmp/atlas.sha256 \
    && echo "$(cat /tmp/atlas.sha256)  /usr/local/bin/atlas" | sha256sum -c - \
    && chmod +x /usr/local/bin/atlas \
    && rm /tmp/atlas.sha256
```

#### CI workflow: `migration-lint` job

New job in `.github/workflows/ci.yml`, runs on PRs only. Uses the same
PostgreSQL service as the `unit` job for the PostgreSQL dev-url.

```yaml
migration-lint:
  if: github.event_name == 'pull_request'
  runs-on: ubuntu-24.04
  timeout-minutes: 5
  services:
    postgres:
      image: postgres:17
      env:
        POSTGRES_USER: blockyard
        POSTGRES_PASSWORD: blockyard
        POSTGRES_DB: blockyard_lint
      ports:
        - 5432:5432
      options: >-
        --health-cmd "pg_isready -U blockyard"
        --health-interval 2s
        --health-timeout 5s
        --health-retries 5
  steps:
    - uses: actions/checkout@v6
    - name: Install Atlas Community
      run: |
        curl -fsSL https://release.ariga.io/atlas/atlas-community-linux-amd64-v1.1.0 \
          -o /usr/local/bin/atlas
        curl -fsSL https://release.ariga.io/atlas/atlas-community-linux-amd64-v1.1.0.sha256 \
          -o /tmp/atlas.sha256
        echo "$(cat /tmp/atlas.sha256)  /usr/local/bin/atlas" | sha256sum -c -
        chmod +x /usr/local/bin/atlas
    - name: Lint SQLite migrations
      run: |
        atlas migrate lint \
          --dir "file://internal/db/migrations/sqlite?format=golang-migrate" \
          --dev-url "sqlite://dev?mode=memory" \
          --latest 1
    - name: Lint PostgreSQL migrations
      run: |
        atlas migrate lint \
          --dir "file://internal/db/migrations/postgres?format=golang-migrate" \
          --dev-url "postgres://blockyard:blockyard@localhost:5432/blockyard_lint?sslmode=disable" \
          --latest 1
```

The `--latest 1` flag lints only the most recent migration — existing
migrations are assumed correct. On the initial run after consolidation,
this lints the `001_initial`.

### Step 4: Migration test infrastructure

Before writing the convention check and roundtrip tests, extract a
reusable migrator constructor from `runMigrations()` and add schema
dump helpers.

#### Add `connURL` field to `DB` struct

The pre-migration backup utility (step 7) needs the original connection
URL to pass to `pg_dump`. Add a `connURL` field to the `DB` struct and
set it in `openPostgres()`:

```go
type DB struct {
    *sqlx.DB
    dialect  Dialect
    tempPath string // non-empty when using a temp file for SQLite :memory:
    connURL  string // original connection URL, set for PostgreSQL
}
```

In `openPostgres()`, store the URL before returning:

```go
func openPostgres(url string) (*DB, error) {
    db, err := sqlx.Open("pgx", url)
    // ...
    return &DB{DB: db, dialect: DialectPostgres, connURL: url}, nil
}
```

Also update `testPostgresDB()` in `db_test.go` — it constructs the
`DB` struct directly (bypassing `openPostgres()`), so it must pass
`connURL` too:

```go
db := &DB{DB: rawDB, dialect: DialectPostgres, connURL: testURL}
```

#### Extract `newMigrator()` from `runMigrations()`

In `internal/db/db.go`, factor out the migrator construction so both
production code and tests can use it:

```go
func (db *DB) newMigrator() (*migrate.Migrate, error) {
    var fsys fs.FS
    var err error

    switch db.dialect {
    case DialectSQLite:
        fsys, err = fs.Sub(sqliteMigrations, "migrations/sqlite")
    case DialectPostgres:
        fsys, err = fs.Sub(postgresMigrations, "migrations/postgres")
    }
    if err != nil {
        return nil, fmt.Errorf("migration fs: %w", err)
    }

    source, err := iofs.New(fsys, ".")
    if err != nil {
        return nil, fmt.Errorf("migration source: %w", err)
    }

    driver, err := db.migrateDriver()
    if err != nil {
        return nil, fmt.Errorf("migration driver: %w", err)
    }

    return migrate.NewWithInstance("iofs", source, db.driverName(), driver)
}

func (db *DB) runMigrations() error {
    m, err := db.newMigrator()
    if err != nil {
        return fmt.Errorf("create migrator: %w", err)
    }
    if err := m.Up(); err != nil && err != migrate.ErrNoChange {
        return fmt.Errorf("run migrations: %w", err)
    }
    return nil
}
```

#### Schema dump helpers

New file: `internal/db/migrate_test.go`. The `dumpSchema` function
produces a deterministic string representation of the database schema
for comparison. Excludes golang-migrate's bookkeeping table
(`schema_migrations`).

```go
func dumpSQLiteSchema(t *testing.T, db *DB) string {
    t.Helper()
    rows, err := db.Query(
        `SELECT sql FROM sqlite_master
         WHERE sql IS NOT NULL
           AND name != 'schema_migrations'
         ORDER BY type, name`)
    if err != nil {
        t.Fatal(err)
    }
    defer rows.Close()

    var stmts []string
    for rows.Next() {
        var s string
        if err := rows.Scan(&s); err != nil {
            t.Fatal(err)
        }
        stmts = append(stmts, s)
    }
    return strings.Join(stmts, "\n")
}

func dumpPostgresSchema(t *testing.T, db *DB) string {
    t.Helper()

    // Tables and columns
    rows, err := db.Query(`
        SELECT table_name, column_name, data_type,
               column_default, is_nullable
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name != 'schema_migrations'
        ORDER BY table_name, ordinal_position`)
    if err != nil {
        t.Fatal(err)
    }
    defer rows.Close()

    var lines []string
    for rows.Next() {
        var tbl, col, dtype, nullable string
        var dflt *string
        if err := rows.Scan(&tbl, &col, &dtype, &dflt, &nullable); err != nil {
            t.Fatal(err)
        }
        d := "NULL"
        if dflt != nil {
            d = *dflt
        }
        lines = append(lines, fmt.Sprintf("%s.%s %s default=%s nullable=%s",
            tbl, col, dtype, d, nullable))
    }

    // Indexes
    idxRows, err := db.Query(`
        SELECT tablename, indexname, indexdef
        FROM pg_indexes
        WHERE schemaname = 'public'
          AND tablename != 'schema_migrations'
        ORDER BY tablename, indexname`)
    if err != nil {
        t.Fatal(err)
    }
    defer idxRows.Close()

    for idxRows.Next() {
        var tbl, name, def string
        if err := idxRows.Scan(&tbl, &name, &def); err != nil {
            t.Fatal(err)
        }
        lines = append(lines, fmt.Sprintf("INDEX %s: %s", name, def))
    }

    // CHECK constraints (exclude system-generated NOT NULL constraints whose
    // names contain OIDs that change across drop/create cycles)
    chkRows, err := db.Query(`
        SELECT tc.table_name, cc.constraint_name, cc.check_clause
        FROM information_schema.check_constraints cc
        JOIN information_schema.table_constraints tc
            ON cc.constraint_name = tc.constraint_name
           AND cc.constraint_schema = tc.constraint_schema
        WHERE tc.table_schema = 'public'
          AND tc.table_name != 'schema_migrations'
          AND cc.constraint_name NOT LIKE '%_not_null'
        ORDER BY tc.table_name, cc.constraint_name`)
    if err != nil {
        t.Fatal(err)
    }
    defer chkRows.Close()

    for chkRows.Next() {
        var tbl, name, clause string
        if err := chkRows.Scan(&tbl, &name, &clause); err != nil {
            t.Fatal(err)
        }
        lines = append(lines, fmt.Sprintf("CHECK %s.%s: %s", tbl, name, clause))
    }

    // Foreign key constraints
    fkRows, err := db.Query(`
        SELECT tc.table_name, tc.constraint_name,
               kcu.column_name, ccu.table_name, ccu.column_name
        FROM information_schema.table_constraints tc
        JOIN information_schema.key_column_usage kcu
            ON tc.constraint_name = kcu.constraint_name
           AND tc.table_schema = kcu.table_schema
        JOIN information_schema.constraint_column_usage ccu
            ON tc.constraint_name = ccu.constraint_name
           AND tc.table_schema = ccu.table_schema
        WHERE tc.constraint_type = 'FOREIGN KEY'
          AND tc.table_schema = 'public'
        ORDER BY tc.table_name, tc.constraint_name`)
    if err != nil {
        t.Fatal(err)
    }
    defer fkRows.Close()

    for fkRows.Next() {
        var tbl, name, col, refTbl, refCol string
        if err := fkRows.Scan(&tbl, &name, &col, &refTbl, &refCol); err != nil {
            t.Fatal(err)
        }
        lines = append(lines, fmt.Sprintf("FK %s.%s: %s -> %s.%s",
            tbl, name, col, refTbl, refCol))
    }

    // Functions (excludes internal/system functions)
    fnRows, err := db.Query(`
        SELECT p.proname, pg_get_functiondef(p.oid)
        FROM pg_proc p
        JOIN pg_namespace n ON p.pronamespace = n.oid
        WHERE n.nspname = 'public'
        ORDER BY p.proname`)
    if err != nil {
        t.Fatal(err)
    }
    defer fnRows.Close()

    for fnRows.Next() {
        var name, def string
        if err := fnRows.Scan(&name, &def); err != nil {
            t.Fatal(err)
        }
        lines = append(lines, fmt.Sprintf("FUNC %s: %s", name, def))
    }

    // Triggers
    trgRows, err := db.Query(`
        SELECT tgname, pg_get_triggerdef(t.oid)
        FROM pg_trigger t
        JOIN pg_class c ON t.tgrelid = c.oid
        JOIN pg_namespace n ON c.relnamespace = n.oid
        WHERE n.nspname = 'public'
          AND NOT t.tgisinternal
        ORDER BY c.relname, t.tgname`)
    if err != nil {
        t.Fatal(err)
    }
    defer trgRows.Close()

    for trgRows.Next() {
        var name, def string
        if err := trgRows.Scan(&name, &def); err != nil {
            t.Fatal(err)
        }
        lines = append(lines, fmt.Sprintf("TRIGGER %s: %s", name, def))
    }

    // RLS policies
    polRows, err := db.Query(`
        SELECT pol.polname, c.relname, pg_get_expr(pol.polqual, pol.polrelid) AS using_expr,
               pg_get_expr(pol.polwithcheck, pol.polrelid) AS check_expr
        FROM pg_policy pol
        JOIN pg_class c ON pol.polrelid = c.oid
        JOIN pg_namespace n ON c.relnamespace = n.oid
        WHERE n.nspname = 'public'
        ORDER BY c.relname, pol.polname`)
    if err != nil {
        t.Fatal(err)
    }
    defer polRows.Close()

    for polRows.Next() {
        var name, tbl string
        var usingExpr, checkExpr *string
        if err := polRows.Scan(&name, &tbl, &usingExpr, &checkExpr); err != nil {
            t.Fatal(err)
        }
        u, c := "NULL", "NULL"
        if usingExpr != nil {
            u = *usingExpr
        }
        if checkExpr != nil {
            c = *checkExpr
        }
        lines = append(lines, fmt.Sprintf("POLICY %s.%s: USING(%s) CHECK(%s)",
            tbl, name, u, c))
    }

    return strings.Join(lines, "\n")
}

func dumpSchema(t *testing.T, db *DB) string {
    t.Helper()
    switch db.dialect {
    case DialectSQLite:
        return dumpSQLiteSchema(t, db)
    case DialectPostgres:
        return dumpPostgresSchema(t, db)
    default:
        t.Fatalf("unknown dialect: %d", db.dialect)
        return ""
    }
}
```

### Step 5: File convention check

`TestMigrationConventions` in `internal/db/migrate_test.go`. Reads from
the embedded filesystems so it runs with `go test` — no filesystem
assumptions.

Add an embed for `released.txt` in `internal/db/db.go` alongside the
existing migration embeds:

```go
//go:embed migrations/released.txt
var releasedFile embed.FS
```

```go
func TestMigrationConventions(t *testing.T) {
    released := parseReleased(t)

    for _, dialect := range []string{"sqlite", "postgres"} {
        t.Run(dialect, func(t *testing.T) {
            var fsys fs.FS
            var err error
            switch dialect {
            case "sqlite":
                fsys, err = fs.Sub(sqliteMigrations, "migrations/sqlite")
            case "postgres":
                fsys, err = fs.Sub(postgresMigrations, "migrations/postgres")
            }
            if err != nil {
                t.Fatal(err)
            }
            checkConventions(t, dialect, fsys, released)
        })
    }

    // Cross-dialect: matching migration numbers
    sqliteNums := migrationNumbers(t, sqliteMigrations, "migrations/sqlite")
    pgNums := migrationNumbers(t, postgresMigrations, "migrations/postgres")
    if !reflect.DeepEqual(sqliteNums, pgNums) {
        t.Errorf("migration numbers differ: sqlite=%v postgres=%v",
            sqliteNums, pgNums)
    }
}

func checkConventions(t *testing.T, dialect string, fsys fs.FS, released map[int]bool) {
    t.Helper()

    entries, err := fs.ReadDir(fsys, ".")
    if err != nil {
        t.Fatal(err)
    }

    ups := map[int]string{}
    downs := map[int]string{}

    for _, e := range entries {
        name := e.Name()
        if !strings.HasSuffix(name, ".sql") {
            continue
        }

        // Parse NNN_description.{up,down}.sql
        parts := strings.SplitN(name, "_", 2)
        num, err := strconv.Atoi(parts[0])
        if err != nil {
            t.Errorf("%s: migration number is not an integer: %q", name, parts[0])
            continue
        }

        switch {
        case strings.HasSuffix(name, ".up.sql"):
            ups[num] = name
        case strings.HasSuffix(name, ".down.sql"):
            downs[num] = name
        default:
            t.Errorf("%s: unexpected suffix (want .up.sql or .down.sql)", name)
        }
    }

    // Every up has a matching down and vice versa
    for num, name := range ups {
        if _, ok := downs[num]; !ok {
            t.Errorf("%s: missing matching .down.sql", name)
        }
    }
    for num, name := range downs {
        if _, ok := ups[num]; !ok {
            t.Errorf("%s: missing matching .up.sql", name)
        }
    }

    // Sequential numbering with no gaps
    var nums []int
    for num := range ups {
        nums = append(nums, num)
    }
    sort.Ints(nums)
    for i, num := range nums {
        expected := i + 1
        if num != expected {
            t.Errorf("gap in migration numbering: expected %03d, got %03d", expected, num)
        }
    }

    // No empty files
    for _, name := range ups {
        checkNonEmpty(t, fsys, name)
    }
    for _, name := range downs {
        checkNonEmpty(t, fsys, name)
    }

    // Phase tags (up files only)
    phases := checkPhaseTags(t, fsys, ups)

    // Contract referential integrity
    for num, phase := range phases {
        if phase != "contract" {
            continue
        }
        refs := contractRefs(t, fsys, ups[num])
        if len(refs) == 0 {
            t.Errorf("%s: contract migration missing -- contracts: NNN", ups[num])
            continue
        }
        for _, ref := range refs {
            if ref >= num {
                t.Errorf("%s: contracts reference %03d must be lower than %03d",
                    ups[num], ref, num)
            }
            if _, ok := ups[ref]; !ok {
                t.Errorf("%s: contracts reference %03d does not exist",
                    ups[num], ref)
            } else if phases[ref] != "expand" {
                t.Errorf("%s: contracts reference %03d is %q, not expand",
                    ups[num], ref, phases[ref])
            }
            if !released[ref] {
                t.Errorf("%s: contracts reference %03d has not been released "+
                    "(not in released.txt)", ups[num], ref)
            }
        }
    }
}

// checkPhaseTags verifies every .up.sql has a valid -- phase: tag on its
// first line and returns the phase for each migration number.
func checkPhaseTags(t *testing.T, fsys fs.FS, ups map[int]string) map[int]string {
    t.Helper()
    phases := map[int]string{}
    for num, name := range ups {
        data, err := fs.ReadFile(fsys, name)
        if err != nil {
            t.Fatal(err)
        }
        first, _, _ := strings.Cut(string(data), "\n")
        first = strings.TrimSpace(first)
        switch first {
        case "-- phase: expand":
            phases[num] = "expand"
        case "-- phase: contract":
            phases[num] = "contract"
        default:
            t.Errorf("%s: first line must be '-- phase: expand' or '-- phase: contract', got %q",
                name, first)
        }
    }
    return phases
}

// contractRefs parses -- contracts: NNN[, NNN...] from a migration file.
func contractRefs(t *testing.T, fsys fs.FS, name string) []int {
    t.Helper()
    data, err := fs.ReadFile(fsys, name)
    if err != nil {
        t.Fatal(err)
    }
    var refs []int
    for _, line := range strings.Split(string(data), "\n") {
        line = strings.TrimSpace(line)
        if !strings.HasPrefix(line, "-- contracts:") {
            continue
        }
        val := strings.TrimPrefix(line, "-- contracts:")
        for _, s := range strings.Split(val, ",") {
            s = strings.TrimSpace(s)
            if s == "" {
                continue
            }
            num, err := strconv.Atoi(s)
            if err != nil {
                t.Errorf("%s: invalid contracts reference %q", name, s)
                continue
            }
            refs = append(refs, num)
        }
    }
    return refs
}

// parseReleased reads migrations/released.txt and returns the set of
// migration numbers that have shipped in a release.
func parseReleased(t *testing.T) map[int]bool {
    t.Helper()
    data, err := releasedFile.ReadFile("migrations/released.txt")
    if err != nil {
        t.Fatal(err)
    }
    released := map[int]bool{}
    for _, line := range strings.Split(string(data), "\n") {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }
        fields := strings.Fields(line)
        if len(fields) < 2 {
            t.Errorf("released.txt: malformed line: %q", line)
            continue
        }
        num, err := strconv.Atoi(fields[0])
        if err != nil {
            t.Errorf("released.txt: invalid migration number: %q", fields[0])
            continue
        }
        released[num] = true
    }
    return released
}

func checkNonEmpty(t *testing.T, fsys fs.FS, name string) {
    t.Helper()
    data, err := fs.ReadFile(fsys, name)
    if err != nil {
        t.Fatal(err)
    }
    content := strings.TrimSpace(string(data))
    if content == "" {
        t.Errorf("%s: migration file is empty", name)
    }
}

func migrationNumbers(t *testing.T, embedFS embed.FS, dir string) []int {
    t.Helper()
    fsys, err := fs.Sub(embedFS, dir)
    if err != nil {
        t.Fatal(err)
    }
    entries, err := fs.ReadDir(fsys, ".")
    if err != nil {
        t.Fatal(err)
    }
    seen := map[int]bool{}
    for _, e := range entries {
        parts := strings.SplitN(e.Name(), "_", 2)
        num, err := strconv.Atoi(parts[0])
        if err != nil {
            continue
        }
        seen[num] = true
    }
    var nums []int
    for num := range seen {
        nums = append(nums, num)
    }
    sort.Ints(nums)
    return nums
}
```

### Step 6: Up-down-up roundtrip test

`TestMigrateRoundtrip` in the same file. Opens a database without
auto-migration (using a raw connection), then drives migrations
manually via `newMigrator()`.

The test needs databases that have not already been migrated. For
SQLite, open a raw `:memory:` database. For PostgreSQL, create a fresh
empty database (bypassing the template clone, which is pre-migrated).

```go
func TestMigrateRoundtrip(t *testing.T) {
    t.Run("sqlite", func(t *testing.T) {
        db := openRawSQLite(t)
        roundtrip(t, db)
    })
    t.Run("postgres", func(t *testing.T) {
        db := openRawPostgres(t)
        if db == nil {
            return // skipped
        }
        roundtrip(t, db)
    })
}

func roundtrip(t *testing.T, db *DB) {
    t.Helper()

    m, err := db.newMigrator()
    if err != nil {
        t.Fatal(err)
    }

    // Up
    if err := m.Up(); err != nil {
        t.Fatalf("initial up: %v", err)
    }
    schemaAfterUp := dumpSchema(t, db)

    // Need a fresh migrator — golang-migrate is stateful
    m, err = db.newMigrator()
    if err != nil {
        t.Fatal(err)
    }

    // Down
    if err := m.Down(); err != nil {
        t.Fatalf("down: %v", err)
    }

    m, err = db.newMigrator()
    if err != nil {
        t.Fatal(err)
    }

    // Up again
    if err := m.Up(); err != nil {
        t.Fatalf("second up: %v", err)
    }
    schemaAfterRoundtrip := dumpSchema(t, db)

    if schemaAfterUp != schemaAfterRoundtrip {
        t.Errorf("schema differs after up-down-up roundtrip:\n--- after first up ---\n%s\n--- after roundtrip ---\n%s",
            schemaAfterUp, schemaAfterRoundtrip)
    }
}
```

The `openRawSQLite` and `openRawPostgres` helpers open database
connections without calling `runMigrations()`:

```go
func openRawSQLite(t *testing.T) *DB {
    t.Helper()
    f, err := os.CreateTemp("", "blockyard-roundtrip-*.db")
    if err != nil {
        t.Fatal(err)
    }
    path := f.Name()
    f.Close()
    t.Cleanup(func() { os.Remove(path) })

    db, err := sqlx.Open("sqlite", path+"?_pragma=foreign_keys(1)")
    if err != nil {
        t.Fatal(err)
    }
    db.SetMaxOpenConns(1)
    t.Cleanup(func() { db.Close() })

    return &DB{DB: db, dialect: DialectSQLite}
}

func openRawPostgres(t *testing.T) *DB {
    t.Helper()
    if pgBaseURL == "" {
        t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set")
        return nil
    }

    dbName := "test_rt_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]
    admin, err := sql.Open("pgx", pgBaseURL)
    if err != nil {
        t.Fatal(err)
    }
    if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
        admin.Close()
        t.Fatal(err)
    }
    admin.Close()

    testURL := replaceDBName(pgBaseURL, dbName)
    rawDB, err := sqlx.Open("pgx", testURL)
    if err != nil {
        t.Fatal(err)
    }
    rawDB.SetMaxOpenConns(5)

    t.Cleanup(func() {
        rawDB.Close()
        cleanup, _ := sql.Open("pgx", pgBaseURL)
        cleanup.Exec("DROP DATABASE IF EXISTS " + dbName)
        cleanup.Close()
    })

    return &DB{DB: rawDB, dialect: DialectPostgres, connURL: testURL}
}
```

### Step 7: Pre-migration backup utility

New file: `internal/db/backup.go`. Used by `by admin update` (phase
3-5) before starting the new server.

```go
package db

import (
    "bytes"
    "context"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "time"
)

// Backup creates a point-in-time backup of the database.
//
// SQLite: VACUUM INTO to {path}.backup.{timestamp} — an atomic,
// consistent snapshot safe for live databases under concurrent access.
// PostgreSQL: pg_dump --format=custom to {dbname}.backup.{timestamp}.
//
// Returns the path to the backup file.
func (db *DB) Backup(ctx context.Context) (string, error) {
    switch db.dialect {
    case DialectSQLite:
        return db.backupSQLite(ctx)
    case DialectPostgres:
        return db.backupPostgres(ctx)
    default:
        return "", fmt.Errorf("backup: unsupported dialect")
    }
}

func (db *DB) backupSQLite(ctx context.Context) (string, error) {
    if db.tempPath != "" {
        return "", fmt.Errorf("backup: cannot back up in-memory database")
    }

    var seq int
    var name, path string
    if err := db.QueryRowContext(ctx,
        "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
        return "", fmt.Errorf("backup: resolve database path: %w", err)
    }

    ts := time.Now().UTC().Format("20060102T150405Z")
    dest := path + ".backup." + ts

    // VACUUM INTO creates an atomic, consistent snapshot of the database.
    // Unlike a raw file copy, it is safe while the server is concurrently
    // reading and writing — SQLite handles the locking internally.
    if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
        return "", fmt.Errorf("backup: vacuum into: %w", err)
    }

    return dest, nil
}

func (db *DB) backupPostgres(ctx context.Context) (string, error) {
    var dbname string
    if err := db.QueryRowContext(ctx,
        "SELECT current_database()").Scan(&dbname); err != nil {
        return "", fmt.Errorf("backup: resolve database name: %w", err)
    }

    ts := time.Now().UTC().Format("20060102T150405Z")
    dest := filepath.Join(".", dbname+".backup."+ts)

    // Use the stored connection URL so pg_dump inherits the full DSN
    // including credentials. The connURL field is set in openPostgres().
    cmd := exec.CommandContext(ctx, "pg_dump", //nolint:gosec // connURL is from our own config, not user input
        "--format=custom", "--dbname="+db.connURL, "-f", dest)
    var stderr bytes.Buffer
    cmd.Stderr = &stderr
    if err := cmd.Run(); err != nil {
        os.Remove(dest)
        return "", fmt.Errorf("backup: pg_dump: %w: %s", err, stderr.String())
    }

    return dest, nil
}
```

**`backupSQLite` note:** `VACUUM INTO` (SQLite 3.27.0+, supported by
`modernc.org/sqlite`) creates an atomic, consistent snapshot of the
database. Unlike a raw file copy, it is safe while the server is
concurrently reading and writing — SQLite acquires a read lock and
writes a complete copy. No need to handle WAL files or journal mode.

**`backupPostgres` note:** `pg_dump` needs the full connection URL
including credentials. The `DB` struct gains a `connURL` field (set in
`openPostgres()`) so the backup can pass it via `--dbname=`. This
avoids fragile SQL introspection of connection parameters.

#### Backup tests

New file: `internal/db/backup_test.go`.

```go
func TestBackupSQLite(t *testing.T) {
    // Create a real file-backed database (not :memory:).
    dir := t.TempDir()
    path := filepath.Join(dir, "test.db")
    db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: path})
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    // Insert some data so backup is non-trivial.
    _, err = db.CreateApp("test-app", "admin")
    if err != nil {
        t.Fatal(err)
    }

    dest, err := db.Backup(context.Background())
    if err != nil {
        t.Fatal(err)
    }

    // Verify backup file exists and is non-empty.
    info, err := os.Stat(dest)
    if err != nil {
        t.Fatalf("backup file not found: %v", err)
    }
    if info.Size() == 0 {
        t.Error("backup file is empty")
    }

    // Verify backup is a valid SQLite database.
    backupDB, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: dest})
    if err != nil {
        t.Fatalf("cannot open backup: %v", err)
    }
    defer backupDB.Close()

    app, err := backupDB.GetAppByName("test-app")
    if err != nil || app == nil {
        t.Error("backup does not contain expected data")
    }
}

func TestBackupSQLiteMemoryFails(t *testing.T) {
    db := testDB(t) // in-memory
    _, err := db.Backup(context.Background())
    if err == nil {
        t.Error("expected error backing up in-memory database")
    }
}

func TestBackupPostgres(t *testing.T) {
    if pgBaseURL == "" {
        t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set")
    }

    // Check pg_dump is available.
    if _, err := exec.LookPath("pg_dump"); err != nil {
        t.Skip("pg_dump not available")
    }

    db := testPostgresDB(t)

    _, err := db.CreateApp("test-app", "admin")
    if err != nil {
        t.Fatal(err)
    }

    dest, err := db.Backup(context.Background())
    if err != nil {
        // pg_dump fails on version mismatch (e.g., pg_dump 15 vs server 17).
        if strings.Contains(err.Error(), "version mismatch") {
            t.Skip("pg_dump version mismatch with server")
        }
        t.Fatal(err)
    }
    defer os.Remove(dest)

    info, err := os.Stat(dest)
    if err != nil {
        t.Fatalf("backup file not found: %v", err)
    }
    if info.Size() == 0 {
        t.Error("backup file is empty")
    }
}
```

## Design decisions

1. **Consolidate existing migrations.** No persistent installations
   exist, so there's no upgrade path to preserve. A single `001_initial`
   eliminates the SQLite-missing-004 gap and gives a clean baseline for
   the convention check. All future migrations start at 002 under the
   new backward-compatibility rules.

2. **Atlas Community over regex-based DDL linting.** Atlas parses SQL
   properly — it resolves table references, tracks state across
   statements, and handles comments and string literals correctly. A
   regex linter is fragile (multi-line statements, comments, exceptions
   like "DROP TABLE unless CREATE'd in the same file"). Atlas Community
   (Apache 2.0) includes generic destructive-change detection for free.
   The paywalled PostgreSQL-specific rules (lock duration analysis) are
   not our primary concern — we care about backward-incompatible DDL.

3. **No regex DDL linter.** Atlas subsumes it entirely. Maintaining both
   adds complexity with no unique value.

4. **Deferred `migration-compat` CI job.** The definitive backward-
   compatibility check (run old code's tests against new schema) is
   deferred to phase 3-5 when rolling updates land. The value of this
   job scales with the number of releases — right now there's only
   `v0.0.1` / `v0.0.2`, and both predate the migration consolidation.

5. **Dialect-specific no-ops over gap tolerance.** Requiring matching
   migration numbers across dialects with explicit `-- no-op: <reason>`
   files is simpler than teaching the convention check about allowable
   gaps. It makes the SQLite/PostgreSQL divergence visible in code
   review.

6. **`newMigrator()` extraction.** The roundtrip test needs to drive
   `Up()` / `Down()` manually. Extracting the migrator constructor from
   `runMigrations()` avoids duplicating the setup logic. The production
   path (`runMigrations`) is unchanged — it just calls `newMigrator()`
   internally.

7. **Schema dump via SQL introspection, not `pg_dump`.** The roundtrip
   test needs a deterministic schema comparison. `sqlite_master` and
   `information_schema`/`pg_indexes`/`pg_proc`/`pg_trigger`/`pg_policy`
   produce stable, sorted output without external tool dependencies.
   `pg_dump` output varies across versions and includes comments that
   complicate diffing. The PostgreSQL dump covers columns, indexes,
   CHECK constraints, foreign keys, functions, triggers, and RLS
   policies — a complete picture of the public schema. This avoids
   relying on duplicate-object errors to catch missing `DROP`s in down
   migrations, which would silently fail for any object created with
   `CREATE OR REPLACE`.

8. **Pre-migration backup as a `DB` method.** The backup utility lives
   on the `DB` struct because it needs dialect awareness and a live
   connection (for `VACUUM INTO` on SQLite and the stored connection URL
   for PostgreSQL `pg_dump`). It's called by `by admin update`
   (phase 3-5) before launching the new server.

9. **`VACUUM INTO` over raw file copy for SQLite backup.** A raw
   `io.Copy` of the database file is unsafe when the server is
   concurrently writing — it can produce a corrupt or incomplete copy.
   `VACUUM INTO` (SQLite 3.27.0+, supported by `modernc.org/sqlite`)
   creates an atomic, consistent snapshot by acquiring a read lock
   internally. No need to handle WAL files or journal mode.

10. **Store `connURL` on `DB` struct for PostgreSQL backup.** The
    `pg_dump` command needs the full connection URL including
    credentials. Reconstructing it from SQL introspection
    (`inet_server_addr()`, `current_user`) is fragile and cannot recover
    the password. Storing the URL at open time is simple and no worse
    security-wise than the connection pool already holding it in memory.

11. **Explicit phase tags over implicit classification.** Each `.up.sql`
    must declare `-- phase: expand` or `-- phase: contract` on its first
    line. Contract migrations must reference the expand(s) they clean up
    via `-- contracts: NNN`. This lets the convention check enforce
    referential integrity mechanically — contracts reference real,
    lower-numbered expands. Tags live only in `.up.sql` files — the
    `.down.sql` inherits the phase from its pair, avoiding redundant
    annotations.

12. **`released.txt` for enforceable release-cycle gating.** The "one
    full release cycle must have passed" rule is enforced via
    `migrations/released.txt`, which maps migration numbers to the
    release they shipped in. The convention check verifies that every
    contract's referenced expand appears in this file. The release
    process maintains the file — when a release is tagged, new migration
    numbers are appended. This eliminates a judgment call that AI agents
    would otherwise need to make by inspecting git tags, and turns it
    into a simple set-membership check.
