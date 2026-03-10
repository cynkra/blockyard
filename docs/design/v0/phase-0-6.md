# Phase 0-6: Health Polling + Orphan Cleanup + Log Capture

Background operational subsystems that keep the server healthy at runtime
and make debugging possible. Without these, hung processes silently swallow
traffic, server restarts leak containers, and there is no way to see what
a Shiny app printed to stdout.

## Deliverables

1. `LogStore` — in-memory per-worker log buffer with broadcast + retention
2. `startup_cleanup()` — remove orphaned containers/networks + fail stale
   bundles on startup
3. `graceful_shutdown()` — stop all workers and build containers on SIGTERM
4. `spawn_health_poller()` — periodic background health checks on all workers
5. `spawn_log_capture()` — per-worker background log capture from backend
6. `spawn_log_retention_cleaner()` — periodic pruning of expired log entries
7. Updated `GET /api/v1/apps/{id}/logs` — serve from log store (live and
   historical) instead of directly from the backend
8. `AppState` addition — `log_store: Arc<LogStore>` field
9. Config addition — `proxy.log_retention` field
10. DB addition — `fail_stale_bundles()` query for bundles stuck in `building`
11. `main.rs` wiring — startup cleanup, graceful shutdown, background tasks
12. Mock backend additions — `set_managed_resources()`, `set_log_lines()`
    for testing
13. Integration tests — orphan cleanup, stale bundle recovery, graceful
    shutdown, health poller behavior, log capture and persistence

## What's already done

Phase 0-2 delivered the backend trait methods this phase consumes:

- `Backend::health_check(&handle) -> bool` — TCP connect with 10s timeout
  (Docker) or configurable response (mock)
- `Backend::list_managed() -> Vec<ManagedResource>` — queries Docker for
  containers + networks with `dev.blockyard/managed=true`; sorted containers
  before networks
- `Backend::remove_resource(&resource)` — force-removes a container or
  network; ignores 404
- `Backend::logs(&handle) -> LogStream` — follows stdout/stderr from a
  running container via Docker's log API

Phase 0-4 delivered:

- `GET /api/v1/apps/{id}/logs?worker_id=<optional>` — currently streams
  directly from `backend.logs()` on a live worker; returns 404 if no worker
  is running

Config already has:

- `proxy.health_interval` — default `15s`

## Step-by-step

### Step 1: LogStore

`src/ops.rs` — new module for all background operations.

`LogStore` captures stdout/stderr per worker and retains logs after exit.
Pattern mirrors `InMemoryTaskStore` from phase 0-3 (broadcast channel +
buffered snapshot for subscribe-then-snapshot deduplication).

```rust
struct LogEntry {
    app_id: String,
    buffer: Arc<Mutex<Vec<String>>>,
    tx: broadcast::Sender<String>,
    ended_at: Option<Instant>,
}

pub struct LogStore {
    entries: DashMap<String, LogEntry>,  // keyed by worker_id
}

pub struct LogSubscription {
    pub lines: Vec<String>,              // buffered snapshot
    pub rx: broadcast::Receiver<String>, // live lines
    pub ended: bool,                     // true if stream ended
}

pub struct LogSender {
    worker_id: String,
    tx: broadcast::Sender<String>,
    buffer: Arc<Mutex<Vec<String>>>,
}
```

**LogStore methods:**

- `create(worker_id, app_id) -> LogSender` — insert entry, return sender
- `subscribe(worker_id) -> Option<LogSubscription>` — subscribe-then-
  snapshot: subscribe to broadcast first, then snapshot buffer, so no lines
  are missed. Caller skips `buffer.len()` items from the receiver to
  deduplicate (same pattern as `InMemoryTaskStore::subscribe`)
- `subscribe_by_app(app_id) -> Option<(worker_id, LogSubscription)>` — find
  a worker for the app; prefer a live (not ended) worker over an ended one
- `mark_ended(worker_id)` — set `ended_at = Some(Instant::now())`
- `cleanup_expired(retention: Duration)` — remove entries whose `ended_at`
  is older than `retention`
- `has_active(worker_id) -> bool` — true if entry exists and not ended

**LogSender:**

- `send(line: String)` — append to buffer + broadcast

**Tests:**

- Create + subscribe returns buffered lines
- `mark_ended` sets the ended flag
- `subscribe_by_app` prefers live workers over ended ones
- `cleanup_expired` with zero retention removes ended entries
- Subscribe to nonexistent worker returns `None`
- `has_active` reflects ended state

### Step 2: AppState addition

Add `log_store: Arc<LogStore>` to `AppState`. Initialize in
`AppState::new()`.

### Step 3: Config addition

Add `proxy.log_retention` — how long to keep log entries after a worker
exits.

```rust
// In ProxyConfig:
#[serde(default = "default_log_retention", with = "humantime_serde")]
pub log_retention: Duration,

fn default_log_retention() -> Duration {
    Duration::from_secs(3600) // 1 hour
}
```

Env var: `BLOCKYARD_PROXY_LOG_RETENTION`. Add to `supported_env_vars()` and
`apply_env_overrides()`. The existing `env_var_coverage_complete` test
enforces this.

### Step 4: Orphan cleanup + stale bundle recovery

`ops::startup_cleanup(state)` — called in `main()` before binding the
listener. Two jobs:

**4a. Remove orphaned containers and networks.**

Since the server just started, `state.workers` is empty. Every managed
resource is an orphan from a crashed or unclean previous run.

```rust
pub async fn startup_cleanup<B: Backend>(state: &AppState<B>) {
    // 1. Remove orphaned containers and networks
    let resources = state.backend.list_managed().await;
    // Log count, iterate, remove each. Containers before networks
    // (already sorted by list_managed). Log individual failures but
    // don't abort — partial cleanup is better than none.

    // 2. Fail stale bundles
    db::sqlite::fail_stale_bundles(&state.db).await;
}
```

`list_managed()` returns resources sorted by `ResourceKind` (containers
first, networks second). This is important because networks cannot be
removed while containers are still connected to them.

Errors removing individual resources are logged but do not prevent the
server from starting.

**4b. Fail stale bundles.**

If the server crashed while a dependency restore was running, the bundle
is stuck in `building` status forever. The build container was already
cleaned up in step 4a (it carries `dev.blockyard/managed=true`), but the
DB record still says `building`. The caller must re-deploy.

New query in `db/sqlite.rs`:

```rust
pub async fn fail_stale_bundles(pool: &SqlitePool) -> Result<u64, sqlx::Error> {
    let result = sqlx::query(
        "UPDATE bundles SET status = 'failed' WHERE status = 'building'"
    )
    .execute(pool)
    .await?;
    Ok(result.rows_affected())
}
```

Log the count if any were failed. This is idempotent — safe to run on
every startup regardless of whether the previous shutdown was clean.

### Step 5: Health polling

`ops::spawn_health_poller(state) -> JoinHandle<()>` — spawns a tokio task
that runs at `config.proxy.health_interval`.

Each cycle:

1. Snapshot worker IDs from `state.workers` (avoids holding the DashMap
   during async health checks).
2. For each worker, call `backend.health_check(&handle)`.
3. On failure: remove from `state.workers`, call `backend.stop(&handle)`,
   mark log capture as ended via `log_store.mark_ended(worker_id)`.

The first tick is skipped so workers have time to start before being
health-checked. Phase 0-5's cold-start hold already polls health before
releasing the initial request — the health poller catches hung processes
*after* the initial startup succeeds.

No replacement spawning in v0. The roadmap mentions "(if auto-scaling is
enabled) spawn a replacement" — that's a v1 concern tied to multi-worker
load balancing.

### Step 6: Graceful shutdown

The current `main.rs` calls `shutdown_signal()` (waits for `ctrl_c`) but
does not stop any containers. The roadmap specifies that on SIGTERM the
server should stop all managed containers and networks, and mark in-progress
builds as `failed`.

`ops::graceful_shutdown(state)` — called after `axum::serve` returns
(i.e., after the HTTP server has drained in-flight requests).

```rust
pub async fn graceful_shutdown<B: Backend>(state: &AppState<B>) {
    // 1. Stop all tracked workers
    let worker_ids: Vec<String> = state.workers
        .iter()
        .map(|e| e.key().clone())
        .collect();

    for worker_id in &worker_ids {
        if let Some((_, worker)) = state.workers.remove(worker_id) {
            if let Err(e) = state.backend.stop(&worker.handle).await {
                tracing::warn!(worker_id, error = %e, "shutdown: failed to stop worker");
            }
        }
    }

    // 2. Remove any remaining managed resources (build containers, networks)
    //    that weren't tracked in state.workers
    if let Ok(resources) = state.backend.list_managed().await {
        for resource in &resources {
            let _ = state.backend.remove_resource(resource).await;
        }
    }

    // 3. Fail any in-progress builds
    if let Ok(count) = db::sqlite::fail_stale_bundles(&state.db).await {
        if count > 0 {
            tracing::info!(count, "shutdown: marked stale bundles as failed");
        }
    }
}
```

The shutdown sequence in `main.rs` becomes:

```rust
axum::serve(listener, app)
    .with_graceful_shutdown(shutdown_signal())
    .await?;

// HTTP server has stopped — clean up containers
ops::graceful_shutdown(&state).await;
```

`shutdown_timeout` (default 30s) applies to the axum drain period. The
subsequent container cleanup is not time-bounded — it runs to completion.
In practice, stopping a Docker container takes at most 10s (the stop
timeout in `DockerBackend::stop`), so the total shutdown time is bounded
by `shutdown_timeout + 10s × worker_count`.

After a clean shutdown, the next `startup_cleanup` finds nothing to remove.
After an unclean shutdown (OOM, SIGKILL), `startup_cleanup` handles the
leftovers. Both paths converge to the same clean state.

### Step 7: Log capture

When `POST /apps/{id}/start` spawns a worker (in `api/apps.rs`), also call
`ops::spawn_log_capture_for_app(state, worker_id, app_id, handle)`.

This spawns a background tokio task that:

1. Creates a log store entry via `log_store.create(worker_id, app_id)`.
2. Calls `backend.logs(&handle)` to get a `LogStream`.
3. Drains the stream, calling `log_sender.send(line)` for each line.
4. When the stream ends (container exited), calls
   `log_store.mark_ended(worker_id)`.

The task is fire-and-forget — its `JoinHandle` is not tracked. If the
backend log stream errors, the task logs a warning and marks the entry
as ended.

When `stop_app_workers()` stops a worker, it also calls
`log_store.mark_ended(worker_id)`.

### Step 8: Log retention cleanup

`ops::spawn_log_retention_cleaner(state, retention) -> JoinHandle<()>` —
runs periodically at `min(retention, 60s)` and calls
`log_store.cleanup_expired(retention)`.

### Step 9: Updated logs endpoint

`GET /api/v1/apps/{id}/logs?worker_id=<optional>` changes from streaming
directly from the backend to serving from the log store.

**Behavior:**

- If `worker_id` is given: `log_store.subscribe(worker_id)`. Verify the
  worker belongs to the app if it's still in `state.workers`.
- If `worker_id` is omitted: `log_store.subscribe_by_app(app_id)`.
- If no log entry exists: return 404.

**Response format (unchanged):**

- `Content-Type: text/plain`
- If worker is still running: stream buffered lines + live broadcast
  (chunked transfer encoding, same as current behavior)
- If worker has exited (within retention): return buffered lines as a
  complete response (no chunked encoding needed)

This is the key behavioral change: logs are now available after a worker
exits. Previously, `GET .../logs` returned 404 once the worker was gone.

### Step 10: main.rs wiring

```rust
// After building state, before binding listener:
ops::startup_cleanup(&state).await;

// Spawn background tasks:
let _health_poller = ops::spawn_health_poller(state.clone());
let _log_cleaner = ops::spawn_log_retention_cleaner(
    state.clone(),
    config.proxy.log_retention,
);

// ...

axum::serve(listener, app)
    .with_graceful_shutdown(shutdown_signal())
    .await?;

// HTTP server has stopped — clean up containers
ops::graceful_shutdown(&state).await;
```

Orphan cleanup blocks startup. Health polling and log retention run in the
background. Graceful shutdown runs after the HTTP server finishes draining.

### Step 11: Mock backend updates

Add test helpers to `MockBackend`:

- `set_managed_resources(Vec<ManagedResource>)` — configures what
  `list_managed()` returns (consumed on call, subsequent calls return
  empty). For testing orphan cleanup.
- `set_log_lines(Vec<String>)` — configures what `logs()` emits for any
  worker. Lines are sent and then the channel is dropped (stream ends).
  For testing log capture.

Update `list_managed()` to drain from the configured resources and
`logs()` to emit configured lines instead of returning an empty channel.

### Step 12: Integration tests

Added to `tests/bundle_test.rs`:

**Orphan cleanup + stale bundles:**

- `startup_cleanup_removes_orphans` — set managed resources on mock, run
  `startup_cleanup()`, verify `list_managed()` returns empty afterward.
- `startup_cleanup_fails_stale_bundles` — create a bundle with `building`
  status in the DB, run `startup_cleanup()`, verify bundle status is now
  `failed`.

**Graceful shutdown:**

- `graceful_shutdown_stops_all_workers` — start two apps, run
  `graceful_shutdown()`, verify `state.workers` is empty and
  `list_managed()` returns empty.
- `graceful_shutdown_fails_in_progress_builds` — create a bundle with
  `building` status, run `graceful_shutdown()`, verify bundle status is
  `failed`.

**Health polling:**

- `health_poller_removes_unhealthy_workers` — start app, spawn health
  poller with short interval (50ms), set health response to false, wait,
  verify worker removed from `state.workers`.
- `health_poller_keeps_healthy_workers` — same setup but health stays true;
  verify worker is still present after several poll cycles.

**Log capture:**

- `log_capture_stores_worker_logs` — configure mock with log lines, start
  app, wait for capture, `GET .../logs`, verify lines appear in response.
- `logs_persist_after_worker_stops` — start app, wait for capture, stop
  app, `GET .../logs` still returns the captured lines (previously would
  have returned 404).
- `logs_unavailable_returns_404` — create app but never start it, `GET
  .../logs` returns 404.

## Config summary

New field:

| Section | Field           | Default | Env var                          | Description                              |
|---------|-----------------|---------|----------------------------------|------------------------------------------|
| `proxy` | `log_retention` | `1h`    | `BLOCKYARD_PROXY_LOG_RETENTION`  | How long to keep logs after worker exits |

Existing fields consumed:

| Section | Field              | Default | Description                    |
|---------|--------------------|---------|--------------------------------|
| `proxy` | `health_interval`  | `15s`   | Health poll cycle interval     |

## Files changed

| File                      | Change                                                    |
|---------------------------|-----------------------------------------------------------|
| `src/ops.rs`              | New module: LogStore, startup_cleanup, graceful_shutdown, health poller, log capture, retention cleaner |
| `src/lib.rs`              | Add `pub mod ops`                                         |
| `src/app.rs`              | Add `log_store: Arc<LogStore>` to `AppState`              |
| `src/config.rs`           | Add `log_retention` to `ProxyConfig` + env var + default  |
| `src/db/sqlite.rs`        | Add `fail_stale_bundles()` query                          |
| `src/main.rs`             | Call `startup_cleanup`, spawn background tasks, call `graceful_shutdown` after serve |
| `src/api/apps.rs`         | `start_app`: spawn log capture; `stop_app_workers`: mark ended; `app_logs`: serve from log store |
| `src/backend/mock.rs`     | Add `set_managed_resources()`, `set_log_lines()`          |
| `tests/bundle_test.rs`    | 9 new integration tests                                   |

## Reminders

- **Network isolation test:** Spawn two workers, verify they cannot reach
  each other. Deferred from phase 0-2 — this is the natural place since
  multi-worker scenarios are already exercised here.

- **Native mode E2E test:** Phase 0-2 unit-tests the cgroup/hostname
  parsing for server container ID detection, but there is no E2E test for
  the native-mode path (server running outside Docker, no network joining).
  Add an integration test here or document as a manual verification step.

## Exit criteria

Phase 0-6 is done when:

- `cargo test --features test-support` passes all existing + new tests
- `cargo clippy --features test-support` is warning-free
- Orphan cleanup runs on startup and removes stale managed resources
- Stale `building` bundles are marked `failed` on startup
- Graceful shutdown stops all workers and removes managed resources
- Health poller detects and evicts unhealthy workers within one poll cycle
- Log capture stores stdout/stderr lines in memory per worker
- `GET .../logs` returns captured logs for both running and recently-exited
  workers
- Expired log entries are cleaned up after `log_retention`
- The design doc reminders (network isolation test, native mode E2E) are
  addressed or explicitly deferred with justification
