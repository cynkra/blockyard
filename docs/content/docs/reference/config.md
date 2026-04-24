---
title: Configuration File
description: Full reference for the blockyard.toml configuration file.
weight: 3
---

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
# backend              = "docker"       # "docker" (default) or "process"
# skip_preflight       = false
# default_memory_limit = "2g"           # fallback per worker; empty = unlimited
# default_cpu_limit    = 4.0            # fallback per worker; 0 = unlimited
# management_bind      = "127.0.0.1:9100"
# log_level            = "info"
# session_secret       = "random-secret"   # required when [oidc] is configured
# external_url         = "https://blockyard.example.com"
# trusted_proxies      = ["10.0.0.0/8"]
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `bind` | `string` | `127.0.0.1:8080` | No | Socket address to listen on |
| `backend` | `string` | `docker` | No | Worker backend: `docker` or `process`. See [Backend Security](/docs/guides/backend-security/) for the trade-offs. `process` requires a `[process]` section. |
| `skip_preflight` | `boolean` | `false` | No | Skip backend-specific preflight checks at startup. Use for development or when you are certain the environment is correctly configured. |
| `default_memory_limit` | `string` | — | No | Fallback memory limit for workers when no per-app limit is set (e.g. `"2g"`). Empty means unlimited. Enforced by the Docker backend via cgroups; the process backend emits a warning and does not enforce. |
| `default_cpu_limit` | `float` | `0` | No | Fallback CPU limit for workers when no per-app limit is set (e.g. `4.0`). `0` means unlimited. Enforced by the Docker backend via cgroups; the process backend emits a warning and does not enforce. |
| `management_bind` | `string` | — | No | Separate listener for `/healthz`, `/readyz`, `/metrics`. See [Management listener](/docs/guides/observability/#management-listener). |
| `shutdown_timeout` | `duration` | `30s` | No | Grace period for draining requests on shutdown |
| `drain_timeout` | `duration` | — | No | Maximum time the old server will wait for sessions to end during a rolling update drain. See the [process backend rolling update walkthrough](/docs/guides/process-backend/#rolling-update-walkthrough). |
| `log_level` | `string` | `info` | No | Log verbosity. One of `trace`, `debug`, `info`, `warn` (or `warning`), `error`. |
| `session_secret` | `string` | — | When `[oidc]` is set without `[vault]` | Secret for signing session cookies. Supports [vault references](#vault-references). Auto-generated and stored in vault when `[vault]` is configured. |
| `external_url` | `string` | — | No | Public-facing URL of the server (used for OIDC redirect URIs) |
| `trusted_proxies` | `string[]` | — | No | CIDRs whose `X-Forwarded-For` headers to trust (e.g. `["10.0.0.0/8"]`). Each entry must be a valid CIDR. Set via env as comma-separated: `BLOCKYARD_SERVER_TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12`. |
| `bootstrap_token` | `string` | — | No | One-time token that can be exchanged for a real PAT via `POST /api/v1/bootstrap`. Requires `oidc.initial_admin` to be set. Intended for dev/CI bootstrapping — do not use in production. See [Bootstrap tokens](/docs/reference/api/#post-apiv1bootstrap). |
| `worker_env` | `map[string]string` | — | No | Extra environment variables injected into every worker. Common use: point workers at an OTLP collector (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`) for hosted-app tracing. Blockyard-managed keys (`BLOCKYARD_API_URL`, `SHINY_HOST`, `VAULT_ADDR`, …) cannot be overridden. See [Tracing hosted Shiny apps](/docs/guides/observability/#tracing-hosted-shiny-apps). |

> [!NOTE]
> `server.skip_docker_preflight` is deprecated and has been renamed to
> `skip_preflight`. The old name is still accepted for one release with a
> deprecation warning.

> [!NOTE]
> API authentication uses [Personal Access Tokens](/docs/guides/authorization/#personal-access-tokens)
> (for CLI/CI access) or OIDC session cookies (for browser access). The v0
> static bearer token (`server.token`) has been removed.

## `[docker]`

Required when `[server] backend = "docker"` (the default). Configures the
Docker/Podman runtime used for worker and build containers.

```toml
[docker]
socket          = "/var/run/docker.sock"
image           = "ghcr.io/cynkra/blockyard-worker:4.4.3"
shiny_port      = 3838
pak_version     = "stable"
# service_network  = ""
# runtime          = ""          # OCI runtime; empty = Docker daemon default
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `socket` | `string` | `/var/run/docker.sock` | No | Path to Docker or Podman socket |
| `image` | `string` | — | **Yes** | Base image for worker and build containers |
| `shiny_port` | `integer` | `3838` | No | Port Shiny listens on inside containers |
| `pak_version` | `string` | `stable` | No | [pak](https://pak.r-lib.org/) release channel (`stable`, `rc`, or `devel`) |
| `service_network` | `string` | — | No | Docker network whose containers are made reachable from workers. Used when apps need access to sidecar services (e.g. PocketBase, PostgREST). |
| `runtime` | `string` | — | No | Default OCI runtime for worker containers (e.g. `kata-runtime` for stronger isolation). Empty means the Docker daemon's default. |
| `runtime_defaults` | `map` | — | No | Per-access-type runtime defaults (e.g. `{ public = "kata-runtime" }`). Overrides `runtime` for apps matching the access type. |

> [!NOTE]
> In earlier releases `default_memory_limit`, `default_cpu_limit`, and
> `store_retention` lived in `[docker]`. They have moved to `[server]` and
> `[storage]` respectively because they are backend-neutral. The old
> names are still parsed for one release with a deprecation warning.

## `[process]`

Required when `[server] backend = "process"`. Configures the
bubblewrap-based worker sandbox. See the
[Process Backend (Native)](/docs/guides/process-backend/) and
[Process Backend (Containerized)](/docs/guides/process-backend-container/)
guides for deployment walkthroughs, and
[Backend Security](/docs/guides/backend-security/) for the trade-offs
compared to the Docker backend.

```toml
[process]
bwrap_path             = "/usr/bin/bwrap"
r_path                 = "/usr/bin/R"
# seccomp_profile        = "/etc/blockyard/seccomp.bpf"  # empty = no seccomp
port_range_start       = 10000
port_range_end         = 10999
worker_uid_range_start = 60000
worker_uid_range_end   = 60999
worker_gid             = 65534
# skip_metadata_check    = false
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `bwrap_path` | `path` | `/usr/bin/bwrap` | No | Path to the `bubblewrap` binary on the host. |
| `r_path` | `path` | `/usr/bin/R` | No | Path to the R binary. |
| `seccomp_profile` | `path` | — | No | Path to a compiled BPF seccomp profile applied to the worker R process via `bwrap --seccomp`. The `blockyard` and `blockyard-process` images ship a profile at `/etc/blockyard/seccomp.bpf` and set this via `BLOCKYARD_PROCESS_SECCOMP_PROFILE`. Empty disables in-sandbox seccomp filtering (the outer namespace and capability drops still apply). |
| `port_range_start` | `integer` | `10000` | No | First localhost port allocated to workers (inclusive). |
| `port_range_end` | `integer` | `10999` | No | Last localhost port allocated to workers (inclusive). |
| `worker_uid_range_start` | `integer` | `60000` | No | First host UID assigned to worker sandboxes (inclusive). Must be sized to at least the port range. |
| `worker_uid_range_end` | `integer` | `60999` | No | Last host UID assigned to worker sandboxes (inclusive). |
| `worker_gid` | `integer` | `65534` | No | Shared host GID for all workers. Used as the match key for iptables owner-match egress rules. |
| `skip_metadata_check` | `boolean` | `false` | No | Suppress the `cloud_metadata` preflight check, which fails startup with Error when `169.254.169.254:80` is reachable from blockyard itself (and therefore from every worker). Set to `true` only when blockyard legitimately needs cloud metadata access (e.g., using the VM's IAM role for S3 storage); opting in accepts that a compromised worker can read instance credentials. |

> [!WARNING]
> Per-worker resource limits (`server.default_memory_limit`,
> `server.default_cpu_limit`, per-app overrides) are **not enforced** by
> the process backend. Setting them produces a preflight warning. Use
> systemd `MemoryMax=` / `CPUQuota=` or the outer container's cgroups
> for a shared ceiling.

## `[storage]`

```toml
[storage]
bundle_server_path    = "/data/bundles"
bundle_worker_path    = "/app"
bundle_retention      = 50
max_bundle_size       = 104857600
# soft_delete_retention = "720h"   # 30 days; omit or 0 = immediate hard delete
# store_retention       = "0"      # R library cache eviction; 0 = disabled

# [[storage.data_mounts]]
# name = "datasets"
# path = "/srv/shared/datasets"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `bundle_server_path` | `path` | `/data/bundles` | No | Directory for storing uploaded bundles. Must be writable. |
| `bundle_worker_path` | `path` | `/app` | No | Mount point inside worker containers |
| `bundle_retention` | `integer` | `50` | No | Max bundles kept per app (oldest pruned first) |
| `max_bundle_size` | `integer` | `104857600` | No | Maximum bundle upload size in bytes (default 100 MB) |
| `soft_delete_retention` | `duration` | `0` | No | How long to keep soft-deleted apps before permanent removal. When `0` (default), `DELETE` is an immediate hard delete. When set (e.g. `"720h"` for 30 days), deleted apps are recoverable during the retention window and purged automatically afterwards. |
| `store_retention` | `duration` | `0` | No | How long to keep unused entries in the shared R package store. `0` (default) disables eviction — the store grows indefinitely. Moved from `[docker]` in a recent release; the old location is still parsed with a deprecation warning. |
| `data_mounts` | `array` | — | No | Admin-approved host directories that apps can mount read-only or read-write. Each entry has `name` (referenced by apps) and `path` (host-side location). |

## `[database]`

```toml
[database]
driver = "sqlite"
path   = "/data/db/blockyard.db"
# url  = ""   # PostgreSQL connection string (when driver = "postgres")

# Vault-managed Postgres credentials (optional; postgres only):
# vault_mount = "database"
# vault_role  = "blockyard_admin"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `driver` | `string` | `sqlite` | No | Database driver: `sqlite` or `postgres` |
| `path` | `path` | `/data/db/blockyard.db` | When `driver = "sqlite"` | Path to the SQLite database file (created if missing). The parent directory must be writable. |
| `url` | `string` | — | When `driver = "postgres"` | PostgreSQL connection string (e.g. `postgres://user:pass@host/dbname`). Userinfo is ignored when `vault_role` is set. |
| `vault_mount` | `string` | `database` | No | Vault database secrets-engine mount path. Requires `[vault]` and `driver = "postgres"`. |
| `vault_role` | `string` | — | No | Vault static-role name. When set, Blockyard reads `{vault_mount}/static-creds/{vault_role}` at startup and uses those credentials instead of any user/password in `url`. Requires `[vault]` and `driver = "postgres"`. |

### Vault-managed Postgres credentials

When `database.vault_role` is set, Blockyard obtains its Postgres
credentials from the vault database secrets engine on every startup
and whenever the cached password stops working. The role's password
is owned by the vault (rotated on the schedule the operator configures
on the role), not by the token that created the lease — so Blockyard
restarts, deploy pipelines, and token renewals do not affect database
access.

One-time operator setup:

1. Create a PostgreSQL role for Blockyard with the privileges it needs
   (at minimum `LOGIN` plus `CREATE` and `USAGE` on the target
   database). A typical setup uses a dedicated `blockyard_admin` role:

   ```sql
   CREATE ROLE blockyard_admin LOGIN PASSWORD '<temp>' CREATEROLE;
   GRANT ALL PRIVILEGES ON DATABASE blockyard TO blockyard_admin;
   ```

2. In the vault, register the role as a static-role on the database
   secrets engine. This tells the vault to adopt the role and manage
   its password:

   ```sh
   bao write database/static-roles/blockyard_admin \
       db_name=postgresql \
       username=blockyard_admin \
       rotation_period=24h
   ```

   The vault immediately rotates the password; subsequent reads of
   `database/static-creds/blockyard_admin` return the current one.

3. Grant Blockyard's AppRole policy read access to the static-creds
   endpoint:

   ```hcl
   path "database/static-creds/blockyard_admin" {
     capabilities = ["read"]
   }
   ```

4. Configure Blockyard:

   ```toml
   [database]
   driver     = "postgres"
   url        = "postgres://postgres.internal/blockyard?sslmode=verify-full"
   vault_role = "blockyard_admin"
   ```

   The `url` does not need (and should not include) a username or
   password — Blockyard injects the vault-issued credentials on every
   connection.

At runtime, Blockyard re-reads the static-creds endpoint on any
Postgres authentication failure, so an out-of-band password rotation
heals automatically on the next health poll.

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

## `[redis]` *(optional)*

Enables Redis-backed shared state for the session store, worker
registry, and the process backend's port/UID allocators. Required for
rolling updates via `by admin update` — the old and new server
processes use Redis as the cross-process coordination layer. Single-node
deployments without rolling updates can omit this section and the
in-memory implementation is used.

```toml
[redis]
url        = "redis://localhost:6379"
# key_prefix = "blockyard:"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `url` | `string` | — | **Yes** (when section is present) | Redis connection URL, e.g. `redis://[:password@]host:port[/db]`. |
| `key_prefix` | `string` | `blockyard:` | No | Key prefix for every Redis operation. Useful when multiple blockyard deployments share a Redis instance. |

## `[update]` *(optional)*

Configures the rolling-update orchestrator driven by `by admin update`.
The orchestrator has two variants, picked automatically based on the
configured backend:

- **Docker variant** — clones the blockyard container next to the old
  one. Uses only `schedule`, `channel`, and `watch_period`.
- **Process variant** — forks a new blockyard process on an alternate
  bind port. Uses `schedule`, `channel`, `watch_period`, plus
  `alt_bind_range` and `drain_idle_wait`. Requires `[redis]`.

See the [process backend rolling update walkthrough](/docs/guides/process-backend/#rolling-update-walkthrough)
for the containerized vs. native rules.

```toml
[update]
# schedule        = "0 3 * * 0"     # cron; empty = no scheduled updates
# channel         = "stable"        # "stable" or "main"
# watch_period    = "15m"           # post-update health monitoring
# alt_bind_range  = "8090-8099"     # process variant: alternate bind pool
# drain_idle_wait = "5m"            # process variant: session drain timeout
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `schedule` | `string` | — | No | Cron expression (5 fields) for automatic rolling updates. Empty disables the scheduler. |
| `channel` | `string` | `stable` | No | Release channel to pull from: `stable` or `main`. |
| `watch_period` | `duration` | — | No | Time the orchestrator monitors the new server's health after activation. An unhealthy signal triggers automatic rollback (Docker variant only). |
| `alt_bind_range` | `string` | `8090-8099` | No | Port range the process orchestrator picks an alternate bind from when spawning the new server. Must not overlap `[process] port_range_start..end`. Ignored by the Docker variant. |
| `drain_idle_wait` | `duration` | `5m` | No | Maximum time the old server waits for active sessions to end during a rolling drain. Ignored by the Docker variant, which relies on the reverse proxy to drain in-flight requests. |

## `[oidc]` *(optional)*

Enable OIDC-based authentication. When this section is present, `server.session_secret` is required unless `[vault]` is also configured (in which case it can be auto-generated).

```toml
[oidc]
issuer_url           = "https://idp.example.com/realms/myapp"
# issuer_discovery_url = ""      # optional: internal URL for OIDC discovery
client_id            = "blockyard"
client_secret        = "oidc-client-secret"
cookie_max_age       = "24h"
initial_admin        = "google-oauth2|abc123"
default_role         = "viewer"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `issuer_url` | `string` | — | **Yes** | OIDC provider issuer URL (must match the `iss` claim in tokens) |
| `issuer_discovery_url` | `string` | — | No | Internal URL for OIDC discovery and server-side requests. Use when the IdP is reachable at a different address from the server than from browsers (e.g. Docker DNS). See [Split-URL OIDC](#split-url-oidc). |
| `client_id` | `string` | — | **Yes** | OIDC client ID |
| `client_secret` | `string` | — | **Yes** | OIDC client secret. Supports [vault references](#vault-references). |
| `cookie_max_age` | `duration` | `24h` | No | Maximum lifetime of session cookies |
| `initial_admin` | `string` | — | No | OIDC `sub` of the first admin user. Checked only on first login. See [First Admin Setup](/docs/guides/authorization/#first-admin-setup). |
| `default_role` | `string` | `viewer` | No | Role assigned to new users on first OIDC login. Must be `viewer` or `publisher`. Set to `publisher` when the IdP itself is the access gate and every authenticated user should be trusted to deploy. `admin` is rejected — bootstrap admins via `initial_admin`. |

> [!WARNING]
> When OIDC is configured, the proxy routes (`/app/{name}/`) enforce
> authentication. Users must log in before accessing apps (except for apps
> with `public` visibility).

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

## `[vault]` *(optional)*

Enable Vault-compatible credential management. Requires `[oidc]` to also be configured.

```toml
[vault]
address          = "http://openbao:8200"
role_id          = "blockyard-server"           # AppRole role identifier (recommended)
# admin_token    = "vault-admin-token"          # deprecated: use role_id instead
token_ttl        = "1h"
jwt_auth_path    = "jwt"
# secret_id_file = "/run/secrets/vault_secret_id"   # opt-in: re-read secret_id on each login for rotation
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `address` | `string` | — | **Yes** | Vault server address (must start with `http://` or `https://`) |
| `role_id` | `string` | — | One of `role_id` or `admin_token` | AppRole role identifier. The `secret_id` is delivered via `BLOCKYARD_VAULT_SECRET_ID` or (opt-in) `secret_id_file`. |
| `admin_token` | `string` | — | One of `role_id` or `admin_token` | **Deprecated.** Static admin token. Supports [vault references](#vault-references). Use `role_id` with AppRole auth instead. |
| `token_ttl` | `duration` | `1h` | No | TTL hint; the actual TTL is whatever vault returns on login. Shorten it to make rotation propagate faster. |
| `jwt_auth_path` | `string` | `jwt` | No | Auth method mount path in the vault |
| `secret_id_file` | `string` | — | No | Path to a file containing the AppRole `secret_id`. When set, the file is re-read on every login so `secret_id` rotations on disk take effect without restarting Blockyard. Takes precedence over `BLOCKYARD_VAULT_SECRET_ID`. |
| `ca_cert` | `string` | — | No | Path to a PEM-encoded CA bundle used to verify the vault server's TLS certificate. When set, replaces the system CA bundle for vault HTTP calls (matches `VAULT_CACERT` semantics). Overridable via `BLOCKYARD_VAULT_CA_CERT`. |
| `skip_policy_scope_check` | `boolean` | `false` | No | Skip the policy scope check during vault bootstrap. Useful when the vault policy format differs from what Blockyard expects. |

> [!TIP]
> With AppRole auth (`role_id`), Blockyard logs in against the vault and
> re-logs in shortly before each token expires; a 403 on any admin call
> also triggers an immediate re-login and retry. Point `secret_id_file`
> at a path written by Vault Agent (or any rotation tool) to rotate the
> `secret_id` without restarting the server. When no file is configured,
> the `secret_id` is read once from `BLOCKYARD_VAULT_SECRET_ID` at startup.
> `session_secret` is also auto-generated and stored in vault.

> [!WARNING]
> `admin_token` and `role_id` are mutually exclusive — setting both is a
> configuration error.

### `[[vault.services]]`

Define third-party services whose API keys users can enroll via the vault. Each
entry must have `id` and `label`. Service IDs must be unique.

Credentials are stored at `secret/data/users/{sub}/apikeys/{id}`.

```toml
[[vault.services]]
id    = "openai"
label = "OpenAI"
```

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `id` | `string` | — | **Yes** | Unique identifier for the service (also used as the vault path segment) |
| `label` | `string` | — | **Yes** | Human-readable label shown to users |

## `[board_storage]` *(optional)*

Enable board storage via PostgREST. Requires `database.driver = "postgres"` and
`[vault]` (for vault Identity OIDC tokens that PostgREST uses to enforce
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
the vault instead of containing the literal secret. Use the `vault:` prefix:

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
