# Phase 2-8: Web UI Expansion

Restructures the single-page dashboard into a multi-page layout with
persistent left navigation, adds a per-app management sidebar, and
improves operational visibility. Server-rendered HTML with htmx for
dynamic fragment loading. No JavaScript framework.

Depends on phases 2-2 (rollback, soft-delete), 2-3 (pre-warming config),
and 2-7 (refresh API).

Content filtering (search + tag) is already implemented in the
dashboard — this phase does not revisit it.

## Sessions Table

A new `sessions` table tracks the full chain: **user → app → worker →
session → logs**. This enables the primary debugging workflow: "user X
reported a problem" → find their session → read its logs.

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

1. **Logs** — per-session log drill-down (user → session → logs).
2. **Activity metrics** — derived from session records:
   - Total views: `COUNT(*) WHERE app_id = ?`
   - Last 7 days: `COUNT(*) WHERE app_id = ? AND started_at >= ?`
   - Unique visitors: `COUNT(DISTINCT user_sub) WHERE app_id = ?`
   - Avg session duration: `AVG(ended_at - started_at) WHERE ended_at IS NOT NULL`
3. **Debugging** — multiple lookup paths:
   - By user: "alice had a crash" → filter by `user_sub`
   - By worker: "worker is misbehaving" → filter by `worker_id`
   - By status: "what crashed recently?" → filter by `status = 'crashed'`

No separate `app_views` counter table is needed — all activity metrics
are derived from sessions.

## Bundle Schema Changes

Add deployment tracking columns to the existing `bundles` table:

```sql
ALTER TABLE bundles ADD COLUMN deployed_by TEXT;
ALTER TABLE bundles ADD COLUMN deployed_at TEXT;
```

- `deployed_by` — user_sub of the person who triggered the deployment
  (upload or rollback). Set in `ActivateBundle()`.
- `deployed_at` — timestamp of bundle activation (distinct from
  `uploaded_at` which records the upload time before build).

**Migration:** For existing bundles with status "ready", set
`deployed_at = uploaded_at` and `deployed_by` to the app owner.
Bundles with status "pending" or "failed" get NULL for both.

## Session Lifecycle Tracking

The proxy layer must create and update session records as it routes
requests to workers.

### Session Creation

When the proxy assigns a user to a worker (new WebSocket connection or
first HTTP request in a session), it:

1. Generates a session ID.
2. Inserts a row into `sessions` with status "active".
3. Passes the session ID to the worker so it can tag log lines.

```go
// In the proxy's session assignment logic:
sessionID := ulid.New()
db.CreateSession(sessionID, appID, workerID, userSub)
// Pass sessionID to worker via environment or header
```

### Session End

When a session ends normally (WebSocket close, idle timeout):

```go
db.EndSession(sessionID, "ended")
```

### Session Crash

When a worker crashes or is killed, all its active sessions are marked:

```go
db.CrashWorkerSessions(workerID)
// UPDATE sessions SET status = 'crashed', ended_at = NOW()
// WHERE worker_id = ? AND status = 'active'
```

## Per-Session Log Tagging

Workers must tag log output with session IDs so logs can be stored and
queried per-session.

### Log Format

Each log line is prefixed with the session ID:

```
[session:01ABC123] 2026-03-26T11:02:51Z stdout: Loading package...
[session:01ABC123] 2026-03-26T11:02:52Z stderr: Warning: deprecated function
```

### Log Storage

Log lines are written to per-session files or a structured log store
keyed by session ID. The exact storage mechanism depends on the
existing log infrastructure, but must support:

- Retrieving all log lines for a given session ID.
- Streaming new log lines for an active session.
- Retention policy (e.g., 30 days).

## API Endpoints

### Deployments

`GET /api/v1/deployments`

Cross-app deployment listing. Queries bundles joined with apps,
filtered by user role:

- **admin** — all deployments.
- **publisher** — deployments for own apps.
- **viewer** — deployments for apps shared with them.

Query parameters:
- `page` (int, default 1)
- `per_page` (int, default 25, max 100)
- `search` (string, optional — filters by app name)

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

### Sessions

`GET /api/v1/apps/{id}/sessions`

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

### Session Logs

`GET /api/v1/apps/{id}/sessions/{sid}/logs`

Historical logs for a completed or active session. Returns plain text.

`GET /api/v1/apps/{id}/sessions/{sid}/logs?stream=true`

Streams live logs for an active session via chunked transfer encoding.
Connection stays open until the session ends or the client disconnects.

### App Overview Data

`GET /api/v1/apps/{id}` — extend existing response with:

```json
{
  "active_sessions": 3,
  "total_views": 1247,
  "recent_views": 89,
  "unique_visitors": 42,
  "last_deployed_at": "2026-03-26T10:00:00Z"
}
```

These are derived from the sessions table and bundle deployment
columns. No additional storage needed.

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
```

## New Dependency

```go
// go.mod — no Go dependency; htmx is a client-side JS library.
```

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

```html
<nav class="left-nav">
    <div class="left-nav-brand">blockyard</div>
    <a href="/" class="left-nav-link {{if eq .ActivePage "apps"}}active{{end}}">Apps</a>
    <a href="/deployments" class="left-nav-link {{if eq .ActivePage "deployments"}}active{{end}}">Deployment History</a>
    <a href="/api-keys" class="left-nav-link {{if eq .ActivePage "api-keys"}}active{{end}}">API Keys</a>
    <a href="/profile" class="left-nav-link {{if eq .ActivePage "profile"}}active{{end}}">Profile</a>
    <div class="left-nav-version">{{.Version}}</div>
</nav>
```

### Apps Page (`/`)

The app grid with search and tag filter. This is the existing dashboard
content minus the user identity header and API keys section. The gear
icon on each card opens the per-app management sidebar (see below).

### Deployment History Page (`/deployments`)

A cross-app timeline of all deployments the user has visibility into.
Provides a single view for answering "what changed?" without checking
individual apps.

| Column | Content |
|--------|---------|
| App | App name (links to app card sidebar) |
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
            <td><a href="/"
                   hx-get="/ui/apps/{{.AppName}}/sidebar"
                   hx-target="#sidebar"
                   hx-swap="innerHTML">{{.AppName}}</a></td>
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

Sorted by deployment time, most recent first. Paginated. Admins see
all deployments; publishers see their own; viewers see deployments for
apps shared with them.

### API Keys Page (`/api-keys`)

Third-party credential management, moved from the dashboard. Manages
credentials for external services (e.g., OpenAI, Anthropic) stored in
Vault/Openbao.

Only rendered if Openbao is configured in the server config. Each
service shows its label, current status (configured / not set), and
an input to set or update the key.

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
          hx-post="/api/v1/users/me/tokens"
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

**Token creation response fragment** (returned by `POST /api/v1/users/me/tokens`):

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

## Apps Page: Sidebar

### Gear Icon

Each app card gains a gear icon (⚙ or SVG) that opens the management
sidebar. The card itself remains a link to `/app/{name}/` (the running
app). The gear icon is positioned in the card's top-right corner and
stops event propagation so clicking it does not navigate to the app.

```html
<a href="/app/{{.Name}}/" class="app-card">
    <button class="app-card-gear"
            hx-get="/ui/apps/{{.Name}}/sidebar"
            hx-target="#sidebar"
            hx-swap="innerHTML"
            aria-label="Manage {{.Name}}"
            onclick="event.preventDefault()">⚙</button>
    <!-- existing card content -->
</a>
```

### Sidebar Container

A fixed-position panel on the right side of the viewport. Hidden by
default. When htmx populates `#sidebar`, CSS transitions slide it in.
Clicking outside or the close button clears the content and slides it
out.

```html
<!-- appended to dashboard template -->
<aside id="sidebar" class="sidebar"></aside>
<div id="sidebar-overlay" class="sidebar-overlay" hx-on:click="closeSidebar()"></div>
```

The sidebar scrolls independently of the dashboard. Width is responsive:
`min-width: 28rem; width: 50%; max-width: 720px`.

## Sidebar Structure

### Header

App name as the title. An external-link icon (↗) opens `/app/{name}/`
in a new tab. A close button (✕) dismisses the sidebar.

```html
<div class="sidebar-header">
    <h2>{{.App.Name}}</h2>
    <a href="/app/{{.App.Name}}/" target="_blank" title="Open app">↗</a>
    <button onclick="closeSidebar()" aria-label="Close">✕</button>
</div>
```

### Tabs

Six tabs below the header. Each tab fetches its content via htmx on
click. The first tab (Overview) loads automatically when the sidebar
opens.

```
┌──────────┬──────────┬─────────┬─────────┬───────────────┬──────┐
│ Overview │ Settings │ Runtime │ Bundles │ Collaborators │ Logs │
└──────────┴──────────┴─────────┴─────────┴───────────────┴──────┘
```

```html
<nav class="sidebar-tabs">
    <button class="tab active"
            hx-get="/ui/apps/{{.App.Name}}/tab/overview"
            hx-target="#tab-content">Overview</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/settings"
            hx-target="#tab-content">Settings</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/runtime"
            hx-target="#tab-content">Runtime</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/bundles"
            hx-target="#tab-content">Bundles</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/collaborators"
            hx-target="#tab-content">Collaborators</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/logs"
            hx-target="#tab-content">Logs</button>
</nav>
<div id="tab-content"></div>
```

Active tab state is managed with `hx-on::after-request` toggling an
`active` class. No client-side router.

## Field Editing UX

All editable fields across tabs follow a consistent pattern:

**Text inputs and textareas** — fields are directly editable. A small
save button (✓) appears next to the field when the current value differs
from the saved value. Clicking save submits via `hx-patch` and shows
inline feedback.

**Dropdowns / selects** — save automatically on `change` via
`hx-patch`, with the same inline feedback. No explicit save button
needed since selection is already an intentional action.

**Inline validation** — on save, the server returns either:
- **200** with a success indicator fragment (brief checkmark / green
  flash next to the field).
- **422** with an error fragment that swaps into a `<span class="field-error">`
  below the field (e.g., "Minimum 64MB", "Must be a multiple of 0.25").
  The field also receives a `field-invalid` class for red border styling.

```html
<!-- Example: text field with per-field save -->
<div class="field-group">
    <label for="title">Title</label>
    <div class="field-row">
        <input type="text" id="title" name="title" value="{{.App.Title}}"
               data-original="{{.App.Title}}"
               oninput="toggleSaveBtn(this)">
        <button class="field-save hidden"
                hx-patch="/api/v1/apps/{{.App.ID}}"
                hx-include="[name='title']"
                hx-target="next .field-feedback"
                hx-swap="innerHTML">✓</button>
        <span class="field-feedback"></span>
    </div>
</div>

<!-- Example: dropdown with auto-save on change -->
<div class="field-group">
    <label for="access-type">Access type</label>
    <div class="field-row">
        <select id="access-type" name="access_type"
                hx-patch="/api/v1/apps/{{.App.ID}}"
                hx-trigger="change"
                hx-target="next .field-feedback"
                hx-swap="innerHTML">
            <option value="public" {{if eq .App.AccessType "public"}}selected{{end}}>Public</option>
            <option value="logged_in" {{if eq .App.AccessType "logged_in"}}selected{{end}}>Logged in</option>
            <option value="acl" {{if eq .App.AccessType "acl"}}selected{{end}}>ACL</option>
        </select>
        <span class="field-feedback"></span>
    </div>
</div>
```

## Tab Content

### Overview Tab

**Endpoint:** `GET /ui/apps/{name}/tab/overview`

The default tab shown when the sidebar opens. Provides at-a-glance
status for the app without requiring navigation to other tabs.

| Section | Content | Data source |
|---------|---------|-------------|
| Status | Deployment status (running / stopped / failed), last deployed timestamp | App status + `bundles.deployed_at` |
| Workers | Active worker count, active session count | Worker map + `sessions` table |
| Activity | Total sessions, last 7 days, unique visitors | Derived from `sessions` table |
| Bundle | Current bundle ID (truncated), build status | `apps.active_bundle` + `bundles` |

```html
<div class="overview-grid">
    <div class="overview-card">
        <h3>Status</h3>
        <span class="status-badge status-{{.Status}}">{{.Status}}</span>
        <p class="overview-meta">Last deployed {{.LastDeployed | timeAgo}}</p>
    </div>
    <div class="overview-card">
        <h3>Workers</h3>
        <p class="overview-stat">{{.ActiveWorkers}} active</p>
        <p class="overview-meta">{{.ActiveSessions}} sessions</p>
    </div>
    <div class="overview-card">
        <h3>Activity</h3>
        <p class="overview-stat">{{.TotalViews}} total views</p>
        <p class="overview-meta">{{.RecentViews}} last 7 days</p>
    </div>
    <div class="overview-card">
        <h3>Bundle</h3>
        <p class="overview-stat">{{.ActiveBundle.ID | truncate}}</p>
        <span class="status-badge status-{{.ActiveBundle.Status}}">{{.ActiveBundle.Status}}</span>
    </div>
</div>
```

The overview cards link to their respective tabs where relevant (e.g.,
clicking the Bundle card switches to the Bundles tab).

### Settings Tab

**Endpoint:** `GET /ui/apps/{name}/tab/settings`

App metadata fields with per-field save (see Field Editing UX above).

| Field | Input Type | API Field |
|-------|-----------|-----------|
| Title | text | `title` |
| Description | textarea | `description` |
| Tags | tag chips with add/remove | `POST/DELETE /api/v1/apps/{id}/tags` |

**Soft-delete** at the bottom of this tab:

```html
<div class="danger-zone">
    <button class="btn btn-danger"
            hx-delete="/api/v1/apps/{{.App.ID}}"
            hx-confirm="Delete {{.App.Name}}? This can be undone within 30 days."
            hx-on::after-request="if(event.detail.successful) closeSidebar(); location.reload()">
        Delete app
    </button>
</div>
```

### Runtime Tab

**Endpoint:** `GET /ui/apps/{name}/tab/runtime`

| Field | Input Type | API Field |
|-------|-----------|-----------|
| Memory limit | number + unit dropdown (MB / GB) | `memory_limit` |
| CPU limit | number (`step="0.25"`) | `cpu_limit` |
| Max workers | number | `max_workers_per_app` |
| Max sessions per worker | number | `max_sessions_per_worker` |
| Pre-warmed seats | number | `pre_warmed_seats` |

All fields use per-field save (see Field Editing UX). The memory field
uses a composite input:

```html
<div class="field-group">
    <label>Memory limit</label>
    <div class="field-row">
        <input type="number" name="memory_value" value="{{.MemoryValue}}" min="64">
        <select name="memory_unit"
                hx-trigger="change"
                hx-include="[name='memory_value']"
                hx-patch="/api/v1/apps/{{.App.ID}}"
                hx-target="next .field-feedback"
                hx-swap="innerHTML">
            <option value="m" {{if eq .MemoryUnit "m"}}selected{{end}}>MB</option>
            <option value="g" {{if eq .MemoryUnit "g"}}selected{{end}}>GB</option>
        </select>
        <button class="field-save hidden"
                hx-patch="/api/v1/apps/{{.App.ID}}"
                hx-include="[name='memory_value'],[name='memory_unit']"
                hx-target="next .field-feedback"
                hx-swap="innerHTML">✓</button>
        <span class="field-feedback"></span>
    </div>
</div>
```

Inline validation examples: "Minimum 64MB", "Must be a multiple of
0.25", "Cannot exceed max_workers_per_app".

**Start / Stop controls:**

```html
<div class="app-controls">
    {{if eq .Status "running"}}
    <button class="btn" hx-post="/api/v1/apps/{{.App.ID}}/stop">Stop</button>
    {{else}}
    <button class="btn btn-primary" hx-post="/api/v1/apps/{{.App.ID}}/start">Start</button>
    {{end}}
</div>
```

### Bundles Tab

**Endpoint:** `GET /ui/apps/{name}/tab/bundles`

Lists bundles from `GET /api/v1/apps/{id}/bundles`, most recent first.
Each bundle shows:

- Bundle ID (truncated)
- Created timestamp
- Status (ready / building / failed)
- Active indicator (★) for the current bundle

Non-active ready bundles have a rollback button:

```html
<button class="btn btn-sm"
        hx-post="/api/v1/apps/{{$.App.ID}}/rollback"
        hx-vals='{"bundle_id": "{{.ID}}"}'
        hx-confirm="Roll back to bundle {{.ID | truncate}}?"
        hx-target="#tab-content"
        hx-swap="innerHTML">Rollback</button>
```

On success, the server re-renders the bundles tab with the updated
active bundle.

**Dependency refresh** (shown only for unpinned deployments):

```html
{{if not .App.IsPinned}}
<div class="refresh-section">
    <button class="btn"
            hx-post="/api/v1/apps/{{.App.ID}}/refresh"
            hx-target="#refresh-status">Refresh dependencies</button>
    <div id="refresh-status"></div>
</div>
{{end}}
```

### Collaborators Tab

**Endpoint:** `GET /ui/apps/{name}/tab/collaborators`

Manages app visibility and per-user access control. Separated from
Settings to keep metadata editing distinct from access management.

**Visibility / access type:**

```html
<div class="field-group">
    <label for="access-type">Access type</label>
    <p class="field-description">Controls who can access this app.</p>
    <div class="field-row">
        <select id="access-type" name="access_type"
                hx-patch="/api/v1/apps/{{.App.ID}}"
                hx-trigger="change"
                hx-target="next .field-feedback"
                hx-swap="innerHTML">
            <option value="public" {{if eq .App.AccessType "public"}}selected{{end}}>Public</option>
            <option value="logged_in" {{if eq .App.AccessType "logged_in"}}selected{{end}}>Logged in</option>
            <option value="acl" {{if eq .App.AccessType "acl"}}selected{{end}}>ACL</option>
        </select>
        <span class="field-feedback"></span>
    </div>
</div>
```

**ACL management** (shown when access_type = "acl"):

A list of current grants loaded from `GET /api/v1/apps/{id}/access`.
Each grant shows the user and a role dropdown. A remove button
(`hx-delete`) revokes access. An inline form adds new grants
(`hx-post`).

```html
{{if eq .App.AccessType "acl"}}
<div class="acl-section">
    <h3>Access grants</h3>
    <table class="acl-table">
        <thead><tr><th>User</th><th>Role</th><th></th></tr></thead>
        <tbody>
        {{range .Grants}}
        <tr>
            <td>{{.User}}</td>
            <td>{{.Role}}</td>
            <td><button class="btn btn-sm btn-danger"
                        hx-delete="/api/v1/apps/{{$.App.ID}}/access/{{.ID}}"
                        hx-target="closest tr"
                        hx-swap="outerHTML swap:0.2s">Remove</button></td>
        </tr>
        {{end}}
        </tbody>
    </table>
    <form class="acl-add-form"
          hx-post="/api/v1/apps/{{.App.ID}}/access"
          hx-target=".acl-table tbody"
          hx-swap="beforeend">
        <input type="text" name="user" placeholder="Username or email" required>
        <select name="role">
            <option value="viewer">Viewer</option>
            <option value="publisher">Publisher</option>
        </select>
        <button type="submit" class="btn btn-sm">Add</button>
    </form>
</div>
{{end}}
```

### Logs Tab

**Endpoint:** `GET /ui/apps/{name}/tab/logs`

The logs tab uses a user-centric drill-down. The default view lists
recent sessions showing **who** connected, to **which worker**, with
what **outcome** — enabling the primary debugging workflow: "user X
reported a problem" → find their session → read its logs.

```html
<div class="log-viewer">
    <div class="session-list">
        <h3>Sessions</h3>
        <input type="search" placeholder="Filter by user..."
               hx-get="/ui/apps/{{.App.Name}}/tab/logs"
               hx-trigger="input changed delay:300ms"
               hx-target="closest .log-viewer"
               hx-swap="outerHTML"
               name="user">
        <table class="session-table">
            <thead>
                <tr><th>User</th><th>Worker</th><th>Status</th><th>Started</th></tr>
            </thead>
            <tbody>
            {{range .Sessions}}
            <tr class="session-entry {{if eq .Status "active"}}session-active{{end}}"
                hx-get="/ui/apps/{{$.App.Name}}/tab/logs/session/{{.ID}}"
                hx-target="#log-content">
                <td>{{.UserSub | userDisplay}}</td>
                <td class="monospace">{{.WorkerID | truncate}}</td>
                <td><span class="status-badge status-{{.Status}}">{{.Status}}</span></td>
                <td>{{.StartedAt | timeAgo}}</td>
            </tr>
            {{end}}
            </tbody>
        </table>
    </div>
    <div id="log-content" class="log-content">
        <p class="log-placeholder">Select a session to view logs.</p>
    </div>
</div>
```

**Session log view fragment** (`GET /ui/apps/{name}/tab/logs/session/{sid}`):

```html
<div class="log-session-view">
    <div class="log-controls">
        <span class="log-session-label">Session {{.Session.ID | truncate}}</span>
        {{if .Session.Active}}
        <button id="log-toggle" onclick="toggleLogs('{{.Session.ID}}')">
            Start streaming
        </button>
        {{end}}
        <button onclick="clearLogs()">Clear</button>
    </div>
    <pre id="log-output" class="log-output">{{.HistoricalLogs}}</pre>
</div>

<script>
let logController = null;

function toggleLogs(sessionId) {
    if (logController) { stopLogs(); return; }
    const btn = document.getElementById('log-toggle');
    const output = document.getElementById('log-output');
    logController = new AbortController();
    btn.textContent = 'Stop streaming';

    fetch('/api/v1/apps/{{.App.ID}}/sessions/' + sessionId + '/logs?stream=true', {
        signal: logController.signal,
        headers: { 'Accept': 'text/plain' }
    }).then(resp => {
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        function read() {
            reader.read().then(({ done, value }) => {
                if (done) { stopLogs(); return; }
                output.textContent += decoder.decode(value);
                output.scrollTop = output.scrollHeight;
                read();
            });
        }
        read();
    }).catch(() => stopLogs());
}

function stopLogs() {
    if (logController) { logController.abort(); logController = null; }
    const btn = document.getElementById('log-toggle');
    if (btn) btn.textContent = 'Start streaming';
}

function clearLogs() {
    document.getElementById('log-output').textContent = '';
}
</script>
```

## Page Routes

Full-page routes, all requiring authentication (session cookie):

| Method | Path | Returns | Purpose |
|--------|------|---------|---------|
| GET | `/` | Full page | Apps page (app grid with search/filter) |
| GET | `/deployments` | Full page | Deployment history |
| GET | `/api-keys` | Full page | Third-party credential management |
| GET | `/profile` | Full page | User identity, role, PAT management |

## Fragment Routes

htmx fragment routes for sidebar tab content:

| Method | Path | Returns | Purpose |
|--------|------|---------|---------|
| GET | `/ui/apps/{name}/sidebar` | HTML fragment | Sidebar shell (header + tabs + initial Overview tab) |
| GET | `/ui/apps/{name}/tab/overview` | HTML fragment | Overview tab content |
| GET | `/ui/apps/{name}/tab/settings` | HTML fragment | Settings tab content |
| GET | `/ui/apps/{name}/tab/runtime` | HTML fragment | Runtime tab content |
| GET | `/ui/apps/{name}/tab/bundles` | HTML fragment | Bundles tab content |
| GET | `/ui/apps/{name}/tab/collaborators` | HTML fragment | Collaborators tab content |
| GET | `/ui/apps/{name}/tab/logs` | HTML fragment | Logs tab content (session list) |
| GET | `/ui/apps/{name}/tab/logs/session/{sid}` | HTML fragment | Log viewer for a specific session |

All fragment routes require authentication (user must have access to
the app). Fragments are rendered via Go templates — same `html/template`
engine, just returning partials instead of full pages.

The `/ui/` prefix distinguishes fragment routes from the REST API
(`/api/v1/`) and the app proxy (`/app/`). Fragment routes are
internal to the UI and not part of the public API contract.

## API Dependencies

This phase consumes the following API endpoints:

| Method | Path | Provided by |
|--------|------|-------------|
| GET | `/api/v1/apps/{id}/sessions` | New (this phase) |
| GET | `/api/v1/apps/{id}/sessions/{sid}/logs` | New (this phase) |
| GET | `/api/v1/apps/{id}/sessions/{sid}/logs?stream=true` | New (this phase) |
| GET | `/api/v1/deployments` | New (this phase) |
| GET | `/api/v1/apps/{id}` (extended with session/activity data) | New (this phase) |
| POST | `/api/v1/users/me/tokens` | Already implemented |
| GET | `/api/v1/users/me/tokens` | Already implemented |
| DELETE | `/api/v1/users/me/tokens/{id}` | Already implemented |

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
├── sidebar.html           # sidebar shell: header, tabs, tab-content div
├── tab_overview.html      # Overview tab partial
├── tab_settings.html      # Settings tab partial
├── tab_runtime.html       # Runtime tab partial
├── tab_bundles.html       # Bundles tab partial
├── tab_collaborators.html # Collaborators tab partial
├── tab_logs.html          # Logs tab partial (session list)
└── tab_logs_session.html  # Log viewer for a specific session
```

Page templates extend `base.html` which provides the left nav and
common layout. Tab templates are self-contained partials — no base
wrapper. They render directly into `#tab-content`.

## Static Assets

```
static/
├── style.css        # (existing — extended with sidebar, tab, log-viewer styles)
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
- **`.credential-row`** — service label + status + key input form.
- **`.profile-section`** — stacked read-only profile fields.
- **`.profile-field`** — label + value pair.
- **`.pat-section`** — PAT management area.
- **`.pat-create-form`** — inline label input + create button.
- **`.pat-created`** — one-time token display with copy button.
- **`.pat-warning`** — yellow/amber warning text for token display.

**App sidebar:**
- **`.sidebar`** — fixed right, full height, responsive width
  (`min-width: 28rem; width: 50%; max-width: 720px`), white background,
  box shadow, transform/transition for slide-in, overflow-y auto.
- **`.sidebar.open`** — `transform: translateX(0)` (default is
  `translateX(100%)`).
- **`.sidebar-overlay`** — fixed full-screen, semi-transparent backdrop,
  hidden by default, shown when sidebar is open.
- **`.sidebar-header`** — flex row, app name + external link + close button.
- **`.sidebar-tabs`** — flex row of tab buttons, bottom border.
- **`.tab.active`** — bold, border-bottom highlight.
- **`.overview-grid`** — 2x2 grid of status cards.
- **`.overview-card`** — bordered card with heading, stat, and meta text.
- **`.field-group`** — label + field-row + optional error container.
- **`.field-row`** — flex row holding input + save button + feedback span.
- **`.field-save`** — small inline save button, hidden by default, shown
  when field value differs from original.
- **`.field-feedback`** — inline success/error indicator.
- **`.field-error`** — red text below field for validation messages.
- **`.field-invalid`** — red border on input with validation error.
- **`.acl-table`** — simple table for access grants.
- **`.acl-add-form`** — inline form row for adding grants.
- **`.log-viewer`** — flex row: session list on left, log content on right.
- **`.session-list`** — scrollable list of sessions grouped by worker.
- **`.session-entry`** — clickable session item with ID, status, time.
- **`.session-active`** — highlighted style for active sessions.
- **`.log-content`** — flex column, `.log-output` has monospace font, dark
  background, max-height with overflow scroll.
- **`.danger-zone`** — top border, red-tinted button.
- **`.btn-danger`** — red background, white text.

## Sidebar JS

Minimal vanilla JS for sidebar open/close and per-field save visibility:

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

// Show/hide per-field save buttons when values change.
function toggleSaveBtn(input) {
    const saveBtn = input.closest('.field-row').querySelector('.field-save');
    if (!saveBtn) return;
    if (input.value !== input.dataset.original) {
        saveBtn.classList.remove('hidden');
    } else {
        saveBtn.classList.add('hidden');
    }
}
```

## Authorization

Sidebar routes check that the authenticated user has access to the
app. The same authorization logic from the API handlers applies:

- **admin** — can view and edit all apps.
- **publisher** — can view and edit own apps.
- **viewer** — can view apps shared with them (read-only settings).

Read-only mode hides edit controls and action buttons in the templates
(e.g., `{{if .CanEdit}}...{{end}}`).

## Deliverables

**Backend:**

1. **Database migration** — sessions table, bundle schema additions,
   indexes.
2. **Session lifecycle** — create/end/crash tracking in the proxy.
3. **Per-session log tagging** — session ID in log lines, per-session
   storage and retrieval.
4. **Deployments API** — `GET /api/v1/deployments` with pagination
   and role-based filtering.
5. **Sessions API** — `GET /api/v1/apps/{id}/sessions` with filtering.
6. **Session logs API** — historical retrieval and live streaming per
   session.
7. **App overview data** — extend app response with session counts,
   activity metrics, last deployed timestamp.
8. **Bundle deployment tracking** — populate `deployed_by` and
   `deployed_at` on activation.

**Navigation and pages:**

9. **Left navigation** — persistent nav sidebar with Apps, Deployment
   History, API Keys, Profile links. Active page highlighting.
10. **Apps page** — app grid with search/filter (migrated from
    dashboard, credentials section removed).
11. **Deployment History page** — cross-app deployment table with
    search, pagination, role-based visibility.
12. **API Keys page** — third-party credential management (migrated
    from dashboard).
13. **Profile page** — user identity, role display, sign out.
14. **PAT management UI** — create, list, and revoke personal access
    tokens on the Profile page.

**Per-app sidebar:**

15. **htmx integration** — vendor htmx.min.js, add script tag to base
    template.
16. **Sidebar infrastructure** — sidebar container, overlay, open/close
    JS, responsive CSS transitions.
17. **Gear icon on app cards** — opens sidebar via `hx-get`.
18. **Overview tab** — status, workers, activity, and bundle summary
    as the default landing tab.
19. **Settings tab** — app metadata editing (title, description, tags),
    per-field save with inline validation, soft-delete.
20. **Runtime tab** — resource limits (memory with unit dropdown,
    step-constrained CPU), worker scaling, pre-warmed seats,
    start/stop controls.
21. **Bundles tab** — bundle list, active indicator, rollback action,
    dependency refresh (unpinned only).
22. **Collaborators tab** — access type dropdown, ACL grant/revoke
    management.
23. **Logs tab** — user-centric per-session log viewer with session
    table, user search, historical log display, and live streaming
    for active sessions.

**Cross-cutting:**

24. **Fragment routes** — `/ui/apps/{name}/sidebar` and six tab
    endpoints plus session log endpoint, with authorization.
25. **Template partials** — page templates + sidebar shell + six tab
    templates + session log template.
26. **CSS** — left nav, page layouts, sidebar, tabs, overview grid,
    field editing, ACL table, log viewer, danger zone styles.
27. **Authorization + read-only mode** — `CanEdit` flag in templates,
    role-based visibility across all pages and sidebar tabs.

## Implementation Steps

### Step 1: Database Migration

Add sessions table and bundle columns. Write up/down migration for
both SQLite and PostgreSQL.

### Step 2: Session Lifecycle Tracking

Instrument the proxy to create session records on assignment, end them
on disconnect, and crash them on worker failure. Pass session ID to
workers.

### Step 3: Per-Session Log Tagging

Modify worker log capture to tag lines with session IDs. Implement
per-session log storage and retrieval.

### Step 4: Bundle Deployment Tracking

Update `ActivateBundle()` and rollback handlers to set `deployed_by`
and `deployed_at`. Run backfill migration for existing bundles.

### Step 5: API Endpoints

Implement `GET /api/v1/deployments`, `GET /api/v1/apps/{id}/sessions`,
`GET /api/v1/apps/{id}/sessions/{sid}/logs` (historical + streaming).
Extend `GET /api/v1/apps/{id}` with session counts and activity
metrics.

### Step 6: htmx + Left Navigation + Page Restructure

Vendor htmx.min.js. Modify `base.html` to include left nav and htmx
script tag. Create `apps.html` (migrated from `dashboard.html`,
credentials removed), `deployments.html`, `api_keys.html`, and
`profile.html`. Register new page routes. Verify navigation between
pages works.

### Step 7: Deployment History Page

Build the deployment history table consuming
`GET /api/v1/deployments`. Search, pagination,
role-based visibility.

### Step 8: API Keys Page

Migrate credential management from the dashboard to its own page.
Wire htmx form submissions (replacing the existing inline JS).

### Step 9: Profile Page + PAT Management UI

Implement the profile page with identity display and sign out.
Wire PAT management UI to existing PAT API: token creation form
with one-time display, token table with revoke buttons.

### Step 10: Sidebar Infrastructure

Add sidebar container to apps page template, open/close mechanics,
responsive CSS. Wire the gear icon to open the sidebar via `hx-get`.
Verify open/close/overlay works.

### Step 11: Fragment Route Scaffolding

Register `/ui/` routes in the router. Set up template parsing for
partial templates. Implement the sidebar shell endpoint that returns
header + tabs + loads the Overview tab.

### Step 12: Overview Tab

Implement the overview tab partial. Wire to extended app API
(status, worker count, session count, activity metrics,
active bundle).

### Step 13: Settings Tab

Implement the settings tab partial. Wire per-field save with inline
validation for title, description. Add tag management. Add soft-delete
button.

### Step 14: Runtime Tab

Implement the runtime tab partial. Wire per-field save for resource
limits (number + unit dropdown for memory, step-constrained CPU input).
Add start/stop controls.

### Step 15: Bundles Tab

Implement the bundles tab partial. List bundles with active indicator.
Wire rollback button. Add refresh section for unpinned apps.

### Step 16: Collaborators Tab

Implement the collaborators tab partial. Wire access type dropdown
with auto-save. Add ACL grant list with add/remove.

### Step 17: Logs Tab

Implement the logs tab partial with user-centric session table,
user search filter, and drill-down. Wire session log viewer with
historical fetch and live streaming JS.

### Step 18: Authorization + Read-Only Mode

Add `CanEdit` flag to template data. Hide edit controls for viewers.
Verify admin/publisher/viewer access patterns across all pages and
sidebar tabs.

### Step 19: Tests

Integration tests for session lifecycle (create → end, create → crash).
API tests for deployments listing, session listing, session log
retrieval. UI handler tests for page and fragment routes. Verify
role-based filtering and authorization.
