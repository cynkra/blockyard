# Phase 2-9: CLI Tool (Draft)

Draft design for the CLI binary (`cmd/by/`). Final phase — built last to
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

`BLOCKYARD_TOKEN` environment variable (a PAT). No login command — users
create PATs via the web UI and export the env var. A `by login`
convenience command is a future addition.

`BLOCKYARD_URL` environment variable (e.g.,
`https://blockyard.example.com`).

## Deploy Flow

The `by deploy` command prepares a bundle and uploads it. From the user's
perspective, two choices exist: deploy with pinned dependencies
(reproducible) or unpinned dependencies (convenient). Pinning requires
R + renv on the client. Unpinned deploys need no R on the client at all.

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

```
by deploy <path> [--name NAME] [--pin]   Prepare bundle, generate manifest, upload
by list                                   List apps (status, active bundle, owner)
by get <app>                              Get app details
by start <app>                            Start an app
by stop <app>                             Stop an app
by rollback <app> <bundle-id>             Roll back to a previous bundle
by logs <app> [--follow]                  Tail app logs
by bundles <app>                          List bundles for an app
by delete <app>                           Soft-delete an app
by restore <app>                          Restore a soft-deleted app
by config <app> [flags]                   Update app config (--memory, --cpu, etc.)
by refresh <app> [--rollback]              Refresh unpinned dependencies
by users list                             List users (admin only)
by users update <sub> [flags]             Update user role/active status
```

All commands except `deploy` and `refresh` are thin wrappers around the
REST API — parse flags, call endpoint, format response.

### deploy

The primary value over raw `curl`. Handles manifest generation from
multiple input types (renv.lock, DESCRIPTION, bare scripts), bundle
preparation (tar.gz), and upload.

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
- `by refresh` on a pinned app: clear error explaining why.
- `by deploy --pin` without R/renv: clear error with install guidance.

## Deliverables

1. **CLI binary** (`cmd/by/main.go`) — cobra-based subcommand structure.
2. **Deploy command** — manifest generation from all input types, bundle
   preparation, upload. The primary complexity in the CLI.
3. **Refresh command** — wraps the refresh API from phase 2-7.
4. **CRUD commands** — thin API wrappers for list, get, start, stop,
   rollback, logs, bundles, delete, restore, config, users.
5. **Error formatting** — human-friendly error messages from API
   responses.
