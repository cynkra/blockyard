# Phase 3-5: Rolling Update Orchestration

Server-side rolling update orchestration and the CLI commands that
trigger it. The server manages its own replacement using the Docker
socket it already mounts for worker management — no sidecar container,
no CLI-side Docker access. The CLI triggers updates via the management
API and streams progress.

Depends on phases 3-2 (interfaces, token persistence), 3-3 (Redis
shared state), and 3-4 (drain mode, passive mode, `/admin/activate`
endpoint). Completes the operations track.

---

## Prerequisites from Earlier Phases

- **Phase 3-2** — worker token persistence (both servers verify the
  same tokens), interface extraction (shared state contracts).
- **Phase 3-3** — Redis-backed session store, worker registry, and
  worker map. The new server reads existing routing state from Redis
  on startup.
- **Phase 3-4** — drain mode, passive mode (`BLOCKYARD_PASSIVE=1`),
  `POST /api/v1/admin/activate` endpoint. Phase 3-5 requires the
  following from the drain implementation:

  - **Three-method lifecycle: `Drain()` / `Finish()` / `Shutdown()`.**
    `Drain()` sets health → 503 but keeps HTTP listeners alive and
    background goroutines running. `Finish()` shuts down HTTP servers,
    cancels background goroutines, closes the DB, and flushes tracing
    — without evicting workers. `Shutdown()` does everything `Finish()`
    does plus worker eviction. The rolling update path calls `Drain()`,
    runs the watchdog, then either `Finish()` (success) or `Undrain()`
    (failure). The orchestrator goroutine survives drain because
    listeners and background context remain alive.

  - **`Undrain()` — reversible drain.** Clears the draining flag
    (health → 200). Because `Drain()` keeps HTTP listeners alive,
    `Undrain()` is a single flag clear — no listener restart needed.
    Used by the watchdog rollback path when the new server fails
    within the watch period.

## Deliverables

1. **Update orchestrator** (`internal/orchestrator/`) — server-side
   logic that executes the rolling update sequence: pull image, back up
   database, clone own container with new image, poll readyz, drain
   self, activate new, enter watchdog mode.
2. **Backup metadata** — JSON sidecar file recording image tag and
   migration version, used by rollback to know what to restore.
3. **Admin API endpoints** — `POST /api/v1/admin/update`,
   `POST /api/v1/admin/rollback`, `GET /api/v1/admin/update/status`
   for triggering and monitoring.
4. **`by admin` CLI subcommand group** — `by admin update`,
   `by admin rollback`, `by admin status`.
5. **Watchdog mode** — after draining, the old server stays alive to
   monitor the new instance. If the new instance fails within the
   watch period, the old server kills it, un-drains, and resumes.
6. **Scheduled auto-updates** — server-side `[update]` config with
   cron schedule. The server triggers the same orchestration logic
   internally on a timer.
7. **Tests** — orchestrator unit tests with mock Docker client,
   API endpoint tests, CLI integration tests.

---

## Step-by-step

### Step 1: Config additions

In `internal/config/config.go`, add the `[update]` section:

```go
type Config struct {
    // ... existing fields ...
    Update *UpdateConfig `toml:"update"` // nil when not configured
}

type UpdateConfig struct {
    Schedule    string   `toml:"schedule"`     // cron expression; empty = disabled
    Channel     string   `toml:"channel"`      // "stable" (default) or "main"
    WatchPeriod Duration `toml:"watch_period"` // default: 5m
}
```

```toml
[update]
schedule = ""          # cron expression; empty = disabled
channel = "stable"     # "stable" or "main"
watch_period = "5m"    # health monitoring after update completes
```

Environment variable overrides follow the existing pattern:
`BLOCKYARD_UPDATE_SCHEDULE`, `BLOCKYARD_UPDATE_CHANNEL`,
`BLOCKYARD_UPDATE_WATCH_PERIOD`.

### Step 2: Backup metadata

Extend `internal/db/backup.go`. After `Backup()` creates the database
snapshot, write a JSON sidecar at `{backup_path}.meta.json`:

```go
// BackupMeta records the state at the time of backup so rollback
// knows what to restore.
type BackupMeta struct {
    BackupPath       string `json:"backup_path"`
    ImageTag         string `json:"image_tag"`
    MigrationVersion uint   `json:"migration_version"`
    CreatedAt        string `json:"created_at"`
}

// BackupWithMeta creates a database backup and writes a metadata
// sidecar. Returns the backup path and metadata.
func (db *DB) BackupWithMeta(ctx context.Context, imageTag string) (*BackupMeta, error) {
    backupPath, err := db.Backup(ctx)
    if err != nil {
        return nil, err
    }

    ver, _, err := db.MigrationVersion()
    if err != nil {
        os.Remove(backupPath)
        return nil, fmt.Errorf("backup: read migration version: %w", err)
    }

    meta := &BackupMeta{
        BackupPath:       backupPath,
        ImageTag:         imageTag,
        MigrationVersion: ver,
        CreatedAt:        time.Now().UTC().Format(time.RFC3339),
    }

    metaPath := backupPath + ".meta.json"
    data, _ := json.MarshalIndent(meta, "", "  ")
    if err := os.WriteFile(metaPath, data, 0o600); err != nil {
        os.Remove(backupPath)
        return nil, fmt.Errorf("backup: write metadata: %w", err)
    }

    return meta, nil
}
```

`MigrationVersion()` already exists on `*db.DB` — it wraps
`golang-migrate`'s `Version()` method.

Also add `LatestBackupMeta()` to find the most recent metadata file
for rollback:

```go
// LatestBackupMeta finds the most recent backup metadata file in the
// database directory. Returns ErrNoBackup if none exists.
func LatestBackupMeta(dbPath string) (*BackupMeta, error) {
    dir := filepath.Dir(dbPath)
    pattern := filepath.Join(dir, "*.meta.json")
    matches, _ := filepath.Glob(pattern)
    if len(matches) == 0 {
        return nil, ErrNoBackup
    }
    sort.Strings(matches) // timestamp in filename → lexicographic = chronological
    return readBackupMeta(matches[len(matches)-1])
}

var ErrNoBackup = errors.New("no backup metadata found")
```

### Step 3: Update orchestrator

New package `internal/orchestrator/`. This is the core logic — it
runs inside the server process, uses the Docker client the backend
already holds, and does not depend on the CLI.

```go
package orchestrator

// Orchestrator manages rolling updates from inside the running server.
type Orchestrator struct {
    docker   dockerClient     // subset of Docker API (see interface below)
    serverID string           // own container ID from DockerBackend.ServerID()
    db       *db.DB
    cfg      *config.Config
    update   updateAPI        // interface for GitHub release checking
    log      *slog.Logger
}

// dockerClient is the subset of the Docker API needed by the orchestrator.
// Matches methods already available on the backend's client.
type dockerClient interface {
    ContainerInspect(ctx context.Context, id string, opts client.ContainerInspectOptions) (client.ContainerInspectResult, error)
    ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error)
    ContainerStart(ctx context.Context, id string, opts client.ContainerStartOptions) (client.ContainerStartResult, error)
    ContainerStop(ctx context.Context, id string, opts client.ContainerStopOptions) (client.ContainerStopResult, error)
    ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
    ContainerWait(ctx context.Context, id string, opts client.ContainerWaitOptions) client.ContainerWaitResult
    ImagePull(ctx context.Context, ref string, opts client.ImagePullOptions) (io.ReadCloser, error)
}

// updateAPI abstracts the GitHub release check so tests can mock it.
type updateAPI interface {
    CheckLatest(channel, currentVersion string) (*update.Result, error)
}
```

#### Container cloning

The orchestrator inspects its own container and creates a clone with
the new image. This is the only reliable way to replicate arbitrary
deployment configurations (volumes, networks, env, labels, resource
limits).

```go
// cloneConfig inspects the current container and returns a
// ContainerCreateOptions for a new container with the given image
// and additional environment variables.
func (o *Orchestrator) cloneConfig(
    ctx context.Context,
    newImage string,
    extraEnv []string,
) (client.ContainerCreateOptions, error) {
    result, err := o.docker.ContainerInspect(ctx, o.serverID,
        client.ContainerInspectOptions{})
    if err != nil {
        return client.ContainerCreateOptions{},
            fmt.Errorf("inspect self: %w", err)
    }

    cfg := result.Config
    hostCfg := result.HostConfig

    // Override image.
    cfg.Image = newImage

    // Inject passive mode + mark as the new instance.
    cfg.Env = appendOrReplace(cfg.Env, "BLOCKYARD_PASSIVE", "1")
    for _, e := range extraEnv {
        parts := strings.SplitN(e, "=", 2)
        cfg.Env = appendOrReplace(cfg.Env, parts[0], parts[1])
    }

    // Generate a unique container name to avoid conflicts.
    cfg.Hostname = ""
    name := fmt.Sprintf("blockyard-update-%d", time.Now().Unix())

    return client.ContainerCreateOptions{
        Name:       name,
        Config:     cfg,
        HostConfig: hostCfg,
    }, nil
}
```

The clone inherits every aspect of the current container's
configuration: bind mounts (data volume, Docker socket), named
volumes, network connections, labels (for proxy service discovery),
resource limits, restart policy. The only changes are the image tag
and `BLOCKYARD_PASSIVE=1`.

#### Rolling update sequence

```go
// Update executes the rolling update. It reports progress to the
// provided sender (task.Sender) and returns nil on success.
//
// The caller (API handler or cron trigger) runs this in a goroutine.
// The context should be the server's background context, not a
// request context.
func (o *Orchestrator) Update(
    ctx context.Context,
    channel string,
    sender task.Sender,
    drainFn func(),       // sets health → 503 (listeners stay alive)
    undrainFn func(),     // clears draining flag (health → 200)
) error {
    // 1. Check for newer version.
    result, err := o.update.CheckLatest(channel, o.cfg.Server.Version)
    if err != nil {
        return fmt.Errorf("check latest: %w", err)
    }
    if !result.UpdateAvailable {
        sender.Write("Already up to date (" + result.CurrentVersion + ").")
        return nil
    }
    newImage := imageRef(result)
    sender.Write(fmt.Sprintf("Update available: %s → %s",
        result.CurrentVersion, result.LatestVersion))

    // 2. Pull new image.
    sender.Write("Pulling " + newImage + " ...")
    if err := o.pullImage(ctx, newImage); err != nil {
        return fmt.Errorf("pull image: %w", err)
    }

    // 3. Back up database.
    sender.Write("Backing up database ...")
    meta, err := o.db.BackupWithMeta(ctx, o.currentImageTag())
    if err != nil {
        return fmt.Errorf("backup: %w", err)
    }
    sender.Write("Backup: " + meta.BackupPath)

    // 4. Start new container (passive mode).
    sender.Write("Starting new container ...")
    newID, err := o.startClone(ctx, newImage)
    if err != nil {
        return fmt.Errorf("start new container: %w", err)
    }

    // 5. Poll /readyz on new container until 200.
    sender.Write("Waiting for new container to become ready ...")
    newAddr, err := o.waitReady(ctx, newID)
    if err != nil {
        o.killAndRemove(ctx, newID)
        return fmt.Errorf("new container never became ready: %w", err)
    }

    // 6. Drain self.
    sender.Write("Draining current server ...")
    drainFn()

    // 7. Activate new server (start background goroutines).
    sender.Write("Activating new server ...")
    if err := o.activate(ctx, newAddr); err != nil {
        o.killAndRemove(ctx, newID)
        undrainFn()
        return fmt.Errorf("activate new server: %w", err)
    }

    // 8. Record new container ID for watchdog.
    sender.Write("Update complete. Entering watchdog mode ...")
    return nil
}
```

**Each failure point has a defined recovery:**

| Step | Failure | Recovery |
|------|---------|----------|
| 1. Check version | Network/API error | Abort, nothing changed |
| 2. Pull image | Pull fails | Abort, nothing changed |
| 3. Backup | DB error | Abort, nothing changed |
| 4. Start clone | Container create/start fails | Abort, nothing changed |
| 5. Wait ready | Timeout or crash | Kill new container, abort |
| 6. Drain self | N/A (internal call) | — |
| 7. Activate new | HTTP error | Kill new container, un-drain old server, abort |

Every failure point is recoverable. Steps 6–7 use the un-drain
capability added to phase 3-4's `Drainer` (see "Phase 3-4 Changes
Required" above). The proxy re-discovers the old server when its
health endpoints return 200 again.

#### Watchdog mode

After the update sequence completes, the old server enters watchdog
mode instead of exiting immediately. It polls the new server's
`/readyz` endpoint for the configured watch period.

```go
// Watchdog monitors the new server after a successful update.
// If the new server becomes unhealthy within the watch period,
// it kills the new container, un-drains, and resumes serving.
//
// If the new server stays healthy for the full period, the old
// server exits.
func (o *Orchestrator) Watchdog(
    ctx context.Context,
    newID string,
    newAddr string,
    watchPeriod time.Duration,
    undrainFn func(),
    sender task.Sender,
) error {
    deadline := time.Now().Add(watchPeriod)
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            if time.Now().After(deadline) {
                sender.Write("Watch period elapsed. New server healthy. Exiting.")
                return nil // caller exits the process
            }
            if err := o.checkReady(ctx, newAddr); err != nil {
                sender.Write(fmt.Sprintf(
                    "New server unhealthy: %v. Rolling back.", err))
                o.killAndRemove(ctx, newID)
                undrainFn()
                sender.Write("Rolled back. Old server resumed.")
                return fmt.Errorf("watchdog: new server failed: %w", err)
            }
        }
    }
}
```

The old server's HTTP listeners remain alive during drain — only the
health endpoints return 503, causing the proxy to stop routing traffic.
The watchdog goroutine runs in the background context. If the watchdog
triggers a rollback, `undrainFn()` clears the draining flag so health
endpoints return 200 and the proxy resumes routing.

### Step 4: Rollback

Rollback uses the same orchestrator. The operator (or watchdog)
triggers it when the new server is unhealthy.

```go
// Rollback restores the previous version using backup metadata.
//
// 1. Read latest backup metadata
// 2. Pull old image
// 3. Start old container (passive mode)
// 4. Run down migrations to the recorded version
// 5. Poll /readyz on old container
// 6. Drain current server
// 7. Activate old container
func (o *Orchestrator) Rollback(
    ctx context.Context,
    sender task.Sender,
    drainFn func(),
    undrainFn func(),
) error {
    // 1. Find backup metadata.
    dbPath := o.cfg.Database.Path
    if o.cfg.Database.Driver == "postgres" {
        dbPath = "." // pg backups written to cwd
    }
    meta, err := db.LatestBackupMeta(dbPath)
    if errors.Is(err, db.ErrNoBackup) {
        return fmt.Errorf("no backup found — cannot rollback. " +
            "Restore manually from the database backup directory")
    }
    if err != nil {
        return fmt.Errorf("read backup metadata: %w", err)
    }
    sender.Write(fmt.Sprintf("Rolling back to image %s (migration %d)",
        meta.ImageTag, meta.MigrationVersion))

    // 2. Check for irreversible migrations.
    currentVer, _, _ := o.db.MigrationVersion()
    if currentVer != meta.MigrationVersion {
        if err := o.db.CheckDownMigrationSafety(
            meta.MigrationVersion, currentVer); err != nil {
            return fmt.Errorf(
                "cannot rollback: %w. Restore manually from backup: %s",
                err, meta.BackupPath)
        }
    }

    // 3. Pull old image.
    oldImage := imageWithTag(meta.ImageTag)
    sender.Write("Pulling " + oldImage + " ...")
    if err := o.pullImage(ctx, oldImage); err != nil {
        return fmt.Errorf("pull old image: %w", err)
    }

    // 4. Run down migrations.
    if currentVer != meta.MigrationVersion {
        sender.Write(fmt.Sprintf(
            "Running down migrations: %d → %d ...",
            currentVer, meta.MigrationVersion))
        if err := o.db.MigrateDown(meta.MigrationVersion); err != nil {
            return fmt.Errorf(
                "down migration failed: %w. Restore manually from backup: %s",
                err, meta.BackupPath)
        }
    }

    // 5-7. Same clone → wait → drain → activate flow as Update.
    newID, err := o.startClone(ctx, oldImage)
    if err != nil {
        return fmt.Errorf("start old container: %w", err)
    }

    newAddr, err := o.waitReady(ctx, newID)
    if err != nil {
        o.killAndRemove(ctx, newID)
        return fmt.Errorf("old container never became ready: %w", err)
    }

    drainFn()

    if err := o.activate(ctx, newAddr); err != nil {
        o.killAndRemove(ctx, newID)
        undrainFn()
        return fmt.Errorf("activate old container: %w", err)
    }

    sender.Write("Rollback complete.")
    return nil
}
```

`CheckDownMigrationSafety` scans the down migration files between
the two versions for `-- irreversible:` markers. If any are found,
the automated rollback aborts and directs the operator to restore
from the backup file manually. This method is new — added alongside
the rollback logic.

```go
// CheckDownMigrationSafety verifies that all down migrations between
// fromVersion and toVersion are reversible. Returns an error describing
// the first irreversible migration found.
func (db *DB) CheckDownMigrationSafety(toVersion, fromVersion uint) error {
    for v := fromVersion; v > toVersion; v-- {
        content := db.readDownMigration(v)
        if strings.Contains(content, "-- irreversible:") {
            return fmt.Errorf("migration %03d is irreversible", v)
        }
    }
    return nil
}
```

### Step 5: Admin API endpoints

In `internal/api/admin.go`, new handlers behind admin-role auth:

```go
// POST /api/v1/admin/update
// Triggers a rolling update. Returns 202 with a task ID for polling.
//
// Request body (optional):
//   {"channel": "stable"}  — override configured channel
//
// Response:
//   {"task_id": "..."}
func handleAdminUpdate(srv *server.Server, orch *orchestrator.Orchestrator) http.HandlerFunc

// POST /api/v1/admin/rollback
// Triggers a rollback to the previous version.
// Returns 202 with a task ID.
//
// Response:
//   {"task_id": "..."}
func handleAdminRollback(srv *server.Server, orch *orchestrator.Orchestrator) http.HandlerFunc

// GET /api/v1/admin/update/status
// Returns the current update state: idle, in_progress, watching,
// or the result of the last update/rollback.
//
// Response:
//   {"state": "idle"|"updating"|"watching"|"rolling_back",
//    "task_id": "...",
//    "version": "...",
//    "message": "..."}
func handleAdminUpdateStatus(srv *server.Server) http.HandlerFunc
```

The update and rollback handlers create a task via `srv.Tasks.Create`,
spawn the orchestrator in a goroutine, and return the task ID. The
CLI polls via the existing `GET /api/v1/tasks/{id}` endpoint for
progress, same as bundle deploys.

**Concurrency guard:** only one update or rollback can run at a time.
An `atomic.Bool` on the orchestrator gates entry — concurrent
requests return `409 Conflict`.

#### Route registration

In `internal/api/router.go`, add to the management router (admin-only
group):

```go
r.Route("/api/v1/admin", func(r chi.Router) {
    r.Use(requireRole("admin"))
    r.Post("/update", handleAdminUpdate(srv, orch))
    r.Post("/rollback", handleAdminRollback(srv, orch))
    r.Get("/update/status", handleAdminUpdateStatus(srv))
    // POST /api/v1/admin/activate already exists (phase 3-4)
})
```

### Step 6: Orchestrator wiring in main.go

In `cmd/blockyard/main.go`, after the Docker backend and server are
constructed:

```go
// Set up update orchestrator (requires container mode).
var orch *orchestrator.Orchestrator
if be, ok := srv.Backend.(*docker.DockerBackend); ok && be.ServerID() != "" {
    orch = orchestrator.New(
        be.Client(),
        be.ServerID(),
        srv.DB,
        cfg,
        &update.DefaultChecker{},
        slog.Default(),
    )
}
```

The orchestrator is `nil` when running in native mode (no Docker
socket) or with the process backend. The API endpoints check for
nil and return `501 Not Implemented` — these deployments use the
basic restart path (`docker compose pull && up -d`).

The drain and undrain functions are closures that reference the
HTTP servers and background context:

```go
drainFn := func() {
    drainer.Drain()
}
undrainFn := func() {
    drainer.Undrain()
}
```

Where `drainer` is the `drain.Drainer` from phase 3-4.

After a successful update, the old server runs the watchdog then
calls `Finish()` to cleanly tear down:

```go
// In the update handler goroutine:
if err := orch.Update(bgCtx, channel, sender, drainFn, undrainFn); err != nil {
    sender.Fail(err.Error())
    return
}

// Enter watchdog mode.
watchPeriod := cfg.Update.WatchPeriod.Duration
if watchPeriod == 0 {
    watchPeriod = 5 * time.Minute
}
if err := orch.Watchdog(bgCtx, newID, newAddr, watchPeriod, undrainFn, sender); err != nil {
    sender.Fail(err.Error())
    return // rollback happened, server is still running
}

// Watchdog passed — clean exit (workers survive via Redis).
sender.Complete("Update successful. Shutting down old server.")
drainer.Finish(cfg.Server.DrainTimeout.Duration)
```

### Step 7: `by admin` CLI subcommand group

New file `cmd/by/admin.go`:

```go
func adminCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "admin",
        Short: "Server administration commands",
        Long:  "Commands that manage the blockyard server itself. Requires admin role.",
    }
    cmd.AddCommand(
        adminUpdateCmd(),
        adminRollbackCmd(),
        adminStatusCmd(),
    )
    return cmd
}
```

Register in `cmd/by/main.go`:

```go
root.AddCommand(
    // ... existing commands ...
    adminCmd(),
)
```

#### `by admin update`

```go
func adminUpdateCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "update",
        Short: "Trigger a rolling update of the server",
        Args:  cobra.NoArgs,
        RunE: func(cmd *cobra.Command, _ []string) error {
            jsonOutput := jsonFlag(cmd)
            c := mustClient(jsonOutput)

            channel, _ := cmd.Flags().GetString("channel")
            yes, _ := cmd.Flags().GetBool("yes")

            // Pre-flight: check what's available.
            resp, err := c.Get("/api/v1/admin/update/status")
            if err != nil {
                exitError(jsonOutput, err)
            }
            var status updateStatus
            if err := apiclient.DecodeJSON(resp, &status); err != nil {
                exitError(jsonOutput, err)
            }
            if status.State != "idle" {
                exitErrorf(jsonOutput,
                    "update already in progress (state: %s)", status.State)
            }

            // Confirmation prompt.
            if !yes && !jsonOutput {
                fmt.Printf("Update server to latest %s release? [y/N] ", channel)
                var answer string
                fmt.Scanln(&answer)
                if answer != "y" && answer != "Y" {
                    fmt.Println("Cancelled.")
                    return nil
                }
            }

            // Trigger update.
            body := map[string]any{}
            if channel != "" {
                body["channel"] = channel
            }
            resp, err = c.PostJSON("/api/v1/admin/update", body)
            if err != nil {
                exitError(jsonOutput, err)
            }
            var result struct{ TaskID string `json:"task_id"` }
            if err := apiclient.DecodeJSON(resp, &result); err != nil {
                exitError(jsonOutput, err)
            }

            if jsonOutput {
                printJSON(result)
                return nil
            }

            // Stream progress.
            return streamTaskProgress(c, result.TaskID)
        },
    }
    cmd.Flags().String("channel", "",
        `update channel: "stable" or "main" (default: server config)`)
    cmd.Flags().Bool("yes", false, "skip confirmation prompt")
    return cmd
}
```

`streamTaskProgress` polls `GET /api/v1/tasks/{id}` and prints
incremental output lines. Same pattern used by `by deploy` for
bundle upload progress.

#### `by admin rollback`

Same structure — `POST /api/v1/admin/rollback`, poll task progress.
Includes a `--yes` flag for confirmation skip.

#### `by admin status`

```go
func adminStatusCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "status",
        Short: "Show current update state",
        Args:  cobra.NoArgs,
        RunE: func(cmd *cobra.Command, _ []string) error {
            jsonOutput := jsonFlag(cmd)
            c := mustClient(jsonOutput)
            resp, err := c.Get("/api/v1/admin/update/status")
            if err != nil {
                exitError(jsonOutput, err)
            }
            var status updateStatus
            if err := apiclient.DecodeJSON(resp, &status); err != nil {
                exitError(jsonOutput, err)
            }
            if jsonOutput {
                printJSON(status)
            } else {
                fmt.Printf("State:   %s\n", status.State)
                if status.Version != "" {
                    fmt.Printf("Version: %s\n", status.Version)
                }
                if status.Message != "" {
                    fmt.Printf("Message: %s\n", status.Message)
                }
            }
            return nil
        },
    }
}

type updateStatus struct {
    State   string `json:"state"`
    TaskID  string `json:"task_id,omitempty"`
    Version string `json:"version,omitempty"`
    Message string `json:"message,omitempty"`
}
```

### Step 8: Scheduled auto-updates

In `cmd/blockyard/main.go`, alongside the other background goroutines:

```go
if cfg.Update != nil && cfg.Update.Schedule != "" {
    finishFn := func() {
        drainer.Finish(cfg.Server.DrainTimeout.Duration)
    }
    bgWg.Add(1)
    go func() {
        defer bgWg.Done()
        orch.RunScheduled(bgCtx, cfg.Update.Schedule, cfg.Update.Channel,
            drainFn, undrainFn, finishFn)
    }()
}
```

In `internal/orchestrator/scheduled.go`:

```go
// RunScheduled checks for updates on the configured cron schedule.
// When an update is available, it triggers the full update + watchdog
// flow. Blocks until ctx is cancelled.
//
// The drain/undrain/finish functions are closures from main.go —
// same ones passed to the API handlers.
func (o *Orchestrator) RunScheduled(
    ctx context.Context,
    schedule string,
    channel string,
    drainFn func(),
    undrainFn func(),
    finishFn func(),
) {
    if channel == "" {
        channel = "stable"
    }

    parser := cron.NewParser(
        cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
    sched, err := parser.Parse(schedule)
    if err != nil {
        slog.Error("update scheduler: invalid cron expression",
            "schedule", schedule, "error", err)
        return
    }

    slog.Info("update scheduler started",
        "schedule", schedule, "channel", channel)

    for {
        next := sched.Next(time.Now())
        select {
        case <-ctx.Done():
            return
        case <-time.After(time.Until(next)):
        }

        slog.Info("update scheduler: checking for updates")
        result, err := o.update.CheckLatest(channel, o.version)
        if err != nil {
            slog.Warn("update scheduler: check failed", "error", err)
            continue
        }
        if !result.UpdateAvailable {
            slog.Info("update scheduler: already up to date")
            continue
        }

        slog.Info("update scheduler: starting update",
            "current", result.CurrentVersion,
            "latest", result.LatestVersion)

        sender := o.tasks.Create(uuid.New().String(), "scheduled-update")
        if err := o.Update(ctx, channel, sender, drainFn, undrainFn); err != nil {
            slog.Error("update scheduler: update failed", "error", err)
            sender.Fail(err.Error())
            continue
        }

        // Enter watchdog — same as CLI-triggered flow.
        // On success: Finish() + exit. On failure: rollback + continue loop.
        if err := o.Watchdog(ctx, ...); err != nil {
            slog.Error("update scheduler: watchdog rollback", "error", err)
            continue
        }

        finishFn()
    }
}
```

The scheduled path is identical to the CLI-triggered path except:
- No confirmation prompt (unattended).
- Progress logged to slog instead of streamed to a client.
- On watchdog failure, the server resumes and the scheduler loop
  continues (it will retry on the next cron tick).

Scheduled auto-updates are gated behind passive mode: if the server
started in passive mode, the scheduler does not run. This prevents
a newly started replacement from immediately trying to update itself.

### Step 9: Helper methods

Private methods on `Orchestrator` used by both Update and Rollback:

```go
// pullImage pulls the given image via the Docker API.
func (o *Orchestrator) pullImage(ctx context.Context, ref string) error

// startClone inspects self, clones config with new image +
// BLOCKYARD_PASSIVE=1, creates and starts the container.
// Returns the new container ID.
func (o *Orchestrator) startClone(ctx context.Context, image string) (string, error)

// waitReady polls /readyz on the new container until it returns 200.
// Returns the container's internal address (IP:port).
// Times out after WorkerStartTimeout (reuses existing config).
func (o *Orchestrator) waitReady(ctx context.Context, containerID string) (string, error)

// activate calls POST /api/v1/admin/activate on the new server.
func (o *Orchestrator) activate(ctx context.Context, addr string) error

// killAndRemove stops and removes a container. Best-effort — logs
// errors but does not return them.
func (o *Orchestrator) killAndRemove(ctx context.Context, containerID string)

// checkReady does a single /readyz check against the given address.
func (o *Orchestrator) checkReady(ctx context.Context, addr string) error

// currentImageTag reads the image tag from the running container's
// inspect result.
func (o *Orchestrator) currentImageTag() string
```

`waitReady` resolves the new container's IP address via
`ContainerInspect` (reading the network settings) and makes HTTP
requests to `http://{ip}:{port}/readyz`. The port is known from the
current container's config (same port, cloned configuration).

### Step 10: Tests

#### Orchestrator unit tests

In `internal/orchestrator/orchestrator_test.go`, using a mock Docker
client:

```go
type mockDocker struct {
    inspectFn    func(ctx context.Context, id string) (client.ContainerInspectResult, error)
    createFn     func(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error)
    startFn      func(ctx context.Context, id string) error
    stopFn       func(ctx context.Context, id string) error
    removeFn     func(ctx context.Context, id string) error
    pullFn       func(ctx context.Context, ref string) error
    // ... matches dockerClient interface
}
```

**Test cases:**

| Test | What it verifies |
|------|------------------|
| `TestUpdateHappyPath` | Full sequence: pull, backup, clone, wait, drain, activate. Verify drain called after ready, activate called after drain |
| `TestUpdateAlreadyCurrent` | No-op when version matches |
| `TestUpdatePullFails` | Abort before backup, no containers created |
| `TestUpdateCloneFails` | Abort after backup, no drain |
| `TestUpdateReadyTimeout` | New container killed and removed |
| `TestUpdateConcurrencyGuard` | Second call returns 409 while first is running |
| `TestWatchdogHealthy` | Exits after watch period |
| `TestWatchdogUnhealthy` | Kills new container, calls undrain |
| `TestRollbackHappyPath` | Read meta, pull old, down migrations, clone, drain, activate |
| `TestRollbackNoBackup` | Returns ErrNoBackup |
| `TestRollbackIrreversible` | Aborts with manual restore instructions |
| `TestCloneConfig` | Cloned config has new image, BLOCKYARD_PASSIVE=1, same volumes/networks/labels |
| `TestScheduledUpdate` | Cron fires, triggers update |
| `TestScheduledSkipsPassive` | No scheduling when server started in passive mode |

#### API endpoint tests

In `internal/api/admin_test.go`:

```go
func TestAdminUpdateRequiresAdmin(t *testing.T)   // 403 for non-admin
func TestAdminUpdateReturnsTaskID(t *testing.T)    // 202 with task_id
func TestAdminUpdateConflict(t *testing.T)         // 409 when already running
func TestAdminRollbackRequiresAdmin(t *testing.T)  // 403 for non-admin
func TestAdminStatusIdle(t *testing.T)             // returns idle state
func TestAdminUpdateNativeMode(t *testing.T)       // 501 when orchestrator is nil
```

#### Backup metadata tests

In `internal/db/backup_test.go`:

```go
func TestBackupWithMeta(t *testing.T)       // writes both backup and .meta.json
func TestLatestBackupMeta(t *testing.T)     // finds most recent by timestamp
func TestLatestBackupMetaEmpty(t *testing.T) // returns ErrNoBackup
func TestBackupMetaRoundTrip(t *testing.T)  // write → read → compare
```

---

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/config/config.go` | **update** | Add `UpdateConfig` struct and `Update *UpdateConfig` field |
| `internal/db/backup.go` | **update** | Add `BackupMeta`, `BackupWithMeta()`, `LatestBackupMeta()`, `ErrNoBackup` |
| `internal/db/backup_test.go` | **update** | Add metadata round-trip tests |
| `internal/db/migrate.go` | **update** | Add `CheckDownMigrationSafety()` and `MigrateDown()` |
| `internal/orchestrator/orchestrator.go` | **create** | `Orchestrator` struct, `dockerClient` interface, `Update()` method |
| `internal/orchestrator/rollback.go` | **create** | `Rollback()` method |
| `internal/orchestrator/watchdog.go` | **create** | `Watchdog()` method |
| `internal/orchestrator/clone.go` | **create** | `cloneConfig()`, `startClone()` helpers |
| `internal/orchestrator/helpers.go` | **create** | `pullImage()`, `waitReady()`, `activate()`, `killAndRemove()`, `checkReady()` |
| `internal/orchestrator/scheduled.go` | **create** | `RunScheduled()` for cron-triggered updates |
| `internal/orchestrator/orchestrator_test.go` | **create** | Mock Docker client, full orchestration test suite |
| `internal/api/admin.go` | **create** | `handleAdminUpdate`, `handleAdminRollback`, `handleAdminUpdateStatus` |
| `internal/api/admin_test.go` | **create** | Admin endpoint tests |
| `internal/api/router.go` | **update** | Register `/api/v1/admin/` routes |
| `cmd/blockyard/main.go` | **update** | Wire orchestrator, scheduled updates, drain/undrain closures |
| `cmd/by/admin.go` | **create** | `adminCmd()`, `adminUpdateCmd()`, `adminRollbackCmd()`, `adminStatusCmd()` |
| `cmd/by/main.go` | **update** | Register `adminCmd()` |

## Design decisions

1. **Server-side orchestration, not CLI-side.** The CLI is a remote
   HTTP client — it has no Docker socket access. The server already
   mounts the socket for worker management. The server orchestrates
   its own replacement using `ContainerInspect` → `ContainerCreate`
   on the same Docker daemon. The CLI triggers and monitors via the
   management API.

2. **Container cloning via inspect-and-recreate.** The orchestrator
   inspects its own container and clones the full configuration
   (volumes, networks, labels, env, resource limits) with only the
   image tag and `BLOCKYARD_PASSIVE=1` changed. This is the only
   reliable way to replicate arbitrary deployment configurations
   without knowing the compose file, terraform config, or whatever
   tool created the container.

3. **Watchdog mode with un-drain capability.** After draining, the
   old server stays alive to monitor the new instance. If the new
   instance fails, the old server kills it, un-drains, and resumes.
   This is safer than exiting immediately — an operator away from
   their terminal doesn't lose the running server. Relies on
   `Drain()`/`Undrain()`/`Finish()` from phase 3-4 — drain only
   sets a flag, so undrain is a trivial reversal.

4. **Backup metadata as JSON sidecar.** A `.meta.json` file written
   alongside the backup, containing the image tag and migration
   version. Rollback reads this to know what to restore. No database
   table needed — the metadata lives with the backup and is
   self-contained.

5. **Irreversible migration check blocks automated rollback.** Down
   migrations marked `-- irreversible:` (convention from phase 3-1)
   abort automated rollback with the backup path for manual restore.
   This is the right safety boundary — automated rollback should only
   handle the common case (expand-only migrations with clean reversal).

6. **`501 Not Implemented` in native mode.** When the server runs
   without a Docker socket (native mode, process backend), the admin
   update/rollback endpoints return 501. These deployments use the
   basic restart path. The alternative — embedding update logic for
   systemd services — is out of scope.

7. **Existing task polling for progress.** The update and rollback
   handlers use the existing `task.Store` and `task.Sender` to report
   progress. The CLI polls `GET /api/v1/tasks/{id}` — same pattern
   as bundle deploys. No new streaming protocol needed.

8. **Scheduled updates gated by passive mode.** A newly started
   replacement server (passive mode) does not run the update
   scheduler. This prevents the new server from immediately trying
   to update itself before the old server has exited and the watchdog
   has passed.

9. **Concurrency guard via `atomic.Bool`.** Only one update or
   rollback can run at a time. A second request returns `409
   Conflict`. This is simpler than a queue and matches the expected
   usage (infrequent admin operations, not high-throughput).

## Deferred

1. **Old container cleanup.** After the old server exits, its
   container remains in `exited` state. The new server could clean
   it up on startup (find and remove exited containers with the
   blockyard server label), but this adds complexity. For now, the
   operator or `docker system prune` handles it. Revisit if operators
   report friction.

2. **Compose file reconciliation.** After `by admin update`, the
   docker-compose file still references the old image tag. A
   subsequent `docker compose up -d` would recreate the old
   container. The operator must update the compose file's image tag
   manually. Automating this (parsing and rewriting the compose file)
   is fragile and out of scope.

3. **Multi-step rollback (N-2 and beyond).** Only N-1 rollback is
   supported — one backup metadata file, one down-migration step.
   Deeper rollback requires manual intervention with the backup files.

4. **Notification on scheduled update failure.** When a scheduled
   update fails, it logs to slog and retries on the next cron tick.
   There's no webhook, email, or alerting integration. Operators
   should monitor the server logs or the `/admin/update/status`
   endpoint.
