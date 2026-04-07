# Phase 3-9: Pre-Fork Worker Model

An opt-in worker model where one container (Docker) or one bwrap
sandbox (process backend) runs a long-lived **zygote** R process that
pre-loads the bundle's packages, then forks one child process per
session on demand. Each child binds its own port; the proxy routes
the session directly to that port.

The mechanism is symmetrical across both backends. The shared logic
(session ↔ child bookkeeping, child-exit handling, cold-start hook)
lives in `internal/prefork/`. The backend-specific bits (control
transport, child reaping, R process spawn) live in each backend
behind a new optional `Forking` capability interface alongside
`Backend`.

Pre-fork is **experimental** and **opt-in per app** via a new
`pre_fork` boolean column. The existing shared-multiplexing mode
(`max_sessions_per_worker > 1` without pre-fork) is unchanged and
remains the default for multi-session apps.

Phase 3-9 lands the **mechanism** only. Phase 3-10 adds post-fork
sandboxing (private mount namespace, `/tmp` isolation, seccomp,
capability dropping, per-process rlimits).

---

## Prerequisites from earlier phases

- **Phase 3-1** — migration discipline. The `pre_fork` column
  follows expand-only rules: `ADD COLUMN ... NOT NULL DEFAULT 0`.
  The DDL linter, convention check, and roundtrip test all apply.
- **Phase 3-2** — interface extraction. The `session.Store` interface
  is the seam where the new `Entry.Addr` field gets added; both the
  memory and Redis implementations must round-trip it.
- **Phase 3-3** — Redis shared state. The Redis `SessionStore`
  implementation must persist and read back `Entry.Addr` so the
  field survives a rolling update.
- **Phase 3-6** — per-app config. This phase adds another per-app
  field (`pre_fork`) following the same pattern: DB column →
  `AppRow` → `AppUpdate` → API → CLI → UI.
- **Phase 3-7** — process backend core. The process backend's
  `Forking` implementation extends the bwrap spawn flow built here.
  Phase 3-9 assumes phase 3-7 leaves the network namespace shared
  (no `--unshare-net`), which makes loopback TCP a viable control
  transport.
- **Phase 3-8** — process backend packaging. Not directly required,
  but the seccomp profile finalised in 3-8 is what phase 3-10 will
  apply post-fork.

## Deliverables

1. **`pre_fork` column** on the `apps` table. Migration follows
   expand-only rules. Defaults to `0` (off). Validated to require
   `max_sessions_per_worker > 1`.
2. **`session.Entry.Addr`** — new field on the session store entry.
   Populated at session creation; read by the proxy on every
   subsequent request. Replaces the per-request `Registry.Get`
   lookup on the hot path. Round-trips through both `MemoryStore`
   and `RedisStore`.
3. **`Forking` capability sub-interface** in `internal/backend/` —
   optional capability that backends may implement. Three methods:
   `Fork`, `KillChild`, `ChildExits`. Plus a `ChildExit` value type.
4. **`internal/prefork/` package** — backend-agnostic. A `Manager`
   type that owns the session ↔ child bookkeeping, subscribes to
   `ChildExits()`, exposes `Fork(ctx, workerID, sessionID) (addr, error)`
   and `Kill(ctx, workerID, childID) error` to the rest of the server.
   The control protocol client (line-delimited TCP, `AUTH` first
   frame) lives here as a shared library used by both backend
   implementations.
5. **`internal/prefork/zygote.R`** — embedded R script. Loads the
   bundle's packages, listens on the control TCP port, handles
   `FORK`/`KILL`/`STATUS`/`AUTH`, pushes `CHILDEXIT` async on
   `waitpid()`. Embedded into the server binary via `//go:embed`,
   written to a host path at startup, bind-mounted into the worker
   container or bwrap sandbox.
6. **Docker `Forking` implementation** — adds zygote-mode container
   spawn, control port `3837` on the per-worker bridge, child port
   range allocation, control client wired to the shared protocol.
7. **Process `Forking` implementation** — adds zygote-mode bwrap
   spawn, control port allocation from a host-wide range, control
   client. Requires phase 3-7's port allocator.
8. **Per-worker control secret** — a 32-byte random secret written
   to the per-worker token dir at spawn (alongside the existing
   `token` file). Server holds the secret in memory; zygote reads
   it from the mounted token dir. Used as `AUTH` first frame.
9. **Cold-start integration** — `ensureWorker` calls
   `prefork.Manager.Fork` after spawning/finding a zygote worker
   for pre-fork apps. The returned address goes onto `session.Entry.Addr`.
10. **Proxy fallback** — extend the existing
    "session worker not in registry" fallback (`proxy.go:167`) to
    also cover "session addr unreachable", deleting the stale
    session and falling through to cold-start.
11. **Autoscaler integration** — when the idle sweep deletes a
    session, also call `prefork.Manager.Kill` for pre-fork sessions.
    Best-effort; failures logged.
12. **API/CLI/UI** — `pre_fork` field on `updateAppRequest`,
    `--pre-fork` flag on `by scale`, settings tab toggle in the UI
    (admin only).
13. **Tests** — interface compliance, control protocol unit tests,
    Docker integration test (zygote start, FORK two children,
    independent health, KILL one), process integration test (same
    flow under bwrap), session round-trip including the new `Addr`
    field.

---

## Step-by-step

### Step 1: Migration — `pre_fork` column

Migration `003_pre_fork` adds a single boolean column. Additive,
nullable-equivalent (default 0), backward-compatible per phase 3-1
rules. Phase 3-7 may insert a migration earlier in the sequence; if
it does, renumber this one accordingly.

**`internal/db/migrations/sqlite/003_pre_fork.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN pre_fork INTEGER NOT NULL DEFAULT 0;
```

**`internal/db/migrations/sqlite/003_pre_fork.down.sql`:**

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
    pre_warmed_seats        INTEGER NOT NULL DEFAULT 0,
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
    pre_warmed_seats, refresh_schedule, last_refresh_at,
    enabled, image, runtime
FROM apps;
DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;
```

**`internal/db/migrations/postgres/003_pre_fork.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN pre_fork BOOLEAN NOT NULL DEFAULT FALSE;
```

**`internal/db/migrations/postgres/003_pre_fork.down.sql`:**

```sql
ALTER TABLE apps DROP COLUMN pre_fork;
```

### Step 2: DB layer — `AppRow.PreFork`, `AppUpdate.PreFork`

Add to `AppRow` in `internal/db/db.go` (after `Runtime`, line 235):

```go
type AppRow struct {
    // ...existing fields...
    PreFork bool `db:"pre_fork" json:"pre_fork"`
}
```

Add to `AppUpdate` (line 587):

```go
type AppUpdate struct {
    // ...existing fields...
    PreFork *bool
}
```

Update `UpdateApp()` (line 591) to handle the new field:

```go
if u.PreFork != nil {
    app.PreFork = *u.PreFork
}
```

And add `pre_fork = ?` to the UPDATE SQL alongside the other fields,
plus `app.PreFork` to the bind list. Insert path (`CreateApp`)
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
read-back) or for non-prefork sessions where the resolution still
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

For the new code path (prefork apps), cold-start populates
`entry.Addr` directly from the fork return value (Step 9). For
non-prefork apps the field stays empty and the existing
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
// that support the pre-fork worker model. A pre-fork worker is a
// long-lived "zygote" process that has loaded the bundle's packages
// once, and can produce session-specific child processes on demand
// via Fork. Each child has its own network address.
//
// Backends that don't implement Forking simply omit the methods.
// Code that wants to fork checks for the capability via type
// assertion: `f, ok := srv.Backend.(backend.Forking)`.
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

### Step 5: `internal/prefork/` package

New package containing the backend-agnostic glue: control protocol
client (shared between Docker and process backend impls), Manager
type, session ↔ child bookkeeping.

**`internal/prefork/control.go`** — TCP control protocol client.
Shared between both backend `Forking` implementations.

```go
package prefork

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "net"
    "strconv"
    "strings"
    "sync"
    "time"
)

// ControlClient speaks the prefork control protocol over a single
// long-lived TCP connection. Used by both backend Forking
// implementations. The protocol is line-delimited:
//
//   client → server: AUTH <hex secret>\n
//   server → client: OK\n  or  ERR <reason>\n
//
//   client → server: FORK <port>\n
//   server → client: OK <childID> <pid>\n  or  ERR <reason>\n
//
//   client → server: KILL <childID>\n
//   server → client: OK\n  or  ERR <reason>\n
//
//   client → server: STATUS\n
//   server → client: <childID> <pid> <port> <state>\n... END\n
//
//   server → client (push, async): CHILDEXIT <childID> <exitCode> <reason>\n
//
// Single connection per worker. The reader goroutine dispatches
// request responses to the requesting goroutine via a per-request
// channel; CHILDEXIT pushes go to the Exits channel.
type ControlClient struct {
    addr   string
    secret []byte

    mu      sync.Mutex
    conn    net.Conn
    pending chan reply // current in-flight request reply

    Exits chan ChildExitMsg // CHILDEXIT pushes from server
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

// NewControlClient dials and authenticates against a zygote.
// Returns a ready client; the caller is responsible for Close().
func NewControlClient(ctx context.Context, addr string, secret []byte) (*ControlClient, error) {
    var d net.Dialer
    conn, err := d.DialContext(ctx, "tcp", addr)
    if err != nil {
        return nil, fmt.Errorf("control: dial: %w", err)
    }

    cc := &ControlClient{
        addr:    addr,
        secret:  secret,
        conn:    conn,
        Exits:   make(chan ChildExitMsg, 16),
        closed:  make(chan struct{}),
    }

    if err := cc.authenticate(); err != nil {
        conn.Close()
        return nil, err
    }

    go cc.readLoop()
    return cc, nil
}

func (c *ControlClient) authenticate() error {
    // Send AUTH <hex secret>\n; expect OK\n.
    line := fmt.Sprintf("AUTH %x\n", c.secret)
    if _, err := c.conn.Write([]byte(line)); err != nil {
        return fmt.Errorf("control: auth write: %w", err)
    }
    resp, err := bufio.NewReader(c.conn).ReadString('\n')
    if err != nil {
        return fmt.Errorf("control: auth read: %w", err)
    }
    if strings.TrimSpace(resp) != "OK" {
        return fmt.Errorf("control: auth rejected: %s", strings.TrimSpace(resp))
    }
    return nil
}

// Fork sends FORK <port> and returns (childID, pid).
func (c *ControlClient) Fork(ctx context.Context, port int) (childID string, pid int, err error) {
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
func (c *ControlClient) Kill(ctx context.Context, childID string) error {
    line, err := c.request(ctx, fmt.Sprintf("KILL %s\n", childID))
    if err != nil {
        return err
    }
    if !strings.HasPrefix(line, "OK") {
        return fmt.Errorf("control: KILL rejected: %s", line)
    }
    return nil
}

// request serialises one request/response pair on the connection.
func (c *ControlClient) request(ctx context.Context, line string) (string, error) {
    c.mu.Lock()
    if c.pending != nil {
        c.mu.Unlock()
        return "", errors.New("control: request already in flight")
    }
    ch := make(chan reply, 1)
    c.pending = ch
    _, err := c.conn.Write([]byte(line))
    c.mu.Unlock()
    if err != nil {
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
// CHILDEXIT pushes go to Exits.
func (c *ControlClient) readLoop() {
    defer close(c.closed)
    rd := bufio.NewReader(c.conn)
    for {
        line, err := rd.ReadString('\n')
        if err != nil {
            return
        }
        line = strings.TrimSpace(line)
        if strings.HasPrefix(line, "CHILDEXIT ") {
            c.pushExit(line)
            continue
        }
        // Synchronous reply.
        c.mu.Lock()
        ch := c.pending
        c.pending = nil
        c.mu.Unlock()
        if ch != nil {
            ch <- reply{line: line}
        }
    }
}

func (c *ControlClient) pushExit(line string) {
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

// Close shuts down the client.
func (c *ControlClient) Close() error {
    return c.conn.Close()
}

// Idle keep-alive: PING is not in the protocol; the connection is
// expected to live for the worker's lifetime. Detection of dead
// peers happens via TCP keepalive (set on the dialer in production
// code) or the next request returning an error.
var _ = time.Second // placeholder for keepalive setup
```

**`internal/prefork/manager.go`** — backend-agnostic orchestration.
Wraps a `backend.Forking` implementation and adds session ↔ child
bookkeeping plus exit-event handling.

```go
package prefork

import (
    "context"
    "fmt"
    "log/slog"
    "sync"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/session"
)

// Manager owns the session ↔ child bookkeeping for pre-fork workers.
// It is backend-agnostic — the backend-specific bits live in the
// Forking implementation it wraps.
type Manager struct {
    forking  backend.Forking
    sessions session.Store

    mu        sync.Mutex
    bySession map[string]childRef // sessionID → (workerID, childID)
}

type childRef struct {
    workerID string
    childID  string
}

func NewManager(forking backend.Forking, sessions session.Store) *Manager {
    m := &Manager{
        forking:   forking,
        sessions:  sessions,
        bySession: make(map[string]childRef),
    }
    go m.exitLoop()
    return m
}

// Fork creates a child for sessionID inside the given worker.
// Returns the child's network address. The mapping from sessionID
// to childID is recorded so KillChild can find the right child
// later, and so the child-exit handler can identify the session.
func (m *Manager) Fork(ctx context.Context, workerID, sessionID string) (addr string, err error) {
    addr, childID, err := m.forking.Fork(ctx, workerID, sessionID)
    if err != nil {
        return "", fmt.Errorf("prefork: fork: %w", err)
    }
    m.mu.Lock()
    m.bySession[sessionID] = childRef{workerID: workerID, childID: childID}
    m.mu.Unlock()
    slog.Debug("prefork: forked",
        "worker_id", workerID, "session_id", sessionID,
        "child_id", childID, "addr", addr)
    return addr, nil
}

// Kill terminates the child bound to sessionID, if any.
// Best-effort; failures are logged. Called by the autoscaler when
// it sweeps an idle session.
func (m *Manager) Kill(ctx context.Context, sessionID string) {
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
        slog.Warn("prefork: kill failed",
            "worker_id", ref.workerID, "child_id", ref.childID, "error", err)
    }
}

// HasChild returns true if a session has a tracked child. Used by
// the autoscaler to know whether to call Kill on sweep.
func (m *Manager) HasChild(sessionID string) bool {
    m.mu.Lock()
    _, ok := m.bySession[sessionID]
    m.mu.Unlock()
    return ok
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
        slog.Warn("prefork: child exited unexpectedly",
            "worker_id", ev.WorkerID, "child_id", ev.ChildID,
            "exit_code", ev.ExitCode, "reason", ev.Reason,
            "sessions", matched)
    } else {
        slog.Debug("prefork: child exited",
            "worker_id", ev.WorkerID, "child_id", ev.ChildID,
            "reason", ev.Reason)
    }
}
```

**`internal/prefork/secret.go`** — generation and read-back of the
per-worker control secret. Used by both backend `Forking` impls
when spawning a zygote and by the cleanup path.

```go
package prefork

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
        return nil, fmt.Errorf("prefork: secret rand: %w", err)
    }
    path := filepath.Join(tokenDir, "control.secret")
    if err := os.WriteFile(path, buf, 0o600); err != nil {
        return nil, fmt.Errorf("prefork: secret write: %w", err)
    }
    return buf, nil
}
```

The secret lives in the existing per-worker token directory
(`{BundleServerPath}/.worker-tokens/{workerID}/control.secret`),
which is already mounted read-only into the worker container at
`/var/run/blockyard/`. The zygote reads it from
`/var/run/blockyard/control.secret`.

### Step 6: `internal/prefork/zygote.R`

The zygote R script. Embedded into the server binary via `//go:embed`.
Loads packages, listens on the control port, handles control commands,
forks children, reaps via `waitpid()` in a background process.

**`internal/prefork/zygote.R`:**

```r
# blockyard zygote — long-lived R process that pre-loads packages
# and forks per-session children on demand.
#
# Reads from environment:
#   BLOCKYARD_BUNDLE_PATH      — path to the unpacked bundle
#   BLOCKYARD_CONTROL_PORT     — TCP port to listen on for control
#   BLOCKYARD_CONTROL_BIND     — IP to bind ("0.0.0.0" by default)
#   BLOCKYARD_SECRET_PATH      — path to control.secret file
#   BLOCKYARD_PORT_RANGE       — "lo-hi" port range for forked children
#   R_LIBS                     — set externally for the worker library
#
# Protocol: see internal/prefork/control.go for the wire format.

bundle_path  <- Sys.getenv("BLOCKYARD_BUNDLE_PATH",  "/shiny")
control_port <- as.integer(Sys.getenv("BLOCKYARD_CONTROL_PORT", "3837"))
secret_path  <- Sys.getenv("BLOCKYARD_SECRET_PATH",  "/var/run/blockyard/control.secret")
port_range   <- Sys.getenv("BLOCKYARD_PORT_RANGE",   "3839-3938")

# Read the control secret. The file is bind-mounted from the host
# and rotated alongside the worker token; we cache the bytes once
# at startup and compare on each AUTH frame.
secret_bytes <- readBin(secret_path, what = "raw", n = 32)
secret_hex   <- paste(format(as.hexmode(as.integer(secret_bytes)), width = 2),
                      collapse = "")

# Pre-load all packages declared by the bundle. We source the bundle
# entrypoint with shiny disabled so that pak/renv/etc. populate the
# search path without starting the app. Crashes here are fatal —
# the zygote is unusable if its packages didn't load.
preload_packages <- function() {
  app_r <- file.path(bundle_path, "app.R")
  if (file.exists(app_r)) {
    # Load the file in a sandbox env so it executes side-effecting
    # library() calls without invoking shiny::shinyApp().
    env <- new.env(parent = globalenv())
    env$shinyApp <- function(...) NULL
    env$runApp   <- function(...) NULL
    sys.source(app_r, envir = env)
  }
  gc()
}
preload_packages()

# Parse the port range.
port_range_parts <- as.integer(strsplit(port_range, "-")[[1]])
port_lo <- port_range_parts[1]
port_hi <- port_range_parts[2]

# Active children: childID → list(pid, port).
children <- new.env(parent = emptyenv())

# Allocate a child ID. Short hex from the system clock + counter.
child_id_counter <- 0L
next_child_id <- function() {
  child_id_counter <<- child_id_counter + 1L
  sprintf("c%x", child_id_counter)
}

# Reap child processes. Called from the main loop on a poll.
reap_children <- function() {
  exited <- character()
  for (cid in ls(children)) {
    info <- get(cid, envir = children)
    # waitpid is exposed via parallel:::mc.waitpid in stock R.
    res <- tryCatch(parallel:::mc.waitpid(info$pid, FALSE),
                    error = function(e) NULL)
    if (!is.null(res) && res != 0) {
      exited <- c(exited, cid)
      rm(list = cid, envir = children)
      # Push CHILDEXIT to the active control connection.
      reason <- if (res == info$pid) "normal" else "crashed"
      push_event(sprintf("CHILDEXIT %s %d %s\n", cid, 0L, reason))
    }
  }
}

# The control connection lifecycle: one client at a time (the
# server). Reconnects are expected during rolling updates.
con  <- NULL
push_event <- function(line) {
  if (!is.null(con)) {
    tryCatch(writeLines(line, con, sep = ""), error = function(e) NULL)
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
      # Child: close the control connection, run the app on the
      # assigned port. Exit when shiny::runApp returns.
      tryCatch({
        close(con)
        Sys.setenv(SHINY_PORT = as.character(port))
        shiny::runApp(bundle_path, port = port)
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
      writeLines("OK\n", con, sep = "")
    } else {
      writeLines("OK\n", con, sep = "")  # idempotent
    }
  } else if (cmd == "STATUS") {
    for (cid in ls(children)) {
      info <- get(cid, envir = children)
      writeLines(sprintf("%s %d %d alive\n", cid, info$pid, info$port),
                 con, sep = "")
    }
    writeLines("END\n", con, sep = "")
  } else if (cmd == "AUTH") {
    # Reauth on a fresh connection — handled in accept loop, not here.
    writeLines("ERR already authenticated\n", con, sep = "")
  } else {
    writeLines(sprintf("ERR unknown command %s\n", cmd), con, sep = "")
  }
}

# Accept loop. socketConnection blocks; we use a small timeout so
# we can interleave reap_children() polls.
srv <- serverSocket(control_port)
repeat {
  con <- socketAccept(srv, blocking = TRUE, open = "a+", timeout = 60)
  # AUTH must be the first frame.
  auth <- readLines(con, n = 1)
  if (length(auth) == 0 || sub("^AUTH ", "", auth) != secret_hex) {
    writeLines("ERR auth\n", con, sep = "")
    close(con)
    next
  }
  writeLines("OK\n", con, sep = "")
  # Command loop.
  repeat {
    line <- tryCatch(readLines(con, n = 1), error = function(e) character())
    if (length(line) == 0) break
    handle_command(line)
    reap_children()
  }
  close(con)
}
```

This is a sketch — actual code will need refinement, especially
around the R `parallel:::mcfork`/`mc.waitpid` semantics and the
non-blocking socket polling. R's stock socket API is limited; the
production version may want `httpuv` (which is already a Shiny dep
and so present in any pre-fork app) for cleaner async handling.

Embed it in `internal/prefork/embed.go`:

```go
package prefork

import _ "embed"

//go:embed zygote.R
var ZygoteScript []byte
```

### Step 7: Docker `Forking` implementation

New file `internal/backend/docker/forking.go`. Implements the
`backend.Forking` interface for the Docker backend. Uses the shared
`prefork.ControlClient` for the wire protocol.

Key elements:

```go
package docker

import (
    "context"
    "fmt"
    "log/slog"
    "sync"

    "github.com/cynkra/blockyard/internal/backend"
    "github.com/cynkra/blockyard/internal/prefork"
)

// dockerForking adds the Forking capability to DockerBackend.
// It is composed into DockerBackend rather than a separate type
// so type assertions on backend.Forking just work.

// Per-worker control state, kept on DockerBackend alongside workers.
type forkState struct {
    client      *prefork.ControlClient
    secret      []byte
    portRangeLo int
    portRangeHi int
    nextPort    int
    childAddrs  map[string]string // childID → "ip:port"
    childPort   map[string]int    // childID → port (for free-list)
    mu          sync.Mutex
}

// Fork implements backend.Forking.
func (d *DockerBackend) Fork(ctx context.Context, workerID, sessionID string) (string, string, error) {
    d.mu.Lock()
    ws, ok := d.workers[workerID]
    d.mu.Unlock()
    if !ok || ws.fork == nil {
        return "", "", fmt.Errorf("prefork: worker %s is not a zygote", workerID)
    }

    ws.fork.mu.Lock()
    port := ws.fork.allocPortLocked()
    ws.fork.mu.Unlock()
    if port == 0 {
        return "", "", fmt.Errorf("prefork: no free ports for worker %s", workerID)
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
    if port, ok := ws.fork.childPort[childID]; ok {
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

// allocPortLocked / releasePortLocked: simple sequential allocator
// over the configured range. The actual implementation will use a
// bitset or free-list for stability.
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

func (s *forkState) releasePortLocked(_ int) { /* no-op for the linear scan */ }
```

`d.childExits` is a `chan backend.ChildExit` initialised in
`NewDockerBackend`. A goroutine per worker translates `prefork.ChildExitMsg`
into `backend.ChildExit` and forwards onto the shared channel:

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
`spec.PreFork`. When set:

1. The container `Cmd` becomes `["R", "-f", "/blockyard/zygote.R"]`
   instead of the current `shiny::runApp(...)`.
2. Additional env vars: `BLOCKYARD_BUNDLE_PATH`,
   `BLOCKYARD_CONTROL_PORT=3837`, `BLOCKYARD_PORT_RANGE`,
   `BLOCKYARD_SECRET_PATH=/var/run/blockyard/control.secret`.
3. The zygote.R bind mount is added: host path
   `{BundleServerPath}/.zygote/zygote.R` → container path
   `/blockyard/zygote.R` (read-only).
4. After `ContainerStart`, the server waits for the control port
   to accept connections (TCP probe with backoff), then
   `prefork.NewControlClient(ctx, "ip:3837", secret)`.
5. The connected `ControlClient` is stored in `ws.fork`.

The existing `HealthCheck` continues to use the shiny port for
non-prefork workers; for prefork workers it probes the control
port instead (the zygote being responsive on the control port is
the right liveness signal).

`WorkerSpec` gets a small extension:

```go
type WorkerSpec struct {
    // ...existing fields...
    PreFork       bool   // zygote mode
    ControlSecret []byte // 32-byte secret to bind into the worker
    PortRange     string // "lo-hi" for child ports (zygote mode only)
}
```

`ControlSecret` is generated by the cold-start path via
`prefork.WriteSecret(tokenDir)` and attached to the spec.
`PortRange` is read from a new `[docker] prefork_port_range` config
field (default `3839-3938`).

### Step 8: Process backend `Forking` implementation

Mirror of step 7 for the process backend. Lives in
`internal/backend/process/forking.go` (a new file inside the package
that phase 3-7 establishes). Differences from the Docker version:

- Spawn: bwrap invocation with the zygote.R script bind-mounted via
  `--ro-bind {bundleServerPath}/.zygote/zygote.R /blockyard/zygote.R`
  and the per-worker token dir via
  `--ro-bind {tokenDir} /var/run/blockyard`. The R command is
  `R -f /blockyard/zygote.R`.
- Control transport: TCP on `127.0.0.1:{allocatedControlPort}`.
  The control port is allocated from a host-wide range maintained
  by phase 3-7's port allocator (extended in this phase to also
  serve control ports). The bwrap sandbox shares the host network
  namespace, so the loopback dial works.
- Child port allocation: same range concept as Docker, but the
  range is per-worker (phase 3-7's allocator hands out a slice of
  ports per worker for shiny + a control port).
- Child reaping: the bwrapped R zygote `parallel:::mcfork`s exactly
  as in the Docker case; `waitpid` works because the children are
  in the zygote's PID namespace (phase 3-7 sets `--unshare-pid`).
- ChildExit translation: identical pattern to Docker — one goroutine
  per worker drains `client.Exits` onto the shared
  `childExits chan backend.ChildExit`.

The structural similarity is large enough that the
`forking.go` files in both backends could share helper functions
in `internal/prefork/`. The `Manager.Fork`/`Manager.Kill`
already lives there; the per-worker control state (`forkState`)
could too if it doesn't reach into backend-specific types. For
phase 3-9 I'd duplicate it in each backend and DRY in a follow-up
once both are working — premature abstraction risk.

### Step 9: Cold-start integration

Two files change: `internal/proxy/coldstart.go` (the spawn path)
and `internal/proxy/proxy.go` (the session-creation path).

**`coldstart.go` — `spawnWorker` and `ensureWorker`.**

When `app.PreFork`, the spec gets the new fields populated and the
control secret is generated:

```go
// In spawnWorker, after the existing token-refresher block:
var controlSecret []byte
if app.PreFork && tokDir != "" {
    var err error
    controlSecret, err = prefork.WriteSecret(tokDir)
    if err != nil {
        cleanupLocal()
        return "", "", fmt.Errorf("prefork: write secret: %w", err)
    }
}

spec := backend.WorkerSpec{
    // ...existing fields...
    PreFork:       app.PreFork,
    ControlSecret: controlSecret,
    PortRange:     srv.Config.Docker.PreforkPortRange,
}
```

The Cmd construction also changes: for prefork apps the spec.Cmd
is left empty (the backend constructs the right zygote invocation),
otherwise the existing shiny::runApp Cmd is used.

`ensureWorker` calls into the Manager after the worker is healthy:

```go
// In ensureWorker, after the existing lb.Assign / spawnWorker
// block, before returning:
if app.PreFork {
    addr, err := srv.Prefork.Fork(ctx, wid, sessionID)
    if err != nil {
        return "", "", fmt.Errorf("prefork: fork: %w", err)
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

The `srv.Prefork` field is a `*prefork.Manager`, initialised in
`cmd/blockyard/main.go` after the backend is constructed:

```go
if forking, ok := backend.(backend.Forking); ok {
    srv.Prefork = prefork.NewManager(forking, srv.Sessions)
}
```

`srv.Prefork` is `nil` if the configured backend doesn't implement
`Forking`. Cold-start checks `app.PreFork && srv.Prefork != nil`
before calling `Fork` and falls through to a clear error
("backend does not support pre-fork") otherwise. The API layer
also rejects setting `pre_fork=true` when the backend doesn't
support it (Step 12).

### Step 10: Proxy fallback for unreachable child

The existing session-resolution block in `proxy.go` (around line 159)
already has a fallback for "session worker not in registry" — it
falls through to creating a new session. For prefork we extend this
to "session addr present but unreachable", which detects the case
where a child died between the Manager's exit-event handler and
this request.

The cleanest place is around the `forwardHTTP`/`shuttleWS` dispatch.
Currently those just hit "bad gateway" on a connection refused. We
add a small probe just before the dispatch:

```go
// proxy.go, just before the WebSocket vs HTTP dispatch block.
if isPreforkSession(srv, sessionID) {
    if !addrReachable(addr) {
        slog.Debug("proxy: prefork session unreachable, re-cold-starting",
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

`addrReachable` is a 50ms TCP dial. The check is gated on prefork
sessions only (cheap check via `srv.Prefork.HasChild(sessionID)`)
so non-prefork apps see no overhead. The 307 redirect forces the
browser to retry without the stale session cookie — the new
request runs the `isNewSession` path and forks a new child.

This is best-effort. The Manager's exit-event handler is the
authoritative path; the redirect is a fallback for the gap between
"child dies" and "Manager processes the exit".

### Step 11: Autoscaler integration

When the autoscaler's idle sweep deletes a session
(`internal/proxy/autoscaler.go`, the `SweepIdle` call site), also
kill the corresponding child:

```go
// In the sweep loop, before/after the existing Sessions.Delete call:
killed := srv.Sessions.SweepIdle(srv.Config.Proxy.SessionIdleTTL.Duration)
// SweepIdle returns deleted sessionIDs in v3 (extend the interface
// minimally) so we can kill their children.
for _, sid := range killed {
    if srv.Prefork != nil && srv.Prefork.HasChild(sid) {
        srv.Prefork.Kill(context.Background(), sid)
    }
}
```

`SweepIdle` currently returns just an `int` count. Extend the
interface to return `[]string` of deleted session IDs, with both
memory and Redis implementations updated. This is a minimal session
store interface change.

Same hook is needed in:

- The explicit logout path (delete-session API endpoint).
- The OIDC user-mismatch path in `proxy.go` (`if entry.UserSub !=
  callerSub`).
- Worker eviction in `ops.EvictWorker` (which calls
  `Sessions.DeleteByWorker` — extend that to return the list too,
  or call `Manager.Kill` for each session before deletion).

### Step 12: API / CLI / UI surface for `pre_fork`

Mirror the phase 3-6 pattern.

**API** — extend `updateAppRequest` in `internal/api/apps.go`:

```go
type updateAppRequest struct {
    // ...existing fields...
    PreFork *bool `json:"pre_fork"`
}
```

Validation in `UpdateApp()`:

```go
if body.PreFork != nil && *body.PreFork {
    // Pre-fork only makes sense for multi-session apps.
    effectiveMax := app.MaxSessionsPerWorker
    if body.MaxSessionsPerWorker != nil {
        effectiveMax = *body.MaxSessionsPerWorker
    }
    if effectiveMax <= 1 {
        badRequest(w, "pre_fork requires max_sessions_per_worker > 1")
        return
    }
    // Backend must support Forking.
    if _, ok := srv.Backend.(backend.Forking); !ok {
        badRequest(w, "configured backend does not support pre_fork")
        return
    }
}
```

Add `PreFork` to `appResponseV2()` in `internal/api/runtime.go`
and to `swagger_types.go`.

**CLI** — extend `by scale` in `cmd/by/scale.go`:

```go
cmd.Flags().Bool("pre-fork", false,
    "Enable pre-fork worker model (experimental, requires --max-sessions > 1)")

if cmd.Flags().Changed("pre-fork") {
    v, _ := cmd.Flags().GetBool("pre-fork")
    body["pre_fork"] = v
}
```

**UI** — admin-only toggle in `tab_settings.html` next to the
existing per-app config fields:

```html
{{if .IsAdmin}}
<div class="field-group">
    <label for="pre-fork">Pre-fork worker model</label>
    <p class="field-description">
        <em>Experimental.</em> When enabled, each session runs in a
        forked R child inside a shared zygote container. Requires
        Max sessions per worker &gt; 1.
        <a href="...prefork docs link...">Learn more</a>.
    </p>
    <input type="checkbox" id="pre-fork" name="pre_fork"
           {{if .App.PreFork}}checked{{end}}
           hx-patch="/api/v1/apps/{{.App.ID}}"
           hx-include="[name='pre_fork']"
           hx-swap="none">
</div>
{{end}}
```

### Step 13: Config additions

Two new fields on `DockerConfig`:

```go
type DockerConfig struct {
    // ...existing fields...
    PreforkPortRange   string // "3839-3938"; child ports for zygote workers
    PreforkControlPort int    // 3837; zygote control port on the per-worker bridge
}
```

Defaults applied in `applyDefaults()`:

```go
if c.PreforkPortRange == "" {
    c.PreforkPortRange = "3839-3938"
}
if c.PreforkControlPort == 0 {
    c.PreforkControlPort = 3837
}
```

The control port must not collide with `ShinyPort` (3838) or any
port in `PreforkPortRange`. Validate at startup.

For the process backend, phase 3-7's port allocator config gains
two analogous fields (`prefork_port_range`, `prefork_control_range`)
or one combined range that the allocator subdivides per worker.
Exact shape depends on phase 3-7's design.

### Step 14: Tests

#### Unit tests

**`internal/prefork/control_test.go`** — control protocol over a
loopback test server:

```go
func TestControlClient_AuthOK(t *testing.T)
// Spin up a test TCP listener that speaks the protocol; verify
// AUTH succeeds with the right secret.

func TestControlClient_AuthRejected(t *testing.T)
// Same with wrong secret → returns auth error.

func TestControlClient_ForkAndKill(t *testing.T)
// FORK 3839 → OK c1 12345; KILL c1 → OK.

func TestControlClient_ChildExitPushed(t *testing.T)
// Test server pushes CHILDEXIT; client surfaces it on Exits.

func TestControlClient_ConnectionClose(t *testing.T)
// Drop the connection mid-request; pending request returns error.
```

**`internal/prefork/manager_test.go`** — using a mock `Forking`:

```go
type mockForking struct {
    forks    chan forkCall
    kills    chan killCall
    childExits chan backend.ChildExit
}

func TestManager_ForkRecordsBookkeeping(t *testing.T)
// Manager.Fork → mockForking.Fork called → bookkeeping has the
// session, HasChild returns true.

func TestManager_KillRemovesBookkeeping(t *testing.T)
// Manager.Fork then Manager.Kill → bookkeeping cleared, mock
// KillChild called.

func TestManager_ChildExitDeletesSession(t *testing.T)
// Manager.Fork → mockForking pushes ChildExit → session.Delete
// called with the right sessionID, bookkeeping cleared.
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
// Fork a child. SIGKILL its PID via a control hook (or include
// a "die" command in the test zygote). Verify ChildExits emits.

func TestDockerForking_ControlAuthRejected(t *testing.T)
// Spawn a zygote. Connect with wrong secret. Verify the connection
// is dropped after the AUTH frame.
```

**`internal/backend/process/forking_integration_test.go`** (tagged
`process_test`) — analogous tests for the bwrap-sandboxed zygote.
Skipped when bwrap is unavailable, same pattern as the rest of the
process backend tests.

**`internal/proxy/coldstart_test.go`** — extend with prefork-aware
cold-start:

```go
func TestEnsureWorker_PreForkCallsManagerFork(t *testing.T)
// app.PreFork=true. Spawn returns a worker. Verify Manager.Fork
// is called with the sessionID and its return addr is what
// ensureWorker returns.

func TestEnsureWorker_PreForkBackendNotSupported(t *testing.T)
// app.PreFork=true but backend doesn't implement Forking.
// Verify clear error.
```

#### DB and migration tests

```go
func TestUpdateApp_PreForkRequiresMultiSession(t *testing.T)
// PATCH with pre_fork=true and max_sessions_per_worker=1 → 400.

func TestUpdateApp_PreForkRoundTrip(t *testing.T)
// Set pre_fork=true, read back, verify.

// Migration round-trip is covered by the existing TestMigrateRoundtrip
// from phase 3-1.
```

---

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/db/db.go` | **update** | `PreFork` on `AppRow` and `AppUpdate`, UPDATE SQL |
| `internal/backend/backend.go` | **update** | `Forking` interface, `ChildExit` type, `WorkerSpec.PreFork`/`ControlSecret`/`PortRange` |
| `internal/backend/docker/docker.go` | **update** | Spawn branch for prefork (zygote Cmd, env vars, mount, control client connect, healthcheck on control port) |
| `internal/session/store.go` | **update** | `Addr` field on `Entry`, `SweepIdle` returns `[]string` |
| `internal/session/iface.go` | **update** | `SweepIdle` signature change |
| `internal/session/redis.go` | **update** | Hash schema gains `addr` field, `SweepIdle` returns deleted IDs |
| `internal/proxy/proxy.go` | **update** | Pass `sessionID` to `ensureWorker`, populate `Entry.Addr`, prefork unreachable-child fallback |
| `internal/proxy/coldstart.go` | **update** | Generate `ControlSecret`, attach to spec, call `Manager.Fork` for prefork apps, `ensureWorker` signature change |
| `internal/proxy/autoscaler.go` | **update** | Kill prefork children on session sweep |
| `internal/api/apps.go` | **update** | `pre_fork` field on request, validation (multi-session, backend support) |
| `internal/api/runtime.go` | **update** | Add `pre_fork` to `appResponseV2()` |
| `internal/api/swagger_types.go` | **update** | Add `pre_fork` to swagger response type |
| `internal/ui/templates/tab_settings.html` | **update** | Pre-fork toggle, admin-gated |
| `cmd/by/scale.go` | **update** | `--pre-fork` flag |
| `cmd/blockyard/main.go` | **update** | Construct `prefork.Manager` when backend implements `Forking` |
| `internal/server/state.go` | **update** | `Prefork *prefork.Manager` field on `Server` |
| `internal/config/config.go` | **update** | `PreforkPortRange`, `PreforkControlPort` on `DockerConfig` |
| `internal/ops/evict.go` | **update** | Kill prefork children before deleting sessions |

## New files

| File | Purpose |
|------|---------|
| `internal/db/migrations/sqlite/003_pre_fork.up.sql` | Migration up (SQLite) |
| `internal/db/migrations/sqlite/003_pre_fork.down.sql` | Migration down (SQLite) |
| `internal/db/migrations/postgres/003_pre_fork.up.sql` | Migration up (PostgreSQL) |
| `internal/db/migrations/postgres/003_pre_fork.down.sql` | Migration down (PostgreSQL) |
| `internal/prefork/control.go` | TCP control protocol client (shared between backends) |
| `internal/prefork/control_test.go` | Control protocol unit tests |
| `internal/prefork/manager.go` | `Manager` type, session ↔ child bookkeeping, exit handler |
| `internal/prefork/manager_test.go` | Manager unit tests with mock `Forking` |
| `internal/prefork/secret.go` | Per-worker control secret generation |
| `internal/prefork/secret_test.go` | Secret round-trip test |
| `internal/prefork/zygote.R` | Embedded zygote R script |
| `internal/prefork/embed.go` | `//go:embed` declaration |
| `internal/backend/docker/forking.go` | Docker `Forking` implementation |
| `internal/backend/docker/forking_integration_test.go` | Docker prefork integration tests (`docker_test`) |
| `internal/backend/process/forking.go` | Process `Forking` implementation |
| `internal/backend/process/forking_integration_test.go` | Process prefork integration tests (`process_test`) |

## Design decisions

1. **Session addressing via `session.Entry.Addr`.** The smallest
   routable unit becomes the session, not the worker. The proxy
   reads `entry.Addr` on every request; cold-start populates it
   from the fork return value (prefork) or registry lookup
   (non-prefork). This kills a level of indirection on the hot
   path and gives prefork sessions a natural home for their
   per-child address. Alternatives considered: extending the
   `WorkerRegistry` interface to be `(workerID, sessionID)`-keyed
   (too much surface, breaks Redis schema), and computing child
   ports from `hash(sessionID)` (collisions, doesn't survive
   restart). Both rejected.

2. **`Forking` as an optional capability sub-interface.** The Go
   convention for optional capabilities (`io.Reader` /
   `io.WriterTo`). Backends that don't implement pre-fork simply
   omit the methods; the proxy does a type assertion at startup
   (`srv.Backend.(backend.Forking)`) and only constructs the
   `Manager` if the assertion succeeds. The `Backend` interface
   stays minimal and the prefork concept doesn't leak into
   backends that don't support it.

3. **Pre-fork is opt-in per app and coexists with shared
   multi-session mode.** The plan does not deprecate
   `max_sessions_per_worker > 1` without `pre_fork` — the existing
   shared-R multiplexing remains the default for multi-session
   apps. Pre-fork is an experimental alternative gated by the
   per-app `pre_fork` flag. This keeps the surface area of the
   experiment contained and easy to back out.

4. **TCP control transport on the per-worker bridge / loopback,
   with a pre-shared secret.** The plan originally sketched
   `docker exec` per fork. Rejected: 50–200ms exec overhead per
   session start, unnecessary Docker API dependency on the proxy
   hot path. Bind-mounted Unix socket alternative also rejected:
   socket file permissions are owned by the worker UID, which
   conflicts with phase 3-7's per-worker UID assignment plan.
   TCP on the per-worker bridge (Docker) or loopback (process)
   sidesteps both problems. Authentication is a 32-byte
   pre-shared secret in the per-worker token directory, sent as
   `AUTH <hex>` first frame. Defense in depth on top of the
   existing per-worker bridge isolation (which is the primary
   security boundary).

5. **Shared `prefork.ControlClient` and `prefork.Manager`.** The
   wire protocol and session-bookkeeping logic are identical
   across backends. They live in `internal/prefork/` and both
   `Forking` implementations import them. Only the dial address
   and the per-worker spawn details differ between backends.

6. **Resource limit semantics unchanged.** `memory_limit` and
   `cpu_limit` keep their current meaning per backend: Docker
   enforces a pool cap on the container cgroup (matching today's
   `max_sessions_per_worker > 1` behaviour), process backend
   ignores them with a warning (consistent with phase 3-7's "no
   per-worker cgroups" stance). No new fields, no auto-scaling,
   no migration. Documented in the prefork prose.

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
   (one goroutine, one reconnect path) and the volume is low
   (a few events per minute at most). Risks of starvation are
   addressed by the bounded `Exits` channel.

10. **Best-effort proxy fallback for unreachable children.** The
    Manager's `ChildExits` handler is the authoritative removal
    path. The proxy adds a 50ms TCP probe before dispatching
    prefork sessions and 307-redirects on failure, which catches
    the gap between "child died" and "Manager processed the
    exit". The redirect forces the browser to re-cold-start
    transparently. Probe is gated on `srv.Prefork.HasChild` so
    non-prefork apps see no overhead.

11. **`pre_fork` validation requires `max_sessions_per_worker > 1`.**
    Pre-forking with one session per worker is just a more
    expensive way to spawn one R process per session. Validation
    rejects the combination at the API layer with a clear error.

12. **`SweepIdle` returns deleted session IDs.** The session store
    interface gains a small return-value change so the autoscaler
    can call `Manager.Kill` on each swept session. Memory and
    Redis implementations both return the list. Backwards-compat
    is fine because the interface is not stable across major
    versions and phase 3-9 already lands schema and interface
    changes.

## Deferred

1. **Post-fork sandboxing.** Phase 3-10 lands `unshare(CLONE_NEWUSER
   | CLONE_NEWNS)`, private `/tmp` per child via mount namespace,
   seccomp-bpf, capability dropping, and per-process rlimits. Until
   3-10 lands, children share `/tmp` and other in-container resources.
   **Pre-fork must not be enabled on multi-tenant production apps
   between phase 3-9 and phase 3-10.** The phase doc and the UI
   toggle warn about this explicitly. The two phases are intended
   to land back-to-back.

2. **Pre-warm semantics for prefork.** Tracked separately in
   cynkra/blockyard#160. Phase 3-9 documents the obvious mapping
   ("a pre-warmed prefork worker is an idle zygote — first session
   forks instantly") and adopts whatever counting semantics that
   issue lands on. The interaction is functionally correct under
   the current worker-counting semantics; only the *number* of
   pre-warmed workers might be larger than ideal until #160 lands.

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
   swaps workers between bundles by spawning new ones; for prefork
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
   tests are green, that struct can move into `internal/prefork/`
   as a shared type — but only if the test surface confirms the
   semantics are truly identical. Premature DRY here would be
   risky.

8. **Asymmetric / signed control auth.** The pre-shared secret
   model is sufficient because the per-worker bridge (Docker) and
   the per-worker UID + bwrap network namespace (process) already
   isolate the control endpoint. If usage shows that lateral
   movement from a service container to the control port becomes
   a concern, we can replace the shared secret with a JWT signed
   by the existing worker token signing key. Not needed for
   phase 3-9.
