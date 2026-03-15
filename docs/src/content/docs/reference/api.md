---
title: REST API
description: Complete reference for the Blockyard control plane API.
---

All endpoints are under `/api/v1/` and require authentication (except
`/healthz`, `/readyz`, and `/metrics`).

Authenticate with a [Personal Access Token](/guides/authorization/#personal-access-tokens)
(`Authorization: Bearer by_...`) or an OIDC session cookie (browser).

```bash
curl -H "Authorization: Bearer $TOKEN" ...
```

## Health

### `GET /healthz`

Returns `200 OK` with body `ok`. No authentication required.

### `GET /readyz`

Readiness probe that checks backend dependencies (database, Docker socket, and
optionally IdP and OpenBao). No authentication required, but the response
detail varies based on the caller.

**Response:** `200 OK` when all checks pass, `503 Service Unavailable` otherwise.

**Authenticated callers** (bearer token or session cookie) see per-component
results:

```json
{
  "status": "ready",
  "checks": {
    "database": "pass",
    "docker": "pass"
  }
}
```

**Unauthenticated callers** see only the aggregate status:

```json
{
  "status": "ready"
}
```

When not all checks pass, `status` is `"not_ready"` and the HTTP status is `503`.

When OIDC and/or OpenBao are configured, their health is included in the checks
(as `"idp"` and `"openbao"` respectively). When AppRole auth is used
(`openbao.role_id`), a `"vault_token"` check reports whether the token renewal
goroutine is healthy.

When served on the [management listener](/guides/observability/#management-listener),
`/readyz` always returns full per-component check details regardless of
authentication.

### `GET /metrics`

Prometheus metrics endpoint. Only available when `telemetry.metrics_enabled` is
`true`. Requires authentication (bearer token or session cookie) when served on
the main listener. No authentication when served on the
[management listener](/guides/observability/#management-listener).

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

| Field | Type | Default | Description |
|---|---|---|---|
| `max_workers_per_app` | `integer` | unlimited | Max concurrent workers (must be >= 1) |
| `max_sessions_per_worker` | `integer` | `1` | Sessions per worker (must be >= 1). `1` means single-tenant containers. See [Credential Management](/guides/credentials/) for how this affects credential injection. |
| `memory_limit` | `string` | none | Container memory limit (e.g. `"512m"`, `"2g"`) |
| `cpu_limit` | `float` | none | CPU limit (e.g. `0.5` for half a core) |
| `access_type` | `string` | `"acl"` | `"acl"`, `"logged_in"`, or `"public"` (requires owner or admin) |
| `title` | `string` | none | Human-readable title for the catalog |
| `description` | `string` | none | Description for the catalog |

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

Grant a user access to an app.

**Request body:**

```json
{
  "principal": "user-sub-123",
  "kind": "user",
  "role": "viewer"
}
```

- `kind` must be `"user"`
- `role` must be `"viewer"` or `"collaborator"`
- You cannot grant access to yourself

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

## Users

Admin-only endpoints for managing user roles and status. Users are created
automatically on first OIDC login.

### `GET /api/v1/users`

List all users.

**Response:** `200 OK`

```json
[
  {
    "sub": "google-oauth2|abc123",
    "email": "alice@example.com",
    "name": "Alice",
    "role": "publisher",
    "active": true,
    "last_login": "2026-03-10T14:00:00Z"
  }
]
```

### `GET /api/v1/users/{sub}`

Get a single user by OIDC `sub`.

**Response:** `200 OK` — user object.

### `PATCH /api/v1/users/{sub}`

Update a user's role or active status. Admin only.

```json
{
  "role": "publisher",
  "active": true
}
```

Both fields are optional. An admin cannot demote or deactivate themselves.

**Response:** `200 OK` — updated user object.

---

## Personal Access Tokens

Manage personal access tokens for API access. See the
[Authorization guide](/guides/authorization/#personal-access-tokens) for
usage details.

### `POST /api/v1/users/me/tokens`

Create a new PAT. **Must be authenticated via OIDC session cookie** — you
cannot use a PAT to create another PAT.

**Request body:**

```json
{ "name": "deploy-ci", "expires_in": "90d" }
```

**Response:** `201 Created`

```json
{
  "id": "tok-abc123",
  "name": "deploy-ci",
  "token": "by_7kJx9mQ2vR...",
  "created_at": "2026-03-14T10:00:00Z",
  "expires_at": "2026-06-12T10:00:00Z"
}
```

The plaintext `token` is returned **only once**. Save it immediately.

### `GET /api/v1/users/me/tokens`

List your PATs (without the plaintext token values).

**Response:** `200 OK` — array of token objects.

### `DELETE /api/v1/users/me/tokens/{tokenID}`

Revoke a single PAT.

**Response:** `204 No Content`

### `DELETE /api/v1/users/me/tokens`

Revoke all your PATs.

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
