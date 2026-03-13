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

## Project Structure

```
cmd/
└── blockyard/
    └── main.go              # Entry point
internal/
├── config/
│   ├── config.go            # TOML + env var configuration
│   └── secret.go            # Secret type (redacted in logs)
├── server/
│   └── state.go             # Shared application state
├── task/
│   └── store.go             # In-memory task tracking & log streaming
├── api/
│   ├── router.go            # Router setup
│   ├── auth.go              # Bearer token middleware
│   ├── apps.go              # App CRUD & lifecycle endpoints
│   ├── bundles.go           # Bundle upload & list endpoints
│   ├── tasks.go             # Task log streaming endpoint
│   ├── access.go            # ACL management endpoints
│   ├── roles.go             # Role mapping endpoints
│   ├── tags.go              # Tag management endpoints
│   ├── catalog.go           # Content discovery endpoint
│   ├── credentials.go       # Vault credential exchange
│   ├── users.go             # User-facing API (credential enrollment)
│   ├── readyz.go            # Readiness probe
│   └── error.go             # Shared error response helpers
├── auth/
│   ├── handlers.go          # OIDC login/callback/logout handlers
│   ├── middleware.go         # App-plane auth middleware
│   ├── oidc.go              # OIDC provider setup
│   ├── session.go           # Session cookie encode/decode
│   ├── identity.go          # CallerIdentity type & context helpers
│   ├── jwt.go               # JWT/JWKS validation
│   ├── rolecache.go         # Group-to-role mapping cache
│   └── sessiontoken.go      # Worker session reference tokens
├── authz/
│   ├── rbac.go              # Role-based access control
│   └── acl.go               # Per-app access control lists
├── backend/
│   ├── backend.go           # Backend interface definition
│   ├── docker/
│   │   ├── docker.go        # Docker/Podman implementation
│   │   ├── detect.go        # Runtime detection (Docker vs Podman)
│   │   └── mounts.go        # Container mount configuration
│   └── mock/
│       └── mock.go          # In-memory mock for tests
├── bundle/
│   ├── bundle.go            # Archive storage, unpacking, retention
│   └── restore.go           # Dependency restoration pipeline
├── proxy/
│   ├── proxy.go             # Reverse proxy router (/app/{name}/)
│   ├── forward.go           # HTTP and WebSocket forwarding
│   ├── coldstart.go         # On-demand worker startup
│   ├── session.go           # Session-to-worker mapping
│   ├── loadbalancer.go      # Multi-worker load balancing
│   ├── autoscaler.go        # Automatic worker scaling
│   ├── ws.go                # WebSocket proxying
│   └── wscache.go           # WebSocket connection caching
├── ops/
│   └── ops.go               # Health polling, log capture, orphan cleanup
├── db/
│   └── db.go                # Pool creation, migrations & CRUD queries
├── integration/
│   ├── openbao.go           # OpenBao (Vault) client
│   ├── bootstrap.go         # OpenBao policy/role bootstrapping
│   ├── enrollment.go        # Credential enrollment in KV store
│   └── tokencache.go        # Vault token cache
├── audit/
│   └── audit.go             # Append-only JSONL audit logging
├── telemetry/
│   ├── metrics.go           # Prometheus metrics
│   └── tracing.go           # OpenTelemetry tracing middleware
├── logstore/
│   └── store.go             # Container log storage
├── registry/
│   └── registry.go          # Worker address caching
├── rvcache/
│   └── rvcache.go           # R package binary caching
└── session/
    └── store.go             # Session store
migrations/
├── 001_initial.sql          # Initial schema
└── 002_remove_app_status.sql  # Remove runtime status from apps table
```

## Configuration Reference

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "change-me-in-production"
shutdown_timeout = "30s"
# session_secret = "random-secret"   # required when [oidc] is configured
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
# [oidc]
# issuer_url    = "https://idp.example.com/realms/myapp"
# client_id     = "blockyard"
# client_secret = "oidc-client-secret"
# groups_claim  = "groups"       # default: "groups"
# cookie_max_age = "24h"         # default: "24h"

# Optional: OpenBao credential management (requires [oidc])
# [openbao]
# address     = "http://openbao:8200"
# admin_token = "vault-admin-token"
# token_ttl   = "1h"             # default: "1h"
# jwt_auth_path = "jwt"          # default: "jwt"

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
