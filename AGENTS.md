# Agent Guidelines

## Communication style

When presenting open questions or concerns, first give an overview, then
always walk through them one-by-one. Never dump a list and ask for bulk
feedback.

Be concise and critical. Skip compliments and pleasantries — focus on
problems, trade-offs, and things that might be wrong.

## Development environment

Prerequisites for building and running the test suite:

- Go (version per `go.mod`)
- Node 22 + npm (for the Tailwind/DaisyUI CSS build under `internal/ui/`)
- Docker + `docker compose` (used by integration tests, the docker
  backend, and the example deployments under `examples/`)
- Atlas Community CLI (migration linting; `atlas migrate lint` is run by
  CI and reproducible locally)
- Linux host for the process backend (`bubblewrap` + `R`); Docker backend
  works on macOS / Windows via Docker Desktop

How you provision these is up to you — there's no project-shipped
provisioning. Postgres tests are off by default; spin one up on demand
with `docker compose -f dev/compose.yml up -d` (see
[dev/compose.yml](dev/compose.yml)).

## Commit conventions

Commits use `<type>(<scope>): <subject>`. Types: `feat`, `fix`, `docs`,
`ci`, `build`, `refactor`, `test`, `chore`.

Scopes (pick one; use `misc` if none fits):

- `proxy` — `internal/api`, `internal/proxy`, `internal/server`,
  routing, WebSocket, handlers
- `ui` — `internal/ui`, admin templates, CSS, frontend behavior
- `auth` — `internal/auth`, `internal/authz`, `internal/session`, OIDC
- `cli` — `cmd/by`, `cmd/by-builder`
- `db` — `internal/db`, migrations, storage drivers
- `docker` — `internal/backend/docker`, the Docker backend
- `process` — `internal/backend/process`, `internal/orchestrator`,
  spawn, cgroup/sandbox
- `telemetry` — `internal/telemetry`, observability
- `deps` — dependency bumps (dependabot-owned)
- `misc` — cross-cutting; no single area fits

Subject: under 70 characters, imperative mood, no trailing period.
The `codecov.yml` component map mirrors this list.
