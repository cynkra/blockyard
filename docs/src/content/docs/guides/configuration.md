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
| `token` | *(required)* | Bearer token for the control plane API |
| `shutdown_timeout` | `30s` | Time to drain in-flight requests on shutdown |

### `[docker]`

| Field | Default | Description |
|---|---|---|
| `socket` | `/var/run/docker.sock` | Path to the Docker (or Podman) socket |
| `image` | *(required)* | Base container image for workers and builds |
| `shiny_port` | `3838` | Port Shiny listens on inside the container |
| `rv_version` | `latest` | `rv` release tag to use for dependency restoration |

### `[storage]`

| Field | Default | Description |
|---|---|---|
| `bundle_server_path` | *(required)* | Directory for uploaded bundles |
| `bundle_worker_path` | `/app` | Mount point inside worker containers |
| `bundle_retention` | `50` | Max bundles to keep per app before cleanup |
| `max_bundle_size` | `104857600` | Maximum upload size in bytes (default 100 MB) |

### `[database]`

| Field | Default | Description |
|---|---|---|
| `path` | *(required)* | Path to the SQLite database file |

### `[proxy]`

| Field | Default | Description |
|---|---|---|
| `ws_cache_ttl` | `60s` | How long to keep a backend WebSocket open after client disconnect |
| `health_interval` | `15s` | Interval between worker health checks |
| `worker_start_timeout` | `60s` | Max time to wait for a worker to become healthy |
| `max_workers` | `100` | Maximum number of concurrent worker containers |
| `log_retention` | `1h` | How long to keep worker log entries before cleanup |

## Example

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "change-me-in-production"
shutdown_timeout = "30s"

[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/rocker-org/r-ver:latest"
shiny_port = 3838
rv_version = "latest"

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
```
