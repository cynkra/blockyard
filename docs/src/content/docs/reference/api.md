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

### `GET /readyz`

Readiness probe that checks backend dependencies (database, Docker socket, and
optionally IdP and OpenBao). No authentication required.

**Response:** `200 OK` when all checks pass, `503 Service Unavailable` otherwise.

```json
{
  "status": "ready",
  "checks": {
    "database": "pass",
    "docker": "pass"
  }
}
```

When not all checks pass, `status` is `"not_ready"` and the HTTP status is `503`.

When OIDC and/or OpenBao are configured, their health is included in the checks
(as `"idp"` and `"openbao"` respectively).

### `GET /metrics`

Prometheus metrics endpoint. Only available when `telemetry.metrics_enabled` is
`true`. No authentication required.

---

## Authentication

These endpoints are available when OIDC is configured.

### `GET /login`

Redirects the user to the configured OIDC provider for authentication.

### `GET /callback`

OIDC callback endpoint. Completes the login flow and sets a session cookie.

### `POST /logout`

Clears the session cookie and logs the user out.

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

Update app configuration. All fields are optional — only provided fields are
updated.

```json
{
  "max_workers_per_app": 4,
  "max_sessions_per_worker": 1,
  "memory_limit": "512m",
  "cpu_limit": 0.5,
  "access_type": "acl",
  "title": "My Dashboard",
  "description": "A sales analytics dashboard"
}
```

| Field | Type | Description |
|---|---|---|
| `max_workers_per_app` | `integer` | Max concurrent workers (must be >= 1) |
| `max_sessions_per_worker` | `integer` | Sessions per worker (must be >= 1) |
| `memory_limit` | `string` | Container memory limit (e.g. `"512m"`) |
| `cpu_limit` | `float` | CPU limit (e.g. `0.5` for half a core) |
| `access_type` | `string` | `"acl"` or `"public"` (requires owner or admin) |
| `title` | `string` | Human-readable title for the catalog |
| `description` | `string` | Description for the catalog |

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

Stop all workers for an app. Workers are drained asynchronously — active
sessions are allowed to finish before containers are removed.

If no workers are running, returns `200 OK`:

```json
{
  "stopped_workers": 0
}
```

Otherwise, returns `202 Accepted` with a task ID for tracking the drain:

```json
{
  "task_id": "t1234...",
  "worker_count": 2
}
```

Use `GET /api/v1/tasks/{task_id}/logs` to follow drain progress.

### `GET /api/v1/apps/{id}/logs`

Stream logs from a running worker. Returns chunked `text/plain`.

If the worker has already exited (but is within the log retention window), the
buffered logs are returned as a complete response. If the worker is still
running, buffered lines are sent immediately followed by live streaming.

**Query parameters:**

| Parameter | Type | Default | Description |
|---|---|---|---|
| `worker_id` | `string` | — | **Required.** The worker to stream logs from. Use the `workers` field from the app response to discover IDs. |

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

### `GET /api/v1/tasks/{task_id}`

Get the current status of a background task.

**Response:** `200 OK`

```json
{
  "id": "t5678...",
  "status": "running",
  "created_at": "2024-01-15T09:30:00Z"
}
```

`status` is one of `"running"`, `"completed"`, or `"failed"`.

### `GET /api/v1/tasks/{task_id}/logs`

Stream logs for a background task (e.g. dependency restoration).

If the task is still running, the response streams buffered output followed
by live lines. If the task is complete, the full log is returned.

**Response:** `200 OK` — chunked `text/plain`.

---

## Access Control (ACL)

Manage per-app access grants. Requires owner or admin permissions on the app.

### `POST /api/v1/apps/{id}/access`

Grant a user or group access to an app.

**Request body:**

```json
{
  "principal": "user-sub-or-group-name",
  "kind": "user",
  "role": "viewer"
}
```

- `kind` must be `"user"` or `"group"`
- `role` must be `"viewer"` or `"collaborator"`

**Response:** `204 No Content`

### `GET /api/v1/apps/{id}/access`

List all access grants for an app.

**Response:** `200 OK` — array of grant objects.

```json
[
  {
    "principal": "jane",
    "kind": "user",
    "role": "viewer",
    "granted_by": "admin-sub",
    "granted_at": "2025-01-15T09:30:00Z"
  }
]
```

### `DELETE /api/v1/apps/{id}/access/{kind}/{principal}`

Revoke a specific access grant.

**Response:** `204 No Content`

---

## Role Mappings

Map OIDC groups to platform roles. Admin only.

### `GET /api/v1/role-mappings`

List all group-to-role mappings.

**Response:** `200 OK`

```json
[
  { "group_name": "data-team", "role": "publisher" }
]
```

### `PUT /api/v1/role-mappings/{group_name}`

Create or update a role mapping.

**Request body:**

```json
{ "role": "publisher" }
```

Valid roles: `admin`, `publisher`, `viewer`.

**Response:** `204 No Content`

### `DELETE /api/v1/role-mappings/{group_name}`

Delete a role mapping.

**Response:** `204 No Content`

---

## Tags

### `GET /api/v1/tags`

List all tags.

**Response:** `200 OK` — array of tag objects.

### `POST /api/v1/tags`

Create a new tag. Admin only. Tag names follow the same rules as app names
(lowercase slugs, 1–63 characters).

**Request body:**

```json
{ "name": "production" }
```

**Response:** `201 Created`

### `DELETE /api/v1/tags/{tagID}`

Delete a tag. Admin only. Cascades to all app–tag associations.

**Response:** `204 No Content`

### `POST /api/v1/apps/{id}/tags`

Attach a tag to an app. Requires deploy permissions (owner, collaborator, or admin).

**Request body:**

```json
{ "tag_id": "tag-uuid" }
```

**Response:** `204 No Content`

### `DELETE /api/v1/apps/{id}/tags/{tagID}`

Remove a tag from an app. Requires deploy permissions.

**Response:** `204 No Content`

---

## Catalog

### `GET /api/v1/catalog`

Paginated, RBAC-filtered listing of apps with metadata and tags.

**Query parameters:**

| Parameter | Type | Default | Description |
|---|---|---|---|
| `tag` | `string` | — | Filter by tag name |
| `search` | `string` | — | Search by app name, title, or description |
| `page` | `integer` | `1` | Page number |
| `per_page` | `integer` | `20` | Items per page (max 100) |

**Response:** `200 OK`

```json
{
  "items": [
    {
      "id": "a1b2c3...",
      "name": "my-dashboard",
      "title": "My Dashboard",
      "description": "A Shiny dashboard",
      "owner": "jane",
      "tags": ["production"],
      "status": "running",
      "url": "/app/my-dashboard/",
      "updated_at": "2025-01-15T09:30:00Z"
    }
  ],
  "total": 1,
  "page": 1,
  "per_page": 20
}
```

---

## Credentials

### `POST /api/v1/credentials/vault`

Exchange a session reference token for a scoped OpenBao token. This endpoint
uses session token authentication (not the API bearer token). Only available
when OpenBao is configured.

**Response:** `200 OK`

```json
{
  "token": "hvs.CAESIxyz...",
  "ttl": 3600
}
```

### `POST /api/v1/users/me/credentials/{service}`

Store a user credential in OpenBao's KV store. Authenticated via session cookie
or JWT bearer token. Only available when OpenBao is configured.

**Request body:**

```json
{ "api_key": "sk-..." }
```

**Response:** `204 No Content`

---

## Proxy (Data Plane)

When OIDC is configured, proxy routes enforce authentication — users must be
logged in to access apps. Without OIDC, proxy routes are unauthenticated.
Session affinity is managed via cookies.

### `GET /app/{name}/`

Reverse-proxy to the Shiny app. On the first request, Blockyard spawns a
worker container (cold start), waits for it to become healthy, and forwards
the request. A `blockyard_session` cookie is set to pin subsequent requests
to the same worker.

WebSocket upgrade requests are also supported at any path under
`/app/{name}/`.

### `GET /app/{name}/{path}`

Same as above, for any sub-path within the app.

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
| `403` | Insufficient permissions for the requested action |
| `404` | Resource not found |
| `409` | Conflict (e.g. duplicate app name) |
| `413` | Bundle exceeds `max_bundle_size` |
| `500` | Internal server error |
| `502` | Upstream service error (e.g. OpenBao login failure) |
| `503` | Service unavailable (e.g. max workers reached, worker start timeout) |
