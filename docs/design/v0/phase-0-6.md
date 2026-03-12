# Phase 0-6: Health Polling + Orphan Cleanup + Log Capture + Graceful Shutdown

Background operational subsystems that keep the server healthy at runtime
and make debugging possible. Without these, hung processes silently swallow
traffic, server restarts leak containers, and there is no way to see what
a Shiny app printed to stdout.

## Deliverables

1. LogStore bug fixes — idempotent `MarkEnded`, thread-safe `Sender.Write`
2. `internal/ops/` package — `EvictWorker`, `StartupCleanup`,
   `GracefulShutdown`, `SpawnHealthPoller`, `SpawnLogCapture`,
   `SpawnLogRetentionCleaner`
3. Updated `GET /api/v1/apps/{id}/logs` — serve from LogStore (live and
   historical) instead of returning 501; `worker_id` is required
4. Worker IDs in app detail response — `AppResponse` gains a `workers`
   field listing active worker IDs, so callers can discover which
   `worker_id` to pass to the logs endpoint
5. Mock backend additions — `SetManagedResources()`, `SetLogLines()`
6. `main.go` wiring — startup cleanup, background goroutines, graceful
   shutdown
7. Unit tests for ops package
8. Integration tests — orphan cleanup, stale bundle recovery, graceful
   shutdown, health poller behavior, log capture and persistence
9. Docker integration tests — startup cleanup with real containers,
   metadata endpoint blocked

## What's already done

Phase 0-1 delivered:

- `logstore.Store` — in-memory per-worker log buffer with
  `Create(workerID, appID) Sender`, `Subscribe(workerID)`,
  `WorkerIDsByApp(appID)`, `MarkEnded(workerID)`, `HasActive(workerID)`,
  `CleanupExpired(retention)`, `Sender.Write(line)`
- `logstore.Store` field on `server.Server`
- `session.Store.DeleteByWorker(workerID)` — reverse lookup
- `config.ProxyConfig` with `HealthInterval` (default 15s) and
  `LogRetention` (default 1h), both with env var overrides
- `server.WorkerMap` with `All()`, `ForApp(appID)`, `CountForApp(appID)`

Phase 0-2 delivered:

- `Backend.HealthCheck(ctx, id) bool` — TCP connect with 10s timeout
  (Docker) or configurable response (mock)
- `Backend.ListManaged(ctx) ([]ManagedResource, error)` — queries Docker
  for containers + networks with `dev.blockyard/managed=true`; containers
  sorted before networks
- `Backend.RemoveResource(ctx, resource) error` — force-removes a
  container or network; ignores 404
- `Backend.Logs(ctx, id) (LogStream, error)` — follows stdout/stderr
  from a running container via Docker's log API
- Metadata endpoint protection — per-network iptables rules blocking
  `169.254.169.254`, with fallback to host reachability check, mode
  caching (`MetadataBlockMode`), orphan rule cleanup via
  `deleteIptablesRulesByComment`

Phase 0-3 delivered:

- `db.DB.FailStaleBuilds() (int64, error)` — marks bundles stuck in
  `building` as `failed`

Phase 0-4 delivers (not yet implemented):

- `GET /api/v1/apps/{id}/logs` — registered as 501 stub
- `stopAppWorkers` helper — stops all workers for an app (inline cleanup)
- `POST /api/v1/apps/{id}/start` — spawns a worker

Phase 0-5 delivers (not yet implemented):

- `ensureWorker` in `proxy/coldstart.go` — on-demand worker spawning

## Step-by-step

### Step 1: LogStore bug fixes

Two bugs in the existing `internal/logstore/store.go` need fixing before
phase 0-6 can safely use the store from concurrent goroutines.

**1a. `MarkEnded` panics on double close.**

`MarkEnded` unconditionally calls `close(e.ch)`. If called twice (once
from `evictWorker`, once from the log capture goroutine when the stream
ends), the second call panics. The design doc specifies idempotent
behavior.

Fix: add an `ended bool` field to `logEntry`. Guard `close(e.ch)` behind
a check. Use a per-entry mutex (not the store-level RWMutex) to protect
the flag and channel close atomically.

```go
func (s *Store) MarkEnded(workerID string) {
    s.mu.RLock()
    e, ok := s.entries[workerID]
    s.mu.RUnlock()
    if !ok {
        return
    }
    e.mu.Lock()
    defer e.mu.Unlock()
    if e.ended {
        return
    }
    e.ended = true
    e.endedAt = time.Now()
    close(e.ch)
}
```

**1b. `Sender.Write` races with `Subscribe`.**

`Sender.Write` appends to `e.buffer` without any lock. `Subscribe` reads
`e.buffer` under the store-level `RLock`. Since the store lock doesn't
protect buffer mutations from the capture goroutine, this is a data race.

Fix: add a `sync.Mutex` per `logEntry` to protect buffer writes and
reads. `Subscribe` acquires `e.mu` to snapshot the buffer. `Sender.Write`
acquires `e.mu` to append.

```go
type logEntry struct {
    mu      sync.Mutex // protects buffer and ended
    appID   string
    buffer  []string
    ch      chan string
    ended   bool
    endedAt time.Time
}

func (s Sender) Write(line string) {
    s.e.mu.Lock()
    if len(s.e.buffer) < maxLogLines {
        s.e.buffer = append(s.e.buffer, line)
    }
    s.e.mu.Unlock()
    select {
    case s.e.ch <- line:
    default:
    }
}
```

**Tests:**

- `TestMarkEndedIdempotent` — call `MarkEnded` twice, no panic
- `TestMarkEndedNonexistent` — call on unknown worker, no panic

### Step 2: `EvictWorker`

`internal/ops/ops.go` — new package for all background operations.

`EvictWorker` is the single codepath for decommissioning a worker. Every
place that removes a worker (health poller, `stopAppWorkers`, session
expiry, graceful shutdown) calls this instead of doing partial inline
cleanup.

```go
func EvictWorker(ctx context.Context, srv *server.Server, workerID string) {
    _, found := srv.Workers.Get(workerID)
    srv.Workers.Delete(workerID)
    if found {
        if err := srv.Backend.Stop(ctx, workerID); err != nil {
            slog.Warn("evict: failed to stop worker",
                "worker_id", workerID, "error", err)
        }
    }
    srv.Registry.Delete(workerID)
    srv.Sessions.DeleteByWorker(workerID)
    srv.LogStore.MarkEnded(workerID)
}
```

Idempotent — safe to call concurrently from multiple goroutines.
`Workers.Delete` first prevents new requests from routing to the worker
while the backend stop is in progress.

**Callers:**

- Health poller (step 4)
- `stopAppWorkers` in `api/apps.go` (phase 0-4 — replace inline cleanup)
- WS cache TTL expiry in `proxy/ws.go` (phase 0-5 — replace inline
  cleanup)
- `GracefulShutdown` (step 6)

**Tests:**

- `TestEvictWorker` — verify worker removed from all stores, backend
  stopped, log marked ended
- `TestEvictWorkerIdempotent` — call twice, no panic

### Step 3: `StartupCleanup`

Called in `main()` before binding the listener. Two jobs:

**3a. Remove orphaned containers and networks.**

Since the server just started, `srv.Workers` is empty. Every managed
resource is an orphan from a crashed or unclean previous run.

```go
func StartupCleanup(ctx context.Context, srv *server.Server) error {
    resources, err := srv.Backend.ListManaged(ctx)
    if err != nil {
        return err // backend unreachable — refuse to start
    }
    if len(resources) > 0 {
        slog.Info("startup: removing orphaned resources",
            "count", len(resources))
    }
    for _, r := range resources {
        if err := srv.Backend.RemoveResource(ctx, r); err != nil {
            slog.Warn("startup: failed to remove orphan",
                "id", r.ID, "error", err)
        }
    }

    count, err := srv.DB.FailStaleBuilds()
    if err != nil {
        return fmt.Errorf("fail stale builds: %w", err)
    }
    if count > 0 {
        slog.Info("startup: marked stale bundles as failed",
            "count", count)
    }

    return nil
}
```

`ListManaged()` returns resources sorted by `ResourceKind` (containers
first, networks second). This is important because networks cannot be
removed while containers are still connected to them.

`ListManaged()` and `FailStaleBuilds()` errors are propagated — if
Docker or the database is unreachable at startup, the server should not
start. Errors removing individual resources are logged but do not
prevent startup.

**3b. Fail stale bundles.**

If the server crashed while a dependency restore was running, the bundle
is stuck in `building` status forever. The build container was already
cleaned up in step 3a (it carries `dev.blockyard/managed=true`), but the
DB record still says `building`. The caller must re-deploy.

`FailStaleBuilds()` already exists from phase 0-3. It is idempotent —
safe to run on every startup.

**Note on iptables cleanup:** the Docker backend's
`deleteIptablesRulesByComment("blockyard-")` handles orphan iptables rule
cleanup internally — it runs during `Spawn()` and `Stop()`. A separate
cleanup step in `StartupCleanup` is not needed because orphan rules are
harmless (their networks are about to be deleted) and the next `Spawn()`
will insert fresh rules. If a future need arises, the Docker backend can
expose a `CleanupOrphanRules()` method and `StartupCleanup` can call it.

**Tests:**

- `TestStartupCleanupRemovesOrphans` — set managed resources on mock,
  run `StartupCleanup()`, verify `ListManaged()` returns empty
- `TestStartupCleanupFailsStaleBuilds` — create a bundle with `building`
  status, run `StartupCleanup()`, verify bundle status is `failed`

### Step 4: `SpawnHealthPoller`

Background goroutine that runs at `config.Proxy.HealthInterval`. Takes a
`context.Context` for cooperative shutdown.

Each cycle snapshots worker IDs from `srv.Workers.All()` (avoids holding
the map lock during health checks), then checks all workers concurrently.

A worker is evicted after `maxMisses` (2) consecutive failures, not on
the first miss. The health check already has a 10s TCP timeout, so a
single miss means 10s of unresponsiveness — but a transient blip under
heavy load shouldn't kill the worker. Two consecutive misses (20s+ of
total unresponsiveness across two poll cycles) is a strong signal.

Miss counts are tracked in a `map[string]int` local to the poller
goroutine (no synchronization needed — only the poller reads/writes it).
A successful check resets the counter. Workers that disappear from the
snapshot between cycles are removed from the map.

```go
const maxMisses = 2

func pollOnce(ctx context.Context, srv *server.Server, misses map[string]int) {
    workerIDs := srv.Workers.All()
    if len(workerIDs) == 0 {
        return
    }

    type result struct {
        workerID string
        healthy  bool
    }

    results := make(chan result, len(workerIDs))
    var wg sync.WaitGroup

    for _, wid := range workerIDs {
        wg.Add(1)
        go func(id string) {
            defer wg.Done()
            healthy := srv.Backend.HealthCheck(ctx, id)
            results <- result{workerID: id, healthy: healthy}
        }(wid)
    }

    go func() {
        wg.Wait()
        close(results)
    }()

    active := make(map[string]bool, len(workerIDs))
    for r := range results {
        active[r.workerID] = true
        if r.healthy {
            delete(misses, r.workerID)
            continue
        }
        misses[r.workerID]++
        if misses[r.workerID] >= maxMisses {
            slog.Warn("health poller: evicting unhealthy worker",
                "worker_id", r.workerID,
                "consecutive_misses", misses[r.workerID])
            EvictWorker(ctx, srv, r.workerID)
            delete(misses, r.workerID)
        }
    }

    // Prune miss counts for workers no longer in the snapshot
    for wid := range misses {
        if !active[wid] {
            delete(misses, wid)
        }
    }
}
```

The `SpawnHealthPoller` function passes the miss map into each cycle:

```go
func SpawnHealthPoller(ctx context.Context, srv *server.Server) {
    interval := srv.Config.Proxy.HealthInterval.Duration
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    misses := make(map[string]int)
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            pollOnce(ctx, srv, misses)
        }
    }
}
```

This ensures a poll cycle takes ~10s worst case (one TCP timeout)
regardless of worker count, rather than 10s × N sequentially.

`time.NewTicker` fires after the first interval, not immediately. This
gives workers time to start before being health-checked. Phase 0-5's
cold-start hold already polls health before releasing the initial
request — the health poller catches hung processes *after* the initial
startup succeeds.

No replacement spawning in v0. The roadmap mentions "(if auto-scaling is
enabled) spawn a replacement" — that's a v1 concern tied to multi-worker
load balancing.

**Tests:**

- `TestHealthPollerEvictsAfterConsecutiveMisses` — spawn worker, set
  `HealthOK` to false, spawn poller with short interval (50ms), verify
  worker survives first cycle but is evicted after second
- `TestHealthPollerResetsOnRecovery` — fail one cycle, succeed the next,
  fail one more — verify worker is not evicted (counter resets)
- `TestHealthPollerKeepsHealthyWorkers` — health stays true, verify
  worker still present after several poll cycles

### Step 5: `SpawnLogCapture`

Every codepath that spawns a worker must also start log capture. There
are two spawn sites:

- `api/apps.go` — `POST /apps/{id}/start` (explicit start, phase 0-4)
- `proxy/coldstart.go` — on-demand spawn in `ensureWorker()` (phase 0-5)

Both must call `SpawnLogCapture` after a successful spawn. Without this,
on-demand workers (the common case in v0, since users typically visit
`/app/{name}/` rather than calling the start endpoint) would have no log
capture.

```go
func SpawnLogCapture(
    ctx context.Context,
    srv *server.Server,
    workerID, appID string,
) {
    sender := srv.LogStore.Create(workerID, appID)

    go func() {
        stream, err := srv.Backend.Logs(ctx, workerID)
        if err != nil {
            slog.Warn("log capture: failed to open stream",
                "worker_id", workerID, "error", err)
            srv.LogStore.MarkEnded(workerID)
            return
        }
        defer stream.Close()

        for line := range stream.Lines {
            sender.Write(line)
        }
        srv.LogStore.MarkEnded(workerID)
    }()
}
```

The goroutine is fire-and-forget. If the backend log stream errors, the
goroutine logs a warning and marks the entry as ended.

When `stopAppWorkers()` stops a worker (via `EvictWorker`), it also calls
`LogStore.MarkEnded()`. Both the capture goroutine and `EvictWorker` may
call `MarkEnded` — the idempotency fix in step 1 makes this safe.

**Tests:**

- `TestLogCaptureStoresWorkerLogs` — configure mock with log lines,
  spawn log capture, verify lines appear in LogStore snapshot
- `TestLogCaptureMarksEndedWhenStreamCloses` — verify `HasActive`
  returns false after stream ends

### Step 6: `GracefulShutdown`

Called after the HTTP server has drained and background goroutines have
been cancelled.

```go
func GracefulShutdown(ctx context.Context, srv *server.Server) {
    workerIDs := srv.Workers.All()
    if len(workerIDs) > 0 {
        slog.Info("shutdown: stopping workers",
            "count", len(workerIDs))
    }

    var wg sync.WaitGroup
    for _, wid := range workerIDs {
        wg.Add(1)
        go func(id string) {
            defer wg.Done()
            evictCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
            defer cancel()
            EvictWorker(evictCtx, srv, id)
        }(wid)
    }
    wg.Wait()

    // Remove remaining managed resources (build containers, networks)
    resources, err := srv.Backend.ListManaged(ctx)
    if err == nil {
        for _, r := range resources {
            _ = srv.Backend.RemoveResource(ctx, r)
        }
    }

    // Fail in-progress builds
    count, err := srv.DB.FailStaleBuilds()
    if err == nil && count > 0 {
        slog.Info("shutdown: marked stale bundles as failed",
            "count", count)
    }
}
```

Worker evictions run concurrently, each with a 15s timeout. The cleanup
phase takes at most ~15s regardless of worker count. Total shutdown time
is bounded by `shutdown_timeout + ~15s`.

After a clean shutdown, the next `StartupCleanup` finds nothing to
remove. After an unclean shutdown (OOM, SIGKILL), `StartupCleanup`
handles the leftovers. Both paths converge to the same clean state.

**Tests:**

- `TestGracefulShutdownStopsAllWorkers` — start two workers, run
  `GracefulShutdown()`, verify `Workers` empty and mock backend has no
  workers
- `TestGracefulShutdownFailsInProgressBuilds` — create a bundle with
  `building` status, run `GracefulShutdown()`, verify bundle status is
  `failed`

### Step 7: `SpawnLogRetentionCleaner`

Background goroutine that periodically prunes expired log entries.

```go
func SpawnLogRetentionCleaner(
    ctx context.Context,
    srv *server.Server,
) {
    retention := srv.Config.Proxy.LogRetention.Duration
    interval := retention
    if interval > 60*time.Second || interval <= 0 {
        interval = 60 * time.Second
    }

    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            n := srv.LogStore.CleanupExpired(retention)
            if n > 0 {
                slog.Debug("log retention: cleaned up entries",
                    "count", n)
            }
        }
    }
}
```

The interval is `min(retention, 60s)` with a floor to avoid a zero/
negative ticker. `CleanupExpired` already exists from phase 0-1.

**Tests:**

- `TestLogRetentionCleaner` — set short retention, create and end a
  log entry, verify it gets cleaned up after a few ticker cycles

### Step 8: Updated logs endpoint

`GET /api/v1/apps/{id}/logs?worker_id=<required>` changes from the 501
stub (phase 0-4) to serving from the LogStore. `worker_id` is a
required query parameter — multiple workers per app can exist (e.g.,
during a redeploy or cold-start race), so the caller must specify which
worker's logs to view.

This handler lives in `internal/api/apps.go` and replaces the stub
registered in phase 0-4's router.

**Behavior:**

- `worker_id` missing: return 400.
- `LogStore.Subscribe(workerID)` fails: return 404.
- Otherwise: stream logs.

**Response format:**

- `Content-Type: text/plain`
- If worker is still running: stream buffered lines + live channel
  (chunked transfer encoding with `Flusher`, same as task logs)
- If worker has exited (within retention): return buffered lines as a
  complete response

```go
func AppLogs(srv *server.Server) http.HandlerFunc {
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

        workerID := r.URL.Query().Get("worker_id")
        if workerID == "" {
            badRequest(w, "worker_id query parameter is required")
            return
        }

        snapshot, live, ok := srv.LogStore.Subscribe(workerID)
        if !ok {
            notFound(w, "no logs for worker "+workerID)
            return
        }
        ended := srv.LogStore.IsEnded(workerID)

        w.Header().Set("Content-Type", "text/plain")
        w.Header().Set("X-Content-Type-Options", "nosniff")

        flusher, canFlush := w.(http.Flusher)

        // Write buffered lines
        for _, line := range snapshot {
            fmt.Fprintf(w, "%s\n", line)
        }
        if canFlush {
            flusher.Flush()
        }

        // If worker already exited, return buffer only
        if ended {
            return
        }

        // Stream live lines
        w.Header().Set("Transfer-Encoding", "chunked")
        ctx := r.Context()
        for {
            select {
            case <-ctx.Done():
                return
            case line, ok := <-live:
                if !ok {
                    return // stream ended
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

This is the key behavioral change: logs are now available after a worker
exits. Previously, `GET .../logs` returned 501 (or 404 in the prior
implementation) once the worker was gone.

`IsEnded` is a new trivial method on LogStore (check `e.ended` under
lock).

### Step 9: Worker IDs in app detail response

The logs endpoint requires `worker_id`, so callers need a way to
discover active worker IDs. Add a `workers` field to `AppResponse`:

```go
type AppResponse struct {
    // ... existing fields ...
    Status  string   `json:"status"`
    Workers []string `json:"workers"` // active worker IDs (empty when stopped)
}
```

The `appResponse` helper already receives the `WorkerMap`. Populate
the new field from `workers.ForApp(app.ID)`:

```go
func appResponse(app *db.AppRow, workers *server.WorkerMap) AppResponse {
    status := "stopped"
    workerIDs := workers.ForApp(app.ID)
    if len(workerIDs) > 0 {
        status = "running"
    }
    return AppResponse{
        // ... existing fields ...
        Status:  status,
        Workers: workerIDs,
    }
}
```

When the app is stopped, `Workers` is an empty slice (serializes as
`[]` in JSON, not `null`). This is consistent — `status: "running"`
always has at least one entry in `workers`.

The list endpoint (`GET /api/v1/apps`) also uses `appResponse`, so
worker IDs are included there too. This is intentional — an admin
listing apps can see at a glance which workers are active without
making per-app detail calls.

**Tests:**

- Existing integration tests for `GET /api/v1/apps/{id}` gain
  assertions on the `workers` field (empty when stopped, contains
  worker ID after start)

### Step 10: Mock backend updates

Add test helpers to `MockBackend`:

- `SetManagedResources([]ManagedResource)` — configures what
  `ListManaged()` returns. Resources persist across calls (not drained).
  `RemoveResource()` removes individual entries from the list. This
  mirrors real Docker behavior: `ListManaged()` → `RemoveResource()` →
  `ListManaged()` returns empty after cleanup.

- `SetLogLines([]string)` — configures what `Logs()` emits for any
  worker. Lines are sent to the channel and then the channel is closed
  (stream ends). For testing log capture.

```go
func (b *MockBackend) SetManagedResources(resources []ManagedResource) {
    b.mu.Lock()
    defer b.mu.Unlock()
    b.managedResources = make([]ManagedResource, len(resources))
    copy(b.managedResources, resources)
}

func (b *MockBackend) SetLogLines(lines []string) {
    b.mu.Lock()
    defer b.mu.Unlock()
    b.logLines = make([]string, len(lines))
    copy(b.logLines, lines)
}
```

`ListManaged` returns a copy of `b.managedResources`. `RemoveResource`
deletes the matching entry from the slice. `Logs` sends lines from
`b.logLines` then closes the channel.

### Step 11: `main.go` wiring

```go
func main() {
    // ... config, backend, database, server setup (unchanged) ...

    // Startup cleanup — must complete before accepting traffic.
    if err := ops.StartupCleanup(context.Background(), srv); err != nil {
        slog.Error("startup cleanup failed", "error", err)
        os.Exit(1)
    }

    // Background goroutine context
    bgCtx, bgCancel := context.WithCancel(context.Background())
    var bgWg sync.WaitGroup

    bgWg.Add(1)
    go func() {
        defer bgWg.Done()
        ops.SpawnHealthPoller(bgCtx, srv)
    }()

    bgWg.Add(1)
    go func() {
        defer bgWg.Done()
        ops.SpawnLogRetentionCleaner(bgCtx, srv)
    }()

    // ... HTTP server, signal handling (unchanged) ...

    <-ctx.Done()
    slog.Info("shutdown signal received")

    // 1. Drain HTTP server
    shutdownCtx, cancel := context.WithTimeout(context.Background(),
        cfg.Server.ShutdownTimeout.Duration)
    defer cancel()
    httpServer.Shutdown(shutdownCtx)

    // 2. Cancel background goroutines and wait
    bgCancel()
    bgWg.Wait()

    // 3. Stop all workers and clean up
    ops.GracefulShutdown(context.Background(), srv)

    slog.Info("shutdown complete")
}
```

`StartupCleanup` blocks startup and propagates errors — if Docker is
unreachable, the server won't start. Health polling and log retention run
in the background with cooperative cancellation via `context.Context`.
`GracefulShutdown` runs after background goroutines have stopped and the
HTTP server has finished draining.

### Step 12: Docker integration tests

Gated behind `docker_test` build tag. Added to
`internal/backend/docker/docker_integration_test.go`.

Phase 0-2 already tests orphan cleanup (`TestOrphanCleanup`), network
isolation (`TestNetworkIsolation`), spawn/stop lifecycle, and health
checks. New tests for phase 0-6:

- `TestMetadataEndpointBlocked` — spawn a worker, exec
  `wget --spider --timeout=2 http://169.254.169.254/` inside the
  container, verify the request is dropped (timeout/unreachable, not a
  successful response).

Full-stack Docker integration tests (startup cleanup → API → proxy →
graceful shutdown) are deferred until phases 0-4 and 0-5 are
implemented.

## Files changed

| File | Change |
|---|---|
| `internal/ops/ops.go` | New: `EvictWorker`, `StartupCleanup`, `GracefulShutdown`, `SpawnHealthPoller`, `SpawnLogCapture`, `SpawnLogRetentionCleaner` |
| `internal/ops/ops_test.go` | New: unit + integration tests for all ops functions |
| `internal/logstore/store.go` | Fix `MarkEnded` idempotency, fix `Sender.Write` thread safety, add per-entry mutex, add `IsEnded` |
| `internal/logstore/store_test.go` | Add tests for idempotency, `IsEnded` |
| `internal/backend/mock/mock.go` | Add `SetManagedResources()`, `SetLogLines()`, persistent resource list, updated `ListManaged`/`RemoveResource`/`Logs` |
| `internal/api/apps.go` | Replace `appLogs` 501 stub with LogStore-backed implementation; add `workers` field to `AppResponse` populated from `WorkerMap.ForApp()`; replace `stopAppWorkers` inline cleanup with `ops.EvictWorker` calls (phase 0-4 creates this file; phase 0-6 updates it) |
| `proxy/coldstart.go` | Add `ops.SpawnLogCapture` call after successful spawn (phase 0-5 creates this file; phase 0-6 updates it) |
| `cmd/blockyard/main.go` | Startup cleanup, background goroutines (health poller + log cleaner), graceful shutdown wiring |
| `internal/backend/docker/docker_integration_test.go` | Add `TestMetadataEndpointBlocked` |

## Exit criteria

Phase 0-6 is done when:

- `go test ./...` passes all existing + new tests
- `go test -tags docker_test ./internal/backend/docker/` passes on a
  Docker-capable runner
- `go vet ./...` is clean
- `MarkEnded` is idempotent (no panic on double call)
- `Sender.Write` is thread-safe (no data race with `Subscribe`)
- `GET .../logs` requires `worker_id` (returns 400 if missing)
- `GET /api/v1/apps/{id}` and `GET /api/v1/apps` include a `workers`
  array listing active worker IDs
- Orphan cleanup runs on startup and removes stale managed resources;
  startup fails if the backend is unreachable
- Stale `building` bundles are marked `failed` on startup
- Graceful shutdown cancels background goroutines, stops all workers,
  and removes managed resources
- Health poller detects and evicts unhealthy workers after 2 consecutive
  failed checks (including registry/session cleanup via `EvictWorker`)
- Log capture stores stdout/stderr lines in memory per worker (capped
  at 50k lines)
- `GET .../logs` returns captured logs for both running and
  recently-exited workers
- Expired log entries are cleaned up after `log_retention`
- Metadata endpoint (`169.254.169.254`) is verified blocked via Docker
  integration test
