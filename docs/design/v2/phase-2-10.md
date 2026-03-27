# Phase 2-10: CLI

Design for the CLI binary (`cmd/by/`). Final phase — built last to
target the stable v2 API surface. The deploy command is the primary new
complexity; all other subcommands are thin REST API wrappers.

See [dep-mgmt.md](../dep-mgmt.md) for the architectural overview that
drives the deploy flow.

## Prerequisites from Earlier Phases

- **Phase 2-5** — `internal/manifest/` types, `FromRenvLock()` and
  `FromDescription()` conversion functions, manifest validation. The CLI
  imports these to generate manifests during `by deploy`.
- **Phase 2-6** — store-aware builds on the server. No direct CLI
  dependency, but deploy benefits from fast builds.
- **Phase 2-7** — `POST /api/v1/packages` and refresh API. The CLI
  wraps the refresh endpoint as `by refresh`.

## Authentication

`BLOCKYARD_TOKEN` environment variable (a PAT). `BLOCKYARD_URL`
environment variable (e.g., `https://blockyard.example.com`).

### `by login`

A convenience command that lowers the barrier for first-time users:

1. Prompt for the server URL (or accept `--server URL`).
2. Open the browser to the web UI's PAT creation page.
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
over the stored config — CI pipelines use env vars, interactive users use
`by login`. The config file stores a single server entry; multi-server
profiles are a future extension if demand arises.

## Output Format

All commands default to human-readable output (tables, formatted text).
A global `--json` flag switches to machine-readable JSON output — useful
for scripting and CI pipelines.

For thin API wrappers, `--json` passes through the API response body
directly. For commands with client-side logic (deploy, refresh), `--json`
emits a structured JSON object on completion instead of streaming
progress text.

## Deploy Flow

The `by deploy` command prepares a bundle and uploads it. From the user's
perspective, two choices exist: deploy with pinned dependencies
(reproducible) or unpinned dependencies (convenient). Pinning requires
R + renv on the client. Unpinned deploys need no R on the client at all.

Deploy is focused on getting code running — bundle prep, manifest
generation, upload. Resource configuration, access control, and metadata
are managed via separate commands after deployment.

### Input Cases

```
by deploy ./myapp/

  Pinned mode (manifest.json in bundle):
  ───────────────────────────────────────
  1a. manifest.json already exists
      → validate, include in bundle. Pure Go, no R needed.

  1b. renv.lock already exists
      → manifest.FromRenvLock(): parse JSON, copy package records
        into manifest, add metadata. Pure Go, no R needed.

  1c. No lockfile, user wants pinned deps (--pin flag or prompt)
      → R + renv required on client
      → renv::dependencies() + renv::snapshot()
      → parse generated renv.lock → manifest.FromRenvLock()
      → clean up renv artifacts

  Unpinned mode (manifest without packages):
  ────────────────────────────────────────
  2a. DESCRIPTION already exists
      → manifest.FromDescription(): JSON-ify DCF fields, add metadata
        + file checksums, add repositories from renv/PPM config or
        --repositories flag.
      → Pure Go, no R needed.

  2b. No DESCRIPTION (bare scripts only)
      → upload as-is. No manifest generated.
      → server scans via pkgdepends::scan_deps(), generates
        DESCRIPTION, then builds unpinned manifest.
```

### Priority

`manifest.json` > `renv.lock` > `DESCRIPTION` > bare scripts. The CLI
uses the highest-priority file and warns if lower-priority files are
also present (e.g., "Using manifest.json; ignoring renv.lock").

The default when neither pinned manifest nor lockfile is present:
if a DESCRIPTION exists, build an unpinned manifest and deploy (2a).
If only scripts exist, upload them and let the server scan (2b).

### Deploy Confirmation

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
skip the prompt automatically — the manifest is the source of truth.

### `by init`

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

### Bundle Preparation

1. Detect app mode and entrypoint (`app.R` → shiny, `server.R`/`ui.R`
   → shiny, etc.).
2. Generate manifest (per input case above) using `internal/manifest/`
   types. Write `manifest.json` into the bundle directory.
3. Compute file checksums for the `files` section.
4. Create tar.gz archive of the directory.
5. `POST /api/v1/apps/{name}/bundles` with the archive.

### Manifest Generation

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

// Case 2b: bare scripts → no manifest generated, upload as-is
```

### renv Invocation (Pinning Only)

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

### Repository URL Handling

The `--repositories` flag allows specifying repository URLs on the
command line. When absent, the CLI reads repository configuration from:

1. `renv.lock` → `R.Repositories` (case 1b)
2. `renv::config$repos()` (case 1c, captured during snapshot)
3. A default (e.g., latest PPM) when nothing else is available

Repository URLs in the manifest are platform-neutral — no PPM platform
segments. The server adds its own platform segment at resolve time.

### renv Availability

renv is not part of base R. The CLI only needs R + renv for `--pin`:

| State | Behavior | Mode |
|---|---|---|
| `manifest.json` with `packages` exists | Use as-is. Pure Go. | pinned |
| `renv.lock` exists | Convert to manifest. Pure Go. | pinned |
| `--pin`, R + renv available | Snapshot → lockfile → manifest. | pinned |
| `--pin`, no R/renv | Error: "pinning requires R + renv." | — |
| `DESCRIPTION` exists | Build unpinned manifest. Pure Go. | unpinned |
| Bare scripts only | Upload as-is. Server scans. | unpinned |

R is only required on the client for pinning without a lockfile.
All other paths are pure Go or need no client-side processing at all.

## Subcommands

All commands accept `<app>` as either the unique app name or UUID.
Common aliases are supported: `ls` → `list`, `rm` → `remove`/`delete`.

### Setup

```
by login [--server URL]                   Store credentials interactively
by init <path> [--pin]                    Generate manifest.json without deploying
```

### App Lifecycle

```
by deploy <path> [--name NAME] [--pin] [--yes]  Prepare bundle, generate manifest, upload
by list                                   List apps (status, active bundle, owner)
by get <app>                              App details (config, active bundle, status)
by enable <app>                           Allow traffic (cold-start, pre-warming)
by disable <app>                          Block new traffic, drain existing sessions
by delete <app> [--purge]                 Soft-delete (--purge: admin-only hard delete)
by restore <app>                          Restore a soft-deleted app
```

#### Enable / Disable

Replace the previous start/stop commands with proper state management.
`disable` sets an `enabled` flag to false on the app, which:

- Prevents the proxy from cold-starting new workers
- Prevents the autoscaler from pre-warming
- Lets existing sessions drain naturally
- Returns 503 for new requests

`enable` re-enables the app, allowing cold-start and pre-warming to
resume normally.

**Requires new server-side work:** a migration adding
`enabled INTEGER NOT NULL DEFAULT 1` to the `apps` table, plus checks
in the proxy cold-start path and autoscaler pre-warming loop.

### Bundles & Rollback

```
by bundles <app>                          List bundles (id, status, upload time)
by rollback <app> <bundle-id>             Roll back to a previous bundle
```

### Configuration

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

### Access Control

```
by access <app> show                      Show access type + ACL entries
by access <app> set-type <type>           Set access mode (acl|logged_in|public)
by access <app> grant <user> --role ROLE  Add ACL entry (viewer|collaborator)
by access <app> revoke <user>             Remove ACL entry
```

### Tags

```
by tags list                              List all tags (global pool)
by tags create <tag>                      Create tag (admin only)
by tags delete <tag>                      Delete tag (admin only, cascades)

by tags <app> list                        List tags on an app
by tags <app> add <tag>                   Attach tag to app
by tags <app> remove <tag>               Detach tag from app
```

### Dependencies

```
by refresh <app> [--rollback]             Refresh unpinned dependencies
```

### Logs

```
by logs <app> [--follow]                  Tail app logs
```

### User Management (Admin)

```
by users list                             List users
by users update <sub> [flags]             Update user role/active status
    --role ROLE                              Set role (admin|publisher|viewer)
    --active BOOL                            Enable/disable user account
```

## Command Details

### deploy

The primary value over raw `curl`. Handles manifest generation from
multiple input types (renv.lock, DESCRIPTION, bare scripts), bundle
preparation (tar.gz), and upload.

Sensible defaults: newly deployed apps start with restrictive settings
(access_type=acl, no pre-warming, default resource limits). Users
configure access, scaling, and metadata via separate commands after
the initial deploy.

### refresh

Wraps `POST /api/v1/apps/{id}/refresh`. Only available for unpinned
deployments:

```
$ by refresh my-app
Refreshing dependencies for my-app...
  Remotes updated: blockr-org/blockr (abc123 → def456)
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
worker libraries from it. Only one level of rollback is supported —
the store retains old package versions (append-only), so rollback is
instant.

## Error Handling

- Print the `message` field from error responses, not raw JSON.
- Non-zero exit codes on failure.
- `--json` mode: errors are JSON objects with `error` and `message`
  fields, still with non-zero exit codes.
- `by refresh` on a pinned app: clear error explaining why.
- `by deploy --pin` without R/renv: clear error with install guidance.
- `by delete --purge` on a non-deleted app: error requiring soft-delete
  first.

## Deliverables

1. **Server-side: enable/disable** — migration adding `enabled` column,
   proxy and autoscaler checks, API endpoints for enable/disable.
2. **Server-side: hard delete** — API endpoint for permanent app removal
   (admin only, requires prior soft-delete).
3. **CLI binary** (`cmd/by/main.go`) — cobra-based subcommand structure
   with global `--json` flag.
4. **Login command** — interactive credential storage with browser-based
   PAT creation flow.
5. **Init command** — manifest generation without deploy, same detection
   logic as deploy.
6. **Deploy command** — manifest generation from all input types, bundle
   preparation, upload, first-deploy confirmation prompt. The primary
   complexity in the CLI.
7. **Refresh command** — wraps the refresh API from phase 2-7.
8. **Scale command** — resource and scaling configuration.
9. **Access command** — ACL management with show/set-type/grant/revoke
   subcommands.
10. **Tags command** — global pool management + per-app tag operations.
11. **CRUD commands** — thin API wrappers for list, get, enable, disable,
    rollback, logs, bundles, delete, restore, update, rename, users.
12. **Error formatting** — human-friendly error messages from API
    responses, with JSON error output in `--json` mode.
