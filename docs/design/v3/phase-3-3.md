# Phase 3-3: Redis Shared State

Implement Redis-backed versions of the three store interfaces extracted
in phase 3-2 (`session.Store`, `WorkerRegistry`, `WorkerMap`). When `[redis]`
is configured, the server uses Redis for shared state; otherwise it
keeps the in-memory stores from phase 3-2. This is the prerequisite
for two servers to share session routing and worker state during a
rolling update (phases 3-4 and 3-5).

Depends on phase 3-2 (interface extraction). Adds one new dependency
(`go-redis/v9`) and one new package (`internal/redisstate`).

---

## Prerequisites from Earlier Phases

- **Phase 3-1** — migration tooling and conventions.
- **Phase 3-2** — `session.Store`, `WorkerRegistry`, and `WorkerMap`
  interfaces extracted; `Server` struct fields use interface types;
  worker signing key persisted; `CancelToken` extracted from
  `ActiveWorker` into `Server.cancelTokens` sync.Map.

## Deliverables

1. **go-redis/v9 dependency** — Redis client library added to `go.mod`.
2. **`[redis]` config section** — `RedisConfig` struct with `url` and
   `key_prefix` fields. Optional section (nil pointer), auto-constructed
   from `BLOCKYARD_REDIS_*` env vars.
3. **Redis client package** (`internal/redisstate/`) — shared
   `*redis.Client` wrapper with health check, key prefix helper, and
   Lua script registration.
4. **Redis session Store** (`internal/session/redis.go`) — implements
   `session.Store` using Redis hashes + TTL-based idle expiry.
5. **Redis WorkerRegistry** (`internal/registry/redis.go`) — implements
   `WorkerRegistry` using simple string keys.
6. **`ClearDraining` interface addition** — add
   `ClearDraining(workerID string)` to the `WorkerMap` interface,
   with implementations in both `MemoryWorkerMap` and
   `RedisWorkerMap`. Deferred from phase 3-2 (`0856ac8`).
7. **Redis WorkerMap** (`internal/server/workermap_redis.go`) —
   implements `WorkerMap` using Redis hashes.
8. **Startup store selection** — `cmd/blockyard/main.go` selects Redis
   or memory stores based on `cfg.Redis.URL`.
9. **`/readyz` integration** — Redis ping included in readiness checks
   when configured.
10. **Tests** — unit tests using miniredis (no build tag, no external
    dependencies). Integration tests in respective packages using a
    real Redis service container (`redis_test` build tag).

## Step-by-step

### Step 1: go-redis/v9 + miniredis dependencies

Add to `go.mod`:

```
require (
    github.com/redis/go-redis/v9  v9.x
    github.com/alicebob/miniredis/v2  v2.x  // test only
)
```

**miniredis** is an in-process Redis server written in Go. It supports
all data structures and Lua scripting used in this phase. It runs in
`go test` with no Docker dependency and no build tag, which means the
Redis store tests run as part of the normal `go test ./...` suite
alongside the existing memory store tests. Real-Redis integration tests
(tagged `redis_test`) run in CI only.

### Step 2: Config additions

New type in `internal/config/config.go`:

```go
type RedisConfig struct {
    URL       string `toml:"url"`        // redis://[:password@]host:port[/db]
    KeyPrefix string `toml:"key_prefix"` // default: "blockyard:"
}
```

Add to `Config`:

```go
type Config struct {
    // ... existing fields ...
    Redis *RedisConfig `toml:"redis"` // nil when not configured
}
```

Add to `applyEnvOverrides`:

```go
if cfg.Redis == nil && envPrefixExists("BLOCKYARD_REDIS_") {
    cfg.Redis = &RedisConfig{}
}
```

Add to `applyDefaults`:

```go
if cfg.Redis != nil && cfg.Redis.KeyPrefix == "" {
    cfg.Redis.KeyPrefix = "blockyard:"
}
```

Add to `validate` (before the URL check):

```go
if cfg.Redis != nil && cfg.Redis.KeyPrefix != "" && !strings.HasSuffix(cfg.Redis.KeyPrefix, ":") {
    cfg.Redis.KeyPrefix += ":"
}
```

This ensures the prefix always ends with `:` so that `Key("session", "abc")`
produces `"myprefix:session:abc"` rather than `"myprefixsession:abc"`.
The same guarantee is needed by Lua scripts that concatenate
`prefix .. "session:*"` for SCAN patterns.

Add to `validate`:

```go
if cfg.Redis != nil && cfg.Redis.URL == "" {
    return fmt.Errorf("[redis] section present but url is empty")
}
```

Environment variables: `BLOCKYARD_REDIS_URL`,
`BLOCKYARD_REDIS_KEY_PREFIX`.

### Step 3: Redis client package

New file `internal/redisstate/redisstate.go`:

```go
package redisstate

import (
    "context"
    "fmt"
    "log/slog"
    "time"

    "github.com/redis/go-redis/v9"

    "github.com/cynkra/blockyard/internal/config"
)

// Client wraps a Redis connection with a key prefix.
type Client struct {
    rdb    *redis.Client
    prefix string
}

// New parses the config, connects, and verifies with a PING.
func New(ctx context.Context, cfg *config.RedisConfig) (*Client, error) {
    opts, err := redis.ParseURL(cfg.URL)
    if err != nil {
        return nil, fmt.Errorf("parse redis url: %w", err)
    }

    rdb := redis.NewClient(opts)

    // Warn about unauthenticated connections (design decision 8).
    if opts.Password == "" {
        slog.Warn("redis connection has no authentication; consider enabling AUTH for production")
    }

    pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    if err := rdb.Ping(pingCtx).Err(); err != nil {
        rdb.Close()
        return nil, fmt.Errorf("redis ping: %w", err)
    }

    return &Client{rdb: rdb, prefix: cfg.KeyPrefix}, nil
}

// Close closes the underlying Redis connection.
func (c *Client) Close() error {
    return c.rdb.Close()
}

// Ping checks Redis connectivity.
func (c *Client) Ping(ctx context.Context) error {
    return c.rdb.Ping(ctx).Err()
}

// Key returns the prefixed key for the given components.
// Key("session", "abc") → "blockyard:session:abc"
func (c *Client) Key(parts ...string) string {
    k := c.prefix
    for i, p := range parts {
        if i > 0 {
            k += ":"
        }
        k += p
    }
    return k
}

// Redis returns the underlying go-redis client for direct use
// by store implementations.
func (c *Client) Redis() *redis.Client {
    return c.rdb
}

// Prefix returns the configured key prefix.
func (c *Client) Prefix() string {
    return c.prefix
}
```

The `Client` is intentionally thin — it owns the connection and the
key prefix, nothing else. Store implementations call `c.Redis()`
for direct access to the go-redis API and `c.Key(...)` for consistent
key construction.

### Step 4: Redis SessionStore

New file `internal/session/redis.go`:

```go
package session

import (
    "context"
    "log/slog"
    "strconv"
    "time"

    "github.com/redis/go-redis/v9"

    "github.com/cynkra/blockyard/internal/redisstate"
)

// RedisStore implements session.Store using Redis hashes.
//
// Key schema:
//   {prefix}session:{sessionID}  →  hash {worker_id, user_sub, last_access}
//
// Each session key has a TTL equal to idleTTL. Touch refreshes it.
// SweepIdle is a no-op — Redis TTL expiry handles idle cleanup.
type RedisStore struct {
    client  *redisstate.Client
    idleTTL time.Duration // from config: proxy.session_idle_ttl
}

func NewRedisStore(client *redisstate.Client, idleTTL time.Duration) *RedisStore {
    return &RedisStore{client: client, idleTTL: idleTTL}
}
```

**Key schema:** `{prefix}session:{sessionID}` → Redis hash with
fields:

| Field | Type | Example |
|-------|------|---------|
| `worker_id` | string | `"w-abc123"` |
| `user_sub` | string | `"Cg1kZW1v..."` (empty if no OIDC) |
| `last_access` | string (Unix seconds) | `"1712000000"` |

**TTL strategy:** when `proxy.session_idle_ttl > 0`, every session
key gets a TTL equal to `idleTTL`. `Set` and `Touch` both reset the
TTL via `EXPIRE`. `SweepIdle` becomes a no-op — Redis expires idle
sessions automatically. When `session_idle_ttl = 0` (the default,
meaning disabled), no TTL is set — sessions persist until explicit
deletion (matching the in-memory behavior where `SweepIdle` is
never called with a zero duration).

**Autoscaler interaction:** the autoscaler calls `SweepIdle` and logs
the count. With Redis, the count is always 0 so the log line
disappears. This is expected — TTL expiry replaces the sweep. The
autoscaler's `SetIdleSinceIfZero` loop still works correctly: on
the next tick after a session's TTL expires, `CountForWorker`
returns 0 and the worker gets marked idle.

**Method implementations:**

| Method | Redis operations |
|--------|-----------------|
| `Get` | `HGETALL {prefix}session:{id}` |
| `Set` | `HSET {prefix}session:{id} ... + EXPIRE` (pipeline) |
| `Touch` | `HSET {prefix}session:{id} last_access ... + EXPIRE` (pipeline) |
| `Delete` | `DEL {prefix}session:{id}` |
| `DeleteByWorker` | Lua: SCAN `{prefix}session:*`, check `worker_id` field, DEL matches |
| `CountForWorker` | Lua: SCAN + count matching `worker_id` |
| `CountForWorkers` | Lua: SCAN + count matching any of the given worker IDs |
| `RerouteWorker` | Lua: SCAN + HSET `worker_id` on matches |
| `EntriesForWorker` | SCAN + HGETALL pipeline (client-side; returns Go maps) |
| `SweepIdle` | No-op (returns 0). Redis TTL handles expiry natively. |

**Lua script for DeleteByWorker** (representative example — other
batch scripts follow the same SCAN + filter + mutate pattern):

```lua
local prefix = KEYS[1]
local worker_id = ARGV[1]
local cursor = "0"
local deleted = 0
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "session:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "worker_id") == worker_id then
            redis.call("DEL", key)
            deleted = deleted + 1
        end
    end
until cursor == "0"
return deleted
```

The SCAN-based approach is chosen over secondary indexes. At the
expected scale (max 100 workers → a few hundred sessions), a full
SCAN completes in under 1ms. Secondary indexes (a Redis SET per
worker tracking its session IDs) would avoid the scan but require
two-phase maintenance on every Set/Delete and create consistency
issues with TTL-based expiry (expired sessions aren't removed from
the set). SCAN is simpler and fast enough.

**Error handling:** Redis errors are logged at `slog.Error` level.
Methods return zero values on failure (`Get` returns `Entry{}, false`;
count methods return `0`; mutating methods return `0`). A separate
`/readyz` check (step 8) detects Redis unavailability and fails the
readiness probe, causing the load balancer to route traffic away. This
avoids adding `error` returns to the interface — see design decision 1.

### Step 5: Redis WorkerRegistry

New file `internal/registry/redis.go`:

```go
package registry

import (
    "context"
    "log/slog"
    "time"

    "github.com/cynkra/blockyard/internal/redisstate"
)

// RedisRegistry implements WorkerRegistry using simple Redis strings.
//
// Key schema:
//   {prefix}registry:{workerID}  →  string "host:port"
//
// Each key has a TTL equal to registryTTL. The health poller refreshes
// the TTL on every successful check by calling Set again. If the server
// crashes without cleanup, entries expire on their own.
type RedisRegistry struct {
    client      *redisstate.Client
    registryTTL time.Duration // 3× health_interval
}

func NewRedisRegistry(client *redisstate.Client, registryTTL time.Duration) *RedisRegistry {
    return &RedisRegistry{client: client, registryTTL: registryTTL}
}
```

**Key schema:** `{prefix}registry:{workerID}` → plain string value
(`"172.18.0.5:3838"`).

**Method implementations:**

| Method | Redis operation |
|--------|---------------|
| `Get` | `GET {prefix}registry:{id}` |
| `Set` | `SET {prefix}registry:{id} addr` + `EXPIRE` (pipeline) |
| `Delete` | `DEL {prefix}registry:{id}` |

**TTL strategy:** each registry key gets a TTL of 3× the configured
`proxy.health_interval` (default 15s → TTL 45s). The TTL is refreshed
on every `Set` call. The health poller already iterates all workers
and checks their addresses — after a successful health check, it calls
`registry.Set(workerID, addr)` which resets the TTL. If the server
crashes without cleanup, registry entries expire on their own within
45 seconds.

**Health poller change:** the health poller (`internal/ops/ops.go`,
`pollOnce`) iterates all worker IDs via `srv.Workers.All()` and calls
`srv.Backend.HealthCheck(ctx, id)` for each. It does not currently
interact with the registry. This phase adds a registry TTL refresh
after each successful health check:

```go
if r.healthy {
    if addr, ok := srv.Registry.Get(r.workerID); ok {
        srv.Registry.Set(r.workerID, addr)
    }
    delete(misses, r.workerID)
    continue
}
```

If `Registry.Get` returns `false` (worker not yet registered — e.g.,
spawn in progress or registry entry lost), the `Set` is skipped. This
is safe: the next successful spawn will re-register the address, and
the TTL will be refreshed on the following poll cycle. The
MemoryRegistry's `Set` is idempotent (re-setting the same value is a
no-op in practice), so this doesn't affect the in-memory path.

**Error handling:** same pattern as RedisStore — log + return zero
values.

### Step 6: `ClearDraining` interface addition

Deferred from phase 3-2: `drainAndReplace` restores workers on
failure via `Get` + modify `Draining` + `Set`, which is not atomic.
Add a dedicated method that only touches the `Draining` field.

Add to `internal/server/workermap_iface.go`:

```go
ClearDraining(workerID string)
```

Add to `internal/server/workermap_memory.go`:

```go
func (m *MemoryWorkerMap) ClearDraining(workerID string) {
    m.mu.Lock()
    defer m.mu.Unlock()
    if w, ok := m.workers[workerID]; ok {
        w.Draining = false
        m.workers[workerID] = w
    }
}
```

Update `internal/server/refresh.go` to use `ClearDraining` instead
of the `Get` + modify + `Set` pattern in the `drainAndReplace`
failure path.

### Step 7: Redis WorkerMap

New file `internal/server/workermap_redis.go`:

```go
package server

import (
    "context"
    "log/slog"
    "strconv"
    "time"

    "github.com/redis/go-redis/v9"

    "github.com/cynkra/blockyard/internal/redisstate"
)

// RedisWorkerMap implements WorkerMap using Redis hashes.
//
// Key schema:
//   {prefix}worker:{workerID}  →  hash {app_id, bundle_id, draining, idle_since, started_at, server_id}
//
// No TTL — workers are explicitly deleted on eviction.
type RedisWorkerMap struct {
    client   *redisstate.Client
    serverID string // os.Hostname(), written to every worker hash
}

func NewRedisWorkerMap(client *redisstate.Client, serverID string) *RedisWorkerMap {
    return &RedisWorkerMap{client: client, serverID: serverID}
}
```

**Key schema:** `{prefix}worker:{workerID}` → Redis hash:

| Field | Type | Notes |
|-------|------|-------|
| `app_id` | string | |
| `bundle_id` | string | |
| `draining` | string | `"0"` or `"1"` |
| `idle_since` | string | Unix seconds, `"0"` = not idle |
| `started_at` | string | Unix seconds |
| `server_id` | string | Hostname of the server that spawned this worker |

**`server_id` field:** written on `Set`, not read by any phase 3-3
code. Included now so phase 3-4 (multi-server) can scope
`GracefulShutdown` and `StartupCleanup` to the current server's
workers without migrating every existing worker hash. The value is
`os.Hostname()`, captured once at `RedisWorkerMap` construction.

**Serialization note:** `ActiveWorker` is fully serializable — phase
3-2 extracted `CancelToken func()` from `ActiveWorker` into
`Server.cancelTokens` (a process-local `sync.Map`). The Redis store
persists the five `ActiveWorker` data fields plus `server_id`
(which lives only in Redis, not in the Go struct).

**Method implementations:**

| Method | Redis operations |
|--------|-----------------|
| `Get` | `HGETALL {prefix}worker:{id}` → unmarshal to `ActiveWorker` |
| `Set` | `HSET {prefix}worker:{id} ...` (all fields) |
| `Delete` | `DEL {prefix}worker:{id}` |
| `Count` | Lua: SCAN `{prefix}worker:*`, count |
| `CountForApp` | Lua: SCAN + filter by `app_id` field, count |
| `All` | SCAN `{prefix}worker:*`, collect IDs (client-side) |
| `ForApp` | SCAN + HGET `app_id` pipeline, filter |
| `ForAppAvailable` | SCAN + HGETALL pipeline, filter `app_id` match + `draining == "0"` |
| `MarkDraining` | Lua: SCAN + HSET `draining "1"` on `app_id` matches, return matched IDs |
| `SetDraining` | Lua: `if EXISTS then HSET draining "1"` (guard against ghost entries) |
| `ClearDraining` | Lua: `if EXISTS then HSET draining "0"` (guard against ghost entries) |
| `SetIdleSince` | Lua: `if EXISTS then HSET idle_since {unix}` (guard against ghost entries) |
| `SetIdleSinceIfZero` | Lua: `if EXISTS then HGET idle_since`, if `"0"` then HSET |
| `ClearIdleSince` | Lua: `if EXISTS then HGET idle_since`, HSET `"0"`, return whether was non-zero |
| `IdleWorkers` | Lua: SCAN + filter non-zero `idle_since` older than timeout, exclude draining |
| `AppIDs` | Lua: SCAN + collect unique `app_id` values |
| `IsDraining` | Lua: SCAN + check if any worker with matching `app_id` has `draining == "1"` |

**Existence guards on single-field mutations:** `SetDraining`,
`ClearDraining`, `SetIdleSince`, and `ClearIdleSince` all use a Lua
script that checks `redis.call("EXISTS", key) == 1` before writing.
Without the guard, a bare `HSET` on a non-existent key *creates* the
hash — resurrecting a deleted worker as a ghost entry with only one
field. This matters in the `restoreOld` failure path of
`drainAndReplace`: the health poller may evict a worker between
`SetDraining` and the `ClearDraining` rollback, so the key may
already be gone. The memory implementations have the same guard
(`if w, ok := m.workers[workerID]; ok`). Representative script:

```lua
local key = KEYS[1]
if redis.call("EXISTS", key) == 1 then
    redis.call("HSET", key, ARGV[1], ARGV[2])
end
return 0
```

**Lua script for MarkDraining** (SCAN + mutate + collect IDs):

```lua
local prefix = KEYS[1]
local app_id = ARGV[1]
local cursor = "0"
local ids = {}
repeat
    local result = redis.call("SCAN", cursor, "MATCH", prefix .. "worker:*", "COUNT", 100)
    cursor = result[1]
    for _, key in ipairs(result[2]) do
        if redis.call("HGET", key, "app_id") == app_id then
            redis.call("HSET", key, "draining", "1")
            -- Extract workerID from "{prefix}worker:{id}"
            ids[#ids + 1] = string.sub(key, #prefix + #"worker:" + 1)
        end
    end
until cursor == "0"
return ids
```

The Go side deserializes the return value via `cmd.StringSlice()`.

SCAN is used for all list operations, same rationale as the session
store: at max 100 workers, SCAN completes in under 1ms. No secondary
indexes needed.

No TTL on worker keys. Workers have an explicit lifecycle managed by
the server — spawn creates, eviction deletes. Orphaned keys after a
crash are acceptable: on restart, the server runs `StartupCleanup`
which reconciles Docker containers against the worker map and removes
stale entries.

**Startup reconciliation note:** `StartupCleanup` (`internal/ops/ops.go`)
currently cleans orphaned Docker resources via `srv.Backend.ListManaged()`
and removes them, but it does not touch the worker map (memory stores
start empty). With Redis-backed state, worker keys persist across
restarts, so `StartupCleanup` must reconcile Redis state against
running Docker containers. Add a new block after the existing
`ListManaged` cleanup:

```go
// Reconcile Redis worker map against containers still running.
// With in-memory stores this is a no-op (All() returns empty).
workerIDs := srv.Workers.All()
if len(workerIDs) > 0 {
    // Re-query after the removal loop: what's actually running now?
    remaining, _ := srv.Backend.ListManaged(ctx)
    alive := make(map[string]bool, len(remaining))
    for _, r := range remaining {
        alive[r.ID] = true
    }
    var stale int
    for _, wid := range workerIDs {
        if !alive[wid] {
            srv.Workers.Delete(wid)
            srv.Sessions.DeleteByWorker(wid)
            srv.Registry.Delete(wid)
            stale++
        }
    }
    if stale > 0 {
        slog.Info("startup: removed stale worker entries from redis",
            "count", stale)
    }
}
```

This re-queries `ListManaged` after the removal loop to reconcile
against *current* container state, not the pre-cleanup snapshot.
In the single-server case, all containers were just removed so all
Redis entries are stale. In the future multi-server case (phase
3-4+), when container cleanup becomes selective, the other
instance's containers are still running so their Redis entries
survive — no rework needed.

For the in-memory path, `Workers.All()` returns empty (fresh map)
so the block is a no-op — no behavioral change for existing
deployments.

**Error handling:** same pattern as the other stores.

### Step 8: Startup store selection

In `cmd/blockyard/main.go`, after `NewServer()` and OpenBao init,
add Redis initialization:

```go
// ── Redis shared state (optional) ──
var redisClient *redisstate.Client
if cfg.Redis != nil {
    rc, err := redisstate.New(context.Background(), cfg.Redis)
    if err != nil {
        slog.Error("failed to connect to redis", "error", err)
        os.Exit(1)
    }
    defer rc.Close()
    redisClient = rc

    srv.Sessions = session.NewRedisStore(rc, cfg.Proxy.SessionIdleTTL.Duration)
    registryTTL := 3 * cfg.Proxy.HealthInterval.Duration
    srv.Registry = registry.NewRedisRegistry(rc, registryTTL)
    hostname, _ := os.Hostname()
    srv.Workers = server.NewRedisWorkerMap(rc, hostname)
    slog.Info("using redis for shared state",
        "url", maskRedisPassword(cfg.Redis.URL),
        "prefix", cfg.Redis.KeyPrefix)
}
```

This replaces the memory stores that `NewServer()` initialized by
default. The `Server` struct fields are already interface-typed (from
phase 3-2), so the assignment is a direct swap.

The `maskRedisPassword` helper redacts the password from the URL
before logging (replace password portion with `***`).

**Placement:** after the worker key resolution block (~line 204 in
`main.go`), before the OIDC and HTTP listener setup. The sequence
is: operation hooks → OpenBao init → worker key → **Redis init** →
OIDC → audit → telemetry → startup cleanup → HTTP listeners.

### Step 9: `/readyz` integration

In `internal/api/readyz.go`, add a Redis check when a Redis client
is available. Add a `RedisClient` field to the `Server` struct:

```go
// In internal/server/state.go:
// RedisClient — nil when [redis] is not configured.
RedisClient *redisstate.Client
```

In `readyz.go`, after the Docker check:

```go
if srv.RedisClient != nil {
    func() {
        ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
        defer cancel()
        if err := srv.RedisClient.Ping(ctx); err != nil {
            checks["redis"] = "fail"
        } else {
            checks["redis"] = "pass"
        }
    }()
}
```

The response includes a `"redis"` key only when Redis is configured,
matching the pattern used by the `"idp"`, `"openbao"`, and
`"vault_token"` checks.

### Step 10: Tests

#### Unit tests (miniredis, no build tag)

These run as part of the normal `go test ./...` suite.

**`internal/session/redis_test.go`:**

```go
func TestRedisStoreGetSet(t *testing.T) {
    mr := miniredis.RunT(t)
    client := redisstate.TestClient(t, mr.Addr())

    store := NewRedisStore(client, time.Hour)
    // ... same behavioral tests as store_test.go: Get/Set/Touch/Delete
}

func TestRedisStoreDeleteByWorker(t *testing.T) { ... }
func TestRedisStoreCountForWorker(t *testing.T) { ... }
func TestRedisStoreRerouteWorker(t *testing.T) { ... }
func TestRedisStoreEntriesForWorker(t *testing.T) { ... }
func TestRedisStoreSweepIdleIsNoOp(t *testing.T) {
    // Verify SweepIdle returns 0 and does not delete anything.
    // Instead, verify that expired keys are gone after TTL.
}
func TestRedisStoreTTLRefreshOnTouch(t *testing.T) {
    // Set session, advance miniredis clock past half the TTL,
    // Touch, verify key still exists after original TTL.
}
```

**`internal/registry/redis_test.go`:**

```go
func TestRedisRegistryGetSetDelete(t *testing.T) {
    mr := miniredis.RunT(t)
    client := redisstate.TestClient(t, mr.Addr())
    reg := NewRedisRegistry(client, 45*time.Second)
    // ... same behavioral tests as registry_test.go
}
```

**`internal/server/workermap_redis_test.go`:**

```go
func TestRedisWorkerMapBasicOps(t *testing.T) { ... }
func TestRedisWorkerMapForApp(t *testing.T) { ... }
func TestRedisWorkerMapDraining(t *testing.T) { ... }
func TestRedisWorkerMapIdleWorkers(t *testing.T) { ... }
func TestRedisWorkerMapRoundTrip(t *testing.T) {
    // Set worker, read back, verify all fields preserved.
}
```

**`internal/redisstate/redisstate_test.go`:**

```go
func TestClientKeyPrefix(t *testing.T) {
    // Verify Key("session", "abc") returns "blockyard:session:abc"
}
func TestClientPing(t *testing.T) { ... }
```

**Shared test helper** (`internal/redisstate/testutil.go`):

```go
// TestClient creates a redisstate.Client connected to the given addr
// (typically miniredis) with no authentication. Exported so
// session/registry/server test packages can use it.
func TestClient(t *testing.T, addr string) *Client {
    cfg := &config.RedisConfig{URL: "redis://" + addr + "/0", KeyPrefix: "test:"}
    c, err := New(context.Background(), cfg)
    if err != nil {
        t.Fatal("redisstate.TestClient:", err)
    }
    t.Cleanup(func() { c.Close() })
    return c
}
```

#### Integration tests (real Redis, `redis_test` build tag)

These use a real Redis service container in CI, following the pattern
of the `docker_test`, `idp_test`, and `openbao_test` suites.
Integration tests live in their respective packages (not centralized
in `redisstate`) so they can test each store without cross-package
import awkwardness.

**`internal/redisstate/redis_integration_test.go`:**

```go
//go:build redis_test

func TestRealRedisPing(t *testing.T) { ... }
```

**`internal/session/redis_integration_test.go`:**

```go
//go:build redis_test

func TestRealRedisSessionStoreRoundTrip(t *testing.T) { ... }
```

**`internal/registry/redis_integration_test.go`:**

```go
//go:build redis_test

func TestRealRedisRegistryRoundTrip(t *testing.T) { ... }
```

**`internal/server/workermap_redis_integration_test.go`:**

```go
//go:build redis_test

func TestRealRedisWorkerMapRoundTrip(t *testing.T) { ... }
```

Each integration test connects to the Redis instance at
`REDIS_TEST_ADDR` (set by CI) using `redisstate.TestClient`.

#### CI configuration

Add a `redis` job to `.github/workflows/ci.yml`, matching the
pattern of the `docker`, `idp`, and `openbao` jobs:

```yaml
redis:
  runs-on: ubuntu-24.04
  timeout-minutes: 10
  needs: [unit]
  if: github.event_name != 'merge_group' && github.event_name != 'push' && (inputs.job == '' || inputs.job == 'redis')
  services:
    redis:
      image: redis:7-alpine
      options: >-
        --health-cmd "redis-cli ping"
        --health-interval 5s
        --health-timeout 3s
        --health-retries 5
      ports:
        - 6379:6379
  steps:
    - uses: actions/checkout@v6
    - uses: actions/setup-go@v6
      with: { go-version-file: go.mod }
    - run: go test -tags redis_test -count=1 -coverprofile=coverage-redis.out -coverpkg=./internal/...,./cmd/by/... ./internal/redisstate/... ./internal/session/... ./internal/registry/... ./internal/server/...
      env:
        REDIS_TEST_ADDR: localhost:6379
    - uses: actions/upload-artifact@v7
      with:
        name: coverage-redis
        path: coverage-redis.out
        retention-days: 1
```

Also update the `coverage` job's `needs` to include `redis`, and
add `redis` to the `workflow_dispatch` job choices.

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `go.mod` / `go.sum` | **update** | Add `go-redis/v9`, `miniredis/v2` |
| `internal/config/config.go` | **update** | Add `RedisConfig` struct, `Redis *RedisConfig` field, env override, defaults, validation |
| `internal/redisstate/redisstate.go` | **create** | Redis client wrapper: `New()`, `Close()`, `Ping()`, `Key()`, `Redis()` |
| `internal/redisstate/redisstate_test.go` | **create** | Unit tests (miniredis) |
| `internal/redisstate/testutil.go` | **create** | `TestClient()` helper for test packages |
| `internal/redisstate/redis_integration_test.go` | **create** | Integration ping test (`redis_test` tag) |
| `internal/session/redis.go` | **create** | `RedisStore` implementing `session.Store` (hashes + TTL + Lua scripts) |
| `internal/session/redis_test.go` | **create** | Unit tests (miniredis) |
| `internal/session/redis_integration_test.go` | **create** | Integration round-trip test (`redis_test` tag) |
| `internal/registry/redis.go` | **create** | `RedisRegistry` implementing `WorkerRegistry` (string keys) |
| `internal/registry/redis_test.go` | **create** | Unit tests (miniredis) |
| `internal/registry/redis_integration_test.go` | **create** | Integration round-trip test (`redis_test` tag) |
| `internal/server/workermap_iface.go` | **update** | Add `ClearDraining(workerID string)` to `WorkerMap` interface |
| `internal/server/workermap_memory.go` | **update** | Add `ClearDraining` to `MemoryWorkerMap` |
| `internal/server/workermap_redis.go` | **create** | `RedisWorkerMap` implementing `WorkerMap` (hashes + Lua scripts) |
| `internal/server/workermap_redis_test.go` | **create** | Unit tests (miniredis) |
| `internal/server/workermap_redis_integration_test.go` | **create** | Integration round-trip test (`redis_test` tag) |
| `internal/server/refresh.go` | **update** | Use `ClearDraining` instead of `Get` + modify + `Set` in failure path |
| `internal/server/state.go` | **update** | Add `RedisClient *redisstate.Client` field to `Server` |
| `internal/ops/ops.go` | **update** | Add `registry.Set()` after successful health check (TTL refresh); add Redis worker map reconciliation to `StartupCleanup` |
| `internal/api/readyz.go` | **update** | Add Redis health check |
| `cmd/blockyard/main.go` | **update** | Redis init + store selection + `srv.RedisClient` assignment |
| `.github/workflows/ci.yml` | **update** | Add `redis` job |

## Design decisions

1. **No `context.Context` or `error` returns on the store interfaces.**
   The phase 3-2 interfaces match the existing method signatures — no
   context, no error returns. Redis operations are I/O that can fail,
   but adding context/error to the interfaces would change every call
   site (50+ locations) for a condition that should never occur under
   normal operation. Instead, Redis stores handle errors internally:
   log at error level, return zero values (Get returns `false`, counts
   return `0`), and the `/readyz` health check detects Redis
   unavailability at the operational level. The caller cannot
   distinguish "not found" from "Redis error" — but when Redis is
   down, `/readyz` fails within seconds and the load balancer stops
   routing new traffic. This matches the existing behavior for
   infrastructure failures (database, Docker): the server degrades
   rather than propagating errors through every code path.

2. **SCAN-based lookups, not secondary indexes.** Operations like
   `DeleteByWorker` and `ForApp` need to find entries matching a
   field value. Two approaches: (a) maintain a secondary Redis SET
   per worker/app containing the associated keys, or (b) SCAN all
   keys and filter. At the expected scale (max 100 workers, a few
   hundred sessions), a full SCAN completes in well under 1ms.
   Secondary indexes add two-phase write maintenance (hash + set on
   every mutation) and create consistency issues with TTL-based
   expiry (expired sessions aren't automatically removed from the
   set). SCAN is simpler, correct, and fast enough.

3. **TTL-based session expiry.** Sessions get a TTL equal to
   `proxy.session_idle_ttl` on creation. `Touch` (called on every
   proxy request) refreshes the TTL. `SweepIdle` becomes a no-op.
   This is simpler than the memory implementation's periodic sweep
   and lets Redis handle the expiry clock. The trade-off: no server
   log entry for individual session expirations (Redis TTL is silent).
   If debugging requires it, Redis keyspace notifications could be
   added later.

4. **TTL on registry keys, not on worker keys.** Registry entries
   get a TTL of 3× `health_interval` (default 45s), refreshed by the
   health poller after each successful check. This provides
   self-healing: if the server crashes, stale registry entries expire
   within 45 seconds rather than persisting indefinitely. The health
   poller change is minimal — one `Set` call after each successful
   probe. Worker keys do *not* get TTL: they have longer lifecycles,
   and orphaned worker keys are cleaned up by `StartupCleanup` which
   reconciles against Docker containers on restart. TTL on workers
   would require choosing an expiry window that doesn't race with
   legitimate idle timeouts — complexity for minimal benefit.

5. **`ActiveWorker` is already serializable.** Phase 3-2 extracted
   `CancelToken func()` from `ActiveWorker` into
   `Server.cancelTokens` (`sync.Map`). The Redis WorkerMap only needs
   to persist the five data fields (`AppID`, `BundleID`, `Draining`,
   `IdleSince`, `StartedAt`). Process-local cancel functions remain
   on the server that spawned the worker.

6. **Inline Lua scripts, not separate files.** The Lua scripts are
   short (10-20 lines each) and tightly coupled to the Go method
   they serve. Embedding them as `const` strings in the same file
   keeps the implementation self-contained. Each script is registered
   via `redis.NewScript()` at package init time and executed with
   `EVALSHA` (go-redis handles the fallback to `EVAL` automatically).

7. **miniredis for unit tests, real Redis for integration.** miniredis
   supports all Redis features used here (hashes, SCAN, Lua, TTL,
   time advancement). Unit tests with miniredis run in-process with
   no Docker dependency — they're as fast and reliable as the memory
   store tests. A separate `redis_test`-tagged integration suite with
   a real Redis container catches any miniredis fidelity gaps. This
   means the default `go test ./...` exercises both memory and Redis
   code paths.

8. **Redis AUTH recommended — startup warns on unauthenticated URLs.**
   `redisstate.New()` parses the URL and logs a warning at
   `slog.Warn` level if no password is present. This avoids
   rejecting legitimate test or development setups (e.g. miniredis,
   local Redis without AUTH) while alerting operators to harden
   production deployments. Combined with network isolation (dedicated
   internal Docker network in production), this provides
   defense-in-depth against workers reaching session data.

9. **Single Redis client, not a pool per store.** All three stores
   share one `redisstate.Client` (and thus one go-redis connection
   pool). At the expected request rate (tens of ops/sec per store),
   the default pool size (10 connections) is more than sufficient.
   This avoids resource waste and simplifies lifecycle management
   (one `defer rc.Close()`).

10. **`RedisClient` on Server struct, not a global.** The Redis
    client is stored as `srv.RedisClient` (nilable) so readyz and
    any future health-dependent logic can access it without import
    cycles or globals. Same pattern as `srv.VaultClient`.

11. **Single-instance Redis only — no Cluster support.** The Lua
    scripts use `SCAN` (which is node-local in a cluster) and pass
    the key prefix via `KEYS[1]` as a pattern, not an actual key.
    Both assumptions break under Redis Cluster, where keys are
    sharded and Lua scripts can only access keys on the same slot.
    At the target scale (≤100 workers, hundreds of sessions), a
    single Redis instance is more than sufficient. Cluster support
    would require replacing SCAN-based Lua scripts with secondary
    indexes or application-level sharding — complexity that isn't
    justified at this scale.
