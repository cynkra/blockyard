# Phase 3-4: Drain Mode & Server Handoff

Adds `SIGUSR1` drain mode for rolling updates and passive mode for safe
server overlap. Together with phases 3-2 (interfaces + token persistence)
and 3-3 (Redis shared state), this completes the server-side machinery
that `by admin update` (phase 3-5) orchestrates.

Depends on phase 3-3 (Redis shared state). The new server must connect to
Redis at startup to discover existing workers — passive mode without
shared state is meaningless.

---

## Prerequisites from Earlier Phases

- **Phase 3-2** — interface extraction, worker token persistence. The
  `Server` struct already holds interface-typed fields (`SessionStore`,
  `WorkerRegistry`, `WorkerMap`).
- **Phase 3-3** — Redis shared state. Both old and new servers read/write
  the same Redis-backed stores. The `[redis]` config section and
  `redisstate` package exist.

## Deliverables

1. **Drain package** (`internal/drain/drain.go`) — orchestrates the
   SIGUSR1 drain sequence and the SIGTERM shutdown sequence.
2. **Server draining flag** — `atomic.Bool` on `Server`, checked by
   health endpoints.
3. **Health endpoint gating** — `/healthz` and `/readyz` return 503
   when the draining flag is set.
4. **SIGUSR1 signal handling** — distinguished from SIGTERM/SIGINT in
   `main.go`.
5. **Config: `drain_timeout`** — new `[server]` field, separate from
   `shutdown_timeout`.
6. **Passive mode** — `BLOCKYARD_PASSIVE=1` env var. Server serves
   requests but does not start background goroutines.
7. **Activation endpoint** — `POST /api/v1/admin/activate` starts
   background goroutines on a passive server.
8. **Tests** — drain sequence ordering, passive mode verification,
   activation endpoint, health gating.

## Step-by-step

### Step 1: Server draining flag

Add an `atomic.Bool` field to the `Server` struct in
`internal/server/state.go`:

```go
type Server struct {
    // ...existing fields...

    // Draining is set when the server enters drain mode (SIGUSR1) or
    // shutdown (SIGTERM). Health endpoints return 503 when set.
    Draining atomic.Bool
}
```

No constructor change — `atomic.Bool` zero value is `false`.

### Step 2: Health endpoint gating

Modify `/healthz` and `/readyz` to check the draining flag before doing
anything else. Two locations: `NewRouter()` (main listener, lines 172-175)
and `NewManagementRouter()` (management listener, lines 340-343) in
`internal/api/router.go`.

**`/healthz`** — currently returns a hardcoded `"ok"`. Change to:

```go
r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
    if srv.Draining.Load() {
        w.WriteHeader(http.StatusServiceUnavailable)
        w.Write([]byte("draining"))
        return
    }
    w.Write([]byte("ok"))
})
```

Both the main and management router get the same change.

**`/readyz`** — add an early return at the top of `readyzHandler` in
`internal/api/readyz.go`:

```go
func readyzHandler(srv *server.Server, trusted bool) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if srv.Draining.Load() {
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusServiceUnavailable)
            json.NewEncoder(w).Encode(map[string]any{
                "status": "draining",
            })
            return
        }
        // ...existing checks unchanged...
    }
}
```

The draining check runs before any dependency checks (DB ping, Docker
socket, etc.) — this is intentional. The proxy must see 503 immediately,
not after a 5-second database timeout.

### Step 3: Config — drain_timeout

Add `drain_timeout` to `ServerConfig` in `internal/config/config.go`:

```go
type ServerConfig struct {
    // ...existing fields...
    DrainTimeout    Duration `toml:"drain_timeout"`
}
```

Default in `applyDefaults()`:

```go
if cfg.Server.DrainTimeout.Duration == 0 {
    cfg.Server.DrainTimeout.Duration = 30 * time.Second
}
```

**Why separate from `shutdown_timeout`:** drain mode (SIGUSR1) leaves
workers alive — it only needs to finish in-flight HTTP requests. Shutdown
(SIGTERM) also evicts workers and waits for sessions to drain. Different
operations, different time budgets. Keeping them separate lets operators
tune independently: a fast drain (5s) for rolling updates where the proxy
shifts traffic quickly, but a longer shutdown (60s) for `docker stop`
scenarios where sessions should finish gracefully.

### Step 4: Drain package

New file `internal/drain/drain.go`:

```go
package drain

import (
    "context"
    "log/slog"
    "net/http"
    "sync"
    "time"

    "github.com/cynkra/blockyard/internal/ops"
    "github.com/cynkra/blockyard/internal/server"
)

// Drainer manages the two shutdown paths: drain (SIGUSR1, workers
// survive) and shutdown (SIGTERM, full cleanup).
type Drainer struct {
    Srv        *server.Server
    MainServer *http.Server
    MgmtServer *http.Server   // may be nil
    BGCancel   context.CancelFunc
    BGWait     *sync.WaitGroup
    TracingShutdown func(context.Context) error // may be nil
}

// Drain executes the SIGUSR1 sequence. Workers survive — the new
// server manages them via Redis.
//
// Sequence:
//  1. Set draining flag (health endpoints return 503 immediately)
//  2. Drain HTTP servers (finish in-flight requests)
//  3. Cancel background goroutines
//  4. Close database
//  5. Flush tracing
func (d *Drainer) Drain(timeout time.Duration) {
    slog.Info("drain mode: entering (SIGUSR1)")

    // 1. Health endpoints start returning 503.
    d.Srv.Draining.Store(true)

    // 2. Drain HTTP servers.
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    if d.MgmtServer != nil {
        if err := d.MgmtServer.Shutdown(ctx); err != nil {
            slog.Error("drain: management server shutdown error", "error", err)
        }
    }
    if err := d.MainServer.Shutdown(ctx); err != nil {
        slog.Error("drain: main server shutdown error", "error", err)
    }

    // 3. Stop background goroutines.
    d.BGCancel()
    d.BGWait.Wait()

    // 4. Close database.
    if err := d.Srv.DB.Close(); err != nil {
        slog.Error("drain: database close error", "error", err)
    }

    // 5. Flush tracing.
    if d.TracingShutdown != nil {
        d.TracingShutdown(context.Background()) //nolint:errcheck
    }

    slog.Info("drain mode: complete, exiting")
}

// Shutdown executes the SIGTERM sequence. Workers are evicted and
// resources cleaned up — the full current shutdown behavior.
//
// Sequence:
//  1. Set draining flag (health endpoints return 503 immediately)
//  2. Drain HTTP servers
//  3. Cancel background goroutines
//  4. Graceful worker shutdown (drain sessions, evict, remove resources)
//  5. Close database
//  6. Flush tracing
func (d *Drainer) Shutdown(timeout time.Duration) {
    slog.Info("shutdown: entering (SIGTERM/SIGINT)")

    // 1. Health endpoints start returning 503.
    d.Srv.Draining.Store(true)

    // 2. Drain HTTP servers.
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    if d.MgmtServer != nil {
        if err := d.MgmtServer.Shutdown(ctx); err != nil {
            slog.Error("shutdown: management server error", "error", err)
        }
    }
    if err := d.MainServer.Shutdown(ctx); err != nil {
        slog.Error("shutdown: main server error", "error", err)
    }

    // 3. Stop background goroutines.
    d.BGCancel()
    d.BGWait.Wait()

    // 4. Stop all workers and clean up.
    ops.GracefulShutdown(context.Background(), d.Srv)

    // 5. Close database.
    if err := d.Srv.DB.Close(); err != nil {
        slog.Error("shutdown: database close error", "error", err)
    }

    // 6. Flush tracing.
    if d.TracingShutdown != nil {
        d.TracingShutdown(context.Background()) //nolint:errcheck
    }

    slog.Info("shutdown complete")
}
```

**Key difference between Drain and Shutdown:** `Drain` skips step 4
(`ops.GracefulShutdown`). Workers, networks, and token directories are
left intact — the new server already manages them via Redis.

### Step 5: Signal handling in main.go

Replace the current signal handling block (lines 385-439) in
`cmd/blockyard/main.go`. Also remove the `defer database.Close()` (line
89) — the Drainer now owns DB lifecycle via its `Drain()` and
`Shutdown()` methods. The new code distinguishes three signals:

```go
// Set up signal channels.
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)

drainer := &drain.Drainer{
    Srv:             srv,
    MainServer:      httpServer,
    MgmtServer:      mgmtServer,
    BGCancel:        bgCancel,
    BGWait:          &bgWg,
    TracingShutdown: tracingShutdown,
}

// Wait for signal.
sig := <-sigCh
switch sig {
case syscall.SIGUSR1:
    drainer.Drain(cfg.Server.DrainTimeout.Duration)
default:
    // SIGTERM, SIGINT → full shutdown.
    drainer.Shutdown(cfg.Server.ShutdownTimeout.Duration)
}
```

This replaces `signal.NotifyContext` with an explicit channel so we can
distinguish signal types. The `<-sigCh` blocks the same way `<-ctx.Done()`
did.

### Step 6: Passive mode — environment variable

When `BLOCKYARD_PASSIVE=1` is set, the server skips all background
goroutine spawning. It still:

- Runs migrations
- Connects to Redis (reads existing worker state)
- Joins worker networks
- Serves HTTP requests (proxies to workers it discovers via Redis)
- Responds to health checks

It does **not** start:

- Health poller (`ops.SpawnHealthPoller`)
- Log retention cleaner (`ops.SpawnLogRetentionCleaner`)
- Autoscaler (`proxy.RunAutoscaler`)
- Soft-delete sweeper (`ops.SpawnSoftDeleteSweeper`)
- Update checker (`update.SpawnChecker`)
- Store eviction sweeper (`pkgstore.SpawnEvictionSweeper`)
- Refresh scheduler (`srv.RunRefreshScheduler`)

The **audit log writer** and **vault token renewer** run even in passive
mode. Neither mutates shared state — the audit writer appends to a local
file, and the renewer refreshes this server's own vault token. More
importantly, the audit writer *must* run: the passive server serves
requests that generate audit entries via a buffered channel (capacity
1000). Without the writer draining it, the channel fills and blocks
request goroutines.

**Startup guard:** passive mode requires Redis. Without shared state the
server has no worker map, no session routing, no registry — it can't
proxy anything. In `cmd/blockyard/main.go`, immediately after parsing
the env var:

```go
passive := os.Getenv("BLOCKYARD_PASSIVE") == "1"
if passive && cfg.Redis.URL == "" {
    slog.Error("BLOCKYARD_PASSIVE=1 requires [redis] to be configured")
    os.Exit(1)
}
if passive {
    slog.Info("starting in passive mode (background goroutines deferred)")
}

if !passive {
    bgWg.Add(1)
    go func() {
        defer bgWg.Done()
        ops.SpawnHealthPoller(bgCtx, srv)
    }()

    // ...all other background goroutines...
}
```

Add a field to `Server` to track activation state:

```go
type Server struct {
    // ...existing fields...

    Draining atomic.Bool

    // Passive is true when BLOCKYARD_PASSIVE=1 is set. Background
    // goroutines are deferred until POST /api/v1/admin/activate.
    Passive atomic.Bool
}
```

### Step 7: Activation endpoint

New endpoint `POST /api/v1/admin/activate` in `internal/api/router.go`,
registered in the authenticated API group (requires admin PAT).

The endpoint receives the background context and WaitGroup from a
closure — the same pattern used for goroutine spawning in `main.go`.

**Handler in `internal/api/activate.go`:**

```go
package api

import (
    "encoding/json"
    "net/http"
    "sync"

    "github.com/cynkra/blockyard/internal/auth"
    "github.com/cynkra/blockyard/internal/server"
)

// activateHandler starts background goroutines on a passive server.
// Returns 200 on success, 403 if not admin, 409 if already active or
// not passive.
//
// The sync.Once is scoped to the closure — no exported field on Server
// needed, and the once-guard lives where it's used.
func activateHandler(srv *server.Server, startBG func()) http.HandlerFunc {
    var once sync.Once
    return func(w http.ResponseWriter, r *http.Request) {
        caller := auth.CallerFromContext(r.Context())
        if caller == nil || !caller.Role.CanManageRoles() {
            forbidden(w, "admin only")
            return
        }

        if !srv.Passive.Load() {
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusConflict)
            json.NewEncoder(w).Encode(map[string]string{
                "error": "server is already active",
            })
            return
        }

        activated := false
        once.Do(func() {
            startBG()
            srv.Passive.Store(false)
            activated = true
        })

        w.Header().Set("Content-Type", "application/json")
        if activated {
            w.WriteHeader(http.StatusOK)
            json.NewEncoder(w).Encode(map[string]string{
                "status": "activated",
            })
        } else {
            w.WriteHeader(http.StatusConflict)
            json.NewEncoder(w).Encode(map[string]string{
                "error": "activation already in progress",
            })
        }
    }
}
```

**Router registration** in the admin-authed API group:

```go
r.Post("/api/v1/admin/activate", activateHandler(srv, startBG))
```

**`startBG` closure in `main.go`:**

The background goroutine spawning code is extracted into a function that
`main.go` calls directly (when not passive) or passes to the activate
handler (when passive):

```go
startBG := func() {
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

    // ...RunAutoscaler, SpawnSoftDeleteSweeper, SpawnChecker,
    // RunRefreshScheduler (same as current code)...

    // Store eviction sweeper — not bgWg-tracked (same as current
    // code; spawns its own goroutine, exits via bgCtx cancellation).
    if cfg.Docker.StoreRetention.Duration > 0 {
        pkgstore.SpawnEvictionSweeper(bgCtx, srv.PkgStore, cfg.Docker.StoreRetention.Duration)
    }
}

// Audit log writer runs unconditionally — even in passive mode the
// server serves requests that produce audit entries. Without the
// writer draining the buffered channel, it fills and blocks request
// goroutines.
if srv.AuditLog != nil {
    bgWg.Add(1)
    go func() {
        defer bgWg.Done()
        srv.AuditLog.Run(bgCtx, cfg.Audit.Path)
    }()
}

if !passive {
    startBG()
} else {
    srv.Passive.Store(true)
}

// Pass startBG to router setup for the activate endpoint.
handler := api.NewRouter(srv, startBG)
```

**Why a closure, not a method on Server:** the goroutines need `bgCtx`,
`bgWg`, and `cfg` — all owned by `main.go`, not `Server`. Passing a
closure keeps the dependency graph clean.

**Router signature change:** `NewRouter` gains a `startBG func()`
parameter. When the server is not passive (`startBG == nil` would also
work), the activate endpoint can still be registered but returns 409
immediately.

### Step 8: Readyz integration — passive mode

When the server is passive, `/readyz` should still return 200 (the
server is ready to serve requests). Add the passive state to the
response body so `by admin update` can distinguish:

```go
result := map[string]any{"status": status}
if srv.Passive.Load() {
    result["mode"] = "passive"
}
```

This lets `by admin update` (phase 3-5) poll `/readyz` until 200, then
know the server is passive and needs activation after the old server
exits.

### Step 9: Vault token renewal in passive mode

The vault token renewer is special — it's not a state-mutating background
loop, it keeps the server's own vault authentication alive. If the
passive window is short (typical: seconds), this doesn't matter. But if
the passive window is long (e.g., readyz polling hangs for minutes), the
AppRole token could expire.

**Decision:** start the vault token renewer even in passive mode. It
only affects this server's own token — no shared state mutation, no
conflict with the old server. This keeps the passive server's vault
operations (credential exchange, session secret reads) functional
throughout the overlap.

In `main.go`, the vault token renewal goroutine spawns unconditionally
(before the `if !passive` block), same as today. Only the state-mutating
goroutines are deferred.

### Step 10: Tests

#### Drain sequence test

`internal/drain/drain_test.go`:

```go
func TestDrainSetsFlag(t *testing.T) {
    srv := &server.Server{}
    main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
    defer main.Close()

    var wg sync.WaitGroup
    ctx, cancel := context.WithCancel(context.Background())

    d := &drain.Drainer{
        Srv:        srv,
        MainServer: main.Config, // *http.Server from httptest
        BGCancel:   cancel,
        BGWait:     &wg,
    }

    // Drain should set the flag.
    d.Drain(5 * time.Second)
    if !srv.Draining.Load() {
        t.Error("expected Draining to be true after Drain()")
    }
}
```

#### Health endpoint gating test

`internal/api/readyz_test.go` (extend existing file or new):

```go
func TestHealthzDraining(t *testing.T) {
    srv := testServer(t)
    srv.Draining.Store(true)

    r := httptest.NewRequest("GET", "/healthz", nil)
    w := httptest.NewRecorder()

    handler := NewManagementRouter(srv)
    handler.ServeHTTP(w, r)

    if w.Code != http.StatusServiceUnavailable {
        t.Errorf("expected 503, got %d", w.Code)
    }
}

func TestReadyzDraining(t *testing.T) {
    srv := testServer(t)
    srv.Draining.Store(true)

    r := httptest.NewRequest("GET", "/readyz", nil)
    w := httptest.NewRecorder()

    handler := NewManagementRouter(srv)
    handler.ServeHTTP(w, r)

    if w.Code != http.StatusServiceUnavailable {
        t.Errorf("expected 503, got %d", w.Code)
    }

    var body map[string]any
    json.NewDecoder(w.Body).Decode(&body)
    if body["status"] != "draining" {
        t.Errorf("expected status 'draining', got %v", body["status"])
    }
}
```

#### Passive mode + activation test

`internal/api/activate_test.go`:

```go
func TestActivateEndpoint(t *testing.T) {
    srv := testServer(t)
    srv.Passive.Store(true)

    activated := false
    startBG := func() { activated = true }

    handler := activateHandler(srv, startBG)

    // First call: activates.
    r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil)
    w := httptest.NewRecorder()
    handler.ServeHTTP(w, r)

    if w.Code != http.StatusOK {
        t.Errorf("expected 200, got %d", w.Code)
    }
    if !activated {
        t.Error("expected startBG to be called")
    }
    if srv.Passive.Load() {
        t.Error("expected Passive to be false after activation")
    }

    // Second call: conflict.
    w2 := httptest.NewRecorder()
    handler.ServeHTTP(w2, r)
    if w2.Code != http.StatusConflict {
        t.Errorf("expected 409, got %d", w2.Code)
    }
}

func TestActivateWhenAlreadyActive(t *testing.T) {
    srv := testServer(t)
    // Passive is false (default).

    handler := activateHandler(srv, func() {
        t.Error("startBG should not be called when not passive")
    })

    r := httptest.NewRequest("POST", "/api/v1/admin/activate", nil)
    w := httptest.NewRecorder()
    handler.ServeHTTP(w, r)

    if w.Code != http.StatusConflict {
        t.Errorf("expected 409, got %d", w.Code)
    }
}
```

#### Passive readyz test

```go
func TestReadyzPassiveMode(t *testing.T) {
    srv := testServer(t)
    srv.Passive.Store(true)

    r := httptest.NewRequest("GET", "/readyz", nil)
    w := httptest.NewRecorder()

    handler := NewManagementRouter(srv)
    handler.ServeHTTP(w, r)

    // Should return 200 — passive servers are ready to serve.
    if w.Code != http.StatusOK {
        t.Errorf("expected 200, got %d", w.Code)
    }

    var body map[string]any
    json.NewDecoder(w.Body).Decode(&body)
    if body["mode"] != "passive" {
        t.Errorf("expected mode 'passive', got %v", body["mode"])
    }
}
```

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/drain/drain.go` | **create** | `Drainer` struct with `Drain()` and `Shutdown()` methods |
| `internal/server/state.go` | **update** | Add `Draining atomic.Bool`, `Passive atomic.Bool` fields |
| `internal/config/config.go` | **update** | Add `DrainTimeout Duration` to `ServerConfig`, default 30s |
| `internal/api/router.go` | **update** | Gate `/healthz` on draining flag; register `/api/v1/admin/activate`; `NewRouter` gains `startBG` parameter |
| `internal/api/readyz.go` | **update** | Early 503 return when draining; add `mode` field when passive |
| `internal/api/activate.go` | **create** | `activateHandler()` — starts background goroutines on passive server |
| `cmd/blockyard/main.go` | **update** | SIGUSR1 handling; extract `startBG` closure; passive mode gating; construct `Drainer` |
| `internal/drain/drain_test.go` | **create** | Drain flag, shutdown sequence tests |
| `internal/api/activate_test.go` | **create** | Activation endpoint tests |
| `internal/api/readyz_test.go` | **update** | Health gating and passive mode tests |

## Design decisions

1. **`Drainer` struct, not free functions.** The drain and shutdown
   sequences share the same dependencies (servers, cancel func, wait
   group). A struct captures these once at startup rather than threading
   six parameters through signal handlers. The struct is simple — no
   methods beyond `Drain()` and `Shutdown()`.

2. **Draining flag is `atomic.Bool` on `Server`, not on `Drainer`.**
   The health endpoints need to read it, and they have access to `srv`
   but not to the `Drainer`. Adding it to `Server` keeps the check to
   a single `srv.Draining.Load()` in the hot path.

3. **Health endpoints return 503 before any dependency checks.** When
   draining, the proxy needs to see 503 instantly — not after waiting
   for a DB ping timeout. The draining check is a single atomic load,
   effectively zero cost on the non-draining path.

4. **`/healthz` returns `"draining"` text, not just 503.** This lets
   operators and monitoring distinguish "server is draining" from
   "server is unhealthy" in logs and alerting. Same for `/readyz`
   returning `{"status": "draining"}` rather than `{"status":
   "not_ready"}`.

5. **`drain_timeout` separate from `shutdown_timeout`.** Drain mode
   only finishes in-flight HTTP requests. Shutdown also evicts workers
   and waits for sessions. Different time budgets. The v3 plan specifies
   this as a distinct config field.

6. **Passive mode via environment variable, not config file.** The
   `BLOCKYARD_PASSIVE=1` var is set by `by admin update` when starting
   the new container. It's a one-shot runtime flag, not a persistent
   configuration choice. Environment is the right mechanism — it
   doesn't require modifying the compose file or blockyard.toml. The
   startup guard hard-fails if Redis isn't configured — a passive
   server without shared state can't route to any workers, so
   proceeding would just produce a server that passes readyz but 502s
   every request.

7. **Vault token renewal runs in passive mode.** It's not a
   state-mutating background loop — it only keeps this server's own
   vault authentication alive. Deferring it would risk token expiry
   during a long passive window, breaking credential operations the
   moment the server activates.

8. **Activation is a `sync.Once`, not a flag toggle.** `sync.Once`
   guarantees the goroutines start exactly once even if the endpoint
   is called concurrently. The `Once` is scoped to the handler closure,
   not a field on `Server` — it doesn't need to be accessed from
   outside the `api` package.

9. **`NewRouter` takes `startBG func()`.** The alternative — storing
   `startBG` on `Server` — would leak main.go's goroutine management
   into the server package. The closure keeps the dependency explicit
   and directional: main → api, not api → main via Server.

10. **Drain does not close Redis.** The Redis connection is managed by
    the `redisstate` package (phase 3-3). When the process exits, the
    connection closes automatically. Explicitly closing it before the
    process exits adds no value and risks premature disconnection if
    there's any Redis I/O in the shutdown path.

11. **No `StartupCleanup` changes.** The current `StartupCleanup`
    force-removes all managed containers on boot. For rolling updates,
    the new server must *not* do this — it should adopt existing workers
    via Redis. This change belongs in phase 3-3 (Redis startup path),
    not here. Phase 3-4 only adds drain/passive behavior, not startup
    behavior changes.
