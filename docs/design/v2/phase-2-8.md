# Phase 2-8: Backend Prerequisites + CLI

Backend schema, APIs, RBAC changes, and the `by` CLI binary.
Establishes the full server-side API surface and a command-line client
that wraps it. UI work (navigation, pages, sidebar) follows in phases
2-9 and 2-10.

Depends on phases 2-2 (rollback, soft-delete), 2-3 (pre-warming config),
2-5 (manifest types), and 2-7 (refresh API).

Content filtering (search + tag) is already implemented in the
dashboard — this phase does not revisit it.

---

## Backend

### Sessions Table

A new `sessions` table tracks the full chain: **user -> app -> worker ->
session -> logs**. This enables activity metrics (phase 2-10 Overview
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

Add deployment tracking columns to the existing `bundles` table:

```sql
ALTER TABLE bundles ADD COLUMN deployed_by TEXT;
ALTER TABLE bundles ADD COLUMN deployed_at TEXT;
```

- `deployed_by` -- user_sub of the person who triggered the deployment.
  Set at bundle creation time in `UploadBundle()`, since the async
  restore goroutine does not have access to the caller context. For
  rollbacks, set to the caller who triggered the rollback.
- `deployed_at` -- timestamp of bundle activation (distinct from
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
field is added.

**`GET /api/v1/apps/{id}/runtime`** -- collaborator+ only. New
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

#### RBAC Tightening

The following existing endpoints need stricter authorization:

| Endpoint | Current | New |
|----------|---------|-----|
| `GET /api/v1/apps/{id}/bundles` | any access | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/refresh` | any access | collaborator+ (`CanDeploy`) |
| `POST /api/v1/apps/{id}/refresh/rollback` | any access | collaborator+ (`CanDeploy`) |

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

Cross-app deployment listing. Queries bundles joined with apps.
Results are filtered to apps where the caller has collaborator+
access (viewers are excluded).

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

// Deployment tracking
SetBundleDeployed(bundleID, deployedBy string) error
ListDeployments(opts DeploymentListOpts) ([]DeploymentRow, int, error)

// Enable/disable
SetAppEnabled(appID string, enabled bool) error

// Hard delete
PurgeApp(appID string) error
// Deletes app + bundles + sessions + access grants + workers.
// Caller must verify app is already soft-deleted.
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
| `PATCH /api/v1/apps/{id}` | collaborator+ (`CanUpdateConfig`) |
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

## CLI

Design for the CLI binary (`cmd/by/`). The deploy command is the
primary new complexity; all other subcommands are thin REST API
wrappers.

See [dep-mgmt.md](../dep-mgmt.md) for the architectural overview that
drives the deploy flow.

### Prerequisites from Earlier Phases

- **Phase 2-5** -- `internal/manifest/` types, `FromRenvLock()` and
  `FromDescription()` conversion functions, manifest validation. The CLI
  imports these to generate manifests during `by deploy`.
- **Phase 2-6** -- store-aware builds on the server. No direct CLI
  dependency, but deploy benefits from fast builds.
- **Phase 2-7** -- `POST /api/v1/packages` and refresh API. The CLI
  wraps the refresh endpoint as `by refresh`.

### Authentication

`BLOCKYARD_TOKEN` environment variable (a PAT). `BLOCKYARD_URL`
environment variable (e.g., `https://blockyard.example.com`).

#### `by login`

A convenience command that lowers the barrier for first-time users:

1. Prompt for the server URL (or accept `--server URL`).
2. Open the browser to `{server}/profile#tokens` (the PAT section on
   the Profile page).
3. Prompt the user to paste the token.
4. Store credentials in `~/.config/by/config.json` (XDG-compliant).

```
$ by login
Server URL: https://blockyard.example.com
Opening browser to create a token...
Paste your token: ****
Logged in to blockyard.example.com as alice.
```

The env vars `BLOCKYARD_TOKEN` and `BLOCKYARD_URL` always take precedence
over the stored config -- CI pipelines use env vars, interactive users use
`by login`. The config file stores a single server entry; multi-server
profiles are a future extension if demand arises.

### Output Format

All commands default to human-readable output (tables, formatted text).
A global `--json` flag switches to machine-readable JSON output -- useful
for scripting and CI pipelines.

For thin API wrappers, `--json` passes through the API response body
directly. For commands with client-side logic (deploy, refresh), `--json`
emits a structured JSON object on completion instead of streaming
progress text.

### Deploy Flow

The `by deploy` command prepares a bundle and uploads it. From the user's
perspective, two choices exist: deploy with pinned dependencies
(reproducible) or unpinned dependencies (convenient). Pinning requires
R + renv on the client. Unpinned deploys need no R on the client at all.

Deploy is focused on getting code running -- bundle prep, manifest
generation, upload. Resource configuration, access control, and metadata
are managed via separate commands after deployment.

#### Input Cases

```
by deploy ./myapp/

  Pinned mode (manifest.json in bundle):
  ---
  1a. manifest.json already exists
      -> validate, include in bundle. Pure Go, no R needed.

  1b. renv.lock already exists
      -> manifest.FromRenvLock(): parse JSON, copy package records
        into manifest, add metadata. Pure Go, no R needed.

  1c. No lockfile, user wants pinned deps (--pin flag or prompt)
      -> R + renv required on client
      -> renv::dependencies() + renv::snapshot()
      -> parse generated renv.lock -> manifest.FromRenvLock()
      -> clean up renv artifacts

  Unpinned mode (manifest without packages):
  ---
  2a. DESCRIPTION already exists
      -> manifest.FromDescription(): JSON-ify DCF fields, add metadata
        + file checksums, add repositories from renv/PPM config or
        --repositories flag.
      -> Pure Go, no R needed.

  2b. No DESCRIPTION (bare scripts only)
      -> upload as-is. No manifest generated.
      -> server scans via pkgdepends::scan_deps(), generates
        DESCRIPTION, then builds unpinned manifest.
```

#### Priority

`manifest.json` > `renv.lock` > `DESCRIPTION` > bare scripts. The CLI
uses the highest-priority file and warns if lower-priority files are
also present (e.g., "Using manifest.json; ignoring renv.lock").

The default when neither pinned manifest nor lockfile is present:
if a DESCRIPTION exists, build an unpinned manifest and deploy (2a).
If only scripts exist, upload them and let the server scan (2b).

#### Deploy Confirmation

On the first deploy of a given path (no manifest.json present yet), the
CLI shows detected settings and asks for confirmation before uploading:

```
$ by deploy ./myapp/
Detected:
  Name:       myapp
  Mode:       shiny (entrypoint: app.R)
  Deps:       pinned (renv.lock found)
  Repository: https://p3m.dev/cran/2026-03-18

Deploy? [Y/n]
```

The `--yes` / `-y` flag skips the prompt for CI and scripting use.
Subsequent deploys of the same path (manifest.json already present)
skip the prompt automatically -- the manifest is the source of truth.

#### `by init`

Generate a manifest without deploying. Useful for inspecting or editing
the manifest before shipping, and for version-controlling the manifest
alongside application code.

```
$ by init ./myapp/ [--pin]
Detected:
  Name:       myapp
  Mode:       shiny (entrypoint: app.R)
  Deps:       pinned (renv.lock found)
  Repository: https://p3m.dev/cran/2026-03-18

Wrote manifest.json
```

Follows the same detection logic and input cases as `by deploy`. The
`--pin` flag triggers renv snapshot just like in deploy. After `init`,
`by deploy` picks up the existing manifest.json (case 1a) and skips
detection entirely.

#### Bundle Preparation

1. Detect app mode and entrypoint (`app.R` -> shiny, `server.R`/`ui.R`
   -> shiny, etc.).
2. Generate manifest (per input case above) using `internal/manifest/`
   types. Write `manifest.json` into the bundle directory.
3. Compute file checksums for the `files` section.
4. Create tar.gz archive of the directory.
5. `POST /api/v1/apps/{name}/bundles` with the archive.

#### Manifest Generation

The CLI uses `internal/manifest/` (from phase 2-5) for all manifest work:

```go
// Case 1a: manifest.json exists
m, err := manifest.ReadFile("manifest.json")
m.Validate()

// Case 1b: renv.lock exists
m, err := manifest.FromRenvLock("renv.lock", meta, files)

// Case 1c: --pin (requires R + renv)
// Shell out to Rscript, then:
m, err := manifest.FromRenvLock("renv.lock", meta, files)
// Clean up generated renv artifacts

// Case 2a: DESCRIPTION exists
m, err := manifest.FromDescription("DESCRIPTION", meta, files, repos)

// Case 2b: bare scripts -> no manifest generated, upload as-is
```

#### renv Invocation (Pinning Only)

The CLI only shells out to R for `--pin` (case 1c). Following
rsconnect's pattern (`snapshotRenvDependencies()`):

```r
options(renv.consent = TRUE)
deps <- renv::dependencies(".", quiet = TRUE, progress = FALSE)
renv::snapshot(".", packages = deps$Package, prompt = FALSE)
```

Run via `Rscript -e`. Read resulting `renv.lock`, convert to manifest
(pure Go), then clean up (`renv.lock`, `renv/` directory) unless they
pre-existed.

#### Repository URL Handling

The `--repositories` flag allows specifying repository URLs on the
command line. When absent, the CLI reads repository configuration from:

1. `renv.lock` -> `R.Repositories` (case 1b)
2. `renv::config$repos()` (case 1c, captured during snapshot)
3. A default (e.g., latest PPM) when nothing else is available

Repository URLs in the manifest are platform-neutral -- no PPM platform
segments. The server adds its own platform segment at resolve time.

#### renv Availability

renv is not part of base R. The CLI only needs R + renv for `--pin`:

| State | Behavior | Mode |
|---|---|---|
| `manifest.json` with `packages` exists | Use as-is. Pure Go. | pinned |
| `renv.lock` exists | Convert to manifest. Pure Go. | pinned |
| `--pin`, R + renv available | Snapshot -> lockfile -> manifest. | pinned |
| `--pin`, no R/renv | Error: "pinning requires R + renv." | -- |
| `DESCRIPTION` exists | Build unpinned manifest. Pure Go. | unpinned |
| Bare scripts only | Upload as-is. Server scans. | unpinned |

R is only required on the client for pinning without a lockfile.
All other paths are pure Go or need no client-side processing at all.

### Subcommands

All commands accept `<app>` as either the unique app name or UUID.
Common aliases are supported: `ls` -> `list`, `rm` -> `remove`/`delete`.

#### Setup

```
by login [--server URL]                   Store credentials interactively
by init <path> [--pin]                    Generate manifest.json without deploying
```

#### App Lifecycle

```
by deploy <path> [--name NAME] [--pin] [--yes]  Prepare bundle, generate manifest, upload
by list                                   List apps (status, active bundle, owner)
by get <app> [--runtime]                  App details (config, active bundle, status)
by enable <app>                           Allow traffic (cold-start, pre-warming)
by disable <app>                          Block new traffic, drain existing sessions
by delete <app> [--purge]                 Soft-delete (--purge: admin-only hard delete)
by restore <app>                          Restore a soft-deleted app
```

#### `by get --runtime`

By default, `by get` calls `GET /api/v1/apps/{id}` and shows app
metadata (config, active bundle, status, enabled state). With `--runtime`,
it also calls `GET /api/v1/apps/{id}/runtime` and appends live
operational data: active workers with CPU/memory stats, session counts,
and activity metrics.

The `--runtime` call requires collaborator+ access. If the caller is a
viewer, `--runtime` is silently skipped (metadata-only output).

#### Enable / Disable

Replace the previous start/stop commands with proper state management.
`disable` sets `enabled` to false on the app, which:

- Prevents the proxy from cold-starting new workers
- Prevents the autoscaler from pre-warming
- Lets existing sessions drain naturally
- Returns 503 for new requests

`enable` re-enables the app, allowing cold-start and pre-warming to
resume normally.

#### Bundles & Rollback

```
by bundles <app>                          List bundles (id, status, upload time)
by rollback <app> <bundle-id>             Roll back to a previous bundle
```

#### Configuration

```
by scale <app> [flags]                    Resource tuning
    --memory TEXT                            Memory limit (e.g., "2g")
    --cpu FLOAT                              CPU limit
    --max-workers INT                        Max workers per app
    --max-sessions INT                       Max sessions per worker
    --pre-warm INT                           Pre-warmed standby workers

by update <app> [flags]                   App metadata
    --title TEXT                             Display title
    --description TEXT                       Description

by rename <app> <new-name>                Change app name (changes URL)
```

#### Access Control

```
by access <app> show                      Show access type + ACL entries
by access <app> set-type <type>           Set access mode (acl|logged_in|public)
by access <app> grant <user> --role ROLE  Add ACL entry (viewer|collaborator)
by access <app> revoke <user>             Remove ACL entry
```

#### Tags

```
by tags list                              List all tags (global pool)
by tags create <tag>                      Create tag (admin only)
by tags delete <tag>                      Delete tag (admin only, cascades)

by tags <app> list                        List tags on an app
by tags <app> add <tag>                   Attach tag to app
by tags <app> remove <tag>               Detach tag from app
```

#### Dependencies

```
by refresh <app> [--rollback]             Refresh unpinned dependencies
```

#### Logs

```
by logs <app> [--follow]                  Tail app logs
```

#### User Management (Admin)

```
by users list                             List users
by users update <sub> [flags]             Update user role/active status
    --role ROLE                              Set role (admin|publisher|viewer)
    --active BOOL                            Enable/disable user account
```

### Command Details

#### deploy

The primary value over raw `curl`. Handles manifest generation from
multiple input types (renv.lock, DESCRIPTION, bare scripts), bundle
preparation (tar.gz), and upload.

Sensible defaults: newly deployed apps start with restrictive settings
(access_type=acl, no pre-warming, default resource limits). Users
configure access, scaling, and metadata via separate commands after
the initial deploy.

#### refresh

Wraps `POST /api/v1/apps/{id}/refresh`. Only available for unpinned
deployments:

```
$ by refresh my-app
Refreshing dependencies for my-app...
  Remotes updated: blockr-org/blockr (abc123 -> def456)
  CRAN packages: unchanged (dated repo 2026-03-18)
  Worker swap: in progress...
Done.

$ by refresh my-pinned-app
Error: my-pinned-app was deployed with pinned dependencies.
Redeploy to update.

$ by refresh my-app --rollback
Rolling back dependencies for my-app...
  Restored previous lockfile
  Worker swap: in progress...
Done.

$ by refresh my-app --rollback
Error: no previous lockfile to roll back to.
```

The `--rollback` flag wraps `POST /api/v1/apps/{id}/refresh/rollback`
from phase 2-7. It restores the previous pak lockfile and reassembles
worker libraries from it. Only one level of rollback is supported --
the store retains old package versions (append-only), so rollback is
instant.

### Error Handling

- Print the `message` field from error responses, not raw JSON.
- Non-zero exit codes on failure.
- `--json` mode: errors are JSON objects with `error` and `message`
  fields, still with non-zero exit codes.
- `by refresh` on a pinned app: clear error explaining why.
- `by deploy --pin` without R/renv: clear error with install guidance.
- `by delete --purge` on a non-deleted app: error requiring soft-delete
  first.

---

## Deliverables

**Backend:**

1. **Database migration** -- sessions table, bundle schema additions,
   `enabled` column, indexes.
2. **Session lifecycle** -- create/end/crash tracking in the proxy
   and worker lifecycle code.
3. **Bundle deployment tracking** -- populate `deployed_by` at upload
   time, `deployed_at` at activation time.
4. **Backend interface** -- add `ContainerStats()` method for live
   CPU/memory data.
5. **API split** -- remove workers from `GetApp`, add
   `GET /api/v1/apps/{id}/runtime` (collaborator+).
6. **RBAC tightening** -- `ListBundles`, `PostRefresh`,
   `PostRefreshRollback` require collaborator+.
7. **Enable/disable** -- `POST /apps/{id}/enable` and
   `POST /apps/{id}/disable` endpoints, proxy and autoscaler checks.
8. **Hard delete** -- `DELETE /apps/{id}?purge=true` endpoint
   (admin only, requires prior soft-delete).
9. **Deployments API** -- `GET /api/v1/deployments` with pagination
   and collaborator+ per-app filtering.
10. **Sessions API** -- `GET /api/v1/apps/{id}/sessions` with filtering.

**CLI:**

11. **CLI binary** (`cmd/by/main.go`) -- cobra-based subcommand structure
    with global `--json` flag.
12. **Login command** -- interactive credential storage with browser-based
    PAT creation flow (opens `/profile#tokens`).
13. **Init command** -- manifest generation without deploy, same detection
    logic as deploy.
14. **Deploy command** -- manifest generation from all input types, bundle
    preparation, upload, first-deploy confirmation prompt. The primary
    complexity in the CLI.
15. **Refresh command** -- wraps the refresh API from phase 2-7.
16. **Scale command** -- resource and scaling configuration.
17. **Access command** -- ACL management with show/set-type/grant/revoke
    subcommands.
18. **Tags command** -- global pool management + per-app tag operations.
19. **CRUD commands** -- thin API wrappers for list, get (with `--runtime`),
    enable, disable, rollback, logs, bundles, delete (with `--purge`),
    restore, update, rename, users.
20. **Error formatting** -- human-friendly error messages from API
    responses, with JSON error output in `--json` mode.

## Implementation Steps

### Step 1: Database Migration

Add sessions table, bundle columns, and `enabled` column. Write up/down
migration for both SQLite and PostgreSQL.

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
`PostRefreshRollback`.

### Step 5: Enable/Disable + Hard Delete

Implement enable/disable endpoints. Add proxy cold-start check and
autoscaler skip for disabled apps. Implement hard-delete endpoint with
soft-delete precondition check.

### Step 6: Deployments + Sessions API

Implement `GET /api/v1/deployments` with collaborator+ per-app
filtering. Implement `GET /api/v1/apps/{id}/sessions`.

### Step 7: Backend Tests

Integration tests for session lifecycle (create -> end, create -> crash).
API tests for runtime endpoint, deployments listing, session listing.
RBAC tests verifying viewer cannot access runtime API, bundles listing,
or refresh endpoints. Enable/disable behavior tests. Hard-delete
precondition tests.

### Step 8: CLI Scaffolding + Auth

Set up cobra command tree in `cmd/by/`. Implement `--json` global flag.
Implement `by login` with credential storage and browser open to
`/profile#tokens`.

### Step 9: Deploy + Init Commands

Implement the deploy flow: mode detection, manifest generation from all
input types (manifest.json, renv.lock, --pin, DESCRIPTION, bare scripts),
bundle preparation, upload. Implement `by init` sharing the same
detection logic.

### Step 10: Thin Wrapper Commands

Implement list, get (with `--runtime`), enable, disable, delete
(with `--purge`), restore, bundles, rollback, scale, update, rename,
access (show/set-type/grant/revoke), tags, refresh (with `--rollback`),
logs (with `--follow`), users.

### Step 11: CLI Tests

Test deploy flow for all input cases. Test `--json` output. Test
error formatting. Test credential storage and precedence (env vars
override config file).
