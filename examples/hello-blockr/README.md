# hello-blockr

Blockr app with OIDC authentication (Dex), per-user secrets (OpenBao),
and PocketBase for board storage with per-user scoping and sharing.

Two demo users are pre-provisioned to demonstrate board sharing.

## What's included

- **Dex** — lightweight OIDC identity provider with two static test users
- **OpenBao** — secrets management (dev mode) with JWT auth backed by Dex
- **PocketBase** — board storage backend with record-level access rules
- **Blockyard** — configured for OIDC login, OpenBao credential management,
  and pre-enrolled PocketBase credentials for both demo users

## Prerequisites

- Docker (with Compose v2)

## Usage

```bash
# Start the full stack (Dex, OpenBao, PocketBase, blockyard)
docker compose up -d

# Deploy the blockr app
./deploy.sh

# Open in browser — you'll be redirected to Dex to log in
open http://localhost:8080/
```

### Test credentials

Both users share the password `password`.

| User   | Email                 |
|--------|-----------------------|
| User 1 | `demo@example.com`   |
| User 2 | `demo2@example.com`  |

## What the init script does

The `setup.sh` script runs as a one-shot container and configures all
backing services:

1. **OpenBao** — enables JWT auth backed by Dex, creates AppRole auth
   for the blockyard server, sets up policies and roles
2. **PocketBase** — creates a superuser, two regular user accounts,
   updates the users collection rules for user discovery, and creates
   a `boards` collection with record-level rules for per-user scoping
   and targeted sharing
3. **Credential enrollment** — writes PocketBase credentials (email,
   password, URL) to OpenBao for both demo users so they appear
   pre-enrolled on the dashboard

## Architecture

```
Browser
  │
  ├── http://localhost:8080   → blockyard (Shiny apps + API)
  ├── http://localhost:5556   → Dex (OIDC login redirect)
  └── http://localhost:8090   → PocketBase (board storage API + admin UI)

blockyard ──OIDC──→ dex:5556         (discovery, token exchange, JWKS via Docker DNS)
blockyard ──HTTP──→ openbao:8200     (credential storage, JWT→vault token exchange)
openbao   ──JWKS──→ dex:5556         (JWT signature verification via Docker DNS)
worker    ──HTTP──→ openbao:8200     (read user secrets via service network)
worker    ──HTTP──→ pocketbase:8090  (board storage via service network)
```

### Service network

Worker containers run on isolated per-worker bridge networks for security.
To let them reach backing services (OpenBao, PocketBase), those containers
are placed on a shared `blockyard-services` Docker network. At spawn time,
blockyard connects each service container to the worker's network with the
original DNS aliases preserved. Workers can resolve `openbao` and
`pocketbase` by name but cannot discover or reach other workers.

## Services

| Service    | Port | Purpose                              |
|------------|------|--------------------------------------|
| blockyard  | 8080 | Shiny app platform                   |
| dex        | 5556 | OIDC identity provider               |
| openbao    | 8200 | Secrets management (dev mode)        |
| pocketbase | 8090 | Board storage (API + admin UI)       |
| init       | —    | One-shot: configures all services    |

## PocketBase

### Board storage model

The `boards` collection has the following fields:

| Field        | Type     | Purpose                                    |
|--------------|----------|--------------------------------------------|
| `name`       | text     | Board name                                 |
| `data`       | json     | Board content (JSON blob)                  |
| `owner`      | relation | User who created the board                 |
| `shared_with`| relation | Users the board is shared with (multi)     |

### Access rules

- **List/View** — owner sees own boards; shared users see boards shared
  with them
- **Create** — any authenticated user, owner must be self
- **Update/Delete** — owner only

### Admin UI

PocketBase ships with an admin dashboard for inspecting collections,
records, and rules:

```
http://localhost:8090/_/
```

Admin credentials: `admin@pocketbase.local` / `pb-admin-dev-password`

## Cleanup

```bash
docker compose down -v
```

## Notes

- All services run in **dev/ephemeral mode** — data is not persisted
  across restarts.
- Blockyard authenticates to OpenBao via **AppRole** (not a static admin
  token). The `session_secret` is auto-generated and stored in vault.
- PocketBase credentials (email + password) are scoped per user and
  stored at `secret/data/users/{sub}/apikeys/pocketbase` in OpenBao.
  This is the same path pattern used by the credential enrollment UI.
- The app is set to `access_type: logged_in` so both demo users can
  access it without explicit per-user grants.
- Dex's issuer URL is `http://localhost:5556` (what the browser sees).
  Containers reach Dex via Docker DNS (`dex:5556`).
- The `blockyard-services` network makes OpenBao and PocketBase
  reachable from worker containers. Add more services to this network
  in `docker-compose.yml` to make them visible to workers.
