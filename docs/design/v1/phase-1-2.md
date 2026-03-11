# Phase 1-2: RBAC + Per-Content ACL + Control-Plane JWT Auth

Add authorization to blockyard. Phase 1-1 established *who the user is*
(authentication); this phase decides *what they can do* (authorization).
Three capabilities land together because they're tightly coupled:

1. **Role-based access control (RBAC)** — three system roles (`admin`,
   `publisher`, `viewer`) mapped from IdP groups via a `role_mappings`
   table.
2. **Per-content ACL** — fine-grained access grants per app (`viewer`,
   `collaborator`) for individual users or groups.
3. **Control-plane JWT auth** — replace the v0 static bearer token with
   JWT validation against the IdP's JWKS, enabling machine-to-machine
   auth via the OAuth 2.0 client credentials flow.

This phase depends on phase 1-1 (OIDC sessions, JWKS discovery, signed
cookies, `AuthenticatedUser` in request extensions). The static bearer
token is retained as a fallback when `[oidc]` is not configured (dev mode).

## Design decision: unified CallerIdentity

Both the app plane (session cookie) and control plane (Bearer JWT) need a
common identity type for authorization checks. Phase 1-2 introduces
`CallerIdentity` — a lightweight struct carrying `sub`, `groups`, and
`role`. Both auth middlewares produce it; all authorization code consumes it.

`CallerIdentity` lives in `auth/mod.rs` alongside phase 1-1's
`AuthenticatedUser`. The two types coexist in app-plane request extensions:
`AuthenticatedUser` carries the IdP access token (needed by phase 1-3 for
OpenBao credential injection), while `CallerIdentity` carries the derived
role (needed for authz checks). Control-plane requests only have
`CallerIdentity`.

**Static-token identity:** when `[oidc]` is not configured, the static
bearer token middleware injects a `CallerIdentity` with
`sub = "admin"`, empty groups, and `role = Admin`. This is a hardcoded
sentinel — static-token mode is dev/single-operator only and will be
superseded once OIDC is configured. Apps created via static token get
`owner = "admin"`.

## Design decision: role derivation from IdP groups

Roles are not stored per-user in blockyard's database. Instead, the
`role_mappings` table maps IdP group names to blockyard roles. When a user
authenticates (OIDC session or JWT), their groups claim is checked against
`role_mappings` and the highest-privilege match wins. If no groups match,
the user has `Role::None` — they can only access apps explicitly granted
to them via ACL.

**Rationale:** role changes happen in the IdP (group membership) and are
reflected on next authentication — no sync protocol, no user table, no
"promote user" API that drifts from the IdP. The trade-off: operators must
manage group-to-role mappings, and role changes require re-authentication
to take effect.

**Machine clients (client credentials):** the IdP can include a groups
claim in client credentials tokens (configurable in Keycloak, Auth0, etc.).
Machine clients are assigned roles through the same group → role mapping.
If a machine client's token has no groups claim, it gets `Role::None`.
Operators must configure their IdP to include the appropriate groups for
machine clients that need elevated access.

## Design decision: per-content collaborator permissions

The per-content ACL supports two roles:

- **`viewer`** — proxy access only (can use the app).
- **`collaborator`** — proxy access + deploy bundles + start/stop + update
  config on the granted app. Essentially a co-owner without delete or
  ACL management rights.

This mirrors Posit Connect's Viewer / Collaborator model.

## Design decision: ACL management permissions

- **Admins** can manage ACLs on any app.
- **Owners** can manage ACLs on their own apps.
- **Collaborators and viewers** cannot manage ACLs.

## Design decision: 404 on unauthorized access (not 403)

When a user requests an app they don't have access to — via the API or the
proxy — blockyard returns 404 (not 403). The `GET /apps` list silently
omits apps the caller can't see. This matches Posit Connect's behavior:
apps you can't access simply don't exist from your perspective.

**Rationale:** 403 leaks information — it confirms the app exists. For a
multi-tenant platform, this is an unnecessary disclosure. The trade-off is
debuggability (a 404 doesn't tell you "ask for access"), but this is the
standard pattern for content platforms with per-item access control.

## Deliverables

1. `CallerIdentity` and `Role` types in `auth/mod.rs`
2. `authz/` module — permission checks, content-role evaluation, ACL logic
3. JWKS cache + JWT validation (`auth/client_credentials.rs`)
4. Control-plane auth middleware — validate Bearer tokens as JWTs, fall
   back to static token when OIDC is not configured
5. App-plane auth middleware extension — derive role, insert
   `CallerIdentity` alongside `AuthenticatedUser`
6. Schema migration — `app_access` and `role_mappings` tables
7. `owner` column on `apps` table (via migration consolidation)
8. Role mapping cache — in-memory `DashMap`, loaded on startup, updated on
   writes
9. Authorization guards on all existing API endpoints
10. Per-content ACL check on proxy routes
11. ACL management API — `POST/GET/DELETE /api/v1/apps/{id}/access`
12. Role mapping management API — `GET/PUT/DELETE /api/v1/role-mappings`
13. New dependency: `jsonwebtoken`
14. New dependency: `reqwest` (promoted from dev-dependencies)

## Step-by-step

### Step 1: New dependencies

Add to `Cargo.toml`:

```toml
# JWT validation for control-plane auth
jsonwebtoken = "9"

# HTTP client for JWKS fetching (+ OpenBao in phase 1-3)
reqwest = { version = "0.12", features = ["json", "rustls-tls"] }
```

**Dependency rationale:**

- **jsonwebtoken** — JWT decode + signature validation against JWKS. The
  `openidconnect` crate (phase 1-1) handles ID token validation internally
  during the callback flow, but control-plane Bearer tokens are plain JWTs
  (not OIDC ID tokens) and need direct validation.
- **reqwest** — HTTP client for fetching the raw JWKS JSON from the IdP's
  `jwks_uri`. Already used transitively by `openidconnect`; promoted to a
  direct dependency. Also needed by phase 1-3 (OpenBao client).

### Step 2: Prerequisite — migration consolidation

The v1 plan calls for consolidating the two v0 migrations
(`001_initial.sql`, `002_remove_app_status.sql`) into a single
`001_initial.sql` before v0.1.0. Phase 1-2 is the first phase that adds
schema changes, so consolidation happens now.

**New `001_initial.sql`** — the consolidated schema, including the `owner`
column needed by this phase:

```sql
CREATE TABLE IF NOT EXISTS apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    active_bundle           TEXT REFERENCES bundles(id),
    max_workers_per_app     INTEGER,
    max_sessions_per_worker INTEGER DEFAULT 1,
    memory_limit            TEXT,
    cpu_limit               REAL,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS bundles (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    status      TEXT NOT NULL DEFAULT 'pending',
    path        TEXT NOT NULL,
    uploaded_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_bundles_app_id ON bundles(app_id);
```

`owner` has `DEFAULT 'admin'` as a safety net — the code always sets it
explicitly, but the default prevents insert failures during development if
a code path is missed. The value `'admin'` matches the static-token
sentinel identity.

**Developer migration:** since we're pre-release with no external consumers,
developers delete their local databases and let sqlx recreate them. No
upgrade path is maintained.

Delete `002_remove_app_status.sql`. Renumber subsequent migrations.

### Step 3: Role types + CallerIdentity

Add to `src/auth/mod.rs` (extending the phase 1-1 module):

```rust
/// System-level role, derived from IdP groups via role_mappings.
/// Ordered by privilege — used for max() when multiple groups match.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub enum Role {
    None,
    Viewer,
    Publisher,
    Admin,
}

impl Role {
    /// Can this role create new apps?
    pub fn can_create_app(&self) -> bool {
        matches!(self, Role::Admin | Role::Publisher)
    }

    /// Can this role see all apps (regardless of ownership/grants)?
    pub fn can_view_all_apps(&self) -> bool {
        matches!(self, Role::Admin)
    }

    /// Can this role manage role mappings?
    pub fn can_manage_roles(&self) -> bool {
        matches!(self, Role::Admin)
    }
}

/// How the caller authenticated. Informational — not used for
/// permission decisions, but useful for audit logging (phase 1-6).
#[derive(Debug, Clone, Copy)]
pub enum AuthSource {
    /// Browser session via OIDC (phase 1-1)
    Session,
    /// JWT Bearer token (client credentials or direct IdP token)
    Jwt,
    /// Static bearer token (v0 compat, dev mode)
    StaticToken,
}

/// Unified caller identity produced by both auth middlewares.
/// Inserted into request extensions for use by authorization checks.
#[derive(Debug, Clone)]
pub struct CallerIdentity {
    pub sub: String,
    pub groups: Vec<String>,
    pub role: Role,
    pub source: AuthSource,
}
```

**Role derivation helper** (also in `auth/mod.rs`):

```rust
/// Derive the effective role for a set of groups by looking up each
/// group in the role mapping cache and taking the highest-privilege match.
pub fn derive_role(groups: &[String], role_cache: &RoleMappingCache) -> Role {
    groups.iter()
        .filter_map(|g| role_cache.get(g))
        .max()
        .unwrap_or(Role::None)
}
```

**Tests:**

- `Role` ordering: `None < Viewer < Publisher < Admin`
- `derive_role` with no matching groups → `Role::None`
- `derive_role` with one match → that role
- `derive_role` with multiple matches → highest privilege
- `can_create_app`: true for Admin + Publisher, false for Viewer + None
- `can_view_all_apps`: true only for Admin
- `can_manage_roles`: true only for Admin

### Step 4: Authorization types + ACL evaluation

New module: `src/authz/mod.rs`

```rust
pub mod acl;

use crate::auth::Role;

/// Per-content role granted via the app_access table.
/// Ordered by privilege for max() resolution.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
pub enum ContentRole {
    Viewer,
    Collaborator,
}

/// The effective relationship between a caller and a specific app.
/// Determines what operations the caller can perform on that app.
/// Computed from system role + ownership + ACL grants.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AppRelation {
    /// No access at all.
    None,
    /// Per-content viewer (ACL grant). Can use the app via proxy.
    ContentViewer,
    /// Per-content collaborator (ACL grant). Can deploy, start/stop,
    /// update config. Cannot delete or manage ACLs.
    ContentCollaborator,
    /// App owner. Full access except role management.
    Owner,
    /// System admin. Full access to everything.
    Admin,
}

impl AppRelation {
    pub fn can_access_proxy(&self) -> bool {
        !matches!(self, AppRelation::None)
    }

    pub fn can_deploy(&self) -> bool {
        matches!(self, AppRelation::Admin | AppRelation::Owner | AppRelation::ContentCollaborator)
    }

    pub fn can_start_stop(&self) -> bool {
        matches!(self, AppRelation::Admin | AppRelation::Owner | AppRelation::ContentCollaborator)
    }

    pub fn can_update_config(&self) -> bool {
        matches!(self, AppRelation::Admin | AppRelation::Owner | AppRelation::ContentCollaborator)
    }

    pub fn can_delete(&self) -> bool {
        matches!(self, AppRelation::Admin | AppRelation::Owner)
    }

    pub fn can_manage_acl(&self) -> bool {
        matches!(self, AppRelation::Admin | AppRelation::Owner)
    }

    pub fn can_view_details(&self) -> bool {
        !matches!(self, AppRelation::None)
    }
}
```

**ACL evaluation** — `src/authz/acl.rs`:

```rust
use super::ContentRole;
use crate::auth::{CallerIdentity, Role};
use crate::authz::AppRelation;

/// Row from the app_access table.
#[derive(Debug, Clone)]
pub struct AccessGrant {
    pub app_id: String,
    pub principal: String,
    pub kind: AccessKind,
    pub role: ContentRole,
    pub granted_by: String,
    pub granted_at: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum AccessKind {
    User,
    Group,
}

/// Determine the caller's relationship to a specific app.
///
/// Evaluation order:
/// 1. System admin → AppRelation::Admin (overrides all)
/// 2. App owner → AppRelation::Owner
/// 3. Explicit ACL grants (user + group) → highest content role
/// 4. No match → AppRelation::None
pub fn evaluate_access(
    caller: &CallerIdentity,
    app_owner: &str,
    grants: &[AccessGrant],
) -> AppRelation {
    // 1. System admin
    if caller.role == Role::Admin {
        return AppRelation::Admin;
    }

    // 2. Owner
    if caller.sub == app_owner {
        return AppRelation::Owner;
    }

    // 3. ACL grants — collect all matching grants and take max role
    let effective_content_role = grants.iter()
        .filter(|g| match g.kind {
            AccessKind::User => g.principal == caller.sub,
            AccessKind::Group => caller.groups.contains(&g.principal),
        })
        .map(|g| g.role)
        .max();

    match effective_content_role {
        Some(ContentRole::Collaborator) => AppRelation::ContentCollaborator,
        Some(ContentRole::Viewer) => AppRelation::ContentViewer,
        None => AppRelation::None,
    }
}
```

**Tests:**

- Admin caller → `AppRelation::Admin` regardless of ownership/grants
- Owner caller → `AppRelation::Owner`
- User with direct viewer grant → `ContentViewer`
- User with direct collaborator grant → `ContentCollaborator`
- User with viewer grant via group → `ContentViewer`
- User with both viewer (direct) and collaborator (group) → `ContentCollaborator` (max)
- User with no grants → `None`
- Admin who is also owner → `Admin` (admin takes precedence)
- `AppRelation` permission methods: all combinations tested

### Step 5: Schema migration + DB access layer

**New migration `002_access_control.sql`:**

```sql
-- Per-content access grants.
-- A principal (user or group) is granted a content role on an app.
CREATE TABLE app_access (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    principal   TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('user', 'group')),
    role        TEXT NOT NULL CHECK (role IN ('viewer', 'collaborator')),
    granted_by  TEXT NOT NULL,
    granted_at  TEXT NOT NULL,
    PRIMARY KEY (app_id, principal, kind)
);

-- Maps IdP group names to blockyard system roles.
-- Managed by admins via /api/v1/role-mappings.
CREATE TABLE role_mappings (
    group_name  TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'publisher', 'viewer')),
    PRIMARY KEY (group_name)
);
```

**DB access layer additions** (in `src/db/sqlite.rs`):

```rust
// --- App changes ---

/// Create an app with an explicit owner.
pub async fn create_app(
    pool: &SqlitePool,
    name: &str,
    owner: &str,
) -> Result<AppRow, sqlx::Error> {
    let id = uuid::Uuid::new_v4().to_string();
    let now = chrono::Utc::now().to_rfc3339();
    sqlx::query_as::<_, AppRow>(
        "INSERT INTO apps (id, name, owner, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?)
         RETURNING *"
    )
    .bind(&id).bind(name).bind(owner).bind(&now).bind(&now)
    .fetch_one(pool).await
}

// --- Role mappings ---

pub struct RoleMappingRow {
    pub group_name: String,
    pub role: String,
}

pub async fn list_role_mappings(
    pool: &SqlitePool,
) -> Result<Vec<RoleMappingRow>, sqlx::Error> {
    sqlx::query_as::<_, RoleMappingRow>("SELECT * FROM role_mappings")
        .fetch_all(pool).await
}

pub async fn upsert_role_mapping(
    pool: &SqlitePool,
    group_name: &str,
    role: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO role_mappings (group_name, role) VALUES (?, ?)
         ON CONFLICT (group_name) DO UPDATE SET role = excluded.role"
    )
    .bind(group_name).bind(role)
    .execute(pool).await?;
    Ok(())
}

pub async fn delete_role_mapping(
    pool: &SqlitePool,
    group_name: &str,
) -> Result<bool, sqlx::Error> {
    let result = sqlx::query("DELETE FROM role_mappings WHERE group_name = ?")
        .bind(group_name)
        .execute(pool).await?;
    Ok(result.rows_affected() > 0)
}

// --- App access (ACL) ---

pub struct AppAccessRow {
    pub app_id: String,
    pub principal: String,
    pub kind: String,
    pub role: String,
    pub granted_by: String,
    pub granted_at: String,
}

pub async fn list_app_access(
    pool: &SqlitePool,
    app_id: &str,
) -> Result<Vec<AppAccessRow>, sqlx::Error> {
    sqlx::query_as::<_, AppAccessRow>(
        "SELECT * FROM app_access WHERE app_id = ?"
    )
    .bind(app_id)
    .fetch_all(pool).await
}

pub async fn grant_app_access(
    pool: &SqlitePool,
    app_id: &str,
    principal: &str,
    kind: &str,
    role: &str,
    granted_by: &str,
) -> Result<(), sqlx::Error> {
    let now = chrono::Utc::now().to_rfc3339();
    sqlx::query(
        "INSERT INTO app_access (app_id, principal, kind, role, granted_by, granted_at)
         VALUES (?, ?, ?, ?, ?, ?)
         ON CONFLICT (app_id, principal, kind)
         DO UPDATE SET role = excluded.role,
                       granted_by = excluded.granted_by,
                       granted_at = excluded.granted_at"
    )
    .bind(app_id).bind(principal).bind(kind).bind(role)
    .bind(granted_by).bind(&now)
    .execute(pool).await?;
    Ok(())
}

pub async fn revoke_app_access(
    pool: &SqlitePool,
    app_id: &str,
    principal: &str,
    kind: &str,
) -> Result<bool, sqlx::Error> {
    let result = sqlx::query(
        "DELETE FROM app_access WHERE app_id = ? AND principal = ? AND kind = ?"
    )
    .bind(app_id).bind(principal).bind(kind)
    .execute(pool).await?;
    Ok(result.rows_affected() > 0)
}
```

See Step 10 for `list_accessible_apps` (the filtered list query used by
`GET /apps`).

**`AppRow` changes:**

```rust
pub struct AppRow {
    pub id: String,
    pub name: String,
    pub owner: String,               // new
    pub active_bundle: Option<String>,
    pub max_workers_per_app: Option<i64>,
    pub max_sessions_per_worker: i64,
    pub memory_limit: Option<String>,
    pub cpu_limit: Option<f64>,
    pub created_at: String,
    pub updated_at: String,
}
```

**Tests:**

- `create_app` sets owner correctly
- `list_role_mappings` returns inserted mappings
- `upsert_role_mapping` inserts new + updates existing
- `delete_role_mapping` returns true on success, false on no-op
- `grant_app_access` inserts new grant
- `grant_app_access` upserts on conflict (updates role)
- `revoke_app_access` returns true on success, false on no-op
- `list_app_access` returns all grants for an app

### Step 6: Role mapping cache

The role-mapping lookup happens on every authenticated request (both
middlewares). Querying the database per-request is wasteful for data that
changes rarely. An in-memory cache loaded on startup and updated on writes
avoids this.

```rust
use dashmap::DashMap;
use crate::auth::Role;

/// In-memory cache of group → role mappings.
/// Loaded from the database at startup. Updated synchronously when
/// role mappings are modified via the management API.
pub struct RoleMappingCache {
    mappings: DashMap<String, Role>,
}

impl RoleMappingCache {
    /// Load all mappings from the database.
    pub async fn load(pool: &SqlitePool) -> Result<Self, sqlx::Error> {
        let rows = crate::db::sqlite::list_role_mappings(pool).await?;
        let cache = Self { mappings: DashMap::new() };
        for row in rows {
            if let Some(role) = parse_role(&row.role) {
                cache.mappings.insert(row.group_name, role);
            }
        }
        Ok(cache)
    }

    /// Look up the role for a group name.
    pub fn get(&self, group: &str) -> Option<Role> {
        self.mappings.get(group).map(|r| *r)
    }

    /// Update a mapping (called after DB write).
    pub fn set(&self, group: String, role: Role) {
        self.mappings.insert(group, role);
    }

    /// Remove a mapping (called after DB delete).
    pub fn remove(&self, group: &str) {
        self.mappings.remove(group);
    }
}

fn parse_role(s: &str) -> Option<Role> {
    match s {
        "admin" => Some(Role::Admin),
        "publisher" => Some(Role::Publisher),
        "viewer" => Some(Role::Viewer),
        _ => None,
    }
}
```

**AppState addition:**

```rust
pub struct AppState<B: Backend> {
    // ... existing fields ...
    pub role_cache: Arc<RoleMappingCache>,  // new
}
```

Initialized in `main.rs` after DB pool creation:

```rust
let role_cache = Arc::new(RoleMappingCache::load(&db).await?);
```

### Step 7: JWKS cache + JWT validation

`src/auth/client_credentials.rs` — JWKS fetching, caching, and JWT
validation for control-plane Bearer tokens.

Phase 1-1's `openidconnect` crate handles JWKS internally for ID token
validation during the callback flow. Control-plane tokens are plain JWTs
(from the client credentials flow), not OIDC ID tokens, and need direct
validation via `jsonwebtoken`. This requires fetching the raw JWKS JSON
from the IdP's `jwks_uri` (discovered in phase 1-1) and caching it.

```rust
use jsonwebtoken::{
    decode, decode_header, Algorithm, DecodingKey, Validation,
    jwk::JwkSet,
};
use tokio::sync::RwLock;

/// Cached JWKS for JWT validation on the control plane.
pub struct JwksCache {
    jwks: RwLock<JwkSet>,
    jwks_uri: String,
    http_client: reqwest::Client,
    /// Prevents refresh storms — if a kid-not-found triggers a refresh,
    /// subsequent requests reuse the refreshed JWKS instead of all
    /// hammering the IdP.
    last_refresh: RwLock<std::time::Instant>,
}

/// Minimum time between JWKS refreshes (prevents hammering the IdP).
const REFRESH_COOLDOWN: std::time::Duration = std::time::Duration::from_secs(60);

impl JwksCache {
    /// Fetch JWKS from the IdP's jwks_uri and initialize the cache.
    /// Called once at startup, alongside OIDC discovery.
    pub async fn new(jwks_uri: &str) -> Result<Self, JwksError> {
        let http_client = reqwest::Client::new();
        let jwks = fetch_jwks(&http_client, jwks_uri).await?;
        Ok(Self {
            jwks: RwLock::new(jwks),
            jwks_uri: jwks_uri.to_string(),
            http_client,
            last_refresh: RwLock::new(std::time::Instant::now()),
        })
    }

    /// Refresh the JWKS from the IdP. No-op if called within the
    /// cooldown period.
    pub async fn refresh(&self) -> Result<bool, JwksError> {
        let last = *self.last_refresh.read().await;
        if last.elapsed() < REFRESH_COOLDOWN {
            return Ok(false); // within cooldown, skip
        }

        let jwks = fetch_jwks(&self.http_client, &self.jwks_uri).await?;
        *self.jwks.write().await = jwks;
        *self.last_refresh.write().await = std::time::Instant::now();
        Ok(true)
    }

    /// Validate a JWT and return its claims.
    /// On kid-not-found, refreshes the JWKS once and retries.
    pub async fn validate(
        &self,
        token: &str,
        issuer: &str,
        audience: &str,
    ) -> Result<JwtClaims, JwksError> {
        match self.try_validate(token, issuer, audience).await {
            Ok(claims) => Ok(claims),
            Err(JwksError::KidNotFound(_)) => {
                // Key rotation: refresh JWKS and retry once
                self.refresh().await?;
                self.try_validate(token, issuer, audience).await
            }
            Err(e) => Err(e),
        }
    }

    async fn try_validate(
        &self,
        token: &str,
        issuer: &str,
        audience: &str,
    ) -> Result<JwtClaims, JwksError> {
        let header = decode_header(token)
            .map_err(|e| JwksError::InvalidToken(e.to_string()))?;
        let kid = header.kid.as_deref()
            .ok_or_else(|| JwksError::MissingKid)?;

        let jwks = self.jwks.read().await;
        let jwk = jwks.find(kid)
            .ok_or_else(|| JwksError::KidNotFound(kid.to_string()))?;

        let key = DecodingKey::from_jwk(jwk)
            .map_err(|e| JwksError::InvalidKey(e.to_string()))?;

        let mut validation = Validation::new(
            header.alg.unwrap_or(Algorithm::RS256)
        );
        validation.set_issuer(&[issuer]);
        validation.set_audience(&[audience]);

        let token_data = decode::<JwtClaims>(token, &key, &validation)
            .map_err(|e| JwksError::InvalidToken(e.to_string()))?;

        Ok(token_data.claims)
    }
}

async fn fetch_jwks(
    client: &reqwest::Client,
    uri: &str,
) -> Result<JwkSet, JwksError> {
    let resp = client.get(uri).send().await
        .map_err(|e| JwksError::Fetch(e.to_string()))?;
    let jwks = resp.json::<JwkSet>().await
        .map_err(|e| JwksError::Parse(e.to_string()))?;
    Ok(jwks)
}

/// Claims extracted from a validated JWT.
#[derive(Debug, Clone, Deserialize)]
pub struct JwtClaims {
    pub sub: String,
    pub iss: String,
    #[serde(default)]
    pub groups: Vec<String>,
    /// Catch additional claims (some IdPs use non-standard group claim
    /// names). The middleware checks the configured groups_claim name
    /// in this map, same as phase 1-1's GroupsClaims approach.
    #[serde(flatten)]
    pub extra: HashMap<String, serde_json::Value>,
}

impl JwtClaims {
    /// Extract groups from the configured claim name.
    /// Checks the typed `groups` field first, then falls back to
    /// the extra claims map (for non-standard claim names).
    pub fn extract_groups(&self, groups_claim: &str) -> Vec<String> {
        // If the configured claim is "groups" and the typed field has values, use it
        if groups_claim == "groups" && !self.groups.is_empty() {
            return self.groups.clone();
        }

        // Otherwise, check the extra claims map
        match self.extra.get(groups_claim) {
            Some(serde_json::Value::Array(arr)) => {
                arr.iter()
                    .filter_map(|v| v.as_str().map(String::from))
                    .collect()
            }
            _ => Vec::new(),
        }
    }
}
```

**Audience validation:** the expected audience is `oidc.client_id`. Machine
clients must be configured in the IdP to include blockyard's client ID in
the token's `aud` claim. This is standard practice for resource-server
audience validation.

**AppState additions:**

```rust
pub struct AppState<B: Backend> {
    // ... existing fields ...
    pub jwks_cache: Option<Arc<JwksCache>>,  // new (None when OIDC not configured)
}
```

**Initialization in `main.rs`** (extends the phase 1-1 OIDC setup block):

```rust
let jwks_cache = if let Some(oidc_client) = &oidc_client {
    let jwks_uri = oidc_client.provider_metadata
        .jwks_uri().to_string();
    let cache = JwksCache::new(&jwks_uri).await?;
    Some(Arc::new(cache))
} else {
    None
};
```

**Tests:**

- `JwksCache::validate` with valid token → returns claims
- `JwksCache::validate` with expired token → error
- `JwksCache::validate` with wrong issuer → error
- `JwksCache::validate` with wrong audience → error
- `JwksCache::validate` with unknown kid → triggers refresh
- Cooldown: two rapid refreshes → second is skipped
- `JwtClaims::extract_groups` with standard "groups" claim
- `JwtClaims::extract_groups` with custom claim name in extras

### Step 8: Control-plane auth middleware

Replace `src/api/auth.rs` to support JWT validation with static-token
fallback.

```rust
use crate::auth::{CallerIdentity, Role, AuthSource, derive_role};

/// Control-plane auth middleware. Replaces v0's static bearer token check.
///
/// When OIDC is configured:
///   1. Extract Bearer token from Authorization header
///   2. Validate as JWT against the IdP's JWKS
///   3. Extract sub + groups from claims
///   4. Derive role from groups via role_cache
///   5. Insert CallerIdentity into request extensions
///
/// When OIDC is not configured (v0 compat / dev mode):
///   1. Extract Bearer token
///   2. Compare against static config token
///   3. Insert CallerIdentity with sub="admin", role=Admin
pub async fn api_auth<B: Backend>(
    State(state): State<AppState<B>>,
    mut req: Request,
    next: Next,
) -> Result<Response, StatusCode> {
    let token = extract_bearer_token(&req)
        .ok_or(StatusCode::UNAUTHORIZED)?;

    let identity = if let (Some(oidc), Some(jwks_cache)) =
        (&state.config.oidc, &state.jwks_cache)
    {
        // JWT validation path
        let claims = jwks_cache.validate(
            token,
            &oidc.issuer_url,
            &oidc.client_id,
        ).await.map_err(|e| {
            tracing::debug!("JWT validation failed: {e}");
            StatusCode::UNAUTHORIZED
        })?;

        let groups = claims.extract_groups(&oidc.groups_claim);
        let role = derive_role(&groups, &state.role_cache);

        CallerIdentity {
            sub: claims.sub,
            groups,
            role,
            source: AuthSource::Jwt,
        }
    } else {
        // Static token fallback
        if token != state.config.server.token.expose() {
            return Err(StatusCode::UNAUTHORIZED);
        }

        CallerIdentity {
            sub: "admin".to_string(),
            groups: Vec::new(),
            role: Role::Admin,
            source: AuthSource::StaticToken,
        }
    };

    req.extensions_mut().insert(identity);
    Ok(next.run(req).await)
}

fn extract_bearer_token(req: &Request) -> Option<&str> {
    req.headers()
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
}
```

**Changes to existing code:**

- `bearer_auth` in `api/auth.rs` is replaced entirely by `api_auth`.
- The function signature changes (returns `Response` instead of
  `StatusCode` on the error path — needed for consistency with
  `CallerIdentity` insertion). Update the router accordingly.
- `state.config.server.token` is now accessed via `.expose()` (the
  `Secret` newtype from phase 1-1).

### Step 9: App-plane auth middleware extension

Phase 1-1's `app_auth_middleware` inserts `AuthenticatedUser` into request
extensions. Phase 1-2 extends it to also insert `CallerIdentity` with the
derived role.

Add to the end of `app_auth_middleware` (after the `AuthenticatedUser`
insertion):

```rust
// --- Phase 1-2 addition: derive role and insert CallerIdentity ---
let caller = CallerIdentity {
    sub: user.sub.clone(),
    groups: user.groups.clone(),
    role: derive_role(&user.groups, &state.role_cache),
    source: AuthSource::Session,
};
req.extensions_mut().insert(caller);
```

This is a minimal change — the existing `AuthenticatedUser` insertion
is unchanged, and `CallerIdentity` is added alongside it.

### Step 10: Authorization guards on API endpoints

Each API handler extracts `CallerIdentity` from request extensions and
checks permissions. Two patterns:

**System-level checks** (create app, manage roles):

```rust
async fn create_app<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
    Json(body): Json<CreateAppRequest>,
) -> Result<impl IntoResponse, ApiError> {
    if !caller.role.can_create_app() {
        return Err(forbidden("insufficient permissions".into()));
    }

    let app = crate::db::sqlite::create_app(
        &state.db, &body.name, &caller.sub,
    ).await.map_err(/* ... */)?;

    Ok((StatusCode::CREATED, Json(app)))
}
```

**Per-app checks** (deploy, start, stop, delete, update):

```rust
/// Helper: load app + ACL grants, evaluate caller's relationship.
/// Returns 404 both when the app doesn't exist and when the caller
/// has no access — this prevents leaking app existence to unauthorized
/// users (same behavior as Posit Connect).
async fn resolve_app_relation<B: Backend>(
    state: &AppState<B>,
    caller: &CallerIdentity,
    app_id: &str,
) -> Result<(AppRow, AppRelation), ApiError> {
    let app = crate::db::sqlite::get_app(&state.db, app_id).await
        .map_err(|_| not_found(format!("app {app_id} not found")))?;

    let grants = crate::db::sqlite::list_app_access(&state.db, &app.id).await
        .map_err(|e| server_error(e.to_string()))?;

    let access_grants: Vec<AccessGrant> = grants.into_iter()
        .map(|row| row.into())
        .collect();

    let relation = crate::authz::acl::evaluate_access(
        caller, &app.owner, &access_grants,
    );

    // No access → 404 (hide app existence)
    if matches!(relation, AppRelation::None) {
        return Err(not_found(format!("app {app_id} not found")));
    }

    Ok((app, relation))
}
```

Used in handlers:

```rust
async fn delete_app<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
    Path(id): Path<String>,
) -> Result<impl IntoResponse, ApiError> {
    // resolve_app_relation already returns 404 if caller has no access.
    let (app, relation) = resolve_app_relation(&state, &caller, &id).await?;

    // Caller has some access but not delete permission → also 404.
    // Returning 403 here would confirm the app exists to non-owners.
    if !relation.can_delete() {
        return Err(not_found(format!("app {} not found", id)));
    }

    // ... existing delete logic ...
}
```

**Endpoint-by-endpoint authorization:**

| Endpoint | Check | On failure |
|---|---|---|
| `POST /apps` | `caller.role.can_create_app()` | 403 |
| `GET /apps` | filter results (see below) | omit |
| `GET /apps/{id}` | `relation != None` | 404 |
| `PATCH /apps/{id}` | `relation.can_update_config()` | 404 |
| `DELETE /apps/{id}` | `relation.can_delete()` | 404 |
| `POST /apps/{id}/bundles` | `relation.can_deploy()` | 404 |
| `GET /apps/{id}/bundles` | `relation != None` | 404 |
| `POST /apps/{id}/start` | `relation.can_start_stop()` | 404 |
| `POST /apps/{id}/stop` | `relation.can_start_stop()` | 404 |
| `GET /apps/{id}/logs` | `relation != None` | 404 |

All per-app endpoints return 404 on insufficient permissions — never 403.
This hides app existence from unauthorized callers. Only `POST /apps`
(a system-level action with no specific app) uses 403.

**`GET /apps` (list) filtering:**

```rust
async fn list_apps<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
) -> Result<Json<Vec<AppResponse>>, ApiError> {
    let apps = if caller.role.can_view_all_apps() {
        crate::db::sqlite::list_apps(&state.db).await
    } else {
        crate::db::sqlite::list_accessible_apps(
            &state.db, &caller.sub, &caller.groups,
        ).await
    }.map_err(|e| server_error(e.to_string()))?;

    Ok(Json(apps.into_iter().map(Into::into).collect()))
}
```

**DB function** (in `src/db/sqlite.rs`):

```rust
/// List apps the caller can see: owned apps + apps with an ACL grant
/// matching the caller's sub or any of their groups.
pub async fn list_accessible_apps(
    pool: &SqlitePool,
    sub: &str,
    groups: &[String],
) -> Result<Vec<AppRow>, sqlx::Error> {
    // Build the group placeholders dynamically. SQLite doesn't support
    // array parameters, so we build the IN clause with positional binds.
    //
    // The query returns DISTINCT apps where the caller is the owner OR
    // has a direct user grant OR has a group grant via any of their groups.
    let group_placeholders = if groups.is_empty() {
        // No groups — the group clause can never match.
        // Use a dummy that matches nothing.
        "SELECT 1 WHERE 0".to_string()
    } else {
        groups.iter()
            .enumerate()
            .map(|(i, _)| format!("?{}", i + 3))  // ?3, ?4, ...
            .collect::<Vec<_>>()
            .join(", ")
    };

    let sql = format!(
        "SELECT DISTINCT a.*
         FROM apps a
         LEFT JOIN app_access aa ON a.id = aa.app_id
         WHERE a.owner = ?1
            OR (aa.kind = 'user'  AND aa.principal = ?2)
            OR (aa.kind = 'group' AND aa.principal IN ({group_placeholders}))"
    );

    let mut query = sqlx::query_as::<_, AppRow>(&sql)
        .bind(sub)   // ?1 — owner check
        .bind(sub);  // ?2 — direct user grant

    for group in groups {
        query = query.bind(group);  // ?3, ?4, ... — group grants
    }

    query.fetch_all(pool).await
}
```

Single query, no N+1. The `LEFT JOIN` + `DISTINCT` handles the case where
a user has multiple grants for the same app (direct + via group) without
duplicating rows.

**New error helper** (add to `api/error.rs`):

```rust
pub fn forbidden(msg: String) -> ApiError {
    (StatusCode::FORBIDDEN, Json(ErrorResponse { error: msg }))
}
```

**`AppResponse` changes:**

The API response for apps gains an `owner` field:

```rust
#[derive(Serialize)]
pub struct AppResponse {
    pub id: String,
    pub name: String,
    pub owner: String,       // new
    // ... remaining fields unchanged ...
}
```

**Tests:**

- Create app: publisher succeeds, viewer gets 403, admin succeeds
- Create app: owner is set to caller's sub
- Delete app: owner succeeds, collaborator gets 404, admin succeeds
- Deploy bundle: owner succeeds, collaborator succeeds, viewer gets 404
- Start/stop: owner + collaborator succeed, viewer gets 404
- Update config: owner + collaborator succeed, viewer gets 404
- Get app: any relation except None succeeds, None gets 404
- List apps: admin sees all, publisher sees own + granted, viewer sees
  granted only (unauthorized apps silently omitted)
- Static-token mode: all operations succeed with admin identity

### Step 11: ACL management API

New file: `src/api/access.rs`

```
POST   /api/v1/apps/{id}/access                      — grant access
GET    /api/v1/apps/{id}/access                       — list grants
DELETE /api/v1/apps/{id}/access/{kind}/{principal}     — revoke access
```

**Grant access:**

```rust
#[derive(Deserialize)]
pub struct GrantRequest {
    pub principal: String,
    pub kind: AccessKind,     // "user" | "group"
    pub role: ContentRole,    // "viewer" | "collaborator"
}

async fn grant_access<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
    Path(app_id): Path<String>,
    Json(body): Json<GrantRequest>,
) -> Result<StatusCode, ApiError> {
    let (app, relation) = resolve_app_relation(&state, &caller, &app_id).await?;

    if !relation.can_manage_acl() {
        return Err(not_found(format!("app {app_id} not found")));
    }

    // Prevent self-grant (owner/admin already have full access)
    if body.kind == AccessKind::User && body.principal == caller.sub {
        return Err(bad_request("cannot grant access to yourself".into()));
    }

    crate::db::sqlite::grant_app_access(
        &state.db,
        &app.id,
        &body.principal,
        &body.kind.as_str(),
        &body.role.as_str(),
        &caller.sub,
    ).await.map_err(|e| server_error(e.to_string()))?;

    Ok(StatusCode::NO_CONTENT)
}
```

**List grants:**

```rust
#[derive(Serialize)]
pub struct AccessGrantResponse {
    pub principal: String,
    pub kind: String,
    pub role: String,
    pub granted_by: String,
    pub granted_at: String,
}

async fn list_access<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
    Path(app_id): Path<String>,
) -> Result<Json<Vec<AccessGrantResponse>>, ApiError> {
    let (_app, relation) = resolve_app_relation(&state, &caller, &app_id).await?;

    if !relation.can_manage_acl() {
        return Err(not_found(format!("app not found")));
    }

    let grants = crate::db::sqlite::list_app_access(&state.db, &app_id).await
        .map_err(|e| server_error(e.to_string()))?;

    Ok(Json(grants.into_iter().map(Into::into).collect()))
}
```

**Revoke access:**

```rust
async fn revoke_access<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
    Path((app_id, kind, principal)): Path<(String, String, String)>,
) -> Result<StatusCode, ApiError> {
    let (_app, relation) = resolve_app_relation(&state, &caller, &app_id).await?;

    if !relation.can_manage_acl() {
        return Err(not_found(format!("app not found")));
    }

    let removed = crate::db::sqlite::revoke_app_access(
        &state.db, &app_id, &principal, &kind,
    ).await.map_err(|e| server_error(e.to_string()))?;

    if removed {
        Ok(StatusCode::NO_CONTENT)
    } else {
        Err(not_found("grant not found".into()))
    }
}
```

**ACL enforcement on active sessions:** ACL checks run on HTTP requests
only, not on individual WebSocket frames. When a user's access is revoked,
it takes effect on their next HTTP request or WebSocket reconnect — active
WS connections continue until the next reconnect. This avoids per-frame
database lookups on the hot path.

### Step 12: Role mapping management API

New file: `src/api/roles.rs`

```
GET    /api/v1/role-mappings                  — list all mappings
PUT    /api/v1/role-mappings/{group_name}     — set mapping
DELETE /api/v1/role-mappings/{group_name}     — delete mapping
```

All endpoints are admin-only.

**List mappings:**

```rust
#[derive(Serialize)]
pub struct RoleMappingResponse {
    pub group_name: String,
    pub role: String,
}

async fn list_role_mappings<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
) -> Result<Json<Vec<RoleMappingResponse>>, ApiError> {
    if !caller.role.can_manage_roles() {
        return Err(forbidden("admin only".into()));
    }

    let rows = crate::db::sqlite::list_role_mappings(&state.db).await
        .map_err(|e| server_error(e.to_string()))?;

    Ok(Json(rows.into_iter().map(Into::into).collect()))
}
```

**Set mapping:**

```rust
#[derive(Deserialize)]
pub struct SetRoleMappingRequest {
    pub role: String,  // "admin" | "publisher" | "viewer"
}

async fn set_role_mapping<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
    Path(group_name): Path<String>,
    Json(body): Json<SetRoleMappingRequest>,
) -> Result<StatusCode, ApiError> {
    if !caller.role.can_manage_roles() {
        return Err(forbidden("admin only".into()));
    }

    // Validate role value
    let role = parse_role(&body.role)
        .ok_or_else(|| bad_request(format!(
            "invalid role '{}', must be one of: admin, publisher, viewer",
            body.role
        )))?;

    crate::db::sqlite::upsert_role_mapping(
        &state.db, &group_name, &body.role,
    ).await.map_err(|e| server_error(e.to_string()))?;

    // Update in-memory cache
    state.role_cache.set(group_name, role);

    Ok(StatusCode::NO_CONTENT)
}
```

**Delete mapping:**

```rust
async fn delete_role_mapping<B: Backend>(
    State(state): State<AppState<B>>,
    Extension(caller): Extension<CallerIdentity>,
    Path(group_name): Path<String>,
) -> Result<StatusCode, ApiError> {
    if !caller.role.can_manage_roles() {
        return Err(forbidden("admin only".into()));
    }

    let removed = crate::db::sqlite::delete_role_mapping(
        &state.db, &group_name,
    ).await.map_err(|e| server_error(e.to_string()))?;

    if removed {
        state.role_cache.remove(&group_name);
        Ok(StatusCode::NO_CONTENT)
    } else {
        Err(not_found(format!("no mapping for group '{group_name}'")))
    }
}
```

**Note on cache consistency:** the role mapping cache is updated
synchronously after the DB write succeeds. Since blockyard v1 is
single-server, there's no cache coherence concern. For v2 multi-node,
this would need a cache invalidation mechanism (or short TTL with
DB-backed reads).

### Step 13: Proxy authorization

Phase 1-1's app-plane auth middleware verifies identity and inserts
`AuthenticatedUser`. Phase 1-2 extends the proxy to check whether the
authenticated user can actually access the requested app.

The ACL check is added to the proxy request handler, after app lookup
and before session assignment / cold-start:

```rust
async fn proxy_request<B: Backend>(
    state: &AppState<B>,
    app_name: &str,
    req: Request,
    // ...
) -> Result<Response, Response> {
    let app = crate::db::sqlite::get_app_by_name(&state.db, app_name).await
        .map_err(|_| StatusCode::NOT_FOUND.into_response())?;

    // --- Phase 1-2: ACL check ---
    // CallerIdentity is only present when OIDC is configured.
    // When absent (v0 compat), skip the check.
    if let Some(caller) = req.extensions().get::<CallerIdentity>() {
        let grants = crate::db::sqlite::list_app_access(&state.db, &app.id)
            .await.unwrap_or_default();
        let access_grants: Vec<AccessGrant> = grants.into_iter()
            .map(Into::into).collect();
        let relation = evaluate_access(caller, &app.owner, &access_grants);

        if !relation.can_access_proxy() {
            return Err(StatusCode::NOT_FOUND.into_response());
        }
    }

    // ... existing proxy logic (session lookup, cold-start, forward) ...
}
```

**Performance note:** this adds a database query (`list_app_access`) to
every proxied HTTP request. SQLite reads are fast and the query is indexed
by `app_id` (primary key prefix). If this becomes a bottleneck, an
in-memory ACL cache with short TTL (30–60s) keyed by `app_id` and
invalidated on ACL writes can be added.

### Step 14: Router integration

**API router changes** (`src/api/mod.rs`):

```rust
pub fn api_router<B: Backend + Clone>(state: AppState<B>) -> Router<AppState<B>> {
    let max_body = state.config.storage.max_bundle_size;

    let authed = Router::new()
        // Existing app endpoints
        .route("/apps", post(apps::create_app).get(apps::list_apps))
        .route("/apps/{id}", get(apps::get_app).patch(apps::update_app).delete(apps::delete_app))
        .route("/apps/{id}/bundles", post(bundles::upload_bundle).get(bundles::list_bundles))
        .route("/apps/{id}/start", post(apps::start_app))
        .route("/apps/{id}/stop", post(apps::stop_app))
        .route("/apps/{id}/logs", get(apps::app_logs))
        .route("/tasks/{task_id}/logs", get(tasks::task_logs))
        // Phase 1-2: ACL management
        .route("/apps/{id}/access", post(access::grant_access).get(access::list_access))
        .route("/apps/{id}/access/{kind}/{principal}", delete(access::revoke_access))
        // Phase 1-2: Role mapping management
        .route("/role-mappings", get(roles::list_role_mappings))
        .route("/role-mappings/{group_name}", put(roles::set_role_mapping).delete(roles::delete_role_mapping))
        .layer(axum::extract::DefaultBodyLimit::max(max_body))
        .layer(middleware::from_fn_with_state(state, auth::api_auth));  // updated

    Router::new()
        .nest("/api/v1", authed)
        .route("/healthz", get(healthz))
}
```

**Module declarations** — update `src/api/mod.rs`:

```rust
pub mod auth;     // existing, updated
pub mod apps;     // existing, updated
pub mod bundles;  // existing
pub mod tasks;    // existing
pub mod error;    // existing, extended
pub mod access;   // new
pub mod roles;    // new
```

Update `src/lib.rs`:

```rust
pub mod api;
pub mod app;
pub mod auth;     // phase 1-1
pub mod authz;    // new — phase 1-2
pub mod backend;
pub mod bundle;
pub mod config;
pub mod db;
pub mod ops;
pub mod proxy;
pub mod task;
```

### Step 15: Tests

**Unit tests** (extend existing test modules):

- **Role types:** ordering, permission methods, derive_role
- **ACL evaluation:** all combinations from Step 4 tests
- **AppRelation permissions:** each method × each relation level
- **JWT validation:** valid, expired, wrong issuer, wrong audience, missing
  kid, kid-not-found refresh
- **JwtClaims::extract_groups:** standard claim, custom claim, missing claim
- **RoleMappingCache:** load, get, set, remove

**Integration tests** (`tests/rbac_test.rs`):

These use the mock IdP from phase 1-1 and issue JWTs with specific groups
to test role-based behavior end-to-end.

```rust
#[tokio::test]
async fn publisher_can_create_app() {
    let idp = MockIdp::start().await;
    let (addr, state) = spawn_test_server_with_oidc(&idp).await;

    // Configure role mapping: "developers" → publisher
    state.role_cache.set("developers".into(), Role::Publisher);

    // Issue JWT with groups=["developers"]
    let token = idp.issue_jwt("user-1", &["developers"]);

    let client = reqwest::Client::new();
    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth(&token)
        .json(&json!({"name": "my-app"}))
        .send().await.unwrap();

    assert_eq!(resp.status(), 201);
    let body: serde_json::Value = resp.json().await.unwrap();
    assert_eq!(body["owner"], "user-1");
}

#[tokio::test]
async fn viewer_cannot_create_app() {
    let idp = MockIdp::start().await;
    let (addr, state) = spawn_test_server_with_oidc(&idp).await;

    state.role_cache.set("readonly".into(), Role::Viewer);
    let token = idp.issue_jwt("user-2", &["readonly"]);

    let client = reqwest::Client::new();
    let resp = client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth(&token)
        .json(&json!({"name": "my-app"}))
        .send().await.unwrap();

    assert_eq!(resp.status(), 403);  // system-level check, not per-app → 403
}

#[tokio::test]
async fn admin_sees_all_apps() {
    let idp = MockIdp::start().await;
    let (addr, state) = spawn_test_server_with_oidc(&idp).await;

    state.role_cache.set("admins".into(), Role::Admin);
    state.role_cache.set("developers".into(), Role::Publisher);

    // Publisher creates an app
    let pub_token = idp.issue_jwt("publisher-1", &["developers"]);
    let client = reqwest::Client::new();
    client.post(format!("http://{addr}/api/v1/apps"))
        .bearer_auth(&pub_token)
        .json(&json!({"name": "app-1"}))
        .send().await.unwrap();

    // Admin lists all apps
    let admin_token = idp.issue_jwt("admin-1", &["admins"]);
    let resp = client.get(format!("http://{addr}/api/v1/apps"))
        .bearer_auth(&admin_token)
        .send().await.unwrap();

    assert_eq!(resp.status(), 200);
    let body: Vec<serde_json::Value> = resp.json().await.unwrap();
    assert_eq!(body.len(), 1);
}

#[tokio::test]
async fn publisher_sees_only_own_and_granted_apps() {
    // Create two apps (one owned, one not).
    // Grant access to the second app.
    // Verify list returns both but not others.
}

#[tokio::test]
async fn collaborator_can_deploy() {
    // Publisher creates app.
    // Publisher grants collaborator access to another user.
    // Collaborator deploys a bundle → 200.
}

#[tokio::test]
async fn content_viewer_cannot_deploy() {
    // Publisher creates app.
    // Publisher grants viewer access to another user.
    // Viewer attempts deploy → 404 (hides insufficient permission).
}

#[tokio::test]
async fn acl_grant_revoke_cycle() {
    // Admin creates app.
    // Admin grants viewer access to user-2.
    // GET /access → shows grant.
    // Admin revokes access.
    // GET /access → empty.
    // User-2 cannot access app via proxy → 404.
}

#[tokio::test]
async fn role_mapping_crud() {
    // Admin creates mapping.
    // GET → shows mapping.
    // Admin updates mapping.
    // GET → shows updated role.
    // Admin deletes mapping.
    // GET → empty.
}

#[tokio::test]
async fn unmapped_user_has_no_role() {
    // User authenticates with groups not in role_mappings.
    // Cannot create apps (403).
    // Cannot see any apps (empty list).
    // Can access app only if explicitly granted.
}

#[tokio::test]
async fn static_token_fallback() {
    // No [oidc] config — v0 compat mode.
    let (addr, _state) = spawn_test_server().await;  // existing helper

    // All operations work with static bearer token.
    // Created app has owner = "admin".
}

#[tokio::test]
async fn proxy_acl_check() {
    // Deploy and start an app.
    // User with access → proxied successfully.
    // User without access → 403.
}
```

**Mock IdP extension:** the phase 1-1 mock IdP issues ID tokens for the
OIDC callback flow. Phase 1-2 extends it with `issue_jwt()` — a method
that issues access-token-style JWTs suitable for Bearer auth on the
control plane. Same RSA signing key, same JWKS endpoint, but the token
format is a plain JWT with `sub`, `iss`, `aud`, `exp`, and groups.

```rust
impl MockIdp {
    /// Issue a JWT for control-plane Bearer auth (client credentials style).
    /// Same signing key as ID tokens, different claims structure.
    pub fn issue_jwt(&self, sub: &str, groups: &[&str]) -> String {
        let now = chrono::Utc::now().timestamp();
        let claims = json!({
            "sub": sub,
            "iss": format!("http://{}", self.addr),
            "aud": "blockyard",  // matches test OidcConfig.client_id
            "exp": now + 3600,
            "iat": now,
            "groups": groups,
        });
        // Sign with RS256 using the test RSA key
        jsonwebtoken::encode(
            &jsonwebtoken::Header::new(jsonwebtoken::Algorithm::RS256),
            &claims,
            &self.encoding_key,
        ).unwrap()
    }
}
```

## Changes to existing code

Summary of modifications to v0/phase-1-1 files:

| File | Change |
|---|---|
| `src/api/auth.rs` | Replace `bearer_auth` with `api_auth` (JWT + static fallback) |
| `src/api/mod.rs` | Add access + roles routes, update auth middleware reference |
| `src/api/apps.rs` | Add `CallerIdentity` extraction, authorization checks, owner on create |
| `src/api/bundles.rs` | Add authorization check (deploy permission) |
| `src/api/error.rs` | Add `forbidden()` helper |
| `src/auth/mod.rs` | Add `Role`, `AuthSource`, `CallerIdentity`, `derive_role` |
| `src/app.rs` | Add `role_cache` and `jwks_cache` to `AppState` |
| `src/proxy/mod.rs` | Add ACL check in proxy_request |
| `src/db/sqlite.rs` | Add `owner` to `AppRow`, add role_mapping + app_access functions |
| `src/config.rs` | No changes (OIDC config already added in phase 1-1) |
| `src/lib.rs` | Add `pub mod authz` |
| `src/main.rs` | Initialize `RoleMappingCache` and `JwksCache` |
| `migrations/001_initial.sql` | Consolidate: add `owner` column |
| `migrations/002_remove_app_status.sql` | Delete (consolidated into 001) |

## File summary

```
src/
├── auth/
│   ├── mod.rs                  # + Role, AuthSource, CallerIdentity, derive_role
│   ├── oidc.rs                 # unchanged (phase 1-1)
│   ├── session.rs              # unchanged (phase 1-1)
│   └── client_credentials.rs   # new: JwksCache, JwtClaims, JWT validation
├── authz/
│   ├── mod.rs                  # new: ContentRole, AppRelation, RoleMappingCache
│   └── acl.rs                  # new: AccessGrant, AccessKind, evaluate_access
├── api/
│   ├── auth.rs                 # rewritten: api_auth (JWT + static fallback)
│   ├── apps.rs                 # + owner on create, authz checks
│   ├── bundles.rs              # + authz check on upload
│   ├── access.rs               # new: ACL management endpoints
│   ├── roles.rs                # new: role mapping management endpoints
│   ├── error.rs                # + forbidden()
│   ├── mod.rs                  # + new routes, updated middleware
│   └── tasks.rs                # unchanged
├── app.rs                      # + role_cache, jwks_cache
├── proxy/mod.rs                # + ACL check
├── db/sqlite.rs                # + owner, role_mapping fns, app_access fns
├── main.rs                     # + RoleMappingCache + JwksCache init
├── lib.rs                      # + pub mod authz
└── ...
migrations/
├── 001_initial.sql             # consolidated (includes owner column)
└── 002_access_control.sql      # new: app_access + role_mappings tables
tests/
└── rbac_test.rs                # new: RBAC + ACL integration tests
```

## Exit criteria

Phase 1-2 is done when:

- `cargo build` succeeds with and without `[oidc]` config
- Migration consolidation: single `001_initial.sql` with `owner` column
- `002_access_control.sql` creates `app_access` and `role_mappings`
- Role type tests pass: ordering, derive_role, permission methods
- ACL evaluation tests pass: all relation types, conflict resolution
- JWT validation tests pass: valid, expired, wrong issuer, wrong audience,
  kid rotation
- JWKS cache tests pass: fetch, refresh, cooldown
- RoleMappingCache tests pass: load, get, set, remove
- Integration tests pass:
  - Publisher creates app → owner set to caller sub
  - Viewer cannot create app → 403
  - Admin sees all apps, publisher sees own + granted
  - Collaborator can deploy, viewer cannot
  - ACL grant/revoke cycle works end-to-end
  - Role mapping CRUD works end-to-end
  - Unmapped user has no role
  - Proxy ACL check: access with grant, 404 without
  - Static-token fallback: all operations work, owner = "admin"
- Existing v0 tests continue to pass (no regression)
- `env_var_coverage_complete` test passes (no new env vars in this phase)
