# blockyard v2 — Draft Notes

This document collects v2 items from the roadmap and architectural decisions
deferred from v0/v1 planning.

## Deferred from v0/v1

### Backend crate extraction

v0 uses a single crate with feature-gated backends:

```toml
[features]
default = ["docker"]
docker = ["dep:bollard"]
```

`docker.rs` is behind `#[cfg(feature = "docker")]`. This works for one or two
backends but may become unwieldy when Kubernetes arrives. At that point,
consider extracting a trait crate:

```
blockyard/               # binary + app logic
blockyard-core/          # traits: Backend, WorkerHandle, SessionStore, etc.
blockyard-backend-docker/      # depends on core + bollard
blockyard-backend-k8s/         # depends on core + kube-rs
```

**What triggers the refactor:** if adding the k8s backend to the single crate
causes problems — conditional compilation sprawl, test matrix complexity, or
the trait definitions needing to change to accommodate both backends — then
extract. If feature flags remain clean, keep the single crate.

**What the refactor involves:**

- Extract all traits (`Backend`, `WorkerHandle`, `SessionStore`,
  `WorkerRegistry`, `TaskStore`) and their associated types (`WorkerSpec`,
  `BuildSpec`, `BuildResult`, `ManagedResource`, `LogStream`) into
  `blockyard-core`
- Each backend crate depends on `blockyard-core` for the trait definitions
- The main crate depends on `blockyard-core` and on each backend crate
  (optionally, via features)
- The mock backend stays in the main crate (test-only code, no heavy deps)

### Trait extraction for SessionStore, WorkerRegistry, TaskStore

v0 implemented `SessionStore`, `WorkerRegistry`, and `TaskStore` as concrete
`DashMap`-backed structs with synchronous methods. The roadmap describes them
as traits with swappable implementations (in-memory for single-node,
PostgreSQL-backed for k8s HA). v1 continues with concrete structs since it
runs a single server.

For v2 multi-node deployments, these need to become traits:

- `SessionStore` → async methods, Redis or PostgreSQL-backed implementation
  for shared session state across nodes
- `WorkerRegistry` → async methods, shared registry so any node can route
  to any worker
- `TaskStore` → async methods, PostgreSQL-backed for HA

The current method signatures were designed to map cleanly onto async trait
methods — the switch is mechanical: extract a trait, make methods async, and
parameterize `AppState` over the trait (same pattern as `Backend`).

**Trigger:** when the Kubernetes backend is implemented and multi-replica
server deployments are needed.

### Build image consolidation

v0 introduced a `build_image` config field (separate from `image`) as a
shortcut to avoid downloading `rv` on every build. For v2's bring-your-own-
image support (roadmap item #36), this split creates a version-matching
problem: rv's library path is namespaced by R version/arch/codename, so the
build image's R version must match the worker image.

Preferred path: collapse back to a single image and mount the `rv` binary
from the server container into build containers (bind mount to
`/usr/local/bin/rv`). This avoids the download cost without the version-
matching problem. Only works with shared filesystem (Docker DooD); for
Kubernetes, use an init container or shared volume.

## Roadmap v2 features

From `../roadmap.md` items 31–39:

1. **Kubernetes backend** — Deployments for apps, Jobs for tasks; `kube-rs`
2. **Bundle rollback** — activate a previous bundle; drain sessions gracefully
3. **Per-content resource limit enforcement** — CPU/memory caps via Docker /
   K8s (fields carried in `WorkerSpec` from v0)
4. **CLI tool** — dedicated Rust binary for deployment and management
5. **Web UI** — admin dashboard, content browser, log viewer; credential
   enrollment UI
6. **Multiple execution environment images** — per-app image selection
7. **Scale-to-zero** — idle shutdown; pair with pre-warming
8. **Seat-based pre-warming** — pre-started container pools
9. **Runtime package installation** — writable library mount for user-driven
   package experimentation
10. **Soft-delete for apps** — mark apps as deleted instead of immediate
   removal; background cleanup routine purges deleted apps after a
   retention period. Enables undo and audit trails.
