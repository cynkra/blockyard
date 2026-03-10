# Phase 0-6: Health Polling + Orphan Cleanup + Log Capture

*Draft — full spec to be written before implementation begins.*

Background operational concerns that run alongside the main server.

## Reminders

- **Network isolation test:** Spawn two workers, verify they cannot reach
  each other. Deferred from phase 0-2 — this is the natural place since
  multi-worker scenarios are already exercised here.

- **Native mode E2E test:** Phase 0-2 unit-tests the cgroup/hostname
  parsing for server container ID detection, but there is no E2E test for
  the native-mode path (server running outside Docker, no network joining).
  Add an integration test here or document as a manual verification step.
