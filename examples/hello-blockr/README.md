# hello-blockr

Blockr app with OIDC authentication (Dex), per-user secrets (OpenBao),
and S3-compatible object storage (Garage) with pre-provisioned
user-scoped credentials.

## What's included

- **Dex** — lightweight OIDC identity provider with a static test user
- **OpenBao** — secrets management (dev mode) with JWT auth backed by Dex
- **Garage** — S3-compatible object storage (single-node dev mode)
- **Blockyard** — configured for OIDC login, OpenBao credential management,
  and a pre-enrolled S3 credential pair for the demo user

## Prerequisites

- Docker (with Compose v2)

## Usage

```bash
# Start the full stack (Dex, OpenBao, Garage, blockyard)
docker compose up -d

# Deploy the blockr app
./deploy.sh

# Open in browser — you'll be redirected to Dex to log in
open http://localhost:8080/
```

### Test credentials

| Field    | Value              |
|----------|--------------------|
| Email    | `demo@example.com` |
| Password | `password`         |

## What the init script does

The `setup.sh` script runs as a one-shot container and configures all
backing services:

1. **OpenBao** — enables JWT auth backed by Dex, creates AppRole auth
   for the blockyard server, sets up policies and roles
2. **Garage** — assigns the single node to a cluster layout, creates an
   S3 access key and bucket (`blockyard-demo`)
3. **Credential enrollment** — writes the generated S3 credentials to
   OpenBao at the demo user's credential path so they appear
   pre-enrolled on the dashboard

## Architecture

```
Browser
  │
  ├── http://localhost:8080   → blockyard (Shiny apps + API)
  ├── http://localhost:5556   → Dex (OIDC login redirect)
  └── http://localhost:3900   → Garage S3 API (optional direct access)

blockyard ──OIDC──→ dex:5556         (discovery, token exchange, JWKS via Docker DNS)
blockyard ──HTTP──→ openbao:8200     (credential storage, JWT→vault token exchange)
openbao   ──JWKS──→ dex:5556         (JWT signature verification via Docker DNS)
```

## Services

| Service   | Port | Purpose                              |
|-----------|------|--------------------------------------|
| blockyard | 8080 | Shiny app platform                   |
| dex       | 5556 | OIDC identity provider               |
| openbao   | 8200 | Secrets management (dev mode)        |
| garage    | 3900 | S3-compatible object storage         |
| init      | —    | One-shot: configures all services    |

## S3 access

The demo user's S3 credentials are pre-enrolled in OpenBao and visible
on the dashboard. You can also access the Garage S3 API directly:

```bash
# List bucket contents (using awscli)
aws --endpoint-url http://localhost:3900 \
    --region garage \
    s3 ls s3://blockyard-demo/
```

The access key and secret are generated dynamically by the init script.
Check the init container logs for the values:

```bash
docker compose logs init | grep accessKeyId
```

## Cleanup

```bash
docker compose down -v
```

## Notes

- All services run in **dev/ephemeral mode** — data is not persisted
  across restarts.
- Garage runs as a single-node cluster with `replication_factor = 1`.
- Blockyard authenticates to OpenBao via **AppRole** (not a static admin
  token). The `session_secret` is auto-generated and stored in vault.
- The S3 credential pair (access key + secret key) is scoped to the demo
  user and stored at `secret/data/users/{sub}/apikeys/s3` in OpenBao.
  This is the same path pattern used by the credential enrollment UI.
- Dex's issuer URL is `http://localhost:5556` (what the browser sees).
  Containers reach Dex via Docker DNS (`dex:5556`).
