# Phase 1-1: OIDC Authentication + User Sessions

Establish user identity on the app plane. This is the foundation for v1 —
RBAC (phase 1-2), identity injection (phase 1-3), and OpenBao integration
(phase 1-3) all require a logged-in user. The control plane API continues to
use the v0 static bearer token; JWT-based machine auth replaces it in phase
1-2.

This phase adds the `internal/auth/` package, three public HTTP endpoints
(`/login`, `/callback`, `/logout`), a session cookie + server-side session
store, and an auth middleware that protects all `/app/` proxy routes.

## Design decision: server-side session store

The cookie carries only user identity (`sub`, `issued_at`) and an HMAC
signature (~100-150 bytes). All sensitive/bulky data — groups, access token,
refresh token — lives server-side in a `sync.RWMutex`-protected
`map[string]*UserSession` keyed by `sub`.

**Why not cookie-only sessions (the original design)?**

- **Cookie size:** IdP access tokens are often JWTs (800-2000 bytes).
  Combined with groups, encrypted refresh token, and base64 + HMAC overhead,
  the cookie easily exceeds the 4093-byte browser limit. Browsers silently
  drop oversized cookies — hard to debug.
- **Security:** Tokens never transit the browser. No need for AES-GCM
  encryption of the refresh token. Smaller attack surface.
- **Logout works:** Deleting the server-side session entry immediately
  invalidates the session. Cookie-only sessions can't do this without a
  revocation list.
- **Simpler code:** No AES-GCM encryption, no base64 encoding of encrypted
  payloads, fewer dependencies.

**Trade-off:** sessions are lost on server restart — users must
re-authenticate. This is the same failure mode as every other piece of
in-memory state in v1 (workers, proxy sessions, task store). v2's
PostgreSQL-backed state migration would naturally extend to sessions.

## Deliverables

1. `[oidc]` config section — issuer URL, client ID/secret, groups claim,
   cookie max-age
2. `session_secret` field on `[server]` — HMAC key for cookie signing
3. OIDC discovery client — fetch provider metadata and JWKS
4. Authorization code flow endpoints: `GET /login`, `GET /callback`,
   `POST /logout`
5. Server-side session store — `sync.RWMutex` + `map[string]*UserSession`
   keyed by `sub`
6. Signed session cookie — HMAC-SHA256 signed, carries only `sub` +
   `issued_at`
7. Transparent access token refresh middleware
8. App-plane auth middleware — protect `/app/` routes, redirect to `/login`
9. New dependencies: `github.com/coreos/go-oidc/v3`, `golang.org/x/oauth2`
   (stdlib handles HMAC, SHA-256, base64)

## Step-by-step

### Step 1: New dependencies

Add to `go.mod`:

```
require (
    github.com/coreos/go-oidc/v3 v3.12.0
    golang.org/x/oauth2        v0.25.0
)
```

Everything else comes from the standard library:

- `crypto/hmac` + `crypto/sha256` — HMAC-SHA256 for cookie signing
- `encoding/base64` — encoding the cookie payload for transport
- `encoding/json` — cookie payload serialization
- `net/url` — `url.QueryEscape` for return_url in login redirects

**Note:** `go-oidc` handles ID token verification internally. A separate
JWT library (`go-jose`) will be added in phase 1-2 for control-plane JWT
auth.

### Step 2: `Secret` type + config additions

**`Secret` type** — prevents secret leakage in logs. Added to
`internal/config/secret.go`:

```go
package config

import "github.com/BurntSushi/toml"

// Secret wraps a secret string. Its String() and GoString() methods
// return "[REDACTED]" to prevent accidental logging. Use Expose() to
// retrieve the actual value.
type Secret struct {
	value string
}

func NewSecret(s string) Secret {
	return Secret{value: s}
}

func (s Secret) Expose() string { return s.value }
func (s Secret) IsEmpty() bool  { return s.value == "" }
func (s Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer for %#v formatting.
func (s Secret) GoString() string { return "[REDACTED]" }

// UnmarshalText implements encoding.TextUnmarshaler so TOML
// decoding writes the raw string into the Secret wrapper.
func (s *Secret) UnmarshalText(text []byte) error {
	s.value = string(text)
	return nil
}

// Verify interface compliance.
var _ toml.TextUnmarshaler = (*Secret)(nil)
```

**Existing field migrated:** `ServerConfig.Token` changes from `string` to
`Secret`. This is a pre-existing leak (v0 can log the bearer token via
`%+v`) — fix it now since we're touching config anyway.

**New structs** in `internal/config/config.go`:

```go
type OidcConfig struct {
	IssuerURL    string        `toml:"issuer_url"`
	ClientID     string        `toml:"client_id"`
	ClientSecret Secret        `toml:"client_secret"`
	// GroupsClaim removed in wrap-up §1 — IdP groups play no role
	// in blockyard's authorization model.
	InitialAdmin string        `toml:"initial_admin"` // added in wrap-up §1
	CookieMaxAge Duration      `toml:"cookie_max_age"`
}

// oidcDefaults fills in zero-value fields with sensible defaults.
func oidcDefaults(c *OidcConfig) {
	if c.CookieMaxAge.Duration == 0 {
		c.CookieMaxAge.Duration = 24 * time.Hour
	}
}
```

**Changes to existing structs:**

```go
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Docker   DockerConfig   `toml:"docker"`
	Storage  StorageConfig  `toml:"storage"`
	Database DatabaseConfig `toml:"database"`
	Proxy    ProxyConfig    `toml:"proxy"`
	OIDC     *OidcConfig    `toml:"oidc"`     // nil when not configured
}

type ServerConfig struct {
	Bind            string   `toml:"bind"`
	Token           Secret   `toml:"token"`           // changed from string
	SessionSecret   *Secret  `toml:"session_secret"`  // new — required when [oidc] is set without [openbao]
	ExternalURL     string   `toml:"external_url"`    // new
	ShutdownTimeout Duration `toml:"shutdown_timeout"`
}
```

`SessionSecret` is `*Secret` — required when `[oidc]` is configured,
ignored otherwise. Validation enforces this:

```go
func validate(cfg *Config) error {
	// ... existing checks ...
	if cfg.OIDC != nil {
		if cfg.OIDC.IssuerURL == "" {
			return fmt.Errorf("config: oidc.issuer_url must not be empty")
		}
		if cfg.OIDC.ClientID == "" {
			return fmt.Errorf("config: oidc.client_id must not be empty")
		}
		if cfg.OIDC.ClientSecret.IsEmpty() {
			return fmt.Errorf("config: oidc.client_secret must not be empty")
		}
		// (Updated in v1 wrap-up §4B): session_secret can be auto-generated
		// when [openbao] is configured — only required without vault.
		if cfg.Openbao == nil && (cfg.Server.SessionSecret == nil || cfg.Server.SessionSecret.IsEmpty()) {
			return fmt.Errorf("config: server.session_secret is required when [oidc] is configured without [openbao]")
		}
	}
	return nil
}
```

**Env var overlay additions:**

```
BLOCKYARD_SERVER_SESSION_SECRET
BLOCKYARD_SERVER_EXTERNAL_URL
BLOCKYARD_OIDC_ISSUER_URL
BLOCKYARD_OIDC_CLIENT_ID
BLOCKYARD_OIDC_CLIENT_SECRET
BLOCKYARD_OIDC_GROUPS_CLAIM
BLOCKYARD_OIDC_COOKIE_MAX_AGE
```

**Auto-construction of the `[oidc]` section from env vars** (per the v1
plan): if any `BLOCKYARD_OIDC_*` env var is set and `cfg.OIDC` is `nil`,
auto-construct a default `OidcConfig`. Required fields start as zero values
and are caught by `validate()`.

```go
// In applyEnvOverrides(), before applying individual OIDC overrides:
if cfg.OIDC == nil && envPrefixExists("BLOCKYARD_OIDC_") {
	cfg.OIDC = &OidcConfig{}
	oidcDefaults(cfg.OIDC)
}
```

**Tests:**

- `Secret`: `String()` and `GoString()` output `[REDACTED]`, `Expose()` returns value
- `Secret`: deserializes transparently from TOML string
- Parse config with `[oidc]` section present
- Parse config without `[oidc]` section (backward compat)
- Validation: reject empty `issuer_url`, `client_id`, `client_secret`
- Validation: reject `[oidc]` without `session_secret`
- Env var override for each OIDC field
- Auto-construction: set `BLOCKYARD_OIDC_ISSUER_URL` without `[oidc]` in
  TOML, verify section is created
- `TestEnvVarCoverageComplete` passes with new fields

### Step 3: OIDC discovery

`internal/auth/oidc.go` — OIDC discovery client and custom claims.

```go
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCClient wraps the go-oidc provider and oauth2 config.
// Initialized once at server startup via Discover().
type OIDCClient struct {
	provider     *oidc.Provider
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	groupsClaim  string
}

// Discover performs OIDC discovery against the issuer URL and returns
// a configured client ready for the authorization code flow.
func Discover(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL, groupsClaim string) (*OIDCClient, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile"},
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID: clientID,
	})

	return &OIDCClient{
		provider:     provider,
		oauth2Config: oauth2Cfg,
		verifier:     verifier,
		groupsClaim:  groupsClaim,
	}, nil
}

// AuthCodeURL generates the authorization URL with a random state and nonce.
func (c *OIDCClient) AuthCodeURL(state string, nonce string) string {
	return c.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce))
}

// Exchange trades an authorization code for tokens.
func (c *OIDCClient) Exchange(ctx context.Context, code string) (*oauth2.Token, *oidc.IDToken, map[string]json.RawMessage, error) {
	oauth2Token, err := c.oauth2Config.Exchange(ctx, code)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("token exchange: %w", err)
	}

	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		return nil, nil, nil, fmt.Errorf("token response missing id_token")
	}

	idToken, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("id token verification: %w", err)
	}

	// Extract all claims into a generic map for groups extraction.
	var allClaims map[string]json.RawMessage
	if err := idToken.Claims(&allClaims); err != nil {
		return nil, nil, nil, fmt.Errorf("extracting claims: %w", err)
	}

	return oauth2Token, idToken, allClaims, nil
}

// RefreshToken exchanges a refresh token for a new access token.
func (c *OIDCClient) RefreshToken(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	src := c.oauth2Config.TokenSource(ctx, &oauth2.Token{
		RefreshToken: refreshToken,
	})
	newToken, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("token refresh: %w", err)
	}
	return newToken, nil
}

// ExtractGroups reads the configured groups claim from the ID token's
// extra claims. Returns nil if the claim is missing or not an array.
func (c *OIDCClient) ExtractGroups(claims map[string]json.RawMessage) []string {
	raw, ok := claims[c.groupsClaim]
	if !ok {
		slog.Debug("groups claim not present in ID token", "claim", c.groupsClaim)
		return nil
	}

	var groups []string
	if err := json.Unmarshal(raw, &groups); err != nil {
		slog.Warn("groups claim is not a string array — ignoring",
			"claim", c.groupsClaim, "error", err)
		return nil
	}
	return groups
}

// EndSessionEndpoint returns the IdP's end_session_endpoint if
// advertised in discovery metadata, or empty string otherwise.
func (c *OIDCClient) EndSessionEndpoint() string {
	// go-oidc exposes provider claims; check for the optional field.
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := c.provider.Claims(&meta); err != nil {
		return ""
	}
	return meta.EndSession
}
```

**JWKS refresh:** `go-oidc` caches the JWKS internally and re-fetches when
it encounters an unknown key ID during token verification. No manual JWKS
refresh loop is needed for phase 1-1.

**Server struct changes** in `internal/server/state.go`:

```go
type Server struct {
	Config   *config.Config
	Backend  backend.Backend
	DB       *db.DB
	Workers  *WorkerMap
	Sessions *session.Store
	Registry *registry.Registry
	Tasks    *task.Store
	LogStore *logstore.Store

	// Auth fields — nil when [oidc] is not configured (v0 compat).
	OIDCClient   *auth.OIDCClient
	SigningKey    *auth.SigningKey
	UserSessions *auth.UserSessionStore
}

// AuthDeps returns an auth.Deps populated from this server's fields.
// Used by the router to wire auth handlers and middleware without a
// circular import (auth cannot import server).
func (s *Server) AuthDeps() *auth.Deps {
	return &auth.Deps{
		Config:       s.Config,
		OIDCClient:   s.OIDCClient,
		SigningKey:    s.SigningKey,
		UserSessions: s.UserSessions,
	}
}
```

All three auth fields are nil when OIDC is not configured (v0
compatibility).

**Initialization in `cmd/blockyard/main.go`:**

```go
if cfg.OIDC != nil {
	baseURL := cfg.Server.ExternalURL
	if baseURL == "" {
		baseURL = "http://" + cfg.Server.Bind
	}
	redirectURL := baseURL + "/callback"

	oidcClient, err := auth.Discover(
		ctx,
		cfg.OIDC.IssuerURL,
		cfg.OIDC.ClientID,
		cfg.OIDC.ClientSecret.Expose(),
		redirectURL,
		cfg.OIDC.GroupsClaim,
	)
	if err != nil {
		slog.Error("OIDC discovery failed", "error", err)
		os.Exit(1)
	}

	srv.OIDCClient = oidcClient
	srv.SigningKey = auth.DeriveSigningKey(cfg.Server.SessionSecret.Expose())
	srv.UserSessions = auth.NewUserSessionStore()
}
```

**Note on redirect URL:** the redirect URL is constructed from
`config.Server.ExternalURL` if set, otherwise falls back to
`config.Server.Bind` (with `http://`). Production deployments behind a
reverse proxy should set `external_url` to their public HTTPS URL.

### Step 4: Session cookie signing

`internal/auth/session.go` — session cookie signing and server-side session
store.

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
	"sync"
	"time"
)

// SigningKey holds the derived HMAC key for cookie signing.
type SigningKey struct {
	key []byte
}

// DeriveSigningKey derives a signing key from a secret using HMAC
// with a domain separation string.
func DeriveSigningKey(secret string) *SigningKey {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("blockyard-cookie-signing"))
	return &SigningKey{key: mac.Sum(nil)}
}
```

**Session cookie payload:**

```go
// CookiePayload is the minimal payload encoded into the session cookie.
// Signed with HMAC-SHA256. All other session data lives server-side.
type CookiePayload struct {
	Sub      string `json:"sub"`
	IssuedAt int64  `json:"issued_at"`
}

// Encode serializes the payload and signs it with HMAC-SHA256.
// Format: base64url(json) + "." + base64url(hmac)
func (p *CookiePayload) Encode(key *SigningKey) (string, error) {
	jsonBytes, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal cookie payload: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	sig := mac.Sum(nil)

	payload := base64.RawURLEncoding.EncodeToString(jsonBytes)
	signature := base64.RawURLEncoding.EncodeToString(sig)
	return payload + "." + signature, nil
}

// DecodeCookie verifies the HMAC signature and deserializes the payload.
func DecodeCookie(value string, key *SigningKey) (*CookiePayload, error) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed cookie: missing separator")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode cookie payload: %w", err)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode cookie signature: %w", err)
	}

	// Verify signature (constant-time comparison).
	mac := hmac.New(sha256.New, key.key)
	mac.Write(payloadBytes)
	expected := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expected) {
		return nil, errors.New("invalid cookie signature")
	}

	var payload CookiePayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal cookie payload: %w", err)
	}
	return &payload, nil
}
```

**Server-side session store:**

```go
// UserSession holds per-user session data stored server-side. Keyed by sub.
type UserSession struct {
	Groups       []string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // unix timestamp
}

// UserSessionStore is an in-memory session store protected by a RWMutex.
// Same pattern as session.Store, WorkerMap, and task.Store.
type UserSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*UserSession

	// Per-user refresh locks. Prevents concurrent token refreshes for
	// the same user — important because some IdPs (e.g. Keycloak with
	// refresh token rotation) invalidate the refresh token on first use.
	// Without this, concurrent requests during token expiry would race
	// to refresh, and all but the first would fail.
	refreshMu    sync.Mutex
	refreshLocks map[string]*sync.Mutex
}

// NewUserSessionStore creates an empty session store.
func NewUserSessionStore() *UserSessionStore {
	return &UserSessionStore{
		sessions:     make(map[string]*UserSession),
		refreshLocks: make(map[string]*sync.Mutex),
	}
}

// RefreshLock returns the per-user mutex for token refresh. Creates
// one if it does not exist.
func (s *UserSessionStore) RefreshLock(sub string) *sync.Mutex {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	mu, ok := s.refreshLocks[sub]
	if !ok {
		mu = &sync.Mutex{}
		s.refreshLocks[sub] = mu
	}
	return mu
}

// Set inserts or replaces the session for a user.
func (s *UserSessionStore) Set(sub string, session *UserSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sub] = session
}

// Get looks up a user's session by sub. Returns nil if not found.
func (s *UserSessionStore) Get(sub string) *UserSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess := s.sessions[sub]
	if sess == nil {
		return nil
	}
	// Return a copy to avoid holding the lock.
	cp := *sess
	cp.Groups = append([]string(nil), sess.Groups...)
	return &cp
}

// Delete removes a user's session (on logout).
func (s *UserSessionStore) Delete(sub string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sub)

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	delete(s.refreshLocks, sub)
}

// UpdateTokens updates the access/refresh tokens after a refresh.
// Returns false if the session does not exist.
func (s *UserSessionStore) UpdateTokens(sub, accessToken string, refreshToken *string, expiresAt int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sub]
	if !ok {
		return false
	}
	sess.AccessToken = accessToken
	if refreshToken != nil {
		sess.RefreshToken = *refreshToken
	}
	sess.ExpiresAt = expiresAt
	return true
}
```

**Tests:**

- Cookie round-trip: `Encode` then `DecodeCookie` produces identical payload
- Tampered cookie (modified payload) is rejected
- Tampered cookie (modified signature) is rejected
- Malformed cookie value (no dot, empty segments) returns error
- `UserSessionStore`: `Set`, `Get`, `Delete`, `UpdateTokens`

### Step 5: Authorization code flow endpoints

Three new routes: `GET /login`, `GET /callback`, `POST /logout`. These are
registered outside the app-plane auth layer (see Step 7 for the full router
structure).

`internal/auth/handlers.go`:

**Dependency struct and cookie security helpers:**

The `auth` package cannot import `server` (which imports `auth`), so
handlers and middleware receive an `auth.Deps` struct instead of
`*server.Server`. The `Server.AuthDeps()` method (see Step 7) builds it.

```go
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cynkra/blockyard/internal/config"
)

// nowUnix returns the current time as a unix timestamp. Declared as a
// package variable so tests can override it.
var nowUnix = func() int64 {
	return time.Now().Unix()
}

// NowUnix is the exported accessor for nowUnix, used by tests.
func NowUnix() int64 { return nowUnix() }

// Deps carries the dependencies that auth handlers and middleware need.
// Constructed in the router layer from the server struct, avoiding a
// circular import between auth and server.
type Deps struct {
	Config       *config.Config
	OIDCClient   *OIDCClient
	SigningKey    *SigningKey
	UserSessions *UserSessionStore
}

// secureFlag returns "; Secure" if external_url is HTTPS, empty
// string otherwise. Used by all cookie-setting code paths.
func secureFlag(cfg *config.Config) string {
	if strings.HasPrefix(cfg.Server.ExternalURL, "https://") {
		return "; Secure"
	}
	return ""
}

// randomHex generates a cryptographically random hex string of n bytes.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

**`GET /login`:**

```go
// LoginHandler initiates the OIDC authorization code flow.
// Query params: ?return_url=/app/my-app/ (optional, default: /)
func LoginHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.OIDCClient == nil {
			http.NotFound(w, r)
			return
		}

		// Generate random state (CSRF) and nonce.
		state := randomHex(16)
		nonce := randomHex(16)

		authURL := deps.OIDCClient.AuthCodeURL(state, nonce)

		// Validate return_url to prevent open redirect attacks.
		// Must be a relative path (starts with /, no //) or falls back to /.
		returnURL := r.URL.Query().Get("return_url")
		if !strings.HasPrefix(returnURL, "/") || strings.HasPrefix(returnURL, "//") {
			returnURL = "/"
		}

		// Store state + nonce + return_url in a short-lived signed cookie.
		statePayload := oidcStatePayload{
			CSRFToken: state,
			Nonce:     nonce,
			ReturnURL: returnURL,
		}
		stateCookie, err := buildStateCookie(&statePayload, deps.SigningKey, deps.Config)
		if err != nil {
			slog.Error("failed to build state cookie", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Add("Set-Cookie", stateCookie)
		http.Redirect(w, r, authURL, http.StatusFound)
	}
}
```

**OIDC state cookie:** the `state` (CSRF), `nonce`, and `return_url` must
survive the redirect to the IdP and back. They are stored in a short-lived
(5 minute), signed, HttpOnly cookie named `blockyard_oidc_state`. The
`Secure` flag is set via `secureFlag()` (same as the session cookie). The
cookie is deleted in the callback handler.

```go
type oidcStatePayload struct {
	CSRFToken string `json:"csrf_token"`
	Nonce     string `json:"nonce"`
	ReturnURL string `json:"return_url"`
}

func buildStateCookie(payload *oidcStatePayload, key *SigningKey, cfg *config.Config) (string, error) {
	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(jsonBytes)

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	value := encoded + "." + sig
	secure := secureFlag(cfg)
	return fmt.Sprintf(
		"blockyard_oidc_state=%s; Path=/; HttpOnly; SameSite=Lax%s; Max-Age=300",
		value, secure,
	), nil
}

func extractStateCookie(r *http.Request, key *SigningKey) (*oidcStatePayload, error) {
	cookie, err := r.Cookie("blockyard_oidc_state")
	if err != nil {
		return nil, fmt.Errorf("missing oidc state cookie: %w", err)
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed state cookie")
	}

	jsonBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode state cookie: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode state signature: %w", err)
	}

	mac := hmac.New(sha256.New, key.key)
	mac.Write(jsonBytes)
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return nil, errors.New("invalid state cookie signature")
	}

	var payload oidcStatePayload
	if err := json.Unmarshal(jsonBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal state cookie: %w", err)
	}
	return &payload, nil
}
```

**`GET /callback`:**

```go
// CallbackHandler handles the IdP callback after user authentication.
// Exchanges the authorization code for tokens, validates the ID token,
// extracts user identity, stores session server-side, and sets a
// signed session cookie.
func CallbackHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.OIDCClient == nil {
			http.NotFound(w, r)
			return
		}

		// 1. Extract and validate OIDC state cookie.
		statePayload, err := extractStateCookie(r, deps.SigningKey)
		if err != nil {
			slog.Error("invalid state cookie", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// 2. Verify CSRF token matches.
		if r.URL.Query().Get("state") != statePayload.CSRFToken {
			http.Error(w, "CSRF token mismatch", http.StatusBadRequest)
			return
		}

		// 3. Exchange authorization code for tokens.
		code := r.URL.Query().Get("code")
		oauth2Token, _, allClaims, err := deps.OIDCClient.Exchange(r.Context(), code)
		if err != nil {
			slog.Error("token exchange failed", "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		// 4. Extract sub and groups.
		var subClaim string
		if raw, ok := allClaims["sub"]; ok {
			_ = json.Unmarshal(raw, &subClaim)
		}
		if subClaim == "" {
			slog.Error("ID token missing sub claim")
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		groups := deps.OIDCClient.ExtractGroups(allClaims)

		// 5. Store session server-side.
		expiresAt := nowUnix() + 300 // default 5 min
		if !oauth2Token.Expiry.IsZero() {
			expiresAt = oauth2Token.Expiry.Unix()
		}

		deps.UserSessions.Set(subClaim, &UserSession{
			Groups:       groups,
			AccessToken:  oauth2Token.AccessToken,
			RefreshToken: oauth2Token.RefreshToken,
			ExpiresAt:    expiresAt,
		})

		// 6. Build signed session cookie.
		cookiePayload := &CookiePayload{
			Sub:      subClaim,
			IssuedAt: nowUnix(),
		}
		cookieValue, err := cookiePayload.Encode(deps.SigningKey)
		if err != nil {
			slog.Error("failed to encode session cookie", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		cookieMaxAge := int64(24 * 60 * 60) // 24h default
		if deps.Config.OIDC != nil {
			cookieMaxAge = int64(deps.Config.OIDC.CookieMaxAge.Duration.Seconds())
		}

		secure := secureFlag(deps.Config)
		sessionCookie := fmt.Sprintf(
			"blockyard_session=%s; Path=/; HttpOnly; SameSite=Lax%s; Max-Age=%d",
			cookieValue, secure, cookieMaxAge,
		)

		// 7. Clear the OIDC state cookie.
		clearState := fmt.Sprintf(
			"blockyard_oidc_state=; Path=/; HttpOnly%s; Max-Age=0", secure,
		)

		// 8. Redirect to return_url.
		w.Header().Add("Set-Cookie", sessionCookie)
		w.Header().Add("Set-Cookie", clearState)
		http.Redirect(w, r, statePayload.ReturnURL, http.StatusFound)
	}
}
```

**`POST /logout`:**

```go
// LogoutHandler clears the session cookie and removes the server-side
// session. Redirects to / (or to the IdP's end_session_endpoint if
// available).
func LogoutHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Remove server-side session if cookie is valid.
		if deps.SigningKey != nil && deps.UserSessions != nil {
			if cookieValue := extractSessionCookie(r); cookieValue != "" {
				if payload, err := DecodeCookie(cookieValue, deps.SigningKey); err == nil {
					deps.UserSessions.Delete(payload.Sub)
				}
			}
		}

		secure := secureFlag(deps.Config)
		clearCookie := fmt.Sprintf(
			"blockyard_session=; Path=/; HttpOnly%s; Max-Age=0", secure,
		)
		w.Header().Set("Set-Cookie", clearCookie)

		// Redirect to IdP logout if available, otherwise to /.
		if deps.OIDCClient != nil {
			if endSession := deps.OIDCClient.EndSessionEndpoint(); endSession != "" {
				http.Redirect(w, r, endSession, http.StatusFound)
				return
			}
		}

		http.Redirect(w, r, "/", http.StatusFound)
	}
}
```

**Tests:**

- `/login` without OIDC configured returns 404
- `/login` with OIDC configured returns 302 to IdP authorize URL
- `/login?return_url=/app/foo/` encodes return URL in state cookie
- `/login?return_url=https://evil.com/` falls back to `/` (open redirect prevention)
- `/login?return_url=//evil.com` falls back to `/` (protocol-relative prevention)
- `/callback` with mismatched CSRF token returns 400
- `/callback` stores session in `UserSessionStore`
- `/logout` removes session from `UserSessionStore`
- `/logout` clears the session cookie

### Step 6: App-plane auth middleware

Insert a `chi` middleware into the proxy router that:
1. Extracts the `blockyard_session` cookie
2. Verifies the HMAC signature
3. Looks up the server-side session by `sub`
4. Checks access token expiry — refreshes if needed
5. Stores `AuthenticatedUser` in the request context

`internal/auth/middleware.go`:

```go
package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// AuthenticatedUser represents a validated user identity extracted
// from a session. Stored in the request context by the auth middleware.
type AuthenticatedUser struct {
	Sub         string
	Groups      []string
	AccessToken string
}

// contextKey is an unexported type for context keys in this package.
type contextKey struct{}

// userKey is the context key for AuthenticatedUser.
var userKey = contextKey{}

// UserFromContext retrieves the AuthenticatedUser from the request
// context, or nil if not present.
func UserFromContext(ctx context.Context) *AuthenticatedUser {
	u, _ := ctx.Value(userKey).(*AuthenticatedUser)
	return u
}

// RequireAuth returns a chi middleware that protects routes behind
// OIDC authentication. Unauthenticated requests are redirected to
// /login with the current URL as return_url.
//
// When OIDC is not configured (v0 compat), the middleware passes
// all requests through unchanged.
func RequireAuth(deps *Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// If OIDC is not configured, pass through (v0 compat).
			if deps.SigningKey == nil {
				next.ServeHTTP(w, r)
				return
			}

			// Extract session cookie.
			cookieValue := extractSessionCookie(r)
			if cookieValue == "" {
				redirectToLogin(w, r)
				return
			}

			// Decode and verify signature.
			cookie, err := DecodeCookie(cookieValue, deps.SigningKey)
			if err != nil {
				redirectToLogin(w, r)
				return
			}

			// Check cookie max-age.
			maxAge := int64(24 * 60 * 60)
			if deps.Config.OIDC != nil {
				maxAge = int64(deps.Config.OIDC.CookieMaxAge.Duration.Seconds())
			}
			if nowUnix()-cookie.IssuedAt > maxAge {
				redirectToLogin(w, r)
				return
			}

			// Look up server-side session.
			session := deps.UserSessions.Get(cookie.Sub)
			if session == nil {
				redirectToLogin(w, r)
				return
			}

			// Refresh access token if near expiry (within 60 seconds).
			// Uses a per-user lock to prevent concurrent refresh attempts.
			if session.ExpiresAt-nowUnix() < 60 {
				lock := deps.UserSessions.RefreshLock(cookie.Sub)
				lock.Lock()

				// Re-check after acquiring the lock — another request
				// may have already refreshed while we waited.
				session = deps.UserSessions.Get(cookie.Sub)
				needsRefresh := session == nil || session.ExpiresAt-nowUnix() < 60

				if needsRefresh {
					if err := refreshAccessToken(r.Context(), deps, cookie.Sub); err != nil {
						lock.Unlock()
						slog.Error("token refresh failed, removing session",
							"sub", cookie.Sub, "error", err)
						deps.UserSessions.Delete(cookie.Sub)
						redirectToLogin(w, r)
						return
					}
				}
				lock.Unlock()

				// Re-read session after refresh.
				session = deps.UserSessions.Get(cookie.Sub)
				if session == nil {
					redirectToLogin(w, r)
					return
				}
			}

			// Store authenticated user in context.
			user := &AuthenticatedUser{
				Sub:         cookie.Sub,
				Groups:      session.Groups,
				AccessToken: session.AccessToken,
			}
			ctx := context.WithValue(r.Context(), userKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractSessionCookie reads the blockyard_session cookie value from
// the request. Returns empty string if not found.
func extractSessionCookie(r *http.Request) string {
	for _, c := range r.Cookies() {
		if c.Name == "blockyard_session" {
			return c.Value
		}
	}
	return ""
}

// redirectToLogin sends a 302 redirect to /login with the current URL
// as return_url.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	returnURL := r.URL.RequestURI()
	http.Redirect(w, r, "/login?return_url="+url.QueryEscape(returnURL), http.StatusFound)
}

// refreshAccessToken exchanges the refresh token for a new access
// token via the IdP's token endpoint and updates the server-side
// session in place.
func refreshAccessToken(ctx context.Context, deps *Deps, sub string) error {
	if deps.OIDCClient == nil {
		return fmt.Errorf("OIDC not configured")
	}

	session := deps.UserSessions.Get(sub)
	if session == nil {
		return fmt.Errorf("session not found for sub %q", sub)
	}

	newToken, err := deps.OIDCClient.RefreshToken(ctx, session.RefreshToken)
	if err != nil {
		return err
	}

	newExpiresAt := nowUnix() + 300
	if !newToken.Expiry.IsZero() {
		newExpiresAt = newToken.Expiry.Unix()
	}

	var newRefresh *string
	if newToken.RefreshToken != "" {
		newRefresh = &newToken.RefreshToken
	}

	deps.UserSessions.UpdateTokens(sub, newToken.AccessToken, newRefresh, newExpiresAt)
	return nil
}
```

Note that token refresh no longer requires re-encoding the cookie or setting
a new `Set-Cookie` header — the cookie is unchanged (same `sub` and
`issued_at`), and the refreshed tokens are written directly to the
server-side store. This eliminates the "set cookie on response but also need
to insert context value before ServeHTTP" ordering issue from the original
design.

### Step 7: Router integration

Wire the auth middleware and handlers into the `chi` router:

```go
func buildRouter(srv *server.Server) chi.Router {
	r := chi.NewRouter()

	authDeps := srv.AuthDeps()

	// API routes (bearer-token auth, outside app-plane auth).
	r.Mount("/api/v1", api.Router(srv))

	// Auth endpoints (outside app-plane auth layer).
	r.Get("/login", auth.LoginHandler(authDeps))
	r.Get("/callback", auth.CallbackHandler(authDeps))
	r.Post("/logout", auth.LogoutHandler(authDeps))

	// Health check (unauthenticated).
	r.Get("/healthz", healthHandler)

	// Proxy routes with app-plane auth middleware.
	r.Route("/app", func(sub chi.Router) {
		sub.Use(auth.RequireAuth(authDeps))
		sub.Get("/{name}", trailingSlashRedirect)
		sub.HandleFunc("/{name}/", proxyHandler(srv))
		sub.HandleFunc("/{name}/*", proxyHandler(srv))
	})

	return r
}
```

**Key point:** `/login`, `/callback`, `/logout`, `/healthz`, and `/api/v1/*`
are outside the app-plane auth layer. The auth middleware only applies to
`/app/` routes.

### Step 8: Package structure

The `internal/auth/` package layout:

```
internal/
└── auth/
    ├── oidc.go        # OIDCClient, Discover, Exchange, ExtractGroups
    ├── session.go     # SigningKey, CookiePayload, UserSession,
    │                  # UserSessionStore
    ├── middleware.go   # RequireAuth, AuthenticatedUser, UserFromContext
    └── handlers.go    # Deps, LoginHandler, CallbackHandler, LogoutHandler
```

### Step 9: Test infrastructure — Mock IdP

Integration tests need a mock OIDC provider. Implemented as a test helper
using `net/http/httptest.Server`, `crypto/rsa`, and
`github.com/go-jose/go-jose/v4` for signing test JWTs.

`internal/auth/testutil_test.go` (or `internal/testutil/mockidp.go` if
shared across packages):

```go
import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// MockIdP is a minimal OIDC-compliant mock identity provider for
// integration tests. Serves:
//   GET  /.well-known/openid-configuration
//   GET  /jwks
//   POST /token
//   GET  /authorize  (redirects back with code)
type MockIdP struct {
	Server     *httptest.Server
	signingKey *rsa.PrivateKey
	keyID      string
}

// NewMockIdP starts a mock IdP on a random port.
func NewMockIdP() *MockIdP {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "test-key-1"

	m := &MockIdP{
		signingKey: key,
		keyID:      kid,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("GET /jwks", m.handleJWKS)
	mux.HandleFunc("POST /token", m.handleToken)
	mux.HandleFunc("GET /authorize", m.handleAuthorize)

	m.Server = httptest.NewServer(mux)
	return m
}

func (m *MockIdP) IssuerURL() string { return m.Server.URL }

func (m *MockIdP) Close() { m.Server.Close() }

func (m *MockIdP) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	base := m.Server.URL
	doc := map[string]any{
		"issuer":                 base,
		"authorization_endpoint": base + "/authorize",
		"token_endpoint":         base + "/token",
		"jwks_uri":               base + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"subject_types_supported":               []string{"public"},
		"response_types_supported":              []string{"code"},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func (m *MockIdP) handleJWKS(w http.ResponseWriter, r *http.Request) {
	jwk := jose.JSONWebKey{
		Key:       &m.signingKey.PublicKey,
		KeyID:     m.keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(set)
}

// IssueIDToken creates a signed JWT with the given sub and groups.
func (m *MockIdP) IssueIDToken(sub string, groups []string, audience string, nonce string) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: m.signingKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", m.keyID),
	)
	if err != nil {
		return "", err
	}

	now := time.Now()
	claims := map[string]any{
		"iss":    m.Server.URL,
		"sub":    sub,
		"aud":    audience,
		"exp":    now.Add(1 * time.Hour).Unix(),
		"iat":    now.Unix(),
		"nonce":  nonce,
		"groups": groups,
	}

	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", err
	}
	return raw, nil
}
```

Add `go-jose` to dev/test dependencies (used only in test files):

```
require github.com/go-jose/go-jose/v4 v4.x.x
```

The mock IdP's `/token` endpoint accepts authorization codes and returns
pre-configured token responses (including the signed ID token). The
`/authorize` endpoint redirects back to the callback URL with a code
parameter.

### Step 10: Integration tests

`internal/auth/auth_test.go`:

Tests build an `auth.Deps` directly (no `server.Server` needed) and wire
it into a test router via `buildTestRouter(deps)`. This mirrors the real
router's use of `srv.AuthDeps()` without pulling in the full server
dependency tree.

```go
func TestLoginRedirectsToIdP(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, idp.IssuerURL()) {
		t.Errorf("expected redirect to IdP, got %s", location)
	}
}

func TestFullAuthFlow(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	// 1. GET /login → 302 to IdP
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}

	// 2. Simulate IdP redirect back to /callback with code + state.
	//    Extract CSRF token from the redirect URL's state parameter.
	stateCookie := findCookie(w.Result(), "blockyard_oidc_state")
	csrfToken := extractStateParam(w.Header().Get("Location"))

	callbackURL := fmt.Sprintf("/callback?code=test-code&state=%s", csrfToken)
	req = httptest.NewRequest("GET", callbackURL, nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", w.Code)
	}

	// 3. Verify session exists server-side.
	session := deps.UserSessions.Get("test-sub")
	if session == nil {
		t.Fatal("expected server-side session to exist")
	}

	// 4. Access /app/my-app/ with session cookie — should succeed.
	sessionCookie := findCookie(w.Result(), "blockyard_session")
	req = httptest.NewRequest("GET", "/app/my-app/page", nil)
	req.AddCookie(sessionCookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should not redirect to login (2xx or proxy-related error, not 302).
	if w.Code == http.StatusFound {
		t.Error("authenticated request should not redirect to login")
	}
}

func TestUnauthenticatedProxyRedirectsToLogin(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()

	deps := buildTestDeps(t, idp)
	router := buildTestRouter(deps)

	req := httptest.NewRequest("GET", "/app/my-app/page", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, "/login") {
		t.Errorf("expected redirect to /login, got %s", location)
	}
}

func TestLogoutRemovesSession(t *testing.T) {
	// ... authenticate first, then POST /logout ...
	// Verify session is removed from UserSessionStore.
	session := deps.UserSessions.Get("test-sub")
	if session != nil {
		t.Error("expected session to be removed after logout")
	}
}

func TestNoOIDCConfigPassesThrough(t *testing.T) {
	// v0 compatibility: without [oidc] config, proxy routes are unprotected.
	deps := &auth.Deps{Config: &config.Config{}}
	router := buildTestRouter(deps)

	req := httptest.NewRequest("GET", "/app/my-app/page", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Should not redirect to login.
	if w.Code == http.StatusFound {
		loc := w.Header().Get("Location")
		if strings.HasPrefix(loc, "/login") {
			t.Error("v0 compat: request should not redirect to login without OIDC")
		}
	}
}
```

## Changes to existing v0 proxy session handling

v0's proxy already sets a `blockyard_session` cookie for session-to-worker
routing. v1's auth cookie uses the same name. This is intentional —
the session cookie now carries identity and is used for session-to-worker
pinning:

1. **Auth middleware** reads the cookie, verifies the signature, looks up
   `sub` in the server-side `UserSessionStore`, and stores
   `AuthenticatedUser` in `context.Context`
2. **Proxy handler** uses the `sub` from the cookie (or a hash of it) for
   worker pinning via `session.Store`

The v0 cookie format (plain UUID) is incompatible with the v1 format (signed
payload). When OIDC is enabled, the v0 cookie format is no longer used.
When OIDC is disabled (v0 compat mode), the plain UUID cookie continues to
work unchanged.

**Migration concern:** none. v0 has no production deployments with persistent
sessions. The switch from plain UUID to signed payload is a clean break.

## File summary

```
internal/
├── auth/
│   ├── oidc.go            # OIDCClient, Discover, Exchange, ExtractGroups
│   ├── session.go         # SigningKey, CookiePayload, UserSession,
│   │                      # UserSessionStore
│   ├── middleware.go       # RequireAuth, AuthenticatedUser,
│   │                      # UserFromContext, refreshAccessToken
│   └── handlers.go        # Deps, LoginHandler, CallbackHandler,
│                          # LogoutHandler, oidcStatePayload, cookie helpers
├── config/
│   ├── config.go          # + OidcConfig, SessionSecret, ExternalURL,
│   │                      # validation
│   └── secret.go          # Secret type (new file)
├── server/
│   └── state.go           # + OIDCClient, SigningKey, UserSessions fields,
│                          # AuthDeps() method
cmd/
└── blockyard/
    └── main.go            # + OIDC initialization, auth route registration
```

## Exit criteria

Phase 1-1 is done when:

- `go build ./...` succeeds with and without `[oidc]` config
- Config tests pass: OIDC parsing, validation, env var coverage
- Session cookie tests pass: sign/verify round-trip, tamper rejection
- `UserSessionStore` tests pass: `Set`, `Get`, `Delete`, `UpdateTokens`
- Mock IdP test infrastructure works
- Integration tests pass:
  - Login redirects to IdP authorize URL
  - Full auth flow: login -> callback -> session cookie -> server-side
    session -> authenticated access
  - Unauthenticated proxy request redirects to `/login`
  - Token refresh updates server-side session (no cookie change)
  - Logout removes server-side session and clears cookie
  - No-OIDC mode: proxy passes through without auth (v0 compat)
- Existing v0 tests continue to pass (no regression)
- `TestEnvVarCoverageComplete` passes with new fields
