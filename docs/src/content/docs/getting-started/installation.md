---
title: Installation
description: How to install and run Blockyard.
---

## Server

### Prerequisites

- **Docker** (or Podman with a Docker-compatible socket)
- A Linux host (Blockyard runs as a container or native binary)

### Running with Docker (recommended)

The easiest way to run Blockyard is as a Docker container with access to the
host's Docker socket:

```bash
docker run -d \
  --name blockyard \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v blockyard-data:/data \
  -e BLOCKYARD_DOCKER_IMAGE=ghcr.io/rocker-org/r-ver:4.4.3 \
  ghcr.io/cynkra/blockyard:latest
```

This gives Blockyard access to Docker for spawning worker containers, and
persists application data (bundles, database) in a named volume.

API authentication requires OIDC configuration and
[Personal Access Tokens](/guides/authorization/#personal-access-tokens).
See the [Authorization guide](/guides/authorization/) for details.

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
terminal. See the [CLI Reference](/reference/cli/) for the full command list.

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
credentials in `~/.config/by/config.json`. See [Quick Start](/getting-started/quickstart/)
for a full walkthrough.
