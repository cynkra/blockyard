# Phase 3-2: Interface Extraction & Token Persistence

Two prerequisites for shared state (phase 3-3) and rolling updates
(phase 3-5): extract interfaces from the three in-memory stores so
Redis can implement the same contracts, and persist the worker token
signing key so both the old and new server verify the same tokens
during a rolling update.

Depends on phase 3-1 (migration tooling). No new dependencies.

---

## Prerequisites from Earlier Phases

- **Phase 3-1** -- migration tooling, expand-and-contract conventions.
  Phase 3-2 does not add migrations, but follows the same file and
  testing conventions established there.

## Deliverables

1. **Store interface** (`internal/session/iface.go`) -- extracted
   from the existing `Store` type.
2. **MemoryStore rename** -- `Store` becomes `MemoryStore`, constructor
   becomes `NewMemoryStore()`.
3. **WorkerRegistry interface** (`internal/registry/iface.go`) --
   extracted from the existing `Registry` type.
4. **MemoryRegistry rename** -- `Registry` becomes `MemoryRegistry`,
   constructor becomes `NewMemoryRegistry()`.
5. **WorkerMap interface** (`internal/server/workermap_iface.go`) --
   extracted from the existing `WorkerMap` type in `state.go`.
6. **MemoryWorkerMap extraction** -- implementation moves to
   `workermap_memory.go`, renamed to `MemoryWorkerMap`, constructor
   becomes `NewMemoryWorkerMap()`.
7. **Server struct update** -- field types change from concrete to
   interface.
8. **Worker token: OpenBao storage** -- persist the signing key in
   vault when `[openbao]` is configured.
9. **Worker token: file-based fallback** -- persist the signing key to
   disk when vault is not available.
10. **Tests** -- interface compliance, worker key round-trip (both
    paths), existing tests pass unchanged.

## Step-by-step

### Step 1: Store interface

New file `internal/session/iface.go`:

```go
package session

import "time"

// Store defines the contract for session state storage.
// MemoryStore is the in-process implementation; Redis implements
// the same interface for shared state during rolling updates.
type Store interface {
    Get(sessionID string) (Entry, bool)
    Set(sessionID string, entry Entry)
    Touch(sessionID string) bool
    Delete(sessionID string)
    DeleteByWorker(workerID string) int
    CountForWorker(workerID string) int
    CountForWorkers(workerIDs []string) int
    RerouteWorker(oldWorkerID, newWorkerID string) int
    EntriesForWorker(workerID string) map[string]Entry
    SweepIdle(maxAge time.Duration) int
}
```

Every method matches the existing `Store` method set exactly -- same
names, same signatures. No behavioral changes.

### Step 2: MemoryStore rename

In `internal/session/store.go`:

```go
// Before:
type Store struct {
    mu       sync.Mutex
    sessions map[string]Entry
}

func NewStore() *Store {
    return &Store{sessions: make(map[string]Entry)}
}

// After:
type MemoryStore struct {
    mu       sync.Mutex
    sessions map[string]Entry
}

func NewMemoryStore() *MemoryStore {
    return &MemoryStore{sessions: make(map[string]Entry)}
}
```

All method receivers change from `(s *Store)` to `(s *MemoryStore)`.
No logic changes. Add the compile-time interface check at the bottom
of the file:

```go
var _ Store = (*MemoryStore)(nil)
```

### Step 3: WorkerRegistry interface

New file `internal/registry/iface.go`:

```go
package registry

// WorkerRegistry defines the contract for worker address lookup.
// MemoryRegistry is the in-process implementation; Redis implements
// the same interface for shared state during rolling updates.
type WorkerRegistry interface {
    Get(workerID string) (string, bool)
    Set(workerID string, addr string)
    Delete(workerID string)
}
```

### Step 4: MemoryRegistry rename

In `internal/registry/registry.go`:

```go
// Before:
type Registry struct {
    mu    sync.Mutex
    addrs map[string]string
}

func New() *Registry {
    return &Registry{addrs: make(map[string]string)}
}

// After:
type MemoryRegistry struct {
    mu    sync.Mutex
    addrs map[string]string
}

func NewMemoryRegistry() *MemoryRegistry {
    return &MemoryRegistry{addrs: make(map[string]string)}
}
```

Method receivers change from `(r *Registry)` to `(r *MemoryRegistry)`.
Add the compile-time check:

```go
var _ WorkerRegistry = (*MemoryRegistry)(nil)
```

### Step 5: WorkerMap interface

New file `internal/server/workermap_iface.go`:

```go
package server

import "time"

// WorkerMap defines the contract for the active worker map.
// MemoryWorkerMap is the in-process implementation; Redis implements
// the same interface for shared state during rolling updates.
type WorkerMap interface {
    Get(id string) (ActiveWorker, bool)
    Set(id string, w ActiveWorker)
    Delete(id string)
    Count() int
    CountForApp(appID string) int
    All() []string
    ForApp(appID string) []string
    ForAppAvailable(appID string) []string
    MarkDraining(appID string) []string
    SetDraining(workerID string)
    SetIdleSince(workerID string, t time.Time)
    SetIdleSinceIfZero(workerID string, t time.Time)
    ClearIdleSince(workerID string) bool
    IdleWorkers(timeout time.Duration) []string
    AppIDs() []string
    IsDraining(appID string) bool
}
```

The `ActiveWorker` struct stays in `state.go` -- it is a value type
shared by all implementations.

### Step 6: MemoryWorkerMap extraction

Move the `WorkerMap` struct and all its methods from `state.go` into a
new file `internal/server/workermap_memory.go`. Rename to
`MemoryWorkerMap`:

```go
package server

import (
    "sync"
    "time"
)

// MemoryWorkerMap is a concurrent in-memory map of worker ID → ActiveWorker.
type MemoryWorkerMap struct {
    mu      sync.Mutex
    workers map[string]ActiveWorker
}

func NewMemoryWorkerMap() *MemoryWorkerMap {
    return &MemoryWorkerMap{workers: make(map[string]ActiveWorker)}
}

func (m *MemoryWorkerMap) Get(id string) (ActiveWorker, bool) {
    m.mu.Lock()
    defer m.mu.Unlock()
    w, ok := m.workers[id]
    return w, ok
}

// ... all other methods with receiver changed from *WorkerMap to
// *MemoryWorkerMap. Logic unchanged.

var _ WorkerMap = (*MemoryWorkerMap)(nil)
```

After extraction, `state.go` retains:

- The `Server` struct (updated in step 7)
- The `ActiveWorker` struct
- `NewServer()` and all `Server` methods
- The `installMus`, `transferring` maps and their helpers

The `WorkerMap` struct, `NewWorkerMap()`, and all 16 `WorkerMap`
methods are removed from `state.go`.

### Step 7: Server struct update

Change the three field types from concrete to interface in
`internal/server/state.go`:

```go
type Server struct {
    // ...
    Workers  WorkerMap                  // was *WorkerMap
    Sessions session.Store               // was *session.Store
    Registry registry.WorkerRegistry    // was *registry.Registry
    // ...
}
```

Update `NewServer()`:

```go
func NewServer(cfg *config.Config, be backend.Backend, database *db.DB) *Server {
    return &Server{
        Config:   cfg,
        Backend:  be,
        DB:       database,
        Workers:  NewMemoryWorkerMap(),
        Sessions: session.NewMemoryStore(),
        Registry: registry.NewMemoryRegistry(),
        Tasks:    task.NewStore(),
        LogStore: logstore.NewStore(),
    }
}
```

Update the `WorkerTokenKey` field comment to remove the "no persistence
needed" note:

```go
// Ephemeral HMAC key for worker tokens. Generated from crypto/rand
// at startup — independent of SessionSecret and OIDC. Workers are
// evicted on restart, so no persistence needed.

// HMAC key for worker tokens. Persisted via OpenBao or file-based
// fallback so both servers verify the same tokens during a rolling
// update. Independent of SessionSecret and OIDC.
```

### Step 8: Call site updates

Mechanical rename. Every call site already uses the method-based API --
the compiler verifies completeness. The changes are:

**Constructor calls** (only in test setup and `NewServer`):

| Call site | Before | After |
|-----------|--------|-------|
| `NewServer()` | `session.NewStore()` | `session.NewMemoryStore()` |
| `NewServer()` | `registry.New()` | `registry.NewMemoryRegistry()` |
| `NewServer()` | `NewWorkerMap()` | `NewMemoryWorkerMap()` |
| test helpers | `session.NewStore()` | `session.NewMemoryStore()` |
| test helpers | `registry.New()` | `registry.NewMemoryRegistry()` |
| test helpers | `NewWorkerMap()` | `NewMemoryWorkerMap()` |

**Type references** (in function signatures and struct fields):

| Location | Before | After |
|----------|--------|-------|
| `srv.Sessions` field | `*session.Store` | `session.Store` |
| `srv.Registry` field | `*registry.Registry` | `registry.WorkerRegistry` |
| `srv.Workers` field | `*WorkerMap` | `WorkerMap` |

No logic changes. No behavioral changes. The method sets are identical.

### Step 9a: `KVRead` sentinel error

`KVRead` currently wraps both "not found" and transient failures into
a generic `error`. Callers that do read-or-generate (like
`ResolveSessionSecret` and `ResolveWorkerKey` below) need to
distinguish the two: a missing key means generate, a vault error
means abort.

In `internal/integration/openbao.go`, add a sentinel and use it in
`KVRead`:

```go
// ErrNotFound is returned by KVRead when the secret path does not
// exist in vault. Callers can use errors.Is to distinguish this
// from transient failures.
var ErrNotFound = errors.New("secret not found")
```

```go
if resp.StatusCode == http.StatusNotFound {
    return nil, fmt.Errorf("openbao kv read %s: %w", path, ErrNotFound)
}
```

Then update `ResolveSessionSecret` to use the same pattern shown
below for `ResolveWorkerKey` -- treat `ErrNotFound` as "generate",
treat any other error as fatal. This is a small, backward-compatible
change to an existing function.

### Step 9b: Worker token -- OpenBao storage

New file `internal/integration/worker_key.go`:

```go
package integration

import (
    "context"
    "crypto/rand"
    "encoding/base64"
    "errors"
    "fmt"
    "log/slog"
)

const workerKeyKVPath = "blockyard/worker-signing-key"

// ResolveWorkerKey reads or generates the worker signing key from vault.
// Follows the same pattern as ResolveSessionSecret: read if exists,
// generate + store if not. Transient vault errors are fatal --
// only ErrNotFound triggers generation.
func ResolveWorkerKey(ctx context.Context, client *Client) ([]byte, error) {
    data, err := client.KVRead(ctx, workerKeyKVPath, client.AdminToken())
    if err != nil && !errors.Is(err, ErrNotFound) {
        return nil, fmt.Errorf("read worker key from vault: %w", err)
    }
    if err == nil {
        if v, ok := data["worker_signing_key"]; ok {
            if s, ok := v.(string); ok && s != "" {
                key, err := base64.RawURLEncoding.DecodeString(s)
                if err != nil {
                    return nil, fmt.Errorf("decode worker key from vault: %w", err)
                }
                if len(key) != 32 {
                    return nil, fmt.Errorf("worker key in vault has wrong length: %d", len(key))
                }
                slog.Info("worker signing key loaded from vault")
                return key, nil
            }
        }
    }

    // ErrNotFound: generate new key.
    key := make([]byte, 32)
    if _, err := rand.Read(key); err != nil {
        return nil, fmt.Errorf("generate worker signing key: %w", err)
    }

    // Store in vault.
    encoded := base64.RawURLEncoding.EncodeToString(key)
    if err := client.KVWrite(ctx, workerKeyKVPath, map[string]any{
        "worker_signing_key": encoded,
    }); err != nil {
        return nil, fmt.Errorf("store worker signing key in vault: %w", err)
    }

    slog.Info("auto-generated worker signing key (stored in vault)")
    return key, nil
}
```

The key is stored at `secret/data/blockyard/worker-signing-key`
under the field `worker_signing_key`, base64url-encoded (32 raw bytes
= 43 encoded characters).

### Step 10: Worker token -- file-based fallback

New file `internal/server/workerkey.go`:

```go
package server

import (
    "context"
    "crypto/rand"
    "encoding/base64"
    "fmt"
    "log/slog"
    "os"
    "path/filepath"

    "github.com/cynkra/blockyard/internal/auth"
    "github.com/cynkra/blockyard/internal/config"
    "github.com/cynkra/blockyard/internal/integration"
)

// LoadOrCreateWorkerKey resolves the worker signing key. It tries
// three sources in order:
//
//  1. OpenBao (if configured) -- read or generate + store
//  2. File ({bundle_server_path}/.worker-key) -- read existing
//  3. Generate new + write to file
//
// This ensures both the old and new server use the same key during
// a rolling update. When OpenBao is not available, the file path
// provides persistence across restarts.
func LoadOrCreateWorkerKey(
    ctx context.Context,
    vaultClient *integration.Client,
    cfg *config.Config,
) (*auth.SigningKey, error) {
    // Path 1: OpenBao.
    if vaultClient != nil {
        key, err := integration.ResolveWorkerKey(ctx, vaultClient)
        if err != nil {
            return nil, fmt.Errorf("resolve worker key via vault: %w", err)
        }
        return auth.NewSigningKey(key), nil
    }

    // Path 2/3: file-based.
    keyPath := filepath.Join(cfg.Storage.BundleServerPath, ".worker-key")
    return loadOrCreateWorkerKeyFile(keyPath)
}

// loadOrCreateWorkerKeyFile reads the key from disk if it exists,
// or generates a new one and writes it. File permissions: 0600.
func loadOrCreateWorkerKeyFile(path string) (*auth.SigningKey, error) {
    // Try reading existing file.
    data, err := os.ReadFile(path)
    if err == nil {
        key, err := base64.RawURLEncoding.DecodeString(string(data))
        if err != nil {
            return nil, fmt.Errorf("decode worker key file %s: %w", path, err)
        }
        if len(key) != 32 {
            return nil, fmt.Errorf("worker key file %s has wrong length: %d", path, len(key))
        }
        slog.Info("worker signing key loaded from file", "path", path)
        return auth.NewSigningKey(key), nil
    }

    if !os.IsNotExist(err) {
        return nil, fmt.Errorf("read worker key file %s: %w", path, err)
    }

    // Generate new key and write to file.
    raw := make([]byte, 32)
    if _, err := rand.Read(raw); err != nil {
        return nil, fmt.Errorf("generate worker signing key: %w", err)
    }

    encoded := base64.RawURLEncoding.EncodeToString(raw)
    if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
        return nil, fmt.Errorf("create worker key directory: %w", err)
    }
    if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
        return nil, fmt.Errorf("write worker key file: %w", err)
    }

    slog.Info("auto-generated worker signing key (stored to file)", "path", path)
    return auth.NewSigningKey(raw), nil
}
```

**File format:** base64url-encoded 32-byte key, no newline. Same
encoding as the vault path. File permissions `0600` (owner read/write
only).

**File location:** `{storage.bundle_server_path}/.worker-key`. This
directory already exists (it holds bundle data and `.worker-tokens/`)
and is persisted across restarts.

### Step 11: Startup flow change

In `cmd/blockyard/main.go`, replace the ephemeral key generation
(lines 134-140):

```go
// Before:
workerKeyBytes := make([]byte, 32)
if _, err := rand.Read(workerKeyBytes); err != nil {
    slog.Error("failed to generate worker token key", "error", err)
    os.Exit(1)
}
srv.WorkerTokenKey = auth.NewSigningKey(workerKeyBytes)

// After:
workerKey, err := server.LoadOrCreateWorkerKey(context.Background(), srv.VaultClient, &cfg)
if err != nil {
    slog.Error("failed to load or create worker signing key", "error", err)
    os.Exit(1)
}
srv.WorkerTokenKey = workerKey
```

The call moves from its current location (line 134, before OpenBao
init) to just after the OpenBao block (~line 206), so that
`srv.VaultClient` is available when the vault path is configured.
The reordered sequence:

```go
// 1. NewServer()
// 2. Preflight checks
// 3. OpenBao initialization (existing, sets srv.VaultClient)
// 4. LoadOrCreateWorkerKey(ctx, srv.VaultClient, &cfg)  ← moved here
// 5. Operation hooks
// 6. OIDC setup
// 7. HTTP listeners
```

**Why this is safe:** `WorkerTokenKey` has four consumers in the
codebase -- `proxy/coldstart.go` (worker spawn on first request),
`server/refresh.go` (drain-and-replace), `server/transfer.go`
(container transfer), and `api/router.go` (wiring `WorkerAuth`
middleware). All four are guarded by `if srv.WorkerTokenKey != nil`
and none execute before `api.NewRouter(srv)` at line 306 -- 100 lines
after the new location. Nothing between lines 134 and 306 reads
`WorkerTokenKey`: lines 142-148 set operation hooks and create the
background context, the OpenBao block (150-205) only touches vault
auth and session secrets, and `StartupCleanup` (line 270) only cleans
orphaned containers. No timing window exists.

The `crypto/rand` import in `main.go` can be removed -- this was its
only call site. Confirm with a grep before deleting.

### Step 12: Tests

#### Interface compliance tests

In `internal/session/store_test.go`:

```go
func TestMemoryStoreImplementsStore(t *testing.T) {
    // Compile-time check is in store.go:
    //   var _ Store = (*MemoryStore)(nil)
    // This test exists as documentation.
    var store Store = NewMemoryStore()
    _ = store
}
```

Same pattern for `internal/registry/registry_test.go` and
`internal/server/workermap_memory_test.go`. The real enforcement is
the `var _ Interface = (*Impl)(nil)` line -- the tests are
documentation.

#### Worker key round-trip: file path

In `internal/server/workerkey_test.go`:

```go
func TestWorkerKeyFileRoundTrip(t *testing.T) {
    dir := t.TempDir()
    keyPath := filepath.Join(dir, ".worker-key")

    // First call: generates and writes.
    key1, err := loadOrCreateWorkerKeyFile(keyPath)
    if err != nil {
        t.Fatal(err)
    }

    // Second call: reads existing.
    key2, err := loadOrCreateWorkerKeyFile(keyPath)
    if err != nil {
        t.Fatal(err)
    }

    // Verify same key.
    tok1, _ := testToken(key1)
    tok2, _ := testToken(key2)
    // Decode tok1 with key2 — must succeed.
    _, err = auth.DecodeSessionToken(tok1, key2)
    if err != nil {
        t.Fatalf("token signed with key1 should verify with key2: %v", err)
    }
    _, err = auth.DecodeSessionToken(tok2, key1)
    if err != nil {
        t.Fatalf("token signed with key2 should verify with key1: %v", err)
    }
}

func TestWorkerKeyFilePermissions(t *testing.T) {
    dir := t.TempDir()
    keyPath := filepath.Join(dir, ".worker-key")

    _, err := loadOrCreateWorkerKeyFile(keyPath)
    if err != nil {
        t.Fatal(err)
    }

    info, err := os.Stat(keyPath)
    if err != nil {
        t.Fatal(err)
    }
    if perm := info.Mode().Perm(); perm != 0o600 {
        t.Errorf("expected 0600 permissions, got %04o", perm)
    }
}

func TestWorkerKeyFileCorrupt(t *testing.T) {
    dir := t.TempDir()
    keyPath := filepath.Join(dir, ".worker-key")

    // Write garbage.
    os.WriteFile(keyPath, []byte("not-valid-base64!@#$"), 0o600)

    _, err := loadOrCreateWorkerKeyFile(keyPath)
    if err == nil {
        t.Fatal("expected error for corrupt key file")
    }
}

func TestWorkerKeyFileWrongLength(t *testing.T) {
    dir := t.TempDir()
    keyPath := filepath.Join(dir, ".worker-key")

    // Write valid base64 but wrong key length (16 bytes instead of 32).
    short := make([]byte, 16)
    rand.Read(short)
    os.WriteFile(keyPath, []byte(base64.RawURLEncoding.EncodeToString(short)), 0o600)

    _, err := loadOrCreateWorkerKeyFile(keyPath)
    if err == nil {
        t.Fatal("expected error for wrong-length key")
    }
}

// testToken creates a dummy worker token for verification.
func testToken(key *auth.SigningKey) (string, error) {
    claims := &auth.SessionTokenClaims{
        Sub: "worker:test",
        App: "test-app",
        Wid: "test-worker",
        Iat: time.Now().Unix(),
        Exp: time.Now().Add(15 * time.Minute).Unix(),
    }
    return auth.EncodeSessionToken(claims, key)
}
```

#### Worker key round-trip: vault path

In `internal/integration/worker_key_test.go`:

```go
// mockKVStore returns an http.Handler that implements KV v2 read/write
// with in-memory state. Reuses the mockBao helper from openbao_test.go.
func mockKVStore(t *testing.T) *Client {
    t.Helper()
    store := make(map[string]map[string]any) // path → data
    return mockBao(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // KV v2 paths: /v1/secret/data/{path}
        path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
        switch r.Method {
        case "GET":
            data, ok := store[path]
            if !ok {
                w.WriteHeader(http.StatusNotFound)
                return
            }
            json.NewEncoder(w).Encode(map[string]any{
                "data": map[string]any{"data": data},
            })
        case "PUT":
            var body struct {
                Data map[string]any `json:"data"`
            }
            json.NewDecoder(r.Body).Decode(&body)
            store[path] = body.Data
            w.WriteHeader(http.StatusOK)
        default:
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        }
    }))
}

func TestResolveWorkerKeyRoundTrip(t *testing.T) {
    client := mockKVStore(t)
    ctx := context.Background()

    // First call: generates and stores.
    key1, err := ResolveWorkerKey(ctx, client)
    if err != nil {
        t.Fatal(err)
    }
    if len(key1) != 32 {
        t.Fatalf("expected 32-byte key, got %d", len(key1))
    }

    // Second call: reads existing.
    key2, err := ResolveWorkerKey(ctx, client)
    if err != nil {
        t.Fatal(err)
    }

    if !bytes.Equal(key1, key2) {
        t.Fatal("second call should return the same key")
    }
}
```

#### LoadOrCreateWorkerKey integration

In `internal/server/workerkey_test.go`:

```go
func TestLoadOrCreateWorkerKeyNoVault(t *testing.T) {
    dir := t.TempDir()
    cfg := &config.Config{}
    cfg.Storage.BundleServerPath = dir

    // No vault client — uses file path.
    key1, err := LoadOrCreateWorkerKey(context.Background(), nil, cfg)
    if err != nil {
        t.Fatal(err)
    }

    // Second call — reads from file.
    key2, err := LoadOrCreateWorkerKey(context.Background(), nil, cfg)
    if err != nil {
        t.Fatal(err)
    }

    // Verify same key via token round-trip.
    tok, _ := testToken(key1)
    _, err = auth.DecodeSessionToken(tok, key2)
    if err != nil {
        t.Fatal("keys should match across calls")
    }
}
```

#### Existing tests

All existing tests pass with only constructor name changes:

- `session.NewStore()` → `session.NewMemoryStore()`
- `registry.New()` → `registry.NewMemoryRegistry()`
- `NewWorkerMap()` → `NewMemoryWorkerMap()`

No behavioral changes. Grep for these constructors and update.

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/session/iface.go` | **create** | `Store` interface (10 methods) |
| `internal/session/store.go` | **rename** | `Store` → `MemoryStore`, `NewStore()` → `NewMemoryStore()` |
| `internal/registry/iface.go` | **create** | `WorkerRegistry` interface (3 methods) |
| `internal/registry/registry.go` | **rename** | `Registry` → `MemoryRegistry`, `New()` → `NewMemoryRegistry()` |
| `internal/server/workermap_iface.go` | **create** | `WorkerMap` interface (16 methods) |
| `internal/server/workermap_memory.go` | **create** | `MemoryWorkerMap` implementation (extracted from `state.go`) |
| `internal/server/state.go` | **update** | Remove `WorkerMap` impl, update `Server` field types, update `NewServer()` |
| `internal/integration/openbao.go` | **update** | Add `ErrNotFound` sentinel, wrap 404 in `KVRead` |
| `internal/integration/session_secret.go` | **update** | Use `ErrNotFound` to distinguish missing from transient errors |
| `internal/integration/worker_key.go` | **create** | `ResolveWorkerKey()` -- vault persistence |
| `internal/server/workerkey.go` | **create** | `LoadOrCreateWorkerKey()` -- orchestrator + file fallback |
| `cmd/blockyard/main.go` | **update** | Replace ephemeral key gen with `LoadOrCreateWorkerKey()`, reorder after OpenBao init |
| `internal/server/workerkey_test.go` | **create** | File path round-trip, permissions, corrupt/wrong-length, no-vault integration |
| `internal/integration/worker_key_test.go` | **create** | Vault path round-trip |
| call sites (mechanical) | **update** | Constructor renames across ~20 files |

## Design decisions

1. **Interfaces in separate `iface.go` files, not alongside the
   implementation.** The v3 plan specifies this layout. It keeps the
   contract definition clean and makes it easy to find. The alternative
   (interface at the top of the implementation file) works for single
   implementations but becomes awkward when Redis adds a second file
   in phase 3-3.

2. **`MemoryStore` / `MemoryRegistry` / `MemoryWorkerMap` naming.**
   The `Memory` prefix distinguishes these from the Redis
   implementations added in phase 3-3. The alternative (`LocalStore`,
   `InMemoryStore`) is longer and no clearer.

3. **Worker key persistence follows `ResolveSessionSecret` exactly.**
   Same read-or-generate pattern, same KV path structure
   (`blockyard/<name>`), same base64url encoding, same admin token
   usage. Consistency means fewer surprises and the same error handling
   patterns apply.

4. **File-based fallback uses `BundleServerPath`, not a new config
   field.** This directory already exists, is persisted, and has the
   right lifecycle (it survives container restarts but is scoped to
   one installation). Adding a dedicated config field for a single
   file would be overengineering.

5. **No config changes in this phase.** The vault path
   (`blockyard/worker-signing-key`) and file path
   (`{bundle_server_path}/.worker-key`) are convention, not
   configuration. If a deployment needs to customize these, it can be
   added later -- but there's no evidence of that need today.

6. **Worker key resolution moves after OpenBao init in main.go.** The
   current ephemeral key generation sits before OpenBao init because
   it doesn't need vault. The persistent version does. Moving it after
   OpenBao init is the minimal reordering that makes the vault path
   work. All other startup ordering is unchanged.

7. **Interface fields, not pointer-to-interface, in `Server`.**
   The fields change from `*session.Store` (pointer to concrete) to
   `session.Store` (interface). Interfaces in Go are already
   reference types -- no `*` needed. This is the standard Go pattern
   for interface-typed struct fields.
