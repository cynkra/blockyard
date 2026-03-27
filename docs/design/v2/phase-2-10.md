# Phase 2-10: Multi-Page Navigation + htmx

Converts the single-page dashboard into a multi-page layout with
persistent left navigation, htmx integration, and four distinct pages:
Apps, Deployment History, API Keys, and Profile (with PAT management).

Depends on phase 2-8 (backend prerequisites) for:
- `ListCatalogWithRelation()` DB method (Apps page -- search, tags,
  pagination, per-app `relation` for gear icon conditionality).
- `ListDeployments()` DB method and bundle schema additions
  (`deployed_by`, `deployed_at`) for the Deployment History page.
- Form-encoded body support on `POST /api/v1/users/me/credentials/{service}`
  (API Keys page htmx form).
- htmx-aware DELETE responses (200 instead of 204) on
  `DELETE /api/v1/users/me/tokens/{id}` (PAT revoke row removal).

UI page handlers call DB methods and server state directly (same
process) rather than making HTTP calls to the API endpoints. The API
endpoints serve the CLI and external clients.

## htmx Integration

htmx is included as a vendored static asset (`internal/ui/static/htmx.min.js`)
served alongside `style.css`. Single file, ~14 KB gzipped. No npm, no
build step.

```
static/
├── style.css        # (existing -- extended with nav, page, sidebar shell styles)
└── htmx.min.js      # vendored htmx 2.x (~14 KB gzipped)
```

Loaded via a `<script>` tag in `base.html`:

```html
<script src="/static/htmx.min.js"></script>
```

## Template Infrastructure

This phase introduces the two-map template architecture that both this
phase and phase 2-11 (sidebar) use:

```go
type UI struct {
    pages     map[string]*template.Template  // parsed with base.html
    fragments map[string]*template.Template  // parsed standalone
    static    http.Handler
}
```

Page templates (`apps.html`, `deployments.html`, `api_keys.html`,
`profile.html`) are parsed paired with `base.html` and stored in
`pages`. Fragment templates (`pat_created.html` in this phase, many
more in phase 2-11) are parsed standalone and stored in `fragments`.

Page handlers call `ui.pages[name].ExecuteTemplate(w, "base", data)`.
Fragment handlers call `ui.fragments[name].Execute(w, data)`.

The template function map extends the existing `deref` with:
- `timeAgo` -- formats a timestamp as relative time (e.g., "2 hours ago").
- `truncate` -- truncates a string to 8 characters with ellipsis (for bundle IDs).
- `add` / `subtract` -- integer arithmetic for pagination links.

### Shared Layout Data

Every page template receives a common set of layout fields via an
embedded struct:

```go
type layoutData struct {
    ActivePage     string // "apps", "deployments", "api-keys", "profile"; empty for landing
    OpenbaoEnabled bool   // controls API Keys nav link visibility
    Version        string // build-time version string (from srv.Version)
}
```

Each page-specific data struct (`appsData`, `deploymentsData`, etc.)
embeds `layoutData`. The `Version` field is populated from
`srv.Version` (set via `-ldflags` at build time, same value reported
by `/healthz`).

## Navigation Restructure

The current single-page dashboard (app grid + user info + API keys) is
split into four pages with a persistent left navigation sidebar.

```
+---------------------+----------------------------------+
| blockyard           |                                  |
|                     |  [page content]                  |
| * Apps              |                                  |
| o Deployment History|                                  |
| o API Keys          |                                  |
| o Profile           |                                  |
|                     |                                  |
|                     |                                  |
| v0.x.x              |                                  |
+---------------------+----------------------------------+
```

The left nav is a fixed-width column (~180px) present on all
authenticated pages. It shows the blockyard logo/name at the top,
navigation links with active state highlighting, and the version
number at the bottom.

**Landing page exclusion:** `base.html` conditionally renders the nav
and page-layout wrapper only when `ActivePage` is non-empty. The
landing page passes an empty `ActivePage`, so it retains its existing
full-width centered layout with no nav.

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

## Apps Page (`/`)

The app grid with search and tag filter. This is the existing dashboard
content minus the user identity header and API keys section. The gear
icon on each card opens the per-app management sidebar (phase 2-11).
The gear icon is only rendered for users with collaborator+ access
to the app (using the per-app `relation` field from
`ListCatalogWithRelation()`, added in phase 2-8). The handler maps
`relation` to a `CanManage` boolean (true for collaborator, owner,
admin). The `status` field (running/stopped) is computed from
`srv.Workers` per app, same as the existing dashboard.

```html
<a href="/app/{{.Name}}/" class="app-card">
    {{if .CanManage}}
    <button class="app-card-gear"
            hx-get="/ui/apps/{{.Name}}/sidebar"
            hx-target="#sidebar"
            hx-swap="innerHTML"
            aria-label="Manage {{.Name}}"
            onclick="event.stopPropagation()">&#9881;</button>
    {{end}}
    <!-- existing card content -->
</a>
```

The sidebar container is added to the page but is non-functional until
phase 2-11 registers the fragment routes. A placeholder route
(`GET /ui/apps/{name}/sidebar`) is registered in this phase returning
an empty `200` response so that gear icon clicks don't produce console
errors; phase 2-11 replaces it with the real sidebar content.

```html
<aside id="sidebar" class="sidebar"></aside>
<div id="sidebar-overlay" class="sidebar-overlay" hx-on:click="closeSidebar()"></div>
```

## Deployment History Page (`/deployments`)

A cross-app timeline of all deployments the user has visibility into
(collaborator+ on the respective apps).

| Column | Content |
|--------|---------|
| App | App name (links to app card sidebar in phase 2-11) |
| Bundle | Bundle ID (truncated) |
| Deployed by | User who triggered the deployment |
| Deployed | Relative timestamp (e.g., "2 hours ago") |
| Status | Badge: ready / building / failed |

```html
<div class="page-header">
    <h1>Deployment History</h1>
    <form method="GET" action="/deployments" class="search-form">
        <input type="search" name="search" placeholder="Search by app name..."
               class="search-input" value="{{.Search}}">
    </form>
</div>
{{if .Deployments}}
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
            <td>{{.DeployedByName}}</td>
            <td>{{.DeployedAt | timeAgo}}</td>
            <td><span class="status-badge status-{{.Status}}">{{.Status}}</span></td>
        </tr>
    {{end}}
    </tbody>
</table>
{{template "pagination" .Pagination}}
{{else}}
<p class="empty-state">No deployments found.</p>
{{end}}
```

Sorted by deployment time, most recent first. Paginated. The
`.Pagination` struct carries `Page`, `TotalPages`, and `Search`
(to preserve the search term across page links). The pagination
partial renders prev/next links as plain `<a>` tags with query
parameters (e.g., `/deployments?page=2&search=...`).

## API Keys Page (`/api-keys`)

Third-party credential management, moved from the dashboard. Manages
credentials for external services (e.g., OpenAI, Anthropic) stored in
Vault/Openbao.

Only rendered if Openbao is configured in the server config. If not
configured, the route redirects to `/`. Each service shows its label,
current status (configured / not set), and an input to set or update
the key.

**Redirect target change:** The credential save form currently
redirects to `/?credential_saved=1`. The htmx approach replaces the
full-page redirect with a fire-and-reload pattern: `hx-swap="none"`
suppresses response processing, and `hx-on::after-request` reloads
the page on success. The credential enrollment endpoint gains
form-encoded body support in phase 2-8:

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
          hx-swap="none"
          hx-on::after-request="if(event.detail.successful) location.reload()">
        <input type="password" name="api_key" placeholder="Enter API key" required>
        <button type="submit" class="btn btn-sm">Save</button>
    </form>
</div>
{{end}}
```

## Profile Page (`/profile`)

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

The CLI's `by login` command (phase 2-9) opens `{server}/profile#tokens`
to direct users here for token creation.

```html
<div id="tokens" class="pat-section">
    <h2>Personal Access Tokens</h2>
    <p class="section-description">Tokens authenticate against the blockyard API.
       Treat them like passwords.</p>

    <form class="pat-create-form"
          hx-post="/ui/tokens"
          hx-target="#pat-result"
          hx-swap="innerHTML">
        <input type="text" name="name" placeholder="Token name (e.g., CI deploy)" required>
        <button type="submit" class="btn">Create token</button>
    </form>
    <div id="pat-result"></div>

    {{if .Tokens}}
    <table class="data-table">
        <thead>
            <tr><th>Name</th><th>Created</th><th>Last used</th><th></th></tr>
        </thead>
        <tbody>
        {{range .Tokens}}
            <tr>
                <td>{{.Name}}</td>
                <td>{{.CreatedAt | timeAgo}}</td>
                <td>{{if .LastUsedAt}}{{.LastUsedAt | timeAgo}}{{else}}Never{{end}}</td>
                <td><button class="btn btn-sm btn-danger"
                            hx-delete="/api/v1/users/me/tokens/{{.ID}}"
                            hx-confirm="Revoke token '{{.Name}}'? This cannot be undone."
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
and returns an HTML fragment (stored in `ui.fragments["pat_created"]`).
Tokens created via the UI default to 90-day expiration (no expiration
selector in the form -- keep the UI simple; users who need custom
expiration can use the API directly).

```html
<div class="pat-created">
    <p class="pat-warning">Copy this token now -- it will not be shown again.</p>
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
| GET | `/ui/apps/{name}/sidebar` | HTML fragment | required (placeholder -- returns empty 200) |

**Auth enforcement:** All UI routes share the existing soft-auth
middleware (`AppAuthMiddleware`). Page handlers for `/deployments`,
`/api-keys`, `/profile`, and `POST /ui/tokens` check for an
authenticated user via `auth.UserFromContext(r.Context())`. If not
authenticated, GET handlers redirect to `/login?return_url=<path>`;
`POST /ui/tokens` returns 401 (htmx surfaces this via its error
event).

**PAT creation errors:** On failure (e.g., empty name after trimming,
server error), `POST /ui/tokens` returns a 200 with an error fragment
swapped into `#pat-result` so the user sees inline feedback:

```html
<p class="pat-error">{{.Error}}</p>
```

## Templates

Template files in `internal/ui/templates/`:

```
templates/
├── base.html              # (existing -- modified: add left nav, htmx script tag)
├── landing.html           # (existing)
├── apps.html              # Apps page (replaces dashboard.html -- app grid, sidebar container)
├── deployments.html       # Deployment history page (includes pagination partial)
├── api_keys.html          # API keys page (credential management)
├── profile.html           # Profile page (identity, PATs)
└── pat_created.html       # One-time token display fragment
```

The pagination partial is defined inside `deployments.html` (it is the only
paginated page in this phase). If other pages gain pagination later, extract
it to a shared partial.

```html
{{define "pagination"}}
{{if gt .TotalPages 1}}
<nav class="pagination">
    {{if gt .Page 1}}
    <a href="?page={{subtract .Page 1}}{{if .Search}}&search={{urlquery .Search}}{{end}}" class="btn btn-sm">&laquo; Prev</a>
    {{end}}
    <span class="pagination-info">Page {{.Page}} of {{.TotalPages}}</span>
    {{if lt .Page .TotalPages}}
    <a href="?page={{add .Page 1}}{{if .Search}}&search={{urlquery .Search}}{{end}}" class="btn btn-sm">Next &raquo;</a>
    {{end}}
</nav>
{{end}}
{{end}}
```

The `add` and `subtract` template functions are registered alongside the
existing `deref` in the template function map.

Page templates (`apps.html`, `deployments.html`, `api_keys.html`,
`profile.html`) extend `base.html` which provides the left nav and
common layout. They are stored in `ui.pages` and parsed with
`base.html`. `landing.html` is also in `ui.pages` and parsed with
`base.html`, but its empty `ActivePage` triggers the conditional in
`base.html` that omits the nav (see Navigation Restructure above).
`dashboard.html` is removed.

The `pat_created.html` fragment is stored in `ui.fragments` and parsed
standalone (no base wrapper). Phase 2-11 adds many more fragment
templates to the `fragments` map.

## CSS Additions

Key new styles (added to existing `style.css`):

**Left navigation:**
- **`.left-nav`** -- fixed left column, ~180px wide, full height, dark
  background, flex column.
- **`.left-nav-brand`** -- logo/name at top.
- **`.left-nav-link`** -- nav item with hover and active states.
- **`.left-nav-link.active`** -- highlighted background for current page.
- **`.left-nav-version`** -- version text at bottom, muted.
- **`.page-layout`** -- flex row: left nav + main content area with
  `margin-left` matching nav width.

**Page-level:**
- **`.page-header`** -- page title + optional search/description.
- **`.data-table`** -- standard table for deployment history, PATs.
- **`.status-badge`** -- deployment status indicators.
- **`.role-badge`** -- user role indicators.
- **`.credential-row`** -- service label + status + key input form.
- **`.profile-section`** -- stacked read-only profile fields.
- **`.profile-field`** -- label + value pair.
- **`.pat-section`** -- PAT management area.
- **`.pat-create-form`** -- inline label input + create button.
- **`.pat-created`** -- one-time token display with copy button.
- **`.pat-warning`** -- yellow/amber warning text for token display.

**Sidebar shell** (non-functional until phase 2-11):
- **`.sidebar`** -- fixed right, full height, responsive width
  (`min-width: 28rem; width: 50%; max-width: 720px`), white background,
  box shadow, transform/transition for slide-in, overflow-y auto.
- **`.sidebar.open`** -- `transform: translateX(0)` (default is
  `translateX(100%)`).
- **`.sidebar-overlay`** -- fixed full-screen, semi-transparent backdrop,
  hidden by default, shown when sidebar is open.
- **`.app-card-gear`** -- gear icon positioning in app card.

**Pagination:**
- **`.pagination`** -- flex row, centered, gap between items.
- **`.pagination-info`** -- muted text showing current page.

**Utility:**
- **`.btn-danger`** -- red background, white text.
- **`.pat-error`** -- red text for inline PAT creation errors.

## Sidebar JS

Minimal vanilla JS for sidebar open/close (ready for phase 2-11):

```js
function closeSidebar() {
    // Phase 2-11 adds stopLogs(); guard avoids errors before that phase.
    if (typeof stopLogs === 'function') stopLogs();
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

1. **htmx integration** -- vendor htmx.min.js, add script tag to base
   template.
2. **Template infrastructure** -- two-map template architecture (`pages`
   vs `fragments` in `UI` struct), page templates parsed with `base.html`,
   fragment templates parsed standalone.
3. **Left navigation** -- persistent nav sidebar with Apps, Deployment
   History, API Keys (conditional), Profile links. Active page
   highlighting. `OpenbaoEnabled` flag in layout data.
4. **Apps page** -- app grid with search/filter (migrated from
   dashboard, credentials section removed). Gear icon conditional
   on collaborator+ access (using `relation` from list API). Sidebar
   shell (non-functional).
5. **Deployment History page** -- cross-app deployment table with
   search, pagination, collaborator+ visibility.
6. **API Keys page** -- third-party credential management (migrated
   from dashboard). Redirect to `/` when Openbao not configured.
7. **Profile page** -- user identity, role display, sign out.
8. **PAT management UI** -- create (with `POST /ui/tokens` fragment
   endpoint), list, and revoke personal access tokens on the Profile
   page. `id="tokens"` anchor for CLI `by login` integration.
9. **CSS** -- left nav, page layouts, sidebar shell, data tables,
   status/role badges, credential forms, profile fields, PAT section.

## Implementation Steps

### Step 1: htmx + Template Infrastructure + Left Navigation

Vendor htmx.min.js. Set up `pages`/`fragments` template maps in `UI`
struct. Define shared `layoutData` struct (embedded by all page data
structs). Modify `base.html` to include left nav (conditional on
`ActivePage`), htmx script tag, and page-layout wrapper. Remove
`dashboard.html`. Add left nav CSS.

### Step 2: Apps Page

Create `apps.html` (migrated from `dashboard.html`, credentials
section removed). Register route. Add gear icon conditional on
collaborator+ access. Add sidebar shell container (non-functional).

### Step 3: Deployment History Page

Build the deployment history table consuming `ListDeployments()`.
Search (by app name), pagination, empty state, collaborator+
visibility. Auth redirect for unauthenticated users.

### Step 4: API Keys Page

Migrate credential management from the dashboard to its own page.
Wire htmx form submissions (replacing the existing inline JS).
Redirect to `/` when Openbao not configured.

### Step 5: Profile Page + PAT Management

Implement the profile page with identity display and sign out.
Wire PAT management UI: token creation via `POST /ui/tokens`
fragment endpoint (using `ui.fragments["pat_created"]`) with
one-time display, token table with revoke buttons. Add `id="tokens"`
anchor on the PAT section.

### Step 6: Sidebar Shell + CSS

Add sidebar container and overlay to apps page. Wire gear icon
(collaborator+ conditional) to open sidebar. Register placeholder
`GET /ui/apps/{name}/sidebar` route (empty 200). Add sidebar
open/close JS and CSS transitions. Phase 2-11 replaces the
placeholder with real sidebar content.

### Step 7: Tests

Page route tests. Navigation tests (active page highlighting,
conditional API Keys link). PAT management tests (create, list,
revoke). Deployment history pagination and access filtering tests.
