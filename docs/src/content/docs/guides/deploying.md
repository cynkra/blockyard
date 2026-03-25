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

```bash
tar -czf bundle.tar.gz -C my-app .
```

## Uploading a bundle

```bash
curl -X POST "$BLOCKYARD/api/v1/apps/<app-id>/bundles" \
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
curl "$BLOCKYARD/api/v1/tasks/<task-id>/logs" \
  -H "Authorization: Bearer $TOKEN"
```

## Accessing the app

Once the build completes, the app is accessible at:

```
http://<blockyard-host>/app/<app-name>/
```

Blockyard spawns a worker container on the first request (cold start) and
proxies HTTP and WebSocket traffic to it. A session cookie pins the user to
the same worker for subsequent requests.

You can also pre-start a worker via the API:

```bash
curl -X POST "$BLOCKYARD/api/v1/apps/<app-id>/start" \
  -H "Authorization: Bearer $TOKEN"
```

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

To deploy a new version, upload another bundle to the same app. Once it
builds successfully, it becomes the new active bundle.

Old bundles are automatically pruned based on `bundle_retention` (default
50 per app). The active bundle is never pruned.
