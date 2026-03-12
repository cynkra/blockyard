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
cookies, `AuthenticatedUser` in request context). The static bearer
token is retained as a fallback when `[oidc]` is not configured (dev mode).

## Design decision: unified CallerIdentity

Both the app plane (session cookie) and control plane (Bearer JWT) need a
common identity type for authorization checks. Phase 1-2 introduces
`CallerIdentity` — a lightweight struct carrying `Sub`, `Groups`, and
`Role`. Both auth middlewares produce it; all authorization code consumes it.

`CallerIdentity` lives in `internal/auth/identity.go` alongside phase 1-1's
`AuthenticatedUser`. The two types coexist in app-plane request contexts:
`AuthenticatedUser` carries the IdP access token (needed by phase 1-3 for
OpenBao credential injection), while `CallerIdentity` carries the derived
role (needed for authz checks). Control-plane requests only have
`CallerIdentity`.

**Static-token identity:** when `[oidc]` is not configured, the static
bearer token middleware injects a `CallerIdentity` with
`Sub = "admin"`, empty groups, and `Role = RoleAdmin`. This is a hardcoded
sentinel — static-token mode is dev/single-operator only and will be
superseded once OIDC is configured. Apps created via static token get
`owner = "admin"`.

## Design decision: role derivation from IdP groups

Roles are not stored per-user in blockyard's database. Instead, the
`role_mappings` table maps IdP group names to blockyard roles. When a user
authenticates (OIDC session or JWT), their groups claim is checked against
`role_mappings` and the highest-privilege match wins. If no groups match,
the user has `RoleNone` — they can only access apps explicitly granted
to them via ACL.

**Rationale:** role changes happen in the IdP (group membership) and are
reflected on next authentication — no sync protocol, no user table, no
"promote user" API that drifts from the IdP. The trade-off: operators must
manage group-to-role mappings, and role changes require re-authentication
to take effect.

**Machine clients (client credentials):** the IdP can include a groups
claim in client credentials tokens (configurable in Keycloak, Auth0, etc.).
Machine clients are assigned roles through the same group-to-role mapping.
If a machine client's token has no groups claim, it gets `RoleNone`.
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

## Design decision: public (anonymous) access

Apps have an `access_type` column with two values:

- **`acl`** (default) — only authenticated users with an explicit grant
  (owner, admin, ACL entry) can access the app via the proxy or see it in
  the catalog. Unauthenticated requests are redirected to `/login`.
- **`public`** — anyone can access the app via the proxy, including
  unauthenticated users. Public apps appear in the catalog for all callers
  (authenticated or not). Identity headers (`X-Shiny-User`,
  `X-Shiny-Groups`) are injected when the user happens to be
  authenticated; absent otherwise.

This matches Posit Connect's four-level access model (Anonymous, Viewer,
Publisher, Administrator). In Connect, content can be set to "Anyone — no
login required"; `access_type = 'public'` is the blockyard equivalent.

**App-plane auth middleware change:** phase 1-1's middleware does a hard
redirect to `/login` for unauthenticated requests. Phase 1-2 softens it:
the middleware becomes "authenticate if possible, but don't require it."
The enforcement decision moves to the proxy handler's ACL check (Step 13),
which examines the app's `access_type`:

- `acl` app + no caller → 404 (or redirect to `/login` for browser
  requests)
- `acl` app + caller → normal ACL evaluation
- `public` app + no caller → `RelationAnonymous` (proxy access only)
- `public` app + caller → normal ACL evaluation (they may be
  owner/admin/collaborator with elevated permissions)

**Setting access_type:** publishers and owners set it via
`PATCH /api/v1/apps/{id}` with `{"access_type": "public"}`. Only owners
and admins can change an app's access type. The default is `acl`.

## Deliverables

1. `CallerIdentity` and `Role` types in `internal/auth/identity.go`
2. `internal/authz/` package — permission checks, content-role evaluation, ACL logic
3. JWKS cache + JWT validation (`internal/auth/jwt.go`)
4. Control-plane auth middleware — validate Bearer tokens as JWTs, fall
   back to static token when OIDC is not configured
5. App-plane auth middleware extension — derive role, insert
   `CallerIdentity` alongside `AuthenticatedUser`
6. Schema migration — `app_access` and `role_mappings` tables
7. `owner` column on `apps` table (via migration consolidation)
8. Role mapping cache — in-memory `sync.RWMutex` + `map[string]Role`,
   loaded on startup, updated on writes
9. Authorization guards on all existing API endpoints
10. Per-content ACL check on proxy routes (including public/anonymous
    access bypass for `access_type = 'public'` apps)
11. App-plane auth middleware softened to "authenticate if possible"
    (enforcement deferred to proxy ACL check)
12. `access_type` column on `apps` table (`'acl'` default, `'public'`
    for anonymous access)
13. ACL management API — `POST/GET/DELETE /api/v1/apps/{id}/access`
14. Role mapping management API — `GET/PUT/DELETE /api/v1/role-mappings`
15. New dependency: `github.com/golang-jwt/jwt/v5`

## Step-by-step

### Step 1: New dependencies

Add to `go.mod`:

```
github.com/golang-jwt/jwt/v5 v5.2.1
```

**Dependency rationale:**

- **golang-jwt/jwt** — JWT decode + signature validation against JWKS. The
  `coreos/go-oidc` package (phase 1-1) handles ID token validation
  internally during the callback flow, but control-plane Bearer tokens are
  plain JWTs (not OIDC ID tokens) and need direct validation. The
  `golang-jwt` library is the de facto standard Go JWT library with JWKS
  support via `jwt.Keyfunc`.
- **net/http** (stdlib) — HTTP client for fetching the raw JWKS JSON from
  the IdP's `jwks_uri`. Already used elsewhere in the project.

### Step 2: Prerequisite — migration consolidation

The v1 plan calls for consolidating the embedded schema in `internal/db/db.go`
before v0.1.0. Phase 1-2 is the first phase that adds schema changes, so
consolidation happens now.

**Updated schema** in `internal/db/db.go` — the consolidated schema,
including the `owner` column needed by this phase:

```sql
CREATE TABLE IF NOT EXISTS apps (
    id                      TEXT PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    owner                   TEXT NOT NULL DEFAULT 'admin',
    access_type             TEXT NOT NULL DEFAULT 'acl' CHECK (access_type IN ('acl', 'public')),
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
    uploaded_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_bundles_app_id ON bundles(app_id);
```

`owner` has `DEFAULT 'admin'` as a safety net — the code always sets it
explicitly, but the default prevents insert failures during development if
a code path is missed. The value `'admin'` matches the static-token
sentinel identity.

**Developer migration:** since we're pre-release with no external consumers,
developers delete their local databases and let the schema be recreated. No
upgrade path is maintained.

### Step 3: Role types + CallerIdentity

New file: `internal/auth/identity.go`

```go
package auth

// Role is a system-level role derived from IdP groups via role_mappings.
// Ordered by privilege — higher value means more privilege.
type Role int

const (
	RoleNone      Role = iota // No mapped role
	RoleViewer                // Can view granted apps
	RolePublisher             // Can create + manage own apps
	RoleAdmin                 // Full access to everything
)

// String returns the lowercase name of the role.
func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RolePublisher:
		return "publisher"
	case RoleAdmin:
		return "admin"
	default:
		return "none"
	}
}

// ParseRole converts a string to a Role. Returns RoleNone for unrecognized values.
func ParseRole(s string) Role {
	switch s {
	case "admin":
		return RoleAdmin
	case "publisher":
		return RolePublisher
	case "viewer":
		return RoleViewer
	default:
		return RoleNone
	}
}

// CanCreateApp reports whether this role can create new apps.
func (r Role) CanCreateApp() bool {
	return r >= RolePublisher
}

// CanViewAllApps reports whether this role can see all apps regardless
// of ownership or grants.
func (r Role) CanViewAllApps() bool {
	return r >= RoleAdmin
}

// CanManageRoles reports whether this role can manage role mappings.
func (r Role) CanManageRoles() bool {
	return r >= RoleAdmin
}

// AuthSource describes how the caller authenticated. Informational — not
// used for permission decisions, but useful for audit logging (phase 1-6).
type AuthSource int

const (
	AuthSourceSession    AuthSource = iota // Browser session via OIDC (phase 1-1)
	AuthSourceJWT                          // JWT Bearer token (client credentials)
	AuthSourceStaticToken                  // Static bearer token (v0 compat, dev mode)
)

// CallerIdentity is the unified caller identity produced by both auth
// middlewares. Stored in request context for use by authorization checks.
type CallerIdentity struct {
	Sub    string
	Groups []string
	Role   Role
	Source AuthSource
}
```

**Context helpers** (also in `internal/auth/identity.go`):

```go
type contextKey int

const callerKey contextKey = iota

// ContextWithCaller returns a new context carrying the given CallerIdentity.
func ContextWithCaller(ctx context.Context, c *CallerIdentity) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerFromContext extracts the CallerIdentity from the context.
// Returns nil if no identity is present.
func CallerFromContext(ctx context.Context) *CallerIdentity {
	c, _ := ctx.Value(callerKey).(*CallerIdentity)
	return c
}
```

**Role derivation helper** (also in `internal/auth/identity.go`):

```go
// DeriveRole determines the effective role for a set of groups by looking
// up each group in the role mapping cache and taking the highest-privilege match.
func DeriveRole(groups []string, cache *RoleMappingCache) Role {
	best := RoleNone
	for _, g := range groups {
		if r, ok := cache.Get(g); ok && r > best {
			best = r
		}
	}
	return best
}
```

**Tests:**

- `Role` ordering: `RoleNone < RoleViewer < RolePublisher < RoleAdmin`
- `DeriveRole` with no matching groups returns `RoleNone`
- `DeriveRole` with one match returns that role
- `DeriveRole` with multiple matches returns highest privilege
- `CanCreateApp`: true for Admin + Publisher, false for Viewer + None
- `CanViewAllApps`: true only for Admin
- `CanManageRoles`: true only for Admin
- `CallerFromContext` round-trips through `ContextWithCaller`

### Step 4: Authorization types + ACL evaluation

New file: `internal/authz/rbac.go`

```go
package authz

// ContentRole is a per-content role granted via the app_access table.
// Ordered by privilege for max-wins resolution.
type ContentRole int

const (
	ContentRoleViewer       ContentRole = iota // Can use the app via proxy
	ContentRoleCollaborator                    // Can deploy, start/stop, update config
)

// String returns the lowercase name of the content role.
func (r ContentRole) String() string {
	switch r {
	case ContentRoleCollaborator:
		return "collaborator"
	default:
		return "viewer"
	}
}

// ParseContentRole converts a string to a ContentRole.
// Returns ContentRoleViewer and false for unrecognized values.
func ParseContentRole(s string) (ContentRole, bool) {
	switch s {
	case "collaborator":
		return ContentRoleCollaborator, true
	case "viewer":
		return ContentRoleViewer, true
	default:
		return ContentRoleViewer, false
	}
}

// AppRelation is the effective relationship between a caller and a specific
// app. Determines what operations the caller can perform. Computed from
// system role + ownership + ACL grants.
type AppRelation int

const (
	RelationNone                AppRelation = iota // No access at all
	RelationAnonymous                              // Public app, unauthenticated user
	RelationContentViewer                          // Per-content viewer (ACL grant)
	RelationContentCollaborator                    // Per-content collaborator (ACL grant)
	RelationOwner                                  // App owner
	RelationAdmin                                  // System admin
)

// CanAccessProxy reports whether this relation allows using the app via proxy.
func (r AppRelation) CanAccessProxy() bool {
	return r > RelationNone
}

// CanDeploy reports whether this relation allows deploying bundles.
func (r AppRelation) CanDeploy() bool {
	return r >= RelationContentCollaborator
}

// CanStartStop reports whether this relation allows starting/stopping the app.
func (r AppRelation) CanStartStop() bool {
	return r >= RelationContentCollaborator
}

// CanUpdateConfig reports whether this relation allows updating app config.
func (r AppRelation) CanUpdateConfig() bool {
	return r >= RelationContentCollaborator
}

// CanDelete reports whether this relation allows deleting the app.
func (r AppRelation) CanDelete() bool {
	return r >= RelationOwner
}

// CanManageACL reports whether this relation allows managing ACL grants.
func (r AppRelation) CanManageACL() bool {
	return r >= RelationOwner
}

// CanViewDetails reports whether this relation allows viewing app details.
func (r AppRelation) CanViewDetails() bool {
	return r > RelationNone
}
```

**ACL evaluation** — new file: `internal/authz/acl.go`

```go
package authz

import (
	"github.com/cynkra/blockyard/internal/auth"
)

// AccessKind distinguishes user grants from group grants.
type AccessKind string

const (
	AccessKindUser  AccessKind = "user"
	AccessKindGroup AccessKind = "group"
)

// AccessGrant represents a row from the app_access table.
type AccessGrant struct {
	AppID     string
	Principal string
	Kind      AccessKind
	Role      ContentRole
	GrantedBy string
	GrantedAt string
}

// EvaluateAccess determines the caller's relationship to a specific app.
//
// Evaluation order:
//  0. Public app + nil caller -> RelationAnonymous
//  1. System admin -> RelationAdmin (overrides all)
//  2. App owner -> RelationOwner
//  3. Explicit ACL grants (user + group) -> highest content role
//  4. Public app + authenticated caller with no grants -> RelationAnonymous
//  5. No match -> RelationNone
//
// accessType is the app's access_type column ("acl" or "public").
// caller may be nil for unauthenticated requests to public apps.
func EvaluateAccess(
	caller *auth.CallerIdentity,
	appOwner string,
	grants []AccessGrant,
	accessType string,
) AppRelation {
	// 0. Unauthenticated caller — only allowed on public apps
	if caller == nil {
		if accessType == "public" {
			return RelationAnonymous
		}
		return RelationNone
	}

	// 1. System admin
	if caller.Role == auth.RoleAdmin {
		return RelationAdmin
	}

	// 2. Owner
	if caller.Sub == appOwner {
		return RelationOwner
	}

	// 3. ACL grants — collect all matching grants and take max role
	best := ContentRole(-1) // sentinel below ContentRoleViewer
	found := false
	for _, g := range grants {
		match := false
		switch g.Kind {
		case AccessKindUser:
			match = g.Principal == caller.Sub
		case AccessKindGroup:
			for _, cg := range caller.Groups {
				if cg == g.Principal {
					match = true
					break
				}
			}
		}
		if match && g.Role > best {
			best = g.Role
			found = true
		}
	}

	if found {
		switch best {
		case ContentRoleCollaborator:
			return RelationContentCollaborator
		default:
			return RelationContentViewer
		}
	}

	// 4. Public app — authenticated caller with no explicit grants
	//    still gets proxy access (same as anonymous, but identity
	//    headers will be injected)
	if accessType == "public" {
		return RelationAnonymous
	}

	return RelationNone
}
```

**Tests:**

- Nil caller + `acl` app returns `RelationNone`
- Nil caller + `public` app returns `RelationAnonymous`
- Admin caller returns `RelationAdmin` regardless of ownership/grants
- Owner caller returns `RelationOwner`
- User with direct viewer grant returns `RelationContentViewer`
- User with direct collaborator grant returns `RelationContentCollaborator`
- User with viewer grant via group returns `RelationContentViewer`
- User with both viewer (direct) and collaborator (group) returns
  `RelationContentCollaborator` (max)
- User with no grants + `acl` app returns `RelationNone`
- User with no grants + `public` app returns `RelationAnonymous`
- Admin who is also owner returns `RelationAdmin` (admin takes precedence)
- `RelationAnonymous.CanAccessProxy()` returns true
- `RelationAnonymous.CanDeploy()` returns false
- `AppRelation` permission methods: all combinations tested

### Step 5: Schema migration + DB access layer

**New schema added to `internal/db/db.go`** (appended to the existing
`schema` const):

```sql
-- Per-content access grants.
-- A principal (user or group) is granted a content role on an app.
CREATE TABLE IF NOT EXISTS app_access (
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
CREATE TABLE IF NOT EXISTS role_mappings (
    group_name  TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'publisher', 'viewer')),
    PRIMARY KEY (group_name)
);
```

**DB access layer additions** (in `internal/db/db.go`):

```go
// --- App changes ---

// CreateApp creates an app with an explicit owner.
func (db *DB) CreateApp(name, owner string) (*AppRow, error) {
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := db.Exec(
		`INSERT INTO apps (id, name, owner, max_sessions_per_worker, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		id, name, owner, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert app: %w", err)
	}

	return db.GetApp(id)
}

// --- Role mappings ---

// RoleMappingRow represents a row from the role_mappings table.
type RoleMappingRow struct {
	GroupName string
	Role      string
}

func (db *DB) ListRoleMappings() ([]RoleMappingRow, error) {
	rows, err := db.Query("SELECT group_name, role FROM role_mappings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []RoleMappingRow
	for rows.Next() {
		var m RoleMappingRow
		if err := rows.Scan(&m.GroupName, &m.Role); err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, rows.Err()
}

func (db *DB) UpsertRoleMapping(groupName, role string) error {
	_, err := db.Exec(
		`INSERT INTO role_mappings (group_name, role) VALUES (?, ?)
		 ON CONFLICT (group_name) DO UPDATE SET role = excluded.role`,
		groupName, role,
	)
	return err
}

func (db *DB) DeleteRoleMapping(groupName string) (bool, error) {
	result, err := db.Exec(
		"DELETE FROM role_mappings WHERE group_name = ?", groupName,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// --- App access (ACL) ---

// AppAccessRow represents a row from the app_access table.
type AppAccessRow struct {
	AppID     string
	Principal string
	Kind      string
	Role      string
	GrantedBy string
	GrantedAt string
}

func (db *DB) ListAppAccess(appID string) ([]AppAccessRow, error) {
	rows, err := db.Query(
		"SELECT app_id, principal, kind, role, granted_by, granted_at FROM app_access WHERE app_id = ?",
		appID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []AppAccessRow
	for rows.Next() {
		var g AppAccessRow
		if err := rows.Scan(&g.AppID, &g.Principal, &g.Kind, &g.Role, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

func (db *DB) GrantAppAccess(appID, principal, kind, role, grantedBy string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO app_access (app_id, principal, kind, role, granted_by, granted_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app_id, principal, kind)
		 DO UPDATE SET role = excluded.role,
		               granted_by = excluded.granted_by,
		               granted_at = excluded.granted_at`,
		appID, principal, kind, role, grantedBy, now,
	)
	return err
}

func (db *DB) RevokeAppAccess(appID, principal, kind string) (bool, error) {
	result, err := db.Exec(
		"DELETE FROM app_access WHERE app_id = ? AND principal = ? AND kind = ?",
		appID, principal, kind,
	)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}
```

See Step 10 for `ListAccessibleApps` (the filtered list query used by
`GET /apps`).

**`AppRow` changes:**

```go
type AppRow struct {
	ID                   string
	Name                 string
	Owner                string   // new
	AccessType           string   // new: "acl" (default) or "public"
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

- `CreateApp` sets owner correctly
- `ListRoleMappings` returns inserted mappings
- `UpsertRoleMapping` inserts new + updates existing
- `DeleteRoleMapping` returns true on success, false on no-op
- `GrantAppAccess` inserts new grant
- `GrantAppAccess` upserts on conflict (updates role)
- `RevokeAppAccess` returns true on success, false on no-op
- `ListAppAccess` returns all grants for an app

### Step 6: Role mapping cache

The role-mapping lookup happens on every authenticated request (both
middlewares). Querying the database per-request is wasteful for data that
changes rarely. An in-memory cache loaded on startup and updated on writes
avoids this.

New file: `internal/auth/rolecache.go`

```go
package auth

import (
	"sync"

	"github.com/cynkra/blockyard/internal/db"
)

// RoleMappingCache is an in-memory cache of group -> role mappings.
// Loaded from the database at startup. Updated synchronously when
// role mappings are modified via the management API.
type RoleMappingCache struct {
	mu       sync.RWMutex
	mappings map[string]Role
}

// NewRoleMappingCache creates an empty cache.
func NewRoleMappingCache() *RoleMappingCache {
	return &RoleMappingCache{
		mappings: make(map[string]Role),
	}
}

// Load populates the cache from the database.
func (c *RoleMappingCache) Load(database *db.DB) error {
	rows, err := database.ListRoleMappings()
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.mappings = make(map[string]Role, len(rows))
	for _, row := range rows {
		role := ParseRole(row.Role)
		if role != RoleNone {
			c.mappings[row.GroupName] = role
		}
	}
	return nil
}

// Get looks up the role for a group name.
func (c *RoleMappingCache) Get(group string) (Role, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.mappings[group]
	return r, ok
}

// Set updates a mapping (called after DB write).
func (c *RoleMappingCache) Set(group string, role Role) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mappings[group] = role
}

// Remove deletes a mapping (called after DB delete).
func (c *RoleMappingCache) Remove(group string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.mappings, group)
}
```

**Server struct addition** (in `internal/server/state.go`):

```go
type Server struct {
	// ... existing fields ...
	RoleCache *auth.RoleMappingCache // new
}
```

Initialized in `cmd/blockyard/main.go` after DB creation:

```go
roleCache := auth.NewRoleMappingCache()
if err := roleCache.Load(database); err != nil {
	slog.Error("failed to load role mappings", "error", err)
	os.Exit(1)
}
```

### Step 7: JWKS cache + JWT validation

New file: `internal/auth/jwt.go` — JWKS fetching, caching, and JWT
validation for control-plane Bearer tokens.

Phase 1-1's `coreos/go-oidc` package handles JWKS internally for ID token
validation during the callback flow. Control-plane tokens are plain JWTs
(from the client credentials flow), not OIDC ID tokens, and need direct
validation via `golang-jwt/jwt`. This requires fetching the raw JWKS JSON
from the IdP's `jwks_uri` (discovered in phase 1-1) and caching it.

```go
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWKSCache caches the IdP's JSON Web Key Set for JWT validation on the
// control plane.
type JWKSCache struct {
	mu          sync.RWMutex
	keys        map[string]any // kid -> parsed public key
	jwksURI     string
	httpClient  *http.Client
	lastRefresh time.Time
}

// refreshCooldown is the minimum time between JWKS refreshes
// (prevents hammering the IdP).
const refreshCooldown = 60 * time.Second

// NewJWKSCache fetches the JWKS from the IdP's jwks_uri and initializes
// the cache. Called once at startup, alongside OIDC discovery.
func NewJWKSCache(jwksURI string) (*JWKSCache, error) {
	c := &JWKSCache{
		keys:       make(map[string]any),
		jwksURI:    jwksURI,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	if err := c.fetchKeys(); err != nil {
		return nil, fmt.Errorf("initial JWKS fetch: %w", err)
	}
	c.lastRefresh = time.Now()
	return c, nil
}

// Refresh re-fetches the JWKS from the IdP. No-op if called within the
// cooldown period. Returns true if the keys were actually refreshed.
func (c *JWKSCache) Refresh() (bool, error) {
	c.mu.RLock()
	elapsed := time.Since(c.lastRefresh)
	c.mu.RUnlock()

	if elapsed < refreshCooldown {
		return false, nil
	}

	if err := c.fetchKeys(); err != nil {
		return false, err
	}

	c.mu.Lock()
	c.lastRefresh = time.Now()
	c.mu.Unlock()
	return true, nil
}

// Validate parses and validates a JWT, returning its claims.
// On kid-not-found, refreshes the JWKS once and retries.
func (c *JWKSCache) Validate(tokenStr, issuer, audience string) (*JWTClaims, error) {
	claims, err := c.tryValidate(tokenStr, issuer, audience)
	if err != nil {
		// If the error indicates an unknown key, try refreshing
		if errors.Is(err, ErrKidNotFound) {
			if _, refreshErr := c.Refresh(); refreshErr != nil {
				return nil, refreshErr
			}
			return c.tryValidate(tokenStr, issuer, audience)
		}
		return nil, err
	}
	return claims, nil
}

// ErrKidNotFound is returned when a JWT's kid does not match any key in the JWKS.
var ErrKidNotFound = errors.New("kid not found in JWKS")

func (c *JWKSCache) tryValidate(tokenStr, issuer, audience string) (*JWTClaims, error) {
	claims := &JWTClaims{}

	_, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, fmt.Errorf("missing kid in token header")
		}

		c.mu.RLock()
		key, found := c.keys[kid]
		c.mu.RUnlock()

		if !found {
			return nil, ErrKidNotFound
		}
		return key, nil
	},
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}),
	)
	if err != nil {
		return nil, err
	}

	return claims, nil
}

func (c *JWKSCache) fetchKeys() error {
	resp, err := c.httpClient.Get(c.jwksURI)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}

	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]any, len(jwks.Keys))
	for _, raw := range jwks.Keys {
		var header struct {
			Kid string `json:"kid"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || header.Kid == "" {
			continue
		}
		key, err := jwt.ParseRSAPublicKeyFromPEM(raw)
		if err != nil {
			// Try parsing as JWK instead of PEM
			// The actual implementation will use a JWK parser
			// to convert the JSON key to *rsa.PublicKey
			slog.Debug("skipping unparseable JWK", "kid", header.Kid)
			continue
		}
		keys[header.Kid] = key
	}

	c.mu.Lock()
	c.keys = keys
	c.mu.Unlock()
	return nil
}

// JWTClaims holds the claims extracted from a validated JWT.
type JWTClaims struct {
	jwt.RegisteredClaims
	Groups []string `json:"groups,omitempty"`
	// Extra holds additional claims for non-standard group claim names.
	Extra map[string]any `json:"-"`
}

// ExtractGroups extracts groups from the configured claim name.
// Checks the typed Groups field first, then falls back to the extra
// claims map (for non-standard claim names like "cognito:groups").
func (c *JWTClaims) ExtractGroups(groupsClaim string) []string {
	// If the configured claim is "groups" and the typed field has values, use it
	if groupsClaim == "groups" && len(c.Groups) > 0 {
		return c.Groups
	}

	// Otherwise, check the extra claims map
	raw, ok := c.Extra[groupsClaim]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	groups := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			groups = append(groups, s)
		}
	}
	return groups
}

// UnmarshalJSON implements custom unmarshaling to capture extra claims.
func (c *JWTClaims) UnmarshalJSON(data []byte) error {
	// First unmarshal the known fields
	type Alias JWTClaims
	aux := &struct {
		*Alias
	}{Alias: (*Alias)(c)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	// Then unmarshal everything into the extra map
	if err := json.Unmarshal(data, &c.Extra); err != nil {
		return err
	}
	// Remove known fields from extra
	for _, key := range []string{"sub", "iss", "aud", "exp", "iat", "nbf", "jti", "groups"} {
		delete(c.Extra, key)
	}
	return nil
}
```

**Audience validation:** the expected audience is the OIDC `client_id`.
Machine clients must be configured in the IdP to include blockyard's client
ID in the token's `aud` claim. This is standard practice for
resource-server audience validation.

**Server struct additions** (in `internal/server/state.go`):

```go
type Server struct {
	// ... existing fields ...
	JWKSCache *auth.JWKSCache // nil when OIDC is not configured
}
```

**Initialization in `cmd/blockyard/main.go`** (extends the phase 1-1 OIDC
setup block):

```go
var jwksCache *auth.JWKSCache
if cfg.OIDC != nil {
	jwksURI := oidcProvider.Endpoint().JWKSURI
	var err error
	jwksCache, err = auth.NewJWKSCache(jwksURI)
	if err != nil {
		slog.Error("failed to initialize JWKS cache", "error", err)
		os.Exit(1)
	}
}
```

**Tests:**

- `JWKSCache.Validate` with valid token returns claims
- `JWKSCache.Validate` with expired token returns error
- `JWKSCache.Validate` with wrong issuer returns error
- `JWKSCache.Validate` with wrong audience returns error
- `JWKSCache.Validate` with unknown kid triggers refresh
- Cooldown: two rapid refreshes — second is skipped
- `JWTClaims.ExtractGroups` with standard "groups" claim
- `JWTClaims.ExtractGroups` with custom claim name in extras

### Step 8: Control-plane auth middleware

Replace the bearer auth middleware in `internal/api/` to support JWT
validation with static-token fallback.

New file: `internal/api/auth.go`

```go
package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/server"
)

// APIAuth returns a chi-compatible middleware that authenticates
// control-plane requests.
//
// When OIDC is configured:
//  1. Extract Bearer token from Authorization header
//  2. Validate as JWT against the IdP's JWKS
//  3. Extract sub + groups from claims
//  4. Derive role from groups via RoleCache
//  5. Store CallerIdentity in request context
//
// When OIDC is not configured (v0 compat / dev mode):
//  1. Extract Bearer token
//  2. Compare against static config token
//  3. Store CallerIdentity with Sub="admin", Role=RoleAdmin
func APIAuth(srv *server.Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearerToken(r)
			if token == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			var identity *auth.CallerIdentity

			if srv.Config.OIDC != nil && srv.JWKSCache != nil {
				// JWT validation path
				claims, err := srv.JWKSCache.Validate(
					token,
					srv.Config.OIDC.IssuerURL,
					srv.Config.OIDC.ClientID,
				)
				if err != nil {
					slog.Debug("JWT validation failed", "error", err)
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}

				groups := claims.ExtractGroups(srv.Config.OIDC.GroupsClaim)
				role := auth.DeriveRole(groups, srv.RoleCache)

				identity = &auth.CallerIdentity{
					Sub:    claims.Subject,
					Groups: groups,
					Role:   role,
					Source: auth.AuthSourceJWT,
				}
			} else {
				// Static token fallback
				if token != srv.Config.Server.Token {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}

				identity = &auth.CallerIdentity{
					Sub:    "admin",
					Groups: nil,
					Role:   auth.RoleAdmin,
					Source: auth.AuthSourceStaticToken,
				}
			}

			ctx := auth.ContextWithCaller(r.Context(), identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractBearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
```

**Changes to existing code:**

- The existing bearer auth middleware in the API package is replaced
  entirely by `APIAuth`.
- The function signature changes to return a chi-compatible middleware
  (`func(http.Handler) http.Handler`).
- Handler code extracts the caller via `auth.CallerFromContext(r.Context())`
  instead of checking Bearer tokens directly.

### Step 9: App-plane auth middleware — soften to "authenticate if possible"

Phase 1-1's app-plane auth middleware does a hard redirect to `/login` for
unauthenticated requests. Phase 1-2 changes it to **try to authenticate
but not require it**. The enforcement decision moves to the proxy handler's
ACL check (Step 13), which examines the app's `access_type`.

The middleware still validates the session cookie and inserts
`AuthenticatedUser` + `CallerIdentity` when a valid session exists.
The change is that it no longer redirects when no session is present —
it simply calls `next` with no identity in the context.

```go
// AppAuthMiddleware authenticates if possible, but does not require it.
// Public apps allow unauthenticated access; the proxy handler (Step 13)
// decides whether to allow or deny based on the app's access_type.
func AppAuthMiddleware(store *SessionStore, srv *server.Server) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload, err := store.VerifyCookie(r)
			if err != nil {
				// No valid session — proceed without identity.
				// The proxy ACL check will enforce access_type.
				next.ServeHTTP(w, r)
				return
			}
			session := store.Get(payload.Sub)
			if session == nil {
				next.ServeHTTP(w, r)
				return
			}
			store.RefreshIfNeeded(r.Context(), payload.Sub)
			ctx := WithAuthenticatedUser(r.Context(), payload, session)

			// --- Phase 1-2 addition: derive role and insert CallerIdentity ---
			caller := &auth.CallerIdentity{
				Sub:    payload.Sub,
				Groups: session.Groups,
				Role:   auth.DeriveRole(session.Groups, srv.RoleCache),
				Source: auth.AuthSourceSession,
			}
			ctx = auth.ContextWithCaller(ctx, caller)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
```

This replaces phase 1-1's hard-redirect middleware. The key difference:
unauthenticated requests pass through with no identity in the context
instead of being redirected. The proxy handler (Step 13) checks the app's
`access_type` and redirects to `/login` for `acl` apps when no caller is
present.

### Step 10: Authorization guards on API endpoints

Each API handler extracts `CallerIdentity` from the request context and
checks permissions. Two patterns:

**System-level checks** (create app, manage roles):

```go
func (h *Handler) CreateApp(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	if caller == nil || !caller.Role.CanCreateApp() {
		writeJSON(w, http.StatusForbidden, errorResp("insufficient permissions"))
		return
	}

	var body createAppRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResp("invalid request body"))
		return
	}

	app, err := h.srv.DB.CreateApp(body.Name, caller.Sub)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	writeJSON(w, http.StatusCreated, appToResponse(app))
}
```

**Per-app checks** (deploy, start, stop, delete, update):

```go
// resolveAppRelation loads an app + ACL grants, evaluates the caller's
// relationship. Returns the app and relation, or writes an error response
// and returns false.
//
// Returns 404 both when the app doesn't exist and when the caller has no
// access — this prevents leaking app existence to unauthorized users
// (same behavior as Posit Connect).
func (h *Handler) resolveAppRelation(
	w http.ResponseWriter,
	caller *auth.CallerIdentity,
	appID string,
) (*db.AppRow, authz.AppRelation, bool) {
	app, err := h.srv.DB.GetApp(appID)
	if err != nil || app == nil {
		writeJSON(w, http.StatusNotFound, errorResp("app not found"))
		return nil, authz.RelationNone, false
	}

	rows, err := h.srv.DB.ListAppAccess(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return nil, authz.RelationNone, false
	}

	grants := make([]authz.AccessGrant, len(rows))
	for i, row := range rows {
		grants[i] = accessRowToGrant(row)
	}

	relation := authz.EvaluateAccess(caller, app.Owner, grants, app.AccessType)

	// No access -> 404 (hide app existence)
	if relation == authz.RelationNone {
		writeJSON(w, http.StatusNotFound, errorResp("app not found"))
		return nil, authz.RelationNone, false
	}

	return app, relation, true
}
```

Used in handlers:

```go
func (h *Handler) DeleteApp(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	appID := chi.URLParam(r, "id")

	app, relation, ok := h.resolveAppRelation(w, caller, appID)
	if !ok {
		return
	}

	// Caller has some access but not delete permission -> also 404.
	// Returning 403 here would confirm the app exists to non-owners.
	if !relation.CanDelete() {
		writeJSON(w, http.StatusNotFound, errorResp("app not found"))
		return
	}

	// ... existing delete logic using app ...
	_ = app
}
```

**Endpoint-by-endpoint authorization:**

| Endpoint | Check | On failure |
|---|---|---|
| `POST /apps` | `caller.Role.CanCreateApp()` | 403 |
| `GET /apps` | filter results (see below) | omit |
| `GET /apps/{id}` | `relation != RelationNone` | 404 |
| `PATCH /apps/{id}` | `relation.CanUpdateConfig()` (+ `relation.CanManageACL()` for `access_type` changes) | 404 |
| `DELETE /apps/{id}` | `relation.CanDelete()` | 404 |
| `POST /apps/{id}/bundles` | `relation.CanDeploy()` | 404 |
| `GET /apps/{id}/bundles` | `relation != RelationNone` | 404 |
| `POST /apps/{id}/start` | `relation.CanStartStop()` | 404 |
| `POST /apps/{id}/stop` | `relation.CanStartStop()` | 404 |
| `GET /apps/{id}/logs` | `relation != RelationNone` | 404 |

All per-app endpoints return 404 on insufficient permissions — never 403.
This hides app existence from unauthorized callers. Only `POST /apps`
(a system-level action with no specific app) uses 403.

**`access_type` change restriction:** `PATCH /apps/{id}` allows
collaborators to update resource limits and worker config, but changing
`access_type` requires `CanManageACL()` (owner or admin). Making an app
public is an access control decision, not a config change. The handler
checks whether the request body includes `access_type` and applies the
stricter check only for that field.

**`GET /apps` (list) filtering:**

```go
func (h *Handler) ListApps(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())

	var apps []db.AppRow
	var err error

	if caller.Role.CanViewAllApps() {
		apps, err = h.srv.DB.ListApps()
	} else {
		apps, err = h.srv.DB.ListAccessibleApps(caller.Sub, caller.Groups)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	resp := make([]appResponse, len(apps))
	for i, app := range apps {
		resp[i] = appToResponse(&app)
	}
	writeJSON(w, http.StatusOK, resp)
}
```

**DB function** (in `internal/db/db.go`):

```go
// ListAccessibleApps returns apps the caller can see: owned apps + apps
// with an ACL grant matching the caller's sub or any of their groups.
func (db *DB) ListAccessibleApps(sub string, groups []string) ([]AppRow, error) {
	// Build the group placeholders dynamically. SQLite doesn't support
	// array parameters, so we build the IN clause with positional args.
	//
	// The query returns DISTINCT apps where the caller is the owner OR
	// has a direct user grant OR has a group grant via any of their groups.
	args := []any{sub, sub} // owner check + direct user grant

	groupClause := "SELECT 1 WHERE 0" // no groups -> never matches
	if len(groups) > 0 {
		placeholders := make([]string, len(groups))
		for i, g := range groups {
			placeholders[i] = "?"
			args = append(args, g)
		}
		groupClause = strings.Join(placeholders, ", ")
	}

	query := fmt.Sprintf(
		`SELECT DISTINCT a.id, a.name, a.owner, a.access_type, a.active_bundle,
		        a.max_workers_per_app, a.max_sessions_per_worker,
		        a.memory_limit, a.cpu_limit, a.created_at, a.updated_at
		 FROM apps a
		 LEFT JOIN app_access aa ON a.id = aa.app_id
		 WHERE a.access_type = 'public'
		    OR a.owner = ?
		    OR (aa.kind = 'user'  AND aa.principal = ?)
		    OR (aa.kind = 'group' AND aa.principal IN (%s))
		 ORDER BY a.created_at DESC`, groupClause,
	)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []AppRow
	for rows.Next() {
		var app AppRow
		if err := rows.Scan(&app.ID, &app.Name, &app.Owner, &app.AccessType,
			&app.ActiveBundle, &app.MaxWorkersPerApp, &app.MaxSessionsPerWorker,
			&app.MemoryLimit, &app.CPULimit,
			&app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}
```

Single query, no N+1. The `LEFT JOIN` + `DISTINCT` handles the case where
a user has multiple grants for the same app (direct + via group) without
duplicating rows. Public apps are included regardless of grants.

**Unauthenticated list:** when the caller is nil (unauthenticated),
`ListApps` in the handler falls through to a simpler query:

```go
// ListPublicApps returns only public apps. Used for unauthenticated callers.
func (db *DB) ListPublicApps() ([]AppRow, error) {
	return db.queryApps("SELECT ... FROM apps WHERE access_type = 'public' ORDER BY created_at DESC")
}
```

**New error helper** (add to the API package):

```go
func forbidden(msg string) {
	// Used by writeJSON(w, http.StatusForbidden, errorResp(msg))
}
```

**`appResponse` changes:**

The API response for apps gains `Owner` and `AccessType` fields:

```go
type appResponse struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Owner                string   `json:"owner"`       // new
	AccessType           string   `json:"access_type"` // new: "acl" or "public"
	ActiveBundle         *string  `json:"active_bundle"`
	MaxWorkersPerApp     *int     `json:"max_workers_per_app"`
	MaxSessionsPerWorker int      `json:"max_sessions_per_worker"`
	MemoryLimit          *string  `json:"memory_limit"`
	CPULimit             *float64 `json:"cpu_limit"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
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

New file: `internal/api/access.go`

```
POST   /api/v1/apps/{id}/access                      — grant access
GET    /api/v1/apps/{id}/access                       — list grants
DELETE /api/v1/apps/{id}/access/{kind}/{principal}     — revoke access
```

**Grant access:**

```go
type grantRequest struct {
	Principal string `json:"principal"`
	Kind      string `json:"kind"` // "user" | "group"
	Role      string `json:"role"` // "viewer" | "collaborator"
}

func (h *Handler) GrantAccess(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	appID := chi.URLParam(r, "id")

	_, relation, ok := h.resolveAppRelation(w, caller, appID)
	if !ok {
		return
	}

	if !relation.CanManageACL() {
		writeJSON(w, http.StatusNotFound, errorResp("app not found"))
		return
	}

	var body grantRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResp("invalid request body"))
		return
	}

	// Validate kind
	if body.Kind != "user" && body.Kind != "group" {
		writeJSON(w, http.StatusBadRequest, errorResp("kind must be 'user' or 'group'"))
		return
	}

	// Validate role
	if _, valid := authz.ParseContentRole(body.Role); !valid {
		writeJSON(w, http.StatusBadRequest, errorResp("role must be 'viewer' or 'collaborator'"))
		return
	}

	// Prevent self-grant (owner/admin already have full access)
	if body.Kind == "user" && body.Principal == caller.Sub {
		writeJSON(w, http.StatusBadRequest, errorResp("cannot grant access to yourself"))
		return
	}

	if err := h.srv.DB.GrantAppAccess(
		appID, body.Principal, body.Kind, body.Role, caller.Sub,
	); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
```

**List grants:**

```go
type accessGrantResponse struct {
	Principal string `json:"principal"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	GrantedBy string `json:"granted_by"`
	GrantedAt string `json:"granted_at"`
}

func (h *Handler) ListAccess(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	appID := chi.URLParam(r, "id")

	_, relation, ok := h.resolveAppRelation(w, caller, appID)
	if !ok {
		return
	}

	if !relation.CanManageACL() {
		writeJSON(w, http.StatusNotFound, errorResp("app not found"))
		return
	}

	rows, err := h.srv.DB.ListAppAccess(appID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	resp := make([]accessGrantResponse, len(rows))
	for i, row := range rows {
		resp[i] = accessGrantResponse{
			Principal: row.Principal,
			Kind:      row.Kind,
			Role:      row.Role,
			GrantedBy: row.GrantedBy,
			GrantedAt: row.GrantedAt,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
```

**Revoke access:**

```go
func (h *Handler) RevokeAccess(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	appID := chi.URLParam(r, "id")
	kind := chi.URLParam(r, "kind")
	principal := chi.URLParam(r, "principal")

	_, relation, ok := h.resolveAppRelation(w, caller, appID)
	if !ok {
		return
	}

	if !relation.CanManageACL() {
		writeJSON(w, http.StatusNotFound, errorResp("app not found"))
		return
	}

	removed, err := h.srv.DB.RevokeAppAccess(appID, principal, kind)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	if !removed {
		writeJSON(w, http.StatusNotFound, errorResp("grant not found"))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
```

**ACL enforcement on active sessions:** ACL checks run on HTTP requests
only, not on individual WebSocket frames. When a user's access is revoked,
it takes effect on their next HTTP request or WebSocket reconnect — active
WS connections continue until the next reconnect. This avoids per-frame
database lookups on the hot path.

### Step 12: Role mapping management API

New file: `internal/api/roles.go`

```
GET    /api/v1/role-mappings                  — list all mappings
PUT    /api/v1/role-mappings/{group_name}     — set mapping
DELETE /api/v1/role-mappings/{group_name}     — delete mapping
```

All endpoints are admin-only.

**List mappings:**

```go
type roleMappingResponse struct {
	GroupName string `json:"group_name"`
	Role      string `json:"role"`
}

func (h *Handler) ListRoleMappings(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	if !caller.Role.CanManageRoles() {
		writeJSON(w, http.StatusForbidden, errorResp("admin only"))
		return
	}

	rows, err := h.srv.DB.ListRoleMappings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	resp := make([]roleMappingResponse, len(rows))
	for i, row := range rows {
		resp[i] = roleMappingResponse{
			GroupName: row.GroupName,
			Role:      row.Role,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
```

**Set mapping:**

```go
type setRoleMappingRequest struct {
	Role string `json:"role"` // "admin" | "publisher" | "viewer"
}

func (h *Handler) SetRoleMapping(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	if !caller.Role.CanManageRoles() {
		writeJSON(w, http.StatusForbidden, errorResp("admin only"))
		return
	}

	groupName := chi.URLParam(r, "group_name")

	var body setRoleMappingRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResp("invalid request body"))
		return
	}

	// Validate role value
	role := auth.ParseRole(body.Role)
	if role == auth.RoleNone {
		writeJSON(w, http.StatusBadRequest, errorResp(
			"invalid role '"+body.Role+"', must be one of: admin, publisher, viewer",
		))
		return
	}

	if err := h.srv.DB.UpsertRoleMapping(groupName, body.Role); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	// Update in-memory cache
	h.srv.RoleCache.Set(groupName, role)

	w.WriteHeader(http.StatusNoContent)
}
```

**Delete mapping:**

```go
func (h *Handler) DeleteRoleMapping(w http.ResponseWriter, r *http.Request) {
	caller := auth.CallerFromContext(r.Context())
	if !caller.Role.CanManageRoles() {
		writeJSON(w, http.StatusForbidden, errorResp("admin only"))
		return
	}

	groupName := chi.URLParam(r, "group_name")

	removed, err := h.srv.DB.DeleteRoleMapping(groupName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResp(err.Error()))
		return
	}

	if !removed {
		writeJSON(w, http.StatusNotFound, errorResp("no mapping for group '"+groupName+"'"))
		return
	}

	h.srv.RoleCache.Remove(groupName)
	w.WriteHeader(http.StatusNoContent)
}
```

**Note on cache consistency:** the role mapping cache is updated
synchronously after the DB write succeeds. Since blockyard v1 is
single-server, there's no cache coherence concern. For v2 multi-node,
this would need a cache invalidation mechanism (or short TTL with
DB-backed reads).

### Step 13: Proxy authorization

Phase 1-2 adds access control to the proxy handler. The app-plane auth
middleware (Step 9) now authenticates if possible but doesn't require it.
The proxy handler is responsible for enforcing access based on the app's
`access_type` and the caller's identity.

The ACL check is added to the proxy request handler, after app lookup
and before session assignment / cold-start:

```go
func (p *Proxy) handleRequest(w http.ResponseWriter, r *http.Request, appName string) {
	app, err := p.srv.DB.GetAppByName(appName)
	if err != nil || app == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// --- Phase 1-2: ACL check ---
	// caller is nil for unauthenticated requests; non-nil when the
	// app-plane middleware found a valid session.
	caller := auth.CallerFromContext(r.Context())

	// When OIDC is not configured (v0 compat), skip ACL entirely.
	if p.srv.Config.OIDC != nil {
		rows, err := p.srv.DB.ListAppAccess(app.ID)
		if err != nil {
			rows = nil // treat as no grants (fail closed)
		}

		grants := make([]authz.AccessGrant, len(rows))
		for i, row := range rows {
			grants[i] = accessRowToGrant(row)
		}

		relation := authz.EvaluateAccess(caller, app.Owner, grants, app.AccessType)
		if !relation.CanAccessProxy() {
			// ACL app + unauthenticated -> redirect to login
			// (rather than 404, since there's nothing to hide
			// from an unauthenticated user — they know the URL)
			if caller == nil {
				http.Redirect(w, r, "/login?return_url="+r.URL.Path, http.StatusFound)
				return
			}
			// Authenticated but no access -> 404 (hide existence)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	// ... existing proxy logic (session lookup, cold-start, forward) ...
}
```

**Behavior matrix:**

| `access_type` | Caller | Result |
|---|---|---|
| `acl` | nil (unauthenticated) | redirect to `/login` |
| `acl` | authenticated, no access | 404 |
| `acl` | authenticated, has access | proxy |
| `public` | nil (unauthenticated) | proxy (no identity headers) |
| `public` | authenticated | proxy (identity headers injected) |

**Performance note:** this adds a database query (`ListAppAccess`) to
every proxied HTTP request. SQLite reads are fast and the query is indexed
by `app_id` (primary key prefix). If this becomes a bottleneck, an
in-memory ACL cache with short TTL (30-60s) keyed by `app_id` and
invalidated on ACL writes can be added.

### Step 14: Router integration

**API router changes** (in the router setup, e.g. `internal/api/routes.go`
or wherever the chi router is built):

```go
func RegisterRoutes(r chi.Router, srv *server.Server) {
	h := &Handler{srv: srv}

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(APIAuth(srv))

		// Existing app endpoints
		r.Post("/apps", h.CreateApp)
		r.Get("/apps", h.ListApps)
		r.Get("/apps/{id}", h.GetApp)
		r.Patch("/apps/{id}", h.UpdateApp)
		r.Delete("/apps/{id}", h.DeleteApp)
		r.Post("/apps/{id}/bundles", h.UploadBundle)
		r.Get("/apps/{id}/bundles", h.ListBundles)
		r.Post("/apps/{id}/start", h.StartApp)
		r.Post("/apps/{id}/stop", h.StopApp)
		r.Get("/apps/{id}/logs", h.AppLogs)
		r.Get("/tasks/{task_id}/logs", h.TaskLogs)

		// Phase 1-2: ACL management
		r.Post("/apps/{id}/access", h.GrantAccess)
		r.Get("/apps/{id}/access", h.ListAccess)
		r.Delete("/apps/{id}/access/{kind}/{principal}", h.RevokeAccess)

		// Phase 1-2: Role mapping management
		r.Get("/role-mappings", h.ListRoleMappings)
		r.Put("/role-mappings/{group_name}", h.SetRoleMapping)
		r.Delete("/role-mappings/{group_name}", h.DeleteRoleMapping)
	})

	r.Get("/healthz", h.Healthz)
}
```

**Package layout:**

```
internal/
├── auth/
│   ├── identity.go      # new: Role, AuthSource, CallerIdentity, DeriveRole, context helpers
│   ├── rolecache.go     # new: RoleMappingCache
│   ├── jwt.go           # new: JWKSCache, JWTClaims, JWT validation
│   ├── oidc.go          # unchanged (phase 1-1)
│   └── session.go       # unchanged (phase 1-1)
├── authz/
│   ├── rbac.go          # new: ContentRole, AppRelation + permission methods
│   └── acl.go           # new: AccessGrant, AccessKind, EvaluateAccess
├── api/
│   ├── auth.go          # rewritten: APIAuth middleware (JWT + static fallback)
│   ├── apps.go          # + owner on create, authz checks
│   ├── bundles.go       # + authz check on upload
│   ├── access.go        # new: ACL management endpoints
│   ├── roles.go         # new: role mapping management endpoints
│   └── ...
```

### Step 15: Tests

**Unit tests** (extend existing test modules):

- **Role types** (`internal/auth/identity_test.go`): ordering, permission
  methods, DeriveRole, ParseRole, context round-trip
- **ACL evaluation** (`internal/authz/acl_test.go`): all combinations from
  Step 4 tests
- **AppRelation permissions** (`internal/authz/rbac_test.go`): each
  method x each relation level
- **JWT validation** (`internal/auth/jwt_test.go`): valid, expired, wrong
  issuer, wrong audience, missing kid, kid-not-found refresh
- **JWTClaims.ExtractGroups**: standard claim, custom claim, missing claim
- **RoleMappingCache** (`internal/auth/rolecache_test.go`): Load, Get,
  Set, Remove

**Integration tests** (`internal/api/rbac_test.go` or separate test file):

These use the mock IdP from phase 1-1, extended with an `IssueJWT()`
method, and issue JWTs with specific groups to test role-based behavior
end-to-end. Tests use `net/http/httptest` for the test server.

```go
func TestPublisherCanCreateApp(t *testing.T) {
	idp := testutil.StartMockIdP(t)
	srv := testutil.NewTestServerWithOIDC(t, idp)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Configure role mapping: "developers" -> publisher
	srv.RoleCache.Set("developers", auth.RolePublisher)

	// Issue JWT with groups=["developers"]
	token := idp.IssueJWT("user-1", []string{"developers"})

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps", strings.NewReader(`{"name":"my-app"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["owner"] != "user-1" {
		t.Errorf("expected owner 'user-1', got %v", body["owner"])
	}
}

func TestViewerCannotCreateApp(t *testing.T) {
	idp := testutil.StartMockIdP(t)
	srv := testutil.NewTestServerWithOIDC(t, idp)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	srv.RoleCache.Set("readonly", auth.RoleViewer)
	token := idp.IssueJWT("user-2", []string{"readonly"})

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps", strings.NewReader(`{"name":"my-app"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestAdminSeesAllApps(t *testing.T) {
	idp := testutil.StartMockIdP(t)
	srv := testutil.NewTestServerWithOIDC(t, idp)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	srv.RoleCache.Set("admins", auth.RoleAdmin)
	srv.RoleCache.Set("developers", auth.RolePublisher)

	// Publisher creates an app
	pubToken := idp.IssueJWT("publisher-1", []string{"developers"})
	createReq, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps", strings.NewReader(`{"name":"app-1"}`))
	createReq.Header.Set("Authorization", "Bearer "+pubToken)
	createReq.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(createReq)

	// Admin lists all apps
	adminToken := idp.IssueJWT("admin-1", []string{"admins"})
	listReq, _ := http.NewRequest("GET", ts.URL+"/api/v1/apps", nil)
	listReq.Header.Set("Authorization", "Bearer "+adminToken)

	resp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var apps []map[string]any
	json.NewDecoder(resp.Body).Decode(&apps)
	if len(apps) != 1 {
		t.Errorf("expected 1 app, got %d", len(apps))
	}
}

func TestPublisherSeesOnlyOwnAndGrantedApps(t *testing.T) {
	// Create two apps (one owned, one not).
	// Grant access to the second app.
	// Verify list returns both but not others.
}

func TestCollaboratorCanDeploy(t *testing.T) {
	// Publisher creates app.
	// Publisher grants collaborator access to another user.
	// Collaborator deploys a bundle -> 200.
}

func TestContentViewerCannotDeploy(t *testing.T) {
	// Publisher creates app.
	// Publisher grants viewer access to another user.
	// Viewer attempts deploy -> 404 (hides insufficient permission).
}

func TestACLGrantRevokeCycle(t *testing.T) {
	// Admin creates app.
	// Admin grants viewer access to user-2.
	// GET /access -> shows grant.
	// Admin revokes access.
	// GET /access -> empty.
	// User-2 cannot access app via proxy -> 404.
}

func TestRoleMappingCRUD(t *testing.T) {
	// Admin creates mapping.
	// GET -> shows mapping.
	// Admin updates mapping.
	// GET -> shows updated role.
	// Admin deletes mapping.
	// GET -> empty.
}

func TestUnmappedUserHasNoRole(t *testing.T) {
	// User authenticates with groups not in role_mappings.
	// Cannot create apps (403).
	// Cannot see any apps (empty list).
	// Can access app only if explicitly granted.
}

func TestStaticTokenFallback(t *testing.T) {
	// No OIDC config — v0 compat mode.
	srv := testutil.NewTestServer(t) // existing helper
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// All operations work with static bearer token.
	// Created app has owner = "admin".
}

func TestProxyACLCheck(t *testing.T) {
	// Deploy and start an app (access_type = "acl").
	// User with access -> proxied successfully.
	// User without access -> 404.
	// Unauthenticated -> redirect to /login.
}

func TestPublicAppAnonymousAccess(t *testing.T) {
	// Create and deploy an app.
	// Owner sets access_type = "public" via PATCH.
	// Unauthenticated request to /app/{name}/ -> proxied (no identity headers).
	// Authenticated request -> proxied (identity headers injected).
}

func TestPublicAppStillRespectsRoles(t *testing.T) {
	// Public app — anyone can use via proxy.
	// But only owner/admin/collaborator can deploy, start/stop, etc.
	// Unauthenticated user cannot deploy (401).
	// Authenticated viewer cannot deploy (404).
}

func TestSetAccessType(t *testing.T) {
	// Owner can set access_type to "public" via PATCH.
	// Admin can set access_type on any app.
	// Collaborator cannot change access_type (404).
	// Invalid access_type rejected (400).
}
```

**Mock IdP extension:** the phase 1-1 mock IdP issues ID tokens for the
OIDC callback flow. Phase 1-2 extends it with `IssueJWT()` — a method
that issues access-token-style JWTs suitable for Bearer auth on the
control plane. Same RSA signing key, same JWKS endpoint, but the token
format is a plain JWT with `sub`, `iss`, `aud`, `exp`, and groups.

```go
// IssueJWT creates a JWT for control-plane Bearer auth (client credentials
// style). Same signing key as ID tokens, different claims structure.
func (m *MockIdP) IssueJWT(sub string, groups []string) string {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":    sub,
		"iss":    m.IssuerURL(),
		"aud":    "blockyard", // matches test OIDC config ClientID
		"exp":    now.Add(time.Hour).Unix(),
		"iat":    now.Unix(),
		"groups": groups,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = m.Kid()

	signed, err := token.SignedString(m.PrivateKey())
	if err != nil {
		panic("MockIdP.IssueJWT: " + err.Error())
	}
	return signed
}
```

## Changes to existing code

Summary of modifications to v0/phase-1-1 files:

| File | Change |
|---|---|
| `internal/api/auth.go` (or equivalent) | Replace bearer auth with `APIAuth` (JWT + static fallback) |
| `internal/api/` router setup | Add access + roles routes, update auth middleware reference |
| `internal/api/apps.go` | Add `CallerIdentity` extraction, authorization checks, owner on create |
| `internal/api/bundles.go` | Add authorization check (deploy permission) |
| `internal/auth/` (phase 1-1 middleware) | Soften to "authenticate if possible" + add `CallerIdentity` insertion |
| `internal/server/state.go` | Add `RoleCache` and `JWKSCache` fields to `Server` |
| `internal/proxy/` | Add ACL check in proxy handler |
| `internal/db/db.go` | Add `Owner` + `AccessType` to `AppRow`, add role_mapping + app_access functions, extend schema |
| `internal/config/config.go` | No changes (OIDC config already added in phase 1-1) |
| `cmd/blockyard/main.go` | Initialize `RoleMappingCache` and `JWKSCache` |

## File summary

```
internal/
├── auth/
│   ├── identity.go           # new: Role, AuthSource, CallerIdentity, DeriveRole
│   ├── rolecache.go          # new: RoleMappingCache (sync.RWMutex + map)
│   ├── jwt.go                # new: JWKSCache, JWTClaims, JWT validation
│   ├── oidc.go               # unchanged (phase 1-1)
│   └── session.go            # unchanged (phase 1-1)
├── authz/
│   ├── rbac.go               # new: ContentRole, AppRelation + permission methods
│   └── acl.go                # new: AccessGrant, AccessKind, EvaluateAccess
├── api/
│   ├── auth.go               # rewritten: APIAuth middleware (JWT + static fallback)
│   ├── apps.go               # + owner on create, authz checks
│   ├── bundles.go            # + authz check on upload
│   ├── access.go             # new: ACL management endpoints
│   ├── roles.go              # new: role mapping management endpoints
│   └── tasks.go              # unchanged
├── server/
│   └── state.go              # + RoleCache, JWKSCache fields
├── proxy/
│   └── ...                   # + ACL check
├── db/
│   └── db.go                 # + owner, role_mapping fns, app_access fns, schema extension
└── ...
cmd/
└── blockyard/
    └── main.go               # + RoleMappingCache + JWKSCache init
internal/
├── auth/
│   ├── identity_test.go      # new: Role, DeriveRole, context helpers
│   ├── rolecache_test.go     # new: RoleMappingCache
│   └── jwt_test.go           # new: JWKS + JWT validation
├── authz/
│   ├── rbac_test.go          # new: AppRelation permission methods
│   └── acl_test.go           # new: EvaluateAccess
├── api/
│   └── rbac_test.go          # new: RBAC + ACL integration tests
└── testutil/
    └── mockidp.go            # + IssueJWT() method
```

## Exit criteria

Phase 1-2 is done when:

- `go build ./...` succeeds with and without `[oidc]` config
- Schema consolidation: updated schema const in `db.go` with `owner` and `access_type` columns
- Schema extension: `app_access` and `role_mappings` tables created
- Role type tests pass: ordering, DeriveRole, permission methods
- ACL evaluation tests pass: all relation types, conflict resolution
- JWT validation tests pass: valid, expired, wrong issuer, wrong audience,
  kid rotation
- JWKS cache tests pass: fetch, refresh, cooldown
- RoleMappingCache tests pass: Load, Get, Set, Remove
- Integration tests pass:
  - Publisher creates app with owner set to caller sub
  - Viewer cannot create app (403)
  - Admin sees all apps, publisher sees own + granted
  - Collaborator can deploy, viewer cannot
  - ACL grant/revoke cycle works end-to-end
  - Role mapping CRUD works end-to-end
  - Unmapped user has no role
  - Proxy ACL check: access with grant, 404 without
  - Public app: unauthenticated access proxied, identity headers absent
  - Public app: authenticated access proxied, identity headers present
  - Public app: unauthenticated cannot deploy/start/stop
  - Set access_type: owner and admin can, collaborator cannot
  - Static-token fallback: all operations work, owner = "admin"
- Existing v0 tests continue to pass (no regression)
- `env_var_coverage_complete` test passes (no new env vars in this phase)
