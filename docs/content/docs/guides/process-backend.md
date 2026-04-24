---
title: Process Backend (Native)
description: Deploy blockyard on a bare Linux host using the bubblewrap-based process backend.
weight: 7
---

The **process backend** runs each worker as a `bubblewrap`-sandboxed
child of the blockyard server — no Docker socket, no outer container,
no daemon to talk to. This guide covers native deployment: installing
blockyard on a Linux host, configuring the sandbox, and operating
rolling updates.

For the containerized variant (blockyard-process image, Docker Compose,
seccomp profile extraction) see
[Process Backend (Containerized)]({{< relref "process-backend-container.md" >}}).
For the security trade-offs that determine which backend to pick, see
[Backend Security]({{< relref "backend-security.md" >}}).

## Prerequisites

### Linux distribution

The process backend is Linux-only. Bubblewrap is unavailable on macOS
and Windows; operators who need blockyard on those platforms should use
the Docker backend via Docker Desktop.

Supported distributions:

| Distribution | Minimum version | Notes |
|---|---|---|
| Debian | 12 (Bookworm) | bwrap is not setuid by default; see below |
| Ubuntu | 24.04 LTS | same caveat as Debian 12+ |
| Fedora | 39 | bwrap setuid by default |
| RHEL/Rocky | 9 | same as Fedora |
| Arch | rolling | bwrap setuid by default |

Alpine is **not** supported: R on musl has known numerics and locale
issues, and several common R packages fail to build against musl.

### System packages

Install the runtime dependencies on the target host:

```bash
# Debian / Ubuntu
sudo apt-get install -y bubblewrap r-base ca-certificates iptables

# Fedora / RHEL
sudo dnf install -y bubblewrap R ca-certificates iptables

# Arch
sudo pacman -S --needed bubblewrap r ca-certificates iptables
```

### Kernel: unprivileged user namespaces

The process backend relies on `bwrap --unshare-user`. Verify the kernel
allows it:

```bash
cat /proc/sys/kernel/unprivileged_userns_clone  # should print 1
```

If you see `0`, enable it:

```bash
echo "kernel.unprivileged_userns_clone = 1" | sudo tee /etc/sysctl.d/99-blockyard.conf
sudo sysctl --system
```

### Worker egress isolation options

Per-worker egress filtering (layer 6 in the isolation model — see
[backends.md](/docs/design/backends/#deployment-mode--isolation-layer-matrix))
depends on how blockyard is deployed:

- **Root blockyard (containerized deployments):** the spawn path
  fork+setuid's each worker into a distinct host UID before
  `exec(bwrap)`, so operator `iptables -m owner --uid-owner` rules
  match worker traffic.
- **Non-root blockyard (native or unprivileged containers):** the
  fork+setuid path fails without CAP_SETUID. Reach layer 6 via
  [cgroup-v2 delegation](#per-worker-egress-via-cgroup-v2-delegation)
  and `iptables -m cgroup --path` rules instead.
- **k8s / restricted containers:** when neither path is available,
  use the Docker backend for per-worker network namespaces.

On Ubuntu 23.10+ the kernel's AppArmor restriction on unprivileged
user namespaces
(`kernel.apparmor_restrict_unprivileged_userns=1`) intercepts any
non-root `unshare(CLONE_NEWUSER)` unless the caller runs under a
profile granting `userns`. Blockyard ships a narrow profile — see
[AppArmor profile](#apparmor-profile-ubuntu-2310).

### AppArmor profile (Ubuntu 23.10+)

Extract the shipped profile and load it:

```bash
by admin install-apparmor
sudo apparmor_parser -r /etc/apparmor.d/blockyard
```

Or, from a built image:

```bash
docker run --rm --entrypoint cat \
    ghcr.io/cynkra/blockyard-process:${VERSION} \
    /etc/blockyard/apparmor/blockyard | sudo tee /etc/apparmor.d/blockyard
sudo apparmor_parser -r /etc/apparmor.d/blockyard
```

The profile grants the `userns` permission narrowly to blockyard and
its subprocesses (`bwrap`, the `bwrap-exec` shim, the worker R
interpreter) so rootless bwrap can create its sandbox user namespace.
It does **not** confine blockyard itself — blockyard is the trusted
component here; the workers it spawns are confined by bwrap's
capability drop, seccomp, and bind-mount restrictions, not by
AppArmor.

The alternative, `sysctl kernel.apparmor_restrict_unprivileged_userns=0`,
disables the restriction host-wide for every unprivileged process.
The profile is the narrow equivalent.

## Install blockyard

Download the release binary:

```bash
VERSION=1.2.3
curl -fsSL -o blockyard "https://github.com/cynkra/blockyard/releases/download/v${VERSION}/blockyard-linux-$(uname -m)"
chmod +x blockyard
sudo mv blockyard /usr/local/bin/
```

Create the system user and data directories:

```bash
sudo useradd --system --create-home --home-dir /var/lib/blockyard blockyard
sudo mkdir -p /etc/blockyard /var/lib/blockyard/bundles /var/lib/blockyard/db
sudo chown -R blockyard:blockyard /var/lib/blockyard
```

Install the seccomp profile (optional but recommended):

```bash
# Either:
sudo curl -fsSL -o /etc/blockyard/seccomp.bpf \
    "https://github.com/cynkra/blockyard/releases/download/v${VERSION}/blockyard-bwrap-seccomp.bpf"

# Or extract from the process-backend image:
docker run --rm --entrypoint cat \
    ghcr.io/cynkra/blockyard-process:${VERSION} \
    /etc/blockyard/seccomp.bpf > /tmp/blockyard-seccomp.bpf
sudo mv /tmp/blockyard-seccomp.bpf /etc/blockyard/seccomp.bpf
```

## Configure `blockyard.toml`

Minimal configuration at `/etc/blockyard/blockyard.toml`:

```toml
[server]
bind = "127.0.0.1:8080"
backend = "process"

[storage]
bundle_server_path = "/var/lib/blockyard/bundles"
bundle_worker_path = "/app"

[database]
driver = "sqlite"
path = "/var/lib/blockyard/db/blockyard.db"

[process]
bwrap_path = "/usr/bin/bwrap"
r_path = "/usr/bin/R"
seccomp_profile = "/etc/blockyard/seccomp.bpf"
port_range_start = 10000
port_range_end = 10999
worker_uid_range_start = 60000
worker_uid_range_end = 60999
worker_gid = 65534

# Required for rolling updates (two blockyard processes share state).
[redis]
url = "redis://localhost:6379"

# Alt bind range for the rolling-update new server.
[update]
alt_bind_range = "8090-8099"
drain_idle_wait = "5m"
```

The `[redis]` and `[update]` sections are only required if you want
rolling updates. Single-node deployments without rolling updates can
omit both.

## Egress firewall

Workers run under the shared host GID configured in
`[process] worker_gid` (default `65534`). Use `iptables` owner-match
rules to block them from reaching specific internal destinations:

```bash
# Block cloud metadata.
sudo iptables -A OUTPUT -m owner --gid-owner 65534 \
    -d 169.254.169.254 -j REJECT

# Block workers from reaching Redis, the vault, and the database.
sudo iptables -A OUTPUT -m owner --gid-owner 65534 -d 10.0.0.5 -j REJECT
sudo iptables -A OUTPUT -m owner --gid-owner 65534 -d 10.0.0.6 -j REJECT
sudo iptables -A OUTPUT -m owner --gid-owner 65534 -d 10.0.0.7 -j REJECT
```

Persist the rules across reboots with `iptables-save` /
`iptables-restore` or your distro's equivalent.

Blockyard's preflight spawns a probe under the worker UID/GID and
attempts TCP connections to the same internal endpoints at startup.
A reachable metadata endpoint is reported as an error; reachable
Redis/vault/database endpoints are reported as warnings.

> [!IMPORTANT]
> Rules must be **destination-scoped**, not blanket `REJECT` — workers
> legitimately need the open internet (CRAN, package downloads, user
> `httr` calls). For the rationale and the host-UID-mapping requirement
> that makes `-m owner` actually match, see
> [Backend Security](/docs/guides/backend-security/#2-install-a-destination-scoped-egress-firewall).

## Per-worker egress via cgroup-v2 delegation

For non-root deployments (and as an alternative to `-m owner` for
root deployments), blockyard moves each worker's PID into a delegated
cgroup-v2 subtree so operators can match worker traffic with
`iptables -m cgroup --path <path>/workers`. The preflight check
`cgroup_delegation` reports at startup whether this mechanism is
available on the host.

Prerequisites:

- Host is on cgroup-v2 unified hierarchy
  (`grep cgroup2 /proc/mounts` shows a line).
- The `xt_cgroup` netfilter module is loaded
  (`lsmod | grep xt_cgroup`, or `sudo modprobe xt_cgroup`; add to
  `/etc/modules-load.d/` for persistence).
- blockyard's cgroup is delegated. With systemd, add `Delegate=yes`
  to the service unit (see below).

### systemd unit with cgroup delegation

`/etc/systemd/system/blockyard.service`:

```ini
[Unit]
Description=Blockyard R application server
After=network-online.target redis.service
Wants=network-online.target

[Service]
Type=simple
User=blockyard
Group=blockyard
ExecStart=/usr/local/bin/blockyard --config /etc/blockyard/blockyard.toml
Restart=on-failure
RestartSec=5s

# Delegate blockyard's cgroup-v2 subtree to the service, so blockyard
# can create a `workers/` subcgroup and enroll each worker PID into
# it. `iptables -m cgroup --path` rules then match worker traffic.
Delegate=yes

# Shared ceilings — per-worker cgroup limits are not enforced by the
# process backend. These apply to the entire blockyard service unit
# including all workers.
MemoryMax=16G
CPUQuota=800%

# Stop signal — SIGUSR1 enters drain mode (workers survive), SIGTERM
# fully shuts down.
KillSignal=SIGTERM
TimeoutStopSec=60s

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now blockyard
sudo systemctl status blockyard
```

With `Delegate=yes` in place, the startup preflight reports the
delegated path in `cgroup_delegation`. Install iptables rules
matching that path — typically
`system.slice/blockyard.service/workers`:

```bash
CGPATH=system.slice/blockyard.service/workers
sudo iptables -A OUTPUT -m cgroup --path "$CGPATH" \
    -d 169.254.169.254 -j REJECT
sudo iptables -A OUTPUT -m cgroup --path "$CGPATH" \
    -d <redis-ip>   -j REJECT
sudo iptables -A OUTPUT -m cgroup --path "$CGPATH" \
    -d <openbao-ip> -j REJECT
```

If the `xt_cgroup` module is missing, the preflight escalates
`cgroup_delegation` to WARNING and iptables will fail rule
installation at runtime with "No chain/target/match by that name".

## Reverse proxy for rolling updates

Rolling updates spawn a new blockyard process alongside the old one on
a different port. An external reverse proxy fronts both bind ports and
routes by health.

### Caddy

```caddy
blockyard.example.com {
    reverse_proxy 127.0.0.1:8080 127.0.0.1:8090 127.0.0.1:8091 {
        health_uri /healthz
        health_interval 5s
        health_timeout 2s
        lb_policy first
    }
}
```

During a rolling update, the old server's `/healthz` returns 503 as
soon as `drain_idle_wait` begins. Caddy stops routing new traffic to
it; existing sessions (keep-alive) stay on the old upstream until
they end. The new server binds on `8090-8099` (picked by the
orchestrator from `alt_bind_range`), and Caddy starts routing to it.

### Traefik

```yaml
http:
  services:
    blockyard:
      loadBalancer:
        healthCheck:
          path: /healthz
          interval: 5s
          timeout: 2s
        servers:
          - url: http://127.0.0.1:8080
          - url: http://127.0.0.1:8090
          - url: http://127.0.0.1:8091
```

The pattern is the same: list every port in the upstream pool and let
health checks pick the live one.

## Rolling update walkthrough

Use [`by admin update`](/docs/reference/cli/#by-admin-update) to trigger
a rolling update. Blockyard forks a new process on an alternate bind,
drains the old server, and exits the old process once sessions have
ended:

```bash
by admin update --yes --channel stable
```

The command streams the orchestrator task log; `by admin status` shows
the current state out-of-band.

**Prerequisites:**

- Redis must be configured and reachable (`[redis]` section — see
  [Configuration reference](/docs/reference/config/#redis-optional)).
- The reverse proxy must be configured with every port in the
  `[update] alt_bind_range` (see
  [`[update]`](/docs/reference/config/#update-optional) in the
  configuration reference) as an upstream.
- The new blockyard binary must be present in the same location as
  the running one. Operators upgrade by replacing
  `/usr/local/bin/blockyard` *before* running `by admin update`;
  `os.Executable()` resolves the running binary path.

**Failure modes:**

- If the new binary fails to bind, the orchestrator retries the next
  port in the range.
- If `/readyz` doesn't return 200 within `proxy.worker_start_timeout`,
  the orchestrator kills the new process and leaves the old one
  running (no drain).
- If the new server's watchdog detects unhealthy behavior after
  activation, the orchestrator kills the new process, undrains the
  old server, and resumes normal operation.

## Rollback

`by admin rollback` returns `501 Not Implemented` on the process
backend. Rollback requires the previous version's binary, which the
process variant does not track — the operator's install scheme owns
the binary path.

Manual rollback:

1. Restore the database backup from
   `/var/lib/blockyard/db/.backups/<timestamp>`.
2. Swap `/usr/local/bin/blockyard` back to the previous version.
3. `sudo systemctl restart blockyard`.

The database backup is written before every `by admin update` and
contains both the pre-update schema version and a snapshot of the
data.

## Limitations

- **No per-worker resource limits.** The process backend does not
  enforce CPU or memory ceilings per worker; `default_cpu_limit` /
  `default_memory_limit` / per-app overrides are silently ignored.
  Use systemd's `MemoryMax` / `CPUQuota` on the service unit for
  shared ceilings.
- **No per-worker network isolation.** Workers share the host network
  stack (just like the server). Egress is gated by the iptables
  owner-match rules above.
- **No automated rollback.** See above.
- **No macOS support.** Use containerized mode or the Docker backend.

See [Backend Security]({{< relref "backend-security.md" >}}) for a
full comparison with the Docker backend.
