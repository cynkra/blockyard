# v1 Wrap-Up

Two items remain before v1 can ship: the role mapping system should be
replaced with direct role management in blockyard, and the API needs
Personal Access Tokens to replace the static bearer token. Section 2
depends on Section 1 — the `users` table and role model introduced in
Section 1 are the foundation for PAT ownership and validation.

The design principle behind both changes: the **IdP handles
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
| Has viewer ACL, or app is `logged_in` and user is authenticated | `viewer` |
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
