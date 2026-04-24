---
title: Credential Management
description: How to set up the vault backend and manage per-user credentials for Shiny apps.
weight: 4
---

Blockyard integrates with a Vault-compatible secrets manager
(tested against [OpenBao](https://openbao.org/); HashiCorp Vault also
works) to deliver per-user credentials to Shiny apps at runtime. Each
user registers API keys for external services (AI providers, databases,
object storage, etc.) that are securely injected into their sessions.

## Prerequisites

- OIDC authentication must be configured (see [Configuration](/docs/guides/configuration/))
- A vault instance, initialized and unsealed

## How it works

1. An operator configures the vault and defines which services users can
   enroll credentials for
2. Users store their API keys via the web UI or the REST API
3. When a user visits a Shiny app, Blockyard injects a scoped vault token
   into the request so the app can read that user's credentials

No single compromised component can exfiltrate all user credentials. The
server's vault token is write-scoped — it cannot read user secrets. Only a
valid IdP access token (from an active user session) can produce a read-scoped
token.

## Server configuration

Add the `[vault]` section to your config file. This requires `[oidc]` to
also be configured.

```toml
[vault]
address       = "http://openbao:8200"
role_id       = "blockyard-server"     # AppRole role identifier
token_ttl     = "1h"
jwt_auth_path = "jwt"

[[vault.services]]
id    = "openai"
label = "OpenAI"

[[vault.services]]
id    = "anthropic"
label = "Anthropic"
```

Each `[[vault.services]]` entry defines a third-party service whose API
keys users can enroll. The `id` is used in API paths and as the vault path
segment, `label` is shown in the web UI. Credentials are stored at
`secret/data/users/{sub}/apikeys/{id}`.

## Authentication

Blockyard authenticates to the vault using **AppRole**. This replaces the
previous static `admin_token` approach with a short-lived token Blockyard
refreshes on its own cadence.

### How it works

1. At startup, Blockyard reads the `secret_id` and logs in against the
   AppRole endpoint, obtaining a scoped token and its lease duration.
2. A background goroutine re-logs in shortly before the current token
   expires. On each re-login Blockyard re-reads the `secret_id` source,
   so any rotation that happened on disk is picked up automatically.
3. If any admin call returns 403 — because the token was externally
   revoked, or vault restarted, or clocks drifted — Blockyard re-logs in
   on the spot (coalesced with any in-flight login) and retries the
   request once.

There is no persisted token on disk and no long-running renewal loop.

### Static `secret_id` (env var)

The simplest setup reads `secret_id` once from the process environment:

```bash
BLOCKYARD_VAULT_SECRET_ID="your-secret-id" blockyard
```

The value is used at every login, so the server runs until that
`secret_id` is revoked upstream. Good for deployments where rotation is
tolerable at restart boundaries.

### Rotatable `secret_id` (file)

For deployments that need to rotate `secret_id` without restarting
Blockyard, point `secret_id_file` at a path that a rotation tool (Vault
Agent, a sidecar, a scheduled job) rewrites on its own cadence:

```toml
[vault]
address        = "http://vault:8200"
role_id        = "blockyard-server"
token_ttl      = "1h"
secret_id_file = "/run/secrets/vault_secret_id"
```

Or via the env-var equivalent:

```bash
BLOCKYARD_VAULT_SECRET_ID_FILE=/run/secrets/vault_secret_id blockyard
```

Blockyard re-reads the file on each AppRole login (proactive, 403-driven,
or startup). A rotation written to disk is picked up within one
`token_ttl` of the rotation; shorten `token_ttl` to tighten that window.
The file should be mode `0400`, owned by the Blockyard user, ideally on
`tmpfs`.

When both are set, `secret_id_file` takes precedence.

### Migrating from `admin_token`

The `admin_token` field is deprecated but still accepted. To migrate:

1. Set up AppRole in the vault (enable the auth method, create a policy and role)
2. Replace `admin_token` with `role_id` in your config
3. Set `BLOCKYARD_VAULT_SECRET_ID` (or `secret_id_file`) for startup
4. Remove the old `admin_token` / `BLOCKYARD_VAULT_ADMIN_TOKEN`

## Bootstrapping

On startup, Blockyard verifies the vault is configured correctly:

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
  # Get the scoped vault token from the request header
  vault_token <- session$request$HTTP_X_BLOCKYARD_VAULT_TOKEN
  vault_addr  <- Sys.getenv("VAULT_ADDR")

  # Read your OpenAI API key from the vault
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

  # Then use vault_token to read credentials from the vault (same as above)
}
```

## Environment variables injected into workers

All worker containers receive:

| Variable | Value |
|---|---|
| `SHINY_PORT` | The Shiny port (from `[docker] shiny_port`, default `3838`) |
| `R_LIBS` | The restored package library path — typically `/blockyard-lib`, or `/blockyard-lib-store` when using the shared package store |
| `BLOCKYARD_API_URL` | The server's internal API URL (used for runtime package installs and credential exchange) |

When `[vault]` is configured, workers also receive:

| Variable | Value |
|---|---|
| `VAULT_ADDR` | The vault server address (from `[vault] address`) |
| `BLOCKYARD_VAULT_SERVICES` | JSON map of service IDs to vault paths (only when `[[vault.services]]` are defined) |

## Security model

See the [Credential Trust Model](https://github.com/cynkra/blockyard/blob/main/docs/design/architecture.md#credential-trust-model)
in the architecture documentation for a detailed security analysis.

Key properties:
- No single compromised component yields all user credentials
- The server cannot read stored secrets (admin token is write-scoped)
- A compromised server can only intercept credentials for users with
  active sessions during the window of compromise
- User credentials are encrypted at rest in the vault
