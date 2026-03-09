# blockr.cloud Prior Art

## Overview

This document surveys six existing R/Shiny deployment platforms and records
the build-vs-fork decision for `blockr.cloud`. It is the research base for the
design choices documented in `roadmap.md`.

Projects surveyed:

- **[Shiny Server](https://github.com/rstudio/shiny-server)** — the original
  OSS R/Shiny hosting server from Posit; the reference implementation for Shiny
  process management
- **[Posit Connect](https://docs.posit.co/connect/)** — the commercial
  all-in-one publishing platform from Posit; the 900-pound gorilla and the
  benchmark for full-featured R/Python deployment
- **[Scaly](https://docs.scalyapps.io)** — a closed-source hosted platform for
  R, Python, and Julia apps; notable for its Stack abstraction and User Pool
  auth model
- **[ricochet-rs](https://github.com/ricochet-rs)** — a closed-source
  deployment platform written in Rust ("R & Julia belong in production")
- **[faucet](https://github.com/ixpantia/faucet)** — an OSS Rust reverse
  proxy and process manager for R/Python apps
- **[ShinyProxy](https://github.com/openanalytics/shinyproxy)** — an OSS
  Java/Spring Boot container-orchestration platform for Shiny apps

## Existing Projects

### Shiny Server

The original OSS Shiny hosting server from Posit, written in **Node.js**. The
server acts as a reverse proxy in front of R processes — it does zero R work
itself.

**Process model:** One R process per app URL, shared across all users. Sessions
are multiplexed inside that one process. The `SchedulerRegistry` maintains one
`SimpleScheduler` per unique app key; each scheduler holds at most one R
process in the OSS version. Shiny Server Pro (closed-source) adds per-user and
pooled schedulers.

**WebSocket / session handling:**
- Uses **SockJS** (WebSocket with XHR-polling/EventSource fallback) for broad
  browser compatibility
- **RobustSockJS** implements session resumption: on disconnect, the backend WS
  connection is buffered for 15 seconds; if the client reconnects within that
  window, a sequence-numbered message exchange replays any missed messages
- **MultiplexSocket** allows multiple logical Shiny sessions to share one
  physical SockJS connection

**Security patterns worth adopting:**
- **Stdin config injection:** R processes receive their configuration (port,
  appDir, shared secret, etc.) over stdin as JSON — not as command-line
  arguments — so app paths and secrets are invisible in `ps` output
- **Shared secret header:** R validates a `shiny-shared-secret` HTTP header on
  every proxied request; only the Node proxy can talk to it

**Connection lifecycle:** Reference-counted per-worker tracking of three
counters — `httpConn`, `sockConn`, `pendingConn`. The idle timer only fires
when all three reach zero. The `pendingConn` "reservation" is pre-incremented
when the HTML page is served (before the WebSocket is opened), preventing
premature idle detection during the HTTP → WS transition.

**Known limitations:**
- Single process per app (no pooling in OSS version); R is single-threaded so
  all users compete for one event loop
- No process pre-warming; every first request pays full cold-start cost
- No request queuing — at `maxRequests` capacity, users immediately get 503
- No active health polling after startup; a hung R process holds its slot until
  all sessions disconnect and the idle timer fires
- No REST API; deployment is file-copy + `restart.txt` touch + SIGHUP

### Posit Connect

The commercial publishing platform from Posit. Closed-source, but thoroughly
documented. The benchmark for what a full-featured R/Python deployment platform
looks like.

**Content types:** 20+ types unified under one platform — Shiny for R/Python,
Streamlit, Dash, Bokeh, Gradio, Plumber, FastAPI, Flask, Vetiver (ML model
serving), Quarto, R Markdown, Jupyter Notebooks, parameterized reports, static
sites, Pins (versioned data), scheduled scripts, MCP servers.

**Deployment:** Push from RStudio/VS Code IDE, `rsconnect`/`rsconnect-python`
CLI, REST API, or **git-backed CD** (Connect polls a `manifest.json` in a git
repo every 15 minutes and re-deploys on change).

**Runtime management:**
- Multiple R and Python versions installed side-by-side; version locked per
  content item at publish time; matching strategies: `Nearest`, `Major-Minor`,
  `Exact`
- Per-content isolated Python virtual environments (using `uv` by default)
- Per-content resource limits: `MaxProcesses`, `MaxConnsPerProcess`,
  `MemoryLimit`, `CPULimit`, GPU limits (Kubernetes mode)
- Linux namespace sandboxing for local execution; full container isolation on
  Kubernetes via Posit Launcher

**Access control:** Four user roles (Anonymous, Viewer, Publisher,
Administrator). Per-content ACLs grant Viewer or Collaborator access to
specific users or groups. Content visibility: public, all-authenticated, or
specific ACL. `MostPermissiveAccessType` allows admins to cap maximum
visibility server-wide.

**Content discovery:**
- Hierarchical tag system (Category → Tag → Subtag) for organizing content
- Content catalog with search and filter (`type:`, `owner:`, `tag:`,
  `is:scheduled`, `is:r`, etc.)
- **Vanity URLs** — per-content custom URL paths (e.g. `/sales-dashboard`)
- `connectwidgets` R package for building curated content portals

**Bundle management:** Each deployment stored as a versioned bundle; previous
bundles retained up to a configurable limit; rollback by activating a previous
bundle (apps/APIs drain existing sessions gracefully before switching).

**Key features not in open-source alternatives:** parameterized report
variants with per-variant schedules, git-backed CD, OAuth credential delegation
(viewer's own identity flows into content session), content locking, vanity
URLs, Prometheus + OpenTelemetry export, usage analytics per content item, and
a comprehensive tamper-proof audit trail.

### Scaly

A **closed-source hosted platform** for deploying R, Python, and Julia
applications without web development expertise. Documentation is sparse but
the product gives a useful picture of what a simplified, opinionated deployment
UX looks like.

**Supported frameworks:** R Shiny, Python Shiny, Streamlit, Bokeh, Jupyter
Notebooks, Reflex, Flask, Bottle, Genie.jl (Julia). App type is auto-detected
from file structure — no explicit declaration required.

**Infrastructure model (Stacks):** The core unit is a "Stack" — a container
setup that hosts one or more apps. Stacks come in fixed T-shirt sizes (Eco:
0.25 vCPU / 0.5 GB RAM → Performance_L: 8 vCPU / 16 GB RAM). Apps and stacks
are independent objects; an app can be moved between stacks or removed from
hosting without deletion. This is a cleaner separation than bundling resource
sizing into the app itself.

**Deployment:** ZIP file upload or GitHub-integrated CD. When GitHub is
selected at app creation, Scaly generates workflow files to add to the repo —
a nice onboarding touch. Separate QA and production stacks/apps are the
recommended workflow.

**Dependency management:** R Shiny apps require `renv.lock`; Python apps
require `requirements.txt`. No mention of multi-version R/Python management.

**Scaling and idle management:** Configurable minimum idle processes per stack
(`min_idle`). Setting `min_idle ≥ 1` keeps a warm process running to avoid
cold starts. Autoscaling is listed as "coming soon."

**Authentication (User Pools):** A User Pool is a shared user directory that
multiple apps can attach to as an addon. User identity and group membership
are injected into the app via HTTP headers (`HTTP_SCALY_USER_ID`, etc.) — no
SDK required, works the same way for R and Python. Supports MFA via
authenticator apps, configurable password policies, per-user
enable/disable/reset. Groups allow differentiated in-app experiences.

**Addons model:** Databases and storage buckets (S3-compatible) are first-class
platform resources that apps attach to via the Addon tab. Attaching an addon
triggers a redeployment. This is a notably broader scope than the other
projects — Scaly positions itself as a full application platform, not just a
process host.

**Notable for our design:** The Stack abstraction (a named, sized hosting
environment that apps are assigned to, independent of the apps themselves) is
worth considering. It maps naturally to our Backend concept but surfaces as a
user-facing object rather than an internal implementation detail. The HTTP
header injection pattern for user identity (simpler than OAuth flows for
in-app auth) is also worth noting.

### ricochet-rs

A deployment platform for data scientists. The server is written in Rust but is
**closed-source** — only the CLI, container images, Helm charts, Ansible roles,
and documentation are publicly available.

**R runtime management by deployment mode:**

- **Host/VM (systemd):** R must be pre-installed (they recommend
  [rig](https://github.com/r-lib/rig)). Ricochet uses whatever R is on the
  system PATH.
- **Containers (Docker/Podman):** Pre-built execution environment images
  (`ricochetrs/r-ubuntu`, `r-alpine`, `r-alma`) with R + system dev libraries
  baked in. Rebuilt weekly for amd64/arm64.
- **Kubernetes:** Helm chart deploys the server, which spawns Deployments for
  long-lived apps and Jobs for tasks. A shared PVC caches renv packages.

**Rust-R interface:** Process-level orchestration, not FFI. The Rust server
spawns and manages R processes — it does not embed R.

**Key features:** Bundle upload via REST API, renv-based dependency management,
auto-scaling with scale-to-zero, task scheduling (cron), static site serving,
OIDC auth, encrypted env vars, multi-language (R/Python/Julia).

### faucet

A **single-binary Rust reverse proxy + process manager**. Directly spawns
R/Python processes, assigns random ports, health-checks via TCP polling, and
proxies HTTP/WebSocket traffic.

- Auto-detection of app type (Shiny, Plumber, Quarto, FastAPI) by scanning for
  known files
- 4 load-balancing strategies: round-robin, IP hash, cookie hash (sticky
  sessions for Shiny), RPS-based autoscaling
- WebSocket session caching: preserves backend WS connections for 60s on client
  disconnect
- Multi-app router via TOML config
- Optional telemetry to PostgreSQL
- No auth, no deployment/bundling, no package management — purely a runtime
  proxy

### ShinyProxy

A **container-orchestration platform** for Shiny apps. Written in Java (Spring
Boot). Every app must be packaged as a Docker image — ShinyProxy never touches R
directly.

- 4 container backends: Docker Engine, Docker Swarm, Kubernetes, AWS ECS (clean
  `IContainerBackend` abstraction)
- 7 auth backends: Simple, LDAP, OIDC, SAML, WebService, CustomHeader, None
- Seat-based pre-warming: pool containers with multiple seats to reduce cold
  starts
- Per-user container isolation by default (one container per user per app)
- App recovery: scans backend for existing containers on restart
- Heartbeat-based session management: kills inactive containers after timeout
- No CLI-driven deployment, no task/scheduling support, no scale-to-zero

## Build vs. Fork Assessment

We evaluated two approaches for leveraging faucet:

- **Option A (fork):** Fork faucet, refactor the backend layer to support
  containers alongside bare-metal processes.
- **Option B (reference build):** Use faucet's proxy/LB architecture as a
  blueprint, build fresh with a pluggable backend from day one.

### What transfers cleanly from faucet

The following layers are address-agnostic — they operate on `SocketAddr` or
abstract `Client` objects and don't care whether the upstream is a Docker
container or a Kubernetes pod:

- **Reverse proxy / connection pool** (`Client`, `ConnectionManager` in
  `pool.rs`)
- **Load balancing strategies** (round-robin, IP hash, cookie hash, RPS
  autoscale in `load_balancing/`)
- **Service/middleware pipeline** (`onion.rs` — `Service` and `Layer` traits)
- **WebSocket proxying & session caching** (`websockets.rs`)
- **Health checking** — a pure `TcpStream::connect(addr)` function

### What doesn't transfer

The process management layer in `worker.rs` is deeply coupled to local OS
processes:

- **Port assignment** is hardcoded to `127.0.0.1` with local bind-testing
- **Spawning** calls `tokio::process::Command` with Unix-specific `setpgid` and
  `kill_on_drop`
- **Lifecycle loop** uses `child.kill()`, `child.wait()`, `child.id()` — all OS
  process primitives
- **Logging** reads `Child` stdout/stderr streams directly
- **No `Backend` trait** exists — `WorkerConfig` is simultaneously config
  holder, spawner, lifecycle manager, and state container

Roughly half of `worker.rs` is process-specific code that would be dead weight
in a container-first project.

### Decision: Option B (reference build)

Since production workloads will be exclusively containerized, we build fresh
with a pluggable `Backend` trait from day one. The local-process backend serves
as a reference implementation and is useful for development, testing, and
examples. Docker and Kubernetes backends are introduced early.

The proxy, load-balancing, and middleware patterns from faucet are reused
architecturally but reimplemented to avoid fork maintenance.
