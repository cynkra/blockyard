---
title: Quick Start
description: Deploy your first Shiny app to Blockyard in under five minutes.
weight: 3
---

This guide walks through deploying a Shiny app using the `by` CLI. It assumes
Blockyard is already running (see [Installation](/docs/getting-started/installation/)).

## 1. Install the CLI

Download the `by` binary for your platform from the
[releases page](https://github.com/cynkra/blockyard/releases) and place it on
your `PATH`. See [Installation](/docs/getting-started/installation/#cli) for details.

## 2. Log in

```bash
by login --server http://localhost:8080
```

This opens your browser to create a
[Personal Access Token](/docs/guides/authorization/#personal-access-tokens), then
asks you to paste it back into the terminal.

## 3. Create your app

Your app directory should look something like:

```
my-app/
├── app.R          # or ui.R + server.R
└── DESCRIPTION    # optional: declares R package dependencies
```

## 4. Deploy

```bash
by deploy ./my-app --wait
```

The CLI detects your dependencies, bundles the directory, uploads it, and
streams the build logs:

```
Detected:
  Name:        my-app
  Mode:        DESCRIPTION (entrypoint: app.R)
  Deps:        3 packages
  Repository:  https://cran.r-project.org

Deploy? [Y/n] y

Uploading bundle... done.

  App:       my-app
  Bundle:    b1234... (building)
  Task:      t5678...
  URL:       http://localhost:8080/app/my-app/

Streaming build logs...
  ✓ Installing pak
  ✓ Restoring packages (3)
  ✓ Build complete
```

## 5. Open the app

Visit the URL printed by `by deploy`:

```
http://localhost:8080/app/my-app/
```

Blockyard spawns a worker container on demand when the first request arrives.

## Next steps

- [`by deploy` reference](/docs/reference/cli/#by-deploy-path) — all flags and
  dependency detection modes
- [Deploying an App](/docs/guides/deploying/) — bundle structure, build process,
  pinned vs. unpinned dependencies
- [Authorization](/docs/guides/authorization/) — roles, per-app ACLs, and
  visibility settings

---

## Using the REST API directly

If you prefer scripting with `curl` instead of the CLI, the same workflow
looks like this:

```bash
export BLOCKYARD=http://localhost:8080
export TOKEN=by_...   # your Personal Access Token

# Create the app
curl -X POST "$BLOCKYARD/api/v1/apps" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-app"}'

# Bundle and upload
tar -czf bundle.tar.gz -C my-app .
curl -X POST "$BLOCKYARD/api/v1/apps/my-app/bundles" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @bundle.tar.gz

# Stream build logs (use the task_id from the upload response)
curl "$BLOCKYARD/api/v1/tasks/<task-id>/logs" \
  -H "Authorization: Bearer $TOKEN"
```

See the [REST API reference](/docs/reference/api/) for the full endpoint list.
