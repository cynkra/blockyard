---
title: What is Blockyard?
description: An overview of Blockyard and the blockr threat model it is built for.
weight: 1
---

Blockyard is a self-hosted platform for running
[blockr](https://github.com/blockr-org/blockr) applications in production.
blockr apps evaluate user-supplied R expressions inside every session,
which is a security model no general-purpose
[Shiny](https://shiny.posit.co/) host was designed for. Blockyard
provides hardened per-session worker isolation (as a Docker container
or a `bubblewrap`-sandboxed process), per-user credential management
via a Vault-compatible secrets manager, server-side dependency resolution,
and built-in board storage — the full blockr stack as a single binary.

## How it works

1. **You deploy a bundle** — run `by deploy ./my-board` or upload a
   `.tar.gz` archive via the REST API. Bundles can include dependency
   metadata (`renv.lock`, `DESCRIPTION`, or `manifest.json`), or none at
   all — the server discovers what's needed.
2. **Blockyard resolves dependencies server-side** — it spins up a build
   container and uses [pak](https://pak.r-lib.org/) to install packages.
   Unpinned bundles can be refreshed in place later without a redeploy.
3. **Users visit the app** — when a request hits `/app/<name>/`, Blockyard
   spawns a worker on demand and reverse-proxies HTTP and WebSocket
   traffic to it. Each session gets its own isolated worker.

Workers run in one of two backends:

- **Docker backend** (default). Each worker is a container with a
  private bridge network, read-only filesystem, all Linux capabilities
  dropped, `no-new-privileges` set, and cloud metadata endpoint access
  blocked. High-security deployments can run workers under the
  [Kata](https://katacontainers.io/) runtime for per-session VMs.
- **Process backend.** Each worker is a `bubblewrap`-sandboxed child
  process with PID/mount/user namespaces, capability dropping, and
  seccomp filtering. No container runtime required. Suitable for
  hosts where the Docker socket is unacceptable or where cold-start
  latency matters.

See [Backend Security](/docs/guides/backend-security/) for the full
comparison and [Docker worker hardening](/docs/guides/deploying/#docker-worker-hardening)
for the Docker backend's baseline settings.

## Authentication & Authorization

Blockyard supports OIDC-based authentication. When configured, users must
log in before accessing apps. System roles (admin, publisher, viewer) are
assigned directly by blockyard admins — not derived from IdP groups. Per-app
access control lists (ACLs) provide fine-grained authorization, and each app
has a visibility setting (`acl`, `logged_in`, or `public`).

Optionally, Blockyard integrates with a Vault-compatible secrets manager
(tested against [OpenBao](https://openbao.org/); HashiCorp Vault also works)
for per-user credential management, allowing Shiny apps to securely access
external services like AI providers, databases, and object storage.

## Key concepts

**App**
:   A named Shiny application registered in Blockyard. Each app has a unique
    URL-safe slug (e.g. `my-dashboard`) and can have multiple bundles, with
    one marked as active.

**Bundle**
:   A versioned deployment artifact — the `.tar.gz` you upload. Bundles go
    through a build step (dependency restore) before becoming ready to serve.

**Worker**
:   A running process serving a Shiny app. Implemented as a Docker
    container or a bubblewrap-sandboxed child process depending on the
    configured backend. Workers are spawned on-demand and pinned to
    user sessions via cookies.

**Session**
:   A user's connection to a running worker. Sessions are tracked
    automatically — they start when the proxy assigns a user to a worker
    and end when the worker is stopped or the session is idle for too long.

**Task**
:   A background operation with streamable logs. Currently used for
    dependency restoration builds.
