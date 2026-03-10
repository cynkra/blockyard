# Phase 0-3: Content Management

*Draft — full spec to be written before implementation begins.*

Bundle upload, dependency restoration, content registry. These form the
deployment pipeline — the path from "user has a tar.gz" to "app is ready to
run."

## Reminders

- **Image pulling** was deferred from phase 0-2. This phase must add
  pull-if-missing logic before `backend.spawn()` and `backend.build()` calls.
  Without it, deployments fail silently if the configured image isn't
  pre-pulled on the host.
