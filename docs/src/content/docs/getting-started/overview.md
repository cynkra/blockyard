---
title: What is Blockyard?
description: An overview of Blockyard and what it does.
---

Blockyard is a hosting platform for [Shiny](https://shiny.posit.co/)
applications. It runs each app in an isolated Docker container, handles
dependency restoration, and reverse-proxies HTTP and WebSocket traffic to
the right worker.

## How it works

1. **You deploy a bundle** — a `.tar.gz` archive of your Shiny app source code
   and an `rv.lock` file describing its R package dependencies.
2. **Blockyard restores dependencies** — it spins up a build container, runs
   [`rv sync`](https://github.com/a2-ai/rv) to install packages, and caches
   the resulting library.
3. **Users visit the app** — when a request hits `/app/<name>/`, Blockyard
   spawns a worker container on demand and reverse-proxies HTTP and WebSocket
   traffic to it.

Workers are isolated from each other via per-container bridge networks.
Containers run with a read-only filesystem, all Linux capabilities dropped,
and `no-new-privileges` set.

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
