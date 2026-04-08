# Phase 3-9: Zygote Worker Model

An opt-in worker model where one container (Docker) or one bwrap
sandbox (process backend) runs a long-lived **zygote** R process that
pre-loads the bundle's packages, then forks one child process per
session on demand. Each child binds its own port; the proxy routes
the session directly to that port. The term comes from the same
pattern Android uses for app process startup: a single long-lived
parent that has the expensive shared state loaded once, forked on
demand into specialised children.

The mechanism is symmetrical across both backends. The shared logic
splits across two packages: `internal/zygotectl/` (control
protocol client, wire types, embedded R script and C helper —
no dependency on `backend`) and `internal/zygote/` (the
`Manager` type that owns session ↔ child bookkeeping,
child-exit handling, and the cold-start hook). The
backend-specific bits (control transport, child reaping, R
process spawn) live in each backend behind a new optional
`Forking` capability interface alongside `Backend`. See design
decision #5 for why the zygote support is split across two
packages.

The zygote model is **experimental** and requires **two levels of
opt-in**: a server-wide `experimental.zygote` config flag AND a
per-app `zygote` boolean column. Both default off; operators who
upgrade blockyard get identical behaviour to the prior version
until they explicitly flip them. See design decision #15 for the
full gating rationale. The existing shared-multiplexing mode
(`max_sessions_per_worker > 1` without the zygote flag) is
unchanged and remains the default for multi-session apps.

Phase 3-9 lands the **mechanism** only — the two unconditional
benefits (startup-latency elimination and per-session isolation).
Phase 3-10 adds post-fork sandboxing (private mount namespace,
`/tmp` isolation, seccomp, capability dropping, per-process
rlimits) and the opt-in KSM memory-sharing story (C helper,
`STATS` observability, preflight, byte-compilation,
`oom_score_adj` pinning). See `phase-3-10-draft.md` for the
KSM design — it was originally folded into phase 3-9 but carved
out to keep this phase focused on the mechanism. Nothing in phase
3-9 depends on KSM; KSM is layered on top in 3-10 without
revisiting the protocol surface other than to add a `STATS`
command and two fields to `INFO`.

## What the zygote model actually optimizes

Two durable benefits, both unconditional on any supported host:

1. **Session startup latency (the main point).** Forking a
   zygote takes single-digit milliseconds. Starting a fresh R
   process and loading a heavy bundle's packages takes
   seconds-to-tens-of-seconds for rstan, arrow, sf, torch, large
   model files, and so on. For apps where cold start is the
   user-visible bottleneck — interactive dashboards, frequent
   intermittent users, scale-to-zero deployments — the zygote
   model dramatically improves first-byte time on every new
   session.

2. **Per-session isolation.** Each child is a separate R
   process. GC pauses freeze only the affected child, not all
   multiplexed sessions. Crashes don't take down siblings. No
   shared global mutation between users. Shared multi-session
   mode couples all of these together for memory efficiency;
   the zygote model trades that coupling for isolation.

**Memory sharing via copy-on-write is an opportunistic
third benefit that does not ship in phase 3-9.** Forked children
start by sharing every page with the zygote via the kernel's
COW mechanism, but R's generational GC writes mark bits to SEXP
headers during level-2 collections, dirtying every page
containing a live SEXP and breaking COW. Recovering that sharing
needs a kernel-level merge mechanism (KSM via
`prctl(PR_SET_MEMORY_MERGE)` on Linux 6.4+), up-front
byte-compilation of bundle code so the JIT doesn't mutate closure
headers post-fork, and `oom_score_adj` pinning so the kernel OOM
killer reaps children first when KSM recovery lags behind a
coordinated GC burst. That whole story lives in phase 3-10 — see
`phase-3-10-draft.md` decisions K1, K3, and K5. Phase 3-9 ships
the zygote mechanism alone; it delivers benefits #1 and #2
unchanged regardless of whether phase 3-10 ever lands.

**When to enable the zygote model.** Apps where cold start is the
user-visible bottleneck — interactive dashboards, frequent
intermittent users, scale-to-zero deployments — and per-session
isolation matters. No kernel requirement beyond what the rest of
blockyard needs; the zygote model delivers these benefits on any
supported host.

Apps with small package sets, fast loads, and tight RAM budgets
are usually better served by shared multi-session mode
(`max_sessions_per_worker > 1` without `zygote`), which was
designed for exactly that trade-off.

---

## Prerequisites from earlier phases

- **Phase 3-1** — migration discipline. The `zygote` column
  follows expand-only rules: `ADD COLUMN ... NOT NULL DEFAULT 0`.
  The DDL linter, convention check, and roundtrip test all apply.
- **Phase 3-2** — interface extraction. The `session.Store` interface
  is the seam where the new `Entry.Addr` field gets added; both the
  memory and Redis implementations must round-trip it.
- **Phase 3-3** — Redis shared state. The Redis `SessionStore`
  implementation must persist and read back `Entry.Addr` so the
  field survives a rolling update.
- **Phase 3-6** — per-app config. This phase adds one per-app
  field (`zygote`) following the same pattern: DB column →
  `AppRow` → `AppUpdate` → API → CLI → UI. See decision #15
  for the two-level opt-in that layers the server-wide
  `experimental.zygote` flag on top of the per-app column.
- **Phase 3-7** — process backend core. The process backend's
  `Forking` implementation extends the bwrap spawn flow built here.
  Phase 3-9 assumes phase 3-7 leaves the network namespace shared
  (no `--unshare-net`), which makes loopback TCP a viable control
  transport.
- **Phase 3-8** — process backend packaging. Provides the
  `portAllocator` interface (`Reserve() (port, ln net.Listener, err error)`
  + `Release`) and both the `memoryPortAllocator` and
  `redisPortAllocator` implementations that phase 3-9's two new
  allocators reuse — phase 3-9 instantiates each implementation
  three times (one per range), branching on `cfg.Redis != nil`
  the same way phase 3-8 does for the existing `ports`
  allocator. Phase 3-9 also lifts phase 3-8's hardcoded
  `"port:"` Redis key prefix to a constructor parameter so the
  three allocators can share `redisPortAllocator` against
  disjoint key namespaces. Phase 3-8 also provides
  `drainer.FinishIdleWait`, which gracefully waits zygote
  sessions out during a process-backend rolling update — zygote
  sessions have `Entry.WorkerID = zygoteWorkerID` so they're
  counted by `Sessions.CountForWorkers(own)` unchanged, and the
  zygote worker (with all its forked children) survives until
  the last session ends or the timeout elapses. The seccomp
  profile finalised in 3-8 is what phase 3-10 will apply
  post-fork.

## Deliverables

1. **Two-level opt-in gating for zygote** — see decision #15
   for the full gating model. This deliverable has two layers,
   both default off:
   - **`experimental.zygote` server-wide config flag**: new
     `[experimental]` section on `Config`, default off when the
     section is absent. Runtime paths short-circuit to
     non-zygote behaviour whenever the flag is off, giving
     operators a kill switch without database updates.
   - **`zygote` column** on the `apps` table: added in
     migration `003_zygote` following expand-only rules,
     defaults to `0` (off). Validated to require
     `max_sessions_per_worker > 1` and the server-wide
     `experimental.zygote` flag.
2. **`session.Entry.Addr`** — new field on the session store entry.
   Populated at session creation; read by the proxy on every
   subsequent request. Gives zygote sessions a per-child routing
   target without disturbing the registry-based path used by
   non-zygote sessions. Round-trips through both `MemoryStore`
   and `RedisStore`.
3. **`Forking` capability sub-interface** in `internal/backend/` —
   optional capability that backends may implement. Three methods:
   `Fork`, `KillChild`, `ChildExits`. Plus a `ChildExit` value type.
4. **`internal/zygotectl/` and `internal/zygote/` packages** —
   the zygote support is split across two packages to break an
   import cycle (decision #5). `internal/zygotectl/` holds the
   control-protocol surface (`Client`, `Info`, `ChildExitMsg`,
   secret helpers) and has no dependency on `backend`, so
   `backend.Forking` can reference `zygotectl` types on its
   interface without cycling. `internal/zygote/` holds the
   backend-agnostic `Manager` type that owns the session ↔
   child bookkeeping, subscribes to `ChildExits()`, exposes
   `Fork(ctx, workerID, sessionID) (addr, error)` to the rest
   of the server, and runs a periodic sweep that
   cross-references its bookkeeping against the session store
   and kills children whose session has vanished.
5. **`internal/zygotectl/zygote.R`** — embedded R script. Loads
   the bundle's packages, sources `app.R` to capture the
   `shinyApp()` arguments for child `runApp`, listens on the
   control TCP port, handles `FORK`/`KILL`/`STATUS`/`INFO`/`AUTH`,
   pushes `CHILDEXIT` from a `socketSelect`-driven 100ms poll
   loop. Single-threaded throughout — no `httpuv`/`later` (see
   decision #4 for why). Embedded into the server binary via
   `//go:embed`, written to a host path at startup, bind-mounted
   into the worker container or bwrap sandbox. Phase 3-10 will
   extend this script with up-front byte-compilation, KSM
   helper loading, a `STATS` handler, and child `oom_score_adj`
   pinning — see `phase-3-10-draft.md` Step K6.
6. **Docker `Forking` implementation** — adds zygote-mode container
   spawn, control port `3837` on the per-worker bridge, child port
   range allocation, control client wired to the shared protocol,
   control-connection watcher goroutine that evicts the worker on
   unexpected disconnect, and idempotent `Stop` hardening.
7. **Process `Forking` implementation** — adds zygote-mode bwrap
   spawn, control port allocation from a host-wide range, control
   client, same control-connection watcher / idempotent-Stop
   hardening as the Docker impl. Requires phase 3-7's port
   allocator.
8. **Per-worker control secret** — a 32-byte random secret written
   to the per-worker token dir at spawn (alongside the existing
   `token` file). Server holds the secret in memory; zygote reads
   it from the mounted token dir. Used as `AUTH` first frame.
9. **Cold-start integration** — `ensureWorker` calls
   `zygote.Manager.Fork` after spawning/finding a zygote worker
   for zygote apps. The returned address goes onto `session.Entry.Addr`.
10. **Proxy fallback** — extend the existing
    "session worker not in registry" fallback (`proxy.go:167`) to
    also cover "session addr unreachable", deleting the stale
    session and falling through to cold-start. Pairs with the
    control-connection watcher in deliverables #6 and #7: when
    a zygote dies unexpectedly, the watcher evicts the worker
    and this fallback catches the sessions that were still
    routed there.
11. **Manager child sweep** — `zygote.Manager` runs its own
    periodic tick that cross-references its `bySession` map against
    `session.Store.Get` and kills children whose session has
    disappeared (TTL expiry on Redis, `Sessions.Delete` from any
    code path on memory). Removes the need for a `SweepIdle` return
    value change and works symmetrically across both stores.
12. **API/CLI/UI** — `zygote` field on `updateAppRequest`,
    `--zygote` flag on `by scale`, settings tab toggle in the
    UI (admin only), and a new `GET /api/v1/server/capabilities`
    endpoint that surfaces the effective `experimental.zygote`
    flag for the UI to gate its toggle state. Worker detail
    page surfaces the cached `zygotectl.Info` (R version,
    preload time) from `INFO`.
13. **Tests** — interface compliance, control protocol unit tests
    including the `INFO` command, Docker integration test
    (zygote start, FORK two children, independent health, KILL
    one, INFO reports expected fields), process integration
    test (same flow under bwrap), session round-trip including
    the new `Addr` field.

---

## Step-by-step

### Step 1: Migration — `zygote` column

Migration `003_zygote` adds one boolean column. Additive,
nullable-equivalent (default 0), backward-compatible per phase 3-1
rules. Phase 3-7 does not add migrations, so `003` is correct as of
phase 3-9. Phase 3-10 will add a separate `004_ksm` migration for
the KSM opt-in.

**`internal/db/migrations/sqlite/003_zygote.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN zygote INTEGER NOT NULL DEFAULT 0;
```

**`internal/db/migrations/sqlite/003_zygote.down.sql`:**

```sql
-- SQLite does not support DROP COLUMN before 3.35.0. Recreate the
-- table without the column. Same pattern as migration 002.
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
    enabled                 INTEGER NOT NULL DEFAULT 1,
    image                   TEXT NOT NULL DEFAULT '',
    runtime                 TEXT NOT NULL DEFAULT ''
);
INSERT INTO apps_new SELECT
    id, name, owner, access_type, active_bundle,
    max_workers_per_app, max_sessions_per_worker,
    memory_limit, cpu_limit, title, description,
    created_at, updated_at, deleted_at,
    pre_warmed_sessions, refresh_schedule, last_refresh_at,
    enabled, image, runtime
FROM apps;
DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;
```

**`internal/db/migrations/postgres/003_zygote.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN zygote BOOLEAN NOT NULL DEFAULT FALSE;
```

**`internal/db/migrations/postgres/003_zygote.down.sql`:**

```sql
ALTER TABLE apps DROP COLUMN zygote;
```

### Step 2: DB layer — `AppRow.Zygote`, `AppUpdate.Zygote`

Add to `AppRow` in `internal/db/db.go` (after `Runtime`, line 235):

```go
type AppRow struct {
    // ...existing fields...
    Zygote bool `db:"zygote" json:"zygote"`
}
```

Add to `AppUpdate` (line 587):

```go
type AppUpdate struct {
    // ...existing fields...
    Zygote *bool
}
```

Update `UpdateApp()` (line 591) to handle the new field:

```go
if u.Zygote != nil {
    app.Zygote = *u.Zygote
}
```

Add `zygote = ?` to the UPDATE SQL alongside the other fields,
plus `app.Zygote` to the bind list. Insert path (`CreateApp`)
defaults to `false`.

### Step 3: `session.Entry.Addr` field

Add an `Addr` field to `session.Entry` in `internal/session/store.go`:

```go
type Entry struct {
    WorkerID   string
    Addr       string    // resolved network address; "" = look up via Registry
    UserSub    string
    LastAccess time.Time
}
```

`Addr` is empty for sessions created before the field existed (Redis
read-back) or for non-zygote sessions where the resolution still
goes through the registry. The proxy reads it on the hot path:

```go
// proxy.go, in the existing session-resolution block (around line 159)
if ok {
    addr := entry.Addr
    if addr == "" {
        a, addrOk := srv.Registry.Get(entry.WorkerID)
        if addrOk {
            addr = a
        }
    }
    if addr != "" {
        workerID, /* addr already set */ = entry.WorkerID, addr
        srv.Sessions.Touch(sessionID)
        // ...continue
    }
}
```

For the new code path (zygote apps), cold-start populates
`entry.Addr` directly from the fork return value (Step 9). For
non-zygote apps the field stays empty and the existing
`Registry.Get` path is used unchanged.

**Memory store** — no method changes needed; `Set/Get` already
copy the whole `Entry`.

**Redis store** (phase 3-3, `internal/session/redis.go`) — add
`addr` to the hash schema:

```go
// Set:
err := r.client.HSet(ctx, key,
    "worker_id", entry.WorkerID,
    "addr", entry.Addr,
    "user_sub", entry.UserSub,
    "last_access", entry.LastAccess.Unix(),
).Err()

// Get: read addr via HGet, default to "" on missing field.
```

Backwards compatible — old entries without `addr` read as `""`,
which triggers the registry fallback path in the proxy.

### Step 4: `Forking` capability interface

Add to `internal/backend/backend.go` (after the `Backend` interface
and `ErrNotSupported`):

```go
// Forking is an optional capability interface implemented by backends
// that support the zygote worker model. A zygote worker is a
// long-lived "zygote" process that has loaded the bundle's packages
// once, and can produce session-specific child processes on demand
// via Fork. Each child has its own network address.
//
// Backends that don't implement Forking simply omit the methods.
// Code that wants to fork checks for the capability via type
// assertion: `f, ok := srv.Backend.(backend.Forking)`.
//
// Phase 3-10 extends this interface with a StatsClient method
// for KSM metrics polling. Splitting the control-protocol types
// into `internal/zygotectl/` (decision #5) already breaks the
// import cycle that method will need, so the addition is a pure
// additive change.
type Forking interface {
    // Fork creates a session-specific child inside the given worker.
    // Returns the child's network address ("ip:port") and an opaque
    // childID used for KillChild and ChildExit correlation.
    Fork(ctx context.Context, workerID, sessionID string) (addr, childID string, err error)

    // KillChild terminates a previously-forked child. Idempotent —
    // killing an already-dead child returns nil.
    KillChild(ctx context.Context, workerID, childID string) error

    // ChildExits returns a long-lived channel that emits an event
    // every time a child process exits, whether by KillChild,
    // crash, or natural termination. The backend owns the goroutine
    // that produces events. The channel is closed when the backend
    // shuts down.
    ChildExits() <-chan ChildExit
}

// ChildExit is the event emitted by Forking.ChildExits when a
// child process terminates.
type ChildExit struct {
    WorkerID string
    ChildID  string
    ExitCode int
    Reason   string // "killed" / "crashed" / "oom" / "normal"
}
```

The interface lives in the same file as `Backend` because it's
adjacent in concept. Implementations live in each backend's package.

### Step 5: `internal/zygotectl/` and `internal/zygote/` packages

The zygote support splits across two packages to keep the
control-protocol surface decoupled from `backend`. The boundary
follows what depends on `backend.Forking` and what doesn't:

- **`internal/zygotectl/`** — the control-protocol surface:
  `Client`, `Info`, `ChildExitMsg`, the per-worker secret
  helpers, and the embedded `zygote.R` script. This package
  imports only the standard library, so nothing forms an import
  cycle when phase 3-10 extends `backend.Forking` with a
  `StatsClient(workerID) *zygotectl.Client` method.
- **`internal/zygote/`** — the `Manager` type that owns the
  session ↔ child bookkeeping and subscribes to
  `Forking.ChildExits`. It imports both `backend` (for `Forking`
  and `ChildExit`) and `zygotectl` (for `Client`).

**`internal/zygotectl/control.go`** — TCP control protocol client.
Shared between both backend `Forking` implementations.

> **Why bare TCP and not httpuv/HTTP?** I researched the
> alternative seriously (httpuv-as-server with HTTP requests +
> WebSocket push for events) and ruled it out: `httpuv::startServer`
> documents that I/O runs on a background thread (rstudio/httpuv#106),
> and R core's own docs strongly discourage `parallel::mcfork`
> from any multi-threaded R process — the combination is
> officially unsafe. The bare-socket model below uses
> `base::socketSelect` (single-threaded, fork-safe, no extra
> dependencies) to drive non-blocking polling with 100ms-bounded
> CHILDEXIT timeliness. The R 3.4.3 fractional-timeout bug
> (HenrikBengtsson/Wishlist-for-R#35) was fixed in 2017 and
> doesn't affect modern targets. See "Design decisions" #4 for
> the full reasoning.

The protocol is line-delimited:

```
client → server: AUTH <hex secret>\n
server → client: OK\n  or  ERR <reason>\n

client → server: FORK <port>\n
server → client: OK <childID> <pid>\n  or  ERR <reason>\n

client → server: KILL <childID>\n
server → client: OK\n  or  ERR <reason>\n

client → server: STATUS\n
server → client: <childID> <pid> <port> <state>\n... END\n

client → server: INFO\n
server → client: <key>=<value>\n... END\n
                 # Static, queried once at construction. Known keys:
                 # r_version, preload_ms. Phase 3-10 adds ksm_status
                 # and ksm_errno. Parser ignores unknown keys
                 # (forward-compatible), so the 3-10 additions land
                 # without a protocol bump.

server → client (push, async): CHILDEXIT <childID> <exitCode> <reason>\n
```

```go
package zygotectl

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "log/slog"
    "net"
    "strconv"
    "strings"
    "sync"
)

// Client speaks the zygote control protocol over a single
// long-lived TCP connection. Used by both backend Forking
// implementations. The reader goroutine dispatches request
// responses to the requesting goroutine via a per-request channel;
// CHILDEXIT pushes go to the Exits channel.
//
// Concurrency: requests on a single client are serialised on
// `reqMu`. Two HTTP handlers calling Manager.Fork against the same
// zygote at the same time is a normal pattern (multiple sessions
// arrive in parallel), so the second caller waits rather than
// erroring. The protocol stays request/response — no in-flight
// IDs — because Fork latency is dominated by the actual fork(2)
// in R, and serialising at the client adds negligible overhead
// compared to the inherent cost.
type Client struct {
    addr   string
    secret []byte

    info Info // queried once at construction, read-only after

    reqMu sync.Mutex // serialises request/response cycles

    mu      sync.Mutex
    conn    net.Conn
    reader  *bufio.Reader
    pending chan reply // current in-flight request reply

    Exits  chan ChildExitMsg // CHILDEXIT pushes from server
    closed chan struct{}
}

type reply struct {
    line string
    err  error
}

// ChildExitMsg is the value pushed on Exits when the zygote reports
// a child has terminated. The backend Forking impl translates this
// into backend.ChildExit and forwards it on its own channel.
type ChildExitMsg struct {
    ChildID  string
    ExitCode int
    Reason   string
}

// NewClient dials, authenticates, queries zygote info, then
// starts the background reader goroutine. Returns a ready client;
// the caller is responsible for Close(). The setup is strictly
// sequential — auth → INFO → start reader — because both auth and
// INFO read directly from the connection and would race the reader
// if it were already running.
func NewClient(ctx context.Context, addr string, secret []byte) (*Client, error) {
    var d net.Dialer
    conn, err := d.DialContext(ctx, "tcp", addr)
    if err != nil {
        return nil, fmt.Errorf("control: dial: %w", err)
    }

    cc := &Client{
        addr:   addr,
        secret: secret,
        conn:   conn,
        reader: bufio.NewReader(conn),
        Exits:  make(chan ChildExitMsg, 16),
        closed: make(chan struct{}),
    }

    if err := cc.authenticate(); err != nil {
        conn.Close()
        return nil, err
    }

    info, err := cc.fetchInfo(cc.reader)
    if err != nil {
        conn.Close()
        return nil, fmt.Errorf("control: fetch info: %w", err)
    }
    cc.info = info
    slog.Info("zygote: control client ready",
        "addr", addr,
        "r_version", info.RVersion,
        "preload_ms", info.PreloadMS)

    go cc.readLoop()
    return cc, nil
}

func (c *Client) authenticate() error {
    // Send AUTH <hex secret>\n; expect OK\n.
    line := fmt.Sprintf("AUTH %x\n", c.secret)
    if _, err := c.conn.Write([]byte(line)); err != nil {
        return fmt.Errorf("control: auth write: %w", err)
    }
    resp, err := c.reader.ReadString('\n')
    if err != nil {
        return fmt.Errorf("control: auth read: %w", err)
    }
    if strings.TrimSpace(resp) != "OK" {
        return fmt.Errorf("control: auth rejected: %s", strings.TrimSpace(resp))
    }
    return nil
}

// Fork sends FORK <port> and returns (childID, pid).
func (c *Client) Fork(ctx context.Context, port int) (childID string, pid int, err error) {
    line, err := c.request(ctx, fmt.Sprintf("FORK %d\n", port))
    if err != nil {
        return "", 0, err
    }
    // Expect: "OK <childID> <pid>"
    fields := strings.Fields(line)
    if len(fields) != 3 || fields[0] != "OK" {
        return "", 0, fmt.Errorf("control: bad FORK reply: %q", line)
    }
    pid, err = strconv.Atoi(fields[2])
    if err != nil {
        return "", 0, fmt.Errorf("control: bad FORK pid: %q", fields[2])
    }
    return fields[1], pid, nil
}

// Kill sends KILL <childID>. Idempotent.
func (c *Client) Kill(ctx context.Context, childID string) error {
    line, err := c.request(ctx, fmt.Sprintf("KILL %s\n", childID))
    if err != nil {
        return err
    }
    if !strings.HasPrefix(line, "OK") {
        return fmt.Errorf("control: KILL rejected: %s", line)
    }
    return nil
}

// Info is the structured view of a zygote's startup state,
// populated from the INFO control command. Unknown keys are
// ignored so the protocol can be extended backward-compatibly
// — phase 3-10 adds ksm_status and ksm_errno without a schema
// bump via this mechanism.
type Info struct {
    RVersion  string
    PreloadMS int
    Unknown   map[string]string // forward-compat: unrecognised keys
}

// Info returns the cached zygote info populated at client
// construction. Does not reach over the network — INFO is queried
// once during NewClient before the reader goroutine starts.
func (c *Client) Info() Info { return c.info }

// fetchInfo is called synchronously during NewClient after
// authentication but before the readLoop goroutine starts. It owns
// the connection exclusively at this point, so it can read the
// multi-line INFO response directly without coordinating with the
// reader.
func (c *Client) fetchInfo(rd *bufio.Reader) (Info, error) {
    if _, err := c.conn.Write([]byte("INFO\n")); err != nil {
        return Info{}, fmt.Errorf("control: info write: %w", err)
    }
    info := Info{Unknown: map[string]string{}}
    for {
        line, err := rd.ReadString('\n')
        if err != nil {
            return Info{}, fmt.Errorf("control: info read: %w", err)
        }
        line = strings.TrimSpace(line)
        if line == "END" {
            return info, nil
        }
        key, val, ok := strings.Cut(line, "=")
        if !ok {
            continue
        }
        switch key {
        case "r_version":
            info.RVersion = val
        case "preload_ms":
            info.PreloadMS, _ = strconv.Atoi(val)
        default:
            info.Unknown[key] = val
        }
    }
}

// request serialises one single-line request/response pair on the
// connection. Used by FORK and KILL. Concurrent callers queue on
// reqMu — the protocol has no in-flight IDs, so the next request
// can only be sent after the previous reply has landed. The lock
// is held briefly (one write + one read from the response channel),
// so the queue depth is bounded by the rate of incoming Fork calls
// and not by anything blocking on R.
//
// Phase 3-10 adds a multi-line variant for the STATS command;
// extending this loop to accumulate lines until "END" is a small
// delta. See phase-3-10-draft.md Step K5.
func (c *Client) request(ctx context.Context, line string) (string, error) {
    c.reqMu.Lock()
    defer c.reqMu.Unlock()

    c.mu.Lock()
    ch := make(chan reply, 1)
    c.pending = ch
    _, err := c.conn.Write([]byte(line))
    c.mu.Unlock()
    if err != nil {
        c.mu.Lock()
        c.pending = nil
        c.mu.Unlock()
        return "", fmt.Errorf("control: write: %w", err)
    }

    select {
    case r := <-ch:
        return r.line, r.err
    case <-ctx.Done():
        return "", ctx.Err()
    case <-c.closed:
        return "", errors.New("control: connection closed")
    }
}

// readLoop reads frames from the control connection and dispatches
// them. Synchronous request replies go to the pending channel;
// CHILDEXIT pushes go to Exits. Uses the same bufio.Reader that
// auth and fetchInfo used during construction, so no buffered
// bytes are lost across the handoff. Single-line replies only in
// phase 3-9 (FORK/KILL); phase 3-10 extends this to accumulate
// multi-line STATS responses terminated by "END".
func (c *Client) readLoop() {
    defer close(c.closed)
    for {
        line, err := c.reader.ReadString('\n')
        if err != nil {
            return
        }
        line = strings.TrimSpace(line)
        if strings.HasPrefix(line, "CHILDEXIT ") {
            c.pushExit(line)
            continue
        }

        c.mu.Lock()
        ch := c.pending
        c.pending = nil
        c.mu.Unlock()

        if ch == nil {
            // No pending request — unexpected reply, drop.
            continue
        }
        ch <- reply{line: line}
    }
}

func (c *Client) pushExit(line string) {
    // "CHILDEXIT <childID> <exitCode> <reason>"
    fields := strings.Fields(line)
    if len(fields) != 4 {
        return
    }
    code, _ := strconv.Atoi(fields[2])
    select {
    case c.Exits <- ChildExitMsg{ChildID: fields[1], ExitCode: code, Reason: fields[3]}:
    default:
        // Drop if channel is full — exits should be drained promptly.
    }
}

// Done returns a channel that closes when the control connection
// has been lost — either through an explicit Close call or because
// the reader goroutine observed a read error. Backend Forking
// implementations listen on this to detect zygote death and trigger
// worker eviction. See Step 7 / Step 8 for the watcher goroutine.
func (c *Client) Done() <-chan struct{} {
    return c.closed
}

// Close shuts down the client. Safe to call multiple times — the
// underlying conn.Close is idempotent, and Done() fires exactly
// once when readLoop observes the closed conn.
func (c *Client) Close() error {
    return c.conn.Close()
}
```

**`internal/zygote/manager.go`** — backend-agnostic orchestration.
Wraps a `backend.Forking` implementation and adds session ↔ child
bookkeeping plus exit-event handling.

```go
package zygote

import (
    "context"
    "fmt"
    "log/slog"
    "sync"
    "time"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/session"
)

// Manager owns the session ↔ child bookkeeping for zygote workers.
// It is backend-agnostic — the backend-specific bits live in the
// Forking implementation it wraps. Phase 3-10 extends this type
// with a metrics-poll loop for KSM observability (see
// phase-3-10-draft.md Step K8); phase 3-9 ships the session
// bookkeeping and sweep loop only.
type Manager struct {
    forking       backend.Forking
    sessions      session.Store
    sweepInterval time.Duration

    mu        sync.Mutex
    bySession map[string]childRef // sessionID → (workerID, childID)

    stop chan struct{}
}

type childRef struct {
    workerID string
    childID  string
}

// ManagerConfig bundles the optional dependencies so NewManager
// doesn't grow a long positional parameter list.
type ManagerConfig struct {
    SweepInterval time.Duration
}

func NewManager(forking backend.Forking, sessions session.Store, cfg ManagerConfig) *Manager {
    if cfg.SweepInterval <= 0 {
        cfg.SweepInterval = 30 * time.Second
    }
    m := &Manager{
        forking:       forking,
        sessions:      sessions,
        sweepInterval: cfg.SweepInterval,
        bySession:     make(map[string]childRef),
        stop:          make(chan struct{}),
    }
    go m.exitLoop()
    go m.sweepLoop()
    return m
}

// Stop terminates the sweep and exit loops. Safe to call once.
func (m *Manager) Stop() { close(m.stop) }

// Fork creates a child for sessionID inside the given worker.
// Returns the child's network address. The mapping from sessionID
// to childID is recorded so KillChild can find the right child
// later, and so the child-exit handler can identify the session.
func (m *Manager) Fork(ctx context.Context, workerID, sessionID string) (addr string, err error) {
    addr, childID, err := m.forking.Fork(ctx, workerID, sessionID)
    if err != nil {
        return "", fmt.Errorf("zygote: fork: %w", err)
    }
    m.mu.Lock()
    m.bySession[sessionID] = childRef{workerID: workerID, childID: childID}
    m.mu.Unlock()
    slog.Debug("zygote: forked",
        "worker_id", workerID, "session_id", sessionID,
        "child_id", childID, "addr", addr)
    return addr, nil
}

// killSession terminates the child bound to sessionID, if any.
// Internal helper used by sweepLoop and the explicit-cleanup paths
// in Step 11. Best-effort; failures are logged.
func (m *Manager) killSession(ctx context.Context, sessionID string) {
    m.mu.Lock()
    ref, ok := m.bySession[sessionID]
    if ok {
        delete(m.bySession, sessionID)
    }
    m.mu.Unlock()
    if !ok {
        return
    }
    if err := m.forking.KillChild(ctx, ref.workerID, ref.childID); err != nil {
        slog.Warn("zygote: kill failed",
            "worker_id", ref.workerID, "child_id", ref.childID, "error", err)
    }
}

// HasChild returns true if a session has a tracked child. Called
// by the proxy on the hot path (Step 10) to gate the unreachable-
// child probe, and by tests.
func (m *Manager) HasChild(sessionID string) bool {
    m.mu.Lock()
    _, ok := m.bySession[sessionID]
    m.mu.Unlock()
    return ok
}

// sweepLoop is the authoritative cleanup path for sessions that
// disappeared without going through ChildExits — TTL expiry on
// Redis, explicit Sessions.Delete from logout / OIDC mismatch /
// admin tooling, partial-failure paths, etc. Runs every
// sweepInterval, snapshots bySession under the lock, then probes
// session.Store for each entry. Children whose session is gone get
// killed and dropped from bySession.
func (m *Manager) sweepLoop() {
    t := time.NewTicker(m.sweepInterval)
    defer t.Stop()
    for {
        select {
        case <-t.C:
            m.sweepOnce()
        case <-m.stop:
            return
        }
    }
}

func (m *Manager) sweepOnce() {
    // Snapshot first so we don't hold the Manager mutex across
    // session-store calls (which on Redis is a network round-trip).
    m.mu.Lock()
    snapshot := make(map[string]childRef, len(m.bySession))
    for sid, ref := range m.bySession {
        snapshot[sid] = ref
    }
    m.mu.Unlock()

    var orphaned []string
    for sid := range snapshot {
        if _, ok := m.sessions.Get(sid); !ok {
            orphaned = append(orphaned, sid)
        }
    }
    if len(orphaned) == 0 {
        return
    }
    slog.Info("zygote: sweep killing orphaned children",
        "count", len(orphaned))
    ctx := context.Background()
    for _, sid := range orphaned {
        m.killSession(ctx, sid)
    }
}

// exitLoop drains the Forking ChildExits channel and removes the
// corresponding session entries. Runs for the lifetime of the
// Manager.
func (m *Manager) exitLoop() {
    for ev := range m.forking.ChildExits() {
        m.handleExit(ev)
    }
}

func (m *Manager) handleExit(ev backend.ChildExit) {
    // Find the session(s) bound to this child.
    m.mu.Lock()
    var matched []string
    for sid, ref := range m.bySession {
        if ref.workerID == ev.WorkerID && ref.childID == ev.ChildID {
            matched = append(matched, sid)
            delete(m.bySession, sid)
        }
    }
    m.mu.Unlock()
    for _, sid := range matched {
        m.sessions.Delete(sid)
    }
    if ev.Reason == "crashed" || ev.Reason == "oom" {
        slog.Warn("zygote: child exited unexpectedly",
            "worker_id", ev.WorkerID, "child_id", ev.ChildID,
            "exit_code", ev.ExitCode, "reason", ev.Reason,
            "sessions", matched)
    } else {
        slog.Debug("zygote: child exited",
            "worker_id", ev.WorkerID, "child_id", ev.ChildID,
            "reason", ev.Reason)
    }
}
```

**`internal/zygotectl/secret.go`** — generation and read-back of
the per-worker control secret. Used by both backend `Forking` impls
when spawning a zygote and by the cleanup path. Lives in
`zygotectl` (not `zygote`) because it has no dependency on
`backend`, and the backend packages already import `zygotectl`
for the `Client` type.

```go
package zygotectl

import (
    "crypto/rand"
    "fmt"
    "os"
    "path/filepath"
)

// SecretBytes is the size of the per-worker control secret.
const SecretBytes = 32

// WriteSecret generates a fresh secret and writes it to the given
// directory as `control.secret` (mode 0600). Returns the bytes for
// the server to keep in memory.
func WriteSecret(tokenDir string) ([]byte, error) {
    buf := make([]byte, SecretBytes)
    if _, err := rand.Read(buf); err != nil {
        return nil, fmt.Errorf("zygote: secret rand: %w", err)
    }
    path := filepath.Join(tokenDir, "control.secret")
    if err := os.WriteFile(path, buf, 0o600); err != nil {
        return nil, fmt.Errorf("zygote: secret write: %w", err)
    }
    return buf, nil
}
```

The secret lives in the existing per-worker token directory
(`{BundleServerPath}/.worker-tokens/{workerID}/control.secret`),
which is already mounted read-only into the worker container at
`/var/run/blockyard/`. The zygote reads it from
`/var/run/blockyard/control.secret`.

### Step 6: `internal/zygotectl/zygote.R`

The zygote R script. Embedded into the server binary via `//go:embed`.
Loads packages, listens on the control port, handles control commands,
forks children, reaps via `mc.waitpid` on a 100ms `socketSelect` poll.

**`internal/zygotectl/zygote.R`:**

```r
# blockyard zygote — long-lived R process that pre-loads packages
# and forks per-session children on demand.
#
# Threading model: strictly single-threaded. We use base R's
# `socketSelect` (which calls select(2) directly) to drive a
# non-blocking poll loop. No httpuv, no later, no background
# threads — those are all incompatible with parallel::mcfork
# per R core's own documentation. CHILDEXIT timeliness is bounded
# by the poll interval (100ms).
#
# Reads from environment:
#   BLOCKYARD_BUNDLE_PATH      — path to the unpacked bundle
#   BLOCKYARD_CONTROL_PORT     — TCP port to listen on for control
#   BLOCKYARD_CONTROL_BIND     — IP to bind ("0.0.0.0" by default)
#   BLOCKYARD_SECRET_PATH      — path to control.secret file
#   BLOCKYARD_PORT_RANGE       — "lo-hi" port range for forked children
#   R_LIBS                     — set externally for the worker library
#
# Protocol: see internal/zygotectl/control.go for the wire format.

POLL_SECS <- 0.1  # bounds CHILDEXIT push latency

bundle_path  <- Sys.getenv("BLOCKYARD_BUNDLE_PATH",  "/shiny")
control_port <- as.integer(Sys.getenv("BLOCKYARD_CONTROL_PORT", "3837"))
secret_path  <- Sys.getenv("BLOCKYARD_SECRET_PATH",  "/var/run/blockyard/control.secret")
port_range   <- Sys.getenv("BLOCKYARD_PORT_RANGE",   "3839-3938")

# Read the control secret. Cached at startup and compared on each
# AUTH frame.
secret_bytes <- readBin(secret_path, what = "raw", n = 32)
secret_hex   <- paste(format(as.hexmode(as.integer(secret_bytes)), width = 2),
                      collapse = "")

# zygote_info holds structured facts about this zygote that the
# blockyard server can query via the control-protocol `INFO`
# command. Filled in during startup; never mutated after that.
# Phase 3-10 adds ksm_status / ksm_errno fields here when KSM
# support lands (see phase-3-10-draft.md Step K6).
zygote_info <- list(
  r_version  = R.version.string,
  preload_ms = NA_integer_
)

# Pre-load the bundle. We source app.R under a stubbed shinyApp()
# so packages are loaded and the shinyApp arguments are captured
# without starting the shiny server. Children then call
# runApp(captured_app, ...) on the captured object, which means
# app.R is parsed and evaluated exactly once, in the zygote.
#
# Phase 3-10 replaces the naive source() with compiler::cmpfile()
# so bundle closures are born as BCODESXP before fork (see
# phase-3-10-draft.md decision K3). That optimisation is tied to
# the KSM track because its primary motivation is preventing the
# JIT from mutating closure pages post-fork; it also saves 100–500ms
# per cold start independently, but we keep the simpler source()
# path in phase 3-9 to match the minimal mechanism scope.
#
# Crashes here are fatal — the zygote is unusable if its bundle
# didn't load.
captured_app <- NULL  # shiny.appobj captured during app.R evaluation

preload_bundle <- function() {
  env <- new.env(parent = globalenv())
  env$shinyApp <- function(ui, server, ...) {
    captured_app <<- shiny::shinyApp(ui, server, ...)
    invisible(NULL)
  }
  env$runApp <- function(appDir = ".", ...) invisible(NULL)

  load_if_present <- function(src) {
    if (file.exists(src)) sys.source(src, envir = env)
  }
  load_if_present(file.path(bundle_path, "global.R"))
  load_if_present(file.path(bundle_path, "app.R"))
}

preload_start <- Sys.time()
preload_bundle()
zygote_info$preload_ms <- as.integer(
  as.numeric(Sys.time() - preload_start, units = "secs") * 1000
)
if (is.null(captured_app)) {
  message("blockyard_zygote event=preload status=ok ms=",
          zygote_info$preload_ms,
          " warning=no_shinyapp_captured")
} else {
  message("blockyard_zygote event=preload status=ok ms=",
          zygote_info$preload_ms)
}

# Hygiene: full GC after package preload. Puts every surviving
# SEXP into the oldest generation with stable mark state, so
# children fork from a clean, deterministic heap.
gc(full = TRUE)
message("blockyard_zygote event=gc_hygiene status=ok")

# Parse the port range (validation only — actual port allocation
# is server-side and passed in via FORK).
port_range_parts <- as.integer(strsplit(port_range, "-")[[1]])
port_lo <- port_range_parts[1]
port_hi <- port_range_parts[2]

# Active children: childID → list(pid, port).
children <- new.env(parent = emptyenv())

# Pending CHILDEXIT events that haven't been pushed yet — either
# because no client is connected (rare; the server normally keeps
# a long-lived connection) or because reap_children() ran in the
# same poll tick and we want to drain them on the next write.
pending_events <- character(0)

# Allocate a child ID. Monotonic counter, short hex.
child_id_counter <- 0L
next_child_id <- function() {
  child_id_counter <<- child_id_counter + 1L
  sprintf("c%x", child_id_counter)
}

# Active control connection. NULL when no client connected.
con <- NULL

push_event <- function(line) {
  if (!is.null(con)) {
    ok <- tryCatch({
      writeLines(line, con, sep = "")
      TRUE
    }, error = function(e) FALSE)
    if (ok) return(invisible())
  }
  # Buffer for the next connection. The buffer is per-zygote and
  # bounded by the number of children that can exit between
  # connections — small in practice. No size cap here because
  # children are bounded by the port allocator on the server side.
  pending_events <<- c(pending_events, line)
}

flush_pending <- function() {
  if (length(pending_events) == 0L || is.null(con)) return(invisible())
  for (line in pending_events) {
    ok <- tryCatch({
      writeLines(line, con, sep = "")
      TRUE
    }, error = function(e) FALSE)
    if (!ok) return(invisible())
  }
  pending_events <<- character(0)
}

# Reap exited child processes via parallel::mc.waitpid (non-blocking).
# Called every poll tick.
reap_children <- function() {
  for (cid in ls(children)) {
    info <- get(cid, envir = children)
    res <- tryCatch(parallel:::mc.waitpid(info$pid, FALSE),
                    error = function(e) NULL)
    # mc.waitpid returns 0 for "still running", a positive pid for
    # "exited", or NA for an error. We treat anything non-zero
    # positive as "exited".
    if (!is.null(res) && !is.na(res) && res > 0) {
      rm(list = cid, envir = children)
      reason <- "normal"  # exit code parsing not yet done; phase 3-10 refines
      push_event(sprintf("CHILDEXIT %s %d %s\n", cid, 0L, reason))
    }
  }
}

handle_command <- function(line) {
  parts <- strsplit(line, " ", fixed = TRUE)[[1]]
  cmd <- parts[1]
  if (cmd == "FORK") {
    port <- as.integer(parts[2])
    if (is.na(port) || port < port_lo || port > port_hi) {
      writeLines(sprintf("ERR port %s out of range\n", parts[2]), con, sep = "")
      return()
    }
    cid <- next_child_id()
    pid <- parallel:::mcfork()
    if (inherits(pid, "masterProcess")) {
      # Child: close the inherited control connection, run the app
      # on the assigned port. runApp receives the shiny.appobj that
      # was captured during zygote preload — no re-sourcing of
      # app.R in the child.
      #
      # Phase 3-10 adds an oom_score_adj=1000 self-write here so
      # the kernel OOM killer reaps a child before the zygote
      # under memory pressure (see phase-3-10-draft.md decision K5).
      tryCatch({
        close(con)
        Sys.setenv(SHINY_PORT = as.character(port))
        if (is.null(captured_app)) {
          # Fallback: bundle did not call shinyApp() at the top
          # level (unusual structure, e.g. ui.R + server.R split,
          # or a conditional shinyApp() call). Re-source in the
          # child, accepting the per-child re-parse cost.
          shiny::runApp(bundle_path, port = port)
        } else {
          shiny::runApp(captured_app, port = port)
        }
      }, error = function(e) {
        message("blockyard zygote child error: ", conditionMessage(e))
      })
      parallel:::mcexit(0L)
    }
    # Parent.
    assign(cid, list(pid = pid$pid, port = port), envir = children)
    writeLines(sprintf("OK %s %d\n", cid, pid$pid), con, sep = "")
  } else if (cmd == "KILL") {
    cid <- parts[2]
    if (exists(cid, envir = children)) {
      info <- get(cid, envir = children)
      tools::pskill(info$pid, tools::SIGTERM)
    }
    writeLines("OK\n", con, sep = "")  # idempotent
  } else if (cmd == "STATUS") {
    for (cid in ls(children)) {
      info <- get(cid, envir = children)
      writeLines(sprintf("%s %d %d alive\n", cid, info$pid, info$port),
                 con, sep = "")
    }
    writeLines("END\n", con, sep = "")
  } else if (cmd == "INFO") {
    # Structured zygote facts for the blockyard server to query at
    # startup and expose via API/UI. Key=value lines, terminated by
    # "END". Adding new fields is backward-compatible — the Go-side
    # parser skips unknown keys. Phase 3-10 adds ksm_status and
    # ksm_errno fields here.
    writeLines(sprintf("r_version=%s\n", zygote_info$r_version),
               con, sep = "")
    writeLines(sprintf("preload_ms=%d\n", zygote_info$preload_ms),
               con, sep = "")
    writeLines("END\n", con, sep = "")
  } else {
    writeLines(sprintf("ERR unknown command %s\n", cmd), con, sep = "")
  }
}

# Main loop. Uses base::socketSelect to interleave:
#   - accept on the server socket
#   - read from the active client connection (if any)
#   - reap_children() / push pending events
# The select call is the only blocking syscall; everything else
# returns promptly. POLL_SECS bounds CHILDEXIT push latency.
srv <- serverSocket(control_port)

repeat {
  # Build the poll set: server socket always; client connection if
  # one is open.
  socks <- if (is.null(con)) list(srv) else list(srv, con)
  ready <- tryCatch(
    socketSelect(socks, write = FALSE, timeout = POLL_SECS),
    error = function(e) rep(FALSE, length(socks))
  )

  # Always reap and try to flush, regardless of ready state.
  reap_children()
  flush_pending()

  # Server socket has a pending connection.
  if (isTRUE(ready[[1]])) {
    new_con <- tryCatch(
      socketAccept(srv, blocking = TRUE, open = "a+", timeout = 1),
      error = function(e) NULL
    )
    if (!is.null(new_con)) {
      # Replace any stale client connection with the new one.
      if (!is.null(con)) tryCatch(close(con), error = function(e) NULL)
      con <- new_con
      # AUTH must be the first frame on the new connection.
      auth <- tryCatch(readLines(con, n = 1), error = function(e) character())
      if (length(auth) == 0 || sub("^AUTH ", "", auth) != secret_hex) {
        tryCatch(writeLines("ERR auth\n", con, sep = ""),
                 error = function(e) NULL)
        tryCatch(close(con), error = function(e) NULL)
        con <- NULL
      } else {
        writeLines("OK\n", con, sep = "")
        flush_pending()  # drain any events queued before this connection
      }
    }
  }

  # Client connection has an inbound command.
  if (!is.null(con) && length(ready) >= 2L && isTRUE(ready[[2]])) {
    line <- tryCatch(readLines(con, n = 1), error = function(e) character())
    if (length(line) == 0) {
      # Connection closed cleanly or errored; drop it and wait for
      # the next AUTH.
      tryCatch(close(con), error = function(e) NULL)
      con <- NULL
    } else {
      handle_command(line)
    }
  }
}
```

Notes on the design:

- **Single-threaded throughout.** No `httpuv`, no `later`, no
  background threads. Verified compatible with `parallel::mcfork`
  by construction (R stock sockets and `socketSelect` are stock
  base R, both predate the multicore package).
- **`socketSelect` polling drives both accept and read.** The
  100ms timeout gives us bounded CHILDEXIT timeliness without
  busy-waiting. Below ~10ms the syscall overhead would start to
  matter; above ~500ms the user-visible latency on a child crash
  would be noticeable. 100ms is the comfortable middle.
- **Pending event buffer.** If a child exits while no client is
  connected (rare in practice — the server keeps a long-lived
  connection per zygote), the event is held in `pending_events`
  and flushed when the next connection authenticates. The buffer
  is unbounded but bounded in practice by the port allocator's
  `max_sessions_per_worker`.
- **Single client at a time.** A new connection replaces the
  previous one. The blockyard server maintains exactly one
  control connection per zygote, so this is the normal pattern.
- **R 3.4.3+ required.** Earlier R versions had a bug where
  fractional `socketSelect` timeouts hung indefinitely on Linux
  (HenrikBengtsson/Wishlist-for-R#35). The fix shipped in 2017
  and is well below any plausible runtime target.

Embed the R script in `internal/zygotectl/embed.go`:

```go
package zygotectl

import _ "embed"

//go:embed zygote.R
var ZygoteScript []byte
```

Phase 3-10 extends this file with a `zygote_helper.c` source embed
plus per-architecture `HelperSO` embeds for the compiled
`.so` binaries (see `phase-3-10-draft.md` Step K4). Phase 3-9
ships only the R script.

### Step 7: Docker `Forking` implementation

New file `internal/backend/docker/forking.go`. Implements the
`backend.Forking` interface for the Docker backend. Uses the
shared `zygotectl.Client` for the wire protocol.

Key elements:

```go
package docker

import (
    "context"
    "fmt"
    "log/slog"
    "sync"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/zygotectl"
)

// dockerForking adds the Forking capability to DockerBackend.
// It is composed into DockerBackend rather than a separate type
// so type assertions on backend.Forking just work.

// Per-worker control state, kept on DockerBackend alongside workers.
// childPort is a small in-package free-list over the container-
// internal child port range. Each container has its own copy of
// the range on its own bridge, so allocation is purely per-worker
// bookkeeping — there's no host-side `net.Listen` probe to
// perform (the ports live inside the container's network
// namespace, not on the host), and therefore no reuse of the
// phase-3-8 host-side `portAllocator` interface here. The process
// backend uses a single host-wide phase-3-8 allocator instead
// (Step 8) because its child ports *are* host ports.
type forkState struct {
    client      *zygotectl.Client
    secret      []byte
    portRangeLo int
    portRangeHi int
    childAddrs  map[string]string // childID → "ip:port"
    childPort   map[string]int    // childID → port (for free-list bookkeeping)
    mu          sync.Mutex
}

// Fork implements backend.Forking.
func (d *DockerBackend) Fork(ctx context.Context, workerID, sessionID string) (string, string, error) {
    d.mu.Lock()
    ws, ok := d.workers[workerID]
    d.mu.Unlock()
    if !ok || ws.fork == nil {
        return "", "", fmt.Errorf("zygote: worker %s is not a zygote", workerID)
    }

    ws.fork.mu.Lock()
    port := ws.fork.allocPortLocked()
    ws.fork.mu.Unlock()
    if port == 0 {
        return "", "", fmt.Errorf("zygote: no free ports for worker %s", workerID)
    }

    childID, _, err := ws.fork.client.Fork(ctx, port)
    if err != nil {
        ws.fork.mu.Lock()
        ws.fork.releasePortLocked(port)
        ws.fork.mu.Unlock()
        return "", "", err
    }

    addr, err := d.zygoteContainerAddr(ctx, ws, port)
    if err != nil {
        // Best-effort kill, then bubble the error.
        _ = ws.fork.client.Kill(ctx, childID)
        ws.fork.mu.Lock()
        ws.fork.releasePortLocked(port)
        ws.fork.mu.Unlock()
        return "", "", err
    }

    ws.fork.mu.Lock()
    ws.fork.childAddrs[childID] = addr
    ws.fork.childPort[childID] = port
    ws.fork.mu.Unlock()

    return addr, childID, nil
}

// KillChild implements backend.Forking.
func (d *DockerBackend) KillChild(ctx context.Context, workerID, childID string) error {
    d.mu.Lock()
    ws, ok := d.workers[workerID]
    d.mu.Unlock()
    if !ok || ws.fork == nil {
        return nil // worker gone — child is implicitly dead
    }
    err := ws.fork.client.Kill(ctx, childID)
    ws.fork.mu.Lock()
    if port, hasPort := ws.fork.childPort[childID]; hasPort {
        ws.fork.releasePortLocked(port)
        delete(ws.fork.childPort, childID)
    }
    delete(ws.fork.childAddrs, childID)
    ws.fork.mu.Unlock()
    return err
}

// ChildExits implements backend.Forking. Returns a single channel
// fed by all per-worker control clients.
func (d *DockerBackend) ChildExits() <-chan backend.ChildExit {
    return d.childExits
}

// allocPortLocked / releasePortLocked: simple per-container free
// list. Container-internal ports — no kernel probe, no host-side
// listener hold, no cross-worker collision concern (every container
// has its own copy of the range on its own bridge network).
func (s *forkState) allocPortLocked() int {
    for p := s.portRangeLo; p <= s.portRangeHi; p++ {
        used := false
        for _, q := range s.childPort {
            if q == p {
                used = true
                break
            }
        }
        if !used {
            return p
        }
    }
    return 0
}

func (s *forkState) releasePortLocked(_ int) { /* free-list lookup is the map; no extra state */ }
```

`d.childExits` is a `chan backend.ChildExit` initialised in
`NewDockerBackend`. A goroutine per worker translates
`zygotectl.ChildExitMsg` into `backend.ChildExit` and forwards
onto the shared channel:

```go
// In DockerBackend.Spawn, after the control client is connected
// for a zygote worker:
go func() {
    for msg := range ws.fork.client.Exits {
        d.childExits <- backend.ChildExit{
            WorkerID: spec.WorkerID,
            ChildID:  msg.ChildID,
            ExitCode: msg.ExitCode,
            Reason:   msg.Reason,
        }
    }
}()
```

**Control-connection watcher.** A second goroutine per zygote
worker watches `client.Done()` and evicts the worker if the
control channel dies unexpectedly. This closes the gap where a
zygote crash or container OOM would otherwise leave a stale
worker sitting around until the autoscaler's minutes-scale idle
sweep evicted it:

```go
// In DockerBackend.Spawn, right after the translator goroutine:
go func() {
    <-ws.fork.client.Done()
    // Distinguish "normal shutdown" from "unexpected disconnect".
    // ws.stopping is set by Stop before it tears down the control
    // client; the watcher bails out if this is a graceful stop.
    d.mu.Lock()
    stopping := ws.stopping
    d.mu.Unlock()
    if stopping {
        return
    }
    slog.Warn("zygote: control connection lost, evicting worker",
        "worker_id", spec.WorkerID)
    // Fire through the normal stop path. Stop() is idempotent and
    // handles the race with any concurrent call.
    if err := d.Stop(context.Background(), spec.WorkerID); err != nil {
        slog.Error("zygote: eviction after control loss failed",
            "worker_id", spec.WorkerID, "error", err)
    }
}()
```

`ws.stopping` is a new field on `workerState` — a plain bool
guarded by `d.mu`. `DockerBackend.Stop` sets it to `true` at the
very beginning of the method (before closing the control client
or calling `ContainerStop`), so the watcher sees it and exits
cleanly when an explicit `Stop` races the disconnect signal.

`DockerBackend.Stop` itself must be idempotent and safe against
concurrent invocation. Two call sites can race: an explicit
`Stop` from `ops.EvictWorker` / the autoscaler, and the watcher
above. The existing `Stop` body already deletes the worker from
`d.workers` under the mutex at the start; we keep that pattern
but also check whether the deletion found an entry before
proceeding with teardown. If the entry is already gone, return
nil — another call is handling it.

**Container address resolution for children.** The existing `Addr()`
returns `containerIP:shinyPort`. For children we need
`containerIP:childPort`. Add a helper:

```go
func (d *DockerBackend) zygoteContainerAddr(ctx context.Context, ws *workerState, port int) (string, error) {
    info, err := d.client.ContainerInspect(ctx, ws.containerID, client.ContainerInspectOptions{})
    if err != nil {
        return "", fmt.Errorf("addr: inspect: %w", err)
    }
    endpoint, ok := info.Container.NetworkSettings.Networks[ws.networkName]
    if !ok {
        return "", fmt.Errorf("addr: container not on network %s", ws.networkName)
    }
    return fmt.Sprintf("%s:%d", endpoint.IPAddress, port), nil
}
```

**Spawn changes for zygote workers** — `DockerBackend.Spawn` checks
`spec.Zygote`. When set:

1. The container `Cmd` becomes `["R", "-f", "/blockyard/zygote.R"]`
   instead of the current `shiny::runApp(...)`.
2. Additional env vars: `BLOCKYARD_BUNDLE_PATH`,
   `BLOCKYARD_CONTROL_PORT=3837`, `BLOCKYARD_PORT_RANGE`,
   `BLOCKYARD_SECRET_PATH=/var/run/blockyard/control.secret`.
3. One bind mount added under `/blockyard/` (read-only): Host
   `{BundleServerPath}/.zygote/zygote.R` → container
   `/blockyard/zygote.R` (the embedded R script). Phase 3-10
   adds a second bind mount for the KSM helper `.so` alongside
   this one.
4. After `ContainerStart`, the server waits for the control port
   to accept connections (TCP probe with backoff), then
   `zygotectl.NewClient(ctx, "ip:3837", secret)`. The client
   constructor synchronously fetches `INFO` and logs the R
   version and preload time.
5. The connected `*zygotectl.Client` is stored in `ws.fork`,
   along with the cached `zygotectl.Info` (`ws.fork.info =
   client.Info()`) so startup info is available for API/UI
   exposure without another network round-trip.

**`Backend.Addr` and `HealthCheck` for zygote workers.** Both
methods inspect `ws.fork` and branch:

- Non-zygote workers: existing behaviour — `Addr` returns
  `containerIP:shinyPort`, `HealthCheck` dials the shiny port.
- Zygote workers: `Addr` returns `containerIP:controlPort`
  (3837 by default), `HealthCheck` dials the same. The zygote
  being responsive on the control port is the right liveness
  signal; the shiny port is never bound on a zygote container.

The control address registered in `srv.Registry` is therefore
non-empty and meaningful even though no proxy traffic ever flows
through it (the proxy uses `entry.Addr`, which holds the per-child
shiny address). This keeps `Registry.Get` from looking like a
stale/missing entry to anything that probes it (autoscaler, ops
tooling).

**`Backend.Stop` for zygote workers.** Before tearing down the
container, drain the tracked children and synthesise
`backend.ChildExit` events for each onto `d.childExits` with
`Reason = "killed"`. The zygote's control connection dies as soon
as the container stops, so the natural CHILDEXIT push path is
unavailable — without the synthetic events, `zygote.Manager`'s
`bySession` map keeps stale references to children that no longer
exist.

```go
// In DockerBackend.Stop, before ContainerStop:
if ws.fork != nil {
    ws.fork.mu.Lock()
    children := make([]string, 0, len(ws.fork.childAddrs))
    for childID := range ws.fork.childAddrs {
        children = append(children, childID)
    }
    ws.fork.mu.Unlock()
    for _, childID := range children {
        d.childExits <- backend.ChildExit{
            WorkerID: id,
            ChildID:  childID,
            Reason:   "killed",
        }
    }
}
```

The same logic lives in a helper called from both `Stop` and the
container-exit detection path (`ContainerWait` or the autoscaler's
`evictUnhealthy` follow-up), so unexpected container exits also
synthesise the events. The helper is idempotent — calling it twice
on the same worker drains an empty child set on the second call.

`WorkerSpec` gets a small extension:

```go
type WorkerSpec struct {
    // ...existing fields...
    Zygote        bool   // zygote mode
    ControlSecret []byte // 32-byte secret to bind into the worker
    ChildPortRange string // "lo-hi" for child ports inside the container
                          // (Docker zygote mode only; process backend uses
                          // its own host-wide childPorts allocator)
}
```

`ControlSecret` is generated by the cold-start path via
`zygotectl.WriteSecret(tokenDir)` and attached to the spec.
`Zygote` is populated from the *effective* state computed via
`effectiveZygoteState(srv.Config, app)` in `coldstart.go` (see
Step 9 for the helper). This is the **runtime kill switch**
from decision #15 — flipping `experimental.zygote` off in
server config disables the feature for every app without
touching the database. Apps with `apps.zygote = true` in the db
silently fall through to the non-zygote path when the
server-wide flag is off. Phase 3-10 adds a `KSM bool` field
here when the KSM opt-in lands.

`ChildPortRange` is read from a new `[docker] zygote_child_port_range`
config field (default `3839-3938`) and is only meaningful for the
Docker backend — each container gets its own copy of the range on
its own bridge network. The process backend ignores this field
and allocates child ports from `ProcessConfig.ZygoteChildRange*`
instead.

### Step 8: Process backend `Forking` implementation

Mirror of step 7 for the process backend. Lives in
`internal/backend/process/forking.go` (a new file inside the package
that phase 3-7 establishes). Differences from the Docker version:

- Spawn: bwrap invocation with one bind mount under `/blockyard/`:
  `--ro-bind {bundleServerPath}/.zygote/zygote.R /blockyard/zygote.R`.
  The per-worker token dir comes in via
  `--ro-bind {tokenDir} /var/run/blockyard`. The R command is
  `R -f /blockyard/zygote.R`. Phase 3-10 adds a second bind
  mount for the KSM helper.
- Control transport: TCP on `127.0.0.1:{allocatedControlPort}`.
  The control port is allocated from a dedicated host-wide range
  (see "Port allocator extension" below). The bwrap sandbox shares
  the host network namespace, so the loopback dial works.
- Child port allocation: lazy from a dedicated host-wide range,
  one port per `Forking.Fork` call (see "Port allocator extension"
  below).
- Child reaping: the bwrapped R zygote `parallel:::mcfork`s exactly
  as in the Docker case; `waitpid` works because the children are
  in the zygote's PID namespace (phase 3-7 sets `--unshare-pid`).
- ChildExit translation: identical pattern to Docker — one goroutine
  per worker drains `client.Exits` onto the shared
  `childExits chan backend.ChildExit`.
- `Backend.Addr` for zygote workers returns
  `127.0.0.1:{controlPort}`; non-zygote workers continue to return
  `127.0.0.1:{shinyPort}`. `HealthCheck` branches the same way.
- `Backend.Stop` for zygote workers synthesises
  `backend.ChildExit{Reason: "killed"}` events for every tracked
  child *before* killing the bwrap process, mirroring the Docker
  case. Without this, `zygote.Manager.bySession` retains stale
  references after worker eviction.
- **Control-connection watcher** — same pattern as the Docker
  backend. A per-worker goroutine blocks on
  `ws.fork.client.Done()` and calls `ProcessBackend.Stop` if the
  channel closes unexpectedly (and `workerProc.stopping` is not
  set). Covers zygote crashes, OOM kills, bwrap sandbox faults —
  any path that kills the R process without going through
  `Stop`. `Stop` is idempotent and safe against concurrent
  invocation from the watcher and the normal eviction path.
  `cmd.Wait()` in the existing `Spawn` cleanup goroutine is a
  second signal for the same condition; both converge on the
  same idempotent `Stop` call.

The structural similarity is large enough that the
`forking.go` files in both backends could share helper functions
in `internal/zygote/` or `internal/zygotectl/`. `Manager.Fork`
and the sweep/exit loops already live in `zygote`; the
per-worker control state (`forkState`) could move to `zygote`
if it doesn't reach into backend-specific types. For phase 3-9
I'd duplicate it in each backend and DRY in a follow-up once
both are working — premature abstraction risk.

**Port allocator extension (process backend only).** Phase 3-8
turned the process backend's existing single-range port allocator
into an interface (`portAllocator` with
`Reserve() (port int, ln net.Listener, err error)` and
`Release(port int) error`) plus two implementations
(`memoryPortAllocator` and `redisPortAllocator`) — both living in
`internal/backend/process/`. Phase 3-9 adds two more *instances*
of that same interface on `ProcessBackend` — no new allocator type,
just extra ranges — giving three independent host-wide ranges:

(The Docker backend does *not* reuse this interface. Docker child
ports live inside per-container bridge networks, so there's no
host-side `net.Listen` probe to perform and no cross-worker
collision to coordinate. The Docker `forkState` carries its own
small per-worker free list, see Step 7.)

| Allocator | Config field | Purpose |
|-----------|--------------|---------|
| `ports` | `port_range_*` (existing) | Shiny port for non-zygote workers |
| `controlPorts` | `zygote_control_range_*` (new) | One control port per zygote worker |
| `childPorts` | `zygote_child_range_*` (new) | One child port per forked session |

Three independent instances rather than carving subranges out of
one because each has a different sizing rule and preflight wants
to validate them independently:

- `ports` is sized for peak non-zygote worker count (and the
  rolling-update overlap headroom phase 3-7 already documents).
- `controlPorts` is sized for peak zygote worker count.
- `childPorts` is sized for peak zygote worker count ×
  `max_sessions_per_worker`.

**Both new instances mirror phase 3-8's memory-vs-Redis split.**
Phase 3-8 added `memoryPortAllocator` and `redisPortAllocator`
implementations selected by `process.New` based on
`fullCfg.Redis != nil`. Phase 3-9 routes the two new ranges
through the same branch — when Redis is configured, all three
allocators are Redis-backed; when it isn't, all three are
memory-backed:

```go
// In process.New, after the existing ports/uids construction
// from phase 3-8:
if fullCfg.Redis != nil {
    b.controlPorts = newRedisPortAllocator(client,
        cfg.ZygoteControlRangeStart, cfg.ZygoteControlRangeEnd,
        hostname, "controlport:")
    b.childPorts = newRedisPortAllocator(client,
        cfg.ZygoteChildRangeStart, cfg.ZygoteChildRangeEnd,
        hostname, "childport:")
} else {
    b.controlPorts = newMemoryPortAllocator(
        cfg.ZygoteControlRangeStart, cfg.ZygoteControlRangeEnd)
    b.childPorts = newMemoryPortAllocator(
        cfg.ZygoteChildRangeStart, cfg.ZygoteChildRangeEnd)
}
```

This requires one small extension to phase 3-8's
`newRedisPortAllocator` constructor: a fourth `keyPrefix string`
parameter so the three allocators write to disjoint key
namespaces (`{prefix}port:<N>`, `{prefix}controlport:<N>`,
`{prefix}childport:<N>`). Phase 3-8 hardcodes the prefix as
`"port:"` inside the constructor; phase 3-9 lifts it to a
parameter and updates the existing `b.ports` call site to pass
`"port:"` explicitly. The Lua scripts (SETNX scan, ownership-
checked DEL) take the prefix from `KEYS[1]` already, so no
script changes — only the Go-side wiring.

`CleanupOwnedOrphans` extends symmetrically: phase 3-8's
`ProcessBackend.CleanupOrphanResources` already type-asserts
`b.ports.(*redisPortAllocator)` and calls cleanup on hit; phase
3-9 adds two more parallel branches for `b.controlPorts` and
`b.childPorts`. Each Redis allocator instance owns its own key
namespace, so the cleanups are independent and the order is
irrelevant.

This closes the cross-server collision window for the new
ranges during a process-backend rolling-update overlap, the
same way phase 3-8 closes it for `ports` and `uids`. There is
no rolling-update × zygote caveat to document — operators
running rolling updates with zygote enabled get the same
correctness guarantees as for non-zygote workers.

The `childPorts` allocator additionally inherits phase 3-8's
kernel-probe retry loop on the Redis variant: after a SETNX
claim, `net.Listen` probes the port to catch non-blockyard host
processes binding inside the range, and on failure DELs the
claim and advances `skip_from`. The same probe loop runs for
the memory variant via `memoryPortAllocator`'s built-in retry.

The default ranges for the process backend's containerised
deployment mode (where the outer container has effectively the
whole ephemeral range to itself) are deliberately generous:

```toml
[process]
port_range_start          = 10000
port_range_end            = 10999
zygote_control_range_start = 11000
zygote_control_range_end   = 11099
zygote_child_range_start   = 11100
zygote_child_range_end     = 12099
```

The `process.RunPreflight` check `checkPortRanges` (added in this
phase, alongside the existing port-range validation) verifies all
three are non-overlapping, each end >= start, and each is sized
for at least one peak worker plus rolling-update headroom.

Allocation lifecycle on the process backend (note: all three
allocators share the phase 3-8 `Reserve()`/`Release()` semantics —
`Reserve` returns a `(port, ln)` pair where `ln` holds the kernel
binding through setup, and the caller closes `ln` immediately
before the worker process binds for itself, narrowing the
single-process race window per #173):

- Non-zygote worker `Spawn`: `ports.Reserve()` once; the held
  listener is closed immediately before `cmd.Start()` (the
  existing #173 pattern). Released by `ports.Release()` in the
  cleanup goroutine.
- Zygote `Spawn`: `controlPorts.Reserve()` once; the held
  listener is closed immediately before `cmd.Start()` so the
  bwrapped R zygote can bind on it. Released by
  `controlPorts.Release()` in the cleanup goroutine. No child
  ports are reserved up-front — `Forking.Fork` allocates lazily.
- `Forking.Fork`: `childPorts.Reserve()` once per call; the
  held listener is closed immediately before the FORK control
  command is sent to the zygote, recorded in
  `forkState.childPort[childID]`. Released by
  `childPorts.Release()` in `Forking.KillChild` and in the
  synthetic-exit path of `Stop` (Step 8 above).

The `childPorts` Reserve→close→FORK sequence has a residual race
window: between closing the Go-side listener and the R child
calling `runApp(port = N)` (which triggers httpuv's bind), the
port is unheld. R startup and httpuv initialisation take
seconds, so the window exists but is bounded. Both allocator
variants protect against it the same way — the Redis variant
holds the SETNX claim across the FORK round-trip (peers see the
key and skip past), and both variants probe via `net.Listen` on
each Reserve to catch non-blockyard processes binding inside the
range. We do not pass the listener FD into the R child via
`SCM_RIGHTS`; the complexity isn't justified given the existing
probe + ownership coordination.

`Forking.Fork` returning "no free ports" is a real failure mode
but vanishingly rare with the default range — operators who push
into it should resize `zygote_child_range`. The error is surfaced
to the proxy as a normal `ensureWorker` failure (HTTP 503).

### Step 9: Cold-start integration

Two files change: `internal/proxy/coldstart.go` (the spawn path)
and `internal/proxy/proxy.go` (the session-creation path).

**`coldstart.go` — `spawnWorker` and `ensureWorker`.**

Both functions start by computing the *effective* zygote state
from the server-wide `experimental.zygote` flag and the per-app
`app.Zygote` bool — the two-level opt-in from decision #15. This
is the **runtime kill switch**: flipping `experimental.zygote`
off in server config disables the feature for every app without
touching the database.

```go
// Helper used by both spawnWorker and ensureWorker.
func effectiveZygoteState(cfg *config.Config, app *db.AppRow) bool {
    flags := cfg.ExperimentalFlags() // nil-safe (Step 13)
    return flags.Zygote && app.Zygote
}
```

In `spawnWorker`, use the effective state to gate secret
generation and spec construction:

```go
// In spawnWorker, after the existing token-refresher block:
effectiveZygote := effectiveZygoteState(srv.Config, app)

var controlSecret []byte
if effectiveZygote && tokDir != "" {
    var err error
    controlSecret, err = zygotectl.WriteSecret(tokDir)
    if err != nil {
        cleanupLocal()
        return "", "", fmt.Errorf("zygote: write secret: %w", err)
    }
}

spec := backend.WorkerSpec{
    // ...existing fields...
    Zygote:         effectiveZygote,
    ControlSecret:  controlSecret,
    ChildPortRange: srv.Config.Docker.ZygoteChildPortRange,
}
```

The Cmd construction also changes: for zygote apps the spec.Cmd
is left empty (the backend constructs the right zygote
invocation), otherwise the existing shiny::runApp Cmd is used.
Apps with `apps.zygote = true` in the db but
`experimental.zygote = false` in server config fall through to
the non-zygote branch silently — their runtime behaviour is
identical to `apps.zygote = false`. This is the kill switch
from decision #15.

`ensureWorker` calls into the Manager after the worker is
healthy — again using the effective state, not `app.Zygote`:

```go
// In ensureWorker, after the existing lb.Assign / spawnWorker
// block, before returning:
if effectiveZygoteState(srv.Config, app) && srv.Zygotes != nil {
    addr, err := srv.Zygotes.Fork(ctx, wid, sessionID)
    if err != nil {
        return "", "", fmt.Errorf("zygote: fork: %w", err)
    }
    return wid, addr, nil
}
return wid, registryAddr, nil
```

Note: this requires `ensureWorker` to receive `sessionID` from
`proxy.go`. Update the signature:

```go
func ensureWorker(ctx context.Context, srv *server.Server, app *db.AppRow, sessionID string) (workerID, addr string, err error)
```

**`proxy.go` — pass sessionID to ensureWorker, populate Entry.Addr.**

```go
if workerID == "" {
    isNewSession = true
    sessionID = uuid.New().String()
    // ...

    wid, a, err := ensureWorker(r.Context(), srv, app, sessionID)
    // ...
    workerID, addr = wid, a
    srv.Sessions.Set(sessionID, session.Entry{
        WorkerID:   workerID,
        Addr:       a, // populated for both modes — see Step 3
        UserSub:    callerSub,
        LastAccess: time.Now(),
    })
}
```

The `srv.Zygotes` field is a `*zygote.Manager`, initialised in
`cmd/blockyard/main.go` after the backend is constructed — but
*only* if `experimental.zygote` is enabled in the server config.
When the flag is off, the Manager isn't constructed at all, and
the runtime kill switch from decision #15 means no zygote path
ever runs:

```go
flags := srv.Config.ExperimentalFlags()
if flags.Zygote {
    if forking, ok := backend.(backend.Forking); ok {
        srv.Zygotes = zygote.NewManager(
            forking,
            srv.Sessions,
            zygote.ManagerConfig{
                SweepInterval: srv.Config.Proxy.AutoscalerInterval.Duration,
            },
        )
    }
}
```

`srv.Zygotes` is `nil` when either the server-wide flag is off
*or* the configured backend doesn't implement `Forking`.
Cold-start uses the effective-state helper (`effectiveZygoteState`
above) and checks `srv.Zygotes != nil` before calling `Fork`,
falling through to the non-zygote path when either condition
holds. The API layer rejects *new* `zygote=true` transitions
when the backend doesn't support it or when the server-wide
flag is off (Step 12), but existing `apps.zygote = true` rows
from an older config state silently degrade to non-zygote
behaviour — no errors, no panics, just the current version's
pre-zygote code path.

### Step 10: Proxy fallback for unreachable child

The existing session-resolution block in `proxy.go` (around line 159)
already has a fallback for "session worker not in registry" — it
falls through to creating a new session. For zygote apps we extend this
to "session addr present but unreachable", which detects the case
where a child died between the Manager's exit-event handler and
this request.

The cleanest place is around the `forwardHTTP`/`shuttleWS` dispatch.
Currently those just hit "bad gateway" on a connection refused. We
add a small probe just before the dispatch:

```go
// proxy.go, just before the WebSocket vs HTTP dispatch block.
if isZygoteSession(srv, sessionID) {
    if !addrReachable(addr) {
        slog.Debug("proxy: zygote session unreachable, re-cold-starting",
            "session_id", sessionID, "addr", addr)
        srv.Sessions.Delete(sessionID)
        // Restart the proxy handler logic — easiest is to issue a
        // 307 redirect to self, the client retries with no session
        // cookie and the new-session path runs.
        http.Redirect(w, r, r.URL.RequestURI(), http.StatusTemporaryRedirect)
        return
    }
}
```

`addrReachable` is a 50ms TCP dial. The check is gated on zygote
sessions only (cheap check via `srv.Zygotes.HasChild(sessionID)`)
so non-zygote apps see no overhead. The 307 redirect forces the
browser to retry without the stale session cookie — the new
request runs the `isNewSession` path and forks a new child.

This is best-effort. The Manager's exit-event handler is the
authoritative path; the redirect is a fallback for the gap between
"child dies" and "Manager processes the exit".

### Step 11: Cleanup paths

Three independent paths converge on `Manager.bySession` cleanup,
covering the three ways a session can disappear:

1. **Child crash / OOM / killed.** The zygote pushes `CHILDEXIT`
   over the control connection, the per-worker translator goroutine
   forwards a `backend.ChildExit` onto `d.childExits`, and
   `Manager.exitLoop` removes the matching `bySession` entry and
   calls `m.sessions.Delete(sid)`. Authoritative for live workers.

2. **Worker eviction.** `Backend.Stop` for a zygote worker
   synthesises `backend.ChildExit{Reason: "killed"}` events for
   every tracked child *before* tearing the container/process down
   (Step 7 / Step 8). These events flow through the same
   `exitLoop` path as #1, so the Manager's bookkeeping stays
   consistent. **`ops.EvictWorker` does not call `Manager.Kill`
   directly** — the synthesised events have already done the work,
   and the control connection is dead by the time eviction reaches
   any explicit kill step.

3. **Session disappeared without a child exit.** TTL expiry on
   Redis, explicit `Sessions.Delete` from the logout endpoint, the
   OIDC user-mismatch branch in `proxy.go`, or any future path
   that removes a session without going through the backend.
   `Manager.sweepLoop` (Step 5) ticks every `sweep_interval`,
   snapshots `bySession`, probes `session.Store.Get` for each, and
   kills any child whose session has vanished. This is the only
   cleanup path that works on Redis-backed deployments — Redis
   `SweepIdle` is a no-op because TTL expiry is invisible to the
   server. The same loop covers explicit logout and OIDC mismatch
   paths so they don't need their own per-call `Manager.Kill`
   hooks.

The autoscaler tick continues to call `srv.Sessions.SweepIdle(...)`
unchanged — it's the right cleanup path for memory-backed sessions
and a no-op for Redis. Phase 3-9 does not extend `SweepIdle`'s
return type. The Manager's sweep loop is the load-bearing cleanup
path for zygote children regardless of which session store is
configured.

### Step 12: API / CLI / UI surface for `zygote`

Mirror the phase 3-6 pattern. The `zygote` field is gated
against the `experimental.zygote` server-wide flag — see
decision #15 for the rationale.

**API** — extend `updateAppRequest` in `internal/api/apps.go`:

```go
type updateAppRequest struct {
    // ...existing fields...
    Zygote *bool `json:"zygote"`
}
```

Validation in `UpdateApp()`. The check runs against the *effective
end-state* (current row + this PATCH applied) so that a request
which only changes `max_sessions_per_worker` cannot leave a stale
`zygote = true` in an invalid state:

```go
// Compute the effective end-state.
effectiveZygote := app.Zygote
if body.Zygote != nil {
    effectiveZygote = *body.Zygote
}
effectiveMax := app.MaxSessionsPerWorker
if body.MaxSessionsPerWorker != nil {
    effectiveMax = *body.MaxSessionsPerWorker
}

// Server-wide experimental gate (decision #15). Must be enabled
// before apps.zygote can transition from false to true. Flags
// that are already true in the db can stay true even when the
// server-wide gate is off — the runtime short-circuits to the
// non-zygote path — but the API rejects new transitions so
// operators don't accidentally commit intent that isn't
// currently runnable.
flags := srv.Config.ExperimentalFlags() // nil-safe accessor (Step 13)
transitioningToZygote := body.Zygote != nil && *body.Zygote && !app.Zygote

if transitioningToZygote && !flags.Zygote {
    badRequest(w, "zygote feature not enabled in server config (set experimental.zygote = true)")
    return
}

if effectiveZygote {
    if effectiveMax <= 1 {
        badRequest(w, "zygote requires max_sessions_per_worker > 1")
        return
    }
    // Backend must support Forking. Only check on the transition
    // (zygote is being enabled, or was already on but the backend
    // changed at restart) — we don't want to fence off unrelated
    // updates to apps that were configured under an older backend.
    if transitioningToZygote {
        if _, ok := srv.Backend.(backend.Forking); !ok {
            badRequest(w, "configured backend does not support zygote")
            return
        }
    }
}
```

Add `Zygote` to `appResponseV2()` in
`internal/api/runtime.go` and to `swagger_types.go`. The response
reflects the *stored* intent; the UI combines it with the
server-wide capabilities (see below) to show the *effective*
state.

**Server capabilities endpoint** — add a new
`GET /api/v1/server/capabilities` (or extend an existing bootstrap
endpoint — whichever is closer to existing patterns) that returns:

```json
{
  "experimental": {
    "zygote": true
  }
}
```

The UI reads this at page load to decide whether to enable or
disable the zygote toggle. Admin-only — the capability list is
considered operational info, not exposed to non-admin users.
Phase 3-10 adds an `experimental.ksm` field here.

**CLI** — extend `by scale` in `cmd/by/scale.go`:

```go
cmd.Flags().Bool("zygote", false,
    "Enable zygote worker model (experimental, requires --max-sessions > 1 and experimental.zygote in server config)")

if cmd.Flags().Changed("zygote") {
    v, _ := cmd.Flags().GetBool("zygote")
    body["zygote"] = v
}
```

Server-side validation rejects invalid combinations; the CLI
surfaces the API error message verbatim.

**UI** — admin-only toggle in `tab_settings.html`. Checks the
server capabilities at render time and shows a disabled state
with an explanatory tooltip when the server-wide flag is off:

```html
{{if .IsAdmin}}
<div class="field-group">
    <label for="zygote">Zygote worker model</label>
    <p class="field-description">
        <em>Experimental.</em> When enabled, each session runs in a
        forked R child inside a shared zygote container. Requires
        Max sessions per worker &gt; 1.
        <a href="...zygote docs link...">Learn more</a>.
    </p>
    <input type="checkbox" id="zygote" name="zygote"
           {{if .App.Zygote}}checked{{end}}
           {{if not .Capabilities.ExperimentalZygote}}disabled
             title="Set experimental.zygote = true in the server config to enable"{{end}}
           hx-patch="/api/v1/apps/{{.App.ID}}"
           hx-include="[name='zygote']"
           hx-swap="none">
</div>
{{end}}
```

### Step 13: Config additions

**New top-level `ExperimentalConfig` section** on `Config` in
`internal/config/config.go`. The flag is the server-wide gate
from decision #15 — default off, opt-in via config file. Nil
when the `[experimental]` section is absent from the config
file, which is the intended default for operators who aren't
running the zygote model.

```go
type Config struct {
    // ...existing fields...
    Experimental *ExperimentalConfig `toml:"experimental"` // nil when not configured
}

type ExperimentalConfig struct {
    Zygote bool `toml:"zygote"` // enable the zygote worker model
    // Phase 3-10 adds KSM here.
}
```

Access throughout the codebase goes through a helper on `Config`
that returns a zero-value struct when the section is absent, so
callers don't need nil checks:

```go
// ExperimentalFlags returns the experimental config, defaulting
// to all-disabled when the [experimental] section is absent.
func (c *Config) ExperimentalFlags() ExperimentalConfig {
    if c.Experimental == nil {
        return ExperimentalConfig{}
    }
    return *c.Experimental
}
```

Example `blockyard.toml`:

```toml
[experimental]
zygote = true
```

Two new fields on `DockerConfig`:

```go
type DockerConfig struct {
    // ...existing fields...
    ZygoteChildPortRange   string // "3839-3938"; child ports for zygote workers
    ZygoteControlPort int    // 3837; zygote control port on the per-worker bridge
}
```

Defaults applied in `applyDefaults()`:

```go
if c.ZygoteChildPortRange == "" {
    c.ZygoteChildPortRange = "3839-3938"
}
if c.ZygoteControlPort == 0 {
    c.ZygoteControlPort = 3837
}
```

The control port must not collide with `ShinyPort` (3838) or any
port in `ZygoteChildPortRange`. Validate at startup.

Four new fields on `ProcessConfig` (extending phase 3-7's
`port_range_*` pair):

```go
type ProcessConfig struct {
    // ...existing fields including PortRangeStart/End...
    ZygoteControlRangeStart int `toml:"zygote_control_range_start"`
    ZygoteControlRangeEnd   int `toml:"zygote_control_range_end"`
    ZygoteChildRangeStart   int `toml:"zygote_child_range_start"`
    ZygoteChildRangeEnd     int `toml:"zygote_child_range_end"`
}
```

Defaults applied in `processDefaults()`:

```go
if c.ZygoteControlRangeStart == 0 {
    c.ZygoteControlRangeStart = 11000
}
if c.ZygoteControlRangeEnd == 0 {
    c.ZygoteControlRangeEnd = 11099
}
if c.ZygoteChildRangeStart == 0 {
    c.ZygoteChildRangeStart = 11100
}
if c.ZygoteChildRangeEnd == 0 {
    c.ZygoteChildRangeEnd = 12099
}
```

Validation in `config.validate()` (alongside phase 3-7's existing
port-range checks): each new range has end >= start, and the three
ranges (`port_range`, `zygote_control_range`, `zygote_child_range`)
do not overlap. Sizing relative to UID/worker counts is checked at
runtime by `process.RunPreflight.checkPortRanges`, not at config
parse time, so operators get a usable error rather than a startup
failure when their pool is slightly under-sized.

Phase 3-10 adds a `ZygoteMetricsInterval` field to `ProxyConfig`
for the KSM metrics-poll cadence.

### Step 14: Tests

#### Unit tests

**`internal/zygotectl/control_test.go`** — control protocol over a
loopback test server:

```go
func TestClient_AuthOK(t *testing.T)
// Spin up a test TCP listener that speaks the protocol; verify
// AUTH succeeds with the right secret.

func TestClient_AuthRejected(t *testing.T)
// Same with wrong secret → returns auth error.

func TestClient_ForkAndKill(t *testing.T)
// FORK 3839 → OK c1 12345; KILL c1 → OK.

func TestClient_ChildExitPushed(t *testing.T)
// Test server pushes CHILDEXIT; client surfaces it on Exits.

func TestClient_ConnectionClose(t *testing.T)
// Drop the connection mid-request; pending request returns error.

func TestClient_ConcurrentForks(t *testing.T)
// Two goroutines call Fork against one client at the same time.
// Both succeed (queued on reqMu), neither errors with "request
// in flight".

func TestClient_InfoAtStartup(t *testing.T)
// Test server responds to INFO with a canned key=value block.
// NewClient succeeds; client.Info() returns the parsed Info
// with all expected fields populated.

func TestClient_InfoUnknownKeys(t *testing.T)
// Test server includes unrecognised keys (future-compat). Client
// parses them into Info.Unknown without erroring. Also exercises
// the forward-compatibility path that phase 3-10 relies on for
// ksm_status / ksm_errno fields.
```

**`internal/zygote/manager_test.go`** — using a mock `Forking`
(note: unchanged location — `Manager` stays in the `zygote`
package; only the control-protocol types moved to `zygotectl`):

```go
type mockForking struct {
    forks    chan forkCall
    kills    chan killCall
    childExits chan backend.ChildExit
}

func TestManager_ForkRecordsBookkeeping(t *testing.T)
// Manager.Fork → mockForking.Fork called → bookkeeping has the
// session, HasChild returns true.

func TestManager_ChildExitDeletesSession(t *testing.T)
// Manager.Fork → mockForking pushes ChildExit → session.Delete
// called with the right sessionID, bookkeeping cleared.

func TestManager_SweepKillsOrphanedChildren(t *testing.T)
// Manager.Fork against a memory session store. Delete the session
// directly via the store. Tick the sweep loop. Verify mock
// KillChild was called and HasChild returns false.

func TestManager_SweepIgnoresLiveSessions(t *testing.T)
// Manager.Fork. Tick the sweep loop. Verify KillChild was not
// called and HasChild remains true.
```

**`internal/session/store_test.go`** — extend existing tests:

```go
func TestEntry_AddrRoundTrip_Memory(t *testing.T)
// Set entry with Addr, Get returns the same Addr.

func TestEntry_AddrRoundTrip_Redis(t *testing.T)
// Same but against miniredis.
```

#### Integration tests

**`internal/backend/docker/forking_integration_test.go`** (tagged
`docker_test`):

```go
func TestDockerForking_ZygoteSpawn(t *testing.T)
// Spawn a zygote container with a minimal bundle (just `library(shiny)`).
// Verify control port is reachable, AUTH succeeds.

func TestDockerForking_ForkTwoChildren(t *testing.T)
// Fork two children on different ports.
// Verify both addresses are reachable independently.
// Kill one. Verify the other still responds.

func TestDockerForking_ChildCrashEmitsExit(t *testing.T)
// Fork a child. SIGKILL its PID. Verify ChildExits emits within
// 1 second (the zygote's 100ms poll loop should observe the exit
// and the push round-trip should complete well under the bound).

func TestDockerForking_ControlAuthRejected(t *testing.T)
// Spawn a zygote. Connect with wrong secret. Verify the connection
// is dropped after the AUTH frame.

func TestDockerForking_ControlConnectionLossEvictsWorker(t *testing.T)
// Spawn a zygote. Kill the zygote R process from outside via
// `docker exec <id> pkill R` (bypassing Backend.Stop). Verify:
//   1. Within ~5 seconds, the worker is no longer in d.workers.
//   2. A subsequent Fork on the same workerID returns an error
//      (worker not found), forcing cold-start on the next request.
//   3. A `zygote: control connection lost` log line was emitted
//      with the correct worker_id.
// Covers the watcher goroutine path — without it, the worker
// would hang around until the autoscaler's idle sweep noticed.

func TestDockerForking_StopIdempotentUnderRace(t *testing.T)
// Spawn a zygote. Call Backend.Stop and simultaneously close the
// control client (simulating the watcher firing at the same
// time). Verify both calls return nil (or a known benign error)
// and no panic. The worker is removed from d.workers exactly once.
```

**`internal/backend/process/forking_integration_test.go`** (tagged
`process_test`) — analogous tests for the bwrap-sandboxed zygote.
Skipped when bwrap is unavailable, same pattern as the rest of the
process backend tests.

**`internal/backend/process/ports_redis_test.go`** (extends phase
3-8's existing tests) — three additional cases on the
`controlport:` and `childport:` namespaces:

```go
func TestRedisPortAllocator_NamespaceIsolation(t *testing.T)
// Construct three allocators against one miniredis with prefixes
// "port:", "controlport:", "childport:". Reserve the same numeric
// port (e.g. 11000) on each. Verify all three succeed — claims
// in one namespace do not block claims in another.

func TestRedisPortAllocator_ControlPortConcurrentReserve(t *testing.T)
// Mirror of phase 3-8's existing concurrent-Reserve test, run
// against the controlport namespace. N goroutines call Reserve;
// all return distinct ports.

func TestRedisPortAllocator_ChildPortCleanup(t *testing.T)
// Populate the childport namespace with stale entries owned by
// the local hostname plus a peer hostname. Call CleanupOwnedOrphans
// on the child-port allocator. Verify only the local stale entries
// are removed and the controlport / port namespaces are untouched.
```

These exercise the cross-server collision-avoidance path
end-to-end for the new ranges, the same way phase 3-8's existing
`ports_redis_test.go` exercises it for the existing range.

**`internal/proxy/coldstart_test.go`** — extend with zygote-aware
cold-start:

```go
func TestEnsureWorker_ZygoteCallsManagerFork(t *testing.T)
// app.Zygote=true. Spawn returns a worker. Verify Manager.Fork
// is called with the sessionID and its return addr is what
// ensureWorker returns.

func TestEnsureWorker_ZygoteBackendNotSupported(t *testing.T)
// app.Zygote=true but backend doesn't implement Forking.
// Verify clear error.
```

#### DB and migration tests

```go
func TestUpdateApp_ZygoteRequiresMultiSession(t *testing.T)
// PATCH with zygote=true and max_sessions_per_worker=1 → 400.

func TestUpdateApp_ZygoteRoundTrip(t *testing.T)
// Set zygote=true, read back, verify.

func TestUpdateApp_ZygoteRequiresServerFlag(t *testing.T)
// Server config has experimental.zygote = false. PATCH with
// zygote=true → 400 "zygote feature not enabled in server config".

func TestServerCapabilities_ReflectsExperimentalFlags(t *testing.T)
// GET /api/v1/server/capabilities as admin returns the effective
// experimental.* flags. Non-admin → 403.

func TestRuntimeKillSwitch_ZygoteDisabledFallsBackToNonZygote(t *testing.T)
// App has zygote=true in db. Server config has
// experimental.zygote=false. Cold-start path does not call
// Manager.Fork, spawns a regular (non-zygote) worker.

// Migration round-trip is covered by the existing TestMigrateRoundtrip
// from phase 3-1.
```

---

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/db/db.go` | **update** | `Zygote` on `AppRow` and `AppUpdate`, UPDATE SQL |
| `internal/backend/backend.go` | **update** | `Forking` interface, `ChildExit` type, `WorkerSpec.Zygote`/`ControlSecret`/`ChildPortRange` |
| `internal/backend/docker/docker.go` | **update** | Spawn branch for zygote (Cmd, env vars, mount, control client connect); `Addr`/`HealthCheck` branch on `ws.fork` to use control port; `Stop` synthesises ChildExit events before container teardown; `workerState.stopping` flag + idempotent `Stop`; control-connection watcher goroutine that calls `Stop` on `client.Done()` when not stopping |
| `internal/backend/process/process.go` | **update** | Same shape as `docker.go`: spawn branch, `Addr`/`HealthCheck` branch on `workerProc.fork`, synthesised-ChildExit on `Stop`, `workerProc.stopping` flag + idempotent `Stop`, control-connection watcher; plus `controlPorts` and `childPorts` allocators on the backend struct, constructed in `process.New` via the same `cfg.Redis != nil` branch phase 3-8 already uses for `b.ports`; `CleanupOrphanResources` extends to call the new allocators' `CleanupOwnedOrphans` when they're the Redis variants |
| `internal/backend/process/ports.go` | **update** | Lift the hardcoded `"port:"` Redis key prefix into a constructor parameter (`newRedisPortAllocator(client, lo, hi, hostname, keyPrefix)`) so the existing `b.ports` and the two new `b.controlPorts` / `b.childPorts` instances write to disjoint key namespaces. Phase 3-8's existing call site updates to pass `"port:"` explicitly. The `memoryPortAllocator` constructor is unchanged. |
| `internal/backend/process/ports_redis.go` | **update** | Constructor takes the new `keyPrefix` parameter and passes it as the `KEYS[1]` prefix in both the SETNX-scan and ownership-checked-DEL Lua scripts. Scripts themselves are unchanged (they already read the prefix from `KEYS[1]`). `CleanupOwnedOrphans` scans `{prefix}<keyPrefix><N>` instead of the hardcoded form. |
| `internal/backend/process/ports_redis_test.go` | **update** | Existing tests pass `"port:"` explicitly. Add tests covering the new `controlport:` and `childport:` namespaces (concurrent Reserve, exhaustion, ownership-checked Release, three-way namespace isolation: claiming control port 11000 does not block port 11000 in the `port:` namespace). |
| `internal/backend/process/ports_cleanup_test.go` | **update** | Mirror of `ports_redis_test.go` updates: cleanup scoping per namespace, no cross-namespace deletion. |
| `internal/session/store.go` | **update** | `Addr` field on `Entry` |
| `internal/session/redis.go` | **update** | Hash schema gains `addr` field |
| `internal/proxy/proxy.go` | **update** | Pass `sessionID` to `ensureWorker`, populate `Entry.Addr`, zygote unreachable-child fallback |
| `internal/proxy/coldstart.go` | **update** | Generate `ControlSecret`, attach to spec, call `Manager.Fork` for zygote apps, `ensureWorker` signature change |
| `internal/api/apps.go` | **update** | `zygote` field on request, effective-state validation (multi-session, backend support, server-wide `experimental.zygote` gate from decision #15) |
| `internal/api/server.go` | **update** | New `GET /api/v1/server/capabilities` endpoint returning effective `experimental.*` flags (admin-only) |
| `internal/api/runtime.go` | **update** | Add `zygote` to `appResponseV2()` |
| `internal/api/swagger_types.go` | **update** | Add `zygote` and capabilities shape to swagger response types |
| `internal/ui/templates/tab_settings.html` | **update** | Zygote toggle, admin-gated, server-capabilities-gated; worker detail surfaces `zygotectl.Info` from `INFO` |
| `cmd/by/scale.go` | **update** | `--zygote` flag |
| `cmd/blockyard/main.go` | **update** | Construct `zygote.Manager` when backend implements `Forking` and `experimental.zygote` is set; write embedded `ZygoteScript` to host path at startup |
| `internal/server/state.go` | **update** | `Zygotes *zygote.Manager` field on `Server` |
| `internal/config/config.go` | **update** | New `Experimental *ExperimentalConfig` top-level section (`Zygote` bool, default off) with `ExperimentalFlags()` nil-safe accessor; `ZygoteChildPortRange`, `ZygoteControlPort` on `DockerConfig`; `ZygoteControlRangeStart/End`, `ZygoteChildRangeStart/End` on `ProcessConfig` |

## New files

| File | Purpose |
|------|---------|
| `internal/db/migrations/sqlite/003_zygote.up.sql` | Migration up (SQLite) |
| `internal/db/migrations/sqlite/003_zygote.down.sql` | Migration down (SQLite) |
| `internal/db/migrations/postgres/003_zygote.up.sql` | Migration up (PostgreSQL) |
| `internal/db/migrations/postgres/003_zygote.down.sql` | Migration down (PostgreSQL) |
| `internal/zygotectl/control.go` | TCP control protocol client (`Client`, `Info`, `ChildExitMsg`) — shared between backends |
| `internal/zygotectl/control_test.go` | Control protocol unit tests |
| `internal/zygotectl/secret.go` | Per-worker control secret generation |
| `internal/zygotectl/secret_test.go` | Secret round-trip test |
| `internal/zygotectl/zygote.R` | Embedded zygote R script |
| `internal/zygotectl/embed.go` | `//go:embed` declaration for the R script |
| `internal/zygote/manager.go` | `Manager` type, session ↔ child bookkeeping, exit handler, sweep loop |
| `internal/zygote/manager_test.go` | Manager unit tests with mock `Forking` |
| `internal/backend/docker/forking.go` | Docker `Forking` implementation |
| `internal/backend/docker/forking_integration_test.go` | Docker zygote integration tests (`docker_test`) |
| `internal/backend/process/forking.go` | Process `Forking` implementation |
| `internal/backend/process/forking_integration_test.go` | Process zygote integration tests (`process_test`) |

## Design decisions

1. **Session addressing via `session.Entry.Addr`.** Zygote
   sessions need a per-child routing target that survives across
   requests. Adding `Addr` to `session.Entry` keeps that target
   alongside the rest of the session state and avoids touching
   the `WorkerRegistry` schema (which is keyed by workerID and
   couldn't disambiguate per-child addresses). For non-zygote
   sessions the field stays empty and the existing
   `Registry.Get(workerID)` path runs unchanged — no hot-path
   change for the common case. Alternatives considered: extending
   the `WorkerRegistry` interface to be `(workerID, sessionID)`-keyed
   (too much surface, breaks Redis schema), and computing child
   ports from `hash(sessionID)` (collisions, doesn't survive
   restart). Both rejected.

2. **`Forking` as an optional capability sub-interface.** The Go
   convention for optional capabilities (`io.Reader` /
   `io.WriterTo`). Backends that don't implement the zygote model simply
   omit the methods; the proxy does a type assertion at startup
   (`srv.Backend.(backend.Forking)`) and only constructs the
   `Manager` if the assertion succeeds. The `Backend` interface
   stays minimal and the zygote concept doesn't leak into
   backends that don't support it.

3. **Zygote is opt-in per app and coexists with shared
   multi-session mode.** The plan does not deprecate
   `max_sessions_per_worker > 1` without `zygote` — the existing
   shared-R multiplexing remains the default for multi-session
   apps. The zygote model is an experimental alternative gated by
   the per-app `zygote` flag AND a server-wide
   `experimental.zygote` config flag — see decision #15 for the
   rationale behind two-level gating. This keeps the surface area
   of the experiment contained and easy to back out.

4. **Bare TCP line protocol with `socketSelect` polling on the R
   side, not httpuv/HTTP.** Three alternatives were rejected:

   - **`docker exec` per fork.** 50–200ms exec overhead per
     session start, unnecessary Docker API dependency on the
     proxy hot path.
   - **Bind-mounted Unix socket.** Socket file permissions are
     owned by the worker UID, which conflicts with phase 3-7's
     per-worker UID assignment.
   - **`httpuv`-as-server with HTTP requests + WebSocket push for
     events.** This is the obviously cleaner shape on paper —
     real async I/O, standard HTTP semantics, debuggable with
     `curl`. But `httpuv::startServer` documents that it runs
     I/O on a background thread (rstudio/httpuv#106 made this
     deliberate), and R core's own `mcfork` documentation
     explicitly warns: *"it is strongly discouraged to use
     mcfork [...] in any multi-threaded R process [...] as this
     can lead to deadlocks or crashes."* httpuv + mcfork is a
     documented anti-pattern, not a hypothetical concern. The
     `later` package has the same property. The only fork-safe
     non-blocking I/O primitive in stock R is `base::socketSelect`,
     which calls `select(2)` directly with no background threads.

   So the design is: TCP on the per-worker bridge (Docker) or
   loopback (process), line-delimited protocol, AUTH first frame,
   `socketSelect`-driven 100ms poll loop on the R side. The R
   3.4.3 fractional-timeout bug
   (HenrikBengtsson/Wishlist-for-R#35) was fixed in 2017 and
   doesn't affect modern targets. Authentication is a 32-byte
   pre-shared secret in the per-worker token directory, sent as
   `AUTH <hex>` first frame. Defense in depth on top of the
   existing per-worker bridge isolation (which is the primary
   security boundary).

   If httpuv ever ships a single-threaded mode (or if a
   hypothetical successor with a single-threaded I/O loop
   appears), the wire layer can be migrated cleanly — the
   protocol surface is small, and `internal/zygotectl/control.go`
   plus `zygote.R` are the only files affected. No external
   client of this protocol exists.

5. **Shared `zygotectl.Client` and `zygote.Manager`, split across
   two packages to keep `backend` free of control-client
   dependencies.** The wire protocol and session-bookkeeping
   logic are identical across backends. They live in
   `internal/zygotectl/` (the control protocol surface —
   `Client`, `Info`, `ChildExitMsg`, secret helpers, embedded R
   script; no dependency on `backend`) and `internal/zygote/`
   (the `Manager` type; imports both `backend` and `zygotectl`).
   Both backend `Forking` implementations import `zygotectl`
   directly and store `*zygotectl.Client` in their per-worker
   state. Only the dial address and the per-worker spawn details
   differ between backends.

   **Why the split matters beyond phase 3-9.** Phase 3-10 will
   add a `StatsClient(workerID) *zygotectl.Client` method to
   the `Forking` interface for KSM metrics polling. Without the
   split, `backend` would need to import a package containing
   the control client while that same package would need to
   import `backend` (for `Forking` and `ChildExit`) — an import
   cycle. Splitting now means the 3-10 addition is purely
   additive: `backend` already imports `zygotectl` (implicitly
   through existing code paths), `zygote.Manager` imports both
   `backend` and `zygotectl`, and nothing cycles.

6. **Resource limit semantics unchanged.** `memory_limit` and
   `cpu_limit` keep their current meaning per backend: Docker
   enforces a pool cap on the container cgroup (matching today's
   `max_sessions_per_worker > 1` behaviour), process backend
   ignores them with a warning (consistent with phase 3-7's "no
   per-worker cgroups" stance). No new fields, no auto-scaling,
   no migration. Documented in the zygote prose.

7. **`zygote.R` embedded in the server binary.** Shipped via
   `//go:embed`, written to a host path at startup, bind-mounted
   into the container or bwrap sandbox. Same approach for both
   backends. Alternative considered: bake `zygote.R` into the
   worker Docker image. Rejected because (a) it doesn't work for
   the process backend at all (no image), (b) it couples the
   zygote protocol to the image release cycle, requiring image
   rebuilds for any protocol change, (c) custom worker images
   would need to keep `zygote.R` in sync manually.

8. **Per-worker control secret in the existing token directory.**
   Reuses the per-worker token dir (already mounted ro into the
   worker as `/var/run/blockyard`) instead of adding a new mount.
   The secret is unrelated to the existing JWT worker token —
   different direction (server → zygote vs worker → server),
   different lifecycle (one-shot, not refreshed). Simpler than
   reversing the JWT direction or sharing a signing key with the
   worker.

9. **Single connection per worker for control + events.** The
   `CHILDEXIT` push goes over the same TCP connection as
   `FORK`/`KILL` request/responses, demultiplexed by frame
   prefix in the reader goroutine. Alternative: a second
   connection for events. The single-connection model is simpler
   (one goroutine, one death-detection signal via `Done()` — see
   decision #13) and the volume is low (a few events per minute
   at most). Risks of starvation are addressed by the bounded
   `Exits` channel.

10. **Best-effort proxy fallback for unreachable children.** The
    Manager's `ChildExits` handler is the authoritative removal
    path. The proxy adds a 50ms TCP probe before dispatching
    zygote sessions and 307-redirects on failure, which catches
    the gap between "child died" and "Manager processed the
    exit". The redirect forces the browser to re-cold-start
    transparently. Probe is gated on `srv.Zygotes.HasChild` so
    non-zygote apps see no overhead.

11. **`zygote` validation requires `max_sessions_per_worker > 1`.**
    Zygote mode with one session per worker is just a more
    expensive way to spawn one R process per session. Validation
    rejects the combination at the API layer with a clear error.

12. **`Manager` runs its own sweep loop instead of hooking
    `SweepIdle`.** The session store's `SweepIdle` is a no-op on
    Redis (TTL expiry happens in the Redis server, invisible to
    blockyard), so a `SweepIdle returns []string` extension would
    only have worked for memory-backed deployments and silently
    leaked children on Redis. Instead, `Manager.sweepLoop` snapshots
    its `bySession` map every `sweep_interval` and probes
    `session.Store.Get` for each entry — symmetric across both
    stores, and as a side benefit it covers explicit logout, OIDC
    user-mismatch, and any future code path that calls
    `Sessions.Delete` without a backend round-trip. The trade-off
    is up to one sweep-interval of wall-clock latency between a
    session disappearing and its child being killed; for the zygote model
    that's negligible because child memory is bounded by the port
    allocator and the worst-case duplicate is "fork an extra child
    for the same user", which the existing `Manager.Fork` flow
    handles cleanly.

13. **No control-connection reconnect; lost connection triggers
    worker eviction.** When a zygote's control connection dies,
    the natural instinct is to reconnect — retry the dial, re-auth,
    resume. Phase 3-9 explicitly does not do this. Three reasons:

    - **Disconnects are not transient on our transports.** Docker
      per-worker bridges and loopback TCP don't have middleboxes,
      physical networks, or anything else that drops connections
      randomly. If the control connection dies, something actually
      broke — OOM-killed zygote, container crash, kernel issue,
      helper `.so` fault. Reconnecting papers over the symptom.
    - **Half-dead zygotes would fool a reconnect loop.** A zygote
      in a partial-OOM-recovery state might still accept TCP and
      authenticate, but fail on the next `FORK` because its R
      heap is corrupted or its forked children can't start. A
      reconnect loop gives us false confidence; a fresh spawn
      gives us a known-good state.
    - **Fresh spawn is fast enough.** Starting a new zygote costs
      the preload time we're already paying on every cold start.
      Recovery latency from "connection dies" to "fresh zygote
      serving" is bounded by the preload cost, not the sweep
      interval.

    Instead: each backend's `Spawn` starts a **watcher goroutine**
    that blocks on `client.Done()` and calls `Backend.Stop` if the
    channel closes unexpectedly. `Stop` is idempotent and
    race-safe, so concurrent firing from the watcher and an
    explicit eviction is benign. Sessions bound to the dead
    zygote are cleaned up via the existing synthesised-ChildExit
    path (decision #12 already requires this for normal eviction).
    On the next request, the proxy's unreachable-child fallback
    (Step 10) triggers a 307 redirect and the session lands on
    a fresh zygote.

    **User-visible behaviour:** brief latency hit on one request
    per affected session, no errors surfaced to the browser.
    Ops sees a `zygote: control connection lost, evicting worker`
    log line with the worker ID, plus the subsequent new spawn.
    Post-mortem investigation has a clear timeline and a clear
    root-cause question ("why did the zygote die?"), rather than
    a murky "the reconnect loop ran for 30 seconds and it
    eventually came back".

14. **The `shinyApp` object is captured during preload, not
    re-sourced per child.** The preload stubs `shinyApp(...)`
    as a capturing closure that stores the arguments into
    `captured_app <<- shiny::shinyApp(ui, server, ...)`. The
    FORK handler then calls `runApp(captured_app, port = ...)`
    in the child instead of `runApp(bundle_path, port = ...)`.
    This means `app.R` is parsed and evaluated exactly once, in
    the zygote, which saves 100–500ms per cold start for typical
    bundles (the re-parse / re-source cost of `app.R` in every
    child otherwise).

    **Fallback for unusual bundle structures.** Bundles that
    split `ui.R` and `server.R` without a top-level `shinyApp()`
    call, or that call `shinyApp()` conditionally, will leave
    `captured_app == NULL` after preload. The FORK handler
    detects this and falls back to `runApp(bundle_path, ...)`
    in the child — identical to the pre-capture behaviour.
    The zygote logs `preload ... warning=no_shinyapp_captured`
    so operators can see the fallback is active. Bundle
    packages themselves still benefit from being pre-loaded,
    so startup latency is still improved.

    **Phase 3-10 layers up-front byte-compilation on top of
    this.** The same preload path will switch from `sys.source`
    to `compiler::cmpfile()` + `compiler::loadcmp()` so bundle
    closures are born as `BCODESXP` — a prerequisite for KSM
    finding stable pages to merge (see
    `phase-3-10-draft.md` decision K3). Phase 3-9 ships the
    simpler `sys.source` path since it captures the shinyApp
    object correctly and delivers the latency win unchanged.

    **What this does not catch (residuals):**

    - Files `source()`d from *inside* the server function at
      request time (e.g., `source("R/utils.R", local = TRUE)`).
      Those run in the child, not the zygote. Operators who
      care can call `compiler::cmpfile()` themselves in `app.R`
      before the server function is defined, which folds the
      helpers into the compiled bundle env.
    - Anonymous closures created at runtime
      (`factory <- function(x) function(y) x + y`). These are
      small in number and short-lived; residual JIT activity on
      them is bounded.
    - Lazy S4 / R6 method table materialization. Bounded by
      construction.

    Residual activity is small enough that we leave JIT enabled
    at the default level; disabling it would give up the speedup
    on runtime-created closures in exchange for a very small
    reduction in page divergence. Revisit if measurements show
    otherwise.

15. **Two-level opt-in for zygote: server-wide config flag AND
    per-app flag.** The zygote model is new territory in the R
    ecosystem — nothing like it exists in Shiny Server, Posit
    Connect, or any other R deployment tool. Operators have no
    institutional knowledge of the failure modes, no runbooks,
    no tuning intuition. Forcing the feature on by default
    would mean operators who upgrade blockyard start running
    unfamiliar code paths on their production traffic without
    asking for it. That's a bad default for anything
    experimental.

    **The design therefore has two independent gates, both
    default off, that must be opened together before any
    behaviour changes:**

    | Gate                  | Location   | Controls                              |
    |-----------------------|------------|---------------------------------------|
    | `experimental.zygote` | server toml | Whether the zygote code path runs at all |
    | `apps.zygote`         | db column  | Whether a specific app uses the zygote model |

    **Why server-wide AND per-app, not just per-app:** the
    server-wide gate protects operators who upgrade blockyard
    without reading the release notes — they get identical
    behaviour to the prior version until they explicitly opt
    into the new subsystem. The per-app gate lets operators
    who *have* opted in enable the experiment surgically, on
    one low-stakes app first, before rolling it to the rest of
    their fleet. Removing either gate would force operators
    into an all-or-nothing decision, which is exactly the
    wrong shape for experimental infrastructure.

    **Validation rules at the API layer (enforced in
    `UpdateApp` and `CreateApp`):** Setting `apps.zygote = true`
    requires `experimental.zygote = true` in server config →
    otherwise 400 with `"zygote feature not enabled in server
    config"`.

    **Runtime gating (orthogonal to validation):** Even if an
    app row has `apps.zygote = true` from an older config state,
    the runtime short-circuits to the non-zygote path whenever
    `experimental.zygote = false`. This gives operators a
    **kill switch**: flipping `experimental.zygote` to `false`
    in config and restarting the server instantly disables the
    feature for every app, without requiring database updates
    or API calls.

    **UI gating:** The settings tab reads the effective server
    config via the capabilities endpoint and disables the
    zygote toggle when `experimental.zygote` is off. Tooltip
    on the disabled toggle explains "set `experimental.zygote =
    true` in the server config to enable this feature."

    **Phase 3-10 layers the same shape on top for KSM** — a
    second `experimental.ksm` server flag and a per-app `ksm`
    column, both default off, both gated against the existing
    zygote flags. The two-level opt-in pattern is the
    structural primitive; phase 3-10 applies it twice rather
    than inventing a different shape. See
    `phase-3-10-draft.md` decision K4.

    **Alternatives considered:**

    - **Per-app only, no server-wide gate.** Operators who
      upgrade would inherit the new subsystem without opting
      in. Acceptable if the feature were mature, but for a
      new experimental subsystem it's wrong. Rejected.
    - **Server-wide only, no per-app gate.** Forces an
      all-or-nothing rollout, defeats the point of the
      per-app toggle. Rejected.

## Deferred

1. **Post-fork sandboxing.** Phase 3-10 lands `unshare(CLONE_NEWUSER
   | CLONE_NEWNS)`, private `/tmp` per child via mount namespace,
   seccomp-bpf, capability dropping, and per-process rlimits. Until
   3-10 lands, children share `/tmp` and other in-container resources.
   **The zygote model must not be enabled on multi-tenant production apps
   between phase 3-9 and phase 3-10.** The phase doc and the UI
   toggle warn about this explicitly. The two phases are intended
   to land back-to-back.

2. **Capacity-model guidance for zygote deployments.** Tracked in
   cynkra/blockyard#160. The `max_sessions_per_worker` and
   `pre_warmed_sessions` fields mean numerically the same thing in
   zygote mode as in shared mode — a pre-warmed zygote holds all
   its packages in the oldest GC generation with no children yet
   forked, costing roughly the same RSS as an idle shared-mode
   worker, and each active session is a forked child whose
   steady-state memory (after GC + KSM recovery) is approximately
   `per-session working set` on top of the shared base. The open
   question tracked in #160 is whether operators need extra
   tuning guidance (for example, recommended `pre_warmed_sessions`
   values for bundles with large package sets or long session
   lifetimes), not whether the existing fields mean the wrong
   thing. Phase 3-9 ships without that guidance and lets operator
   feedback drive its shape.

   **Worst-case headroom math** is a sub-question in scope for
   the same tracking issue, but really becomes concrete only
   once KSM lands in phase 3-10: that's when the transient RSS
   spike during coordinated GC recovery becomes the
   load-bearing concern. Phase 3-9 does not produce the spike
   (no KSM means no mass page dirtying to recover from), so the
   capacity math stays simple: `peak_RSS ≈ N × bundle + N ×
   per_session_delta` after R's generational GC has done its
   work in each child. Revisit once phase 3-10 ships and KSM is
   the dominant variable.

3. **Per-child cgroups in Docker.** Would let `memory_limit` mean
   "per session" in Docker the way it does in the process backend.
   Requires rootless cgroup delegation, which is not yet available
   in all supported deployments. Deferred until usage data
   indicates it's worth the complexity.

4. **Fork-safe package allowlist / metadata.** Some R packages are
   not safe to load before forking (rJava, arrow, anything with
   open fds or threads at load time). Phase 3-10's documentation
   covers the categories. Phase 3-9 ships without runtime checks
   — a bundle that loads fork-unsafe packages into the zygote
   will fail at fork time with an opaque error. Adding a
   bundle-build-time check (parse package list, warn on known-unsafe)
   is a follow-up.

5. **Bundle hot-swap interaction.** When a bundle is replaced, the
   zygote has loaded the old bundle's packages and can't switch.
   Today's transfer mechanism (`internal/server/transfer.go`)
   swaps workers between bundles by spawning new ones; for zygote apps
   this means spawning a new zygote with the new bundle and
   draining the old. The transfer logic is mostly orthogonal —
   it operates at the worker level — but the timing of "old
   zygote drained, new zygote ready" needs careful sequencing
   when N > 1 children are mid-session. Phase 3-9 inherits the
   existing transfer behaviour: drain marks the old zygote, the
   autoscaler eventually evicts it once children exit, new
   sessions go to the new zygote. Document this; revisit if it
   bites.

6. **Per-child logging.** All children currently write to the
   same container stdout, so `Logs(workerID)` returns interleaved
   output from the zygote and all children. For debugging this
   is annoying but not blocking. A follow-up could prefix each
   line with `[childID]` from inside the zygote.

7. **Shared control state extraction.** The `forkState` per-worker
   struct is duplicated between the Docker and process backend
   `forking.go` files. After both backends are working and
   tests are green, that struct can move into
   `internal/zygote/` or `internal/zygotectl/` as a shared type
   — but only if the test surface confirms the semantics are
   truly identical. Premature DRY here would be risky.

8. **Asymmetric / signed control auth.** The pre-shared secret
   model is sufficient because the per-worker bridge (Docker) and
   the per-worker UID + bwrap network namespace (process) already
   isolate the control endpoint. If usage shows that lateral
   movement from a service container to the control port becomes
   a concern, we can replace the shared secret with a JWT signed
   by the existing worker token signing key. Not needed for
   phase 3-9.

9. **KSM memory sharing and its full observability, preflight,
   threat-model, and capacity story.** Originally folded into
   this phase; carved out to phase 3-10 to keep the zygote
   mechanism and the KSM opt-in landable as separate units.
   See `phase-3-10-draft.md` — that document covers the C
   helper, the `STATS` control command, the Prometheus
   metrics, the preflight checks, the `oom_score_adj` pinning,
   the up-front byte-compilation, the two-level opt-in for
   `ksm`, and the multi-tenant side-channel story (Suzaki
   2011 / Bosman 2016). Nothing in phase 3-9 blocks phase 3-10
   from layering KSM on; the two-package split (decision #5)
   specifically exists to make the additive surface clean.
