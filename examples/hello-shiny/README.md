# hello-shiny

Minimal Shiny app behind OIDC authentication (Dex). No secrets manager
— just Dex and blockyard.

## What's included

- **Dex** — lightweight OIDC identity provider with a static test user
- **Blockyard** — configured for OIDC login, serving a simple Shiny app

## Prerequisites

- Docker (with Compose v2)

## Usage

```bash
# Start the stack (Dex + blockyard)
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

blockyard ──OIDC──→ dex:5556   (discovery, token exchange, JWKS via Docker DNS)
```

## Services

| Service   | Port | Purpose                |
|-----------|------|------------------------|
| blockyard | 8080 | Shiny app platform     |
| dex       | 5556 | OIDC identity provider |

## Cleanup

```bash
docker compose down -v
```

## Notes

- The `session_secret` is set via an environment variable in
  `docker-compose.yml`. In production, use a strong random value.
- Dex runs with in-memory storage — users and sessions are not persisted
  across restarts.
- Dex's issuer URL is `http://localhost:5556` (what the browser sees).
  Containers reach Dex via Docker DNS (`dex:5556`). Blockyard uses
  `issuer_discovery_url` to perform OIDC discovery and server-side
  requests against the internal address while validating tokens against
  the public issuer URL.
