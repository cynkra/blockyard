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

## Bundles

### `POST /api/v1/apps/{id}/bundles`

Upload a new bundle. The app must already exist in the database.

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
| `400` | Bad request (e.g. empty bundle body) |
| `401` | Missing or invalid bearer token |
| `404` | Resource not found |
| `413` | Bundle exceeds `max_bundle_size` |
| `500` | Internal server error |
