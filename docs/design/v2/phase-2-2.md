# Phase 2-2: Quick Wins

Three independent features that build on the phase 2-1 foundation (sqlx,
migrations, dual-backend). Low risk, high usability impact. Each can be
implemented and reviewed independently; the step numbering within each
section is local.

## Deliverables

1. **Bundle rollback** — new endpoint `POST /api/v1/apps/{id}/rollback`.
   Validates the target bundle, drains active sessions, switches the
   active bundle, and returns the updated app.
2. **Soft-delete for apps** — `deleted_at` column, modified delete
   handler, restore endpoint, configurable retention period, and a
   background sweeper that purges expired apps.
3. **Resource limit validation and runtime verification** — API-level
   input validation for `memory_limit` and `cpu_limit` in `UpdateApp`,
   plus a post-spawn container inspection in the Docker backend that
   warns on mismatches. Resource limits are already enforced at container
   creation (wired in v0); this deliverable adds guardrails around it.

---

## Bundle Rollback

Activate a previous bundle for an app. When workers are running, the
endpoint drains active sessions before switching — same drain machinery
as `StopApp`.

**Endpoint:**

```
POST /api/v1/apps/{id}/rollback  { "bundle_id": "..." }

  1. Validate bundle exists, belongs to app, status = ready
  2. Reject if target is already the active bundle
  3. Stop running workers (drain + evict via stopAppSync)
  4. Set active_bundle = target bundle
  5. Return 200 { app details with new active_bundle }
```

**Permission:** `CanDeploy()` — same gate as bundle upload.
Collaborators, owners, and admins can rollback.

### Step 1: Rollback handler

New file content in `internal/api/apps.go` (appended to the existing
file):

```go
type rollbackRequest struct {
    BundleID string `json:"bundle_id"`
}

func RollbackApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        caller := auth.CallerFromContext(r.Context())
        id := chi.URLParam(r, "id")

        var body rollbackRequest
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
            badRequest(w, "invalid JSON body")
            return
        }

        if body.BundleID == "" {
            badRequest(w, "bundle_id is required")
            return
        }

        app, relation, ok := resolveAppRelation(srv, w, caller, id)
        if !ok {
            return
        }
        if !relation.CanDeploy() {
            notFound(w, "app not found")
            return
        }

        // Validate target bundle.
        b, err := srv.DB.GetBundle(body.BundleID)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if b == nil || b.AppID != app.ID {
            notFound(w, "bundle not found")
            return
        }
        if b.Status != "ready" {
            badRequest(w, "bundle is not ready (status: "+b.Status+")")
            return
        }
        if app.ActiveBundle != nil && *app.ActiveBundle == body.BundleID {
            badRequest(w, "bundle is already active")
            return
        }

        slog.Info("rolling back app",
            "app_id", app.ID, "name", app.Name,
            "target_bundle", body.BundleID, "caller", caller.Sub)

        // Capture previous bundle before switching.
        previousBundle := app.ActiveBundle

        // Drain and stop running workers.
        // stopAppSync waits up to shutdown_timeout for sessions to end,
        // then force-evicts. If no workers are running, this is a no-op.
        stopAppSync(srv, app.ID)

        // Switch active bundle.
        if err := srv.DB.SetActiveBundle(app.ID, body.BundleID); err != nil {
            serverError(w, "set active bundle: "+err.Error())
            return
        }

        // Re-read app to get updated state.
        app, err = srv.DB.GetApp(app.ID)
        if err != nil || app == nil {
            serverError(w, "get app after rollback")
            return
        }

        if srv.AuditLog != nil {
            srv.AuditLog.Emit(auditEntry(r, audit.ActionAppRollback, app.ID,
                map[string]any{
                    "bundle_id":          body.BundleID,
                    "previous_bundle_id": stringOrNil(previousBundle),
                }))
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
    }
}

func stringOrNil(s *string) any {
    if s == nil {
        return nil
    }
    return *s
}
```

**Rollback is synchronous.** The drain takes at most
`shutdown_timeout` (default 30s). This is acceptable for a
deployment operation — the caller (CLI, CI/CD, admin) needs to know the
rollback is complete before proceeding. If the app has no running
workers, the drain is a no-op and the response is instant.

### Step 2: Audit action

Add to `internal/audit/audit.go`:

```go
ActionAppRollback Action = "app.rollback"
```

### Step 3: Route registration

In `internal/api/router.go`, inside the `limitBody` group alongside the
existing app endpoints:

```go
r.Post("/apps/{id}/rollback", RollbackApp(srv))
```

### Step 4: Tests

**Unit tests** (in `internal/api/api_test.go`):

- Rollback to a valid ready bundle → 200, `active_bundle` updated
- Rollback to non-existent bundle → 404
- Rollback to a bundle belonging to a different app → 404
- Rollback to a bundle with status `failed` → 400
- Rollback to the already-active bundle → 400
- Rollback without `bundle_id` → 400
- Rollback without deploy permission → 404
- Rollback with running workers → 200, workers stopped, bundle switched

---

## Soft-Delete for Apps

Mark apps as deleted instead of immediate removal. A background sweeper
purges soft-deleted apps after a configurable retention period. A
restore endpoint allows undoing a delete before purge.

### Step 1: Migration 002 — soft-delete column

Add migration files under `internal/db/migrations/`:

**`sqlite/002_v2_soft_delete.up.sql`:**

```sql
ALTER TABLE apps ADD COLUMN deleted_at TEXT;

-- Replace the column-level UNIQUE on name with a partial unique index
-- that only covers live (non-deleted) apps. This allows a new app to
-- reuse the name of a soft-deleted app. The app ID (UUID) remains the
-- stable identifier for soft-deleted rows.
DROP INDEX IF EXISTS sqlite_autoindex_apps_1;
CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;
```

**`sqlite/002_v2_soft_delete.down.sql`:**

```sql
DROP INDEX IF EXISTS idx_apps_name_live;
ALTER TABLE apps DROP COLUMN deleted_at;
-- The column-level UNIQUE constraint is restored by dropping and
-- recreating the column (SQLite rebuilds the table).
```

**`postgres/002_v2_soft_delete.up.sql`:**

```sql
ALTER TABLE apps ADD COLUMN deleted_at TEXT;

-- Replace the column-level UNIQUE on name with a partial unique index.
ALTER TABLE apps DROP CONSTRAINT apps_name_key;
CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;
```

**`postgres/002_v2_soft_delete.down.sql`:**

```sql
DROP INDEX IF EXISTS idx_apps_name_live;
ALTER TABLE apps ADD CONSTRAINT apps_name_key UNIQUE (name);
ALTER TABLE apps DROP COLUMN deleted_at;
```

Same SQL on both dialects — `TEXT` timestamp, consistent with the
phase 2-1 schema decisions. `ALTER TABLE ... DROP COLUMN` is supported
by SQLite 3.35.0+ (2021-03) and modernc.org/sqlite includes this.

### Step 2: AppRow struct update

Add the `DeletedAt` field to `AppRow` in `internal/db/db.go`:

```go
type AppRow struct {
    // ... existing fields ...
    CreatedAt            string   `db:"created_at" json:"created_at"`
    UpdatedAt            string   `db:"updated_at" json:"updated_at"`
    DeletedAt            *string  `db:"deleted_at" json:"deleted_at,omitempty"`
}
```

The `json:"deleted_at,omitempty"` tag ensures the field is absent from
API responses when `nil` (non-deleted apps), keeping the JSON clean.

**`AppResponse` update** in `internal/api/apps.go`:

```go
type AppResponse struct {
    // ... existing fields ...
    Status    string   `json:"status"`
    Workers   []string `json:"workers"`
    DeletedAt *string  `json:"deleted_at,omitempty"`
}
```

Update `appResponse()` to copy the field:

```go
func appResponse(app *db.AppRow, workers *server.WorkerMap) AppResponse {
    // ... existing logic ...
    return AppResponse{
        // ... existing fields ...
        DeletedAt: app.DeletedAt,
    }
}
```

### Step 3: Query updates — add `deleted_at IS NULL` filters

Every query that reads live apps must exclude soft-deleted rows. With
phase 2-1's `SELECT *` and struct scanning, the column is already
picked up — only the `WHERE` clauses change.

**`GetApp`:**

```go
func (db *DB) GetApp(id string) (*AppRow, error) {
    var app AppRow
    err := db.DB.Get(&app, db.rebind(
        `SELECT * FROM apps WHERE id = ? AND deleted_at IS NULL`), id)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    return &app, nil
}
```

**`GetAppByName`:**

```go
func (db *DB) GetAppByName(name string) (*AppRow, error) {
    var app AppRow
    err := db.DB.Get(&app, db.rebind(
        `SELECT * FROM apps WHERE name = ? AND deleted_at IS NULL`), name)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    return &app, nil
}
```

**`ListApps`:**

```go
func (db *DB) ListApps() ([]AppRow, error) {
    var apps []AppRow
    err := db.DB.Select(&apps,
        `SELECT * FROM apps WHERE deleted_at IS NULL ORDER BY created_at DESC`)
    if err != nil {
        return nil, err
    }
    return apps, nil
}
```

**`ListAccessibleApps`:**

Add `AND a.deleted_at IS NULL` to the existing WHERE clause:

```go
query := `SELECT DISTINCT a.*
     FROM apps a
     LEFT JOIN app_access aa ON a.id = aa.app_id
     WHERE a.deleted_at IS NULL
       AND (a.access_type IN ('public', 'logged_in')
            OR a.owner = ?
            OR (aa.kind = 'user' AND aa.principal = ?))
     ORDER BY a.created_at DESC`
```

**`ListCatalog`:**

Add `apps.deleted_at IS NULL` as the first condition, always present:

```go
func (db *DB) ListCatalog(params CatalogParams) ([]AppRow, int, error) {
    conditions := []string{"apps.deleted_at IS NULL"}
    var args []any

    // ... existing access control, tag, and search filters ...
```

This ensures soft-deleted apps never appear in the catalog regardless
of caller role.

### Step 4: New DB methods — soft-delete lifecycle

Add to `internal/db/db.go`:

```go
// GetAppIncludeDeleted returns an app by ID regardless of soft-delete
// status. Used by the restore endpoint and the sweeper.
func (db *DB) GetAppIncludeDeleted(id string) (*AppRow, error) {
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

// SoftDeleteApp sets deleted_at on an app.
func (db *DB) SoftDeleteApp(id string) error {
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := db.DB.Exec(db.rebind(
        `UPDATE apps SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
        now, now, id,
    )
    return err
}

// RestoreApp clears deleted_at on a soft-deleted app.
func (db *DB) RestoreApp(id string) error {
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := db.DB.Exec(db.rebind(
        `UPDATE apps SET deleted_at = NULL, updated_at = ? WHERE id = ? AND deleted_at IS NOT NULL`),
        now, id,
    )
    return err
}

// HardDeleteApp permanently removes an app row. Used by the sweeper
// after all associated resources (bundles, files) have been cleaned up.
func (db *DB) HardDeleteApp(id string) error {
    _, err := db.DB.Exec(db.rebind(`DELETE FROM apps WHERE id = ?`), id)
    return err
}

// ListDeletedApps returns all soft-deleted apps, newest deletion first.
func (db *DB) ListDeletedApps() ([]AppRow, error) {
    var apps []AppRow
    err := db.DB.Select(&apps,
        `SELECT * FROM apps WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC`)
    if err != nil {
        return nil, err
    }
    return apps, nil
}

// ListExpiredDeletedApps returns soft-deleted apps whose deleted_at is
// older than the given cutoff time. Used by the sweeper.
func (db *DB) ListExpiredDeletedApps(cutoff string) ([]AppRow, error) {
    var apps []AppRow
    err := db.DB.Select(&apps, db.rebind(
        `SELECT * FROM apps WHERE deleted_at IS NOT NULL AND deleted_at < ?
         ORDER BY deleted_at ASC`),
        cutoff,
    )
    if err != nil {
        return nil, err
    }
    return apps, nil
}
```

The original `DeleteApp` method (hard delete by ID) is renamed to
`HardDeleteApp`. The name `DeleteApp` is no longer used as a DB method
— the API handler decides whether to soft-delete or hard-delete based
on config.

### Step 5: Config addition — soft_delete_retention

Expand `StorageConfig` in `internal/config/config.go`:

```go
type StorageConfig struct {
    BundleServerPath    string   `toml:"bundle_server_path"`
    BundleWorkerPath    string   `toml:"bundle_worker_path"`
    BundleRetention     int      `toml:"bundle_retention"`
    MaxBundleSize       int64    `toml:"max_bundle_size"`
    SoftDeleteRetention Duration `toml:"soft_delete_retention"`
}
```

**No default.** Unlike other `Duration` fields, `SoftDeleteRetention`
has no entry in `applyDefaults()`. Zero (the absent/unset value) means
"disabled — delete means delete." Operators opt in to soft-delete by
setting a positive retention:

```toml
[storage]
soft_delete_retention = "720h"   # 30 days
```

Env var: `BLOCKYARD_STORAGE_SOFT_DELETE_RETENTION`.

### Step 6: Modify DeleteApp handler

Replace the delete logic in `internal/api/apps.go`:

```go
func DeleteApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        caller := auth.CallerFromContext(r.Context())
        id := chi.URLParam(r, "id")

        app, relation, ok := resolveAppRelation(srv, w, caller, id)
        if !ok {
            return
        }

        if !relation.CanDelete() {
            notFound(w, "app not found")
            return
        }

        slog.Info("deleting app",
            "app_id", app.ID, "name", app.Name, "caller", caller.Sub)

        // Always stop running workers.
        stopAppSync(srv, app.ID)

        if srv.Config.Storage.SoftDeleteRetention.Duration > 0 {
            // Soft-delete: mark as deleted, retain files and rows.
            if err := srv.DB.SoftDeleteApp(app.ID); err != nil {
                serverError(w, "soft delete: "+err.Error())
                return
            }
        } else {
            // Immediate hard delete (legacy behavior).
            purgeApp(srv, app)
        }

        if srv.AuditLog != nil {
            srv.AuditLog.Emit(auditEntry(r, audit.ActionAppDelete, app.ID,
                map[string]any{"name": app.Name}))
        }

        w.WriteHeader(http.StatusNoContent)
    }
}
```

**Extract `purgeApp` as a shared function** (used by both the handler's
immediate-delete path and the background sweeper). Place in
`internal/api/apps.go` since it's called from the handler, and import
it from ops:

Actually, the sweeper runs in `internal/ops/`. The handler is in
`internal/api/`. Both need the purge logic. Extract it into `ops`:

```go
// internal/ops/purge.go

package ops

import (
    "context"
    "log/slog"
    "os"
    "path/filepath"

    "github.com/cynkra/blockyard/internal/bundle"
    "github.com/cynkra/blockyard/internal/db"
    "github.com/cynkra/blockyard/internal/server"
)

// PurgeApp permanently removes an app's bundles, files, and database
// rows. The app must already have no running workers. Used by both
// the DeleteApp handler (immediate delete) and the sweeper.
func PurgeApp(srv *server.Server, app *db.AppRow) {
    bundles, err := srv.DB.ListBundlesByApp(app.ID)
    if err != nil {
        slog.Warn("purge: list bundles failed",
            "app_id", app.ID, "error", err)
    }

    for _, b := range bundles {
        paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, b.ID)
        bundle.DeleteFiles(paths)
    }

    if err := srv.DB.ClearActiveBundle(app.ID); err != nil {
        slog.Warn("purge: clear active bundle failed",
            "app_id", app.ID, "error", err)
    }

    for _, b := range bundles {
        if _, err := srv.DB.DeleteBundle(b.ID); err != nil {
            slog.Warn("purge: delete bundle row failed",
                "bundle_id", b.ID, "app_id", app.ID, "error", err)
        }
    }

    if err := srv.DB.HardDeleteApp(app.ID); err != nil {
        slog.Warn("purge: delete app row failed",
            "app_id", app.ID, "error", err)
    }

    appDir := filepath.Join(srv.Config.Storage.BundleServerPath, app.ID)
    if err := os.RemoveAll(appDir); err != nil {
        slog.Warn("purge: remove app directory failed",
            "app_id", app.ID, "path", appDir, "error", err)
    }

    slog.Info("purged app", "app_id", app.ID, "name", app.Name)
}
```

The handler calls `ops.PurgeApp(srv, app)` for immediate hard delete.

### Step 7: Restore endpoint

New handler in `internal/api/apps.go`:

```go
func RestoreApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        caller := auth.CallerFromContext(r.Context())
        id := chi.URLParam(r, "id")

        if caller == nil {
            forbidden(w, "insufficient permissions")
            return
        }

        // Look up the app including deleted — GetApp filters them out.
        app, err := srv.DB.GetAppIncludeDeleted(id)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if app == nil || app.DeletedAt == nil {
            notFound(w, "deleted app not found")
            return
        }

        // Only admins and the original owner can restore.
        if !caller.Role.CanViewAllApps() && app.Owner != caller.Sub {
            notFound(w, "deleted app not found")
            return
        }

        if err := srv.DB.RestoreApp(app.ID); err != nil {
            if db.IsUniqueConstraintError(err) {
                conflict(w, "another app already uses the name "+app.Name)
                return
            }
            serverError(w, "restore app: "+err.Error())
            return
        }

        app, err = srv.DB.GetApp(app.ID)
        if err != nil || app == nil {
            serverError(w, "get app after restore")
            return
        }

        slog.Info("app restored",
            "app_id", app.ID, "name", app.Name, "caller", caller.Sub)

        if srv.AuditLog != nil {
            srv.AuditLog.Emit(auditEntry(r, audit.ActionAppRestore, app.ID,
                map[string]any{"name": app.Name}))
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
    }
}
```

**Audit action** — add to `internal/audit/audit.go`:

```go
ActionAppRestore Action = "app.restore"
```

### Step 8: Admin visibility of deleted apps

Admins need to discover which apps are soft-deleted so they know what
can be restored. Add a query parameter to the existing `ListApps`
handler:

```go
func ListApps(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        caller := auth.CallerFromContext(r.Context())

        var apps []db.AppRow
        var err error

        if caller == nil {
            forbidden(w, "insufficient permissions")
            return
        }

        // ?deleted=true — admin-only, returns soft-deleted apps
        if r.URL.Query().Get("deleted") == "true" {
            if !caller.Role.CanViewAllApps() {
                forbidden(w, "admin only")
                return
            }
            apps, err = srv.DB.ListDeletedApps()
        } else if caller.Role.CanViewAllApps() {
            apps, err = srv.DB.ListApps()
        } else {
            apps, err = srv.DB.ListAccessibleApps(caller.Sub)
        }
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }

        responses := make([]AppResponse, len(apps))
        for i, app := range apps {
            responses[i] = appResponse(&app, srv.Workers)
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(responses)
    }
}
```

### Step 9: Background sweeper

Add to `internal/ops/ops.go`:

```go
// SpawnSoftDeleteSweeper periodically purges soft-deleted apps whose
// retention period has expired. Blocks until ctx is cancelled.
// Does not start if soft_delete_retention is zero (soft-delete
// disabled — nothing to sweep).
func SpawnSoftDeleteSweeper(ctx context.Context, srv *server.Server) {
    retention := srv.Config.Storage.SoftDeleteRetention.Duration
    if retention == 0 {
        <-ctx.Done()
        return
    }

    // Sweep every hour or every retention period, whichever is shorter.
    interval := 1 * time.Hour
    if retention < interval {
        interval = retention
    }

    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            sweepDeletedApps(ctx, srv)
        }
    }
}

func sweepDeletedApps(ctx context.Context, srv *server.Server) {
    retention := srv.Config.Storage.SoftDeleteRetention.Duration
    cutoff := time.Now().Add(-retention).UTC().Format(time.RFC3339)

    apps, err := srv.DB.ListExpiredDeletedApps(cutoff)
    if err != nil {
        slog.Warn("soft-delete sweeper: list failed", "error", err)
        return
    }

    if len(apps) == 0 {
        return
    }

    slog.Info("soft-delete sweeper: purging expired apps", "count", len(apps))
    for _, app := range apps {
        // Safety: stop any workers that might still be running
        // (shouldn't happen for deleted apps, but defensive).
        stopAppSync(srv, app.ID)
        PurgeApp(srv, &app)
    }
}
```

Note: `stopAppSync` is in `internal/api/apps.go`. It needs to be
accessible from `ops`. Two options:

1. Move `stopAppSync` to `internal/ops/` (alongside `EvictWorker`).
2. Have the sweeper call `EvictWorker` directly for each worker.

Option 1 is cleaner — `stopAppSync` is fundamentally an ops function
(drain + evict), not an API concern. Move it to `internal/ops/ops.go`
and rename to `StopAppSync` (exported). Update the `DeleteApp` and
`StopApp` handlers to call `ops.StopAppSync`.

### Step 10: Route registration

In `internal/api/router.go`, inside the `limitBody` group:

```go
r.Post("/apps/{id}/rollback", RollbackApp(srv))
r.Post("/apps/{id}/restore", RestoreApp(srv))
```

### Step 11: main.go wiring

In `cmd/blockyard/main.go`, spawn the sweeper alongside the existing
background goroutines:

```go
go ops.SpawnSoftDeleteSweeper(bgCtx, srv)
```

### Step 12: Tests

**DB tests** (in `internal/db/db_test.go`, using `eachDB`):

- `SoftDeleteApp` sets `deleted_at`, app disappears from `GetApp`
- `SoftDeleteApp` on already-deleted app is a no-op
- `RestoreApp` clears `deleted_at`, app reappears in `GetApp`
- `RestoreApp` on non-deleted app is a no-op
- `GetAppIncludeDeleted` returns soft-deleted apps
- `ListApps` excludes soft-deleted apps
- `ListDeletedApps` returns only soft-deleted apps
- `ListExpiredDeletedApps` returns only apps deleted before cutoff
- `ListCatalog` excludes soft-deleted apps
- `ListAccessibleApps` excludes soft-deleted apps
- `HardDeleteApp` permanently removes the row
- Soft-deleted app name is reusable (creating a new app with the
  same name succeeds)
- Restore with name collision → unique constraint error

**API tests** (in `internal/api/api_test.go`):

- `DELETE /api/v1/apps/{id}` with soft-delete enabled → 204,
  app disappears from listings, app still in DB with `deleted_at` set
- `DELETE /api/v1/apps/{id}` with soft-delete disabled
  (retention unset/zero) → 204, app fully removed
- `POST /api/v1/apps/{id}/restore` → 200, app reappears in listings
- Restore when name is taken by a new app → 409
- Restore non-existent app → 404
- Restore non-deleted app → 404
- Restore without permission → 404
- `GET /api/v1/apps?deleted=true` as admin → lists soft-deleted apps
- `GET /api/v1/apps?deleted=true` as non-admin → 403
- Soft-deleted app not routable via proxy

---

## Resource Limit Validation and Verification

Resource limits (`memory_limit`, `cpu_limit`) are already enforced at
container creation — the Docker backend's `createWorkerContainer`
parses `memory_limit` via `parseMemoryLimit` and sets
`Resources.Memory` and `Resources.NanoCPUs`. Both the API `StartApp`
path and the proxy cold-start path pass these values from the app
config.

What's missing: no input validation at the API boundary, and no runtime
verification that the limits actually took effect.

### Step 1: API-level validation in UpdateApp

Add validation to `internal/api/apps.go` in the `UpdateApp` handler,
after the existing `MaxSessionsPerWorker` / `MaxWorkersPerApp` checks:

```go
if body.MemoryLimit != nil && *body.MemoryLimit != "" {
    if _, ok := docker.ParseMemoryLimit(*body.MemoryLimit); !ok {
        badRequest(w, "invalid memory_limit format: use e.g. \"256m\", \"1g\", \"512mb\"")
        return
    }
}
if body.CPULimit != nil {
    if *body.CPULimit < 0 {
        badRequest(w, "cpu_limit must be non-negative")
        return
    }
    if *srv.Config.Proxy.MaxCPULimit > 0 && *body.CPULimit > *srv.Config.Proxy.MaxCPULimit {
        badRequest(w, fmt.Sprintf("cpu_limit must not exceed %.1f", *srv.Config.Proxy.MaxCPULimit))
        return
    }
}
```

This requires exporting `parseMemoryLimit` from the Docker backend
package. Rename to `ParseMemoryLimit` and keep it in
`internal/backend/docker/docker.go`:

```go
// ParseMemoryLimit converts human-readable memory strings like "512m",
// "1g", "256mb" to bytes. Returns (bytes, true) on success.
func ParseMemoryLimit(s string) (int64, bool) {
    // ... existing implementation, unchanged ...
}
```

Update internal callers (`createWorkerContainer`) to use the new name.

**Why validate in the API layer, not the DB layer?** The DB stores
user-supplied strings. Validation belongs at the system boundary (the
API handler) where meaningful error messages can be returned. The DB is
a storage layer, not a validation layer.

**CPU ceiling is configurable.** The `[proxy] max_cpu_limit` setting
(default 16) is a server-level guardrail against typos like
`cpu_limit: 1000` (meant 1.0). Operators on larger hosts can raise it.
Setting `max_cpu_limit = 0` disables the ceiling (only the `>= 0`
check applies).

Add `MaxCPULimit` to `ProxyConfig` in `internal/config/config.go`:

```go
MaxCPULimit *float64 `toml:"max_cpu_limit"`
```

Default in `applyDefaults()`:

```go
if cfg.Proxy.MaxCPULimit == nil {
    v := 16.0
    cfg.Proxy.MaxCPULimit = &v
}
```

A pointer distinguishes "absent" (`nil` → apply default 16) from
"explicitly set to 0" (`*0.0` → no ceiling). Same pattern as
`AppRow.MaxWorkersPerApp`.

Env var: `BLOCKYARD_PROXY_MAX_CPU_LIMIT`.

```toml
[proxy]
max_cpu_limit = 16   # max per-app cpu_limit; 0 = no ceiling
```

### Step 2: Runtime verification in Docker backend

After starting a worker container, inspect it and compare actual
resource limits against the requested values. Log a warning on
mismatch. This catches silent failures — e.g., Docker ignoring a limit
due to cgroup configuration issues, or a parsing bug in
`ParseMemoryLimit`.

Add to `internal/backend/docker/docker.go`, called from `Spawn` after
the container is started (step 7, before recording internal state):

```go
// verifyResourceLimits inspects a running container and warns if
// actual resource limits don't match what was requested.
func (d *DockerBackend) verifyResourceLimits(
    ctx context.Context,
    containerID string,
    spec backend.WorkerSpec,
) {
    info, err := d.client.ContainerInspect(ctx, containerID)
    if err != nil {
        slog.Warn("spawn: failed to verify resource limits",
            "worker_id", spec.WorkerID, "error", err)
        return
    }

    if spec.MemoryLimit != "" {
        expected, ok := ParseMemoryLimit(spec.MemoryLimit)
        if ok && info.HostConfig.Resources.Memory != expected {
            slog.Warn("spawn: memory limit mismatch",
                "worker_id", spec.WorkerID,
                "requested", spec.MemoryLimit,
                "expected_bytes", expected,
                "actual_bytes", info.HostConfig.Resources.Memory)
        }
    }

    if spec.CPULimit > 0 {
        expected := int64(spec.CPULimit * 1e9)
        if info.HostConfig.Resources.NanoCPUs != expected {
            slog.Warn("spawn: CPU limit mismatch",
                "worker_id", spec.WorkerID,
                "requested_cpus", spec.CPULimit,
                "expected_nanocpus", expected,
                "actual_nanocpus", info.HostConfig.Resources.NanoCPUs)
        }
    }
}
```

**Call site** in `Spawn`, after step 7 (container started):

```go
// 7. Start the container
if err := d.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
    // ... existing cleanup ...
}

// 8. Verify resource limits
d.verifyResourceLimits(ctx, containerID, spec)

// Record internal state
d.mu.Lock()
// ...
```

The inspect call is cheap (single Docker API call, no container-side
cost) and happens once per spawn. It does not block or affect the
spawn outcome — it only logs.

### Step 3: Document current behavior

Resource limit changes via `PATCH /api/v1/apps/{id}` take effect on
the next worker spawn. Running workers are not affected — their
limits were set at container creation time and are immutable for the
container's lifetime. This is consistent with how all other app config
fields work (image, worker scaling, etc.).

Add a note to the `PATCH /api/v1/apps/{id}` response documentation
(when the web UI or CLI docs are written):

> Resource limit changes apply to newly spawned workers only. Running
> workers retain their original limits until they are stopped and
> replaced.

### Step 4: Tests

**Unit tests:**

- `ParseMemoryLimit` validation: valid formats accepted, invalid
  strings rejected (already has `TestParseMemoryLimit` and
  `TestParseMemoryLimitEdgeCases`)
- `UpdateApp` with invalid `memory_limit` → 400
- `UpdateApp` with negative `cpu_limit` → 400
- `UpdateApp` with `cpu_limit` exceeding `max_cpu_limit` → 400
- `UpdateApp` with `cpu_limit` within ceiling → 200
- `UpdateApp` with `max_cpu_limit = 0` (disabled) → any positive value accepted
- `UpdateApp` with valid limits → 200

**Integration tests:**

- Spawn with memory limit → inspect container → verify
  `Resources.Memory` matches
- Spawn with CPU limit → inspect container → verify
  `Resources.NanoCPUs` matches
- Spawn without limits → no warning logs

---

## Design decisions

1. **Synchronous rollback.** The rollback endpoint blocks until the
   drain completes (at most `shutdown_timeout / 2`). The alternative —
   returning 202 with a task ID — adds polling complexity for the caller
   without meaningful benefit. Rollback is a deployment operation; the
   caller needs to know it's complete before proceeding (e.g., to verify
   the rolled-back app starts correctly). If the app has no running
   workers, the response is instant.

2. **Rollback reuses `stopAppSync`.** The drain-and-evict machinery
   already exists for `DeleteApp` and graceful shutdown. Rollback needs
   the exact same behavior: mark workers as draining, wait for sessions
   to end, force-evict. No new drain logic needed.

3. **Soft-deleted app names are reusable.** The column-level UNIQUE
   constraint on `apps.name` is replaced by a partial unique index
   (`WHERE deleted_at IS NULL`). This allows a new app to reuse the
   name of a soft-deleted app immediately — operators don't need to
   wait for the sweeper or manually hard-delete. The app ID (UUID) is
   the stable identifier for soft-deleted rows. If a name-colliding
   soft-deleted app is later restored, the restore fails with a
   unique constraint error and returns 409 — the operator must rename
   or delete the new app first. This is a rare edge case and a clear
   error, preferable to blocking name reuse for the entire retention
   period.

4. **Soft-delete stops workers.** When an app is soft-deleted, its
   workers are stopped immediately. Keeping workers running for a
   deleted app would waste resources and confuse operators who expect
   deleted apps to be inactive. Bundle files and database rows are
   retained for the retention period; compute resources are freed
   immediately.

5. **No default for soft-delete retention.** Unlike other `Duration`
   fields, `SoftDeleteRetention` has no entry in `applyDefaults()`.
   Zero (absent/unset) means disabled — delete is permanent, matching
   current behavior. Operators opt in to soft-delete by setting a
   positive value. This avoids both a behavior change on upgrade and
   the need for a `-1s`-style sentinel hack.

6. **`PurgeApp` in `ops/` package.** The purge logic (delete files,
   delete rows, remove directory) is shared between the API handler's
   immediate-delete path and the background sweeper. Placing it in `ops`
   (alongside `EvictWorker`, `GracefulShutdown`, etc.) avoids circular
   imports between `api` and `ops`.

7. **`stopAppSync` moves to `ops/`.** The function is used by three
   callers: `DeleteApp` handler, `RollbackApp` handler, and the sweeper.
   It belongs with the worker lifecycle operations in `ops`, not in the
   API layer.

8. **Resource limit validation at the API boundary.** The DB stores
   user-supplied strings. `parseMemoryLimit` silently returns `(0, false)`
   for invalid inputs, which means Docker creates a container with no
   memory limit — not what the operator intended. Validating at the API
   boundary (the `UpdateApp` handler) gives immediate, actionable
   feedback.

9. **Runtime verification logs, does not fail.** The post-spawn inspect
   check logs a warning if limits don't match but does not prevent the
   worker from running. A mismatch is a configuration or infrastructure
   issue (e.g., cgroup v1 vs v2 differences, Docker version quirks),
   not a reason to kill a healthy worker. The warning gives operators
   visibility without disrupting the service.

10. **Dynamic resource limit updates deferred to v3.** Docker supports
    `ContainerUpdate()` to change limits on running containers without
    restart. This is feasible for the Docker backend but not generalizable
    — Kubernetes pod resource changes may trigger restarts depending on
    the field. v3 adds this alongside the process backend, where the
    concept of "update limits on a running process" has different
    mechanics (cgroup writes vs container API). See the v3 draft note.

## New source files

| File | Purpose |
|------|---------|
| `internal/ops/purge.go` | `PurgeApp` — shared purge logic for hard delete |
| `internal/db/migrations/sqlite/002_v2_soft_delete.up.sql` | Add `deleted_at` column |
| `internal/db/migrations/sqlite/002_v2_soft_delete.down.sql` | Remove `deleted_at` column |
| `internal/db/migrations/postgres/002_v2_soft_delete.up.sql` | Add `deleted_at` column |
| `internal/db/migrations/postgres/002_v2_soft_delete.down.sql` | Remove `deleted_at` column |

## Modified files

| File | Change |
|------|--------|
| `internal/db/db.go` | `AppRow.DeletedAt` field; `deleted_at IS NULL` filters on `GetApp`, `GetAppByName`, `ListApps`, `ListAccessibleApps`, `ListCatalog`; new methods `SoftDeleteApp`, `RestoreApp`, `HardDeleteApp`, `GetAppIncludeDeleted`, `ListDeletedApps`, `ListExpiredDeletedApps`; rename `DeleteApp` → `HardDeleteApp` |
| `internal/api/apps.go` | `RollbackApp` handler, `RestoreApp` handler; `DeleteApp` handler soft-delete logic; `AppResponse.DeletedAt` field; `ListApps` handler `?deleted=true` support; resource limit validation in `UpdateApp`; `purgeApp` → `ops.PurgeApp` |
| `internal/api/router.go` | Add `/apps/{id}/rollback` and `/apps/{id}/restore` routes |
| `internal/config/config.go` | `StorageConfig.SoftDeleteRetention` field; `ProxyConfig.MaxCPULimit` field (default 16); defaults in `applyDefaults` |
| `internal/ops/ops.go` | `SpawnSoftDeleteSweeper`, `sweepDeletedApps`; `StopAppSync` (moved from api) |
| `internal/audit/audit.go` | `ActionAppRollback`, `ActionAppRestore` constants |
| `internal/backend/docker/docker.go` | Export `ParseMemoryLimit`; `verifyResourceLimits` method; call from `Spawn` |
| `cmd/blockyard/main.go` | Spawn `SpawnSoftDeleteSweeper` goroutine |

## Exit criteria

**Bundle rollback:**

- `POST /api/v1/apps/{id}/rollback` with valid ready bundle → 200,
  `active_bundle` updated
- Workers stopped before bundle switch
- Invalid bundle (wrong app, wrong status, non-existent) → 4xx
- Already-active bundle → 400
- Audit event emitted

**Soft-delete:**

- `DELETE /api/v1/apps/{id}` marks app as deleted, stops workers,
  retains files
- Soft-deleted apps absent from `GET /api/v1/apps`, catalog, proxy
- `POST /api/v1/apps/{id}/restore` brings app back
- Sweeper purges apps after retention period
- `soft_delete_retention` absent/zero → immediate hard delete
  (default, current behavior)
- `GET /api/v1/apps?deleted=true` (admin) → lists soft-deleted apps
- Soft-deleted app name is reusable (new app can take the name)
- Restore with name collision → 409
- Migration 002 applies cleanly on both SQLite and PostgreSQL

**Resource limits:**

- `PATCH /api/v1/apps/{id}` with `memory_limit: "banana"` → 400
- `PATCH /api/v1/apps/{id}` with `cpu_limit: -1` → 400
- `PATCH /api/v1/apps/{id}` with `cpu_limit` exceeding `max_cpu_limit` → 400
- `max_cpu_limit = 0` disables ceiling check
- Valid limits accepted and stored
- Spawn logs warning if actual container limits don't match requested
- No warning when limits match or are unset

**General:**

- All new unit and integration tests pass on both SQLite and PostgreSQL
- All existing tests still pass
- `go vet ./...` clean
- `go test ./...` green
