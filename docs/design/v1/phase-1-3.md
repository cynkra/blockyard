# Phase 1-3: Identity Injection + OpenBao Integration

Deliver authenticated user identity and per-user credentials to Shiny apps
at runtime. Phase 1-1 established who the user is (OIDC session), phase 1-2
established what they can do (RBAC + ACL). This phase makes identity and
credentials available *inside* the R process — the last piece needed for
apps to personalize content and access external services on behalf of the
logged-in user.

Two capabilities land together because they share the same dependency (the
`AuthenticatedUser` in request context from phase 1-1) and serve the same
goal (making identity actionable inside the app):

1. **Identity header injection** — `X-Shiny-User` and `X-Shiny-Groups`
   headers on every proxied request. R apps read these via
   `session$request$HTTP_X_SHINY_USER`.
2. **OpenBao credential injection** — exchange the user's IdP access token
   for a scoped OpenBao token, injected as `X-Blockyard-Vault-Token` and
   `X-Blockyard-Vault-Addr` headers. R apps use these to read per-user
   secrets from OpenBao directly.

This phase depends on phase 1-1 (OIDC sessions, `AuthenticatedUser` in
context, access tokens) and phase 1-2 (`CallerIdentity` in context for the
credential enrollment API's auth guard).

## Design decision: per-request headers, not per-container env vars

Credentials are injected per-request as HTTP headers, not per-container as
environment variables. This is the same model Posit Connect uses for OAuth
Integrations — per-user credentials are part of the Shiny session context,
not the process environment.

**Why not env vars?**

- **Session sharing:** phase 1-4 unlocks `max_sessions_per_worker > 1`.
  With env vars, all sessions on a shared worker would see the same
  credential — the one belonging to whoever triggered the container spawn.
  Per-request headers give each user their own scoped token.
- **Token rotation:** OpenBao tokens expire. Headers carry the current
  token on every request. Env vars would require restarting the container
  to rotate.
- **Isolation:** env vars are readable by any code in the process. Headers
  are scoped to the request that carries them.

**Trade-off:** the R app reads credentials via `session$request` (captured
at Shiny session init on the WebSocket upgrade), not via `Sys.getenv()`.
This is slightly less ergonomic for app developers, but it's the standard
Shiny pattern for per-session data and matches how Posit Connect exposes
OAuth tokens.

**Consequence for `WorkerSpec`:** no changes. `WorkerSpec` does not carry
`vault_token` or `vault_addr` fields. Credentials never enter the container
environment.

## Design decision: OpenBao JWT auth (not AppRole)

The server exchanges user access tokens for scoped OpenBao tokens via
OpenBao's JWT auth method. The flow:

1. Server configures OpenBao's JWT auth with the IdP's JWKS endpoint (once,
   at bootstrap).
2. On each proxied request, the server `POST`s the user's access token to
   OpenBao's `/auth/jwt/login` endpoint.
3. OpenBao validates the JWT against the IdP's JWKS, checks the
   `blockyard-user` role, and returns a short-lived token scoped to
   `secret/users/{sub}/*`.

**Why JWT auth instead of AppRole?**

- **No server-side secret storage:** the server doesn't need to store or
  rotate role IDs and secret IDs. The IdP access token is already
  available in the session.
- **Per-user scoping is automatic:** OpenBao's JWT auth can template the
  policy using claims from the JWT (`{{identity.entity.aliases.auth_jwt_*.metadata.sub}}`).
  Each token is scoped to the user's own secrets.
- **Standard IdP integration:** no custom auth backend. Any IdP that
  speaks OIDC works.

**Trade-off:** requires the IdP's access tokens to be valid JWTs with a
`sub` claim (standard for OIDC). Opaque access tokens won't work — the
IdP must be configured to issue JWT access tokens.

## Design decision: server-side token cache

The server caches OpenBao tokens in memory, keyed by user `sub`. Without
caching, every proxied request would call OpenBao's login endpoint — this
is too expensive for interactive Shiny apps that make many HTTP requests
per user action.

**Cache behavior:**

- **Key:** user `sub` (same user gets same cached token regardless of
  which worker serves them).
- **TTL:** matches `[openbao] token_ttl` (default 1h). Conservative:
  evict from cache at 90% of TTL to avoid using near-expired tokens.
- **Eviction:** expired entries are lazily evicted on access. No background
  cleanup goroutine — the cache size is bounded by the number of active
  users.
- **Invalidation on logout:** `POST /logout` removes the user's cached
  token (phase 1-1's `UserSessionStore.Delete` is extended to also clear
  the vault token cache entry).

**Trade-off:** a user who changes their OpenBao policy (via admin action)
won't see the change until the cached token expires. Acceptable for v1 —
policies change rarely, and the TTL is bounded.

## Design decision: bootstrap verification is best-effort

When `[openbao]` is configured, the server checks OpenBao's health and
configuration at startup. If any check fails, the server **logs a warning
but starts anyway**.

**Why not fail hard?**

- **Startup ordering:** in Docker Compose, the server and OpenBao may
  start in any order. If the server fails hard when OpenBao is
  unreachable, you need `depends_on` with health checks — adding
  operational complexity.
- **Graceful degradation:** identity headers (`X-Shiny-User`,
  `X-Shiny-Groups`) work without OpenBao. Apps that only need user
  identity (not credentials) should not be blocked by an OpenBao outage.
- **Observability:** the `/readyz` endpoint (phase 1-6) reports OpenBao
  health. Operators can use it for liveness probes.

**What the proxy does when OpenBao is unreachable at request time:** the
credential injection middleware skips the `X-Blockyard-Vault-Token` and
`X-Blockyard-Vault-Addr` headers. The request is proxied with identity
headers only. R apps that depend on credentials will fail gracefully (the
header is absent, not stale).

## Design decision: credential enrollment via admin token

The credential enrollment API (`POST /api/v1/users/me/credentials/{service}`)
writes secrets to OpenBao using the server's admin token, not the user's
scoped token. The user's scoped token only has `read` permission on their
secrets path.

**Why?**

- **Least privilege at runtime:** the per-request scoped token can read
  but not write. If a Shiny app is compromised, it can read the user's
  existing credentials but cannot write new ones or modify existing ones.
- **Simpler policy:** one policy (`read` on `secret/users/{sub}/*`) for
  all runtime tokens. Write access is only needed during enrollment, which
  goes through the server's authenticated API.

**Trade-off:** the server's admin token has broad write access. It's
protected by: (a) never entering the container environment, (b) being
loaded from an env var, not the config file, (c) being wrapped in the
`Secret` type that redacts in logs.

## Design decision: services network for worker-to-OpenBao connectivity

In v0, each worker gets its own isolated bridge network
(`blockyard-{worker-id}`). The server joins each worker's network to reach
it. Workers are fully isolated — they cannot reach each other, and they
cannot reach anything outside their per-worker network.

This creates a problem for OpenBao: the `[openbao].address` is reachable
from the server (which runs on the host or on the Docker Compose default
network), but not from inside a worker container. The R app needs to call
OpenBao directly using the address from `X-Blockyard-Vault-Addr`, and
that call will fail if the worker has no network path to OpenBao.

**Solution:** an optional `docker.services_network` config field. When set,
every worker container is connected to this network in addition to its
per-worker bridge network. In a Docker Compose deployment, this is
typically the Compose-managed default network that the server, OpenBao,
and other infrastructure services share.

```toml
[docker]
services_network = "blockyard_default"
```

**What this preserves:**

- **Worker-to-worker isolation:** workers are still on separate per-worker
  bridge networks. Being on a shared services network does not let them
  reach each other by worker IP — they'd need to know the other worker's
  IP on the services network, which is not exposed. (This is the same
  isolation model as Docker Compose services on a shared network.)
- **Metadata endpoint protection:** iptables rules are scoped to the
  per-worker network's subnet. The services network does not bypass them.

**What this enables:**

- Workers can reach OpenBao at the same address the server uses.
- Workers can reach any other service on the shared network (e.g., a
  database the R app needs to connect to). This is a useful side effect
  — currently workers are fully isolated from all external services,
  which limits what Shiny apps can do.

**When not set:** workers remain fully isolated (v0 behavior). OpenBao
credential injection still works — the server exchanges tokens on behalf
of the user — but R apps cannot call OpenBao directly. The
`X-Blockyard-Vault-Addr` header is still injected; the R app will get a
connection error if it tries to use it. This is documented as an operator
configuration requirement.

**Implementation:** in `DockerBackend.Spawn`, after creating the container
on its per-worker network and before starting it, connect it to the
services network:

```go
// Connect to services network (if configured) for external service access
if d.config.ServicesNetwork != "" {
    if err := d.joinNetwork(ctx, containerID, d.config.ServicesNetwork); err != nil {
        // cleanup: remove container, iptables rule, per-worker network
        return fmt.Errorf("spawn: join services network: %w", err)
    }
}
```

**Build containers:** the `Build` flow currently runs on the default
bridge network (no explicit network assignment). Build containers need
internet access to download rv from GitHub. If `services_network` is set,
build containers are also connected to it for consistency — though builds
don't currently need OpenBao access, having a uniform network policy
simplifies reasoning about what containers can reach.

**Validation:** if `[openbao]` is configured and `docker.services_network`
is not set, the server logs a warning at startup:

```
WARN openbao configured but docker.services_network is not set —
     worker containers may not be able to reach OpenBao directly
```

This is a warning, not an error — the server-side credential injection
still works without direct worker-to-OpenBao connectivity.

## Design decision: WebSocket credential capture at session init

Shiny apps communicate over WebSocket after the initial HTTP upgrade.
Headers are only present on the upgrade request — there's no mechanism to
inject new headers on subsequent WebSocket frames.

R apps read per-session data via `session$request`, which captures the
headers from the upgrade request at session init time. This means:

- The OpenBao token available to the R app is the one issued at session
  start.
- If the token expires mid-session (default TTL 1h), the R app can call
  OpenBao's token renewal API directly using the token from
  `session$request`.
- For most Shiny apps, sessions are shorter than the token TTL, so this
  is not a practical concern.

**Alternative considered:** injecting credentials via a custom WebSocket
message. Rejected — it would require a companion R package to receive and
store the token, breaking the "no companion package needed" principle from
the roadmap.

## Deliverables

1. Identity header injection — `X-Shiny-User` and `X-Shiny-Groups` on
   every proxied request (HTTP and WebSocket upgrade)
2. Header spoofing prevention — strip client-supplied identity and
   credential headers before forwarding
3. `internal/integration/openbao.go` — OpenBao HTTP client for KV v2
   read/write and JWT auth login
4. OpenBao bootstrap verification — health + configuration checks at
   startup
5. Per-request credential injection — exchange user's access token for
   scoped OpenBao token, inject as headers
6. `VaultTokenCache` — in-memory token cache keyed by user `sub`
7. Cache invalidation on logout
8. Credential enrollment API — `POST /api/v1/users/me/credentials/{service}`
9. Config additions: `[openbao]` section, `docker.services_network` field
10. Services network — connect workers to a shared network for OpenBao
    (and general external service) reachability
11. Server struct additions: `OpenBaoClient`, `VaultTokenCache`

## Step-by-step

### Step 1: Config additions

**New struct** in `internal/config/config.go`:

```go
type OpenbaoConfig struct {
    Address     string   `toml:"address"`        // e.g. https://bao.example.com
    AdminToken  Secret   `toml:"admin_token"`    // use BLOCKYARD_OPENBAO_ADMIN_TOKEN env var
    TokenTTL    Duration `toml:"token_ttl"`      // default: 1h
    JWTAuthPath string   `toml:"jwt_auth_path"`  // default: "jwt"
}
```

**Changes to `DockerConfig` struct:**

```go
type DockerConfig struct {
    Socket          string `toml:"socket"`           // default: "/var/run/docker.sock"
    Image           string `toml:"image"`            // required
    ShinyPort       int    `toml:"shiny_port"`       // default: 3838
    RvVersion       string `toml:"rv_version"`       // default: "latest"
    ServicesNetwork string `toml:"services_network"` // new — optional, e.g. "blockyard_default"
}
```

**Changes to `Config` struct:**

```go
type Config struct {
    Server   ServerConfig   `toml:"server"`
    Docker   DockerConfig   `toml:"docker"`
    Storage  StorageConfig  `toml:"storage"`
    Database DatabaseConfig `toml:"database"`
    Proxy    ProxyConfig    `toml:"proxy"`
    OIDC     *OidcConfig    `toml:"oidc"`
    Openbao  *OpenbaoConfig `toml:"openbao"`   // new — nil when not configured
}
```

**Defaults:**

```go
func openbaoDefaults(c *OpenbaoConfig) {
    if c.TokenTTL.Duration == 0 {
        c.TokenTTL.Duration = time.Hour
    }
    if c.JWTAuthPath == "" {
        c.JWTAuthPath = "jwt"
    }
}
```

**Validation:**

```go
if cfg.Openbao != nil {
    if cfg.Openbao.Address == "" {
        return fmt.Errorf("config: openbao.address must not be empty")
    }
    if cfg.Openbao.AdminToken.IsEmpty() {
        return fmt.Errorf("config: openbao.admin_token must not be empty")
    }
    if cfg.OIDC == nil {
        return fmt.Errorf("config: [oidc] is required when [openbao] is configured")
    }
}
```

The last check enforces that OpenBao integration requires OIDC — the JWT
auth flow needs IdP access tokens.

**Startup warning** (in `cmd/blockyard/main.go`, after config validation):

```go
if cfg.Openbao != nil && cfg.Docker.ServicesNetwork == "" {
    slog.Warn("openbao configured but docker.services_network is not set — " +
        "worker containers may not be able to reach OpenBao directly")
}
```

**Auto-construction from env vars:**

```go
if cfg.Openbao == nil && envPrefixExists("BLOCKYARD_OPENBAO_") {
    cfg.Openbao = &OpenbaoConfig{}
    openbaoDefaults(cfg.Openbao)
}
```

**Env var mappings:**

```
BLOCKYARD_DOCKER_SERVICES_NETWORK
BLOCKYARD_OPENBAO_ADDRESS
BLOCKYARD_OPENBAO_ADMIN_TOKEN
BLOCKYARD_OPENBAO_TOKEN_TTL
BLOCKYARD_OPENBAO_JWT_AUTH_PATH
```

**Tests:**

- Parse config with `[openbao]` section present
- Parse config without `[openbao]` section (backward compat)
- Validation: reject empty address, empty admin_token
- Validation: reject `[openbao]` without `[oidc]`
- Default: `token_ttl` defaults to 1h, `jwt_auth_path` defaults to "jwt"
- Env var override for each field
- Auto-construction from env vars
- `TestEnvVarCoverageComplete` passes with new fields

### Step 2: Identity header injection

New file: `internal/proxy/identity.go`

```go
package proxy

import (
    "net/http"
    "strings"

    "github.com/cynkra/blockyard/internal/auth"
)

// identityHeaders are the headers injected by the proxy. Client-supplied
// values are stripped before forwarding to prevent spoofing.
var identityHeaders = []string{
    "X-Shiny-User",
    "X-Shiny-Groups",
    "X-Blockyard-Vault-Token",
    "X-Blockyard-Vault-Addr",
}

// stripSpoofedHeaders removes any client-supplied identity/credential
// headers from the incoming request. Called before forwarding.
func stripSpoofedHeaders(r *http.Request) {
    for _, h := range identityHeaders {
        r.Header.Del(h)
    }
}

// injectIdentityHeaders adds X-Shiny-User and X-Shiny-Groups to the
// outgoing request based on the authenticated user in context. If no
// user is authenticated (public app, anonymous access), the headers
// are absent.
func injectIdentityHeaders(r *http.Request, user *auth.AuthenticatedUser) {
    if user == nil {
        return
    }
    r.Header.Set("X-Shiny-User", user.Sub)
    if len(user.Groups) > 0 {
        r.Header.Set("X-Shiny-Groups", strings.Join(user.Groups, ","))
    }
}
```

**Integration into the proxy Director** — in `forward.go`, the Director
function is extended to strip and inject headers:

```go
proxy.Director = func(req *http.Request) {
    originalDirector(req)
    req.URL.Path = stripAppPrefix(req.URL.Path, appName)
    req.URL.RawPath = ""
    req.Host = addr
    req.Header.Set("X-Forwarded-Proto", "http")

    // v1: identity + credential headers
    stripSpoofedHeaders(req)
    user := auth.AuthenticatedUserFromContext(req.Context())
    injectIdentityHeaders(req, user)
}
```

The same stripping and injection happens in the WebSocket upgrade path
(`ws.go`), on the backend dial request.

**Tests:**

- `stripSpoofedHeaders` removes all identity/credential headers
- `injectIdentityHeaders` with authenticated user sets both headers
- `injectIdentityHeaders` with nil user leaves headers absent
- Groups with commas in names: verify join behavior (commas in group names
  are an IdP configuration issue, not a blockyard concern — document it)
- Integration test: proxy request to mock worker, verify headers arrive
- Integration test: client-supplied `X-Shiny-User` is stripped and
  replaced with the authenticated user's identity

### Step 3: OpenBao client

New file: `internal/integration/openbao.go`

```go
package integration

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "time"

    "github.com/cynkra/blockyard/internal/config"
)

// OpenBaoClient is an HTTP client for OpenBao's API. It handles JWT auth
// login (for per-user token issuance) and KV v2 operations (for credential
// enrollment).
type OpenBaoClient struct {
    address     string
    adminToken  string
    jwtAuthPath string
    httpClient  *http.Client
}

// NewOpenBaoClient creates a new client from the [openbao] config.
func NewOpenBaoClient(cfg *config.OpenbaoConfig) *OpenBaoClient {
    return &OpenBaoClient{
        address:     cfg.Address,
        adminToken:  cfg.AdminToken.Expose(),
        jwtAuthPath: cfg.JWTAuthPath,
        httpClient:  &http.Client{Timeout: 10 * time.Second},
    }
}

// JWTLoginResult holds the response from a JWT auth login.
type JWTLoginResult struct {
    Token     string
    TTL       time.Duration
    Renewable bool
}

// JWTLogin exchanges a JWT (the user's IdP access token) for a scoped
// OpenBao token. The "blockyard-user" role's policy restricts the token
// to read access on secret/users/{sub}/*.
func (c *OpenBaoClient) JWTLogin(ctx context.Context, jwt string) (*JWTLoginResult, error) {
    url := fmt.Sprintf("%s/v1/auth/%s/login", c.address, c.jwtAuthPath)

    body, err := json.Marshal(map[string]string{
        "role": "blockyard-user",
        "jwt":  jwt,
    })
    if err != nil {
        return nil, fmt.Errorf("marshal login body: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
    if err != nil {
        return nil, fmt.Errorf("create login request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return nil, fmt.Errorf("jwt login request: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        return nil, fmt.Errorf("jwt login failed (status %d): %s", resp.StatusCode, respBody)
    }

    var result struct {
        Auth struct {
            ClientToken string `json:"client_token"`
            LeaseDuration int  `json:"lease_duration"`
            Renewable   bool   `json:"renewable"`
        } `json:"auth"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return nil, fmt.Errorf("decode login response: %w", err)
    }

    return &JWTLoginResult{
        Token:     result.Auth.ClientToken,
        TTL:       time.Duration(result.Auth.LeaseDuration) * time.Second,
        Renewable: result.Auth.Renewable,
    }, nil
}

// WriteSecret writes a secret to OpenBao's KV v2 engine using the admin
// token. Used by the credential enrollment API.
func (c *OpenBaoClient) WriteSecret(ctx context.Context, path string, data map[string]any) error {
    url := fmt.Sprintf("%s/v1/secret/data/%s", c.address, path)

    body, err := json.Marshal(map[string]any{"data": data})
    if err != nil {
        return fmt.Errorf("marshal secret data: %w", err)
    }

    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
    if err != nil {
        return fmt.Errorf("create write request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-Vault-Token", c.adminToken)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("write secret request: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
        respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
        return fmt.Errorf("write secret failed (status %d): %s", resp.StatusCode, respBody)
    }

    return nil
}

// Health checks if OpenBao is reachable and unsealed. Returns nil on
// success.
func (c *OpenBaoClient) Health(ctx context.Context) error {
    url := fmt.Sprintf("%s/v1/sys/health", c.address)

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return fmt.Errorf("create health request: %w", err)
    }

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("health request: %w", err)
    }
    defer resp.Body.Close()

    // 200 = initialized + unsealed + active
    // 429 = unsealed + standby
    // 472 = data recovery mode
    // 473 = performance standby
    // 501 = not initialized
    // 503 = sealed
    if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
        return fmt.Errorf("openbao unhealthy (status %d)", resp.StatusCode)
    }

    return nil
}
```

**Tests:**

- `JWTLogin` with mock OpenBao server returning 200 + token
- `JWTLogin` with mock OpenBao server returning 403 (bad JWT)
- `WriteSecret` with mock server returning 200
- `WriteSecret` with mock server returning 403 (bad admin token)
- `Health` with mock server returning 200 (healthy)
- `Health` with mock server returning 503 (sealed)

### Step 4: Bootstrap verification

New file: `internal/integration/bootstrap.go`

```go
package integration

import (
    "context"
    "fmt"
    "log/slog"

    "github.com/cynkra/blockyard/internal/config"
)

// BootstrapResult contains the outcome of OpenBao bootstrap verification.
type BootstrapResult struct {
    Reachable    bool
    Unsealed     bool
    JWTAuthReady bool
    KVReady      bool
}

// AllOK reports whether all bootstrap checks passed.
func (r *BootstrapResult) AllOK() bool {
    return r.Reachable && r.Unsealed && r.JWTAuthReady && r.KVReady
}

// Bootstrap verifies OpenBao's health and configuration. Returns the
// result; callers decide whether to fail or warn.
func Bootstrap(ctx context.Context, client *OpenBaoClient, cfg *config.OpenbaoConfig) *BootstrapResult {
    result := &BootstrapResult{}

    // 1. Health check — reachable and unsealed?
    if err := client.Health(ctx); err != nil {
        slog.Warn("openbao health check failed", "error", err)
        return result
    }
    result.Reachable = true
    result.Unsealed = true

    // 2. JWT auth method enabled?
    if err := client.checkJWTAuth(ctx, cfg.JWTAuthPath); err != nil {
        slog.Warn("openbao jwt auth check failed", "error", err)
        return result
    }
    result.JWTAuthReady = true

    // 3. KV v2 secrets engine mounted?
    if err := client.checkKVMount(ctx); err != nil {
        slog.Warn("openbao kv mount check failed", "error", err)
        return result
    }
    result.KVReady = true

    return result
}
```

**Helper methods** on `OpenBaoClient` (in `openbao.go`):

```go
// checkJWTAuth verifies the JWT auth method is enabled at the configured
// path by reading its configuration.
func (c *OpenBaoClient) checkJWTAuth(ctx context.Context, path string) error {
    url := fmt.Sprintf("%s/v1/auth/%s/config", c.address, path)

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return err
    }
    req.Header.Set("X-Vault-Token", c.adminToken)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("jwt auth at %q returned %d", path, resp.StatusCode)
    }
    return nil
}

// checkKVMount verifies the KV v2 secrets engine is mounted at secret/.
func (c *OpenBaoClient) checkKVMount(ctx context.Context) error {
    url := fmt.Sprintf("%s/v1/sys/mounts/secret", c.address)

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return err
    }
    req.Header.Set("X-Vault-Token", c.adminToken)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("kv mount at secret/ returned %d", resp.StatusCode)
    }
    return nil
}
```

**Wiring in `cmd/blockyard/main.go`:**

```go
if cfg.Openbao != nil {
    baoClient := integration.NewOpenBaoClient(cfg.Openbao)
    result := integration.Bootstrap(ctx, baoClient, cfg.Openbao)

    if result.AllOK() {
        slog.Info("openbao bootstrap: all checks passed")
    } else {
        slog.Warn("openbao bootstrap: some checks failed — credential injection will be unavailable",
            "reachable", result.Reachable,
            "unsealed", result.Unsealed,
            "jwt_auth", result.JWTAuthReady,
            "kv_mount", result.KVReady,
        )
    }

    srv.OpenBaoClient = baoClient
    srv.VaultTokenCache = integration.NewVaultTokenCache(cfg.Openbao.TokenTTL.Duration)
}
```

**Tests:**

- All checks pass — `AllOK()` returns true
- Health fails — `Reachable` false, other fields false
- JWT auth check fails — `JWTAuthReady` false
- KV mount check fails — `KVReady` false

### Step 5: Vault token cache

New file: `internal/integration/cache.go`

```go
package integration

import (
    "sync"
    "time"
)

// VaultTokenCache caches OpenBao tokens keyed by user sub. Avoids
// calling OpenBao's JWT login endpoint on every proxied request.
type VaultTokenCache struct {
    mu         sync.RWMutex
    tokens     map[string]*cachedToken
    defaultTTL time.Duration
}

type cachedToken struct {
    Token     string
    ExpiresAt time.Time
}

// NewVaultTokenCache creates an empty cache with the given default TTL.
func NewVaultTokenCache(defaultTTL time.Duration) *VaultTokenCache {
    return &VaultTokenCache{
        tokens:     make(map[string]*cachedToken),
        defaultTTL: defaultTTL,
    }
}

// Get retrieves a cached token for the given user sub. Returns empty
// string if not cached or expired. Expired entries are lazily deleted.
func (c *VaultTokenCache) Get(sub string) string {
    c.mu.RLock()
    entry, ok := c.tokens[sub]
    c.mu.RUnlock()

    if !ok {
        return ""
    }

    // Use 90% of TTL as effective expiry to avoid near-expired tokens
    if time.Now().After(entry.ExpiresAt) {
        c.mu.Lock()
        delete(c.tokens, sub)
        c.mu.Unlock()
        return ""
    }

    return entry.Token
}

// Set caches a token for the given user sub. The TTL is taken from the
// login result if available, otherwise falls back to the configured
// default.
func (c *VaultTokenCache) Set(sub, token string, ttl time.Duration) {
    if ttl == 0 {
        ttl = c.defaultTTL
    }

    // Cache at 90% of actual TTL to avoid using near-expired tokens
    effectiveTTL := time.Duration(float64(ttl) * 0.9)

    c.mu.Lock()
    c.tokens[sub] = &cachedToken{
        Token:     token,
        ExpiresAt: time.Now().Add(effectiveTTL),
    }
    c.mu.Unlock()
}

// Delete removes a cached token for the given user sub.
// Called on logout.
func (c *VaultTokenCache) Delete(sub string) {
    c.mu.Lock()
    delete(c.tokens, sub)
    c.mu.Unlock()
}
```

**Tests:**

- `Get` on empty cache returns empty string
- `Set` then `Get` returns the token
- `Get` after expiry returns empty string and removes entry
- `Delete` removes entry
- Concurrent access: parallel `Set`/`Get`/`Delete` (race detector)

### Step 6: Per-request credential injection

New file: `internal/proxy/credentials.go`

```go
package proxy

import (
    "context"
    "log/slog"
    "net/http"

    "github.com/cynkra/blockyard/internal/auth"
    "github.com/cynkra/blockyard/internal/integration"
    "github.com/cynkra/blockyard/internal/server"
)

// injectCredentialHeaders exchanges the user's access token for a scoped
// OpenBao token and injects it as request headers. Skips silently if
// OpenBao is not configured or the user is not authenticated.
func injectCredentialHeaders(ctx context.Context, r *http.Request, srv *server.Server) {
    if srv.OpenBaoClient == nil || srv.VaultTokenCache == nil {
        return
    }

    user := auth.AuthenticatedUserFromContext(ctx)
    if user == nil {
        return
    }

    // Check cache first
    token := srv.VaultTokenCache.Get(user.Sub)
    if token == "" {
        // Cache miss — exchange access token for OpenBao token
        result, err := srv.OpenBaoClient.JWTLogin(ctx, user.AccessToken)
        if err != nil {
            slog.Warn("openbao jwt login failed — skipping credential injection",
                "sub", user.Sub, "error", err)
            return
        }
        token = result.Token
        srv.VaultTokenCache.Set(user.Sub, result.Token, result.TTL)
    }

    r.Header.Set("X-Blockyard-Vault-Token", token)
    r.Header.Set("X-Blockyard-Vault-Addr", srv.OpenBaoClient.Address())
}
```

**`Address()` accessor** on `OpenBaoClient`:

```go
func (c *OpenBaoClient) Address() string { return c.address }
```

**Integration into the proxy Director** — extending the changes from
Step 2:

```go
proxy.Director = func(req *http.Request) {
    originalDirector(req)
    req.URL.Path = stripAppPrefix(req.URL.Path, appName)
    req.URL.RawPath = ""
    req.Host = addr
    req.Header.Set("X-Forwarded-Proto", "http")

    // v1: identity + credential headers
    stripSpoofedHeaders(req)
    user := auth.AuthenticatedUserFromContext(req.Context())
    injectIdentityHeaders(req, user)
    injectCredentialHeaders(req.Context(), req, srv)
}
```

**WebSocket upgrade path** — the same injection happens on the backend
dial request in `ws.go`. The backend request is created from the original
request's context, so `AuthenticatedUser` is available.

**Server struct additions** (in `internal/server/state.go`):

```go
type Server struct {
    // ... existing fields ...

    // OpenBao fields — nil when [openbao] is not configured.
    OpenBaoClient   *integration.OpenBaoClient
    VaultTokenCache *integration.VaultTokenCache
}
```

**Tests:**

- OpenBao not configured — no headers injected, no error
- User not authenticated — no headers injected
- Cache hit — uses cached token, no OpenBao call
- Cache miss — calls `JWTLogin`, caches result, injects headers
- OpenBao login failure — logs warning, no headers, request still proxied
- Integration test: full flow through proxy with mock OpenBao

### Step 7: Logout cache invalidation

Phase 1-1's logout handler deletes the server-side session. Extend it to
also clear the vault token cache entry.

**Change in `internal/api/auth_handlers.go`** (the logout handler):

```go
func logoutHandler(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // ... existing: extract user from cookie, delete session ...

        // Clear vault token cache on logout
        if srv.VaultTokenCache != nil && user != nil {
            srv.VaultTokenCache.Delete(user.Sub)
        }

        // ... existing: clear cookie, redirect ...
    }
}
```

**Tests:**

- Logout clears vault token cache entry
- Logout with no vault cache configured — no panic

### Step 8: Credential enrollment API

New file: `internal/api/credentials.go`

```go
package api

import (
    "encoding/json"
    "fmt"
    "net/http"
    "regexp"

    "github.com/go-chi/chi/v5"

    "github.com/cynkra/blockyard/internal/auth"
    "github.com/cynkra/blockyard/internal/server"
)

// serviceNamePattern validates credential service names. Same rules as
// app names: lowercase ASCII letters, digits, hyphens. 1-63 chars.
var serviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// enrollCredential handles POST /api/v1/users/me/credentials/{service}.
// Writes a secret to OpenBao on behalf of the authenticated user.
func enrollCredential(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // 1. Require OpenBao
        if srv.OpenBaoClient == nil {
            writeError(w, http.StatusServiceUnavailable, "openbao_not_configured",
                "Credential management requires OpenBao to be configured")
            return
        }

        // 2. Extract caller identity
        caller := auth.CallerFromContext(r.Context())
        if caller == nil {
            writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
            return
        }

        // 3. Validate service name
        service := chi.URLParam(r, "service")
        if !serviceNamePattern.MatchString(service) {
            writeError(w, http.StatusBadRequest, "invalid_service_name",
                "Service name must be 1-63 lowercase ASCII letters, digits, or hyphens")
            return
        }

        // 4. Parse request body
        var body map[string]any
        if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
            writeError(w, http.StatusBadRequest, "invalid_body", "Request body must be valid JSON")
            return
        }
        if len(body) == 0 {
            writeError(w, http.StatusBadRequest, "empty_body", "Request body must not be empty")
            return
        }

        // 5. Write to OpenBao
        path := fmt.Sprintf("users/%s/apikeys/%s", caller.Sub, service)
        if err := srv.OpenBaoClient.WriteSecret(r.Context(), path, body); err != nil {
            writeError(w, http.StatusBadGateway, "openbao_write_failed",
                "Failed to write credential to OpenBao")
            return
        }

        w.WriteHeader(http.StatusNoContent)
    }
}
```

**Router addition:**

```go
r.Route("/api/v1", func(r chi.Router) {
    r.Use(authMiddleware(srv))

    // ... existing app/bundle/task routes ...

    // Credential enrollment (requires authenticated user)
    r.Post("/users/me/credentials/{service}", enrollCredential(srv))
})
```

**Tests:**

- Enroll credential with valid body — 204, secret written
- Enroll without authentication — 401
- Enroll with invalid service name — 400
- Enroll with empty body — 400
- Enroll when OpenBao not configured — 503
- Enroll when OpenBao write fails — 502
- Integration test: enroll credential, then verify the user's scoped
  token can read it

### Step 9: Services network in Docker backend

Extend `DockerBackend.Spawn` to connect worker containers to the
configured services network. This gives workers a network path to OpenBao
and other external services.

**Change in `internal/backend/docker/docker.go`**, in `Spawn`, between
container creation (step 4) and server network join (step 5):

```go
// 4b. Connect to services network (if configured)
if d.config.ServicesNetwork != "" {
    if err := d.joinNetwork(ctx, containerID, d.config.ServicesNetwork); err != nil {
        _ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
        d.unblockMetadataForWorker(spec.WorkerID)
        _ = d.client.NetworkRemove(ctx, networkID)
        return fmt.Errorf("spawn: join services network %q: %w",
            d.config.ServicesNetwork, err)
    }
}
```

**Change in `Build`:** connect build containers to the services network
too. Build containers currently use the default bridge. When
`services_network` is set, the build container is connected after
creation:

```go
// Connect build container to services network (if configured)
if d.config.ServicesNetwork != "" {
    if err := d.joinNetwork(ctx, containerID, d.config.ServicesNetwork); err != nil {
        _ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
        return backend.BuildResult{}, fmt.Errorf("build: join services network: %w", err)
    }
}
```

**Stop cleanup:** no change needed. The services network is shared — we
don't remove it. We only disconnect the worker from its per-worker network
(which we do remove). Docker automatically disconnects a container from
all networks when the container is removed.

**Startup validation:** verify the services network exists at
`DockerBackend` initialization time, not at each spawn:

```go
func New(ctx context.Context, cfg *config.DockerConfig) (*DockerBackend, error) {
    // ... existing: create client, ping, detect server ID ...

    if cfg.ServicesNetwork != "" {
        _, err := cli.NetworkInspect(ctx, cfg.ServicesNetwork, network.InspectOptions{})
        if err != nil {
            return nil, fmt.Errorf("services_network %q not found: %w",
                cfg.ServicesNetwork, err)
        }
        slog.Info("services network verified", "network", cfg.ServicesNetwork)
    }

    return &DockerBackend{ /* ... */ }, nil
}
```

This fails fast at startup if the operator configured a network that
doesn't exist, rather than failing on the first spawn.

**Tests:**

- Spawn with `services_network` set — container is connected to both
  per-worker and services networks
- Spawn with `services_network` empty — container is only on per-worker
  network (v0 behavior)
- Build with `services_network` set — build container is connected
- Startup with nonexistent `services_network` — returns error
- Docker integration test: spawn worker with services network, verify
  worker can reach a service on that network

### Step 10: Update proxy handler to pass `srv`

The proxy handler currently does not have access to the `Server` struct
at the point where `forwardHTTP` is called. The Director closure needs
`srv` for credential injection.

**Change in `proxy.go`:** pass `srv` to `forwardHTTP` and `shuttleWS`:

```go
func forwardHTTP(w http.ResponseWriter, r *http.Request, addr, appName string, transport http.RoundTripper, srv *server.Server) {
    // ... existing setup ...

    proxy.Director = func(req *http.Request) {
        originalDirector(req)
        req.URL.Path = stripAppPrefix(req.URL.Path, appName)
        req.URL.RawPath = ""
        req.Host = addr
        req.Header.Set("X-Forwarded-Proto", "http")

        // v1: identity + credential headers
        stripSpoofedHeaders(req)
        user := auth.AuthenticatedUserFromContext(req.Context())
        injectIdentityHeaders(req, user)
        injectCredentialHeaders(req.Context(), req, srv)
    }

    proxy.ServeHTTP(w, r)
}
```

This is a minimal change — `srv` was already available in the `Handler`
closure and just needs to be threaded through to `forwardHTTP` and
`shuttleWS`.

**Tests:**

- Existing proxy tests still pass (no behavioral change when OpenBao not
  configured)
- New test: proxy with OpenBao configured injects credential headers
