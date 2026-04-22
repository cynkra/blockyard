---
title: Configuration
description: How to configure a Blockyard server.
weight: 2
---

Blockyard is configured via a TOML file. By default, it looks for
`blockyard.toml` in the current directory. Override the path with the
`--config` CLI argument:

```bash
blockyard --config /etc/blockyard/config.toml
```

Every field can also be overridden via environment variables using the
pattern `BLOCKYARD_<SECTION>_<FIELD>` (uppercased). For example,
`server.bind` becomes `BLOCKYARD_SERVER_BIND`. Environment variables
take precedence over the TOML file.

For the full, field-by-field reference see
[Configuration File](/docs/reference/config/).

## Choosing a worker backend

`[server] backend` selects how workers run. The default is `docker`;
set it to `process` to use the bubblewrap-sandboxed process backend.

```toml
[server]
backend = "process"    # or "docker" (default)
```

The decision affects which other sections are required:

- **`docker` backend** — requires `[docker] image` and access to the
  Docker/Podman socket.
- **`process` backend** — requires a `[process]` section (bwrap path,
  R path, port and UID ranges, worker GID). Linux only.

See [Backend Security](/docs/guides/backend-security/) for the security
trade-offs between the two, and
[Process Backend (Native)](/docs/guides/process-backend/) /
[Process Backend (Containerized)](/docs/guides/process-backend-container/)
for end-to-end process-backend deployment walkthroughs.

## Common scenarios

### Authentication

Add an `[oidc]` section to require users to log in before accessing
apps. When OIDC is enabled, `server.session_secret` is required unless
`[openbao]` is also configured (in which case the secret is
auto-generated and stored in vault). See the
[Authorization guide](/docs/guides/authorization/).

### Credential management

Add an `[openbao]` section to enroll per-user API keys that Shiny apps
read at runtime. Requires `[oidc]`. See the
[Credential Management guide](/docs/guides/credentials/).

### Rolling updates

Add `[redis]` (shared state) and `[update]` (alt bind range, drain
timeout, optional schedule) to enable `by admin update`. See the
[process backend rolling update walkthrough](/docs/guides/process-backend/#rolling-update-walkthrough)
for prerequisites.

### Observability

Add `[telemetry]` to expose Prometheus metrics and forward traces to an
OpenTelemetry collector. Add `[audit]` for an append-only JSONL audit
log. Configure `server.management_bind` to move health and metrics to a
dedicated loopback listener — see [Observability](/docs/guides/observability/).

## Example

```toml
[server]
bind             = "127.0.0.1:8080"
shutdown_timeout = "30s"
# backend        = "docker"              # or "process"
# default_memory_limit = "2g"            # fallback per worker
# default_cpu_limit    = 4.0             # fallback per worker

[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/cynkra/blockyard-worker:4.4.3"
shiny_port = 3838
pak_version = "stable"

[storage]
bundle_server_path    = "/data/bundles"
bundle_worker_path    = "/app"
bundle_retention      = 50
max_bundle_size       = 104857600

[database]
driver = "sqlite"
path   = "/data/db/blockyard.db"

[proxy]
ws_cache_ttl         = "60s"
health_interval      = "15s"
worker_start_timeout = "60s"
max_workers          = 100
log_retention        = "1h"
idle_worker_timeout  = "5m"

# Optional sections — see the reference for every field.
#
# [process]    — process backend; required when server.backend = "process"
# [redis]      — Redis-backed shared state; required for rolling updates
# [update]     — rolling update orchestrator (schedule, drain, alt bind)
# [oidc]       — OIDC authentication
# [openbao]    — OpenBao credential management (requires [oidc])
# [audit]      — append-only JSONL audit log
# [telemetry]  — Prometheus metrics and OpenTelemetry tracing
```
