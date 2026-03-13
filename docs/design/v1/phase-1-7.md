# Phase 1-7: User-Facing Web UI

Server-rendered HTML pages for browser users. Without this phase, v1 has
no answer for "what happens when a user opens the site in a browser" —
the catalog API exists (phase 1-5) but nothing renders it. The OIDC flow
redirects users to the IdP and back, but there is no page that initiates
that flow or shows what's available after login.

This phase depends on phase 1-1 (auth middleware, session cookies),
phase 1-2 (RBAC for visibility filtering), and phase 1-5 (catalog
queries, tag data). It is independent of phase 1-6 (audit/telemetry)
and can be developed in parallel with it.

## Design decision: server-rendered Go templates, no JavaScript framework

The UI uses Go's `html/template` with templates and static assets
embedded via `embed.FS`. No JavaScript framework, no build toolchain,
no node_modules.

**Why server-rendered?**

- **Simplicity:** the UI is a read-only catalog browser with forms.
  There is no interactive state that benefits from client-side rendering.
  Search and tag filtering are query-parameter-driven — the browser
  submits a form, the server renders a new page.
- **Zero frontend dependencies:** no npm, no bundler, no transpiler.
  The entire UI ships as compiled Go code. No asset pipeline to break.
- **Fast initial load:** the server renders the full HTML document.
  No JavaScript download, parse, or hydration step. On slow connections,
  this is noticeably faster than an SPA.
- **Accessibility:** server-rendered HTML with standard forms works
  without JavaScript. Screen readers, keyboard navigation, and
  progressive enhancement come for free.

**Trade-off:** interactive features (live search, drag-and-drop) are
harder to add later. This is acceptable — the v1 UI is intentionally
minimal. If richer interactivity is needed in v2, it can be layered on
with htmx or a lightweight JS library without rewriting the templates.

## Design decision: embedded assets via embed.FS

Templates and static files are embedded into the Go binary at compile
time using `//go:embed`. The binary is self-contained — no external
file paths to configure, no asset directory to deploy alongside.

**Why not external files?**

- **Deployment simplicity:** a single binary is easier to distribute
  than a binary plus a static directory. No "where are my templates?"
  debugging.
- **Immutability:** embedded files cannot be modified at runtime.
  This prevents accidental or malicious template modifications on the
  server filesystem.

**Trade-off:** template changes require recompilation. For v1 this is
fine — the UI is minimal and changes infrequently. If operators need
to customize branding, a v2 override mechanism (external template
directory that shadows the embedded defaults) is straightforward to
add.

## Design decision: no loopback HTTP call for catalog data

The dashboard renders catalog data by calling the same internal Go
functions that the `GET /api/v1/catalog` handler uses — specifically
`DB.ListCatalog()` and `DB.ListAppTags()`. It does NOT issue an HTTP
request to its own API.

**Why?**

- **Performance:** a loopback HTTP call adds serialization,
  deserialization, TCP overhead, and potentially auth token handling —
  all unnecessary when the data is available in-process.
- **Simplicity:** no need to construct an authenticated HTTP request
  to the server's own API. The template handler already has the
  authenticated user's context from the session middleware.

**Trade-off:** the UI and API could drift — the dashboard might show
data formatted differently than the API returns. This is mitigated by
the fact that both use the same `ListCatalog` query; the only
difference is the response format (HTML vs JSON).

## Design decision: credential enrollment inline on the dashboard

The dashboard includes a "Your API Keys" section when `[openbao]` is
configured with `[[openbao.services]]`. Each service shows its label,
enrollment status, and an inline form.

**Why on the dashboard, not a separate page?**

- The number of services is small (typically 1-5). A dedicated
  settings page would be an empty shell with one section. Inlining
  keeps the user on a single page and avoids navigation.
- The enrollment form is trivial: a text input and a submit button.
  It doesn't warrant its own route.

**How enrollment status is checked:** the server queries OpenBao for
the existence of a secret at the service's configured path for the
current user (a metadata read, not a value read). The enrollment
token's policy grants `create` and `update` but not `read` on user
secret paths — so the server checks existence via a list operation on
the parent path, not by reading the secret value. This preserves the
security invariant from phase 1-3: the server can store secrets but
never retrieve them.

**Form submission:** a small inline script (~20 lines) intercepts the
form submit, POSTs JSON to the existing
`POST /api/v1/users/me/credentials/{service}` endpoint (delivered in
phase 1-3), and redirects with a query parameter on completion. On
success, the page reloads showing the updated status. On error, the
page reloads with an error query parameter that the template renders
as a flash message. This keeps the enrollment API as the single code
path — no server-side form handler needed.

## Design decision: v0-compatible fallback when OIDC is not configured

When the `[oidc]` config section is absent, `GET /` renders a minimal
page listing all deployed apps with status indicators and links. No
sign-in prompt, no auth required.

**Why?**

- v0 has no app-plane authentication. Operators upgrading from v0 to
  v1 without configuring OIDC should see the same behavior — a list
  of apps they can click on.
- Development workflows (local Docker, no IdP) need a functional
  landing page.

**Implementation:** the root handler checks `srv.Config.OIDC == nil`.
If true, it queries all apps (no RBAC filter) and renders a simplified
template. If false, it checks the session cookie and branches to
landing page (unauthenticated) or dashboard (authenticated).

## Design decision: minimal CSS, no framework

A single `style.css` file (target: under 200 lines) provides layout,
typography, and app card styling. No Bootstrap, no Tailwind, no CSS
framework.

**Why?**

- The UI has ~3 page states (landing, dashboard, v0 fallback) and one
  component type (app card). A CSS framework would be 99% unused.
- Keeping the CSS minimal makes it easy for operators to override if
  they need to match corporate branding.
- No build step — the CSS is authored directly and embedded.

**Trade-off:** the UI won't look "polished" by modern SPA standards.
The design language is intentionally neutral — clean enough that
operators don't feel compelled to customize immediately, plain enough
that it doesn't impose an aesthetic.

## Deliverables

1. `internal/ui/` package — template rendering, route registration
2. `internal/ui/templates/base.html` — shared layout (head, body, CSS link)
3. `internal/ui/templates/landing.html` — unauthenticated: sign-in prompt +
   public apps
4. `internal/ui/templates/dashboard.html` — authenticated: RBAC-filtered app
   listing, search, tag filter, credential enrollment section
5. `internal/ui/static/style.css` — minimal stylesheet
6. v0-compatible fallback rendering (no OIDC)
7. Router integration — `GET /` and `/static/*`

## Step-by-step

### Step 1: Package scaffolding and embed setup

New package: `internal/ui/`

```go
package ui

import (
    "embed"
    "html/template"
    "net/http"

    "github.com/cynkra/blockyard/internal/server"
    "github.com/go-chi/chi/v5"
)

//go:embed templates/*.html static/*
var content embed.FS

// UI holds parsed templates and the static file handler.
type UI struct {
    templates *template.Template
    static    http.Handler
}

// New parses all embedded templates and prepares the static file server.
func New() *UI {
    funcMap := template.FuncMap{
        "deref": func(s *string) string {
            if s == nil {
                return ""
            }
            return *s
        },
    }
    tmpl := template.Must(
        template.New("").Funcs(funcMap).ParseFS(content, "templates/*.html"),
    )
    static := http.FileServer(http.FS(content))
    return &UI{templates: tmpl, static: static}
}

// RegisterRoutes mounts the UI routes on the router.
func (ui *UI) RegisterRoutes(r chi.Router, srv *server.Server) {
    r.Get("/", ui.root(srv))
    r.Handle("/static/*", ui.static)
}
```

The `deref` template function safely dereferences `*string` pointers
(for `Title` and `Description` which are nullable columns). Additional
template functions can be added to `funcMap` as needed.

**Tests:**

- `New()` does not panic (templates parse successfully)
- `embed.FS` contains expected files

### Step 2: Base template

New file: `internal/ui/templates/base.html`

```html
{{define "base"}}
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>{{block "title" .}}blockyard{{end}}</title>
    <link rel="stylesheet" href="/static/style.css">
</head>
<body>
    <div class="container">
        {{block "content" .}}{{end}}
    </div>
</body>
</html>
{{end}}
```

The base template provides the HTML skeleton. Child templates override
the `title` and `content` blocks. The `container` div is the only
layout wrapper — CSS handles the rest.

**Tests:**

- Base template renders valid HTML
- Title block defaults to "blockyard"

### Step 3: Landing page template

New file: `internal/ui/templates/landing.html`

```html
{{template "base" .}}

{{define "title"}}Sign in — blockyard{{end}}

{{define "content"}}
<header>
    <h1>blockyard</h1>
</header>
<main>
    <section class="sign-in">
        <a href="/login" class="btn btn-primary">Sign in</a>
    </section>

    {{if .PublicApps}}
    <section class="public-apps">
        <h2>Public apps</h2>
        <div class="app-grid">
            {{range .PublicApps}}
            <a href="/app/{{.Name}}/" class="app-card">
                <span class="app-name">{{.Name}}</span>
                {{if .Title}}<span class="app-title">{{deref .Title}}</span>{{end}}
                <span class="app-status app-status--{{.Status}}">{{.Status}}</span>
            </a>
            {{end}}
        </div>
    </section>
    {{end}}
</main>
{{end}}
```

**Data struct:**

```go
type landingData struct {
    PublicApps []catalogEntry
}
```

The landing page is rendered when the user is not authenticated and
OIDC is configured. It shows a sign-in button and, if any public apps
exist, lists them below.

**Tests:**

- Renders sign-in link pointing to `/login`
- Public apps section present when apps exist
- Public apps section absent when no public apps

### Step 4: Dashboard template

New file: `internal/ui/templates/dashboard.html`

```html
{{template "base" .}}

{{define "title"}}Dashboard — blockyard{{end}}

{{define "content"}}
<header>
    <h1>blockyard</h1>
    <nav>
        <span class="user-identity">{{.UserSub}}</span>
        <form method="POST" action="/logout" class="inline">
            <button type="submit" class="btn btn-link">Sign out</button>
        </form>
    </nav>
</header>
<main>
    <section class="search-filter">
        <form method="GET" action="/">
            <input type="text" name="search" value="{{.Search}}"
                   placeholder="Search apps..." aria-label="Search apps">
            {{if .AllTags}}
            <select name="tag" aria-label="Filter by tag">
                <option value="">All tags</option>
                {{range .AllTags}}
                <option value="{{.Name}}" {{if eq .Name $.ActiveTag}}selected{{end}}>
                    {{.Name}}
                </option>
                {{end}}
            </select>
            {{end}}
            <button type="submit" class="btn">Search</button>
        </form>
    </section>

    {{if .Apps}}
    <section class="app-grid">
        {{range .Apps}}
        <a href="/app/{{.Name}}/" class="app-card">
            <span class="app-name">{{.Name}}</span>
            {{if .Title}}<span class="app-title">{{deref .Title}}</span>{{end}}
            {{if .Description}}<span class="app-desc">{{deref .Description}}</span>{{end}}
            <span class="app-status app-status--{{.Status}}">{{.Status}}</span>
            {{if .Tags}}
            <span class="app-tags">
                {{range .Tags}}<span class="tag">{{.}}</span>{{end}}
            </span>
            {{end}}
        </a>
        {{end}}
    </section>
    {{else}}
    <section class="empty-state">
        {{if eq .UserRole "publisher"}}
        <p>No apps yet. Deploy your first app using the API.</p>
        {{else if eq .UserRole "admin"}}
        <p>No apps deployed.</p>
        {{else}}
        <p>No apps shared with you.</p>
        {{end}}
    </section>
    {{end}}

    {{if .Services}}
    <section class="credentials">
        <h2>Your API Keys</h2>
        {{if .CredentialError}}<p class="flash flash--error">{{.CredentialError}}</p>{{end}}
        {{if .CredentialSuccess}}<p class="flash flash--success">Credential saved.</p>{{end}}
        {{range .Services}}
        <div class="credential-row">
            <span class="credential-label">{{.Label}}</span>
            <span class="credential-status credential-status--{{.Status}}">
                {{if eq .Status "configured"}}configured{{else}}not set{{end}}
            </span>
            <form class="credential-form" data-service="{{.ID}}">
                <input type="text" name="api_key" placeholder="Paste API key"
                       aria-label="{{.Label}} API key" required>
                <button type="submit" class="btn btn-sm">Save</button>
            </form>
        </div>
        {{end}}
    </section>
    {{end}}

    <script>
    document.querySelectorAll('.credential-form').forEach(form => {
        form.addEventListener('submit', async (e) => {
            e.preventDefault();
            const service = form.dataset.service;
            const input = form.querySelector('input[name="api_key"]');
            const btn = form.querySelector('button');
            btn.disabled = true;
            btn.textContent = 'Saving...';
            try {
                const resp = await fetch('/api/v1/users/me/credentials/' + service, {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({api_key: input.value}),
                });
                if (resp.ok) {
                    window.location.replace('/?credential_saved=1');
                } else {
                    const body = await resp.json().catch(() => null);
                    const msg = body && body.message ? body.message : 'Failed to save credential';
                    window.location.replace('/?credential_error=' + encodeURIComponent(msg));
                }
            } catch {
                window.location.replace('/?credential_error=Network+error');
            }
        });
    });
    </script>
</main>
{{end}}
```

The inline script intercepts credential form submissions, POSTs JSON
to the existing phase 1-3 enrollment API, and redirects with a query
parameter for flash message display. This avoids a separate server-side
form handler — the JSON API is the only enrollment code path.

**Data structs:**

```go
type dashboardData struct {
    UserSub           string
    UserRole          string
    Search            string
    ActiveTag         string
    AllTags           []db.TagRow
    Apps              []catalogEntry
    Services          []serviceEntry
    CredentialError   string
    CredentialSuccess bool
}

type catalogEntry struct {
    ID          string
    Name        string
    Title       *string
    Description *string
    Status      string
    Tags        []string
}

type serviceEntry struct {
    ID     string
    Label  string
    Status string // "configured" or "not_set"
}
```

**Tests:**

- Renders user identity and sign-out form
- Search form preserves current query parameters
- App cards link to `/app/{name}/`
- Empty state message varies by role
- Credentials section present when services configured
- Credentials section absent when no services
- Tag filter dropdown renders all tags with correct selected state

### Step 5: Root handler

The `root` method on `UI` is the main router for `GET /`. It branches
on three conditions: v0 mode (no OIDC), unauthenticated, and
authenticated.

```go
func (ui *UI) root(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // v0 mode — no OIDC configured
        if srv.Config.OIDC == nil {
            ui.renderV0(w, r, srv)
            return
        }

        // Check session
        user := auth.UserFromContext(r.Context())
        if user == nil {
            ui.renderLanding(w, r, srv)
            return
        }

        ui.renderDashboard(w, r, srv, user)
    }
}
```

**v0 fallback handler:**

```go
func (ui *UI) renderV0(w http.ResponseWriter, r *http.Request, srv *server.Server) {
    // Query all apps — no RBAC filter (v0 mode has no auth)
    apps, _, err := srv.DB.ListCatalog(db.CatalogParams{
        CallerRole: "admin", // no filtering
        Page:       1,
        PerPage:    1000,
    })
    if err != nil {
        http.Error(w, "Internal error", http.StatusInternalServerError)
        return
    }

    entries := buildCatalogEntries(apps, srv)
    ui.templates.ExecuteTemplate(w, "landing.html", landingData{
        PublicApps: entries,
    })
}
```

The v0 fallback reuses the landing template but shows all apps (not
just public ones) since there is no authentication.

**Landing page handler:**

```go
func (ui *UI) renderLanding(w http.ResponseWriter, r *http.Request, srv *server.Server) {
    // Query public apps only
    apps, _, err := srv.DB.ListCatalog(db.CatalogParams{
        Page:    1,
        PerPage: 100,
    })
    if err != nil {
        http.Error(w, "Internal error", http.StatusInternalServerError)
        return
    }

    entries := buildCatalogEntries(apps, srv)
    ui.templates.ExecuteTemplate(w, "landing.html", landingData{
        PublicApps: entries,
    })
}
```

When `CallerSub` is empty (unauthenticated), `ListCatalog` returns
only public apps — matching the existing catalog API behavior from
phase 1-5.

**Dashboard handler:**

```go
func (ui *UI) renderDashboard(w http.ResponseWriter, r *http.Request, srv *server.Server, user *auth.User) {
    caller := auth.CallerFromContext(r.Context())

    search := r.URL.Query().Get("search")
    activeTag := r.URL.Query().Get("tag")

    params := db.CatalogParams{
        Search:  search,
        Tag:     activeTag,
        Page:    1,
        PerPage: 100,
    }
    if caller != nil {
        params.CallerSub = caller.Sub
        params.CallerGroups = caller.Groups
        params.CallerRole = caller.Role.String()
    }

    apps, _, err := srv.DB.ListCatalog(params)
    if err != nil {
        http.Error(w, "Internal error", http.StatusInternalServerError)
        return
    }

    allTags, _ := srv.DB.ListTags()
    entries := buildCatalogEntries(apps, srv)

    data := dashboardData{
        UserSub:   user.Sub,
        UserRole:  caller.Role.String(),
        Search:    search,
        ActiveTag: activeTag,
        AllTags:   allTags,
        Apps:      entries,
    }

    // Credential enrollment section
    if srv.Config.Openbao != nil && len(srv.Config.Openbao.Services) > 0 {
        data.Services = buildServiceEntries(srv, user.Sub)
    }

    // Flash messages from credential form redirect
    if errMsg := r.URL.Query().Get("credential_error"); errMsg != "" {
        data.CredentialError = errMsg
    }
    if r.URL.Query().Get("credential_saved") == "1" {
        data.CredentialSuccess = true
    }

    ui.templates.ExecuteTemplate(w, "dashboard.html", data)
}
```

**Catalog entry builder:**

```go
func buildCatalogEntries(apps []db.AppRow, srv *server.Server) []catalogEntry {
    entries := make([]catalogEntry, 0, len(apps))
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

        entries = append(entries, catalogEntry{
            ID:          app.ID,
            Name:        app.Name,
            Title:       app.Title,
            Description: app.Description,
            Status:      status,
            Tags:        tagNames,
        })
    }
    return entries
}
```

**Service entry builder:**

```go
func buildServiceEntries(srv *server.Server, sub string) []serviceEntry {
    entries := make([]serviceEntry, 0, len(srv.Config.Openbao.Services))
    for _, svc := range srv.Config.Openbao.Services {
        status := "not_set"
        if srv.OpenBaoClient != nil {
            // Check existence via list on the parent path.
            // The enrollment token has list but not read permission.
            exists, err := srv.OpenBaoClient.SecretExists(
                context.Background(),
                "secret/data/users/"+sub+"/"+svc.Path,
            )
            if err == nil && exists {
                status = "configured"
            }
        }
        entries = append(entries, serviceEntry{
            ID:     svc.ID,
            Label:  svc.Label,
            Status: status,
        })
    }
    return entries
}
```

**Tests:**

- `GET /` with no OIDC config — renders v0 page with all apps
- `GET /` unauthenticated with OIDC — renders landing page
- `GET /` authenticated — renders dashboard with RBAC-filtered apps
- `GET /?search=foo` — passes search param to catalog query
- `GET /?tag=finance` — passes tag filter to catalog query
- `GET /?credential_saved=1` — renders success flash
- `GET /?credential_error=bad+key` — renders error flash
- Service entries reflect OpenBao enrollment status

### Step 6: Static CSS

New file: `internal/ui/static/style.css`

The stylesheet targets clean, functional layout. Key rules:

```css
/* Reset and base */
*, *::before, *::after { box-sizing: border-box; }
body {
    font-family: system-ui, -apple-system, sans-serif;
    line-height: 1.5;
    color: #1a1a1a;
    background: #fafafa;
    margin: 0;
    padding: 0;
}

.container { max-width: 64rem; margin: 0 auto; padding: 1.5rem; }

/* Header */
header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 2rem;
    padding-bottom: 1rem;
    border-bottom: 1px solid #e0e0e0;
}
header h1 { margin: 0; font-size: 1.5rem; }
header nav { display: flex; align-items: center; gap: 1rem; }

/* Buttons */
.btn {
    display: inline-block;
    padding: 0.4rem 1rem;
    border: 1px solid #ccc;
    border-radius: 4px;
    background: #fff;
    color: #1a1a1a;
    text-decoration: none;
    cursor: pointer;
    font-size: 0.875rem;
}
.btn-primary { background: #2563eb; color: #fff; border-color: #2563eb; }
.btn-link { border: none; background: none; color: #2563eb; padding: 0; cursor: pointer; }
.btn-sm { padding: 0.2rem 0.6rem; font-size: 0.8rem; }

/* Search and filter */
.search-filter form { display: flex; gap: 0.5rem; margin-bottom: 1.5rem; }
.search-filter input[type="text"] {
    flex: 1;
    padding: 0.4rem 0.75rem;
    border: 1px solid #ccc;
    border-radius: 4px;
}
.search-filter select { padding: 0.4rem; border: 1px solid #ccc; border-radius: 4px; }

/* App grid */
.app-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(18rem, 1fr)); gap: 1rem; }
.app-card {
    display: flex;
    flex-direction: column;
    padding: 1rem;
    border: 1px solid #e0e0e0;
    border-radius: 6px;
    background: #fff;
    text-decoration: none;
    color: inherit;
    transition: border-color 0.15s;
}
.app-card:hover { border-color: #2563eb; }
.app-name { font-weight: 600; }
.app-title { color: #555; font-size: 0.9rem; }
.app-desc { color: #777; font-size: 0.85rem; margin-top: 0.25rem; }
.app-status { font-size: 0.8rem; margin-top: auto; padding-top: 0.5rem; }
.app-status--running { color: #16a34a; }
.app-status--stopped { color: #999; }

/* Tags */
.app-tags { display: flex; gap: 0.25rem; flex-wrap: wrap; margin-top: 0.25rem; }
.tag {
    font-size: 0.75rem;
    padding: 0.1rem 0.4rem;
    background: #f0f0f0;
    border-radius: 3px;
    color: #555;
}

/* Credentials section */
.credentials { margin-top: 2rem; padding-top: 1.5rem; border-top: 1px solid #e0e0e0; }
.credentials h2 { font-size: 1.1rem; margin-bottom: 1rem; }
.credential-row {
    display: flex;
    align-items: center;
    gap: 1rem;
    padding: 0.5rem 0;
    border-bottom: 1px solid #f0f0f0;
}
.credential-label { font-weight: 500; min-width: 10rem; }
.credential-status { font-size: 0.85rem; min-width: 6rem; }
.credential-status--configured { color: #16a34a; }
.credential-status--not_set { color: #999; }
.credential-form { display: flex; gap: 0.5rem; }
.credential-form input { padding: 0.3rem 0.5rem; border: 1px solid #ccc; border-radius: 4px; }

/* Flash messages */
.flash { padding: 0.5rem 1rem; border-radius: 4px; margin-bottom: 1rem; }
.flash--error { background: #fee2e2; color: #b91c1c; }
.flash--success { background: #dcfce7; color: #166534; }

/* Empty state */
.empty-state { padding: 2rem; text-align: center; color: #777; }

/* Sign-in section */
.sign-in { text-align: center; padding: 3rem 0; }

/* Utilities */
.inline { display: inline; }
.user-identity { font-size: 0.9rem; color: #555; }
```

This stays under 200 lines and covers all template components.

**Tests:**

- Static file server returns `style.css` at `/static/style.css`
- Response has `Content-Type: text/css`

### Step 7: OpenBao SecretExists method

The dashboard needs to check whether a user has enrolled a credential
without reading the secret value. Add a `SecretExists` method to the
OpenBao client (in `internal/integration/openbao.go`):

```go
// SecretExists checks whether a secret exists at the given path
// without reading its value. Uses the metadata endpoint which
// requires list permission, not read.
func (c *Client) SecretExists(ctx context.Context, path string) (bool, error) {
    // Convert data path to metadata path for existence check.
    // secret/data/users/sub/apikeys/openai → secret/metadata/users/sub/apikeys/openai
    metaPath := strings.Replace(path, "secret/data/", "secret/metadata/", 1)

    resp, err := c.client.Logical().ReadWithContext(ctx, metaPath)
    if err != nil {
        // 404 from Vault means the secret doesn't exist
        if respErr, ok := err.(*api.ResponseError); ok && respErr.StatusCode == 404 {
            return false, nil
        }
        return false, err
    }
    return resp != nil, nil
}
```

This uses the metadata endpoint, which requires `read` on
`secret/metadata/users/*` — a separate policy path from
`secret/data/users/*`. The enrollment token's policy should grant
this. The actual secret data at `secret/data/users/*` remains
unreadable to the server.

**Policy addition for enrollment token:**

```hcl
path "secret/metadata/users/*" {
    capabilities = ["read"]
}
```

**Tests:**

- `SecretExists` returns true when secret exists
- `SecretExists` returns false when secret does not exist
- `SecretExists` returns false (not error) on 404

### Step 8: Service catalog config

Phase 1-7 requires the `[[openbao.services]]` config to be parsed
and available. This config structure was specified in the plan's phase
1-7 section.

**Config types** (in `internal/config/config.go`):

```go
type ServiceConfig struct {
    ID    string `toml:"id"`
    Label string `toml:"label"`
    Path  string `toml:"path"`
}

type OpenbaoConfig struct {
    // ... existing fields ...
    Services []ServiceConfig `toml:"services"`
}
```

**Validation:**

```go
if cfg.Openbao != nil {
    seen := make(map[string]bool)
    for _, svc := range cfg.Openbao.Services {
        if svc.ID == "" || svc.Label == "" || svc.Path == "" {
            return fmt.Errorf("config: openbao.services entries must have id, label, and path")
        }
        if seen[svc.ID] {
            return fmt.Errorf("config: duplicate openbao.services id %q", svc.ID)
        }
        seen[svc.ID] = true
    }
}
```

**Container env var injection** (in `coldstart.go:spawnWorker()`):

When `[openbao]` is configured with services, inject
`BLOCKYARD_VAULT_SERVICES` alongside `VAULT_ADDR` and
`BLOCKYARD_API_URL`:

```go
if len(srv.Config.Openbao.Services) > 0 {
    svcMap := make(map[string]string, len(srv.Config.Openbao.Services))
    for _, svc := range srv.Config.Openbao.Services {
        svcMap[svc.ID] = svc.Path
    }
    svcJSON, _ := json.Marshal(svcMap)
    extraEnv["BLOCKYARD_VAULT_SERVICES"] = string(svcJSON)
}
```

**Tests:**

- Parse config with `[[openbao.services]]` — all fields populated
- Parse config without services — empty slice
- Validation: reject empty id/label/path
- Validation: reject duplicate service IDs
- `BLOCKYARD_VAULT_SERVICES` env var injected into worker spec
- JSON format correct: `{"openai":"apikeys/openai"}`

### Step 9: Router integration

Wire the UI into the main router in `internal/api/router.go`:

```go
func NewRouter(srv *server.Server) *chi.Mux {
    r := chi.NewRouter()

    // UI routes — before API and proxy routes so GET / is handled by the UI
    uiHandler := ui.New()
    uiHandler.RegisterRoutes(r, srv)

    // Auth endpoints
    r.Get("/login", loginHandler(srv))
    r.Get("/callback", callbackHandler(srv))
    r.Post("/logout", logoutHandler(srv))

    // Health endpoints (unauthenticated)
    r.Get("/healthz", healthz)
    r.Get("/readyz", readyzHandler(srv))

    // API routes
    r.Route("/api/v1", func(r chi.Router) { /* ... existing ... */ })

    // Proxy routes
    r.Get("/app/{ref}", trailingSlashRedirect)
    r.HandleFunc("/app/{ref}/", proxyHandler(srv))
    r.HandleFunc("/app/{ref}/*", proxyHandler(srv))

    return r
}
```

The UI's `GET /` route must be registered before the proxy catch-all
to ensure it is matched. The `/static/*` route is explicitly prefixed
and does not conflict with app routes.

**Auth middleware on `GET /`:** the root handler reads the session
from context (if present) but does NOT require authentication. The
auth middleware should run on `GET /` in a "soft" mode — populating
the context with user info if a valid session exists, but not
redirecting to `/login` if absent. This is already the behavior of
the app-plane middleware for non-`/app/` routes (it only enforces
auth on `/app/*`), so no middleware change is needed.

**Tests:**

- `GET /` returns 200 (not 404 or redirect)
- `GET /static/style.css` returns 200 with CSS content
- `GET /` does not conflict with `/app/{ref}/` routes
- `GET /` does not conflict with `/api/v1/` routes

## New source files

| File | Purpose |
|------|---------|
| `internal/ui/ui.go` | Package scaffolding, `New()`, `RegisterRoutes()`, handlers |
| `internal/ui/templates/base.html` | Shared HTML layout |
| `internal/ui/templates/landing.html` | Unauthenticated landing page |
| `internal/ui/templates/dashboard.html` | Authenticated dashboard |
| `internal/ui/static/style.css` | Minimal stylesheet |
| `internal/ui/ui_test.go` | Template rendering and handler tests |

## Modified files

| File | Change |
|------|--------|
| `internal/api/router.go` | Mount UI routes (`GET /`, `/static/*`) |
| `internal/config/config.go` | Add `ServiceConfig` struct, `Services` field on `OpenbaoConfig`, validation |
| `internal/integration/openbao.go` | Add `SecretExists()` method |
| `internal/proxy/coldstart.go` | Inject `BLOCKYARD_VAULT_SERVICES` env var |

## Exit criteria

Phase 1-7 is done when:

**Landing page:**

- `GET /` with OIDC configured and no session renders landing page
  with "Sign in" link pointing to `/login`
- Public apps appear on the landing page when they exist
- No public apps — sign-in prompt only, no empty list

**Dashboard:**

- `GET /` with valid session renders dashboard with RBAC-filtered
  app list
- User identity and "Sign out" link displayed in header
- Search box filters by name, title, description (server-side)
- Tag filter dropdown shows all tags, filters catalog
- App cards link to `/app/{name}/`
- App cards show title, description, status, tags when present
- Empty state message varies by role (publisher, admin, viewer)

**Credential enrollment:**

- "Your API Keys" section rendered when `[[openbao.services]]`
  configured
- Each service shows label and enrollment status (configured / not set)
- Inline form POSTs JSON to the existing enrollment API via fetch
- Success and error feedback via flash messages after redirect
- Section hidden when `[openbao]` not configured or has no services

**v0 compatibility:**

- `GET /` with no OIDC config renders all apps without auth
- No sign-in prompt in v0 mode

**Static assets:**

- `GET /static/style.css` returns CSS with correct content type
- CSS embedded in binary (no external file dependency)

**General:**

- All new unit and integration tests pass
- All existing tests still pass
- `go vet ./...` clean
- `go test ./...` green
- Templates render valid HTML
- JavaScript limited to credential form submission (~20 lines inline)
