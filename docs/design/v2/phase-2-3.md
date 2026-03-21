# Phase 2-3: Worker Lifecycle (Scale-to-Zero, Pre-Warming, Loading Page)

Three features that share idle-detection and worker-lifecycle machinery.
Built together because they interact tightly: scale-to-zero creates the
cold-start scenario, pre-warming mitigates it, and the loading page makes
it visible to users when it does occur.

## Deliverables

1. **Scale-to-zero** — remove the "keep at least one worker per app"
   constraint. When all sessions disconnect and the idle timeout expires,
   the app has zero running workers. The next request triggers a cold
   start.
2. **Pre-warming** — a new per-app `pre_warmed_seats` field (default
   `0`). When `> 0`, the autoscaler maintains a pool of standby workers
   with no assigned sessions. When a session claims a warm worker, a
   replacement is spawned immediately via an event-driven trigger (with
   the autoscaler tick as a safety net).
3. **Cold-start loading page** — when a browser request arrives for an
   app with no healthy workers, the proxy serves an HTML page with a
   spinner instead of holding the request open. The page polls a
   readiness endpoint and redirects when the worker is ready. Non-browser
   requests continue using the existing hold-until-healthy behavior.

---

## Step 1: Migration 003 — pre_warmed_seats column

Add migration files under `internal/db/migrations/`:

**`sqlite/003_v2_pre_warming.up.sql`:**

```sql
ALTER TABLE apps ADD COLUMN pre_warmed_seats INTEGER NOT NULL DEFAULT 0;
```

**`sqlite/003_v2_pre_warming.down.sql`:**

```sql
ALTER TABLE apps DROP COLUMN pre_warmed_seats;
```

**`postgres/003_v2_pre_warming.up.sql`:**

```sql
ALTER TABLE apps ADD COLUMN pre_warmed_seats INTEGER NOT NULL DEFAULT 0;
```

**`postgres/003_v2_pre_warming.down.sql`:**

```sql
ALTER TABLE apps DROP COLUMN pre_warmed_seats;
```

Same SQL on both dialects. `DEFAULT 0` means every existing app starts
with scale-to-zero behavior — no behavior change on migration for
existing deployments.

## Step 2: AppRow struct update

Add the `PreWarmedSeats` field to `AppRow` in `internal/db/db.go`:

```go
type AppRow struct {
    // ... existing fields ...
    CreatedAt            string   `db:"created_at" json:"created_at"`
    UpdatedAt            string   `db:"updated_at" json:"updated_at"`
    PreWarmedSeats       int      `db:"pre_warmed_seats" json:"pre_warmed_seats"`
}
```

The field uses `int` (not `*int`) because the column is `NOT NULL
DEFAULT 0` — every row always has a value.

## Step 3: API additions — pre_warmed_seats in UpdateApp

Add `PreWarmedSeats` to the `updateAppRequest` struct in
`internal/api/apps.go`:

```go
type updateAppRequest struct {
    // ... existing fields ...
    PreWarmedSeats       *int     `json:"pre_warmed_seats"`
}
```

Add validation in the `UpdateApp` handler, alongside the existing
`MaxSessionsPerWorker` / `MaxWorkersPerApp` checks:

```go
if body.PreWarmedSeats != nil {
    if *body.PreWarmedSeats < 0 {
        badRequest(w, "pre_warmed_seats must be non-negative")
        return
    }
    if *body.PreWarmedSeats > 10 {
        badRequest(w, "pre_warmed_seats must not exceed 10")
        return
    }
}
```

**Why cap at 10?** Pre-warmed seats are idle containers consuming
resources with no active users. 10 is a generous ceiling — most
deployments will use 0 (scale-to-zero) or 1 (single warm standby). The
cap prevents accidental resource exhaustion (e.g., `pre_warmed_seats:
100` would spin up 100 idle containers). Operators who need more can
raise this in a future config option.

Add the field to `db.UpdateApp`:

```go
// In the dynamic UPDATE builder in db.go:
if body.PreWarmedSeats != nil {
    sets = append(sets, "pre_warmed_seats = ?")
    args = append(args, *body.PreWarmedSeats)
}
```

The `AppResponse` struct already inherits the field from `AppRow` via
the `json` tag — no separate response struct change needed.

## Step 4: Scale-to-zero — remove keep-at-least-one constraint

The current `IdleWorkers()` method in `internal/server/state.go`
(line 223) explicitly keeps at least one worker per app:

```go
// Don't remove the last worker for an app (no scale-to-zero).
if appWorkerCount[w.AppID] <= 1 {
    continue
}
```

**Remove this constraint.** The new `IdleWorkers` returns all workers
idle beyond the timeout, regardless of whether they're the last for
their app:

```go
func (m *WorkerMap) IdleWorkers(timeout time.Duration) []string {
    m.mu.Lock()
    defer m.mu.Unlock()

    now := time.Now()
    var idle []string
    for id, w := range m.workers {
        if w.IdleSince.IsZero() || w.Draining {
            continue
        }
        if now.Sub(w.IdleSince) < timeout {
            continue
        }
        idle = append(idle, id)
    }
    return idle
}
```

The per-app worker count tracking is removed — it was only used for the
keep-at-least-one check. Pre-warming (step 5) replaces this with a
configurable minimum via `pre_warmed_seats`.

**Interaction with `idle_worker_timeout`:** the existing
`idle_worker_timeout` (default 5m) still controls how long an idle
worker survives before eviction. With `pre_warmed_seats = 0`, this is
the grace period before scale-to-zero. With `pre_warmed_seats > 0`, the
autoscaler replenishes the pool after eviction (see step 5), so the
timeout only affects workers that are excess to the warm pool target.

## Step 5: Pre-warming — autoscaler extension

The autoscaler gains a pre-warming check that runs after idle eviction
and health checks. A shared `ensurePreWarmed` function is called from
two sites: the autoscaler tick (baseline) and the proxy handler
(event-driven, see step 6).

### `ensurePreWarmed` function

New function in `internal/proxy/autoscaler.go`:

```go
// ensurePreWarmed spawns workers to maintain the pre-warmed pool for an
// app. Called from both the autoscaler tick and the proxy handler (when
// a warm worker is claimed). Respects per-app and global worker limits.
// Spawns are routed through spawnGroup to deduplicate against concurrent
// callers (event-driven trigger vs autoscaler tick) and the loading page
// triggerSpawn path.
func ensurePreWarmed(ctx context.Context, srv *server.Server, app *db.AppRow) {
    if app.PreWarmedSeats <= 0 {
        return
    }

    // Count non-draining workers with zero sessions (the idle pool).
    idleCount := 0
    for _, wid := range srv.Workers.ForAppAvailable(app.ID) {
        if srv.Sessions.CountForWorker(wid) == 0 {
            idleCount++
        }
    }

    deficit := app.PreWarmedSeats - idleCount
    if deficit <= 0 {
        return
    }

    // Per-app limit check.
    currentWorkers := len(srv.Workers.ForAppAvailable(app.ID))
    for i := 0; i < deficit; i++ {
        if app.MaxWorkersPerApp != nil && currentWorkers >= *app.MaxWorkersPerApp {
            slog.Debug("pre-warm: per-app limit reached",
                "app_id", app.ID, "limit", *app.MaxWorkersPerApp)
            break
        }
        if srv.Workers.Count() >= srv.Config.Proxy.MaxWorkers {
            slog.Debug("pre-warm: global limit reached",
                "app_id", app.ID, "limit", srv.Config.Proxy.MaxWorkers)
            break
        }

        slog.Info("pre-warm: spawning standby worker",
            "app_id", app.ID, "deficit", deficit-i)
        // Route through spawnGroup to deduplicate against concurrent
        // ensurePreWarmed calls and triggerSpawn. The singleflight key
        // is the app ID, so only one spawn proceeds per app at a time.
        _, _, err := spawnGroup.do(app.ID, func() (string, string, error) {
            return spawnWorker(ctx, srv, app)
        })
        if err != nil {
            slog.Warn("pre-warm: spawn failed",
                "app_id", app.ID, "error", err)
            break // don't retry on failure — autoscaler tick will catch it
        }
        currentWorkers++
    }
}
```

### Autoscaler tick integration

Add the pre-warming check to `autoscaleTick()`, after idle eviction and
health checks:

```go
func autoscaleTick(ctx context.Context, srv *server.Server) {
    // ... existing: sweep sessions, mark idle, evict idle ...

    appIDs := srv.Workers.AppIDs()

    for _, appID := range appIDs {
        // ... existing: skip draining, health checks, scale-up ...
    }

    // Pre-warming: maintain warm pools for all configured apps.
    // This runs after eviction so deficit counts are accurate.
    // Also checks apps that currently have zero workers (not in
    // appIDs above) — they may have pre_warmed_seats > 0 and need
    // workers spawned from scratch.
    preWarmApps(ctx, srv)
}
```

**`preWarmApps` function:**

```go
// preWarmApps checks all apps with pre_warmed_seats > 0 and spawns
// standby workers to maintain the target pool size. Runs on each
// autoscaler tick as a safety net for the event-driven trigger.
func preWarmApps(ctx context.Context, srv *server.Server) {
    apps, err := srv.DB.ListPreWarmedApps()
    if err != nil {
        slog.Warn("pre-warm: list apps failed", "error", err)
        return
    }
    for _, app := range apps {
        if srv.Workers.IsDraining(app.ID) {
            continue
        }
        ensurePreWarmed(ctx, srv, &app)
    }
}
```

**New DB method — `ListPreWarmedApps`:**

```go
// ListPreWarmedApps returns all non-deleted apps with pre_warmed_seats > 0.
func (db *DB) ListPreWarmedApps() ([]AppRow, error) {
    var apps []AppRow
    err := db.DB.Select(&apps,
        `SELECT * FROM apps WHERE pre_warmed_seats > 0 AND deleted_at IS NULL`)
    if err != nil {
        return nil, err
    }
    return apps, nil
}
```

Note: this query references `deleted_at` from the phase 2-2 soft-delete
migration. If phase 2-3 is implemented before phase 2-2, replace with
just `WHERE pre_warmed_seats > 0`.

### Pre-warming for zero-worker apps

The existing autoscaler only iterates apps that have active workers
(`srv.Workers.AppIDs()`). Apps that have scaled to zero are invisible to
it. The `preWarmApps` function queries the DB directly, catching apps
with `pre_warmed_seats > 0` that currently have zero workers — e.g.,
after a server restart or after the app was idle-evicted and the
pre_warmed_seats was later increased via the API.

## Step 6: Event-driven pre-warm replacement

When the proxy claims a warm (idle) worker for a new session, trigger
an immediate pre-warm replacement instead of waiting up to 15s for the
autoscaler tick.

### `ClearIdleSince` returns `bool`

Modify `ClearIdleSince` in `internal/server/state.go` to report whether
the worker was previously idle:

```go
// ClearIdleSince resets the idle timer (a new session was assigned).
// Returns true if the worker was idle before clearing.
func (m *WorkerMap) ClearIdleSince(workerID string) bool {
    m.mu.Lock()
    defer m.mu.Unlock()
    if w, ok := m.workers[workerID]; ok {
        wasIdle := !w.IdleSince.IsZero()
        w.IdleSince = time.Time{}
        m.workers[workerID] = w
        return wasIdle
    }
    return false
}
```

### Proxy handler trigger

In `internal/proxy/proxy.go`, after claiming a worker (around line 173),
use the return value to trigger a pre-warm replacement:

```go
wasIdle := srv.Workers.ClearIdleSince(workerID)
srv.Sessions.Set(sessionID, session.Entry{
    WorkerID:   workerID,
    UserSub:    callerSub,
    LastAccess: time.Now(),
})
telemetry.SessionsActive.Inc()

// Trigger pre-warm replacement if we just claimed a warm worker.
if wasIdle && app.PreWarmedSeats > 0 {
    go ensurePreWarmed(context.Background(), srv, app)
}
```

The goroutine is fire-and-forget — the request proceeds immediately.
`spawnWorker` handles all limit checks and health polling internally.
Both `ensurePreWarmed` and `triggerSpawn` route spawns through
`spawnGroup.do()`, so racing with the autoscaler tick or concurrent
requests is safe — only one spawn proceeds per app at a time.

The autoscaler's tick-based `preWarmApps` check remains as a safety net
— it catches any missed events (e.g., a worker crash reducing the idle
pool) and handles startup recovery.

## Step 7: Loading page template

New file `internal/proxy/loading.go` with an embedded HTML template.
The loading page is a proxy concern (served by the proxy handler when
no workers are available), not a UI concern — so it lives in the proxy
package with its own embed, separate from the dashboard templates in
`internal/ui/`.

### Template file

**`internal/proxy/loading.html`:**

```html
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Starting {{.AppName}} — blockyard</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI",
                         Roboto, sans-serif;
            background: #f8f9fa;
            color: #333;
            display: flex;
            align-items: center;
            justify-content: center;
            min-height: 100vh;
        }
        .loading {
            text-align: center;
            max-width: 400px;
            padding: 2rem;
        }
        .spinner {
            width: 40px;
            height: 40px;
            margin: 0 auto 1.5rem;
            border: 3px solid #e9ecef;
            border-top-color: #495057;
            border-radius: 50%;
            animation: spin 0.8s linear infinite;
        }
        @keyframes spin { to { transform: rotate(360deg); } }
        h1 { font-size: 1.25rem; font-weight: 500; margin-bottom: 0.5rem; }
        .status { color: #6c757d; font-size: 0.9rem; }
        .error { color: #dc3545; display: none; }
    </style>
</head>
<body>
    <div class="loading">
        <div class="spinner" id="spinner"></div>
        <h1>Starting <strong>{{.AppName}}</strong></h1>
        <p class="status" id="status">Waiting for the application to start&hellip;</p>
        <p class="error" id="error">
            The application failed to start. Please try again later or
            contact an administrator.
        </p>
    </div>
    <script>
        (function() {
            var readyURL = {{.ReadyURL}};
            var appURL = {{.AppURL}};
            var timeout = {{.TimeoutMs}};
            var interval = 2000;
            var elapsed = 0;

            function poll() {
                fetch(readyURL)
                    .then(function(res) { return res.json(); })
                    .then(function(data) {
                        if (data.ready) {
                            window.location.replace(appURL);
                            return;
                        }
                        elapsed += interval;
                        if (elapsed >= timeout) {
                            showError();
                            return;
                        }
                        setTimeout(poll, interval);
                    })
                    .catch(function() {
                        elapsed += interval;
                        if (elapsed >= timeout) {
                            showError();
                            return;
                        }
                        setTimeout(poll, interval);
                    });
            }

            function showError() {
                document.getElementById("spinner").style.display = "none";
                document.getElementById("status").style.display = "none";
                document.getElementById("error").style.display = "block";
            }

            setTimeout(poll, interval);
        })();
    </script>
</body>
</html>
```

The page is fully self-contained — inline CSS and JS, no external
dependencies, no asset loading. Template values are injected as JSON
strings via Go's `template.JSStr` to prevent XSS.

### Embed and render

**`internal/proxy/loading.go`:**

```go
package proxy

import (
    "embed"
    "html/template"
    "net/http"
    "strings"
    "time"

    "github.com/cynkra/blockyard/internal/db"
    "github.com/cynkra/blockyard/internal/server"
)

//go:embed loading.html
var loadingHTML string

var loadingTmpl = template.Must(template.New("loading").Parse(loadingHTML))

type loadingData struct {
    AppName   string
    ReadyURL  template.JSStr
    AppURL    template.JSStr
    TimeoutMs int64
}

// serveLoadingPage renders the cold-start loading page for browser
// requests when no healthy worker is available.
func serveLoadingPage(w http.ResponseWriter, app *db.AppRow, appName string, srv *server.Server) {
    appPath := "/app/" + appName + "/"
    readyPath := appPath + "__blockyard/ready"
    timeout := srv.Config.Proxy.WorkerStartTimeout.Duration

    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Header().Set("Cache-Control", "no-store")
    w.WriteHeader(http.StatusOK)

    // Add 10s buffer to the client-side timeout so the loading page
    // doesn't race the server-side spawn. The server's triggerSpawn
    // uses WorkerStartTimeout as its context deadline; if the spawn
    // takes nearly the full timeout, the client would otherwise show
    // an error at the same instant the worker becomes healthy.
    clientTimeout := timeout + 10*time.Second

    loadingTmpl.Execute(w, loadingData{
        AppName:   displayName(app),
        ReadyURL:  template.JSStr(readyPath),
        AppURL:    template.JSStr(appPath),
        TimeoutMs: clientTimeout.Milliseconds(),
    })
}

// displayName returns the app's title if set, otherwise its name.
func displayName(app *db.AppRow) string {
    if app.Title != nil && *app.Title != "" {
        return *app.Title
    }
    return app.Name
}

// isBrowserRequest returns true if the request's Accept header
// indicates a browser expecting HTML content.
func isBrowserRequest(r *http.Request) bool {
    accept := r.Header.Get("Accept")
    // Simple substring check — browsers always include text/html in
    // their Accept header. API clients (curl with default Accept,
    // programmatic clients) typically don't.
    return strings.Contains(accept, "text/html")
}
```

## Step 8: `/__blockyard/` prefix interception

The `/__blockyard/` path prefix is reserved for proxy-internal endpoints
and is never forwarded to worker containers. Intercept it in the proxy
handler before session resolution and worker assignment.

### Ready endpoint

**`GET /app/{name}/__blockyard/ready`**

Returns the readiness status of the app — whether at least one healthy
worker is available to serve requests.

```
→ 200 { "ready": true }    when a healthy worker exists
→ 200 { "ready": false }   when still starting or no workers
```

The endpoint runs after auth (step 1b in the proxy handler) but before
session resolution (step 2). This means:

- **Auth is enforced** — the same access check that gates the app also
  gates the ready endpoint. Unauthorized callers get 404, same as the
  app itself.
- **No session is created** — polling requests don't consume sessions
  or create session cookies.

### Implementation

Add to `internal/proxy/proxy.go`, inside the handler after the ACL
check (after line 108) and before session resolution (line 110):

```go
// Intercept /__blockyard/ internal endpoints before session routing.
if handleBlockyardInternal(w, r, app, appName, srv) {
    return
}
```

**`internal/proxy/internal.go`:**

```go
package proxy

import (
    "encoding/json"
    "net/http"
    "strings"

    "github.com/cynkra/blockyard/internal/db"
    "github.com/cynkra/blockyard/internal/server"
)

const blockyardPrefix = "/__blockyard/"

// handleBlockyardInternal handles requests under the /__blockyard/
// reserved prefix. Returns true if the request was handled (caller
// should return), false if the request should proceed to normal proxy
// routing.
func handleBlockyardInternal(
    w http.ResponseWriter,
    r *http.Request,
    app *db.AppRow,
    appName string,
    srv *server.Server,
) bool {
    // Extract the path after /app/{name}/ — chi strips the route
    // prefix, so we look for /__blockyard/ in the remaining path.
    path := r.URL.Path
    prefix := "/app/" + appName + "/"
    remainder := strings.TrimPrefix(path, prefix)

    if !strings.HasPrefix(remainder, "__blockyard/") {
        return false
    }

    endpoint := strings.TrimPrefix(remainder, "__blockyard/")

    switch endpoint {
    case "ready":
        handleReady(w, r, app, srv)
    default:
        http.NotFound(w, r)
    }
    return true
}

type readyResponse struct {
    Ready bool `json:"ready"`
}

// handleReady responds with whether the app has at least one healthy
// available (non-draining) worker.
func handleReady(
    w http.ResponseWriter,
    r *http.Request,
    app *db.AppRow,
    srv *server.Server,
) {
    ready := false
    for _, wid := range srv.Workers.ForAppAvailable(app.ID) {
        if srv.Backend.HealthCheck(r.Context(), wid) {
            ready = true
            break
        }
    }

    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Cache-Control", "no-store")
    json.NewEncoder(w).Encode(readyResponse{Ready: ready})
}
```

The health check in `handleReady` calls `srv.Backend.HealthCheck()`
directly rather than just checking worker existence. This ensures the
loading page redirects only when the worker can actually serve requests
— not when it's registered but still starting up.

## Step 9: Proxy handler modification — browser vs non-browser cold start

The core change to the proxy handler. When a new session needs a worker
and no healthy workers exist, the handler diverges based on request type:

- **Browser requests** (`Accept: text/html`): serve the loading page
  and trigger a worker spawn in the background.
- **Non-browser requests** (API calls, WebSocket upgrades): continue
  with the existing hold-until-healthy behavior.

### Modification to `proxy.go`

Replace the worker assignment block (lines 145-180) with:

```go
if workerID == "" {
    isNewSession = true
    sessionID = uuid.New().String()
    slog.Debug("proxy: creating new session",
        "app", appName, "session_id", sessionID)

    // Check if any healthy workers exist before calling ensureWorker.
    // If none exist and this is a browser request, serve the loading
    // page instead of blocking.
    if !hasAvailableWorker(srv, app.ID) && isBrowserRequest(r) {
        // Trigger spawn in the background (idempotent via singleflight).
        go triggerSpawn(srv, app)
        serveLoadingPage(w, app, appName, srv)
        return
    }

    wid, a, err := ensureWorker(r.Context(), srv, app)
    if err != nil {
        // ... existing error handling unchanged ...
        return
    }
    workerID, addr = wid, a
    wasIdle := srv.Workers.ClearIdleSince(workerID)
    srv.Sessions.Set(sessionID, session.Entry{
        WorkerID:   workerID,
        UserSub:    callerSub,
        LastAccess: time.Now(),
    })
    telemetry.SessionsActive.Inc()

    // Trigger pre-warm replacement if we just claimed a warm worker.
    if wasIdle && app.PreWarmedSeats > 0 {
        go ensurePreWarmed(context.Background(), srv, app)
    }
}
```

### Helper functions (in `internal/proxy/coldstart.go`)

```go
// hasAvailableWorker returns true if the app has at least one available
// (non-draining) worker registered in the worker map. Note: this checks
// worker existence, not health — a worker may be registered but still
// starting up. This is intentional: the loading page path is only for
// the zero-workers case (true cold start). When a worker exists but is
// still starting, the request goes through ensureWorker which blocks
// via spawnGroup.do() until the in-progress spawn completes.
func hasAvailableWorker(srv *server.Server, appID string) bool {
    return len(srv.Workers.ForAppAvailable(appID)) > 0
}

// triggerSpawn spawns a worker for the app in the background. Uses
// spawnSingleFlight to deduplicate concurrent calls. Errors are logged
// but not returned — the loading page polls for readiness.
func triggerSpawn(srv *server.Server, app *db.AppRow) {
    ctx, cancel := context.WithTimeout(
        context.Background(),
        srv.Config.Proxy.WorkerStartTimeout.Duration,
    )
    defer cancel()

    _, _, err := spawnGroup.do(app.ID, func() (string, string, error) {
        return spawnWorker(ctx, srv, app)
    })
    if err != nil {
        slog.Warn("triggerSpawn: background spawn failed",
            "app_id", app.ID, "error", err)
    }
}
```

### Loading page redirect flow

The complete flow for a browser cold start:

```
1. Browser requests GET /app/my-app/
2. Proxy: app lookup → ACL check → pass
3. Proxy: /__blockyard/ check → not internal → proceed
4. Proxy: no session cookie → new session needed
5. Proxy: hasAvailableWorker() → false, isBrowserRequest() → true
6. Proxy: go triggerSpawn() → spawns worker in background
7. Proxy: serveLoadingPage() → returns 200 with HTML
8. Browser: renders loading page, starts polling

9. Loading page: GET /app/my-app/__blockyard/ready → { "ready": false }
   (repeat every 2s)

10. Worker becomes healthy

11. Loading page: GET /app/my-app/__blockyard/ready → { "ready": true }
12. JavaScript: window.location.replace("/app/my-app/")

13. Browser requests GET /app/my-app/ (redirect)
14. Proxy: no session cookie → new session needed
15. Proxy: hasAvailableWorker() → true → ensureWorker() assigns worker
16. Proxy: session created, cookie set, request forwarded
17. User sees the Shiny app
```

### Non-browser cold start flow

API calls and programmatic clients continue with the existing behavior:

```
1. curl -H "Authorization: Bearer by_..." https://example.com/app/my-app/
2. Proxy: hasAvailableWorker() → false, isBrowserRequest() → false
3. Proxy: ensureWorker() → spawnWorker() → pollHealthy() → blocks
4. Worker becomes healthy → request forwarded
5. curl receives the response
```

WebSocket upgrades also follow the non-browser path — `isWebSocketUpgrade`
requests never include `Accept: text/html`.

## Step 10: Tests

### DB tests (in `internal/db/db_test.go`, using `eachDB`)

- `CreateApp` sets `pre_warmed_seats = 0` by default
- `UpdateApp` with `pre_warmed_seats = 2` → field updated
- `ListPreWarmedApps` returns only apps with `pre_warmed_seats > 0`
- `ListPreWarmedApps` excludes soft-deleted apps
- Migration 003 applies cleanly on both SQLite and PostgreSQL

### Unit tests

**Scale-to-zero (`internal/server/state_test.go`):**

- `IdleWorkers` with single worker for app, idle beyond timeout →
  returns the worker (scale to zero)
- `IdleWorkers` with draining workers → excluded
- `IdleWorkers` with workers idle less than timeout → excluded
- `ClearIdleSince` returns `true` when worker was idle, `false` when not

**Loading page (`internal/proxy/loading_test.go`):**

- `serveLoadingPage` renders valid HTML with correct app name
- `serveLoadingPage` sets correct `Cache-Control: no-store` header
- `isBrowserRequest` returns `true` for typical browser Accept headers
- `isBrowserRequest` returns `false` for `Accept: application/json`
- `isBrowserRequest` returns `false` for empty Accept header

**Ready endpoint (`internal/proxy/internal_test.go`):**

- `handleBlockyardInternal` returns `true` for `/__blockyard/ready`
- `handleBlockyardInternal` returns `false` for normal app paths
- `handleReady` returns `{ "ready": true }` when healthy worker exists
- `handleReady` returns `{ "ready": false }` when no workers exist
- `handleReady` returns `{ "ready": false }` when worker exists but
  unhealthy
- Unknown `/__blockyard/foo` → 404

**Pre-warming (`internal/proxy/autoscaler_test.go`):**

- `ensurePreWarmed` with deficit → spawns workers
- `ensurePreWarmed` with pool already full → no spawns
- `ensurePreWarmed` respects `max_workers_per_app` limit
- `ensurePreWarmed` respects global `max_workers` limit
- `ensurePreWarmed` with `pre_warmed_seats = 0` → no-op

### API tests (in `internal/api/api_test.go`)

- `PATCH /api/v1/apps/{id}` with `pre_warmed_seats: 1` → 200, field
  updated
- `PATCH /api/v1/apps/{id}` with `pre_warmed_seats: -1` → 400
- `PATCH /api/v1/apps/{id}` with `pre_warmed_seats: 11` → 400
- `GET /api/v1/apps/{id}` includes `pre_warmed_seats` in response

### Integration tests (mock backend)

- **Scale-to-zero:** start app → all sessions end → autoscaler tick →
  worker evicted → zero workers for app
- **Pre-warming:** set `pre_warmed_seats = 1` → autoscaler spawns
  standby worker → session claims it → replacement spawned
- **Event-driven replacement:** claim warm worker → verify
  `ensurePreWarmed` called → replacement spawned before next autoscaler
  tick
- **Loading page (browser):** request with `Accept: text/html` to app
  with no workers → 200 HTML response containing spinner and poll script
- **Hold-until-healthy (API):** request without `Accept: text/html` to
  app with no workers → blocks until worker healthy → 200
- **Ready endpoint:** poll `/__blockyard/ready` during cold start →
  `false` → worker healthy → `true`
- **Loading page timeout:** mock backend `Spawn` returns error →
  loading page shows error after timeout
- **Pre-warming + global limit:** `max_workers` reached → pre-warming
  logged but does not exceed limit
- **Pre-warming after restart:** server restart with `pre_warmed_seats > 0`
  → autoscaler tick spawns warm workers from scratch

---

## Design decisions

1. **Scale-to-zero is the default.** `pre_warmed_seats` defaults to `0`,
   meaning all apps scale to zero when idle. This matches the current
   `idle_worker_timeout` behavior with the keep-at-least-one constraint
   removed. Operators opt into warm pools per app. The alternative —
   defaulting to 1 warm seat — would silently increase resource usage
   for all existing deployments on upgrade.

2. **Event-driven replacement with tick-based safety net.** When a warm
   worker is claimed, the proxy triggers `ensurePreWarmed` immediately
   in a goroutine. The autoscaler tick still runs the pre-warming check
   every 15s as a safety net. This gives responsive replacement (< 1s
   gap) without relying solely on the event path — the autoscaler
   catches missed events, worker crashes, and startup recovery. The
   added complexity is minimal: `ClearIdleSince` returns a `bool`,
   and the proxy adds a 3-line goroutine trigger. Both paths route
   spawns through `spawnGroup.do()` so concurrent callers (event-driven
   trigger racing the autoscaler tick) are deduplicated — only one spawn
   proceeds per app at a time.

3. **Loading page for browsers only.** API clients (`curl`, scripts,
   CI/CD) and WebSocket connections continue with the hold-until-healthy
   pattern. Serving HTML to a programmatic client would break their
   response parsing. The `Accept: text/html` check is the standard way
   to distinguish browser requests from API calls. The check is
   intentionally simple — substring match, not full content negotiation.

4. **Loading page in the proxy package, not `ui/`.** The loading page
   is a proxy concern — it's served by the proxy handler when no workers
   are available. The `ui/` package handles the dashboard and landing
   page (application-level UI). The loading page is a standalone HTML
   file with inline CSS/JS — no shared base template, no static asset
   dependencies. This separation means the proxy package has no
   dependency on `ui/`.

5. **`/__blockyard/` interception before session routing.** The ready
   endpoint runs after auth but before session resolution. This means
   polling requests don't create sessions or set cookies, and auth is
   enforced (no information leak about app readiness to unauthorized
   callers). The `handleBlockyardInternal` function returns a `bool` so
   the proxy handler can cleanly short-circuit.

6. **Ready endpoint calls `HealthCheck`, not just worker existence.**
   A worker can be registered in the `WorkerMap` but not yet healthy
   (still starting up). The ready endpoint calls
   `srv.Backend.HealthCheck()` to verify the worker can actually serve
   requests. This prevents the loading page from redirecting to a
   worker that would immediately 502. The cost is one health check call
   per poll (every 2s) — negligible.

7. **Pre-warming queries the DB, not just the worker map.** The
   autoscaler's `preWarmApps` function queries `ListPreWarmedApps()`
   from the DB rather than iterating `srv.Workers.AppIDs()`. This is
   necessary because scaled-to-zero apps have no entries in the worker
   map — they're invisible to `AppIDs()`. The DB query catches apps
   that should have warm workers but don't (after scale-to-zero,
   server restart, or a newly configured `pre_warmed_seats`).

8. **`triggerSpawn` runs outside request context.** The background spawn
   triggered by the loading page path uses `context.Background()` (with
   a `WorkerStartTimeout` deadline), not the request context. If the
   user closes the loading page tab, the spawn continues — another user
   may arrive shortly, and the pre-warming pool should be maintained.

9. **Loading page is not customizable in v2.** The template is embedded
   and hardcoded. Branding and customization are v3 concerns. The page
   is intentionally minimal — a spinner, the app name, and a timeout
   error. No logos, no configuration, no operator-defined messaging.

10. **No server-wide `pre_warmed_seats` config default.** The v2 plan
    includes a `[proxy] pre_warmed_seats` config field as a default for
    new apps. This design doc omits it — `pre_warmed_seats` is per-app
    only, set via the API, defaulting to 0. Rationale: pre-warming is
    a per-app decision that depends on the app's usage pattern and cost
    tolerance. A server-wide default would either be 0 (no effect) or
    silently increase resource usage for every app. Operators who want
    pre-warming on all apps can script it via the API. If a server-wide
    default proves necessary, it can be added later without schema
    changes — just a config field that `CreateApp` reads as the initial
    column value.

11. **Pre-warmed seats cap at 10.** The API rejects values above 10.
    Each warm seat is an idle container consuming CPU and memory with no
    active users. 10 standby workers per app is generous for any
    reasonable workload. The cap prevents accidental resource exhaustion
    from a typo or misconfiguration. If operators need more, this can
    become configurable.

## New source files

| File | Purpose |
|------|---------|
| `internal/proxy/loading.go` | Loading page template embed, render, and browser detection |
| `internal/proxy/loading.html` | Cold-start loading page (inline CSS/JS) |
| `internal/proxy/internal.go` | `/__blockyard/` prefix interception and ready endpoint |
| `internal/db/migrations/sqlite/003_v2_pre_warming.up.sql` | Add `pre_warmed_seats` column |
| `internal/db/migrations/sqlite/003_v2_pre_warming.down.sql` | Remove `pre_warmed_seats` column |
| `internal/db/migrations/postgres/003_v2_pre_warming.up.sql` | Add `pre_warmed_seats` column |
| `internal/db/migrations/postgres/003_v2_pre_warming.down.sql` | Remove `pre_warmed_seats` column |

## Modified files

| File | Change |
|------|--------|
| `internal/db/db.go` | `AppRow.PreWarmedSeats` field; `ListPreWarmedApps` method; `UpdateApp` handles `pre_warmed_seats` |
| `internal/server/state.go` | `IdleWorkers` removes keep-at-least-one constraint; `ClearIdleSince` returns `bool` |
| `internal/proxy/proxy.go` | Browser vs non-browser cold-start branch; `/__blockyard/` interception; event-driven pre-warm trigger |
| `internal/proxy/autoscaler.go` | `ensurePreWarmed` function; `preWarmApps` function; call from `autoscaleTick` |
| `internal/proxy/coldstart.go` | `triggerSpawn` helper (fire-and-forget spawn for loading page path) |
| `internal/api/apps.go` | `updateAppRequest.PreWarmedSeats` field; validation in `UpdateApp` handler |

## Exit criteria

**Scale-to-zero:**

- App with no active sessions → idle timeout expires → all workers
  evicted → zero workers for the app
- Next request triggers cold start (loading page or hold-until-healthy)
- `IdleWorkers()` returns the last worker for an app when idle beyond
  timeout (no keep-at-least-one)
- Existing `idle_worker_timeout` config honored unchanged

**Pre-warming:**

- App with `pre_warmed_seats = 1` → autoscaler maintains one idle
  worker
- Claiming warm worker → event-driven replacement spawned within
  seconds
- Autoscaler tick also detects and fills deficits (safety net)
- Pre-warming respects `max_workers_per_app` and global `max_workers`
- Pre-warming works for apps with zero running workers (DB query path)
- `PATCH /api/v1/apps/{id}` with `pre_warmed_seats: 2` → accepted
- Invalid values (negative, >10) → 400
- Default value is 0 (scale-to-zero)

**Loading page:**

- Browser request to app with no workers → 200 HTML loading page (not
  held open)
- Loading page polls `GET /app/{name}/__blockyard/ready` every 2s
- `/__blockyard/ready` returns `{ "ready": false }` during startup
- `/__blockyard/ready` returns `{ "ready": true }` when worker healthy
- JavaScript redirects to `/app/{name}/` on ready
- Redirected request gets a session and sees the Shiny app
- Loading page shows error on timeout (`worker_start_timeout`)
- Non-browser requests (API, WebSocket) use existing hold-until-healthy
- `/__blockyard/ready` enforces same auth as the app itself
- `/__blockyard/ready` does not create sessions or set cookies
- Unknown `/__blockyard/*` paths → 404

**General:**

- All new unit and integration tests pass on both SQLite and PostgreSQL
- All existing tests still pass (the `ClearIdleSince` signature change
  requires updating existing callers to ignore or use the return value)
- `go vet ./...` clean
- `go test ./...` green
- Migration 003 applies cleanly on both dialects
