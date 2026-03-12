# Phase 0-3: Content Management

Bundle upload, dependency restoration, content registry. These form the
deployment pipeline — the path from "user has a tar.gz" to "app is ready to
run."

## Deliverables

1. Bundle storage — atomic writes, tar.gz unpacking, retention cleanup
2. Bundle DB queries — create, list, update status, set active, delete
3. Restore pipeline — background goroutine calling `backend.Build()`
4. Bundle upload endpoint (`POST /api/v1/apps/{id}/bundles`)
5. Bundle list endpoint (`GET /api/v1/apps/{id}/bundles`)
6. Task status endpoint (`GET /api/v1/tasks/{task_id}`)
7. Task log streaming endpoint (`GET /api/v1/tasks/{task_id}/logs`)
8. Bearer token auth middleware
9. `/healthz` endpoint (unauthenticated)
10. chi router wiring — enough to serve the above endpoints
11. `NewServer()` constructor for shared state
12. `main.go` wiring — HTTP server with graceful shutdown
13. Bundle size limit — `max_bundle_size` enforcement via
    `http.MaxBytesReader` + 413 response on oversized uploads
14. Unit and integration tests

## What's already done

Phase 0-1 delivered:

- `task.Store` — in-memory task tracking with create, status, subscribe
  (snapshot + live channel + done channel), sender (Write, Complete)
- `logstore.Store` — per-worker log capture with subscribe, cleanup
- `session.Store` — session-to-worker mapping
- `registry.Registry` — worker-to-address mapping
- `server.Server` struct with `Workers`, `Sessions`, `Registry`, `Tasks`,
  `LogStore` fields
- `server.WorkerMap` — concurrent worker tracking
- `config.Config` with `StorageConfig.MaxBundleSize` (default 100 MiB),
  `StorageConfig.BundleRetention` (default 50)
- `db.DB` with apps table, `CreateApp`, `GetApp`, `GetAppByName`,
  `ListApps`, `DeleteApp`, `FailStaleBuilds`
- `backend.Backend` interface with `Build(ctx, BuildSpec) (BuildResult, error)`
- `backend.LogStream` type
- Mock backend with configurable `BuildSuccess`

Phase 0-2 delivered:

- `DockerBackend` with `EnsureImage()` — on-demand image pulling
- Full `Backend` interface implementation (Spawn, Stop, HealthCheck, Logs,
  Addr, Build, ListManaged, RemoveResource)

## Step-by-step

### Step 1: Drop `path` column from bundles table

The `path` column in the `bundles` table is redundant — bundle filesystem
paths are derivable from `(bundle_server_path, app_id, bundle_id)` via
`NewBundlePaths()`. Remove it to keep the schema minimal.

There is no production database yet, so no migration is needed — just
update the schema string in `db.go`.

Updated schema:

```go
CREATE TABLE IF NOT EXISTS bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    uploaded_at TEXT NOT NULL
);
```

Update `BundleRow` to drop the `Path` field:

```go
type BundleRow struct {
    ID         string
    AppID      string
    Status     string
    UploadedAt string
}
```

Update `TestFailStaleBuilds` to drop the `path` column from its INSERT.

### Step 2: Bundle DB queries

Add bundle CRUD methods to `db.DB`. The bundle ID is caller-supplied
because the upload handler needs the ID for filesystem paths before the
DB row exists.

```go
func (db *DB) CreateBundle(id, appID string) (*BundleRow, error) {
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := db.Exec(
        `INSERT INTO bundles (id, app_id, status, uploaded_at)
         VALUES (?, ?, 'pending', ?)`,
        id, appID, now,
    )
    if err != nil {
        return nil, fmt.Errorf("insert bundle: %w", err)
    }
    return db.GetBundle(id)
}

func (db *DB) GetBundle(id string) (*BundleRow, error) {
    row := db.QueryRow(
        `SELECT id, app_id, status, uploaded_at FROM bundles WHERE id = ?`, id,
    )
    var b BundleRow
    err := row.Scan(&b.ID, &b.AppID, &b.Status, &b.UploadedAt)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    return &b, nil
}

func (db *DB) ListBundlesByApp(appID string) ([]BundleRow, error) {
    rows, err := db.Query(
        `SELECT id, app_id, status, uploaded_at
         FROM bundles WHERE app_id = ?
         ORDER BY uploaded_at DESC`, appID,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var bundles []BundleRow
    for rows.Next() {
        var b BundleRow
        if err := rows.Scan(&b.ID, &b.AppID, &b.Status, &b.UploadedAt); err != nil {
            return nil, err
        }
        bundles = append(bundles, b)
    }
    return bundles, rows.Err()
}

func (db *DB) UpdateBundleStatus(id, status string) error {
    _, err := db.Exec(
        `UPDATE bundles SET status = ? WHERE id = ?`, status, id,
    )
    return err
}

func (db *DB) SetActiveBundle(appID, bundleID string) error {
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := db.Exec(
        `UPDATE apps SET active_bundle = ?, updated_at = ? WHERE id = ?`,
        bundleID, now, appID,
    )
    return err
}

func (db *DB) DeleteBundle(id string) (bool, error) {
    result, err := db.Exec(`DELETE FROM bundles WHERE id = ?`, id)
    if err != nil {
        return false, err
    }
    n, _ := result.RowsAffected()
    return n > 0, nil
}
```

**Tests:**

- `TestCreateAndGetBundle` — create, verify fields and `pending` status
- `TestListBundlesByApp` — create two bundles, list, verify count and order
- `TestUpdateBundleStatus` — create, update to `building`, verify
- `TestSetActiveBundle` — create app + bundle, set active, re-fetch app,
  verify `ActiveBundle` pointer
- `TestDeleteBundle` — create, delete, verify gone

### Step 3: Bundle storage module

`internal/bundle/bundle.go` — filesystem operations for bundle management.
No HTTP, no DB — pure storage logic.

```go
package bundle

import (
    "fmt"
    "io"
    "os"
    "path/filepath"
)

// Paths holds the filesystem locations for a bundle.
type Paths struct {
    Archive string // {base}/{app_id}/{bundle_id}.tar.gz
    Unpacked string // {base}/{app_id}/{bundle_id}/
    Library  string // {base}/{app_id}/{bundle_id}_lib/
}

// NewBundlePaths constructs paths for a bundle. Single source of truth for
// the on-disk layout.
func NewBundlePaths(base, appID, bundleID string) Paths {
    appDir := filepath.Join(base, appID)
    return Paths{
        Archive:  filepath.Join(appDir, bundleID+".tar.gz"),
        Unpacked: filepath.Join(appDir, bundleID),
        Library:  filepath.Join(appDir, bundleID+"_lib"),
    }
}
```

**WriteArchive** — streams the request body directly to a temp file, then
atomically renames into place. No buffering the full upload in memory.

```go
// WriteArchive streams r to a temp file, then atomically renames it to
// the archive path. Creates the app directory if needed.
func WriteArchive(paths Paths, r io.Reader) error {
    appDir := filepath.Dir(paths.Archive)
    if err := os.MkdirAll(appDir, 0o755); err != nil {
        return fmt.Errorf("create app dir: %w", err)
    }

    // Temp file in the same directory for same-filesystem rename
    tmp, err := os.CreateTemp(appDir, ".bundle-*.tar.gz.tmp")
    if err != nil {
        return fmt.Errorf("create temp file: %w", err)
    }
    tmpPath := tmp.Name()

    // Clean up temp file on any error
    ok := false
    defer func() {
        if !ok {
            tmp.Close()
            os.Remove(tmpPath)
        }
    }()

    if _, err := io.Copy(tmp, r); err != nil {
        return fmt.Errorf("write archive: %w", err)
    }
    if err := tmp.Close(); err != nil {
        return fmt.Errorf("close temp file: %w", err)
    }

    // Atomic rename
    if err := os.Rename(tmpPath, paths.Archive); err != nil {
        return fmt.Errorf("rename archive: %w", err)
    }
    ok = true
    return nil
}
```

**UnpackArchive** — decompress and untar into the unpacked directory.

```go
import (
    "archive/tar"
    "compress/gzip"
)

// UnpackArchive decompresses the tar.gz archive into the unpacked directory.
func UnpackArchive(paths Paths) error {
    f, err := os.Open(paths.Archive)
    if err != nil {
        return fmt.Errorf("open archive: %w", err)
    }
    defer f.Close()

    gz, err := gzip.NewReader(f)
    if err != nil {
        return fmt.Errorf("gzip reader: %w", err)
    }
    defer gz.Close()

    if err := os.MkdirAll(paths.Unpacked, 0o755); err != nil {
        return fmt.Errorf("create unpack dir: %w", err)
    }

    tr := tar.NewReader(gz)
    for {
        hdr, err := tr.Next()
        if err == io.EOF {
            break
        }
        if err != nil {
            return fmt.Errorf("tar next: %w", err)
        }

        target := filepath.Join(paths.Unpacked, hdr.Name)

        // Prevent path traversal
        rel, err := filepath.Rel(paths.Unpacked, target)
        if err != nil || strings.HasPrefix(rel, "..") {
            return fmt.Errorf("tar path escapes unpack dir: %s", hdr.Name)
        }

        switch hdr.Typeflag {
        case tar.TypeDir:
            if err := os.MkdirAll(target, 0o755); err != nil {
                return fmt.Errorf("mkdir %s: %w", target, err)
            }
        case tar.TypeReg:
            if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
                return fmt.Errorf("mkdir parent %s: %w", target, err)
            }
            out, err := os.Create(target)
            if err != nil {
                return fmt.Errorf("create %s: %w", target, err)
            }
            if _, err := io.Copy(out, tr); err != nil {
                out.Close()
                return fmt.Errorf("write %s: %w", target, err)
            }
            out.Close()
        }
    }
    return nil
}
```

**Path traversal protection** is critical — a malicious tar.gz could
contain entries like `../../etc/passwd`. The check ensures every
extracted path stays within the unpack directory.

**CreateLibraryDir:**

```go
// CreateLibraryDir creates the output directory for dependency restoration.
func CreateLibraryDir(paths Paths) error {
    return os.MkdirAll(paths.Library, 0o755)
}
```

**DeleteFiles** — best-effort cleanup:

```go
// DeleteFiles removes a bundle's archive, unpacked dir, and library dir.
// Errors are logged but do not fail the operation.
func DeleteFiles(paths Paths) {
    for _, p := range []string{paths.Archive, paths.Unpacked, paths.Library} {
        if err := os.RemoveAll(p); err != nil {
            slog.Warn("failed to delete bundle path", "path", p, "error", err)
        }
    }
}
```

**EnforceRetention** — keep at most `retention` bundles per app:

```go
// EnforceRetention deletes the oldest non-active bundles when the count
// exceeds retention. Returns IDs of deleted bundles.
func EnforceRetention(database *db.DB, base, appID string, activeBundleID string, retention int) []string {
    bundles, err := database.ListBundlesByApp(appID)
    if err != nil {
        slog.Warn("retention: list bundles failed", "app_id", appID, "error", err)
        return nil
    }

    // Bundles are ordered newest-first. Keep the first `retention` plus
    // any bundle that is the active one.
    var toDelete []db.BundleRow
    kept := 0
    for _, b := range bundles {
        isActive := b.ID == activeBundleID
        if isActive || kept < retention {
            if !isActive {
                kept++
            }
            continue
        }
        toDelete = append(toDelete, b)
    }

    var deletedIDs []string
    for _, b := range toDelete {
        paths := NewBundlePaths(base, appID, b.ID)
        DeleteFiles(paths)
        if _, err := database.DeleteBundle(b.ID); err != nil {
            slog.Warn("retention: delete bundle row failed",
                "bundle_id", b.ID, "error", err)
        } else {
            deletedIDs = append(deletedIDs, b.ID)
        }
    }
    return deletedIDs
}
```

**Tests:**

- `TestWriteAndUnpackArchive` — write a minimal tar.gz, unpack, verify
  `app.R` exists in unpacked dir
- `TestDeleteFiles` — write + unpack + create library, delete, verify all
  gone
- `TestPathTraversal` — archive with `../../evil` entry, verify unpack
  returns error

Test helper for building a minimal tar.gz lives in
`internal/testutil/bundle.go` so both `bundle` and `api` tests can use
it:

```go
package testutil

import (
    "archive/tar"
    "bytes"
    "compress/gzip"
    "testing"
)

// MakeBundle returns a valid tar.gz containing a single app.R file.
func MakeBundle(t *testing.T) []byte {
    t.Helper()
    var buf bytes.Buffer
    gz := gzip.NewWriter(&buf)
    tw := tar.NewWriter(gz)

    content := []byte("library(shiny)\nshinyApp(ui, server)")
    hdr := &tar.Header{
        Name: "app.R",
        Mode: 0o644,
        Size: int64(len(content)),
    }
    tw.WriteHeader(hdr)
    tw.Write(content)
    tw.Close()
    gz.Close()
    return buf.Bytes()
}
```

### Step 4: Restore pipeline

`internal/bundle/restore.go` — orchestrates the async restore task.
Spawns a goroutine that calls `backend.Build()`, streams status to the
`task.Store`, updates bundle status in SQLite, and sets `active_bundle`
on success.

```go
package bundle

import (
    "context"
    "fmt"
    "log/slog"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/db"
    "github.com/cynkra/blockyard/internal/task"
)

// RestoreParams holds everything the restore goroutine needs.
type RestoreParams struct {
    Backend    backend.Backend
    DB         *db.DB
    Tasks      *task.Store
    Sender     task.Sender
    AppID      string
    BundleID   string
    Paths      Paths
    Image      string
    RvVersion  string
    Retention  int
    BasePath   string // bundle_server_path for retention cleanup
}

// SpawnRestore launches the restore pipeline in a background goroutine.
// Returns immediately.
func SpawnRestore(params RestoreParams) {
    go func() {
        err := runRestore(params)
        if err != nil {
            params.Sender.Write(fmt.Sprintf("ERROR: %s", err))
            params.Sender.Complete(task.Failed)
            if err := params.DB.UpdateBundleStatus(params.BundleID, "failed"); err != nil {
                slog.Error("restore: update status to failed",
                    "bundle_id", params.BundleID, "error", err)
            }
            return
        }
        params.Sender.Complete(task.Completed)
        // Enforce retention after successful deploy
        EnforceRetention(
            params.DB, params.BasePath, params.AppID,
            params.BundleID, params.Retention,
        )
    }()
}

func runRestore(p RestoreParams) error {
    // 1. Update status to "building"
    if err := p.DB.UpdateBundleStatus(p.BundleID, "building"); err != nil {
        return fmt.Errorf("update status: %w", err)
    }
    p.Sender.Write("Starting dependency restoration...")

    // 2. Build the spec
    labels := map[string]string{
        "dev.blockyard/managed":   "true",
        "dev.blockyard/app-id":    p.AppID,
        "dev.blockyard/bundle-id": p.BundleID,
    }

    spec := backend.BuildSpec{
        AppID:       p.AppID,
        BundleID:    p.BundleID,
        Image:       p.Image,
        RvVersion:   p.RvVersion,
        BundlePath:  p.Paths.Unpacked,
        LibraryPath: p.Paths.Library,
        Labels:      labels,
    }

    // 3. Run the build
    result, err := p.Backend.Build(context.Background(), spec)
    if err != nil {
        return fmt.Errorf("build: %w", err)
    }
    if !result.Success {
        return fmt.Errorf("build failed with exit code %d", result.ExitCode)
    }

    // 4. Mark bundle as ready and activate
    p.Sender.Write("Build succeeded. Activating bundle...")

    if err := p.DB.UpdateBundleStatus(p.BundleID, "ready"); err != nil {
        return fmt.Errorf("update status to ready: %w", err)
    }
    if err := p.DB.SetActiveBundle(p.AppID, p.BundleID); err != nil {
        return fmt.Errorf("set active bundle: %w", err)
    }

    p.Sender.Write("Bundle activated.")
    return nil
}
```

**Build log streaming (decided: opaque build, no real-time streaming).**
`backend.Build()` runs the container to completion and returns a
`BuildResult`. Logs are not streamed in real time — the `task.Sender`
receives status messages from the restore pipeline ("Starting dependency
restoration...", "Build succeeded", etc.) but not raw build output
line-by-line. This is sufficient for v0. Real-time streaming can be added
later by splitting `Build()` or accepting a log callback.

### Step 5: `NewServer()` constructor

Add a constructor to `server/state.go` that wires up all in-memory stores.
This is the single place that guarantees all fields are initialized.

```go
func NewServer(cfg *config.Config, be backend.Backend, database *db.DB) *Server {
    return &Server{
        Config:   cfg,
        Backend:  be,
        DB:       database,
        Workers:  NewWorkerMap(),
        Sessions: session.NewStore(),
        Registry: registry.New(),
        Tasks:    task.NewStore(),
        LogStore: logstore.NewStore(),
    }
}
```

### Step 6: Bearer token auth middleware

`internal/api/auth.go` — chi middleware that validates the `Authorization:
Bearer <token>` header against `config.Server.Token`.

```go
package api

import (
    "net/http"
    "strings"

    "github.com/cynkra/blockyard/internal/server"
)

// BearerAuth returns a chi middleware that validates the bearer token.
func BearerAuth(srv *server.Server) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            auth := r.Header.Get("Authorization")
            token, found := strings.CutPrefix(auth, "Bearer ")
            if !found || token != srv.Config.Server.Token {
                http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

### Step 7: Bundle upload endpoint

`internal/api/bundles.go` — bundle upload and list handlers.

```go
package api

import (
    "encoding/json"
    "log/slog"
    "net/http"

    "github.com/go-chi/chi/v5"
    "github.com/google/uuid"

    "github.com/cynkra/blockyard/internal/bundle"
    "github.com/cynkra/blockyard/internal/server"
)

func UploadBundle(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        appID := chi.URLParam(r, "id")

        // 1. Validate app exists
        app, err := srv.DB.GetApp(appID)
        if err != nil {
            writeError(w, http.StatusInternalServerError, "internal_error",
                "db error: "+err.Error())
            return
        }
        if app == nil {
            writeError(w, http.StatusNotFound, "not_found",
                "app "+appID+" not found")
            return
        }

        // 2. Enforce body size limit
        // http.MaxBytesReader wraps r.Body; reads beyond the limit
        // return an error, and the handler responds with 413.
        r.Body = http.MaxBytesReader(w, r.Body, srv.Config.Storage.MaxBundleSize)

        // 3. Generate IDs
        bundleID := uuid.New().String()
        taskID := uuid.New().String()
        slog.Info("bundle upload started", "app_id", appID, "bundle_id", bundleID)

        // 4. Stream archive to disk
        paths := bundle.NewBundlePaths(srv.Config.Storage.BundleServerPath, app.ID, bundleID)
        if err := bundle.WriteArchive(paths, r.Body); err != nil {
            // Check if this was a size limit error
            if err.Error() == "http: request body too large" {
                writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
                    "bundle exceeds max_bundle_size")
                return
            }
            writeError(w, http.StatusInternalServerError, "internal_error",
                "write archive: "+err.Error())
            return
        }

        // 5. Unpack
        if err := bundle.UnpackArchive(paths); err != nil {
            bundle.DeleteFiles(paths)
            writeError(w, http.StatusInternalServerError, "internal_error",
                "unpack: "+err.Error())
            return
        }

        // 6. Create library dir
        if err := bundle.CreateLibraryDir(paths); err != nil {
            bundle.DeleteFiles(paths)
            writeError(w, http.StatusInternalServerError, "internal_error",
                "create library dir: "+err.Error())
            return
        }

        // 7. Insert bundle row (status = pending)
        if _, err := srv.DB.CreateBundle(bundleID, app.ID); err != nil {
            bundle.DeleteFiles(paths)
            writeError(w, http.StatusInternalServerError, "internal_error",
                "create bundle row: "+err.Error())
            return
        }

        // 8. Create task in TaskStore
        sender := srv.Tasks.Create(taskID)

        // 9. Spawn async restore
        bundle.SpawnRestore(bundle.RestoreParams{
            Backend:   srv.Backend,
            DB:        srv.DB,
            Tasks:     srv.Tasks,
            Sender:    sender,
            AppID:     app.ID,
            BundleID:  bundleID,
            Paths:     paths,
            Image:     srv.Config.Docker.Image,
            RvVersion: srv.Config.Docker.RvVersion,
            Retention: srv.Config.Storage.BundleRetention,
            BasePath:  srv.Config.Storage.BundleServerPath,
        })

        // 10. Return 202
        slog.Info("bundle upload accepted, restore spawned",
            "app_id", appID, "bundle_id", bundleID, "task_id", taskID)
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusAccepted)
        json.NewEncoder(w).Encode(map[string]string{
            "bundle_id": bundleID,
            "task_id":   taskID,
        })
    }
}

func ListBundles(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        appID := chi.URLParam(r, "id")

        bundles, err := srv.DB.ListBundlesByApp(appID)
        if err != nil {
            writeError(w, http.StatusInternalServerError, "internal_error",
                "db error: "+err.Error())
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(bundles)
    }
}
```

**Body size enforcement.** `http.MaxBytesReader` wraps `r.Body` so that
reads beyond `max_bundle_size` return an error. Because `WriteArchive`
streams the body via `io.Copy`, oversized uploads are caught during the
stream — at most `max_bundle_size` bytes are written to the temp file
before the error triggers, and the deferred cleanup removes the partial
file.

### Step 8: Task endpoints

`internal/api/tasks.go` — task status and log streaming.

**Task status:**

```go
package api

import (
    "encoding/json"
    "net/http"

    "github.com/go-chi/chi/v5"

    "github.com/cynkra/blockyard/internal/server"
    "github.com/cynkra/blockyard/internal/task"
)

func GetTaskStatus(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        taskID := chi.URLParam(r, "taskID")

        status, ok := srv.Tasks.Status(taskID)
        if !ok {
            writeError(w, http.StatusNotFound, "not_found",
                "task "+taskID+" not found")
            return
        }

        statusStr := "running"
        switch status {
        case task.Completed:
            statusStr = "completed"
        case task.Failed:
            statusStr = "failed"
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]string{
            "id":     taskID,
            "status": statusStr,
        })
    }
}
```

**Task log streaming:**

```go
func TaskLogs(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        taskID := chi.URLParam(r, "taskID")

        status, ok := srv.Tasks.Status(taskID)
        if !ok {
            writeError(w, http.StatusNotFound, "not_found",
                "task "+taskID+" not found")
            return
        }

        snapshot, live, done, ok := srv.Tasks.Subscribe(taskID)
        if !ok {
            writeError(w, http.StatusNotFound, "not_found",
                "task "+taskID+" not found")
            return
        }

        w.Header().Set("Content-Type", "text/plain")
        w.Header().Set("Transfer-Encoding", "chunked")
        w.Header().Set("X-Content-Type-Options", "nosniff")

        flusher, canFlush := w.(http.Flusher)

        // Write buffered lines
        for _, line := range snapshot {
            fmt.Fprintf(w, "%s\n", line)
        }
        if canFlush {
            flusher.Flush()
        }

        // If the task is already done, return the buffer only
        if status != task.Running {
            return
        }

        // Drain any overlap between snapshot and live channel
        drained := 0
        for drained < len(snapshot) {
            select {
            case <-live:
                drained++
            default:
                drained = len(snapshot)
            }
        }

        // Follow live output until task completes or client disconnects
        ctx := r.Context()
        for {
            select {
            case <-ctx.Done():
                return
            case <-done:
                // Drain remaining lines from channel
                for {
                    select {
                    case line, ok := <-live:
                        if !ok {
                            return
                        }
                        fmt.Fprintf(w, "%s\n", line)
                    default:
                        return
                    }
                }
            case line, ok := <-live:
                if !ok {
                    return
                }
                fmt.Fprintf(w, "%s\n", line)
                if canFlush {
                    flusher.Flush()
                }
            }
        }
    }
}
```

The handler writes directly to `http.ResponseWriter` with `Flusher`.
Buffered lines are written first, flushed, then the handler loops on the
live channel. The loop exits when the task completes (done channel
closes), the client disconnects (`r.Context().Done()`), or the live
channel is closed.

### Step 9: Shared error response helper

`internal/api/error.go` — consistent error response shape used by all
handlers.

```go
package api

import (
    "encoding/json"
    "net/http"
)

type errorResponse struct {
    Error   string `json:"error"`
    Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(errorResponse{Error: code, Message: msg})
}
```

### Step 10: chi router wiring

`internal/api/router.go` — assembles the chi router with auth middleware
and all phase 0-3 endpoints.

```go
package api

import (
    "net/http"

    "github.com/go-chi/chi/v5"

    "github.com/cynkra/blockyard/internal/server"
)

func NewRouter(srv *server.Server) http.Handler {
    r := chi.NewRouter()

    // Unauthenticated
    r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
        w.Write([]byte("ok"))
    })

    // Authenticated API
    r.Route("/api/v1", func(r chi.Router) {
        r.Use(BearerAuth(srv))

        r.Post("/apps/{id}/bundles", UploadBundle(srv))
        r.Get("/apps/{id}/bundles", ListBundles(srv))
        r.Get("/tasks/{taskID}", GetTaskStatus(srv))
        r.Get("/tasks/{taskID}/logs", TaskLogs(srv))
    })

    return r
}
```

Phase 0-4 expands this router with app CRUD and lifecycle endpoints.

### Step 11: `main.go` wiring

Update `cmd/blockyard/main.go` to start the HTTP server with graceful
shutdown.

```go
package main

import (
    "context"
    "flag"
    "log/slog"
    "net/http"
    "os"
    "os/signal"
    "syscall"

    "github.com/cynkra/blockyard/internal/api"
    "github.com/cynkra/blockyard/internal/backend/docker"
    "github.com/cynkra/blockyard/internal/config"
    "github.com/cynkra/blockyard/internal/db"
    "github.com/cynkra/blockyard/internal/server"
)

func main() {
    configPath := flag.String("config", "blockyard.toml", "path to config file")
    flag.Parse()

    slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
        Level: slog.LevelInfo,
    })))

    cfg, err := config.Load(*configPath)
    if err != nil {
        slog.Error("failed to load config", "error", err)
        os.Exit(1)
    }
    slog.Info("loaded config", "bind", cfg.Server.Bind)

    // Initialize backend
    be, err := docker.New(context.Background(), &cfg.Docker)
    if err != nil {
        slog.Error("failed to create docker backend", "error", err)
        os.Exit(1)
    }

    // Initialize database
    database, err := db.Open(cfg.Database.Path)
    if err != nil {
        slog.Error("failed to open database", "error", err)
        os.Exit(1)
    }
    defer database.Close()

    // Build shared state and router
    srv := server.NewServer(cfg, be, database)
    handler := api.NewRouter(srv)

    httpServer := &http.Server{
        Addr:    cfg.Server.Bind,
        Handler: handler,
    }

    // Graceful shutdown on SIGTERM / SIGINT
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGTERM, syscall.SIGINT)
    defer stop()

    go func() {
        slog.Info("server listening", "bind", cfg.Server.Bind)
        if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
            slog.Error("server error", "error", err)
            os.Exit(1)
        }
    }()

    <-ctx.Done()
    slog.Info("shutdown signal received")

    shutdownCtx, cancel := context.WithTimeout(context.Background(),
        cfg.Server.ShutdownTimeout.Duration)
    defer cancel()

    if err := httpServer.Shutdown(shutdownCtx); err != nil {
        slog.Error("shutdown error", "error", err)
    }
}
```

Graceful shutdown drains in-flight requests within `shutdown_timeout`.
Background task cleanup (stopping containers, failing stale builds) is
phase 0-6 work.

### Step 12: Integration tests

`internal/api/api_test.go` — tests that exercise the HTTP layer using the
mock backend. Apps are created directly via `srv.DB.CreateApp()` since app
CRUD endpoints are phase 0-4.

**Note for phase 0-4:** once `POST /api/v1/apps` exists, add integration
tests that go through the full HTTP flow (create app → upload bundle →
check task → verify activation).

**Test helper:**

```go
package api

import (
    "bytes"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/cynkra/blockyard/internal/backend/mock"
    "github.com/cynkra/blockyard/internal/config"
    "github.com/cynkra/blockyard/internal/db"
    "github.com/cynkra/blockyard/internal/server"
    "github.com/cynkra/blockyard/internal/testutil"
)

func testServer(t *testing.T) (*server.Server, *httptest.Server) {
    t.Helper()
    tmp := t.TempDir()

    cfg := &config.Config{
        Server:  config.ServerConfig{Token: "test-token"},
        Docker:  config.DockerConfig{Image: "test-image", ShinyPort: 3838},
        Storage: config.StorageConfig{
            BundleServerPath: tmp,
            BundleRetention:  50,
            MaxBundleSize:    10 * 1024 * 1024, // 10 MiB for tests
        },
    }

    database, err := db.Open(":memory:")
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(func() { database.Close() })

    be := mock.New()
    srv := server.NewServer(cfg, be, database)
    handler := NewRouter(srv)
    ts := httptest.NewServer(handler)
    t.Cleanup(ts.Close)

    return srv, ts
}
```

**Tests:**

```go
func TestHealthz(t *testing.T) {
    _, ts := testServer(t)
    resp, err := http.Get(ts.URL + "/healthz")
    if err != nil {
        t.Fatal(err)
    }
    if resp.StatusCode != 200 {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }
}

func TestUploadWithoutAuth(t *testing.T) {
    srv, ts := testServer(t)
    app, _ := srv.DB.CreateApp("test-app")

    resp, err := http.Post(
        ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
        "application/octet-stream",
        bytes.NewReader(testutil.MakeBundle(t)),
    )
    if err != nil {
        t.Fatal(err)
    }
    if resp.StatusCode != http.StatusUnauthorized {
        t.Errorf("expected 401, got %d", resp.StatusCode)
    }
}

func TestUploadToNonexistentApp(t *testing.T) {
    _, ts := testServer(t)
    req, _ := http.NewRequest("POST",
        ts.URL+"/api/v1/apps/nonexistent/bundles",
        bytes.NewReader(testutil.MakeBundle(t)))
    req.Header.Set("Authorization", "Bearer test-token")

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    if resp.StatusCode != http.StatusNotFound {
        t.Errorf("expected 404, got %d", resp.StatusCode)
    }
}

func TestUploadBundleReturns202(t *testing.T) {
    srv, ts := testServer(t)
    app, _ := srv.DB.CreateApp("test-app")

    req, _ := http.NewRequest("POST",
        ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
        bytes.NewReader(testutil.MakeBundle(t)))
    req.Header.Set("Authorization", "Bearer test-token")

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    if resp.StatusCode != http.StatusAccepted {
        t.Errorf("expected 202, got %d", resp.StatusCode)
    }

    var body map[string]string
    json.NewDecoder(resp.Body).Decode(&body)
    if body["bundle_id"] == "" {
        t.Error("expected non-empty bundle_id")
    }
    if body["task_id"] == "" {
        t.Error("expected non-empty task_id")
    }
}

func TestTaskLogsStreamsOutput(t *testing.T) {
    srv, ts := testServer(t)
    app, _ := srv.DB.CreateApp("test-app")

    // Upload a bundle
    req, _ := http.NewRequest("POST",
        ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
        bytes.NewReader(testutil.MakeBundle(t)))
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ := http.DefaultClient.Do(req)
    var body map[string]string
    json.NewDecoder(resp.Body).Decode(&body)
    taskID := body["task_id"]

    // Give the background goroutine a moment to run
    time.Sleep(100 * time.Millisecond)

    // Fetch task logs
    req, _ = http.NewRequest("GET",
        ts.URL+"/api/v1/tasks/"+taskID+"/logs", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatal(err)
    }
    if resp.StatusCode != http.StatusOK {
        t.Errorf("expected 200, got %d", resp.StatusCode)
    }

    logs, _ := io.ReadAll(resp.Body)
    if !strings.Contains(string(logs), "Starting dependency restoration") {
        t.Errorf("expected restore log output, got: %s", logs)
    }
}

func TestListBundles(t *testing.T) {
    srv, ts := testServer(t)
    app, _ := srv.DB.CreateApp("test-app")

    // Upload two bundles
    for range 2 {
        req, _ := http.NewRequest("POST",
            ts.URL+"/api/v1/apps/"+app.ID+"/bundles",
            bytes.NewReader(testutil.MakeBundle(t)))
        req.Header.Set("Authorization", "Bearer test-token")
        http.DefaultClient.Do(req)
    }

    // Give restore goroutines time to finish
    time.Sleep(100 * time.Millisecond)

    req, _ := http.NewRequest("GET",
        ts.URL+"/api/v1/apps/"+app.ID+"/bundles", nil)
    req.Header.Set("Authorization", "Bearer test-token")
    resp, _ := http.DefaultClient.Do(req)

    var bundles []map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&bundles)
    if len(bundles) != 2 {
        t.Errorf("expected 2 bundles, got %d", len(bundles))
    }
}
```

### Step 13: New dependency

```
go get github.com/go-chi/chi/v5
```

No other new dependencies — `archive/tar`, `compress/gzip`, and `io` are
all stdlib.

## New source files

| File | Purpose |
|---|---|
| `internal/bundle/bundle.go` | Bundle storage — write, unpack, delete, retention |
| `internal/bundle/restore.go` | Async restore pipeline |
| `internal/api/router.go` | chi router wiring |
| `internal/api/auth.go` | Bearer token middleware |
| `internal/api/bundles.go` | Bundle upload + list handlers |
| `internal/api/tasks.go` | Task status + log streaming handlers |
| `internal/api/error.go` | Shared error response helper |
| `internal/api/api_test.go` | Integration tests |
| `internal/testutil/bundle.go` | Shared test helper — `MakeBundle()` |

## Modified files

| File | Change |
|---|---|
| `cmd/blockyard/main.go` | HTTP server with graceful shutdown |
| `internal/server/state.go` | Add `NewServer()` constructor |
| `internal/db/db.go` | Drop `path` from bundles schema; add `CreateBundle`, `GetBundle`, `ListBundlesByApp`, `UpdateBundleStatus`, `SetActiveBundle`, `DeleteBundle` |
| `internal/db/db_test.go` | Update `TestFailStaleBuilds` INSERT; add bundle query tests |
| `go.mod` | Add `github.com/go-chi/chi/v5` |

## Exit criteria

Phase 0-3 is done when:

- `path` column removed from `bundles` table
- Bundle archive is streamed to disk (not buffered in memory) and
  unpacked correctly
- Path traversal in tar archives is rejected
- Restore pipeline calls `backend.Build()`, updates bundle status, sets
  `active_bundle` on success
- Retention cleanup deletes oldest non-active bundles when limit is exceeded
- Uploads exceeding `max_bundle_size` are rejected with 413
- `POST /api/v1/apps/{id}/bundles` returns 202 with bundle_id + task_id
- `GET /api/v1/apps/{id}/bundles` returns bundle list
- `GET /api/v1/tasks/{taskID}` returns task status
- `GET /api/v1/tasks/{taskID}/logs` streams buffered + live log output
- Bearer auth middleware rejects unauthenticated requests with 401
- `/healthz` returns 200 without auth
- `main.go` starts the server and serves requests
- `NewServer()` wires up all shared state
- All unit tests pass (`bundle/bundle.go`, `db/db.go`)
- Integration tests pass with mock backend (`api/api_test.go`)
- `go vet ./...` clean
- `go test ./...` green

