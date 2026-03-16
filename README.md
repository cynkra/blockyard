# blockyard

[![ci](https://github.com/cynkra/blockyard/actions/workflows/ci.yml/badge.svg)](https://github.com/cynkra/blockyard/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/cynkra/blockyard/graph/badge.svg?token=xvgKIhFWeW)](https://codecov.io/gh/cynkra/blockyard)

A containerized hosting platform for [Shiny](https://shiny.posit.co/) applications, built in Go. Blockyard manages the deployment, scaling, and reverse-proxying of isolated R Shiny app containers using Docker.

## Overview

Blockyard acts as a container-orchestrated reverse proxy and application server. Each Shiny app runs in its own Docker container with resource limits, health checks, and automatic lifecycle management.

**Key design choices:**

- One content type: Shiny apps only (not Plumber APIs, static sites, or scheduled tasks)
- Single R version configured server-wide
- Docker/Podman required — no bare-metal processes
- Per-container session isolation by default

## Architecture

```
Client Request
      │
      ▼
  ┌────────┐     ┌──────┐     ┌──────────┐     ┌──────────────────┐
  │  Chi   │────▶│ Auth │────▶│  Reverse │────▶│  Shiny Container │
  │ Router │     │(OIDC)│     │  Proxy   │     │  (per session)   │
  └────────┘     └──────┘     └──────────┘     └──────────────────┘
      │                                               │
      ▼                                               ▼
  ┌────────┐                                    ┌──────────┐
  │ SQLite │  (app & bundle metadata)           │ OpenBao  │ (credentials)
  └────────┘                                    └──────────┘
```

The server is generic over a `Backend` interface, allowing the Docker runtime to be swapped for a mock backend during testing.

## Tech Stack

- **Go** 1.24 with standard library `net/http`
- **Chi** — HTTP router with middleware support
- **Docker SDK** — Docker API client (`github.com/docker/docker`)
- **modernc.org/sqlite** — pure-Go SQLite driver
- **OIDC** — OpenID Connect authentication (`coreos/go-oidc/v3`)
- **OpenBao** — credential management (Vault-compatible)
- **Prometheus** — metrics (`prometheus/client_golang`)
- **OpenTelemetry** — distributed tracing
- **log/slog** — structured JSON logging

## Getting Started

### Prerequisites

- Go 1.24+
- Docker or Podman
- SQLite 3

### Configuration

Copy and edit the example configuration:

```bash
cp blockyard.toml blockyard.toml.local
```

All settings can be overridden with environment variables using the
`BLOCKYARD_<SECTION>_<FIELD>` pattern (uppercased). For example,
`server.bind` becomes `BLOCKYARD_SERVER_BIND`.

### Build & Run

```bash
# Build
go build -o blockyard ./cmd/blockyard

# Run tests
go test ./...
```

### Dev Container

A devcontainer configuration is included for VS Code / GitHub Codespaces:

```bash
# Open in VS Code with the Dev Containers extension
code .
# Then: Reopen in Container
```

**Native mode** (`go run ./cmd/blockyard` directly) requires that Docker
container IPs on bridge networks are routable from the host. This is the
case on Linux and with some macOS Docker runtimes, but not all. If
container IPs are not routable from your host, run the server inside a
container (e.g. the devcontainer) instead.

## Project Layout

- `cmd/blockyard/` — Entry point.
- `internal/` — All application code, organized by domain:
  - `api/` — HTTP handlers for the management API (apps, bundles, users, tags, etc.)
  - `auth/`, `authz/` — OIDC authentication, session management, RBAC, and per-app ACLs.
  - `proxy/` — Reverse proxy, WebSocket forwarding, cold-start, session routing, autoscaling.
  - `backend/` — Container runtime abstraction (`docker/` for Docker/Podman, `mock/` for tests).
  - `bundle/` — Bundle archive storage, unpacking, and R dependency restoration.
  - `db/` — SQLite pool, migrations, and CRUD queries.
  - `integration/` — OpenBao (Vault) client, bootstrapping, and credential enrollment.
  - `config/`, `server/` — TOML/env configuration and shared server state.
  - `ops/` — Health polling, log capture, orphan cleanup.
  - `audit/`, `telemetry/` — Audit logging, Prometheus metrics, OpenTelemetry tracing.
- `migrations/` — SQL migration files.

## Configuration Reference

```toml
[server]
bind             = "0.0.0.0:8080"
shutdown_timeout = "30s"
# management_bind = "127.0.0.1:9100"  # separate listener for /healthz, /readyz, /metrics
# log_level      = "info"   # trace, debug, info, warn, error
# session_secret = "random-secret"   # required when [oidc] is set without [openbao]
# external_url   = "https://blockyard.example.com"

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
# issuer_url           = "https://idp.example.com/realms/myapp"
# issuer_discovery_url = ""      # optional: internal URL for OIDC discovery (e.g. Docker DNS)
# client_id            = "blockyard"
# client_secret        = "oidc-client-secret"
# cookie_max_age       = "24h"   # default: "24h"
# initial_admin        = "google-oauth2|abc123"

# Optional: OpenBao credential management (requires [oidc])
# [openbao]
# address       = "http://openbao:8200"
# role_id       = "blockyard-server"       # AppRole role identifier (recommended)
# # admin_token = "vault-admin-token"      # deprecated: use role_id instead
# token_ttl     = "1h"             # default: "1h"
# jwt_auth_path = "jwt"            # default: "jwt"

# Optional: Audit logging
# [audit]
# path = "/data/audit/blockyard.jsonl"

# Optional: Telemetry
# [telemetry]
# metrics_enabled = true
# otlp_endpoint   = "http://otel-collector:4317"
```

## Status

Blockyard is in early development. See [`docs/design/roadmap.md`](docs/design/roadmap.md) for the full plan.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
