# blockyard v3 — Draft Notes

v3 has two tracks: the lightweight process backend (the headline feature)
and deferred single-node features that didn't make the v2 cut. The process
backend is the architectural work; the deferred items are scoped extensions
to the existing Docker deployment.

## Process Backend

See [backends.md](../backends.md) for the full design and isolation
analysis.

### Process Backend Implementation

Implement the `Backend` interface using bubblewrap (`bwrap`) for process
sandboxing. Workers are spawned as bwrap-sandboxed R processes with PID
namespace isolation, filesystem isolation via bind mounts, seccomp
filtering, and capability dropping. No per-worker network isolation or
resource limits — these are deferred to the Docker and Kubernetes backends.

### Containerized Deployment Mode

A Docker image shipping blockyard, R, bwrap, and system libraries. Runs
with a custom seccomp profile allowing user namespace creation — no Docker
socket, no `CAP_SYS_ADMIN`. The recommended deployment mode for most users
of the process backend. Provides rootfs containment: a bwrap sandbox escape
lands in the outer container's filesystem, not the host.

### Native Deployment Mode

Documentation and tooling for running the process backend directly on a
Linux host without an outer container. Operator provisions R, bwrap, and
system libraries.

### Custom Seccomp Profile

A JSON seccomp profile based on Docker's default, adding `CLONE_NEWUSER`
to the allowlist. Shipped alongside the Docker Compose configuration.

## Deferred Single-Node Features

### Data Mounts

App owners can map additional host directories into worker containers —
e.g., shared model files, reference datasets, or writable scratch space.

#### Mount Specification

Each mount has three fields:

| Field      | Required | Description                                      |
|------------|----------|--------------------------------------------------|
| `source`   | yes      | Named mount source (defined by admin), optionally followed by a subpath — e.g., `models` or `models/v2` |
| `target`   | yes      | Absolute path inside the worker container — e.g., `/data/models` |
| `readonly` | no       | Default `true`. Set to `false` for writable mounts. |

Stored as a JSON column (`data_mounts`) on the `apps` table, consistent
with other per-app settings like `memory_limit` and `cpu_limit`.

#### Admin-Defined Mount Sources

The server config defines a whitelist of named mount sources. App owners
can only mount from these sources — they never specify raw host paths.

```toml
[[storage.data_mounts]]
name = "models"
path = "/data/shared-models"

[[storage.data_mounts]]
name = "scratch"
path = "/data/scratch"
```

`path` is from the server's perspective (i.e., inside the server
container in a DooD setup). The existing `MountConfig` logic in
`mounts.go` handles translation to the host-side path before passing it
to the Docker API — the same mechanism that already translates bundle
paths across native, bind-mount, and volume deployment modes.

When an app owner specifies `source: "models/v2"`, the server:
1. Splits into source name (`models`) and subpath (`v2`).
2. Validates the source name against the configured mount sources.
3. Validates the subpath contains no `..` components (path traversal).
4. Resolves the full server-side path (`/data/shared-models/v2`).
5. Translates to the host-side path via `MountConfig`.
6. Adds the bind mount to the worker container spec.

This gives operators full control over which host directories are
exposed, while app owners get a portable, environment-agnostic
interface. The same app config works across staging and production —
only the admin-defined paths differ.

#### Interaction with Existing Mounts

Worker containers already receive two read-only mounts: the bundle at
`/app` and the restored library at `/blockyard-lib`. Data mounts must
not collide with these paths or with each other. The server rejects
mount specs where `target` conflicts with a reserved path.

### Docker Daemon Hardening

The server communicates with the Docker daemon via the Docker socket.
A compromised server process has unrestricted access to the Docker API —
it could create privileged containers, mount arbitrary host paths, use
host networking, or mount the Docker socket itself into a worker. Data
mounts increase the mount surface area, making this more visible.

#### Docker Authorization Plugins

Docker supports [authorization plugins](https://docs.docker.com/engine/extend/plugins_authorization/)
that intercept every API request before the daemon processes it. An
authorization plugin can inspect the request (image, mounts, network
mode, capabilities, etc.) and return allow or deny.

The policy for blockyard workers should enforce:

- **Mount sources** are restricted to configured data mount paths,
  bundle storage, and library paths. No access to `/`, `/etc`,
  the Docker socket, or other sensitive host paths.
- **No privileged containers.** Workers must never run with
  `--privileged` or elevated capabilities.
- **No host networking.** Workers use the blockyard-managed bridge
  network only.
- **Image allowlist.** Workers may only use the server-wide default
  image or explicitly configured per-app images.

This can be implemented as a purpose-built plugin (a small HTTP server
that parses Docker API create-container requests and validates against
the policy) or via an existing OPA-based plugin like `opa-docker-authz`
with a Rego policy encoding the above rules.

The authorization plugin runs as a daemon-level configuration — the
operator enables it in Docker's `daemon.json`. This means enforcement
is independent of blockyard's application code: even if the server is
fully compromised, the Docker daemon itself refuses to create containers
that violate the policy.

### Multiple Execution Environment Images

Per-app image selection. Add an `image` field to app configuration that
overrides the server-wide `[docker] image` default. Operators or app
developers specify which image to use per deployment.

This interacts with the `build_image` config: rv's library path is
namespaced by R version/arch/codename, so the build image's R version must
match the worker image. The fix is to mount the `rv` binary from the server
container into build containers (bind mount to `/usr/local/bin/rv`),
collapsing back to a single image for builds. This works with Docker DooD;
the Kubernetes variant (init container or shared volume) is a v4 concern.

### UI Branding and Customization

- **Customizable cold-start loading page** — v2 ships a default
  blockyard-branded spinner. v3 makes it configurable (custom HTML,
  logo, messaging).
- **In-app navigation chrome** — navbar, app switcher for navigating
  between deployed apps without returning to the dashboard.
- **General branding** — configurable logo, colors, landing page
  content for the app browser.
