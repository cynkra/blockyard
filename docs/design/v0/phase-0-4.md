# Phase 0-4: REST API + Auth

The control plane HTTP API. All endpoints under `/api/v1/`, protected by
static bearer token. Phase 0-3 delivered the bundle upload pipeline and a
minimal router to support it. This phase expands that router to the full
v0 API surface: app CRUD, app lifecycle (start/stop), and log streaming.

## Deliverables

1. Remove `status` column from `apps` table — runtime state is derived
   from `WorkerMap`, not stored in the DB
2. App CRUD endpoints — create, list, get, update, delete
3. App name validation — URL-safe slugs only
4. App lifecycle endpoints — start and stop
5. App log streaming endpoint — 501 stub (deferred to phase 0-6)
6. DB additions — `UpdateApp`, `ClearActiveBundle`
7. `resolveApp` helper — resolve `{id}` by UUID first, then by name
8. Shared error response helpers — extract from `bundles.go`, add
   `conflict` and `serviceUnavailable`
9. Router expansion — wire all endpoints into `NewRouter`
10. Integration tests for all new endpoints

## What's already done

Phase 0-3 delivered:

- Bearer token auth middleware (`api/auth.go`)
- `/healthz` endpoint (unauthenticated)
- `POST /api/v1/apps/{id}/bundles` — upload bundle
- `GET /api/v1/apps/{id}/bundles` — list bundles
- `GET /api/v1/tasks/{taskID}` — task status
- `GET /api/v1/tasks/{taskID}/logs` — stream task logs
- Error response helpers (`writeError` with code + message)
- DB queries: `CreateApp`, `GetApp`, `GetAppByName`, `ListApps`,
  `DeleteApp`, bundle CRUD, `SetActiveBundle`, `UpdateBundleStatus`
- `server.Server` with `Workers *WorkerMap`
- `NewServer()` constructor
- `main.go` with HTTP server and graceful shutdown

## Step-by-step

### Step 1: Remove `status` column from `apps` table

The `apps` table in the current schema does not have a `status` column
(it was never added in the Go rewrite), so no schema migration is needed.
Confirm this is the case and move on.

**Deriving status at read time:**

The `GET /apps/{id}` handler computes status from `WorkerMap` before
returning the response. A lightweight wrapper serializes the app row
plus the derived status:

```go
type AppResponse struct {
    ID                   string   `json:"id"`
    Name                 string   `json:"name"`
    ActiveBundle         *string  `json:"active_bundle"`
    MaxWorkersPerApp     *int     `json:"max_workers_per_app"`
    MaxSessionsPerWorker int      `json:"max_sessions_per_worker"`
    MemoryLimit          *string  `json:"memory_limit"`
    CPULimit             *float64 `json:"cpu_limit"`
    CreatedAt            string   `json:"created_at"`
    UpdatedAt            string   `json:"updated_at"`
    Status               string   `json:"status"`
}

func appResponse(app *db.AppRow, workers *server.WorkerMap) AppResponse {
    status := "stopped"
    if workers.CountForApp(app.ID) > 0 {
        status = "running"
    }
    return AppResponse{
        ID:                   app.ID,
        Name:                 app.Name,
        ActiveBundle:         app.ActiveBundle,
        MaxWorkersPerApp:     app.MaxWorkersPerApp,
        MaxSessionsPerWorker: app.MaxSessionsPerWorker,
        MemoryLimit:          app.MemoryLimit,
        CPULimit:             app.CPULimit,
        CreatedAt:            app.CreatedAt,
        UpdatedAt:            app.UpdatedAt,
        Status:               status,
    }
}
```

`appResponse` wraps the DB row and adds the computed `status` field.
`listApps` uses the same pattern — iterate the DB rows and annotate
each with its derived status.

### Step 2: `resolveApp` — lookup by UUID or name

All `{id}` parameters across app endpoints (GET, PATCH, DELETE, start,
stop, logs, bundles) resolve by UUID first, then by name. This is safe
from collisions because names must start with a lowercase letter while
UUIDs start with a hex digit.

```go
func resolveApp(database *db.DB, id string) (*db.AppRow, error) {
    app, err := database.GetApp(id)
    if err != nil {
        return nil, err
    }
    if app != nil {
        return app, nil
    }
    return database.GetAppByName(id)
}
```

All handlers that accept `{id}` call `resolveApp` instead of
`db.GetApp` directly.

### Step 3: App name validation

App names are used in proxy URLs (`/app/{name}/`), so they must be
URL-safe slugs. Validation rules:

- Lowercase ASCII letters, digits, and hyphens only
- Must start with a letter
- Must not end with a hyphen
- Length: 1–63 characters
- Regex: `^[a-z][a-z0-9-]*[a-z0-9]$` (or `^[a-z]$` for single char)

```go
func validateAppName(name string) error {
    if len(name) == 0 || len(name) > 63 {
        return fmt.Errorf("name must be 1-63 characters")
    }
    for _, c := range name {
        if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
            return fmt.Errorf("name must contain only lowercase letters, digits, and hyphens")
        }
    }
    if name[0] < 'a' || name[0] > 'z' {
        return fmt.Errorf("name must start with a lowercase letter")
    }
    if name[len(name)-1] == '-' {
        return fmt.Errorf("name must not end with a hyphen")
    }
    return nil
}
```

**Tests:**

- `TestValidateAppName` — table-driven: valid names (`a`, `my-app`,
  `app-123`), invalid names (`""`, `"A"`, `"-app"`, `"app-"`,
  `"app_name"`, `"1app"`, 64-char string)

### Step 4: Extract shared error response helpers

The `writeError` function already exists in `api/error.go` from
phase 0-3. Add convenience wrappers for common status codes:

```go
func badRequest(w http.ResponseWriter, msg string) {
    writeError(w, http.StatusBadRequest, "bad_request", msg)
}

func notFound(w http.ResponseWriter, msg string) {
    writeError(w, http.StatusNotFound, "not_found", msg)
}

func conflict(w http.ResponseWriter, msg string) {
    writeError(w, http.StatusConflict, "conflict", msg)
}

func serviceUnavailable(w http.ResponseWriter, msg string) {
    writeError(w, http.StatusServiceUnavailable, "service_unavailable", msg)
}

func serverError(w http.ResponseWriter, msg string) {
    writeError(w, http.StatusInternalServerError, "internal_error", msg)
}
```

Update existing handlers in `bundles.go` and `tasks.go` to use these
wrappers instead of calling `writeError` directly.

### Step 5: App CRUD endpoints

`internal/api/apps.go` — all app management endpoints.

**Create app:**

```go
type createAppRequest struct {
    Name string `json:"name"`
}

func CreateApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var body createAppRequest
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
            badRequest(w, "invalid JSON body")
            return
        }

        if err := validateAppName(body.Name); err != nil {
            badRequest(w, err.Error())
            return
        }

        // Check for duplicate name
        existing, err := srv.DB.GetAppByName(body.Name)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if existing != nil {
            conflict(w, fmt.Sprintf("app name %q already exists", body.Name))
            return
        }

        app, err := srv.DB.CreateApp(body.Name)
        if err != nil {
            serverError(w, "create app: "+err.Error())
            return
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusCreated)
        json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
    }
}
```

We check for duplicate names explicitly before the INSERT rather than
relying on the UNIQUE constraint error. This produces a clear 409
response instead of a generic 500. The DB constraint is still there
as a safety net.

**List apps:**

```go
func ListApps(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        apps, err := srv.DB.ListApps()
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

**Get app:**

```go
func GetApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := chi.URLParam(r, "id")

        app, err := resolveApp(srv.DB, id)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if app == nil {
            notFound(w, "app "+id+" not found")
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
    }
}
```

**Update app:**

```go
type updateAppRequest struct {
    MaxWorkersPerApp     *int     `json:"max_workers_per_app"`
    MaxSessionsPerWorker *int     `json:"max_sessions_per_worker"`
    MemoryLimit          *string  `json:"memory_limit"`
    CPULimit             *float64 `json:"cpu_limit"`
}

func UpdateApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := chi.URLParam(r, "id")

        var body updateAppRequest
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
            badRequest(w, "invalid JSON body")
            return
        }

        // v0: max_sessions_per_worker is locked to 1
        if body.MaxSessionsPerWorker != nil && *body.MaxSessionsPerWorker != 1 {
            badRequest(w, "max_sessions_per_worker must be 1 in this version")
            return
        }

        app, err := resolveApp(srv.DB, id)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if app == nil {
            notFound(w, "app "+id+" not found")
            return
        }

        update := db.AppUpdate{
            MaxWorkersPerApp:     body.MaxWorkersPerApp,
            MaxSessionsPerWorker: body.MaxSessionsPerWorker,
            MemoryLimit:          body.MemoryLimit,
            CPULimit:             body.CPULimit,
        }
        app, err = srv.DB.UpdateApp(app.ID, update)
        if err != nil {
            serverError(w, "update app: "+err.Error())
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(appResponse(app, srv.Workers))
    }
}
```

The updatable fields are resource limits and worker scaling — things an
operator adjusts without redeploying. Name and active_bundle are not
mutable via PATCH. Name is immutable because it appears in proxy URLs.
Active bundle is set by the restore pipeline, not by manual update.

**Delete app:**

```go
func DeleteApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := chi.URLParam(r, "id")

        app, err := resolveApp(srv.DB, id)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if app == nil {
            notFound(w, "app "+id+" not found")
            return
        }

        // 1. Stop all workers for this app
        stopAppWorkers(srv, app.ID)

        // 2. Delete bundle files from disk
        bundles, err := srv.DB.ListBundlesByApp(app.ID)
        if err != nil {
            serverError(w, "list bundles: "+err.Error())
            return
        }
        for _, b := range bundles {
            paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, b.ID)
            bundle.DeleteFiles(paths)
        }

        // 3. Clear active_bundle FK before deleting bundles
        if err := srv.DB.ClearActiveBundle(app.ID); err != nil {
            serverError(w, "clear active bundle: "+err.Error())
            return
        }

        // 4. Delete bundle rows
        for _, b := range bundles {
            srv.DB.DeleteBundle(b.ID)
        }

        // 5. Delete app row
        if _, err := srv.DB.DeleteApp(app.ID); err != nil {
            serverError(w, "delete app: "+err.Error())
            return
        }

        // 6. Remove app directory from disk (best-effort)
        appDir := filepath.Join(srv.Config.Storage.BundleServerPath, app.ID)
        os.RemoveAll(appDir)

        w.WriteHeader(http.StatusNoContent)
    }
}
```

Delete is the most complex endpoint because of the multi-step teardown.
The ordering matters: stop workers first (so nothing is using the bundle
files), then delete files from disk, then clear the `active_bundle` FK
(so the app row no longer references any bundle), then delete bundle
rows, then delete the app row. The FK constraint on `bundles.app_id`
enforces that bundles are deleted before the app.

### Step 6: DB additions

New queries in `internal/db/db.go`.

**UpdateApp:**

```go
func (db *DB) UpdateApp(id string, update interface{}) (*AppRow, error) {
    // Fetch-modify-write: read current row, overlay non-nil fields,
    // write back. With 4 optional fields this is simpler than building
    // a dynamic SET clause.
    app, err := db.GetApp(id)
    if err != nil {
        return nil, err
    }
    if app == nil {
        return nil, fmt.Errorf("app %q not found", id)
    }

    // Type-assert the update payload. The api package passes
    // updateAppRequest which has optional fields.
    type updater interface {
        Apply(app *AppRow)
    }
    if u, ok := update.(updater); ok {
        u.Apply(app)
    }

    now := time.Now().UTC().Format(time.RFC3339)
    _, err = db.Exec(
        `UPDATE apps SET
            max_workers_per_app = ?,
            max_sessions_per_worker = ?,
            memory_limit = ?,
            cpu_limit = ?,
            updated_at = ?
        WHERE id = ?`,
        app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
        app.MemoryLimit, app.CPULimit,
        now, id,
    )
    if err != nil {
        return nil, fmt.Errorf("update app: %w", err)
    }

    return db.GetApp(id)
}
```

Actually, to avoid a circular import between `api` and `db`, keep the
update logic simple. Accept individual fields directly:

```go
type AppUpdate struct {
    MaxWorkersPerApp     *int
    MaxSessionsPerWorker *int
    MemoryLimit          *string
    CPULimit             *float64
}

func (db *DB) UpdateApp(id string, u AppUpdate) (*AppRow, error) {
    app, err := db.GetApp(id)
    if err != nil {
        return nil, err
    }
    if app == nil {
        return nil, fmt.Errorf("app not found")
    }

    if u.MaxWorkersPerApp != nil {
        app.MaxWorkersPerApp = u.MaxWorkersPerApp
    }
    if u.MaxSessionsPerWorker != nil {
        app.MaxSessionsPerWorker = *u.MaxSessionsPerWorker
    }
    if u.MemoryLimit != nil {
        app.MemoryLimit = u.MemoryLimit
    }
    if u.CPULimit != nil {
        app.CPULimit = u.CPULimit
    }

    now := time.Now().UTC().Format(time.RFC3339)
    _, err = db.Exec(
        `UPDATE apps SET
            max_workers_per_app = ?,
            max_sessions_per_worker = ?,
            memory_limit = ?,
            cpu_limit = ?,
            updated_at = ?
        WHERE id = ?`,
        app.MaxWorkersPerApp, app.MaxSessionsPerWorker,
        app.MemoryLimit, app.CPULimit,
        now, id,
    )
    if err != nil {
        return nil, fmt.Errorf("update app: %w", err)
    }

    return db.GetApp(id)
}
```

The fetch-modify-write pattern is fine here because updates are rare
admin operations, not high-frequency paths. `AppUpdate` lives in the
`db` package to avoid circular imports. The API handler maps its
request struct to `db.AppUpdate`.

**ClearActiveBundle:**

```go
func (db *DB) ClearActiveBundle(appID string) error {
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := db.Exec(
        `UPDATE apps SET active_bundle = NULL, updated_at = ? WHERE id = ?`,
        now, appID,
    )
    return err
}
```

### Step 7: App lifecycle — start

`POST /api/v1/apps/{id}/start` — start an app by spawning a worker.

```go
type startResponse struct {
    WorkerID string `json:"worker_id"`
    Status   string `json:"status"`
}

func StartApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := chi.URLParam(r, "id")

        app, err := resolveApp(srv.DB, id)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if app == nil {
            notFound(w, "app "+id+" not found")
            return
        }

        // Already running — return existing worker
        workerIDs := srv.Workers.ForApp(app.ID)
        if len(workerIDs) > 0 {
            w.Header().Set("Content-Type", "application/json")
            json.NewEncoder(w).Encode(startResponse{
                WorkerID: workerIDs[0],
                Status:   "running",
            })
            return
        }

        // Must have an active bundle
        if app.ActiveBundle == nil {
            conflict(w, "app has no active bundle — upload and build a bundle first")
            return
        }

        // Check global worker limit
        if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
            serviceUnavailable(w, "max workers reached")
            return
        }

        // Build WorkerSpec
        workerID := uuid.New().String()
        paths := bundle.NewBundlePaths(
            srv.Config.Storage.BundleServerPath, app.ID, *app.ActiveBundle,
        )

        labels := map[string]string{
            "dev.blockyard/managed":   "true",
            "dev.blockyard/app-id":    app.ID,
            "dev.blockyard/worker-id": workerID,
            "dev.blockyard/role":      "worker",
        }

        spec := backend.WorkerSpec{
            AppID:       app.ID,
            WorkerID:    workerID,
            Image:       srv.Config.Docker.Image,
            Cmd: []string{"R", "-e",
                fmt.Sprintf("shiny::runApp('%s', port = as.integer(Sys.getenv('SHINY_PORT')))",
                    srv.Config.Storage.BundleWorkerPath)},
            BundlePath:  paths.Unpacked,
            LibraryPath: paths.Library,
            WorkerMount: srv.Config.Storage.BundleWorkerPath,
            ShinyPort:   srv.Config.Docker.ShinyPort,
            MemoryLimit: stringOrEmpty(app.MemoryLimit),
            CPULimit:    floatOrZero(app.CPULimit),
            Labels:      labels,
        }

        // Spawn worker
        if err := srv.Backend.Spawn(r.Context(), spec); err != nil {
            serverError(w, "spawn worker: "+err.Error())
            return
        }

        // Register in worker map
        srv.Workers.Set(workerID, server.ActiveWorker{AppID: app.ID})

        // Register address
        addr, err := srv.Backend.Addr(r.Context(), workerID)
        if err != nil {
            slog.Warn("failed to get worker address", "worker_id", workerID, "error", err)
        } else {
            srv.Registry.Set(workerID, addr)
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(startResponse{
            WorkerID: workerID,
            Status:   "running",
        })
    }
}

func stringOrEmpty(s *string) string {
    if s == nil {
        return ""
    }
    return *s
}

func floatOrZero(f *float64) float64 {
    if f == nil {
        return 0
    }
    return *f
}
```

The start endpoint spawns a single worker. In v0, the proxy (phase 0-5)
will also spawn workers on-demand. The start endpoint is for explicit
pre-warming — e.g. start the app before the first user hits it.

The "already running" check uses `WorkerMap`, not a stored DB status.
If any worker exists for this app, the app is running.

Start does not health-check. The proxy layer (phase 0-5) handles
cold-start holding — when a request arrives for a starting worker, the
proxy polls `HealthCheck` until the worker is ready or the timeout
expires. The start endpoint's job is just to pre-warm the container.

### Step 8: App lifecycle — stop

`POST /api/v1/apps/{id}/stop` — stop all workers for an app.

```go
func StopApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := chi.URLParam(r, "id")

        app, err := resolveApp(srv.DB, id)
        if err != nil {
            serverError(w, "db error: "+err.Error())
            return
        }
        if app == nil {
            notFound(w, "app "+id+" not found")
            return
        }

        stopped := stopAppWorkers(srv, app.ID)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "status":          "stopped",
            "workers_stopped": stopped,
        })
    }
}
```

**Shared helper — stop all workers for an app:**

```go
// stopAppWorkers stops all workers belonging to the given app. Returns
// the count of workers stopped. Errors from individual worker stops
// are logged but do not fail the operation — best-effort cleanup.
func stopAppWorkers(srv *server.Server, appID string) int {
    workerIDs := srv.Workers.ForApp(appID)
    stopped := 0
    for _, wid := range workerIDs {
        srv.Workers.Delete(wid)
        srv.Registry.Delete(wid)
        srv.Sessions.DeleteByWorker(wid)
        if err := srv.Backend.Stop(context.Background(), wid); err != nil {
            slog.Warn("failed to stop worker",
                "worker_id", wid, "app_id", appID, "error", err)
        }
        stopped++
    }
    return stopped
}
```

`stopAppWorkers` is used by both `StopApp` and `DeleteApp`. It removes
workers from the `WorkerMap` first, then stops them via the backend.
This ordering means that concurrent requests won't try to route to a
worker that is being torn down.

### Step 9: App log streaming — deferred to phase 0-6

`GET /api/v1/apps/{id}/logs` is deferred to phase 0-6. The endpoint
needs to read from `LogStore` (which is fed by per-worker log capture
goroutines), not from `backend.Logs()` directly. Implementing it here
would mean building against the raw backend log stream, only to rewrite
it in phase 0-6 when `LogStore` is wired up. The route is registered in
the router but returns 501 until phase 0-6.

### Step 10: Update bundle endpoints to use `resolveApp`

Update `UploadBundle` and `ListBundles` in `bundles.go` to call
`resolveApp` instead of `srv.DB.GetApp` so that all `{id}` parameters
consistently resolve by UUID-then-name.

### Step 11: Router expansion

Update `NewRouter` in `internal/api/router.go` to wire all endpoints:

```go
func NewRouter(srv *server.Server) http.Handler {
    r := chi.NewRouter()

    // Unauthenticated
    r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
        w.Write([]byte("ok"))
    })

    // Authenticated API
    r.Route("/api/v1", func(r chi.Router) {
        r.Use(BearerAuth(srv))

        r.Post("/apps", CreateApp(srv))
        r.Get("/apps", ListApps(srv))
        r.Get("/apps/{id}", GetApp(srv))
        r.Patch("/apps/{id}", UpdateApp(srv))
        r.Delete("/apps/{id}", DeleteApp(srv))

        r.Post("/apps/{id}/bundles", UploadBundle(srv))
        r.Get("/apps/{id}/bundles", ListBundles(srv))

        r.Post("/apps/{id}/start", StartApp(srv))
        r.Post("/apps/{id}/stop", StopApp(srv))
        r.Get("/apps/{id}/logs", func(w http.ResponseWriter, _ *http.Request) {
            writeError(w, http.StatusNotImplemented, "not_implemented",
                "app log streaming is implemented in phase 0-6")
        })

        r.Get("/tasks/{taskID}", GetTaskStatus(srv))
        r.Get("/tasks/{taskID}/logs", TaskLogs(srv))
    })

    return r
}
```

### Step 12: Integration tests

`internal/api/api_test.go` — extend with tests for all new endpoints.
Use the existing `testServer` helper with `MockBackend`.

**App CRUD tests:**

```go
func TestCreateApp(t *testing.T) {
    _, ts := testServer(t)
    body := `{"name":"my-app"}`
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(body))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    if resp.StatusCode != http.StatusCreated {
        t.Errorf("expected 201, got %d", resp.StatusCode)
    }

    var result map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&result)
    if result["name"] != "my-app" {
        t.Errorf("expected name=my-app, got %v", result["name"])
    }
    if result["status"] != "stopped" {
        t.Errorf("expected status=stopped, got %v", result["status"])
    }
}

func TestCreateAppRejectsInvalidName(t *testing.T) {
    _, ts := testServer(t)
    for _, name := range []string{"My-App", "-app", "app-", "app_name", "1app"} {
        body := fmt.Sprintf(`{"name":"%s"}`, name)
        req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
            strings.NewReader(body))
        req.Header.Set("Authorization", "Bearer test-token")
        req.Header.Set("Content-Type", "application/json")

        resp, _ := http.DefaultClient.Do(req)
        if resp.StatusCode != http.StatusBadRequest {
            t.Errorf("name %q: expected 400, got %d", name, resp.StatusCode)
        }
    }
}

func TestCreateDuplicateNameReturns409(t *testing.T) {
    _, ts := testServer(t)
    body := `{"name":"my-app"}`
    for range 2 {
        req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
            strings.NewReader(body))
        req.Header.Set("Authorization", "Bearer test-token")
        req.Header.Set("Content-Type", "application/json")
        resp, _ := http.DefaultClient.Do(req)
        // First call: 201, second: 409
        _ = resp
    }
    // Second call
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(body))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ := http.DefaultClient.Do(req)
    if resp.StatusCode != http.StatusConflict {
        t.Errorf("expected 409, got %d", resp.StatusCode)
    }
}

func TestListApps(t *testing.T) {
    _, ts := testServer(t)
    for _, name := range []string{"app-a", "app-b"} {
        req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
            strings.NewReader(fmt.Sprintf(`{"name":"%s"}`, name)))
        req.Header.Set("Authorization", "Bearer test-token")
        req.Header.Set("Content-Type", "application/json")
        http.DefaultClient.Do(req)
    }

    req, _ := http.NewRequest("GET", ts.URL+"/api/v1/apps", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ := http.DefaultClient.Do(req)

    if resp.StatusCode != 200 {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }
    var apps []map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&apps)
    if len(apps) != 2 {
        t.Errorf("expected 2 apps, got %d", len(apps))
    }
}

func TestGetAppByID(t *testing.T) {
    _, ts := testServer(t)
    // Create
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(`{"name":"my-app"}`))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ := http.DefaultClient.Do(req)
    var created map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&created)
    id := created["id"].(string)

    // Get by UUID
    req, _ = http.NewRequest("GET", ts.URL+"/api/v1/apps/"+id, nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 200 {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }

    // Get by name
    req, _ = http.NewRequest("GET", ts.URL+"/api/v1/apps/my-app", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 200 {
        t.Errorf("expected 200 for name lookup, got %d", resp.StatusCode)
    }
}

func TestGetNonexistentAppReturns404(t *testing.T) {
    _, ts := testServer(t)
    req, _ := http.NewRequest("GET", ts.URL+"/api/v1/apps/nonexistent", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ := http.DefaultClient.Do(req)
    if resp.StatusCode != 404 {
        t.Errorf("expected 404, got %d", resp.StatusCode)
    }
}

func TestUpdateApp(t *testing.T) {
    _, ts := testServer(t)
    // Create
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(`{"name":"my-app"}`))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ := http.DefaultClient.Do(req)
    var created map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&created)
    id := created["id"].(string)

    // Update
    req, _ = http.NewRequest("PATCH", ts.URL+"/api/v1/apps/"+id,
        strings.NewReader(`{"memory_limit":"512m"}`))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 200 {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }
    var updated map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&updated)
    if updated["memory_limit"] != "512m" {
        t.Errorf("expected memory_limit=512m, got %v", updated["memory_limit"])
    }
}

func TestDeleteApp(t *testing.T) {
    _, ts := testServer(t)
    // Create
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(`{"name":"my-app"}`))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ := http.DefaultClient.Do(req)
    var created map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&created)
    id := created["id"].(string)

    // Delete
    req, _ = http.NewRequest("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 204 {
        t.Errorf("expected 204, got %d", resp.StatusCode)
    }

    // Confirm gone
    req, _ = http.NewRequest("GET", ts.URL+"/api/v1/apps/"+id, nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 404 {
        t.Errorf("expected 404, got %d", resp.StatusCode)
    }
}
```

**App lifecycle tests:**

```go
func TestStartAppWithoutBundleReturnsConflict(t *testing.T) {
    _, ts := testServer(t)
    // Create app (no bundle)
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(`{"name":"my-app"}`))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ := http.DefaultClient.Do(req)
    var created map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&created)
    id := created["id"].(string)

    // Start
    req, _ = http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != http.StatusConflict {
        t.Errorf("expected 409, got %d", resp.StatusCode)
    }
}

func TestStartAndStopApp(t *testing.T) {
    srv, ts := testServer(t)

    // Create app
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(`{"name":"my-app"}`))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ := http.DefaultClient.Do(req)
    var created map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&created)
    id := created["id"].(string)

    // Upload bundle and wait for restore
    req, _ = http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/bundles",
        bytes.NewReader(testutil.MakeBundle(t)))
    req.Header.Set("Authorization", "Bearer test-token")
    http.DefaultClient.Do(req)
    time.Sleep(200 * time.Millisecond)

    // Start
    req, _ = http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 200 {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }
    var startBody map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&startBody)
    if startBody["status"] != "running" {
        t.Errorf("expected status=running, got %v", startBody["status"])
    }

    // Verify worker count
    if srv.Workers.Count() != 1 {
        t.Errorf("expected 1 worker, got %d", srv.Workers.Count())
    }

    // Start again — should be no-op, return same worker
    req, _ = http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 200 {
        t.Errorf("expected 200 on second start, got %d", resp.StatusCode)
    }
    if srv.Workers.Count() != 1 {
        t.Errorf("expected still 1 worker, got %d", srv.Workers.Count())
    }

    // Stop
    req, _ = http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/stop", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 200 {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }
    var stopBody map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&stopBody)
    if stopBody["workers_stopped"] != float64(1) {
        t.Errorf("expected workers_stopped=1, got %v", stopBody["workers_stopped"])
    }

    if srv.Workers.Count() != 0 {
        t.Errorf("expected 0 workers, got %d", srv.Workers.Count())
    }
}

func TestDeleteAppStopsWorkers(t *testing.T) {
    srv, ts := testServer(t)

    // Create, upload, start
    req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
        strings.NewReader(`{"name":"my-app"}`))
    req.Header.Set("Authorization", "Bearer test-token")
    req.Header.Set("Content-Type", "application/json")
    resp, _ := http.DefaultClient.Do(req)
    var created map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&created)
    id := created["id"].(string)

    req, _ = http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/bundles",
        bytes.NewReader(testutil.MakeBundle(t)))
    req.Header.Set("Authorization", "Bearer test-token")
    http.DefaultClient.Do(req)
    time.Sleep(200 * time.Millisecond)

    req, _ = http.NewRequest("POST", ts.URL+"/api/v1/apps/"+id+"/start", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    http.DefaultClient.Do(req)
    if srv.Workers.Count() != 1 {
        t.Fatalf("expected 1 worker, got %d", srv.Workers.Count())
    }

    // Delete
    req, _ = http.NewRequest("DELETE", ts.URL+"/api/v1/apps/"+id, nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ = http.DefaultClient.Do(req)
    if resp.StatusCode != 204 {
        t.Errorf("expected 204, got %d", resp.StatusCode)
    }
    if srv.Workers.Count() != 0 {
        t.Errorf("expected 0 workers after delete, got %d", srv.Workers.Count())
    }
}
```

## Complete endpoint table

| Endpoint | Method | Status | Behavior |
|---|---|---|---|
| `/api/v1/apps` | POST | **new** | Create app. Body: `{ "name": "..." }`. Returns 201 with app object. |
| `/api/v1/apps` | GET | **new** | List all apps. Returns array. |
| `/api/v1/apps/{id}` | GET | **new** | Get app details. Resolves by UUID then name. |
| `/api/v1/apps/{id}` | PATCH | **new** | Update app config. Body: partial fields. |
| `/api/v1/apps/{id}` | DELETE | **new** | Delete app. Stops workers, removes files, deletes DB rows. Returns 204. |
| `/api/v1/apps/{id}/bundles` | POST | 0-3 | Upload bundle. Returns 202. |
| `/api/v1/apps/{id}/bundles` | GET | 0-3 | List bundles. |
| `/api/v1/apps/{id}/start` | POST | **new** | Start app. Spawns worker. No-op if running. |
| `/api/v1/apps/{id}/stop` | POST | **new** | Stop app. Stops all workers. |
| `/api/v1/apps/{id}/logs` | GET | **stub** | Returns 501. Implemented in phase 0-6 (needs LogStore). |
| `/api/v1/tasks/{taskID}` | GET | 0-3 | Task status. |
| `/api/v1/tasks/{taskID}/logs` | GET | 0-3 | Stream task logs. |
| `/healthz` | GET | 0-3 | Returns 200. No auth. |

## New source files

| File | Purpose |
|---|---|
| `internal/api/apps.go` | App CRUD + lifecycle + logs handlers |

## Modified files

| File | Change |
|---|---|
| `internal/db/db.go` | Add `AppUpdate`, `UpdateApp`, `ClearActiveBundle` |
| `internal/db/db_test.go` | Add tests for `UpdateApp`, `ClearActiveBundle` |
| `internal/api/error.go` | Add convenience wrappers (`badRequest`, `notFound`, `conflict`, `serviceUnavailable`, `serverError`) |
| `internal/api/router.go` | Wire all new endpoints |
| `internal/api/bundles.go` | Use `resolveApp` for `{id}` resolution |
| `internal/api/tasks.go` | Use convenience error wrappers |
| `internal/api/api_test.go` | Add integration tests for all new endpoints |

## Implementation notes

- **App status is derived, not stored.** Runtime state ("running" /
  "stopped") is computed from `WorkerMap` at request time. The API
  returns `AppResponse` (which wraps `AppRow` + derived `status`) so
  callers still see a `status` field in the JSON.

- **`stopAppWorkers` is best-effort.** Individual worker stop failures
  are logged but don't block the operation. If a container is already
  gone (e.g. it crashed), the stop call fails but the worker is still
  removed from the `WorkerMap`.

- **No drain on stop.** When `POST /apps/{id}/stop` is called, workers
  are stopped immediately without waiting for in-flight requests to
  complete. Graceful drain is a v1 feature alongside session sharing.

- **Worker lookup by app_id scans the WorkerMap.** `WorkerMap` is keyed
  by `worker_id`, not `app_id`. Finding workers for an app requires
  iterating all entries via `ForApp`. With `max_workers = 100`, this is
  a trivial scan.

- **Start does not health-check.** The start endpoint spawns the worker
  and returns immediately. The proxy layer (phase 0-5) handles cold-start
  holding.

- **App log streaming deferred to phase 0-6.** The endpoint needs
  `LogStore` (fed by per-worker log capture goroutines), not raw
  `backend.Logs()`. Registered as a 501 stub until phase 0-6.

- **`resolveApp` enables name-based URLs.** All `{id}` params resolve
  UUID first, name second. No collision risk because names must start
  with `[a-z]` and UUIDs start with `[0-9a-f]`.

## Deferred to later phases

- **Phase 0-6: Replace `stopAppWorkers` with `evictWorker`.** The
  `stopAppWorkers` helper introduced here does not call
  `LogStore.MarkEnded`. Phase 0-6 introduces `evictWorker` as the
  single codepath for worker teardown (including log cleanup). At that
  point, `StopApp` and `DeleteApp` should call `evictWorker` instead.

- **Phase 0-6: Implement `GET /apps/{id}/logs`.** Replace the 501 stub
  with the real handler that reads from `LogStore.Subscribe()`.

- **Fix `task.Store.Subscribe` dedup.** The current `Subscribe`
  implementation requires the consumer to drain overlap between the
  snapshot and live channel. This is fragile. `Subscribe` should
  internally ensure the live channel only delivers lines written after
  the snapshot was taken. Simplify the `TaskLogs` handler accordingly.

## Exit criteria

Phase 0-4 is done when:

- App status is derived from `WorkerMap`, not stored
- `AppResponse` includes computed `status` field in all app endpoints
- `POST /api/v1/apps` creates an app with a validated name, returns 201
- `GET /api/v1/apps` lists all apps with derived status
- `GET /api/v1/apps/{id}` returns app details, resolves by UUID or name
- `PATCH /api/v1/apps/{id}` updates resource limits, returns updated app
- `DELETE /api/v1/apps/{id}` stops workers, cleans up files, returns 204
- `POST /api/v1/apps/{id}/start` spawns a worker with hardcoded Shiny
  command, returns worker_id
- `POST /api/v1/apps/{id}/stop` stops all workers, returns count
- `GET /api/v1/apps/{id}/logs` returns 501 (implemented in phase 0-6)
- `task.Store.Subscribe` dedup is handled internally (no consumer-side drain)
- Invalid app names are rejected with 400
- Duplicate app names are rejected with 409
- Starting without an active bundle returns 409
- Starting at max_workers limit returns 503
- Error responses follow the `{ "error": "...", "message": "..." }` shape
- All existing phase 0-3 tests still pass
- All new integration tests pass
- `go vet ./...` clean
- `go test ./...` green
