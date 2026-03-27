# Phase 2-10: Multi-Page Navigation + htmx

Converts the single-page dashboard into a multi-page layout with
persistent left navigation, htmx integration, and four distinct pages:
Apps, Deployment History, API Keys, and Profile (with PAT management).

Depends on phase 2-8 (backend prerequisites) for the API endpoints
consumed by the Deployment History page and Profile page.

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

## Apps Page (`/`)

The app grid with search and tag filter. This is the existing dashboard
content minus the user identity header and API keys section. The gear
icon on each card opens the per-app management sidebar (phase 2-11).
The gear icon is only rendered for users with collaborator+ access
to the app (using the `relation` field from the list API, added in
phase 2-8).

```html
<a href="/app/{{.Name}}/" class="app-card">
    {{if .CanManage}}
    <button class="app-card-gear"
            hx-get="/ui/apps/{{.Name}}/sidebar"
            hx-target="#sidebar"
            hx-swap="innerHTML"
            aria-label="Manage {{.Name}}"
            onclick="event.preventDefault()">&#9881;</button>
    {{end}}
    <!-- existing card content -->
</a>
```

The sidebar container is added to the page but is non-functional until
phase 2-11 registers the fragment routes:

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
and returns an HTML fragment (stored in `ui.fragments["pat_created"]`):

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

## Templates

Template files in `internal/ui/templates/`:

```
templates/
├── base.html              # (existing -- modified: add left nav, htmx script tag)
├── landing.html           # (existing)
├── apps.html              # Apps page (replaces dashboard.html -- app grid, sidebar container)
├── deployments.html       # Deployment history page
├── api_keys.html          # API keys page (credential management)
├── profile.html           # Profile page (identity, PATs)
└── pat_created.html       # One-time token display fragment
```

Page templates (`apps.html`, `deployments.html`, `api_keys.html`,
`profile.html`) extend `base.html` which provides the left nav and
common layout. They are stored in `ui.pages` and parsed with `base.html`.

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

**Utility:**
- **`.btn-danger`** -- red background, white text.

## Sidebar JS

Minimal vanilla JS for sidebar open/close (ready for phase 2-11):

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
struct. Modify `base.html` to include left nav and htmx script tag.
Add `OpenbaoEnabled` flag to layout data. Add left nav CSS.

### Step 2: Apps Page

Create `apps.html` (migrated from `dashboard.html`, credentials
section removed). Register route. Add gear icon conditional on
collaborator+ access. Add sidebar shell container (non-functional).

### Step 3: Deployment History Page

Build the deployment history table consuming
`GET /api/v1/deployments`. Search, pagination, collaborator+
visibility.

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
(collaborator+ conditional) to open sidebar. Add sidebar open/close
JS and CSS transitions. Non-functional until phase 2-11 registers
fragment routes.

### Step 7: Tests

Page route tests. Navigation tests (active page highlighting,
conditional API Keys link). PAT management tests (create, list,
revoke). Deployment history pagination and access filtering tests.
