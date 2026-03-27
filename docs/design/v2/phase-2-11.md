# Phase 2-11: Per-App Management Sidebar

Adds the per-app management sidebar to the multi-page UI established
in phase 2-10. The sidebar provides six tabs (Overview, Settings,
Runtime, Bundles, Collaborators, Logs) for managing individual apps
without leaving the dashboard. All content is loaded via htmx fragment
routes.

Depends on phase 2-8 (backend prerequisites: runtime API, session
metrics, enable/disable, hard delete, form-encoded PATCH,
`resolveApp()` extraction, `ListAppAccessWithNames()`) and phase 2-10
(multi-page navigation, htmx, sidebar shell, template infrastructure).

## Fragment Route Infrastructure

### Template Parsing

Fragment templates use the `fragments` map established in phase 2-10.
Fragment handlers call `ui.fragments[name].Execute(w, data)` directly --
no `ExecuteTemplate` with a named block.

### App Name -> ID Resolution

Fragment routes use `{name}` in URLs for user-friendly paths. The
handler resolves the name to an app ID using the shared `resolveApp()`
helper (extracted to a shared location in phase 2-8). The helper tries
UUID first, then name lookup.

With soft-delete, the name lookup uses the partial unique index on
live apps, so a soft-deleted "foo" does not conflict with a new "foo".

### Auth Middleware

Fragment routes use the same `auth.AppAuthMiddleware` as the UI pages
(soft auth), but each handler explicitly checks the caller's
`AppRelation` via `resolveAppRelation` (shared, from phase 2-8). If
the caller is nil (unauthenticated) or has insufficient access, the
handler returns 404 (consistent with the API convention of not leaking
app existence).

For htmx requests, 404 results in an empty swap -- the sidebar shows
nothing. Since the gear icon is already hidden for unauthorized users,
this is a rare edge case (direct URL access or revoked access).

## RBAC for Tabs

| UI Element | Visible to |
|------------|-----------|
| Gear icon on app card | collaborator+ |
| Overview tab | collaborator+ |
| Settings tab | collaborator+ (editable for `CanUpdateConfig`) |
| Runtime tab | collaborator+ |
| Bundles tab | collaborator+ |
| Collaborators tab | **owner+ only** (`CanManageACL`) |
| Logs tab | collaborator+ |

The Collaborators tab is further restricted to owners and admins
because it exposes user identities and session activity.

### Template-Level Enforcement

The gear icon is conditionally rendered:

```html
{{if .CanManage}}
<button class="app-card-gear"
        hx-get="/ui/apps/{{.Name}}/sidebar"
        hx-target="#sidebar"
        hx-swap="innerHTML"
        aria-label="Manage {{.Name}}"
        onclick="event.preventDefault()">&#9881;</button>
{{end}}
```

The `CanManage` flag is true when the caller's `AppRelation >=
ContentCollaborator`.

The Collaborators tab button is conditionally rendered:

```html
{{if .CanManageACL}}
<button class="tab"
        hx-get="/ui/apps/{{.App.Name}}/tab/collaborators"
        hx-target="#tab-content">Collaborators</button>
{{end}}
```

### Server-Level Enforcement

All fragment routes enforce RBAC at the handler level, not just the
template level. A direct request to a tab endpoint from a viewer
returns 404. The Collaborators tab endpoint checks `CanManageACL()`.

## Sidebar Structure

### Header

App name as the title. An external-link icon opens `/app/{name}/`
in a new tab. A close button dismisses the sidebar.

```html
<div class="sidebar-header">
    <h2>{{.App.Name}}</h2>
    <a href="/app/{{.App.Name}}/" target="_blank" title="Open app">&#8599;</a>
    <button onclick="closeSidebar()" aria-label="Close">&#10005;</button>
</div>
```

### Tabs

```html
<nav class="sidebar-tabs">
    <button class="tab active"
            hx-get="/ui/apps/{{.App.Name}}/tab/overview"
            hx-target="#tab-content"
            hx-on::after-request="setActiveTab(this)">Overview</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/settings"
            hx-target="#tab-content"
            hx-on::after-request="setActiveTab(this)">Settings</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/runtime"
            hx-target="#tab-content"
            hx-on::after-request="setActiveTab(this)">Runtime</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/bundles"
            hx-target="#tab-content"
            hx-on::after-request="setActiveTab(this)">Bundles</button>
    {{if .CanManageACL}}
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/collaborators"
            hx-target="#tab-content"
            hx-on::after-request="setActiveTab(this)">Collaborators</button>
    {{end}}
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/logs"
            hx-target="#tab-content"
            hx-on::after-request="setActiveTab(this)">Logs</button>
</nav>
<div id="tab-content"></div>
```

Active tab state is managed with `hx-on::after-request` toggling an
`active` class via `setActiveTab()`. No client-side router.

**Overview auto-load:** The sidebar shell endpoint (`GET /ui/apps/{name}/sidebar`)
returns the header, tabs, and the Overview tab content pre-rendered
inline in `#tab-content`. This avoids a second request on sidebar open.

## Field Editing UX

All editable fields across tabs follow a consistent pattern:

**Text inputs and textareas** -- fields are directly editable. A small
save button appears next to the field when the current value differs
from the saved value. Clicking save submits via `hx-patch` and shows
inline feedback.

**Dropdowns / selects** -- save automatically on `change` via
`hx-patch`, with the same inline feedback.

**Inline validation** -- on save, the server returns either:
- **200** with a success indicator fragment.
- **422** with an error fragment that swaps into a `<span class="field-error">`
  below the field.

The `hx-patch` attributes target `PATCH /api/v1/apps/{id}` (which
accepts form-encoded bodies and returns HTML fragments for htmx
requests, added in phase 2-8).

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
                hx-swap="innerHTML">&#10003;</button>
        <span class="field-feedback"></span>
    </div>
</div>
```

## Tab Content

### Overview Tab

**Endpoint:** `GET /ui/apps/{name}/tab/overview`

The default tab shown when the sidebar opens. Provides at-a-glance
status for the app.

| Section | Content | Data source |
|---------|---------|-------------|
| Status | Running / stopped / failed, last deployed timestamp | App status + `bundles.deployed_at` |
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

Overview cards link to their respective tabs where relevant.

### Settings Tab

**Endpoint:** `GET /ui/apps/{name}/tab/settings`

App metadata and resource configuration with per-field save.

**Metadata fields:**

| Field | Input Type | API Field |
|-------|-----------|-----------|
| Title | text | `title` |
| Description | textarea | `description` |
| Tags | tag chips with add/remove | `POST/DELETE /api/v1/apps/{id}/tags` |

**Resource configuration fields:**

| Field | Input Type | API Field |
|-------|-----------|-----------|
| Memory limit | number + unit dropdown (MB / GB) | `memory_limit` |
| CPU limit | number (`step="0.25"`) | `cpu_limit` |
| Max workers | number | `max_workers_per_app` |
| Max sessions per worker | number | `max_sessions_per_worker` |
| Pre-warmed seats | number | `pre_warmed_seats` |

**Enable / Disable toggle:**

Enable/disable replaces the old start/stop controls. `enabled` is
persistent (survives restarts). Disabling triggers active worker drain;
enabling allows cold-start and pre-warming to resume.

```html
<div class="app-controls"
     hx-trigger="appEnabled from:body, appDisabled from:body"
     hx-get="/ui/apps/{{.App.Name}}/tab/settings"
     hx-target="#tab-content">
    {{if eq .Status "stopping"}}
    <span class="status-badge status-stopping">Disabling...</span>
    {{else if .App.Enabled}}
    <button class="btn" hx-post="/api/v1/apps/{{.App.ID}}/disable"
            hx-swap="none">Disable</button>
    <span class="status-badge status-{{.Status}}">{{.Status}}</span>
    {{else}}
    <button class="btn btn-primary" hx-post="/api/v1/apps/{{.App.ID}}/enable"
            hx-swap="none">Enable</button>
    <span class="status-badge status-disabled">Disabled</span>
    {{end}}
</div>
```

The API endpoints return JSON as usual. When the `HX-Request` header
is present, they add an `HX-Trigger` response header (`appEnabled` or
`appDisabled`). The `hx-trigger` listener on the controls container
re-fetches the settings tab to reflect the new state.

**Soft-delete** at the bottom:

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

Live operational data. Shows active workers with container stats
(CPU, memory) and session-to-user mapping. Data comes from
`GET /api/v1/apps/{id}/runtime` (added in phase 2-8).

```html
<div class="runtime-view">
    {{if .Workers}}
    <table class="data-table">
        <thead>
            <tr>
                <th>Worker</th>
                <th>Status</th>
                <th>CPU</th>
                <th>Memory</th>
                <th>Sessions</th>
            </tr>
        </thead>
        <tbody>
        {{range .Workers}}
            <tr>
                <td class="monospace">{{.ID | truncate}}</td>
                <td><span class="status-badge status-{{.Status}}">{{.Status}}</span></td>
                <td>{{.Stats.CPUPercent | printf "%.1f"}}%</td>
                <td>{{.Stats.MemoryUsageBytes | humanBytes}} / {{.Stats.MemoryLimitBytes | humanBytes}}</td>
                <td>{{len .Sessions}}</td>
            </tr>
            {{if .Sessions}}
            <tr class="worker-sessions-row">
                <td colspan="5">
                    <div class="worker-sessions">
                        {{range .Sessions}}
                        <span class="session-chip">
                            {{.UserDisplayName}} ({{.StartedAt | timeAgo}})
                        </span>
                        {{end}}
                    </div>
                </td>
            </tr>
            {{end}}
        {{end}}
        </tbody>
    </table>
    {{else}}
    <p class="empty-state">No active workers.</p>
    {{end}}

    <div class="runtime-summary">
        <p>{{.ActiveSessions}} active sessions, {{.UniqueVisitors}} unique visitors</p>
        <p>{{.TotalViews}} total views, {{.RecentViews}} last 7 days</p>
    </div>
</div>
```

### Bundles Tab

**Endpoint:** `GET /ui/apps/{name}/tab/bundles`

Lists bundles from `GET /api/v1/apps/{id}/bundles`, most recent first.
Each bundle shows ID (truncated), created timestamp, status, and
active indicator.

Non-active ready bundles have a rollback button:

```html
<button class="btn btn-sm"
        hx-post="/api/v1/apps/{{$.App.ID}}/rollback"
        hx-vals='{"bundle_id": "{{.ID}}"}'
        hx-confirm="Roll back to bundle {{.ID | truncate}}?"
        hx-swap="none">Rollback</button>
```

The rollback API returns JSON with `HX-Trigger: bundleRolledBack`.
The bundles tab container listens and re-fetches:

```html
<div id="tab-content"
     hx-trigger="bundleRolledBack from:body"
     hx-get="/ui/apps/{{.App.Name}}/tab/bundles">
```

**Dependency refresh** (shown only for unpinned deployments, using
the `pinned` field on the active bundle from phase 2-8):

```html
{{if not .ActiveBundle.Pinned}}
<div class="refresh-section">
    <button class="btn"
            hx-post="/api/v1/apps/{{.App.ID}}/refresh"
            hx-swap="none">Refresh dependencies</button>
    <div id="refresh-status"
         hx-trigger="refreshStarted from:body"
         hx-get="/ui/apps/{{.App.Name}}/tab/bundles"
         hx-target="#tab-content"></div>
</div>
{{end}}
```

### Collaborators Tab

**Endpoint:** `GET /ui/apps/{name}/tab/collaborators`

**Visible to owner+ only.** Manages app visibility and per-user access
control.

The collaborators list uses `ListAppAccessWithNames()` (added in phase
2-8) to join `app_access` with the `users` table for human-readable
display names. If a granted principal has never logged in, the raw
principal (OIDC subject) is shown instead.

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

```html
{{if eq .App.AccessType "acl"}}
<div class="acl-section">
    <h3>Access grants</h3>
    <table class="acl-table">
        <thead><tr><th>User</th><th>Role</th><th></th></tr></thead>
        <tbody>
        {{range .Grants}}
        <tr>
            <td>{{if .UserName}}{{.UserName}}{{else}}{{.Principal}}{{end}}</td>
            <td>{{.Role}}</td>
            <td><button class="btn btn-sm btn-danger"
                        hx-delete="/api/v1/apps/{{$.App.ID}}/access/user/{{.Principal}}"
                        hx-target="closest tr"
                        hx-swap="outerHTML swap:0.2s">Remove</button></td>
        </tr>
        {{end}}
        </tbody>
    </table>
    <form class="acl-add-form"
          hx-post="/api/v1/apps/{{.App.ID}}/access"
          hx-swap="none">
        <input type="text" name="user" placeholder="Username or email" required>
        <select name="role">
            <option value="viewer">Viewer</option>
            <option value="collaborator">Collaborator</option>
        </select>
        <button type="submit" class="btn btn-sm">Add</button>
    </form>
</div>
{{end}}
```

The grant API returns JSON with `HX-Trigger: accessGranted`. The ACL
section listens and re-fetches the collaborators tab to show the new
row with a resolved display name. The delete endpoint returns 200
with an empty body for htmx requests (per phase 2-8's htmx-aware
response handling), so `hx-swap="outerHTML"` removes the row directly.

### Logs Tab

**Endpoint:** `GET /ui/apps/{name}/tab/logs`

Logs are scoped to **workers**, not sessions. The tab shows active and
recent workers for the app, each with a link to view that worker's log
stream. This matches the underlying architecture: Docker containers
produce a single stdout/stderr stream per worker, and
`logstore.Subscribe(workerID)` is the read path.

When `max_sessions_per_worker = 1`, worker logs are effectively
per-session logs. For shared workers, log output from multiple
sessions is interleaved -- the same trade-off Posit Connect makes.

```html
<div class="log-viewer">
    <div class="worker-list">
        <h3>Workers</h3>
        <table class="session-table">
            <thead>
                <tr><th>Worker</th><th>Status</th><th>Sessions</th><th>Since</th></tr>
            </thead>
            <tbody>
            {{range .Workers}}
            <tr class="worker-entry {{if eq .Status "active"}}worker-active{{end}}"
                hx-get="/ui/apps/{{$.App.Name}}/tab/logs/worker/{{.ID}}"
                hx-target="#log-content">
                <td class="monospace">{{.ID | truncate}}</td>
                <td><span class="status-badge status-{{.Status}}">{{.Status}}</span></td>
                <td>{{.SessionCount}}</td>
                <td>{{.StartedAt | timeAgo}}</td>
            </tr>
            {{end}}
            </tbody>
        </table>
    </div>
    <div id="log-content" class="log-content">
        <p class="log-placeholder">Select a worker to view logs.</p>
    </div>
</div>
```

**Worker log view fragment** (`GET /ui/apps/{name}/tab/logs/worker/{wid}`):

```html
<div class="log-worker-view">
    <div class="log-controls">
        <span class="log-worker-label">Worker {{.WorkerID | truncate}}</span>
        {{if .Active}}
        <button id="log-toggle" onclick="toggleLogs('{{.WorkerID}}')">
            Start streaming
        </button>
        {{end}}
        <button onclick="clearLogs()">Clear</button>
    </div>
    <pre id="log-output" class="log-output">{{.HistoricalLogs}}</pre>
</div>

<script>
let logController = null;

function toggleLogs(workerId) {
    if (logController) { stopLogs(); return; }
    const btn = document.getElementById('log-toggle');
    const output = document.getElementById('log-output');
    logController = new AbortController();
    btn.textContent = 'Stop streaming';

    fetch('/api/v1/apps/{{.App.ID}}/logs?worker_id=' + workerId + '&stream=true', {
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

## htmx Error Handling

When an htmx request fails, the following behavior applies:

**Network errors / 5xx:** htmx fires `htmx:responseError`. A global
handler shows a brief error message in the target:

```js
document.body.addEventListener('htmx:responseError', function(e) {
    var target = e.detail.target;
    if (target) {
        target.innerHTML = '<p class="error-message">Something went wrong. Try again.</p>';
    }
});
```

**401 (session expired):** When auth middleware returns 401 for an
htmx request (detected via `HX-Request` header), it sets
`HX-Redirect: /login` in the response header. htmx automatically
follows this and redirects the full page to login.

**404 (app deleted, access revoked):** Returns empty content. The
tab area shows nothing.

**422 (validation error):** Returns an error fragment that swaps
into the `field-error` span (see Field Editing UX).

## Fragment Routes

| Method | Path | Returns | Auth |
|--------|------|---------|------|
| GET | `/ui/apps/{name}/sidebar` | HTML fragment | collaborator+ |
| GET | `/ui/apps/{name}/tab/overview` | HTML fragment | collaborator+ |
| GET | `/ui/apps/{name}/tab/settings` | HTML fragment | collaborator+ |
| GET | `/ui/apps/{name}/tab/runtime` | HTML fragment | collaborator+ |
| GET | `/ui/apps/{name}/tab/bundles` | HTML fragment | collaborator+ |
| GET | `/ui/apps/{name}/tab/collaborators` | HTML fragment | **owner+** |
| GET | `/ui/apps/{name}/tab/logs` | HTML fragment | collaborator+ |
| GET | `/ui/apps/{name}/tab/logs/worker/{wid}` | HTML fragment | collaborator+ |

## Templates

Fragment templates in `internal/ui/templates/`:

```
templates/
├── sidebar.html           # sidebar shell: header, tabs, tab-content div
├── tab_overview.html      # Overview tab partial
├── tab_settings.html      # Settings tab partial
├── tab_runtime.html       # Runtime tab partial (worker list + stats)
├── tab_bundles.html       # Bundles tab partial
├── tab_collaborators.html # Collaborators tab partial
├── tab_logs.html          # Logs tab partial (worker list)
├── tab_logs_worker.html   # Log viewer for a specific worker
└── error_fragment.html    # Generic error message fragment
```

All parsed standalone (no `base.html` wrapper) and stored in the
`ui.fragments` map established in phase 2-10.

## CSS Additions

Sidebar-specific styles (added to existing `style.css`):

- **`.sidebar-header`** -- flex row, app name + external link + close button.
- **`.sidebar-tabs`** -- flex row of tab buttons, bottom border.
- **`.tab.active`** -- bold, border-bottom highlight.
- **`.overview-grid`** -- 2x2 grid of status cards.
- **`.overview-card`** -- bordered card with heading, stat, and meta text.
- **`.field-group`** -- label + field-row + optional error container.
- **`.field-row`** -- flex row holding input + save button + feedback span.
- **`.field-save`** -- small inline save button, hidden by default.
- **`.field-feedback`** -- inline success/error indicator.
- **`.field-error`** -- red text below field for validation messages.
- **`.field-invalid`** -- red border on input with validation error.
- **`.acl-table`** -- simple table for access grants.
- **`.acl-add-form`** -- inline form row for adding grants.
- **`.runtime-view`** -- container for runtime tab content.
- **`.runtime-summary`** -- summary stats below worker table.
- **`.worker-sessions`** -- inline session chips within worker row.
- **`.session-chip`** -- small pill showing user + duration.
- **`.log-viewer`** -- flex row: worker list on left, log content on right.
- **`.worker-list`** -- scrollable list of workers.
- **`.worker-entry`** -- clickable worker item.
- **`.worker-active`** -- highlighted style for active workers.
- **`.log-content`** -- flex column, `.log-output` has monospace font, dark
  background, max-height with overflow scroll.
- **`.danger-zone`** -- top border, red-tinted button.
- **`.status-stopping`** -- amber/yellow badge for draining state.
- **`.status-disabled`** -- grey badge for disabled apps.
- **`.error-message`** -- red text for htmx error display.

## Sidebar JS Additions

Per-field save visibility and htmx error handling (added to the
sidebar JS from phase 2-10):

```js
// Toggle active tab highlight.
function setActiveTab(btn) {
    btn.closest('.sidebar-tabs').querySelectorAll('.tab').forEach(
        t => t.classList.remove('active'));
    btn.classList.add('active');
}

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

// Global htmx error handler.
document.body.addEventListener('htmx:responseError', function(e) {
    var target = e.detail.target;
    if (target) {
        target.innerHTML = '<p class="error-message">Something went wrong. Try again.</p>';
    }
});
```

## Deliverables

1. **Fragment route scaffolding** -- register `/ui/` routes, set up
   fragment template parsing in `ui.fragments` map.
2. **Sidebar infrastructure** -- wire gear icon to fragment route,
   sidebar shell endpoint (header + tabs + initial Overview load).
3. **Overview tab** -- status, workers, activity, and bundle summary.
4. **Settings tab** -- metadata editing (title, description, tags),
   resource configuration (memory, CPU, workers, sessions, pre-warming),
   per-field save with inline validation, enable/disable, soft-delete.
5. **Runtime tab** -- live worker table with CPU/memory stats,
   session-to-user mapping, activity summary.
6. **Bundles tab** -- bundle list, active indicator, rollback action,
   dependency refresh (unpinned only, using `pinned` field from
   phase 2-8).
7. **Collaborators tab** -- access type dropdown, ACL grant/revoke
   management with display names (using `ListAppAccessWithNames()`
   from phase 2-8). Owner+ only.
8. **Logs tab** -- worker-scoped log viewer with worker list,
   historical log display, and live streaming for active workers.
9. **htmx error handling** -- global error handler, 401 redirect via
   `HX-Redirect`, 422 validation fragments, error fragment template.
10. **CSS** -- sidebar tabs, overview grid, field editing, ACL table,
    runtime view, log viewer, danger zone, error message styles.
11. **Tests** -- fragment route tests, RBAC tests (viewer blocked,
    collaborator allowed, owner-only for collaborators tab), sidebar
    open/close behavior.

## Implementation Steps

### Step 1: Fragment Route Scaffolding

Register `/ui/apps/{name}/` routes with auth middleware. Add fragment
templates to the `ui.fragments` map. Implement sidebar shell endpoint
returning header + tabs.

### Step 2: Overview Tab

Implement overview tab partial. Wire to app data + session metrics
from `GET /api/v1/apps/{id}/runtime`.

### Step 3: Settings Tab

Implement settings tab partial. Wire per-field save for title,
description, resource config (using form-encoded PATCH from phase 2-8).
Add tag management. Add enable/disable controls. Add soft-delete button.

### Step 4: Runtime Tab

Implement runtime tab partial. Wire to
`GET /api/v1/apps/{id}/runtime` for live worker data with CPU/memory
stats and session-to-user mapping.

### Step 5: Bundles Tab

Implement bundles tab partial. List bundles with active indicator.
Wire rollback button. Add refresh section for unpinned apps (using
`pinned` field).

### Step 6: Collaborators Tab

Implement collaborators tab partial (owner+ only). Wire access type
dropdown with auto-save. Add ACL grant list with display names (using
`ListAppAccessWithNames()`), add/remove.

### Step 7: Logs Tab

Implement logs tab partial with worker list and drill-down. Wire
worker log viewer with historical fetch and live streaming JS.

### Step 8: htmx Error Handling + Polish

Add global htmx error handler. Handle 401 via `HX-Redirect`. Add
error fragment template. Verify all htmx interactions degrade
gracefully on errors.

### Step 9: Tests

Fragment route tests for all tabs. RBAC tests verifying viewer cannot
access sidebar or tabs. Owner-only test for collaborators tab.
Integration tests for per-field save, rollback, enable/disable via htmx.
