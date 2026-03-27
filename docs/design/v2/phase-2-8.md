# Phase 2-8: Backend Prerequisites + Multi-Page Navigation

Backend changes (schema, APIs, RBAC) and navigation restructure needed
to support the per-app management sidebar (phase 2-9). Converts the
single-page dashboard into a multi-page layout with persistent left
navigation, htmx fragment loading, and four distinct pages.

Depends on phases 2-2 (rollback, soft-delete), 2-3 (pre-warming config),
and 2-7 (refresh API).

Content filtering (search + tag) is already implemented in the
dashboard — this phase does not revisit it.

## Sessions Table

A new `sessions` table tracks the full chain: **user → app → worker →
session → logs**. This enables activity metrics (phase 2-9 Overview
tab) and prepares for future per-session log filtering.

```sql
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    app_id      TEXT NOT NULL REFERENCES apps(id),
    worker_id   TEXT NOT NULL,
    user_sub    TEXT,           -- NULL for public/unauthenticated apps
    started_at  TEXT NOT NULL,
    ended_at    TEXT,           -- NULL while active
    status      TEXT NOT NULL DEFAULT 'active'  -- active / ended / crashed
);

CREATE INDEX idx_sessions_app_started ON sessions(app_id, started_at DESC);
CREATE INDEX idx_sessions_user ON sessions(user_sub, app_id, started_at DESC);
CREATE INDEX idx_sessions_worker ON sessions(worker_id, started_at DESC);
```

This table serves three purposes:

1. **Activity metrics** — derived from session records:
   - Total views: `COUNT(*) WHERE app_id = ?`
   - Last 7 days: `COUNT(*) WHERE app_id = ? AND started_at >= ?`
   - Unique visitors: `COUNT(DISTINCT user_sub) WHERE app_id = ?`
   - Avg session duration: `AVG(ended_at - started_at) WHERE ended_at IS NOT NULL`
2. **Runtime data** — live session-to-user-to-worker mapping.
3. **Debugging** — multiple lookup paths:
   - By user: "alice had a crash" → filter by `user_sub`
   - By worker: "worker is misbehaving" → filter by `worker_id`
   - By status: "what crashed recently?" → filter by `status = 'crashed'`

No separate `app_views` counter table is needed — all activity metrics
are derived from sessions.

**Future: per-session log filtering.** The sessions table maps each
session to a worker. Logs are currently captured per-worker (Docker
container stdout/stderr). When `max_sessions_per_worker = 1`, worker
logs are effectively session logs. For shared workers, log lines from
multiple sessions are interleaved — the same trade-off Posit Connect
makes. Per-session log annotation (tagging log lines with session
tokens at the R level) is deferred to a future phase.

## Bundle Schema Changes

Add deployment tracking columns to the existing `bundles` table:

```sql
ALTER TABLE bundles ADD COLUMN deployed_by TEXT;
ALTER TABLE bundles ADD COLUMN deployed_at TEXT;
```

- `deployed_by` — user_sub of the person who triggered the deployment.
  Set at bundle creation time in `UploadBundle()`, since the async
  restore goroutine does not have access to the caller context. For
  rollbacks, set to the caller who triggered the rollback.
- `deployed_at` — timestamp of bundle activation (distinct from
  `uploaded_at` which records the upload time before build). Set in
  `ActivateBundle()` when the build completes.

**Migration:** For existing bundles with status "ready", set
`deployed_at = uploaded_at` and `deployed_by` to the app owner.
Bundles with status "pending" or "failed" get NULL for both.

**Implementation detail:** `deployed_by` is stored on the bundle row
at INSERT time (in `CreateBundle`), passed from `UploadBundle` which
has access to `auth.CallerFromContext()`. `deployed_at` is set later
by `ActivateBundle` when the restore completes. For rollbacks, both
fields are set atomically in the rollback handler.

## Session Lifecycle Tracking

The proxy layer must create and update session records as it routes
requests to workers.

### Session Creation

When the proxy assigns a user to a worker (new session cookie
assignment in `proxy.go`), it:

1. Generates a session ID (same one stored in the routing cookie).
2. Inserts a row into `sessions` with status "active".

**Integration point:** In `proxy.go`, after the block that creates a
new session entry in `srv.Sessions.Put()` (~line 130), add a
`srv.DB.CreateSession()` call. The `callerSub` and `workerID` are
already in scope.

```go
// After srv.Sessions.Put(sessionID, entry):
srv.DB.CreateSession(sessionID, app.ID, workerID, callerSub)
```

### Session End

When a session ends normally (idle timeout eviction in
`ops/evict.go`, or explicit stop in `api/apps.go` StopApp):

```go
db.EndSession(sessionID, "ended")
```

**Integration points:**
- `ops/evict.go` — when evicting a worker, end all its sessions.
- `api/apps.go` StopApp — when stopping an app, end all sessions
  for all workers of that app.
- `session.Store` cleanup — when the in-memory session store
  expires an entry, end the corresponding DB session.

### Session Crash

When a worker crashes or is killed (detected by health polling in
`ops/health.go`), all its active sessions are marked:

```go
db.CrashWorkerSessions(workerID)
// UPDATE sessions SET status = 'crashed', ended_at = NOW()
// WHERE worker_id = ? AND status = 'active'
```

## API Changes

### API Split: GetApp vs GetAppRuntime

The current `GET /api/v1/apps/{id}` returns operational details
(worker list) to any user with access. Under the new RBAC model,
viewers should only see app metadata.

**`GET /api/v1/apps/{id}`** — any access. Returns app metadata only:

```json
{
  "id": "...",
  "name": "my-app",
  "owner": "...",
  "access_type": "acl",
  "active_bundle": "...",
  "max_workers_per_app": 4,
  "max_sessions_per_worker": 1,
  "memory_limit": "512m",
  "cpu_limit": 2.0,
  "title": "My App",
  "description": "...",
  "pre_warmed_seats": 1,
  "created_at": "...",
  "updated_at": "...",
  "status": "running"
}
```

The `workers` field is **removed** from this response.

**`GET /api/v1/apps/{id}/runtime`** — collaborator+ only. New
endpoint returning live operational data:

```json
{
  "workers": [
    {
      "id": "w-a3f2...",
      "bundle_id": "01ABC...",
      "status": "active",
      "idle_since": null,
      "stats": {
        "cpu_percent": 12.5,
        "memory_usage_bytes": 268435456,
        "memory_limit_bytes": 536870912
      },
      "sessions": [
        {
          "id": "s-9e1b...",
          "user_sub": "alice@company.com",
          "user_display_name": "Alice",
          "started_at": "2026-03-26T11:00:00Z"
        }
      ]
    }
  ],
  "active_sessions": 3,
  "total_views": 1247,
  "recent_views": 89,
  "unique_visitors": 42,
  "last_deployed_at": "2026-03-26T10:00:00Z"
}
```

This requires a new `ContainerStats` method on the `Backend` interface:

```go
// In backend/backend.go:
type ContainerStats struct {
    CPUPercent       float64
    MemoryUsageBytes uint64
    MemoryLimitBytes uint64
}

ContainerStats(ctx context.Context, containerID string) (*ContainerStats, error)
```

Implemented in `backend/docker/` using the Docker SDK's
`ContainerStats` API call. Returns a point-in-time snapshot (not a
stream). The mock backend returns zero values.

### RBAC Tightening

The following existing endpoints need stricter authorization:

| Endpoint | Current | New |
|----------|---------|-----|
| `GET /api/v1/apps/{id}/bundles` | any access | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/refresh` | any access | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/refresh/rollback` | any access | collaborator+ (`CanDeploy`) |

### Deployments API

`GET /api/v1/deployments` — collaborator+ per-app.

Cross-app deployment listing. Queries bundles joined with apps.
Results are filtered to apps where the caller has collaborator+
access (viewers are excluded since they cannot access the sidebar).

Query parameters:
- `page` (int, default 1)
- `per_page` (int, default 25, max 100)
- `search` (string, optional — filters by app name)
- `status` (string, optional — filters by bundle status)

Sort: `deployed_at DESC` (most recent first).

Response:

```json
{
  "deployments": [
    {
      "app_id": "...",
      "app_name": "my-app",
      "bundle_id": "01ABC...",
      "deployed_by": "alice@company.com",
      "deployed_at": "2026-03-26T10:00:00Z",
      "status": "ready"
    }
  ],
  "total": 42,
  "page": 1,
  "per_page": 25
}
```

### Sessions API

`GET /api/v1/apps/{id}/sessions` — collaborator+.

List sessions for an app, most recent first. Default: last 50 sessions.

Query parameters:
- `user` (string, optional — filter by user_sub)
- `status` (string, optional — filter by status)
- `limit` (int, default 50, max 200)

Response:

```json
{
  "sessions": [
    {
      "id": "01ABC...",
      "app_id": "...",
      "worker_id": "w-a3f2...",
      "user_sub": "alice@company.com",
      "started_at": "2026-03-26T11:00:00Z",
      "ended_at": "2026-03-26T11:15:00Z",
      "status": "ended"
    }
  ]
}
```

### Form-Encoded PATCH Support

`PATCH /api/v1/apps/{id}` must accept both JSON
(`Content-Type: application/json`) and form-encoded
(`Content-Type: application/x-www-form-urlencoded`) request bodies.

For htmx requests (detected via `HX-Request` header), the response
is an HTML fragment instead of JSON — either a success indicator or
a validation error. For non-htmx requests, the existing JSON response
is unchanged.

## Database Operations

New DB methods:

```go
// Sessions
CreateSession(id, appID, workerID, userSub string) error
EndSession(id string, status string) error
CrashWorkerSessions(workerID string) error
ListSessions(appID string, opts SessionListOpts) ([]SessionRow, error)
GetSession(id string) (*SessionRow, error)

// Activity metrics (derived from sessions)
CountSessions(appID string) (int, error)
CountRecentSessions(appID string, since time.Time) (int, error)
CountUniqueVisitors(appID string) (int, error)

// Deployment tracking
SetBundleDeployed(bundleID, deployedBy string) error
ListDeployments(opts DeploymentListOpts) ([]DeploymentRow, int, error)

// Collaborators display names (used by phase 2-9 Collaborators tab)
ListAppAccessWithNames(appID string) ([]AppAccessWithNameRow, error)
// Joins app_access with users to get display names.
// Returns principal, kind, role, granted_at, granted_by, plus
// user_name and user_email from the users table (may be empty
// if the principal has never logged in).
```

## Authorization Model

### RBAC Rules

The per-app management sidebar (phase 2-9) is restricted to users with
**collaborator or higher** access to the app. Viewers can only view
the running app via `/app/{name}/` — they do not see the gear icon
and cannot access any sidebar tabs or their backing API endpoints.

This phase establishes the API-level enforcement. Phase 2-9 adds the
UI-level enforcement (gear icon visibility, tab rendering).

### API Authorization Table

| Endpoint | Required relation |
|----------|------------------|
| `GET /api/v1/apps/{id}` | any access (metadata only, no workers) |
| `GET /api/v1/apps/{id}/runtime` | collaborator+ |
| `PATCH /api/v1/apps/{id}` | collaborator+ (`CanUpdateConfig`) |
| `DELETE /api/v1/apps/{id}` | owner+ (`CanDelete`) |
| `GET /api/v1/apps/{id}/bundles` | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/bundles` | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/rollback` | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/start` | collaborator+ (`CanStartStop`) |
| `POST /api/v1/apps/{id}/stop` | collaborator+ (`CanStartStop`) |
| `GET /api/v1/apps/{id}/logs` | collaborator+ (`CanDeploy`) |
| `GET /api/v1/apps/{id}/access` | owner+ (`CanManageACL`) |
| `POST /api/v1/apps/{id}/access` | owner+ (`CanManageACL`) |
| `DELETE /api/v1/apps/{id}/access/...` | owner+ (`CanManageACL`) |
| `POST /api/v1/apps/{id}/refresh` | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/refresh/rollback` | collaborator+ (`CanDeploy`) |
| `GET /api/v1/deployments` | collaborator+ (per-app filtered) |
| `GET /api/v1/apps/{id}/sessions` | collaborator+ |

## New Dependency

htmx is included as a vendored static asset (`internal/ui/static/htmx.min.js`)
served alongside `style.css`. Single file, ~14 KB gzipped. No npm, no
build step.

## Navigation Restructure

The current single-page dashboard (app grid + user info + API keys) is
split into four pages with a persistent left navigation sidebar.

```
┌─────────────────────┬──────────────────────────────────┐
│ blockyard           │                                  │
│                     │  [page content]                  │
│ ● Apps              │                                  │
│ ○ Deployment History│                                  │
│ ○ API Keys          │                                  │
│ ○ Profile           │                                  │
│                     │                                  │
│                     │                                  │
│ v0.x.x              │                                  │
└─────────────────────┴──────────────────────────────────┘
```

The left nav is a fixed-width column (~180px) present on all pages.
It shows the blockyard logo/name at the top, navigation links with
active state highlighting, and the version number at the bottom.

**API Keys nav link conditionality:** The API Keys link is only shown
when OpenBao is configured. Every page template receives an
`OpenbaoEnabled` flag via a shared layout data struct. When false, the
link is not rendered and the `/api-keys` route returns a redirect to
`/`.

```html
<nav class="left-nav">
    <div class="left-nav-brand">blockyard</div>
    <a href="/" class="left-nav-link {{if eq .ActivePage "apps"}}active{{end}}">Apps</a>
    <a href="/deployments" class="left-nav-link {{if eq .ActivePage "deployments"}}active{{end}}">Deployment History</a>
    {{if .OpenbaoEnabled}}
    <a href="/api-keys" class="left-nav-link {{if eq .ActivePage "api-keys"}}active{{end}}">API Keys</a>
    {{end}}
    <a href="/profile" class="left-nav-link {{if eq .ActivePage "profile"}}active{{end}}">Profile</a>
    <div class="left-nav-version">{{.Version}}</div>
</nav>
```

### Apps Page (`/`)

The app grid with search and tag filter. This is the existing dashboard
content minus the user identity header and API keys section. The gear
icon on each card opens the per-app management sidebar (phase 2-9).
The gear icon is only rendered for users with collaborator+ access
to the app.

```html
<a href="/app/{{.Name}}/" class="app-card">
    {{if .CanManage}}
    <button class="app-card-gear"
            hx-get="/ui/apps/{{.Name}}/sidebar"
            hx-target="#sidebar"
            hx-swap="innerHTML"
            aria-label="Manage {{.Name}}"
            onclick="event.preventDefault()">⚙</button>
    {{end}}
    <!-- existing card content -->
</a>
```

The sidebar container is added to the page but is non-functional until
phase 2-9 registers the fragment routes:

```html
<aside id="sidebar" class="sidebar"></aside>
<div id="sidebar-overlay" class="sidebar-overlay" hx-on:click="closeSidebar()"></div>
```

### Deployment History Page (`/deployments`)

A cross-app timeline of all deployments the user has visibility into
(collaborator+ on the respective apps).

| Column | Content |
|--------|---------|
| App | App name (links to app card sidebar in phase 2-9) |
| Bundle | Bundle ID (truncated) |
| Deployed by | User who triggered the deployment |
| Deployed | Relative timestamp (e.g., "2 hours ago") |
| Status | Badge: success / building / failed |

```html
<div class="page-header">
    <h1>Deployment History</h1>
    <input type="search" name="search" placeholder="Search deployments..."
           class="search-input">
</div>
<table class="data-table">
    <thead>
        <tr>
            <th>App</th>
            <th>Bundle</th>
            <th>Deployed by</th>
            <th>Deployed</th>
            <th>Status</th>
        </tr>
    </thead>
    <tbody>
    {{range .Deployments}}
        <tr>
            <td>{{.AppName}}</td>
            <td class="monospace">{{.BundleID | truncate}}</td>
            <td>{{.DeployedBy}}</td>
            <td>{{.DeployedAt | timeAgo}}</td>
            <td><span class="status-badge status-{{.Status}}">{{.Status}}</span></td>
        </tr>
    {{end}}
    </tbody>
</table>
{{template "pagination" .Pagination}}
```

Sorted by deployment time, most recent first. Paginated.

### API Keys Page (`/api-keys`)

Third-party credential management, moved from the dashboard. Manages
credentials for external services (e.g., OpenAI, Anthropic) stored in
Vault/Openbao.

Only rendered if Openbao is configured in the server config. If not
configured, the route redirects to `/`. Each service shows its label,
current status (configured / not set), and an input to set or update
the key.

**Redirect target change:** The credential save form currently
redirects to `/?credential_saved=1`. The htmx approach replaces the
full-page redirect with an inline fragment swap:

```html
<div class="page-header">
    <h1>API Keys</h1>
    <p class="page-description">Manage credentials for external services.</p>
</div>
{{range .Services}}
<div class="credential-row">
    <div class="credential-info">
        <span class="credential-label">{{.Label}}</span>
        <span class="status-badge status-{{.Status}}">{{.Status}}</span>
    </div>
    <form class="credential-form"
          hx-post="/api/v1/users/me/credentials/{{.ID}}"
          hx-target="closest .credential-row"
          hx-swap="outerHTML">
        <input type="password" name="key" placeholder="Enter API key" required>
        <button type="submit" class="btn btn-sm">Save</button>
    </form>
</div>
{{end}}
```

### Profile Page (`/profile`)

User identity, role information, sign out, and personal access token
(PAT) management.

**Identity section** (read-only):

| Field | Content |
|-------|---------|
| Display name | User's display name from identity provider |
| Email | User's email address |
| Role | admin / publisher / viewer |

```html
<div class="page-header">
    <h1>Profile</h1>
</div>
<div class="profile-section">
    <div class="profile-field">
        <label>Display name</label>
        <p>{{.User.DisplayName}}</p>
    </div>
    <div class="profile-field">
        <label>Email</label>
        <p>{{.User.Email}}</p>
    </div>
    <div class="profile-field">
        <label>Role</label>
        <span class="role-badge">{{.User.Role}}</span>
    </div>
    <form method="POST" action="/logout">
        <button type="submit" class="btn">Sign out</button>
    </form>
</div>
```

**Personal Access Tokens (PATs):**

PATs authenticate against the blockyard API (e.g., for CLI deployments
or CI/CD). Users can create tokens with a label, view existing tokens,
and revoke them. The token value is shown only once at creation time.

```html
<div class="pat-section">
    <h2>Personal Access Tokens</h2>
    <p class="section-description">Tokens authenticate against the blockyard API.
       Treat them like passwords.</p>

    <form class="pat-create-form"
          hx-post="/ui/tokens"
          hx-target="#pat-result"
          hx-swap="innerHTML">
        <input type="text" name="label" placeholder="Token label (e.g., CI deploy)" required>
        <button type="submit" class="btn">Create token</button>
    </form>
    <div id="pat-result"></div>

    {{if .Tokens}}
    <table class="data-table">
        <thead>
            <tr><th>Label</th><th>Created</th><th>Last used</th><th></th></tr>
        </thead>
        <tbody>
        {{range .Tokens}}
            <tr>
                <td>{{.Label}}</td>
                <td>{{.CreatedAt | timeAgo}}</td>
                <td>{{if .LastUsedAt}}{{.LastUsedAt | timeAgo}}{{else}}Never{{end}}</td>
                <td><button class="btn btn-sm btn-danger"
                            hx-delete="/api/v1/users/me/tokens/{{.ID}}"
                            hx-confirm="Revoke token '{{.Label}}'? This cannot be undone."
                            hx-target="closest tr"
                            hx-swap="outerHTML swap:0.2s">Revoke</button></td>
            </tr>
        {{end}}
        </tbody>
    </table>
    {{else}}
    <p class="empty-state">No tokens created.</p>
    {{end}}
</div>
```

**Token creation fragment** (`POST /ui/tokens`):

A UI-specific endpoint that wraps the existing token creation logic
and returns an HTML fragment:

```html
<div class="pat-created">
    <p class="pat-warning">Copy this token now — it will not be shown again.</p>
    <div class="pat-value">
        <code>{{.Token}}</code>
        <button onclick="navigator.clipboard.writeText('{{.Token}}')"
                class="btn btn-sm">Copy</button>
    </div>
</div>
```

## Page Routes

Full-page routes, all requiring authentication (session cookie):

| Method | Path | Returns | Auth |
|--------|------|---------|------|
| GET | `/` | Full page | soft auth (show landing if not authenticated) |
| GET | `/deployments` | Full page | required |
| GET | `/api-keys` | Full page | required (redirect to `/` if Openbao not configured) |
| GET | `/profile` | Full page | required |
| POST | `/ui/tokens` | HTML fragment | required |

## Templates

Template files in `internal/ui/templates/`:

```
templates/
├── base.html              # (existing — modified: add left nav, htmx script tag)
├── landing.html           # (existing)
├── apps.html              # Apps page (replaces dashboard.html — app grid, sidebar container)
├── deployments.html       # Deployment history page
├── api_keys.html          # API keys page (credential management)
├── profile.html           # Profile page (identity, PATs)
└── pat_created.html       # One-time token display fragment
```

Page templates (`apps.html`, `deployments.html`, `api_keys.html`,
`profile.html`) extend `base.html` which provides the left nav and
common layout. They are parsed paired with `base.html`.

The `pat_created.html` fragment is parsed standalone (no base wrapper).
Phase 2-9 adds many more fragment templates and extends the `UI` struct
to maintain separate template maps for pages vs fragments.

## Static Assets

```
static/
├── style.css        # (existing — extended with nav, page, sidebar shell styles)
└── htmx.min.js      # vendored htmx 2.x (~14 KB gzipped)
```

htmx is loaded via a `<script>` tag in `base.html`:

```html
<script src="/static/htmx.min.js"></script>
```

## CSS Additions

Key new styles (added to existing `style.css`):

**Left navigation:**
- **`.left-nav`** — fixed left column, ~180px wide, full height, dark
  background, flex column.
- **`.left-nav-brand`** — logo/name at top.
- **`.left-nav-link`** — nav item with hover and active states.
- **`.left-nav-link.active`** — highlighted background for current page.
- **`.left-nav-version`** — version text at bottom, muted.
- **`.page-layout`** — flex row: left nav + main content area with
  `margin-left` matching nav width.

**Page-level:**
- **`.page-header`** — page title + optional search/description.
- **`.data-table`** — standard table for deployment history, PATs.
- **`.status-badge`** — deployment status indicators.
- **`.role-badge`** — user role indicators.
- **`.credential-row`** — service label + status + key input form.
- **`.profile-section`** — stacked read-only profile fields.
- **`.profile-field`** — label + value pair.
- **`.pat-section`** — PAT management area.
- **`.pat-create-form`** — inline label input + create button.
- **`.pat-created`** — one-time token display with copy button.
- **`.pat-warning`** — yellow/amber warning text for token display.

**Sidebar shell** (non-functional until phase 2-9):
- **`.sidebar`** — fixed right, full height, responsive width
  (`min-width: 28rem; width: 50%; max-width: 720px`), white background,
  box shadow, transform/transition for slide-in, overflow-y auto.
- **`.sidebar.open`** — `transform: translateX(0)` (default is
  `translateX(100%)`).
- **`.sidebar-overlay`** — fixed full-screen, semi-transparent backdrop,
  hidden by default, shown when sidebar is open.
- **`.app-card-gear`** — gear icon positioning in app card.

**Utility:**
- **`.btn-danger`** — red background, white text.

## Sidebar JS

Minimal vanilla JS for sidebar open/close (ready for phase 2-9):

```js
function closeSidebar() {
    document.getElementById('sidebar').classList.remove('open');
    document.getElementById('sidebar-overlay').classList.remove('open');
    document.getElementById('sidebar').innerHTML = '';
}

// Open sidebar after htmx loads content.
document.body.addEventListener('htmx:afterSwap', function(e) {
    if (e.detail.target.id === 'sidebar') {
        document.getElementById('sidebar').classList.add('open');
        document.getElementById('sidebar-overlay').classList.add('open');
    }
});
```

## Deliverables

**Backend:**

1. **Database migration** — sessions table, bundle schema additions,
   indexes.
2. **Session lifecycle** — create/end/crash tracking in the proxy
   and worker lifecycle code.
3. **Bundle deployment tracking** — populate `deployed_by` at upload
   time, `deployed_at` at activation time.
4. **Backend interface** — add `ContainerStats()` method for live
   CPU/memory data.
5. **API split** — remove workers from `GetApp`, add
   `GET /api/v1/apps/{id}/runtime` (collaborator+).
6. **RBAC tightening** — `ListBundles`, `PostRefresh`,
   `PostRefreshRollback` require collaborator+.
7. **Deployments API** — `GET /api/v1/deployments` with pagination
   and collaborator+ per-app filtering.
8. **Sessions API** — `GET /api/v1/apps/{id}/sessions` with filtering.
9. **Collaborator display names** — `ListAppAccessWithNames()` DB
   method joining `app_access` with `users`.
10. **Form-encoded PATCH** — `UpdateApp` handler accepts both JSON
    and form-encoded bodies, returns HTML fragments for htmx requests.

**Navigation and pages:**

11. **htmx integration** — vendor htmx.min.js, add script tag to base
    template.
12. **Left navigation** — persistent nav sidebar with Apps, Deployment
    History, API Keys (conditional), Profile links. Active page
    highlighting. `OpenbaoEnabled` flag in layout data.
13. **Apps page** — app grid with search/filter (migrated from
    dashboard, credentials section removed). Gear icon conditional
    on collaborator+ access. Sidebar shell (non-functional).
14. **Deployment History page** — cross-app deployment table with
    search, pagination, collaborator+ visibility.
15. **API Keys page** — third-party credential management (migrated
    from dashboard). Redirect to `/` when Openbao not configured.
16. **Profile page** — user identity, role display, sign out.
17. **PAT management UI** — create (with `POST /ui/tokens` fragment
    endpoint), list, and revoke personal access tokens on the Profile
    page.
18. **CSS** — left nav, page layouts, sidebar shell, data tables,
    status/role badges, credential forms, profile fields, PAT section.

## Implementation Steps

### Step 1: Database Migration

Add sessions table and bundle columns. Write up/down migration for
both SQLite and PostgreSQL.

### Step 2: Session Lifecycle Tracking

Instrument the proxy to create session records on assignment, end them
on disconnect/eviction, and crash them on worker failure. Integration
points: `proxy.go` (create), `ops/evict.go` (end), `api/apps.go`
StopApp (end), `ops/health.go` (crash).

### Step 3: Bundle Deployment Tracking

Store `deployed_by` at bundle INSERT time in `UploadBundle()` (caller
available from context). Set `deployed_at` in `ActivateBundle()` when
restore completes. Update rollback handler similarly. Run backfill
migration for existing bundles.

### Step 4: Backend Interface + API Changes

Add `ContainerStats()` to `Backend` interface. Implement in Docker
backend. Add `GET /api/v1/apps/{id}/runtime` endpoint. Remove workers
from `GetApp` response. Tighten RBAC on `ListBundles`, `PostRefresh`,
`PostRefreshRollback`. Make `UpdateApp` accept form-encoded bodies and
return HTML fragments for htmx requests.

### Step 5: Deployments + Sessions API

Implement `GET /api/v1/deployments` with collaborator+ per-app
filtering. Implement `GET /api/v1/apps/{id}/sessions`. Add
`ListAppAccessWithNames()` DB method.

### Step 6: htmx + Left Navigation + Page Restructure

Vendor htmx.min.js. Modify `base.html` to include left nav and htmx
script tag. Create `apps.html` (migrated from `dashboard.html`,
credentials removed), `deployments.html`, `api_keys.html`, and
`profile.html`. Register new page routes. Add `OpenbaoEnabled` flag
to layout data. Update credential form redirect targets. Verify
navigation between pages works.

### Step 7: Deployment History Page

Build the deployment history table consuming
`GET /api/v1/deployments`. Search, pagination,
collaborator+ visibility.

### Step 8: API Keys Page

Migrate credential management from the dashboard to its own page.
Wire htmx form submissions (replacing the existing inline JS).
Redirect to `/` when Openbao not configured.

### Step 9: Profile Page + PAT Management UI

Implement the profile page with identity display and sign out.
Wire PAT management UI: token creation via `POST /ui/tokens`
fragment endpoint with one-time display, token table with revoke
buttons.

### Step 10: Sidebar Shell + CSS

Add sidebar container and overlay to apps page. Wire gear icon
(collaborator+ conditional) to open sidebar. Add sidebar open/close
JS and CSS transitions. Non-functional until phase 2-9 registers
fragment routes.

### Step 11: Tests

Integration tests for session lifecycle (create → end, create → crash).
API tests for runtime endpoint, deployments listing, session listing.
RBAC tests verifying viewer cannot access runtime API, bundles listing,
or refresh endpoints. Tests for form-encoded PATCH and htmx fragment
responses.
