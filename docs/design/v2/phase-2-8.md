# Phase 2-8: Backend Prerequisites

Backend schema, APIs, RBAC changes, and shared infrastructure.
Establishes the full server-side API surface that the CLI (phase 2-9),
navigation UI (phase 2-10), and per-app sidebar (phase 2-11) consume.

Depends on phases 2-2 (rollback, soft-delete), 2-3 (pre-warming config),
2-5 (manifest types), and 2-7 (refresh API).

Content filtering (search + tag) is already implemented in the
dashboard — this phase does not revisit it.

---

## Backend

### Sessions Table

A new `sessions` table tracks the full chain: **user -> app -> worker ->
session -> logs**. This enables activity metrics (phase 2-11 Overview
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

1. **Activity metrics** -- derived from session records:
   - Total views: `COUNT(*) WHERE app_id = ?`
   - Last 7 days: `COUNT(*) WHERE app_id = ? AND started_at >= ?`
   - Unique visitors: `COUNT(DISTINCT user_sub) WHERE app_id = ?`
   - Avg session duration: `AVG(ended_at - started_at) WHERE ended_at IS NOT NULL`
2. **Runtime data** -- live session-to-user-to-worker mapping.
3. **Debugging** -- multiple lookup paths:
   - By user: "alice had a crash" -> filter by `user_sub`
   - By worker: "worker is misbehaving" -> filter by `worker_id`
   - By status: "what crashed recently?" -> filter by `status = 'crashed'`

No separate `app_views` counter table is needed -- all activity metrics
are derived from sessions.

**Future: per-session log filtering.** The sessions table maps each
session to a worker. Logs are currently captured per-worker (Docker
container stdout/stderr). When `max_sessions_per_worker = 1`, worker
logs are effectively session logs. For shared workers, log lines from
multiple sessions are interleaved -- the same trade-off Posit Connect
makes. Per-session log annotation (tagging log lines with session
tokens at the R level) is deferred to a future phase.

### Bundle Schema Changes

Add deployment tracking and dependency mode columns to the existing
`bundles` table:

```sql
ALTER TABLE bundles ADD COLUMN deployed_by TEXT;
ALTER TABLE bundles ADD COLUMN deployed_at TEXT;
ALTER TABLE bundles ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
```

- `deployed_by` -- user_sub of the person who triggered the deployment.
  Set at bundle creation time in `UploadBundle()`, since the async
  restore goroutine does not have access to the caller context. For
  rollbacks, set to the caller who triggered the rollback.
- `deployed_at` -- timestamp of bundle activation (distinct from
  `uploaded_at` which records the upload time before build). Set in
  `ActivateBundle()` when the build completes.
- `pinned` -- whether the bundle was deployed with pinned dependencies
  (1) or unpinned (0). Set at bundle creation time based on whether the
  manifest includes a `packages` section. This is static over the
  bundle's lifetime. The app-level "is pinned" check (used by the UI's
  refresh button and the CLI's `by refresh` error) is derived from the
  active bundle's `pinned` value.

**Migration:** For existing bundles with status "ready", set
`deployed_at = uploaded_at` and `deployed_by` to the app owner.
Bundles with status "pending" or "failed" get NULL for both. For
`pinned`, set to 1 if the bundle has an `renv.lock` or a manifest
with packages, 0 otherwise (default 0 is safe for existing bundles
since the feature is new).

**Implementation detail:** `deployed_by` is stored on the bundle row
at INSERT time (in `CreateBundle`), passed from `UploadBundle` which
has access to `auth.CallerFromContext()`. `deployed_at` is set later
by `ActivateBundle` when the restore completes. For rollbacks, both
fields are set atomically in the rollback handler. `pinned` is set
at INSERT time based on the uploaded manifest.

### App Enable/Disable

New `enabled` column on the `apps` table:

```sql
ALTER TABLE apps ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;
```

When `enabled = 0`:

- Proxy does not cold-start new workers for the app.
- Autoscaler does not pre-warm standby seats.
- Existing sessions drain naturally (workers stay alive until idle).
- New requests get 503 Service Unavailable.

This is persistent state that survives server restarts. The proxy
cold-start path (`proxy.go`, where a new worker is spawned on incoming
request) checks `app.Enabled` before proceeding. The autoscaler
pre-warming loop (`ops/autoscale.go`) skips disabled apps.

**Integration points:**

- `proxy.go` -- before cold-starting a worker, check `app.Enabled`.
  Return 503 if disabled.
- `ops/autoscale.go` -- skip pre-warming for apps where `enabled = 0`.
- `api/apps.go` -- new `EnableApp` and `DisableApp` handlers.

### Hard Delete

Admin-only permanent app removal. Requires the app to be soft-deleted
first (via `DELETE /api/v1/apps/{id}`).

Permanently removes the app record and all associated data (bundles,
sessions, access grants, workers). Cannot be undone.

If the app is not already soft-deleted, returns 409 Conflict with a
message requiring soft-delete first.

### Session Lifecycle Tracking

The proxy layer must create and update session records as it routes
requests to workers.

#### Session Creation

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

#### Session End

When a session ends normally (idle timeout eviction in
`ops/evict.go`, or explicit stop in `api/apps.go` StopApp):

```go
db.EndSession(sessionID, "ended")
```

**Integration points:**
- `ops/evict.go` -- when evicting a worker, end all its sessions.
- `api/apps.go` StopApp -- when stopping an app, end all sessions
  for all workers of that app.
- `session.Store` cleanup -- when the in-memory session store
  expires an entry, end the corresponding DB session.

#### Session Crash

When a worker crashes or is killed (detected by health polling in
`ops/health.go`), all its active sessions are marked:

```go
db.CrashWorkerSessions(workerID)
// UPDATE sessions SET status = 'crashed', ended_at = NOW()
// WHERE worker_id = ? AND status = 'active'
```

### API Changes

#### API Split: GetApp vs GetAppRuntime

The current `GET /api/v1/apps/{id}` returns operational details
(worker list) to any user with access. Under the new RBAC model,
viewers should only see app metadata.

**`GET /api/v1/apps/{id}`** -- any access. Returns app metadata only:

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
  "enabled": true,
  "created_at": "...",
  "updated_at": "...",
  "status": "running"
}
```

The `workers` field is **removed** from this response. The `enabled`
field is added. The `relation` field is added (see below).

**`GET /api/v1/apps`** -- list apps. Each app in the response includes
a `relation` field indicating the caller's access level (`"viewer"`,
`"collaborator"`, `"owner"`, `"admin"`). This is computed during the
existing access-filtering query (which already evaluates access) and
avoids N+1 lookups when the UI needs to know per-app permissions (e.g.,
whether to show the gear icon on each app card in phase 2-10).

**`GET /api/v1/apps/{id}/runtime`** -- collaborator+ only. New
endpoint returning live operational data:

```json
{
  "workers": [
    {
      "id": "w-a3f2...",
      "bundle_id": "01ABC...",
      "status": "active",
      "started_at": "2026-03-26T10:55:00Z",
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

The runtime endpoint also needs worker start time. Extend
`server.ActiveWorker` to include `StartedAt time.Time`. Set this in
`StartApp()` and in the proxy cold-start path when a new worker is
spawned. The Logs tab (phase 2-11) uses worker start time to show
"Since" in the worker list.

#### Logs Stream Parameter

`GET /api/v1/apps/{id}/logs` gains an optional `stream` query
parameter (default `true`). When `stream=false`, the endpoint writes
the historical log snapshot and closes the response immediately
without subscribing to live updates.

Phase 2-11's Logs tab uses this to pre-fill historical logs in the
worker log viewer fragment (`stream=false`), with live streaming
handled separately via JS `fetch()` with `stream=true`.

#### htmx Content Negotiation

The form-encoded PATCH (below) is one instance of a general pattern:
when a request includes the `HX-Request` header, action endpoints
return an `HX-Trigger` response header naming the event that occurred
(e.g., `appStarted`, `accessGranted`) alongside the normal JSON body.
htmx listeners on the page re-fetch the relevant fragment in response.

This avoids having every action endpoint render HTML. The UI fragment
routes (phase 2-11) listen for the triggered events and re-fetch the
affected tab content. The specific event names are defined when the
fragment routes are registered.

Endpoints where this applies: `POST .../start`, `POST .../stop`,
`POST .../rollback`, `POST .../access`, `POST .../refresh`.
For `DELETE` actions (access revoke, app delete, token revoke) the
existing 204 empty response works directly with htmx's
`hx-swap="outerHTML"` to remove the target element.

#### RBAC Tightening

The following existing endpoints need stricter authorization:

| Endpoint | Current | New |
|----------|---------|-----|
| `GET /api/v1/apps/{id}/bundles` | any access | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/refresh` | any access | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/refresh/rollback` | any access | collaborator+ (`CanDeploy`) |

#### App Rename

`PATCH /api/v1/apps/{id}` with `name` field -- owner+ (`CanDelete`).

Renaming changes the app's URL (`/app/{name}/`), so it requires owner+
rather than collaborator+. The existing `validateAppName()` rules
apply. Returns 409 if the new name conflicts with another live app.

The `updateAppRequest` gains an optional `Name` field. The handler
checks owner+ permission when `name` is present (same as `access_type`
which checks `CanManageACL`). The DB layer adds `Name` to `AppUpdate`
and includes it in the UPDATE query.

#### Enable/Disable API

```
POST /api/v1/apps/{id}/enable   -- collaborator+ (CanStartStop)
POST /api/v1/apps/{id}/disable  -- collaborator+ (CanStartStop)
```

Both return the updated app metadata (same shape as `GET /api/v1/apps/{id}`).
`enable` sets `enabled = 1`; `disable` sets `enabled = 0`.

#### Hard Delete API

```
DELETE /api/v1/apps/{id}?purge=true  -- admin only
```

Extends the existing soft-delete endpoint. When `purge=true`:
- If the app is not already soft-deleted, returns 409.
- If the app is soft-deleted, permanently removes all data.
- Only admins can purge; owners can only soft-delete.

Without `?purge`, behavior is unchanged (soft-delete, owner+).

#### Deployments API

`GET /api/v1/deployments` -- collaborator+ per-app.

Cross-app deployment listing. Queries bundles joined with apps and
the users table (for display names). Results are filtered to apps
where the caller has collaborator+ access (viewers are excluded).

Query parameters:
- `page` (int, default 1)
- `per_page` (int, default 25, max 100)
- `search` (string, optional -- filters by app name)
- `status` (string, optional -- filters by bundle status)

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
      "deployed_by_name": "Alice Chen",
      "deployed_at": "2026-03-26T10:00:00Z",
      "status": "ready"
    }
  ],
  "total": 42,
  "page": 1,
  "per_page": 25
}
```

#### Sessions API

`GET /api/v1/apps/{id}/sessions` -- collaborator+.

List sessions for an app, most recent first. Default: last 50 sessions.

Query parameters:
- `user` (string, optional -- filter by user_sub)
- `status` (string, optional -- filter by status)
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

#### Form-Encoded PATCH

`PATCH /api/v1/apps/{id}` is extended to accept
`application/x-www-form-urlencoded` in addition to JSON. When the
request includes an `HX-Request` header (htmx), the response is an
HTML fragment (success indicator or validation error) instead of JSON.
For non-htmx requests, behavior is unchanged.

This dual-format support enables the per-app sidebar (phase 2-11) to
use `hx-patch` for inline field editing without client-side JSON
serialization.

### Shared App Resolution

`resolveApp()` and `resolveAppRelation()` are currently in
`internal/api/apps.go`. Both the API and UI handlers need to resolve
an app by name or UUID and evaluate the caller's access level.

Extract these to a shared location (`internal/db/` for `resolveApp`,
`internal/server/` for `resolveAppRelation`) so UI fragment routes
(phase 2-11) can reuse them without importing the API package.

### Database Operations

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

// Deployment tracking (DeploymentRow includes deployed_by_name from users join)
SetBundleDeployed(bundleID, deployedBy string) error
ListDeployments(opts DeploymentListOpts) ([]DeploymentRow, int, error)

// Enable/disable
SetAppEnabled(appID string, enabled bool) error

// Hard delete
PurgeApp(appID string) error
// Deletes app + bundles + sessions + access grants + workers.
// Caller must verify app is already soft-deleted.

// Collaborator display names (for phase 2-11 Collaborators tab)
ListAppAccessWithNames(appID string) ([]AccessGrantWithName, error)
// Joins app_access with users table to get display names.
// Falls back to raw principal (OIDC subject) when no user row exists.
```

### Authorization Model

#### RBAC Rules

Viewers can only see app metadata via `GET /api/v1/apps/{id}` and
view the running app via `/app/{name}/`. All management endpoints
(runtime, bundles, refresh, enable/disable, config changes) require
collaborator+ access. Destructive operations (soft-delete) require
owner+. Hard delete (purge) requires admin.

#### API Authorization Table

| Endpoint | Required relation |
|----------|------------------|
| `GET /api/v1/apps/{id}` | any access (metadata only, no workers) |
| `GET /api/v1/apps/{id}/runtime` | collaborator+ |
| `PATCH /api/v1/apps/{id}` | collaborator+ (`CanUpdateConfig`); `name` field requires owner+ (`CanDelete`) |
| `DELETE /api/v1/apps/{id}` | owner+ (`CanDelete`) |
| `DELETE /api/v1/apps/{id}?purge=true` | admin only |
| `GET /api/v1/apps/{id}/bundles` | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/bundles` | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/rollback` | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/enable` | collaborator+ (`CanStartStop`) |
| `POST /api/v1/apps/{id}/disable` | collaborator+ (`CanStartStop`) |
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

---

## Deliverables

1. **Database migration** -- sessions table, bundle schema additions
   (`deployed_by`, `deployed_at`, `pinned`), `enabled` column, indexes.
2. **Session lifecycle** -- create/end/crash tracking in the proxy
   and worker lifecycle code.
3. **Bundle deployment tracking** -- populate `deployed_by` at upload
   time, `deployed_at` at activation time, `pinned` at creation time.
4. **Backend interface** -- add `ContainerStats()` method for live
   CPU/memory data.
5. **API split** -- remove workers from `GetApp`, add per-app `relation`
   to list response, add `GET /api/v1/apps/{id}/runtime` (collaborator+).
6. **RBAC tightening** -- `ListBundles`, `PostRefresh`,
   `PostRefreshRollback` require collaborator+.
7. **Enable/disable** -- `POST /apps/{id}/enable` and
   `POST /apps/{id}/disable` endpoints, proxy and autoscaler checks.
8. **Hard delete** -- `DELETE /apps/{id}?purge=true` endpoint
   (admin only, requires prior soft-delete).
9. **Deployments API** -- `GET /api/v1/deployments` with pagination,
   collaborator+ per-app filtering, and `deployed_by_name` from users
   table join.
10. **Sessions API** -- `GET /api/v1/apps/{id}/sessions` with filtering.
11. **Shared app resolution** -- extract `resolveApp()` and
    `resolveAppRelation()` to shared location for reuse by UI handlers.
12. **Form-encoded PATCH** -- `PATCH /api/v1/apps/{id}` accepts
    form-encoded bodies, returns HTML fragments for htmx requests.
13. **Collaborator display names** -- `ListAppAccessWithNames()` DB
    method joining `app_access` with `users`.
14. **App rename** -- add `name` field to `PATCH /api/v1/apps/{id}`
    with owner+ permission check and name validation/conflict handling.
15. **Worker metadata** -- extend `ActiveWorker` with `StartedAt` for
    the runtime API and logs tab.
16. **Logs stream parameter** -- `GET /api/v1/apps/{id}/logs` gains
    `stream` query parameter (default `true`); `stream=false` returns
    historical snapshot only.
17. **htmx event triggers** -- action endpoints return `HX-Trigger`
    response headers for htmx requests, enabling UI fragment re-fetch
    without HTML rendering in the API layer.

## Implementation Steps

### Step 1: Database Migration

Add sessions table, bundle columns (`deployed_by`, `deployed_at`,
`pinned`), and `enabled` column. Write up/down migration for both
SQLite and PostgreSQL.

### Step 2: Session Lifecycle Tracking

Instrument the proxy to create session records on assignment, end them
on disconnect/eviction, and crash them on worker failure. Integration
points: `proxy.go` (create), `ops/evict.go` (end), `api/apps.go`
StopApp (end), `ops/health.go` (crash).

### Step 3: Bundle Deployment Tracking

Store `deployed_by` and `pinned` at bundle INSERT time in
`UploadBundle()` (caller available from context). Set `deployed_at`
in `ActivateBundle()` when restore completes. Update rollback handler
similarly. Run backfill migration for existing bundles.

### Step 4: Backend Interface + API Changes

Add `ContainerStats()` to `Backend` interface. Implement in Docker
backend. Extend `ActiveWorker` with `StartedAt`. Add
`GET /api/v1/apps/{id}/runtime` endpoint. Remove workers from
`GetApp` response. Add per-app `relation` to list response. Tighten
RBAC on `ListBundles`, `PostRefresh`, `PostRefreshRollback`. Add
`stream` query parameter to `AppLogs`.

### Step 5: Enable/Disable + Hard Delete

Implement enable/disable endpoints. Add proxy cold-start check and
autoscaler skip for disabled apps. Implement hard-delete endpoint with
soft-delete precondition check.

### Step 6: Deployments + Sessions API

Implement `GET /api/v1/deployments` with collaborator+ per-app
filtering. Implement `GET /api/v1/apps/{id}/sessions`.

### Step 7: Shared Infrastructure

Extract `resolveApp()` and `resolveAppRelation()` to shared location.
Add form-encoded PATCH support to `UpdateApp` (dual JSON + form
encoding, htmx fragment responses). Add `name` to `AppUpdate` with
owner+ permission check and conflict detection. Add `HX-Trigger`
response headers to action endpoints for htmx requests. Implement
`ListAppAccessWithNames()` DB method.

### Step 8: Tests

Integration tests for session lifecycle (create -> end, create -> crash).
API tests for runtime endpoint, deployments listing, session listing.
RBAC tests verifying viewer cannot access runtime API, bundles listing,
or refresh endpoints. Enable/disable behavior tests. Hard-delete
precondition tests. Form-encoded PATCH tests. `ListAppAccessWithNames`
tests.
