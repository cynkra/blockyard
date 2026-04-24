# Agent Guidelines

## Communication style

When presenting open questions or concerns, first give an overview, then
always walk through them one-by-one. Never dump a list and ask for bulk
feedback.

Be concise and critical. Skip compliments and pleasantries — focus on
problems, trade-offs, and things that might be wrong.

## Development environment

This project uses a devcontainer (`.devcontainer/`). Do not install tools
directly in the running container — they will be lost on rebuild. Instead,
add any needed tools to `.devcontainer/Dockerfile` and rebuild.

The Go module cache is mounted as a Docker volume (see `devcontainer.json`).
Go, gopls, and delve are pre-installed in the image. The Go version is
defined in `go.mod` (single source of truth).

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
