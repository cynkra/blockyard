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

### bwrap setuid requirement (Debian 12+/Ubuntu 24.04+)

Debian 12 and Ubuntu 24.04 ship `bwrap` as a regular (non-setuid) binary.
With user namespaces enabled, bwrap *can* still enter a new namespace,
but `--uid <N>` / `--gid <N>` no longer produce a host-visible UID —
the kernel silently writes a namespace-local mapping that the host's
iptables `--uid-owner` rules cannot match. Workers then appear as
regular unprivileged processes from the init namespace, and the
per-worker egress firewall breaks.

Blockyard's preflight detects this with `checkBwrapHostUIDMapping` and
refuses to start with a clear error. The fix on those distros is:

```bash
sudo chmod u+s /usr/bin/bwrap
```

This is the same configuration Fedora/RHEL ship by default. An
alternative is to run blockyard as root, which inherits `CAP_SYS_ADMIN`
and bypasses the restriction; see the containerized guide for that
path.

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

Workers in the process backend run under a shared host GID
(`[process] worker_gid`). Use `iptables` owner-match rules to block
workers from reaching internal services:

```bash
# Block workers from reaching Redis (match by destination IP,
# not the entire internal subnet).
sudo iptables -A OUTPUT -m owner --gid-owner 65534 \
    -d 10.0.0.5 -j REJECT

# Block workers from reaching OpenBao.
sudo iptables -A OUTPUT -m owner --gid-owner 65534 \
    -d 10.0.0.6 -j REJECT

# Block cloud metadata.
sudo iptables -A OUTPUT -m owner --gid-owner 65534 \
    -d 169.254.169.254 -j REJECT
```

**Do not use a blanket `REJECT`** — workers legitimately need internet
access (CRAN mirrors, package downloads, `httr` calls from user code).
Scope each rule to a specific destination.

Persist the rules across reboots with `iptables-save` /
`iptables-restore` or your distro's equivalent.

Blockyard's preflight runs a probe binary inside a bwrap sandbox to
verify the rules are effective. If a worker can reach cloud metadata,
Redis, OpenBao, or the database at startup, the preflight logs a
warning or error.

## systemd unit

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

```bash
# Trigger the update — blockyard downloads the new version, forks a
# new process on an alt bind, drains the old server, and exits the
# old process once sessions have ended.
by admin update --yes --channel stable

# Watch the update progress (task log streams by default).
```

**Prerequisites:**

- Redis must be configured and reachable.
- The reverse proxy must be configured with every port in the
  `[update] alt_bind_range` as an upstream.
- The new blockyard binary must be present in the same location
  (`os.Executable()` resolves the running binary; operators upgrade
  by replacing `/usr/local/bin/blockyard` *before* running
  `by admin update`).

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
