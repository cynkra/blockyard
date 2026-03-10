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

Returns `200 OK`. No authentication required.

---

## Apps

### `POST /api/v1/apps`

Create a new app.

**Request body:**

```json
{ "name": "my-app" }
```

`name` must be a unique, URL-safe slug.

**Response:** `201 Created`

```json
{
  "id": "a3f2c1...",
  "name": "my-app",
  "status": "created"
}
```

### `GET /api/v1/apps`

List all apps.

**Response:** `200 OK` — array of app objects.

### `GET /api/v1/apps/{id}`

Get a single app by ID, including its active bundle and status.

### `PATCH /api/v1/apps/{id}`

Update app configuration (e.g. resource limits).

**Request body:** partial app object with fields to update.

### `DELETE /api/v1/apps/{id}`

Delete an app. Stops all workers, removes bundles from disk, and deletes
database records.

**Response:** `204 No Content`

---

## Bundles

### `POST /api/v1/apps/{id}/bundles`

Upload a new bundle.

**Request body:** raw `.tar.gz` bytes (`Content-Type: application/octet-stream`).

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

---

## App Lifecycle

### `POST /api/v1/apps/{id}/start`

Pre-start a worker for the app. No-op if a worker is already running.

**Response:** `200 OK`

### `POST /api/v1/apps/{id}/stop`

Stop all workers for the app.

**Response:** `200 OK`

### `GET /api/v1/apps/{id}/logs`

Stream container logs for the app.

**Query parameters:**

| Parameter | Type | Description |
|---|---|---|
| `worker_id` | string | Filter to a specific worker (optional) |
| `follow` | bool | Keep the connection open for live output |

**Response:** `200 OK` — chunked `text/plain`.

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
  "error": "app_not_found",
  "message": "No app with ID a3f2c1..."
}
```

| Status | Meaning |
|---|---|
| `400` | Bad request or validation error |
| `401` | Missing or invalid bearer token |
| `404` | Resource not found |
| `409` | Conflict (e.g. duplicate app name) |
| `503` | Max workers reached |
