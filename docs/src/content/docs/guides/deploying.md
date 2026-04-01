---
title: Deploying an App
description: How to deploy and update Shiny applications on Blockyard.
---

## Bundle structure

A bundle is a `.tar.gz` archive containing your Shiny app source. At
minimum it needs:

- **`app.R`** (or `ui.R` + `server.R`) — the Shiny application code

Optionally include dependency metadata in one of these formats (highest
priority first): `manifest.json`, `renv.lock`, or `DESCRIPTION`. If none
is present, Blockyard scans your scripts to discover dependencies
automatically.

Any additional files (data, assets, R scripts sourced by the app) are
included automatically when you tar the directory.

## Deploying with the CLI

The `by deploy` command handles bundling, uploading, and optionally waiting
for the build — all in one step:

```bash
by deploy ./my-app --wait
```

The CLI auto-detects your entrypoint and dependency format, creates the tar
archive, and uploads it. If the app does not exist yet it is created
automatically.

### Pinned vs. unpinned dependencies

By default, if your app has a `DESCRIPTION` file, the deploy creates an
**unpinned** manifest — Blockyard resolves the latest compatible versions at
build time. To lock exact versions:

```bash
by deploy ./my-app --pin --wait
```

This runs `renv::snapshot()` locally (requires R and renv) and includes the
resulting lockfile in the bundle. You can also commit a `renv.lock` or
`manifest.json` directly in your app directory — the CLI will use it as-is.

### Generating a manifest without deploying

To inspect or commit the resolved manifest before deploying:

```bash
by init ./my-app
# writes my-app/manifest.json
```

### Choosing a name

The app name defaults to the directory basename. Override it with `--name`:

```bash
by deploy ./my-app --name sales-dashboard --wait
```

See the [CLI reference](/reference/cli/#by-deploy-path) for the full flag list.

## Deploying with the REST API

If you prefer scripting with `curl`:

```bash
tar -czf bundle.tar.gz -C my-app .

curl -X POST "$BLOCKYARD/api/v1/apps/<app-name>/bundles" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @bundle.tar.gz
```

The upload returns `202 Accepted` with a `task_id`. The server unpacks the
archive, resolves a manifest from the bundle contents, starts a build
container to restore dependencies via [pak](https://pak.r-lib.org/), and
streams logs to the task endpoint.

Bundles larger than `max_bundle_size` (default 100 MB) are rejected with
`413 Payload Too Large`.

## Build process

During the build:

1. The bundle is unpacked and a manifest is resolved from its contents
2. pak installs packages into a library directory inside a build container
3. On success, the bundle is activated as the app's current deployment
4. On failure, the bundle is marked `failed` and the previous active bundle
   (if any) remains unchanged

You can monitor the build by streaming task logs:

```bash
# With the CLI (the easiest way — streams build logs automatically)
by deploy ./my-app --wait

# With curl (use the task_id from the bundle upload response)
curl "$BLOCKYARD/api/v1/tasks/<task-id>/logs" \
  -H "Authorization: Bearer $TOKEN"
```

Note: `by logs` streams **worker** container logs (for a running app),
not build logs. Use `by deploy --wait` or the task logs endpoint to
follow the build.

## Accessing the app

Once the build completes, the app is accessible at:

```
http://<blockyard-host>/app/<app-name>/
```

Blockyard spawns a worker container on the first request (cold start) and
proxies HTTP and WebSocket traffic to it. A session cookie pins the user to
the same worker for subsequent requests.

### Reconnection on network interruptions

If the browser's WebSocket connection drops briefly (network blip, laptop
sleep), the proxy keeps the backend connection alive for up to `ws_cache_ttl`
(default 60 s). When the client reconnects, it resumes the same Shiny
session transparently — no page reload, no lost state. This works for all
apps with no code changes.

For disconnects **longer** than the cache TTL, the session is lost and the
user sees Shiny's "Disconnected" overlay. Apps whose outputs are purely
determined by their inputs can opt into Shiny's
[new-session reconnection](https://shiny.posit.co/r/articles/improve/reconnecting/)
to recover automatically:

```r
server <- function(input, output, session) {
  session$allowReconnect(TRUE)
  # ...
}
```

This is **not safe** for apps that store state in `reactiveValues`, count
`actionButton` presses, accept file uploads, or generate random values —
that state is lost when a new session starts.

## Container security

Worker containers run with hardened defaults:

- **All Linux capabilities dropped** (`--cap-drop ALL`)
- **No new privileges** (`--security-opt no-new-privileges`)
- **Read-only root filesystem** — only `/tmp` (tmpfs) and the bundle
  mount are writable
- **Per-worker bridge network** — each worker gets its own isolated
  Docker network
- **Cloud metadata blocked** — requests to `169.254.169.254` are dropped
  via iptables rules to prevent SSRF against cloud instance metadata

These settings are not configurable — they are always applied.

## Updating an app

To deploy a new version, run `by deploy` again (or upload another bundle via
the API). Once it builds successfully, it becomes the new active bundle.

To roll back to a previous bundle:

```bash
by bundles my-app          # list available bundles
by rollback my-app <id>    # activate a previous bundle
```

Old bundles are automatically pruned based on `bundle_retention` (default
50 per app). The active bundle is never pruned.

## Refreshing unpinned dependencies

For apps deployed with a `DESCRIPTION` (unpinned), you can re-resolve
packages from repositories without uploading a new bundle:

```bash
by refresh my-app
```

This triggers a background build that pulls the latest compatible package
versions. Use `by refresh my-app --rollback` to revert to the previous set.

### Automatic refresh

You can schedule periodic dependency refreshes via the REST API using a
standard 5-field cron expression:

```bash
curl -X PATCH "$BLOCKYARD/api/v1/apps/my-app" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"refresh_schedule": "0 3 * * 1"}'
```

This example refreshes every Monday at 03:00. To remove the schedule, set
`refresh_schedule` to an empty string. See
[`PATCH /api/v1/apps/{id}`](/reference/api/#patch-apiv1appsid) for details.

## Disabling an app

You can temporarily take an app offline without deleting it:

```bash
by disable my-app
```

Disabling an app ends all active sessions, drains running workers, and
returns `503 Service Unavailable` for any proxy requests. To bring it back
online:

```bash
by enable my-app
```
