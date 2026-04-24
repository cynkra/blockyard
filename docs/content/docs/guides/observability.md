---
title: Observability
description: Logging, metrics, tracing, and audit logging in Blockyard.
weight: 5
---

Blockyard provides four observability mechanisms: structured logging,
Prometheus metrics, OpenTelemetry tracing, and an append-only audit log.

## Logging

Blockyard writes structured JSON logs to stderr. Control verbosity with
the `log_level` setting:

```toml
[server]
log_level = "info"   # trace, debug, info, warn, error
```

Or via environment variable:

```bash
BLOCKYARD_SERVER_LOG_LEVEL=debug
```

### Log levels

| Level | Use case |
|---|---|
| `trace` | Fine-grained diagnostics (WebSocket frames, load-balancer decisions). Noisy. |
| `debug` | Subsystem internals (health checks, session lifecycle, container operations) |
| `info` | Normal operations (startup, requests, deploys). **Default.** |
| `warn` | Degraded conditions (failed health checks, capacity limits) |
| `error` | Failures requiring attention (container crashes, build failures) |

### HTTP request logging

All HTTP requests are logged automatically:

- **Health probes** (`/healthz`, `/readyz`) are logged at `debug` level
  to avoid noise in production
- **Other requests** are logged at `info` (2xx/3xx), `warn` (4xx), or
  `error` (5xx) based on the response status code

Each log entry includes `method`, `path`, `status`, and `duration_ms`.

## Management listener

By default, operational endpoints (`/healthz`, `/readyz`, `/metrics`) are
served on the main listener alongside the application proxy and API. This
means containers running untrusted Shiny app code can reach these endpoints.

For production deployments, configure a separate management listener bound
to a loopback address:

```toml
[server]
bind            = "0.0.0.0:3838"
management_bind = "127.0.0.1:9100"
```

When `management_bind` is set:

- `/healthz`, `/readyz`, and `/metrics` move to the management listener
  and are removed from the main listener
- The management listener requires **no authentication** — access is
  controlled by the network binding (loopback = host-only)
- `/readyz` always returns full per-component check details
- Prometheus can scrape `/metrics` without a bearer token
- Container bridge networks cannot reach `127.0.0.1`, so untrusted
  workloads cannot access operational data

When AppRole auth is used (`vault.role_id`), `/readyz` also reports a
`vault_token` check that reflects whether the token renewal goroutine is
healthy. A stale or expired token degrades readiness, signaling the
operator to re-bootstrap with a fresh `secret_id`.

Point your health checks and Prometheus scraper at the management port:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: blockyard
    static_configs:
      - targets: ['localhost:9100']
```

On shutdown, the management listener stops first (health probes fail,
signaling load balancers to drain traffic), then the main listener is
shut down gracefully.

## Prometheus metrics

Enable the `/metrics` endpoint in your configuration:

```toml
[telemetry]
metrics_enabled = true
```

When served on the main listener (no `management_bind`), the endpoint
requires authentication (bearer token or session cookie). When served on
the management listener, no authentication is required.

### Available metrics

**Gauges:**

| Metric | Description |
|---|---|
| `blockyard_workers_active` | Number of currently running worker containers |
| `blockyard_sessions_active` | Number of active proxy sessions |

**Counters:**

| Metric | Description |
|---|---|
| `blockyard_workers_spawned_total` | Total workers spawned |
| `blockyard_workers_stopped_total` | Total workers stopped |
| `blockyard_bundles_uploaded_total` | Total bundles uploaded |
| `blockyard_bundle_restores_succeeded_total` | Successful dependency restores |
| `blockyard_bundle_restores_failed_total` | Failed dependency restores |
| `blockyard_proxy_requests_total` | Total requests through the reverse proxy |
| `blockyard_health_checks_failed_total` | Total failed worker health checks |
| `blockyard_audit_entries_dropped_total` | Audit log entries dropped due to buffer overflow |

**Histograms:**

| Metric | Description |
|---|---|
| `blockyard_cold_start_seconds` | Time from worker spawn to healthy (buckets: 0.5s–64s) |
| `blockyard_proxy_request_seconds` | Proxy request duration, excluding cold start |
| `blockyard_build_seconds` | Bundle dependency restore duration (buckets: 5s–640s) |

## OpenTelemetry tracing

This section covers tracing of **blockyard itself** — the HTTP handlers,
proxy, and API. Tracing for hosted Shiny apps is an app-level concern,
covered under [Tracing hosted Shiny apps](#tracing-hosted-shiny-apps).

Send distributed traces to an OpenTelemetry collector:

```toml
[telemetry]
otlp_endpoint = "otel-collector:4317"
```

The service name is `blockyard`. Spans include `http.method`, `http.route`,
and `http.status_code` attributes.

The exporter speaks **OTLP/gRPC only** (default collector port `4317`);
OTLP/HTTP (port `4318`) is not supported. Transport security is picked
automatically from the endpoint string: plaintext for `http://`,
`localhost`, or `127.0.0.1`, TLS for everything else. The endpoint
itself can point anywhere reachable from the blockyard process — for
example `host.docker.internal:4317` when blockyard runs in a container
and the collector runs on the host.

### Tracing hosted Shiny apps

Blockyard forwards W3C `traceparent` into each worker — on HTTP
requests and on the initial WebSocket upgrade. If a hosted Shiny app
is instrumented with [`{otel}` / `{otelsdk}`](https://shiny.posit.co/r/articles/improve/opentelemetry/),
its spans attach to the blockyard trace automatically, giving a single
end-to-end view from inbound request to reactive computation.

Point the worker at a collector via `server.worker_env`:

```toml
[server]
worker_env = { OTEL_EXPORTER_OTLP_ENDPOINT = "http://alloy:4317" }
```

Blockyard does not care what sits at that address — an OpenTelemetry
Collector, Grafana Alloy, or a vendor endpoint all work. Reachability
is your responsibility: for the Docker backend, the collector must be
reachable on the worker's network (see `docker.service_network`); for
the process backend, it must be reachable from `localhost`.

When `OTEL_EXPORTER_OTLP_ENDPOINT` is set in `worker_env`, blockyard
auto-populates identity attributes so signals from different apps and
workers are distinguishable in the backend:

| Variable | Value |
|---|---|
| `OTEL_SERVICE_NAME` | the app name |
| `OTEL_RESOURCE_ATTRIBUTES` | `blockyard.app=<name>,blockyard.worker_id=<id>`, merged with any user-supplied attributes |

User-set values in `worker_env` win, so you can override either for a
specific deployment.

## Security headers

All HTTP responses include the following security headers:

| Header | Value |
|---|---|
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `Strict-Transport-Security` | `max-age=63072000; includeSubDomains` (HTTPS only) |

API endpoints additionally set `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` and `Cache-Control: no-store`.

## Audit logging

Enable append-only audit logging to a JSONL file:

```toml
[audit]
path = "/data/audit/blockyard.jsonl"
```

Each line is a JSON object with the following fields:

| Field | Description |
|---|---|
| `ts` | Timestamp (RFC 3339 with nanoseconds) |
| `action` | Event type (see below) |
| `actor` | OIDC `sub` of the user who triggered the action |
| `target` | Resource ID (app ID, user sub, etc.), if applicable |
| `detail` | Additional context (map of key-value pairs), if applicable |
| `source_ip` | Caller's IP address, if applicable |

### Audit actions

| Action | Trigger |
|---|---|
| `app.create` | App created |
| `app.update` | App settings changed |
| `app.delete` | App deleted |
| `app.start` | Worker started for an app |
| `app.stop` | App workers stopped |
| `app.rollback` | App rolled back to a previous bundle |
| `app.restore` | Soft-deleted app restored |
| `app.rename` | App renamed (old name becomes an alias) |
| `bundle.upload` | Bundle uploaded |
| `bundle.restore.success` | Dependency restore completed |
| `bundle.restore.fail` | Dependency restore failed |
| `access.grant` | Per-app access granted to a user |
| `access.revoke` | Per-app access revoked |
| `credential.enroll` | User enrolled a credential in the vault |
| `user.login` | User logged in via OIDC |
| `user.logout` | User logged out |
| `user.update` | User role or active status changed |
| `token.create` | Personal Access Token created |
| `token.revoke` | Single PAT revoked |
| `token.revoke_all` | All PATs revoked for a user |

### Buffering

Audit entries are buffered in memory (up to 1000 entries) and flushed to
disk by a background writer. If the buffer is full, new entries wait up
to 500 ms for space before being dropped. Dropped entries increment the
`blockyard_audit_entries_dropped_total` metric. Under normal load,
entries are written within milliseconds.
