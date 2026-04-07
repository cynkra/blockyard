---
title: What is Blockyard?
description: An overview of Blockyard and the blockr threat model it is built for.
weight: 1
---

Blockyard is a self-hosted platform for running
[blockr](https://github.com/blockr-org/blockr) applications in production.
blockr apps evaluate user-supplied R expressions inside every session,
which is a security model no general-purpose
[Shiny](https://shiny.posit.co/) host was designed for. Blockyard provides
hardened container-per-session isolation, per-user credential management
via [OpenBao](https://openbao.org/), server-side dependency resolution,
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
   spawns a worker container on demand and reverse-proxies HTTP and
   WebSocket traffic to it. Each session gets its own container.

Workers are isolated from each other via per-container bridge networks.
Containers run with a read-only filesystem, all Linux capabilities dropped,
`no-new-privileges` set, and cloud metadata endpoint access blocked
(`169.254.169.254`). For high-security deployments, workers can run under
the [Kata](https://katacontainers.io/) runtime, which gives each session
its own lightweight VM. See
[Deploying an App](/docs/guides/deploying/#container-security)
for the full list of security settings.

## Authentication & Authorization

Blockyard supports OIDC-based authentication. When configured, users must
log in before accessing apps. System roles (admin, publisher, viewer) are
assigned directly by blockyard admins — not derived from IdP groups. Per-app
access control lists (ACLs) provide fine-grained authorization, and each app
has a visibility setting (`acl`, `logged_in`, or `public`).

Optionally, Blockyard integrates with [OpenBao](https://openbao.org/) (a
Vault-compatible secrets manager) for per-user credential management, allowing
Shiny apps to securely access external services like AI providers, databases,
and object storage.

## Key concepts

**App**
:   A named Shiny application registered in Blockyard. Each app has a unique
    URL-safe slug (e.g. `my-dashboard`) and can have multiple bundles, with
    one marked as active.

**Bundle**
:   A versioned deployment artifact — the `.tar.gz` you upload. Bundles go
    through a build step (dependency restore) before becoming ready to serve.

**Worker**
:   A running container serving a Shiny app. Workers are spawned on-demand
    and pinned to user sessions via cookies.

**Session**
:   A user's connection to a running worker. Sessions are tracked
    automatically — they start when the proxy assigns a user to a worker
    and end when the worker is stopped or the session is idle for too long.

**Task**
:   A background operation with streamable logs. Currently used for
    dependency restoration builds.
