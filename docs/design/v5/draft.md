# blockyard v5 — Draft Notes

v5 adds the Kubernetes backend and the architectural refactors it triggers.
Everything in v5 is either the k8s implementation itself or a prerequisite
for multi-node deployments.

## Features

### Kubernetes Backend

Implement the `Backend` interface using `k8s.io/client-go` to create
Deployments (long-lived apps) and Jobs (tasks). Involves pod specs, service
creation for routing, PVC management for shared caches, and pod status
polling.

## Deferred Architectural Refactors

The following refactors are triggered by the k8s backend. They are not
needed for single-node Docker deployments (v2) or the process backend (v3).

### Backend Package Extraction

v0 uses a single module with build-tag-gated backends. This works for one or
two backends but may become unwieldy when Kubernetes arrives. At that point,
consider extracting separate packages or modules:

```
blockyard/
├── internal/backend/           # Backend interface + shared types
├── internal/backend/docker/    # Docker implementation
├── internal/backend/process/   # Process (bwrap) implementation
├── internal/backend/k8s/       # Kubernetes implementation
```

**What triggers the refactor:** if adding the k8s backend to the single
module causes problems — conditional compilation sprawl, test matrix
complexity, or the interface definitions needing to change to accommodate
all backends — then extract. If build tags remain clean, keep the single
module.

**What the refactor involves:**

- Extract all interfaces (`Backend`, `WorkerHandle`, `SessionStore`,
  `WorkerRegistry`, `TaskStore`) and their associated types (`WorkerSpec`,
  `BuildSpec`, `BuildResult`, `ManagedResource`, `LogStream`) into a shared
  package
- Each backend package depends on the shared package for the interface
  definitions
- The main package depends on the shared package and on each backend package
  (optionally, via build tags)
- The mock backend stays in the main module (test-only code, no heavy deps)

### Interface Extraction for SessionStore, WorkerRegistry, TaskStore

v0 implemented `SessionStore`, `WorkerRegistry`, and `TaskStore` as concrete
`sync.Map`-backed structs with synchronous methods. The roadmap describes
them as interfaces with swappable implementations (in-memory for single-node,
PostgreSQL-backed for k8s HA). v1 continues with concrete structs since it
runs a single server.

For v4 multi-node deployments, these need to become interfaces:

- `SessionStore` → async methods, Redis or PostgreSQL-backed implementation
  for shared session state across nodes
- `WorkerRegistry` → async methods, shared registry so any node can route
  to any worker
- `TaskStore` → async methods, PostgreSQL-backed for HA

The current method signatures were designed to map cleanly onto interface
methods — the switch is mechanical: extract an interface and parameterize
`AppState` over the interface (same pattern as `Backend`).

**Trigger:** when the Kubernetes backend is implemented and multi-replica
server deployments are needed.

### Build Image Consolidation

v3 addresses multi-image builds for Docker by mounting the
`by-builder` Go binary from the server's cache into build containers
(bind mount to `/tools/by-builder`). pak itself runs as an R package
inside the build image, so no external binary mount is needed for pak.
For Kubernetes, the equivalent of the `by-builder` mount is an init
container that copies the binary onto a shared `emptyDir` volume, or
bundling `by-builder` into the build image directly — this is the v4
concern.
