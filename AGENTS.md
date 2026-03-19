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

After every `git push` and merge queue submission, monitor CI status
and fix failures without being asked. Do not wait for the user to
report a broken build.

How to monitor:
- **PR checks**: `gh pr checks <number>`
- **Merge queue runs**: Poll the actual workflow runs, NOT `gh pr view`
  (which only shows OPEN/pending regardless of queue failures):
  ```
  gh api repos/cynkra/blockyard/actions/runs?event=merge_group&per_page=4 \
    --jq '.workflow_runs[] | "\(.id) \(.conclusion // "running") \(.name)"'
  ```
- **Failed logs**: `gh run view <id> --log-failed`
- **Merge confirmation**: `gh pr view <n> --json state,mergedAt`

## Development environment

This project uses a devcontainer (`.devcontainer/`). Do not install tools
directly in the running container — they will be lost on rebuild. Instead,
add any needed tools to `.devcontainer/Dockerfile` and rebuild.

The Go module cache is mounted as a Docker volume (see `devcontainer.json`).
Go, gopls, and delve are pre-installed in the image. The Go version is
defined in `go.mod` (single source of truth).
