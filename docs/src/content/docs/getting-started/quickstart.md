---
title: Quick Start
description: Deploy your first Shiny app to Blockyard in under five minutes.
---

This guide walks through deploying a Shiny app from scratch. It assumes
Blockyard is already running (see [Installation](/getting-started/installation/)).

Set a couple of shell variables to keep the examples concise. Create a
[Personal Access Token](/guides/authorization/#personal-access-tokens) via the
web UI first.

```bash
export BLOCKYARD=http://localhost:8080
export TOKEN=by_...   # your Personal Access Token
```

## 1. Create an app

Before uploading a bundle, you need to register the app:

```bash
curl -X POST "$BLOCKYARD/api/v1/apps" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "hello-shiny"}'
```

## 2. Prepare your bundle

Your app directory should look something like:

```
my-app/
├── app.R          # or ui.R + server.R
└── DESCRIPTION    # optional: declares R package dependencies
```

Package it into a `.tar.gz`:

```bash
tar -czf bundle.tar.gz -C my-app .
```

## 3. Upload the bundle

```bash
curl -X POST "$BLOCKYARD/api/v1/apps/hello-shiny/bundles" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @bundle.tar.gz
```

```json
{
  "bundle_id": "b1234...",
  "task_id": "t5678..."
}
```

The response returns immediately with a `202 Accepted`. Dependency
restoration happens in the background.

## 4. Watch the build

Stream the build logs to see dependency restoration progress:

```bash
curl "$BLOCKYARD/api/v1/tasks/t5678.../logs" \
  -H "Authorization: Bearer $TOKEN"
```

When the build completes, the bundle status changes to `ready` and becomes
the active bundle for the app.

## 5. Open the app

Once the build completes, visit the app in your browser:

```
http://localhost:8080/app/hello-shiny/
```

Blockyard spawns a worker container on demand when the first request arrives.
You can also pre-start a worker via the API:

```bash
curl -X POST "$BLOCKYARD/api/v1/apps/hello-shiny/start" \
  -H "Authorization: Bearer $TOKEN"
```
