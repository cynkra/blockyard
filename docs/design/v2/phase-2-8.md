# Phase 2-8: Web UI Expansion

Extends the v1 dashboard with a per-app settings sidebar and operational
visibility. Server-rendered HTML with htmx for dynamic fragment loading.
No JavaScript framework.

Depends on phases 2-2 (rollback, soft-delete), 2-3 (pre-warming config),
and 2-7 (refresh API). Content filtering (search + tag) is already
implemented in the dashboard — this phase does not revisit it.

## New Dependency

```go
// go.mod — no Go dependency; htmx is a client-side JS library.
```

htmx is included as a vendored static asset (`internal/ui/static/htmx.min.js`)
served alongside `style.css`. Single file, ~14 KB gzipped. No npm, no
build step.

## Dashboard Changes

### Gear Icon

Each app card gains a gear icon (⚙ or SVG) that opens the settings
sidebar. The card itself remains a link to `/app/{name}/` (the running
app). The gear icon is positioned in the card's top-right corner and
stops event propagation so clicking it does not navigate to the app.

```html
<a href="/app/{{.Name}}/" class="app-card">
    <button class="app-card-gear"
            hx-get="/ui/apps/{{.Name}}/settings"
            hx-target="#sidebar"
            hx-swap="innerHTML"
            aria-label="Settings for {{.Name}}"
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

The sidebar scrolls independently of the dashboard.

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

Four tabs below the header. Each tab fetches its content via htmx on
click. The first tab (Settings) loads automatically when the sidebar
opens.

```
┌──────────┬─────────┬─────────┬──────┐
│ Settings │ Runtime │ Bundles │ Logs │
└──────────┴─────────┴─────────┴──────┘
```

```html
<nav class="sidebar-tabs">
    <button class="tab active"
            hx-get="/ui/apps/{{.App.Name}}/tab/settings"
            hx-target="#tab-content">Settings</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/runtime"
            hx-target="#tab-content">Runtime</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/bundles"
            hx-target="#tab-content">Bundles</button>
    <button class="tab"
            hx-get="/ui/apps/{{.App.Name}}/tab/logs"
            hx-target="#tab-content">Logs</button>
</nav>
<div id="tab-content"></div>
```

Active tab state is managed with `hx-on::after-request` toggling an
`active` class. No client-side router.

## Tab Content

### Settings Tab

**Endpoint:** `GET /ui/apps/{name}/tab/settings`

Editable fields submitted individually or as a group via
`hx-patch="/api/v1/apps/{id}"`.

| Field | Input Type | API Field |
|-------|-----------|-----------|
| Title | text | `title` |
| Description | textarea | `description` |
| Access type | select (acl / logged_in / public) | `access_type` |
| Tags | tag chips with add/remove | `POST/DELETE /api/v1/apps/{id}/tags` |

**ACL management** (shown when access_type = "acl"):

A list of current grants loaded from `GET /api/v1/apps/{id}/access`.
Each grant has a remove button (`hx-delete`). An inline form adds new
grants (`hx-post`).

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
| Memory limit | text (e.g. "512m") | `memory_limit` |
| CPU limit | number (e.g. 1.0) | `cpu_limit` |
| Max workers | number | `max_workers_per_app` |
| Max sessions per worker | number | `max_sessions_per_worker` |
| Pre-warmed seats | number | `pre_warmed_seats` |

All fields submitted via `hx-patch="/api/v1/apps/{id}"`. A success
flash confirms the save.

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

### Logs Tab

**Endpoint:** `GET /ui/apps/{name}/tab/logs`

The logs tab uses a small vanilla JS snippet (not htmx) because it
needs streaming via `fetch()` + `ReadableStream`. htmx's SSE extension
could work but adds complexity for no benefit here.

```html
<div class="log-viewer">
    <div class="log-controls">
        <button id="log-toggle" onclick="toggleLogs()">Start streaming</button>
        <button onclick="clearLogs()">Clear</button>
    </div>
    <pre id="log-output" class="log-output"></pre>
</div>

<script>
let logController = null;

function toggleLogs() {
    if (logController) { stopLogs(); return; }
    const btn = document.getElementById('log-toggle');
    const output = document.getElementById('log-output');
    logController = new AbortController();
    btn.textContent = 'Stop streaming';

    fetch('/api/v1/apps/{{.App.ID}}/logs', {
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
    document.getElementById('log-toggle').textContent = 'Start streaming';
}

function clearLogs() {
    document.getElementById('log-output').textContent = '';
}
</script>
```

Historical logs are fetched first (non-streaming GET), then the user
can toggle live streaming.

## UI Routes

New routes registered under soft auth (session cookie, same as `/`):

| Method | Path | Returns | Purpose |
|--------|------|---------|---------|
| GET | `/ui/apps/{name}/settings` | HTML fragment | Sidebar shell (header + tabs + initial Settings tab) |
| GET | `/ui/apps/{name}/tab/settings` | HTML fragment | Settings tab content |
| GET | `/ui/apps/{name}/tab/runtime` | HTML fragment | Runtime tab content |
| GET | `/ui/apps/{name}/tab/bundles` | HTML fragment | Bundles tab content |
| GET | `/ui/apps/{name}/tab/logs` | HTML fragment | Logs tab content |

All routes require authentication (user must have access to the app).
Fragments are rendered via Go templates — same `html/template` engine,
just returning partials instead of full pages.

The `/ui/` prefix distinguishes fragment routes from the REST API
(`/api/v1/`) and the app proxy (`/app/`). Fragment routes are
internal to the UI and not part of the public API contract.

## Templates

New template files in `internal/ui/templates/`:

```
templates/
├── base.html              # (existing)
├── landing.html           # (existing)
├── dashboard.html         # (modified — add sidebar container, gear icon, htmx script tag)
├── sidebar.html           # sidebar shell: header, tabs, tab-content div
├── tab_settings.html      # Settings tab partial
├── tab_runtime.html       # Runtime tab partial
├── tab_bundles.html       # Bundles tab partial
└── tab_logs.html          # Logs tab partial
```

Tab templates are self-contained partials — no `{{template "base" .}}`
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

- **`.sidebar`** — fixed right, full height, 28rem wide, white background,
  box shadow, transform/transition for slide-in, overflow-y auto.
- **`.sidebar.open`** — `transform: translateX(0)` (default is
  `translateX(100%)`).
- **`.sidebar-overlay`** — fixed full-screen, semi-transparent backdrop,
  hidden by default, shown when sidebar is open.
- **`.sidebar-header`** — flex row, app name + external link + close button.
- **`.sidebar-tabs`** — flex row of tab buttons, bottom border.
- **`.tab.active`** — bold, border-bottom highlight.
- **`.log-viewer`** — flex column, `.log-output` has monospace font, dark
  background, max-height with overflow scroll.
- **`.danger-zone`** — top border, red-tinted button.
- **`.btn-danger`** — red background, white text.

## Sidebar JS

Minimal vanilla JS for sidebar open/close (not htmx-managed because
the overlay click and close button are simpler as direct DOM
manipulation):

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

## Authorization

Sidebar routes check that the authenticated user has access to the
app. The same authorization logic from the API handlers applies:

- **admin** — can view and edit all apps.
- **publisher** — can view and edit own apps.
- **viewer** — can view apps shared with them (read-only settings).

Read-only mode hides edit controls and action buttons in the templates
(e.g., `{{if .CanEdit}}...{{end}}`).

## Deliverables

1. **htmx integration** — vendor htmx.min.js, add script tag to base
   template.
2. **Sidebar infrastructure** — sidebar container, overlay, open/close
   JS, CSS transitions.
3. **Gear icon on app cards** — opens sidebar via `hx-get`.
4. **Settings tab** — app metadata editing, access type, ACL
   management, tag management, soft-delete.
5. **Runtime tab** — resource limits, worker scaling, pre-warmed seats,
   start/stop controls.
6. **Bundles tab** — bundle list, active indicator, rollback action,
   dependency refresh (unpinned only).
7. **Logs tab** — streaming log viewer with start/stop/clear controls.
8. **Fragment routes** — `/ui/apps/{name}/settings` and four tab
   endpoints, with authorization.
9. **Template partials** — sidebar shell + four tab templates.
10. **CSS** — sidebar, tabs, log viewer, danger zone styles.

## Implementation Steps

### Step 1: htmx + Sidebar Infrastructure

Add htmx static asset, sidebar container to dashboard template,
open/close mechanics, CSS. Verify the gear icon opens an empty sidebar
and the close button / overlay dismiss works.

### Step 2: Fragment Route Scaffolding

Register `/ui/` routes in the router. Set up template parsing for
partial templates. Implement the sidebar shell endpoint that returns
header + tabs + loads the first tab.

### Step 3: Settings Tab

Implement the settings tab partial. Wire `hx-patch` for metadata
fields, access type dropdown. Add ACL list with grant/revoke. Add tag
management. Add soft-delete button.

### Step 4: Runtime Tab

Implement the runtime tab partial. Wire `hx-patch` for resource limits
and scaling fields. Add start/stop controls.

### Step 5: Bundles Tab

Implement the bundles tab partial. List bundles with active indicator.
Wire rollback button. Add refresh section for unpinned apps.

### Step 6: Logs Tab

Implement the logs tab partial with the streaming JS. Wire to the
existing logs API endpoint.

### Step 7: Authorization + Read-Only Mode

Add `CanEdit` flag to template data. Hide edit controls for viewers.
Verify admin/publisher/viewer access patterns.
