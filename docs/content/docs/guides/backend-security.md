---
title: Backend Security
description: Security trade-offs between the Docker and process backends, and how to harden the process backend.
weight: 6
---

Blockyard supports two worker backends: **Docker** (the default) and
**process** (bubblewrap-based, single-host). Both run untrusted R code with
PID, mount, user-namespace, and capability isolation. They differ on three
axes — network isolation between workers, per-worker resource limits, and
syscall filtering — and on how much privilege the server process itself
needs. This page helps you decide which backend fits your deployment and,
if you pick the process backend, how to close the gaps with
operator-configured controls.

## The trade-off in one paragraph

The Docker backend gives each worker a private bridge network and cgroup
resource limits, but the server needs access to `/var/run/docker.sock` —
root-equivalent access to the host in a Docker-out-of-Docker deployment.
The process backend needs no socket, just a `bwrap` binary, so a
compromised server in containerized mode is confined to its own outer
container instead of landing on the host. In exchange, workers share the
host network stack and have no per-worker CPU/memory ceiling. Both gaps
are mitigated by operator-installed firewall rules and outer-container
resource limits, but neither is enforced by the backend itself.

## What both backends share

Regardless of which backend you pick, each worker runs with:

- **Mount namespace** with a read-only bundle and library mount, a
  writable `/tmp` tmpfs, and no access to other apps' files or the
  server's data directory.
- **PID namespace** — workers cannot see, signal, or trace processes
  outside their own sandbox.
- **User namespace** with all Linux capabilities dropped
  (`CapDrop: ALL`, `no_new_privs`).
- **Minimal environment** — the server's database URL, Redis credentials,
  Vault tokens, and session secret are never exported into worker
  processes.

Worker-to-worker filesystem access, signal delivery, and `/proc` visibility
are blocked on both backends. The open questions are *network*,
*resource limits*, and *syscall filtering*.

## What only the Docker backend gives you

| Property | Docker backend | Process backend |
|---|---|---|
| **Per-worker network** | Each worker gets a private bridge network. Inter-worker traffic is impossible at the kernel level; cloud metadata (`169.254.169.254`) is blocked via iptables per network. | Workers share the host network stack. Worker A can TCP-connect to worker B's loopback Shiny port, reach services on the host network, and (without operator firewall rules) hit cloud metadata. |
| **Per-worker resource limits** | Memory and CPU limits are enforced by the kernel via cgroups. PID limits prevent fork bombs. | No cgroup delegation. The outer container's (or systemd slice's) limits act as a shared ceiling — one runaway worker can starve its siblings. |
| **Syscall filtering** | Docker's default seccomp profile is applied automatically. | The `blockyard` and `blockyard-process` images ship a compiled BPF profile at `/etc/blockyard/seccomp.bpf` and point `process.seccomp_profile` at it by default; `bwrap` applies it via `--seccomp`. Native deployments can install the same profile from the release tarball or extract it from the image. |
| **Server privilege model** | Server needs `/var/run/docker.sock`, which grants root-equivalent access to the host. | Server needs only a `bwrap` binary. In containerized mode, the outer container needs a custom seccomp profile that allows `CLONE_NEWUSER`, but no capabilities, no socket, and no daemon dependency. |

The first three rows are strengths of the Docker backend. The last row is
a strength of the process backend — the weakest link is different in each
case.

## Decision guide

Pick the **Docker backend** when:

- The deployment is internet-facing with untrusted public users and you
  want cross-worker network isolation enforced by the kernel, not by the
  operator.
- Per-worker CPU and memory limits need to be guaranteed, not shared
  across all workers on the host.
- You run multi-tenant workloads where a compromised worker reaching
  another worker's Shiny port is part of your threat model.
- The operational team already runs Docker or Podman.

Pick the **process backend** when:

- The Docker socket is unacceptable — e.g. hardened hosts that prohibit
  Docker-out-of-Docker, or policies that forbid root-equivalent mounts in
  server containers.
- Cold-start latency matters. The Docker backend creates a container
  and per-worker network on every spawn; the process backend skips
  both steps.
- The deployment is single-tenant or internal, and cross-worker network
  isolation is not part of the threat model.
- Operational simplicity matters — no daemon, no socket, only a
  lightweight sandbox helper on the host or in the image.

If you need cross-worker network isolation *and* the reduced server
privilege of the process backend, use the Docker backend on a host that
tolerates the socket mount. No configuration gives you both properties at
once on the process backend.

## Hardening the process backend

Picking the process backend is the start of the work, not the end. The
backend provides the per-worker sandbox; the operator provides the
network and resource boundaries around it. Three controls matter.

### 1. Prefer containerized mode

Native (bare-host) mode is supported for dedicated single-purpose VMs,
but containerized mode is the default recommendation:

- A sandbox escape lands in the outer container's rootfs, not on the host.
- The outer container's cgroup acts as a shared ceiling, so no single
  worker can take the whole host down.
- Portability is better — the same image runs on Linux, macOS via Docker
  Desktop, or any OCI runtime.

See [Process Backend (Containerized)](/docs/guides/process-backend-container/)
for the image layout, the custom seccomp profile Docker needs to allow
`CLONE_NEWUSER`, and the Docker Compose recipe. The containerized
deployment runs with no `--privileged`, no `--cap-add SYS_ADMIN`, and
no Docker socket mount.

### 2. Install a destination-scoped egress firewall

Workers share the host network stack, so the only way to keep them away
from sensitive destinations is an egress firewall outside the sandbox.
Blockyard assigns each running worker a unique host UID from
`[process] worker_uid_range_start..worker_uid_range_end` (default
60000–60999), and a shared `worker_gid` (default 65534, `nogroup`). This
gives iptables an `owner` match to key rules on.

```sh
# Allow blockyard's own egress to internal services.
iptables -A OUTPUT -m owner --uid-owner blockyard -j ACCEPT

# Block worker access to specific internal destinations. The worker GID
# is the match; the destination address narrows the rule.
iptables -A OUTPUT -m owner --gid-owner 65534 -d 169.254.169.254 -j REJECT
iptables -A OUTPUT -m owner --gid-owner 65534 -d <redis-ip>      -j REJECT
iptables -A OUTPUT -m owner --gid-owner 65534 -d <vault-ip>    -j REJECT
iptables -A OUTPUT -m owner --gid-owner 65534 -d <database-ip>   -j REJECT
```

The rules are **destination-scoped, not blanket**. A rule like
`iptables -A OUTPUT -m owner --gid-owner 65534 -j REJECT` would also cut
off the open internet — but workers legitimately need to fetch data,
call external APIs, and download models. Enumerate the specific internal
endpoints you want to protect and scope each rule to them.

Blockyard's preflight actively verifies these rules at startup by
spawning a probe under the worker UID/GID and attempting TCP connections
to the same internal endpoints. A reachable cloud metadata endpoint is
reported as an error; reachable Redis, vault, or database endpoints
are reported as warnings. The probe never tests the open internet —
workers are expected to reach it.

#### Host UID mapping is load-bearing

`iptables -m owner` matches on the *host-side* UID/GID of the process
creating the socket, not on the namespace-local UID inside the sandbox.
For bubblewrap's `--uid`/`--gid` flags to produce host-visible IDs, one
of these must hold:

- **Blockyard runs as root** (typical containerized mode, where
  blockyard is PID 1 root inside a container). bwrap inherits root and
  can set up any uid_map.
- **bwrap is setuid root on the host** (`sudo chmod u+s /usr/bin/bwrap`
  if the distro package doesn't ship it that way). Required for native
  non-root deployments.

If neither condition holds, workers still start — but they all run
under blockyard's own host UID, and the operator's iptables rules
silently match nothing. Blockyard's preflight catches this at startup
by spawning a bwrap probe with a distinct sandbox UID and checking
whether the child's host-side `/proc/<pid>/status` reports the
requested UID. If it reports an error here, your iptables rules will
not work until you add the setuid bit (or switch to running blockyard
as root).

### 3. Apply resource limits outside the sandbox

The process backend does not enforce per-worker CPU, memory, or PID
limits. Any limit must be applied at a layer above the sandbox:

- **Containerized mode:** set `--memory`, `--cpus`, and `--pids-limit`
  on the outer container. These act as a shared ceiling across all
  workers — a runaway worker cannot take the whole host down, but it
  can starve its siblings inside the same container.
- **Native mode on a systemd host:** run blockyard under a systemd
  unit with `MemoryMax=`, `CPUQuota=`, and `TasksMax=`. The same
  shared-ceiling semantics apply.

If per-worker limits (not just a shared ceiling) are part of your
requirements, the process backend is the wrong choice. The configuration
fields `server.default_memory_limit` and `server.default_cpu_limit` are
accepted for compatibility with the Docker backend, but the process
backend's preflight emits a warning when they are set and does not
enforce them.

## What neither backend mitigates

Three classes of risk remain regardless of backend choice:

- **Code execution within a worker's own scope.** A malicious app can
  read any file its bundle mounts give it access to, exfiltrate data
  over allowed egress (including the open internet on both backends),
  and exhaust CPU up to its allotted ceiling. Neither backend defends
  against what the worker legitimately runs — it only constrains what
  the worker can reach.
- **Credentials inside the process.** Any secret injected into a worker
  (e.g. a vault-issued per-user API token) is readable by the code
  running in that worker. Treat any credential in worker scope as
  exfiltrable by the code in that session. See
  [Credential Management](/docs/guides/credentials/) for the scoping
  model.
- **Shared kernel attack surface.** Both backends isolate workers with
  Linux namespaces and seccomp, but they share the host kernel. A
  kernel vulnerability that allows namespace escape compromises every
  worker on the host. Keeping the host kernel (and Docker/bwrap)
  patched is the primary defence. For public-internet deployments where
  this risk is unacceptable, run worker containers under the
  [Kata](https://katacontainers.io/) runtime (`docker.runtime` or
  `docker.runtime_defaults` in the config) so each worker gets its own
  guest kernel inside a lightweight VM.

## Further reading

- [Process Backend (Native)](/docs/guides/process-backend/) — operator
  walkthrough for bare-Linux deployments: prerequisites, firewall
  rules, systemd unit, rolling updates.
- [Process Backend (Containerized)](/docs/guides/process-backend-container/)
  — operator walkthrough for the `blockyard-process` image: seccomp
  profile extraction, Docker Compose recipe, network segmentation.
- [Docker worker hardening in the deploying guide](/docs/guides/deploying/#docker-worker-hardening)
  — the Docker backend's baseline hardening.
- [Configuration reference](/docs/reference/config/) — `[server] backend`,
  the `[process]` section, and the full set of config keys.
