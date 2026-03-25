---
title: What is Blockyard?
description: An overview of Blockyard and what it does.
---

Blockyard is a hosting platform for [Shiny](https://shiny.posit.co/)
applications. It runs each app in an isolated Docker container, handles
dependency restoration, and reverse-proxies HTTP and WebSocket traffic to
the right worker.

## How it works

1. **You deploy a bundle** — a `.tar.gz` archive of your Shiny app source code,
   optionally with dependency metadata (`renv.lock`, `DESCRIPTION`, or `manifest.json`).
2. **Blockyard restores dependencies** — it resolves a manifest, spins up a
   build container, and uses [pak](https://pak.r-lib.org/) to install packages.
3. **Users visit the app** — when a request hits `/app/<name>/`, Blockyard
   spawns a worker container on demand and reverse-proxies HTTP and WebSocket
   traffic to it.

Workers are isolated from each other via per-container bridge networks.
Containers run with a read-only filesystem, all Linux capabilities dropped,
`no-new-privileges` set, and cloud metadata endpoint access blocked
(`169.254.169.254`). See [Deploying an App](/guides/deploying/#container-security)
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

**Task**
:   A background operation with streamable logs. Currently used for
    dependency restoration builds.
