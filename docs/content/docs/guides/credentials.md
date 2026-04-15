---
title: Credential Management
description: How to set up OpenBao and manage per-user credentials for Shiny apps.
weight: 4
---

Blockyard integrates with [OpenBao](https://openbao.org/) (a Vault-compatible
secrets manager) to deliver per-user credentials to Shiny apps at runtime.
This allows each user to register API keys for external services (AI providers,
databases, object storage, etc.) that are securely injected into their
sessions.

## Prerequisites

- OIDC authentication must be configured (see [Configuration](/docs/guides/configuration/))
- An OpenBao (or HashiCorp Vault) instance, initialized and unsealed

## How it works

1. An operator configures OpenBao and defines which services users can
   enroll credentials for
2. Users store their API keys via the web UI or the REST API
3. When a user visits a Shiny app, Blockyard injects a scoped OpenBao token
   into the request so the app can read that user's credentials

No single compromised component can exfiltrate all user credentials. The
server's OpenBao token is write-scoped — it cannot read user secrets. Only a
valid IdP access token (from an active user session) can produce a read-scoped
token.

## Server configuration

Add the `[openbao]` section to your config file. This requires `[oidc]` to
also be configured.

```toml
[openbao]
address       = "http://openbao:8200"
role_id       = "blockyard-server"     # AppRole role identifier
token_ttl     = "1h"
jwt_auth_path = "jwt"

[[openbao.services]]
id    = "openai"
label = "OpenAI"

[[openbao.services]]
id    = "anthropic"
label = "Anthropic"
```

Each `[[openbao.services]]` entry defines a third-party service whose API
keys users can enroll. The `id` is used in API paths and as the vault path
segment, `label` is shown in the web UI. Credentials are stored at
`secret/data/users/{sub}/apikeys/{id}`.

## Authentication

Blockyard authenticates to OpenBao using **AppRole**. This replaces the
previous static `admin_token` approach with a short-lived, revocable
credential.

### Initial bootstrap

1. Configure the AppRole role in OpenBao (see the `setup-openbao.sh` script
   in the hello-pocketbase example for reference)
2. Set `role_id` in your config (this is a role identifier, not a secret)
3. Deliver the `secret_id` via env var or file:

```bash
BLOCKYARD_OPENBAO_SECRET_ID="your-secret-id" blockyard
# or
BLOCKYARD_OPENBAO_SECRET_ID_FILE=/run/secrets/blockyard-secret-id blockyard
```

On startup, blockyard reads `secret_id` from the configured source, performs
an AppRole login, and caches the resulting token in process memory.

### Steady state

The server holds the AppRole-issued token for the duration of its process.
If OpenBao returns 403 to any admin-scoped call — because the token
expired, was revoked, or the `secret_id` rotated — blockyard transparently
re-reads its credentials (re-reading any `_FILE` source) and performs a
fresh AppRole login, then retries the original request once. Concurrent
403 retries share one login attempt (singleflight).

There is no proactive renewal loop and no on-disk token. Operators who
want shorter-lived tokens can set `token_ttl` on the AppRole role to
e.g. 5–15 minutes; the server will simply re-login more often.

### Rotating `secret_id` without a restart

Deliver `secret_id` via `BLOCKYARD_OPENBAO_SECRET_ID_FILE` pointing at a
file managed by Vault Agent, a scheduled `bao write`, or another rotation
tool. When the file's contents change, the new `secret_id` is picked up
at the next AppRole login — which happens at the first 403 after the
previous token expired or was revoked. Mounting the file on tmpfs with
mode `0400` owned by the blockyard user further narrows exposure.

### Migrating from `admin_token`

The `admin_token` field is deprecated but still accepted. To migrate:

1. Set up AppRole in OpenBao (enable the auth method, create a policy and role)
2. Replace `admin_token` with `role_id` in your config
3. Set `BLOCKYARD_OPENBAO_SECRET_ID` (or `_FILE`) for the first startup
4. Remove the old `admin_token` / `BLOCKYARD_OPENBAO_ADMIN_TOKEN`

## Bootstrapping

On startup, Blockyard verifies OpenBao is configured correctly:

- KV v2 secrets engine is mounted at `secret/`
- JWT auth is configured with your IdP
- Per-user policies restrict each user to reading only their own secrets
  (`secret/users/{sub}/*`)

The AppRole token must have sufficient permissions for these checks and for
writing user secrets.

## Enrolling credentials

### Via the web UI

Log in to Blockyard and navigate to the dashboard. Each configured service
has an enrollment form where you can enter your API key.

### Via the REST API

```bash
curl -X POST https://blockyard.example.com/api/v1/users/me/credentials/openai \
  -H "Authorization: Bearer by_..." \
  -H "Content-Type: application/json" \
  -d '{"api_key": "sk-proj-abc123..."}'
```

The service name in the URL path must match a configured service `id`.

## Reading credentials in Shiny apps

Blockyard injects credentials differently depending on the worker mode:

### Single-tenant mode (`max_sessions_per_worker = 1`, default)

The proxy injects an `X-Blockyard-Vault-Token` header on each request. The
R process reads it directly:

```r
server <- function(input, output, session) {
  # Get the scoped OpenBao token from the request header
  vault_token <- session$request$HTTP_X_BLOCKYARD_VAULT_TOKEN
  vault_addr  <- Sys.getenv("VAULT_ADDR")

  # Read your OpenAI API key from OpenBao
  resp <- httr2::request(vault_addr) |>
    httr2::req_url_path("/v1/secret/data/users", session$request$HTTP_X_SHINY_USER, "openai") |>
    httr2::req_headers("X-Vault-Token" = vault_token) |>
    httr2::req_perform()

  api_key <- httr2::resp_body_json(resp)$data$data$api_key
}
```

### Shared containers (`max_sessions_per_worker > 1`)

In shared mode, the proxy injects an `X-Blockyard-Session-Token` header
instead. The app exchanges it for a vault token via a server callback:

```r
server <- function(input, output, session) {
  session_token <- session$request$HTTP_X_BLOCKYARD_SESSION_TOKEN
  api_url       <- Sys.getenv("BLOCKYARD_API_URL")

  # Exchange session token for a vault token
  resp <- httr2::request(api_url) |>
    httr2::req_url_path("/api/v1/credentials/vault") |>
    httr2::req_headers("Authorization" = paste("Bearer", session_token)) |>
    httr2::req_method("POST") |>
    httr2::req_perform()

  vault_token <- httr2::resp_body_json(resp)$token

  # Then use vault_token to read credentials from OpenBao (same as above)
}
```

## Environment variables injected into workers

All worker containers receive:

| Variable | Value |
|---|---|
| `SHINY_PORT` | The Shiny port (from `[docker] shiny_port`, default `3838`) |
| `R_LIBS` | The restored package library path — typically `/blockyard-lib`, or `/blockyard-lib-store` when using the shared package store |
| `BLOCKYARD_API_URL` | The server's internal API URL (used for runtime package installs and credential exchange) |

When `[openbao]` is configured, workers also receive:

| Variable | Value |
|---|---|
| `VAULT_ADDR` | The OpenBao server address (from `[openbao] address`) |
| `BLOCKYARD_VAULT_SERVICES` | JSON map of service IDs to Vault paths (only when `[[openbao.services]]` are defined) |

## Security model

See the [Credential Trust Model](https://github.com/cynkra/blockyard/blob/main/docs/design/architecture.md#credential-trust-model)
in the architecture documentation for a detailed security analysis.

Key properties:
- No single compromised component yields all user credentials
- The server cannot read stored secrets (admin token is write-scoped)
- A compromised server can only intercept credentials for users with
  active sessions during the window of compromise
- User credentials are encrypted at rest in OpenBao
