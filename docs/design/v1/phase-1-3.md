# Phase 1-3: OpenBao Integration + Credential Injection

Deliver per-user credentials to Shiny apps at runtime via OpenBao (Vault
fork). Phase 1-1 established user identity and phase 1-2 added
authorization; this phase uses the authenticated user's IdP access token
to obtain scoped OpenBao tokens and inject them into proxied requests.

Identity header injection (`X-Shiny-User`, `X-Shiny-Groups`) was
completed in phase 1-2 and is not repeated here.

This phase depends on phases 1-1 (OIDC sessions, access tokens) and 1-2
(CallerIdentity, RBAC). OpenBao is an optional dependency — when
`[openbao]` is not configured, credential injection is skipped and the
server behaves as before.

## Design decisions

### Per-request token injection via HTTP headers

OpenBao tokens are injected per-request as the `X-Blockyard-Vault-Token`
header. This matches Posit Connect's model for OAuth Integrations: R
code reads `session$request$HTTP_X_BLOCKYARD_VAULT_TOKEN` at session
init.

**Note (updated in phase 1-5):** raw vault tokens in headers are safe
for single-tenant containers (`max_sessions_per_worker = 1`) but not
for shared containers — they could leak between co-tenant sessions if
the app logs request headers or stores them in a shared variable.
Phase 1-5 introduces a two-phase exchange pattern for shared
containers: the proxy injects a signed session reference token
(`X-Blockyard-Session-Token`) instead, and the app exchanges it for
the real vault credential via `POST /api/v1/credentials/vault`.

### VAULT_ADDR as container environment variable

The OpenBao address is the same for all users and all requests, so it is
injected once at container startup as the `VAULT_ADDR` environment
variable (recognized natively by Vault/OpenBao client libraries). This
is added to `WorkerSpec.Env` when `[openbao]` is configured.

### Token caching

Per-request calls to OpenBao's JWT login endpoint would be expensive.
The `VaultTokenCache` caches scoped tokens keyed by user `sub` with TTL
matching `[openbao] token_ttl` (default 1h). Cache misses trigger a JWT
login; cache hits return the existing token. The cache uses a renewal
buffer (30s before expiry) to avoid serving tokens that expire
mid-request.

### Credential enrollment dual auth

The `POST /api/v1/users/me/credentials/{service}` endpoint accepts both
session cookie auth (app-plane users) and PAT bearer auth (control-plane
clients). Users can only manage their own credentials — the `sub` comes
from the authenticated identity, never from the URL.

**(Updated in v1 wrap-up §2):** JWT bearer auth on the control plane
was replaced by Personal Access Tokens (PATs).

### Soft dependency on OpenBao

If `[openbao]` is configured but OpenBao is unreachable at startup, the
server logs a warning and starts anyway. Credential injection fails
gracefully (logs warning, omits headers). The `/readyz` endpoint (phase
1-6) will report OpenBao as unhealthy.

## Deliverables

1. **`internal/integration/` package** — OpenBao HTTP client, JWT login,
   KV v2 read/write, health check, bootstrap verification
2. **`VaultTokenCache`** — in-memory cache keyed by `sub`, avoids
   per-request OpenBao calls
3. **Per-request credential injection** — exchange user's access token
   for scoped OpenBao token, inject `X-Blockyard-Vault-Token` header
4. **`VAULT_ADDR` env var** — added to worker containers when `[openbao]`
   is configured
5. **Credential enrollment API** — `POST /api/v1/users/me/credentials/{service}`
   with dual auth (session cookie + JWT bearer)
6. **Config additions** — `[openbao]` section
7. **OpenBao bootstrap** — startup verification of OpenBao state
8. **Tests** — unit tests with `httptest.Server` mock + integration tests
   against real OpenBao container (`//go:build openbao_test`)

## Config additions

```toml
[openbao]
address = "https://bao.example.com"
role_id = "blockyard-server"   # AppRole role ID (not secret)
# secret_id via BLOCKYARD_OPENBAO_SECRET_ID env var at bootstrap only
# admin_token = "hvs.xxx"     # deprecated — use role_id + secret_id instead
token_ttl = "1h"               # default: 1h
jwt_auth_path = "jwt"          # default: "jwt"
```

```go
type OpenbaoConfig struct {
    Address     string   `toml:"address"`
    RoleID      string   `toml:"role_id"`       // AppRole role ID
    AdminToken  Secret   `toml:"admin_token"`   // deprecated — use RoleID
    TokenTTL    Duration `toml:"token_ttl"`     // default: 1h
    JWTAuthPath string   `toml:"jwt_auth_path"` // default: "jwt"
}
```

Validation: when `[openbao]` is set, `address` is required along with
either `role_id` (preferred) or `admin_token` (deprecated). Both
cannot be set simultaneously. `[openbao]` requires `[oidc]` (JWT
login needs an IdP).

**(Updated in v1 wrap-up §4A):** `admin_token` is deprecated. The
preferred auth method is AppRole via `role_id` + `secret_id` env var.
See [v1 wrap-up §4](wrap-up.md#4-secret-lifecycle) for the full
design.

Env var auto-construction: if any `BLOCKYARD_OPENBAO_*` env var is set,
the `[openbao]` section is created with defaults applied (same pattern
as `[oidc]`).

## Package: `internal/integration/`

### openbao.go — OpenBao HTTP client

```go
// Client is a lightweight HTTP client for OpenBao's REST API.
// It wraps net/http and targets only the endpoints blockyard needs:
// JWT auth login, KV v2 read/write, sys/health.
//
// (Updated in v1 wrap-up §4A): the client accepts a token callback
// (adminTokenFunc) rather than a static token, so AppRole-issued
// tokens can be transparently renewed. NewClient accepts either a
// static token or an AppRole config.
type Client struct {
    addr           string              // OpenBao base URL
    adminTokenFunc func() string       // returns current admin token
    httpClient     *http.Client
}

func NewClient(addr string, adminTokenFunc func() string) *Client

// Health checks if OpenBao is reachable and unsealed.
// GET {addr}/v1/sys/health
func (c *Client) Health(ctx context.Context) error

// JWTLogin exchanges an IdP access token for a scoped OpenBao token.
// POST {addr}/v1/auth/{mountPath}/login
// Body: {"role": "blockyard-user", "jwt": "{accessToken}"}
// Returns the client token and its TTL.
func (c *Client) JWTLogin(ctx context.Context, mountPath, accessToken string) (token string, ttl time.Duration, err error)

// KVWrite writes a secret to the KV v2 secrets engine.
// POST {addr}/v1/secret/data/{path}
// Used by credential enrollment (admin token auth).
func (c *Client) KVWrite(ctx context.Context, path string, data map[string]any) error

// KVRead reads a secret from the KV v2 secrets engine.
// GET {addr}/v1/secret/data/{path}
func (c *Client) KVRead(ctx context.Context, path string, token string) (map[string]any, error)
```

### bootstrap.go — Startup verification

```go
// Bootstrap verifies OpenBao is configured correctly for blockyard.
// Checks:
// 1. OpenBao is reachable and unsealed (GET /v1/sys/health)
// 2. JWT auth method is enabled at the configured path
// 3. The "blockyard-user" role exists
// 4. KV v2 secrets engine is mounted at "secret/"
// 5. At least one attached policy uses per-user path scoping (warning only)
//
// Returns nil if all checks pass. Returns an error describing the
// first failure. The caller decides whether to treat this as fatal.
func Bootstrap(ctx context.Context, client *Client, jwtAuthPath string) error
```

### tokencache.go — Token cache

```go
// VaultTokenCache caches OpenBao tokens keyed by user sub.
type VaultTokenCache struct {
    mu     sync.RWMutex
    tokens map[string]*cachedToken
}

type cachedToken struct {
    Token     string
    ExpiresAt time.Time
}

func NewVaultTokenCache() *VaultTokenCache

// Get returns a cached token if it exists and has at least 30 seconds
// of remaining validity. Returns ("", false) on miss or near-expiry.
func (c *VaultTokenCache) Get(sub string) (string, bool)

// Set stores a token with the given TTL.
func (c *VaultTokenCache) Set(sub string, token string, ttl time.Duration)

// Delete removes a cached token (e.g. on logout).
func (c *VaultTokenCache) Delete(sub string)

// Sweep removes all expired tokens from the cache.
// Called periodically by the health poller.
func (c *VaultTokenCache) Sweep() int
```

### enrollment.go — Credential enrollment logic

```go
// EnrollCredential writes a user's credential for a service into
// OpenBao's KV v2 store at secret/data/users/{sub}/apikeys/{service}.
// Uses the admin token, not the user's scoped token.
func EnrollCredential(ctx context.Context, client *Client, sub, service string, data map[string]any) error
```

## WorkerSpec changes

Add an `Env` field to `WorkerSpec` for additional environment variables:

```go
type WorkerSpec struct {
    // ... existing fields ...
    Env map[string]string // additional env vars (e.g. VAULT_ADDR)
}
```

In `coldstart.go`, populate from config when `[openbao]` is configured:

```go
if srv.Config.Openbao != nil {
    spec.Env = map[string]string{
        "VAULT_ADDR": srv.Config.Openbao.Address,
    }
}
```

In `docker.go`, append to the container environment:

```go
env := []string{
    fmt.Sprintf("SHINY_PORT=%d", spec.ShinyPort),
    "R_LIBS=/blockyard-lib",
}
for k, v := range spec.Env {
    env = append(env, k+"="+v)
}
```

## Server state additions

```go
type Server struct {
    // ... existing fields ...

    // OpenBao — nil when [openbao] is not configured.
    VaultClient     *integration.Client
    VaultTokenCache *integration.VaultTokenCache
}
```

Initialization in `main.go` after OIDC setup:

**(Updated in v1 wrap-up §4A):** the vault client is now initialized
via the AppRole startup flow (persisted token → AppRole login → fail).
The static admin token path is preserved for migration. A background
renewal goroutine maintains the token. See
[v1 wrap-up §4](wrap-up.md#4-secret-lifecycle) for the full startup
sequence.

```go
if cfg.Openbao != nil {
    // Wrap-up §4A: init vault client via AppRole or static token
    tokenFunc, stopRenewal, err := integration.InitVaultAuth(ctx, cfg)
    if err != nil {
        slog.Error("vault auth failed", "error", err)
        os.Exit(1)
    }
    defer stopRenewal()

    srv.VaultClient = integration.NewClient(cfg.Openbao.Address, tokenFunc)
    srv.VaultTokenCache = integration.NewVaultTokenCache()

    if err := integration.Bootstrap(ctx, srv.VaultClient, cfg.Openbao.JWTAuthPath); err != nil {
        slog.Warn("OpenBao bootstrap failed — credential injection disabled until resolved",
            "error", err)
    }
}
```

## Proxy changes — credential injection

In `proxy.go`, after identity header injection (step 4), add step 4b:

```go
// 4b. Inject OpenBao credentials when configured.
injectVaultToken(r, srv)
```

`injectVaultToken` is extracted as a named function:

```go
func injectVaultToken(r *http.Request, srv *server.Server) {
    r.Header.Del("X-Blockyard-Vault-Token")

    if srv.VaultClient == nil {
        return
    }
    user := auth.UserFromContext(r.Context())
    if user == nil || user.AccessToken == "" {
        return
    }

    token, ok := srv.VaultTokenCache.Get(user.Sub)
    if !ok {
        var ttl time.Duration
        var err error
        token, ttl, err = srv.VaultClient.JWTLogin(
            r.Context(),
            srv.Config.Openbao.JWTAuthPath,
            user.AccessToken,
        )
        if err != nil {
            slog.Warn("vault JWT login failed", "sub", user.Sub, "error", err)
            return
        }
        if ttl == 0 {
            ttl = srv.Config.Openbao.TokenTTL.Duration
        }
        srv.VaultTokenCache.Set(user.Sub, token, ttl)
    }
    r.Header.Set("X-Blockyard-Vault-Token", token)
}
```

The same headers are forwarded on WebSocket upgrade (already handled by
`ws.go`'s header copying pattern — extend to include
`X-Blockyard-Vault-Token`).

## API: credential enrollment

### Route

```
POST /api/v1/users/me/credentials/{service}
```

Mounted outside the standard `APIAuth` middleware group. Instead, uses
a dual-auth middleware that accepts either:
- Session cookie (app-plane) — extracts `sub` from `AuthenticatedUser`
- PAT bearer (control-plane) — extracts `sub` from `CallerIdentity`

### Dual-auth middleware

A new `UserAuth` middleware that tries session cookie first, then JWT
bearer. Produces a `CallerIdentity` in context either way. Mounted on
the `/api/v1/users/me` sub-router.

```go
r.Route("/api/v1/users/me", func(r chi.Router) {
    r.Use(UserAuth(srv))
    r.Post("/credentials/{service}", EnrollCredential(srv))
})
```

### Handler

```go
func EnrollCredential(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        caller := auth.CallerFromContext(r.Context())
        if caller == nil {
            writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
            return
        }

        if srv.VaultClient == nil {
            serviceUnavailable(w, "credential storage not configured")
            return
        }

        service := chi.URLParam(r, "service")
        // Validate service name (alphanumeric + hyphens + underscores, max 64 chars)

        var body struct {
            APIKey string `json:"api_key"`
        }
        // Decode and validate body

        err := integration.EnrollCredential(
            r.Context(), srv.VaultClient,
            caller.Sub, service,
            map[string]any{"api_key": body.APIKey},
        )
        if err != nil {
            slog.Error("credential enrollment failed", "sub", caller.Sub, "service", service, "error", err)
            serverError(w, "failed to store credential")
            return
        }

        w.WriteHeader(http.StatusNoContent)
    }
}
```

## WebSocket credential forwarding

The `X-Blockyard-Vault-Token` header is injected on the HTTP request
before the WebSocket upgrade check. The existing `ws.go` pattern copies
named headers from the incoming request to the backend dial. Extend it
to also forward `X-Blockyard-Vault-Token`:

```go
if v := r.Header.Get("X-Blockyard-Vault-Token"); v != "" {
    dialHeaders.Set("X-Blockyard-Vault-Token", v)
}
```

R/Shiny reads the token at session init via `session$request`. The
token is available when the WebSocket session starts. Mid-session token
refresh is not needed — the token TTL (default 1h) exceeds typical
Shiny session durations, and if needed, the R code can call OpenBao's
renewal API directly.

## Testing strategy

### Unit tests (no build tag — run in CI `check` job)

- **`internal/integration/` unit tests** — `httptest.Server` that mimics
  OpenBao's API responses:
  - `TestJWTLogin` — success, invalid JWT, OpenBao error
  - `TestKVWrite` / `TestKVRead` — success, not found, permission denied
  - `TestHealth` — healthy, sealed, unreachable
  - `TestBootstrap` — all checks pass, each check failing individually
  - `TestVaultTokenCache` — hit, miss, expiry, near-expiry renewal buffer
  - `TestEnrollCredential` — success, OpenBao error

- **Proxy credential injection tests** — extend existing proxy
  integration tests to verify header injection when a mock OpenBao is
  wired up

- **API credential enrollment tests** — test dual auth, service name
  validation, body validation

### Integration tests (`//go:build openbao_test`)

Run against a real OpenBao dev server container. CI job pulls
`ghcr.io/openbao/openbao:latest`, starts in dev mode.

- `TestMain` — starts OpenBao container, configures JWT auth method
  with the test IdP's JWKS URI, creates `blockyard-user` role and
  policy, mounts KV v2 at `secret/`
- `TestBootstrapReal` — verify bootstrap passes against configured
  instance
- `TestJWTLoginReal` — issue a JWT via MockIdP, exchange for OpenBao
  token
- `TestEnrollAndReadCredential` — write credential via admin token,
  read via user-scoped token
- `TestTokenScopingReal` — verify user-scoped tokens can only read
  their own secrets

## CI additions

New job in `.github/workflows/ci.yml`:

```yaml
openbao:
  runs-on: ubuntu-latest
  needs: [check]
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: '1.24'
    - run: docker pull ghcr.io/openbao/openbao:latest
    - run: go test -tags openbao_test -coverprofile=coverage-openbao.out -coverpkg=./... ./internal/integration/...
    - uses: actions/upload-artifact@v4
      with:
        name: coverage-openbao
        path: coverage-openbao.out
```

Update coverage job to include `coverage-openbao.out`.

## File inventory

New files:
- `internal/integration/openbao.go` — Client, JWTLogin, KVWrite, KVRead, Health
- `internal/integration/bootstrap.go` — Bootstrap verification
- `internal/integration/tokencache.go` — VaultTokenCache
- `internal/integration/enrollment.go` — EnrollCredential
- `internal/integration/openbao_test.go` — unit tests (httptest mock)
- `internal/integration/tokencache_test.go` — cache unit tests
- `internal/integration/openbao_integration_test.go` — real OpenBao tests (`//go:build openbao_test`)

Modified files:
- `internal/config/config.go` — add OpenbaoConfig, validation, defaults, env auto-construction
- `internal/backend/backend.go` — add Env field to WorkerSpec
- `internal/backend/docker/docker.go` — append WorkerSpec.Env to container env
- `internal/server/state.go` — add VaultClient + VaultTokenCache fields
- `internal/proxy/proxy.go` — credential injection (step 4b)
- `internal/proxy/ws.go` — forward X-Blockyard-Vault-Token on dial
- `internal/api/router.go` — add /users/me route group with dual auth
- `internal/api/auth.go` — add UserAuth dual-auth middleware
- `internal/api/users.go` — EnrollCredential handler (new file)
- `cmd/blockyard/main.go` — OpenBao client init + bootstrap
- `.github/workflows/ci.yml` — add openbao job
- `internal/backend/mock/mock.go` — handle Env field (if needed)
