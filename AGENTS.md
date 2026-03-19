# Agent Guidelines

## Communication style

When presenting open questions or concerns, first give an overview, then
always walk through them one-by-one. Never dump a list and ask for bulk
feedback.

Be concise and critical. Skip compliments and pleasantries — focus on
problems, trade-offs, and things that might be wrong.

## Pull requests

Do not create pull requests unless explicitly asked. When PRs are created,
never include a "Generated with Claude Code" signature or similar attribution
in the PR description.

## CI monitoring

After every `git push`, monitor CI status (`gh pr checks`, `gh run view
--log-failed`) and fix failures without being asked. Do not wait for the
user to report a broken build.

## Development environment

This project uses a devcontainer (`.devcontainer/`). Do not install tools
directly in the running container — they will be lost on rebuild. Instead,
add any needed tools to `.devcontainer/Dockerfile` and rebuild.

The Go module cache is mounted as a Docker volume (see `devcontainer.json`).
Go 1.24, gopls, and delve are pre-installed in the image.
