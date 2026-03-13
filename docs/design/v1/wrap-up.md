# v1 Wrap-Up

One item remains before v1 can ship: the API authentication model is
incomplete and the hello-auth example is broken.

The v1 security review found only minor issues (timing-vulnerable token
comparison, cookie Secure flag inconsistency). Both were fixed in-tree.
Remaining accepted findings (rate limiting, body size limits, error
detail leakage) are deferred to infrastructure or production hardening.

---

## 1. API Authentication Strategy

### Problem

v1 added OIDC for browser-based authentication but left the
control-plane API (`/api/v1/*`) in a broken state when both OIDC and a
static token are configured — exactly the setup the hello-auth example
uses.

Three concrete issues:

**A. `APIAuth` middleware is either/or.** When OIDC is configured,
`APIAuth` (`internal/api/auth.go:37`) takes the JWT-only path. The
static token fallback is in an `else` branch that is unreachable. A
`Bearer my-secret-token` request fails JWT parsing and returns 401. The
`authenticateFromBearer` helper in `internal/api/users.go:106` already
implements the correct fallthrough (try JWT, then static token), but
`APIAuth` does not use it.

**B. No path for human API access.** A user who authenticates via OIDC
in the browser has a session cookie, but no way to obtain a token for
CLI or script use. There is no Personal Access Token (PAT) mechanism,
no device authorization flow, and no way to export a bearer token from
the web UI.

**C. No path for machine API access once the static token is removed.**
The static token was a bootstrap measure. With OIDC in place, CI/CD
pipelines need a proper machine-to-machine credential. Without one,
every deployment that uses OIDC must also configure a static shared
secret with no expiry, no per-client identity, and no revocation.

### Callers and their needs

| Caller | Example | Needs |
|---|---|---|
| Admin script | `deploy.sh` | Pre-shared credential, no browser |
| CI/CD pipeline | GitHub Actions | Machine identity, revocable, auditable |
| Human via CLI | Interactive `curl` / future CLI tool | User identity, scoped permissions |
| Human via browser | Web UI | Already works (OIDC session cookie) |

### Bundle ownership

Who uploads a bundle determines who owns it. This matters for access
control and audit. Three caller types produce different ownership
semantics:

- **Human upload** — bundle owned by the authenticated user. Natural.
- **CI upload with user token** — bundle owned by the user whose token
  CI uses. Fragile: token tied to an individual who may leave.
- **CI upload with machine credential** — bundle owned by... what?

The cleanest model: **ownership lives on the app, not the bundle.** An
app has an owner (set at creation, transferable). Bundles record the
uploader identity as metadata for audit, but the app owner is what
governs access control. A CI job uploading as `ci-deploy` produces a
bundle with `uploaded_by: ci-deploy` on an app owned by `team-x`. This
decouples access control from deployment automation.

### Design

The fix has three parts: an immediate bug fix, a short-term bridge, and
a medium-term solution.

#### Part 1: Fix `APIAuth` fallthrough (immediate)

Make `APIAuth` try JWT first, then fall through to static token — the
same pattern `authenticateFromBearer` in `users.go` already uses. This
unblocks the hello-auth example and any deployment combining OIDC with a
static admin token.

```go
// Try JWT when OIDC is configured.
if srv.Config.OIDC != nil && srv.JWKSCache != nil {
    claims, err := srv.JWKSCache.Validate(token, ...)
    if err == nil {
        // JWT valid — use JWT identity.
        identity = jwtIdentity(claims, srv)
        goto authenticated
    }
    slog.Debug("JWT validation failed, trying static token", "error", err)
}

// Static token fallback (works with or without OIDC).
if srv.Config.Server.Token.Expose() != "" {
    if subtle.ConstantTimeCompare(...) == 1 {
        identity = staticAdminIdentity()
        goto authenticated
    }
}

writeError(w, 401, "unauthorized", "invalid token")
return

authenticated:
    ctx := auth.ContextWithCaller(r.Context(), identity)
    next.ServeHTTP(w, r.WithContext(ctx))
```

Alternatively, refactor `APIAuth` to call `authenticateFromBearer`
directly, eliminating the duplicated logic.

#### Part 2: OAuth2 Client Credentials (short-term, machine auth)

Add support for machine-to-machine authentication via the standard
OAuth2 Client Credentials flow (RFC 6749 §4.4). This requires no new
infrastructure in blockyard — the IdP issues tokens, blockyard validates
them with its existing JWKS path.

**How it works:**

1. Admin registers a service client in the IdP (e.g.,
   `blockyard-ci` with a client secret).
2. CI/CD calls the IdP's token endpoint:
   ```
   POST /token
   grant_type=client_credentials
   client_id=blockyard-ci&client_secret=...
   ```
3. IdP returns a short-lived JWT.
4. CI/CD sends it as `Authorization: Bearer <jwt>` to blockyard.
5. Blockyard validates it via the existing JWKS cache.

**What changes in blockyard:**

- **Audience validation.** Currently `JWKSCache.Validate` checks
  `aud == oidc.client_id` ("blockyard"). A client-credentials token may
  carry a different audience. Either: accept multiple audiences via a
  config list, or require the IdP to issue tokens with `aud: blockyard`
  (Keycloak and Authentik support this; Dex requires the client to
  request it).

- **Role assignment for service accounts.** Rich IdPs (Keycloak,
  Authentik, Auth0) can embed roles/groups in client-credentials tokens
  via service account roles or property mappings. For these, the
  existing group→role mapping works unchanged. Minimal IdPs (Dex) do
  not. For Dex, we need a config-level mapping from service-account
  `sub` to role — a small addition to the existing `RoleMappingCache`.

**IdP compatibility:**

| IdP | Client Credentials | Roles in token | Notes |
|---|---|---|---|
| Dex | Yes | No | Bare JWT; needs blockyard-side role mapping |
| Keycloak | Yes | Yes | Service account roles in claims |
| Authentik | Yes | Yes | Property mappings on provider |
| Auth0 / Okta | Yes | Yes | Custom claims via rules/actions |
| Entra ID | Yes | Yes | App roles in token |

#### Part 3: Personal Access Tokens (medium-term, human API auth)

For human CLI/script access, add a PAT mechanism. This is
application-level — blockyard issues and manages tokens, not the IdP.
This is the pattern used by GitHub, GitLab, and Posit Connect.

**User flow:**

1. User logs into the web UI via OIDC.
2. User navigates to settings → "Access Tokens".
3. User creates a PAT with a name, optional expiry, and optional scope.
4. Blockyard generates a token, stores a hash (not the plaintext), and
   shows the token once.
5. User copies the token into their script / `.env` / CI config.

**Validation flow:**

1. `APIAuth` receives `Bearer <pat>`.
2. JWT parsing fails (PATs are opaque, not JWTs).
3. Static token check fails (different format).
4. New step: hash the token, look up in the PAT store.
5. If found and not expired/revoked → authenticate as the owning user.

**Storage:** OpenBao's token auth method (`/v1/auth/token/create`,
`token/lookup`, `token/revoke`) provides issuance, validation,
revocation, and TTL out of the box. However, it has significant
drawbacks as a PAT backend: every API request requires a network
round-trip for token/lookup (vs. a local hash comparison), OpenBao
downtime would break all PAT-based API auth (currently OpenBao is
only needed for the optional credential/secrets flow), there is no
list-by-user API (we'd need a SQLite index anyway to power the UI),
and it creates confusion with the existing vault tokens used for
secret-reading in the proxy layer.

SQLite is the better fit. Store a `(hash, sub, name, created_at,
expires_at, last_used_at, revoked)` row, look up by hash on the auth
hot path. No external dependency, microsecond validation, and the
listing/revocation UI queries are trivial. OpenBao stays focused on
what it's good at: secret storage and JWT→token exchange for app
credentials.

**Scope:** Initially, PATs can be unscoped (full permissions of the
owning user). Scoped PATs (read-only, single-app, etc.) are a future
refinement.

**Revocation:** Deleting the hash from the store. A "revoke all" button
covers the credential compromise case.

### Implementation Plan

1. **Fix `APIAuth` fallthrough** — Refactor to use
   `authenticateFromBearer` or replicate its try-JWT-then-static
   pattern. Add test coverage for the combined OIDC + static token
   case. Fix the hello-auth example to work end-to-end.

2. **Client Credentials support** — Extend audience validation to
   accept a configurable list. Add service-account-to-role mapping in
   config. Update hello-auth example with a `blockyard-ci` Dex client.
   Document IdP-specific setup for Keycloak and Authentik.

3. **PAT infrastructure** — Schema migration for PAT table (hash,
   user sub, name, created, expires, revoked). API endpoints:
   `POST /api/v1/users/me/tokens` (create),
   `GET /api/v1/users/me/tokens` (list),
   `DELETE /api/v1/users/me/tokens/{id}` (revoke).
   PAT validation step in `APIAuth`. Web UI: token management page
   under user settings (generate with name/expiry, list active tokens
   with created/last-used dates, revoke individual or all). The
   generate flow must show the plaintext token exactly once and
   require the user to copy it before dismissing.

4. **Bundle ownership** — Add `uploaded_by` field to bundles table.
   Ensure app-level ownership is the access control boundary.
   CI uploads record the service account identity.
