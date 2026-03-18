# Board Storage

Board storage is a **blockr concern**, not a blockyard concern. Blockyard
does not store, read, or mediate access to board data. Its role is limited
to:

1. Authenticating users (OIDC)
2. Injecting credentials into the R session (access token + env vars)
3. Running the app

The choice of storage backend, the data model, the sharing semantics,
and the path layout are all owned by blockr.

## Requirements

A board is a JSON string. The storage backend must support:

- **Per-user scoping.** Each user sees only their own boards by default.
- **Targeted sharing.** User A can grant user B read access to a specific
  board. User B can fork (copy to their own space).
- **CRUD.** Save, load, list, delete.
- **Versioning.** Each save creates a new version; loading retrieves the
  most recent version.

## Recommended Backend: PostgreSQL + PostgREST

PostgreSQL with Row-Level Security (RLS) enforced via PostgREST. This
is the recommended backend because it requires **zero provisioning** —
the user's existing OIDC JWT flows through to the database with no
onboarding hooks, no admin tokens, and no blockyard involvement in the
data path.

Blockyard provisions the schema (via its own migration system —
`golang-migrate` with embedded SQL files) and PostgREST roles.
PostgREST validates JWTs against the IdP's JWKS endpoint. PostgreSQL
evaluates RLS policies on every query. Blockyard is not in the data
path at runtime.

See [v2/plan.md](v2/plan.md) (Phase 2-4) for the full implementation
plan including migration files, JWT injection, and example updates.

### Why This Combination

- **JWT pass-through.** PostgREST validates the user's OIDC access token
  against the IdP's JWKS endpoint. The JWT's `sub` claim becomes the
  database identity. No separate user creation, no credential
  provisioning.
- **Database-enforced access control.** RLS policies are evaluated by
  PostgreSQL itself, regardless of how the query arrives. A bug in blockr
  or PostgREST cannot bypass them.
- **No admin tokens in the hot path.** The R app sends its JWT as
  `Authorization: Bearer ...` to PostgREST. No shared database password,
  no `SET` tricks, no impersonation risk.
- **Sharing is native SQL.** A `board_shares` table with RLS policies
  handles targeted per-user sharing. No storage-backend-specific ACL
  APIs to learn.

### Architecture

```
blockr (R app)
  │
  ├── Authorization: Bearer <OIDC access token>
  │
  ▼
PostgREST ──JWKS──→ IdP (JWT signature verification)
  │
  ▼
PostgreSQL (RLS enforces per-user scoping + sharing)
```

Blockyard is not in this path. The R app talks directly to PostgREST.

### Schema

Board identity and access control are separated from versioned data.
The `boards` table holds metadata and sharing semantics; the
`board_versions` table holds immutable snapshots. This ensures ACL
settings and tags are per-board, not per-version — sharing a board
means sharing all its versions.

```sql
-- PostgREST roles
CREATE ROLE blockr_user NOLOGIN;
GRANT USAGE ON SCHEMA public TO blockr_user;
CREATE ROLE anon NOLOGIN;

-- Board identity and metadata
CREATE TABLE boards (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub   TEXT NOT NULL,
    board_id    TEXT NOT NULL,
    acl_type    TEXT NOT NULL DEFAULT 'private'
                CHECK (acl_type IN ('private', 'public', 'restricted')),
    tags        TEXT[] DEFAULT '{}',
    created_at  TIMESTAMPTZ DEFAULT now(),
    updated_at  TIMESTAMPTZ DEFAULT now(),
    UNIQUE (owner_sub, board_id)
);

-- Versioned board data
CREATE TABLE board_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_sub   TEXT NOT NULL,
    board_id    TEXT NOT NULL,
    data        JSONB NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now(),
    FOREIGN KEY (owner_sub, board_id)
        REFERENCES boards(owner_sub, board_id) ON DELETE CASCADE
);

CREATE INDEX idx_board_versions_lookup
    ON board_versions(owner_sub, board_id, created_at DESC);

-- Sharing (for restricted ACL)
CREATE TABLE board_shares (
    owner_sub       TEXT NOT NULL,
    board_id        TEXT NOT NULL,
    shared_with_sub TEXT NOT NULL,
    created_at      TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (owner_sub, board_id, shared_with_sub),
    FOREIGN KEY (owner_sub, board_id)
        REFERENCES boards(owner_sub, board_id) ON DELETE CASCADE
);
```

Three visibility modes via `acl_type`:

| `acl_type` | Who can read |
|---|---|
| `private` | Owner only. Default. |
| `public` | Anyone with a valid JWT. |
| `restricted` | Owner + users listed in `board_shares`. |

### Identity Helper

PostgREST sets the JWT claims as a PostgreSQL session variable. A helper
function extracts the `sub` claim:

```sql
CREATE FUNCTION current_sub() RETURNS TEXT AS $$
  SELECT current_setting('request.jwt.claims', true)::json->>'sub'
$$ LANGUAGE sql STABLE;
```

### RLS Policies

```sql
-- boards
ALTER TABLE boards ENABLE ROW LEVEL SECURITY;

CREATE POLICY owner_all ON boards
  USING (owner_sub = current_sub())
  WITH CHECK (owner_sub = current_sub());

CREATE POLICY public_read ON boards FOR SELECT
  USING (acl_type = 'public');

CREATE POLICY restricted_read ON boards FOR SELECT
  USING (acl_type = 'restricted' AND EXISTS (
      SELECT 1 FROM board_shares
      WHERE board_shares.owner_sub = boards.owner_sub
      AND board_shares.board_id = boards.board_id
      AND board_shares.shared_with_sub = current_sub()
  ));

-- board_versions (inherits access from parent board)
ALTER TABLE board_versions ENABLE ROW LEVEL SECURITY;

CREATE POLICY version_owner ON board_versions
  USING (owner_sub = current_sub())
  WITH CHECK (owner_sub = current_sub());

CREATE POLICY version_public ON board_versions FOR SELECT
  USING (EXISTS (
      SELECT 1 FROM boards
      WHERE boards.owner_sub = board_versions.owner_sub
      AND boards.board_id = board_versions.board_id
      AND boards.acl_type = 'public'
  ));

CREATE POLICY version_restricted ON board_versions FOR SELECT
  USING (EXISTS (
      SELECT 1 FROM boards b
      JOIN board_shares bs
          ON b.owner_sub = bs.owner_sub AND b.board_id = bs.board_id
      WHERE b.owner_sub = board_versions.owner_sub
      AND b.board_id = board_versions.board_id
      AND b.acl_type = 'restricted'
      AND bs.shared_with_sub = current_sub()
  ));

-- board_shares
ALTER TABLE board_shares ENABLE ROW LEVEL SECURITY;

CREATE POLICY shares_owner ON board_shares
  USING (owner_sub = current_sub())
  WITH CHECK (owner_sub = current_sub());

CREATE POLICY shares_see_own ON board_shares FOR SELECT
  USING (shared_with_sub = current_sub());

-- Grant PostgREST access
GRANT SELECT, INSERT, UPDATE, DELETE ON boards, board_versions, board_shares
    TO blockr_user;
```

### Operations from R

The R app uses `httr2` to talk to PostgREST. The OIDC access token
(available via `session$request$HTTP_X_BLOCKYARD_ACCESS_TOKEN`) is the
only credential needed — no vault token required for board storage.

```
Save:    POST   /boards          { owner_sub, board_id }
                                 → creates board metadata
         POST   /board_versions  { owner_sub, board_id, data }
                                 → creates versioned snapshot
Load:    GET    /board_versions?owner_sub=eq.{sub}&board_id=eq.{id}
                                 &order=created_at.desc&limit=1
List:    GET    /boards          (RLS filters automatically)
Delete:  DELETE /boards?owner_sub=eq.{sub}&board_id=eq.{id}
Share:   POST   /board_shares   { owner_sub, board_id, shared_with_sub }
Tags:    PATCH  /boards?owner_sub=eq.{sub}&board_id=eq.{id}
                                 { tags: ["analysis", "demo"] }
Fork:    Load shared board, POST as new board with own owner_sub
```

### Passing the JWT to the R App

Blockyard injects the user's OIDC access token as an
`X-Blockyard-Access-Token` HTTP header on every proxied request when
OIDC is configured — the same injection pattern as the existing
`X-Blockyard-Vault-Token`. The header is injected per-request (not
per-container), so it always carries the current, refreshed token.

The R app reads it from
`session$request$HTTP_X_BLOCKYARD_ACCESS_TOKEN` and uses it as the
Bearer token for PostgREST calls. Since the access token is short-lived
(typically 5–15 minutes, configured on the IdP), the R app should read
the header on each save operation rather than caching it at session
start. Blockyard refreshes the token transparently via the OIDC refresh
flow, so the header always carries a valid token as long as the user's
session is active.

The trust model is the same as vault token injection: the R process
runs arbitrary code and can exfiltrate any header it receives.
Injecting the access token does not change the blast radius.

### PostgREST Configuration

PostgREST needs:
- The PostgreSQL connection string
- The JWKS URL from the IdP's `/.well-known/openid-configuration`
- A database role for authenticated requests (`blockr_user`)
- An anonymous role with no access (denies unauthenticated requests)

```
db-uri = "postgres://authenticator:password@db:5432/blockyard"
db-schemas = "public"
db-anon-role = "anon"
jwt-aud = "blockyard"
jwt-secret = "@/path/to/jwks.json"  # or fetched from IdP JWKS endpoint
```

### Example Docker Compose Services

```yaml
postgres:
  image: postgres:17
  environment:
    POSTGRES_DB: blockyard
    POSTGRES_PASSWORD: dev-password
  volumes:
    - ./init.sql:/docker-entrypoint-initdb.d/init.sql:ro
    - pgdata:/var/lib/postgresql/data
  healthcheck:
    test: ["CMD", "pg_isready"]
    interval: 5s
    retries: 10

postgrest:
  image: postgrest/postgrest:v12
  depends_on:
    postgres:
      condition: service_healthy
  environment:
    PGRST_DB_URI: postgres://authenticator:dev-password@postgres:5432/blockyard
    PGRST_DB_SCHEMAS: public
    PGRST_DB_ANON_ROLE: anon
    PGRST_JWT_AUD: blockyard
    PGRST_JWT_SECRET: "@/etc/postgrest/jwks.json"
  ports:
    - "3001:3000"
```

## Alternative Backends

The PostgreSQL + PostgREST combination is recommended because it
requires no provisioning and enforces access control at the database
level. However, blockr is storage-agnostic. Any backend works if:

1. The R app can obtain credentials for it (typically from vault)
2. The backend supports per-user scoping and sharing

For backends that require per-user credentials (S3, PocketBase, Gitea,
etc.), the operator provisions credentials and stores them in OpenBao
at `secret/data/users/{sub}/apikeys/{service}`. Blockyard's existing
credential injection (vault token + `VAULT_ADDR`) delivers them to the
R app at runtime. No blockyard code changes are needed.

| Backend               | Provisioning         | Sharing model             | Versioning  |
|-----------------------|----------------------|---------------------------|-------------|
| PostgreSQL + PostgREST | None (JWT)          | RLS + shares table        | Via schema  |
| PocketBase            | User + token → vault | Record-level rules        | Manual      |
| S3 / MinIO            | Access key → vault   | Bucket policies (limited) | Via object versions |
| Gitea                 | User + token → vault | Collaborators (per-repo)  | Git history |
| Vault KV v2           | None (existing token)| Broadcast only (no targeted sharing) | Built-in |

## Appendix: Real-Time Collaborative Editing

A feasibility sketch for multi-user real-time board editing. Out of
scope for v2 but relevant to storage design decisions made now.

### Approach

A board decomposes naturally into CRDT-friendly structures:

- **Set of blocks** → add-wins set (concurrent add + remove of the
  same block resolves to "block survives").
- **Block parameters** → last-writer-wins register per field.
- **Connections** → add-wins set of edges.
- **Layout positions** → last-writer-wins register per block.

Libraries like [Yjs](https://github.com/yjs/yjs) and
[Automerge](https://automerge.org/) implement these primitives as
document CRDTs with proven sync protocols.

### Architecture

Yjs runs in the browser (JavaScript). R does not need CRDT logic —
it exchanges granular operations with the Yjs document over Shiny's
existing custom-message channel (the same pattern blockr.dock uses
with dockview for layout state). A [y-websocket][yws] server relays
changes between browsers.

[yws]: https://github.com/yjs/y-websocket

```
Browser A (Yjs)  ←—ws—→  y-websocket  ←—ws—→  Browser B (Yjs)
      ↕ Shiny msg               │                  ↕ Shiny msg
  R session A              persistence          R session B
```

Outgoing: user edits a parameter → R sends a granular operation to
JS → JS applies it to the Yjs doc → Yjs syncs to other browsers.

Incoming: Yjs observer fires → JS sends the operation to R via
`Shiny.setInputValue()` → R updates reactive state → UI updates.

### Storage Implications

The Yjs document is persisted as an opaque binary encoding, not
application-level JSON. This creates a potential separation between
two kinds of board data:

- **Static save-points.** Explicit user-initiated snapshots ("save
  board"). These remain `JSONB` in `board_versions` — a materialized
  view of the board state at a point in time. Useful for listing,
  search, forking, and restoring boards outside a live session.
- **Dynamic sync state.** The live CRDT document used during
  collaborative editing. Stored as `BYTEA` (or in a separate system
  entirely) and managed by the Yjs persistence layer, not PostgREST.

Whether these live in the same table, separate tables, or separate
systems is TBD. The key constraint: the `board_versions` table and
PostgREST CRUD operations remain valid for single-user save/load.
Real-time sync adds a parallel storage path, it does not replace the
existing one.

### Auth Integration

The y-websocket server must verify the user's JWT and check board
permissions (owner or shared-with) before granting access to a
document. This likely requires a query against the `boards` /
`board_shares` tables or a call to PostgREST. The `board_shares`
model would also need to support write access, not just read.

### Open Conflict Semantics

- User A deletes a block while User B edits its parameters — does the
  block survive (add-wins) or disappear (delete-wins)?
- Concurrent connection additions could create cycles in a DAG.
  CRDTs do not enforce structural invariants — application-level
  validation is needed.
- CRDT documents grow over time (tombstones). A compaction / garbage
  collection strategy is needed for long-lived boards.
