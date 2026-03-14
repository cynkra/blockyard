---
title: Configuration File
description: Full reference for the blockyard.toml configuration file.
---

import { Aside } from '@astrojs/starlight/components';

Blockyard reads its configuration from a TOML file. The default path is
`blockyard.toml` in the working directory. Override it with the
`--config` CLI argument:

```bash
blockyard --config /etc/blockyard/config.toml
```

## Environment variable overrides

Every configuration field can be set via an environment variable. The naming
convention is:

```
BLOCKYARD_<SECTION>_<FIELD>
```

All uppercased. For example, `[server] bind` becomes `BLOCKYARD_SERVER_BIND`.

Environment variables take precedence over values in the TOML file.

## `[server]`

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "change-me-in-production"
shutdown_timeout = "30s"
# session_secret = "random-secret"   # required when [oidc] is configured
# external_url   = "https://blockyard.example.com"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `bind` | `string` | `0.0.0.0:8080` | No | Socket address to listen on |
| `token` | `string` | — | When `[oidc]` is **not** set | Static bearer token for API authentication (v0 only; replaced by [Personal Access Tokens](/guides/authorization/#personal-access-tokens) when OIDC is configured) |
| `shutdown_timeout` | `duration` | `30s` | No | Grace period for draining requests on shutdown |
| `session_secret` | `string` | — | When `[oidc]` is set | Secret for encrypting session cookies |
| `external_url` | `string` | — | No | Public-facing URL of the server (used for OIDC redirect URIs) |

<Aside type="caution">
  When running without OIDC (v0 mode), use a strong, randomly generated
  token. When OIDC is configured, the static token is ignored — use
  Personal Access Tokens instead.
</Aside>

## `[docker]`

```toml
[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/rocker-org/r-ver:4.4.3"
shiny_port = 3838
rv_version = "v0.19.0"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `socket` | `string` | `/var/run/docker.sock` | No | Path to Docker or Podman socket |
| `image` | `string` | — | **Yes** | Base image for worker and build containers |
| `shiny_port` | `integer` | `3838` | No | Port Shiny listens on inside containers |
| `rv_version` | `string` | `v0.19.0` | No | [rv](https://github.com/a2-ai/rv) release tag (e.g. `v0.19.0`) |

## `[storage]`

```toml
[storage]
bundle_server_path = "/data/bundles"
bundle_worker_path = "/app"
bundle_retention   = 50
max_bundle_size    = 104857600
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `bundle_server_path` | `path` | `/data/bundles` | No | Directory for storing uploaded bundles |
| `bundle_worker_path` | `path` | `/app` | No | Mount point inside worker containers |
| `bundle_retention` | `integer` | `50` | No | Max bundles kept per app (oldest pruned first) |
| `max_bundle_size` | `integer` | `104857600` | No | Maximum bundle upload size in bytes (default 100 MB) |

## `[database]`

```toml
[database]
path = "/data/db/blockyard.db"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `path` | `path` | `/data/db/blockyard.db` | No | Path to the SQLite database file (created if missing) |

## `[proxy]`

```toml
[proxy]
ws_cache_ttl         = "60s"
health_interval      = "15s"
worker_start_timeout = "60s"
max_workers          = 100
log_retention        = "1h"
session_idle_ttl     = "1h"
idle_worker_timeout  = "5m"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `ws_cache_ttl` | `duration` | `60s` | No | Time to keep a backend WebSocket alive after client disconnects |
| `health_interval` | `duration` | `15s` | No | How often workers are health-checked |
| `worker_start_timeout` | `duration` | `60s` | No | Max time to wait for a new worker to become healthy |
| `max_workers` | `integer` | `100` | No | Global cap on concurrent worker containers |
| `log_retention` | `duration` | `1h` | No | How long to keep worker log entries before cleanup |
| `session_idle_ttl` | `duration` | `1h` | No | Time before an idle session is cleaned up |
| `idle_worker_timeout` | `duration` | `5m` | No | Time before an idle worker container is stopped |

## `[oidc]` *(optional)*

Enable OIDC-based authentication. When this section is present, `server.session_secret` is required.

```toml
[oidc]
issuer_url     = "https://idp.example.com/realms/myapp"
client_id      = "blockyard"
client_secret  = "oidc-client-secret"
cookie_max_age = "24h"
initial_admin  = "google-oauth2|abc123"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `issuer_url` | `string` | — | **Yes** | OIDC provider issuer URL |
| `client_id` | `string` | — | **Yes** | OIDC client ID |
| `client_secret` | `string` | — | **Yes** | OIDC client secret |
| `cookie_max_age` | `duration` | `24h` | No | Maximum lifetime of session cookies |
| `initial_admin` | `string` | — | No | OIDC `sub` of the first admin user. Checked only on first login. See [First Admin Setup](/guides/authorization/#first-admin-setup). |

<Aside type="caution">
  When OIDC is configured, the proxy routes (`/app/{name}/`) enforce
  authentication. Users must log in before accessing apps (except for apps
  with `public` visibility).
</Aside>

## `[openbao]` *(optional)*

Enable OpenBao (Vault-compatible) credential management. Requires `[oidc]` to also be configured.

```toml
[openbao]
address     = "http://openbao:8200"
admin_token = "vault-admin-token"
token_ttl   = "1h"
jwt_auth_path = "jwt"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `address` | `string` | — | **Yes** | OpenBao server address |
| `admin_token` | `string` | — | **Yes** | Admin token for OpenBao API access |
| `token_ttl` | `duration` | `1h` | No | TTL for issued credential tokens |
| `jwt_auth_path` | `string` | `jwt` | No | Auth method mount path in OpenBao |

### `[[openbao.services]]`

Define third-party services whose API keys users can enroll via OpenBao. Each
entry must have `id`, `label`, and `path`. Service IDs must be unique.

```toml
[[openbao.services]]
id    = "openai"
label = "OpenAI"
path  = "openai"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `id` | `string` | — | **Yes** | Unique identifier for the service |
| `label` | `string` | — | **Yes** | Human-readable label shown to users |
| `path` | `string` | — | **Yes** | KV store path prefix for user credentials |

## `[audit]` *(optional)*

Enable append-only audit logging to a JSONL file.

```toml
[audit]
path = "/data/audit/blockyard.jsonl"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `path` | `path` | — | **Yes** | Path to the JSONL audit log file |

## `[telemetry]` *(optional)*

Enable observability features: Prometheus metrics and OpenTelemetry tracing.

```toml
[telemetry]
metrics_enabled = true
otlp_endpoint   = "http://otel-collector:4317"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `metrics_enabled` | `boolean` | `false` | No | Expose a `/metrics` endpoint for Prometheus scraping |
| `otlp_endpoint` | `string` | — | No | OpenTelemetry collector gRPC endpoint for distributed tracing |
