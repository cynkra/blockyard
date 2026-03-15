---
title: Observability
description: Logging, metrics, tracing, and audit logging in Blockyard.
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

## Prometheus metrics

Enable the `/metrics` endpoint in your configuration:

```toml
[telemetry]
metrics_enabled = true
```

The endpoint requires authentication (bearer token or session cookie).

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

Send distributed traces to an OpenTelemetry collector:

```toml
[telemetry]
otlp_endpoint = "http://otel-collector:4317"
```

The service name is `blockyard`. Spans include `http.method`, `http.route`,
and `http.status_code` attributes. Endpoints using `http://`, `localhost`,
or `127.0.0.1` connect without TLS; all others use TLS.

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
| `app.start` | Worker started via API |
| `app.stop` | Workers stopped via API |
| `bundle.upload` | Bundle uploaded |
| `bundle.restore.success` | Dependency restore completed |
| `bundle.restore.fail` | Dependency restore failed |
| `access.grant` | Per-app access granted to a user |
| `access.revoke` | Per-app access revoked |
| `credential.enroll` | User enrolled a credential in OpenBao |
| `user.login` | User logged in via OIDC |
| `user.logout` | User logged out |
| `user.update` | User role or active status changed |
| `token.create` | Personal Access Token created |
| `token.revoke` | Single PAT revoked |
| `token.revoke_all` | All PATs revoked for a user |

### Buffering

Audit entries are buffered in memory (up to 1000 entries) and flushed to
disk by a background writer. If the buffer is full, new entries are
dropped and the `blockyard_audit_entries_dropped_total` metric is
incremented. Under normal load, entries are written within milliseconds.
