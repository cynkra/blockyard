# Phase 1-4: Session Sharing + Load Balancing + Auto-scaling

Unlock multi-worker operation. v0's proxy model is strict: one session =
one worker, one worker = one session (`max_sessions_per_worker = 1`,
enforced by a validation rejection in the update endpoint). This phase
changes the proxy's core routing model to "sessions distributed across a
worker pool" — enabling shared workers, demand-based scaling, and graceful
drain on stop.

This phase depends on phase 1-2 (RBAC — per-app worker limits are only
meaningful when multiple users access the same app, and the `owner` field
is needed for per-app config).

## Design decision: least-loaded assignment

New sessions are assigned to the worker with the fewest active sessions
(least-loaded). Ties are broken arbitrarily (first match in map iteration).

**Why least-loaded instead of round-robin or hash-based?**

- **Round-robin** distributes evenly but ignores current load. If one
  worker has sessions that all disconnected (pending WS cache TTL expiry),
  round-robin would still send new sessions there.
- **Hash-based** (e.g., hash of user ID mod worker count) gives consistent
  assignment but can't respond to uneven load. If one user drives 80% of
  traffic, their worker is overloaded while others idle.
- **Least-loaded** naturally balances: when workers fill up, new sessions
  go to the one with the most headroom. When a worker's sessions end, it
  becomes the preferred target for new sessions.

**Trade-off:** least-loaded requires a session count lookup per assignment
(a scan of the session store). With `max_workers = 100` and
`max_sessions_per_worker` in the low hundreds, this is a trivial
operation.

## Design decision: sticky sessions after assignment

Once a session is assigned to a worker, it stays pinned for the lifetime
of the session. The load balancer only runs on new session creation — it
does not redistribute existing sessions.

This is unchanged from v0's session-pinning model. Shiny apps are
stateful: the R process holds in-memory state per session (reactive
values, uploaded files, plot caches). Moving a session to a different
worker would lose all of that state.

**Consequence:** load imbalance can develop over time if some sessions are
long-lived and others are short. The auto-scaler handles this: idle
workers (zero sessions) are eventually removed, and new workers are
spawned when all existing ones are at capacity.

## Design decision: auto-scaling strategy

**Scale up:** when all workers for an app are at capacity
(`sessions >= max_sessions_per_worker`) and a new session arrives, the
proxy spawns a new worker (if `max_workers_per_app` allows). This is
reactive — scaling happens on demand at request time, not proactively.

**Scale down:** a background goroutine (running alongside the health
poller) checks for idle workers — workers with zero sessions while other
workers for the same app have capacity. Idle workers are evicted after a
cooldown period (default: 5 minutes) to avoid thrashing.

**Scale to zero** is not supported in v1. If an app is running, it always
has at least one worker. The operator must explicitly `POST /apps/{id}/stop`
to remove all workers. Scale-to-zero (stop the last worker after a period
of inactivity) is deferred to v2.

**Why reactive scale-up instead of predictive?**

- Predictive scaling requires workload modeling (request rate, session
  duration distribution, time-of-day patterns). Too complex for v1.
- Reactive scaling is simple and correct: you spawn a worker when you
  need one. The cold-start hold mechanism (v0) already handles the
  latency — the first request to a new worker blocks until it's healthy.
- For Shiny apps, session creation rate is typically low (users navigate
  to the app, not robots sending thousands of requests). The cold-start
  penalty is paid once per session.

## Design decision: graceful drain on stop

v0 kills workers immediately on `POST /apps/{id}/stop`. v1 introduces
graceful drain: the stop endpoint marks the app as draining (no new
sessions routed), waits for existing sessions to end (up to
`shutdown_timeout`), then force-stops remaining workers.

**Why?**

- Abrupt worker termination drops active Shiny sessions without warning.
  Users lose in-progress work (form inputs, unsaved state).
- Graceful drain lets existing sessions complete naturally. Most Shiny
  sessions are short; the timeout bounds the wait.

**Trade-off:** the stop endpoint becomes asynchronous — it returns 202
with a task ID immediately and drains in the background. The caller polls
`GET /tasks/{taskID}` to track drain progress and see when all workers
have stopped. This reuses the existing task store infrastructure (same
pattern as bundle uploads) and avoids blocking the HTTP response for up
to `shutdown_timeout`. The task log captures drain events (sessions
remaining, timeout reached, workers evicted) for operator visibility.

## Design decision: draining state in WorkerMap, not in the database

The "draining" flag is tracked in memory on the `ActiveWorker` struct,
not in the database. This is consistent with v0's design: app runtime
status (running/stopped) is derived from in-memory state, not persisted.

If the server crashes during drain, all workers are orphaned and cleaned
up by `StartupCleanup` on the next start. No stale "draining" state to
worry about.

## Deliverables

1. Remove the `max_sessions_per_worker = 1` validation rejection — allow
   values > 1
2. Load balancer — least-loaded worker assignment for new sessions
3. Updated `ensureWorker` — use load balancer instead of always spawning
4. Per-app worker limit enforcement (`max_workers_per_app`)
5. Auto-scaler background goroutine — scale-down of idle workers
6. Graceful drain on `POST /apps/{id}/stop` — returns 202 with task ID,
   drains in background, streams progress via task log
7. Draining state on `ActiveWorker`
8. New proxy config: `idle_worker_timeout` (default: 5m)
9. Updated `WorkerMap` with session-aware methods

## Step-by-step

### Step 1: Remove max_sessions_per_worker validation lock

In `internal/api/apps.go`, the update handler rejects
`max_sessions_per_worker != 1`:

```go
// v0: max_sessions_per_worker is locked to 1
if body.MaxSessionsPerWorker != nil && *body.MaxSessionsPerWorker != 1 {
    badRequest(w, "max_sessions_per_worker must be 1 in this version")
    return
}
```

Remove this block. Replace with a minimum-value check:

```go
if body.MaxSessionsPerWorker != nil && *body.MaxSessionsPerWorker < 1 {
    badRequest(w, "max_sessions_per_worker must be at least 1")
    return
}
```

**Tests:**

- Update `max_sessions_per_worker` to 5 — succeeds
- Update `max_sessions_per_worker` to 0 — rejected
- Update `max_sessions_per_worker` to 1 — still works (v0 compat)

### Step 2: Config addition

Add `idle_worker_timeout` to `ProxyConfig`:

```go
type ProxyConfig struct {
    WsCacheTTL         Duration `toml:"ws_cache_ttl"`
    HealthInterval     Duration `toml:"health_interval"`
    WorkerStartTimeout Duration `toml:"worker_start_timeout"`
    MaxWorkers         int      `toml:"max_workers"`
    LogRetention       Duration `toml:"log_retention"`
    IdleWorkerTimeout  Duration `toml:"idle_worker_timeout"` // new — default: 5m
}
```

Default:

```go
if c.IdleWorkerTimeout.Duration == 0 {
    c.IdleWorkerTimeout.Duration = 5 * time.Minute
}
```

**Env var:** `BLOCKYARD_PROXY_IDLE_WORKER_TIMEOUT`

**Tests:**

- Default is 5 minutes
- Env var override works
- `TestEnvVarCoverageComplete` passes

### Step 3: Extend ActiveWorker and WorkerMap

Add draining state and idle tracking to `ActiveWorker`:

```go
type ActiveWorker struct {
    AppID       string
    Draining    bool      // set by graceful drain; no new sessions routed
    IdleSince   time.Time // zero value = not idle; set when session count hits 0
}
```

Add session-aware methods to `WorkerMap`:

```go
// ForAppAvailable returns worker IDs for an app that are not draining.
func (m *WorkerMap) ForAppAvailable(appID string) []string {
    m.mu.Lock()
    defer m.mu.Unlock()
    var ids []string
    for id, w := range m.workers {
        if w.AppID == appID && !w.Draining {
            ids = append(ids, id)
        }
    }
    return ids
}

// MarkDraining sets the draining flag on all workers for an app.
// Returns the list of affected worker IDs.
func (m *WorkerMap) MarkDraining(appID string) []string {
    m.mu.Lock()
    defer m.mu.Unlock()
    var ids []string
    for id, w := range m.workers {
        if w.AppID == appID {
            w.Draining = true
            m.workers[id] = w
            ids = append(ids, id)
        }
    }
    return ids
}

// SetIdleSince marks when a worker became idle (zero sessions).
// Called when the last session for a worker is removed.
func (m *WorkerMap) SetIdleSince(workerID string, t time.Time) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if w, ok := m.workers[workerID]; ok {
        w.IdleSince = t
        m.workers[workerID] = w
    }
}

// ClearIdleSince resets the idle timer (a new session was assigned).
func (m *WorkerMap) ClearIdleSince(workerID string) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if w, ok := m.workers[workerID]; ok {
        w.IdleSince = time.Time{}
        m.workers[workerID] = w
    }
}

// IdleWorkers returns workers that have been idle longer than the
// given timeout, excluding the last worker for each app (don't scale
// to zero).
func (m *WorkerMap) IdleWorkers(timeout time.Duration) []string {
    m.mu.Lock()
    defer m.mu.Unlock()

    now := time.Now()

    // Count non-draining workers per app
    appWorkerCount := make(map[string]int)
    for _, w := range m.workers {
        if !w.Draining {
            appWorkerCount[w.AppID]++
        }
    }

    var idle []string
    for id, w := range m.workers {
        if w.IdleSince.IsZero() || w.Draining {
            continue
        }
        if now.Sub(w.IdleSince) < timeout {
            continue
        }
        // Don't remove the last worker for an app (no scale-to-zero)
        if appWorkerCount[w.AppID] <= 1 {
            continue
        }
        idle = append(idle, id)
    }
    return idle
}
```

**Tests:**

- `ForAppAvailable` excludes draining workers
- `MarkDraining` sets flag on all app workers
- `IdleWorkers` returns workers idle beyond timeout
- `IdleWorkers` does not return the last worker for an app
- `SetIdleSince` / `ClearIdleSince` round-trip

### Step 4: Load balancer

New file: `internal/proxy/loadbalancer.go`

```go
package proxy

import (
    "errors"

    "github.com/cynkra/blockyard/internal/server"
    "github.com/cynkra/blockyard/internal/session"
)

var errCapacityExhausted = errors.New("all workers at capacity and max workers reached")

// assignWorker picks a worker for a new session using least-loaded
// assignment.
//
// Returns:
//   - (workerID, nil) if a worker with available capacity was found
//   - ("", nil) if no worker has capacity but max_workers_per_app
//     allows spawning a new one (caller should spawn)
//   - ("", errCapacityExhausted) if no capacity and can't spawn
func assignWorker(
    appID string,
    workers *server.WorkerMap,
    sessions *session.Store,
    maxSessionsPerWorker int,
    maxWorkersPerApp *int,
) (string, error) {
    available := workers.ForAppAvailable(appID)

    // Find the worker with the fewest sessions that has capacity
    bestID := ""
    bestCount := maxSessionsPerWorker + 1 // sentinel above max

    for _, wid := range available {
        count := sessions.CountForWorker(wid)
        if count < maxSessionsPerWorker && count < bestCount {
            bestID = wid
            bestCount = count
        }
    }

    if bestID != "" {
        return bestID, nil
    }

    // No worker has capacity — can we spawn a new one?
    if maxWorkersPerApp != nil && len(available) >= *maxWorkersPerApp {
        return "", errCapacityExhausted
    }

    return "", nil // caller should spawn
}
```

**Tests:**

- Single worker with capacity — returns it
- Multiple workers — returns the one with fewest sessions
- All workers at capacity, can spawn — returns ("", nil)
- All workers at capacity, max reached — returns errCapacityExhausted
- No workers exist — returns ("", nil) (spawn first worker)
- Draining workers are excluded from consideration

### Step 5: Update ensureWorker to use load balancer

Rewrite `ensureWorker` in `internal/proxy/coldstart.go`:

```go
func ensureWorker(
    ctx context.Context,
    srv *server.Server,
    app *db.AppRow,
    sessionID string,
) (workerID, addr string, err error) {
    // 1. Try to assign to an existing worker via load balancer
    wid, err := assignWorker(
        app.ID,
        srv.Workers,
        srv.Sessions,
        app.MaxSessionsPerWorker,
        app.MaxWorkersPerApp,
    )
    if err != nil {
        return "", "", errMaxWorkers // reuse existing error for 503
    }

    if wid != "" {
        // Found a worker with capacity
        a, ok := srv.Registry.Get(wid)
        if ok {
            srv.Sessions.Set(sessionID, wid)
            srv.Workers.ClearIdleSince(wid)
            return wid, a, nil
        }
        // Registry miss — try re-resolve
        a, err := srv.Backend.Addr(ctx, wid)
        if err == nil {
            srv.Registry.Set(wid, a)
            srv.Sessions.Set(sessionID, wid)
            srv.Workers.ClearIdleSince(wid)
            return wid, a, nil
        }
        // Worker unreachable — evict and fall through to spawn
        slog.Warn("evicting stale worker", "worker_id", wid, "error", err)
        ops.EvictWorker(ctx, srv, wid)
    }

    // 2. Need to spawn a new worker

    // Check global worker limit
    if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
        return "", "", errMaxWorkers
    }

    // Must have an active bundle
    if app.ActiveBundle == nil {
        return "", "", errNoBundle
    }

    // Build WorkerSpec and spawn
    newWID := uuid.New().String()
    paths := bundle.NewBundlePaths(
        srv.Config.Storage.BundleServerPath, app.ID, *app.ActiveBundle,
    )
    spec := backend.WorkerSpec{
        AppID:       app.ID,
        WorkerID:    newWID,
        Image:       srv.Config.Docker.Image,
        BundlePath:  paths.Unpacked,
        LibraryPath: paths.Library,
        WorkerMount: srv.Config.Storage.BundleWorkerPath,
        ShinyPort:   srv.Config.Docker.ShinyPort,
        MemoryLimit: ptrOr(app.MemoryLimit, ""),
        CPULimit:    ptrOr(app.CPULimit, 0.0),
    }

    if err := srv.Backend.Spawn(ctx, spec); err != nil {
        return "", "", fmt.Errorf("spawn worker: %w", err)
    }

    // Resolve address and register
    a, err := srv.Backend.Addr(ctx, newWID)
    if err != nil {
        srv.Backend.Stop(ctx, newWID)
        return "", "", fmt.Errorf("resolve worker address: %w", err)
    }

    srv.Workers.Set(newWID, server.ActiveWorker{AppID: app.ID})
    srv.Registry.Set(newWID, a)

    // Start log capture before health polling
    ops.SpawnLogCapture(ctx, srv, newWID, app.ID)

    // Cold-start hold
    if err := pollHealthy(ctx, srv, newWID); err != nil {
        srv.Workers.Delete(newWID)
        srv.Registry.Delete(newWID)
        srv.Backend.Stop(context.Background(), newWID)
        return "", "", err
    }

    // Pin session to new worker
    srv.Sessions.Set(sessionID, newWID)

    slog.Info("worker ready",
        "worker_id", newWID, "app_id", app.ID, "addr", a)
    return newWID, a, nil
}
```

**Key changes from v0:**

- `ensureWorker` now receives `sessionID` so it can pin the session to
  the assigned worker inside the function.
- First tries `assignWorker` to find an existing worker with capacity.
- Only spawns a new worker if no existing worker has capacity.
- Clears `IdleSince` when assigning a session to a previously idle worker.

**Proxy handler update** — in `proxy.go`, the session pinning that was
previously done after `ensureWorker` is now done inside it:

```go
// Before (v0):
wid, a, err := ensureWorker(r.Context(), srv, app)
// ... error handling ...
srv.Sessions.Set(sessionID, workerID)

// After (v1):
wid, a, err := ensureWorker(r.Context(), srv, app, sessionID)
// ... error handling ...
// session already pinned inside ensureWorker
```

**Tests:**

- First session for an app — spawns a worker
- Second session when worker has capacity — reuses worker, no spawn
- Session when all workers full but can spawn — spawns new worker
- Session when all workers full and at max — returns errMaxWorkers (503)
- Session to draining app — draining workers excluded, spawns or fails
- Stale worker in registry — evicted, new worker spawned

### Step 6: Idle worker tracking

When a session ends (session deleted from the store), check if the worker
has become idle and set `IdleSince`.

**Sessions are deleted in three places:**

1. **WS cache TTL expiry** (`ws.go`) — already deletes the session and
   may evict the worker. Update to set `IdleSince` instead of immediate
   eviction when other workers exist for the app.

2. **`EvictWorker`** (`ops.go`) — calls `sessions.DeleteByWorker`. No
   idle tracking needed (the worker is being removed).

3. **Explicit session cleanup** — not currently implemented. In v1 with
   session sharing, we need to handle the case where a browser session
   ends (cookie expires or is cleared) but the WS wasn't active. This is
   handled by the idle worker timeout — eventually the worker is evicted
   if no sessions remain.

**Change in `ws.go`** — the WS cache TTL expiry callback:

```go
cache.Cache(sessionID, backendConn,
    srv.Config.Proxy.WsCacheTTL.Duration, func() {
        workerID, ok := srv.Sessions.Get(sessionID)
        if !ok {
            return
        }
        srv.Sessions.Delete(sessionID)

        remaining := srv.Sessions.CountForWorker(workerID)
        if remaining == 0 {
            // Worker has no sessions — mark idle instead of
            // immediately evicting. The auto-scaler will remove
            // it after idle_worker_timeout if no new sessions arrive.
            srv.Workers.SetIdleSince(workerID, time.Now())
        }
    })
```

In v0, an idle worker was evicted immediately. In v1, we set `IdleSince`
and let the auto-scaler handle it. The auto-scaler protects against
scale-to-zero (keeps at least one worker per app).

**Tests:**

- WS disconnect with other sessions remaining — `IdleSince` not set
- WS disconnect with no sessions remaining — `IdleSince` set
- New session assigned to idle worker — `IdleSince` cleared

### Step 7: Auto-scaler

New file: `internal/ops/autoscale.go`

```go
package ops

import (
    "context"
    "log/slog"
    "time"

    "github.com/cynkra/blockyard/internal/server"
)

// SpawnIdleWorkerReaper periodically checks for workers that have been
// idle beyond the configured timeout and evicts them. It never removes
// the last worker for an app (no scale-to-zero).
//
// Runs alongside SpawnHealthPoller as a separate background goroutine.
func SpawnIdleWorkerReaper(ctx context.Context, srv *server.Server) {
    timeout := srv.Config.Proxy.IdleWorkerTimeout.Duration
    // Check more frequently than the timeout to avoid delayed eviction.
    // Use health_interval as the check frequency — same cadence as
    // the health poller.
    interval := srv.Config.Proxy.HealthInterval.Duration

    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            idle := srv.Workers.IdleWorkers(timeout)
            for _, wid := range idle {
                slog.Info("auto-scaler: evicting idle worker",
                    "worker_id", wid, "idle_for", timeout)
                EvictWorker(ctx, srv, wid)
            }
        }
    }
}
```

**Wiring in `cmd/blockyard/main.go`:**

```go
go ops.SpawnHealthPoller(bgCtx, srv)
go ops.SpawnLogRetentionCleaner(bgCtx, srv)
go ops.SpawnIdleWorkerReaper(bgCtx, srv)  // new
```

**Tests:**

- Worker idle beyond timeout with other workers for same app — evicted
- Worker idle beyond timeout but is last for app — kept
- Worker not idle (has sessions) — not evicted
- Worker idle but within timeout — not evicted
- Draining workers not considered for idle eviction (they have their own
  lifecycle via graceful drain)

### Step 8: Graceful drain on stop

Rewrite `stopApp` in `internal/api/apps.go` to use graceful drain:

```go
// stopAppGraceful marks all workers as draining, waits for sessions to
// end, then evicts remaining workers.
func stopAppGraceful(ctx context.Context, srv *server.Server, appID string) int {
    // 1. Mark all workers as draining — no new sessions routed
    workerIDs := srv.Workers.MarkDraining(appID)
    if len(workerIDs) == 0 {
        return 0
    }

    slog.Info("draining app workers",
        "app_id", appID, "worker_count", len(workerIDs))

    // 2. Wait for sessions to end (up to shutdown_timeout)
    deadline := time.Now().Add(srv.Config.Server.ShutdownTimeout.Duration)
    for {
        remaining := 0
        for _, wid := range workerIDs {
            remaining += srv.Sessions.CountForWorker(wid)
        }
        if remaining == 0 {
            slog.Info("drain complete, all sessions ended",
                "app_id", appID)
            break
        }
        if time.Now().After(deadline) {
            slog.Warn("drain timeout, forcing stop",
                "app_id", appID, "remaining_sessions", remaining)
            break
        }
        time.Sleep(time.Second)
    }

    // 3. Evict all workers
    for _, wid := range workerIDs {
        ops.EvictWorker(ctx, srv, wid)
    }

    return len(workerIDs)
}
```

**Stop endpoint change** — the handler returns 202 with a task ID
immediately. The drain runs in a background goroutine, writing progress
to the task store. This is the same pattern as bundle uploads: the
caller gets a task ID and polls `GET /tasks/{taskID}` or streams
`GET /tasks/{taskID}/logs` for real-time progress.

```go
func stopApp(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        app, err := resolveApp(r, srv)
        if err != nil || app == nil {
            // ... existing error handling ...
            return
        }

        workerIDs := srv.Workers.MarkDraining(app.ID)
        if len(workerIDs) == 0 {
            writeJSON(w, http.StatusOK, map[string]any{
                "stopped_workers": 0,
            })
            return
        }

        // Create task for drain tracking
        taskID := uuid.New().String()
        sender := srv.Tasks.Create(taskID)
        sender.Write(fmt.Sprintf("draining %d workers", len(workerIDs)))

        // Drain in background
        go drainWorkers(context.Background(), srv, app.ID, workerIDs, sender)

        writeJSON(w, http.StatusAccepted, map[string]any{
            "task_id":       taskID,
            "worker_count":  len(workerIDs),
        })
    }
}
```

**`drainWorkers`** — the background drain function:

```go
func drainWorkers(
    ctx context.Context,
    srv *server.Server,
    appID string,
    workerIDs []string,
    sender task.Sender,
) {
    defer sender.Complete()

    deadline := time.Now().Add(srv.Config.Server.ShutdownTimeout.Duration)

    // Wait for sessions to end
    for {
        remaining := 0
        for _, wid := range workerIDs {
            remaining += srv.Sessions.CountForWorker(wid)
        }
        if remaining == 0 {
            sender.Write("all sessions ended")
            break
        }
        if time.Now().After(deadline) {
            sender.Write(fmt.Sprintf(
                "drain timeout reached, %d sessions remaining — forcing stop",
                remaining))
            break
        }
        time.Sleep(time.Second)
    }

    // Evict all workers
    for _, wid := range workerIDs {
        ops.EvictWorker(ctx, srv, wid)
        sender.Write(fmt.Sprintf("stopped worker %s", wid))
    }
    sender.Write(fmt.Sprintf("stopped %d workers", len(workerIDs)))
}
```

This gives operators real-time visibility into drain progress via
`GET /tasks/{taskID}/logs` — they can see how many sessions remain and
when each worker is stopped.

**If no workers are running:** the endpoint returns 200 immediately with
`stopped_workers: 0` (no task created). This keeps the simple case fast.

**GracefulShutdown also uses drain:** the server shutdown path
(`ops.GracefulShutdown`) reuses the same drain logic but without the
task store (no one is polling during shutdown). It marks all workers
draining, waits up to half the shutdown timeout for sessions to end,
then force-evicts:

```go
func GracefulShutdown(ctx context.Context, srv *server.Server) {
    workerIDs := srv.Workers.All()
    if len(workerIDs) == 0 {
        return
    }

    slog.Info("shutdown: draining workers", "count", len(workerIDs))

    // Mark all draining
    for _, wid := range workerIDs {
        w, ok := srv.Workers.Get(wid)
        if ok {
            w.Draining = true
            srv.Workers.Set(wid, w)
        }
    }

    // Wait for sessions to end (up to half of shutdown timeout)
    drainTimeout := srv.Config.Server.ShutdownTimeout.Duration / 2
    deadline := time.Now().Add(drainTimeout)
    for time.Now().Before(deadline) {
        total := 0
        for _, wid := range workerIDs {
            total += srv.Sessions.CountForWorker(wid)
        }
        if total == 0 {
            break
        }
        time.Sleep(time.Second)
    }

    // Force-evict all remaining
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

    // ... existing: remove remaining resources, fail stale builds ...
}
```

**Tests:**

- Stop app with no workers — returns 200 with `stopped_workers: 0`
- Stop app with workers, no sessions — returns 202 with task ID,
  workers evicted promptly
- Stop app with active sessions — returns 202, task log shows drain
  progress, workers evicted after sessions end
- Stop app with sessions that outlive timeout — task log shows forced
  stop, workers evicted
- Task status is `completed` after all workers are evicted
- `GET /tasks/{taskID}/logs` streams drain events in real time
- Server shutdown drains before evicting
- Draining workers reject new session assignment

### Step 9: Scale-up at request time

The current `ensureWorker` already spawns a new worker when
`assignWorker` returns `("", nil)`. No additional scale-up goroutine is
needed — scale-up is reactive, triggered by incoming requests.

However, we should handle a subtle race: two concurrent requests for the
same app may both see that all workers are at capacity and both try to
spawn. This is acceptable — both spawns will succeed (if within global
limits), and the extra worker will be reaped by the idle worker reaper
if it ends up with zero sessions. The global `max_workers` check prevents
unbounded spawning.

To avoid excessive concurrent spawns, `ensureWorker` should acquire a
per-app spawn lock:

New file: `internal/proxy/spawnlock.go`

```go
package proxy

import "sync"

// spawnLock serializes worker spawning per app to avoid thundering herd.
// Multiple concurrent requests for an at-capacity app would all try to
// spawn; the lock ensures only one spawn proceeds while others wait and
// then find the newly spawned worker via assignWorker.
type spawnLock struct {
    mu    sync.Mutex
    locks map[string]*sync.Mutex
}

func newSpawnLock() *spawnLock {
    return &spawnLock{locks: make(map[string]*sync.Mutex)}
}

func (s *spawnLock) ForApp(appID string) *sync.Mutex {
    s.mu.Lock()
    defer s.mu.Unlock()
    if _, ok := s.locks[appID]; !ok {
        s.locks[appID] = &sync.Mutex{}
    }
    return s.locks[appID]
}
```

In `ensureWorker`, before spawning:

```go
// Serialize spawns for this app
appLock := spawnLocks.ForApp(app.ID)
appLock.Lock()

// Re-check — another goroutine may have spawned while we waited
wid, err := assignWorker(app.ID, srv.Workers, srv.Sessions,
    app.MaxSessionsPerWorker, app.MaxWorkersPerApp)
if err != nil {
    appLock.Unlock()
    return "", "", errMaxWorkers
}
if wid != "" {
    appLock.Unlock()
    // ... assign to existing worker (same as above) ...
}

// Proceed with spawn...
// ... spawn, register, health check ...
appLock.Unlock()
```

The `spawnLocks` instance is created in `Handler()` alongside the
`WsCache` and `Transport`, and captured by the handler closure.

**Tests:**

- Concurrent requests to at-capacity app — only one spawn, others
  reuse the new worker
- Concurrent requests to different apps — spawns proceed in parallel
