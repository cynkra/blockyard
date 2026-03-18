# Phase 2-1: Database Dual-Backend Foundation

Migrate the database layer from raw `database/sql` + inline DDL to `sqlx` +
`golang-migrate`. Support both SQLite and PostgreSQL as a config option.
This is the foundation for v2 — board storage (phase 2-4) requires
PostgreSQL, and v4 multi-node HA requires it for session/worker stores.

There are no existing deployments to migrate — we start clean with proper
versioned migrations from day one.

## Deliverables

1. New dependencies — `sqlx`, `golang-migrate`, `pgx/v5`
2. Versioned migrations via `golang-migrate` — extract inline DDL from
   `db.go` into `migrations/sqlite/` and `migrations/postgres/` embedded
   via `embed.FS`
3. `sqlx` conversion — replace `*sql.DB` with `*sqlx.DB`, add `db` struct
   tags to row types, convert queries to use `Rebind` and struct scanning
4. PostgreSQL driver — register `pgx/v5/stdlib`, add `openPostgres()` path
5. Dialect helpers — `internal/db/dialect.go` with
   `IsUniqueConstraintError` dispatch and portable SQL helpers
6. Config changes — add `driver` and `url` fields to `DatabaseConfig`,
   validation rules
7. Test infrastructure — dual-backend tests; PostgreSQL in devcontainer
   and CI
8. CI updates — add PostgreSQL service to the `check` job

## Step-by-step

### Step 1: New dependencies

Add to `go.mod`:

```
require (
    github.com/jmoiron/sqlx              v1.4.0
    github.com/golang-migrate/migrate/v4 v4.18.1
    github.com/jackc/pgx/v5              v5.7.2
)
```

**Why these specific libraries:**

- **sqlx** — thin layer over `database/sql`. `Rebind()` rewrites `?`
  placeholders to `$1,$2,...` for PostgreSQL. `Get()` / `Select()` map
  result rows to structs via `db` tags. Not an ORM — all existing SQL
  stays recognizable.
- **golang-migrate** — versioned up/down migration files. Supports both
  SQLite and PostgreSQL drivers. Embedded via `embed.FS` for single-binary
  distribution.
- **pgx/v5** — the standard PostgreSQL driver for Go. Used via its
  `stdlib` adapter to register with `database/sql` (which sqlx wraps).

### Step 2: Config changes

Expand `DatabaseConfig` in `internal/config/config.go`:

```go
type DatabaseConfig struct {
    Driver string `toml:"driver"` // "sqlite" (default) or "postgres"
    Path   string `toml:"path"`   // used when driver = "sqlite"
    URL    string `toml:"url"`    // PostgreSQL connection string; used when driver = "postgres"
}
```

**Defaults** — add to `applyDefaults()`:

```go
if cfg.Database.Driver == "" {
    cfg.Database.Driver = "sqlite"
}
// existing: cfg.Database.Path default "/data/db/blockyard.db"
```

**Validation** — replace the current `database.path` directory check in
`validate()`:

```go
switch cfg.Database.Driver {
case "sqlite":
    dbDir := filepath.Dir(cfg.Database.Path)
    if err := ensureDirWritable(dbDir, "database.path parent directory"); err != nil {
        return err
    }
case "postgres":
    if cfg.Database.URL == "" {
        return fmt.Errorf("config: database.url is required when driver = \"postgres\"")
    }
default:
    return fmt.Errorf("config: database.driver must be \"sqlite\" or \"postgres\", got %q", cfg.Database.Driver)
}
```

Env var overlay works automatically via the reflection walker:
`BLOCKYARD_DATABASE_DRIVER`, `BLOCKYARD_DATABASE_URL`.

**Config test additions:**

- Valid: `driver = "sqlite"` with `path` set
- Valid: `driver = "postgres"` with `url` set
- Invalid: `driver = "postgres"` with empty `url`
- Invalid: `driver = "sqlite"` with unwritable path
- Invalid: `driver = "banana"`
- Env var override: `BLOCKYARD_DATABASE_DRIVER=postgres` +
  `BLOCKYARD_DATABASE_URL=...`

### Step 3: Migration files

Extract the inline DDL from `db.go` (lines 16-88) into versioned migration
files. The `schema` constant and `db.Exec(schema)` call in `Open()` are
removed — `golang-migrate` handles schema creation.

#### Directory layout

```
migrations/
├── sqlite/
│   ├── 001_initial.up.sql
│   └── 001_initial.down.sql
└── postgres/
    ├── 001_initial.up.sql
    └── 001_initial.down.sql
```

#### `migrations/sqlite/001_initial.up.sql`

Direct extraction of the current schema — identical SQL, just
`CREATE TABLE IF NOT EXISTS` changed to `CREATE TABLE`:

```sql
CREATE TABLE apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    access_type             TEXT NOT NULL DEFAULT 'acl'
                            CHECK (access_type IN ('acl', 'logged_in', 'public')),
    active_bundle           TEXT,
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    title                   TEXT,
    description             TEXT,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    uploaded_at TEXT NOT NULL
);

CREATE INDEX idx_bundles_app_id ON bundles(app_id);

-- Deferred FK: apps.active_bundle → bundles.id.
-- SQLite cannot add a FK after table creation, so we rely on application
-- logic to enforce the reference (same as current behavior). The column
-- is nullable to allow app creation before any bundle exists.

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

CREATE INDEX idx_pat_token_hash
    ON personal_access_tokens(token_hash);
CREATE INDEX idx_pat_user_sub
    ON personal_access_tokens(user_sub);

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

#### `migrations/sqlite/001_initial.down.sql`

```sql
DROP TABLE IF EXISTS app_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS personal_access_tokens;
DROP TABLE IF EXISTS app_access;
DROP TABLE IF EXISTS bundles;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS users;
```

#### `migrations/postgres/001_initial.up.sql`

PostgreSQL dialect of the same schema. Key differences from SQLite:

- `TEXT` timestamps stay `TEXT` (not `TIMESTAMPTZ`) — the Go layer
  formats RFC3339 strings and the schema stays consistent across
  dialects. PostgreSQL-only tables (boards, added later) use native
  `TIMESTAMPTZ` since they're never shared with SQLite.
- `INTEGER ... DEFAULT 1` for booleans stays as-is — PostgreSQL accepts
  integer columns and the Go layer reads them as `bool` via
  `database/sql` scanning. No need to change to `BOOLEAN` when
  the existing code and tests work with integers.
- `BLOB` → `BYTEA` for `token_hash`.
- `active_bundle` FK can be a proper `REFERENCES` since PostgreSQL
  handles circular FKs with nullable columns.

```sql
CREATE TABLE apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
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
    updated_at              TEXT NOT NULL
);

CREATE TABLE bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    uploaded_at TEXT NOT NULL
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

CREATE INDEX idx_pat_token_hash
    ON personal_access_tokens(token_hash);
CREATE INDEX idx_pat_user_sub
    ON personal_access_tokens(user_sub);

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

#### `migrations/postgres/001_initial.down.sql`

```sql
DROP TABLE IF EXISTS app_tags;
DROP TABLE IF EXISTS tags;
DROP TABLE IF EXISTS personal_access_tokens;
DROP TABLE IF EXISTS app_access;
ALTER TABLE apps DROP CONSTRAINT IF EXISTS fk_apps_active_bundle;
DROP TABLE IF EXISTS bundles;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS users;
```

#### Embedding

Add an `embed.go` file (or add the declarations to `db.go`) that
embeds both migration directories:

```go
package db

import "embed"

//go:embed migrations/sqlite/*.sql
var sqliteMigrations embed.FS

//go:embed migrations/postgres/*.sql
var postgresMigrations embed.FS
```

The `migrations/` directory moves under `internal/db/` so it's
co-located with the embed declarations:

```
internal/db/
├── db.go
├── dialect.go
├── migrations/
│   ├── sqlite/
│   │   ├── 001_initial.up.sql
│   │   └── 001_initial.down.sql
│   └── postgres/
│       ├── 001_initial.up.sql
│       └── 001_initial.down.sql
└── db_test.go
```

This replaces the top-level `migrations/` directory in the v2 plan.
Embedding requires the directory to be under the package that declares
the `embed.FS`. Keeping them together avoids a separate embed package.

### Step 4: DB struct and Open() rewrite

Replace the `DB` struct and `Open()` function in `internal/db/db.go`.

**New struct:**

```go
type Dialect int

const (
    DialectSQLite Dialect = iota
    DialectPostgres
)

type DB struct {
    *sqlx.DB
    dialect  Dialect
    tempPath string // non-empty when using a temp file for SQLite :memory:
}
```

**New Open():**

```go
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

**openSQLite():**

```go
func openSQLite(path string) (*DB, error) {
    var tempPath string
    dsn := path + "?_pragma=foreign_keys(1)"
    if path == ":memory:" {
        f, err := os.CreateTemp("", "blockyard-*.db")
        if err != nil {
            return nil, fmt.Errorf("create temp db: %w", err)
        }
        tempPath = f.Name()
        f.Close()
        dsn = tempPath + "?_pragma=foreign_keys(1)"
    } else if dir := filepath.Dir(path); dir != "." {
        if err := os.MkdirAll(dir, 0o700); err != nil {
            return nil, fmt.Errorf("create db directory: %w", err)
        }
    }

    db, err := sqlx.Open("sqlite", dsn)
    if err != nil {
        return nil, fmt.Errorf("open sqlite: %w", err)
    }

    db.SetMaxOpenConns(1)

    if err := db.Ping(); err != nil {
        db.Close()
        return nil, fmt.Errorf("ping sqlite: %w", err)
    }

    d := &DB{DB: db, dialect: DialectSQLite, tempPath: tempPath}
    if err := d.runMigrations(); err != nil {
        db.Close()
        return nil, err
    }
    return d, nil
}
```

**openPostgres():**

```go
func openPostgres(url string) (*DB, error) {
    db, err := sqlx.Open("pgx", url)
    if err != nil {
        return nil, fmt.Errorf("open postgres: %w", err)
    }

    // Reasonable pool defaults — tune via connection string parameters
    // if needed (e.g. ?pool_max_conns=20).
    db.SetMaxOpenConns(25)
    db.SetMaxIdleConns(5)
    db.SetConnMaxLifetime(5 * time.Minute)

    if err := db.Ping(); err != nil {
        db.Close()
        return nil, fmt.Errorf("ping postgres: %w", err)
    }

    d := &DB{DB: db, dialect: DialectPostgres, tempPath: ""}
    if err := d.runMigrations(); err != nil {
        db.Close()
        return nil, err
    }
    return d, nil
}
```

**runMigrations():**

```go
func (db *DB) runMigrations() error {
    var source iofs.PartialDriver
    var fsys fs.FS
    var err error

    switch db.dialect {
    case DialectSQLite:
        fsys, err = fs.Sub(sqliteMigrations, "migrations/sqlite")
    case DialectPostgres:
        fsys, err = fs.Sub(postgresMigrations, "migrations/postgres")
    }
    if err != nil {
        return fmt.Errorf("migration fs: %w", err)
    }

    if err := source.Open(fsys); err != nil {
        return fmt.Errorf("migration source: %w", err)
    }

    driver, err := db.migrateDriver()
    if err != nil {
        return fmt.Errorf("migration driver: %w", err)
    }

    m, err := migrate.NewWithInstance("iofs", &source, db.driverName(), driver)
    if err != nil {
        return fmt.Errorf("create migrator: %w", err)
    }

    if err := m.Up(); err != nil && err != migrate.ErrNoChange {
        return fmt.Errorf("run migrations: %w", err)
    }

    return nil
}
```

The `migrateDriver()` helper returns either a `migrate/sqlite3` or
`migrate/postgres` database driver instance, wrapping the existing
`*sql.DB` connection.

**Import registration** — pgx driver must be registered. Add an import
alongside the existing sqlite import:

```go
import (
    _ "modernc.org/sqlite"
    _ "github.com/jackc/pgx/v5/stdlib"
)
```

### Step 5: Struct tags for row types

Add `db` tags to all row types so sqlx can map columns to fields.
Existing `json` tags are preserved where present.

```go
type AppRow struct {
    ID                   string   `db:"id" json:"id"`
    Name                 string   `db:"name" json:"name"`
    Owner                string   `db:"owner" json:"owner"`
    AccessType           string   `db:"access_type" json:"access_type"`
    ActiveBundle         *string  `db:"active_bundle" json:"active_bundle"`
    MaxWorkersPerApp     *int     `db:"max_workers_per_app" json:"max_workers_per_app"`
    MaxSessionsPerWorker int      `db:"max_sessions_per_worker" json:"max_sessions_per_worker"`
    MemoryLimit          *string  `db:"memory_limit" json:"memory_limit"`
    CPULimit             *float64 `db:"cpu_limit" json:"cpu_limit"`
    Title                *string  `db:"title" json:"title"`
    Description          *string  `db:"description" json:"description"`
    CreatedAt            string   `db:"created_at" json:"created_at"`
    UpdatedAt            string   `db:"updated_at" json:"updated_at"`
}

type BundleRow struct {
    ID         string `db:"id" json:"id"`
    AppID      string `db:"app_id" json:"app_id"`
    Status     string `db:"status" json:"status"`
    UploadedAt string `db:"uploaded_at" json:"uploaded_at"`
}

type UserRow struct {
    Sub       string `db:"sub" json:"sub"`
    Email     string `db:"email" json:"email"`
    Name      string `db:"name" json:"name"`
    Role      string `db:"role" json:"role"`
    Active    bool   `db:"active" json:"active"`
    LastLogin string `db:"last_login" json:"last_login"`
}

type AppAccessRow struct {
    AppID     string `db:"app_id"`
    Principal string `db:"principal"`
    Kind      string `db:"kind"`
    Role      string `db:"role"`
    GrantedBy string `db:"granted_by"`
    GrantedAt string `db:"granted_at"`
}

type TagRow struct {
    ID        string `db:"id"`
    Name      string `db:"name"`
    CreatedAt string `db:"created_at"`
}

type PATRow struct {
    ID         string  `db:"id" json:"id"`
    UserSub    string  `db:"user_sub" json:"user_sub,omitempty"`
    Name       string  `db:"name" json:"name"`
    CreatedAt  string  `db:"created_at" json:"created_at"`
    ExpiresAt  *string `db:"expires_at" json:"expires_at"`
    LastUsedAt *string `db:"last_used_at" json:"last_used_at"`
    Revoked    bool    `db:"revoked" json:"revoked"`
}
```

The `PATLookupResult` struct is not tagged — it's a join result that
uses inline scanning (see step 6).

### Step 6: Query conversion

Every query method gets two changes:

1. **Rebind** — `?` placeholders are rewritten for the active dialect
   via a helper method on `*DB`.
2. **Struct scanning** — `db.Get()` / `db.Select()` replace manual
   `Scan()` calls where the result maps cleanly to a tagged struct.

**Rebind helper:**

```go
// rebind rewrites ? placeholders for the active dialect.
func (db *DB) rebind(query string) string {
    return sqlx.Rebind(db.BindType(), query)
}
```

**Conversion patterns:**

Single-row query (before):
```go
func (db *DB) GetApp(id string) (*AppRow, error) {
    row := db.QueryRow(`SELECT `+appColumns+` FROM apps WHERE id = ?`, id)
    return scanApp(row)
}
```

Single-row query (after):
```go
func (db *DB) GetApp(id string) (*AppRow, error) {
    var app AppRow
    err := db.DB.Get(&app, db.rebind(`SELECT * FROM apps WHERE id = ?`), id)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    return &app, nil
}
```

Multi-row query (before):
```go
func (db *DB) ListApps() ([]AppRow, error) {
    rows, err := db.Query(`SELECT ` + appColumns + ` FROM apps ORDER BY created_at DESC`)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    return scanApps(rows)
}
```

Multi-row query (after):
```go
func (db *DB) ListApps() ([]AppRow, error) {
    var apps []AppRow
    err := db.DB.Select(&apps, `SELECT * FROM apps ORDER BY created_at DESC`)
    if err != nil {
        return nil, err
    }
    return apps, nil
}
```

Exec (before):
```go
_, err := db.Exec(
    `INSERT INTO apps (id, name, owner, max_sessions_per_worker, created_at, updated_at)
     VALUES (?, ?, ?, 1, ?, ?)`,
    id, name, owner, now, now,
)
```

Exec (after):
```go
_, err := db.DB.Exec(db.rebind(
    `INSERT INTO apps (id, name, owner, max_sessions_per_worker, created_at, updated_at)
     VALUES (?, ?, ?, 1, ?, ?)`),
    id, name, owner, now, now,
)
```

**`SELECT *` note:** struct scanning with `SELECT *` requires the struct
to have `db` tags for every column in the table. This works cleanly for
`AppRow`, `BundleRow`, `UserRow`, `TagRow`, and `AppAccessRow` — they
are 1:1 with their tables.

For `PATRow` (which omits `token_hash` from results) and
`PATLookupResult` (a JOIN), keep explicit column lists and manual
scanning or use a dedicated scan struct.

**Removals:** the `appColumns` constant, `scanApp()`, and `scanApps()`
helper functions are no longer needed and are deleted.

#### Portable SQL fixes

Three queries use SQLite-specific syntax. The fix in each case is to
write portable SQL, not dialect-conditional logic:

**1. `INSERT OR IGNORE` → `ON CONFLICT DO NOTHING`**

Current (`AddAppTag`):
```go
"INSERT OR IGNORE INTO app_tags (app_id, tag_id) VALUES (?, ?)"
```

Portable:
```go
"INSERT INTO app_tags (app_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING"
```

Both SQLite (3.24+) and PostgreSQL support this syntax.

**2. `LIKE` case sensitivity in `ListCatalog`**

Current:
```go
"(apps.name LIKE ? ESCAPE '\\' OR apps.title LIKE ? ESCAPE '\\' OR ...)"
```

The Go layer already generates lowercase search terms. Wrap columns in
`LOWER()` for consistent case-insensitive behavior on both dialects:

```go
"(LOWER(apps.name) LIKE LOWER(?) ESCAPE '\\' OR LOWER(apps.title) LIKE LOWER(?) ESCAPE '\\' OR ...)"
```

**3. Timestamps — already portable**

All timestamp values are formatted in Go via
`time.Now().UTC().Format(time.RFC3339)` and passed as parameters. No SQL
timestamp functions (`datetime('now')`, `now()`) are used. No changes
needed.

#### Transaction handling

`ActivateBundle()` uses `db.Begin()`. With sqlx this becomes
`db.Beginx()` returning `*sqlx.Tx`, which also has `Rebind()`:

```go
func (db *DB) ActivateBundle(appID, bundleID string) error {
    tx, err := db.Beginx()
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback()

    if _, err := tx.Exec(db.rebind(`UPDATE bundles SET status = 'ready' WHERE id = ?`), bundleID); err != nil {
        return fmt.Errorf("update bundle status: %w", err)
    }

    now := time.Now().UTC().Format(time.RFC3339)
    if _, err := tx.Exec(db.rebind(`UPDATE apps SET active_bundle = ?, updated_at = ? WHERE id = ?`), bundleID, now, appID); err != nil {
        return fmt.Errorf("set active bundle: %w", err)
    }

    return tx.Commit()
}
```

#### `revoked` column: int vs bool

The `revoked` column is `INTEGER NOT NULL DEFAULT 0` in both dialects.
SQLite has no native boolean; PostgreSQL accepts integers in integer
columns. The `PATRow.Revoked` field is `bool` in Go — `database/sql`
(and sqlx) scan integers to bools correctly on both drivers (0 → false,
1 → true). Similarly, when writing, Go's `bool` is sent as 0/1.

The queries `SET revoked = 1` and `WHERE revoked = 0` work on both
dialects. No changes needed.

### Step 7: Dialect helpers

New file `internal/db/dialect.go`:

```go
package db

import (
    "errors"
    "strings"

    "github.com/jackc/pgx/v5/pgconn"
)

// IsUniqueConstraintError reports whether err is a unique constraint
// violation, regardless of dialect.
func IsUniqueConstraintError(err error) bool {
    if err == nil {
        return false
    }

    // PostgreSQL: error code 23505 (unique_violation)
    var pgErr *pgconn.PgError
    if errors.As(err, &pgErr) {
        return pgErr.Code == "23505"
    }

    // SQLite: string match
    return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
```

This function is already used in the API layer (`api/apps.go`,
`api/tags.go`) to detect duplicate names. The signature is unchanged —
callers don't need to know which dialect is in use.

**driverName() helper** — returns the string identifier for
`golang-migrate`:

```go
func (db *DB) driverName() string {
    switch db.dialect {
    case DialectPostgres:
        return "postgres"
    default:
        return "sqlite"
    }
}
```

### Step 8: main.go wiring

One-line change in `cmd/blockyard/main.go`:

```go
// Before:
database, err := db.Open(cfg.Database.Path)

// After:
database, err := db.Open(cfg.Database)
```

The rest of the wiring (`server.NewServer(cfg, be, database)`,
`database.Close()`) is unchanged — the `*db.DB` type is the same from
the caller's perspective.

### Step 9: PostgreSQL in devcontainer

Add PostgreSQL to `.devcontainer/Dockerfile`:

```dockerfile
# Install PostgreSQL client tools (psql)
RUN apt-get update && apt-get install -y --no-install-recommends \
    postgresql-client \
    && rm -rf /var/lib/apt/lists/*
```

Add a PostgreSQL service to `.devcontainer/devcontainer.json` using
Docker Compose or a `docker-compose.yml` alongside the devcontainer.
The simpler approach — a compose file:

`.devcontainer/docker-compose.yml`:

```yaml
services:
  app:
    build:
      context: ..
      dockerfile: .devcontainer/Dockerfile
    volumes:
      - ..:/workspace:cached
      - blockyard-gomodcache:/home/dev/go/pkg/mod
    env_file: .env
    depends_on:
      postgres:
        condition: service_healthy

  postgres:
    image: postgres:17
    environment:
      POSTGRES_USER: blockyard
      POSTGRES_PASSWORD: blockyard
      POSTGRES_DB: blockyard_test
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U blockyard"]
      interval: 2s
      timeout: 5s
      retries: 5

volumes:
  blockyard-gomodcache:
```

Update `devcontainer.json` to use Docker Compose instead of a standalone
Dockerfile build:

```json
{
  "name": "blockyard (Go)",
  "dockerComposeFile": "docker-compose.yml",
  "service": "app",
  "workspaceFolder": "/workspace",
  ...
}
```

Set `BLOCKYARD_TEST_POSTGRES_URL` in `.devcontainer/.env`:

```
BLOCKYARD_TEST_POSTGRES_URL=postgres://blockyard:blockyard@postgres:5432/blockyard_test?sslmode=disable
```

### Step 10: Test infrastructure

**SQLite tests** — unchanged. The `testDB(t)` helper creates a
`:memory:` SQLite database:

```go
func testDB(t *testing.T) *DB {
    t.Helper()
    db, err := Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() { db.Close() })
    return db
}
```

**PostgreSQL tests** — a parallel helper that connects to a real
PostgreSQL instance and creates a unique test database per test:

```go
func testPostgresDB(t *testing.T) *DB {
    t.Helper()

    url := os.Getenv("BLOCKYARD_TEST_POSTGRES_URL")
    if url == "" {
        t.Skip("BLOCKYARD_TEST_POSTGRES_URL not set; skipping PostgreSQL tests")
    }

    // Create a unique database for this test to avoid cross-test pollution.
    dbName := "test_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

    // Connect to the default database to create the test database.
    adminDB, err := sql.Open("pgx", url)
    if err != nil {
        t.Fatal(err)
    }
    if _, err := adminDB.Exec("CREATE DATABASE " + dbName); err != nil {
        adminDB.Close()
        t.Fatal(err)
    }
    adminDB.Close()

    // Build the test database URL by replacing the database name.
    testURL := replaceDBName(url, dbName)
    db, err := Open(config.DatabaseConfig{Driver: "postgres", URL: testURL})
    if err != nil {
        t.Fatal(err)
    }

    t.Cleanup(func() {
        db.Close()
        // Drop the test database.
        cleanup, _ := sql.Open("pgx", url)
        cleanup.Exec("DROP DATABASE IF EXISTS " + dbName)
        cleanup.Close()
    })
    return db
}
```

**Dual-backend test runner** — a helper that runs a test function
against both SQLite and PostgreSQL:

```go
func eachDB(t *testing.T, fn func(t *testing.T, db *DB)) {
    t.Run("sqlite", func(t *testing.T) {
        fn(t, testDB(t))
    })
    t.Run("postgres", func(t *testing.T) {
        fn(t, testPostgresDB(t))
    })
}
```

All existing tests are converted to use `eachDB`:

```go
// Before:
func TestCreateAndGetApp(t *testing.T) {
    db := testDB(t)
    app, err := db.CreateApp("my-app", "admin")
    ...
}

// After:
func TestCreateAndGetApp(t *testing.T) {
    eachDB(t, func(t *testing.T, db *DB) {
        app, err := db.CreateApp("my-app", "admin")
        ...
    })
}
```

PostgreSQL subtests are skipped automatically when
`BLOCKYARD_TEST_POSTGRES_URL` is not set. `go test ./internal/db/...`
runs SQLite tests everywhere; PostgreSQL tests run in the devcontainer
and CI.

### Step 11: CI updates

Add a PostgreSQL service to the `check` job in
`.github/workflows/ci.yml`:

```yaml
check:
  runs-on: ubuntu-latest
  services:
    postgres:
      image: postgres:17
      env:
        POSTGRES_USER: blockyard
        POSTGRES_PASSWORD: blockyard
        POSTGRES_DB: blockyard_test
      ports:
        - 5432:5432
      options: >-
        --health-cmd "pg_isready -U blockyard"
        --health-interval 2s
        --health-timeout 5s
        --health-retries 5
  env:
    BLOCKYARD_TEST_POSTGRES_URL: postgres://blockyard:blockyard@localhost:5432/blockyard_test?sslmode=disable
  steps:
    - uses: actions/checkout@v6
    - uses: actions/setup-go@v6
      with:
        go-version: '1.24'
    - run: go vet ./...
    - run: go test -coverprofile=coverage-check.out ./...
    - uses: actions/upload-artifact@v7
      with:
        name: coverage-check
        path: coverage-check.out
```

The PostgreSQL service starts alongside the job runner. The env var
makes PostgreSQL tests run automatically. No build tags needed — the
`testPostgresDB` helper skips gracefully when the env var is absent.

## Full method conversion reference

For completeness, here is every `db.go` method and the conversion it
requires. Methods are grouped by complexity.

### Trivial (rebind + struct scan)

These methods need `db.rebind()` on the query and switch from manual
`Scan()` to `db.DB.Get()` or `db.DB.Select()`:

| Method | Change |
|--------|--------|
| `GetApp` | `Get(&app, rebind("SELECT * FROM apps WHERE id = ?"))` |
| `GetAppByName` | `Get(&app, rebind("SELECT * FROM apps WHERE name = ?"))` |
| `ListApps` | `Select(&apps, "SELECT * FROM apps ORDER BY created_at DESC")` |
| `GetBundle` | `Get(&bundle, rebind("SELECT * FROM bundles WHERE id = ?"))` |
| `ListBundlesByApp` | `Select(&bundles, rebind("SELECT * ... WHERE app_id = ?"))` |
| `GetUser` | `Get(&user, rebind("SELECT * FROM users WHERE sub = ?"))` |
| `ListUsers` | `Select(&users, "SELECT * FROM users ORDER BY last_login DESC")` |
| `ListAppAccess` | `Select(&grants, rebind("SELECT * FROM app_access WHERE app_id = ?"))` |
| `GetTag` | `Get(&tag, rebind("SELECT * FROM tags WHERE id = ?"))` |
| `ListTags` | `Select(&tags, "SELECT * FROM tags ORDER BY name")` |
| `ListAppTags` | `Select(&tags, rebind("SELECT t.* FROM tags t JOIN ..."))` |
| `ListPATsByUser` | `Select(&pats, rebind("SELECT id, name, ... WHERE user_sub = ?"))` |

### Exec-only (rebind only, no scanning)

| Method | Change |
|--------|--------|
| `CreateApp` | `rebind` on INSERT |
| `DeleteApp` | `rebind` on DELETE |
| `CreateBundle` | `rebind` on INSERT |
| `UpdateBundleStatus` | `rebind` on UPDATE |
| `SetActiveBundle` | `rebind` on UPDATE |
| `DeleteBundle` | `rebind` on DELETE |
| `UpdateApp` | `rebind` on UPDATE |
| `ClearActiveBundle` | `rebind` on UPDATE |
| `FailStaleBuilds` | no placeholders — unchanged |
| `UpsertUser` | `rebind` on INSERT ... ON CONFLICT |
| `UpsertUserWithRole` | `rebind` on INSERT ... ON CONFLICT |
| `UpdateUser` | `rebind` on UPDATE |
| `GrantAppAccess` | `rebind` on INSERT ... ON CONFLICT |
| `RevokeAppAccess` | `rebind` on DELETE |
| `CreateTag` | `rebind` on INSERT |
| `DeleteTag` | `rebind` on DELETE |
| `AddAppTag` | rewrite to `ON CONFLICT DO NOTHING` + `rebind` |
| `RemoveAppTag` | `rebind` on DELETE |
| `CreatePAT` | `rebind` on INSERT |
| `RevokePAT` | `rebind` on UPDATE |
| `RevokeAllPATs` | `rebind` on UPDATE |
| `UpdatePATLastUsed` | `rebind` on UPDATE |
| `Ping` | no placeholders — unchanged |

### Complex (custom handling)

| Method | Notes |
|--------|-------|
| `ListAccessibleApps` | Rebind + `Select`. Remove `fmt.Sprintf` for column list — use `SELECT a.* FROM apps a` instead |
| `ActivateBundle` | `Beginx()` + `rebind` on both statements |
| `ListCatalog` | Dynamic query building stays. Add `LOWER()` wrapping for search. Rebind the final query. Use `Select` for the data query, `Get` for the count |
| `LookupPATByHash` | JOIN query — keep explicit column list and manual scan (two structs from one row) |

## Design decisions

1. **Migrations under `internal/db/`, not top-level `migrations/`.** Go's
   `embed.FS` requires the directory to be at or below the package that
   declares the embed directive. Keeping migrations next to `db.go`
   avoids a separate embed package and makes the relationship obvious.

2. **`TEXT` timestamps in PostgreSQL (not `TIMESTAMPTZ`).** The Go layer
   already formats all timestamps as RFC3339 strings and passes them as
   parameters. Using `TEXT` keeps the schema identical across dialects
   for the shared tables. PostgreSQL-only tables (boards, added in
   phase 2-4) use native `TIMESTAMPTZ` since they never exist in SQLite.

3. **No `BOOLEAN` conversion for `active` / `revoked`.** Both dialects
   accept `INTEGER` columns with 0/1 values. Go's `database/sql` scans
   integers to `bool` correctly on both drivers. Changing to PostgreSQL
   `BOOLEAN` would create a schema divergence for no functional benefit.

4. **Per-test PostgreSQL databases.** Each test creates a uniquely-named
   database and drops it on cleanup. This allows parallel test execution
   and eliminates cross-test contamination. The overhead (~50ms per
   CREATE/DROP) is acceptable for a test suite.

5. **No build tag for PostgreSQL tests.** The `testPostgresDB` helper
   skips when `BLOCKYARD_TEST_POSTGRES_URL` is unset. This is simpler
   than build tags — developers who don't have PostgreSQL running see
   their tests pass (with postgres subtests skipped), not fail to
   compile. CI always sets the env var.

6. **`SELECT *` for struct-scanned queries.** With `db` tags on every
   struct field matching every column, `SELECT *` is safe and eliminates
   the `appColumns` constant that has to be kept in sync. When a column
   is added in a future migration, add the field + tag to the struct and
   all queries pick it up. The exception is `PATRow` queries that
   intentionally omit `token_hash` — these keep explicit column lists.

7. **Shared `eachDB` test helper over separate test files.** Running
   every test against both backends in a single test function catches
   dialect-specific bugs immediately. It's simpler than maintaining
   parallel test files or conditional logic spread across tests.
