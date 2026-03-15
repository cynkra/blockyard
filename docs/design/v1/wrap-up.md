# v1 Wrap-Up

Sections 1–3 are implemented. Section 4 (secret lifecycle) is the
remaining work before v1 can ship.

The design principle behind Sections 1 and 2: the **IdP handles
authentication** (proving who you are via OIDC), and **blockyard handles
authorization** (deciding what you can do). This is a common pattern —
most applications manage their own roles rather than syncing them from
an external directory. It keeps blockyard IdP-agnostic (pure OIDC, no
per-IdP directory API adapters) and makes access revocation immediate
and single-place (change it in blockyard, done).

This also changes how identity is communicated to Shiny apps. The
previous design injected raw IdP groups via `X-Shiny-Groups`, but IdP
groups are meaningless to the app — it has no way to interpret what
"engineering-team" or "blockyard:admin" means. Instead, the proxy
injects `X-Shiny-Access` — the user's effective access level for the
specific app being accessed, derived from blockyard's own authorization
model. This gives apps a single, actionable value to branch on.

The v1 security review found only minor issues (timing-vulnerable token
comparison, cookie Secure flag inconsistency). Both were fixed in-tree.
Remaining accepted findings (rate limiting, body size limits, error
detail leakage) are deferred to infrastructure or production hardening.

---

## 1. Replace Role Mapping with Direct Role Management

### Problem

Blockyard maintains its own group→role mapping system: a `role_mappings`
SQLite table, a `RoleMappingCache`, CRUD API endpoints
(`/api/v1/role-mappings`), and admin UI for managing mappings. This
exists because the v1 design assumed IdP group names would not match
blockyard role names, so a translation layer was needed.

In practice this creates two problems:

**A. Bootstrapping.** On first boot the mapping table is empty, so even
a user in the IdP's "admins" group gets `RoleNone`. No one can create
mappings because only admins can manage them. The current workaround is
the static bearer token — itself a problem (see Section 2).

**B. Split authority.** Role assignment depends on both the IdP (group
membership) and blockyard (the mapping table). Revoking access requires
changes in two places. If the mapping table is misconfigured, users get
the wrong role regardless of their IdP groups. And keeping blockyard's
mapping table in sync with IdP group changes is a manual, error-prone
process — or requires per-IdP directory sync adapters that break
blockyard's IdP-agnostic design.

### Design

Blockyard manages roles and access control directly. The IdP
authenticates users via OIDC; blockyard decides what they can do.

A `users` table tracks every user who has logged in via OIDC. On each
login, blockyard upserts the user's `sub`, `email`, and `name` from
the ID token. IdP groups are not stored — they play no role in
blockyard's authorization model.

The `role` field is managed by blockyard admins via the API.

```sql
CREATE TABLE IF NOT EXISTS users (
    sub          TEXT PRIMARY KEY,
    email        TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    role         TEXT NOT NULL DEFAULT 'viewer',
    active       INTEGER NOT NULL DEFAULT 1,
    last_login   TEXT NOT NULL
);
```

**Role assignment:** admins assign roles to users in blockyard. The
three system roles (`admin`, `publisher`, `viewer`) are unchanged. A
new user who logs in for the first time gets `viewer` by default —
they can see the catalog and access apps they're explicitly granted
access to via per-content ACL. Admins promote users as needed.

**Access revocation:** an admin sets `active = 0` on the user record.
All their sessions and PATs immediately stop working. No IdP change
required. If the user's IdP account is also disabled, they can't
re-authenticate — but blockyard doesn't need to know about that to
enforce its own access control.

**Per-content ACL:** user-to-resource grants. "User X may read app Y"
or "user Z may update the bundle of app W." The ACL system references
users by `sub`. There are no group-based grants — authorization is
managed per-user in blockyard, not derived from IdP groups.

**App visibility:** each app has an `access_type` that controls who
can access it:

| `access_type` | Who can access |
|---|---|
| `acl` | Only users with an explicit ACL grant (owner, collaborator, viewer). Default. |
| `logged_in` | Any authenticated user. No per-user grant required. |
| `public` | Anyone, including unauthenticated users. |

The current schema supports `acl` and `public`. This change adds
`logged_in` as a third option — useful for internal deployments
where every employee should see every app without needing per-app
grants. The CHECK constraint becomes
`CHECK (access_type IN ('acl', 'logged_in', 'public'))`.

#### Identity injection

The proxy injects two headers on each proxied request to a Shiny app:

- **`X-Shiny-User`** — the user's `sub` (unchanged from current
  behavior).
- **`X-Shiny-Access`** — the user's effective access level for the
  specific app being accessed.

`X-Shiny-Access` is derived at proxy time from blockyard's
authorization model. The values align with the per-content ACL levels:

| Condition | Value |
|---|---|
| System admin, or app owner | `owner` |
| Has collaborator ACL on this app | `collaborator` |
| Has viewer ACL, or app is `logged_in`/`public` and user is authenticated | `viewer` |
| App is `public`, user is not authenticated | `anonymous` |

The app reads `session$request$HTTP_X_SHINY_ACCESS` and branches on
it — one value, no group parsing, no ambiguity about what it means.

`X-Shiny-Groups` is removed. IdP groups are not injected into Shiny
apps.

#### Bootstrapping

The first admin is configured via a config field:

```toml
[oidc]
initial_admin = "google-oauth2|abc123"   # OIDC sub of the first admin
```

When this user first logs in via OIDC, their `role` is set to `admin`
instead of the default `viewer`. The field is checked only when the
user record is *created* (first login) — changing it later has no
effect on existing users. Once an admin exists, they can promote other
users via the API.

If `initial_admin` is not set and no admin exists, blockyard operates
in a view-only state — users can log in and see public apps, but no
one can manage content. This is a safe default; the operator fixes it
by setting `initial_admin` and having that user log in.

#### User management API

Admins manage users via new API endpoints:

```
GET    /api/v1/users              — List all users
GET    /api/v1/users/{sub}        — Get a user
PATCH  /api/v1/users/{sub}        — Update role or active status
```

**List response:**
```json
[
  {
    "sub": "google-oauth2|abc123",
    "email": "alice@example.com",
    "name": "Alice",
    "role": "admin",
    "active": true,
    "last_login": "2026-03-14T10:00:00Z"
  }
]
```

**Update request:**
```json
{
  "role": "publisher"
}
```

or:

```json
{
  "active": false
}
```

Only admins can access these endpoints. An admin cannot demote or
deactivate themselves (prevents lockout).

#### What gets removed

- `role_mappings` table from SQLite schema.
- `RoleMappingCache` and its refresh logic.
- `RoleMappingRow`, `ListRoleMappings`, `UpsertRoleMapping`,
  `DeleteRoleMapping` from `internal/db/db.go`.
- `/api/v1/role-mappings` endpoints and handlers.
- Role mapping management UI.
- `seed_admin` concept (replaced by `initial_admin`).
- `DeriveRole` function (role is read directly from the user record,
  not derived from groups).
- `X-Shiny-Groups` header injection.
- `groups_claim` config field from `[oidc]` (IdP groups are no longer
  consumed by blockyard).

#### What gets added

- `users` table (schema above).
- OIDC callback upserts user record on every login.
- `X-Shiny-Access` header injection (replaces `X-Shiny-Groups`).
- `logged_in` as a third `access_type` option (alongside `acl` and
  `public`).
- `/api/v1/users` endpoints for admin user management.
- `initial_admin` config field.
- User management section in admin UI.

### Implementation Plan

1. **Users table and OIDC login update** — Add the `users` table.
   Update the OIDC login callback to upsert a user row with `sub`,
   `email`, and `name` from the ID token. On first login, check
   `initial_admin` config and set role to `admin` if matched. Update
   `CallerIdentity` construction to read `role` from the user record
   instead of deriving it from groups via `RoleMappingCache`.

2. **Remove role mapping infrastructure** — Drop the `role_mappings`
   table, `RoleMappingCache`, DB methods, API endpoints, and UI
   components. Remove `DeriveRole` and all group→role derivation logic.
   Remove `groups_claim` from `OidcConfig`. This is a clean removal —
   `CREATE TABLE IF NOT EXISTS` means the table simply won't be
   created.

3. **Replace `X-Shiny-Groups` with `X-Shiny-Access`** — Update the
   proxy identity injection middleware. Remove `X-Shiny-Groups`.
   Compute `X-Shiny-Access` from the user's system role, per-content
   ACL, and app `access_type` for the target app. Add `logged_in` to
   the `access_type` CHECK constraint and update `EvaluateAccess` to
   handle all three visibility levels. Update the hello-auth example
   app to read `X-Shiny-Access` instead of `X-Shiny-Groups`.

4. **User management API** — Add `GET /api/v1/users`,
   `GET /api/v1/users/{sub}`, `PATCH /api/v1/users/{sub}`. Admin-only.
   Self-demotion/deactivation guard.

5. **User management UI** — Add a user list page to the admin UI.
   Show all users with role, active status, last login. Allow role
   changes and deactivation.

6. **Update hello-auth example** — Remove role mapping setup from
   the example's docker-compose and README. Add `initial_admin` config
   pointing to one of the Dex static users.

---

## 2. Personal Access Tokens

*Depends on Section 1: the `users` table provides PAT ownership and
the `role` field provides authorization for PAT-authenticated
requests.*

### Problem

The control-plane API (`/api/v1/*`) uses a static bearer token for
non-browser access. This has three problems:

**A. Shared secret with no identity.** Every caller uses the same token.
There is no audit trail, no per-client revocation, and no way to scope
access. If the token leaks, the only remediation is rotating it for
everyone.

**B. Broken when OIDC is enabled.** `APIAuth`
(`internal/api/auth.go`) takes an either/or path: when OIDC is
configured, the static token fallback is in an unreachable `else`
branch. The hello-auth example is broken because of this.

**C. No path for human API access.** A user who authenticates via OIDC
in the browser has a session cookie, but no way to obtain a credential
for CLI or script use.

### Callers and their needs

| Caller | Example | Auth mechanism |
|---|---|---|
| Human via browser | Web UI | OIDC session cookie (existing, unchanged) |
| Human via CLI | `curl`, future CLI tool | PAT |
| CI/CD pipeline | GitHub Actions | PAT (created by a service account) |
| Admin script | `deploy.sh` | PAT |

Machine-to-machine auth uses PATs created by service accounts — regular
users in the IdP that represent machines rather than humans. Some IdPs
(Keycloak, Entra ID) have dedicated service account types; others use a
regular user with a descriptive name. No special codepath in blockyard.

### Design

#### `APIAuth` middleware

The middleware tries authentication sources in order:

1. **Session cookie** — if a valid OIDC session exists, use it.
2. **PAT** — if `Authorization: Bearer <token>` is present, hash the
   token and look it up in the PAT store.
3. **Reject** — 401.

Both mechanisms are accepted on all API paths. This is safe because
secret data never flows back through the blockyard API — blockyard
writes secrets to OpenBao, and the Shiny app reads them via vault tokens
injected by the proxy. This boundary is enforced by vault policy, not
application code.

JWT validation of raw IdP tokens is removed from the API auth path.
JWTs are validated during the OIDC login flow (session creation), not on
every API request.

#### Token format

Tokens use a recognizable prefix for identification and secret scanning:
`by_` followed by 32 cryptographically random bytes, base62-encoded.
Example: `by_7kJx9mQ2vR4nL8pW1tY6bH3cF5gD0aE`.

The prefix lets tools like GitHub secret scanning, truffleHog, and
`grep` identify leaked tokens.

#### Schema

One new table in `internal/db/db.go` (the `users` table is created in
Section 1):

```sql
CREATE TABLE IF NOT EXISTS personal_access_tokens (
    id           TEXT PRIMARY KEY,
    token_hash   BLOB NOT NULL UNIQUE,
    user_sub     TEXT NOT NULL REFERENCES users(sub),
    name         TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_pat_token_hash
    ON personal_access_tokens(token_hash);
CREATE INDEX IF NOT EXISTS idx_pat_user_sub
    ON personal_access_tokens(user_sub);
```

- `token_hash`: SHA-256 of the plaintext token. Only the hash is stored.
- `user_sub`: FK to the `users` table.
- `revoked`: soft-delete flag — revoked tokens remain for audit.

#### API endpoints

All endpoints require an authenticated session (OIDC cookie). PATs
cannot be used to create other PATs.

```
POST   /api/v1/users/me/tokens          — Create a new PAT
GET    /api/v1/users/me/tokens          — List caller's PATs (metadata only)
DELETE /api/v1/users/me/tokens/{id}     — Revoke a single PAT
DELETE /api/v1/users/me/tokens          — Revoke all PATs
```

**Create request:**
```json
{
  "name": "deploy-ci",
  "expires_in": "90d"
}
```

**Create response** (plaintext token shown exactly once):
```json
{
  "id": "...",
  "name": "deploy-ci",
  "token": "by_7kJx9mQ2vR4nL8pW1tY6bH3cF5gD0aE",
  "created_at": "...",
  "expires_at": "..."
}
```

**List response:**
```json
[
  {
    "id": "...",
    "name": "deploy-ci",
    "created_at": "...",
    "expires_at": "...",
    "last_used_at": "...",
    "revoked": false
  }
]
```

#### Validation flow

1. Extract bearer token from `Authorization` header.
2. Compute `SHA-256(token)`.
3. Query `personal_access_tokens` joined with `users` by `user_sub`.
4. Check: PAT exists, not revoked, not expired; user is active.
5. Read `role` directly from the user record.
6. Update `last_used_at` asynchronously (don't block the request).

No group→role derivation, no IdP sync. The user's role is managed in
blockyard (Section 1) and read directly at validation time. If an admin
demotes or deactivates a user, the change takes effect on the next PAT
request — no background sync required.

#### Scope

PATs are initially unscoped — they carry the full permissions of the
owning user. Scoped PATs (read-only, single-app, etc.) are a future
refinement.

#### Revocation

Setting `revoked = 1` on the token row. A "revoke all" button covers
the credential compromise case. Deactivating the user (Section 1) also
effectively revokes all their PATs since validation checks `active`.

#### Identity

`AuthSource` is simplified:

```go
const (
    AuthSourceSession AuthSource = iota // OIDC session cookie
    AuthSourcePAT                       // Personal Access Token
)
```

`AuthSourceStaticToken` and `AuthSourceJWT` are removed.

### Implementation Plan

1. **PAT schema and DB methods** — Add the `personal_access_tokens`
   table with FK to `users`. DB methods: `CreatePAT`,
   `LookupPATByHash` (joined with `users`), `ListPATsByUser`,
   `RevokePAT`, `RevokeAllPATs`, `UpdatePATLastUsed`.

2. **Token generation** — Implement `by_`-prefixed token generation
   with `crypto/rand` + base62 encoding. SHA-256 hashing for storage.

3. **API endpoints** — `POST/GET/DELETE /api/v1/users/me/tokens`
   handlers. Session-only auth (no PAT-to-create-PAT).

4. **Rewrite `APIAuth`** — Replace the current JWT/static-token
   either/or logic with: try session cookie → try PAT → reject.
   Remove `authenticateFromBearer` and all static token logic.

5. **Remove static token** — Delete `Server.Token` from config.
   Remove `BLOCKYARD_SERVER_TOKEN` from docker-compose files, examples,
   and docs.

6. **Update hello-auth example** — Demonstrate OIDC login → PAT
   creation → API access with PAT. Remove static token from the
   example's docker-compose.

7. **Web UI** — Token management page under user settings: create
   with name/expiry (show plaintext once), list active tokens with
   created/last-used dates, revoke individual or all.

---

## 3. Operational Endpoints and Untrusted Workloads

### Problem

Blockyard exposes operational endpoints (`/metrics`, `/healthz`,
`/readyz`) on the same HTTP listener as the application proxy and
control-plane API. This creates a tension: operational endpoints are
typically unauthenticated so that monitoring infrastructure (Prometheus
scrapers, load balancer health checks) can reach them without managing
credentials. But blockyard runs **arbitrary user-supplied code** inside
Shiny app containers, and those containers can reach the host listener.

The current architecture routes all traffic through a single
`http.Server` bound to `cfg.Server.Bind`. Workers run on per-container
bridge networks with access to the host — they already use this path
to call `POST /api/v1/credentials/vault` via `BLOCKYARD_API_URL`. Any
endpoint reachable without authentication is therefore also reachable
by every running Shiny app.

For `/healthz` and `/readyz` this is harmless — the response is a
static string. For `/metrics`, the Prometheus exposition format leaks
operational data: request rates, error counts, active worker counts,
audit buffer depth, and any custom metrics added in the future. On a
deployment with public apps (`access_type = 'public'`), the app author
is untrusted, and exposing this data is an information leak.

### Current State

The v1 implementation places `/metrics` behind `APIAuth` — requiring
either a valid session cookie or a PAT. This deviates from the
phase-1-6 design (which specified unauthenticated access) but is the
safer default given the threat model above. The trade-off is that
Prometheus must be configured with a PAT bearer token to scrape
metrics, which is operationally inconvenient but not blocking.

### Proper Fix

The clean solution is a **separate management listener** bound to an
internal-only address (e.g. `127.0.0.1:9100` or a Unix socket):

```toml
[server]
bind = "0.0.0.0:3838"            # public listener (proxy + API)
management_bind = "127.0.0.1:9100"  # internal listener (metrics, health)
```

The management listener serves `/metrics`, `/healthz`, and `/readyz`
without authentication. Because it binds to `127.0.0.1`, it is not
reachable from container bridge networks — only from the host itself
and any co-located monitoring agents. In Kubernetes (v2), the
management port maps to a pod port with a `Service` that only the
in-cluster Prometheus can reach.

This is a standard pattern (Kubernetes API server uses `--secure-port`
vs `--bind-address` for exactly this reason) and cleanly separates the
trust boundaries: the public listener handles untrusted traffic with
full auth, the management listener handles trusted infrastructure
traffic without auth.

### Design

A new optional `management_bind` field in `[server]`:

```toml
[server]
bind = "0.0.0.0:3838"
management_bind = "127.0.0.1:9100"
```

When set, a second `http.Server` starts on the management address and
serves `/healthz`, `/readyz`, and `/metrics` **without authentication**.
These endpoints are removed from the main listener entirely — the main
listener only serves the application proxy and control-plane API.

Because the management listener binds to a loopback address, it is
unreachable from container bridge networks. Only the host and
co-located monitoring agents (Prometheus, health check probes) can
reach it.

When `management_bind` is not set, current behavior is preserved:
`/healthz` and `/readyz` are unauthenticated on the main listener,
`/metrics` requires `APIAuth`.

On the management listener, `/readyz` always returns full per-component
check details (no auth gating). The listener is trusted by definition.

Graceful shutdown drains both listeners: the management listener is
shut down first (so health probes start failing, signaling load
balancers to stop sending traffic), then the main listener is drained.

### Implementation Plan

1. **Config** — Add `ManagementBind string` to `ServerConfig`. Env
   var: `BLOCKYARD_SERVER_MANAGEMENT_BIND`. No validation beyond
   non-empty when set (Go's `net.Listen` validates the address).

2. **Management router** — New `NewManagementRouter(srv)` function in
   `internal/api/router.go`. Registers `/healthz`, `/readyz` (with
   full detail exposure), and `/metrics` (when enabled). Minimal
   middleware: request logging only.

3. **Conditional main router** — When `ManagementBind` is set,
   `NewRouter` skips registering `/healthz`, `/readyz`, and `/metrics`.

4. **Readyz handler** — Add a `trusted` parameter to `readyzHandler`.
   When true (management listener), always include per-component
   checks in the response.

5. **Second HTTP server** — In `cmd/blockyard/main.go`, when
   `ManagementBind` is configured, create and start a second
   `http.Server`. Shutdown order: management listener first, then main
   listener (so health probes fail before traffic stops).

6. **Tests** — Config field parsing and env var override. Management
   router serves expected endpoints without auth. Main router omits
   ops endpoints when management bind is set.

7. **Docs** — Update config reference, observability guide, and
   example TOML files.

---

## 4. Secret Lifecycle

### Problem

Blockyard requires two categories of secrets at runtime:

**A. The vault credential.** The server authenticates to OpenBao with a
static `admin_token` — a long-lived privileged token stored in the
config file. There is no renewal: if the token has a TTL and expires,
all vault operations fail silently. The token also has no scoping — it
is typically a root or highly-privileged token, and if the config file
leaks, the blast radius is the entire vault.

**B. Server-owned secrets** that blockyard generates and consumes
itself. `session_secret` is the primary example — it's the HMAC seed
for session cookies and worker session tokens. Today it must be
manually configured, and if omitted the server refuses to start. This
is operationally annoying for a value the operator has no reason to
choose themselves.

Both problems stem from the same gap: blockyard has no secret lifecycle
management for its own credentials.

### Design

#### 4A. AppRole auth for the server's vault connection

Replace the static `admin_token` with AppRole authentication. AppRole
is Vault/OpenBao's machine-oriented auth method — designed exactly for
this use case.

The key principle: **persist the vault token, not the login
credential.** The `secret_id` is a one-time bootstrap input, not a
permanent config value. Once authenticated, the server sustains its
own vault access indefinitely through token renewal.

**Startup flow:**

1. Check for a persisted vault token at
   `{data_dir}/.vault-token`. If found, try
   `POST /v1/auth/token/renew-self`. If renewal succeeds, use this
   token — no AppRole login needed, no `secret_id` needed.
2. If no persisted token or renewal fails: read `role_id` from config
   and `secret_id` from the `BLOCKYARD_OPENBAO_SECRET_ID` env var.
   Call `POST /v1/auth/approle/login`. Persist the returned token.
3. If neither path succeeds (no persisted token, no env var), exit
   with: `"vault bootstrap required: set BLOCKYARD_OPENBAO_SECRET_ID"`.

After authentication, a background goroutine renews the token at
`ttl / 2` intervals and re-persists it after each renewal. The
`Client` struct's `adminTokenFunc` callback returns the current token,
so renewal is transparent to all callers.

**Steady state:** the server renews its own token indefinitely. The
AppRole's `token_max_ttl` should be `0` (unlimited renewals) — this
is the correct configuration for a long-lived server role.

**Extended downtime:** if the server is down long enough for the
persisted token to expire beyond renewal, the operator re-delivers a
`secret_id` via env var. This is an explicit operational event (redeploy
or restart with the env var set), not routine maintenance.

**Config:**

```toml
[openbao]
address = "http://openbao:8200"
role_id = "blockyard-server"     # not secret — identifies the role
# secret_id is NEVER in config — delivered once via env var at bootstrap
# admin_token: deprecated, still accepted for migration
```

`role_id` is safe in config — it's a role identifier, like a username.
`admin_token` remains accepted but is deprecated. If both `admin_token`
and `role_id` are set, the server rejects the config.

**Token persistence:** the vault token is written to
`{data_dir}/.vault-token` (mode `0600`, atomic write via temp +
rename). This file contains a scoped, time-limited, renewable token —
not a root credential. If it leaks, the blast radius is limited to
blockyard's KV namespace and the token can be revoked in vault.

**Policy scoping:** the AppRole role gets a policy granting exactly
what blockyard needs — no more:

```hcl
# Read/write user secrets under the blockyard namespace
path "secret/data/blockyard/*" {
  capabilities = ["create", "read", "update", "delete"]
}
path "secret/metadata/blockyard/*" {
  capabilities = ["read", "list"]
}

# Bootstrap checks
path "sys/auth" {
  capabilities = ["read"]
}
path "sys/mounts" {
  capabilities = ["read"]
}
path "sys/policies/acl/*" {
  capabilities = ["read"]
}

# JWT auth role management (for bootstrap verification)
path "auth/{{jwt_auth_path}}/role/blockyard-user" {
  capabilities = ["read"]
}

# Token self-renewal
path "auth/token/renew-self" {
  capabilities = ["update"]
}
```

**Failure modes and observability:**

- Renewal failure: retries with exponential backoff (1s → 2s → 4s →
  ... → 60s cap). Logs at `warn` on each failure. If the token
  becomes unrenewable (max TTL exceeded or revoked), logs at `error`
  and vault-dependent operations start failing. The server continues
  running — non-vault features (app proxy, API) are unaffected.
- The `/readyz` health check reports vault token status. A stale
  token degrades readiness, signaling the operator to re-bootstrap.

#### 4B. Vault references and auto-generated secrets

Two changes to how blockyard handles secrets in config:

1. Any `Secret` config field can reference a vault path instead of
   holding a literal value.
2. `session_secret` can be omitted entirely — blockyard auto-generates
   and persists it.

##### Vault references

Any `Secret` field in the config accepts a `vault:` prefix that tells
blockyard to read the value from OpenBao at startup:

```toml
[oidc]
client_secret = "vault:secret/data/blockyard/oidc#client_secret"
```

Format: `vault:{kv_path}#{key}` — the KV v2 data path and the JSON
key within it. At startup, blockyard resolves all `vault:` references
before the rest of init runs. If a reference can't be resolved (vault
unreachable, path missing, key missing), the server exits with a clear
error naming the field and path.

This keeps secrets out of config files entirely. The operator writes
them to vault once (via CLI or UI), and the config points to where
they live. The config is self-documenting — you see exactly where each
secret comes from.

Resolution happens in `Secret.Resolve(vaultClient)`. A `Secret` value
that does not start with `vault:` is treated as a literal, unchanged
from current behavior. `Secret.Expose()` returns an error if called on
an unresolved vault reference — this prevents accidentally using the
raw `vault:...` string as a secret value if resolution is skipped or
reordered.

##### Auto-generated `session_secret`

`session_secret` is special: blockyard owns this value and the
operator has no reason to choose it. If omitted from config, blockyard
auto-generates a 32-byte random value and persists it for reuse:

- **With OpenBao:** read from `secret/data/blockyard/server-secrets`.
  If the key is missing, generate, write, and proceed.
- **Without OpenBao:** `session_secret` must be set explicitly via
  config or env var. Missing it is a startup error.
- **Explicit config/env var:** always wins — no vault lookup.

**Validation change:** `session_secret` is no longer required in config
when `[oidc]` is set and OpenBao is configured. The startup sequence
resolves it before OIDC initialization. Without OpenBao, the existing
validation ("session_secret required") remains.

**Logging:** when a secret is auto-generated, log at `info` level.
Never log the value.

### Implementation Plan

1. **Config changes** — Add `RoleID` field to `OpenbaoConfig`.
   `SecretID` is not a config field — it is read from
   `BLOCKYARD_OPENBAO_SECRET_ID` env var at startup only. Validation:
   reject if both `admin_token` and `role_id` are set. Deprecation
   warning when `admin_token` is used.

2. **Token persistence** — New `internal/integration/tokenfile.go`.
   Atomic read/write of `{data_dir}/.vault-token` (temp + rename,
   mode `0600`). The `data_dir` is derived from `database.path`.

3. **AppRole auth with token reuse** — New
   `internal/integration/approle.go`. Startup flow: try persisted
   token → try AppRole login with env var `secret_id` → fail with
   actionable error. `AppRoleLogin(ctx, addr, roleID, secretID)`
   returns a token and TTL. Update `NewClient` to accept either a
   static token func or an AppRole config. When AppRole is used,
   `adminTokenFunc` returns the current renewable token.

4. **Token renewal goroutine** — Background loop in
   `internal/integration/renew.go`. Renews at `ttl/2`. Re-persists
   token after each successful renewal. On failure, retries with
   exponential backoff (1s → 60s cap). On unrenewable token, logs
   `error` — vault operations degrade but the server stays up. Takes
   a context for clean shutdown.

5. **Readyz integration** — Report vault token status in `/readyz`.
   Stale or missing token degrades readiness.

6. **Vault references in `Secret` type** — Add a `Resolve(vaultClient)`
   method to `Secret`. If the value starts with `vault:`, parse
   `{kv_path}#{key}`, call `KVRead`, and replace the secret's internal
   value with the result. New `internal/config/resolve.go` with
   `ResolveSecrets(cfg *Config, vaultClient *Client)` that walks all
   `Secret` fields in the config and resolves vault references. Vault
   client initialization is idempotent — `ResolveSecrets` gets or
   initializes the client itself, eliminating ordering concerns.

7. **Auto-generated `session_secret`** — After vault references are
   resolved, if `session_secret` is still empty and vault is
   configured: read from `secret/data/blockyard/server-secrets`
   (generate + write if missing). Without vault, `session_secret` must
   be set explicitly — the existing validation error remains.

8. **Startup sequence update** — `main.go` order becomes: parse config
   → init vault client (persisted token or AppRole) → start renewal
   goroutine → resolve vault references → resolve auto-generated
   secrets → init OIDC → proceed.

9. **Tests** — Token persistence round-trip. AppRole login mock.
    Persisted token reuse on restart. Renewal behavior and backoff.
    Config validation for mutual exclusivity. Vault reference
    resolution (valid path, missing path, missing key, no vault
    client). Auto-generation with vault backend.

10. **Docs** — Update config reference with `vault:` prefix syntax.
    Add operator guide: initial bootstrap with `secret_id` env var,
    re-bootstrap after extended downtime, storing secrets in vault.
    Migration guide from `admin_token`. Update hello-auth example.
