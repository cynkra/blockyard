# hello

Blockyard with OIDC authentication (Dex), per-user secrets (OpenBao),
and credential enrollment.

## What's included

- **Dex** — lightweight OIDC identity provider with a static test user
- **OpenBao** — secrets management (dev mode) with JWT auth backed by Dex
- **Blockyard** — configured for OIDC login, OpenBao credential management,
  and a sample credential enrollment service (OpenAI API Key)

## Prerequisites

- Docker (with Compose v2)

## Usage

```bash
# Start the full stack (Dex, OpenBao, blockyard)
docker compose up -d

# Deploy the hello app
./deploy.sh

# Open in browser — you'll be redirected to Dex to log in
open http://localhost:8080/
```

### Test credentials

| Field    | Value              |
|----------|--------------------|
| Email    | `demo@example.com` |
| Password | `password`         |

## What the deploy script does

The `deploy.sh` script automates the full bootstrap flow:

1. Waits for blockyard to be healthy
2. Performs an OIDC login against Dex using the static demo credentials
3. Creates a short-lived Personal Access Token (PAT) via the API
4. Creates the app, uploads the bundle, restores dependencies, and starts it

No manual browser interaction is needed for deployment.

## Architecture

```
Browser
  │
  ├── http://localhost:8080   → blockyard (Shiny apps + API)
  └── http://localhost:5556   → Dex (OIDC login redirect)

blockyard ──OIDC──→ dex:5556         (discovery, token exchange, JWKS via Docker DNS)
blockyard ──HTTP──→ openbao:8200     (credential storage, JWT→vault token exchange)
openbao   ──JWKS──→ dex:5556         (JWT signature verification via Docker DNS)
```

## Services

| Service        | Port | Purpose                          |
|----------------|------|----------------------------------|
| blockyard      | 8080 | Shiny app platform               |
| dex            | 5556 | OIDC identity provider           |
| openbao        | 8200 | Secrets management (dev mode)    |
| openbao-init   | —    | One-shot: configures OpenBao     |

## Cleanup

```bash
docker compose down -v
```

## Notes

- OpenBao runs in **dev mode** — data is not persisted across restarts.
- Dex static passwords do not support group claims. Use the API to grant
  access directly if needed.
- The `setup-openbao.sh` script runs once at startup to configure JWT auth,
  AppRole auth, policies, and roles in OpenBao.
- Blockyard authenticates to OpenBao via **AppRole** (not a static admin
  token). The `session_secret` is auto-generated and stored in vault —
  no manual secret configuration is needed.
- The credential enrollment section on the dashboard lets users store an
  OpenAI API key in OpenBao. This is configured via `blockyard.toml`.
- Dex's issuer URL is `http://localhost:5556` (what the browser sees).
  Containers reach Dex via Docker DNS (`dex:5556`). Blockyard uses
  `issuer_discovery_url` to perform OIDC discovery and server-side
  requests against the internal address while validating tokens against
  the public issuer URL.
