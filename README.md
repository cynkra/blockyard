# blockyard

A containerized hosting platform for [Shiny](https://shiny.posit.co/) applications, built in Rust. Blockyard manages the deployment, scaling, and reverse-proxying of isolated R Shiny app containers using Docker.

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
  ┌────────┐     ┌──────────┐     ┌──────────────────┐
  │  Axum  │────▶│  Reverse │────▶│  Shiny Container  │
  │ Router │     │  Proxy   │     │  (per session)     │
  └────────┘     └──────────┘     └──────────────────────┘
      │
      ▼
  ┌────────┐
  │ SQLite │  (app & bundle metadata)
  └────────┘
```

The server is generic over a `Backend` trait, allowing the Docker runtime to be swapped for a mock backend during testing.

## Tech Stack

- **Rust** (2024 edition) with **Tokio** async runtime
- **Axum** — HTTP router with WebSocket support
- **Bollard** — Docker API client
- **SQLx** + **SQLite** — async database with compile-time checked queries
- **Tracing** — structured JSON logging

## Getting Started

### Prerequisites

- Rust (stable)
- Docker or Podman
- SQLite 3

### Configuration

Copy and edit the example configuration:

```bash
cp blockyard.toml blockyard.toml.local
```

All settings can be overridden with environment variables using the `BLOCKYARD_` prefix:

| Config Field | Environment Variable |
|---|---|
| `server.bind` | `BLOCKYARD_SERVER_BIND` |
| `server.token` | `BLOCKYARD_SERVER_TOKEN` |
| `docker.socket` | `BLOCKYARD_DOCKER_SOCKET` |
| `storage.bundle_server_path` | `BLOCKYARD_STORAGE_BUNDLE_SERVER_PATH` |
| `database.path` | `BLOCKYARD_DATABASE_PATH` |

### Build & Run

```bash
# Build with Docker backend (default)
cargo build --release

# Build without Docker (for testing)
cargo build --no-default-features --features test-support

# Run tests
cargo test --features test-support
```

### Dev Container

A devcontainer configuration is included for VS Code / GitHub Codespaces:

```bash
# Open in VS Code with the Dev Containers extension
code .
# Then: Reopen in Container
```

## Project Structure

```
src/
├── main.rs              # Entry point
├── lib.rs               # Public API
├── config.rs            # TOML + env var configuration
├── app.rs               # Shared application state
├── backend/
│   ├── mod.rs           # Backend trait definition
│   ├── docker.rs        # Docker/Podman implementation
│   └── mock.rs          # In-memory mock for tests
└── db/
    ├── mod.rs           # Pool creation & migrations
    └── sqlite.rs        # App & bundle CRUD queries
migrations/
└── 001_initial.sql      # Database schema
```

## Configuration Reference

```toml
[server]
bind             = "0.0.0.0:8080"
token            = "change-me-in-production"
shutdown_timeout = "30s"

[docker]
socket     = "/var/run/docker.sock"
image      = "ghcr.io/rocker-org/r-ver:latest"
shiny_port = 3838

[storage]
bundle_server_path = "/data/bundles"
bundle_worker_path = "/app"
bundle_retention   = 50

[database]
path = "/data/db/blockyard.db"

[proxy]
ws_cache_ttl         = "60s"
health_interval      = "10s"
worker_start_timeout = "60s"
max_workers          = 100
```

## Status

Blockyard is in early development. The current phase (0-1) establishes the foundation — crate skeleton, configuration, backend trait, database schema, and CI. See [`docs/roadmap.md`](docs/roadmap.md) for the full plan.

## License

See the project repository for license information.
