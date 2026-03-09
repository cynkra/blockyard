# blockr.cloud v1 / MVP — Draft Notes

This document collects v1 items from the roadmap and design decisions deferred
from v0 planning. It is a staging area, not a plan — the v1 implementation
plan will be written after v0 is stable.

## Deferred from v0

These are v0 design decisions that explicitly punt to v1:

- **Signed session cookies.** v0 uses plain UUIDs. v1 switches to
  HMAC-signed cookies when OIDC is added — the cookie will carry identity,
  not just a routing token.

- **Graceful drain on app stop.** v0 kills workers immediately on
  `POST /apps/{id}/stop`. v1 adds graceful drain alongside session sharing.

- **Proxy concurrency model.** v0 uses a single shared `hyper::Client`.
  Revisit if v1 load balancing across multiple workers shows contention.

## Roadmap v1 features

From `../roadmap.md` items 17–30:

1. **Multi-worker and session sharing** — enforce `max_workers_per_app` and
   `max_sessions_per_worker` when `> 1`; load balancing and auto-scaling
   wired in

2. **OIDC authentication** — enterprise SSO via OpenID Connect; establishes
   user identity

3. **IdP client credentials** — replaces static bearer token; machine auth
   via OAuth 2.0 client credentials flow; same JWT validation path as human
   auth

4. **User sessions** — cookie-based; transparent access token refresh;
   signed cookie carries sub, groups, access + refresh tokens

5. **RBAC + per-content ACL** — roles (admin, developer, viewer) and
   per-app access control

6. **Identity injection** — `X-Shiny-User` and `X-Shiny-Groups` headers
   injected into each proxied request

7. **Integration system (OpenBao)** — IdP JWT → scoped OpenBao token at
   session start; token injected into R process as env var; R process reads
   secrets directly from OpenBao via `httr2`

8. **Audit logging** — append-only JSON Lines of all state-changing
   operations

9. **Vanity URLs** — per-content custom URL paths (`/sales-dashboard`
   instead of `/app/sales-dashboard/`)

10. **Content discovery** — catalog API, tag system, search/filter

11. **Load balancing** — cookie-hash sticky sessions for Shiny; active when
    `max_workers_per_app > 1`

12. **Auto-scaling** — connection-based, paired with load balancing

13. **Telemetry and observability** — Prometheus metrics endpoint,
    OpenTelemetry tracing via `tracing` + `metrics` crates

14. **`/readyz` endpoint** — readiness check against all runtime
    dependencies (DB, Docker socket, IdP, OpenBao); returns 503 with JSON
    body listing failed checks
