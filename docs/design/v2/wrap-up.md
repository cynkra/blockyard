# UI Wrap-Up: Polish & UX Improvements

Post-implementation improvements for the web UI introduced in phases 2-10
and 2-11. These are refinements to the existing functionality, not new
features. The goal is to make the UI feel finished and pleasant to use.

## 1. Left Navigation Overhaul

### Rename & Add Icons

Rename "Deployment History" to "History". Add inline SVG icons to each
nav link. Use [Lucide](https://lucide.dev/) icons (MIT-licensed, 24×24
stroke-based SVGs, consistent with modern dashboard UIs). Embed each
icon inline as `<svg>` elements inside the anchor tags so they inherit
color from CSS and work in both expanded and collapsed states.

Icon assignments:

| Link       | Lucide Icon        |
|------------|--------------------|
| Apps       | `layout-grid`      |
| History    | `history`          |
| API Keys   | `key-round`        |
| Profile    | `user`             |
| Docs       | `book-open`        |
| API        | `code`             |
| Sign Out   | `log-out`          |
| Dark mode  | `sun` / `moon`     |
| Collapse   | `panel-left-close` / `panel-left-open` |

### Increase Link Height

The left nav has significant unused vertical space. Increase vertical
padding on `.left-nav-link` (currently `0.6rem 1rem`) to fill the
available height better, making links easier to click.

### Sign-Out in Nav Footer

Add a sign-out link at the bottom of the left nav, above the version
string. This replaces the need for a separate profile-page action and
gives the sign-out button a permanent, discoverable location. The
existing `.left-nav-version` div can be wrapped in a footer group that
includes both the sign-out link and version info:

```
<div class="left-nav-footer">
    <a href="/auth/logout" class="left-nav-link">Sign Out</a>
    <div class="left-nav-version">{{.Version}}</div>
</div>
```

The footer is pushed to the bottom via `margin-top: auto` on a flex
column layout (the nav already uses flexbox).

### Collapsible Nav

The left nav can be collapsed to an icon-only state. A toggle button
(`panel-left-close` / `panel-left-open` Lucide icon) at the top of
the nav switches between expanded and collapsed. Collapsed width is
**56px** (24px icon + 16px padding each side). In collapsed state,
only the SVG icons are visible. Link text is hidden via
`width: 0; overflow: hidden` with a `transition: width 0.2s ease`
for a smooth collapse. The footer collapses to just icons (sign-out,
dark mode toggle).

The `.main-content` margin-left transitions from 180px to 56px to
match. Sidebar position is unaffected (anchored to viewport right).

Persist the collapsed/expanded preference in `localStorage` under
key `nav-collapsed`. The toggle sets a class on the nav element
(`.left-nav.collapsed`) and all child styling derives from that.

### Docs & API Links

Add links to documentation and API reference in the nav footer, above
sign-out:

- **Docs**: links to the GitHub repository (external link, opens in
  new tab). The URL is derived from the existing server configuration
  or hardcoded to the project repo.
- **API**: links to the existing Swagger UI at `/swagger/`.

In collapsed state these show as icons only (`book-open` for docs,
`code` for API).

### Dark Mode

Add a dark mode toggle in the nav footer (`sun`/`moon` Lucide icon).
Dark mode applies a `.dark` class to `<html>` and all colors are
defined via CSS custom properties (variables) so the switch is purely
CSS.

Persist the preference in `localStorage` under key `theme`. On page
load, apply the class before first paint (inline `<script>` in
`<head>` to avoid flash of wrong theme). Respect
`prefers-color-scheme` as the default when no explicit preference is
stored.

This requires refactoring the existing `style.css` to use CSS
variables instead of hardcoded color values throughout. The complete
color token table:

```css
:root {
    /* Surfaces */
    --bg-body: #fafafa;
    --bg-surface: #ffffff;
    --bg-muted: #f3f4f6;

    /* Navigation (dark chrome in both modes) */
    --bg-nav: #1a1a2e;
    --text-nav: #b0b0c0;
    --text-nav-hover: #e0e0e0;
    --text-nav-active: #ffffff;

    /* Text */
    --text-primary: #1a1a1a;
    --text-secondary: #555555;
    --text-muted: #888888;

    /* Borders */
    --border-default: #e5e7eb;
    --border-input: #cccccc;

    /* Accent */
    --accent: #2563eb;
    --accent-hover: #1d4ed8;
    --accent-subtle: #eef2ff;

    /* Status badges */
    --status-ready-bg: #dcfce7;
    --status-ready-text: #166534;
    --status-building-bg: #fef3c7;
    --status-building-text: #92400e;
    --status-failed-bg: #fee2e2;
    --status-failed-text: #b91c1c;
    --status-stopped-bg: #f3f4f6;
    --status-stopped-text: #6b7280;

    /* Tags */
    --tag-bg: #f0f0f0;
    --tag-text: #555555;
    --tag-chip-bg: #e0e7ff;
    --tag-chip-text: #3730a3;

    /* Components */
    --session-chip-bg: #f0f4ff;
    --worker-hover-bg: #eef2ff;
    --log-bg: #1e1e2e;
    --log-text: #cdd6f4;

    /* Buttons */
    --btn-success: #16a34a;
    --btn-success-hover: #15803d;
    --btn-danger: #dc2626;
    --btn-danger-hover: #b91c1c;

    /* Feedback / flash */
    --flash-success-bg: #dcfce7;
    --flash-success-text: #166534;
    --flash-error-bg: #fee2e2;
    --flash-error-text: #b91c1c;

    /* Sidebar */
    --sidebar-bg: #ffffff;
    --sidebar-border: #e5e7eb;

    /* Toast */
    --toast-success-bg: #dcfce7;
    --toast-success-text: #166534;
    --toast-error-bg: #fee2e2;
    --toast-error-text: #b91c1c;
    --toast-info-bg: #dbeafe;
    --toast-info-text: #1e40af;
}

html.dark {
    /* Surfaces */
    --bg-body: #0f172a;
    --bg-surface: #1e293b;
    --bg-muted: #334155;

    /* Navigation */
    --bg-nav: #0c1021;
    --text-nav: #94a3b8;
    --text-nav-hover: #e2e8f0;
    --text-nav-active: #ffffff;

    /* Text */
    --text-primary: #f1f5f9;
    --text-secondary: #94a3b8;
    --text-muted: #64748b;

    /* Borders */
    --border-default: #334155;
    --border-input: #475569;

    /* Accent */
    --accent: #3b82f6;
    --accent-hover: #60a5fa;
    --accent-subtle: #1e293b;

    /* Status badges */
    --status-ready-bg: #052e16;
    --status-ready-text: #4ade80;
    --status-building-bg: #422006;
    --status-building-text: #fbbf24;
    --status-failed-bg: #450a0a;
    --status-failed-text: #f87171;
    --status-stopped-bg: #1e293b;
    --status-stopped-text: #94a3b8;

    /* Tags */
    --tag-bg: #374151;
    --tag-text: #d1d5db;
    --tag-chip-bg: #312e81;
    --tag-chip-text: #a5b4fc;

    /* Components */
    --session-chip-bg: #1e3a5f;
    --worker-hover-bg: #1e293b;
    --log-bg: #0c0f1a;
    --log-text: #cdd6f4;

    /* Buttons */
    --btn-success: #22c55e;
    --btn-success-hover: #16a34a;
    --btn-danger: #ef4444;
    --btn-danger-hover: #dc2626;

    /* Feedback / flash */
    --flash-success-bg: #052e16;
    --flash-success-text: #4ade80;
    --flash-error-bg: #450a0a;
    --flash-error-text: #f87171;

    /* Sidebar */
    --sidebar-bg: #1e293b;
    --sidebar-border: #334155;

    /* Toast */
    --toast-success-bg: #052e16;
    --toast-success-text: #4ade80;
    --toast-error-bg: #450a0a;
    --toast-error-text: #f87171;
    --toast-info-bg: #172554;
    --toast-info-text: #93c5fd;
}
```

The dark palette uses Tailwind's Slate scale for neutral surfaces
(slate-900 through slate-400) paired with standard Tailwind color
stops for semantic tokens. The nav stays dark-on-dark in both modes,
just slightly deeper in dark mode to maintain contrast with the main
content area. The log viewer retains its terminal-dark aesthetic in
both modes.

## 2. Log Tab: Vertical Layout

The current side-by-side layout for worker selection and log output does
not give the log view enough horizontal space. Switch to a vertically
stacked layout:

1. Worker selection table at the top (compact, collapsed to essential
   columns: worker ID, status, session count).
2. Log output below, taking remaining height.

The worker table should be short -- cap it at ~5 visible rows with
scroll (`max-height: 14rem; overflow-y: auto` on the table wrapper).
This keeps the log viewer as the dominant element. On worker click,
the log output area swaps as it does today.

Change `.log-viewer` from `display: flex` (horizontal) to
`flex-direction: column`. The log content area gets `flex: 1` to
fill remaining height.

## 3. Styled Dropdowns

Replace native `<select>` elements with custom-styled dropdowns. The
current selects (access type, tag picker, role selector) use browser
defaults which clash with the rest of the UI.

Implementation approach: a lightweight JS component that wraps existing
`<select>` elements. The component renders a styled trigger button and a
dropdown list, keeps the real `<select>` in sync for htmx form
submission, and closes on outside click or Escape. The component must
fire `change` events on the hidden `<select>` so that existing
`hx-on:change` handlers (e.g. access type auto-save) continue to work
unchanged.

CSS classes: `.custom-select`, `.custom-select-trigger`,
`.custom-select-options`, `.custom-select-option`. Style to match the
existing input/button design language (border-radius 4px, same font
size, same padding). The trigger shows a chevron-down icon (Lucide
`chevron-down`, 16px) on the right side. Selected option text in the
trigger uses `--text-primary`; the dropdown uses `--bg-surface` with
`--border-default` and a subtle `box-shadow: 0 4px 12px rgba(0,0,0,0.1)`.

Keyboard: arrow keys move focus through options, Enter selects,
Escape closes. Tab closes and moves focus forward.

## 4. ACL Principal Autocomplete

The collaborators tab currently requires typing an exact username or
email to grant access. Add a search-as-you-type input that queries the
server for matching users.

### Backend

New DB method:

```go
func (d *DB) SearchUsers(query string, limit int) ([]UserRow, error)
```

Performs a case-insensitive substring match on `users.name` and
`users.email` where `users.active = true`. Returns at most `limit`
rows ordered by `name ASC`. The query uses `LOWER(name) LIKE '%' ||
LOWER(?) || '%' OR LOWER(email) LIKE '%' || LOWER(?) || '%'`.

Add a UI fragment route `GET /ui/users/search?q={query}` that returns
an HTML list of matching users. Accessible to any authenticated user
who has `CanUpdateConfig()` relation on at least one app (i.e. app
owners, collaborators, and admins). Returns at most 10 results. Each
result is a clickable `<div>` showing the user's display name and
email (dimmed), with `data-value` set to the user's `sub` for the
principal field:

```html
<div class="autocomplete-item" data-value="{{.Sub}}"
     onclick="selectAutocomplete(this)">
    <span class="autocomplete-name">{{.Name}}</span>
    <span class="autocomplete-email">{{.Email}}</span>
</div>
```

### Frontend

Replace the plain `<input name="principal">` with an autocomplete
wrapper:

1. On input (debounced ~300ms), `hx-get` the search endpoint.
2. Results render in a positioned dropdown below the input.
3. Clicking a result fills the input and closes the dropdown.
4. Keyboard navigation (arrow keys, Enter) for accessibility.

The existing htmx form submission for adding the grant remains
unchanged -- the autocomplete just helps fill in the principal value.

## 5. Field Save UX

The current pattern -- edit a field, a green checkmark appears, click
it to save -- is ambiguous. The checkmark suggests the value is already
saved.

### Explicit Save with Floppy Icon

Replace the green checkmark (`&#10003;`) with an inline SVG floppy
disk icon (Lucide `save`, 16px). The icon communicates "click to
persist" rather than "already persisted."

States:

- **Hidden**: when the field value matches `data-original` (no
  changes). The existing `toggleSaveBtn()` logic stays as-is.
- **Unsaved** (visible, needs click): neutral color
  (`--text-secondary`, gray). The icon appears as soon as the value
  differs from `data-original`.
- **Saving** (request in flight): replace the icon with a small
  CSS-only spinner (16px).
- **Saved** (success): icon turns `--btn-success` (green) for 2
  seconds, then hides as `data-original` is updated to the new value.
- **Error**: `.field-feedback` shows the error message in
  `--flash-error-text`.

The access-type dropdown continues to auto-save on change (no save
button needed for selects — the act of choosing is the intent). Text
inputs and number inputs all use the explicit floppy icon pattern.

## 6. Full IDs on Hover

Wherever truncated IDs appear (worker IDs, bundle IDs), add a `title`
attribute with the full ID so the browser shows it on hover:

```html
<td class="monospace" title="{{.ID}}">{{.ID | truncate}}</td>
```

This is a one-line change per template location. Affected templates:
`tab_runtime.html`, `tab_logs.html`, `tab_logs_worker.html`,
`tab_bundles.html`.

## 7. Worker Detail View

Add a detail panel for individual workers in the Runtime tab. Clicking
a worker row replaces the worker table with a detail panel (the table
is swapped out, not overlaid). A "Back to workers" link at the top
returns to the table view.

The detail panel shows:

- Full worker ID with a copy-to-clipboard button (uses
  `navigator.clipboard.writeText()`, falls back to a hidden input +
  `execCommand('copy')`)
- Status badge and uptime (duration since container start, rendered
  as "2h 15m" style)
- CPU % and memory usage (same data as the table row, but with
  larger/clearer presentation)
- Connected sessions list: each entry shows user display name,
  session start time (relative), and session ID. Styled as a compact
  table or list.

Implementation: a new fragment route
`GET /ui/apps/{name}/tab/runtime/worker/{id}` that returns the detail
HTML. The worker row gets an `hx-get` pointing to this route with
`hx-target="#tab-content"` to replace the full tab content. The back
link uses `hx-get` to re-fetch the runtime tab.

## 8. App Renaming

Pull app renaming forward from the v3 draft. The v3 draft proposed a
drain-and-redirect mechanism, but a simpler rolling approach works
because workers are keyed by app ID internally -- they don't know the
app name. Both old and new names can resolve to the same app ID
simultaneously, so existing sessions are never disrupted.

### Database

Add an `app_aliases` table. Migration `007_app_aliases` for both
SQLite and PostgreSQL:

```sql
CREATE TABLE app_aliases (
    app_id      TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    name        TEXT NOT NULL UNIQUE,
    phase       TEXT NOT NULL CHECK (phase IN ('alias', 'redirect')),
    expires_at  TEXT NOT NULL
);
CREATE INDEX idx_app_aliases_app_id ON app_aliases(app_id);
```

The `expires_at` column uses TEXT with ISO 8601 format (consistent
with all other timestamp columns in the schema).

On rename, within a single transaction:
1. Validate the new name (same rules as app creation: 1-63 chars,
   lowercase letters/digits/hyphens, starts with letter, no trailing
   hyphen).
2. Check uniqueness against both `apps.name` and `app_aliases.name`
   where `phase = 'alias'` (redirect-phase entries do not block).
3. Insert the old name into `app_aliases` with `phase = 'alias'` and
   `expires_at = now + 2h`.
4. Update `apps.name` to the new name.
5. Update `apps.updated_at`.

New DB methods:

```go
func (d *DB) RenameApp(id, oldName, newName string) error
func (d *DB) GetAppByAlias(name string) (*AppRow, string, error)
    // returns (app, phase, err) — phase is "alias" or "redirect"
func (d *DB) TransitionExpiredAliases() error
func (d *DB) CleanupExpiredRedirects() error
```

### Two-Phase TTL

**Alias phase (2h):** The old name resolves directly to the app.
Existing session cookies (path-scoped to `/app/old-name/`) continue
to work. After 2h all active sessions have naturally idled out
(session idle TTL is 1h, so 2h provides comfortable margin).

**Redirect phase (7d):** After the alias expires, the row transitions
to `phase = 'redirect'`. Requests to `/app/old-name/*` receive a 301
to `/app/new-name/*`. This covers bookmarks and shared links. After
7d the row is deleted and the name is fully released.

### Name Claiming

If a new app is created with a name that is currently in the redirect
phase, the alias row is deleted and the new app takes the name. The
redirect is a courtesy, not a guarantee. Alias-phase entries (active
sessions depend on them) block creation unconditionally.

Resolve order in the proxy: `apps.name` first (always wins), then
`app_aliases`. Once a real app claims the name, it takes precedence
even before the alias row is cleaned up.

### Proxy Integration

Extend the proxy resolve path (`proxy.go`) to add a third fallback
after the existing `GetAppByName`:

```
1. GetApp(name)          — try as UUID (existing)
2. GetAppByName(name)    — try as canonical name (existing)
3. GetAppByAlias(name)   — try as alias/redirect (new)
```

If step 3 returns `phase = "redirect"`, respond with 301 to
`/app/{canonical-name}/{rest-of-path}`. If `phase = "alias"`,
continue to proxy as normal (the alias resolves to the app ID).

### Chained Renames

If app A is renamed A→B, then B→C within the alias window, two alias
rows exist (A and B) each with independent TTLs, both pointing to the
same app ID. This works without special handling.

### API & UI

Add `Name *string` to `AppUpdate` (in both `db.AppUpdate` and the
`updateAppRequest` in the API handler). The Settings tab gets a name
field in the **Danger Zone** section (alongside delete), not in the
metadata section — renaming affects URLs, bookmarks, and other users'
access, so it should feel deliberate. The field has an explicit
"Rename" button (not the floppy icon pattern from Item 5). Clicking
"Rename" shows an `hx-confirm` dialog: "Rename {old} to {new}? The
old name will redirect for 7 days." Same validation as app creation.
Renaming requires owner role (not just collaborator).

Add `audit.ActionAppRename` with details `{"old_name": "...",
"new_name": "..."}`, logged by the `UpdateApp` handler when the name
field changes.

### Phase Transitions & Cleanup

Add two calls to the autoscaler tick (`autoscaleTick` in
`autoscaler.go`), executed on each tick (~15s):

1. `TransitionExpiredAliases()` — UPDATE rows where
   `phase = 'alias' AND expires_at < now` to `phase = 'redirect'`
   and set `expires_at = now + 7d`.
2. `CleanupExpiredRedirects()` — DELETE rows where
   `phase = 'redirect' AND expires_at < now`.

These are cheap queries (the table will rarely have more than a few
rows) and the autoscaler tick is already the home for similar
lifecycle housekeeping.

## 9. Deployment Logs in History

Build logs are captured live during deployment (streamed to the task
store, available via the CLI) but discarded after the task completes.
Persist them so they can be reviewed later from the History page.

### Database

New `bundle_logs` table in migration `007_app_aliases` (same migration
file, or `008_bundle_logs` if preferred to keep them separate):

```sql
CREATE TABLE bundle_logs (
    bundle_id   TEXT PRIMARY KEY REFERENCES bundles(id) ON DELETE CASCADE,
    output      TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

One row per deployment. The `output` column stores the full combined
stdout/stderr from the build container (`BuildResult.Logs` from
`backend.Backend.Build()`).

### Capture

In the build pipeline (`internal/bundle/restore.go`), insert
`result.Logs` into `bundle_logs` immediately after `Backend.Build()`
returns, **before** the success/failure branch. This ensures failed
build logs are captured too:

```go
result, err := params.Backend.Build(ctx, buildOpts)
if err != nil { ... }

// Persist build log regardless of outcome.
_ = params.DB.InsertBundleLog(params.BundleID, result.Logs)

if !result.Success { ... }
```

New DB method:

```go
func (d *DB) InsertBundleLog(bundleID, output string) error
func (d *DB) GetBundleLog(bundleID string) (string, error)
```

### UI

Add an expand action to each row in the deployments history table.
Clicking a row loads the build log via a fragment route:

```
GET /ui/deployments/{bundle_id}/logs
```

The fragment returns a `<pre>` block with the log output, styled
consistently with the worker log viewer (`.log-output` class). Swap
into a detail row below the clicked entry using htmx `hx-target` on
a sibling `<tr class="log-detail-row">`. Toggle behavior: if the
detail row is already visible, clicking the same entry removes it
(JS checks for existing detail row and removes before fetching, or
htmx `hx-swap="innerHTML"` on the existing row clears it).

For deployments that have no log row (pre-existing bundles from before
this feature), the handler returns a `<p class="empty-state">No build
log available</p>` message.

### Size Considerations

Build logs are typically a few KB (package install output). For large
dependency trees they can reach low hundreds of KB. No truncation
needed at the DB level. The UI fragment can cap display at a
reasonable height with CSS overflow-y scroll, matching the worker log
viewer pattern.

## 10. Tag Browser

Replace the tag filter `<select>` dropdown on the Apps page with a
tag browser chip bar. Serves two purposes: filtering apps by tag, and
global tag management (rename, delete).

### Tag Count Data

The current `AllTags` field is `[]db.TagRow` which has no app count.
Add a new query/type to provide counts:

```go
type TagWithCount struct {
    TagRow
    AppCount int
}

func (d *DB) ListTagsWithCounts() ([]TagWithCount, error)
```

Query:

```sql
SELECT t.id, t.name, t.created_at, COUNT(at.app_id) AS app_count
FROM tags t
LEFT JOIN app_tags at ON t.id = at.tag_id
LEFT JOIN apps ON at.app_id = apps.id AND apps.deleted_at IS NULL
GROUP BY t.id, t.name, t.created_at
ORDER BY t.name
```

Update `appsPageData.AllTags` from `[]db.TagRow` to
`[]db.TagWithCount`.

### Filtering

Display all tags as horizontal chips above the app grid, each showing
the tag name and app count (as a small badge/suffix). Clicking a chip
toggles it as an active filter (highlighted state with `--accent` bg
and white text). Multiple chips can be active simultaneously for AND
filtering (show apps matching all selected tags). Clicking an active
chip deselects it.

The tag chips on individual app cards are also clickable -- clicking
a tag on a card activates that tag in the browser bar, providing a
second intuitive entry point to filtering.

Filter state is reflected in the URL query string
(`?tag=foo&tag=bar&tag_mode=and`) so filtered views are linkable and
survive page reloads.

When multiple tags are active, a small AND/OR toggle appears inline
after the active chips (a pill-shaped toggle or linked text:
`AND | OR` with the active mode highlighted). Defaults to AND.
Clicking the toggle re-fetches the page with the updated `tag_mode`
param. The toggle is hidden when fewer than 2 tags are selected
(irrelevant with a single tag).

Chip bar wraps with `flex-wrap: wrap`. If more than 3 rows of tags
are visible, cap at `max-height: 6rem; overflow: hidden` with a
"Show all (N)" toggle link that removes the cap.

### Multi-Tag Query

Change `CatalogParams.Tag string` to `Tags []string` and add
`TagMode string` (values `"and"` / `"or"`, default `"and"`).

**AND mode** — apps matching all selected tags:

```sql
AND apps.id IN (
    SELECT at2.app_id FROM app_tags at2
    JOIN tags t2 ON at2.tag_id = t2.id
    WHERE t2.name IN (?, ?, ...)
    GROUP BY at2.app_id
    HAVING COUNT(DISTINCT t2.id) = ?
)
```

Where the final `?` is `len(tags)`.

**OR mode** — apps matching any selected tag:

```sql
AND apps.id IN (
    SELECT at2.app_id FROM app_tags at2
    JOIN tags t2 ON at2.tag_id = t2.id
    WHERE t2.name IN (?, ?, ...)
)
```

### Global Tag Management

Add a small gear icon (Lucide `settings`) to the tag browser bar that
toggles management mode. In this mode, each tag chip gains:

- A delete button (Lucide `x`, 14px) that removes the tag globally
  (from all apps). Uses `hx-delete` to
  `DELETE /api/v1/tags/{id}` with `hx-confirm` since this is
  destructive.
- A click-to-rename interaction: clicking the tag name in management
  mode makes it editable inline (replace text node with a small
  `<input>` pre-filled with the tag name). On blur or Enter,
  `hx-patch` saves the new name to `PATCH /api/v1/tags/{id}`.

Management mode is only visible to admin/publisher users
(`CanManageTags()`). Regular users see the tag browser in filter-only
mode. The `appsPageData` already includes `UserRole`; the template
checks this to show/hide the gear icon.

### Tag Rename Endpoint

New API endpoint (does not currently exist):

```
PATCH /api/v1/tags/{tagID}
```

Request body: `{"name": "new-name"}`. Same validation rules as tag
creation (1-63 chars, lowercase, hyphens, etc.). Returns 409 on name
conflict.

New DB method:

```go
func (d *DB) RenameTag(id, newName string) error
```

Handler requires `CanManageTags()` permission (admin/publisher).
Emits `HX-Trigger: showToast` header on success for toast feedback.

### Relation to Existing Tag UX

Tag operations at three levels:

1. **Tag ↔ app association** (add/remove a tag from one app): remains
   in the Settings tab sidebar, unchanged.
2. **Tag filtering** (browse apps by tag): the new chip bar, replaces
   the `<select>` dropdown.
3. **Global tag operations** (rename/delete a tag across all apps):
   management mode in the chip bar.

## 11. Toast Notifications

Add a lightweight toast notification system for app-level action
feedback. Currently, per-field actions use `.field-feedback` spans,
but broader actions (delete app, rollback bundle, enable/disable,
grant access) have no consistent success/error indicator beyond the
browser confirm dialog disappearing.

### Design

A fixed container in the bottom-right corner (`#toast-container`)
holds toast elements. Each toast has a message, a type (success,
error, info), and auto-dismisses after 4 seconds. A close button
allows manual dismissal. Max 5 visible toasts; oldest is removed when
a 6th arrives.

```html
<div id="toast-container" class="toast-container"></div>
```

Add `#toast-container` to `base.html` (outside `.main-content`, fixed
position).

### Integration

A global `showToast(message, type)` JS function creates and appends
toast elements. Hook into htmx events to trigger automatically:

- **Server-driven toasts**: The server includes an `HX-Trigger`
  response header with event name `showToast` and a JSON detail
  payload: `{"showToast": {"message": "App deleted", "type":
  "success"}}`. A global listener on `document` handles the event.

- **Error toasts**: A global `htmx:responseError` listener shows an
  error toast with the status code and a brief message, replacing the
  current silent failure behavior.

Handlers that should emit `HX-Trigger: showToast`:

| Handler            | Message                    | Type    |
|--------------------|----------------------------|---------|
| `DeleteApp`        | "App deleted"              | success |
| `RestoreApp`       | "App restored"             | success |
| `EnableApp`        | "App enabled"              | success |
| `DisableApp`       | "App disabled"             | success |
| `RollbackApp`      | "Rolled back to {id}"      | success |
| `AddAppTag`        | "Tag added"                | success |
| `RemoveAppTag`     | "Tag removed"              | success |
| `AddAccessGrant`   | "Access granted"           | success |
| `RemoveAccessGrant`| "Access revoked"           | success |
| `DeleteTag`        | "Tag deleted"              | success |
| `RenameTag`        | "Tag renamed"              | success |

Using `HX-Trigger` response headers keeps the logic server-driven:
the handler decides what toast to show, the client just renders it.

### Styling

Toasts slide in from the right (`@keyframes slideIn` from
`translateX(100%)` to `translateX(0)`), stack vertically (newest on
top via `flex-direction: column-reverse`), and fade out on dismiss
(`opacity` transition). Colors use the `--toast-*` CSS variables
defined in the color token table. Toast width: `min(360px, 90vw)`.
Border-radius 8px, subtle shadow.

## 12. Session Expiry Handling

When the user's authentication session expires, htmx fragment
requests return 401 or redirect to the login page. Currently this
swaps broken HTML into the DOM with no indication of what happened.

### Detection

Add a global `htmx:beforeSwap` listener that checks the response
status. On 401, or when `evt.detail.xhr.responseURL` contains the
login path (redirect detection), prevent the default swap
(`evt.detail.shouldSwap = false`) and show a full-screen overlay:

```html
<div id="session-expired-overlay" class="session-overlay hidden">
    <div class="session-overlay-card">
        <p>Your session has expired.</p>
        <a href="/auth/login" class="btn">Sign In</a>
    </div>
</div>
```

Add the overlay to `base.html`. The overlay uses
`position: fixed; inset: 0; z-index: 9999` with a
`background: rgba(0,0,0,0.5)` backdrop. The card is centered
(`display: grid; place-items: center`). Once shown, the overlay is
never hidden — the user must click "Sign In" to re-authenticate.

403 responses are NOT treated as session expiry — those indicate
insufficient permissions on a valid session and are handled by
the existing error swap or toast system.

### Full-Page Requests

For full-page navigations (non-htmx), the server already redirects
to login. No change needed there.

## 13. Live Relative Timestamps

The `timeAgo` values ("3 minutes ago", "2 hours ago") are rendered
server-side and become stale if the page stays open. Add a small
client-side script that re-renders them periodically.

### Implementation

Render a `data-timestamp` attribute alongside each `timeAgo` output.
The timestamp value is the raw RFC 3339 string (same format stored in
the database and emitted by the Go templates):

```html
<span class="time-ago" data-timestamp="{{.DeployedAt}}">
    {{.DeployedAt | timeAgo}}
</span>
```

A JS function scans all `.time-ago` elements and recalculates the
relative text from the `data-timestamp` value. Run on a 30-second
`setInterval`. The relative time logic must match the server-side
`timeAgo` output format defined in `ui.go:53-96`:

| Condition               | Output             |
|-------------------------|--------------------|
| < 60 seconds            | "just now"         |
| < 60 minutes            | "{n} minutes ago"  |
| < 24 hours              | "{n} hours ago"    |
| < 48 hours              | "yesterday"        |
| otherwise               | "{n} days ago"     |

(With "1 minute ago" / "1 hour ago" / "1 day ago" singular forms.)

Re-run the scan after each `htmx:afterSwap` event so dynamically
loaded content (tab switches, sidebar opens) also gets live
timestamps.

This covers the apps grid, deployment history, runtime tab, bundles
tab, and log viewer timestamps.

## 14. Table Sorting

Add clickable column headers to the deployment history table and the
apps grid for client-triggered server-side sorting.

### Deployment History

Sortable columns with whitelist mapping:

| UI Header   | Query Value   | SQL Column        | Default |
|-------------|---------------|-------------------|---------|
| App         | `app_name`    | `apps.name`       |         |
| Deployed By | `deployed_by` | `u.name`          |         |
| Date        | `date`        | `b.deployed_at`   | DESC    |
| Status      | `status`      | `b.status`        |         |

Clicking a header adds `?sort=app_name&dir=asc` query params.
Clicking the active sort column toggles direction. The column header
shows a small arrow indicator (Lucide `chevron-up` / `chevron-down`,
12px inline) for the active sort.

The handler reads `sort` and `dir` from the query string, validates
against the whitelist map, and falls back to `date DESC` for unknown
values. The `ListDeployments()` query's `ORDER BY` clause becomes
parameterized:

```go
var sortColumns = map[string]string{
    "app_name":    "apps.name",
    "deployed_by": "u.name",
    "date":        "b.deployed_at",
    "status":      "b.status",
}
```

Add a secondary sort on `b.id DESC` for stable ordering when the
primary column has ties.

### Apps Grid

Sortable by name, status, last deployed. The grid layout doesn't
have column headers, so add a small "Sort by" control above the grid
(inline with the tag browser bar, right-aligned) as a set of linked
options:

```
Sort by: Name | Status | Last deployed ▼
```

Whitelist mapping:

| UI Label       | Query Value      | SQL Expression          | Default |
|----------------|------------------|-------------------------|---------|
| Name           | `name`           | `apps.name`             |         |
| Status         | `status`         | `apps.enabled`          |         |
| Last deployed  | `last_deployed`  | `apps.updated_at`       | DESC    |

Uses `apps.updated_at` as a proxy for last-deployed time (it is
updated on each deployment). The active sort option gets a bold style
and direction arrow.

## 15. Tag Creation from Settings Tab (Bug Fix)

The tag add form in the Settings tab (`tab_settings.html`) only shows
a `<select>` of existing unassigned tags. If no tags exist in the
system, or all tags are already assigned to the app, the form
disappears entirely. There is no way to create a new tag from the UI
-- the `POST /api/v1/tags` endpoint exists but is only reachable via
the API.

### Fix

Replace the select-only form with a combo input that supports both
selecting an existing tag and creating a new one:

1. A text input with autocomplete. Typing filters the list of
   available (unassigned) tags rendered as a dropdown below the input.
   The available tags are already computed server-side in
   `settingsTabData` (all tags minus app's current tags).

2. If the typed value matches an existing unassigned tag, submit
   assigns it via `POST /api/v1/apps/{id}/tags` as today.

3. If the typed value is new, the UI handler chains two calls:
   first `POST /api/v1/tags` to create, then
   `POST /api/v1/apps/{id}/tags` to assign. This uses the existing
   API endpoints and avoids a new backend endpoint. The UI handler
   (`sidebar.go`) performs both calls server-side and returns the
   refreshed settings tab fragment.

   New UI route: `POST /ui/apps/{name}/tags` that accepts
   `{name: "tag-name"}`, creates if needed, assigns, and returns the
   updated settings tab HTML.

The form should always be visible regardless of whether unassigned
tags exist. The autocomplete dropdown shows available tags; an empty
dropdown with typed text shows a "Create '{value}'" option.

Tag creation requires admin/publisher role (`CanManageTags()`). For
non-admin users (app owners, collaborators), the input only allows
selecting from existing tags. If no matching tags exist, the dropdown
shows "No matching tags" instead of a create option. The template
checks the caller's role (already available as `UserRole` in
sidebar/tab data) to control this behavior.

## 16. Additional Polish (Candidates)

Smaller items to consider during implementation:

- **Empty states**: Show helpful messages when tables are empty (no
  workers, no bundles, no collaborators) instead of blank space.
- **Loading indicators**: Add `htmx-indicator` spinners on tab switches
  and form submissions for visual feedback.
- **Confirmation dialogs**: The delete/remove actions use `hx-confirm`
  with browser dialogs. Consider styled modals for consistency.
- **Responsive sidebar width**: The sidebar is fixed-width. On narrow
  viewports it may need to go full-width or be dismissable.

## Implementation Notes

Items 1, 2, 3, 5, and 6 are pure frontend (HTML/CSS/JS). Item 6 is
trivial. Items 4 and 7 require new fragment routes and handlers.

Item 8 (app renaming) is the most substantial addition -- it touches
the DB layer (new migration for `app_aliases`), the proxy resolve path
(`GetAppByAlias` as third fallback), the API (`AppUpdate.Name`), the
autoscaler (alias cleanup in `autoscaleTick`), and the Settings tab
UI. However, the rolling two-phase approach avoids the
drain-and-redirect complexity originally scoped for v3. All rename
steps (uniqueness check, alias insert, name update) execute in a
single DB transaction.

Item 4 (autocomplete) needs a `SearchUsers(query, limit)` DB method
(case-insensitive substring match on `name` and `email`, active users
only) plus a new UI fragment route.

Item 9 (deployment logs) requires a new migration, a single INSERT in
the build pipeline (after `Backend.Build()` returns, before the
success/failure branch), and one fragment route + template. Minimal
blast radius.

Item 10 (tag browser) needs: `ListTagsWithCounts()` DB method for the
chip bar data, `Tags []string` in `CatalogParams` with
`HAVING COUNT(DISTINCT ...) = ?` SQL for multi-tag AND filtering,
and a new `PATCH /api/v1/tags/{id}` endpoint for rename (delete
already exists).

Items 11-14 are all pure frontend (JS/CSS/templates) with minor
backend touches. Item 11 (toasts) uses `HX-Trigger` response headers
-- the handler table above lists all handlers that need the addition.
Item 12 (session expiry) is a global JS listener, no backend changes.
Item 13 (live timestamps) needs `data-timestamp` attributes added to
templates and a ~20-line JS function matching the Go `timeAgo` output.
Item 14 (sorting) needs parameterized ORDER BY in existing DB queries
with a strict column whitelist.

Item 15 (tag creation) is a bug fix. The new
`POST /ui/apps/{name}/tags` handler chains existing API calls
(create + assign) server-side, avoiding a new backend endpoint.
