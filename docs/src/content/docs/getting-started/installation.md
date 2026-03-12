---
title: Installation
description: How to install and run Blockyard.
---

## Prerequisites

- **Docker** (or Podman with a Docker-compatible socket)
- A Linux host (Blockyard runs as a container or native binary)

## Running with Docker (recommended)

The easiest way to run Blockyard is as a Docker container with access to the
host's Docker socket:

```bash
docker run -d \
  --name blockyard \
  -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v blockyard-data:/data \
  -e BLOCKYARD_SERVER_TOKEN=your-secret-token \
  ghcr.io/cynkra/blockyard:latest
```

This gives Blockyard access to Docker for spawning worker containers, and
persists application data (bundles, database) in a named volume.

## Running from source

```bash
git clone https://github.com/cynkra/blockyard.git
cd blockyard
go build -o blockyard ./cmd/blockyard
```

Copy and edit the example configuration:

```bash
cp blockyard.toml my-config.toml
# Edit my-config.toml — at minimum, change server.token
```

Run the server:

```bash
./blockyard -config my-config.toml
```

## Verifying the installation

```bash
curl http://localhost:8080/healthz
# => 200 OK
```
