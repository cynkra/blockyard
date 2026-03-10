---
title: REST API
description: Complete reference for the Blockyard control plane API.
---

All endpoints are under `/api/v1/` and require a bearer token in the
`Authorization` header (except `/healthz`).

```bash
curl -H "Authorization: Bearer $TOKEN" ...
```

## Health

### `GET /healthz`

Returns `200 OK` with body `ok`. No authentication required.

---

## Apps

All `{id}` path parameters accept either the app's UUID or its name.

### `POST /api/v1/apps`

Create a new app.

**Request body:** JSON with a `name` field. Names must be URL-safe slugs
(lowercase letters, digits, and hyphens; must start with a letter; must not
end with a hyphen; 1–63 characters).

```json
{ "name": "my-dashboard" }
```

**Response:** `201 Created`

```json
{
  "id": "a1b2c3...",
  "name": "my-dashboard",
  "status": "stopped",
  "active_bundle": null,
  ...
}
```

### `GET /api/v1/apps`

List all apps.

**Response:** `200 OK` — array of app objects, each with a derived `status`
field (`"running"` or `"stopped"`).

### `GET /api/v1/apps/{id}`

Get a single app by ID.

**Response:** `200 OK` — app object with derived `status`.

### `PATCH /api/v1/apps/{id}`

Update app configuration. Only resource-limit fields are mutable:

```json
{
  "max_workers_per_app": 4,
  "max_sessions_per_worker": 2,
  "memory_limit": "512m",
  "cpu_limit": 0.5
}
```

All fields are optional — only provided fields are updated.

**Response:** `200 OK` — updated app object.

### `DELETE /api/v1/apps/{id}`

Delete an app. Stops all running workers, removes bundle files from disk,
and deletes all database rows.

**Response:** `204 No Content`

---

## App Lifecycle

### `POST /api/v1/apps/{id}/start`

Start an app by spawning a worker container. No-op if already running.
The app must have an active bundle.

**Response:** `200 OK`

```json
{
  "worker_id": "w1234...",
  "status": "running"
}
```

### `POST /api/v1/apps/{id}/stop`

Stop all workers for an app.

**Response:** `200 OK`

```json
{
  "status": "stopped",
  "workers_stopped": 1
}
```

### `GET /api/v1/apps/{id}/logs`

Stream logs from a running worker. Returns chunked `text/plain`.

**Query parameters:**

| Parameter | Type | Default | Description |
|---|---|---|---|
| `worker_id` | `string` | — | Stream logs from a specific worker. If omitted, picks the first worker for the app. |

---

## Bundles

### `POST /api/v1/apps/{id}/bundles`

Upload a new bundle. The app must already exist.

**Request body:** raw `.tar.gz` bytes (`Content-Type: application/octet-stream`).

Uploads larger than `max_bundle_size` (default 100 MB) are rejected with
`413 Payload Too Large`.

**Response:** `202 Accepted`

```json
{
  "bundle_id": "b1234...",
  "task_id": "t5678..."
}
```

The build (dependency restore) runs asynchronously. Use the task endpoint
to follow progress.

### `GET /api/v1/apps/{id}/bundles`

List all bundles for an app.

**Response:** `200 OK` — array of bundle objects.

---

## Tasks

### `GET /api/v1/tasks/{task_id}/logs`

Stream logs for a background task (e.g. dependency restoration).

If the task is still running, the response streams buffered output followed
by live lines. If the task is complete, the full log is returned.

**Response:** `200 OK` — chunked `text/plain`.

---

## Errors

All error responses use a consistent JSON shape:

```json
{
  "error": "not_found",
  "message": "app a3f2c1... not found"
}
```

| Status | Meaning |
|---|---|
| `400` | Bad request (e.g. empty bundle body, invalid app name) |
| `401` | Missing or invalid bearer token |
| `404` | Resource not found |
| `409` | Conflict (e.g. duplicate app name) |
| `413` | Bundle exceeds `max_bundle_size` |
| `500` | Internal server error |
| `503` | Service unavailable (e.g. max workers reached, worker start timeout) |
