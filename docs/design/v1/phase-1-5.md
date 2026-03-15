# Phase 1-5: Credential Exchange API + Content Discovery

This phase covers two independent work streams:

1. **Credential exchange API** — secure vault token delivery for shared
   containers (`max_sessions_per_worker > 1`). Phase 1-4 introduced
   session sharing, exposing a gap in the current credential injection
   model.
2. **Content discovery** — a catalog API listing accessible apps with
   metadata, tags, and search/filter support. Includes proxy-level UUID
   resolution so apps can be accessed via stable `/app/{uuid}/` paths
   in addition to `/app/{name}/`.

This phase depends on phase 1-2 (RBAC), phase 1-3 (OpenBao integration),
and phase 1-4 (session sharing).

**Vanity URLs** (top-level path aliases like `/sales-dashboard/`) were
considered and dropped. App names are already human-readable, so the
marginal improvement didn't justify the routing complexity (catch-all
routes competing with `/healthz`, `/login`, etc.) and auth middleware
concerns. If top-level vanity URLs become important later, they can be
added as a standalone feature.

---

## Part A: Credential Exchange API

### Problem

Phase 1-3 introduced per-request credential injection: the proxy
exchanges the user's IdP access token for a scoped OpenBao token and
injects it as the `X-Blockyard-Vault-Token` HTTP header. This is safe
when `max_sessions_per_worker = 1` — each container is single-tenant,
so the R process only ever sees one user's token.

Phase 1-4 introduced `max_sessions_per_worker > 1`, which means
multiple users' requests are proxied to the same R process. The raw
vault token in a header could leak between co-tenant sessions — e.g.
if the app logs request headers or stores them in a shared global
variable. The current code (in `proxy.go:injectVaultToken`) already
skips injection for shared containers with a comment deferring to this
phase.

### Solution: two-phase exchange pattern

Posit Connect solves an analogous problem (per-user OAuth tokens in
shared R processes) with a two-phase exchange pattern: the proxy
injects a signed, short-lived, scoped *session reference token*, and
the app exchanges it for the real credential by calling back to the
server's API. The actual secret never crosses the proxy layer.

blockyard adopts the same pattern:

1. **Proxy injects `X-Blockyard-Session-Token`** — a signed,
   short-lived token containing the user's identity and the worker ID.
   This replaces `X-Blockyard-Vault-Token` for shared containers.
2. **App calls `POST /api/v1/credentials/vault`** — presenting the
   session token as a bearer credential. The server validates the
   token, exchanges the user's identity for a scoped OpenBao token,
   and returns it.
3. **Single-tenant fallback** — when `max_sessions_per_worker = 1`,
   the proxy continues injecting `X-Blockyard-Vault-Token` directly
   for backwards compatibility (zero code changes in the app).

### Design decision: session token format

The session reference token uses the same `base64url(json).base64url(hmac)`
format as the existing `auth.CookiePayload`. This avoids adding a JWT
library dependency — the token is server-issued and server-validated, so
interoperability with external JWT consumers is not needed.

**Domain separation.** The session token is signed with a key derived
from the same `session_secret` but with a different domain string
(`"blockyard-session-token"` vs `"blockyard-cookie-signing"`). This
prevents a session cookie from being accepted as a session token or
vice versa.

**Claims:**

| Claim | Type   | Description |
|-------|--------|-------------|
| `sub` | string | Authenticated user's subject identifier |
| `app` | string | App ID (scopes the token to one app) |
| `wid` | string | Worker ID (scopes the token to one process) |
| `iat` | int64  | Issued-at timestamp (Unix seconds) |
| `exp` | int64  | Expiry timestamp (Unix seconds) |

**Expiry:** 5 minutes. Refreshed on every proxied request (the proxy
generates a fresh token per request). The short expiry limits the
window for token replay.

**Why `wid`?** Without the worker ID claim, a session token obtained
from worker A could be replayed against the exchange endpoint by code
running in worker B — a different container with potentially different
tenants. The `wid` claim lets the exchange endpoint verify that the
caller is running in the same container that the proxy routed the
request to.

### Design decision: exchange endpoint auth model

The `POST /api/v1/credentials/vault` endpoint uses the session
reference token itself as its authentication — presented as a standard
`Authorization: Bearer <token>` header. It does NOT require the
control-plane API bearer token (the R process in a worker container
does not have it).

**Validation checks:**

1. Signature — HMAC verification with the session token signing key
2. Expiry — reject tokens past their `exp` claim
3. Worker existence — `wid` must be in the active worker map
4. App match — `app` must match the worker's app ID (prevents a token
   for app A from being used to obtain credentials scoped to app B)

### Design decision: BLOCKYARD_API_URL environment variable

Worker containers need to know where to call the exchange endpoint.
The server injects `BLOCKYARD_API_URL` as an environment variable at
spawn time (alongside the existing `VAULT_ADDR`). The value is
derived from the server's `external_url` config. When `external_url`
is not set (dev mode), it falls back to `http://host.docker.internal:{port}`
or the container gateway address.

### What's already done

Phase 1-3 delivered:

- `injectVaultToken()` in `proxy.go` — already skips when
  `maxSessionsPerWorker > 1` with a comment pointing to this phase
- `VaultTokenCache` — caches scoped tokens by user sub
- `VaultClient.JWTLogin()` — exchanges IdP access token for OpenBao token
- `VAULT_ADDR` env var injection in `coldstart.go:spawnWorker()`
- `X-Blockyard-Vault-Token` header forwarding in `ws.go:shuttleWS()`

Phase 1-4 delivered:

- `max_sessions_per_worker > 1` support
- Load balancing, session sharing, auto-scaling

Auth infrastructure (from phase 1-1):

- `auth.SigningKey` — HMAC-SHA256 signing with domain separation
- `auth.DeriveSigningKey(secret)` — derives a key from the session secret
- `auth.CookiePayload` — `base64url(json).base64url(hmac)` encode/decode
- `auth.UserFromContext()` — extracts authenticated user from context
- `auth.CallerFromContext()` — extracts caller identity from context

### Step 1: Session reference token

New file: `internal/auth/sessiontoken.go` — encode and decode session
reference tokens for the credential exchange flow.

```go
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionTokenClaims is the payload of a session reference token.
// Issued by the proxy on each request to shared containers. The app
// exchanges this for real credentials via the credential exchange API.
type SessionTokenClaims struct {
	Sub string `json:"sub"` // user subject
	App string `json:"app"` // app ID
	Wid string `json:"wid"` // worker ID
	Iat int64  `json:"iat"` // issued at (unix seconds)
	Exp int64  `json:"exp"` // expiry (unix seconds)
}

// SessionTokenTTL is the validity window for session reference tokens.
const SessionTokenTTL = 5 * time.Minute

// DeriveSessionTokenKey derives a signing key for session tokens.
// Uses a different domain string than cookie signing to prevent
// cross-protocol token confusion.
func DeriveSessionTokenKey(secret string) *SigningKey {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("blockyard-session-token"))
	return &SigningKey{key: mac.Sum(nil)}
}

// EncodeSessionToken serializes and signs a session reference token.
// Format: base64url(json) + "." + base64url(hmac)
func EncodeSessionToken(claims *SessionTokenClaims, key *SigningKey) (string, error) {
	jsonBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal session token: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	sig := mac.Sum(nil)

	payload := base64.RawURLEncoding.EncodeToString(jsonBytes)
	signature := base64.RawURLEncoding.EncodeToString(sig)
	return payload + "." + signature, nil
}

// DecodeSessionToken verifies the HMAC signature and deserializes the
// claims. Returns an error if the signature is invalid or the token
// has expired.
func DecodeSessionToken(token string, key *SigningKey) (*SessionTokenClaims, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed session token: missing separator")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode session token payload: %w", err)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode session token signature: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(payloadBytes)
	expected := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expected) {
		return nil, errors.New("invalid session token signature")
	}

	var claims SessionTokenClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal session token: %w", err)
	}

	if time.Now().Unix() > claims.Exp {
		return nil, errors.New("session token expired")
	}

	return &claims, nil
}
```

**Server struct addition:**

```go
type Server struct {
    // ... existing fields ...
    SessionTokenKey *auth.SigningKey // for credential exchange tokens
}
```

Initialized in `cmd/blockyard/main.go` alongside the existing
`SigningKey`:

```go
srv.SessionTokenKey = auth.DeriveSessionTokenKey(
    cfg.Server.SessionSecret.Expose(),
)
```

**Tests:**

- `TestSessionTokenRoundTrip` — encode and decode, verify all claims
- `TestSessionTokenExpired` — set exp in the past, decode returns error
- `TestSessionTokenTampered` — modify payload after encoding, decode
  returns signature error
- `TestSessionTokenWrongKey` — sign with key A, decode with key B,
  returns error
- `TestSessionTokenKeyDomainSeparation` — a cookie payload encoded
  with `DeriveSigningKey` must not decode with `DeriveSessionTokenKey`
  and vice versa

### Step 2: Proxy — inject session token for shared containers

Modify `injectVaultToken()` in `internal/proxy/proxy.go` to inject
`X-Blockyard-Session-Token` instead of skipping entirely when
`maxSessionsPerWorker > 1`.

**Current behavior (phase 1-3):**

```
max_sessions_per_worker = 1  → inject X-Blockyard-Vault-Token
max_sessions_per_worker > 1  → skip (no credential injection)
```

**New behavior:**

```
max_sessions_per_worker = 1  → inject X-Blockyard-Vault-Token (unchanged)
max_sessions_per_worker > 1  → inject X-Blockyard-Session-Token
```

```go
// injectCredentials handles per-request credential injection.
// For single-tenant containers: injects raw vault token (backwards compat).
// For shared containers: injects a signed session reference token that
// the app exchanges for vault credentials via the credential exchange API.
func injectCredentials(r *http.Request, srv *server.Server, appID, workerID string, maxSessionsPerWorker int) {
	r.Header.Del("X-Blockyard-Vault-Token")
	r.Header.Del("X-Blockyard-Session-Token")

	if srv.VaultClient == nil {
		return
	}

	user := auth.UserFromContext(r.Context())
	if user == nil || user.AccessToken == "" {
		return
	}

	if maxSessionsPerWorker > 1 {
		// Shared container — inject session reference token.
		// The app exchanges this for real credentials.
		now := time.Now().Unix()
		claims := &auth.SessionTokenClaims{
			Sub: user.Sub,
			App: appID,
			Wid: workerID,
			Iat: now,
			Exp: now + int64(auth.SessionTokenTTL.Seconds()),
		}
		token, err := auth.EncodeSessionToken(claims, srv.SessionTokenKey)
		if err != nil {
			slog.Warn("failed to encode session token",
				"sub", user.Sub, "error", err)
			return
		}
		r.Header.Set("X-Blockyard-Session-Token", token)
		return
	}

	// Single-tenant container — inject raw vault token (backwards compat).
	token, ok := srv.VaultTokenCache.Get(user.Sub)
	if !ok {
		var err error
		var ttl time.Duration
		token, ttl, err = srv.VaultClient.JWTLogin(
			r.Context(),
			srv.Config.Openbao.JWTAuthPath,
			user.AccessToken,
		)
		if err != nil {
			slog.Warn("vault JWT login failed",
				"sub", user.Sub, "error", err)
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

**Call site change in `Handler()`:** the function now needs `appID`
and `workerID` (previously it only needed `maxSessionsPerWorker`):

```go
// Before:
injectVaultToken(r, srv, app.MaxSessionsPerWorker)

// After:
injectCredentials(r, srv, app.ID, workerID, app.MaxSessionsPerWorker)
```

**WebSocket header forwarding.** `shuttleWS` in `ws.go` already
forwards `X-Blockyard-Vault-Token` to the backend on the WebSocket
dial. Add `X-Blockyard-Session-Token` to the same forwarding block:

```go
if v := r.Header.Get("X-Blockyard-Session-Token"); v != "" {
    dialHeaders.Set("X-Blockyard-Session-Token", v)
}
```

**Tests:**

- `TestInjectCredentialsSingleTenant` — `maxSessionsPerWorker = 1`,
  verify `X-Blockyard-Vault-Token` is set and
  `X-Blockyard-Session-Token` is absent
- `TestInjectCredentialsSharedContainer` — `maxSessionsPerWorker > 1`,
  verify `X-Blockyard-Session-Token` is set and
  `X-Blockyard-Vault-Token` is absent. Decode the token and verify
  claims match (sub, app, wid, exp within expected range)
- `TestInjectCredentialsNoVault` — `VaultClient == nil`, verify both
  headers are absent
- `TestInjectCredentialsNoUser` — no authenticated user, verify both
  headers are absent
- `TestInjectCredentialsStripsExisting` — set spoofed
  `X-Blockyard-Session-Token` on incoming request, verify it is
  replaced (not passed through)

### Step 3: BLOCKYARD_API_URL environment variable

Worker containers need to call back to the server's credential
exchange endpoint. Add `BLOCKYARD_API_URL` alongside the existing
`VAULT_ADDR` injection in `coldstart.go:spawnWorker()`.

```go
if srv.Config.Openbao != nil {
    extraEnv = map[string]string{
        "VAULT_ADDR":        srv.Config.Openbao.Address,
        "BLOCKYARD_API_URL": srv.Config.Server.ExternalURL,
    }
}
```

**Config requirement:** `external_url` must be set when `[openbao]` is
configured — the worker container needs a routable URL to call back to
the server. This is already a soft requirement (the OIDC callback URL
depends on it); phase 1-5 makes it a validation error when both
`[openbao]` and `max_sessions_per_worker > 1` are configured but
`external_url` is empty.

**Tests:**

- `TestSpawnWorkerInjectsAPIURL` — spawn with `[openbao]` configured,
  verify `BLOCKYARD_API_URL` is in the worker spec's Env map

### Step 4: Credential exchange endpoint

New file: `internal/api/credentials.go`

`POST /api/v1/credentials/vault` — accepts a session reference token,
validates it, and returns a scoped OpenBao token.

```go
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// ExchangeVaultCredential handles POST /api/v1/credentials/vault.
// Accepts a session reference token (as Bearer auth), validates it,
// and returns a scoped OpenBao token.
//
// This endpoint does NOT use the standard API bearer token auth.
// The session reference token is its own authentication — it proves
// the caller was routed through the proxy to a specific worker.
func ExchangeVaultCredential(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Extract Bearer token
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"Missing Bearer token")
			return
		}
		rawToken := authHeader[7:]

		// 2. Decode and validate session token
		claims, err := auth.DecodeSessionToken(rawToken, srv.SessionTokenKey)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_token",
				"Invalid or expired session token")
			return
		}

		// 3. Verify worker exists and belongs to the claimed app
		worker, ok := srv.Workers.Get(claims.Wid)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid_token",
				"Worker not found")
			return
		}
		if worker.AppID != claims.App {
			writeError(w, http.StatusUnauthorized, "invalid_token",
				"Token app does not match worker")
			return
		}

		// 4. Exchange user identity for a scoped OpenBao token.
		// Look up the user's IdP access token from the session store,
		// then exchange it via VaultClient.JWTLogin (with caching).
		if srv.VaultClient == nil {
			writeError(w, http.StatusServiceUnavailable, "vault_unavailable",
				"Credential service not configured")
			return
		}

		userSession := srv.UserSessions.Get(claims.Sub)
		if userSession == nil || userSession.AccessToken == "" {
			writeError(w, http.StatusUnauthorized, "session_expired",
				"User session not found or expired")
			return
		}

		vaultToken, ok := srv.VaultTokenCache.Get(claims.Sub)
		if !ok {
			var ttl time.Duration
			vaultToken, ttl, err = srv.VaultClient.JWTLogin(
				r.Context(),
				srv.Config.Openbao.JWTAuthPath,
				userSession.AccessToken,
			)
			if err != nil {
				slog.Warn("credential exchange: vault login failed",
					"sub", claims.Sub, "error", err)
				writeError(w, http.StatusBadGateway, "vault_error",
					"Failed to obtain vault token")
				return
			}
			if ttl == 0 {
				ttl = srv.Config.Openbao.TokenTTL.Duration
			}
			srv.VaultTokenCache.Set(claims.Sub, vaultToken, ttl)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"token": vaultToken,
			"ttl":   int(srv.Config.Openbao.TokenTTL.Duration.Seconds()),
		})
	}
}
```

**Router addition.** The exchange endpoint is mounted outside the
standard `APIAuth` middleware — it uses session token auth instead:

```go
// Credential exchange — session token auth (not API bearer token)
r.Post("/api/v1/credentials/vault", ExchangeVaultCredential(srv))
```

This is placed before the `r.Route("/api/v1", ...)` block so it is
not wrapped by `APIAuth`.

**Tests:**

- `TestExchangeValidToken` — encode a valid session token, call
  endpoint, verify 200 with vault token in response
- `TestExchangeExpiredToken` — token with past expiry, verify 401
- `TestExchangeTamperedToken` — modify token after signing, verify 401
- `TestExchangeUnknownWorker` — valid signature but wid not in worker
  map, verify 401
- `TestExchangeAppMismatch` — valid token but wid belongs to a
  different app than the `app` claim, verify 401
- `TestExchangeNoVault` — `VaultClient == nil`, verify 503
- `TestExchangeNoUserSession` — valid token but user's IdP session
  has expired (not in UserSessionStore), verify 401
- `TestExchangeMissingBearer` — no Authorization header, verify 401

### Step 5: R helper documentation

Document the R-side usage pattern for app developers. This is not
shipped code — it's documentation showing how to use the exchange API
from within a Shiny app.

```r
# blockyard_vault_token() — obtain a per-user OpenBao token.
#
# Call this from within a Shiny server function. The session token is
# injected by the blockyard proxy as a request header. The function
# exchanges it for a real vault token by calling back to the server.
#
# Returns the vault token string, or NULL if credentials are not
# available (e.g. OpenBao not configured, user not authenticated).
blockyard_vault_token <- function(session) {
  session_token <- session$request$HTTP_X_BLOCKYARD_SESSION_TOKEN
  if (is.null(session_token) || session_token == "") {
    # Fall back to direct injection (single-tenant mode)
    token <- session$request$HTTP_X_BLOCKYARD_VAULT_TOKEN
    if (!is.null(token) && token != "") return(token)
    return(NULL)
  }

  api_url <- Sys.getenv("BLOCKYARD_API_URL", unset = "")
  if (api_url == "") return(NULL)

  resp <- httr2::request(api_url) |>
    httr2::req_url_path("/api/v1/credentials/vault") |>
    httr2::req_auth_bearer_token(session_token) |>
    httr2::req_error(is_error = function(resp) FALSE) |>
    httr2::req_perform()

  if (httr2::resp_status(resp) != 200L) return(NULL)

  httr2::resp_body_json(resp)$token
}
```

The helper transparently handles both modes:

1. **Shared container** (`max_sessions_per_worker > 1`) — reads
   `X-Blockyard-Session-Token`, exchanges it via the API.
2. **Single-tenant container** (`max_sessions_per_worker = 1`) — falls
   back to reading `X-Blockyard-Vault-Token` directly.

App developers use the same function regardless of deployment mode.

### Credential exchange — new source files

| File | Purpose |
|------|---------|
| `internal/auth/sessiontoken.go` | `SessionTokenClaims`, encode/decode, `DeriveSessionTokenKey` |
| `internal/auth/sessiontoken_test.go` | Unit tests for session token signing |
| `internal/api/credentials.go` | `ExchangeVaultCredential` handler |
| `internal/api/credentials_test.go` | Integration tests for the exchange endpoint |

### Credential exchange — modified files

| File | Change |
|------|--------|
| `internal/proxy/proxy.go` | Rename `injectVaultToken` → `injectCredentials`, add session token path |
| `internal/proxy/ws.go` | Forward `X-Blockyard-Session-Token` on WS dial |
| `internal/proxy/coldstart.go` | Inject `BLOCKYARD_API_URL` env var |
| `internal/api/router.go` | Mount `POST /api/v1/credentials/vault` outside `APIAuth` |
| `internal/server/state.go` | Add `SessionTokenKey *auth.SigningKey` field |
| `cmd/blockyard/main.go` | Initialize `SessionTokenKey` |

---

## Part B: Content Discovery

User-facing features for navigating and accessing deployed content.

### Design decision: proxy resolves apps by UUID and name

The proxy handler at `/app/{name}/*` resolves the URL parameter by
trying `GetApp(id)` (UUID lookup) first, then falling back to
`GetAppByName(name)`. This gives every app a stable URL at
`/app/{uuid}/` that survives renames, in addition to the human-readable
`/app/{name}/` path.

The API already uses this pattern (`resolveApp()` in `apps.go` does
UUID-first, name-second). Extending it to the proxy is a one-line
change and provides stable URLs for bookmarks, external links, and
programmatic integrations without introducing a separate vanity URL
system.

### Design decision: catalog visibility respects RBAC

The catalog API only returns apps the caller has access to:

- **Admins** see all apps.
- **Other authenticated users** see apps they own, apps with explicit
  ACL grants (user or group), and public apps.
- **Unauthenticated callers** (when OIDC is not configured or the caller
  has no token) see only public apps.

This means the catalog is not a uniform view — different users see
different results. The query uses the same `EvaluateAccess` logic from
phase 1-2, but applied at the database level for efficiency (filtering
in SQL rather than loading all apps and filtering in Go).

### Design decision: tags are admin-managed

Tags are created and deleted by admins only. Any authenticated user can
view tags and filter the catalog by tag. App owners and collaborators can
attach/detach existing tags to their apps.

**Why not user-created tags?**

- User-created tags tend toward chaos (duplicate tags, inconsistent
  naming, tag pollution). Admin-managed tags enforce a controlled
  vocabulary.
- The tag set is expected to be small (tens, not thousands) — categories
  like "finance", "reporting", "operations".

### Design decision: title and description fields on apps

Phase 1-5 adds `title` and `description` columns to the `apps` table.
These are optional human-readable metadata for the catalog:

- **`title`** — display name (e.g., "Sales Dashboard"). Falls back to
  the app name if not set.
- **`description`** — short description for catalog listings.

These are set via `PATCH /api/v1/apps/{id}` alongside existing fields.

### Deliverables

1. Proxy UUID resolution — `/app/{uuid}/` resolves to the same app as
   `/app/{name}/`
2. Schema migration — `title`, `description` on apps; `tags` and
   `app_tags` tables
3. `title` and `description` via `PATCH /api/v1/apps/{id}`
4. Tag management API — `POST/GET/DELETE /api/v1/tags`
5. App tag management — `POST/DELETE /api/v1/apps/{id}/tags`
6. Catalog API — `GET /api/v1/catalog` with tag/search/pagination
7. DB access layer for tags and catalog queries

### Step 6: Schema migration

Add columns to `apps` and create tag tables. As with phase 1-2, this is
folded into the consolidated schema (pre-release):

**Apps table additions:**

```sql
ALTER TABLE apps ADD COLUMN title TEXT;
ALTER TABLE apps ADD COLUMN description TEXT;
```

**New tables:**

```sql
CREATE TABLE IF NOT EXISTS tags (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS app_tags (
    app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, tag_id)
);
```

`ON DELETE CASCADE` on both FKs: deleting an app removes its tag
associations; deleting a tag removes all associations to that tag.

**`AppRow` changes:**

```go
type AppRow struct {
    ID                   string
    Name                 string
    Owner                string
    AccessType           string
    Title                *string  // new
    Description          *string  // new
    ActiveBundle         *string
    MaxWorkersPerApp     *int
    MaxSessionsPerWorker int
    MemoryLimit          *string
    CPULimit             *float64
    CreatedAt            string
    UpdatedAt            string
}
```

**Tests:**

- Schema creates without error
- Apps with and without title/description

### Step 7: Proxy UUID resolution

Modify the proxy handler in `proxy.go` to resolve apps by UUID first,
then by name:

```go
// 1. Look up app by ID (UUID) first, then by name.
// UUID lookup gives stable URLs that survive app renames.
app, err := srv.DB.GetApp(appName)
if err != nil {
    // ...
}
if app == nil {
    app, err = srv.DB.GetAppByName(appName)
    // ...
}
```

This mirrors the existing `resolveApp()` pattern used by the API
handlers. Both `/app/{uuid}/` and `/app/{name}/` resolve to the same
app and go through the same auth, session, and proxy logic.

**Tests:**

- Proxy resolves by name (existing behavior)
- Proxy resolves by UUID (new)
- Unknown name/UUID returns 404

### Step 8: Title and description via update endpoint

Extend the `PATCH /api/v1/apps/{id}` handler to accept `title` and
`description`:

```go
type updateAppBody struct {
    // ... existing fields ...
    Title       *string `json:"title"`
    Description *string `json:"description"`
}
```

Both fields are passed through `AppUpdate` and persisted via
`UpdateApp()`. The response includes the new fields in `AppResponse`.

**Tests:**

- Set title and description — 200
- Fields appear in GET response

### Step 9: Tag management API

**DB methods for tags:**

```go
type TagRow struct {
    ID        string
    Name      string
    CreatedAt string
}

func (db *DB) CreateTag(name string) (*TagRow, error) {
    id := uuid.New().String()
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := db.Exec(
        "INSERT INTO tags (id, name, created_at) VALUES (?, ?, ?)",
        id, name, now,
    )
    if err != nil {
        return nil, err
    }
    return &TagRow{ID: id, Name: name, CreatedAt: now}, nil
}

func (db *DB) ListTags() ([]TagRow, error) {
    rows, err := db.Query("SELECT id, name, created_at FROM tags ORDER BY name")
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var tags []TagRow
    for rows.Next() {
        var t TagRow
        if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
            return nil, err
        }
        tags = append(tags, t)
    }
    return tags, rows.Err()
}

func (db *DB) DeleteTag(id string) (bool, error) {
    result, err := db.Exec("DELETE FROM tags WHERE id = ?", id)
    if err != nil {
        return false, err
    }
    n, _ := result.RowsAffected()
    return n > 0, nil
}

func (db *DB) AddAppTag(appID, tagID string) error {
    _, err := db.Exec(
        "INSERT OR IGNORE INTO app_tags (app_id, tag_id) VALUES (?, ?)",
        appID, tagID,
    )
    return err
}

func (db *DB) RemoveAppTag(appID, tagID string) (bool, error) {
    result, err := db.Exec(
        "DELETE FROM app_tags WHERE app_id = ? AND tag_id = ?",
        appID, tagID,
    )
    if err != nil {
        return false, err
    }
    n, _ := result.RowsAffected()
    return n > 0, nil
}

func (db *DB) ListAppTags(appID string) ([]TagRow, error) {
    rows, err := db.Query(
        `SELECT t.id, t.name, t.created_at
         FROM tags t
         JOIN app_tags at ON t.id = at.tag_id
         WHERE at.app_id = ?
         ORDER BY t.name`,
        appID,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var tags []TagRow
    for rows.Next() {
        var t TagRow
        if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
            return nil, err
        }
        tags = append(tags, t)
    }
    return tags, rows.Err()
}
```

**API endpoints** — new file: `internal/api/tags.go`

```go
// Tag management — admin only
r.Route("/api/v1/tags", func(r chi.Router) {
    r.Get("/", listTags(srv))       // any authenticated user
    r.Post("/", createTag(srv))     // admin only
    r.Delete("/{tagID}", deleteTag(srv)) // admin only
})

// App tag management — owner/collaborator/admin
r.Post("/api/v1/apps/{id}/tags", addAppTag(srv))
r.Delete("/api/v1/apps/{id}/tags/{tagID}", removeAppTag(srv))
```

**Tag name validation:** same rules as app names (1-63 lowercase ASCII
letters, digits, hyphens, starting with a letter).

**Tests:**

- Create tag — 201
- Create duplicate tag — 409
- List tags — returns all, sorted by name
- Delete tag — 204, cascade removes app_tags
- Delete nonexistent tag — 404
- Add tag to app — 204
- Add same tag twice — idempotent (no error)
- Remove tag from app — 204
- Non-admin cannot create/delete tags — 404

### Step 10: Catalog API

New file: `internal/api/catalog.go`

**Endpoint:** `GET /api/v1/catalog`

**Query parameters:**

| Parameter | Type | Default | Description |
|---|---|---|---|
| `tag` | string | — | Filter by tag name (exact match) |
| `search` | string | — | Search in name, title, description |
| `page` | int | 1 | Page number (1-indexed) |
| `per_page` | int | 20 | Items per page (max 100) |

**Response:**

```json
{
    "items": [
        {
            "id": "a3f2c1...",
            "name": "sales-dashboard",
            "title": "Sales Dashboard",
            "description": "Q4 sales metrics and KPIs",
            "owner": "user-sub",
            "tags": ["finance", "reporting"],
            "status": "running",
            "url": "/app/sales-dashboard/",
            "updated_at": "2026-03-10T12:00:00Z"
        }
    ],
    "total": 42,
    "page": 1,
    "per_page": 20
}
```

**`url` field:** always `/app/{name}/`. This gives clients the canonical
URL to link to.

**`status` field:** derived from the worker map (same as `GET /apps`).

**DB query for catalog:**

The catalog query must filter by access control. For admins, it returns
all apps. For other callers, it returns apps where:

- The caller is the owner, OR
- The caller has an explicit per-user ACL grant, OR
- The app's `access_type` is `'logged_in'` and the caller is
  authenticated, OR
- The app's `access_type` is `'public'`

*(Updated in wrap-up §1: group-based grants removed. ACL is
per-user only.)*

```go
func (db *DB) ListCatalog(params CatalogParams) ([]AppRow, int, error) {
    var conditions []string
    var args []any

    // Access control filter
    if params.CallerRole == "admin" {
        // Admin sees everything — no filter
    } else if params.CallerSub != "" {
        accessFilter := `(
            apps.owner = ?
            OR apps.access_type IN ('public', 'logged_in')
            OR EXISTS (
                SELECT 1 FROM app_access
                WHERE app_access.app_id = apps.id
                AND app_access.kind = 'user'
                AND app_access.principal = ?
            )
        )`
        conditions = append(conditions, accessFilter)
        args = append(args, params.CallerSub, params.CallerSub)
    } else {
        // Unauthenticated — public apps only
        conditions = append(conditions, "apps.access_type = 'public'")
    }

    // Tag filter
    if params.Tag != "" {
        conditions = append(conditions,
            `EXISTS (
                SELECT 1 FROM app_tags
                JOIN tags ON tags.id = app_tags.tag_id
                WHERE app_tags.app_id = apps.id AND tags.name = ?
            )`)
        args = append(args, params.Tag)
    }

    // Search filter
    if params.Search != "" {
        conditions = append(conditions,
            "(apps.name LIKE ? OR apps.title LIKE ? OR apps.description LIKE ?)")
        like := "%" + params.Search + "%"
        args = append(args, like, like, like)
    }

    where := ""
    if len(conditions) > 0 {
        where = "WHERE " + strings.Join(conditions, " AND ")
    }

    // Count total
    var total int
    countQuery := "SELECT COUNT(*) FROM apps " + where
    if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
        return nil, 0, err
    }

    // Fetch page
    query := fmt.Sprintf(
        `SELECT `+appColumns+`
         FROM apps %s
         ORDER BY updated_at DESC
         LIMIT ? OFFSET ?`,
        where,
    )
    args = append(args, params.PerPage, (params.Page-1)*params.PerPage)

    rows, err := db.Query(query, args...)
    // ... scan rows into []AppRow ...

    return apps, total, nil
}
```

**CatalogParams:**

```go
type CatalogParams struct {
    CallerSub    string
    CallerRole   string
    Tag          string
    Search       string
    Page         int
    PerPage      int
}
```

**Handler** — `internal/api/catalog.go`:

```go
func catalogHandler(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        caller := auth.CallerFromContext(r.Context())

        params := db.CatalogParams{
            Tag:     r.URL.Query().Get("tag"),
            Search:  r.URL.Query().Get("search"),
            Page:    parseIntOr(r.URL.Query().Get("page"), 1),
            PerPage: clamp(parseIntOr(r.URL.Query().Get("per_page"), 20), 1, 100),
        }
        if caller != nil {
            params.CallerSub = caller.Sub
            params.CallerRole = caller.Role.String()
        }

        apps, total, err := srv.DB.ListCatalog(params)
        if err != nil {
            writeError(w, http.StatusInternalServerError, "db_error", "Failed to query catalog")
            return
        }

        // Build response items with tags and status
        items := make([]catalogItem, 0, len(apps))
        for _, app := range apps {
            tags, _ := srv.DB.ListAppTags(app.ID)
            tagNames := make([]string, len(tags))
            for i, t := range tags {
                tagNames[i] = t.Name
            }

            status := "stopped"
            if srv.Workers.CountForApp(app.ID) > 0 {
                status = "running"
            }

            items = append(items, catalogItem{
                ID:          app.ID,
                Name:        app.Name,
                Title:       app.Title,
                Description: app.Description,
                Owner:       app.Owner,
                Tags:        tagNames,
                Status:      status,
                URL:         "/app/" + app.Name + "/",
                UpdatedAt:   app.UpdatedAt,
            })
        }

        writeJSON(w, http.StatusOK, map[string]any{
            "items":    items,
            "total":    total,
            "page":     params.Page,
            "per_page": params.PerPage,
        })
    }
}
```

**Router addition:**

```go
r.Route("/api/v1", func(r chi.Router) {
    r.Use(authMiddleware(srv))

    // ... existing routes ...

    r.Get("/catalog", catalogHandler(srv))
})
```

**Note:** the catalog endpoint uses the same auth middleware as other API
endpoints. When OIDC is not configured (dev mode), the static token gives
admin access, so the catalog shows all apps.

**Tests:**

- Admin sees all apps
- Viewer sees only apps they have access to (owned, ACL, public)
- Unauthenticated caller sees only public apps
- Tag filter — only apps with the given tag
- Search filter — matches name, title, description
- Pagination — correct total, page, per_page
- Empty catalog — returns empty items array
- Status field reflects running/stopped state

---

## New source files (all parts)

| File | Purpose |
|------|---------|
| `internal/auth/sessiontoken.go` | Session reference token encode/decode |
| `internal/auth/sessiontoken_test.go` | Session token unit tests |
| `internal/api/credentials.go` | `POST /api/v1/credentials/vault` exchange endpoint |
| `internal/api/credentials_test.go` | Exchange endpoint tests |
| `internal/api/tags.go` | Tag CRUD and app-tag management endpoints |
| `internal/api/tags_test.go` | Tag endpoint tests |
| `internal/api/catalog.go` | Catalog API handler |
| `internal/api/catalog_test.go` | Catalog endpoint tests |

## Modified files (all parts)

| File | Change |
|------|--------|
| `internal/proxy/proxy.go` | Rename `injectVaultToken` → `injectCredentials`, add session token path, resolve apps by UUID then name |
| `internal/proxy/ws.go` | Forward `X-Blockyard-Session-Token` on WS dial |
| `internal/proxy/coldstart.go` | Inject `BLOCKYARD_API_URL` env var |
| `internal/api/router.go` | Mount credential exchange, tag routes, catalog |
| `internal/api/apps.go` | Accept `title`, `description` in PATCH |
| `internal/server/state.go` | Add `SessionTokenKey` field |
| `internal/db/db.go` | Add `title`, `description` to AppRow; tag CRUD; `ListCatalog`; `IsUniqueConstraintError` |
| `internal/auth/identity.go` | Add `CanManageTags()` role method |
| `cmd/blockyard/main.go` | Initialize `SessionTokenKey` |
| `migrations/001_initial.sql` | Add `title`, `description` columns; `tags` and `app_tags` tables |

## Exit criteria

Phase 1-5 is done when:

**Credential exchange:**

- Proxy injects `X-Blockyard-Vault-Token` for single-tenant apps
  (`max_sessions_per_worker = 1`) — unchanged from phase 1-3
- Proxy injects `X-Blockyard-Session-Token` for shared apps
  (`max_sessions_per_worker > 1`) — new
- Both headers are stripped from incoming requests (anti-spoofing)
- Session token contains correct claims (sub, app, wid, iat, exp)
- Session token expires after 5 minutes
- Session token signed with domain-separated key (not interchangeable
  with session cookies)
- `POST /api/v1/credentials/vault` accepts valid session token and
  returns vault token
- Exchange endpoint rejects expired, tampered, or wrong-worker tokens
- Exchange endpoint does NOT require API bearer token
- `BLOCKYARD_API_URL` is injected into worker containers when
  `[openbao]` is configured
- WebSocket dial forwards `X-Blockyard-Session-Token` header
- R helper function works in both single-tenant and shared modes
- All existing phase 1-3 credential injection tests still pass

**Content discovery:**

- Proxy resolves apps by UUID (`/app/{uuid}/`) and by name
  (`/app/{name}/`)
- `GET /api/v1/catalog` returns accessible apps with metadata
- Catalog respects RBAC (admins see all, users see permitted, anon
  sees public)
- Tag filter, search filter, and pagination work
- Tags are admin-managed (create, delete)
- App owners can attach/detach tags
- `title` and `description` fields on apps
- `url` field is always `/app/{name}/`

**General:**

- All new unit and integration tests pass
- All existing tests still pass
- `go vet ./...` clean
- `go test ./...` green
