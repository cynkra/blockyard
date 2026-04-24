# PocketBase Rack API Implementation

How each rack operation maps to PocketBase REST API calls. This is
the implementation reference for building the `rack_backend_pb` S3
methods in blockr.session.

See [board-storage.md](../../docs/design/board-storage.md#rack-api-contract)
for the full rack API contract.

## Authentication

The R session authenticates to PocketBase once at session start.
Credentials come from the vault via the vault token that blockyard
injects on each proxied request.

```
# 1. Discover user's sub from vault token metadata.
GET {VAULT_ADDR}/v1/auth/token/lookup-self
    X-Vault-Token: {vault_token}

→ { "data": { "meta": { "sub": "Cg1kZW1vLXVzZXItMDAxEgVsb2NhbA" } } }
```

```
# 2. Read PocketBase credentials from vault.
GET {VAULT_ADDR}/v1/secret/data/users/{sub}/apikeys/pocketbase
    X-Vault-Token: {vault_token}

→ { "data": { "data": { "email": "...", "password": "...", "url": "..." } } }
```

```
# 3. Authenticate to PocketBase.
POST {pb_url}/api/collections/users/auth-with-password
    Content-Type: application/json

    { "identity": "{email}", "password": "{password}" }

→ { "token": "eyJ...", "record": { "id": "user_pb_id", "email": "...", "name": "..." } }
```

The response provides two things the backend object holds for all
subsequent calls:

- **token** — PocketBase JWT, sent as `Authorization: {token}`
  (no `Bearer` prefix in PocketBase v0.25+). Default 14-day
  lifetime outlives any Shiny session.
- **record.id** — the PocketBase user ID, used as the `owner`
  value when creating boards.

### Prerequisites

The vault policy must include `auth/token/lookup-self` (read), and
the JWT auth role must have `claim_mappings: {"sub": "sub"}` so the
sub appears in token metadata. Both are configured in `setup.sh`.

---

## Board CRUD

All requests below include `Authorization: {token}`.

### rack_list

List boards visible to the current user. PocketBase's collection
rules handle access filtering (owner, shared, or public).

```
GET /api/collections/boards/records
    ?sort=-updated
    &fields=id,name,owner,acl_type,tags,created,updated
```

With tag filter:

```
GET /api/collections/boards/records
    ?sort=-updated
    &fields=id,name,owner,acl_type,tags,created,updated
    &filter=tags~'"analysis"'
```

Note: `tags` is a JSON field. The `~` operator does substring
matching on the raw JSON string. This is a pragmatic trade-off —
PocketBase lacks native array containment operators for JSON fields.
For production use, PostgREST + PostgreSQL `TEXT[]` with `cs.{}`
operators is more robust.

**Response:**

```json
{
  "page": 1,
  "perPage": 30,
  "totalItems": 5,
  "items": [
    {
      "id": "abc123",
      "name": "my-board",
      "owner": "user_pb_id",
      "acl_type": "private",
      "tags": ["analysis"],
      "created": "2026-03-19T10:00:00.000Z",
      "updated": "2026-03-19T12:00:00.000Z"
    }
  ]
}
```

### rack_save

Two-step: upsert the board record, then create a new version.

**Step 1 — check if board exists:**

```
GET /api/collections/boards/records
    ?filter=owner='{user_id}' && name='{name}'
    &perPage=1
```

**Step 1a — board does not exist, create:**

```
POST /api/collections/boards/records
    Content-Type: application/json

    {
      "name": "{name}",
      "owner": "{user_id}",
      "acl_type": "private",
      "tags": []
    }

→ { "id": "board_id", ... }
```

**Step 1b — board exists, touch updated timestamp:**

```
PATCH /api/collections/boards/records/{board_id}
    Content-Type: application/json

    {}
```

PocketBase auto-updates the `updated` field on any PATCH.

**Step 2 — create version:**

```
POST /api/collections/board_versions/records
    Content-Type: application/json

    {
      "board": "{board_id}",
      "data": { ... },
      "metadata": { "format": "v1" }
    }

→ { "id": "version_id", "created": "2026-03-19T12:00:00.000Z", ... }
```

The returned version ID and the board ID together form the new
`rack_id_pb`.

### rack_load

**With specific version:**

```
GET /api/collections/board_versions/records/{version_id}
    ?fields=id,board,data,metadata,created
```

**Latest version (no version in rack_id):**

```
GET /api/collections/board_versions/records
    ?filter=board='{board_id}'
    &sort=-created
    &perPage=1
    &fields=id,board,data,metadata,created
```

The R code reads `metadata.format` from the response and dispatches
deserialization accordingly.

### rack_delete

Delete a single version:

```
DELETE /api/collections/board_versions/records/{version_id}

→ 204 No Content
```

### rack_purge

Delete the board and all its versions (cascade delete handles
`board_versions`):

```
DELETE /api/collections/boards/records/{board_id}

→ 204 No Content
```

---

## Versioning

### rack_info

Version history for a board, newest first:

```
GET /api/collections/board_versions/records
    ?filter=board='{board_id}'
    &sort=-created
    &fields=id,created
```

**Response:**

```json
{
  "items": [
    { "id": "v3_id", "created": "2026-03-19T12:00:00.000Z" },
    { "id": "v2_id", "created": "2026-03-18T15:00:00.000Z" },
    { "id": "v1_id", "created": "2026-03-17T09:00:00.000Z" }
  ]
}
```

The R code maps this to `data.frame(version = id, created, hash = NA)`.

---

## Tags

### rack_tags

```
GET /api/collections/boards/records/{board_id}
    ?fields=tags
```

### rack_set_tags

```
PATCH /api/collections/boards/records/{board_id}
    Content-Type: application/json

    { "tags": ["analysis", "demo", "v2"] }
```

Replaces all tags. The R code handles add/remove as read-modify-write.

---

## Visibility

### rack_acl

```
GET /api/collections/boards/records/{board_id}
    ?fields=acl_type
```

### rack_set_acl

```
PATCH /api/collections/boards/records/{board_id}
    Content-Type: application/json

    { "acl_type": "restricted" }
```

---

## Sharing

PocketBase uses record IDs for relations, not OIDC subs. The sharing
operations use PocketBase user IDs — the mapping from "user identity"
to "PocketBase record ID" happens at the backend level. The sharing
UI calls `rack_find_users` first, which returns PocketBase user IDs.

### rack_share

Append a user to the `shared_with` multi-relation:

```
PATCH /api/collections/boards/records/{board_id}
    Content-Type: application/json

    { "shared_with+": ["{user_id}"] }
```

The `+` suffix appends without replacing existing entries.

### rack_unshare

Remove a user from the `shared_with` multi-relation:

```
PATCH /api/collections/boards/records/{board_id}
    Content-Type: application/json

    { "shared_with-": ["{user_id}"] }
```

### rack_shares

Fetch the board with expanded `shared_with` to get user details:

```
GET /api/collections/boards/records/{board_id}
    ?expand=shared_with
    &fields=shared_with,expand.shared_with.id,expand.shared_with.name,expand.shared_with.email,expand.shared_with.created
```

**Response:**

```json
{
  "shared_with": ["user2_id", "user3_id"],
  "expand": {
    "shared_with": [
      { "id": "user2_id", "name": "Demo User 2", "email": "demo2@example.com", "created": "..." },
      { "id": "user3_id", "name": "Demo User 3", "email": "demo3@example.com", "created": "..." }
    ]
  }
}
```

---

## User Discovery

### rack_find_users

Search for users by name or email, excluding the current user:

```
GET /api/collections/users/records
    ?filter=(name~'{query}' || email~'{query}') && id!='{current_user_id}'
    &fields=id,name,email
```

This works because `setup.sh` sets the users collection list/view
rules to `@request.auth.id != ""` — any authenticated user can
discover other users.

---

## Capabilities

No API call needed. The PocketBase backend reports all capabilities:

```r
rack_capabilities.rack_backend_pb <- function(backend, ...) {
  list(
    versioning     = TRUE,
    tags           = TRUE,
    metadata       = TRUE,
    sharing        = TRUE,
    visibility     = TRUE,
    user_discovery = TRUE
  )
}
```

---

## Error Handling

PocketBase returns structured errors:

```json
{
  "code": 400,
  "message": "Failed to create record.",
  "data": {
    "name": {
      "code": "validation_required",
      "message": "Missing required value."
    }
  }
}
```

| HTTP status | Meaning                        | rack behavior              |
|-------------|--------------------------------|----------------------------|
| 200         | Success (GET, PATCH)           | Parse response             |
| 201         | Created (POST)                 | Parse response             |
| 204         | Deleted (DELETE)               | Return invisible           |
| 400         | Validation / rule violation    | Error with details         |
| 401         | Token expired or invalid       | Re-authenticate and retry  |
| 403         | Access denied by record rules  | Error (not your board)     |
| 404         | Record not found               | Error (board/version gone) |

The rack layer wraps these into R conditions (warnings/errors) with
custom classes for programmatic handling.
