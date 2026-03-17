# Board Storage

Board storage is a **blockr concern**, not a blockyard concern. Blockyard
does not store, read, or mediate access to board data. Its role is limited
to:

1. Authenticating users (OIDC)
2. Injecting credentials into the R session (vault token + env vars)
3. Running the app

The choice of storage backend, the data model, the sharing semantics,
and the path layout are all owned by blockr.

## Requirements

A board is a JSON string. The storage backend must support:

- **Per-user scoping.** Each user sees only their own boards by default.
- **Targeted sharing.** User A can grant user B read access to a specific
  board. User B can fork (copy to their own space).
- **CRUD.** Save, load, list, delete.
- **Versioning** is desirable but not required.

## Recommended Backend: PostgreSQL + PostgREST

PostgreSQL with Row-Level Security (RLS) enforced via PostgREST. This
is the recommended backend because it requires **zero provisioning** —
the user's existing OIDC JWT flows through to the database with no
onboarding hooks, no admin tokens, and no blockyard involvement in the
data path.

### Why This Combination

- **JWT pass-through.** PostgREST validates the user's Dex JWT against
  the IdP's JWKS endpoint. The JWT's `sub` claim becomes the database
  identity. No separate user creation, no credential provisioning.
- **Database-enforced access control.** RLS policies are evaluated by
  PostgreSQL itself, regardless of how the query arrives. A bug in blockr
  or PostgREST cannot bypass them.
- **No admin tokens in the hot path.** The R app sends its JWT as
  `Authorization: Bearer ...` to PostgREST. No shared database password,
  no `SET` tricks, no impersonation risk.
- **Sharing is native SQL.** A `board_shares` table with RLS policies
  handles targeted per-user sharing. No storage-backend-specific ACL
  APIs to learn.
- **Versioning** can be added via a `board_versions` table or
  PostgreSQL triggers. Not required for v2 but straightforward to add.

### Architecture

```
blockr (R app)
  │
  ├── Authorization: Bearer <Dex JWT>
  │
  ▼
PostgREST ──JWKS──→ Dex (JWT signature verification)
  │
  ▼
PostgreSQL (RLS enforces per-user scoping + sharing)
```

Blockyard is not in this path. The R app talks directly to PostgREST.

### Schema

```sql
CREATE TABLE boards (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_sub  TEXT NOT NULL,
  name       TEXT NOT NULL,
  data       JSONB NOT NULL,
  created_at TIMESTAMPTZ DEFAULT now(),
  updated_at TIMESTAMPTZ DEFAULT now(),
  UNIQUE (owner_sub, name)
);

CREATE TABLE board_shares (
  board_id        UUID REFERENCES boards(id) ON DELETE CASCADE,
  shared_with_sub TEXT NOT NULL,
  created_at      TIMESTAMPTZ DEFAULT now(),
  PRIMARY KEY (board_id, shared_with_sub)
);
```

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
ALTER TABLE boards ENABLE ROW LEVEL SECURITY;

-- Owners have full access to their own boards
CREATE POLICY owner_all ON boards
  USING (owner_sub = current_sub())
  WITH CHECK (owner_sub = current_sub());

-- Shared boards are readable by the target user
CREATE POLICY shared_read ON boards FOR SELECT
  USING (EXISTS (
    SELECT 1 FROM board_shares
    WHERE board_id = boards.id
    AND shared_with_sub = current_sub()
  ));

ALTER TABLE board_shares ENABLE ROW LEVEL SECURITY;

-- Only the board owner can manage shares
CREATE POLICY owner_manages_shares ON board_shares
  USING (EXISTS (
    SELECT 1 FROM boards
    WHERE boards.id = board_shares.board_id
    AND boards.owner_sub = current_sub()
  ));

-- Users can see shares targeting them (for discovery)
CREATE POLICY see_own_shares ON board_shares FOR SELECT
  USING (shared_with_sub = current_sub());
```

### Operations from R

The R app uses `httr2` to talk to PostgREST. The vault token is not
needed for board storage — the Dex JWT (available via `X-Shiny-User`
session headers or injected as an environment variable) is the only
credential.

```
Save:   POST   /boards        { owner_sub, name, data }
Load:   GET    /boards?id=eq.{id}
List:   GET    /boards        (RLS filters automatically)
Delete: DELETE /boards?id=eq.{id}
Update: PATCH  /boards?id=eq.{id}  { data, updated_at }
Share:  POST   /board_shares  { board_id, shared_with_sub }
Fork:   GET shared board, POST as new board with own owner_sub
```

### PostgREST Configuration

PostgREST needs:
- The PostgreSQL connection string
- The JWT secret or JWKS URL (Dex's `/.well-known/openid-configuration`)
- A database role to use for authenticated requests (e.g., `blockr_user`)
- An anonymous role with no access (denies unauthenticated requests)

```
db-uri = "postgres://authenticator:password@db:5432/blockyard"
db-schemas = "public"
db-anon-role = "anon"
jwt-aud = "blockyard"
jwt-secret = "@/path/to/jwks.json"  # or fetched from Dex JWKS endpoint
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

### Passing the JWT to the R App

The R app needs the user's Dex JWT (access token) to authenticate to
PostgREST. Blockyard already holds the user's access token in its
server-side session store and refreshes it transparently. The token
can be injected into the R session via an HTTP header on each proxied
request — the same mechanism used for `X-Blockyard-Vault-Token`.

Candidate header: `X-Blockyard-Access-Token`. The R app reads it from
`session$request$HTTP_X_BLOCKYARD_ACCESS_TOKEN` and uses it as the
Bearer token for PostgREST calls. This header is injected per-request
(not per-container), so it always carries the current, refreshed token.

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

## Open Questions

1. **JWT injection.** The `X-Blockyard-Access-Token` header is a new
   injection point. Should this be opt-in (configured per-app or
   globally), or always injected when OIDC is configured? Injecting
   the raw access token into the R process has the same trust model as
   the vault token injection — the R process runs arbitrary code and
   can exfiltrate any header it receives.

2. **Token lifetime.** Dex access tokens are short-lived (5–15 min).
   PostgREST validates the token on each request. For long-running R
   operations (e.g., a board save after an hour of editing), the token
   may have expired. Since blockyard injects the token per-request and
   refreshes transparently, this is only an issue if the R app caches
   the token. Documentation should advise reading the header on each
   save, not storing it at session start.

3. **Schema migrations.** Who owns the PostgreSQL schema for boards?
   If blockr owns the data model, blockr (or its deployment tooling)
   should manage migrations. An `init.sql` in the example is sufficient
   for v2; a migration tool (e.g., golang-migrate, flyway) is a
   production concern.

4. **Future: real-time collaboration.** The current model is
   single-writer (last write wins). See the appendix below for a
   feasibility sketch of real-time collaborative editing and its
   implications for storage. This is out of scope for v2.

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
  board"). These can remain `JSONB` — a materialized view of the
  board state at a point in time. Useful for listing, search, forking,
  and restoring boards outside a live session. The current `data JSONB`
  column serves this role.
- **Dynamic sync state.** The live CRDT document used during
  collaborative editing. Stored as `BYTEA` (or in a separate system
  entirely) and managed by the Yjs persistence layer, not PostgREST.

Whether these live in the same table, separate tables, or separate
systems is TBD. The key constraint: the current `data JSONB` column
and PostgREST CRUD operations remain valid for single-user save/load.
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
