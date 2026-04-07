# Phase 3-6: Data Mounts & Per-App Configuration

Per-app container configuration: data mounts, execution environment
images, OCI runtime selection, and dynamic resource limit updates.
These all follow the same pattern: per-app field in the DB, API and
CLI support, field in `WorkerSpec`, backend reads it at spawn time.

Independent of the operations track (phases 3-2 through 3-5). Can be
developed in parallel after phase 3-1. The only prerequisite is the
migration discipline from phase 3-1, since this phase adds a schema
migration.

---

## Prerequisites from Earlier Phases

- **Phase 3-1** — migration discipline. The schema changes in this
  phase follow the expand-only rules: `ADD COLUMN` with `DEFAULT`.
  No column renames, no drops. The DDL linter, convention check, and
  roundtrip test all apply.

## Deliverables

1. **Admin-defined mount sources** — `[[storage.data_mounts]]` TOML
   config section with `name` + `path` pairs. Validated at startup
   for uniqueness and absolute paths.
2. **App-level mount specification** — `app_data_mounts` table with
   FK to `apps`. Each row references a named source from config and
   specifies a container target path.
3. **Mount validation** — source must exist in admin whitelist, no
   `..` path traversal, target must not collide with reserved paths,
   no duplicate targets.
4. **Mount backend integration** — `WorkerSpec.DataMounts` field.
   Docker backend translates to bind mounts via `MountConfig`.
   Process backend (phase 3-7) will use bwrap `--bind` / `--ro-bind`.
5. **Per-app execution image** — `image` column on apps. Empty string
   means use server-wide `[docker] image` default. API, CLI, and UI
   support.
6. **Per-app OCI runtime selection** — `runtime` column on apps.
   Docker backend sets `HostConfig.Runtime` when non-empty. Server-wide
   default via `[docker] runtime`.
7. **Dynamic resource limit updates** — new `UpdateResources` method
   on the `Backend` interface. Docker backend calls
   `ContainerUpdate()`. When memory or CPU limits change via the API,
   running workers are updated live (best-effort).
8. **Tests** — mount validation unit tests, config validation, DB
   round-trip, API endpoint validation, Docker integration tests for
   mounts, image override, runtime override, and live resource updates.

---

## Step-by-step

### Step 1: Config — admin-defined mount sources

Add `DataMounts` to `StorageConfig` in `internal/config/config.go`:

```go
type StorageConfig struct {
    // ...existing fields...
    DataMounts []DataMountSource `toml:"data_mounts"`
}

type DataMountSource struct {
    Name string `toml:"name"`
    Path string `toml:"path"`
}
```

```toml
[[storage.data_mounts]]
name = "models"
path = "/host/data/shared-models"

[[storage.data_mounts]]
name = "scratch"
path = "/host/data/scratch"
```

Paths are **host paths** — the actual location on the Docker host
filesystem, not paths inside the blockyard server container. The server
never accesses these paths itself; it passes them to the Docker API
as bind-mount sources for worker containers. This is the same
coordinate system as Docker `-v /host/path:/container/path`.

These define the admin-approved mount sources. App-level specifications
reference sources by name. This is a config-only construct — no schema
changes.

Add startup validation in `applyDefaults()` / `Validate()`:

```go
func validateDataMounts(mounts []DataMountSource) error {
    seen := make(map[string]bool)
    for _, m := range mounts {
        if m.Name == "" {
            return fmt.Errorf("data_mounts: name must not be empty")
        }
        if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(m.Name) {
            return fmt.Errorf("data_mounts: name %q contains invalid characters", m.Name)
        }
        if !filepath.IsAbs(m.Path) {
            return fmt.Errorf("data_mounts: path %q must be absolute", m.Path)
        }
        if seen[m.Name] {
            return fmt.Errorf("data_mounts: duplicate name %q", m.Name)
        }
        seen[m.Name] = true
    }
    return nil
}
```

### Step 2: Config — per-app OCI runtime default

Add runtime fields to `DockerConfig`:

```go
type DockerConfig struct {
    // ...existing fields...
    Runtime         string            `toml:"runtime"`          // OCI runtime; empty = Docker daemon default
    RuntimeDefaults map[string]string `toml:"runtime_defaults"` // per-access-type defaults
}
```

```toml
[docker]
runtime = ""                    # global default (empty = Docker daemon default, typically runc)

[docker.runtime_defaults]
public = "kata-runtime"         # default for public apps
# private and other access types inherit from [docker] runtime
```

The per-app `runtime` field (admin-set, stored in DB) takes precedence
over the config defaults. The fallback chain at spawn time:

1. `app.Runtime` — explicit admin override per app
2. `config.Docker.RuntimeDefaults[app.AccessType]` — policy default
3. `config.Docker.Runtime` — server-wide default

This is config-level policy, not per-app state. If the admin changes
a default, all apps without an explicit override pick it up on the
next worker spawn. No migration needed — `RuntimeDefaults` is
config-only.

Add startup validation for `RuntimeDefaults` keys:

```go
func validateRuntimeDefaults(defaults map[string]string) error {
    validAccessTypes := map[string]bool{
        "acl": true, "logged_in": true, "public": true,
    }
    for key := range defaults {
        if !validAccessTypes[key] {
            return fmt.Errorf("runtime_defaults: unknown access type %q"+
                " (valid: acl, logged_in, public)", key)
        }
    }
    return nil
}
```

### Step 3: Migration — add app columns

Migration `002_app_config` adds two columns to the `apps` table and
a new `app_data_mounts` table. All additive with defaults —
backward-compatible per phase 3-1 rules.

**`internal/db/migrations/sqlite/002_app_config.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN image TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN runtime TEXT NOT NULL DEFAULT '';

CREATE TABLE app_data_mounts (
    app_id   TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    source   TEXT NOT NULL,
    target   TEXT NOT NULL,
    readonly INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (app_id, target)
);
```

**`internal/db/migrations/sqlite/002_app_config.down.sql`:**

```sql
DROP TABLE IF EXISTS app_data_mounts;

-- SQLite does not support DROP COLUMN before 3.35.0 (2021-03-12).
-- Recreate the table with the original schema to preserve constraints.
CREATE TABLE apps_new (
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
INSERT INTO apps_new SELECT
    id, name, owner, access_type, active_bundle,
    max_workers_per_app, max_sessions_per_worker,
    memory_limit, cpu_limit, title, description,
    created_at, updated_at, deleted_at,
    pre_warmed_sessions, refresh_schedule, last_refresh_at, enabled
FROM apps;
DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;
```

**`internal/db/migrations/postgres/002_app_config.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN image TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN runtime TEXT NOT NULL DEFAULT '';

CREATE TABLE app_data_mounts (
    app_id   TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    source   TEXT NOT NULL,
    target   TEXT NOT NULL,
    readonly INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (app_id, target)
);
```

**`internal/db/migrations/postgres/002_app_config.down.sql`:**

```sql
DROP TABLE IF EXISTS app_data_mounts;
ALTER TABLE apps DROP COLUMN image;
ALTER TABLE apps DROP COLUMN runtime;
```

Empty string = use server default, matching the existing
`memory_limit` / `cpu_limit` pattern.

### Step 4: DB layer — AppRow, AppUpdate, and data mount queries

Add fields to `AppRow` in `internal/db/db.go` (after existing
`Enabled` field, line 233):

```go
type AppRow struct {
    // ...existing fields...
    Image   string `db:"image" json:"image"`
    Runtime string `db:"runtime" json:"runtime"`
}
```

Add to `AppUpdate` (line 583):

```go
type AppUpdate struct {
    // ...existing fields...
    Image   *string
    Runtime *string
}
```

Update `UpdateApp()` (line 587) to handle the new fields in the
fetch-modify-write pattern:

```go
if u.Image != nil {
    app.Image = *u.Image
}
if u.Runtime != nil {
    app.Runtime = *u.Runtime
}
```

And add the new columns to the UPDATE SQL:

```go
_, err = db.Exec(db.rebind(
    `UPDATE apps SET
        max_workers_per_app = ?,
        max_sessions_per_worker = ?,
        memory_limit = ?,
        cpu_limit = ?,
        access_type = ?,
        title = ?,
        description = ?,
        pre_warmed_sessions = ?,
        refresh_schedule = ?,
        image = ?,
        runtime = ?,
        updated_at = ?
    WHERE id = ?`),
    app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
    app.MemoryLimit, app.CPULimit,
    app.AccessType,
    app.Title, app.Description,
    app.PreWarmedSessions,
    app.RefreshSchedule,
    app.Image, app.Runtime,
    now, id,
)
```

**Data mount row type and queries:**

```go
type DataMountRow struct {
    AppID    string `db:"app_id" json:"app_id"`
    Source   string `db:"source" json:"source"`
    Target   string `db:"target" json:"target"`
    ReadOnly bool   `db:"readonly" json:"readonly"`
}

func (db *DB) ListAppDataMounts(appID string) ([]DataMountRow, error) {
    var mounts []DataMountRow
    err := db.DB.Select(&mounts,
        db.rebind(`SELECT * FROM app_data_mounts WHERE app_id = ?`),
        appID)
    if err != nil {
        return nil, fmt.Errorf("list data mounts: %w", err)
    }
    return mounts, nil
}

func (db *DB) SetAppDataMounts(appID string, mounts []DataMountRow) error {
    tx, err := db.DB.Beginx()
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.Exec(db.rebind(
        `DELETE FROM app_data_mounts WHERE app_id = ?`), appID)
    if err != nil {
        return fmt.Errorf("clear data mounts: %w", err)
    }

    for _, m := range mounts {
        _, err = tx.Exec(db.rebind(
            `INSERT INTO app_data_mounts (app_id, source, target, readonly)
             VALUES (?, ?, ?, ?)`),
            appID, m.Source, m.Target, m.ReadOnly)
        if err != nil {
            return fmt.Errorf("insert data mount: %w", err)
        }
    }

    return tx.Commit()
}
```

### Step 5: Mount validation package

New package `internal/mount/` with validation and resolution.
This is separate from the API handler so the process backend
(phase 3-7) can reuse it. Works with `db.DataMountRow` directly —
no intermediate types or JSON parsing.

**`internal/mount/mount.go`:**

```go
package mount

import (
    "fmt"
    "path/filepath"
    "strings"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/config"
    "github.com/cynkra/blockyard/internal/db"
)

// ReservedPaths are container paths that mount targets cannot collide
// with. Checked by prefix — "/app/foo" collides with "/app".
var ReservedPaths = []string{
    "/app",
    "/tmp",
    "/blockyard-lib",
    "/blockyard-lib-store",
    "/transfer",
    "/var/run/blockyard",
}

// Validate checks a list of data mount rows against the admin-defined
// mount sources. Returns the first validation error found.
func Validate(mounts []db.DataMountRow, sources []config.DataMountSource) error {
    sourceMap := make(map[string]string, len(sources))
    for _, s := range sources {
        sourceMap[s.Name] = s.Path
    }

    targets := make(map[string]bool)

    for i, m := range mounts {
        // Source must reference a known admin-defined mount.
        baseName := m.Source
        subpath := ""
        if idx := strings.IndexByte(m.Source, '/'); idx >= 0 {
            baseName = m.Source[:idx]
            subpath = m.Source[idx+1:]
        }

        if _, ok := sourceMap[baseName]; !ok {
            return fmt.Errorf("mount[%d]: unknown source %q", i, baseName)
        }

        // No path traversal in source subpath.
        if strings.Contains(subpath, "..") {
            return fmt.Errorf("mount[%d]: source subpath must not contain \"..\"", i)
        }

        // Target must be absolute.
        if !filepath.IsAbs(m.Target) {
            return fmt.Errorf("mount[%d]: target %q must be absolute", i, m.Target)
        }

        // No path traversal in target.
        if strings.Contains(m.Target, "..") {
            return fmt.Errorf("mount[%d]: target must not contain \"..\"", i)
        }

        // Target must not collide with reserved paths.
        cleanTarget := filepath.Clean(m.Target)
        for _, reserved := range ReservedPaths {
            if cleanTarget == reserved || strings.HasPrefix(cleanTarget, reserved+"/") {
                return fmt.Errorf("mount[%d]: target %q collides with reserved path %q",
                    i, m.Target, reserved)
            }
        }

        // No duplicate targets.
        if targets[cleanTarget] {
            return fmt.Errorf("mount[%d]: duplicate target %q", i, m.Target)
        }
        targets[cleanTarget] = true
    }

    return nil
}

// Resolve converts validated data mount rows into MountEntries with
// host paths as Source. The returned entries are ready to be passed
// directly to the Docker API as bind-mount sources — no MountConfig
// translation needed. Returns an error if a source references a name
// that no longer exists in the admin config (can happen if the admin
// removes a source after apps were configured to use it).
func Resolve(mounts []db.DataMountRow, sources []config.DataMountSource) ([]backend.MountEntry, error) {
    sourceMap := make(map[string]string, len(sources))
    for _, s := range sources {
        sourceMap[s.Name] = s.Path
    }

    entries := make([]backend.MountEntry, 0, len(mounts))
    for _, m := range mounts {
        baseName := m.Source
        subpath := ""
        if idx := strings.IndexByte(m.Source, '/'); idx >= 0 {
            baseName = m.Source[:idx]
            subpath = m.Source[idx+1:]
        }

        hostPath, ok := sourceMap[baseName]
        if !ok {
            return nil, fmt.Errorf("mount source %q not found in config", baseName)
        }
        if subpath != "" {
            hostPath = filepath.Join(hostPath, subpath)
        }

        entries = append(entries, backend.MountEntry{
            Source:   hostPath,
            Target:   m.Target,
            ReadOnly: m.ReadOnly,
        })
    }
    return entries, nil
}
```

### Step 6: WorkerSpec — new fields

Add `DataMounts` and `Runtime` to `WorkerSpec` in
`internal/backend/backend.go` (after existing `Env` field, line 60):

```go
type WorkerSpec struct {
    // ...existing fields...
    DataMounts []MountEntry // data mounts from app config; resolved host paths
    Runtime    string       // OCI runtime override; empty = default
}
```

Reuse the existing `MountEntry` type (already defined at line 74 for
`BuildSpec.Mounts`).

### Step 7: Backend interface — UpdateResources

Add to the `Backend` interface in `internal/backend/backend.go`:

```go
type Backend interface {
    // ...existing methods...

    // UpdateResources live-updates memory and CPU limits for a running
    // worker. Returns ErrNotSupported if the backend does not support
    // live resource updates.
    UpdateResources(ctx context.Context, id string, mem int64, nanoCPUs int64) error
}

// ErrNotSupported is returned by backend methods that are not
// available for the current backend type.
var ErrNotSupported = errors.New("operation not supported by this backend")
```

### Step 8: Docker backend — data mounts, runtime, UpdateResources

**Data mounts** — in `createWorkerContainer()` (`docker.go`, around
line 416), after the existing `WorkerMounts()` call, append data mount
entries:

```go
binds, mounts := d.mountCfg.WorkerMounts(spec.BundlePath, spec.LibraryPath,
    spec.LibDir, spec.TransferDir, spec.TokenDir, spec.WorkerMount)

// Append per-app data mounts. Source paths are host paths (not
// server-container paths), so they bypass MountConfig translation
// and go directly into Docker bind strings.
for _, dm := range spec.DataMounts {
    flag := ":ro"
    if !dm.ReadOnly {
        flag = ""
    }
    binds = append(binds, dm.Source+":"+dm.Target+flag)
}
```

Data mount sources are host paths by definition (the admin configures
them as the path on the Docker host, not a path inside the blockyard
server container). Unlike bundle/library/transfer mounts — which the
server works with directly and need mode-dependent translation via
`MountConfig` — data mount paths are opaque to the server. It never
reads from them; it just passes them through to the Docker API as
bind-mount sources. No `TranslateMount` needed.

**Runtime** — in the `ContainerCreate` call (line 450), add the
`Runtime` field to `HostConfig`:

```go
HostConfig: &container.HostConfig{
    NetworkMode:    container.NetworkMode(networkName),
    Binds:          binds,
    Mounts:         mounts,
    Tmpfs:          map[string]string{"/tmp": ""},
    CapDrop:        []string{"ALL"},
    SecurityOpt:    []string{"no-new-privileges"},
    ReadonlyRootfs: true,
    Resources:      resources,
    Runtime:        spec.Runtime, // empty = Docker default
},
```

**UpdateResources** — new method on `DockerBackend`:

```go
// UpdateResources live-updates resource limits on a running container.
func (d *DockerBackend) UpdateResources(ctx context.Context, id string, mem int64, nanoCPUs int64) error {
    d.mu.Lock()
    ws, ok := d.workers[id]
    d.mu.Unlock()
    if !ok {
        return fmt.Errorf("worker %s not found", id)
    }

    resources := container.Resources{}
    if mem > 0 {
        resources.Memory = mem
    }
    if nanoCPUs > 0 {
        resources.NanoCPUs = nanoCPUs
    }

    _, err := d.client.ContainerUpdate(ctx, ws.containerID,
        container.UpdateConfig{Resources: resources})
    return err
}
```

`ContainerUpdate` is part of the moby client API (`client.Client`)
and accepts the container ID plus an `UpdateConfig` with resource
fields. Only memory and CPU limits can be updated live on Linux;
other fields are silently ignored by Docker.

### Step 9: Mock backend — UpdateResources

Add `UpdateResources` to the mock backend in
`internal/backend/mock/mock.go`:

```go
func (b *MockBackend) UpdateResources(_ context.Context, id string, mem int64, nanoCPUs int64) error {
    b.mu.Lock()
    defer b.mu.Unlock()
    w, ok := b.workers[id]
    if !ok {
        return fmt.Errorf("worker %s not found", id)
    }
    if mem > 0 {
        w.spec.MemoryLimit = fmt.Sprintf("%dm", mem/1024/1024)
    }
    if nanoCPUs > 0 {
        w.spec.CPULimit = float64(nanoCPUs) / 1e9
    }
    return nil
}
```

### Step 10: Per-app image fallback — all call sites

Eight call sites hardcode `srv.Config.Docker.Image`. All need the
per-app override: use the app's `Image` field if non-empty, else
fall back to the server-wide default.

Add a helper in `internal/server/` (or inline — it's trivial):

```go
func appImage(app *db.AppRow, serverDefault string) string {
    if app.Image != "" {
        return app.Image
    }
    return serverDefault
}

func appRuntime(app *db.AppRow, cfg config.DockerConfig) string {
    if app.Runtime != "" {
        return app.Runtime
    }
    if rt, ok := cfg.RuntimeDefaults[app.AccessType]; ok && rt != "" {
        return rt
    }
    return cfg.Runtime
}
```

**Worker spawn sites** — each constructs a `WorkerSpec` with
`Image: srv.Config.Docker.Image`. Change to
`Image: appImage(app, srv.Config.Docker.Image)` and add
`Runtime: appRuntime(app, srv.Config.Docker)`:

| File | Line | Context |
|------|------|---------|
| `internal/proxy/coldstart.go` | 251 | Cold-start worker spawn |
| `internal/api/apps.go` | 793 | Pre-warm worker spawn |
| `internal/server/transfer.go` | 214 | Transfer (re-spawn) |

**Build sites** — each constructs a `BuildSpec` or `RestoreParams`
with `Image: srv.Config.Docker.Image`. Change to
`Image: appImage(app, srv.Config.Docker.Image)`:

| File | Line | Context |
|------|------|---------|
| `internal/api/bundles.go` | 126 | Bundle upload → restore |
| `internal/ui/upload.go` | 314 | UI bundle upload → restore |
| `internal/server/refresh.go` | 40, 69 | Scheduled dependency refresh |
| `internal/server/packages.go` | 82, 105 | Runtime package install |

The build and worker images match — the only difference is that
builds mount pak into the container. Using the same base image
ensures system libraries and R version are consistent.

**Data mount resolution** — add after each `WorkerSpec` construction
in the three worker spawn sites:

```go
appMounts, err := srv.DB.ListAppDataMounts(app.ID)
if err != nil {
    slog.Error("failed to list data mounts", "app", app.Name, "error", err)
} else if len(appMounts) > 0 {
    resolved, err := mount.Resolve(appMounts, srv.Config.Storage.DataMounts)
    if err != nil {
        slog.Error("failed to resolve data mounts", "app", app.Name, "error", err)
    } else {
        spec.DataMounts = resolved
    }
}
```

Data mounts apply only to workers, not builds. Build containers
have a controlled mount set (pak, bundle, library, store) and should
not gain arbitrary user-configured mounts.

### Step 11: API layer — new fields and validation

Add to `updateAppRequest` in `internal/api/apps.go` (line 299):

```go
type updateAppRequest struct {
    // ...existing fields...
    Image      *string          `json:"image"`
    Runtime    *string          `json:"runtime"`
    DataMounts []db.DataMountRow `json:"data_mounts,omitempty"`
}
```

The `DataMounts` field is a native JSON array — API consumers send:
```json
{"data_mounts": [{"source": "models", "target": "/data/models", "readonly": true}]}
```

To clear all mounts, send an empty array: `{"data_mounts": []}`.
The `app_id` field in each entry is ignored on input — the handler
fills it from the URL path parameter.

Add validation in `UpdateApp()` handler (after existing validation,
around line 380):

```go
if body.Image != nil && *body.Image != "" {
    img := *body.Image
    if strings.ContainsAny(img, " \t\n") {
        badRequest(w, "image must not contain whitespace")
        return
    }
}

// Runtime changes require admin — runtime controls the container
// isolation boundary (e.g., runc vs kata vs sysbox).
if body.Runtime != nil && (caller == nil || !caller.Role.CanManageRoles()) {
    forbidden(w, "runtime requires admin")
    return
}

if body.DataMounts != nil {
    if err := mount.Validate(body.DataMounts, srv.Config.Storage.DataMounts); err != nil {
        badRequest(w, fmt.Sprintf("data_mounts: %v", err))
        return
    }
}
```

Convert `updateAppRequest` to `db.AppUpdate` and persist mounts:

```go
u := db.AppUpdate{
    // ...existing field mappings...
    Image:   body.Image,
    Runtime: body.Runtime,
}
```

After the app update, persist mounts if provided:

```go
updated, err := srv.DB.UpdateApp(app.ID, u)
if err != nil {
    serverError(w, "update app: "+err.Error())
    return
}

if body.DataMounts != nil {
    for i := range body.DataMounts {
        body.DataMounts[i].AppID = app.ID
    }
    if err := srv.DB.SetAppDataMounts(app.ID, body.DataMounts); err != nil {
        serverError(w, "set data mounts: "+err.Error())
        return
    }
}

// Live-update resource limits on running workers (best-effort).
if body.MemoryLimit != nil || body.CPULimit != nil {
    mem := int64(0)
    if updated.MemoryLimit != nil {
        if parsed, ok := docker.ParseMemoryLimit(*updated.MemoryLimit); ok {
            mem = parsed
        }
    }
    cpuNano := int64(0)
    if updated.CPULimit != nil {
        cpuNano = int64(*updated.CPULimit * 1e9)
    }
    for _, wid := range srv.Workers.ForApp(app.ID) {
        if err := srv.Backend.UpdateResources(r.Context(), wid, mem, cpuNano); err != nil {
            slog.Warn("failed to update worker resources",
                "worker", wid, "error", err)
        }
    }
}
```

Add to `parseUpdateAppForm()` for htmx form support:

```go
if v := r.FormValue("image"); r.Form.Has("image") {
    body.Image = &v
}
if v := r.FormValue("runtime"); r.Form.Has("runtime") {
    body.Runtime = &v
}
// data_mounts not supported via form encoding — use JSON API.
```

**API response** — add the new fields to `appResponseV2()` in
`internal/api/runtime.go` (line 606, after `refresh_schedule`):

```go
"image":            app.Image,
"runtime":          app.Runtime,
```

And add `data_mounts` from a DB query (the mount list lives in its
own table, not on `AppRow`):

```go
dataMounts, _ := srv.DB.ListAppDataMounts(app.ID)
// ...
"data_mounts":      dataMounts,
```

Update the swagger response type in `internal/api/swagger_types.go`:

```go
Image      string          `json:"image"`
Runtime    string          `json:"runtime"`
DataMounts []db.DataMountRow `json:"data_mounts"`
```

### Step 12: CLI — `by update --image` and `by scale` extensions

**`by update`** in `cmd/by/scale.go` (the `updateCmd()` function at
line 75) — add `--image` flag:

```go
if cmd.Flags().Changed("image") {
    v, _ := cmd.Flags().GetString("image")
    body["image"] = v
}
```

```go
cmd.Flags().String("image", "", "Docker image for this app (empty = server default)")
```

**`by scale`** in `cmd/by/scale.go` (the `scaleCmd()` function at
line 10) — add `--runtime` and `--data-mounts` flags:

```go
if cmd.Flags().Changed("runtime") {
    v, _ := cmd.Flags().GetString("runtime")
    body["runtime"] = v
}
if cmd.Flags().Changed("data-mounts") {
    v, _ := cmd.Flags().GetString("data-mounts")
    body["data_mounts"] = json.RawMessage(v)
}
```

```go
cmd.Flags().String("runtime", "", `OCI runtime (e.g., "kata-runtime")`)
cmd.Flags().String("data-mounts", "",
    `data mounts JSON (e.g., '[{"source":"models","target":"/data/models"}]')`)
```

Update the error message for no-flags:

```go
exitErrorf(jsonOutput, "no flags specified; use --memory, --cpu, --max-workers, --max-sessions, --pre-warm, --runtime, or --data-mounts")
```

### Step 13: UI — settings tab

Add to `internal/ui/templates/tab_settings.html` under the "Resource
Configuration" heading (after pre-warmed seats, before app controls):

```html
<h3>Container Configuration</h3>
<div class="field-group">
    <label for="image">Execution image</label>
    <p class="field-description">Docker image for this app's workers.
       Leave empty to use the server default.</p>
    <div class="field-row">
        <input type="text" id="image" name="image"
               value="{{.App.Image}}"
               data-original="{{.App.Image}}"
               placeholder="(server default)"
               oninput="toggleSaveBtn(this)">
        <button class="field-save hidden"
                hx-patch="/api/v1/apps/{{.App.ID}}"
                hx-include="[name='image']"
                hx-swap="none"
                hx-on::after-request="if(event.detail.successful){showToast('Saved','success');this.classList.add('hidden');var inp=this.closest('.field-row').querySelector('input,textarea');if(inp)inp.dataset.original=inp.value;}">
            <!-- save icon (same SVG as other fields) -->
        </button>
    </div>
</div>
{{if .IsAdmin}}
<div class="field-group">
    <label for="runtime">OCI runtime</label>
    <p class="field-description">Override the container runtime
       (e.g. <code>kata-runtime</code>). Leave empty for default.
       Admin only.</p>
    <div class="field-row">
        <input type="text" id="runtime" name="runtime"
               value="{{.App.Runtime}}"
               data-original="{{.App.Runtime}}"
               placeholder="(default)"
               oninput="toggleSaveBtn(this)">
        <button class="field-save hidden"
                hx-patch="/api/v1/apps/{{.App.ID}}"
                hx-include="[name='runtime']"
                hx-swap="none"
                hx-on::after-request="if(event.detail.successful){showToast('Saved','success');this.classList.add('hidden');var inp=this.closest('.field-row').querySelector('input,textarea');if(inp)inp.dataset.original=inp.value;}">
            <!-- save icon -->
        </button>
    </div>
</div>
{{end}}

{{if .DataMounts}}
<div class="field-group">
    <label>Data mounts</label>
    <table class="data-table">
        <thead>
            <tr><th>Source</th><th>Target</th><th>Mode</th></tr>
        </thead>
        <tbody>
        {{range .DataMounts}}
            <tr>
                <td><code>{{.Source}}</code></td>
                <td><code>{{.Target}}</code></td>
                <td>{{if .ReadOnly}}ro{{else}}rw{{end}}</td>
            </tr>
        {{end}}
        </tbody>
    </table>
    <p class="field-description">Manage via CLI:
       <code>by scale &lt;app&gt; --data-mounts '[...]'</code></p>
</div>
{{end}}
```

The data mounts table is read-only in the UI — the inline save-per-field
pattern does not work well for JSON arrays. Editing is done through the
CLI or API.

Add `IsAdmin bool` and `DataMounts []db.DataMountRow` to
`settingsTabData` in `internal/ui/sidebar.go` (line 39). In the
`settingsTab` handler (line 291), populate them:

```go
caller := auth.CallerFromContext(r.Context())
isAdmin := caller != nil && caller.Role.CanManageRoles()
dataMounts, _ := srv.DB.ListAppDataMounts(app.ID)

data := settingsTabData{
    // ...existing fields...
    IsAdmin:    isAdmin,
    DataMounts: dataMounts,
}
```

### Step 14: Tests

#### Mount validation tests

**`internal/mount/mount_test.go`:**

```go
func TestValidate_ValidMounts(t *testing.T)
// Valid: single mount with known source, absolute target.

func TestValidate_UnknownSource(t *testing.T)
// Source "unknown" not in admin whitelist → error.

func TestValidate_SourceSubpathTraversal(t *testing.T)
// Source "models/../secret" → error.

func TestValidate_TargetNotAbsolute(t *testing.T)
// Target "relative/path" → error.

func TestValidate_TargetTraversal(t *testing.T)
// Target "/data/../etc/passwd" → error.

func TestValidate_ReservedTarget(t *testing.T)
// Target "/app" → error. Target "/app/data" → error.
// Target "/tmp" → error. Target "/var/run/blockyard" → error.

func TestValidate_DuplicateTargets(t *testing.T)
// Two mounts with target "/data/models" → error.

func TestResolve_SubpathJoin(t *testing.T)
// Source "models/v2" with admin path "/data/shared" → "/data/shared/v2".
```

#### Config validation tests

Extend existing config tests in `internal/config/`:

```go
func TestValidate_DataMountDuplicateNames(t *testing.T)
// Two sources with name "models" → error.

func TestValidate_DataMountRelativePath(t *testing.T)
// Path "relative" → error.

func TestValidate_DataMountEmptyName(t *testing.T)
// Empty name → error.

func TestValidate_DataMountInvalidChars(t *testing.T)
// Name "models/v2" → error (slashes not allowed in names).
```

#### DB round-trip tests

Extend `internal/db/db_test.go`:

```go
func TestUpdateApp_ImageRuntime(t *testing.T)
// Create app, update image/runtime, verify read-back.
// Verify empty string is the default for image and runtime.

func TestSetAppDataMounts(t *testing.T)
// Create app, set mounts, verify ListAppDataMounts returns them.
// Verify SetAppDataMounts replaces previous mounts.
// Verify empty slice clears all mounts.
// Verify UNIQUE(app_id, target) constraint rejects duplicates.

func TestDataMountsCascadeDelete(t *testing.T)
// Create app with mounts, delete app, verify mounts are gone.
```

#### API validation tests

Extend `internal/api/` tests:

```go
func TestUpdateApp_InvalidImage(t *testing.T)
// Image with whitespace → 400.

func TestUpdateApp_InvalidDataMountsJSON(t *testing.T)
// Malformed JSON array → 400.

func TestUpdateApp_UnknownMountSource(t *testing.T)
// Source not in config → 400.

func TestUpdateApp_MountTargetReserved(t *testing.T)
// Target "/app" → 400.

func TestUpdateApp_ImageClearsToEmpty(t *testing.T)
// Setting image="" clears override, falls back to server default.

func TestUpdateApp_RuntimeRequiresAdmin(t *testing.T)
// Non-admin caller sets runtime → 403.

func TestAppRuntime_FallbackChain(t *testing.T)
// 1. app.Runtime set → used regardless of config.
// 2. app.Runtime empty, RuntimeDefaults has entry for access type → used.
// 3. Both empty → falls back to config.Docker.Runtime.

func TestUpdateApp_DynamicResourceUpdate(t *testing.T)
// Change memory_limit, verify Backend.UpdateResources called for
// each running worker.
```

#### Docker integration tests

Tagged `docker_test`, extend existing test files:

```go
func TestSpawn_ImageOverride(t *testing.T)
// Spawn with spec.Image set to non-default image.
// ContainerInspect → verify Image matches.

func TestSpawn_RuntimeOverride(t *testing.T)
// Spawn with spec.Runtime = "runc" (explicit).
// ContainerInspect → verify HostConfig.Runtime.

func TestSpawn_DataMounts(t *testing.T)
// Spawn with spec.DataMounts containing a bind mount.
// ContainerInspect → verify mount appears in Mounts.

func TestUpdateResources(t *testing.T)
// Spawn worker, call UpdateResources with new memory/CPU.
// ContainerInspect → verify Resources.Memory and NanoCPUs changed.

func TestUpdateResources_UnknownWorker(t *testing.T)
// Call UpdateResources with non-existent worker ID → error.
```

#### Migration round-trip

Covered by the existing `TestMigrateRoundtrip` framework from
phase 3-1 — runs up, down, up and verifies schema stability.

---

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/config/config.go` | **update** | `DataMountSource` type, `DataMounts` on `StorageConfig`, `Runtime`/`RuntimeDefaults` on `DockerConfig`, validation |
| `internal/db/db.go` | **update** | `Image`/`Runtime` on `AppRow` and `AppUpdate`, UPDATE SQL, `DataMountRow` type, `ListAppDataMounts`/`SetAppDataMounts` queries |
| `internal/backend/backend.go` | **update** | `DataMounts`/`Runtime` on `WorkerSpec`, `UpdateResources` on `Backend`, `ErrNotSupported` |
| `internal/backend/docker/docker.go` | **update** | Data mount translation, `Runtime` on HostConfig, `UpdateResources` impl |
| `internal/backend/mock/mock.go` | **update** | `UpdateResources` impl |
| `internal/proxy/coldstart.go` | **update** | Per-app image/runtime fallback, data mount resolution |
| `internal/api/apps.go` | **update** | New fields on request struct, validation, runtime admin check, dynamic resource updates, form parsing |
| `internal/api/runtime.go` | **update** | Add `image`/`runtime`/`data_mounts` to `appResponseV2()` response map |
| `internal/api/swagger_types.go` | **update** | Add `image`/`runtime`/`data_mounts` to swagger response type |
| `internal/api/bundles.go` | **update** | Per-app image for build |
| `internal/ui/upload.go` | **update** | Per-app image for UI upload build |
| `internal/server/refresh.go` | **update** | Per-app image for dependency refresh |
| `internal/server/packages.go` | **update** | Per-app image for runtime package install |
| `internal/server/transfer.go` | **update** | Per-app image/runtime for transfer re-spawn |
| `internal/ui/sidebar.go` | **update** | `IsAdmin`/`DataMounts` on `settingsTabData`, populate in handler |
| `internal/ui/templates/tab_settings.html` | **update** | Image/runtime input fields, data mounts table |
| `cmd/by/scale.go` | **update** | `--image` on `updateCmd`, `--runtime`/`--data-mounts` on `scaleCmd` |

## New files

| File | Purpose |
|------|---------|
| `internal/mount/mount.go` | `Validate`, `Resolve`, `ReservedPaths` |
| `internal/mount/mount_test.go` | Mount validation unit tests |
| `internal/db/migrations/sqlite/002_app_config.up.sql` | Migration up (SQLite) |
| `internal/db/migrations/sqlite/002_app_config.down.sql` | Migration down (SQLite) |
| `internal/db/migrations/postgres/002_app_config.up.sql` | Migration up (PostgreSQL) |
| `internal/db/migrations/postgres/002_app_config.down.sql` | Migration down (PostgreSQL) |

## Design decisions

1. **Mount source paths are host paths, not server-container paths.**
   The `path` field in `[[storage.data_mounts]]` is the path on the
   Docker host — the same coordinate system as `docker run -v`. The
   server never accesses these paths itself; it passes them directly
   to the Docker API as bind-mount sources for worker containers.
   This means data mounts bypass `MountConfig.TranslateMount()`
   entirely — unlike bundle/library mounts (which the server works
   with directly and need mode-dependent path translation), data
   mounts are opaque host-to-worker bind mounts. Sources live in
   TOML, not the database, because they're deployment-specific
   (host filesystem layout), not application-specific.

2. **Validation in a separate package.** Mount validation lives in
   `internal/mount/`, not inline in the API handler. The process
   backend (phase 3-7) will need the same validation when translating
   mounts to bwrap `--bind` / `--ro-bind` arguments. A shared package
   avoids duplicating the security-sensitive path traversal checks.

3. **ReadOnly defaults to true.** The `readonly` column defaults to 1.
   Most data mounts are shared model directories or reference data
   that workers should not modify — read-only by default matches the
   principle of least privilege.

4. **Image override on `by update`, runtime on `by scale`.** The plan
   specifies `by update --image <ref>` for the image override. Runtime
   and data mounts go on `by scale` alongside other infrastructure
   config (memory, CPU, workers). The split: `by update` handles
   app identity/behavior (title, description, image), `by scale`
   handles resource allocation (memory, CPU, runtime, mounts).

5. **Data mounts read-only in UI.** The settings tab's inline
   save-per-field pattern does not extend well to multi-row relations.
   Displaying the mount table with a "use CLI" pointer is simpler
   and less error-prone than building a row editor in htmx.

6. **Best-effort dynamic resource updates.** When memory/CPU limits
   change, running workers are updated live via `ContainerUpdate()`.
   Failures are logged, not returned — the DB update is the source of
   truth, and new workers will pick up the limits. This avoids
   blocking the API response on container updates that may fail for
   transient reasons.

7. **`ErrNotSupported` for backend capabilities.** `UpdateResources`
   returns `ErrNotSupported` from backends that don't support live
   updates (future process backend will return it because there are
   no per-worker cgroups). The mock backend implements the update
   (stores new values) so API tests can verify the call is made.
   Callers check for this sentinel and log rather than fail.

8. **Empty string means server default.** Both `image` and `runtime`
   use empty string as "inherit from server config", matching the
   existing `memory_limit` (nil pointer = unset) pattern but simpler
   — `NOT NULL DEFAULT ''` avoids nullable column complexity. Clearing
   the override is done by setting the field to `""`.

9. **Runtime is admin-only, image is not.** App owners can already
   execute arbitrary R code in bundles — letting them choose a
   different base image doesn't meaningfully expand the attack surface.
   Runtime is different: it controls the isolation boundary itself.
   A misconfigured runtime (e.g., sysbox) could weaken container
   sandboxing. Only admins can set `runtime`; the API returns 403 for
   non-admin callers. The UI hides the field for non-admins.

10. **No R version matching.** Per-app images enable different R
    versions, but we don't validate lockfile R version against image
    R version. If they mismatch, pak fails the build with a clear
    error. Adding a preflight check would be redundant.

## Deferred

1. **Image tag template.** A config like
   `image_template = "ghcr.io/rocker-org/r-ver:{{.RVersion}}"` would
   auto-resolve the image from the lockfile's R version. Useful when
   many apps target different R versions and manual image selection
   becomes tedious. Phase 3-6 requires explicit image selection.

2. **Data mount editing in UI.** The settings tab shows mounts as a
   read-only table. A proper editor (add/remove rows, source
   autocomplete from admin whitelist) is a UI feature that can follow
   independently.

3. **Image allowlisting.** Admins may want to restrict which images
   owners can select (e.g., only company-approved registries). Not
   needed for initial deployments where the admin is also the primary
   user, but may matter for multi-tenant setups.
