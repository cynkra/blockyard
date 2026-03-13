# hello-auth

Blockyard with OIDC authentication (Dex) and per-user secrets (OpenBao).

Extends the [hello-shiny](../hello-shiny/) example with:

- **Dex** — lightweight OIDC identity provider with a static test user
- **OpenBao** — secrets management (dev mode), configured with JWT auth
  backed by Dex
- **Blockyard** — configured for OIDC login + OpenBao credential management

## Prerequisites

- Docker (with Compose v2)
- Add `dex` to your hosts file so the browser can reach the same issuer URL
  that blockyard uses internally:

  ```bash
  echo '127.0.0.1 dex' | sudo tee -a /etc/hosts
  ```

## Usage

```bash
# Start the full stack (Dex, OpenBao, blockyard)
docker compose up -d

# Deploy the hello-shiny app
./deploy.sh

# Open in browser — you'll be redirected to Dex to log in
open http://localhost:8080/app/hello-shiny/
```

### Test credentials

| Field    | Value              |
|----------|--------------------|
| Email    | `demo@example.com` |
| Password | `password`         |

## Architecture

```
Browser
  │
  ├── http://localhost:8080   → blockyard (Shiny apps + API)
  └── http://dex:5556         → Dex (OIDC login redirect)

blockyard ──OIDC──→ dex:5556      (token validation, discovery)
blockyard ──HTTP──→ openbao:8200   (credential storage, JWT→vault token exchange)
openbao   ──JWKS──→ dex:5556      (JWT signature verification)
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
- Dex static passwords do not support group claims. RBAC role mapping via
  `groups_claim` won't apply; use the control-plane API to grant access
  directly if needed.
- The `setup-openbao.sh` script runs once at startup to configure JWT auth,
  policies, and roles in OpenBao.
