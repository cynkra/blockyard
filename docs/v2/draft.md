# blockr.cloud v2 — Draft Notes

This document collects v2 items from the roadmap and architectural decisions
deferred from v0/v1 planning.

## Deferred from v0

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
blockr-cloud/               # binary + app logic
blockr-cloud-core/          # traits: Backend, WorkerHandle, SessionStore, etc.
blockr-backend-docker/      # depends on core + bollard
blockr-backend-k8s/         # depends on core + kube-rs
```

**What triggers the refactor:** if adding the k8s backend to the single crate
causes problems — conditional compilation sprawl, test matrix complexity, or
the trait definitions needing to change to accommodate both backends — then
extract. If feature flags remain clean, keep the single crate.

**What the refactor involves:**

- Extract all traits (`Backend`, `WorkerHandle`, `SessionStore`,
  `WorkerRegistry`, `TaskStore`) and their associated types (`WorkerSpec`,
  `BuildSpec`, `BuildResult`, `ManagedResource`, `LogStream`) into
  `blockr-cloud-core`
- Each backend crate depends on `blockr-cloud-core` for the trait definitions
- The main crate depends on `blockr-cloud-core` and on each backend crate
  (optionally, via features)
- The mock backend stays in the main crate (test-only code, no heavy deps)

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
