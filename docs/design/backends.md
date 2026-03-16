# Backend Strategies

This document compares three backend strategies for running worker processes:
the current Docker/Podman backend, a daemonless OCI runtime backend, and a
bare process backend using kernel primitives directly. It also evaluates the
Posit Connect sandboxing model as prior art and explains why it is
insufficient for blockyard's threat model.

For the Backend interface definition and current Docker backend implementation,
see [architecture.md](architecture.md). For threat model and credential trust
model, see the same document.

## Threat Model Recap

blockyard apps execute **arbitrary user-supplied R code**. Apps can be
`access_type: public`, meaning completely unauthenticated users trigger code
execution. The isolation mechanism must defend against adversarial code, not
just accidental interference between well-meaning content items. Any credential
or token placed in the process space must be treated as exfiltrable.

The required isolation properties:

1. **Worker-to-worker isolation** — workers cannot see, signal, or communicate
   with each other.
2. **Worker-to-host isolation** — workers cannot access the server's management
   API, configuration, database, or Docker socket.
3. **Network scoping** — workers can reach the internet, Vault (OpenBao), and
   the IdP. Nothing else on the local network. Cloud metadata endpoints
   (`169.254.169.254`) are blocked.
4. **Resource limits** — workers cannot fork-bomb, OOM, or CPU-starve the host.
5. **Filesystem isolation** — workers see only their own app bundle and library
   (read-only), plus a writable tmpfs. No access to other apps' code, the
   server's data directory, or the host filesystem.
6. **Privilege containment** — workers run unprivileged with no Linux
   capabilities. No privilege escalation path.

## Why Not the Posit Connect Model

Posit Connect's local execution mode uses Linux namespaces to sandbox content
processes. It is worth understanding what Connect does — and does not do —
because it is the industry-standard R deployment platform and the natural
reference point for any alternative.

### What Connect does

- **Mount namespace** (`CLONE_NEWNS`): Each content process gets a remapped
  filesystem view via bind mounts. Connect hides its own configuration,
  database (`SQLite.Dir`), and data directory (`Server.DataDir`) from content.
  Each process sees only its own bundle at `/opt/rstudio-connect/mnt/app/`.
- **User namespace** (`CLONE_NEWUSER`): Optional, partial. Provides UID
  remapping when available. Disabled on some distros (RHEL 7 with
  `user.max_user_namespaces = 0`). When unavailable, the sandbox simply omits
  this layer.
- **Per-process temp directories**: Each content process gets its own `/tmp`
  via tmpfs mounted over the shared `/tmp`.
- **Home directory masking**: The real `/home` is hidden. The RunAs user's home
  directory is bind-mounted in its place. Controlled by
  `Applications.HomeMounting`.
- **RunAs user**: Content runs as an unprivileged user. By default all content
  shares a single `rstudio-connect` account. Per-content RunAs overrides and
  RunAsCurrentUser mode are available but require additional configuration.

### What Connect does not do

1. **No PID namespace.** Content processes can enumerate all host processes
   via `/proc`. If multiple content items share the same RunAs UID (the
   default), they can `kill(2)` each other.

2. **No network namespace.** Content processes share the host's full network
   stack. A malicious app can probe the local network, reach other content
   processes, access the Connect management API, reach cloud metadata
   endpoints, and exfiltrate data to arbitrary destinations. There is no
   per-process firewall.

3. **No seccomp or AppArmor/SELinux profiles.** No syscall filtering is
   applied to content processes.

4. **No cgroup resource limits.** No per-process CPU, memory, or I/O limits.

5. **Shared UID by default.** All content runs as the same `rstudio-connect`
   user unless the operator explicitly configures per-content RunAs overrides
   or RunAsCurrentUser mode. Shared UIDs mean shared filesystem permissions
   and the ability to signal other processes.

6. **Root process requirement.** The Connect server runs as root to call
   `unshare(2)` and create bind mounts. When running Connect inside Docker,
   the container requires `--cap-add=CAP_SYS_ADMIN` or `--privileged`.

### Why this is insufficient for blockyard

Connect's sandbox assumes **authenticated publishers** deploying their own
code, consumed by **authenticated viewers**. There is an implicit trust
chain — publishers are known humans accountable for what they deploy.
Connect's sandboxing defends against accidental interference between content
items, not against adversarial code.

blockyard's threat model is fundamentally different: apps run arbitrary R code,
apps can be public, and unauthenticated users can trigger code execution. The
absence of network isolation, PID isolation, syscall filtering, and resource
limits makes Connect's approach inadequate for this use case. A malicious app
running under Connect's sandbox could:

- Scan the local network and reach internal services.
- Access cloud metadata and steal instance credentials.
- Reach other content processes and interfere with them.
- Fork-bomb or OOM-kill the host with no resource limits.
- Enumerate host processes and extract information from `/proc`.

## Current Approach: Docker Backend

The production backend uses the Docker API (`github.com/docker/docker/client`)
to run each worker as an isolated container. Podman is supported transparently
via its Docker-compatible socket.

### Isolation properties

| Property | Mechanism |
|---|---|
| Filesystem | Overlay rootfs + read-only bind mounts for app and library. `--read-only` with tmpfs at `/tmp`. No host filesystem access. |
| PID | PID namespace per container. Processes cannot see or signal anything outside. |
| Network | Per-container bridge network. Server joins each network to proxy traffic. No inter-container connectivity. Metadata endpoint blocked via iptables. |
| Syscalls | Default seccomp profile (~44 dangerous syscalls blocked). |
| Capabilities | `--cap-drop=ALL`, `--security-opt=no-new-privileges`. |
| Resources | cgroups enforce `memory_limit` and `cpu_limit` per container. PID limits prevent fork bombs. |
| Privileges | Content runs as unprivileged user inside the container. No Docker socket mounted. |

### Advantages

- **Defense in depth.** All six isolation properties from the threat model are
  satisfied by default, with no additional host configuration.
- **Well-understood security model.** Container isolation is the
  industry-standard approach for running untrusted code. The attack surface
  (container escape) is well-studied and actively hardened.
- **Rootfs isolation.** Each container has its own filesystem image. Even if a
  process escapes other sandboxing layers, it lands in a minimal container
  rootfs, not the host filesystem.
- **Operational familiarity.** `docker ps`, `docker logs`, `docker inspect`
  are universally understood. Debugging is straightforward.
- **Image management.** R runtime, system libraries, and base packages are
  baked into a Docker image. Version pinning, reproducibility, and rollback
  are handled by image tags.

### Disadvantages

- **Startup latency.** Container creation, network creation, and network
  attachment add ~500ms–1s to cold-start time (on top of R startup time).
  This is the dominant cost for single-session-per-worker deployments where
  every new user pays the cold-start penalty.
- **Docker socket privilege.** The server needs access to the Docker socket
  (`/var/run/docker.sock`), which grants root-equivalent access to the host.
  In the Docker Compose deployment, the server container is given the socket
  via a bind mount. This is the standard Docker-out-of-Docker (DooD) pattern
  but it means a compromised server process can escape its own container
  trivially.
- **Daemon dependency.** Requires a running Docker (or Podman) daemon. This
  is an operational dependency that must be monitored, upgraded, and
  configured.
- **Network overhead.** Per-container bridge networks add complexity. The
  server must multi-home across all active networks to proxy traffic.
  Network creation and cleanup are additional API calls on every worker
  lifecycle event.
- **Resource overhead.** Each container carries the overhead of its own
  network namespace, mount namespace, and cgroup hierarchy. For deployments
  with many short-lived single-session workers, this overhead adds up.
- **Not portable.** Requires Linux with a container runtime. Development on
  macOS requires Docker Desktop or a Linux VM.

### Note: Daemonless OCI as a middle ground

A third option exists: calling a lightweight OCI runtime (`crun` or `runc`)
directly, without the Docker daemon. This is what Podman does under the
hood — blockyard would generate an OCI runtime spec (JSON) and call
`crun create` / `crun start` via `os/exec`. This gives full container
isolation (~50ms startup, rootfs via pre-built tarballs, all namespaces)
without the daemon or socket privilege.

In practice, the benefit over Docker is narrow. The main gain is eliminating
the Docker socket (root-equivalent privilege). But you lose `docker ps/logs/
inspect`, the Docker Go client, image pull/caching, and the monitoring
ecosystem — and must reimplement log capture, state tracking, and rootfs
management. The startup improvement (~50ms vs ~500ms) is real but modest
relative to R's own 1–3s startup. For blockyard's likely deployment
scenarios, the operational cost outweighs the security gain.

If the Docker socket privilege ever becomes a dealbreaker (e.g., a hardened
host policy that prohibits it), the OCI backend is the fallback that
preserves container-equivalent isolation. Until then, Docker or the process
backend below are the practical choices.

## Proposed Alternative: Process Backend

A local backend that spawns R processes directly on the host, using Linux
kernel primitives to achieve isolation equivalent to containers — without a
container runtime.

### Building blocks

The Linux kernel provides the same isolation primitives that containers use.
Containers simply bundle all of them behind a runtime API. A process backend
uses the same primitives directly:

| Primitive | Purpose | How containers use it |
|---|---|---|
| Namespaces (`clone`/`unshare`) | Visibility isolation (mount, PID, user, UTS, cgroup) | All namespaces enabled |
| seccomp-bpf | Syscall filtering | Default profile blocks ~44 syscalls |
| cgroups v2 | Resource limits (CPU, memory, IO, PIDs) | Per-container cgroup |
| Capabilities | Privilege restriction | `--cap-drop=ALL` |
| `pivot_root`/`chroot` | Filesystem root isolation | Overlay rootfs |
| AppArmor / SELinux | Mandatory access control | Optional profiles |
| Landlock (5.13+) | Unprivileged filesystem sandboxing | Not yet widely used |

Two practical tools wrap these primitives into usable interfaces:

**bubblewrap (`bwrap`)**: A single static binary designed for running untrusted
code. Used by Flatpak for desktop app sandboxing. Combines namespaces, bind
mounts, seccomp, and capability dropping into one CLI invocation:

```
bwrap \
  --unshare-pid --unshare-user --unshare-uts \
  --ro-bind /app/bundle /app \
  --ro-bind /rv-library /rv-library \
  --tmpfs /tmp \
  --proc /proc \
  --dev /dev \
  --die-with-parent \
  --new-session \
  --cap-drop ALL \
  -- R -e "shiny::runApp('/app', port=PORT)"
```

**systemd transient units**: `systemd-run` spawns a process with cgroups,
seccomp, filesystem protection, and capability dropping — all declaratively:

```
systemd-run --scope \
  --property=ProtectSystem=strict \
  --property=ReadOnlyPaths=/app \
  --property=TemporaryFileSystem=/tmp \
  --property=NoNewPrivileges=yes \
  --property=CapabilityBoundingSet= \
  --property=SystemCallFilter=@system-service \
  --property=MemoryMax=512M \
  --property=CPUQuota=50% \
  --property=TasksMax=64 \
  R -e "shiny::runApp('/app', port=PORT)"
```

### Network isolation without network namespaces

Network namespaces are the hardest primitive to use in isolation — creating one
gives the process a completely disconnected network stack, and reconnecting it
requires veth pairs, bridges, and NAT rules (i.e., reimplementing what the
container runtime does).

blockyard's network requirements are narrowly scoped: workers need to reach
**Vault (OpenBao)**, the **IdP**, and the **internet**. Nothing else. This
known allowlist enables a simpler approach: **per-UID iptables rules on the
shared host network**.

Each worker runs as a dedicated UID from a preallocated pool (e.g.,
`blockyard-w0` through `blockyard-w99`, UIDs 10000–10099). The iptables
`owner` match module (`-m owner --uid-owner`) applies firewall rules
per-UID without requiring a network namespace:

```bash
# Allowlist: Vault and IdP (insert before the deny rules)
-A OUTPUT -m owner --uid-owner 10000:10099 -d $VAULT_HOST -p tcp --dport $VAULT_PORT -j ACCEPT
-A OUTPUT -m owner --uid-owner 10000:10099 -d $IDP_HOST -p tcp --dport $IDP_PORT -j ACCEPT
-A OUTPUT -m owner --uid-owner 10000:10099 -p udp --dport 53 -j ACCEPT

# Block everything local
-A OUTPUT -m owner --uid-owner 10000:10099 -d 127.0.0.0/8 -j DROP
-A OUTPUT -m owner --uid-owner 10000:10099 -d 169.254.0.0/16 -j DROP
-A OUTPUT -m owner --uid-owner 10000:10099 -d 10.0.0.0/8 -j DROP
-A OUTPUT -m owner --uid-owner 10000:10099 -d 172.16.0.0/12 -j DROP
-A OUTPUT -m owner --uid-owner 10000:10099 -d 192.168.0.0/16 -j DROP

# Allow internet (public IPs)
-A OUTPUT -m owner --uid-owner 10000:10099 -j ACCEPT
```

This achieves:

- **Workers cannot reach each other** — inter-worker traffic would go via
  localhost or RFC1918, which is blocked.
- **Workers cannot reach the management API** — the server listens on
  localhost.
- **Workers cannot reach cloud metadata** — `169.254.0.0/16` is blocked.
- **Workers can reach Vault and the IdP** — explicit allowlist holes punched
  before the RFC1918 deny rules.
- **Workers can reach the internet** — public IPs are allowed.
- **The server can reach workers** — the server runs as a different UID, not
  subject to the worker rules. Workers bind on localhost; the server connects
  to them freely.

No network namespace. No veth pairs. No bridge setup. No NAT. Standard
iptables with the `owner` module, which has been in the kernel for decades.

**Modern variant — nftables + cgroup matching:** On cgroups v2, nftables can
match on `meta cgroup` instead of UID. Each worker's cgroup (created
automatically by systemd transient units) becomes the firewall selector. This
avoids preallocating a UID pool, but requires nftables.

### Isolation comparison

| Property | Docker backend | Process backend |
|---|---|---|
| Filesystem | Overlay rootfs, full isolation | Bind mounts via bwrap, read-only. No overlay rootfs — process sees host kernel's `/proc`, `/sys`. Adequate with careful mount construction. |
| PID | PID namespace per container | `CLONE_NEWPID` via bwrap. Equivalent. |
| Network | Per-container bridge + iptables | Per-UID iptables on shared host network. Same effective policy; no network namespace overhead. |
| Syscalls | Default seccomp profile | Same seccomp BPF profile applied via bwrap or systemd. Equivalent. |
| Capabilities | `--cap-drop=ALL` | `PR_SET_NO_NEW_PRIVS` + empty capability bounding set. Equivalent. |
| Resources | Per-container cgroup | Per-process cgroup via systemd or direct cgroupfs writes. Equivalent. |
| Rootfs | Separate overlay filesystem | **Weaker.** The process runs on the host filesystem, with visibility restricted by bind mounts. A sandbox escape lands on the real host filesystem, not a minimal container rootfs. |

The only meaningful gap is **rootfs isolation**: a container escape lands in
a container's minimal filesystem, while a bwrap escape lands on the host.
This is a defense-in-depth difference, not a primary security boundary —
both rely on the kernel for enforcement.

## Comparison

### Trade-off matrix

| Dimension | Docker | Process (bwrap) | Kubernetes |
|---|---|---|---|
| Startup latency | ~500ms–1s (container + network) | ~2ms (fork + exec) | ~2–5s (Pod scheduling + pull + start) |
| Runtime dependencies | Docker/Podman daemon + socket | bubblewrap (~100KB static binary) + systemd (for cgroups) | Kubernetes cluster + CNI with NetworkPolicy + ReadWriteMany PVC |
| Socket privilege | Root-equivalent Docker socket | None. iptables rules require initial root setup (one-time). | RBAC (namespace-scoped, auditable) |
| Rootfs isolation | Full (overlay filesystem) | Partial (bind mounts on host filesystem) | Full (container image per Pod) |
| Image management | Docker images, version pinning, registries | R + system libraries installed on host. Reproducibility is the operator's responsibility. | Same image model as Docker; pulled by kubelet |
| Operational tooling | `docker ps/logs/inspect` | `systemctl`, `journalctl`, `ps`, custom CLI | `kubectl get pods/logs/describe`, standard k8s tooling |
| Network overhead | Per-container bridge, server multi-homing | None — shared host network with per-UID firewall | CNI-managed; NetworkPolicy for isolation |
| Portability | Linux + container runtime | Linux only (kernel primitives). Not portable to macOS. | Any k8s cluster (cloud or on-prem) |
| Go integration | Mature Docker Go client | `os/exec` to call bwrap/systemd-run | Mature `k8s.io/client-go` |
| Maturity | Production-hardened, widely deployed | Requires careful implementation and testing | Kubernetes is battle-tested; integration is custom |
| Scaling model | Single host | Single host | Multi-node (horizontal scaling of both server and workers) |
| Database | SQLite (single-writer) | SQLite (single-writer) | PostgreSQL (multi-replica) |
| State sharing | In-memory (single process) | In-memory (single process) | Redis or PostgreSQL for cross-replica coordination |

### When to use which

**Docker backend** is the right choice when:

- The deployment is internet-facing with untrusted public users.
- Operators want defense-in-depth with the strongest available rootfs
  isolation and the least custom implementation.
- The operational team is already familiar with Docker/Podman.
- Image-based reproducibility (pinned R versions, system libraries) is
  important.
- Single-host deployment is sufficient.

**Process backend** is the right choice when:

- Startup latency is the primary concern (e.g., scale-to-zero with
  near-instant wake-up).
- The Docker socket privilege is unacceptable (e.g., hardened hosts that
  prohibit Docker-out-of-Docker).
- The deployment is internal-only or behind additional network controls
  that compensate for weaker rootfs isolation.
- Simplicity is valued — no daemon, no images, no socket.

**Kubernetes backend** is the right choice when:

- The deployment needs to scale beyond a single host — more workers than
  one machine can run, or HA requirements for the server itself.
- The organization already operates a Kubernetes cluster with a
  NetworkPolicy-capable CNI.
- Operational tooling expectations are Kubernetes-native (`kubectl`,
  Prometheus, Grafana, standard k8s observability).
- The Docker socket privilege model is unacceptable and RBAC-scoped
  permissions are required.
- ReadWriteMany storage (NFS, EFS, CephFS) is available.

All backends implement the same `Backend` interface. The choice is a
deployment-time configuration decision, not an architectural one.

## Implementation Notes

### Backend interface compatibility

The current `Backend` interface methods (`Spawn`, `Stop`, `HealthCheck`,
`Logs`, `Addr`, `Build`, `ListManaged`, `RemoveResource`) are
runtime-agnostic. A process backend implements them as:

| Method | Docker | Process (bwrap) | Kubernetes |
|---|---|---|---|
| `Spawn` | `ContainerCreate` + `ContainerStart` + network setup | `bwrap` + `exec` (or `systemd-run`) | Create Pod + Service + NetworkPolicy |
| `Stop` | `ContainerStop` + `ContainerRemove` + network cleanup | `kill` + `waitpid` | Delete Pod + Service + NetworkPolicy |
| `HealthCheck` | TCP connect to container IP | TCP connect to `127.0.0.1:PORT` | Pod Ready condition via k8s API |
| `Logs` | Docker log stream API | stdout/stderr pipes (or `journalctl` for systemd) | `Pods().GetLogs()` stream |
| `Addr` | Container IP on named network | `127.0.0.1:PORT` (allocated from a port pool) | Pod IP + port from `pod.Status.PodIP` |
| `Build` | Run-to-completion container | bwrap process with write access to library path | Job with `backoffLimit: 0`, wait for completion |
| `ListManaged` | Docker API filter by labels | Scan cgroup hierarchy or PID files for managed processes | Label selector query across Pods, Jobs, Services, NetworkPolicies |
| `RemoveResource` | Docker remove container/network | Kill process, remove cgroup, clean up temp dirs | Delete resource by name and kind |

### UID pool management

The process backend preallocates a range of system UIDs for workers (e.g.,
`blockyard-w0` through `blockyard-w99`). The UID pool is configured at install
time. Each `Spawn` call claims the next available UID; `Stop` returns it to
the pool. iptables rules are configured once for the entire UID range —
individual worker lifecycle events do not modify firewall rules.

### R runtime management

Unlike the Docker backend (where R is baked into the image), the process
backend requires R to be installed on the host. The configured R binary
path, library paths, and environment variables are set per-process at spawn
time. Package libraries are bind-mounted read-only, identically to the
Docker backend.

### Port allocation

Workers listen on localhost ports allocated from a configurable range. The
server connects to `127.0.0.1:PORT` to proxy traffic. Port allocation and
release follow the same lifecycle as UID allocation.

## Alternative C: Kubernetes Backend

The previous alternatives (OCI, process) are single-host isolation strategies
— different ways to sandbox a worker on the same machine. The Kubernetes
backend is a different dimension: it delegates worker lifecycle, isolation,
networking, and resource management to a cluster orchestrator. The server
talks to the Kubernetes API instead of a Docker socket or local runtime
binary.

This is the v2 production backend for multi-node deployments. The Docker
backend remains the recommended choice for single-host deployments.

### Architecture

The blockyard server runs as a Kubernetes Deployment. Worker processes run as
individual Pods in the same namespace (or a dedicated worker namespace).
Build tasks (dependency restore) run as Jobs. The server uses
`k8s.io/client-go` to manage all worker resources.

```
┌─────────────────────────────────────────────────────┐
│ Kubernetes cluster                                  │
│                                                     │
│  ┌──────────────┐     ┌──────────┐  ┌──────────┐   │
│  │  blockyard   │────▶│ worker   │  │ worker   │   │
│  │  server Pod  │     │ Pod      │  │ Pod      │   │
│  │              │────▶│ (app-A)  │  │ (app-B)  │   │
│  └──────┬───────┘     └──────────┘  └──────────┘   │
│         │                                           │
│         │  k8s API     ┌──────────┐                 │
│         └─────────────▶│ build    │                 │
│                        │ Job      │                 │
│                        └──────────┘                 │
│                                                     │
│  ┌──────────┐  ┌──────────┐  ┌───────────────────┐ │
│  │ PVC      │  │ Postgres │  │ Redis (optional)  │ │
│  │ (bundles)│  │          │  │                    │ │
│  └──────────┘  └──────────┘  └───────────────────┘ │
└─────────────────────────────────────────────────────┘
```

### Backend interface mapping

| Method | Kubernetes implementation |
|---|---|
| `Spawn` | Create a Pod + headless Service. Pod mounts the bundle PVC read-only at the worker mount path. Labels carry `dev.blockyard/managed`, `dev.blockyard/app-id`, `dev.blockyard/worker-id`. A NetworkPolicy is created alongside the Pod to enforce isolation. |
| `Stop` | Delete the Pod, Service, and NetworkPolicy by label selector. |
| `HealthCheck` | Query Pod status via the API (check `Ready` condition), or TCP connect to the Pod IP. Using the API is preferred — avoids network round-trips from the server to each worker and leverages the kubelet's existing probe infrastructure. |
| `Logs` | `corev1.Pods().GetLogs()` with `Follow: true`, wrapped in a `LogStream`. |
| `Addr` | Pod IP + Shiny port, read from `pod.Status.PodIP`. Alternatively, the headless Service's DNS name (`{worker-id}.{namespace}.svc.cluster.local:{port}`). Pod IP is simpler and avoids DNS propagation delay. |
| `Build` | Create a Job with `backoffLimit: 0` and `ttlSecondsAfterFinished`. The Job's Pod mounts the bundle PVC read-write for the library output path. Wait for Job completion, stream logs, return `BuildResult`. |
| `ListManaged` | List Pods, Jobs, Services, and NetworkPolicies with label selector `dev.blockyard/managed=true`. |
| `RemoveResource` | Delete the resource by name and kind. |

### Internal state

```go
type workerState struct {
    podName           string
    serviceName       string
    networkPolicyName string
    namespace         string
}
```

The `ManagedResource` type needs to accommodate Kubernetes resource kinds.
The current `ResourceKind` enum (`ResourceContainer`, `ResourceNetwork`) is
Docker-specific. Changing `ResourceKind` to a string makes it
backend-agnostic — callers never inspect the kind, they just pass it back
to `RemoveResource`. Each backend defines its own vocabulary:

```go
// Docker backend uses: "container", "network"
// Kubernetes backend uses: "pod", "job", "service", "networkpolicy"
type ManagedResource struct {
    ID   string
    Kind string
}
```

### Network isolation

Each worker Pod gets a NetworkPolicy that:

1. **Denies all ingress** except from Pods matching the blockyard server's
   label selector. Workers cannot receive traffic from each other or from
   anything else in the cluster.
2. **Allows egress to the internet** (public IPs) and to DNS (UDP/TCP 53).
3. **Denies egress to cluster-internal CIDRs** — the Pod/Service CIDR and
   the node CIDR. This prevents workers from reaching the Kubernetes API
   server, other services, and other Pods.
4. **Denies egress to cloud metadata** — `169.254.169.254/32`.
5. **Allows egress to OpenBao and the IdP** — explicit CIDR/port exceptions
   punched before the deny rules.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: blockyard-worker-{worker-id}
  labels:
    dev.blockyard/managed: "true"
    dev.blockyard/worker-id: "{worker-id}"
spec:
  podSelector:
    matchLabels:
      dev.blockyard/worker-id: "{worker-id}"
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/name: blockyard-server
  egress:
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
              - 10.0.0.0/8
              - 172.16.0.0/12
              - 192.168.0.0/16
              - 169.254.0.0/16
    - ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
```

**CNI requirement.** NetworkPolicy is part of the Kubernetes API but
enforcement depends on the CNI plugin. Calico and Cilium enforce it;
Flannel and the default kubenet do not. This is a hard prerequisite —
without a CNI that enforces NetworkPolicy, worker isolation is not
guaranteed. The Helm chart should document this and optionally run a
startup check (query a known NetworkPolicy and verify the CNI reports
support).

**OpenBao and IdP egress.** If OpenBao and the IdP run inside the cluster
(common in dev/staging), their Pod/Service CIDRs fall within the blocked
RFC1918 ranges. The NetworkPolicy needs explicit exceptions for their
endpoints. These would be configured via the Kubernetes config section
(allowed egress CIDRs/ports) and injected into the generated
NetworkPolicy.

### Bundle storage

The Docker backend uses named volumes or bind mounts. The Kubernetes
backend uses a **ReadWriteMany PersistentVolumeClaim** (PVC) mounted into
both the server Pod and every worker/build Pod. This is the closest
analogue and preserves the existing `WorkerSpec` / `BuildSpec` assumption
that `BundlePath` and `LibraryPath` are filesystem paths.

Supported PVC backends: NFS, AWS EFS, CephFS, GlusterFS. All support
hard links, which the live package installation design (see
[gaps.md](gaps.md)) requires for zero-cost per-worker library views.
Azure Files (SMB) does **not** support hard links and is incompatible
with this strategy.

**Visibility timing.** The live package install design relies on hard-linked
packages being immediately visible inside worker containers. With NFS-backed
PVCs, NFS attribute caching can delay directory entry visibility by seconds.
EFS and CephFS have strong read-after-write consistency. For NFS, the `noac`
mount option eliminates the delay at a performance cost, or the install API
can add a brief settle step after hard-linking.

The PVC is mounted into worker Pods as:

| Mount | Path | Mode |
|---|---|---|
| App bundle | `{pvc_mount}/{app_id}/{bundle_id}/` | Read-only |
| R library | `{pvc_mount}/{app_id}/{bundle_id}_lib/` | Read-only |
| Worker lib view | `{pvc_mount}/.worker-libs/{worker-id}/` | Read-only |

Build Jobs mount the library path read-write for `rv restore` output.

The `MountConfig` abstraction from the Docker backend is not needed — k8s
PodSpecs declare volume mounts directly. The path translation problem
(server-side path vs. container-side path) largely disappears when both
the server and workers mount the same PVC at the same path.

### Container hardening

Worker Pods use a restrictive SecurityContext:

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: [ALL]
  seccompProfile:
    type: RuntimeDefault
```

Resource limits map directly from `WorkerSpec`:

```yaml
resources:
  limits:
    memory: "{memory_limit}"  # from WorkerSpec.MemoryLimit
    cpu: "{cpu_limit}"        # from WorkerSpec.CPULimit
```

A tmpfs volume is mounted at `/tmp` for R's temporary file needs:

```yaml
volumes:
  - name: tmp
    emptyDir:
      medium: Memory
      sizeLimit: 256Mi
```

### Configuration

A new `[kubernetes]` config section, mutually exclusive with `[docker]`:

```toml
[kubernetes]
namespace = "blockyard"          # namespace for worker Pods and Jobs
image = "ghcr.io/cynkra/blockyard-r:latest"
shiny_port = 3838
rv_version = "v0.19.0"
pvc_name = "blockyard-bundles"   # ReadWriteMany PVC
pvc_mount_path = "/data/bundles" # mount point in all Pods
kubeconfig = ""                  # empty = in-cluster; path = out-of-cluster
```

Validation rejects configs where both `[docker]` and `[kubernetes]` are
set, or where neither is set. Backend selection is a config-time decision:

```go
var be backend.Backend
if cfg.Docker != nil {
    be, err = docker.New(ctx, cfg.Docker, cfg.Storage.BundleServerPath)
} else {
    be, err = kubernetes.New(ctx, cfg.Kubernetes)
}
```

### RBAC

The server's ServiceAccount needs a Role granting:

```yaml
rules:
  - apiGroups: [""]
    resources: [pods, pods/log, services]
    verbs: [create, get, list, watch, delete]
  - apiGroups: [batch]
    resources: [jobs, jobs/status]
    verbs: [create, get, list, watch, delete]
  - apiGroups: [networking.k8s.io]
    resources: [networkpolicies]
    verbs: [create, get, list, delete]
```

This is the Kubernetes analogue of Docker socket access. The improvement:
k8s RBAC is namespace-scoped, auditable, and grants only the specific
permissions needed. Docker socket access is root-equivalent with no
granularity.

The Helm chart creates the ServiceAccount, Role, and RoleBinding
automatically.

### Shared state

The single-host Docker deployment keeps all runtime state in-memory (worker
map, registry, session store, log store). With multiple server replicas
behind a Kubernetes Service, this state must be shared.

Two categories:

**Ephemeral runtime state** — high-frequency reads/writes, disposable on
restart. Redis is the natural backend:

| Current in-memory struct | Redis structure |
|---|---|
| WorkerMap (worker ID → app, draining, idle) | Hash: `blockyard:workers:{id}` |
| Registry (worker ID → host:port) | Hash: `blockyard:registry:{id}` |
| SessionStore (session ID → worker, user, access time) | Hash with TTL: `blockyard:sessions:{id}` |
| LogStore (worker ID → log lines) | Stream: `blockyard:logs:{worker-id}` |

Redis pub/sub or Streams also provide cross-replica event notification
(e.g., "worker evicted on replica A, all replicas update their local
caches").

**Persistent state** — apps, bundles, users, tokens, ACLs. PostgreSQL
replaces SQLite. The current `db.DB` struct uses `modernc.org/sqlite`
directly; this needs abstracting behind `database/sql` so both drivers
work.

**Staging.** Shared state is not required for the initial k8s backend
implementation. A single server replica with SQLite and in-memory state
works for validating the backend. Redis and PostgreSQL become necessary
only when scaling the server horizontally.

### Deployment artifacts

**Helm chart** — the primary distribution for Kubernetes. Covers:

- Server Deployment with health probes, resource requests, and RBAC
- ServiceAccount + Role + RoleBinding
- PVC (or reference to an existing one)
- PostgreSQL dependency (via subchart or external reference)
- Redis dependency (optional, for multi-replica)
- Ingress resource with cert-manager annotations
- ConfigMap for `blockyard.toml`
- Secret for sensitive config (OIDC client secret, OpenBao credentials)

**Graceful shutdown.** The server's existing shutdown sequence (management
listener → main listener → background goroutines → worker eviction) works
unchanged. The Pod's `terminationGracePeriodSeconds` must be ≥
`shutdown_timeout` so the kubelet does not SIGKILL the server mid-cleanup.

### Isolation comparison with Docker backend

| Property | Docker backend | Kubernetes backend |
|---|---|---|
| Filesystem | Overlay rootfs per container | Container image per Pod (equivalent) |
| PID | PID namespace per container | PID namespace per Pod (equivalent) |
| Network | Per-container bridge + iptables | NetworkPolicy (requires CNI support) |
| Syscalls | Default seccomp profile | `RuntimeDefault` seccomp profile (equivalent) |
| Capabilities | `--cap-drop=ALL` | `drop: [ALL]` in SecurityContext (equivalent) |
| Resources | cgroups via Docker | Resource limits in PodSpec → cgroups (equivalent) |
| Rootfs | Read-only container + tmpfs | `readOnlyRootFilesystem` + emptyDir tmpfs (equivalent) |
| Metadata endpoint | iptables rule per network | NetworkPolicy egress deny (equivalent, CNI-dependent) |
| Privilege model | Docker socket (root-equivalent) | RBAC (namespace-scoped, auditable) |

The isolation properties are equivalent. The privilege model is strictly
better — RBAC replaces root-equivalent socket access. The trade-off is
operational complexity (cluster, CNI, PVC, PostgreSQL) vs. the Docker
backend's single-host simplicity.

## Open Questions

1. **Hybrid deployments.** Could a single blockyard instance run both
   backends — Docker for public-facing apps, process for internal apps?
   The Backend interface already supports this; the routing layer would
   need per-app backend selection.

2. **bubblewrap vs. systemd vs. both.** bubblewrap handles namespace and
   mount isolation well; systemd handles cgroups well. Using both
   (bubblewrap inside a systemd transient unit) gives the cleanest
   separation but adds complexity. Alternatively, bubblewrap + direct
   cgroup writes, or systemd alone with its sandboxing directives.

3. **Rootfs hardening.** The bind-mount approach exposes the host's `/proc`
   and `/sys`. These could be masked (bwrap supports `--proc /proc` which
   mounts a new procfs), but `/sys` and other pseudo-filesystems need
   careful handling to avoid information leakage while keeping R functional.

4. **R package compatibility.** Some R packages use system libraries that
   must be present on the host (e.g., `libcurl`, `libxml2`, `libssl`).
   Without container images, system library management becomes the
   operator's responsibility. This is the largest operational difference.

5. **Testing strategy.** The process backend needs integration tests on a
   Linux host with bwrap, iptables, and a UID pool configured. CI must
   provision these. The Docker backend's tests are simpler (just a Docker
   socket).

6. **Kubernetes CNI enforcement.** The k8s backend's network isolation
   depends entirely on the CNI plugin enforcing NetworkPolicy. Should the
   server verify this at startup (e.g., create a test NetworkPolicy and
   confirm enforcement), or is documenting the requirement sufficient?
   Clusters without enforcement silently provide no worker isolation.

7. **PVC filesystem requirements.** The live package installation design
   requires hard link support on the PVC's underlying filesystem. NFS,
   EFS, and CephFS support this; Azure Files (SMB) does not. Should the
   k8s backend detect the filesystem type and warn, or is this purely a
   documentation concern?

8. **Worker Pod scheduling.** Should the k8s backend support node affinity,
   tolerations, or topology spread constraints for worker Pods? Operators
   may want workers on dedicated node pools (e.g., GPU nodes for certain
   apps, or spot instances for cost optimization). This could be exposed
   as optional fields in `[kubernetes]` config or as per-app overrides.

9. **Single-replica bootstrap.** The k8s backend can work initially with
   one server replica, SQLite, and in-memory state — deferring PostgreSQL
   and Redis to a later phase. Is this acceptable as a v2.0 scope, with
   multi-replica HA as v2.1?

10. **Pod vs. Deployment for workers.** Bare Pods are simpler and match
    the ephemeral worker lifecycle (no restart policy needed). Deployments
    with 1 replica add restart-on-failure semantics but introduce
    complexity (ReplicaSet management, rolling update semantics that don't
    apply). Bare Pods are likely the right choice — a crashed worker
    should be detected by health polling and re-spawned by the server,
    not auto-restarted by the kubelet with potentially stale state.
