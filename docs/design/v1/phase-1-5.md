# Phase 1-5: Vanity URLs + Content Discovery

User-facing features for navigating and accessing deployed content. This
phase adds two capabilities:

1. **Vanity URLs** — user-friendly paths like `/sales-dashboard/` instead
   of `/app/sales-dashboard/`. Operators or app owners assign vanity URLs
   via the API.
2. **Content discovery** — a catalog API listing accessible apps with
   metadata, tags, and search/filter support. The foundation for a
   future browse UI.

This phase depends on phase 1-2 (RBAC — the catalog must respect access
control, and vanity URL assignment requires permission checks).

## Design decision: vanity URLs as path aliases, not DNS

Vanity URLs are path-based (`/sales-dashboard/`), not subdomain-based
(`sales-dashboard.blockyard.example.com`). Path-based routing:

- **Works without wildcard DNS or TLS certs.** Subdomain routing requires
  `*.blockyard.example.com` DNS records and a wildcard TLS certificate
  (or per-subdomain certs via ACME). Path routing works with a single
  domain and a single certificate.
- **Simpler reverse proxy config.** All traffic goes to the same host;
  blockyard routes internally. Subdomain routing requires the upstream
  reverse proxy to forward all subdomains.
- **Matches the existing `/app/{name}/` pattern.** Vanity URLs are a
  convenience layer on top of the same proxy infrastructure.

**Trade-off:** path-based vanity URLs must not collide with reserved
paths (`/api/`, `/login`, `/healthz`, etc.). A blocklist prevents this.
Subdomain routing would avoid the collision problem entirely, but the
operational overhead is not justified for v1.

## Design decision: vanity routes checked before standard routes

The chi router evaluates routes in registration order. Vanity URL routes
are registered before the `/app/{name}/` catch-all so they take
precedence. If a vanity URL matches, the request is proxied to the
target app. If no vanity URL matches, the request falls through to the
standard `/app/{name}/` handler (or returns 404 if that doesn't match
either).

**Resolution flow for `GET /foo/`:**

1. Check vanity URL table: is there an app with `vanity_url = 'foo'`?
2. If yes → proxy to that app (same handler as `/app/{name}/`).
3. If no → chi tries the next route (`/app/{name}/`). Since `foo` is
   not under `/app/`, this returns 404.

**Caching:** vanity URL resolution uses an in-memory cache
(`VanityCache`) loaded at startup and updated on writes. The cache maps
slug → app ID. Non-vanity requests (`/favicon.ico`, `/robots.txt`,
mistyped URLs) are rejected at the cache level without hitting the
database. Only cache hits proceed to a DB lookup for the full app row.

The cache is small (hundreds of entries at most) and rarely mutated
(only on `PATCH /apps/{id}` with a `vanity_url` field or app deletion).
A `sync.RWMutex` + `map[string]string` is sufficient.

## Design decision: reserved prefix blocklist

Vanity URLs are validated against a static blocklist of reserved prefixes.
Any vanity URL that matches a reserved prefix is rejected.

```go
var reservedPrefixes = []string{
    "api", "app", "login", "callback", "logout",
    "healthz", "readyz", "metrics",
    "static", "assets", "admin",
}
```

The list is intentionally conservative — it includes prefixes that are
not yet used (like `admin`, `static`, `assets`) to prevent future
collisions. Adding a prefix to the blocklist after apps have claimed it
as a vanity URL would require a migration.

**Validation rule:** a vanity URL is rejected if it equals any reserved
prefix (case-insensitive). No partial matching — `api-docs` is allowed
even though `api` is reserved.

## Design decision: catalog visibility respects RBAC

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

## Design decision: tags are admin-managed

Tags are created and deleted by admins only. Any authenticated user can
view tags and filter the catalog by tag. App owners and collaborators can
attach/detach existing tags to their apps.

**Why not user-created tags?**

- User-created tags tend toward chaos (duplicate tags, inconsistent
  naming, tag pollution). Admin-managed tags enforce a controlled
  vocabulary.
- The tag set is expected to be small (tens, not thousands) — categories
  like "finance", "reporting", "operations".

## Design decision: title and description fields on apps

Phase 1-5 adds `title` and `description` columns to the `apps` table.
These are optional human-readable metadata for the catalog:

- **`title`** — display name (e.g., "Sales Dashboard"). Falls back to
  the app name if not set.
- **`description`** — short description for catalog listings.

These are set via `PATCH /api/v1/apps/{id}` alongside existing fields.

## Deliverables

1. Vanity URL assignment — `PATCH /api/v1/apps/{id}` with `vanity_url`
   field
2. Vanity URL routing — resolve `/{vanity}/` to the target app
3. Vanity URL validation — reserved prefix blocklist, uniqueness,
   format rules
4. Trailing-slash redirect for vanity URLs (`/{vanity}` → `/{vanity}/`)
5. Schema migration — `vanity_url`, `title`, `description` on apps;
   `tags` and `app_tags` tables
6. Tag management API — `POST/GET/DELETE /api/v1/tags`
7. App tag management — `POST/DELETE /api/v1/apps/{id}/tags`
8. Catalog API — `GET /api/v1/catalog` with tag/search/pagination
9. `VanityCache` — in-memory slug → app ID cache, loaded at startup,
   invalidated on writes
10. DB access layer for vanity URL resolution, tags, and catalog queries

## Step-by-step

### Step 1: Schema migration

Add columns to `apps` and create tag tables. As with phase 1-2, this is
folded into the consolidated schema (pre-release):

**Apps table additions:**

```sql
ALTER TABLE apps ADD COLUMN vanity_url TEXT UNIQUE;
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
    VanityURL            *string  // new
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
- Apps with and without vanity_url
- Vanity URL uniqueness constraint (insert two apps with same vanity_url
  fails)

### Step 2: Vanity URL validation

New file: `internal/vanity/vanity.go`

```go
package vanity

import (
    "fmt"
    "regexp"
    "strings"
)

// reservedPrefixes are path prefixes that cannot be used as vanity URLs.
var reservedPrefixes = []string{
    "api", "app", "login", "callback", "logout",
    "healthz", "readyz", "metrics",
    "static", "assets", "admin",
}

// vanityPattern matches valid vanity URL slugs: same rules as app names.
// 1-63 chars, lowercase ASCII letters, digits, hyphens. Must start with
// a letter, must not end with a hyphen.
var vanityPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Validate checks whether a vanity URL slug is valid.
// Returns nil on success, or an error describing the problem.
func Validate(slug string) error {
    if !vanityPattern.MatchString(slug) {
        return fmt.Errorf("vanity URL must be 1-63 lowercase letters, digits, or hyphens, starting with a letter")
    }

    if strings.HasSuffix(slug, "-") {
        return fmt.Errorf("vanity URL must not end with a hyphen")
    }

    lower := strings.ToLower(slug)
    for _, prefix := range reservedPrefixes {
        if lower == prefix {
            return fmt.Errorf("vanity URL %q is reserved", slug)
        }
    }

    return nil
}
```

**Tests:**

- Valid slugs: "sales-dashboard", "q4-report", "a"
- Invalid: starts with digit, contains uppercase, too long (64 chars),
  empty, ends with hyphen
- Reserved: "api", "app", "login", "healthz", "admin" — all rejected
- Not reserved: "api-docs", "application", "login-page" — all accepted

### Step 3: DB access layer for vanity URLs

**New methods in `internal/db/db.go`:**

```go
// GetAppByVanityURL looks up an app by its vanity URL slug.
// Returns nil if no app has this vanity URL.
func (db *DB) GetAppByVanityURL(vanityURL string) (*AppRow, error) {
    row := db.QueryRow(
        `SELECT id, name, owner, access_type, vanity_url, title, description,
                active_bundle, max_workers_per_app,
                max_sessions_per_worker, memory_limit, cpu_limit,
                created_at, updated_at
         FROM apps WHERE vanity_url = ?`,
        vanityURL,
    )
    app := &AppRow{}
    err := row.Scan(
        &app.ID, &app.Name, &app.Owner, &app.AccessType,
        &app.VanityURL, &app.Title, &app.Description,
        &app.ActiveBundle, &app.MaxWorkersPerApp,
        &app.MaxSessionsPerWorker, &app.MemoryLimit, &app.CPULimit,
        &app.CreatedAt, &app.UpdatedAt,
    )
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, fmt.Errorf("get app by vanity url: %w", err)
    }
    return app, nil
}

// SetVanityURL sets or clears the vanity URL for an app.
// Pass empty string to clear.
func (db *DB) SetVanityURL(appID, vanityURL string) error {
    var v *string
    if vanityURL != "" {
        v = &vanityURL
    }
    _, err := db.Exec(
        "UPDATE apps SET vanity_url = ?, updated_at = ? WHERE id = ?",
        v, time.Now().UTC().Format(time.RFC3339), appID,
    )
    return err
}
```

**Tests:**

- Set vanity URL and retrieve by it
- Set vanity URL to empty string — clears it
- Duplicate vanity URL — returns error (unique constraint)
- Get by nonexistent vanity URL — returns nil

### Step 4: Vanity URL cache

New file: `internal/vanity/cache.go`

```go
package vanity

import (
    "sync"

    "github.com/cynkra/blockyard/internal/db"
)

// Cache is an in-memory cache of vanity URL slug → app ID.
// Loaded from the database at startup. Updated synchronously when
// vanity URLs are assigned, cleared, or apps are deleted.
//
// The proxy handler checks the cache first. Misses (no matching slug)
// are rejected without hitting the database. Hits are followed by a
// DB lookup for the full app row (which is needed for auth checks,
// active bundle, etc.).
type Cache struct {
    mu    sync.RWMutex
    slugs map[string]string // slug → app ID
}

func NewCache() *Cache {
    return &Cache{slugs: make(map[string]string)}
}

// Load populates the cache from the database.
func (c *Cache) Load(database *db.DB) error {
    apps, err := database.ListVanityURLs()
    if err != nil {
        return err
    }

    c.mu.Lock()
    defer c.mu.Unlock()

    c.slugs = make(map[string]string, len(apps))
    for _, a := range apps {
        c.slugs[a.VanityURL] = a.AppID
    }
    return nil
}

// Lookup checks if a slug is a registered vanity URL.
// Returns the app ID and true if found, or empty string and false if not.
// This is the fast path — no DB hit on miss.
func (c *Cache) Lookup(slug string) (string, bool) {
    c.mu.RLock()
    defer c.mu.RUnlock()
    id, ok := c.slugs[slug]
    return id, ok
}

// Set adds or updates a vanity URL mapping. Called after a successful
// DB write.
func (c *Cache) Set(slug, appID string) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.slugs[slug] = appID
}

// Remove removes a vanity URL mapping. Called when a vanity URL is
// cleared or an app is deleted.
func (c *Cache) Remove(slug string) {
    c.mu.Lock()
    defer c.mu.Unlock()
    delete(c.slugs, slug)
}

// RemoveByAppID removes the vanity URL for a given app ID.
// Used when deleting an app (we know the app ID but may not know
// the slug without a lookup).
func (c *Cache) RemoveByAppID(appID string) {
    c.mu.Lock()
    defer c.mu.Unlock()
    for slug, id := range c.slugs {
        if id == appID {
            delete(c.slugs, slug)
            return
        }
    }
}
```

**DB helper for loading:**

```go
type VanityEntry struct {
    AppID     string
    VanityURL string
}

func (db *DB) ListVanityURLs() ([]VanityEntry, error) {
    rows, err := db.Query(
        "SELECT id, vanity_url FROM apps WHERE vanity_url IS NOT NULL")
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var entries []VanityEntry
    for rows.Next() {
        var e VanityEntry
        if err := rows.Scan(&e.AppID, &e.VanityURL); err != nil {
            return nil, err
        }
        entries = append(entries, e)
    }
    return entries, rows.Err()
}
```

**Server struct addition:**

```go
type Server struct {
    // ... existing fields ...
    VanityCache *vanity.Cache
}
```

**Initialization in `cmd/blockyard/main.go`:**

```go
vanityCache := vanity.NewCache()
if err := vanityCache.Load(database); err != nil {
    slog.Error("failed to load vanity URL cache", "error", err)
    os.Exit(1)
}
srv.VanityCache = vanityCache
```

**Tests:**

- `Load` populates cache from DB entries
- `Lookup` hit returns app ID
- `Lookup` miss returns false (no DB hit)
- `Set` then `Lookup` returns new mapping
- `Remove` then `Lookup` returns false
- `RemoveByAppID` removes correct entry
- Concurrent access: parallel `Set`/`Lookup`/`Remove`

### Step 5: Vanity URL assignment via update endpoint

Extend the `PATCH /api/v1/apps/{id}` handler to accept `vanity_url`:

```go
type updateAppBody struct {
    // ... existing fields ...
    VanityURL   *string `json:"vanity_url"`
    Title       *string `json:"title"`
    Description *string `json:"description"`
}
```

In the handler:

```go
if body.VanityURL != nil {
    if *body.VanityURL == "" {
        // Clear vanity URL — remove from cache first (need old slug)
        if app.VanityURL != nil {
            srv.VanityCache.Remove(*app.VanityURL)
        }
        if err := srv.DB.SetVanityURL(app.ID, ""); err != nil {
            writeError(w, http.StatusInternalServerError, "db_error", "Failed to clear vanity URL")
            return
        }
    } else {
        if err := vanity.Validate(*body.VanityURL); err != nil {
            badRequest(w, err.Error())
            return
        }
        if err := srv.DB.SetVanityURL(app.ID, *body.VanityURL); err != nil {
            if isUniqueConstraintError(err) {
                writeError(w, http.StatusConflict, "vanity_url_taken",
                    "This vanity URL is already in use")
                return
            }
            writeError(w, http.StatusInternalServerError, "db_error", "Failed to set vanity URL")
            return
        }
        // Update cache: remove old slug (if any), add new one
        if app.VanityURL != nil {
            srv.VanityCache.Remove(*app.VanityURL)
        }
        srv.VanityCache.Set(*body.VanityURL, app.ID)
    }
}
```

**Permission check:** vanity URL assignment requires owner or admin
access (`relation >= RelationOwner`). This is enforced by the existing
update endpoint's auth guard from phase 1-2.

**Cache invalidation on app delete:** the existing `DELETE /apps/{id}`
handler must also clear the vanity cache. Add after the DB delete:

```go
srv.VanityCache.RemoveByAppID(app.ID)
```

**Tests:**

- Set vanity URL — 200, app response includes `vanity_url`
- Set vanity URL to reserved prefix — 400
- Set vanity URL already in use — 409
- Clear vanity URL (empty string) — 200
- Set title and description — 200
- Non-owner/admin cannot set vanity URL — 404 (phase 1-2 auth)

### Step 6: Vanity URL routing

Add vanity URL routes to the router, before the `/app/{name}/` routes:

```go
func NewRouter(srv *server.Server) http.Handler {
    r := chi.NewRouter()

    // Unauthenticated
    r.Get("/healthz", healthz)

    // Auth endpoints (phase 1-1)
    r.Get("/login", loginHandler(srv))
    r.Get("/callback", callbackHandler(srv))
    r.Post("/logout", logoutHandler(srv))

    // Vanity URL routes — checked before /app/{name}/
    r.Get("/{vanity}", vanityRedirectTrailingSlash(srv))
    r.Handle("/{vanity}/*", vanityProxyHandler(srv))

    // Standard app proxy routes
    r.Get("/app/{name}", proxy.RedirectTrailingSlash)
    r.Handle("/app/{name}/*", proxy.Handler(srv))

    // Authenticated API
    r.Route("/api/v1", func(r chi.Router) {
        // ... existing ...
    })

    return r
}
```

**Vanity proxy handler:**

```go
// vanityProxyHandler resolves a vanity URL to an app and proxies to it.
// Uses the in-memory VanityCache for fast rejection of non-vanity paths.
// Returns 404 if no app has this vanity URL.
func vanityProxyHandler(srv *server.Server) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        slug := chi.URLParam(r, "vanity")

        // Fast path: check in-memory cache. Rejects /favicon.ico,
        // /robots.txt, mistyped URLs, etc. without hitting the DB.
        _, ok := srv.VanityCache.Lookup(slug)
        if !ok {
            http.NotFound(w, r)
            return
        }

        // Cache hit — fetch full app row from DB (needed for auth,
        // active bundle, etc.)
        app, err := srv.DB.GetAppByVanityURL(slug)
        if err != nil {
            slog.Error("vanity: db error", "slug", slug, "error", err)
            http.Error(w, "internal error", http.StatusInternalServerError)
            return
        }
        if app == nil {
            // Cache stale — slug was in cache but not in DB.
            // Remove from cache and return 404.
            srv.VanityCache.Remove(slug)
            http.NotFound(w, r)
            return
        }

        // Rewrite the request to look like /app/{name}/* so the
        // standard proxy handler can process it.
        r.URL.Path = "/app/" + app.Name + "/" + stripVanityPrefix(r.URL.Path, slug)

        // Set the chi URL param so proxy.Handler can read it
        rctx := chi.RouteContext(r.Context())
        rctx.URLParams.Add("name", app.Name)

        proxy.Handler(srv).ServeHTTP(w, r)
    })
}

func stripVanityPrefix(path, slug string) string {
    prefix := "/" + slug
    stripped := strings.TrimPrefix(path, prefix)
    stripped = strings.TrimPrefix(stripped, "/")
    return stripped
}

func vanityRedirectTrailingSlash(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        slug := chi.URLParam(r, "vanity")

        // Fast path: check cache before redirecting
        if _, ok := srv.VanityCache.Lookup(slug); !ok {
            http.NotFound(w, r)
            return
        }

        http.Redirect(w, r, "/"+slug+"/", http.StatusMovedPermanently)
    }
}
```

**Route ordering concern:** the `/{vanity}` catch-all could match
requests intended for other top-level paths. The key insight is that chi
matches more specific routes first — `/healthz`, `/login`, `/api/v1/*`
are all registered before `/{vanity}` and take precedence. The vanity
catch-all only matches paths that don't match any other route.

**Auth on vanity routes:** vanity proxy routes go through the same
app-plane auth middleware as `/app/{name}/`. The middleware is applied
inside `proxy.Handler`, which the vanity handler delegates to after
resolving the app.

**Tests:**

- `GET /my-dashboard/` with valid vanity URL — proxied to correct app
- `GET /my-dashboard/` with no matching vanity URL — 404
- `GET /my-dashboard` (no trailing slash) — 301 redirect
- `GET /api/v1/apps` — still works (not matched as vanity)
- `GET /login` — still works (not matched as vanity)
- `GET /healthz` — still works (not matched as vanity)

### Step 7: Tag management API

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

### Step 8: Catalog API

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
            "vanity_url": "sales-dashboard",
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

**`url` field:** if the app has a vanity URL, `url` is `/{vanity}/`.
Otherwise, it's `/app/{name}/`. This gives clients the canonical URL to
link to.

**`status` field:** derived from the worker map (same as `GET /apps`).

**DB query for catalog:**

The catalog query must filter by access control. This is the most complex
query in blockyard. For admins, it returns all apps. For other callers,
it returns apps where:

- The caller is the owner, OR
- The caller has an explicit ACL grant (user or group), OR
- The app's `access_type` is `'public'`

```go
func (db *DB) ListCatalog(params CatalogParams) ([]AppRow, int, error) {
    var conditions []string
    var args []any

    // Access control filter
    if params.CallerRole >= auth.RoleAdmin {
        // Admin sees everything — no filter
    } else if params.CallerSub != "" {
        accessFilter := `(
            apps.owner = ?
            OR apps.access_type = 'public'
            OR EXISTS (
                SELECT 1 FROM app_access
                WHERE app_access.app_id = apps.id
                AND (
                    (app_access.kind = 'user' AND app_access.principal = ?)
                    OR (app_access.kind = 'group' AND app_access.principal IN (%s))
                )
            )
        )`
        groupPlaceholders := strings.Repeat("?,", len(params.CallerGroups))
        groupPlaceholders = strings.TrimSuffix(groupPlaceholders, ",")
        if groupPlaceholders == "" {
            groupPlaceholders = "''" // no groups — will never match
        }
        accessFilter = fmt.Sprintf(accessFilter, groupPlaceholders)

        conditions = append(conditions, accessFilter)
        args = append(args, params.CallerSub, params.CallerSub)
        for _, g := range params.CallerGroups {
            args = append(args, g)
        }
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
        `SELECT id, name, owner, access_type, vanity_url, title, description,
                active_bundle, max_workers_per_app,
                max_sessions_per_worker, memory_limit, cpu_limit,
                created_at, updated_at
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
    CallerGroups []string
    CallerRole   auth.Role
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
            params.CallerGroups = caller.Groups
            params.CallerRole = caller.Role
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

            url := "/app/" + app.Name + "/"
            if app.VanityURL != nil {
                url = "/" + *app.VanityURL + "/"
            }

            items = append(items, catalogItem{
                ID:          app.ID,
                Name:        app.Name,
                Title:       app.Title,
                Description: app.Description,
                Owner:       app.Owner,
                VanityURL:   app.VanityURL,
                Tags:        tagNames,
                Status:      status,
                URL:         url,
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
- App with vanity URL has correct `url` field
- Status field reflects running/stopped state
