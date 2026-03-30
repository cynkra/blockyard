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
bind             = "127.0.0.1:8080"
shutdown_timeout = "30s"
# management_bind = "127.0.0.1:9100"
# log_level      = "info"
# session_secret = "random-secret"   # required when [oidc] is configured
# external_url   = "https://blockyard.example.com"
# trusted_proxies = ["10.0.0.0/8"]
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `bind` | `string` | `127.0.0.1:8080` | No | Socket address to listen on |
| `management_bind` | `string` | — | No | Separate listener for `/healthz`, `/readyz`, `/metrics`. See [Management listener](/guides/observability/#management-listener). |
| `shutdown_timeout` | `duration` | `30s` | No | Grace period for draining requests on shutdown |
| `log_level` | `string` | `info` | No | Log verbosity. One of `trace`, `debug`, `info`, `warn` (or `warning`), `error`. |
| `session_secret` | `string` | — | When `[oidc]` is set without `[openbao]` | Secret for signing session cookies. Supports [vault references](#vault-references). Auto-generated and stored in vault when `[openbao]` is configured. |
| `external_url` | `string` | — | No | Public-facing URL of the server (used for OIDC redirect URIs) |
| `trusted_proxies` | `string[]` | — | No | CIDRs whose `X-Forwarded-For` headers to trust (e.g. `["10.0.0.0/8"]`). Each entry must be a valid CIDR. Set via env as comma-separated: `BLOCKYARD_SERVER_TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12`. |

<Aside type="note">
  API authentication uses [Personal Access Tokens](/guides/authorization/#personal-access-tokens)
  (for CLI/CI access) or OIDC session cookies (for browser access). The v0
  static bearer token (`server.token`) has been removed.
</Aside>

## `[docker]`

```toml
[docker]
socket          = "/var/run/docker.sock"
image           = "ghcr.io/rocker-org/r-ver:4.4.3"
shiny_port      = 3838
pak_version     = "stable"
# service_network      = ""
# store_retention      = "0"
# default_memory_limit = "2g"   # fallback per worker; omit = unlimited
# default_cpu_limit    = 4.0    # fallback per worker; 0 = unlimited
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `socket` | `string` | `/var/run/docker.sock` | No | Path to Docker or Podman socket |
| `image` | `string` | — | **Yes** | Base image for worker and build containers |
| `shiny_port` | `integer` | `3838` | No | Port Shiny listens on inside containers |
| `pak_version` | `string` | `stable` | No | [pak](https://pak.r-lib.org/) release channel (`stable`, `rc`, or `devel`) |
| `service_network` | `string` | — | No | Docker network whose containers are made reachable from workers. Used when apps need access to sidecar services (e.g. PocketBase, PostgREST). |
| `store_retention` | `duration` | `0` | No | How long to keep unused entries in the shared package store. `0` (default) disables eviction — the store grows indefinitely. |
| `default_memory_limit` | `string` | — | No | Fallback memory limit for workers when no per-app limit is set (e.g. `"2g"`). Empty or omitted means unlimited. |
| `default_cpu_limit` | `float` | `0` | No | Fallback CPU limit for workers when no per-app limit is set (e.g. `4.0`). `0` means unlimited. |

## `[storage]`

```toml
[storage]
bundle_server_path    = "/data/bundles"
bundle_worker_path    = "/app"
bundle_retention      = 50
max_bundle_size       = 104857600
# soft_delete_retention = "720h"   # 30 days; omit or 0 = immediate hard delete
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `bundle_server_path` | `path` | `/data/bundles` | No | Directory for storing uploaded bundles. Must be writable. |
| `bundle_worker_path` | `path` | `/app` | No | Mount point inside worker containers |
| `bundle_retention` | `integer` | `50` | No | Max bundles kept per app (oldest pruned first) |
| `max_bundle_size` | `integer` | `104857600` | No | Maximum bundle upload size in bytes (default 100 MB) |
| `soft_delete_retention` | `duration` | `0` | No | How long to keep soft-deleted apps before permanent removal. When `0` (default), `DELETE` is an immediate hard delete. When set (e.g. `"720h"` for 30 days), deleted apps are recoverable during the retention window and purged automatically afterwards. |

## `[database]`

```toml
[database]
driver = "sqlite"
path   = "/data/db/blockyard.db"
# url  = ""   # PostgreSQL connection string (when driver = "postgres")
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `driver` | `string` | `sqlite` | No | Database driver: `sqlite` or `postgres` |
| `path` | `path` | `/data/db/blockyard.db` | When `driver = "sqlite"` | Path to the SQLite database file (created if missing). The parent directory must be writable. |
| `url` | `string` | — | When `driver = "postgres"` | PostgreSQL connection string (e.g. `postgres://user:pass@host/dbname`) |

## `[proxy]`

```toml
[proxy]
ws_cache_ttl         = "60s"
health_interval      = "15s"
worker_start_timeout = "60s"
max_workers          = 100
log_retention        = "1h"
# session_idle_ttl     = "0"   # idle timeout for sessions and WebSocket connections; 0 = disabled
idle_worker_timeout  = "5m"
# http_forward_timeout  = "5m"
# max_cpu_limit         = 16.0
# transfer_timeout      = "60s"
# session_max_lifetime  = "0"    # hard cap on session duration; 0 = unlimited
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `ws_cache_ttl` | `duration` | `60s` | No | Time to keep a backend WebSocket alive after client disconnects |
| `health_interval` | `duration` | `15s` | No | How often workers are health-checked |
| `worker_start_timeout` | `duration` | `60s` | No | Max time to wait for a new worker to become healthy |
| `max_workers` | `integer` | `100` | No | Global cap on concurrent worker containers |
| `log_retention` | `duration` | `1h` | No | How long to keep worker log entries before cleanup |
| `session_idle_ttl` | `duration` | `0` | No | Idle timeout for sessions and WebSocket connections. When non-zero, WebSocket connections with no application-level messages for this duration are closed, and stale session records are swept. `0` (default) means disabled. |
| `idle_worker_timeout` | `duration` | `5m` | No | Time before an idle worker container is stopped |
| `http_forward_timeout` | `duration` | `5m` | No | Timeout for forwarding HTTP requests to worker containers |
| `max_cpu_limit` | `float` | `16.0` | No | Maximum CPU limit that can be set per app (caps the `cpu_limit` field on `PATCH /api/v1/apps/{id}`) |
| `transfer_timeout` | `duration` | `60s` | No | Timeout for transferring bundle files to worker containers |
| `session_max_lifetime` | `duration` | `0` | No | Hard cap on session duration regardless of activity. `0` (default) means unlimited — sessions only end via idle timeout or worker shutdown. |

## `[oidc]` *(optional)*

Enable OIDC-based authentication. When this section is present, `server.session_secret` is required unless `[openbao]` is also configured (in which case it can be auto-generated).

```toml
[oidc]
issuer_url           = "https://idp.example.com/realms/myapp"
# issuer_discovery_url = ""      # optional: internal URL for OIDC discovery
client_id            = "blockyard"
client_secret        = "oidc-client-secret"
cookie_max_age       = "24h"
initial_admin        = "google-oauth2|abc123"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `issuer_url` | `string` | — | **Yes** | OIDC provider issuer URL (must match the `iss` claim in tokens) |
| `issuer_discovery_url` | `string` | — | No | Internal URL for OIDC discovery and server-side requests. Use when the IdP is reachable at a different address from the server than from browsers (e.g. Docker DNS). See [Split-URL OIDC](#split-url-oidc). |
| `client_id` | `string` | — | **Yes** | OIDC client ID |
| `client_secret` | `string` | — | **Yes** | OIDC client secret. Supports [vault references](#vault-references). |
| `cookie_max_age` | `duration` | `24h` | No | Maximum lifetime of session cookies |
| `initial_admin` | `string` | — | No | OIDC `sub` of the first admin user. Checked only on first login. See [First Admin Setup](/guides/authorization/#first-admin-setup). |

<Aside type="caution">
  When OIDC is configured, the proxy routes (`/app/{name}/`) enforce
  authentication. Users must log in before accessing apps (except for apps
  with `public` visibility).
</Aside>

### Split-URL OIDC

In Docker or Kubernetes deployments, the OIDC provider (e.g. Dex, Keycloak)
is often reachable at a different address from inside the cluster than from
the user's browser. For example:

- **Browser** reaches the IdP at `http://localhost:5556`
- **Server container** reaches the IdP at `http://dex:5556` (Docker DNS)

Set `issuer_discovery_url` to the internal address. Blockyard will:

1. Perform OIDC discovery against the internal URL
2. Route all server-side requests (token exchange, JWKS fetch, refresh) to the internal URL
3. Keep the public `issuer_url` for browser-facing redirects and token validation

```toml
[oidc]
issuer_url           = "http://localhost:5556"       # public: what the browser sees
issuer_discovery_url = "http://dex:5556"             # internal: Docker DNS
client_id            = "blockyard"
client_secret        = "oidc-client-secret"
```

The corresponding environment variables are `BLOCKYARD_OIDC_ISSUER_URL` and
`BLOCKYARD_OIDC_ISSUER_DISCOVERY_URL`.

## `[openbao]` *(optional)*

Enable OpenBao (Vault-compatible) credential management. Requires `[oidc]` to also be configured.

```toml
[openbao]
address       = "http://openbao:8200"
role_id       = "blockyard-server"    # AppRole role identifier (recommended)
# admin_token = "vault-admin-token"   # deprecated: use role_id instead
token_ttl     = "1h"
jwt_auth_path = "jwt"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `address` | `string` | — | **Yes** | OpenBao server address (must start with `http://` or `https://`) |
| `role_id` | `string` | — | One of `role_id` or `admin_token` | AppRole role identifier. The `secret_id` is delivered via the `BLOCKYARD_OPENBAO_SECRET_ID` env var at bootstrap. |
| `admin_token` | `string` | — | One of `role_id` or `admin_token` | **Deprecated.** Static admin token. Supports [vault references](#vault-references). Use `role_id` with AppRole auth instead. |
| `token_ttl` | `duration` | `1h` | No | TTL for issued credential tokens |
| `jwt_auth_path` | `string` | `jwt` | No | Auth method mount path in OpenBao |
| `skip_policy_scope_check` | `boolean` | `false` | No | Skip the policy scope check during OpenBao bootstrap. Useful when the OpenBao policy format differs from what Blockyard expects. |

<Aside type="tip">
  With AppRole auth (`role_id`), the server authenticates to OpenBao using a
  one-time `secret_id` (via `BLOCKYARD_OPENBAO_SECRET_ID`) and then renews
  its own token indefinitely. After initial bootstrap, the env var is no
  longer needed — the token is persisted to disk and reused across restarts.
  `session_secret` is also auto-generated and stored in vault.
</Aside>

<Aside type="caution">
  `admin_token` and `role_id` are mutually exclusive — setting both is a
  configuration error.
</Aside>

### `[[openbao.services]]`

Define third-party services whose API keys users can enroll via OpenBao. Each
entry must have `id` and `label`. Service IDs must be unique.

Credentials are stored at `secret/data/users/{sub}/apikeys/{id}`.

```toml
[[openbao.services]]
id    = "openai"
label = "OpenAI"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `id` | `string` | — | **Yes** | Unique identifier for the service (also used as the vault path segment) |
| `label` | `string` | — | **Yes** | Human-readable label shown to users |

## `[board_storage]` *(optional)*

Enable board storage via PostgREST. Requires `database.driver = "postgres"` and
`[openbao]` (for vault Identity OIDC tokens that PostgREST uses to enforce
row-level security).

```toml
[board_storage]
postgrest_url = "http://postgrest:3000"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `postgrest_url` | `string` | — | **Yes** | URL of the PostgREST instance serving the board tables |

When configured, workers receive a `POSTGREST_URL` environment variable
pointing to this URL, allowing Shiny apps to store and retrieve board state.

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

## Vault references

Any secret field in the configuration can reference a value stored in
OpenBao instead of containing the literal secret. Use the `vault:` prefix:

```toml
[oidc]
client_secret = "vault:secret/data/blockyard/oidc#client_secret"
```

**Format:** `vault:{kv_v2_data_path}#{key}`

- `{kv_v2_data_path}` — the full KV v2 data path (e.g. `secret/data/blockyard/oidc`)
- `{key}` — the JSON key within the secret's data map

At startup, blockyard resolves all vault references before initializing
other subsystems. If a reference cannot be resolved (vault unreachable,
path missing, key missing), the server exits with a clear error naming
the field and path.

Values without the `vault:` prefix are treated as literals, unchanged
from current behavior.
