---
title: Credential Management
description: How to set up OpenBao and manage per-user credentials for Shiny apps.
---

Blockyard integrates with [OpenBao](https://openbao.org/) (a Vault-compatible
secrets manager) to deliver per-user credentials to Shiny apps at runtime.
This allows each user to register API keys for external services (AI providers,
databases, object storage, etc.) that are securely injected into their
sessions.

## Prerequisites

- OIDC authentication must be configured (see [Configuration](/guides/configuration/))
- An OpenBao (or HashiCorp Vault) instance, initialized and unsealed

## How it works

1. An operator configures OpenBao and defines which services users can
   enroll credentials for
2. Users store their API keys via the web UI or the REST API
3. When a user visits a Shiny app, Blockyard injects a scoped OpenBao token
   into the request so the app can read that user's credentials

No single compromised component can exfiltrate all user credentials. The
server's OpenBao admin token is write-scoped — it cannot read user secrets.
Only a valid IdP access token (from an active user session) can produce a
read-scoped token.

## Server configuration

Add the `[openbao]` section to your config file. This requires `[oidc]` to
also be configured.

```toml
[openbao]
address       = "http://openbao:8200"
admin_token   = "hvs.your-admin-token"     # use BLOCKYARD_OPENBAO_ADMIN_TOKEN env var
token_ttl     = "1h"
jwt_auth_path = "jwt"

[[openbao.services]]
id    = "openai"
label = "OpenAI"
path  = "openai"

[[openbao.services]]
id    = "anthropic"
label = "Anthropic"
path  = "anthropic"
```

Each `[[openbao.services]]` entry defines a third-party service whose API
keys users can enroll. The `id` is used in API paths, `label` is shown in
the web UI, and `path` is the KV store path prefix.

## Bootstrapping

On startup, Blockyard automatically configures OpenBao:

- Enables the KV v2 secrets engine at `secret/`
- Configures JWT auth with your IdP's JWKS endpoint
- Creates per-user policies that restrict each user to reading only their
  own secrets (`secret/users/{sub}/*`)

The admin token must have sufficient permissions for these bootstrap
operations.

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

| Variable | Value |
|---|---|
| `VAULT_ADDR` | The OpenBao server address (from `[openbao] address`) |
| `BLOCKYARD_API_URL` | The server's API URL (shared container mode only) |

## Security model

See the [Credential Trust Model](https://github.com/cynkra/blockyard/blob/main/docs/design/architecture.md#credential-trust-model)
in the architecture documentation for a detailed security analysis.

Key properties:
- No single compromised component yields all user credentials
- The server cannot read stored secrets (admin token is write-scoped)
- A compromised server can only intercept credentials for users with
  active sessions during the window of compromise
- User credentials are encrypted at rest in OpenBao
