# Go Rewrite

This document records the decision to rewrite blockyard from Rust to Go and
the plan for doing so. The rewrite happens at the end of v0 — the Rust
implementation reached feature-completeness for the v0 milestone but was never
validated outside of unit and integration tests. This is the cheapest possible
point to switch languages.

## Why Go

The Rust v0 implementation works, but several areas would be simpler and more
maintainable in Go:

- **Concurrency model.** Blockyard's core is a reverse proxy with background
  container lifecycle management. The Rust implementation uses Tokio tasks,
  `select!`, `JoinSet`, `DashMap`, `broadcast` channels, `CancellationToken`,
  and `AssertUnwindSafe` + `catch_unwind` for panic safety. The WebSocket
  proxy in particular — bidirectional frame shuttling with split/reunite,
  disconnect tracking, and TTL-based caching with cascading spawned tasks —
  is the most complex code in the codebase. In Go, goroutines +
  `context.Context` + channels express the same patterns with less ceremony.

- **Dependency footprint.** The Rust implementation pulls in tokio,
  tokio-util, tokio-tungstenite, axum, hyper, hyper-util, tower, bollard,
  dashmap, futures-util, and more. In Go, the standard library covers HTTP
  serving, reverse proxying, TLS, and concurrency primitives. The Docker
  client is the main external dependency. Fewer upstream changes to track,
  fewer version conflicts.

- **Ecosystem fit.** The Docker/container ecosystem is native Go — Docker
  itself, containerd, Kubernetes, Traefik, Caddy are all Go. The official
  Docker client is a first-party Go package, not a third-party binding.

- **TLS termination.** Go's `crypto/tls` and `autocert` make built-in TLS
  with automatic Let's Encrypt provisioning trivially achievable. This
  simplifies the deployment story by potentially removing the need for a
  separate reverse proxy (Caddy/nginx) in front of blockyard.

- **Build simplicity.** `CGO_ENABLED=0 go build` produces a single static
  binary with no C toolchain required. Cross-compilation is trivial.

The Rust implementation's performance characteristics (zero-cost abstractions,
no GC) are not relevant here — the bottleneck is the Shiny app inside the
container, not the proxy layer.

## What Transfers

The design work and test scenarios are the valuable artifacts from the Rust
v0, not the Rust code itself:

- **Design docs** — roadmap, prior-art, v0 plan, v1/v2 plans. These are
  language-agnostic (updated to remove Rust-isms).
- **Architecture** — Backend interface, API surface, request flows, container
  lifecycle model, network isolation model, shutdown sequence.
- **Database schema** — SQLite tables and migrations transfer directly.
- **API contract** — routes, request/response shapes, status codes, error
  format.
- **Docker container specs** — labels, mounts, network model, hardening.
- **Config format** — `blockyard.toml` structure and env var overlay.
- **Test scenarios** — behavioral specs for what each component should do.

## What Changes — Everything Else

This is a rewrite, not a port. The Go implementation is written from scratch,
informed by the behavioral specs above but unconstrained by the Rust code's
structure, patterns, or language-specific design decisions. The Rust source
serves as a reference for *what* the system should do, never for *how* the Go
code should be organized.

Concretely: do not translate Rust modules into Go packages, do not mirror Rust
type hierarchies, do not carry over patterns that exist because of Rust's
ownership model or async runtime. Start from Go best practices and the
problem domain. Where the Rust design made a choice that was driven by
language mechanics rather than domain requirements, reconsider it from
scratch.

### Key Design Differences from Rust v0

- **Worker handles are plain strings.** The Rust version used an associated
  type (`type Handle: WorkerHandle`) on the `Backend` trait. In Go, the
  Backend interface methods take and return `string` IDs. Each backend
  maintains its own internal state (container metadata, network IDs, etc.)
  keyed by that ID.

- **No premature interface extraction.** The Rust version defined
  `SessionStore`, `WorkerRegistry`, and `TaskStore` as traits from v0 because
  switching from a concrete type to a trait later is a significant refactor in
  Rust (generics propagate through `AppState<B>`). In Go, extracting an
  interface is cheap and local — define it at the call site when you need a
  second implementation, and any struct with matching methods already satisfies
  it. These start as concrete structs in v0.

- **Concurrency via goroutines and context.** No async runtime, no pinning, no
  `Send` bounds. Background work (health polling, log capture, orphan cleanup)
  is managed with goroutines and `context.Context` for cancellation.
  Concurrent maps use `sync.RWMutex` + `map` instead of `DashMap`.

- **Simpler WebSocket proxying.** The bidirectional WS shuttle uses two
  goroutines (one per direction) with a shared context, rather than the
  Rust split/reunite + `select!` + `client_gone` tracking pattern.

- **Standard library where possible.** HTTP reverse proxying, TLS, timers,
  and concurrency primitives come from the standard library. External
  dependencies are limited to what the stdlib doesn't cover.

## Library Choices

| Concern | Library | Rationale |
|---|---|---|
| HTTP router | `github.com/go-chi/chi/v5` | Lightweight, idiomatic (`http.Handler`), middleware/groups for auth separation. Stable, minimal deps. |
| SQLite | `modernc.org/sqlite` | Pure Go — no CGO, no C compiler needed. `CGO_ENABLED=0` builds. Blockyard's SQLite usage (metadata CRUD) won't hit the performance difference vs `mattn/go-sqlite3`. |
| WebSocket | `github.com/coder/websocket` | Context-aware cancellation, clean API, actively maintained (Coder stewardship). Simplifies WS cache TTL and disconnect tracking. |
| Docker client | `github.com/docker/docker/client` | Official first-party client. Full Engine API coverage. Works with Podman's Docker-compatible socket. |
| TOML config | `github.com/BurntSushi/toml` | Standard Go TOML library, written by the spec co-creator. Env var overlay is a manual layer on top. |
| Structured logging | `log/slog` (stdlib) | Built into Go 1.21+. JSON and text output, structured fields, log levels. |

## Project Layout

```
blockyard/
├── go.mod
├── cmd/
│   └── blockyard/
│       ├── main.go                # wiring, signal handling, shutdown
│       └── backend_docker.go      # imports docker backend, provides newBackend()
├── internal/
│   ├── config/                    # TOML parsing + env var overlay
│   ├── backend/
│   │   ├── backend.go             # Backend interface, WorkerSpec, BuildSpec
│   │   ├── docker/
│   │   │   └── docker.go          # DockerBackend
│   │   └── mock/
│   │       └── mock.go            # MockBackend (only imported from _test.go)
│   ├── db/                        # SQLite migrations + queries
│   ├── bundle/                    # upload, storage, unpack, restore
│   ├── session/
│   │   └── store.go               # SessionStore: session ID → worker ID
│   ├── registry/
│   │   └── registry.go            # WorkerRegistry: worker ID → "host:port"
│   ├── logstore/
│   │   └── store.go               # LogStore: per-worker buffer + broadcast
│   ├── task/
│   │   └── store.go               # in-memory task store for async restore jobs
│   ├── api/                       # control plane handlers + auth middleware
│   ├── proxy/                     # reverse proxy, WS forwarding, session, cold start, ws cache
│   ├── ops/                       # health poller, log capture, orphan cleanup, shutdown
│   └── server/
│       └── state.go               # Server struct: shared server state
├── migrations/                    # SQL migration files
├── blockyard.toml                 # reference config
├── docs/                          # design docs (unchanged)
└── .github/
    └── workflows/
        └── ci.yml
```

**Backend selection via build tags:** for now only Docker exists, so
`backend_docker.go` has no build tag. When a Kubernetes backend is added, the
wiring files get build tags:

```
cmd/blockyard/backend_docker.go   # //go:build !k8s
cmd/blockyard/backend_k8s.go      # //go:build k8s
```

`go build` gives Docker (default). `go build -tags k8s` gives Kubernetes.
Only the selected backend's dependencies are compiled into the binary.

**Mock backend:** lives in `internal/backend/mock/` and is only imported from
`_test.go` files. `go build` (without `-test`) never touches it — Go's
toolchain excludes `_test.go` imports from production builds. No build tags
needed.

**Docker integration tests:** gated behind a `docker_test` build tag. Run
with `go test -tags docker_test ./internal/backend/docker/`. Regular
`go test ./...` skips them.

## Approach

Build the Go implementation phase-by-phase following the same feature order
from the v0 plan. Each phase targets the same *behavioral* goals as the Rust
v0 but the internal structure, package boundaries, type design, and
concurrency patterns are derived from Go idioms and the problem domain — not
from the Rust implementation.

The Rust source code (`src/`) is kept in the repo during the rewrite as a
behavioral reference (what does the system do in this scenario?) and removed
when v0 is complete. It should not be consulted for structural decisions (how
should the Go code be organized?).

1. **Foundation** — Go module, config parsing, Backend interface + mock,
   SQLite schema, shared server state
2. **Docker backend** — Backend implementation using the Docker Go client
3. **Content management** — bundle upload, storage, dependency restoration,
   task store
4. **REST API + auth** — chi router, bearer token middleware, all v0 endpoints
5. **Proxy layer** — HTTP/WS reverse proxy, session routing, cold start,
   WS caching
6. **Operations** — health polling, orphan cleanup, log capture, graceful
   shutdown

Each phase produces testable code. Tests are written alongside the
implementation.

## Devcontainer

The devcontainer is already set up for Go (Go 1.24, gopls, delve, Go module
cache volume). No changes needed.

## CI

```yaml
name: CI
on: [push, pull_request]

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: go vet ./...
      - run: go test ./...

  docker-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      - run: go test -tags docker_test ./...
```
