# v1 Security Review Wrap-Up

Security review of the v1 codebase. Findings are ordered by severity.
Each finding includes a **Disposition** indicating whether it was fixed,
deferred, or accepted.

---

## 1. Static Token Comparison Is Timing-Vulnerable

**Disposition: FIXED**

### Problem

The static API token is compared using Go's `==` operator, which is not
constant-time. An attacker with network access to the API can measure
response time differences to progressively guess the token byte-by-byte.

**Affected locations:**

- `internal/api/auth.go` — static token fallback path
- `internal/api/users.go` — `identifyFromBearerToken`

The HMAC cookie verification correctly uses `hmac.Equal()` (constant-time),
but the static token path did not.

### Fix

Both locations now use `crypto/subtle.ConstantTimeCompare`.

### Severity

**High** when OIDC is not configured (static token is the only
authentication mechanism). **Medium** when OIDC is configured.

---

## 2. Build Containers Lack Network Isolation

**Disposition: ACCEPTED**

### Problem

Build containers run on the default Docker bridge network without
metadata endpoint blocking. Worker containers get per-worker bridge
networks and iptables rules blocking `169.254.169.254`.

### Why accepted

No user-supplied code runs during builds. The build container executes
only the server-controlled `rv sync` binary, which reads `rproject.toml`
and `rv.lock` to install R packages. Even if a malicious repo URL
pointed at the metadata endpoint, `rv` would fail to parse the response
as package data and discard it — there is no exfiltration channel back
to the attacker. Additionally, only authenticated users can upload
bundles.

The build container already has strong hardening: read-only root
filesystem, all capabilities dropped, and `no-new-privileges`.

### Severity

**High** in cloud environments per original assessment, but practical
exploitability is very low given the constraints above.

---

## 3. Internal Error Details Leaked to Clients

**Disposition: DEFERRED (development phase)**

### Problem

Several API handlers pass raw Go error messages to `serverError()`,
which returns them verbatim in the JSON response body. SQLite error
messages can reveal table names, column names, and query structure.

### Why deferred

Detailed error messages are useful during active development. This
should be revisited before production hardening — log full errors
server-side and return generic messages to clients.

### Severity

**Medium** — information disclosure that aids further attacks.

---

## 4. No Rate Limiting on Authentication Endpoints

**Disposition: ACCEPTED (reverse proxy responsibility)**

### Problem

No rate limiting exists on any endpoint, including static token
authentication, OIDC callback, and credential exchange.

### Why accepted

Rate limiting is expected to be handled by the reverse proxy (nginx,
Caddy, Traefik) that most deployments already use for TLS termination.
The timing vulnerability (#1) that made brute-force practical has been
fixed.

### Severity

**Medium** — exploitability depends on network exposure.

---

## 5. `X-Forwarded-Proto` Trusted Unconditionally

**Disposition: FIXED**

### Problem

The proxy session cookie set its `Secure` flag based on the
client-supplied `X-Forwarded-Proto` header. Auth cookies already derived
this from `external_url`, but the proxy session cookie did not, creating
an inconsistency.

### Fix

The proxy session cookie now derives `Secure` solely from `external_url`,
matching the auth cookie behavior. The `X-Forwarded-Proto` header is no
longer consulted for this purpose.

### Severity

**Medium** — enabled cookie downgrade in certain deployment topologies.

---

## 6. `/metrics` and `/readyz` Expose Information Without Authentication

**Disposition: ACCEPTED (by design)**

### Problem

`/metrics` and `/readyz` are outside auth middleware, exposing
operational details.

### Why accepted

Unauthenticated health and metrics endpoints are standard practice.
Kubernetes probes and Prometheus scrapers expect unauthenticated access.
Access control is handled at the infrastructure layer (reverse proxy
rules, network policies, or binding to an internal interface).

### Severity

**Low** — information disclosure, no direct exploitation path.

---

## 7. No Request Body Size Limits on API Endpoints

**Disposition: ACCEPTED (reverse proxy responsibility)**

### Problem

Only the bundle upload endpoint uses `http.MaxBytesReader`. Other JSON
endpoints read the full request body without limits.

### Why accepted

Body size limits are typically enforced by the reverse proxy
(`client_max_body_size`). All API endpoints require authentication,
limiting the attack surface to authenticated users.

### Severity

**Low** — denial of service via memory exhaustion.

---

## 8. Logout Endpoint Lacks CSRF Protection

**Disposition: ACCEPTED**

### Problem

`POST /logout` clears the session without verifying a CSRF token.

### Why accepted

`SameSite=Lax` blocks cross-site POSTs in modern browsers. The impact
is limited to forced logout — no data loss or privilege escalation.

### Severity

**Low** — forced logout with no security impact.
