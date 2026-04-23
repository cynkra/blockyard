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
  -e BLOCKYARD_DOCKER_IMAGE=ghcr.io/cynkra/blockyard-worker:4.4.3 \
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

### Quick install (Linux, macOS)

One-liner that downloads the matching release binary, marks it
executable, and drops it into `/usr/local/bin`:

```bash
curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh
```

Install a specific version or a custom directory:

```bash
curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh -s -- \
  --version v0.1.0 --install-dir "$HOME/.local/bin"
```

The same script installs the `blockyard` server binary when invoked with
`--server` (Linux only — for other platforms run the container image):

```bash
curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh -s -- --server
```

Environment variables `BLOCKYARD_VERSION`, `BLOCKYARD_INSTALL_DIR`, and
`BLOCKYARD_BINARY` seed the defaults; passing the matching flag still
wins. Piping to `sh` is optional — download the script first to inspect
it if you prefer.

### Download a release binary

Pick the asset that matches your platform:

| Platform | Asset |
|---|---|
| Linux amd64 | `by-linux-amd64` |
| Linux arm64 | `by-linux-arm64` |
| macOS Intel | `by-darwin-amd64` |
| macOS Apple Silicon | `by-darwin-arm64` |
| Windows amd64 | `by-windows-amd64.exe` |

Download it and place it on your `PATH`. The `-f` flag makes `curl` exit
with an error on HTTP failures (e.g. 404) instead of saving the error
page as your binary:

```bash
# Replace <asset> with the filename from the table above
curl -fLo by https://github.com/cynkra/blockyard/releases/latest/download/<asset>
chmod +x by
sudo mv by /usr/local/bin/
```

You can also grab the binary directly from the
[releases page](https://github.com/cynkra/blockyard/releases).

### Build from source

If you have Go 1.25+ installed:

```bash
go install github.com/cynkra/blockyard/cmd/by@main
```

`@main` tracks the current main branch — the most recent release tag
predates the `by` CLI, so `@latest` will not yet resolve a working
version.

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
