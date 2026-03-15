---
title: Configuration
description: How to configure a Blockyard server.
---

Blockyard is configured via a TOML file. By default, it looks for
`blockyard.toml` in the current directory. Override the path with the
`--config` CLI argument:

```bash
blockyard --config /etc/blockyard/config.toml
```

Every config field can also be overridden via environment variables using the
pattern `BLOCKYARD_<SECTION>_<FIELD>` (uppercased). For example:

```bash
BLOCKYARD_SERVER_BIND=0.0.0.0:9090
BLOCKYARD_DOCKER_IMAGE=ghcr.io/rocker-org/r-ver:4.4.0
```

## Sections

### `[server]`

| Field | Default | Description |
|---|---|---|
| `bind` | `0.0.0.0:8080` | Address and port the server listens on |
| `shutdown_timeout` | `30s` | Time to drain in-flight requests on shutdown |
| `log_level` | `info` | Log verbosity: `trace`, `debug`, `info`, `warn`, `error` |
| `management_bind` | — | Separate listener for `/healthz`, `/readyz`, `/metrics`. See [Observability](/guides/observability/#management-listener). |
| `session_secret` | — | Secret for signing session cookies. Required when `[oidc]` is set without `[openbao]`; auto-generated and stored in vault when `[openbao]` is configured. |
| `external_url` | — | Public-facing URL of the server (used for OIDC redirect URIs) |

### `[docker]`

| Field | Default | Description |
|---|---|---|
| `socket` | `/var/run/docker.sock` | Path to the Docker (or Podman) socket |
| `image` | *(required)* | Base container image for workers and builds |
| `shiny_port` | `3838` | Port Shiny listens on inside the container |
| `rv_version` | `v0.19.0` | `rv` release tag to use for dependency restoration |

### `[storage]`

| Field | Default | Description |
|---|---|---|
| `bundle_server_path` | `/data/bundles` | Directory for uploaded bundles |
| `bundle_worker_path` | `/app` | Mount point inside worker containers |
| `bundle_retention` | `50` | Max bundles to keep per app before cleanup |
| `max_bundle_size` | `104857600` | Maximum upload size in bytes (default 100 MB) |

### `[database]`

| Field | Default | Description |
|---|---|---|
| `path` | `/data/db/blockyard.db` | Path to the SQLite database file |

### `[proxy]`

| Field | Default | Description |
|---|---|---|
| `ws_cache_ttl` | `60s` | How long to keep a backend WebSocket open after client disconnect |
| `health_interval` | `15s` | Interval between worker health checks |
| `worker_start_timeout` | `60s` | Max time to wait for a worker to become healthy |
| `max_workers` | `100` | Maximum number of concurrent worker containers |
| `log_retention` | `1h` | How long to keep worker log entries before cleanup |
| `session_idle_ttl` | `1h` | Time before an idle session is cleaned up |
| `idle_worker_timeout` | `5m` | Time before an idle worker container is stopped |

### `[oidc]` *(optional)*

Enable OIDC authentication. When configured, `server.session_secret` is required unless `[openbao]` is also configured (in which case it can be auto-generated).

| Field | Default | Description |
|---|---|---|
| `issuer_url` | *(required)* | OIDC provider issuer URL |
| `client_id` | *(required)* | OIDC client ID |
| `client_secret` | *(required)* | OIDC client secret |
| `cookie_max_age` | `24h` | Max lifetime of session cookies |
| `initial_admin` | — | OIDC `sub` of the first admin user. Checked only on first login. See [First Admin Setup](/guides/authorization/#first-admin-setup). |

### `[openbao]` *(optional)*

Enable OpenBao credential management. Requires `[oidc]` to be configured.

| Field | Default | Description |
|---|---|---|
| `address` | *(required)* | OpenBao server address |
| `role_id` | One of `role_id` or `admin_token` | AppRole role identifier. The `secret_id` is delivered via `BLOCKYARD_OPENBAO_SECRET_ID` env var at bootstrap. |
| `admin_token` | One of `role_id` or `admin_token` | **Deprecated.** Static admin token. Use `role_id` with AppRole auth instead. |
| `token_ttl` | `1h` | TTL for issued tokens |
| `jwt_auth_path` | `jwt` | Auth method path in OpenBao |

#### `[[openbao.services]]`

Define third-party services whose API keys users can enroll via OpenBao.

```toml
[[openbao.services]]
id    = "openai"
label = "OpenAI"
path  = "openai"
```

| Field | Default | Description |
|---|---|---|
| `id` | *(required)* | Unique identifier for the service |
| `label` | *(required)* | Human-readable label |
| `path` | *(required)* | KV store path prefix for credentials |

### `[audit]` *(optional)*

Enable append-only audit logging.

| Field | Default | Description |
|---|---|---|
| `path` | *(required)* | Path to the JSONL audit log file |

### `[telemetry]` *(optional)*

Enable Prometheus metrics and OpenTelemetry tracing.

| Field | Default | Description |
|---|---|---|
| `metrics_enabled` | `false` | Expose a `/metrics` endpoint for Prometheus |
| `otlp_endpoint` | — | OpenTelemetry collector endpoint (e.g. `http://otel-collector:4317`) |

## Example

```toml
[server]
bind             = "0.0.0.0:8080"
shutdown_timeout = "30s"

[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/rocker-org/r-ver:4.4.3"
shiny_port = 3838
rv_version = "v0.19.0"

[storage]
bundle_server_path = "/data/bundles"
bundle_worker_path = "/app"
bundle_retention   = 50
max_bundle_size    = 104857600

[database]
path = "/data/db/blockyard.db"

[proxy]
ws_cache_ttl         = "60s"
health_interval      = "15s"
worker_start_timeout = "60s"
max_workers          = 100
log_retention        = "1h"
session_idle_ttl     = "1h"
idle_worker_timeout  = "5m"

# Optional: OIDC authentication
# When enabled, server.session_secret is required unless [openbao] is also
# configured (in which case it is auto-generated and stored in vault).
# [oidc]
# issuer_url     = "https://idp.example.com/realms/myapp"
# client_id      = "blockyard"
# client_secret  = "oidc-client-secret"
# cookie_max_age = "24h"
# initial_admin  = "google-oauth2|abc123"   # OIDC sub of the first admin

# Optional: OpenBao credential management (requires [oidc])
# [openbao]
# address       = "http://openbao:8200"
# role_id       = "blockyard-server"       # AppRole role identifier (recommended)
# # admin_token = "vault-admin-token"      # deprecated: use role_id instead
# token_ttl     = "1h"
# jwt_auth_path = "jwt"
#
# [[openbao.services]]
# id    = "openai"
# label = "OpenAI"
# path  = "openai"

# Optional: Audit logging
# [audit]
# path = "/data/audit/blockyard.jsonl"

# Optional: Telemetry
# [telemetry]
# metrics_enabled = true
# otlp_endpoint   = "http://otel-collector:4317"
```
