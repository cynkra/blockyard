# Phase 2-4: Board Storage (PostgreSQL + PostgREST + Vault Identity OIDC)

PostgreSQL-backed board storage with PostgREST as the REST API layer
and vault's Identity OIDC provider for authentication. Blockyard owns
the schema; vault issues JWTs; PostgREST validates them; PostgreSQL
enforces access control via Row-Level Security. Blockyard is not in the
data path at runtime.

## Architecture

```
blockr (R app)
  │
  ├── 1. Vault token from X-Blockyard-Vault-Token (existing flow)
  ├── 2. GET /identity/oidc/token/postgrest → vault-signed JWT
  ├── 3. Authorization: Bearer <vault-issued JWT>
  │
  ▼
PostgREST ──JWKS──→ OpenBao (/identity/oidc/.well-known/keys)
  │
  ▼
PostgreSQL (RLS enforces per-user scoping + sharing)
```

Blockyard provisions the schema and injects `POSTGREST_URL` into
worker containers. Auth is entirely between vault, PostgREST, and
PostgreSQL — blockyard is not in the security-critical path.

### Trust model

The security of this architecture hinges on PostgREST trusting the
correct JWKS. If an attacker could substitute their own public keys,
they could sign arbitrary JWTs and bypass all RLS policies.

**The trust anchor is vault's OIDC signing key.** The private key
never leaves vault. PostgREST reads the corresponding public keys
from a JWKS file (downloaded from vault's unauthenticated endpoint
`/identity/oidc/.well-known/keys` during init).

Attack surfaces and protections:

- **JWKS download (init container → vault):** If this connection is
  intercepted (MITM), an attacker could inject their own public keys.
  **Protection:** TLS for vault communication in production. In the
  docker-compose dev setup, vault and the init container share an
  internal Docker network.

- **JWKS file on disk:** PostgREST reads the JWKS from a file on a
  Docker volume. If an attacker can write to this volume, they can
  replace the keys. **Protection:** the volume is mounted read-only
  into the PostgREST container. Only the init/refresh process has
  write access. The volume is not mounted into worker containers.

- **Vault API reachable from workers:** Worker containers must reach
  vault — the R app reads secrets and requests OIDC tokens from it.
  However, worker vault tokens are policy-scoped: they can read
  secrets and request OIDC tokens (`identity/oidc/token/postgrest`),
  but they cannot modify vault's OIDC configuration
  (`identity/oidc/key/*`, `identity/oidc/role/*`). The OIDC signing
  key is protected by vault's own ACL — reading the public key from
  the JWKS endpoint does not help an attacker forge tokens.

- **JWKS refresh on key rotation:** Vault rotates the OIDC signing
  key on a configurable period (default 24h). The old public key
  remains available for `verification_ttl` (default 2h) so in-flight
  tokens validate. A sidecar or cron job re-downloads the JWKS and
  signals PostgREST to reload (`SIGUSR2` or `NOTIFY pgrst`). The
  same TLS + file permission protections apply to the refresh path.

### Why vault Identity OIDC (not direct OIDC pass-through)

Shiny apps establish a single HTTP request, then switch to WebSocket.
The R app reads headers once via `session$request` — there is no
mechanism to receive updated headers mid-session. OIDC access tokens
are short-lived (5–15 minutes) and would expire before the user saves
their first board.

Vault's Identity OIDC provider solves this: the R app already has a
vault token (existing credential injection flow). It requests
PostgREST JWTs from vault on demand — no blockyard involvement, no
header refresh needed. The vault token is renewable by the R app
itself via `POST /auth/token/renew-self`.

## Deliverables

1. **Board schema migration** — `migrations/postgres/004_v2_boards.up.sql`
   creates the `boards`, `board_versions`, `board_shares` tables, RLS
   policies, and PostgREST roles. PostgreSQL only — SQLite deployments
   skip this migration.
2. **Config addition** — `[board_storage]` section with `postgrest_url`.
3. **PostgREST URL injection** — inject `POSTGREST_URL` as a container
   environment variable when configured.
4. **Vault Identity OIDC setup** — operator/init-container configures
   vault's Identity secrets engine to issue PostgREST JWTs.
5. **Example** — docker-compose with PostgreSQL + PostgREST + vault
   Identity OIDC, demonstrating board save/load/share from blockr.

---

## Step 1: Migration 004 — board schema (PostgreSQL only)

Add migration files under `internal/db/migrations/postgres/`.

No SQLite migration — board storage requires PostgreSQL (PostgREST
cannot run against SQLite). SQLite deployments are unaffected.

**`postgres/004_v2_boards.up.sql`:**

```sql
-- PostgREST roles
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'blockr_user') THEN
        CREATE ROLE blockr_user NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'anon') THEN
        CREATE ROLE anon NOLOGIN;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO blockr_user;

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
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
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

-- Identity helper for RLS.
-- Reads keycloak_sub (custom claim) because vault's Identity OIDC
-- provider hardcodes the standard sub to the vault entity ID. The
-- keycloak_sub claim carries the original IdP subject.
CREATE FUNCTION current_sub() RETURNS TEXT AS $$
    SELECT current_setting('request.jwt.claims', true)::json->>'keycloak_sub'
$$ LANGUAGE sql STABLE;

-- RLS: boards
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

-- RLS: board_versions (inherits access from parent board)
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

-- RLS: board_shares
ALTER TABLE board_shares ENABLE ROW LEVEL SECURITY;

CREATE POLICY shares_owner ON board_shares
    USING (owner_sub = current_sub())
    WITH CHECK (owner_sub = current_sub());

CREATE POLICY shares_see_own ON board_shares FOR SELECT
    USING (shared_with_sub = current_sub());

-- Auto-update boards.updated_at on row modification
CREATE FUNCTION update_boards_timestamp() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER boards_updated_at
    BEFORE UPDATE ON boards
    FOR EACH ROW EXECUTE FUNCTION update_boards_timestamp();

-- Auto-update boards.updated_at when a new version is inserted.
-- The trigger runs as the table owner, but the RLS policy on boards
-- still evaluates current_sub() — which matches, because the user
-- who can insert a version necessarily owns the board.
CREATE FUNCTION update_board_on_version() RETURNS TRIGGER AS $$
BEGIN
    UPDATE boards SET updated_at = now()
    WHERE owner_sub = NEW.owner_sub AND board_id = NEW.board_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER version_updates_board
    AFTER INSERT ON board_versions
    FOR EACH ROW EXECUTE FUNCTION update_board_on_version();

-- Grant PostgREST access
GRANT SELECT, INSERT, UPDATE, DELETE ON boards, board_versions, board_shares
    TO blockr_user;

-- User discovery: expose the existing users table (populated by
-- blockyard's OIDC login flow) so PostgREST can serve user search
-- queries for the sharing UI. No new table needed.
GRANT SELECT ON users TO blockr_user;
```

**`postgres/004_v2_boards.down.sql`:**

```sql
REVOKE SELECT ON users FROM blockr_user;

DROP TRIGGER IF EXISTS version_updates_board ON board_versions;
DROP TRIGGER IF EXISTS boards_updated_at ON boards;
DROP FUNCTION IF EXISTS update_board_on_version();
DROP FUNCTION IF EXISTS update_boards_timestamp();

DROP TABLE IF EXISTS board_shares CASCADE;
DROP TABLE IF EXISTS board_versions CASCADE;
DROP TABLE IF EXISTS boards CASCADE;
DROP FUNCTION IF EXISTS current_sub();

-- Do not drop blockr_user/anon roles — they may be used by other
-- services. Role cleanup is an operator concern.
```

### Key schema decisions

- **`current_sub()` reads `keycloak_sub`, not `sub`.** Vault's
  Identity OIDC provider hardcodes the JWT `sub` claim to the vault
  entity ID (a UUID). The original IdP subject is emitted as a custom
  `keycloak_sub` claim via the OIDC role's claims template (see
  Step 4). All RLS policies use `current_sub()`, so this is the only
  place this mapping is defined.

- **Metadata separate from versions.** The `boards` table holds
  identity, access control, and tags. The `board_versions` table holds
  immutable data snapshots. ACL settings are per-board, not
  per-version — sharing a board shares all its versions.

- **User discovery via existing `users` table.** Blockyard already
  maintains a `users` table (populated during OIDC login) with `sub`,
  `name`, `email`. Migration 004 grants `SELECT` on it to
  `blockr_user` so PostgREST exposes it automatically. The R app
  queries `GET /users?name=like.*query*&select=sub,name,email` for
  the sharing UI. No duplication — blockyard's OIDC login flow keeps
  the data current.

- **`updated_at` auto-maintained via triggers.** The `boards` table
  has an `updated_at` column that is automatically updated by two
  triggers: one on direct board row updates, one on `board_versions`
  inserts. The R app never needs to explicitly set `updated_at`.

- **Three visibility modes via `acl_type`:**

  | `acl_type`   | Who can read                             |
  |--------------|------------------------------------------|
  | `private`    | Owner only. Default.                     |
  | `public`     | Anyone with a valid JWT.                 |
  | `restricted` | Owner + users listed in `board_shares`.  |

## Step 2: Config addition — `[board_storage]` section

Add a `BoardStorageConfig` struct to `internal/config/config.go`:

```go
type BoardStorageConfig struct {
    PostgrestURL string `toml:"postgrest_url"`
}
```

Add the field to `Config`:

```go
type Config struct {
    // ... existing fields ...
    BoardStorage *BoardStorageConfig `toml:"board_storage"`
}
```

The pointer type means `board_storage` is `nil` when the TOML section
is absent — same pattern as `oidc`, `openbao`, etc. The config section
is constructed automatically when `BLOCKYARD_BOARD_STORAGE_POSTGREST_URL`
is set, via the existing `applyEnvOverrides` machinery.

**Validation (in `validate()`):** when `board_storage` is configured:

- Require `database.driver = "postgres"` — board storage needs
  PostgREST, which needs PostgreSQL. The board tables live in
  blockyard's own database.
- Require `[openbao]` — the R app needs vault to obtain PostgREST
  JWTs via Identity OIDC. Without vault, there is no token issuer.

The `WorkerEnv()` function returns `nil` when openbao is not
configured, so this validation also ensures `POSTGREST_URL` is
actually injected into worker containers.

```toml
[board_storage]
# URL of the PostgREST instance. When set, POSTGREST_URL is injected
# into worker containers. Requires driver = "postgres".
postgrest_url = "http://postgrest:3000"
```

## Step 3: PostgREST URL injection in WorkerEnv

In `internal/proxy/coldstart.go`, extend `WorkerEnv()` to inject
`POSTGREST_URL` when board storage is configured:

```go
func WorkerEnv(srv *server.Server) map[string]string {
    // ... existing env setup ...

    // Board storage: inject PostgREST URL so R apps can discover it.
    if srv.Config.BoardStorage != nil && srv.Config.BoardStorage.PostgrestURL != "" {
        env["POSTGREST_URL"] = srv.Config.BoardStorage.PostgrestURL
    }

    return env
}
```

The R app reads `Sys.getenv("POSTGREST_URL")` to discover PostgREST.
No vault token or OIDC configuration is passed via env vars — the R
app obtains its PostgREST JWT from vault at runtime (see Step 4).

## Step 4: Vault Identity OIDC setup

This is operator/init-container configuration, not blockyard code.
The vault Identity secrets engine is auto-enabled — no
`secrets enable` needed.

The setup uses the existing JWT auth method (already configured for
credential injection). The additional steps configure vault to issue
PostgREST-scoped JWTs.

### 4a. Ensure JWT auth maps the IdP subject to entity metadata

The existing JWT auth role (`blockyard-user`) already sets
`user_claim="sub"`. Add `claim_mappings` to copy the IdP `sub` into
entity alias metadata as `keycloak_sub` (belt and suspenders for
template references — the OIDC template uses `.name` which is already
the IdP subject, but having it in metadata under a descriptive key
makes the mapping explicit):

```bash
bao write auth/jwt/role/blockyard-user \
    role_type="jwt" \
    user_claim="sub" \
    claim_mappings='{"sub":"keycloak_sub"}' \
    bound_audiences="blockyard" \
    token_policies="blockyard-user" \
    token_ttl="1h"
```

With `user_claim="sub"`, the entity alias **name** is the IdP `sub`
value. This is what the OIDC token template reads via
`identity.entity.aliases.${JWT_ACCESSOR}.name`.

### 4b. Create an OIDC named key

```bash
bao write identity/oidc/key/postgrest \
    allowed_client_ids="*" \
    verification_ttl="2h" \
    rotation_period="24h" \
    algorithm="RS256"
```

### 4c. Create an OIDC role with claims template

The template emits the original IdP subject as `keycloak_sub` and a
fixed `role` for PostgREST role switching:

```bash
# Get the JWT auth accessor for template references
JWT_ACCESSOR=$(bao auth list -format=json | jq -r '.["jwt/"].accessor')

TEMPLATE=$(cat <<EOF
{
  "keycloak_sub": {{identity.entity.aliases.${JWT_ACCESSOR}.name}},
  "role": "blockr_user"
}
EOF
)

bao write identity/oidc/role/postgrest \
    key="postgrest" \
    client_id="postgrest" \
    ttl="1h" \
    template="$(echo "${TEMPLATE}" | base64 -w0)"
```

- `client_id="postgrest"` sets the JWT `aud` claim — PostgREST
  validates this via `jwt-aud`.
- `ttl="1h"` — the R app requests a new token from vault when the
  current one expires. 1h balances security with refresh frequency.
- `keycloak_sub` carries the original IdP subject — this is what
  `current_sub()` in PostgreSQL reads for RLS evaluation.
- `role` is `"blockr_user"` — PostgREST does
  `SET LOCAL ROLE blockr_user` for every authenticated request.

### 4d. Update the vault policy

Add the OIDC token endpoint and token self-renewal to the existing
`blockyard-user` policy:

```bash
bao policy write blockyard-user - <<EOF
# Existing: read per-user secrets
path "secret/data/users/{{identity.entity.aliases.${JWT_ACCESSOR}.name}}/*" {
  capabilities = ["read"]
}

# Board storage: request PostgREST JWTs from vault
path "identity/oidc/token/postgrest" {
  capabilities = ["read"]
}

# Token lifecycle: lookup TTL + renew (existing lookup-self preserved)
path "auth/token/lookup-self" {
  capabilities = ["read"]
}
path "auth/token/renew-self" {
  capabilities = ["update"]
}
EOF
```

### 4e. Configure the OIDC issuer (optional)

```bash
bao write identity/oidc/config \
    issuer="http://openbao:8200"
```

If not set, vault uses its own address. Set this when vault is behind
a reverse proxy or when the address workers use differs from the
address blockyard uses.

## Step 5: PostgREST configuration

PostgREST validates JWTs against vault's JWKS endpoint. The JWKS is
fetched from vault and stored as a file (PostgREST does not support
remote JWKS URLs natively).

### Init container: download JWKS

```bash
curl -s http://openbao:8200/v1/identity/oidc/.well-known/keys \
    > /shared/vault-jwks.json
```

The JWKS file is mounted into the PostgREST container. On vault key
rotation (default 24h), a sidecar or cron job refreshes the file and
sends `SIGUSR2` to PostgREST to reload.

### PostgREST environment

```yaml
environment:
  PGRST_DB_URI: postgres://authenticator:${DB_PASSWORD}@postgres:5432/blockyard
  PGRST_DB_SCHEMAS: public
  PGRST_DB_ANON_ROLE: anon
  PGRST_JWT_AUD: postgrest
  PGRST_JWT_SECRET: "@/etc/postgrest/vault-jwks.json"
  PGRST_JWT_ROLE_CLAIM_KEY: ".role"
```

- `jwt-aud = "postgrest"` matches the `client_id` on the vault OIDC
  role (Step 4c).
- `jwt-role-claim-key = ".role"` extracts the PostgREST database role
  from the JWT's `role` claim (set to `"blockr_user"` in the template).
- `jwt-secret = "@/etc/postgrest/vault-jwks.json"` loads the JWKS
  from the file written by the init container.

### Database roles

The `authenticator` role connects to PostgreSQL and switches to
`blockr_user` or `anon` per-request:

```sql
-- Run once during database provisioning (init container or setup script)
CREATE ROLE authenticator LOGIN PASSWORD '...';
GRANT blockr_user TO authenticator;
GRANT anon TO authenticator;
```

## Step 6: R app flow (blockr concern)

The R app's interaction with PostgREST is a blockr concern — no
blockyard code changes needed. Documented here for completeness.

### Token acquisition

```r
# At session start: read vault token from existing header injection
vault_token <- session$request$HTTP_X_BLOCKYARD_VAULT_TOKEN
vault_addr  <- Sys.getenv("VAULT_ADDR")

# Request a PostgREST JWT from vault's Identity OIDC provider
postgrest_jwt <- get_postgrest_token(vault_addr, vault_token)

# The JWT is valid for 1h (configurable). Before expiry, request a new one.
# Vault token renewal: POST /auth/token/renew-self
```

### Board operations via PostgREST

```r
postgrest_url <- Sys.getenv("POSTGREST_URL")

# Save board
httr2::request(paste0(postgrest_url, "/boards")) |>
  httr2::req_auth_bearer_token(postgrest_jwt) |>
  httr2::req_body_json(list(owner_sub = my_sub, board_id = "my-board")) |>
  httr2::req_method("POST") |>
  httr2::req_perform()

# Load latest version
httr2::request(paste0(postgrest_url, "/board_versions")) |>
  httr2::req_auth_bearer_token(postgrest_jwt) |>
  httr2::req_url_query(
    owner_sub = paste0("eq.", my_sub),
    board_id = "eq.my-board",
    order = "created_at.desc",
    limit = 1
  ) |>
  httr2::req_perform()

# List boards (RLS filters automatically — user sees own + shared + public)
httr2::request(paste0(postgrest_url, "/boards")) |>
  httr2::req_auth_bearer_token(postgrest_jwt) |>
  httr2::req_perform()

# Find users to share with (queries blockyard's users table via PostgREST)
httr2::request(paste0(postgrest_url, "/users")) |>
  httr2::req_auth_bearer_token(postgrest_jwt) |>
  httr2::req_url_query(
    name = "like.*query*",
    select = "sub,name,email"
  ) |>
  httr2::req_perform()

# Share with another user
httr2::request(paste0(postgrest_url, "/board_shares")) |>
  httr2::req_auth_bearer_token(postgrest_jwt) |>
  httr2::req_body_json(list(
    owner_sub = my_sub,
    board_id = "my-board",
    shared_with_sub = "other-user-sub"
  )) |>
  httr2::req_method("POST") |>
  httr2::req_perform()
```

### Token lifecycle

The R app manages two tokens:

1. **Vault token** — received once via `X-Blockyard-Vault-Token` at
   session start. Renewable via `POST /auth/token/renew-self` (no
   blockyard involvement). Default TTL 1h, renewable.

2. **PostgREST JWT** — requested from vault on demand via
   `GET /identity/oidc/token/postgrest`. Default TTL 1h. When it
   expires, the R app requests a new one (requires a valid vault
   token).

For typical sessions (< 1h), no renewal is needed. For longer
sessions, the R app renews the vault token before it expires, then
requests fresh PostgREST JWTs as needed.

## Step 7: Docker compose example

A new `examples/hello-blockr-postgrest/` directory alongside the
existing `examples/hello-blockr/` (PocketBase). The two examples
demonstrate different board storage patterns and have fundamentally
different configurations — the PostgREST example requires PostgreSQL
as blockyard's primary database, while the PocketBase example uses
SQLite. Keeping them separate avoids confusion.

The docker-compose adds PostgreSQL (as blockyard's database) and
PostgREST as services:

```yaml
postgrest:
  image: postgrest/postgrest:v12
  depends_on:
    postgres:
      condition: service_healthy
  environment:
    PGRST_DB_URI: postgres://authenticator:dev-password@postgres:5432/blockyard
    PGRST_DB_SCHEMAS: public
    PGRST_DB_ANON_ROLE: anon
    PGRST_JWT_AUD: postgrest
    PGRST_JWT_SECRET: "@/etc/postgrest/vault-jwks.json"
    PGRST_JWT_ROLE_CLAIM_KEY: ".role"
  volumes:
    - jwks:/etc/postgrest:ro
  networks:
    - blockyard-services
```

The init container extends the existing OpenBao setup with the
Identity OIDC steps from Step 4.

## Step 8: Tests

### Unit tests

- **Config validation:** verify that `board_storage` requires
  `database.driver = "postgres"` and `[openbao]`.
- **WorkerEnv:** verify `POSTGREST_URL` is present when board storage
  is configured, absent when not.

### Integration tests (PostgreSQL required)

- **Migration up/down:** verify migration 004 applies and rolls back
  cleanly.
- **RLS policies:** with a test PostgREST-like setup (set role +
  GUC), verify:
  - Owner can CRUD their own boards, versions, and shares.
  - Public boards are visible to other users (SELECT).
  - Private boards are invisible to other users.
  - Restricted boards are visible only to shared users.
  - `board_versions` inherit access from parent `boards`.
  - `board_shares` are manageable only by owner.
  - `current_sub()` correctly reads `keycloak_sub` from JWT claims.
  - `users` table is readable by `blockr_user` (user discovery).
  - `boards.updated_at` auto-updates on board row changes and
    version inserts.

RLS tests can run without PostgREST by using `SET LOCAL ROLE` and
`SET LOCAL request.jwt.claims` directly in test transactions:

```sql
BEGIN;
SET LOCAL ROLE blockr_user;
SET LOCAL "request.jwt.claims" = '{"keycloak_sub": "user-a"}';
-- now run queries and verify RLS behavior
ROLLBACK;
```

### E2E tests (full stack)

- Deploy blockr app with PostgREST board storage.
- Save a board, load it back, verify data round-trips.
- Share a board with another user, verify the shared user can read it.
- Verify private boards are invisible to other users.

---

## Summary of blockyard code changes

| File | Change |
|------|--------|
| `internal/db/migrations/postgres/004_v2_boards.up.sql` | Board schema, RLS policies, PostgREST roles, user discovery grant, triggers |
| `internal/db/migrations/postgres/004_v2_boards.down.sql` | Revoke grants, drop triggers, drop board tables |
| `internal/config/config.go` | Add `BoardStorageConfig` struct and field; validate requires postgres + openbao |
| `internal/proxy/coldstart.go` | Inject `POSTGREST_URL` in `WorkerEnv()` |

No proxy changes, no new headers, no new API endpoints. The existing
vault token injection (`X-Blockyard-Vault-Token`) and env var
injection (`VAULT_ADDR`) provide everything the R app needs to obtain
PostgREST JWTs from vault.
