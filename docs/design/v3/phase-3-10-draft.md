# Phase 3-10: Zygote Hardening & KSM (draft)

Two companion tracks layered on top of phase 3-9's zygote mechanism:

1. **Post-fork sandboxing** — per-child isolation (mount namespace,
   seccomp, capability dropping, rlimits) so the zygote model is
   safe for multi-tenant apps.
2. **Opt-in KSM** — kernel same-page merging via
   `prctl(PR_SET_MEMORY_MERGE)` so the zygote model can recover
   copy-on-write memory sharing that R's generational GC breaks.

They land in the same phase because:

- KSM's RSS-spike failure mode during coordinated GC recovery needs
  the sandbox-level containment that this phase introduces.
- KSM's documented side-channel history and the sandboxing phase's
  multi-tenant audit story share the same operator-facing question
  ("is this app safe to enable?"). Landing them together means the
  answer is a single decision, not two.
- KSM's seccomp profile impact (must allow `PR_SET_MEMORY_MERGE =
  67`) is tested alongside the rest of the seccomp profile changes.

> **Draft status.** The KSM track is fully designed in this
> document — the content was carved out of `phase-3-9.md` when that
> phase got too large to land in one piece. The sandboxing track is
> currently a deliverables sketch from `plan.md`; its detailed
> design is pending. Expand the sandboxing section before committing
> to phase 3-10 as ready-to-land.

## Prerequisites from earlier phases

- **Phase 3-8** — process backend packaging finalises
  `docker/blockyard-seccomp.json`. Phase 3-10 extends this profile
  to whitelist `PR_SET_MEMORY_MERGE` and adds the seccomp-bpf
  filter the per-child post-fork hook applies.
- **Phase 3-9** — the zygote mechanism. The `Forking` interface,
  `internal/zygotectl/` and `internal/zygote/` packages,
  `zygote.R`, the cold-start integration, and the `zygote` /
  `experimental.zygote` opt-in all exist by the time phase 3-10
  lands. Phase 3-10 modifies some of these files rather than
  creating parallel copies — see the step list.

## Deliverables

### Post-fork sandboxing

1. **Per-child isolation hook.** The child branch of `zygote.R`'s
   `FORK` handler (introduced in phase 3-9) applies post-fork
   isolation before calling `runApp`:
   - `unshare(CLONE_NEWUSER | CLONE_NEWNS)` — private user and
     mount namespaces
   - Private tmpfs at `/tmp`
   - seccomp-bpf filter loaded from the shared profile
   - `capset()` — drop all capabilities
   - `setrlimit()` — `RLIMIT_AS`, `RLIMIT_CPU`, `RLIMIT_NPROC`
2. **Docker security options.** Container create call for zygote
   workers adds
   `--security-opt seccomp={BundleServerPath}/.zygote/blockyard-seccomp.json`
   (same profile as the process backend, phase 3-8) and
   `--security-opt apparmor=unconfined` (Ubuntu 23.10+ only) when
   `experimental.zygote = true`.
3. **Environment variable hardening** — `OMP_NUM_THREADS=1` and
   `MKL_NUM_THREADS=1` in the zygote process before forking, so
   every child inherits single-threaded BLAS / OpenMP defaults
   unless it explicitly bumps them.
4. **Package compatibility documentation** — categorise the
   bundle's R packages by fork-safety:
   - Safe to pre-load (shiny, ggplot2, dplyr, DT, …)
   - Dangerous to pre-load, must load in each child (arrow, torch,
     rJava, anything with open fds or threads at load time)
   - Safe if not used before fork (DBI, RPostgres, any pool-based
     DB client)
5. **Sandboxing tests** — verify private `/tmp` isolation between
   children, seccomp profile is active, `CLONE_NEWUSER` works
   inside the container, `setrlimit` is enforced, dropped
   capabilities are actually dropped.

### Opt-in KSM

6. **`ksm` column on the `apps` table** — follows the same
   expand-only migration shape as the `zygote` column from phase
   3-9. Validated to require `zygote = true` on the same effective
   end-state and `experimental.ksm = true` in server config (see
   decision K4).
7. **`experimental.ksm` server-wide flag** — second field on the
   `ExperimentalConfig` struct introduced in phase 3-9. Config-load
   validation rejects `experimental.ksm = true` without
   `experimental.zygote = true`. Runtime short-circuits to
   non-KSM behaviour whenever the flag is off — kill switch.
8. **`internal/zygotectl/zygote_helper.c`** — tiny C helper (~15
   lines, no R headers) compiled per-architecture to a shared
   library. Loaded by `zygote.R` via `dyn.load` during startup to
   call `prctl(PR_SET_MEMORY_MERGE, 1)`, enabling kernel-level KSM
   page deduplication across forked children. The call is gated
   on the `BLOCKYARD_KSM_ENABLED` env var — when absent or `0`,
   the helper is not loaded and `ksm_status` is reported as
   `disabled`. When enabled: graceful fallback on older kernels
   (EINVAL) and seccomp-restricted environments (EPERM); failures
   surface via the `INFO` control command for ops visibility. See
   decision K1.
9. **`STATS` control command** — new command on the zygote
   control protocol returning dynamic per-zygote KSM merge counts
   read from `/proc/self/ksm_stat` and
   `/proc/<childpid>/ksm_stat`. Distinct from `INFO`, which stays
   cached startup state. See decision K2.
10. **`zygote.Manager` metrics-poll loop** — new goroutine that
    calls `Stats` on each live zygote's control client on a
    `metrics_interval` tick (default 30s) and updates labeled
    Prometheus gauges. Wired up via a new `StatsClient(workerID)
    *zygotectl.Client` method on the `Forking` interface.
11. **Prometheus metrics** — `blockyard_zygote_ksm_merging_pages{app_id,
    worker_id}` and `blockyard_zygote_ksm_merging_pages_total{app_id,
    worker_id}` plus an unlabeled `blockyard_host_ksm_pages_sharing`
    populated by a server-level scraper reading
    `/sys/kernel/mm/ksm/pages_sharing`. See decision K2.
12. **KSM preflight checks** — each backend's `Preflight()` impl
    (introduced in phase 3-7) gains two checks, gated on both
    `experimental.ksm = true` in server config AND at least one
    app having `ksm = true`: read `/sys/kernel/mm/ksm/run` and
    `/sys/kernel/mm/ksm/pages_to_scan`. If `run=0`, warn that KSM
    is not running on this host. If `run=1` but `pages_to_scan`
    is at the kernel default (100), warn that the scan rate is
    too low for server-class workloads and recommend
    `echo 2000 > /sys/kernel/mm/ksm/pages_to_scan`. Both warnings
    are non-fatal. Operators who have not opted into KSM see no
    preflight noise.
13. **Up-front bundle byte-compilation in `zygote.R`** — replace
    the trivial `source(app.R)` preload with
    `compiler::cmpfile(app.R, tempfile())` + `compiler::loadcmp()`
    so bundle closures are born as `BCODESXP` in the zygote.
    Without this, the R JIT mutates closure SEXP headers in shared
    COW pages post-fork, which defeats KSM for user code. Also
    saves 100–500ms per child start by eliminating the re-source
    cost in children. See decision K3.
14. **Child `oom_score_adj=1000` pinning** — each forked child
    writes `1000` to `/proc/self/oom_score_adj` immediately after
    fork so the kernel's OOM killer prefers children over the
    zygote under memory pressure. Contains the RSS spike that
    coordinated GC bursts can produce before ksmd catches up. See
    decision K5.
15. **API / CLI / UI surface for `ksm`** — `ksm` field on
    `updateAppRequest`, `--ksm` flag on `by scale`, settings tab
    toggle in the UI (admin-only, gated on the server capabilities
    endpoint). The existing server capabilities endpoint from
    phase 3-9 gains an `experimental.ksm` field.
16. **KSM tests** — control protocol unit tests for `STATS`
    (single-line and multi-line parsing, interleaved `CHILDEXIT`
    pushes), manager metrics-loop unit tests, KSM helper graceful
    fallback test on a mocked `prctl` failure path, KSM preflight
    tests, seccomp-allows-`PR_SET_MEMORY_MERGE` integration test,
    KSM-effectiveness integration test (fork two children, force
    `gc(full=TRUE)` in each, poll `STATS` until
    `ksm_merging_pages_total > 0`, skip on `/sys/kernel/mm/ksm/run
    == 0`).

---

## What KSM actually optimises

Forking a zygote delivers near-zero marginal memory at first —
children share every page with the parent via COW. But R's
generational GC writes mark bits to SEXP headers during level-2
collections, dirtying every page containing a live SEXP and
breaking COW. Without a recovery mechanism, the memory advantage
decays to "each child holds its own private copy of the package
memory" after a few GC cycles.

KSM via `prctl(PR_SET_MEMORY_MERGE, 1)` (Linux 6.4+) opts the
zygote into the kernel's same-page merging pool. When enabled,
ksmd scans for pages that are still bit-identical across children
after GC and re-merges them. How much sharing this actually
recovers is workload-dependent and currently unmeasured for R:
pages untouched by per-child mutation (REFCNT bumps, attribute
writes) merge cleanly, while pages where any child has mutated a
shared object do not. Up-front bundle compilation (decision K3)
eliminates the JIT as a source of per-child mutation; that's a
prerequisite for KSM doing useful work on user code. Meta reports
~10% of host RAM saved on Instagram (Python + CPython controller),
but Python's object model is structurally different from R's, so
that figure is a directional anchor at best.

**Five gates must all be open for the memory benefit to materialise:**

1. **`experimental.zygote = true`** in the server config (phase
   3-9). Default off.
2. **`apps.zygote = true`** on the specific app (phase 3-9).
   Default off.
3. **`experimental.ksm = true`** in the server config. Default
   off; independent of `experimental.zygote`.
4. **`apps.ksm = true`** on the specific app. Default off; API
   rejects turning it on without `apps.zygote = true`.
5. **Host-side KSM is available and tuned.** Three sub-conditions
   that all apply at the host level:
   - **Kernel ≥ 6.4** (the version that added
     `PR_SET_MEMORY_MERGE`). Hosts with it: Ubuntu 24.04 LTS
     (6.8), Debian 13 trixie when it ships, Fedora 39+, Amazon
     Linux 2023, most rolling distros. Hosts *without* it: Ubuntu
     22.04 LTS (5.15), Debian 12 bookworm (6.1), RHEL 9 /
     AlmaLinux 9 / Rocky 9 (5.14), k8s nodes on older base
     images. If you're deploying to an enterprise distro released
     before mid-2024, assume you don't have it until you check.
   - **`/sys/kernel/mm/ksm/run == 1`**. ksmd is off by default on
     most distros. Operators must enable it.
   - **`pages_to_scan` tuned above the desktop default of 100.**
     Stock defaults scan at ~20 MB/sec, which is far too slow
     for multi-GB R bundles. See decision K1 for tuning guidance.

Gates 1–4 are the opt-in (application-level); gate 5 is the host
capability (infrastructure-level). When any fail, the zygote
model from phase 3-9 still ships and still delivers its
unconditional benefits (startup latency and isolation) unchanged
— only the memory-sharing story degrades to the PSOCK-equivalent
steady state. The preflight checks (deliverable #12) warn on the
host sub-conditions when the opt-in gates are open, so operators
can see the problem without reading this document. Operators who
have not opted into KSM see no preflight noise.

---

## Step-by-step (KSM track)

> The sandboxing track needs its own detailed step list before
> this phase is ready to land. The steps below cover the KSM
> track only, carved from the original phase-3-9.md draft.

### Step K1: Migration — add `ksm` column

Phase 3-9's migration `003_zygote` adds the `zygote` column.
Phase 3-10's migration `004_ksm` adds the `ksm` column. Both
additive, nullable-equivalent (default 0), backward-compatible
per phase 3-1 rules. Separate migrations because the features
ship in separate phases.

**`internal/db/migrations/sqlite/004_ksm.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN ksm INTEGER NOT NULL DEFAULT 0;
```

**`internal/db/migrations/sqlite/004_ksm.down.sql`:**

SQLite `DROP COLUMN` pattern from migration 002 — recreate the
table without the column. See phase-3-9.md Step 1 for the
template.

**`internal/db/migrations/postgres/004_ksm.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN ksm BOOLEAN NOT NULL DEFAULT FALSE;
```

**`internal/db/migrations/postgres/004_ksm.down.sql`:**

```sql
ALTER TABLE apps DROP COLUMN ksm;
```

### Step K2: DB layer — `AppRow.KSM`, `AppUpdate.KSM`

Add to `AppRow` in `internal/db/db.go`:

```go
type AppRow struct {
    // ...existing fields including Zygote from phase 3-9...
    KSM bool `db:"ksm" json:"ksm"`
}
```

Add to `AppUpdate`:

```go
type AppUpdate struct {
    // ...existing fields including Zygote from phase 3-9...
    KSM *bool
}
```

Update `UpdateApp()` to handle the new field, add `ksm = ?` to
the UPDATE SQL, and default to `false` in `CreateApp`.

### Step K3: `experimental.ksm` flag on `ExperimentalConfig`

Phase 3-9 added the `ExperimentalConfig` struct with a single
`Zygote` bool. Phase 3-10 extends it:

```go
type ExperimentalConfig struct {
    Zygote bool `toml:"zygote"`
    KSM    bool `toml:"ksm"` // enable KSM memory merging
}
```

Config-load validation in `loadAndValidate()`:

```go
if c.Experimental != nil {
    if c.Experimental.KSM && !c.Experimental.Zygote {
        return fmt.Errorf("experimental.ksm requires experimental.zygote = true")
    }
}
```

### Step K4: `internal/zygotectl/zygote_helper.c` — the native helper

The native helper called by `zygote.R` to enable KSM. Deliberately
tiny and dependency-free: no R headers, no Rcpp, no stdlib beyond
what `prctl` needs. Compiles to a shared library with a standard
C compiler, embedded per-arch in the blockyard binary.

```c
#include <sys/prctl.h>
#include <errno.h>

/*
 * blockyard zygote native helper.
 *
 * Called from zygote.R via `dyn.load` + `.C("enable_ksm", integer(1))`.
 * The .C interface uses plain pointer arguments — no R headers needed
 * on the C side — so this file builds with a bare C compiler and has
 * no link-time dependency on libR.
 *
 * The function enables `PR_SET_MEMORY_MERGE` on the current process's
 * mm_struct. The flag is inherited by all `parallel::mcfork` children
 * via `ksm_fork` in the kernel, so setting it once on the zygote
 * covers every child process forked afterward.
 *
 * Linux 6.4+ defines PR_SET_MEMORY_MERGE as 67. On older kernels the
 * value is unused and the prctl returns EINVAL, which we surface as
 * the result so the R side can log it distinctly from other failures.
 */

#ifndef PR_SET_MEMORY_MERGE
#define PR_SET_MEMORY_MERGE 67
#endif

void enable_ksm(int *result) {
    if (prctl(PR_SET_MEMORY_MERGE, 1, 0, 0, 0) == 0) {
        *result = 0;
    } else {
        *result = errno;
    }
}
```

Build: `cc -shared -fPIC -o zygote_helper.so zygote_helper.c`. Done
at blockyard build time via a Makefile rule that produces one `.so`
per supported architecture (amd64, arm64, etc.); the Go build embeds
the architecture-appropriate binary via a build-tag-guarded
`//go:embed` in `embed_linux_amd64.go` / `embed_linux_arm64.go`.
Cross-compilation uses the standard `CC=aarch64-linux-gnu-gcc`
pattern; no Go-level cgo is required.

Phase 3-9's `internal/zygotectl/embed.go` ships `ZygoteScript`.
Phase 3-10 extends it with the helper source (for debugging) and
adds per-arch files for the compiled `.so`:

```go
// embed.go (extended)
package zygotectl

import _ "embed"

//go:embed zygote.R
var ZygoteScript []byte

//go:embed zygote_helper.c
var HelperSource []byte  // kept for debugging / reproducibility only
```

Plus per-architecture `.so` embeds (e.g. `embed_linux_amd64.go`):

```go
//go:build linux && amd64

package zygotectl

import _ "embed"

//go:embed zygote_helper_linux_amd64.so
var HelperSO []byte
```

`HelperSO` is what the backends actually write to disk at zygote
spawn and bind-mount into the worker.

### Step K5: `STATS` command on the control protocol

Phase 3-9 defined the control protocol with `AUTH`, `FORK`,
`KILL`, `STATUS`, `INFO`, and asynchronous `CHILDEXIT` pushes.
Phase 3-10 adds `STATS`:

```
client → server: STATS\n
server → client: <key>=<value>\n... END\n
                 # Dynamic, polled on a metrics tick. Known keys:
                 # ksm_merging_pages_zygote, ksm_merging_pages_children,
                 # ksm_merging_pages_total, child_count.
                 # Values read by zygote.R from /proc/<pid>/ksm_stat
                 # for itself and each tracked child PID. Parser
                 # ignores unknown keys (forward-compatible).
```

`INFO` also gains three fields carrying startup-time KSM state:
`ksm_status`, `ksm_errno`, and continues to carry `r_version` and
`preload_ms`.

`internal/zygotectl/control.go` gains:

```go
// Stats is the dynamic view of a zygote's current KSM merge state.
// The zygote reads its own /proc/self/ksm_stat plus
// /proc/<childpid>/ksm_stat for each tracked child and returns the
// aggregated totals.
type Stats struct {
    KSMMergingPagesZygote   int  // pages from the zygote process itself
    KSMMergingPagesChildren int  // sum across all live children
    KSMMergingPagesTotal    int  // = zygote + children
    ChildCount              int  // number of live children contributing
    KSMStatSupported        bool // false on kernel < 6.1 (no /proc/<pid>/ksm_stat)
}

// Stats sends the STATS command and returns the dynamic per-zygote
// KSM merge counts. Unlike Info(), which is cached startup state
// returned without a network round-trip, Stats() always hits the
// zygote over TCP. Called on the metrics-poll tick by
// zygote.Manager's metrics goroutine — see decision K2. Uses the
// same reqMu as FORK/KILL so metrics polling naturally defers to
// in-flight cold starts.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
    resp, err := c.requestMulti(ctx, "STATS\n")
    if err != nil {
        return Stats{}, err
    }
    return parseStats(resp), nil
}

// parseStats parses a multi-line STATS response into a Stats value.
// Unknown keys are ignored (forward-compatible). Missing keys
// leave the zero value in place.
func parseStats(resp string) Stats {
    stats := Stats{KSMStatSupported: true}
    for _, line := range strings.Split(resp, "\n") {
        key, val, ok := strings.Cut(line, "=")
        if !ok {
            continue
        }
        switch key {
        case "ksm_merging_pages_zygote":
            stats.KSMMergingPagesZygote, _ = strconv.Atoi(val)
        case "ksm_merging_pages_children":
            stats.KSMMergingPagesChildren, _ = strconv.Atoi(val)
        case "ksm_merging_pages_total":
            stats.KSMMergingPagesTotal, _ = strconv.Atoi(val)
        case "child_count":
            stats.ChildCount, _ = strconv.Atoi(val)
        case "ksm_stat_supported":
            stats.KSMStatSupported = val == "1"
        }
    }
    return stats
}
```

`Info` gains `KSMStatus` and `KSMErrno` fields (already described
in decision K1):

```go
type Info struct {
    RVersion  string
    KSMStatus string // "enabled", "disabled", "unsupported", "denied",
                    // "failed", "helper_missing", "dlopen_failed", "unknown"
    KSMErrno  int
    PreloadMS int
    Unknown   map[string]string
}
```

`fetchInfo` adds cases for `ksm_status` and `ksm_errno`.

Phase 3-9 shipped `request()` for single-line replies (`FORK`,
`KILL`). Phase 3-10 adds `requestMulti()` for `STATS`, which
accumulates lines in the read loop until it sees `END`. The
`readLoop` goroutine gains a `pendingMultiline` flag on the
client and a multi-line buffer; `CHILDEXIT` pushes arriving
mid-response are still dispatched to `Exits` without disrupting
the accumulator.

### Step K6: `zygote.R` changes for helper load, INFO, STATS, and oom_score_adj

Phase 3-9's `zygote.R` handles AUTH/FORK/KILL/STATUS/INFO with no
KSM awareness. Phase 3-10 extends it with:

1. **Helper load and `enable_ksm()` function** — called at startup,
   gated on `BLOCKYARD_KSM_ENABLED=1`, writes `ksm_status` into
   `zygote_info`.
2. **`STATS` handler** — reads `/proc/<pid>/ksm_stat` for the
   zygote and each tracked child, returns aggregated totals.
3. **`INFO` handler** — add `ksm_status` and `ksm_errno` fields
   alongside the existing `r_version` and `preload_ms`.
4. **Child `oom_score_adj=1000`** — post-fork, as the first thing
   in the child branch, write `1000` to `/proc/self/oom_score_adj`.
5. **Up-front byte-compilation** — `preload_bundle()` changes
   from a trivial `source(app.R)` to
   `compiler::cmpfile() + compiler::loadcmp()` on the bundle's
   `global.R` / `app.R`, capturing the `shinyApp()` arguments into
   a persistent `captured_app` variable. The FORK handler then
   calls `runApp(captured_app, port=...)` in the child instead of
   `runApp(bundle_path, port=...)`. See decision K3.

Full diff lives in this step when the phase is ready to land. The
original design for all five changes is in this document's
decision K1 / K3 / K5 (below) and in the phase-3-9 history of
`zygote.R` prior to the carve-out.

### Step K7: Backend `Forking.StatsClient` method + zygote spawn wiring

Phase 3-9's `Forking` interface has three methods: `Fork`,
`KillChild`, `ChildExits`. Phase 3-10 adds a fourth:

```go
// StatsClient resolves a workerID to its live zygote control
// client for metrics polling (decision K2). Returns nil if the
// worker is unknown or not in zygote mode. The Manager's metrics
// goroutine calls this on each tick; the backend continues to
// own the client's lifetime.
StatsClient(workerID string) *zygotectl.Client
```

Both `internal/backend/docker/forking.go` and
`internal/backend/process/forking.go` implement it by resolving
`workerID` to `ws.fork.client`.

`WorkerSpec` gains a `KSM bool` field (alongside the existing
`Zygote bool` from phase 3-9) and both backend `Spawn` paths set:

- `BLOCKYARD_KSM_ENABLED=1` env var iff `spec.KSM` is true.
- Bind mount for `{BundleServerPath}/.zygote/zygote_helper.so` →
  container `/blockyard/zygote_helper.so` (or the bwrap equivalent
  for the process backend).
- `BLOCKYARD_HELPER_PATH=/blockyard/zygote_helper.so` env var.

The helper `.so` and its bind mount are added only when KSM is
enabled — apps with only `zygote = true` and no KSM never see the
helper at all, keeping the attack surface minimal.

### Step K8: `zygote.Manager` metrics-poll goroutine

Phase 3-9's `zygote.Manager` owns `bySession` bookkeeping, exit
handling, and the sweep loop. Phase 3-10 adds:

1. **`statsClient func(workerID string) *zygotectl.Client`** —
   set from `ManagerConfig.StatsClient` (supplied by the backend
   at construction time). When nil, metrics polling is skipped
   entirely.
2. **`workersActive map[string]struct{}`** — updated by two new
   methods `NotifyWorkerAlive(workerID)` and
   `NotifyWorkerGone(workerID)`, called by the backend's Spawn
   and Stop paths.
3. **`lastStats map[string]zygotectl.Stats`** — most recent poll
   result per worker, exposed via `LastStats(workerID)`. Used by
   the worker detail page to render without a protocol round-trip.
4. **`metricsLoop()` goroutine** — polls `STATS` from every
   active worker on a `metrics_interval` tick, updates
   `lastStats`, updates the labeled Prometheus gauges, and logs
   failures at debug level without blocking the loop.

Full Go code for `metricsLoop`, `pollMetricsOnce`,
`NotifyWorkerAlive`, `NotifyWorkerGone`, `LastStats` is preserved
in this draft below. Phase 3-10 lands these methods on the
existing `Manager` type; no new file is introduced.

```go
// metricsLoop polls STATS from every live zygote on the metrics
// tick and updates labeled Prometheus gauges. Runs only when
// ManagerConfig.StatsClient is set — i.e. when the backend
// supports dynamic stats collection. Missing or failing STATS
// calls are logged at debug level but do not block the loop;
// the next tick tries again. See decision K2.
func (m *Manager) metricsLoop() {
    t := time.NewTicker(m.metricsInterval)
    defer t.Stop()
    for {
        select {
        case <-t.C:
            m.pollMetricsOnce()
        case <-m.stop:
            return
        }
    }
}

func (m *Manager) pollMetricsOnce() {
    m.mu.Lock()
    workers := make([]string, 0, len(m.workersActive))
    for wid := range m.workersActive {
        workers = append(workers, wid)
    }
    m.mu.Unlock()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    for _, wid := range workers {
        client := m.statsClient(wid)
        if client == nil {
            continue
        }
        stats, err := client.Stats(ctx)
        if err != nil {
            slog.Debug("zygote: stats poll failed",
                "worker_id", wid, "error", err)
            continue
        }
        m.mu.Lock()
        m.lastStats[wid] = stats
        m.mu.Unlock()

        appID := ""
        if m.appIDFor != nil {
            appID = m.appIDFor(wid)
        }
        telemetry.ZygoteKSMMergingPages.WithLabelValues(appID, wid).
            Set(float64(stats.KSMMergingPagesZygote))
        telemetry.ZygoteKSMMergingPagesTotal.WithLabelValues(appID, wid).
            Set(float64(stats.KSMMergingPagesTotal))
    }
}

// NotifyWorkerAlive signals that a zygote worker has been
// successfully spawned and its control client is ready. Called by
// the backend's Spawn path after the control client constructor
// has succeeded. Enables metrics polling for the worker. Safe to
// call multiple times — idempotent.
func (m *Manager) NotifyWorkerAlive(workerID string) {
    m.mu.Lock()
    m.workersActive[workerID] = struct{}{}
    m.mu.Unlock()
}

// NotifyWorkerGone signals that a zygote worker has been stopped,
// evicted, or crashed. Called by the backend's Stop path and the
// control-connection watcher. Removes the worker from metrics
// polling and clears its cached stats + Prometheus gauges.
func (m *Manager) NotifyWorkerGone(workerID string) {
    m.mu.Lock()
    delete(m.workersActive, workerID)
    delete(m.lastStats, workerID)
    m.mu.Unlock()
    appID := ""
    if m.appIDFor != nil {
        appID = m.appIDFor(workerID)
    }
    telemetry.ZygoteKSMMergingPages.DeleteLabelValues(appID, workerID)
    telemetry.ZygoteKSMMergingPagesTotal.DeleteLabelValues(appID, workerID)
}
```

### Step K9: Prometheus metrics

Three new metrics in `internal/telemetry/metrics.go` alongside
phase 3-9's existing gauges:

```go
ZygoteKSMMergingPages = promauto.NewGaugeVec(prometheus.GaugeOpts{
    Name: "blockyard_zygote_ksm_merging_pages",
    Help: "KSM merging pages count for the zygote process itself.",
}, []string{"app_id", "worker_id"})

ZygoteKSMMergingPagesTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
    Name: "blockyard_zygote_ksm_merging_pages_total",
    Help: "KSM merging pages sum across the zygote and all tracked children.",
}, []string{"app_id", "worker_id"})

HostKSMPagesSharing = promauto.NewGauge(prometheus.GaugeOpts{
    Name: "blockyard_host_ksm_pages_sharing",
    Help: "Host-global ksmd pages_sharing, scraped from /sys/kernel/mm/ksm/pages_sharing.",
})
```

A server-level scraper runs on the same metrics tick as the
manager's metrics loop, reads `/sys/kernel/mm/ksm/pages_sharing`,
and updates `HostKSMPagesSharing`. Runs only when
`experimental.ksm = true` — no point opening `/sys/kernel/mm/ksm/`
on operators who haven't opted in.

### Step K10: KSM preflight checks

Phase 3-7 introduced `Backend.Preflight()`. Phase 3-10 adds KSM
checks to both implementations:

```go
func (b *Backend) preflightKSM(cfg *config.Config, apps []*db.AppRow) []PreflightWarning {
    flags := cfg.ExperimentalFlags()
    if !flags.KSM {
        return nil // operator has not opted in — no noise
    }
    anyAppUsesKSM := false
    for _, app := range apps {
        if app.KSM {
            anyAppUsesKSM = true
            break
        }
    }
    if !anyAppUsesKSM {
        return nil // server flag on but no apps opted in — still no noise
    }
    var warns []PreflightWarning
    if run, _ := readUint("/sys/kernel/mm/ksm/run"); run == 0 {
        warns = append(warns, PreflightWarning{
            Code: "ksm_not_running",
            Message: "KSM is not running on this host (/sys/kernel/mm/ksm/run == 0). " +
                "Enable with: echo 1 > /sys/kernel/mm/ksm/run",
        })
    } else if pts, _ := readUint("/sys/kernel/mm/ksm/pages_to_scan"); pts <= 100 {
        warns = append(warns, PreflightWarning{
            Code: "ksm_scan_rate_too_low",
            Message: "KSM pages_to_scan is at the kernel default. Recommended: " +
                "echo 2000 > /sys/kernel/mm/ksm/pages_to_scan",
        })
    }
    return warns
}
```

Both warnings are non-fatal; the zygote model still works. See
decision K1 for the operational impact of `pages_to_scan` being
too low.

### Step K11: API / CLI / UI for `ksm`

Mirror phase 3-9's `zygote` surface.

**API** — extend `updateAppRequest`:

```go
type updateAppRequest struct {
    // ...existing fields including Zygote from phase 3-9...
    KSM *bool `json:"ksm"`
}
```

Validation in `UpdateApp()` runs against the effective end-state:

```go
effectiveKSM := app.KSM
if body.KSM != nil {
    effectiveKSM = *body.KSM
}
transitioningToKSM := body.KSM != nil && *body.KSM && !app.KSM

if transitioningToKSM && !flags.KSM {
    badRequest(w, "ksm feature not enabled in server config (set experimental.ksm = true)")
    return
}

// KSM requires zygote on the same effective end-state.
if effectiveKSM && !effectiveZygote {
    badRequest(w, "ksm requires zygote to be enabled")
    return
}
```

Add `KSM` to `appResponseV2()` in `internal/api/runtime.go` and
to `swagger_types.go`.

**Server capabilities endpoint** (introduced in phase 3-9) gains
a second field:

```json
{
  "experimental": {
    "zygote": true,
    "ksm": false
  }
}
```

**CLI** — extend `by scale`:

```go
cmd.Flags().Bool("ksm", false,
    "Enable KSM memory merging in zygote workers (experimental, requires --zygote and experimental.ksm in server config)")

if cmd.Flags().Changed("ksm") {
    v, _ := cmd.Flags().GetBool("ksm")
    body["ksm"] = v
}
```

**UI** — admin-only toggle in `tab_settings.html`:

```html
{{if .IsAdmin}}
<div class="field-group">
    <label for="ksm">KSM memory merging</label>
    <p class="field-description">
        <em>Experimental.</em> Enables kernel same-page merging for
        zygote workers. Requires Linux 6.4+ and zygote enabled.
        Has known cross-session side-channel risk — not
        recommended for multi-tenant apps.
        <a href="...ksm docs link...">Learn more</a>.
    </p>
    <input type="checkbox" id="ksm" name="ksm"
           {{if .App.KSM}}checked{{end}}
           {{if or (not .Capabilities.ExperimentalKSM) (not .App.Zygote)}}disabled
             title="Requires experimental.ksm in server config AND zygote enabled on this app"{{end}}
           hx-patch="/api/v1/apps/{{.App.ID}}"
           hx-include="[name='ksm']"
           hx-swap="none">
</div>
{{end}}
```

### Step K12: Config — `ZygoteMetricsInterval`

Phase 3-9 added `ExperimentalConfig` with `Zygote bool`. Phase
3-10 adds `KSM bool` (Step K3 above).

One new field on `ProxyConfig` — the cadence at which
`zygote.Manager`'s metrics goroutine polls `STATS` (decision K2):

```go
type ProxyConfig struct {
    // ...existing fields...
    ZygoteMetricsInterval Duration `toml:"zygote_metrics_interval"` // default 30s
}
```

```go
if c.ZygoteMetricsInterval.Duration == 0 {
    c.ZygoteMetricsInterval = Duration{30 * time.Second}
}
```

Wired into `zygote.NewManager` from `cmd/blockyard/main.go`:

```go
srv.Zygotes = zygote.NewManager(
    forking,
    srv.Sessions,
    zygote.ManagerConfig{
        SweepInterval:   srv.Config.Proxy.AutoscalerInterval.Duration,
        MetricsInterval: srv.Config.Proxy.ZygoteMetricsInterval.Duration,
        StatsClient:     forking.StatsClient, // interface method, phase 3-10
        AppIDFor: func(wid string) string {
            return srv.WorkerApp(wid)
        },
    },
)
```

### Step K13: Tests

**`internal/zygotectl/control_test.go`** (extends phase 3-9 tests):

```go
func TestClient_Stats(t *testing.T)
// Test server responds to STATS with a canned multi-line block
// terminated by END. Client.Stats(ctx) returns a Stats value
// with the parsed fields.

func TestClient_StatsInterleavedWithChildExit(t *testing.T)
// Test server pushes a CHILDEXIT in the middle of a STATS
// response. Verify the CHILDEXIT is dispatched to Exits and
// the STATS response still parses correctly once END arrives.
// Covers the multi-line reader accumulation path.

func TestClient_StatsUnknownKeys(t *testing.T)
// STATS response includes future keys. Client ignores them
// without erroring and populates the known keys correctly.

func TestClient_InfoIncludesKSMStatus(t *testing.T)
// INFO response includes ksm_status and ksm_errno fields.
// Verify they land on the parsed Info struct.
```

**`internal/zygote/manager_test.go`** (extends phase 3-9 tests):

```go
func TestManager_MetricsLoopPollsActiveWorkers(t *testing.T)
// NotifyWorkerAlive twice; the mock StatsClient returns canned
// zygotectl.Stats for each. Tick the metrics loop. Verify
// LastStats(w) returns the expected values for both workers
// and the Prometheus gauges were updated with matching labels.

func TestManager_MetricsLoopSkipsGoneWorkers(t *testing.T)
// NotifyWorkerAlive, tick, then NotifyWorkerGone, tick. Verify
// the second tick does not call StatsClient for the gone worker,
// LastStats returns the zero value, and the Prometheus gauges
// were cleared via DeleteLabelValues.
```

**KSM helper unit test** (new):

```go
func TestZygoteHelper_GracefulFallback(t *testing.T)
// Mock the prctl call path via a build-tag-guarded test helper.
// Verify EINVAL → ksm_status="unsupported", EPERM →
// ksm_status="denied", 0 → ksm_status="enabled".
```

**Seccomp integration test** (new, in phase 3-10's sandboxing
track):

```go
func TestZygoteHelper_PrctlAllowedBySeccomp(t *testing.T)
// Run the zygote under the phase 3-10 seccomp profile. Verify
// enable_ksm() returns 0 (prctl(PR_SET_MEMORY_MERGE) not blocked).
```

**KSM-effectiveness integration test** (new, in
`internal/backend/docker/forking_integration_test.go` extension):

```go
func TestDockerForking_KSMEffectiveness(t *testing.T)
// Spawn a zygote with KSM enabled. Fork two children. Force
// gc(full=TRUE) in each. Poll STATS every 500ms until
// ksm_merging_pages_total > 0, timeout 30s. Fail on literal
// zero with the captured value logged. Skip if
// /sys/kernel/mm/ksm/run == 0 on the test host.
```

**DB and migration tests** (new):

```go
func TestUpdateApp_KSMRequiresZygote(t *testing.T)
// PATCH with ksm=true on an app with zygote=false → 400
// "ksm requires zygote to be enabled".

func TestUpdateApp_KSMRequiresServerFlag(t *testing.T)
// Server config has experimental.ksm = false but experimental.zygote
// = true. App already has zygote=true. PATCH with ksm=true → 400
// "ksm feature not enabled in server config".

func TestUpdateApp_KSMRequiresZygoteInSameRequest(t *testing.T)
// App has zygote=false, ksm=false. PATCH with both zygote=true
// and ksm=true in the same request → 200 (effective end-state
// satisfies both constraints).

func TestConfigLoad_RejectsKSMWithoutZygote(t *testing.T)
// Config file with [experimental] ksm=true and zygote=false (or
// absent) → fatal error at config load, server does not start.
```

---

## Design decisions (KSM track)

### K1. KSM via `prctl(PR_SET_MEMORY_MERGE)`, called from R itself through a tiny embedded C helper, gated behind an independent opt-in

R's generational GC writes mark bits to SEXP headers during
level-2 collections, which dirties every page containing a live
SEXP and breaks copy-on-write sharing between forked children.
Without a recovery mechanism, the zygote model's memory-sharing
story decays to "eventually equivalent to PSOCK workers" after
children have done a few GC cycles.

KSM is an **independent opt-in** from the zygote model itself —
see decision K4. Operators must set both `experimental.ksm = true`
at the server level and `apps.ksm = true` on the individual app
before any KSM-related behaviour activates. The zygote model's
startup-latency and isolation benefits do not depend on KSM, so
operators who don't want KSM (pre-6.4 kernels, multi-tenant apps
with side-channel concerns — see Deferred #1, hosts they don't
control) can still use the zygote model fully.

Linux 6.4 added `PR_SET_MEMORY_MERGE`, a process-level KSM opt-in
whose effect is that the kernel's `ksmd` daemon scans the
process's anonymous memory for pages matching other processes'
pages and merges them into shared physical frames. The intuition
is that after R's GC dirties pages in child A and child B, the
two children end up in *substantially similar* states — same
packages loaded, same live objects, same heap layout inherited
from the zygote — so `ksmd` finds pages that aren't touched by
per-child mutation (REFCNT bumps, attribute writes, ALTREP
materialization) and re-merges them. KSM recovers some, but not
all, of the sharing that GC breaks. The exact recovery rate is
workload-dependent and currently unmeasured for R workloads —
Meta reports ~6GB saved per 64GB machine on Instagram (CPython
controller + ~32 workers), but Python's object model is
structurally different (refcount in header, no mark bits, no JIT
in CPython), so that number is an upper-bound anchor, not a
prediction. Phase 3-10 ships the KSM opt-in plus observability
(decision K2) so operators can measure actual recovery in their
own deployments; the design does not commit to a specific
memory-savings figure.

**KSM effectiveness depends on decision K3.** R's JIT compiles
closure bodies in-place on first call, mutating closure SEXP
headers and thereby dirtying every page that holds user code.
Without decision K3's up-front bundle compilation, the JIT path
alone would be enough to keep KSM merge rates near zero for
bundle code. The two decisions are effectively a package: KSM
provides the merge mechanism, up-front compilation ensures there
are mergeable pages for it to find.

**Why the helper has to live on the R side:** `prctl` is
self-directed, and `PR_SET_MEMORY_MERGE` is stored in the
process's `mm_struct`. The kernel replaces `mm_struct` on every
`exec()` and only preserves dump-filter bits plus `MMF_HAS_PINNED`
(per `mmf_init_legacy_flags` in
`include/linux/sched/coredump.h`) — `MMF_VM_MERGE_ANY` is dropped
on the floor. A wrapper process that calls `prctl` and then
`exec`s R would set the flag on itself and lose it immediately;
R would start with KSM disabled. The only way to get the flag
set on R's mm_struct is for R itself to call `prctl` after its
own exec. Hence the helper: a tiny C shared library, loaded into
R's address space via `dyn.load` from `zygote.R`, called via the
`.C` interface.

**Why the C helper is dependency-free.** Using `.C` instead of
`.Call` means the helper needs no R headers — just `sys/prctl.h`
and `errno.h`. Compiled with a stock C compiler at blockyard
build time. Blockyard embeds one precompiled `.so` per supported
architecture via build-tag-guarded `//go:embed`. No R package, no
runtime compilation, no LD_PRELOAD, no cgo.

**Why LD_PRELOAD was rejected.** Would also work, but the
constructor runs before `main()` with no way to surface
success/failure information to the rest of the program in a
structured form. The explicit `dyn.load` + `.C` approach lets
`zygote.R` capture the prctl return value, update
`zygote_info$ksm_status` with one of eight concrete states
(`enabled`, `disabled`, `unsupported`, `denied`, `failed`,
`helper_missing`, `dlopen_failed`, `unknown`), log it in a
structured format, and serve it via the `INFO` control command.
Ops can query `INFO` and see immediately whether KSM is active
without parsing log lines.

**Required operator action.** KSM is host-side; blockyard can opt
a process in but can't force the kernel to scan. Operators must:

1. Enable ksmd: `echo 1 > /sys/kernel/mm/ksm/run`.
2. Tune the scan rate. The kernel default (`pages_to_scan=100`,
   `sleep_millisecs=20`) is ~20MB/sec scanned, which is
   desktop-tuned and far too slow for a server with multi-GB R
   bundles. Recommended starting point:
   `echo 2000 > /sys/kernel/mm/ksm/pages_to_scan`, which scans a
   1GB working set in roughly 2 minutes. Workloads with larger
   bundles or higher session churn may need 5000–10000.

Phase 3-10's preflight check warns if either `run=0` or
`pages_to_scan` is at the default 100, but only when both
`experimental.ksm = true` in server config AND at least one app
has `ksm = true` (decision K4) — operators who have not opted
into KSM see no preflight noise. Both warnings are non-fatal but
the `pages_to_scan` warning is the operationally important one
— see "RSS spike during recovery" below. Phase 3-10's seccomp
profile must allow `PR_SET_MEMORY_MERGE` (verified by the
`TestZygoteHelper_PrctlAllowedBySeccomp` integration test).

**RSS spike during recovery (and how decision K5 contains it).**
When N children all hit a level-2 GC at roughly the same time
(request burst, coordinated work), each one dirties most of its
inherited package pages, breaking COW in lockstep. Peak RSS
spikes from `~1 × bundle + N × per-session-delta` to
`~N × bundle + N × per-session-delta` until ksmd catches up. For
a 1GB bundle × 8 children that's ~7GB of transient growth. With
default `pages_to_scan=100`, recovery takes minutes — long
enough to OOM a host that was sized for the steady-state. Tuning
`pages_to_scan` (above) shrinks the recovery window. Decision K5
makes the spike survivable by pinning forked children at
`oom_score_adj=1000` so the OOM killer reaps a child (one session
lost, recoverable via the existing 307 fallback) instead of the
zygote (entire family lost, full preload cost on every affected
session). Capacity-model guidance for worst-case headroom math
is tracked as a follow-up issue.

**Fallback behaviour.** Every failure path is non-fatal. Opt-in
absent (`BLOCKYARD_KSM_ENABLED` not set, which is the default
whenever `experimental.ksm = false` or `apps.ksm = false`) →
`ksm_status=disabled`, prctl is never called, zygote starts
normally without joining the kernel merge pool. This is the
**most common** path by design. When the opt-in *is* active:
pre-6.4 kernel → `EINVAL` → `ksm_status=unsupported`, zygote
starts normally, memory model decays to PSOCK over time. No ksmd
running → helper succeeds but nothing happens, same result.
Seccomp blocks the syscall → `EPERM` → `ksm_status=denied`.
Helper `.so` missing or can't load → logged, zygote starts
anyway. The zygote model never refuses to run because KSM is
unavailable.

**Threat model caveat.** KSM has a documented side-channel
history (Suzaki et al. 2011; Bosman et al. 2016): bit-identical
pages merged across processes expose timing-based information
leakage via Flush+Reload and similar cache attacks.
`PR_SET_MEMORY_MERGE` opts a process into a *global* kernel merge
pool, so children within a zygote family, and zygotes across apps
on the same host, all participate in one merging domain — phase
3-10's sandboxing does not address this because it operates above
the physical-frame layer. The **primary mitigation is the
independent KSM opt-in from decision K4**: operators who don't
want KSM exposure leave `experimental.ksm = false` (the default)
or `apps.ksm = false` for affected apps, and those apps use the
zygote model with no KSM activation at all. The zygote and KSM
toggles in the UI are both admin-gated. See Deferred #1 below
for the full multi-tenant story and the further hardening tracked
as follow-up work.

### K2. KSM observability via a separate `STATS` control command, not by extending `INFO`

Decision K1 provides the KSM opt-in but the actual recovery rate
for R workloads is unmeasured and workload-dependent (REFCNT
bumps, attribute mutation, residual JIT activity on code not
caught by decision K3, and mark-bit skew all reduce the merge
rate from the theoretical maximum). Operators need to see the
real number for their bundle, not the design's estimate. Three
decisions shape how that data flows:

- **`STATS` is a new command, not a field on `INFO`.** `INFO` is
  queried once at `NewClient` time (phase 3-9) and cached
  read-only — that's correct for static facts (R version, KSM
  enablement status, preload time) but wrong for
  continuously-changing metrics. Mixing the two would force
  either re-querying `INFO` on every metrics tick (and
  invalidating the cached startup state) or racing the reader
  goroutine. A separate `STATS` command is requested on a tick
  by a dedicated metrics goroutine using the normal `request()`
  path; the existing `reqMu` serialises it against `FORK`/`KILL`
  traffic.

- **Source of truth is `/proc/<pid>/ksm_stat` (Linux 6.1+).** The
  zygote knows its own PID and tracks each child's PID in the
  `children` env. On `STATS`, `zygote.R` reads
  `/proc/self/ksm_stat` plus `/proc/<childpid>/ksm_stat` for each
  tracked child, parses the `ksm_merging_pages` line, and returns
  `ksm_merging_pages_zygote`, `ksm_merging_pages_children` (sum),
  and `ksm_merging_pages_total`. Per-process granularity lets the
  worker detail page attribute savings to specific apps; the
  zygote+children sum is the headline number. If
  `/proc/<pid>/ksm_stat` doesn't exist (kernel < 6.1), the values
  are zero and `STATS` returns `ksm_stat_supported=0` so the
  metrics goroutine can record the failure mode without flooding
  logs.

- **Two metric scopes: per-zygote and host-global.**
  `blockyard_zygote_ksm_merging_pages{app_id, worker_id}` and
  `blockyard_zygote_ksm_merging_pages_total{app_id, worker_id}`
  come from `STATS` polling. A separate server-level scraper
  reads `/sys/kernel/mm/ksm/pages_sharing` every metrics-interval
  and updates an unlabeled `blockyard_host_ksm_pages_sharing`
  gauge. Host-global is what operators budget against (whole-host
  RAM); per-zygote is what they need for app-level capacity
  planning. Both ship in phase 3-10. Metrics live in
  `internal/telemetry/metrics.go` alongside the existing gauges
  (`WorkersActive`, `SessionsActive`).

The `metrics_interval` defaults to 30s. Lower values give fresher
graphs at the cost of slightly more `STATS` round-trips on the
control channel; given that round-trips serialise on `reqMu`
against `FORK`/`KILL`, an aggressive value would contend with
cold starts under load. 30s is a comfortable floor; operators can
tune via `[proxy] zygote_metrics_interval` if needed.

**Integration test as the regression net.** A KSM regression
(kernel change, seccomp tightening, ksmd disabled by another
process, helper bug) currently has no signal — the zygote starts
cleanly, sessions work, memory just slowly grows. The integration
test forks two children, forces `gc(full=TRUE)` in each, polls
`STATS` until `ksm_merging_pages_total > 0`, fails on literal
zero with the captured value logged, and skips itself if
`/sys/kernel/mm/ksm/run == 0`. This catches "KSM stopped working
entirely" without flaking on the noisy "what's the exact merge
count" question.

### K3. Bundle code is byte-compiled up front in the zygote via `compiler::cmpfile()`; packages are left alone

R's JIT compiles closure bodies on first call via an in-place
`SET_BODY` write that mutates the closure SEXP header in shared
COW pages. Every forked child hitting a first-time call ends up
dirtying those pages and allocating its own BCODESXPs in
child-local heap — even when two children compile the same
function, the resulting bytecode objects live at different
addresses, so the BODY pointers in the now-private closure
headers differ and KSM cannot merge them. Left unaddressed,
post-fork JIT is the dominant source of page divergence for user
code.

Separately from KSM, up-front byte-compilation saves 100–500ms
per cold start for typical bundles by eliminating the re-parse /
re-source cost of `app.R` in every child — the FORK handler
calls `runApp(captured_app, ...)` against the already-compiled
shiny object instead of `runApp(bundle_path, ...)`. That
latency win is independent of KSM and is reason enough to keep
this optimisation even if KSM is disabled.

The fix has to be scoped carefully:

- **Packages are left alone on purpose.** Most CRAN packages
  arrive byte-compiled at install time (`R_COMPILE_PKGS=1` is the
  R 4.0+ default). Packages that ship with `ByteCompile: no` in
  DESCRIPTION opted out, usually because the compiler chokes on
  something they do (complex `NextMethod` chains, unusual `<<-`
  patterns, etc.). A namespace-walk that mass-recompiles every
  closure across the entire dependency tree would second-guess
  package authors' decisions, risk breaking their `ByteCompile:
  no` opt-outs, and require metaprogramming gymnastics
  (`unlockBinding` / `lockBinding`) to mutate locked namespace
  bindings. None of that is worth it: the package decision is
  already made, we should respect it, and the residual post-fork
  JIT cost for those few packages is bounded.
- **Bundle code is compiled via `cmpfile` + `loadcmp`.** The
  zygote parses `global.R` and `app.R` with
  `compiler::cmpfile(src, out)`, which walks the expression tree
  and compiles function literals as a side effect of compiling
  the enclosing assignment. `compiler::loadcmp(out, envir = env)`
  then evaluates the compiled expressions, producing closures
  whose bodies are `BCODESXP` from birth. No post-fork
  `R_cmpfun1` path, no `SET_BODY` writes, no child-local bytecode
  allocations for bundle code.
- **The shinyApp object is captured, not re-sourced.** The
  current phase-3-9 preload stubs `shinyApp(...)` as a no-op; the
  phase-3-10 version captures the arguments into
  `captured_app <<- shiny::shinyApp(ui, server, ...)`. The FORK
  handler then calls `runApp(captured_app, port = ...)` in the
  child instead of `runApp(bundle_path, ...)`.

**Fallback for unusual bundle structures.** Bundles that split
`ui.R` and `server.R` without a top-level `shinyApp()` call, or
that call `shinyApp()` conditionally, will leave `captured_app
== NULL` after preload. The FORK handler detects this and falls
back to `runApp(bundle_path, ...)` in the child — identical to
the pre-precompile behaviour. The zygote logs
`preload ... warning=no_shinyapp_captured` so operators can see
the fallback is active. Bundle packages themselves still benefit
from being pre-loaded, so startup latency is still improved;
only the KSM memory-sharing story degrades for bundle closures
in this case.

**What this does not catch (residuals):**

- Files `source()`d from *inside* the server function at request
  time (e.g., `source("R/utils.R", local = TRUE)`). Those run in
  the child, not the zygote. Operators who care can call
  `compiler::cmpfile()` themselves in `app.R` before the server
  function is defined, which folds the helpers into the compiled
  bundle env.
- Anonymous closures created at runtime
  (`factory <- function(x) function(y) x + y`). These are small
  in number and short-lived; residual JIT activity on them is
  bounded.
- Lazy S4 / R6 method table materialization. Bounded by
  construction.

Residual activity is small enough that we leave JIT enabled at
the default level; disabling it would give up the speedup on
runtime-created closures in exchange for a very small reduction
in page divergence. Revisit if measurements show otherwise.

### K4. Two-level opt-in for KSM: server-wide `experimental.ksm` flag AND per-app `ksm` column, independent of the zygote opt-in

Phase 3-9 established the two-level opt-in shape for the zygote
model (`experimental.zygote` server flag + `apps.zygote` per-app
column). Phase 3-10 repeats the pattern for KSM with one
deliberate difference: the KSM gate is **independent** of the
zygote gate, not subordinate to it at the gate level. The
subordination happens at the validation layer (`ksm=true`
requires `zygote=true` on the same app) but the two flags are
separate booleans operators can toggle independently.

**Why KSM is its own gate independent of zygote.** The zygote
model's two unconditional benefits (startup latency and
per-session isolation) are *not* coupled to KSM at all. Only the
opportunistic memory-sharing benefit depends on KSM. Operators
who want startup + isolation but don't want the KSM side-channel
exposure (Deferred #1 below) should be able to get that
configuration without forking the design. Two flags is the
simplest way to express it.

**The full gate matrix across phases 3-9 and 3-10:**

| Gate                     | Location   | Phase  | Controls                                            |
|--------------------------|------------|--------|-----------------------------------------------------|
| `experimental.zygote`    | server toml | 3-9   | Whether the zygote code path runs at all            |
| `experimental.ksm`       | server toml | 3-10  | Whether `PR_SET_MEMORY_MERGE` is ever called        |
| `apps.zygote`            | db column  | 3-9    | Whether a specific app uses the zygote model        |
| `apps.ksm`               | db column  | 3-10   | Whether a specific app's zygote enables KSM         |

**Validation rules** (enforced in `UpdateApp` and `CreateApp`):

- Setting `apps.ksm = true` requires `apps.zygote = true` on the
  same effective end-state → otherwise 400 with
  `"ksm requires zygote to be enabled"`
- Setting `apps.ksm = true` requires `experimental.ksm = true`
  in server config → otherwise 400 with
  `"ksm feature not enabled in server config"`
- `experimental.ksm = true` in server config requires
  `experimental.zygote = true` → config load rejects the
  combination with a fatal error at startup

**Runtime gating (orthogonal to validation):** Even if an app
row has `apps.ksm = true` from an older config state, the runtime
short-circuits to the non-KSM path whenever `experimental.ksm =
false`. This gives operators a **kill switch**: flipping
`experimental.ksm` to `false` in config and restarting the
server instantly disables the feature for every app, without
requiring database updates or API calls. Zygote workers spawn
without `BLOCKYARD_KSM_ENABLED`, and `zygote.R`'s `enable_ksm()`
skips the prctl call and reports `ksm_status=disabled`.

**UI gating:** The settings tab reads the effective server config
via the capabilities endpoint (phase 3-9) and disables the KSM
toggle when `experimental.ksm` is off. Tooltip on the disabled
toggle explains "set `experimental.ksm = true` in the server
config to enable this feature."

**Alternatives considered:**

- **Fold KSM into `experimental.zygote`.** Simpler but couples
  the two decisions — operators who want zygote without KSM
  exposure would have no clean way to express it. Rejected.
- **A single `trust_level` enum on each app** (`single_tenant` /
  `multi_tenant`) that implicitly disables KSM for
  `multi_tenant`. Higher-level but overfits to one axis of
  variation — there are tenants who want multi-user but accept
  KSM risk, and tenants who want single-user but still don't
  want KSM for unrelated reasons (e.g., pre-6.4 kernel). Two
  explicit flags compose more cleanly. Rejected for phase 3-10;
  remains an option for future consolidation.

### K5. Children pin themselves at `oom_score_adj=1000`; zygote stays at default

When KSM-broken pages spike RSS during a coordinated GC burst
(decision K1's "RSS spike during recovery"), the kernel's OOM
killer picks the process with the highest `oom_score`. Without
intervention that's the zygote — it has the largest RSS and the
same default `oom_score_adj=0` as everything else in the cgroup.
Killing the zygote loses the entire family: the preload
investment, every active session, and forces a full cold start
on the next request. Killing a single child loses one session,
which the existing 307-redirect fallback recovers transparently.

Linux exposes `/proc/<pid>/oom_score_adj` precisely for biasing
this decision. The value is added to the badness score (max
effective score: 1000), and crucially **raising your own
`oom_score_adj` is unprivileged** — `CAP_SYS_RESOURCE` is only
required to *lower* it. So each forked child writes `1000` to
`/proc/self/oom_score_adj` as the very first thing it does
post-fork, before opening the listening socket or running any
user code. The zygote stays at the default `0`. Under memory
pressure the kernel will always reap a child first, regardless
of the RSS ratio between zygote and children.

Three properties of this approach matter:

- **No privilege coupling.** Phase 3-10 sandboxing drops most
  capabilities; if we relied on `CAP_SYS_RESOURCE` to lower the
  zygote's adj, we'd have to coordinate with the cap-dropping
  story or special-case the zygote. Raising the child's adj
  sidesteps the issue entirely — works in any environment,
  including the most locked-down phase 3-10 sandbox.
- **Bounded blast radius.** In both backends the only OOM
  candidates within the affected cgroup or namespace are the
  zygote and its children. Setting children to the maximum
  `1000` doesn't risk reaping unrelated host processes.
- **Failure mode is observable.** If the `writeLines` to
  `/proc/self/oom_score_adj` fails (extremely unlikely — this is
  a self-write to a procfs file always present on Linux), the
  child logs `blockyard_zygote_child event=oom_adj_failed` and
  continues. The child still serves correctly; only the OOM bias
  is missing. Operators can grep for the event in structured
  logs to confirm whether the bias is in place.

Alternatives considered:

- **Lower the zygote to `-800` or `-1000` instead.** Cleaner
  symbolically (it's "the system process") but requires
  `CAP_SYS_RESOURCE`, which phase 3-10 will need to grant
  explicitly. Also risks edge cases where `-1000` makes the
  zygote OOM-immune and the kernel panics instead of killing
  anything. Rejected.
- **Use cgroup memory limits to bound children individually.**
  Solves a related problem (per-session memory cap) but doesn't
  change the OOM-kill order when the *group* is under pressure.
  Also requires rootless cgroup delegation which isn't available
  in all supported deployments. Orthogonal, not a substitute.
- **Have the zygote `madvise(MADV_DONTNEED)` package pages on
  the children's behalf when memory pressure is high.**
  Theoretically attractive (proactive recovery) but R has no
  idea which pages are "package pages" vs. session pages, and
  getting this wrong corrupts the heap. Way out of scope.

---

## Deferred

1. **KSM side-channel documentation and future hardening.** KSM
   has a well-documented side-channel history — Suzaki et al.,
   *"Memory deduplication as a threat to the guest OS"* (EuroSec
   2011) first described the vector, and Bosman et al., *"Dedup
   Est Machina"* (IEEE S&P 2016) weaponized it. When two
   processes have bit-identical pages merged into one physical
   frame, either process can detect access patterns on the other
   via cache-timing measurement (Flush+Reload and variants). In
   the phase 3-10 model this matters in two places:

   - **Cross-session within a zygote family.** All children
     inherit `MMF_VM_MERGE_ANY` from the zygote, so they
     participate in the same merge domain. A malicious session
     on app X can measure timing patterns against other sessions
     on app X and leak data held in bit-identical pages — code,
     constants, cached auth tokens, shared lookup tables with
     sensitive values.
   - **Cross-zygote across apps on the same host.** Worse:
     `PR_SET_MEMORY_MERGE` puts a process into a *global*
     kernel-level merge pool, not a per-cgroup or per-namespace
     one. If app A's zygote and app B's zygote both opt in, any
     bit-identical pages between them get merged by the same
     ksmd. A malicious session on app A can Flush+Reload against
     app B's memory via the shared physical frames. Mount
     namespaces, seccomp, capability dropping, and cgroups do
     not address this — KSM operates below all of those
     isolation primitives.

   **Phase 3-10's sandboxing does not fix this.** The sandboxing
   track addresses `/tmp` leakage, capability excess, and
   syscall surface — all above the physical-frame layer where
   KSM operates. Treat the two concerns as independent
   prerequisites for multi-tenant deployment.

   **Phase 3-10 already ships the primary mitigation: the per-app
   `ksm` flag and the server-wide `experimental.ksm` flag from
   decision K4.** Operators who are concerned about side channels
   leave `experimental.ksm = false` (the default) and/or leave
   `apps.ksm = false` for affected apps. Those apps get the
   zygote model's unconditional benefits (startup latency,
   per-session isolation) without any KSM exposure —
   `prctl(PR_SET_MEMORY_MERGE)` is never called, the process
   never joins the kernel merge pool, and the memory model
   degrades to the PSOCK-equivalent steady state that phase 3-9's
   fallback path already supports. This is the "kill switch" path
   that decision K4 describes.

   **What remains deferred for multi-tenant hardening beyond the
   opt-in:**

   - **Threat-model documentation.** A dedicated operator-facing
     doc explaining when KSM is safe to enable, what data a
     malicious session can plausibly leak, and how to audit a
     bundle for sensitive singletons. Phase 3-10 ships only the
     inline warnings in the UI and a reference to Suzaki/Bosman
     in this design doc.
   - **Per-app fine-grained controls beyond the boolean.** For
     example, disabling KSM only for apps tagged as
     `access_type=public` (already a column in the apps table),
     or auto-disabling when OIDC user count exceeds a threshold,
     or a host-level deny list. These are all ergonomic
     refinements on top of the opt-in that phase 3-10 ships.
   - **Active side-channel mitigations** — scrubbing sensitive
     memory regions, using `MADV_UNMERGEABLE` on specific
     allocations, or kernel patches that scope the merge pool by
     cgroup. All of these are substantial R&D work and don't
     belong in a phase focused on landing the mechanism.

   The phase 3-10 opt-in gate is the structural fix; the
   deferred items here are refinements that make the opt-in more
   ergonomic and better documented for operators who need to
   make the call.

2. **Sandboxing design detail.** The post-fork sandboxing track
   is currently a deliverables sketch (above). Expand to full
   step-by-step + design decisions before committing this phase
   to land. Specifically:
   - How does the child apply `unshare` without losing access to
     the shiny libraries? (LD_LIBRARY_PATH, bind mounts)
   - What does the seccomp-bpf filter look like in detail? Which
     syscalls survive?
   - How are rlimits chosen (memory, CPU, nproc)? Are they
     per-app configurable?
   - What's the exact order of operations post-fork? (unshare →
     mount → seccomp → capset → setrlimit → runApp?)
