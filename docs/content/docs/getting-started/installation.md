---
title: Installation
description: How to install and run Blockyard.
weight: 2
---

## Server

### Prerequisites

- A Linux host (Blockyard runs as a container or native binary)
- One of:
  - **Docker backend:** Docker or Podman with a Docker-compatible socket, or
  - **Process backend:** `bubblewrap` (`bwrap`) and `R` installed on the host. Linux only.

See [Backend Security](/docs/guides/backend-security/) for the
trade-offs between the two backends and guidance on which to pick.

### Image variants

Three pre-built images are published to `ghcr.io/cynkra/`:

| Image | Backends compiled in | When to use |
|---|---|---|
| `blockyard:<version>` | Docker + process | Default. The "everything" image; switch backends via `[server] backend` in TOML. |
| `blockyard-docker:<version>` | Docker only | Slim image for Docker-only deployments — the Docker SDK is the only backend dependency. |
| `blockyard-process:<version>` | Process only | For containerized process-backend deployments. Ships `bubblewrap`, R, and the compiled bwrap seccomp profile. No Docker SDK. |

`:latest` tracks the most recent release on each variant.

### Running with Docker (Docker backend)

The easiest way to run the Docker-backend variant is as a container with
access to the host's Docker socket:

```bash
docker run -d \
  --name blockyard \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v blockyard-data:/data \
  -e BLOCKYARD_DOCKER_IMAGE=ghcr.io/rocker-org/r-ver:4.4.3 \
  ghcr.io/cynkra/blockyard-docker:latest
```

This gives Blockyard access to Docker for spawning worker containers, and
persists application data (bundles, database) in a named volume.

### Running with the process backend

The process backend avoids the Docker socket mount entirely by
sandboxing workers with `bubblewrap` instead. It has two deployment
modes:

- [Process Backend (Native)](/docs/guides/process-backend/) — run
  `blockyard` directly on a Linux host with `bwrap` and `R` installed.
- [Process Backend (Containerized)](/docs/guides/process-backend-container/) —
  run the `blockyard-process` image with a custom seccomp profile. No
  Docker socket, no `CAP_SYS_ADMIN`.

The containerized variant is the recommended choice for deployments
that cannot bind-mount the Docker socket.

API authentication requires OIDC configuration and
[Personal Access Tokens](/docs/guides/authorization/#personal-access-tokens).
See the [Authorization guide](/docs/guides/authorization/) for details.

### Running from source

```bash
git clone https://github.com/cynkra/blockyard.git
cd blockyard
go build -o blockyard ./cmd/blockyard
```

Copy and edit the example configuration:

```bash
cp blockyard.toml my-config.toml
# Edit my-config.toml — at minimum, set docker.image
```

Run the server:

```bash
./blockyard -config my-config.toml
```

### Verifying the installation

```bash
curl http://localhost:8080/healthz
# => 200 OK
```

---

## CLI

The `by` command-line client lets you deploy and manage apps from your
terminal. See the [CLI Reference](/docs/reference/cli/) for the full command list.

### Download a release binary

Download the latest binary for your platform from the
[releases page](https://github.com/cynkra/blockyard/releases) and place it
somewhere on your `PATH`:

```bash
# Example for Linux amd64
curl -Lo by https://github.com/cynkra/blockyard/releases/latest/download/by-linux-amd64
chmod +x by
sudo mv by /usr/local/bin/
```

### Build from source

If you have Go 1.25+ installed:

```bash
go install github.com/cynkra/blockyard/cmd/by@latest
```

Or clone and build:

```bash
git clone https://github.com/cynkra/blockyard.git
cd blockyard
go build -o by ./cmd/by
sudo mv by /usr/local/bin/
```

### Verify

```bash
by --help
```

### Log in

After installing, authenticate against your Blockyard server:

```bash
by login --server https://blockyard.example.com
```

This opens your browser to create a Personal Access Token and stores the
credentials in `~/.config/by/config.json`. See [Quick Start](/docs/getting-started/quickstart/)
for a full walkthrough.
