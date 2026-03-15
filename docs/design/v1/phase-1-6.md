# Phase 1-6: Audit Logging + Telemetry + /readyz

Operational completeness. This phase adds the observability and compliance
infrastructure needed to run blockyard in production: an audit trail for
security compliance, Prometheus metrics for monitoring dashboards and
alerting, OpenTelemetry tracing for request debugging, and a `/readyz`
endpoint for orchestration health checks.

This phase depends on phase 1-3 (OpenBao health check is one of the
`/readyz` checks). It is independent of phases 1-4 and 1-5 and can be
developed in parallel with them.

## Design decision: audit log as append-only JSON Lines file

The audit log is a local file, one JSON object per line (JSON Lines /
JSONL format). Not a database table, not a remote service.

**Why a local file?**

- **Simplicity:** no additional infrastructure (no Elasticsearch, no
  Loki, no database table with retention policies). The file can be
  rotated with standard tools (`logrotate`).
- **Append-only performance:** sequential writes to a file are fast.
  No write amplification from database indexing.
- **Tamper evidence:** the file is append-only from blockyard's
  perspective. An operator can set filesystem permissions to make it
  immutable (e.g., `chattr +a` on Linux). This is not enforced by
  blockyard — it's an operational choice.
- **Easy integration:** JSON Lines is a standard format. Operators can
  ship it to any log aggregator (Loki, Splunk, CloudWatch) using
  standard file-tailing agents (Promtail, Fluentd, Filebeat).

**Trade-off:** the audit log is local to the server. In a multi-server
deployment (v2), each server writes its own file. Log aggregation across
servers is the operator's responsibility.

## Design decision: buffered async writes

Audit entries are sent to a channel and written by a background
goroutine. The `Log()` method is non-blocking — if the buffer is full,
the entry is dropped and a warning is logged via slog.

**Why async?**

- Audit logging should never block request processing. A slow disk or
  a momentary I/O stall should not add latency to API responses.
- The channel buffer absorbs short bursts. At 1000 entries, the buffer
  handles ~10 seconds of sustained 100 req/s audit events.

**Trade-off:** entries can be dropped under extreme load. This is
acceptable — the slog warning is itself a signal that the audit system
is overwhelmed, and the operator can investigate. The alternative
(blocking writes) risks cascading latency.

## Design decision: Prometheus metrics over custom metrics

Metrics are exposed via a Prometheus-compatible `/metrics` endpoint
using the `prometheus/client_golang` library. Not a custom metrics
format, not StatsD, not a push model.

**Why Prometheus?**

- **Industry standard for Go services.** Prometheus client_golang is
  the most widely used Go metrics library. Every monitoring stack
  (Grafana, Datadog, New Relic) can scrape Prometheus endpoints.
- **Pull model:** no need to configure a metrics destination at startup.
  The server exposes `/metrics`; the scraper finds it. This decouples
  blockyard from the monitoring infrastructure.
- **Low overhead:** metrics are updated in-process (atomic counter
  increments, histogram observations). No network I/O on the hot path.

**(Updated in v1 wrap-up §3):** on the main listener, `/metrics`
requires `APIAuth` (to prevent untrusted Shiny app containers from
scraping operational data). When `management_bind` is configured,
`/metrics` is served without auth on the management listener — which
is unreachable from container bridge networks. This is the recommended
production setup.

## Design decision: OpenTelemetry tracing is opt-in

Tracing is disabled by default and only enabled when
`telemetry.otlp_endpoint` is configured. When enabled, spans are
exported to an OTel collector via gRPC (OTLP protocol).

**Why opt-in?**

- Tracing has non-trivial overhead (span creation, context propagation,
  export). For single-server deployments, structured logs and metrics
  are usually sufficient.
- OTel requires a collector endpoint. Shipping spans nowhere wastes
  resources.

**What is traced:**

- Proxy requests (full lifecycle: auth → session resolution → cold start
  → forward → response)
- API requests (endpoint handler duration)
- OpenBao calls (JWT login, secret write)
- Backend operations (spawn, stop, health check, build)

Each span carries standard attributes: `http.method`, `http.route`,
`http.status_code`, `blockyard.app_id`, `blockyard.worker_id`.

## Design decision: /readyz checks all runtime dependencies

`/readyz` is a dependency health check endpoint that reports whether the
server can serve traffic. Unlike `/healthz` (which always returns 200 if
the process is alive), `/readyz` checks:

1. **Database** — `SELECT 1` succeeds
2. **Docker** — `backend.ListManaged()` succeeds (verifies socket)
3. **IdP** — OIDC discovery endpoint is reachable (when `[oidc]`
   configured)
4. **OpenBao** — health endpoint returns 200 or 429 (when `[openbao]`
   configured)

Returns 200 with `{"status": "ready"}` when all checks pass, or 503
with `{"status": "not_ready", "checks": {...}}` listing individual
results.

**Use case:** Kubernetes/Docker health probes. A load balancer can use
`/readyz` to determine if a server should receive traffic. `/healthz`
is the liveness probe (is the process alive?); `/readyz` is the
readiness probe (can it serve requests?).

**Timeout:** each check has a 5-second timeout. The total `/readyz`
response time is bounded by the slowest check (checks run sequentially
to avoid thundering herd on shared dependencies).

## Deliverables

1. `[audit]` config section — log file path
2. `[telemetry]` config section — metrics enabled flag, OTLP endpoint
3. `AuditLog` — buffered async writer for audit events
4. Audit middleware — capture state-changing operations
5. Prometheus metrics — gauges, counters, histograms for key metrics
6. `/metrics` endpoint
7. OpenTelemetry tracing — span creation and export
8. `/readyz` endpoint with dependency health checks
9. New dependencies: `prometheus/client_golang`,
   `go.opentelemetry.io/otel`

## Step-by-step

### Step 1: Config additions

**New structs** in `internal/config/config.go`:

```go
type AuditConfig struct {
    Path string `toml:"path"` // e.g. /data/audit/blockyard.jsonl
}

type TelemetryConfig struct {
    MetricsEnabled bool   `toml:"metrics_enabled"` // default: false
    OTLPEndpoint   string `toml:"otlp_endpoint"`   // e.g. http://otel-collector:4317
}
```

**Changes to `Config` struct:**

```go
type Config struct {
    // ... existing fields ...
    Audit     *AuditConfig     `toml:"audit"`     // nil when not configured
    Telemetry *TelemetryConfig `toml:"telemetry"` // nil when not configured
}
```

**Validation:**

```go
if cfg.Audit != nil {
    if cfg.Audit.Path == "" {
        return fmt.Errorf("config: audit.path must not be empty")
    }
}
```

No validation for telemetry — both fields are optional (metrics default
to disabled, OTLP endpoint default to empty/disabled).

**Auto-construction from env vars:**

```go
if cfg.Audit == nil && envPrefixExists("BLOCKYARD_AUDIT_") {
    cfg.Audit = &AuditConfig{}
}
if cfg.Telemetry == nil && envPrefixExists("BLOCKYARD_TELEMETRY_") {
    cfg.Telemetry = &TelemetryConfig{}
}
```

**Env var mappings:**

```
BLOCKYARD_AUDIT_PATH
BLOCKYARD_TELEMETRY_METRICS_ENABLED
BLOCKYARD_TELEMETRY_OTLP_ENDPOINT
```

**Tests:**

- Parse config with `[audit]` and `[telemetry]` sections
- Parse config without them (backward compat)
- Validation: reject empty audit path
- Env var overrides
- Auto-construction from env vars
- `TestEnvVarCoverageComplete` passes

### Step 2: Audit log writer

New file: `internal/audit/audit.go`

```go
package audit

import (
    "context"
    "encoding/json"
    "log/slog"
    "os"
    "time"
)

// Action identifies the type of audit event.
type Action string

const (
    ActionAppCreate          Action = "app.create"
    ActionAppUpdate          Action = "app.update"
    ActionAppDelete          Action = "app.delete"
    ActionAppStart           Action = "app.start"
    ActionAppStop            Action = "app.stop"
    ActionBundleUpload       Action = "bundle.upload"
    ActionBundleRestoreOK    Action = "bundle.restore.success"
    ActionBundleRestoreFail  Action = "bundle.restore.fail"
    ActionAccessGrant        Action = "access.grant"
    ActionAccessRevoke       Action = "access.revoke"
    ActionCredentialEnroll   Action = "credential.enroll"
    ActionUserLogin          Action = "user.login"
    ActionUserLogout         Action = "user.logout"
    ActionUserUpdate         Action = "user.update"
    ActionPATCreate          Action = "pat.create"
    ActionPATRevoke          Action = "pat.revoke"
)

// Entry is a single audit log record.
type Entry struct {
    Timestamp string         `json:"ts"`
    Action    Action         `json:"action"`
    Actor     string         `json:"actor"`
    Target    string         `json:"target,omitempty"`
    Detail    map[string]any `json:"detail,omitempty"`
    SourceIP  string         `json:"source_ip,omitempty"`
}

// Log is an append-only audit log backed by a JSON Lines file.
// Writes are buffered via a channel and flushed by a background goroutine.
type Log struct {
    entries chan Entry
}

const bufferSize = 1000

// New creates an audit log. The background writer must be started with
// Run(). If path is empty, returns a no-op logger.
func New(path string) *Log {
    if path == "" {
        return nil
    }
    return &Log{
        entries: make(chan Entry, bufferSize),
    }
}

// Emit sends an entry to the background writer. Non-blocking — if the
// buffer is full, the entry is dropped and a warning is logged.
func (l *Log) Emit(entry Entry) {
    if l == nil {
        return
    }
    entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)

    select {
    case l.entries <- entry:
    default:
        slog.Warn("audit log buffer full, dropping entry",
            "action", entry.Action, "actor", entry.Actor)
    }
}

// Run is the background goroutine that appends entries to the log file.
// Blocks until ctx is cancelled. Drains remaining entries before exit.
func (l *Log) Run(ctx context.Context, path string) {
    if l == nil {
        <-ctx.Done()
        return
    }

    f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
    if err != nil {
        slog.Error("failed to open audit log", "path", path, "error", err)
        return
    }
    defer f.Close()

    enc := json.NewEncoder(f)

    for {
        select {
        case <-ctx.Done():
            // Drain remaining entries
            for {
                select {
                case entry := <-l.entries:
                    enc.Encode(entry)
                default:
                    return
                }
            }
        case entry := <-l.entries:
            enc.Encode(entry)
        }
    }
}
```

**Server struct addition:**

```go
type Server struct {
    // ... existing fields ...
    AuditLog *audit.Log // nil when [audit] not configured
}
```

**Initialization in `cmd/blockyard/main.go`:**

```go
var auditLog *audit.Log
if cfg.Audit != nil {
    auditLog = audit.New(cfg.Audit.Path)
    go auditLog.Run(bgCtx, cfg.Audit.Path)
}
srv.AuditLog = auditLog
```

**Tests:**

- `Emit` writes entry to file
- `Emit` on nil logger — no panic
- Buffer full — entry dropped, slog warning emitted
- `Run` drains remaining entries on context cancellation
- JSON Lines format: each line is valid JSON
- Timestamp is populated automatically

### Step 3: Audit emission points

Add `AuditLog.Emit()` calls to all state-changing API handlers. Each
call is a single line added after the successful operation.

**Helper for extracting audit context from a request:**

```go
func auditEntry(r *http.Request, action audit.Action, target string, detail map[string]any) audit.Entry {
    actor := "anonymous"
    if caller := auth.CallerFromContext(r.Context()); caller != nil {
        actor = caller.Sub
    }
    return audit.Entry{
        Action:   action,
        Actor:    actor,
        Target:   target,
        Detail:   detail,
        SourceIP: r.RemoteAddr,
    }
}
```

**Emission points:**

| Handler | Action | Target | Detail |
|---|---|---|---|
| `CreateApp` | `app.create` | app ID | `{"name": "..."}` |
| `UpdateApp` | `app.update` | app ID | changed fields |
| `DeleteApp` | `app.delete` | app ID | `{"name": "..."}` |
| `StartApp` | `app.start` | app ID | — |
| `StopApp` | `app.stop` | app ID | `{"worker_count": N}` |
| `UploadBundle` | `bundle.upload` | app ID | `{"bundle_id": "..."}` |
| Restore success | `bundle.restore.success` | app ID | `{"bundle_id": "..."}` |
| Restore failure | `bundle.restore.fail` | app ID | `{"bundle_id": "..."}` |
| `GrantAppAccess` | `access.grant` | app ID | `{"principal": "...", "role": "..."}` |
| `RevokeAppAccess` | `access.revoke` | app ID | `{"principal": "..."}` |
| `enrollCredential` | `credential.enroll` | service name | — |
| Login callback | `user.login` | — | `{"sub": "..."}` |
| Logout | `user.logout` | — | `{"sub": "..."}` |
| `PATCH /users/{sub}` | `user.update` | user sub | `{"role": "...", "active": ...}` |
| `POST /users/me/tokens` | `pat.create` | token ID | `{"name": "..."}` |
| `DELETE /users/me/tokens/{id}` | `pat.revoke` | token ID | — |

**Example** (in `CreateApp`):

```go
if srv.AuditLog != nil {
    srv.AuditLog.Emit(auditEntry(r, audit.ActionAppCreate, app.ID,
        map[string]any{"name": app.Name}))
}
```

**Tests:**

- Integration test: create app → verify audit log contains `app.create`
  entry with correct actor and target

### Step 4: Prometheus metrics

New file: `internal/telemetry/metrics.go`

```go
package telemetry

import (
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
)

// Gauges — current state
var (
    WorkersActive = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "blockyard_workers_active",
        Help: "Currently running workers",
    })
    SessionsActive = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "blockyard_sessions_active",
        Help: "Active proxy sessions",
    })
)

// Counters — cumulative totals
var (
    WorkersSpawned = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_workers_spawned_total",
        Help: "Total workers spawned",
    })
    WorkersStopped = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_workers_stopped_total",
        Help: "Total workers stopped",
    })
    BundlesUploaded = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_bundles_uploaded_total",
        Help: "Total bundles uploaded",
    })
    BundleRestoresSucceeded = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_bundle_restores_succeeded_total",
        Help: "Total successful bundle restores",
    })
    BundleRestoresFailed = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_bundle_restores_failed_total",
        Help: "Total failed bundle restores",
    })
    ProxyRequests = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_proxy_requests_total",
        Help: "Total proxied requests",
    })
    HealthChecksFailed = promauto.NewCounter(prometheus.CounterOpts{
        Name: "blockyard_health_checks_failed_total",
        Help: "Failed health checks leading to eviction",
    })
)

// Histograms — distributions
var (
    ColdStartDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "blockyard_cold_start_seconds",
        Help:    "Worker cold-start duration (spawn to healthy)",
        Buckets: prometheus.ExponentialBuckets(0.5, 2, 8), // 0.5s to 64s
    })
    ProxyRequestDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "blockyard_proxy_request_seconds",
        Help:    "Proxy request duration (excluding cold start)",
        Buckets: prometheus.DefBuckets,
    })
    BuildDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Name:    "blockyard_build_seconds",
        Help:    "Bundle restore (build) duration",
        Buckets: prometheus.ExponentialBuckets(5, 2, 8), // 5s to 640s
    })
)
```

**Instrumentation points:**

- `WorkersActive.Inc()` / `Dec()` — in `Workers.Set()` / `EvictWorker()`
- `SessionsActive.Inc()` / `Dec()` — in `Sessions.Set()` / `Sessions.Delete()`
- `WorkersSpawned.Inc()` — in `ensureWorker` after successful spawn
- `WorkersStopped.Inc()` — in `EvictWorker`
- `BundlesUploaded.Inc()` — in `UploadBundle` handler
- `ProxyRequests.Inc()` — at the top of `proxy.Handler`
- `ColdStartDuration.Observe()` — wrap `pollHealthy` with timing
- `ProxyRequestDuration.Observe()` — wrap `forwardHTTP` with timing
- `BuildDuration.Observe()` — wrap `backend.Build` with timing
- `HealthChecksFailed.Inc()` — in health poller on eviction

**Conditional registration:** metrics are always registered (via
`promauto`), but the `/metrics` endpoint is only mounted when
`telemetry.metrics_enabled` is true. This avoids conditional
instrumentation calls (the counters/histograms are always updated; they
just aren't exposed unless metrics are enabled). The overhead of updating
unexposed metrics is negligible.

**Tests:**

- Counter increments are correct after operations
- Histogram observations are recorded
- Gauge reflects current worker/session count

### Step 5: /metrics endpoint

Add to the router:

```go
if srv.Config.Telemetry != nil && srv.Config.Telemetry.MetricsEnabled {
    r.Handle("/metrics", promhttp.Handler())
}
```

The endpoint is unauthenticated, alongside `/healthz` and `/readyz`.

**New dependency:**

```
github.com/prometheus/client_golang v1.20.0
```

**Tests:**

- `GET /metrics` returns 200 with Prometheus text format
- `GET /metrics` when not enabled — 404
- Metrics include expected metric names

### Step 6: OpenTelemetry tracing

New file: `internal/telemetry/tracing.go`

```go
package telemetry

import (
    "context"
    "log/slog"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// InitTracing sets up the OpenTelemetry trace provider. Returns a
// shutdown function that flushes pending spans. If endpoint is empty,
// tracing is not initialized (no-op provider is used).
func InitTracing(ctx context.Context, endpoint string) (func(context.Context) error, error) {
    if endpoint == "" {
        return func(context.Context) error { return nil }, nil
    }

    exporter, err := otlptracegrpc.New(ctx,
        otlptracegrpc.WithEndpoint(endpoint),
        otlptracegrpc.WithInsecure(), // TLS to collector is an operator concern
    )
    if err != nil {
        return nil, err
    }

    res := resource.NewWithAttributes(
        semconv.SchemaURL,
        semconv.ServiceNameKey.String("blockyard"),
    )

    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter),
        sdktrace.WithResource(res),
    )
    otel.SetTracerProvider(tp)

    slog.Info("otel tracing initialized", "endpoint", endpoint)
    return tp.Shutdown, nil
}
```

**Span creation** — a middleware that wraps the chi router:

```go
func TracingMiddleware() func(http.Handler) http.Handler {
    tracer := otel.Tracer("blockyard")

    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            ctx, span := tracer.Start(r.Context(), r.Method+" "+r.URL.Path)
            defer span.End()

            // Wrap response writer to capture status code
            sw := &statusWriter{ResponseWriter: w, status: 200}
            next.ServeHTTP(sw, r.WithContext(ctx))

            span.SetAttributes(
                attribute.String("http.method", r.Method),
                attribute.String("http.route", chi.RouteContext(r.Context()).RoutePattern()),
                attribute.Int("http.status_code", sw.status),
            )
        })
    }
}

type statusWriter struct {
    http.ResponseWriter
    status int
}

func (w *statusWriter) WriteHeader(code int) {
    w.status = code
    w.ResponseWriter.WriteHeader(code)
}
```

**Initialization in `cmd/blockyard/main.go`:**

```go
var tracingShutdown func(context.Context) error
if cfg.Telemetry != nil && cfg.Telemetry.OTLPEndpoint != "" {
    shutdown, err := telemetry.InitTracing(ctx, cfg.Telemetry.OTLPEndpoint)
    if err != nil {
        slog.Error("failed to init tracing", "error", err)
        os.Exit(1)
    }
    tracingShutdown = shutdown
}
// In shutdown path:
if tracingShutdown != nil {
    tracingShutdown(ctx)
}
```

**Router addition** (only when tracing is enabled):

```go
if cfg.Telemetry != nil && cfg.Telemetry.OTLPEndpoint != "" {
    r.Use(telemetry.TracingMiddleware())
}
```

**New dependency:**

```
go.opentelemetry.io/otel v1.28.0
go.opentelemetry.io/otel/sdk v1.28.0
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.28.0
```

**Tests:**

- `InitTracing` with empty endpoint — returns no-op shutdown
- `InitTracing` with endpoint — returns non-nil shutdown
- Tracing middleware sets span attributes
- Integration test: verify spans are exported (use in-memory exporter)

### Step 7: /readyz endpoint

New file: `internal/api/readyz.go`

```go
package api

import (
    "context"
    "encoding/json"
    "net/http"
    "time"

    "github.com/cynkra/blockyard/internal/server"
)

const readyzCheckTimeout = 5 * time.Second

func readyzHandler(srv *server.Server) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        checks := make(map[string]string)

        // Database
        func() {
            ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
            defer cancel()
            if err := srv.DB.Ping(ctx); err != nil {
                checks["database"] = "fail"
            } else {
                checks["database"] = "pass"
            }
        }()

        // Docker socket
        func() {
            ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
            defer cancel()
            if _, err := srv.Backend.ListManaged(ctx); err != nil {
                checks["docker"] = "fail"
            } else {
                checks["docker"] = "pass"
            }
        }()

        // IdP (OIDC discovery endpoint)
        if srv.Config.OIDC != nil {
            func() {
                ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
                defer cancel()
                if err := checkIDP(ctx, srv); err != nil {
                    checks["idp"] = "fail"
                } else {
                    checks["idp"] = "pass"
                }
            }()
        }

        // OpenBao
        if srv.OpenBaoClient != nil {
            func() {
                ctx, cancel := context.WithTimeout(r.Context(), readyzCheckTimeout)
                defer cancel()
                if err := srv.OpenBaoClient.Health(ctx); err != nil {
                    checks["openbao"] = "fail"
                } else {
                    checks["openbao"] = "pass"
                }
            }()
        }

        // Vault token (wrap-up §4A): check token renewal status.
        // A stale or expired token degrades readiness.
        if srv.VaultTokenStatus != nil {
            if srv.VaultTokenStatus.IsHealthy() {
                checks["vault_token"] = "pass"
            } else {
                checks["vault_token"] = "fail"
            }
        }

        allOK := true
        for _, v := range checks {
            if v == "fail" {
                allOK = false
                break
            }
        }

        status := "ready"
        httpStatus := http.StatusOK
        if !allOK {
            status = "not_ready"
            httpStatus = http.StatusServiceUnavailable
        }

        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(httpStatus)
        json.NewEncoder(w).Encode(map[string]any{
            "status": status,
            "checks": checks,
        })
    }
}

// checkIDP verifies the IdP's discovery endpoint is reachable.
func checkIDP(ctx context.Context, srv *server.Server) error {
    // Re-fetch the discovery document to verify the IdP is alive.
    // This is lightweight — the IdP caches it and returns quickly.
    req, err := http.NewRequestWithContext(ctx, http.MethodGet,
        srv.Config.OIDC.IssuerURL+"/.well-known/openid-configuration", nil)
    if err != nil {
        return err
    }
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("idp returned %d", resp.StatusCode)
    }
    return nil
}
```

**DB.Ping** — new method in `internal/db/db.go`:

```go
func (db *DB) Ping(ctx context.Context) error {
    _, err := db.ExecContext(ctx, "SELECT 1")
    return err
}
```

**Router addition:**

```go
r.Get("/healthz", healthz)
r.Get("/readyz", readyzHandler(srv))
```

`/readyz` is unauthenticated, alongside `/healthz`.

**Tests:**

- All checks pass — 200, `{"status": "ready"}`
- Database unreachable — 503, `database: "fail"`
- Docker socket unreachable — 503, `docker: "fail"`
- OIDC not configured — no `idp` check in response
- OpenBao not configured — no `openbao` check in response
- OpenBao configured but unreachable — 503, `openbao: "fail"`
- Individual check timeout — check returns "fail", doesn't hang
