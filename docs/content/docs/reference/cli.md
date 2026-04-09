---
title: CLI Reference
description: Complete reference for the by command-line interface.
weight: 1
---

`by` is the command-line client for Blockyard. It handles authentication,
deployment, app management, and administration from your terminal.

## Global flags

| Flag     | Description                                |
| -------- | ------------------------------------------ |
| `--json` | Output machine-readable JSON (all commands) |

When `--json` is set, all output (including errors) is printed as
pretty-printed JSON. Errors use the shape `{"error": "...", "message": "..."}`.

## Authentication

### `by login`

Store credentials interactively. Opens a browser to create a
[Personal Access Token](/docs/guides/authorization/#personal-access-tokens), then
verifies it against the server.

```bash
by login
by login --server https://blockyard.example.com
```

| Flag               | Description                             |
| ------------------ | --------------------------------------- |
| `--server <url>`   | Server URL (prompts interactively if omitted) |

Credentials are saved to `~/.config/by/config.json` (respects
`$XDG_CONFIG_HOME`) with file mode `0600`.

**Environment variable override.** Instead of `by login`, you can set both
`BLOCKYARD_URL` and `BLOCKYARD_TOKEN`. When both are present they take
precedence over the config file.

---

## Deploying

### `by deploy <path>`

Deploy a Shiny app to Blockyard. This is the primary workflow command — it
detects dependencies, creates a bundle archive, uploads it, and optionally
waits for the build to finish.

```bash
by deploy .
by deploy ./my-app --name dashboard --pin --wait
by deploy ./my-app --yes --wait
```

| Flag                    | Description                                         |
| ----------------------- | --------------------------------------------------- |
| `--name <string>`       | Override app name (default: directory basename)      |
| `--pin`                 | Pin dependencies via `renv::snapshot()` (requires R + renv) |
| `-y, --yes`             | Skip the confirmation prompt                        |
| `--wait`                | Wait for the build to complete and stream logs      |
| `--repositories <csv>`  | R package repository URLs (comma-separated)         |

**Dependency detection** (in priority order):

1. `manifest.json` exists — used as-is
2. `renv.lock` exists — converted to manifest
3. `--pin` flag — runs `renv::snapshot()`, then converts
4. `DESCRIPTION` exists — builds an unpinned manifest
5. Bare R scripts — uploaded without manifest

The command auto-detects the app entrypoint (`app.R` or `server.R`), computes
SHA-256 checksums for all files, creates a gzip tar archive, and uploads it.
If the app does not exist on the server yet, it is created automatically.

**Example output:**

```
Detected:
  Name:        dashboard
  Mode:        DESCRIPTION (entrypoint: app.R)
  Deps:        3 packages
  Repository:  https://cran.r-project.org

Deploy? [Y/n] y

Uploading bundle... done.

  App:       dashboard
  Bundle:    b1a2b3c4 (building)
  Task:      t9876...
  URL:       https://blockyard.example.com/app/dashboard/
```

### `by init <path>`

Generate a `manifest.json` without deploying. Useful for inspecting or
committing the resolved manifest before deploying.

```bash
by init .
by init ./my-app --pin --repositories https://cran.r-project.org
```

| Flag                    | Description                                         |
| ----------------------- | --------------------------------------------------- |
| `--pin`                 | Pin dependencies via `renv::snapshot()`             |
| `--repositories <csv>`  | R package repository URLs (comma-separated)         |

If `manifest.json` already exists, validates it and exits successfully
(no new file is written). Does not support bare-script apps — at least a
`DESCRIPTION` is required.

---

## App management

### `by list`

List all apps. Alias: `by ls`.

```bash
by list
by list --deleted
by ls --json
```

| Flag        | Description                         |
| ----------- | ----------------------------------- |
| `--deleted` | Include soft-deleted apps (admin only) |

**Example output:**

```
NAME        TITLE                OWNER     STATUS    ENABLED
dashboard   Sales Dashboard      alice     running   yes
demo        Demo App             bob       stopped   yes
```

### `by get <app>`

Show details for a single app.

```bash
by get dashboard
by get dashboard --runtime
```

| Flag        | Description                                    |
| ----------- | ---------------------------------------------- |
| `--runtime` | Include live runtime data (workers, sessions, metrics) |

Without `--runtime`, shows static metadata (ID, owner, status, enabled state,
access mode, title, description, active bundle, resource limits, tags,
creation date). With `--runtime`, appends worker table and session/view
statistics.

### `by enable <app>`

Enable an app, allowing traffic to reach it.

```bash
by enable dashboard
```

### `by disable <app>`

Disable an app. Blocks new traffic and drains active sessions.

```bash
by disable dashboard
```

### `by delete <app>`

Soft-delete an app. Alias: `by rm`.

```bash
by delete demo
by rm demo --purge
```

| Flag      | Description                                             |
| --------- | ------------------------------------------------------- |
| `--purge` | Permanently delete (admin only; app must be soft-deleted first) |

### `by restore <app>`

Restore a soft-deleted app.

```bash
by restore demo
```

### `by update <app>`

Update app metadata. At least one flag is required.

```bash
by update dashboard --title "Sales Dashboard v2"
by update dashboard --description "Regional sales metrics"
```

| Flag                    | Description      |
| ----------------------- | ---------------- |
| `--title <string>`      | Display title    |
| `--description <string>` | Description text |

---

## Bundles

### `by bundles <app>`

List all bundles for an app.

```bash
by bundles dashboard
```

**Example output:**

```
ID              STATUS    UPLOADED              DEPLOYED BY   PINNED
b1a2b3c4b1a2    success   2026-03-28T08:00:00Z  alice         yes
d5e6f7a8d5e6    success   2026-03-27T14:30:00Z  bob           no
```

### `by rollback <app> <bundle-id>`

Roll back to a previous bundle.

```bash
by rollback dashboard d5e6f7a8d5e6
```

---

## Scaling

### `by scale <app>`

Configure resource limits and autoscaling. At least one flag is required.

```bash
by scale dashboard --memory 1g --cpu 2.0
by scale dashboard --max-workers 4 --pre-warm 1
```

| Flag                   | Description                              |
| ---------------------- | ---------------------------------------- |
| `--memory <string>`    | Memory limit (e.g., `512m`, `2g`)        |
| `--cpu <float>`        | CPU limit (e.g., `1.0`, `2.5`)          |
| `--max-workers <int>`  | Maximum workers per app                  |
| `--max-sessions <int>` | Maximum sessions per worker              |
| `--pre-warm <int>`     | Pre-warmed standby workers               |

Only the flags you provide are updated; omitted fields are left unchanged.

---

## Access control

### `by access show <app>`

Show the access type and ACL entries for an app.

```bash
by access show dashboard
```

**Example output:**

```
Access type: acl

PRINCIPAL              KIND    ROLE           GRANTED BY
alice@example.com      user    collaborator   admin
bob@example.com        user    viewer         admin
```

### `by access set-type <app> <type>`

Set the access mode. Valid types:

| Type        | Description                          |
| ----------- | ------------------------------------ |
| `acl`       | Per-user access control list         |
| `logged_in` | Any authenticated user               |
| `public`    | No authentication required           |

```bash
by access set-type dashboard acl
by access set-type demo public
```

### `by access grant <app> <user>`

Grant a user access to an app.

```bash
by access grant dashboard bob@example.com
by access grant dashboard bob@example.com --role collaborator
```

| Flag             | Description                                  |
| ---------------- | -------------------------------------------- |
| `--role <string>` | Role to grant: `viewer` (default) or `collaborator` |

### `by access revoke <app> <user>`

Revoke a user's access.

```bash
by access revoke dashboard bob@example.com
```

---

## Tags

### `by tags list`

List all tags in the global pool.

```bash
by tags list
```

### `by tags create <tag>`

Create a tag (admin only).

```bash
by tags create production
```

### `by tags delete <tag>`

Delete a tag (admin only). Cascades — the tag is also removed from all apps.

```bash
by tags delete staging
```

### `by tags app-list <app>`

List tags attached to an app. Hidden from `by tags --help` but functional.

```bash
by tags app-list dashboard
```

### `by tags app-add <app> <tag>`

Attach a tag to an app. Hidden from `by tags --help` but functional.

```bash
by tags app-add dashboard production
```

### `by tags app-remove <app> <tag>`

Detach a tag from an app. Hidden from `by tags --help` but functional.

```bash
by tags app-remove dashboard staging
```

---

## Dependencies

### `by refresh <app>`

Refresh unpinned dependencies. Triggers a background task that re-resolves
packages from configured repositories and streams the task logs.

```bash
by refresh dashboard
by refresh dashboard --rollback
```

| Flag         | Description                                     |
| ------------ | ----------------------------------------------- |
| `--rollback` | Roll back to the previous dependency set instead |

This is useful for apps deployed from a `DESCRIPTION` (unpinned) — it pulls
the latest compatible package versions without requiring a new bundle upload.

---

## Logs

### `by logs <app>`

View or stream app logs.

```bash
by logs dashboard
by logs dashboard --follow
by logs dashboard --worker w-abc123 --follow
```

| Flag                  | Description                                    |
| --------------------- | ---------------------------------------------- |
| `-f, --follow`        | Stream logs live (default: static snapshot)     |
| `-w, --worker <id>`   | Worker ID (auto-selects most recent if omitted) |

When multiple workers exist and `--worker` is not specified, the most recently
started active worker is selected. If no active workers exist, the most
recently ended worker is used.

---

## Server administration

The `by admin` subcommand group manages the blockyard server itself.
Most commands require the `admin` system role.

### `by admin update`

Trigger a rolling update of the server to the latest release on the
configured channel. On the Docker backend this clones the blockyard
container next to the old one; on the process backend it forks a new
blockyard process on an alternate bind port. The command streams the
orchestrator task log until the update completes or fails.

```bash
by admin update
by admin update --yes --channel stable
```

| Flag             | Description                                               |
| ---------------- | --------------------------------------------------------- |
| `--channel <ch>` | Release channel: `stable` or `main` (default: server config) |
| `-y, --yes`      | Skip the confirmation prompt                              |
| `--json`         | Output as JSON                                            |

Prerequisites and failure modes differ per backend — see
[Process Backend rolling update walkthrough](/docs/guides/process-backend/#rolling-update-walkthrough)
and the [admin update API](/docs/reference/api/#post-apiv1adminupdate).

### `by admin rollback`

Roll the server back to the previous version. Supported on the Docker
backend; returns `501 Not Implemented` on the process backend (the
operator's install scheme owns the binary path).

```bash
by admin rollback
by admin rollback --yes
```

| Flag        | Description                  |
| ----------- | ---------------------------- |
| `-y, --yes` | Skip the confirmation prompt |
| `--json`    | Output as JSON               |

### `by admin status`

Show the current rolling-update state (`idle`, `updating`, `watching`,
etc.) and the target version if one is in progress.

```bash
by admin status
by admin status --json
```

| Flag     | Description     |
| -------- | --------------- |
| `--json` | Output as JSON  |

### `by admin install-seccomp`

Write the embedded outer-container seccomp profile to disk. Used when
deploying the process backend via the `blockyard-process` Docker image —
the operator needs a copy of the profile on the host before the
container starts, because Docker reads `--security-opt seccomp=<path>`
from the host filesystem. The profile is embedded in the `by` binary,
so no network access is required.

```bash
sudo by admin install-seccomp
sudo by admin install-seccomp --target /etc/blockyard/seccomp.json
```

| Flag              | Description                                             |
| ----------------- | ------------------------------------------------------- |
| `--target <path>` | Output path (default: `/etc/blockyard/seccomp.json`)   |

This command does not talk to a running blockyard server — it only
writes the profile to disk. No authentication required. See
[Process Backend (Containerized)](/docs/guides/process-backend-container/)
for the full extraction workflow.

---

## User administration

These commands require the `admin` system role.

### `by users list`

List all users.

```bash
by users list
```

**Example output:**

```
SUB                          NAME       EMAIL                ROLE       ACTIVE
google-oauth2|abc123         Alice      alice@example.com    admin      yes
google-oauth2|def456         Bob        bob@example.com      publisher  yes
```

### `by users update <sub>`

Update a user's role or active status. At least one flag is required.

```bash
by users update "google-oauth2|def456" --role admin
by users update "google-oauth2|def456" --active=false
```

| Flag              | Description                                     |
| ----------------- | ----------------------------------------------- |
| `--role <string>` | Set role: `admin`, `publisher`, or `viewer`     |
| `--active <bool>` | Enable or disable the user account (default: `true`) |

---

## Configuration

The CLI stores credentials in `~/.config/by/config.json` (or
`$XDG_CONFIG_HOME/by/config.json`):

```json
{
  "server": "https://blockyard.example.com",
  "token": "by_..."
}
```

**Credential resolution order:**

1. `BLOCKYARD_URL` + `BLOCKYARD_TOKEN` environment variables (both required)
2. Config file
3. Error if neither is available
