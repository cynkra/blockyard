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
2. **App-level mount specification** — `data_mounts` JSON column on
   the `apps` table. Each entry references a named source from config
   and specifies a container target path.
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
path = "/data/shared-models"

[[storage.data_mounts]]
name = "scratch"
path = "/data/scratch"
```

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

### Step 3: Migration — add app columns

Migration `002_app_config` adds three columns to the `apps` table.
All additive with defaults — backward-compatible per phase 3-1 rules.

**`internal/db/migrations/sqlite/002_app_config.up.sql`:**

```sql
ALTER TABLE apps ADD COLUMN image TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN runtime TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN data_mounts TEXT NOT NULL DEFAULT '[]';
```

**`internal/db/migrations/sqlite/002_app_config.down.sql`:**

```sql
-- SQLite does not support DROP COLUMN before 3.35.0 (2021-03-12).
-- Recreate the table without the new columns.
CREATE TABLE apps_backup AS SELECT
    id, name, owner, access_type, active_bundle,
    max_workers_per_app, max_sessions_per_worker,
    memory_limit, cpu_limit, title, description,
    created_at, updated_at, deleted_at,
    pre_warmed_seats, refresh_schedule, last_refresh_at, enabled
FROM apps;
DROP TABLE apps;
ALTER TABLE apps_backup RENAME TO apps;
```

**`internal/db/migrations/postgres/002_app_config.up.sql`:**

```sql
ALTER TABLE apps ADD COLUMN image TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN runtime TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN data_mounts TEXT NOT NULL DEFAULT '[]';
```

**`internal/db/migrations/postgres/002_app_config.down.sql`:**

```sql
ALTER TABLE apps DROP COLUMN image;
ALTER TABLE apps DROP COLUMN runtime;
ALTER TABLE apps DROP COLUMN data_mounts;
```

Empty string = use server default, matching the existing
`memory_limit` / `cpu_limit` pattern. `data_mounts` is a JSON array
stored as TEXT, validated at the application layer.

### Step 4: DB layer — AppRow and AppUpdate

Add fields to `AppRow` in `internal/db/db.go` (after existing
`Enabled` field, line 233):

```go
type AppRow struct {
    // ...existing fields...
    Image      string `db:"image" json:"image"`
    Runtime    string `db:"runtime" json:"runtime"`
    DataMounts string `db:"data_mounts" json:"data_mounts"`
}
```

Add to `AppUpdate` (line 583):

```go
type AppUpdate struct {
    // ...existing fields...
    Image      *string
    Runtime    *string
    DataMounts *string // validated JSON string
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
if u.DataMounts != nil {
    app.DataMounts = *u.DataMounts
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
        pre_warmed_seats = ?,
        refresh_schedule = ?,
        image = ?,
        runtime = ?,
        data_mounts = ?,
        updated_at = ?
    WHERE id = ?`),
    app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
    app.MemoryLimit, app.CPULimit,
    app.AccessType,
    app.Title, app.Description,
    app.PreWarmedSeats,
    app.RefreshSchedule,
    app.Image, app.Runtime, app.DataMounts,
    now, id,
)
```

### Step 5: Mount validation package

New package `internal/mount/` with type definitions and validation.
This is separate from the API handler so the process backend
(phase 3-7) can reuse it.

**`internal/mount/mount.go`:**

```go
package mount

import (
    "encoding/json"
    "fmt"
    "path/filepath"
    "strings"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/config"
)

// DataMount describes a single data mount for an app's workers.
type DataMount struct {
    Source   string `json:"source"`   // "models" or "models/v2"
    Target   string `json:"target"`   // "/data/models"
    ReadOnly *bool  `json:"readonly"` // default true
}

// IsReadOnly returns the effective read-only flag (default true).
func (m DataMount) IsReadOnly() bool {
    if m.ReadOnly == nil {
        return true
    }
    return *m.ReadOnly
}

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

// Parse parses a JSON data_mounts string into a slice of DataMount.
func Parse(raw string) ([]DataMount, error) {
    if raw == "" || raw == "[]" {
        return nil, nil
    }
    var mounts []DataMount
    if err := json.Unmarshal([]byte(raw), &mounts); err != nil {
        return nil, fmt.Errorf("invalid data_mounts JSON: %w", err)
    }
    return mounts, nil
}

// Validate checks a list of DataMounts against the admin-defined
// mount sources. Returns the first validation error found.
func Validate(mounts []DataMount, sources []config.DataMountSource) error {
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

// Resolve converts validated DataMounts into host paths using the
// admin-defined mount sources. Returns backend.MountEntry slices
// ready for WorkerSpec.DataMounts.
func Resolve(mounts []DataMount, sources []config.DataMountSource) []backend.MountEntry {
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

        hostPath := sourceMap[baseName]
        if subpath != "" {
            hostPath = filepath.Join(hostPath, subpath)
        }

        entries = append(entries, backend.MountEntry{
            Source:   hostPath,
            Target:   m.Target,
            ReadOnly: m.IsReadOnly(),
        })
    }
    return entries
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

// Append per-app data mounts.
for _, dm := range spec.DataMounts {
    b, m := d.mountCfg.TranslateMount(dm)
    binds = append(binds, b...)
    mounts = append(mounts, m...)
}
```

This reuses `MountConfig.TranslateMount()` (already used by
`Build()` at line 786) which handles the bind/volume/native mode
translation.

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
if app.DataMounts != "" && app.DataMounts != "[]" {
    parsed, err := mount.Parse(app.DataMounts)
    if err != nil {
        slog.Error("invalid data_mounts for app", "app", app.Name, "error", err)
    } else {
        spec.DataMounts = mount.Resolve(parsed, srv.Config.Storage.DataMounts)
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
    Image      *string `json:"image"`
    Runtime    *string `json:"runtime"`
    DataMounts *string `json:"data_mounts"`
}
```

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
if body.Runtime != nil && !caller.Role.CanManageRoles() {
    forbidden(w, "runtime requires admin")
    return
}

if body.DataMounts != nil && *body.DataMounts != "" && *body.DataMounts != "[]" {
    parsed, err := mount.Parse(*body.DataMounts)
    if err != nil {
        badRequest(w, fmt.Sprintf("invalid data_mounts: %v", err))
        return
    }
    if err := mount.Validate(parsed, srv.Config.Storage.DataMounts); err != nil {
        badRequest(w, fmt.Sprintf("data_mounts: %v", err))
        return
    }
}
```

Convert `updateAppRequest` to `db.AppUpdate`:

```go
u := db.AppUpdate{
    // ...existing field mappings...
    Image:      body.Image,
    Runtime:    body.Runtime,
    DataMounts: body.DataMounts,
}
```

**Dynamic resource limit updates** — after the DB update succeeds,
when memory or CPU limits changed, update running workers:

```go
updated, err := srv.DB.UpdateApp(app.ID, u)
if err != nil {
    serverError(w, err)
    return
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
    body["data_mounts"] = v
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
                <td>{{if .IsReadOnly}}ro{{else}}rw{{end}}</td>
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

The template data needs a `DataMounts` field. In the UI handler that
renders the settings tab, parse `app.DataMounts` JSON into
`[]mount.DataMount` and pass it as `.DataMounts` in the template data.

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

func TestParse_EmptyString(t *testing.T)
// "" → nil, nil.

func TestParse_EmptyArray(t *testing.T)
// "[]" → nil, nil.

func TestParse_InvalidJSON(t *testing.T)
// "not json" → error.

func TestResolve_SubpathJoin(t *testing.T)
// Source "models/v2" with admin path "/data/shared" → "/data/shared/v2".

func TestDataMount_ReadOnlyDefault(t *testing.T)
// nil ReadOnly → IsReadOnly() returns true.
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
func TestUpdateApp_ImageRuntimeDataMounts(t *testing.T)
// Create app, update image/runtime/data_mounts, verify read-back.
// Verify empty string is the default for image and runtime.
// Verify "[]" is the default for data_mounts.
```

#### API validation tests

Extend `internal/api/` tests:

```go
func TestUpdateApp_InvalidImage(t *testing.T)
// Image with whitespace → 400.

func TestUpdateApp_InvalidDataMountsJSON(t *testing.T)
// Malformed JSON → 400.

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
| `internal/db/db.go` | **update** | `Image`/`Runtime`/`DataMounts` on `AppRow` and `AppUpdate`, UPDATE SQL |
| `internal/backend/backend.go` | **update** | `DataMounts`/`Runtime` on `WorkerSpec`, `UpdateResources` on `Backend`, `ErrNotSupported` |
| `internal/backend/docker/docker.go` | **update** | Data mount translation, `Runtime` on HostConfig, `UpdateResources` impl |
| `internal/backend/mock/mock.go` | **update** | `UpdateResources` impl |
| `internal/proxy/coldstart.go` | **update** | Per-app image/runtime fallback, data mount resolution |
| `internal/api/apps.go` | **update** | New fields on request struct, validation, runtime admin check, dynamic resource updates, form parsing |
| `internal/api/bundles.go` | **update** | Per-app image for build |
| `internal/ui/upload.go` | **update** | Per-app image for UI upload build |
| `internal/server/refresh.go` | **update** | Per-app image for dependency refresh |
| `internal/server/packages.go` | **update** | Per-app image for runtime package install |
| `internal/server/transfer.go` | **update** | Per-app image/runtime for transfer re-spawn |
| `internal/ui/templates/tab_settings.html` | **update** | Image/runtime input fields, data mounts table |
| `cmd/by/scale.go` | **update** | `--image` on `updateCmd`, `--runtime`/`--data-mounts` on `scaleCmd` |

## New files

| File | Purpose |
|------|---------|
| `internal/mount/mount.go` | `DataMount` type, `Parse`, `Validate`, `Resolve`, `ReservedPaths` |
| `internal/mount/mount_test.go` | Mount validation unit tests |
| `internal/db/migrations/sqlite/002_app_config.up.sql` | Migration up (SQLite) |
| `internal/db/migrations/sqlite/002_app_config.down.sql` | Migration down (SQLite) |
| `internal/db/migrations/postgres/002_app_config.up.sql` | Migration up (PostgreSQL) |
| `internal/db/migrations/postgres/002_app_config.down.sql` | Migration down (PostgreSQL) |

## Design decisions

1. **Mount sources are config-only, not DB-stored.** Admin-defined
   sources (`[[storage.data_mounts]]`) live in TOML, not the database.
   The paths reference host filesystem locations — they're
   deployment-specific, not application-specific. Storing them in
   config matches this: the admin defines what's available, the app
   references by name.

2. **Validation in a separate package.** Mount validation lives in
   `internal/mount/`, not inline in the API handler. The process
   backend (phase 3-7) will need the same validation when translating
   mounts to bwrap `--bind` / `--ro-bind` arguments. A shared package
   avoids duplicating the security-sensitive path traversal checks.

3. **ReadOnly defaults to true.** Data mounts are read-only unless
   explicitly opted out. This matches the principle of least privilege
   — most data mounts are shared model directories or reference data
   that workers should not modify.

4. **Image override on `by update`, runtime on `by scale`.** The plan
   specifies `by update --image <ref>` for the image override. Runtime
   and data mounts go on `by scale` alongside other infrastructure
   config (memory, CPU, workers). The split: `by update` handles
   app identity/behavior (title, description, image), `by scale`
   handles resource allocation (memory, CPU, runtime, mounts).

5. **Data mounts read-only in UI.** The settings tab's inline
   save-per-field pattern does not extend to JSON arrays. Displaying
   the mount table with a "use CLI" pointer is simpler and less
   error-prone than building a JSON array editor in htmx.

6. **Best-effort dynamic resource updates.** When memory/CPU limits
   change, running workers are updated live via `ContainerUpdate()`.
   Failures are logged, not returned — the DB update is the source of
   truth, and new workers will pick up the limits. This avoids
   blocking the API response on container updates that may fail for
   transient reasons.

7. **`ErrNotSupported` for backend capabilities.** `UpdateResources`
   returns `ErrNotSupported` from backends that don't support live
   updates (mock backend returns it for simplicity; future process
   backend will return it because there are no per-worker cgroups).
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
