---
title: Process Backend (Containerized)
description: Deploy the process backend using the blockyard-process Docker image, without bind-mounting the Docker socket.
weight: 8
---

The **containerized process backend** runs blockyard as PID 1 in a
Docker container, with `bubblewrap` and R pre-installed, and no
Docker socket mount. A compromised blockyard server is confined to
the container — it has no root-equivalent access to the host.

This is the recommended mode for multi-tenant deployments where you
don't want to expose `/var/run/docker.sock` and don't need Docker's
per-worker bridge networks.

For the native (bare-metal) variant, see
[Process Backend (Native)]({{< relref "process-backend.md" >}}).

## The image

`ghcr.io/cynkra/blockyard-process:<version>` ships the blockyard
binary compiled with `-tags 'minimal,process_backend'` (no Docker SDK
in the dep graph), plus:

- R 4.4.3 from `rocker/r-ver`
- `bubblewrap`
- The compiled bwrap seccomp profile at `/etc/blockyard/seccomp.bpf`
- The outer-container seccomp profile at `/etc/blockyard/seccomp.json`
  (for extraction to the host)

## Why the outer seccomp profile is needed

Docker's default seccomp profile blocks the `clone`/`clone3`/`unshare`/
`setns` syscalls with the `CLONE_NEWUSER` flag unless the process has
`CAP_SYS_ADMIN`. When `bwrap` inside the blockyard container tries to
`unshare(CLONE_NEWUSER)` to create a worker sandbox, the kernel rejects
the call with `EPERM` and the worker fails to spawn.

Blockyard ships a custom seccomp profile that relaxes **only** the
user-namespace-creation syscalls. No other capability gates are
relaxed; no additional syscalls are added. The rest of Docker's
default restrictions stay in place.

Operators must pass this profile to the outer container via
`--security-opt seccomp=<path>`. Docker reads the profile from the
host, not from inside the container — so you need a copy on the host
before the container starts.

### Extracting the profile

Three options:

**Option 1 — `docker run --entrypoint cat`** (no local blockyard
binary required):

```bash
docker run --rm --entrypoint cat \
    ghcr.io/cynkra/blockyard-process:1.2.3 \
    /etc/blockyard/seccomp.json \
    > /etc/blockyard/seccomp.json
```

The `--entrypoint cat` override is required because the image's
default entrypoint is `blockyard --config ...`; without it the `cat`
would end up as an argument to blockyard.

**Option 2 — `by admin install-seccomp`** (if you have the `by`
CLI installed):

```bash
sudo by admin install-seccomp --target /etc/blockyard/seccomp.json
```

The profile is embedded in the `by` binary via `//go:embed`, so no
network access is required.

**Option 3 — download from GitHub Releases:**

```bash
VERSION=1.2.3
sudo curl -fsSL -o /etc/blockyard/seccomp.json \
    "https://github.com/cynkra/blockyard/releases/download/v${VERSION}/blockyard-outer.json"
```

## Docker Compose example

```yaml
services:
  blockyard:
    image: ghcr.io/cynkra/blockyard-process:1.2.3
    security_opt:
      - seccomp=/etc/blockyard/seccomp.json
    volumes:
      - blockyard-data:/var/lib/blockyard
      - ./blockyard.toml:/etc/blockyard/blockyard.toml:ro
    environment:
      - BLOCKYARD_REDIS_URL=redis://redis:6379
    networks:
      - state
      - default
    ports:
      - "8080:8080"
    depends_on:
      - redis

  redis:
    image: redis:7-alpine
    volumes:
      - redis-data:/data
    networks:
      - state
    # Redis is only reachable from blockyard, not from workers.
    # Expose no host port.

  caddy:
    image: caddy:2
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
    ports:
      - "80:80"
      - "443:443"
    networks:
      - default

volumes:
  blockyard-data:
  redis-data:

networks:
  state:
    internal: true
  default:
```

Note the **lack** of:

- `--privileged`
- `cap_add`
- `/var/run/docker.sock` mount

The container needs only the custom seccomp profile. `bubblewrap`
inside creates user-namespaced worker sandboxes without any
additional host privileges.

## Egress firewall (containerized mode)

The iptables owner-match pattern from the native guide works
differently here: the outer container has its own UID namespace, and
worker processes appear as the container's own UID (typically root)
from the host's perspective. Host-side iptables rules matching
`--gid-owner 65534` will not fire.

Two approaches:

1. **Run iptables rules inside the container.** The
   blockyard-process image does not ship `iptables`, so this is not
   a drop-in option. Use the everything image
   (`ghcr.io/cynkra/blockyard:<v>`) if you need this.

2. **Use Docker network segmentation.** Put Redis, OpenBao, and the
   database on an `internal: true` network that the blockyard
   container joins, and put worker-egress-sensitive services on a
   separate network that workers cannot reach. This is cleaner than
   iptables but requires the operator to be deliberate about
   service topology.

Blockyard's preflight runs the same worker-egress probe in
containerized mode. Review the startup logs for warnings about
reachable internal services.

## Rolling updates in containerized mode

`by admin update` returns `501 Not Implemented` when blockyard runs
as PID 1 in a container. The process orchestrator's fork+exec model
requires the old and new blockyard to run as sibling processes under
a parent that survives the cutover — killing PID 1 stops the
container regardless of child process tricks.

For containerized rolling updates, use your container runtime's
update mechanism:

**Docker Compose:**

```bash
# Edit docker-compose.yml: update image tag to blockyard-process:1.2.4
docker compose pull blockyard
docker compose up -d blockyard
```

**Kubernetes:**

```bash
kubectl set image deployment/blockyard \
    blockyard=ghcr.io/cynkra/blockyard-process:1.2.4
```

**Nomad:**

```bash
nomad job run blockyard-1.2.4.nomad
```

All three give you rolling-update semantics via the runtime's own
cutover machinery (health checks, graceful shutdown, session
draining), which is more battle-tested than blockyard's
fork+exec path.

## Limitations

Same as [native mode]({{< relref "process-backend.md#limitations" >}}),
plus:

- **No `by admin update` / `by admin rollback`.** Use the container
  runtime.
- **Egress firewall requires either the everything image or
  network segmentation.**
