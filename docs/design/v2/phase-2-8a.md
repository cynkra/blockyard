# Phase 2-8a: Backend Prerequisites for Web UI

Schema changes, session tracking, and API endpoints required before the
UI expansion in phase 2-8b can be built.

Depends on phases 2-2 (rollback, soft-delete) and 2-7 (refresh API).

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

## Deliverables

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

### Step 6: Tests

Integration tests for session lifecycle (create → end, create → crash).
API tests for deployments listing, session listing, session log
retrieval. Verify role-based filtering.
