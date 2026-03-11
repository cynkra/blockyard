# Phase 1-1: OIDC Authentication + User Sessions

Establish user identity on the app plane. This is the foundation for v1 —
RBAC (phase 1-2), identity injection (phase 1-3), and OpenBao integration
(phase 1-3) all require a logged-in user. The control plane API continues to
use the v0 static bearer token; JWT-based machine auth replaces it in phase
1-2.

This phase adds the `auth/` module, three public HTTP endpoints (`/login`,
`/callback`, `/logout`), a session cookie + server-side session store, and an
auth middleware that protects all `/app/` proxy routes.

## Design decision: server-side session store

The cookie carries only user identity (`sub`, `issued_at`) and an HMAC
signature (~100-150 bytes). All sensitive/bulky data — groups, access token,
refresh token — lives server-side in a `DashMap<String, UserSession>` keyed
by `sub`.

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
5. Server-side session store — `DashMap<String, UserSession>` keyed by `sub`
6. Signed session cookie — HMAC-SHA256 signed, carries only `sub` +
   `issued_at`
7. Transparent access token refresh middleware
8. App-plane auth middleware — protect `/app/` routes, redirect to `/login`
9. New dependencies: `openidconnect`, `hmac`, `sha2`, `base64`

## Step-by-step

### Step 1: New dependencies

Add to `Cargo.toml`:

```toml
# OIDC / JWT
openidconnect   = "4"

# Session cookies
hmac            = "0.12"
sha2            = "0.10"

# Encoding
base64          = "0.22"
```

**Dependency rationale:**

- **openidconnect** — full OIDC RP implementation. Handles discovery, JWKS,
  authorization URL generation, and token exchange. Built on the `oauth2`
  crate.
- **hmac + sha2** — HMAC-SHA256 for cookie signing key derivation from
  `session_secret`.
- **base64** — encoding the cookie payload for transport.

**Dependencies removed vs. original design:**

- **aes-gcm** — no longer needed. Refresh tokens are stored server-side, not
  encrypted in the cookie.
- **cookie** — manual cookie parsing (already used in v0 proxy code) is
  sufficient. The `signed` feature is replaced by our own HMAC signing.
- **rand** — no longer needed for AES nonce generation.
- **jsonwebtoken** — not needed in phase 1-1. The `openidconnect` crate
  handles ID token validation internally. `jsonwebtoken` will be added in
  phase 1-2 for control-plane JWT auth.

### Step 2: `Secret` newtype + config additions

**`Secret` newtype** — prevents secret leakage in debug logs, `Display`
output, and accidental serialization. Added to `config.rs` (or a small
`secret.rs` module):

```rust
/// Wraps a secret string. Debug and Display print "[REDACTED]".
/// Does not implement Serialize — structs containing secrets cannot
/// be accidentally serialized without explicit handling.
#[derive(Clone, Deserialize)]
#[serde(transparent)]
pub struct Secret(String);

impl Secret {
    pub fn expose(&self) -> &str { &self.0 }

    pub fn is_empty(&self) -> bool { self.0.is_empty() }
}

impl From<&str> for Secret {
    fn from(s: &str) -> Self { Self(s.to_string()) }
}

impl std::fmt::Debug for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("[REDACTED]")
    }
}

impl std::fmt::Display for Secret {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("[REDACTED]")
    }
}
```

**Existing field migrated:** `ServerConfig::token` changes from `String` to
`Secret`. This is a pre-existing leak (v0 derives `Debug` on a struct
containing the bearer token) — fix it now since we're touching config anyway.

**New structs:**

```rust
#[derive(Clone, Deserialize)]
pub struct OidcConfig {
    pub issuer_url: String,
    pub client_id: String,
    pub client_secret: Secret,
    #[serde(default = "default_groups_claim")]
    pub groups_claim: String,
    #[serde(default = "default_cookie_max_age", with = "humantime_serde")]
    pub cookie_max_age: Duration,
}

fn default_groups_claim() -> String { "groups".into() }
fn default_cookie_max_age() -> Duration { Duration::from_secs(86400) } // 24h
```

**Changes to existing structs:**

```rust
pub struct Config {
    // ... existing fields ...
    #[serde(default)]
    pub oidc: Option<OidcConfig>,
}

pub struct ServerConfig {
    // ... existing fields ...
    pub token: Secret,                      // changed from String
    #[serde(default)]
    pub session_secret: Option<Secret>,
    #[serde(default)]
    pub external_url: Option<String>,
}
```

`session_secret` is `Option<Secret>` — required when `[oidc]` is configured,
ignored otherwise. Validation enforces this:

```rust
fn validate(&self) -> Result<(), ConfigError> {
    // ... existing checks ...
    if let Some(oidc) = &self.oidc {
        if oidc.issuer_url.is_empty() {
            return Err(ConfigError::Validation("oidc.issuer_url must not be empty".into()));
        }
        if oidc.client_id.is_empty() {
            return Err(ConfigError::Validation("oidc.client_id must not be empty".into()));
        }
        if oidc.client_secret.is_empty() {
            return Err(ConfigError::Validation("oidc.client_secret must not be empty".into()));
        }
        let secret_missing = self.server.session_secret.as_ref()
            .map_or(true, |s| s.is_empty());
        if secret_missing {
            return Err(ConfigError::Validation(
                "server.session_secret is required when [oidc] is configured".into()
            ));
        }
    }
    Ok(())
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
plan): if any `BLOCKYARD_OIDC_*` env var is set and `self.oidc` is `None`,
auto-construct a default `OidcConfig`. Required fields start as empty strings
and are caught by `validate()`.

```rust
// In apply_env_overrides(), before applying individual OIDC overrides:
if self.oidc.is_none() && env_prefix_exists("BLOCKYARD_OIDC_") {
    self.oidc = Some(OidcConfig {
        issuer_url: String::new(),
        client_id: String::new(),
        client_secret: Secret::from(""),
        groups_claim: default_groups_claim(),
        cookie_max_age: default_cookie_max_age(),
    });
}
```

**Tests:**

- `Secret`: Debug and Display output `[REDACTED]`, `expose()` returns value
- `Secret`: deserializes transparently from string
- Parse config with `[oidc]` section present
- Parse config without `[oidc]` section (backward compat)
- Validation: reject empty `issuer_url`, `client_id`, `client_secret`
- Validation: reject `[oidc]` without `session_secret`
- Env var override for each OIDC field
- Auto-construction: set `BLOCKYARD_OIDC_ISSUER_URL` without `[oidc]` in
  TOML, verify section is created
- `env_var_coverage_complete` test passes with new fields

### Step 3: OIDC discovery

`src/auth/mod.rs` — module root, shared types.

```rust
pub mod oidc;
pub mod session;

/// Represents a validated user identity extracted from a session.
/// Inserted into axum request extensions by the auth middleware.
#[derive(Debug, Clone)]
pub struct AuthenticatedUser {
    pub sub: String,
    pub groups: Vec<String>,
    pub access_token: String,
}
```

`src/auth/oidc.rs` — OIDC discovery client and custom claims.

```rust
use openidconnect::{
    ClientId, ClientSecret, IssuerUrl, RedirectUrl,
    core::CoreProviderMetadata,
};
use std::sync::Arc;

/// Custom additional claims type that captures arbitrary extra fields
/// from the ID token (e.g. "groups", "roles", "cognito:groups").
/// `EmptyAdditionalClaims` discards these — we need them for group
/// extraction.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GroupsClaims {
    #[serde(flatten)]
    pub extra: HashMap<String, serde_json::Value>,
}

impl openidconnect::AdditionalClaims for GroupsClaims {}

/// Client type alias using our custom claims instead of EmptyAdditionalClaims.
pub type BlockyardClient = openidconnect::Client<
    GroupsClaims,
    openidconnect::core::CoreAuthDisplay,
    openidconnect::core::CoreGenderClaim,
    openidconnect::core::CoreJweContentEncryptionAlgorithm,
    openidconnect::core::CoreJsonWebKey,
    openidconnect::core::CoreJwsSigningAlgorithm,
    openidconnect::core::CoreJsonWebKeyType,
    openidconnect::core::CoreJsonWebKeyUse,
    openidconnect::core::CoreRevocableToken,
    openidconnect::core::CoreRevocationErrorResponse,
    openidconnect::core::CoreTokenIntrospectionResponse,
    openidconnect::core::CoreTokenType,
>;

/// OIDC client initialized from provider discovery.
/// Holds the discovered metadata for token validation.
pub struct OidcClient {
    pub client: BlockyardClient,
    pub provider_metadata: CoreProviderMetadata,
    /// Groups claim name (e.g. "groups", "roles", "cognito:groups")
    pub groups_claim: String,
}

impl OidcClient {
    /// Discover the OIDC provider and build the client.
    /// Called once at server startup.
    pub async fn discover(
        issuer_url: &str,
        client_id: &str,
        client_secret: &str,
        redirect_url: &str,
        groups_claim: &str,
    ) -> Result<Self, OidcError> {
        let issuer = IssuerUrl::new(issuer_url.to_string())?;
        let metadata = CoreProviderMetadata::discover_async(
            issuer,
            openidconnect::reqwest::async_http_client,
        ).await?;

        let client = CoreClient::from_provider_metadata(
            metadata.clone(),
            ClientId::new(client_id.to_string()),
            Some(ClientSecret::new(client_secret.to_string())),
        )
        .set_redirect_uri(RedirectUrl::new(redirect_url.to_string())?);

        Ok(Self {
            client,
            provider_metadata: metadata,
            groups_claim: groups_claim.to_string(),
        })
    }
}
```

**Note on `openidconnect` v4 API:** the code snippets below use
`openidconnect::reqwest::async_http_client` for discovery, token exchange,
and refresh. This was the v3 API — v4 changed the async HTTP client
interface (you pass a `reqwest::Client` instance directly). The exact
call signatures will be adjusted during implementation to match the v4 API.

**JWKS refresh:** the `openidconnect` crate fetches JWKS during discovery and
during token validation. For phase 1-1, rely on the crate's built-in JWKS
fetching during ID token verification in the callback handler. A background
JWKS refresh loop (periodic re-discovery) can be added as a follow-up if
needed — in practice, JWKS rotation is rare and the crate handles it during
token validation.

**AppState changes:**

```rust
pub struct AppState<B: Backend> {
    // ... existing fields ...
    pub oidc_client: Option<Arc<OidcClient>>,
    pub signing_key: Option<Arc<SigningKey>>,
    pub user_sessions: Option<Arc<UserSessionStore>>,
}
```

All three fields are `Option` — `None` when OIDC is not configured (v0
compatibility).

**Initialization in `main.rs`:**

```rust
let (oidc_client, signing_key, user_sessions) = if let Some(oidc_config) = &config.oidc {
    let base_url = config.server.external_url.as_deref()
        .unwrap_or(&format!("http://{}", config.server.bind));
    let redirect_url = format!("{base_url}/callback");
    let client = OidcClient::discover(
        &oidc_config.issuer_url,
        &oidc_config.client_id,
        oidc_config.client_secret.expose(),
        &redirect_url,
        &oidc_config.groups_claim,
    ).await?;
    let key = SigningKey::derive(
        config.server.session_secret.as_ref().unwrap().expose()
    );
    (
        Some(Arc::new(client)),
        Some(Arc::new(key)),
        Some(Arc::new(UserSessionStore::new())),
    )
} else {
    (None, None, None)
};
```

**Note on redirect URL:** the redirect URL is constructed from
`config.server.external_url` if set, otherwise falls back to
`config.server.bind` (with `http://`). Production deployments behind a
reverse proxy should set `external_url` to their public HTTPS URL.

### Step 4: Session cookie signing

`src/auth/session.rs` — session cookie signing and server-side session store.

```rust
use hmac::{Hmac, Mac};
use sha2::Sha256;

/// HMAC signing key derived from session_secret.
pub struct SigningKey {
    key: Vec<u8>,
}

impl SigningKey {
    /// Derive the signing key from a secret using HMAC with a domain
    /// separation string.
    pub fn derive(secret: &str) -> Self {
        let mut mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes())
            .expect("HMAC accepts any key length");
        mac.update(b"blockyard-cookie-signing");
        Self {
            key: mac.finalize().into_bytes().to_vec(),
        }
    }
}
```

**Session cookie payload:**

```rust
/// Minimal payload encoded into the session cookie.
/// Signed with HMAC-SHA256. All other session data is server-side.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CookiePayload {
    /// IdP subject identifier
    pub sub: String,
    /// Cookie issue time (unix timestamp)
    pub issued_at: i64,
}

impl CookiePayload {
    /// Encode the payload into a signed cookie value.
    /// Format: base64(json) + "." + base64(hmac)
    pub fn encode(&self, key: &SigningKey) -> Result<String, SessionError> {
        let json = serde_json::to_string(self)?;
        let mut mac = Hmac::<Sha256>::new_from_slice(&key.key)?;
        mac.update(json.as_bytes());
        let signature = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .encode(mac.finalize().into_bytes());
        let payload = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .encode(json.as_bytes());
        Ok(format!("{payload}.{signature}"))
    }

    /// Decode and verify a signed cookie value.
    pub fn decode(value: &str, key: &SigningKey) -> Result<Self, SessionError> {
        let (payload_b64, sig_b64) = value.split_once('.')
            .ok_or(SessionError::MalformedCookie)?;

        let payload_bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(payload_b64)?;
        let signature = base64::engine::general_purpose::URL_SAFE_NO_PAD
            .decode(sig_b64)?;

        // Verify signature
        let mut mac = Hmac::<Sha256>::new_from_slice(&key.key)?;
        mac.update(&payload_bytes);
        mac.verify_slice(&signature)
            .map_err(|_| SessionError::InvalidSignature)?;

        let payload: CookiePayload = serde_json::from_slice(&payload_bytes)?;
        Ok(payload)
    }
}
```

**Server-side session store:**

```rust
use dashmap::DashMap;

/// Per-user session data stored server-side. Keyed by `sub`.
#[derive(Debug, Clone)]
pub struct UserSession {
    /// Group memberships from IdP groups claim
    pub groups: Vec<String>,
    /// IdP access token (short-lived, refreshed transparently)
    pub access_token: String,
    /// IdP refresh token (long-lived)
    pub refresh_token: String,
    /// Access token expiry (unix timestamp)
    pub expires_at: i64,
}

/// In-memory session store. Wraps a DashMap keyed by user `sub`.
/// Same pattern as SessionStore, WorkerRegistry, and TaskStore.
pub struct UserSessionStore {
    sessions: DashMap<String, UserSession>,
    /// Per-user refresh locks. Prevents concurrent token refreshes for
    /// the same user — important because some IdPs (e.g. Keycloak with
    /// refresh token rotation) invalidate the refresh token on first use.
    /// Without this, concurrent requests during token expiry would race
    /// to refresh, and all but the first would fail.
    refresh_locks: DashMap<String, Arc<tokio::sync::Mutex<()>>>,
}

impl UserSessionStore {
    pub fn new() -> Self {
        Self {
            sessions: DashMap::new(),
            refresh_locks: DashMap::new(),
        }
    }

    /// Get or create a per-user refresh lock.
    pub fn refresh_lock(&self, sub: &str) -> Arc<tokio::sync::Mutex<()>> {
        self.refresh_locks
            .entry(sub.to_string())
            .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(())))
            .clone()
    }

    /// Insert or replace the session for a user.
    pub fn insert(&self, sub: String, session: UserSession) {
        self.sessions.insert(sub, session);
    }

    /// Look up a user's session by sub.
    pub fn get(&self, sub: &str) -> Option<dashmap::mapref::one::Ref<'_, String, UserSession>> {
        self.sessions.get(sub)
    }

    /// Remove a user's session (on logout).
    pub fn remove(&self, sub: &str) -> Option<(String, UserSession)> {
        self.refresh_locks.remove(sub);
        self.sessions.remove(sub)
    }

    /// Update tokens after a refresh. Returns false if session not found.
    pub fn update_tokens(
        &self,
        sub: &str,
        access_token: String,
        refresh_token: Option<String>,
        expires_at: i64,
    ) -> bool {
        if let Some(mut session) = self.sessions.get_mut(sub) {
            session.access_token = access_token;
            if let Some(rt) = refresh_token {
                session.refresh_token = rt;
            }
            session.expires_at = expires_at;
            true
        } else {
            false
        }
    }
}
```

**Tests:**

- Cookie round-trip: encode → decode produces identical payload
- Tampered cookie (modified payload) is rejected
- Tampered cookie (modified signature) is rejected
- Malformed cookie value (no dot, empty segments) returns error
- UserSessionStore: insert, get, remove, update_tokens

### Step 5: Authorization code flow endpoints

Three new routes added to the full router:

```rust
pub fn full_router<B: Backend + Clone>(state: AppState<B>) -> Router {
    let api = crate::api::api_router(state.clone());

    let mut router = Router::new()
        .merge(api)
        // Auth endpoints (unauthenticated)
        .route("/login", axum::routing::get(login_handler::<B>))
        .route("/callback", axum::routing::get(callback_handler::<B>))
        .route("/logout", axum::routing::post(logout_handler::<B>))
        // Proxy routes
        .route("/app/{name}", axum::routing::get(trailing_slash_redirect))
        .route("/app/{name}/", axum::routing::any(proxy_handler_root::<B>))
        .route("/app/{name}/{*rest}", axum::routing::any(proxy_handler::<B>))
        .with_state(state);

    router
}
```

**Cookie security flags helper:**

```rust
/// Unix timestamp (seconds since epoch). Avoids a `chrono` dependency.
fn now_unix() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs() as i64
}

/// Returns "; Secure" if external_url is HTTPS, empty string otherwise.
/// Used by all cookie-setting code paths.
fn secure_flag(config: &Config) -> &'static str {
    let is_https = config.server.external_url.as_deref()
        .map_or(false, |u| u.starts_with("https://"));
    if is_https { "; Secure" } else { "" }
}
```

**`GET /login`:**

```rust
/// Initiates the OIDC authorization code flow.
/// Query params: ?return_url=/app/my-app/ (optional, default: /)
async fn login_handler<B: Backend>(
    State(state): State<AppState<B>>,
    Query(params): Query<LoginParams>,
) -> Result<Response, StatusCode> {
    let oidc = state.oidc_client.as_ref()
        .ok_or(StatusCode::NOT_FOUND)?;

    // Generate CSRF token and nonce
    let (auth_url, csrf_token, nonce) = oidc.client
        .authorize_url(
            openidconnect::AuthenticationFlow::<openidconnect::core::CoreResponseType>::AuthorizationCode,
            openidconnect::CsrfToken::new_random,
            openidconnect::Nonce::new_random,
        )
        .add_scope(openidconnect::Scope::new("openid".to_string()))
        .add_scope(openidconnect::Scope::new("profile".to_string()))
        .url();

    // Validate return_url to prevent open redirect attacks.
    // Must be a relative path (starts with /, no //) or falls back to /.
    let return_url = params.return_url
        .filter(|u| u.starts_with('/') && !u.starts_with("//"))
        .unwrap_or_else(|| "/".to_string());

    // Store CSRF token + nonce + return_url in a short-lived state cookie
    let state_payload = OidcStatePayload {
        csrf_token: csrf_token.secret().clone(),
        nonce: nonce.secret().clone(),
        return_url,
    };
    let state_cookie = build_state_cookie(&state_payload, &state)?;

    Ok((
        [(axum::http::header::SET_COOKIE, state_cookie)],
        axum::response::Redirect::to(auth_url.as_str()),
    ).into_response())
}
```

**OIDC state cookie:** the `csrf_token`, `nonce`, and `return_url` must
survive the redirect to the IdP and back. They are stored in a short-lived
(5 minute), signed, HttpOnly cookie named `blockyard_oidc_state`. The
`Secure` flag is set via `secure_flag()` (same as the session cookie). The
cookie is deleted in the callback handler.

```rust
#[derive(Debug, Serialize, Deserialize)]
struct OidcStatePayload {
    csrf_token: String,
    nonce: String,
    return_url: String,
}
```

**`GET /callback`:**

```rust
/// Handles the IdP callback after user authentication.
/// Exchanges the authorization code for tokens, validates the ID token,
/// extracts user identity, stores session server-side, and sets a
/// signed session cookie.
async fn callback_handler<B: Backend>(
    State(state): State<AppState<B>>,
    Query(params): Query<CallbackParams>,
    headers: axum::http::HeaderMap,
) -> Result<Response, Response> {
    let oidc = state.oidc_client.as_ref()
        .ok_or_else(|| StatusCode::NOT_FOUND.into_response())?;
    let key = state.signing_key.as_ref()
        .ok_or_else(|| StatusCode::INTERNAL_SERVER_ERROR.into_response())?;
    let sessions = state.user_sessions.as_ref()
        .ok_or_else(|| StatusCode::INTERNAL_SERVER_ERROR.into_response())?;

    // 1. Extract and validate OIDC state cookie
    let state_payload = extract_state_cookie(&headers, &state)?;

    // 2. Verify CSRF token matches
    if params.state != state_payload.csrf_token {
        return Err(StatusCode::BAD_REQUEST.into_response());
    }

    // 3. Exchange authorization code for tokens
    let token_response = oidc.client
        .exchange_code(openidconnect::AuthorizationCode::new(params.code.clone()))
        .request_async(openidconnect::reqwest::async_http_client)
        .await
        .map_err(|e| {
            tracing::error!("token exchange failed: {e}");
            StatusCode::BAD_GATEWAY.into_response()
        })?;

    // 4. Validate ID token signature and extract claims
    let id_token = token_response.id_token()
        .ok_or_else(|| StatusCode::BAD_GATEWAY.into_response())?;

    let nonce = openidconnect::Nonce::new(state_payload.nonce);
    let claims = id_token
        .claims(&oidc.client.id_token_verifier(), &nonce)
        .map_err(|e| {
            tracing::error!("ID token verification failed: {e}");
            StatusCode::BAD_GATEWAY.into_response()
        })?;

    // 5. Extract sub and groups
    let sub = claims.subject().to_string();
    let groups = extract_groups(claims, &oidc.groups_claim);

    // 6. Store session server-side
    let access_token = token_response.access_token().secret().clone();
    let refresh_token = token_response.refresh_token()
        .map(|t| t.secret().clone())
        .unwrap_or_default();
    let expires_at = token_response.expires_in()
        .map(|d| now_unix() + d.as_secs() as i64)
        .unwrap_or_else(|| now_unix() + 300); // default 5min

    sessions.insert(sub.clone(), UserSession {
        groups,
        access_token,
        refresh_token,
        expires_at,
    });

    // 7. Build signed session cookie (minimal — just sub + issued_at)
    let cookie_payload = CookiePayload {
        sub,
        issued_at: now_unix(),
    };
    let cookie_value = cookie_payload.encode(key)
        .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())?;

    let cookie_max_age = state.config.oidc.as_ref()
        .map(|c| c.cookie_max_age)
        .unwrap_or(Duration::from_secs(86400));

    let secure = secure_flag(&state.config);
    let session_cookie = format!(
        "blockyard_session={cookie_value}; Path=/; HttpOnly; SameSite=Lax{secure}; Max-Age={}",
        cookie_max_age.as_secs()
    );

    // 8. Clear the OIDC state cookie
    let clear_state = format!(
        "blockyard_oidc_state=; Path=/; HttpOnly{secure}; Max-Age=0"
    );

    // 9. Redirect to return_url
    Ok((
        [
            (axum::http::header::SET_COOKIE, session_cookie),
            (axum::http::header::SET_COOKIE, clear_state),
        ],
        axum::response::Redirect::to(&state_payload.return_url),
    ).into_response())
}
```

**Groups extraction:** the groups claim name varies across IdPs. Some put
groups in a top-level claim, others in nested structures. The `GroupsClaims`
type captures all non-standard claims via `#[serde(flatten)]`, so the
extraction function can look up the configured claim name directly:

```rust
fn extract_groups(
    claims: &openidconnect::IdTokenClaims<
        GroupsClaims,
        openidconnect::core::CoreGenderClaim,
    >,
    groups_claim: &str,
) -> Vec<String> {
    match claims.additional_claims().extra.get(groups_claim) {
        Some(serde_json::Value::Array(arr)) => {
            arr.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        }
        Some(other) => {
            tracing::warn!(
                groups_claim,
                ?other,
                "groups claim is not an array — ignoring"
            );
            Vec::new()
        }
        None => {
            tracing::debug!(groups_claim, "groups claim not present in ID token");
            Vec::new()
        }
    }
}
```

**`POST /logout`:**

```rust
/// Clear the session cookie and remove the server-side session.
/// Optionally redirect to the IdP's end_session_endpoint if available.
async fn logout_handler<B: Backend>(
    State(state): State<AppState<B>>,
    headers: axum::http::HeaderMap,
) -> Response {
    // Remove server-side session if cookie is present
    if let (Some(key), Some(sessions)) = (&state.signing_key, &state.user_sessions) {
        if let Some(cookie_value) = extract_session_cookie(&headers) {
            if let Ok(payload) = CookiePayload::decode(cookie_value, key) {
                sessions.remove(&payload.sub);
            }
        }
    }

    let secure = secure_flag(&state.config);
    let clear_cookie = format!(
        "blockyard_session=; Path=/; HttpOnly{secure}; Max-Age=0"
    );

    // Check if IdP has an end_session_endpoint
    if let Some(oidc) = &state.oidc_client {
        if let Some(end_session) = oidc.provider_metadata
            .additional_metadata()
            // end_session_endpoint is not in the standard discovery
            // fields — check raw metadata JSON
            // ...
        {
            // Redirect to IdP logout
        }
    }

    (
        [(axum::http::header::SET_COOKIE, clear_cookie.to_string())],
        axum::response::Redirect::to("/"),
    ).into_response()
}
```

**Tests:**

- `/login` without OIDC configured returns 404
- `/login` with OIDC configured returns 302 to IdP authorize URL
- `/login?return_url=/app/foo/` encodes return URL in state cookie
- `/login?return_url=https://evil.com/` falls back to `/` (open redirect prevention)
- `/login?return_url=//evil.com` falls back to `/` (protocol-relative prevention)
- `/callback` with mismatched CSRF token returns 400
- `/callback` stores session in UserSessionStore
- `/logout` removes session from UserSessionStore
- `/logout` clears the session cookie

### Step 6: App-plane auth middleware

Insert an auth middleware into the proxy router that:
1. Extracts the `blockyard_session` cookie
2. Verifies the HMAC signature
3. Looks up the server-side session by `sub`
4. Checks access token expiry — refreshes if needed
5. Inserts `AuthenticatedUser` into request extensions

```rust
/// Middleware for app-plane routes. Redirects unauthenticated users
/// to /login with the current URL as return_url.
pub async fn app_auth_middleware<B: Backend>(
    State(state): State<AppState<B>>,
    mut req: Request,
    next: Next,
) -> Result<Response, Response> {
    // If OIDC is not configured, pass through (v0 compat)
    let Some(key) = &state.signing_key else {
        return Ok(next.run(req).await);
    };
    let sessions = state.user_sessions.as_ref().unwrap();

    // Extract session cookie
    let cookie_value = extract_session_cookie(req.headers())
        .ok_or_else(|| redirect_to_login(&req))?;

    // Decode and verify signature
    let cookie = CookiePayload::decode(cookie_value, key)
        .map_err(|_| redirect_to_login(&req))?;

    // Check cookie max-age
    let max_age = state.config.oidc.as_ref()
        .map(|c| c.cookie_max_age.as_secs() as i64)
        .unwrap_or(86400);
    if now_unix() - cookie.issued_at > max_age {
        return Err(redirect_to_login(&req));
    }

    // Look up server-side session
    let session = sessions.get(&cookie.sub)
        .ok_or_else(|| redirect_to_login(&req))?;

    // Refresh access token if near expiry (within 60 seconds).
    // Uses a per-user lock to prevent concurrent refresh attempts —
    // some IdPs invalidate refresh tokens on first use (rotation),
    // so only one request should hit the token endpoint.
    let needs_refresh = session.expires_at - now_unix() < 60;
    drop(session); // release DashMap read lock before async refresh

    if needs_refresh {
        let lock = sessions.refresh_lock(&cookie.sub);
        let _guard = lock.lock().await;

        // Re-check after acquiring the lock — another request may
        // have already refreshed while we waited.
        let still_needs_refresh = sessions.get(&cookie.sub)
            .map_or(true, |s| s.expires_at - now_unix() < 60);

        if still_needs_refresh {
            match refresh_access_token(&state, &cookie.sub).await {
                Ok(()) => {}
                Err(_) => {
                    sessions.remove(&cookie.sub);
                    return Err(redirect_to_login(&req));
                }
            }
        }
    }

    // Re-read session (may have been updated by refresh)
    let session = sessions.get(&cookie.sub)
        .ok_or_else(|| redirect_to_login(&req))?;

    // Insert authenticated user into request extensions
    let user = AuthenticatedUser {
        sub: cookie.sub.clone(),
        groups: session.groups.clone(),
        access_token: session.access_token.clone(),
    };
    drop(session);

    req.extensions_mut().insert(user);
    Ok(next.run(req).await)
}

/// Extract the blockyard_session cookie value from headers.
fn extract_session_cookie(headers: &axum::http::HeaderMap) -> Option<&str> {
    headers.get_all(axum::http::header::COOKIE)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|v| v.split(';'))
        .map(str::trim)
        .find_map(|c| c.strip_prefix("blockyard_session="))
}
```

**Redirect helper:**

```rust
fn redirect_to_login(req: &Request) -> Response {
    let return_url = req.uri().path_and_query()
        .map(|pq| pq.as_str())
        .unwrap_or("/");
    let encoded = urlencoding::encode(return_url);
    axum::response::Redirect::to(&format!("/login?return_url={encoded}"))
        .into_response()
}
```

Add `urlencoding` to dependencies:

```toml
urlencoding = "2"
```

**Token refresh function:**

```rust
/// Exchange the refresh token for a new access token via the IdP's
/// token endpoint. Updates the server-side session in place.
async fn refresh_access_token<B: Backend>(
    state: &AppState<B>,
    sub: &str,
) -> Result<(), SessionError> {
    let oidc = state.oidc_client.as_ref()
        .ok_or(SessionError::OidcNotConfigured)?;
    let sessions = state.user_sessions.as_ref()
        .ok_or(SessionError::OidcNotConfigured)?;

    // Read the refresh token
    let refresh_token = sessions.get(sub)
        .map(|s| s.refresh_token.clone())
        .ok_or(SessionError::SessionNotFound)?;

    // Exchange refresh token for new access token
    let token_response = oidc.client
        .exchange_refresh_token(
            &openidconnect::RefreshToken::new(refresh_token),
        )
        .request_async(openidconnect::reqwest::async_http_client)
        .await
        .map_err(|e| SessionError::RefreshFailed(e.to_string()))?;

    let new_access_token = token_response.access_token().secret().clone();
    let new_refresh_token = token_response.refresh_token()
        .map(|t| t.secret().clone());
    let new_expires_at = token_response.expires_in()
        .map(|d| now_unix() + d.as_secs() as i64)
        .unwrap_or_else(|| now_unix() + 300);

    // Update session in place
    sessions.update_tokens(sub, new_access_token, new_refresh_token, new_expires_at);

    Ok(())
}
```

Note that token refresh no longer requires re-encoding the cookie or setting
a new `Set-Cookie` header — the cookie is unchanged (same `sub` and
`issued_at`), and the refreshed tokens are written directly to the
server-side store. This eliminates the "insert extensions before next.run but
set cookie on response" ordering issue from the original design.

### Step 7: Router integration

Wire the auth middleware into the proxy routes:

```rust
pub fn full_router<B: Backend + Clone>(state: AppState<B>) -> Router {
    let api = crate::api::api_router(state.clone());

    // Proxy routes with app-plane auth
    let proxy_routes = Router::new()
        .route("/app/{name}", axum::routing::get(trailing_slash_redirect))
        .route("/app/{name}/", axum::routing::any(proxy_handler_root::<B>))
        .route("/app/{name}/{*rest}", axum::routing::any(proxy_handler::<B>))
        .layer(axum::middleware::from_fn_with_state(
            state.clone(),
            app_auth_middleware::<B>,
        ));

    Router::new()
        .merge(api)
        // Auth endpoints (outside proxy auth layer)
        .route("/login", axum::routing::get(login_handler::<B>))
        .route("/callback", axum::routing::get(callback_handler::<B>))
        .route("/logout", axum::routing::post(logout_handler::<B>))
        // Proxy with auth
        .merge(proxy_routes)
        .with_state(state)
}
```

**Key point:** `/login`, `/callback`, `/logout`, `/healthz`, and `/api/v1/*`
are outside the app-plane auth layer. The auth middleware only applies to
`/app/` routes.

### Step 8: Module declarations

Update `src/lib.rs`:

```rust
pub mod config;
pub mod backend;
pub mod db;
pub mod app;
pub mod api;
pub mod proxy;
pub mod bundle;
pub mod task;
pub mod ops;
pub mod auth;  // new
```

### Step 9: Test infrastructure — Mock IdP

Integration tests need a mock OIDC provider. Implemented as a test helper
that serves discovery, JWKS, and token endpoints.

```rust
/// Minimal OIDC-compliant mock IdP for integration tests.
/// Runs an axum server that serves:
///   GET  /.well-known/openid-configuration
///   GET  /jwks
///   POST /token
///   GET  /authorize (redirects back with code)
pub struct MockIdp {
    pub addr: SocketAddr,
    signing_key: rsa::RsaPrivateKey,
    jwks: serde_json::Value,
}

impl MockIdp {
    pub async fn start() -> Self { /* ... */ }

    /// Issue a signed JWT with the given sub and groups.
    pub fn issue_id_token(&self, sub: &str, groups: &[&str]) -> String {
        // Sign with RS256 using the test RSA key
    }

    /// Issue a test access token (opaque string for now).
    pub fn issue_access_token(&self) -> String {
        uuid::Uuid::new_v4().to_string()
    }
}
```

Add `rsa` to dev-dependencies for the mock IdP:

```toml
[dev-dependencies]
rsa = "0.9"
```

The mock IdP's `/token` endpoint accepts authorization codes and returns
pre-configured token responses. The `/authorize` endpoint redirects back to
the callback URL with a code parameter.

### Step 10: Integration tests

```rust
#[tokio::test]
async fn login_redirects_to_idp() {
    let idp = MockIdp::start().await;
    let (addr, _state) = spawn_test_server_with_oidc(&idp).await;

    let client = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build().unwrap();

    let resp = client.get(format!("http://{addr}/login")).send().await.unwrap();
    assert_eq!(resp.status(), 302);
    let location = resp.headers().get("location").unwrap().to_str().unwrap();
    assert!(location.starts_with(&format!("http://{}", idp.addr)));
}

#[tokio::test]
async fn full_auth_flow() {
    let idp = MockIdp::start().await;
    let (addr, state) = spawn_test_server_with_oidc(&idp).await;

    let client = reqwest::Client::builder()
        .cookie_store(true)
        .redirect(reqwest::redirect::Policy::none())
        .build().unwrap();

    // 1. GET /login → 302 to IdP
    let resp = client.get(format!("http://{addr}/login")).send().await.unwrap();
    assert_eq!(resp.status(), 302);

    // 2. Simulate IdP redirect back to /callback with code
    // (In reality the user authenticates at the IdP; we skip that)
    let resp = client.get(format!(
        "http://{addr}/callback?code=test-code&state={csrf_token}"
    )).send().await.unwrap();
    assert_eq!(resp.status(), 302);  // Redirect to return_url

    // 3. Verify session exists server-side
    assert!(state.user_sessions.as_ref().unwrap().get("test-sub").is_some());

    // 4. Access /app/my-app/ — should succeed (not redirect to login)
}

#[tokio::test]
async fn unauthenticated_proxy_redirects_to_login() {
    let idp = MockIdp::start().await;
    let (addr, _state) = spawn_test_server_with_oidc(&idp).await;

    let client = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .build().unwrap();

    let resp = client.get(format!("http://{addr}/app/my-app/")).send().await.unwrap();
    assert_eq!(resp.status(), 302);
    let location = resp.headers().get("location").unwrap().to_str().unwrap();
    assert!(location.starts_with("/login"));
}

#[tokio::test]
async fn logout_removes_server_side_session() {
    // ... authenticate first, then POST /logout ...
    // Verify session is removed from UserSessionStore
    assert!(state.user_sessions.as_ref().unwrap().get("test-sub").is_none());
}

#[tokio::test]
async fn no_oidc_config_passes_through() {
    // v0 compatibility: without [oidc] config, proxy routes are unprotected
    let (addr, _state) = spawn_test_server().await;  // existing helper

    let client = reqwest::Client::new();
    // Create app, deploy bundle, then access — should work without auth
}
```

## Changes to existing v0 proxy session handling

v0's proxy already sets a `blockyard_session` cookie for session-to-worker
routing. v1's auth cookie uses the same name. This is intentional —
the session cookie now carries identity and is used for session-to-worker
pinning:

1. **Auth middleware** reads the cookie, verifies the signature, looks up
   `sub` in the server-side `UserSessionStore`, and inserts
   `AuthenticatedUser` into extensions
2. **Proxy handler** uses the `sub` from the cookie (or a hash of it) for
   worker pinning via `SessionStore`

The v0 cookie format (plain UUID) is incompatible with the v1 format (signed
payload). When OIDC is enabled, the v0 cookie format is no longer used.
When OIDC is disabled (v0 compat mode), the plain UUID cookie continues to
work unchanged.

**Migration concern:** none. v0 has no production deployments with persistent
sessions. The switch from plain UUID to signed payload is a clean break.

## File summary

```
src/
├── auth/
│   ├── mod.rs              # AuthenticatedUser type, module exports
│   ├── oidc.rs             # OidcClient, discovery, groups extraction
│   └── session.rs          # SigningKey, CookiePayload, UserSession,
│                           # UserSessionStore
├── config.rs               # + OidcConfig, session_secret, external_url,
│                           # validation
├── app.rs                  # + oidc_client, signing_key, user_sessions
│                           # fields
├── proxy/mod.rs            # + auth middleware layer on proxy routes
├── lib.rs                  # + pub mod auth
tests/
└── auth_test.rs            # login, callback, logout, middleware tests
```

## Exit criteria

Phase 1-1 is done when:

- `cargo build` succeeds with and without `[oidc]` config
- Config tests pass: OIDC parsing, validation, env var coverage
- Session cookie tests pass: sign/verify round-trip, tamper rejection
- UserSessionStore tests pass: insert, get, remove, update_tokens
- Mock IdP test infrastructure works
- Integration tests pass:
  - Login redirects to IdP authorize URL
  - Full auth flow: login → callback → session cookie → server-side
    session → authenticated access
  - Unauthenticated proxy request redirects to `/login`
  - Token refresh updates server-side session (no cookie change)
  - Logout removes server-side session and clears cookie
  - No-OIDC mode: proxy passes through without auth (v0 compat)
- Existing v0 tests continue to pass (no regression)
- `env_var_coverage_complete` test passes with new fields
