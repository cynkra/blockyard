---
title: Authorization
description: How blockyard manages user roles, app access, and permissions.
---

Blockyard separates authentication from authorization. Your identity
provider (IdP) handles authentication — proving who you are via OIDC.
Blockyard handles authorization — deciding what you can do. IdP groups
play no role in blockyard's permission model.

## System Roles

Every user has one system-wide role, assigned by a blockyard admin.
New users get **viewer** by default when they first log in via OIDC.

| Role | What you can do |
|---|---|
| **admin** | Full control: manage all apps, all users, all settings. Implicit owner access to everything. |
| **publisher** | Create and deploy apps. Full control over your own apps. No access to other publishers' apps unless explicitly granted. |
| **viewer** | Access apps you've been granted access to, or apps with `logged_in`/`public` visibility. Cannot create or deploy apps. |

### First Admin Setup

The first admin is configured via the `initial_admin` field in the
`[oidc]` config section. Set this to the OIDC `sub` of the user who
should become the first admin:

```toml
[oidc]
initial_admin = "google-oauth2|abc123"
```

When this user logs in for the first time, they receive the `admin`
role instead of `viewer`. Once an admin exists, they can promote other
users via the API or admin UI. The `initial_admin` field is only checked
on first login — changing it later has no effect on existing users.

### Managing Users

Admins manage users via the REST API:

```
GET    /api/v1/users              — List all users
GET    /api/v1/users/{sub}        — Get a user
PATCH  /api/v1/users/{sub}        — Update role or active status
```

To promote a user to publisher:

```bash
curl -X PATCH https://blockyard.example.com/api/v1/users/user-sub-123 \
  -H "Authorization: Bearer by_..." \
  -H "Content-Type: application/json" \
  -d '{"role": "publisher"}'
```

To deactivate a user (immediately revokes all access, including PATs):

```bash
curl -X PATCH https://blockyard.example.com/api/v1/users/user-sub-123 \
  -H "Authorization: Bearer by_..." \
  -H "Content-Type: application/json" \
  -d '{"active": false}'
```

An admin cannot demote or deactivate themselves (prevents lockout).

## App Visibility

Each app has an `access_type` that controls who can reach it. Set via
the app update API (`PATCH /api/v1/apps/{id}`).

| `access_type` | Who can access | Use case |
|---|---|---|
| `acl` | Only users with an explicit access grant. **Default.** | Sensitive internal tools, per-team apps. |
| `logged_in` | Any authenticated user. | Company-wide dashboards where everyone should have access without per-user grants. |
| `public` | Anyone, including users who are not logged in. | Public demos, open data dashboards. |

## Per-App Access Grants

For apps with `access_type = "acl"`, access is controlled via per-user
grants. Each grant associates a user with an app at a specific
permission level.

| Grant level | What it allows |
|---|---|
| **owner** | Full control: deploy bundles, change settings, delete the app, manage access grants. The user who created the app is automatically the owner. Admins can transfer ownership. |
| **collaborator** | Deploy new bundles and change app settings. Cannot delete the app or manage who has access. |
| **viewer** | Access and use the app. No management capabilities. |

System admins have implicit owner-level access to all apps regardless
of grants.

### Managing Access

App owners and admins manage access via the REST API:

```
POST   /api/v1/apps/{id}/access                    — Grant access
GET    /api/v1/apps/{id}/access                    — List grants
DELETE /api/v1/apps/{id}/access/{kind}/{principal}  — Revoke access
```

Grant a user viewer access to an app:

```bash
curl -X POST https://blockyard.example.com/api/v1/apps/app-id/access \
  -H "Authorization: Bearer by_..." \
  -H "Content-Type: application/json" \
  -d '{"principal": "user-sub-456", "kind": "user", "role": "viewer"}'
```

Revoke access:

```bash
curl -X DELETE https://blockyard.example.com/api/v1/apps/app-id/access/user/user-sub-456 \
  -H "Authorization: Bearer by_..."
```

## Access in Shiny Apps

The proxy injects two HTTP headers on every request forwarded to a
Shiny app. These headers tell the app who the user is and what they
can do.

| Header | Value |
|---|---|
| `X-Shiny-User` | The authenticated user's OIDC `sub`. Empty for anonymous access. |
| `X-Shiny-Access` | The user's effective access level for this specific app. |

### `X-Shiny-Access` Values

| Value | Meaning |
|---|---|
| `owner` | System admin or app owner. |
| `collaborator` | Has a collaborator grant on this app. |
| `viewer` | Has a viewer grant, or the app uses `logged_in`/`public` visibility and the user is authenticated. |
| `anonymous` | App is `public` and the user is not logged in. |

### Reading Access in R

```r
# In your Shiny server function:
server <- function(input, output, session) {
  access <- session$request$HTTP_X_SHINY_ACCESS
  user   <- session$request$HTTP_X_SHINY_USER

  # Show admin controls only to owners
  output$admin_panel <- renderUI({
    if (access == "owner") {
      # render admin controls
    }
  })

  # Restrict data access based on role
  data <- reactive({
    if (access %in% c("owner", "collaborator")) {
      full_dataset()
    } else {
      filtered_dataset()
    }
  })
}
```

## Personal Access Tokens

Personal Access Tokens (PATs) provide identity-aware API access for
CLI tools, CI/CD pipelines, and scripts. They replace the static bearer
token from earlier versions.

PATs carry the full permissions of the user who created them. When a
user's role changes or they are deactivated, the change takes effect
on the next API request using any of their PATs.

### Creating a PAT

PATs can only be created via an authenticated browser session (OIDC
login). You cannot use a PAT to create another PAT.

Via the web UI: navigate to your user settings and use the token
management page.

Via the API:

```bash
# Must use a session cookie (browser), not a PAT
curl -X POST https://blockyard.example.com/api/v1/users/me/tokens \
  -H "Cookie: session=..." \
  -H "Content-Type: application/json" \
  -d '{"name": "deploy-ci", "expires_in": "90d"}'
```

The response includes the plaintext token **exactly once**:

```json
{
  "id": "tok-abc123",
  "name": "deploy-ci",
  "token": "by_7kJx9mQ2vR4nL8pW1tY6bH3cF5gD0aE",
  "expires_at": "2026-06-12T10:00:00Z"
}
```

Save the token immediately — it cannot be retrieved again.

### Using a PAT

```bash
curl https://blockyard.example.com/api/v1/apps \
  -H "Authorization: Bearer by_7kJx9mQ2vR4nL8pW1tY6bH3cF5gD0aE"
```

### Revoking PATs

```bash
# Revoke a single token
curl -X DELETE https://blockyard.example.com/api/v1/users/me/tokens/tok-abc123 \
  -H "Authorization: Bearer by_..."

# Revoke all your tokens
curl -X DELETE https://blockyard.example.com/api/v1/users/me/tokens \
  -H "Authorization: Bearer by_..."
```

Deactivating a user (via `PATCH /api/v1/users/{sub}`) also effectively
revokes all their PATs, since PAT validation checks the user's active
status.

### Token Format

PAT tokens use the prefix `by_` followed by 32 random bytes
(base62-encoded). This prefix enables detection by secret scanning
tools (GitHub secret scanning, truffleHog, etc.).
