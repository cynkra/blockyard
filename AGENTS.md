# Agent Guidelines

## Communication style

When presenting open questions or concerns, first give an overview, then
always walk through them one-by-one. Never dump a list and ask for bulk
feedback.

Be concise and critical. Skip compliments and pleasantries — focus on
problems, trade-offs, and things that might be wrong.

## Git workflow

- Pushes to `main` are blocked. All changes go through branches and PRs.
- After every `git push`, monitor CI and fix failures without being
  asked. After merge queue submission, monitor workflow runs (not PR
  state) and fix failures. See `/commit`, `/pr`, `/merge` skills for
  details.

## Development environment

This project uses a devcontainer (`.devcontainer/`). Do not install tools
directly in the running container — they will be lost on rebuild. Instead,
add any needed tools to `.devcontainer/Dockerfile` and rebuild.

The Go module cache is mounted as a Docker volume (see `devcontainer.json`).
Go, gopls, and delve are pre-installed in the image. The Go version is
defined in `go.mod` (single source of truth).
