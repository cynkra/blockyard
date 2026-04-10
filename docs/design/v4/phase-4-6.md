# Phase 4-6: Zygote Hardening & KSM

Two companion tracks layered on top of phase 4-5's zygote mechanism:

1. **Post-fork sandboxing** — per-child isolation (private user and
   mount namespaces, private `/tmp`, seccomp-bpf, capability
   dropping, `RLIMIT_NPROC` fork-bomb guard) applied by the child
   itself immediately after `parallel::mcparallel` forks and before any
   bundle runtime code executes. Unlocks multi-tenant safety for
   the zygote model.
2. **Opt-in kernel same-page merging** — process-level KSM via
   `prctl(PR_SET_MEMORY_MERGE)` on Linux 6.4+, so the zygote
   model can recover copy-on-write memory sharing that R's
   generational GC breaks. Behind a second two-level opt-in.

These land in the same phase because:

- KSM's RSS-spike failure mode during coordinated GC recovery
  needs the sandbox-level containment that this phase introduces
  (the `oom_score_adj` pinning from the sandboxing track is what
  bounds the blast radius of a KSM recovery window).
- KSM's documented side-channel history and the sandboxing
  track's multi-tenant audit story share the same operator-facing
  question ("is this app safe to enable for multiple users?").
  Landing them together means the answer is one decision, not two.
- Both tracks extend the same `internal/zygotectl/zygote_helper.c`
  file and the same build-time seccomp compilation pipeline.
  Splitting would force either duplicating the helper file or
  landing half of it in each phase.

Nothing outside `internal/zygote{,ctl}/`, the backend `forking.go`
files, `zygote.R`, the seccomp JSON profiles, and the API/UI
surface for the `ksm` column changes. The zygote mechanism from
phase 4-5 stays intact — phase 4-6 extends it, doesn't
restructure it.

> **Multi-tenant gate.** Phase 4-5 explicitly documented
> (Deferred #1) that the zygote model must not be enabled on
> multi-tenant production apps between phase 4-5 and this phase.
> Phase 4-6 closes that gate — post-fork sandboxing provides the
> per-session isolation that makes multi-tenant zygote safe, and
> the opt-in KSM gate keeps operators with side-channel concerns
> firmly in non-KSM territory. Both the UI toggle and the phase
> doc stop warning against multi-tenant zygote once this phase
> lands.

## What phase 4-6 actually adds

### Post-fork sandboxing

Phase 4-5 ships the zygote mechanism with **no per-child
isolation**. Children fork the zygote, inherit its entire address
space (including loaded packages), and run `runApp()` on their
assigned port. They share the worker-level sandbox — the Docker
container or the bwrap sandbox from phase 3-7 — but nothing
separates one child from its siblings. Specifically:

- `/tmp` is shared across all children in a worker. A session can
  read another session's temporary files, cached downloads,
  intermediate `write.csv` outputs, scratch RDS files, etc.
- Capabilities inherited from the worker apply to every child
  equally. A stray `CAP_NET_BIND_SERVICE` in the worker container
  is accessible from every session inside it.
- The outer container / bwrap seccomp profile is sized for "a
  worker running R + shiny + bundle code", not "a Shiny session
  serving untrusted users". It's deliberately permissive enough
  to let package installs, `system()` calls in bundle code, and
  other legitimate R runtime behaviour work.
- No resource limits bound a single child's process-count, so a
  runaway session that `parallel::mclapply`s itself into
  oblivion can consume the whole worker's nproc budget.

**Phase 4-6's post-fork sandboxing adds a second, per-child
isolation layer applied from inside the child immediately after
fork.** The layers compose — the worker-level sandbox is the
first wall, the post-fork child sandbox is the second. Defense in
depth for multi-tenant workloads.

The per-child sandbox is implemented by a single atomic C
function, `apply_post_fork_sandbox`, exported from the existing
`internal/zygotectl/zygote_helper.c`. The child branch of
`zygote.R`'s `FORK` handler calls it exactly once before invoking
`runApp()`. The function performs the entire setup in C, in the
correct order, with a static guard that short-circuits subsequent
calls. The R-visible surface is one symbol. No R code (including
bundle code) can reach the underlying syscall primitives directly
— the seccomp filter installed at the end of the function blocks
them for the remainder of the child's lifetime.

See "Design decisions" D1 (helper shape), D2 (order of
operations), D5 (why only `RLIMIT_NPROC`), and D6 (zygote-variant
bwrap profile).

### KSM memory recovery

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
after GC and re-merges them. Up-front bundle byte-compilation
(decision D9) prevents the JIT from dirtying closure pages
post-fork; together the two decisions provide KSM with a stable
merge substrate for user code.

KSM is an **independent opt-in from the zygote model itself**.
Operators must set both `experimental.ksm = true` at the server
level and `apps.ksm = true` on the individual app before any
KSM-related behaviour activates. The zygote model's
startup-latency and isolation benefits do not depend on KSM;
operators who don't want KSM (pre-6.4 kernels, multi-tenant apps
with side-channel concerns — see Deferred #1, hosts they don't
control) still get the zygote model fully.

The exact memory recovery rate is workload-dependent and
currently unmeasured for R. Meta reports ~6GB saved per 64GB
machine on Instagram (CPython controller + ~32 workers), but
Python's object model is structurally different from R's
(refcount in header, no mark bits, no JIT in CPython), so that
figure is a directional anchor at best. Phase 4-6 ships the
KSM opt-in plus observability (decisions D8, D10) so operators
can measure actual recovery in their own deployments; the
design does not commit to a specific memory-savings figure.

**Five gates must all be open for the memory benefit to materialise:**

1. **`experimental.zygote = true`** in the server config (phase
   4-5). Default off.
2. **`apps.zygote = true`** on the specific app (phase 4-5).
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
     images.
   - **`/sys/kernel/mm/ksm/run == 1`**. ksmd is off by default
     on most distros. Operators must enable it.
   - **`pages_to_scan` tuned above the desktop default of 100.**
     Stock defaults scan at ~20 MB/sec, which is far too slow
     for multi-GB R bundles. See decision D7 for tuning
     guidance.

Gates 1–4 are the application-level opt-in; gate 5 is the host
capability. When any fail, the zygote model from phase 4-5 still
ships and still delivers its unconditional benefits (startup
latency and isolation) unchanged — only the memory-sharing story
degrades to the PSOCK-equivalent steady state. Preflight checks
(step 16) warn on the host sub-conditions when the opt-in gates
are open, so operators see the problem without reading this doc.
Operators who have not opted into KSM see no preflight noise.

---

## Prerequisites from earlier phases

- **Phase 3-1** — migration discipline. The `ksm` column follows
  expand-only rules: `ADD COLUMN ... NOT NULL DEFAULT 0`. The DDL
  linter, convention check, and roundtrip test all apply.
- **Phase 3-6** — per-app config pattern. The `ksm` field
  mirrors `zygote` and the other per-app settings: DB column →
  `AppRow` → `AppUpdate` → API → CLI → UI. The two-level opt-in
  layers on top via the same `ExperimentalConfig` shape phase
  4-5 introduced.
- **Phase 3-7** — process backend with bwrap sandboxing. Phase
  4-6 extends the process backend's spawn path to select
  between two bwrap seccomp profiles (non-zygote and zygote) and
  to bind-mount the zygote helper `.so` into the sandbox.
- **Phase 3-8** — outer-container seccomp profile
  (`docker/blockyard-seccomp.json`) and bwrap seccomp profile
  (`docker/blockyard-bwrap-seccomp.json`) plus the
  `cmd/seccomp-compile/` pipeline that compiles JSON to BPF.
  Phase 4-6 adds two new JSON profiles compiled via the same
  pipeline (`blockyard-bwrap-zygote-seccomp.json` for zygote
  workers on the process backend, `blockyard-post-fork-seccomp.json`
  for the in-helper filter applied inside each child) and
  extends the outer-container profile to conditionally allow
  `prctl(PR_SET_MEMORY_MERGE)` when KSM is enabled.
- **Phase 4-5** — the zygote mechanism. The `Forking` interface,
  `internal/zygotectl/` and `internal/zygote/` packages, the
  embedded `zygote.R` script, the control protocol
  (`AUTH`/`FORK`/`KILL`/`STATUS`/`INFO` + async `CHILDEXIT`),
  the cold-start integration, and the `zygote` /
  `experimental.zygote` opt-in all exist by the time phase 4-6
  lands. Phase 4-6 modifies several of these files rather than
  creating parallel copies — see the step list for specific
  edits.

---

## Deliverables

**Post-fork sandboxing:**

1. **Consolidated C helper** `internal/zygotectl/zygote_helper.c`
   exporting exactly two functions: `enable_ksm` (pre-fork,
   parent zygote, KSM only) and `apply_post_fork_sandbox`
   (post-fork, child, atomic). Per-architecture `.so` built via
   a Makefile rule, embedded via build-tag-guarded `//go:embed`.
2. **Post-fork seccomp profile**
   `docker/blockyard-post-fork-seccomp.json` + overlay, compiled
   via the phase-3-8 `cmd/seccomp-compile/` pipeline to a BPF
   blob embedded in the helper's generated C source at build
   time. Applied by `apply_post_fork_sandbox` as the final step
   of the setup chain.
3. **Zygote-variant bwrap seccomp profile**
   `docker/blockyard-bwrap-zygote-seccomp.json` + overlay,
   compiled alongside the phase-3-8 profile. Permits
   `unshare(CLONE_NEWUSER|CLONE_NEWNS)` so the post-fork
   sandbox setup can run inside the bwrap sandbox on the
   process backend. The non-zygote profile remains unchanged.
4. **Docker backend security options** — container create call
   for zygote workers adds
   `--security-opt seccomp={BundleServerPath}/.zygote/blockyard-seccomp.json`
   (the phase-3-8 outer profile, which already permits
   `unshare(CLONE_NEWUSER)`) and
   `--security-opt apparmor=unconfined` (Ubuntu 23.10+ only)
   when the zygote model is enabled on the target app.
5. **Process backend bwrap profile selection** — `Spawn` reads
   `spec.Zygote` and feeds `/etc/blockyard/seccomp-zygote.bpf`
   to bwrap instead of `/etc/blockyard/seccomp.bpf` when
   spawning a zygote worker.
6. **`zygote.R` post-fork hook** — the child branch of the
   `FORK` handler calls `.C("apply_post_fork_sandbox",
   integer(1))` as its first action, then checks the result
   code and calls `mcexit(1L)` on any non-zero
   value. Only after success does the child call
   `runApp(captured_app, port = ...)`.
7. **Environment variable hardening** — `OMP_NUM_THREADS=1` and
   `MKL_NUM_THREADS=1` exported in the zygote process via the
   `Spawn` env list, so every child inherits single-threaded
   BLAS / OpenMP defaults unless it explicitly bumps them.
8. **Package compatibility documentation** — a new section of
   the operator guide categorising bundle R packages by
   fork-safety:
   - Safe to pre-load (shiny, ggplot2, dplyr, DT, …)
   - Dangerous to pre-load, must load in each child (arrow,
     torch, rJava, anything with open fds or threads at load
     time)
   - Safe if not used before fork (DBI, RPostgres, any
     pool-based DB client)

**Opt-in KSM:**

9. **`ksm` column on the `apps` table** — migration `004_ksm`,
   following the same expand-only shape as migration `003_zygote`
   from phase 4-5. Validated to require `zygote = true` on the
   same effective end-state (see decision D3).
10. **`experimental.ksm` server-wide flag** — second field on the
    `ExperimentalConfig` struct introduced in phase 4-5.
    Config-load validation rejects `experimental.ksm = true`
    without `experimental.zygote = true`. Runtime short-circuits
    to non-KSM behaviour whenever the flag is off — kill switch.
11. **Helper `enable_ksm` function** — called at zygote startup
    immediately before the main control loop begins, gated on
    `BLOCKYARD_KSM_ENABLED=1`. When enabled: graceful fallback
    on older kernels (EINVAL) and seccomp-restricted
    environments (EPERM); failures surface via the `INFO`
    control command for ops visibility. See decision D7.
12. **Outer-container seccomp extension** — the phase-3-8
    `docker/blockyard-seccomp.json` gains a conditional rule
    allowing `prctl(PR_SET_MEMORY_MERGE)` only under the
    appropriate arg-match. The bwrap-zygote profile gains the
    same allow rule. Gated via a regenerated profile — not a
    runtime toggle — so operators running without KSM have no
    code path that could enable it.
13. **`STATS` control command** — new command on the zygote
    control protocol returning dynamic per-zygote KSM merge
    counts read from `/proc/<pid>/ksm_stat`. Distinct from
    `INFO`, which stays cached startup state. See decision D8.
14. **`zygote.Manager` metrics-poll goroutine** — new goroutine
    that calls `Stats` on each live zygote's control client on
    a `metrics_interval` tick (default 30s) and updates labeled
    Prometheus gauges. Wired up via a new
    `StatsClient(workerID) *zygotectl.Client` method on the
    `Forking` interface.
15. **Prometheus metrics** —
    `blockyard_zygote_ksm_merging_pages{app_id, worker_id}` and
    `blockyard_zygote_ksm_merging_pages_total{app_id, worker_id}`
    plus an unlabeled `blockyard_host_ksm_pages_sharing`
    populated by a server-level scraper reading
    `/sys/kernel/mm/ksm/pages_sharing`. See decision D8.
16. **KSM preflight checks** — each backend's `Preflight()` impl
    (introduced in phase 3-7) gains two checks, gated on both
    `experimental.ksm = true` in server config AND at least one
    app having `ksm = true`: read `/sys/kernel/mm/ksm/run` and
    `/sys/kernel/mm/ksm/pages_to_scan`. Non-fatal warnings
    otherwise.
17. **Up-front bundle byte-compilation in `zygote.R`** — replaces
    the phase-4-5 `source(app.R)` preload with
    `compiler::cmpfile(app.R, tempfile())` +
    `compiler::loadcmp()` so bundle closures are born as
    `BCODESXP` in the zygote. See decision D9.
18. **Child `oom_score_adj=1000` pinning** — handled as the
    first syscall inside `apply_post_fork_sandbox` (decision
    D11). Self-write, unprivileged, no capability coupling.
19. **API / CLI / UI surface for `ksm`** — `ksm` field on
    `updateAppRequest`, `--ksm` flag on `by scale`, settings
    tab toggle in the UI (admin-only, gated on the server
    capabilities endpoint). The existing server capabilities
    endpoint from phase 4-5 gains an `experimental.ksm` field.

---

## Step-by-step

### Step 1: Migration — `ksm` column

Phase 4-5's migration `003_zygote` adds the `zygote` column.
Phase 4-6's migration `004_ksm` adds the `ksm` column. Both
additive, nullable-equivalent (default 0), backward-compatible
per phase 3-1 rules. Separate migrations because the features
ship in separate phases — phase 4-5 can land, be deployed, and
run in production before phase 4-6's migration touches the
schema again.

**`internal/db/migrations/sqlite/004_ksm.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN ksm INTEGER NOT NULL DEFAULT 0;
```

**`internal/db/migrations/sqlite/004_ksm.down.sql`:**

SQLite does not support `DROP COLUMN` before 3.35.0, so this
follows the same table-recreate pattern as migration `003_zygote`
(which follows `002_app_config`'s template). The down migration
lists every other column (including `zygote`) and omits `ksm`:

```sql
CREATE TABLE apps_new (
    id                      TEXT PRIMARY KEY,
    -- ... all other columns including zygote, verbatim from 003_zygote ...
    zygote                  INTEGER NOT NULL DEFAULT 0
);
INSERT INTO apps_new SELECT
    id, /* ... */, zygote
FROM apps;
DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
CREATE UNIQUE INDEX idx_apps_name_live ON apps(name) WHERE deleted_at IS NULL;
```

**`internal/db/migrations/postgres/004_ksm.up.sql`:**

```sql
-- phase: expand
ALTER TABLE apps ADD COLUMN ksm BOOLEAN NOT NULL DEFAULT FALSE;
```

**`internal/db/migrations/postgres/004_ksm.down.sql`:**

```sql
ALTER TABLE apps DROP COLUMN ksm;
```

The up-down-up roundtrip test from phase 3-1 covers both
dialects. The DDL linter confirms the up migration is expand-only.

### Step 2: DB layer — `AppRow.KSM`, `AppUpdate.KSM`

Add to `AppRow` in `internal/db/db.go`:

```go
type AppRow struct {
    // ...existing fields including Zygote from phase 4-5...
    KSM bool `db:"ksm" json:"ksm"`
}
```

Add to `AppUpdate`:

```go
type AppUpdate struct {
    // ...existing fields including Zygote from phase 4-5...
    KSM *bool
}
```

Update `UpdateApp()` to handle the new field:

```go
if u.KSM != nil {
    app.KSM = *u.KSM
}
```

Add `ksm = ?` to the UPDATE SQL and `app.KSM` to the bind list.
`CreateApp` defaults to `false`.

### Step 3: `experimental.ksm` flag on `ExperimentalConfig`

Phase 4-5 added the `ExperimentalConfig` struct with a single
`Zygote` bool. Phase 4-6 extends it:

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

The `ExperimentalFlags()` nil-safe accessor from phase 4-5 keeps
working unchanged — an absent `[experimental]` section returns
the zero struct, which has both fields false.

Example config:

```toml
[experimental]
zygote = true
ksm = true
```

### Step 4: `internal/zygotectl/zygote_helper.c` — the consolidated C helper

This is the load-bearing file of the sandboxing track. The
helper exposes exactly two functions to R via the `.C`
interface; no other symbols are visible to bundle code. Both
functions are designed to be either idempotent or call-once
guarded, so user R code calling them via `.C` after the initial
setup is a complete no-op.

The file lives in `internal/zygotectl/zygote_helper.c` and is
compiled to a `.so` per supported architecture by a Makefile
rule (see Step 5 for the embed layout). Deliberately tiny and
dependency-free: no R headers, no Rcpp, no stdlib beyond what
the syscalls need. Compiles with a stock C compiler and has
no link-time dependency on libR. The `.C` interface uses plain
pointer arguments, so this file works with a bare C compiler.

```c
/*
 * blockyard zygote helper.
 *
 * Exposes two functions to R via .C(). Both are loaded once at
 * zygote startup and inherited across the fork performed by
 * parallel::mcparallel.
 *
 *   enable_ksm(int *result)
 *     Called once in the parent zygote, gated on
 *     BLOCKYARD_KSM_ENABLED=1. Opts the zygote's mm_struct into
 *     the kernel's KSM merge pool. The flag is inherited by
 *     every child forked by mcparallel via ksm_fork in the kernel, so setting
 *     it once on the zygote covers every child spawned afterward.
 *
 *   apply_post_fork_sandbox(int *result)
 *     Called once in each child spawned by mcparallel, as the
 *     first R statement after fork. Performs the entire post-fork
 *     sandbox setup in C, in the correct order, with a static
 *     guard that short-circuits subsequent calls. After this
 *     function returns successfully, every dangerous syscall it
 *     wraps (unshare, mount, prctl, setrlimit, capset, seccomp)
 *     is blocked by the installed seccomp filter — even if user
 *     R code calls .C("apply_post_fork_sandbox") again, the
 *     static guard short-circuits and no syscall path runs.
 *
 * Both functions write their result code into the caller-supplied
 * int buffer. Zero is success; non-zero uniquely identifies the
 * failure mode so the R side can log it and, for the sandbox
 * function, call mcexit(1L) to abort the child cleanly.
 *
 * Defensive design:
 *   - No other symbols exported. The internal helpers
 *     (write_oom_score_adj, mount_private_tmp, etc.) are static.
 *   - apply_post_fork_sandbox is guarded by a static int so it
 *     runs at most once per process. User R code cannot undo
 *     a prior sandbox setup by calling it again.
 *   - The post-fork seccomp filter installed at the end of the
 *     function is the primary defense against user R code
 *     reaching any of the underlying syscalls. The static guard
 *     is belt-and-suspenders on top.
 */

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <string.h>
#include <unistd.h>
#include <linux/capability.h>
#include <linux/seccomp.h>
#include <linux/filter.h>
#include <sys/mount.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/syscall.h>

#ifndef PR_SET_MEMORY_MERGE
#define PR_SET_MEMORY_MERGE 67
#endif

#ifndef PR_SET_NO_NEW_PRIVS
#define PR_SET_NO_NEW_PRIVS 38
#endif

/*
 * Error codes written to *result by apply_post_fork_sandbox. Chosen
 * so 0 = success and each nonzero value uniquely identifies the
 * failing step. The R side maps these to a log line before calling
 * mcexit(1L).
 */
#define ERR_OOM_ADJ        1
#define ERR_NO_NEW_PRIVS   2
#define ERR_UNSHARE        3
#define ERR_MOUNT_TMP      4
#define ERR_RLIMIT_NPROC   5
#define ERR_CAPSET         6
#define ERR_SECCOMP        7

/*
 * post_fork_seccomp_filter is the BPF program installed at the end
 * of apply_post_fork_sandbox. Compiled at build time from
 * docker/blockyard-post-fork-seccomp.json via cmd/seccomp-compile/
 * (phase 3-8 pipeline) and written to a generated header included
 * below. The header defines:
 *   static const unsigned char post_fork_seccomp_bpf[];
 *   static const unsigned int  post_fork_seccomp_bpf_len;
 * The filter is computed relative to BPF instruction granularity
 * (sizeof(struct sock_filter)); the generated header exports the
 * byte length and the install function divides.
 */
#include "zygote_helper_seccomp_generated.h"

/* ------- enable_ksm ------- */

void enable_ksm(int *result) {
    if (prctl(PR_SET_MEMORY_MERGE, 1, 0, 0, 0) == 0) {
        *result = 0;
    } else {
        *result = errno;
    }
}

/* ------- apply_post_fork_sandbox ------- */

static int sandbox_applied = 0;

static int write_oom_score_adj_1000(void) {
    int fd = open("/proc/self/oom_score_adj", O_WRONLY | O_CLOEXEC);
    if (fd < 0) return -1;
    ssize_t n = write(fd, "1000\n", 5);
    int save = errno;
    close(fd);
    if (n != 5) {
        errno = save;
        return -1;
    }
    return 0;
}

static int mount_private_tmp(void) {
    /*
     * tmpfs mount inside a user namespace has been permitted since
     * Linux 4.18. The new userns root (mapped uid) has CAP_SYS_ADMIN
     * in the namespace and can mount tmpfs. Size capped at 64 MiB
     * — individual sessions that need more should not be using the
     * zygote model (see the package compatibility documentation in
     * deliverable #8).
     */
    return mount("tmpfs", "/tmp", "tmpfs",
                 MS_NOSUID | MS_NODEV,
                 "size=64m,mode=1777");
}

static int drop_all_capabilities(void) {
    /*
     * capset() with all sets zero drops permitted, effective, and
     * inheritable. The ambient set is cleared automatically when
     * permitted is cleared. After unshare(CLONE_NEWUSER) the process
     * is the mapped root in the new namespace and has a full cap set
     * *in that namespace*; capset() zeroes it so user code runs with
     * no caps even within the sandbox.
     */
    struct __user_cap_header_struct hdr = {
        .version = _LINUX_CAPABILITY_VERSION_3,
        .pid = 0,
    };
    struct __user_cap_data_struct data[2] = {{0, 0, 0}, {0, 0, 0}};
    return syscall(SYS_capset, &hdr, &data);
}

static int install_post_fork_seccomp(void) {
    /*
     * Compute struct sock_fprog pointing at the compiled BPF blob
     * and install via the seccomp(2) syscall. Using seccomp(2)
     * directly instead of prctl(PR_SET_SECCOMP) gets us the
     * SECCOMP_FILTER_FLAG_TSYNC flag, which applies the filter
     * to every thread in the calling process — important because
     * R's BLAS may have spun up helper threads before fork, and
     * we want the filter on all of them.
     */
    struct sock_fprog prog = {
        .len = post_fork_seccomp_bpf_len / sizeof(struct sock_filter),
        .filter = (struct sock_filter *)post_fork_seccomp_bpf,
    };
    return syscall(SYS_seccomp,
                   SECCOMP_SET_MODE_FILTER,
                   SECCOMP_FILTER_FLAG_TSYNC,
                   &prog);
}

void apply_post_fork_sandbox(int *result) {
    if (sandbox_applied) {
        *result = 0;
        return;
    }

    /* 1. oom_score_adj = 1000 — child is OOM-reapable from this
          point on, so if any subsequent step hangs or fails the
          kernel will prefer killing this child over its zygote
          parent (decision D11). */
    if (write_oom_score_adj_1000() != 0) {
        *result = ERR_OOM_ADJ;
        return;
    }

    /* 2. NO_NEW_PRIVS — required before seccomp can install a
          filter without CAP_SYS_ADMIN. Also prevents any setuid
          binary the child might exec from regaining privileges. */
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0) {
        *result = ERR_NO_NEW_PRIVS;
        return;
    }

    /* 3. Create private user + mount namespaces. The new userns
          grants CAP_SYS_ADMIN inside the namespace (needed for
          the tmpfs mount in step 4); the new mount namespace
          ensures the tmpfs at /tmp doesn't leak into the
          parent's mount ns. */
    if (unshare(CLONE_NEWUSER | CLONE_NEWNS) != 0) {
        *result = ERR_UNSHARE;
        return;
    }

    /* 4. Mount private tmpfs at /tmp, replacing whatever was
          there before (inherited from the worker-level sandbox
          or the container). Each child gets its own /tmp that
          siblings cannot observe. */
    if (mount_private_tmp() != 0) {
        *result = ERR_MOUNT_TMP;
        return;
    }

    /* 5. RLIMIT_NPROC = 64 — fork-bomb guard. Does not bound
          memory or CPU; those remain bounded by the worker-level
          cgroup (Docker backend) or unbounded (process backend,
          per phase 3-7's no-cgroup stance). See decision D5. */
    {
        struct rlimit nproc = { .rlim_cur = 64, .rlim_max = 64 };
        if (setrlimit(RLIMIT_NPROC, &nproc) != 0) {
            *result = ERR_RLIMIT_NPROC;
            return;
        }
    }

    /* 6. Drop all capabilities. In the new userns the process
          was the mapped root with a full cap set; capset() zeros
          it so bundle code runs with no caps at all. */
    if (drop_all_capabilities() != 0) {
        *result = ERR_CAPSET;
        return;
    }

    /* 7. Install the post-fork seccomp filter. After this point,
          every dangerous syscall we just used is blocked for the
          remainder of this child's lifetime — including
          unshare(), mount(), setrlimit(), capset(), prctl() with
          the PR_SET_MEMORY_MERGE arg, seccomp() itself, and the
          other items in the filter's deny list. User R code
          calling .C("apply_post_fork_sandbox") again hits the
          static guard before any of these syscalls runs, and
          even if somehow the guard were bypassed the filter
          would return EPERM on each call. Defense in depth. */
    if (install_post_fork_seccomp() != 0) {
        *result = ERR_SECCOMP;
        return;
    }

    sandbox_applied = 1;
    *result = 0;
}
```

**Build.** The helper compiles to a per-architecture `.so` via a
Makefile rule alongside the phase-3-8 seccomp-compile pipeline.
The Makefile invokes `cmd/seccomp-compile` to produce
`post-fork-seccomp.bpf`, then runs a small code generator that
writes `zygote_helper_seccomp_generated.h` (containing the
`post_fork_seccomp_bpf` byte array), then compiles
`zygote_helper.c` with `cc -shared -fPIC -Wl,--no-undefined`.
Output: `zygote_helper_linux_amd64.so`,
`zygote_helper_linux_arm64.so`, etc.

Cross-compilation uses `CC=aarch64-linux-gnu-gcc` or equivalent;
no cgo is required on the Go side because the `.so` is consumed
by R, not by blockyard itself.

**Why a single `.so` and a single entry point.** See decisions D1
(consolidation) and D2 (atomic sandbox setup) for the security
rationale. The short version is: R code (including user bundle
code) can call any exported `.so` symbol via `.C`. Collapsing to
one entry point means user code can't cherry-pick steps, can't
reorder them, and can't reach the underlying syscall primitives
as individual callable units. The static guard plus the seccomp
filter at the end of the function together make the whole
function effectively call-once and its underlying syscalls
unreachable from user code after the first successful call.

### Step 5: `internal/zygotectl/` embed layout

Phase 4-5's `internal/zygotectl/embed.go` ships `ZygoteScript`.
Phase 4-6 extends it with the helper source (for debugging /
reproducibility only) and adds per-arch files for the compiled
`.so`:

```go
// internal/zygotectl/embed.go (extended)
package zygotectl

import _ "embed"

//go:embed zygote.R
var ZygoteScript []byte

//go:embed zygote_helper.c
var HelperSource []byte  // debugging / reproducibility only
```

Plus per-architecture `.so` embeds (one file per supported arch):

```go
// internal/zygotectl/embed_linux_amd64.go
//go:build linux && amd64

package zygotectl

import _ "embed"

//go:embed zygote_helper_linux_amd64.so
var HelperSO []byte
```

```go
// internal/zygotectl/embed_linux_arm64.go
//go:build linux && arm64

package zygotectl

import _ "embed"

//go:embed zygote_helper_linux_arm64.so
var HelperSO []byte
```

`HelperSO` is what the backends write to disk at zygote spawn
and bind-mount into the worker. `HelperSource` is committed for
reproducibility (operators can rebuild the `.so` from source to
verify) but is never touched at runtime.

The non-Linux build tags return a build-time error if anyone
tries to include the zygote mechanism on a non-Linux platform:

```go
// internal/zygotectl/embed_nonlinux.go
//go:build !linux

package zygotectl

var HelperSO []byte // empty — zygote model is Linux-only
```

An empty `HelperSO` byte array causes the backend's Spawn path
to fail loudly when it tries to write the helper to disk; the
zygote model is explicitly Linux-only per phase 4-5's design.

### Step 6: Post-fork seccomp profile — `docker/blockyard-post-fork-seccomp.json`

This is the most restrictive seccomp filter in the design. It's
applied by the child itself (via `seccomp(2)`) at the end of
`apply_post_fork_sandbox`, and it governs what the child can do
for the remainder of its lifetime while serving Shiny requests.

**Authoring.** The profile is a new JSON file compiled via the
phase-3-8 `cmd/seccomp-compile/` pipeline. Same vendored-upstream
+ overlay pattern as the outer and bwrap profiles from phase
3-8. Source files:

- `docker/blockyard-post-fork-seccomp.json` — merged output, committed
- `docker/blockyard-post-fork-seccomp-overlay.json` — hand-edited overlay
- Base: phase-3-8's `blockyard-bwrap-seccomp.json` (the inner
  sandbox profile, sized for "untrusted process running R +
  shiny + user code")

**Differences from the phase-3-8 bwrap inner profile** — the
post-fork filter is strictly more restrictive, re-tightening the
small number of syscalls the helper itself needs during sandbox
setup. Because the filter is installed *after* those syscalls
have already been used by the helper, re-tightening is safe:

- **`unshare`, `clone` with CLONE_NEW*, `setns`** — explicitly
  denied (ENOSYS). The child has already done its unshare; any
  further namespace creation is pathological. Blocking them also
  prevents user R code from calling the helper's `.C` entry
  point in some hypothetical bypass scenario.
- **`mount`, `umount2`, `pivot_root`, `chroot`** — explicitly
  denied. Same reasoning; `/tmp` has already been remounted.
- **`setrlimit`, `prlimit64`** with a non-lowering argument —
  can't express cleanly as an arg match for `setrlimit`; the
  kernel already enforces "unprivileged can only lower" so the
  filter allows these unconditionally and relies on kernel
  semantics.
- **`capset`, `prctl(PR_CAP_AMBIENT)`** — explicitly denied.
  Capabilities were dropped in step 6 of the helper; further
  cap operations are either no-ops or attempts to poison state.
- **`prctl(PR_SET_NO_NEW_PRIVS)`** — allowed (already set, so
  calling again is a no-op).
- **`prctl(PR_SET_MEMORY_MERGE)`** — denied unconditionally.
  KSM is enabled pre-fork on the zygote via `enable_ksm`; the
  flag is inherited by the child, no child-side call is ever
  needed. Blocking this denies user code the ability to opt a
  child into KSM unilaterally against operator configuration.
- **`prctl(PR_SET_SECCOMP)`, `seccomp`** — allowed. A bundle
  that wants to further restrict itself (e.g., call
  `seccomp_load` in a defensive wrapper) should be able to.
  Filters compose with AND, so anything the bundle installs is
  strictly narrower than the post-fork filter.
- **`unshare_user`, `clone3`** — denied.

**What's allowed.** Everything R + shiny + httpuv + standard
bundle packages need at runtime: socket operations (for the
listening port and outbound DB connections), file I/O on
read-only library paths, BLAS/OpenMP operations (`sched_setaffinity`
allowed with a warn-log hook), `brk`/`mmap` for R's allocator,
`read`/`write` for stdin/stdout/log files, `gettimeofday` and
friends, signal handling, futex operations. The filter is
explicitly permissive for the Shiny request-handling path; it's
restrictive for syscalls that could escape the sandbox or poison
shared kernel state.

**Compilation** — the phase-3-8 `seccomp-compile` Docker build
stage gains one more invocation:

```dockerfile
FROM golang:1.25.9-alpine AS seccomp-compiler
RUN apk add --no-cache build-base libseccomp-dev
# ...existing phase-3-8 setup...
COPY docker/blockyard-post-fork-seccomp.json /tmp/post-fork-seccomp.json
RUN /seccomp-compile -in /tmp/post-fork-seccomp.json \
                     -out /blockyard-post-fork-seccomp.bpf
# Also: generate the C header that embeds the BPF blob.
RUN /seccomp-compile -in /tmp/post-fork-seccomp.json \
                     -format c-header \
                     -out /zygote_helper_seccomp_generated.h \
                     -symbol post_fork_seccomp_bpf
```

`seccomp-compile` gains a `-format c-header` mode that emits a
`.h` with a `const unsigned char <symbol>[]` declaration and a
companion `_len` size constant. The helper's build rule copies
this header to `internal/zygotectl/` before compiling
`zygote_helper.c`. Small extension of the existing tool; no new
dependencies.

**Runtime path.** The BPF blob is linked into the helper `.so`
as a const byte array and installed by `install_post_fork_seccomp`
(see Step 4). Unlike the bwrap profile (which bwrap loads from
a separate file), the post-fork profile is statically linked
into the helper. Operators cannot override it without rebuilding
the helper — the blob is part of the binary's trust chain.

### Step 7: Zygote-variant bwrap profile — `docker/blockyard-bwrap-zygote-seccomp.json`

Phase 3-8 step 3 ships `docker/blockyard-bwrap-seccomp.json`, the
inner seccomp profile applied by bwrap to non-zygote process
backend workers. That profile explicitly re-tightens
`clone`/`unshare` so workers cannot create further namespaces
once inside the bwrap sandbox.

Phase 4-6's post-fork sandboxing requires the child to call
`unshare(CLONE_NEWUSER | CLONE_NEWNS)` from inside the bwrap
sandbox. That's a direct conflict with the phase-3-8 re-tightening.

**Resolution: a second bwrap profile, compiled alongside the
first, used only for zygote workers.** The non-zygote profile
stays as-is; zygote workers get a relaxed variant. Two profiles
live in the codebase, two BPF blobs ship in the process-backend
image, the process backend picks between them at spawn time
based on `spec.Zygote`.

**`docker/blockyard-bwrap-zygote-seccomp.json`:**

Source files:

- `docker/blockyard-bwrap-zygote-seccomp.json` — merged output, committed
- `docker/blockyard-bwrap-zygote-seccomp-overlay.json` — hand-edited overlay
- Base: `docker/blockyard-bwrap-seccomp.json` (the non-zygote bwrap profile)

The overlay adds a single relaxation: allow `clone` / `clone3` /
`unshare` with `CLONE_NEWUSER | CLONE_NEWNS` arguments. The
relaxation is narrow — other `CLONE_NEW*` flags (net, pid, ipc,
uts, cgroup) remain denied — because the post-fork sandbox only
needs the user and mount namespaces. The phase-3-8 rationale for
re-tightening everything else still applies: zygote workers
should not be creating PID or network namespaces post-fork.

Overlay content (approximate):

```json
{
  "syscalls": [
    {
      "names": ["clone", "clone3", "unshare"],
      "action": "SCMP_ACT_ALLOW",
      "args": [
        {
          "index": 0,
          "value": 268435456,
          "valueTwo": 402653184,
          "op": "SCMP_CMP_MASKED_EQ"
        }
      ]
    }
  ]
}
```

`268435456 = 0x10000000 = CLONE_NEWUSER`,
`402653184 = 0x18000000 = CLONE_NEWUSER | CLONE_NEWNS`. The
`SCMP_CMP_MASKED_EQ` operator with mask `0x18000000` on the
first argument permits these two flags and denies any other
namespace flag. `cmd/seccomp-compile` already handles this
operator (phase 3-8).

**Also relaxed: `prctl(PR_SET_MEMORY_MERGE)` when KSM is
enabled.** The zygote-variant bwrap profile additionally allows
`prctl` with `PR_SET_MEMORY_MERGE = 67` so `enable_ksm` can run
inside bwrap on the process backend. Gated the same way the
outer-container profile is gated (see Step 8): the profile is
compiled in two flavours, with or without the `prctl` allowance,
and the image includes whichever flavour matches the build
configuration for that image variant.

**Compilation pipeline extension:**

```dockerfile
FROM golang:1.25.9-alpine AS seccomp-compiler
# ...existing phase-3-8 invocations...
COPY docker/blockyard-bwrap-zygote-seccomp.json /tmp/bwrap-zygote-seccomp.json
RUN /seccomp-compile -in /tmp/bwrap-zygote-seccomp.json \
                     -out /blockyard-bwrap-zygote-seccomp.bpf
```

The process-backend image's final stage copies both blobs:

```dockerfile
COPY --from=seccomp-compiler /blockyard-bwrap-seccomp.bpf \
     /etc/blockyard/seccomp.bpf
COPY --from=seccomp-compiler /blockyard-bwrap-zygote-seccomp.bpf \
     /etc/blockyard/seccomp-zygote.bpf
```

The process backend's `Spawn` path reads `process.seccomp_profile`
(existing phase-3-7/3-8 config) for the non-zygote path and a
new `process.zygote_seccomp_profile` for the zygote path, with
defaults `/etc/blockyard/seccomp.bpf` and
`/etc/blockyard/seccomp-zygote.bpf` respectively.

### Step 8: Outer-container seccomp profile — conditional `PR_SET_MEMORY_MERGE`

Phase 3-8's `docker/blockyard-seccomp.json` (the outer container
profile) relaxes `clone`/`unshare` with `CLONE_NEWUSER` to let
bwrap work. It does not currently allow
`prctl(PR_SET_MEMORY_MERGE)`, and `enable_ksm` running inside a
Docker container (either a direct Docker-backend zygote or a
process-backend container wrapping bwrap) would get EPERM.

Phase 4-6 extends the overlay to add the relaxation, but
**gated via a separate profile variant, not a runtime toggle**.
Two reasons:

1. A runtime toggle ("allow prctl(PR_SET_MEMORY_MERGE) when KSM
   is enabled") would need to happen at `docker run` time, which
   means the server orchestration would need to compute the
   right `--security-opt` value on each container create. That's
   a lot of rope for a single syscall arg match.
2. Shipping the relaxed profile only in the image variants where
   KSM is supported means operators who use non-KSM images never
   have a code path that could allow the syscall. Trust chain is
   cleaner.

**Two outer profile variants:**

- `docker/blockyard-seccomp.json` — phase-3-8 version, no KSM
  relaxation. This is the default for all image builds where
  `experimental.ksm` is not part of the supported feature set.
- `docker/blockyard-seccomp-ksm.json` — merged output of the
  phase-3-8 version plus a KSM overlay adding the
  `prctl(PR_SET_MEMORY_MERGE)` allow.

The `blockyard-process` and `blockyard` (everything) image
variants ship the KSM variant at
`/etc/blockyard/seccomp-ksm.json` alongside the base profile at
`/etc/blockyard/seccomp.json`. The `blockyard-docker` image
variant ships only the base profile (Docker-backend zygote uses
Docker's own security-opt mechanism, not bwrap, and the Docker
backend's spawn path picks the right profile at container-create
time — see Step 11).

**The KSM overlay** (`docker/blockyard-seccomp-ksm-overlay.json`):

```json
{
  "syscalls": [
    {
      "names": ["prctl"],
      "action": "SCMP_ACT_ALLOW",
      "args": [
        {
          "index": 0,
          "value": 67,
          "op": "SCMP_CMP_EQ"
        }
      ]
    }
  ]
}
```

Phase 3-8's `cmd/seccomp-merge` handles multiple overlays; phase
4-6 invokes it twice: once for the base profile (existing
phase-3-8 invocation, unchanged) and once for the KSM variant
(new, adds the KSM overlay on top of the base).

**Bwrap-zygote profile — same overlay treatment.** The zygote
variant of the bwrap profile (Step 7) gets the same overlay
applied, producing `/etc/blockyard/seccomp-zygote-ksm.bpf` in
addition to `/etc/blockyard/seccomp-zygote.bpf`. The process
backend's `Spawn` selects between four combinations:

| `spec.Zygote` | `spec.KSM` | Profile |
|---|---|---|
| false | (n/a) | `seccomp.bpf` |
| true | false | `seccomp-zygote.bpf` |
| true | true | `seccomp-zygote-ksm.bpf` |

Non-zygote workers never touch KSM — no reason to expose the
prctl there.

### Step 9: `STATS` command on the control protocol

Phase 4-5 defined the control protocol with `AUTH`, `FORK`,
`KILL`, `STATUS`, `INFO`, and asynchronous `CHILDEXIT` pushes.
Phase 4-6 adds `STATS`:

```
client → server: STATS\n
server → client: <key>=<value>\n... END\n
                 # Dynamic, polled on a metrics tick. Known keys:
                 # ksm_merging_pages_zygote, ksm_merging_pages_children,
                 # ksm_merging_pages_total, child_count,
                 # ksm_stat_supported.
                 # Values read by zygote.R from /proc/<pid>/ksm_stat
                 # for itself and each tracked child PID. Parser
                 # ignores unknown keys (forward-compatible).
```

`INFO` also gains three fields carrying startup-time KSM state:
`ksm_status`, `ksm_errno`, and `sandbox_status`. The existing
`r_version` and `preload_ms` fields continue to ship. Unknown
keys are ignored on the Go side (phase-4-5's forward-compat
behaviour), so the addition is a pure extension.

**`internal/zygotectl/control.go` additions:**

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
// zygote.Manager's metrics goroutine — see decision D8. Uses the
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

`Info` gains `KSMStatus`, `KSMErrno`, and `SandboxStatus` fields:

```go
type Info struct {
    RVersion      string
    PreloadMS     int
    KSMStatus     string // "enabled", "disabled", "unsupported", "denied",
                         // "failed", "helper_missing", "dlopen_failed",
                         // "unknown"
    KSMErrno      int
    SandboxStatus string // "ready" — always, unless the helper failed
                         // to load, in which case the zygote would not
                         // have started serving control requests.
    Unknown       map[string]string
}
```

`fetchInfo` adds cases for `ksm_status`, `ksm_errno`, and
`sandbox_status`.

**`requestMulti()` — multi-line response reader.** Phase 4-5's
`request()` reads exactly one line. Phase 4-6 adds
`requestMulti()` for `STATS`, which accumulates lines in the read
loop until it sees `END`. The `readLoop` goroutine gains a
`pendingMulti` buffer on the client struct; `CHILDEXIT` pushes
arriving mid-response are still dispatched to `Exits` without
disrupting the accumulator. Implementation sketch:

```go
func (c *Client) requestMulti(ctx context.Context, line string) (string, error) {
    c.reqMu.Lock()
    defer c.reqMu.Unlock()

    c.mu.Lock()
    ch := make(chan replyMulti, 1)
    c.pendingMulti = ch
    _, err := c.conn.Write([]byte(line))
    c.mu.Unlock()
    if err != nil {
        // ...clear pendingMulti, return...
    }

    select {
    case r := <-ch:
        return r.body, r.err
    case <-ctx.Done():
        return "", ctx.Err()
    case <-c.closed:
        return "", errors.New("control: connection closed")
    }
}
```

`readLoop` branches on whether `c.pendingMulti` is set: if so,
lines are accumulated until `END\n` is seen, at which point the
accumulated body is dispatched on the channel. If
`c.pendingMulti` is nil, the existing single-line dispatch path
runs (same as phase 4-5).

### Step 10: `zygote.R` changes

Phase 4-5's `zygote.R` handles AUTH/FORK/KILL/STATUS/INFO with no
sandboxing, no KSM, and a `source()` / `sys.source()` preload. Phase
4-6 extends it with:

1. **Helper load.** After `preload_bundle()` completes (so
   bundle code cannot reach the helper during preload),
   `dyn.load(helper_path)` loads `zygote_helper.so`.
2. **Conditional KSM enablement.** If `BLOCKYARD_KSM_ENABLED=1`
   is set, call `.C("enable_ksm", integer(1))` and record the
   result into `zygote_info$ksm_status` / `ksm_errno`. If unset,
   record `ksm_status = "disabled"` without loading any KSM
   code path.
3. **Up-front byte-compilation of bundle code.** Replace the
   phase-4-5 `source()` / `sys.source()` calls with `compiler::cmpfile()` +
   `compiler::loadcmp()` so bundle closures are born as
   `BCODESXP`. Prevents the JIT from mutating closure SEXP
   headers post-fork. See decision D9.
4. **Post-fork sandbox hook.** The child branch of the `FORK`
   handler calls `.C("apply_post_fork_sandbox", integer(1))`
   before `runApp()`. On non-zero result, log the error and
   `mcexit(1L)`.
5. **`STATS` handler.** Reads `/proc/<pid>/ksm_stat` for the
   zygote and each tracked child, aggregates, writes a
   key-value response terminated by `END\n`.
6. **`INFO` handler extension.** Add `ksm_status`, `ksm_errno`,
   and `sandbox_status` keys to the response.

Full modified `zygote.R` (the parts that change — unchanged
lines elided for brevity):

```r
# blockyard zygote — phase 4-6 version
# (preamble, env reads, secret reads, zygote_info initialisation
#  unchanged from phase 4-5)

# NEW in 4-6: KSM startup state, filled in after preload_bundle.
zygote_info$ksm_status   <- "disabled"
zygote_info$ksm_errno    <- 0L
zygote_info$sandbox_status <- "ready"

# NEW in 4-6: captured_app compilation. Replaces the phase-4-5
# preload_bundle that did source() / sys.source() — see decision D9.
captured_app <- NULL

preload_bundle <- function() {
  env <- new.env(parent = globalenv())

  # Defensive runApp stub — same rationale as phase 4-5: bundles
  # that end with runApp(shinyApp(ui, server)) instead of bare
  # shinyApp(ui, server) would start httpuv in the zygote and
  # defeat the loadcmp()$value capture below. The stub intercepts
  # the unqualified runApp lookup and captures its first arg.
  env$runApp <- function(appDir = ".", ...) {
    if (inherits(appDir, "shiny.appobj")) {
      captured_app <<- appDir
    }
    invisible(NULL)
  }

  compile_and_load <- function(src) {
    if (!file.exists(src)) return(invisible())
    out <- tempfile(fileext = ".rc")
    compiler::cmpfile(src, out, options = list(optimize = 3L))
    compiler::loadcmp(out, envir = env)
  }

  # Optional global.R — sourced for side effects only.
  compile_and_load(file.path(bundle_path, "global.R"))

  # Required app.R. Fail fast if missing.
  app_r <- file.path(bundle_path, "app.R")
  if (!file.exists(app_r)) {
    stop(sprintf(paste0(
      "blockyard zygote: bundle has no app.R at %s. ",
      "Zygote mode requires a single app.R entrypoint whose ",
      "last expression evaluates to a shiny.appobj. Bundles ",
      "using the classic server.R + ui.R split are not ",
      "supported in zygote mode — either restructure the ",
      "bundle to use app.R, or disable zygote for this app."),
      bundle_path),
      call. = FALSE)
  }

  # Primary capture: loadcmp() returns the value of the last
  # expression, same as source()$value in phase 4-5. When app.R
  # ends with shinyApp(ui, server) — qualified or not — this is
  # the shiny.appobj.
  res <- compile_and_load(app_r)
  if (inherits(res, "shiny.appobj")) {
    captured_app <<- res
  }

  if (!inherits(captured_app, "shiny.appobj")) {
    last_class <- if (is.null(res)) "NULL" else paste(class(res), collapse = "/")
    stop(sprintf(paste0(
      "blockyard zygote: could not capture shiny.appobj from %s ",
      "(last expression was class '%s'). Zygote mode requires ",
      "app.R to end with a shinyApp(ui, server) call, or an ",
      "explicit runApp(shinyApp(ui, server)). Fix the bundle, ",
      "or disable zygote for this app."),
      app_r, last_class),
      call. = FALSE)
  }
}

preload_start <- Sys.time()
preload_bundle()
zygote_info$preload_ms <- as.integer(
  as.numeric(Sys.time() - preload_start, units = "secs") * 1000
)

# NEW in 4-6: Load the helper AFTER preload_bundle() finishes.
# Bundle code running during preload cannot reach the helper
# symbols because they are not yet loaded. See decision D1.
helper_path <- Sys.getenv("BLOCKYARD_HELPER_PATH",
                          "/blockyard/zygote_helper.so")
helper_loaded <- tryCatch({
  dyn.load(helper_path)
  TRUE
}, error = function(e) {
  message("blockyard_zygote event=helper_load status=failed error=",
          conditionMessage(e))
  zygote_info$ksm_status     <<- "helper_missing"
  zygote_info$sandbox_status <<- "helper_missing"
  FALSE
})

if (helper_loaded) {
  # After load, unlink the helper file. The .so is mmapped into
  # the process address space, symbols are fully resolved, but
  # the filesystem path is gone — bundle code that tries to
  # dyn.load the helper again by path fails with ENOENT.
  tryCatch(unlink(helper_path), error = function(e) NULL)
}

# NEW in 4-6: Conditional KSM opt-in. Gated on the env var set by
# the backend at spawn time (only when both experimental.ksm and
# apps.ksm are true). See decision D7.
ksm_opt_in <- identical(Sys.getenv("BLOCKYARD_KSM_ENABLED"), "1")
if (helper_loaded && ksm_opt_in) {
  result <- .C("enable_ksm", result = integer(1L))$result
  zygote_info$ksm_errno <- as.integer(result)
  zygote_info$ksm_status <- if (result == 0L) {
    "enabled"
  } else if (result == 22L) {   # EINVAL
    "unsupported"
  } else if (result == 1L) {    # EPERM
    "denied"
  } else {
    "failed"
  }
  message("blockyard_zygote event=ksm status=", zygote_info$ksm_status,
          " errno=", zygote_info$ksm_errno)
}

# Hygiene: full GC after package preload + KSM opt-in. Puts every
# surviving SEXP into the oldest generation with stable mark state
# so children fork from a clean, deterministic heap.
gc(full = TRUE)
message("blockyard_zygote event=gc_hygiene status=ok")

# (child tracking, pending event buffer, push_event, flush_pending,
#  reap_children — all unchanged from phase 4-5)

handle_command <- function(line) {
  parts <- strsplit(line, " ", fixed = TRUE)[[1]]
  cmd <- parts[1]
  if (cmd == "FORK") {
    port <- as.integer(parts[2])
    if (is.na(port) || port < port_lo || port > port_hi) {
      writeLines(sprintf("ERR port %s out of range\n", parts[2]),
                 con, sep = "")
      return()
    }
    cid <- next_child_id()
    job <- tryCatch(
      parallel::mcparallel({
        close(con)
        close(srv)

        # NEW in 4-6: apply_post_fork_sandbox must succeed or the
        # child exits. The helper performs the entire sandbox setup
        # atomically (oom_score_adj, NO_NEW_PRIVS, unshare, mount
        # tmpfs, setrlimit, capset, seccomp) with a static guard
        # against re-entry. Failure at any step returns a stable
        # error code. See decision D2.
        sandbox_result <- .C("apply_post_fork_sandbox",
                             result = integer(1L))$result
        if (sandbox_result != 0L) {
          message("blockyard_zygote event=sandbox status=failed step=",
                  sandbox_result)
          parallel:::mcexit(1L)
        }

        Sys.setenv(SHINY_PORT = as.character(port))
        shiny::runApp(captured_app, port = port)
      }, detached = TRUE, mc.set.seed = FALSE),
      error = function(e) {
        message("blockyard zygote mcparallel error: ",
                conditionMessage(e))
        NULL
      }
    )
    if (is.null(job)) {
      writeLines("ERR fork failed\n", con, sep = "")
      return()
    }
    # Parent: record the child.
    assign(cid, list(pid = job$pid, port = port), envir = children)
    writeLines(sprintf("OK %s %d\n", cid, job$pid), con, sep = "")
  } else if (cmd == "KILL") {
    # (unchanged from phase 4-5)
  } else if (cmd == "STATUS") {
    # (unchanged from phase 4-5)
  } else if (cmd == "INFO") {
    # NEW in 4-6: emit ksm_status, ksm_errno, sandbox_status.
    writeLines(sprintf("r_version=%s\n", zygote_info$r_version),
               con, sep = "")
    writeLines(sprintf("preload_ms=%d\n", zygote_info$preload_ms),
               con, sep = "")
    writeLines(sprintf("ksm_status=%s\n", zygote_info$ksm_status),
               con, sep = "")
    writeLines(sprintf("ksm_errno=%d\n", zygote_info$ksm_errno),
               con, sep = "")
    writeLines(sprintf("sandbox_status=%s\n", zygote_info$sandbox_status),
               con, sep = "")
    writeLines("END\n", con, sep = "")
  } else if (cmd == "STATS") {
    # NEW in 4-6. Reads /proc/<pid>/ksm_stat for the zygote and
    # each tracked child, aggregates, emits key=value lines
    # terminated by END. See decision D8.
    handle_stats(con)
  } else {
    writeLines(sprintf("ERR unknown command %s\n", cmd), con, sep = "")
  }
}

read_ksm_merging_pages <- function(pid) {
  path <- sprintf("/proc/%s/ksm_stat", as.character(pid))
  if (!file.exists(path)) return(NA_integer_)
  lines <- tryCatch(readLines(path, warn = FALSE),
                    error = function(e) character())
  for (line in lines) {
    if (startsWith(line, "ksm_merging_pages")) {
      parts <- strsplit(line, "[[:space:]]+")[[1]]
      return(suppressWarnings(as.integer(parts[length(parts)])))
    }
  }
  NA_integer_
}

handle_stats <- function(con) {
  zygote_pages <- read_ksm_merging_pages(Sys.getpid())
  supported <- !is.na(zygote_pages)
  if (!supported) zygote_pages <- 0L

  child_total <- 0L
  child_count <- 0L
  for (cid in ls(children)) {
    info <- get(cid, envir = children)
    p <- read_ksm_merging_pages(info$pid)
    if (!is.na(p)) {
      child_total <- child_total + p
      child_count <- child_count + 1L
    }
  }

  total <- zygote_pages + child_total
  writeLines(sprintf("ksm_merging_pages_zygote=%d\n", zygote_pages),
             con, sep = "")
  writeLines(sprintf("ksm_merging_pages_children=%d\n", child_total),
             con, sep = "")
  writeLines(sprintf("ksm_merging_pages_total=%d\n", total),
             con, sep = "")
  writeLines(sprintf("child_count=%d\n", child_count), con, sep = "")
  writeLines(sprintf("ksm_stat_supported=%d\n",
                     as.integer(supported)), con, sep = "")
  writeLines("END\n", con, sep = "")
}

# (main socketSelect loop unchanged from phase 4-5)
```

**Notes on the R-side changes:**

- **Helper is loaded AFTER `preload_bundle()`.** This is the
  primary defense against hostile bundle code calling helper
  functions during preload. At load time the helper symbols
  don't exist in the R address space; once loaded, bundle code
  has already finished running.
- **`unlink(helper_path)` after load.** Belt-and-suspenders —
  even if bundle code tried to `dyn.load` the helper by path,
  the file is gone from the filesystem. The `.so` remains
  mmapped into the process and its symbols are callable, but
  there's no way to re-load it.
- **KSM enablement is gated on `BLOCKYARD_KSM_ENABLED`.** The
  backend sets this env var only when both `experimental.ksm`
  and `apps.ksm` are true. If the backend doesn't set it,
  `enable_ksm` is never called and the prctl is never issued —
  even if the helper is loaded.
- **The post-fork sandbox call runs BEFORE any user code in the
  child.** The child expression in `mcparallel` does (1) close
  the inherited control and server-listen sockets, (2) run the
  sandbox hook, (3) call `runApp`. Steps (1) and (2) are blockyard-controlled
  code; step (3) is where bundle code executes. By that point
  the sandbox is fully installed.
- **Failure of the sandbox hook is fatal.** The child exits
  with `mcexit(1L)` on any non-zero result. The zygote's reaper
  observes the exit via `pskill(pid, 0L)` liveness polling and pushes a
  `CHILDEXIT c<n> 1 sandbox_failed` message through the control
  protocol. The Manager's `handleExit` cleans up bookkeeping
  and the next request for that session triggers a new cold
  start. Operators see the failure in `blockyard_zygote
  event=sandbox` log lines with a step code they can correlate
  to the helper's error constants.

### Step 11: Docker backend `Spawn` changes

Phase 4-5's Docker `Forking` implementation spawns a zygote
container with the embedded `zygote.R` bind-mounted at
`/blockyard/zygote.R` and environment variables for the bundle
path, control port, secret path, and child port range. Phase
4-6 extends `Spawn` with four additions:

1. **Apparmor and seccomp options.** When `spec.Zygote` is true,
   the container create call adds:
   - `--security-opt seccomp={BundleServerPath}/.zygote/blockyard-seccomp.json`
     — the phase-3-8 outer profile, or the KSM variant
     (`blockyard-seccomp-ksm.json`) when `spec.KSM` is true.
     Written to disk at `{BundleServerPath}/.zygote/` at
     server startup time.
   - `--security-opt apparmor=unconfined` — only on hosts where
     AppArmor is active (Ubuntu 23.10+ in particular). Needed
     because Ubuntu's default container apparmor profile blocks
     unprivileged user-namespace creation (the same operation
     the post-fork sandbox hook needs). `unconfined` disables
     apparmor for the container; the seccomp + user-namespace
     layers are the actual isolation mechanism.
   - Detection: on server startup, `cmd/blockyard/main.go`
     probes `/sys/module/apparmor/parameters/enabled` and
     stores the result in the server state. The Docker
     `Forking` spawn reads it and omits the option when
     apparmor is not active, to avoid spurious warnings in the
     Docker daemon log.
2. **Helper `.so` bind mount.** One additional read-only mount:
   `{BundleServerPath}/.zygote/zygote_helper.so` → container
   `/blockyard/zygote_helper.so`. The helper `.so` is written
   to disk at server startup from the embedded `HelperSO` byte
   array (see Step 5). One `.so` per server instance, reused
   across every zygote spawn.
3. **`BLOCKYARD_HELPER_PATH`** environment variable — points
   the zygote script at the bind-mounted `.so` path. Always
   set to `/blockyard/zygote_helper.so`.
4. **`BLOCKYARD_KSM_ENABLED=1`** environment variable — set iff
   `spec.KSM` is true. When unset, the zygote script skips
   `enable_ksm` and reports `ksm_status=disabled`. When set,
   the zygote attempts `enable_ksm` and records the actual
   result.

**`WorkerSpec` extension** (phase 4-5 adds `Zygote` and
`ControlSecret`; phase 4-6 adds one more field):

```go
type WorkerSpec struct {
    // ...existing fields including Zygote, ControlSecret from phase 4-5...
    KSM bool // enable KSM via PR_SET_MEMORY_MERGE in the zygote
}
```

Populated from the effective end-state (server-wide
`experimental.ksm` AND per-app `apps.ksm`) computed in
`coldstart.go` — see Step 15 for the validation path.

**Server-level helper initialisation.** In
`cmd/blockyard/main.go`, right after the zygote script is
written to disk for phase 4-5, add the helper `.so`:

```go
// Write the embedded helper .so to disk once per server lifetime.
// Both zygote backends bind-mount this path into their sandboxes.
helperPath := filepath.Join(cfg.BundleServerPath, ".zygote",
                            "zygote_helper.so")
if err := os.WriteFile(helperPath, zygotectl.HelperSO, 0o644); err != nil {
    return fmt.Errorf("write zygote helper: %w", err)
}

// Also write the outer container seccomp profile(s) for Docker backend.
// Phase 3-8 puts this at /etc/blockyard/ inside the image, but the
// Docker backend running ON a host needs a host-side path for
// --security-opt.
if err := writeOuterSeccompProfiles(cfg); err != nil {
    return fmt.Errorf("write outer seccomp: %w", err)
}
```

`writeOuterSeccompProfiles` writes one or two files depending on
whether `experimental.ksm` is set: always the base profile, plus
the KSM variant when the flag is on.

### Step 12: Process backend `Spawn` changes

Phase 4-5's process `Forking` spawns a zygote via bwrap, using
the phase-3-8 `blockyard-bwrap-seccomp.bpf` profile. Phase 4-6
adds profile selection logic plus the helper bind mount.

**Profile selection** — new config field on `ProcessConfig`:

```go
type ProcessConfig struct {
    // ...existing fields from phase 3-7 / 3-8 / 3-9...
    ZygoteSeccompProfile    string `toml:"zygote_seccomp_profile"`
    ZygoteKSMSeccompProfile string `toml:"zygote_ksm_seccomp_profile"`
}
```

Defaults in `processDefaults()`:

```go
if c.ZygoteSeccompProfile == "" {
    c.ZygoteSeccompProfile = "/etc/blockyard/seccomp-zygote.bpf"
}
if c.ZygoteKSMSeccompProfile == "" {
    c.ZygoteKSMSeccompProfile = "/etc/blockyard/seccomp-zygote-ksm.bpf"
}
```

Spawn path in `internal/backend/process/forking.go`:

```go
profile := cfg.SeccompProfile // non-zygote, phase-3-7/3-8 default
switch {
case spec.Zygote && spec.KSM:
    profile = cfg.ZygoteKSMSeccompProfile
case spec.Zygote:
    profile = cfg.ZygoteSeccompProfile
}
args := append(baseBwrapArgs, "--seccomp", profile)
```

Preflight extension — phase 3-7's `checkSeccompProfile` now
checks all three profiles when the zygote model is enabled:

```go
func (b *Backend) checkSeccompProfile(cfg *config.Config) []PreflightWarning {
    profiles := []string{cfg.Process.SeccompProfile}
    if cfg.ExperimentalFlags().Zygote {
        profiles = append(profiles, cfg.Process.ZygoteSeccompProfile)
        if cfg.ExperimentalFlags().KSM {
            profiles = append(profiles, cfg.Process.ZygoteKSMSeccompProfile)
        }
    }
    var warns []PreflightWarning
    for _, p := range profiles {
        // existing readable-file-with-BPF-shape check
    }
    return warns
}
```

**Helper `.so` bind mount** — add to the bwrap argument list
when `spec.Zygote` is true:

```
--ro-bind {BundleServerPath}/.zygote/zygote_helper.so /blockyard/zygote_helper.so
```

The process-backend spawn also sets `BLOCKYARD_HELPER_PATH` and
(when KSM enabled) `BLOCKYARD_KSM_ENABLED=1` in the child
environment, same as the Docker backend.

### Step 13: Backend `Forking.StatsClient` method

Phase 4-5's `Forking` interface has three methods: `Fork`,
`KillChild`, `ChildExits`. Phase 4-6 adds a fourth:

```go
// StatsClient resolves a workerID to its live zygote control
// client for metrics polling (decision D8). Returns nil if the
// worker is unknown or not in zygote mode. The Manager's metrics
// goroutine calls this on each tick; the backend continues to
// own the client's lifetime.
StatsClient(workerID string) *zygotectl.Client
```

Both `internal/backend/docker/forking.go` and
`internal/backend/process/forking.go` implement it by resolving
`workerID` to `ws.fork.client`:

```go
func (d *DockerBackend) StatsClient(workerID string) *zygotectl.Client {
    d.mu.Lock()
    defer d.mu.Unlock()
    ws, ok := d.workers[workerID]
    if !ok || ws.fork == nil {
        return nil
    }
    return ws.fork.client
}
```

Phase-4-5's decision #5 split `internal/zygotectl/` from
`internal/zygote/` specifically to enable this: `backend` can
import `zygotectl` (for `*Client` on the `Forking` interface)
without cycling, because `zygotectl` doesn't import `backend`.

### Step 14: `zygote.Manager` metrics-poll goroutine

Phase 4-5's `zygote.Manager` owns `bySession` bookkeeping, exit
handling, and the sweep loop. Phase 4-6 adds:

1. **`statsClient func(workerID string) *zygotectl.Client`** —
   set from `ManagerConfig.StatsClient` (supplied by the backend
   at construction time). When nil, metrics polling is skipped
   entirely (non-zygote backends or test doubles).
2. **`metricsInterval time.Duration`** — poll cadence, from
   `ManagerConfig.MetricsInterval` (wired to
   `proxy.ZygoteMetricsInterval`).
3. **`workersActive map[string]struct{}`** — updated by two new
   methods `NotifyWorkerAlive(workerID)` and
   `NotifyWorkerGone(workerID)`, called by the backend's Spawn
   and Stop paths.
4. **`lastStats map[string]zygotectl.Stats`** — most recent poll
   result per worker, exposed via `LastStats(workerID)`. Used by
   the worker detail page to render without a protocol round-trip.
5. **`metricsLoop()` goroutine** — polls `STATS` from every
   active worker on a `metricsInterval` tick, updates
   `lastStats`, updates the labeled Prometheus gauges, and logs
   failures at debug level without blocking the loop.

Full Go code added to `internal/zygote/manager.go`:

```go
// metricsLoop polls STATS from every live zygote on the metrics
// tick and updates labeled Prometheus gauges. Runs only when
// ManagerConfig.StatsClient is set — i.e. when the backend
// supports dynamic stats collection. Missing or failing STATS
// calls are logged at debug level but do not block the loop;
// the next tick tries again. See decision D8.
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

// LastStats returns the most recent STATS poll result for a
// worker, or the zero value if no successful poll has landed.
// Used by the worker detail page.
func (m *Manager) LastStats(workerID string) zygotectl.Stats {
    m.mu.Lock()
    defer m.mu.Unlock()
    return m.lastStats[workerID]
}
```

`ManagerConfig` extension:

```go
type ManagerConfig struct {
    SweepInterval   time.Duration
    MetricsInterval time.Duration // NEW — default 30s
    StatsClient     func(workerID string) *zygotectl.Client // NEW
    AppIDFor        func(workerID string) string            // NEW
}
```

`NewManager` wires them up and starts `metricsLoop` alongside
`exitLoop` and `sweepLoop`:

```go
if cfg.MetricsInterval <= 0 {
    cfg.MetricsInterval = 30 * time.Second
}
m := &Manager{
    // ...existing fields...
    metricsInterval: cfg.MetricsInterval,
    statsClient:     cfg.StatsClient,
    appIDFor:        cfg.AppIDFor,
    workersActive:   make(map[string]struct{}),
    lastStats:       make(map[string]zygotectl.Stats),
    stop:            make(chan struct{}),
}
go m.exitLoop()
go m.sweepLoop()
if m.statsClient != nil {
    go m.metricsLoop()
}
return m
```

### Step 15: Prometheus metrics

Three new metrics in `internal/telemetry/metrics.go` alongside
phase 4-5's existing gauges:

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

```go
// cmd/blockyard/main.go
if cfg.ExperimentalFlags().KSM {
    go hostKSMScraperLoop(ctx, cfg.Proxy.ZygoteMetricsInterval.Duration)
}

func hostKSMScraperLoop(ctx context.Context, interval time.Duration) {
    t := time.NewTicker(interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            data, err := os.ReadFile("/sys/kernel/mm/ksm/pages_sharing")
            if err != nil {
                continue
            }
            n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
            telemetry.HostKSMPagesSharing.Set(float64(n))
        }
    }
}
```

### Step 16: KSM preflight checks

Phase 3-7 introduced `Backend.Preflight()`. Phase 4-6 adds KSM
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

Both warnings are non-fatal; the zygote model still works with
no KSM recovery (the memory model degrades to the PSOCK-equivalent
steady state). See decision D7 for the operational impact of
`pages_to_scan` being too low.

### Step 17: API / CLI / UI for `ksm`

Mirror phase 4-5's `zygote` surface.

**API** — extend `updateAppRequest` in `internal/api/apps.go`:

```go
type updateAppRequest struct {
    // ...existing fields including Zygote from phase 4-5...
    KSM *bool `json:"ksm"`
}
```

Validation in `UpdateApp()` runs against the effective end-state
(same pattern phase 4-5 introduced for `zygote`):

```go
effectiveZygote := app.Zygote
if body.Zygote != nil {
    effectiveZygote = *body.Zygote
}
effectiveKSM := app.KSM
if body.KSM != nil {
    effectiveKSM = *body.KSM
}
transitioningToKSM := body.KSM != nil && *body.KSM && !app.KSM

flags := srv.Config.ExperimentalFlags()
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

The order matters: compute effective end-state first, then
validate. A single PATCH that sets both `zygote=true` and
`ksm=true` must succeed — the new effective state has both
flags true.

Add `KSM` to `appResponseV2()` in `internal/api/runtime.go` and
to `swagger_types.go`.

**Server capabilities endpoint** (introduced in phase 4-5) gains
a second field:

```json
{
  "experimental": {
    "zygote": true,
    "ksm": false
  }
}
```

**CLI** — extend `by scale` in `cmd/by/scale.go`:

```go
cmd.Flags().Bool("ksm", false,
    "Enable KSM memory merging in zygote workers (experimental, requires --zygote and experimental.ksm in server config)")

if cmd.Flags().Changed("ksm") {
    v, _ := cmd.Flags().GetBool("ksm")
    body["ksm"] = v
}
```

**UI** — admin-only toggle in `tab_settings.html`, alongside the
phase-4-5 zygote toggle:

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

The `disabled` state fires when either server-wide `experimental.ksm`
is off or the per-app `zygote` flag is off, matching the API's
validation rules.

### Step 18: Config — `ZygoteMetricsInterval`

One new field on `ProxyConfig` — the cadence at which
`zygote.Manager`'s metrics goroutine polls `STATS`:

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

Wired into `zygote.NewManager` from `cmd/blockyard/main.go`
(this replaces the phase-4-5 call site):

```go
flags := srv.Config.ExperimentalFlags()
if flags.Zygote {
    if forking, ok := backend.(backend.Forking); ok {
        srv.Zygotes = zygote.NewManager(
            forking,
            srv.Sessions,
            zygote.ManagerConfig{
                SweepInterval:   srv.Config.Proxy.AutoscalerInterval.Duration,
                MetricsInterval: srv.Config.Proxy.ZygoteMetricsInterval.Duration,
                StatsClient:     forking.StatsClient,
                AppIDFor: func(wid string) string {
                    return srv.WorkerApp(wid)
                },
            },
        )
    }
}
```

`StatsClient` is an interface method on `backend.Forking`, so the
method reference resolves cleanly without a cast.

### Step 19: Tests

#### Unit tests

**`internal/zygotectl/control_test.go`** (extends phase 4-5 tests):

```go
func TestClient_Stats(t *testing.T)
// Test server responds to STATS with a canned multi-line block
// terminated by END. Client.Stats(ctx) returns a Stats value
// with the parsed fields populated correctly.

func TestClient_StatsInterleavedWithChildExit(t *testing.T)
// Test server pushes a CHILDEXIT in the middle of a STATS
// response. Verify the CHILDEXIT is dispatched to Exits and
// the STATS response still parses correctly once END arrives.
// Covers the multi-line reader accumulation path against the
// asynchronous push path.

func TestClient_StatsUnknownKeys(t *testing.T)
// STATS response includes future keys. Client ignores them
// without erroring and populates the known keys correctly.

func TestClient_InfoIncludesKSMAndSandboxStatus(t *testing.T)
// INFO response includes ksm_status, ksm_errno, sandbox_status
// fields. Verify they land on the parsed Info struct with the
// expected values.
```

**`internal/zygote/manager_test.go`** (extends phase 4-5 tests):

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

func TestManager_MetricsLoopSkipsWhenStatsClientNil(t *testing.T)
// NewManager with StatsClient=nil. Verify metricsLoop is not
// started (construction sets a flag, check via a test seam).
```

**KSM helper unit test** (new,
`internal/zygotectl/helper_test.go`):

```go
func TestEnableKSM_GracefulFallback(t *testing.T)
// Compile the helper .so. dlopen it via a thin Go test wrapper.
// Call enable_ksm with a mocked prctl (LD_PRELOAD of a stub that
// returns EINVAL). Verify the result code is 22 (EINVAL).
// Repeat with EPERM → 1 (EPERM), success → 0.
```

**Sandbox helper unit test** — the atomic
`apply_post_fork_sandbox` function is integration-test territory
(needs real fork, namespaces, etc.), so unit tests cover only
the guard flag and the error code enumeration:

```go
func TestSandbox_ErrorCodeEnumeration(t *testing.T)
// Build the helper. Parse the generated header for
// post_fork_seccomp_bpf + len. Verify len > 0 and non-nil.
// Verify ERR_OOM_ADJ..ERR_SECCOMP constants are unique and
// match the R-side handler switch cases.
```

**Seccomp profile validation**
(`docker/seccomp_test.go` extensions):

```go
func TestPostForkProfile_DeniesUnshare(t *testing.T)
// Parse docker/blockyard-post-fork-seccomp.json. Verify
// unshare, clone3, setns, mount, umount2, pivot_root, chroot
// are explicitly denied.

func TestPostForkProfile_AllowsBasicRuntime(t *testing.T)
// Verify read, write, openat, mmap, brk, futex, epoll_wait,
// sendto, recvfrom, accept4, close are allowed.

func TestPostForkProfile_DeniesKSMprctl(t *testing.T)
// Verify prctl(PR_SET_MEMORY_MERGE = 67) is explicitly denied.
// KSM is enabled in the parent zygote; children never need to
// call it themselves, so the filter denies it to prevent
// unilateral opt-in by user code.

func TestBwrapZygoteProfile_AllowsNarrowUnshare(t *testing.T)
// Parse docker/blockyard-bwrap-zygote-seccomp.json. Verify
// clone/unshare with CLONE_NEWUSER|CLONE_NEWNS is allowed and
// that other CLONE_NEW* flags are denied by the arg match.
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

// Migration roundtrip is covered by the existing TestMigrateRoundtrip
// from phase 3-1.
```

#### Integration tests

**`internal/backend/docker/forking_integration_test.go`**
extensions (tagged `docker_test`):

```go
func TestDockerForking_SandboxPrivateTmp(t *testing.T)
// Spawn a zygote with a bundle that, on first request, writes a
// file to /tmp/marker with the session ID. Fork two children
// back-to-back with different session IDs. Verify each child sees
// only its own /tmp/marker (not the sibling's).

func TestDockerForking_SandboxSeccompActive(t *testing.T)
// Spawn a zygote. Fork a child with a bundle that tries to call
// mount() via a small C wrapper loaded from the bundle. Verify
// the call fails with EPERM and the child continues serving.
// Confirms the post-fork seccomp filter is active and denies
// the expected syscall.

func TestDockerForking_SandboxCLONE_NEWUSER_Works(t *testing.T)
// Spawn a zygote on a Linux host. Fork a child. Verify the
// child's /proc/self/uid_map is a non-empty mapping — confirming
// that the post-fork unshare(CLONE_NEWUSER) succeeded.

func TestDockerForking_PRCTL_MEMORY_MERGE_AllowedBySeccomp(t *testing.T)
// Spawn a zygote with KSM enabled. Verify enable_ksm()
// returned 0 (INFO.ksm_status == "enabled"). Also verify by
// reading /sys/kernel/mm/ksm/pages_sharing before and after
// — if > 0 after preload, the syscall landed and merging is
// happening. Skip if /sys/kernel/mm/ksm/run == 0 on the test
// host.

func TestDockerForking_SandboxedChildCannotRemountTmp(t *testing.T)
// Fork a child. Verify that trying to mount() a second tmpfs
// at /tmp fails with EPERM (seccomp filter).

func TestDockerForking_KSMEffectiveness(t *testing.T)
// Spawn a zygote with KSM enabled. Fork two children. In each,
// trigger gc(full=TRUE) via a control-protocol FORK with a
// special bundle that calls gc() in its initialisation. Poll
// STATS every 500ms until ksm_merging_pages_total > 0, timeout
// 30s. Fail on literal zero with the captured value logged.
// Skip if /sys/kernel/mm/ksm/run == 0 on the test host.
```

**`internal/backend/process/forking_integration_test.go`**
extensions (tagged `process_test`):

```go
func TestProcessForking_SandboxPrivateTmp(t *testing.T)
// Process-backend analogue of the Docker test above. Verifies
// the zygote-variant bwrap profile is in effect and allows the
// post-fork unshare to succeed.

func TestProcessForking_BwrapSeccompVariantSelected(t *testing.T)
// Spawn a non-zygote worker and a zygote worker back to back.
// Verify bwrap's seccomp-profile argument differs between
// them — the non-zygote path uses seccomp.bpf, the zygote
// path uses seccomp-zygote.bpf.

func TestProcessForking_KSMEffectiveness(t *testing.T)
// Same shape as the Docker KSM-effectiveness test, but
// against a process-backend zygote. Skipped when bwrap is
// unavailable or /sys/kernel/mm/ksm/run == 0.
```

**Shared process-backend / Docker-backend sandbox validation**
(tagged `sandbox_test`):

```go
func TestApplyPostForkSandbox_AllStepsExecuted(t *testing.T)
// Compile the helper, dlopen it via a Go test wrapper, fork
// with exec("<test binary>" --sandbox-child) and verify from
// the child side that every step ran: oom_score_adj reads as
// 1000, NoNewPrivs is set in /proc/self/status, uid_map is
// non-empty, /tmp is a fresh tmpfs, RLIMIT_NPROC is 64,
// capabilities are all zero, seccomp Mode is 2 (FILTER).

func TestApplyPostForkSandbox_IdempotentOnSecondCall(t *testing.T)
// Call apply_post_fork_sandbox twice in a row from a forked
// test helper. Verify the second call returns 0 immediately
// and does not re-run any syscall (no observable side
// effect — seccomp filter is not installed twice, etc.).

func TestApplyPostForkSandbox_FailureCodesRoundTrip(t *testing.T)
// Inject mocked failures (via LD_PRELOAD stubs) for each
// syscall in the chain. Verify the result code matches
// ERR_OOM_ADJ..ERR_SECCOMP and that sandbox_applied stays 0.
```

#### Manual verification

A phase-landing checklist in `docs/design/v4/phase-4-6-verify.md`
covering the cases CI cannot reach cheaply:

- Ubuntu 24.04 host, KSM enabled, zygote + KSM app: verify
  `/sys/kernel/mm/ksm/pages_sharing` increases after forking
  several children and triggering GC.
- Ubuntu 22.04 host (kernel 5.15): verify KSM opt-in gracefully
  reports `unsupported` in `INFO.ksm_status`.
- macOS host with Docker Desktop: verify the Docker backend's
  zygote spawn works (Docker Desktop runs a Linux VM so
  kernel features behave Linux-like).
- Multi-tenant scenario: fork four children with distinct
  session IDs, confirm each sees only its own `/tmp` and its
  own listening port.

---

## Files changed

| File | Action | Summary |
|------|--------|---------|
| `internal/db/db.go` | update | `KSM` on `AppRow` and `AppUpdate`, UPDATE SQL extension |
| `internal/config/config.go` | update | `KSM` on `ExperimentalConfig`, nil-safe accessor unchanged, load-time validation for ksm-requires-zygote |
| `internal/config/config.go` | update | `ZygoteMetricsInterval` on `ProxyConfig`, `ZygoteSeccompProfile` and `ZygoteKSMSeccompProfile` on `ProcessConfig` |
| `internal/backend/backend.go` | update | `StatsClient(workerID) *zygotectl.Client` method on `Forking`; `KSM bool` on `WorkerSpec` |
| `internal/backend/docker/docker.go` | update | Spawn branch for zygote workers: `--security-opt seccomp={path}` (base or ksm variant), `--security-opt apparmor=unconfined` (host-conditional), helper `.so` bind mount, `BLOCKYARD_HELPER_PATH` env, `BLOCKYARD_KSM_ENABLED` env when KSM on, `OMP_NUM_THREADS=1`, `MKL_NUM_THREADS=1` |
| `internal/backend/docker/forking.go` | update | `StatsClient(workerID)` method, `NotifyWorkerAlive/Gone` calls in Spawn/Stop |
| `internal/backend/process/process.go` | update | Spawn: seccomp profile selection by `spec.Zygote`/`spec.KSM`, helper `.so` bind mount, same env additions as Docker |
| `internal/backend/process/forking.go` | update | `StatsClient(workerID)` method, `NotifyWorkerAlive/Gone` calls in Spawn/Stop |
| `internal/backend/process/preflight.go` | update | Validate `ZygoteSeccompProfile` and `ZygoteKSMSeccompProfile` when zygote enabled |
| `internal/zygotectl/control.go` | update | `Stats` struct, `Client.Stats()` method, `requestMulti()` reader, `Info` gains `KSMStatus`, `KSMErrno`, `SandboxStatus` |
| `internal/zygotectl/embed.go` | update | Add `HelperSource` embed for `zygote_helper.c` |
| `internal/zygotectl/zygote.R` | update | Post-fork sandbox hook, helper load (post-preload + unlink), KSM opt-in, `STATS` handler, `INFO` extension, up-front `cmpfile`+`loadcmp` preload |
| `internal/zygote/manager.go` | update | `metricsLoop`, `pollMetricsOnce`, `NotifyWorkerAlive`, `NotifyWorkerGone`, `LastStats`; `ManagerConfig` extensions |
| `internal/api/apps.go` | update | `ksm` field on `updateAppRequest`, effective-state validation (requires `zygote` end-state, requires `experimental.ksm` for new transitions) |
| `internal/api/server.go` | update | Capabilities endpoint exposes `experimental.ksm` |
| `internal/api/runtime.go` | update | `ksm` in `appResponseV2()` |
| `internal/api/swagger_types.go` | update | `ksm` field and capabilities shape |
| `internal/ui/templates/tab_settings.html` | update | KSM toggle, admin-gated, server-capabilities-gated; worker detail shows `ksm_status`, `sandbox_status` from INFO, and `LastStats` |
| `internal/telemetry/metrics.go` | update | `ZygoteKSMMergingPages`, `ZygoteKSMMergingPagesTotal`, `HostKSMPagesSharing` gauges |
| `cmd/by/scale.go` | update | `--ksm` flag |
| `cmd/blockyard/main.go` | update | Write `HelperSO` to disk at startup, host KSM scraper goroutine, write outer seccomp profile(s) for Docker backend, pass `MetricsInterval`/`StatsClient`/`AppIDFor` to `NewManager` |
| `cmd/seccomp-compile/main.go` | update | `-format c-header` mode emitting `const unsigned char <sym>[]` + `_len` for embedding BPF in C |
| `docker/server-process.Dockerfile` | update | Compile and copy post-fork and bwrap-zygote BPF blobs, plus outer seccomp KSM variant |
| `docker/server-everything.Dockerfile` | update | Same as server-process |
| `Makefile` | update | Helper `.so` build rule (per arch), per-profile `regen-seccomp` targets for the new profiles |

## New files

| File | Purpose |
|------|---------|
| `internal/db/migrations/sqlite/004_ksm.up.sql` | Migration up (SQLite) |
| `internal/db/migrations/sqlite/004_ksm.down.sql` | Migration down (SQLite) — table-recreate per phase 3-1 rules |
| `internal/db/migrations/postgres/004_ksm.up.sql` | Migration up (PostgreSQL) |
| `internal/db/migrations/postgres/004_ksm.down.sql` | Migration down (PostgreSQL) |
| `internal/zygotectl/zygote_helper.c` | Consolidated C helper with `enable_ksm` + `apply_post_fork_sandbox` |
| `internal/zygotectl/zygote_helper_linux_amd64.so` | Build artifact (committed per-arch compiled helper) |
| `internal/zygotectl/zygote_helper_linux_arm64.so` | Build artifact (committed per-arch compiled helper) |
| `internal/zygotectl/embed_linux_amd64.go` | Per-arch `HelperSO` embed |
| `internal/zygotectl/embed_linux_arm64.go` | Per-arch `HelperSO` embed |
| `internal/zygotectl/embed_nonlinux.go` | Empty `HelperSO` for non-Linux builds |
| `internal/zygotectl/helper_test.go` | Helper unit tests (enable_ksm fallback, error code enumeration) |
| `docker/blockyard-post-fork-seccomp.json` | Post-fork seccomp profile (generated output, committed) |
| `docker/blockyard-post-fork-seccomp-overlay.json` | Hand-edited overlay |
| `docker/blockyard-bwrap-zygote-seccomp.json` | Zygote-variant bwrap profile (generated output, committed) |
| `docker/blockyard-bwrap-zygote-seccomp-overlay.json` | Hand-edited overlay |
| `docker/blockyard-seccomp-ksm-overlay.json` | KSM relaxation overlay (allows `prctl(PR_SET_MEMORY_MERGE)`) |
| `docs/design/v4/phase-4-6-verify.md` | Phase-landing manual verification checklist |
| `internal/backend/docker/sandbox_integration_test.go` | Docker sandbox integration tests |
| `internal/backend/process/sandbox_integration_test.go` | Process sandbox integration tests |

---

## Design decisions

### D1. Consolidated C helper with exactly two exported functions, not seven or eight

R's `.C` interface exposes every symbol in a `dyn.load`'d shared
library to arbitrary R code in the same process. The zygote
loads the bundle's `global.R` / `app.R` during preload
(pre-fork), and after fork the children run `runApp()` which
evaluates user code on every request. Both phases can call any
loaded `.C` symbol. If the helper exported eight separate
symbols — `pin_oom_score`, `set_no_new_privs`,
`unshare_namespaces`, `mount_private_tmp`, `set_rlimits`,
`drop_capabilities`, `apply_seccomp`, `enable_ksm` — user R code
could call them in arbitrary order, with arbitrary arguments,
and potentially leave the child in a partially-sandboxed state
where some primitives were reachable but seccomp hadn't landed
yet.

Most of the underlying syscalls are **one-way ratchets** —
`setrlimit` can only lower for unprivileged callers, `capset`
can only drop, seccomp filters compose with AND — so the worst
user code could do by calling them *after* the sandbox lands is
make the sandbox tighter. But the truly dangerous primitives
(`unshare(CLONE_NEWUSER)`, `mount`) could be called by hostile
bundle code in the zygote during preload, where no sandbox is
yet in place. And an eight-function API is a much richer
attack surface for "future-us makes a mistake in the calling
R code."

**The fix: collapse to two entry points, both with strong
defensive properties:**

- **`enable_ksm(int *result)`** — called once in the parent
  zygote, gated on `BLOCKYARD_KSM_ENABLED=1`. Opts the mm_struct
  into the KSM merge pool. Idempotent — the kernel returns 0
  on the second call. The only way bundle code could misuse it
  is by calling it during preload when KSM is supposedly
  disabled, but the **outer-container seccomp profile** (D4)
  is what determines whether `prctl(PR_SET_MEMORY_MERGE)` is
  allowed at all: we ship it allowed only in image variants
  built for KSM. A hostile bundle call on a non-KSM image
  gets EPERM from the outer profile; on a KSM image, it's a
  redundant no-op.

- **`apply_post_fork_sandbox(int *result)`** — called once in
  the child expression of `mcparallel`, as the first R statement
  after fork and before any user code. Performs the entire post-fork
  sandbox setup in C, in the correct order, with a static
  guard that short-circuits subsequent calls. After it returns
  successfully, every dangerous syscall it wraps is blocked by
  the installed seccomp filter. User R code calling
  `.C("apply_post_fork_sandbox")` a second time hits the guard
  before any syscall runs; even if the guard were somehow
  bypassed, the seccomp filter would return EPERM on each call.
  The R-visible surface is one symbol.

This removes the "cherry-pick individual primitives" attack
surface entirely. The ordering of the seven operations is
encoded in C and cannot be gotten wrong by an R-side mistake.
The static guard makes the function effectively call-once; the
seccomp filter provides defense-in-depth against any path that
bypasses the guard.

**Why a single file, not split into two helpers.** Both
functions are small, and they share compilation plumbing (same
Makefile rule, same per-arch embed files, same bind-mount into
the sandbox). Splitting into `zygote_helper.c` + `zygote_sandbox.c`
would double the embed surface without reducing attack surface:
the KSM helper is already gated at runtime by
`BLOCKYARD_KSM_ENABLED`, so the `enable_ksm` symbol is simply
not called when KSM is disabled. Having both symbols in the
same `.so` does not introduce additional risk. One file is
simpler to reason about and audit.

**Additional hardening: load after preload + unlink.** Phase
4-6's `zygote.R` calls `dyn.load(helper_path)` *after*
`preload_bundle()` finishes. At load time the bundle code has
already run and cannot reach the helper symbols. Immediately
after the load, the script calls `unlink(helper_path)` — the
`.so` is mmapped into the process (symbols remain resolvable)
but the filesystem path is gone. Bundle code that tries to
`dyn.load` the helper by path later gets ENOENT. Both defenses
are belt-and-suspenders on top of the seccomp filter.

### D2. Atomic ordering of the post-fork sandbox setup

The order of the seven steps inside `apply_post_fork_sandbox`
is not arbitrary — several operations depend on earlier ones
and some cannot be safely reordered. The chosen order:

1. **`oom_score_adj = 1000`** — first, because from this point
   on the child is OOM-reapable and any subsequent hang or
   failure can be killed cleanly by the kernel. If any later
   step fails, the `mcexit(1L)` path in R disposes of the
   child quickly.
2. **`PR_SET_NO_NEW_PRIVS`** — before any privilege-transition
   step because seccomp's filter install requires
   `NO_NEW_PRIVS` (or `CAP_SYS_ADMIN`, which we don't have)
   and any later `exec()` from bundle code must not be able
   to re-gain privileges via setuid binaries.
3. **`unshare(CLONE_NEWUSER | CLONE_NEWNS)`** — gains the
   capability set inside the new user namespace (needed for
   the tmpfs mount in step 4) and creates a private mount
   namespace (so the tmpfs mount doesn't leak into the
   parent's).
4. **`mount tmpfs at /tmp`** — requires `CAP_SYS_ADMIN` in the
   current user namespace, which the unshare in step 3 just
   granted. Must be done after the userns unshare but before
   capabilities are dropped in step 6.
5. **`setrlimit(RLIMIT_NPROC, 64)`** — fork-bomb guard. Does
   not need to be in any particular slot except that it must
   precede step 7 (since `setrlimit` is in the seccomp deny
   list after the filter installs). See D5 for why this is
   the only rlimit.
6. **`capset()` drop all** — zeroes the capability set inside
   the new user namespace. Must be after step 4 (tmpfs mount
   needed `CAP_SYS_ADMIN`) and before step 7 (post-fork
   seccomp filter blocks `capset`).
7. **Post-fork seccomp filter install** — last, so that all
   the setup syscalls above have already run. After this
   point, every dangerous syscall we just used is blocked for
   the remainder of the child's lifetime.

The ordering is fixed in C, not R — R only signals "do it".
That removes an entire class of R-side bugs where someone
reorders the calls and breaks the dependency chain. Tests
verify each step ran (see Step 19's
`TestApplyPostForkSandbox_AllStepsExecuted`).

### D3. Two-level opt-in for KSM: server-wide `experimental.ksm` AND per-app `ksm` column, independent of the zygote opt-in

Phase 4-5 established the two-level opt-in shape for the zygote
model (`experimental.zygote` server flag + `apps.zygote` per-app
column). Phase 4-6 repeats the pattern for KSM with one
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

**The full gate matrix across phases 4-5 and 4-6:**

| Gate                     | Location     | Phase  | Controls                                            |
|--------------------------|--------------|--------|-----------------------------------------------------|
| `experimental.zygote`    | server toml  | 4-5    | Whether the zygote code path runs at all            |
| `experimental.ksm`       | server toml  | 4-6    | Whether `PR_SET_MEMORY_MERGE` is ever called        |
| `apps.zygote`            | db column    | 4-5    | Whether a specific app uses the zygote model        |
| `apps.ksm`               | db column    | 4-6    | Whether a specific app's zygote enables KSM         |

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
never runs.

**UI gating:** The settings tab reads the effective server
config via the capabilities endpoint (phase 4-5) and disables
the KSM toggle when `experimental.ksm` is off. Tooltip on the
disabled toggle explains "set `experimental.ksm = true` in the
server config to enable this feature."

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
  explicit flags compose more cleanly. Rejected for phase 4-6;
  remains an option for future consolidation.

### D4. Outer-container seccomp gates `prctl(PR_SET_MEMORY_MERGE)` via profile variant, not runtime toggle

The outer-container seccomp profile (Docker backend) and the
bwrap-zygote profile (process backend) both need to allow
`prctl(PR_SET_MEMORY_MERGE)` for the helper's `enable_ksm` to
run. But we don't want to allow it on image variants where KSM
is not part of the supported feature set — operators running
the non-KSM image should have no code path that could enable
KSM, even if the in-memory helper were somehow tricked into
calling it.

**The approach: ship two variants of each profile.** The base
profile denies the syscall; the KSM variant adds an allow rule
via an overlay. Image variants that support KSM
(`blockyard-process`, `blockyard`) ship both files; the base
(`blockyard-docker`, ...) ships only the base profile. At
server startup, the Docker backend picks the right file based
on `experimental.ksm`; the process backend picks the right BPF
blob based on `spec.KSM` per worker.

This is structurally different from a runtime toggle because
the profile content itself differs — not a flag, not an
environment variable, not an orchestration decision at spawn
time. The trust chain ends with "which file did the image
ship?" Operators who want to audit whether KSM is allowed on
their host can `cat /etc/blockyard/seccomp.json | jq
'.syscalls[] | select(.names | index("prctl"))'` and verify
what they see.

**Why a single JSON file with a conditional overlay instead of
a runtime arg match.** The phase-3-8 overlay mechanism already
supports this shape cleanly — `cmd/seccomp-merge` composes a
base profile with any number of overlays, each overlay is a
separate committed file, and the output is deterministic. Adding
the KSM variant is one more invocation of the same tool on the
same input format. Alternative: hand-write a dynamic `prctl` arg
match into the base profile with a runtime switch — complex,
harder to audit, couples the runtime to the profile author's
intent.

### D5. Only `RLIMIT_NPROC` is applied post-fork; `RLIMIT_AS` and `RLIMIT_CPU` are deliberately not

Phase 4-6's post-fork hook could in principle apply three
resource limits: `RLIMIT_AS` (virtual memory address space),
`RLIMIT_CPU` (CPU seconds), and `RLIMIT_NPROC` (processes per
user). The draft originally listed all three. The landed
design drops `RLIMIT_AS` and `RLIMIT_CPU` entirely and applies
only `RLIMIT_NPROC` with a fixed default of 64.

**`RLIMIT_AS`: R's allocator fights it.** R aggressively
over-reserves address space at startup — it grabs virtual
ranges before committing physical memory, which is fine under
cgroup memory accounting but hostile to `RLIMIT_AS` because
`RLIMIT_AS` counts address space, not RSS. A `RLIMIT_AS` of
"2 GiB" routinely fails at arbitrary later points inside R's
allocator, producing aborts rather than graceful SIGKILL.
Operators also cannot know the "right value" ahead of time
without per-app working-set measurements, which phase 4-6 does
not ship. Operationally close to useless.

**`RLIMIT_CPU`: wrong shape for Shiny sessions.** Sessions
legitimately run for hours while users read dashboards and
sporadically interact with them. CPU-seconds as a bound would
SIGXCPU a long-running session at arbitrary times regardless
of whether it was active. The right shape for CPU containment
is "CPU share under contention," which is a cgroup property,
not a per-process rlimit. Cgroup CPU shares are already
enforced at the worker level on Docker (per phase 4-5 decision
#6) and unbounded on the process backend (per phase 3-7's
"no per-worker cgroups" stance).

**`RLIMIT_NPROC`: fork-bomb guard, not a resource budget.**
This one is different. It's not a memory or CPU bound — it's
protection against a runaway session that `parallel::mclapply`s
itself into oblivion, or a malicious user who triggers a
`system("find / ...")`. The default value is easy to pick
conservatively (64 is plenty for any legitimate Shiny session
and tight enough to catch a runaway) and doesn't depend on
workload characteristics. It works in all deployment shapes
because it's a process attribute, not a cgroup. Genuinely
useful, not trying to replace cgroups.

**What this means for the design.** No new config fields, no
per-app columns, no migration, no CLI surface for rlimits. One
hardcoded `setrlimit(RLIMIT_NPROC, 64)` call inside
`apply_post_fork_sandbox`. Per-session memory and CPU bounds
remain governed by the worker-level cgroup (Docker) or by
nothing (process). If operators later want per-session memory
bounds on the Docker backend, the right mechanism is
`ContainerUpdate` on a per-child cgroup — blocked today by
cgroup v2 delegation availability, tracked as Deferred #3.

### D6. Process backend ships a zygote-variant bwrap seccomp profile

Phase 3-8's bwrap seccomp profile explicitly **re-tightens**
`clone`/`unshare` because "workers should not be creating
further namespaces once inside the sandbox." Phase 4-6's
post-fork sandboxing requires the child to call
`unshare(CLONE_NEWUSER | CLONE_NEWNS)` from inside that same
bwrap sandbox. The two phases have directly conflicting
requirements for the process-backend bwrap profile.

**Resolution: ship a second profile for zygote workers only.**
The non-zygote profile stays as-is; zygote workers get a
variant that permits the narrow subset of namespace creation
the post-fork hook needs. The process backend picks between
them at spawn time based on `spec.Zygote` (and `spec.KSM` for
the KSM variant). Four combinations live in the image:
`seccomp.bpf`, `seccomp-zygote.bpf`, and
`seccomp-zygote-ksm.bpf`. The non-zygote profile keeps phase
3-8's stricter posture unchanged.

**Why not a single relaxed profile for everyone.** Option
considered during design: drop phase 3-8's re-tightening
entirely, ship one relaxed profile for all workers. Rejected
because phase 3-8's rationale ("workers should not be creating
further namespaces") is defensible on its own terms — once
you're inside a bwrap sandbox, non-zygote workers have no
legitimate use case for namespace creation. Relaxing the
profile for them couples unrelated workers' isolation posture
to zygote's needs.

**Why not skip post-fork unshare on the process backend.**
Second option considered: document that process-backend zygote
children share `/tmp` with their siblings; multi-tenant is
Docker-only. Rejected because the zygote model's two
unconditional benefits advertised in phase 4-5 are "startup
latency" and "per-session isolation" — per-session isolation is
load-bearing, and "it works on one backend but not the other"
would undermine the value proposition. The cost of shipping
one extra profile is small; the benefit is feature parity
across backends.

**Narrow relaxation scope.** The zygote-variant overlay permits
`clone`/`clone3`/`unshare` only with `CLONE_NEWUSER | CLONE_NEWNS`
in argument 0, via the existing `SCMP_CMP_MASKED_EQ` operator.
Other `CLONE_NEW*` flags (net, pid, ipc, uts, cgroup) remain
denied — the post-fork hook needs the user and mount namespaces,
nothing else. This keeps the relaxation as surgical as possible.

### D7. KSM enabled via `prctl(PR_SET_MEMORY_MERGE)` from an embedded C helper, gated behind the independent two-level opt-in

R's generational GC writes mark bits to SEXP headers during
level-2 collections, which dirties every page containing a live
SEXP and breaks copy-on-write sharing between forked children.
Without a recovery mechanism, the zygote model's memory-sharing
story decays to "eventually equivalent to PSOCK workers" after
children have done a few GC cycles.

KSM is an **independent opt-in** from the zygote model itself —
see decision D3. Operators must set both `experimental.ksm =
true` at the server level and `apps.ksm = true` on the
individual app before any KSM-related behaviour activates. The
zygote model's startup-latency and isolation benefits do not
depend on KSM, so operators who don't want KSM (pre-6.4 kernels,
multi-tenant apps with side-channel concerns — see Deferred #1,
hosts they don't control) can still use the zygote model fully.

Linux 6.4 added `PR_SET_MEMORY_MERGE`, a process-level KSM
opt-in whose effect is that the kernel's `ksmd` daemon scans
the process's anonymous memory for pages matching other
processes' pages and merges them into shared physical frames.
The intuition is that after R's GC dirties pages in child A and
child B, the two children end up in *substantially similar*
states — same packages loaded, same live objects, same heap
layout inherited from the zygote — so `ksmd` finds pages that
aren't touched by per-child mutation (REFCNT bumps, attribute
writes, ALTREP materialization) and re-merges them. KSM recovers
some, but not all, of the sharing that GC breaks. The exact
recovery rate is workload-dependent and currently unmeasured
for R workloads — Meta reports ~6GB saved per 64GB machine on
Instagram (CPython controller + ~32 workers), but Python's
object model is structurally different (refcount in header, no
mark bits, no JIT in CPython), so that number is an upper-bound
anchor, not a prediction. Phase 4-6 ships the KSM opt-in plus
observability (D8) so operators can measure actual recovery in
their own deployments; the design does not commit to a specific
memory-savings figure.

**KSM effectiveness depends on D9.** R's JIT compiles closure
bodies in-place on first call, mutating closure SEXP headers
and thereby dirtying every page that holds user code. Without
D9's up-front bundle compilation, the JIT path alone would be
enough to keep KSM merge rates near zero for bundle code. The
two decisions are effectively a package: KSM provides the merge
mechanism, up-front compilation ensures there are mergeable
pages for it to find.

**Why the helper has to live on the R side.** `prctl` is
self-directed, and `PR_SET_MEMORY_MERGE` is stored in the
process's `mm_struct`. The kernel replaces `mm_struct` on every
`exec()` and only preserves dump-filter bits plus
`MMF_HAS_PINNED` (per `mmf_init_legacy_flags` in
`include/linux/sched/coredump.h`) — `MMF_VM_MERGE_ANY` is dropped
on the floor. A wrapper process that calls `prctl` and then
`exec`s R would set the flag on itself and lose it immediately;
R would start with KSM disabled. The only way to get the flag
set on R's mm_struct is for R itself to call `prctl` after its
own exec. Hence the helper: a tiny C shared library, loaded
into R's address space via `dyn.load` from `zygote.R`, called
via the `.C` interface.

**Required operator action.** KSM is host-side; blockyard can
opt a process in but can't force the kernel to scan. Operators
must:

1. Enable ksmd: `echo 1 > /sys/kernel/mm/ksm/run`.
2. Tune the scan rate. The kernel default (`pages_to_scan=100`,
   `sleep_millisecs=20`) is ~20MB/sec scanned, which is
   desktop-tuned and far too slow for a server with multi-GB R
   bundles. Recommended starting point:
   `echo 2000 > /sys/kernel/mm/ksm/pages_to_scan`, which scans
   a 1GB working set in roughly 2 minutes. Workloads with
   larger bundles or higher session churn may need 5000–10000.

Phase 4-6's preflight check warns if either `run=0` or
`pages_to_scan` is at the default 100, but only when both
`experimental.ksm = true` in server config AND at least one
app has `ksm = true` (D3) — operators who have not opted into
KSM see no preflight noise. Both warnings are non-fatal but
the `pages_to_scan` warning is the operationally important one
— see "RSS spike during recovery" below.

**RSS spike during recovery (and how D11 contains it).** When
N children all hit a level-2 GC at roughly the same time
(request burst, coordinated work), each one dirties most of
its inherited package pages, breaking COW in lockstep. Peak RSS
spikes from `~1 × bundle + N × per-session-delta` to
`~N × bundle + N × per-session-delta` until ksmd catches up.
For a 1GB bundle × 8 children that's ~7GB of transient growth.
With default `pages_to_scan=100`, recovery takes minutes — long
enough to OOM a host that was sized for the steady-state.
Tuning `pages_to_scan` (above) shrinks the recovery window.
Decision D11 makes the spike survivable by pinning forked
children at `oom_score_adj=1000` so the OOM killer reaps a
child (one session lost, recoverable via the existing 307
fallback) instead of the zygote (entire family lost, full
preload cost on every affected session). Capacity-model
guidance for worst-case headroom math is tracked as Deferred
item #2.

**Fallback behaviour.** Every failure path is non-fatal. Opt-in
absent (`BLOCKYARD_KSM_ENABLED` not set, which is the default
whenever `experimental.ksm = false` or `apps.ksm = false`) →
`ksm_status=disabled`, prctl is never called, zygote starts
normally without joining the kernel merge pool. This is the
**most common** path by design. When the opt-in *is* active:
pre-6.4 kernel → `EINVAL` → `ksm_status=unsupported`, zygote
starts normally, memory model decays to PSOCK over time. No
ksmd running → helper succeeds but nothing happens, same
result. Outer seccomp blocks the syscall → `EPERM` →
`ksm_status=denied`. Helper `.so` missing or can't load →
logged, zygote starts anyway with `ksm_status=helper_missing`.
The zygote model never refuses to run because KSM is
unavailable.

**Threat model caveat.** KSM has a documented side-channel
history (Suzaki et al. 2011; Bosman et al. 2016): bit-identical
pages merged across processes expose timing-based information
leakage via Flush+Reload and similar cache attacks.
`PR_SET_MEMORY_MERGE` opts a process into a *global* kernel
merge pool, so children within a zygote family, and zygotes
across apps on the same host, all participate in one merging
domain — phase 4-6's sandboxing does not address this because
it operates above the physical-frame layer. The **primary
mitigation is the independent KSM opt-in from D3**: operators
who don't want KSM exposure leave `experimental.ksm = false`
(the default) or `apps.ksm = false` for affected apps, and
those apps use the zygote model with no KSM activation at all.
The zygote and KSM toggles in the UI are both admin-gated. See
Deferred #1 below for the full multi-tenant story and the
further hardening tracked as follow-up work.

### D8. KSM observability via a separate `STATS` control command, not by extending `INFO`

Decision D7 provides the KSM opt-in but the actual recovery
rate for R workloads is unmeasured and workload-dependent
(REFCNT bumps, attribute mutation, residual JIT activity on
code not caught by D9, and mark-bit skew all reduce the merge
rate from the theoretical maximum). Operators need to see the
real number for their bundle, not the design's estimate. Three
decisions shape how that data flows:

- **`STATS` is a new command, not a field on `INFO`.** `INFO`
  is queried once at `NewClient` time (phase 4-5) and cached
  read-only — that's correct for static facts (R version, KSM
  enablement status, preload time, sandbox status) but wrong
  for continuously-changing metrics. Mixing the two would
  force either re-querying `INFO` on every metrics tick (and
  invalidating the cached startup state) or racing the reader
  goroutine. A separate `STATS` command is requested on a
  tick by a dedicated metrics goroutine using the multi-line
  `requestMulti()` path; the existing `reqMu` serialises it
  against `FORK`/`KILL` traffic.

- **Source of truth is `/proc/<pid>/ksm_stat` (Linux 6.1+).**
  The zygote knows its own PID and tracks each child's PID in
  the `children` env. On `STATS`, `zygote.R` reads
  `/proc/self/ksm_stat` plus `/proc/<childpid>/ksm_stat` for
  each tracked child, parses the `ksm_merging_pages` line, and
  returns `ksm_merging_pages_zygote`, `ksm_merging_pages_children`
  (sum), and `ksm_merging_pages_total`. Per-process granularity
  lets the worker detail page attribute savings to specific
  apps; the zygote+children sum is the headline number. If
  `/proc/<pid>/ksm_stat` doesn't exist (kernel < 6.1), the
  values are zero and `STATS` returns `ksm_stat_supported=0`
  so the metrics goroutine can record the failure mode without
  flooding logs.

- **Two metric scopes: per-zygote and host-global.**
  `blockyard_zygote_ksm_merging_pages{app_id, worker_id}` and
  `blockyard_zygote_ksm_merging_pages_total{app_id, worker_id}`
  come from `STATS` polling. A separate server-level scraper
  reads `/sys/kernel/mm/ksm/pages_sharing` every
  metrics-interval and updates an unlabeled
  `blockyard_host_ksm_pages_sharing` gauge. Host-global is
  what operators budget against (whole-host RAM); per-zygote
  is what they need for app-level capacity planning. Both
  ship in phase 4-6.

The `metrics_interval` defaults to 30s. Lower values give
fresher graphs at the cost of slightly more `STATS` round-trips
on the control channel; given that round-trips serialise on
`reqMu` against `FORK`/`KILL`, an aggressive value would
contend with cold starts under load. 30s is a comfortable
floor; operators can tune via `[proxy] zygote_metrics_interval`
if needed.

**Integration test as the regression net.** A KSM regression
(kernel change, seccomp tightening, ksmd disabled by another
process, helper bug) currently has no signal — the zygote
starts cleanly, sessions work, memory just slowly grows. The
integration test forks two children, forces `gc(full=TRUE)` in
each, polls `STATS` until `ksm_merging_pages_total > 0`, fails
on literal zero with the captured value logged, and skips
itself if `/sys/kernel/mm/ksm/run == 0`. This catches "KSM
stopped working entirely" without flaking on the noisy "what's
the exact merge count" question.

### D9. Bundle code is byte-compiled up front in the zygote via `compiler::cmpfile()`; packages are left alone

R's JIT compiles closure bodies on first call via an in-place
`SET_BODY` write that mutates the closure SEXP header in shared
COW pages. Every forked child hitting a first-time call ends up
dirtying those pages and allocating its own BCODESXPs in
child-local heap — even when two children compile the same
function, the resulting bytecode objects live at different
addresses, so the BODY pointers in the now-private closure
headers differ and KSM cannot merge them. Left unaddressed,
post-fork JIT is the dominant source of page divergence for
user code.

Separately from KSM, up-front byte-compilation saves 100–500ms
per cold start for typical bundles by eliminating the re-parse
/ re-source cost of `app.R` in every child — the FORK handler
calls `runApp(captured_app, ...)` against the already-compiled
shiny object instead of `runApp(bundle_path, ...)`. That
latency win is independent of KSM and is reason enough to keep
this optimisation even if KSM is disabled.

The fix has to be scoped carefully:

- **Packages are left alone on purpose.** Most CRAN packages
  arrive byte-compiled at install time (`R_COMPILE_PKGS=1` is
  the R 4.0+ default). Packages that ship with `ByteCompile:
  no` in DESCRIPTION opted out, usually because the compiler
  chokes on something they do (complex `NextMethod` chains,
  unusual `<<-` patterns, etc.). A namespace-walk that
  mass-recompiles every closure across the entire dependency
  tree would second-guess package authors' decisions, risk
  breaking their `ByteCompile: no` opt-outs, and require
  metaprogramming gymnastics (`unlockBinding` / `lockBinding`)
  to mutate locked namespace bindings. None of that is worth
  it: the package decision is already made, we should respect
  it, and the residual post-fork JIT cost for those few
  packages is bounded.
- **Bundle code is compiled via `cmpfile` + `loadcmp`.** The
  zygote parses `global.R` and `app.R` with
  `compiler::cmpfile(src, out, options = list(optimize = 3L))`,
  which walks the expression tree and compiles function
  literals as a side effect of compiling the enclosing
  assignment. `compiler::loadcmp(out, envir = env)` then
  evaluates the compiled expressions, producing closures whose
  bodies are `BCODESXP` from birth. No post-fork `R_cmpfun1`
  path, no `SET_BODY` writes, no child-local bytecode
  allocations for bundle code.
- **The shinyApp object is captured via `loadcmp()` return
  value, not re-sourced.** Same mechanism as phase 4-5's
  `source()$value` — `loadcmp()` returns the value of the last
  expression in `app.R`. When that expression is
  `shinyApp(ui, server)` (qualified or unqualified), the return
  value is the `shiny.appobj`. A defensive `runApp` stub in the
  preload env handles the `runApp(shinyApp(...))` edge case,
  same as phase 4-5. The FORK handler then calls
  `runApp(captured_app, port = ...)` in the child instead of
  `runApp(bundle_path, port = ...)`. Bundles that fail to
  produce a `shiny.appobj` are rejected at preload time with a
  clear error, same as phase 4-5.

**What this does not catch (residuals):**

- Files `source()`d from *inside* the server function at
  request time (e.g., `source("R/utils.R", local = TRUE)`).
  Those run in the child, not the zygote. Operators who care
  can call `compiler::cmpfile()` themselves in `app.R` before
  the server function is defined, which folds the helpers
  into the compiled bundle env.
- Anonymous closures created at runtime
  (`factory <- function(x) function(y) x + y`). These are
  small in number and short-lived; residual JIT activity on
  them is bounded.
- Lazy S4 / R6 method table materialization. Bounded by
  construction.

Residual activity is small enough that we leave JIT enabled at
the default level; disabling it would give up the speedup on
runtime-created closures in exchange for a very small reduction
in page divergence. Revisit if measurements show otherwise.

### D10. Metrics-poll cadence defaults to 30s and is tunable

The `STATS` round-trip serialises on `reqMu` against `FORK` and
`KILL` — that's decision D8's "`reqMu` naturally defers to
in-flight cold starts" point. An aggressive polling interval
would contend with cold starts under load; a too-slow interval
would delay metrics by enough to hide short-lived recovery
windows.

30s is comfortable for both ends: cold starts take
single-digit milliseconds to seconds, so 30s of headroom is
~1000x the expected cold-start budget and contention is
structurally negligible. Dashboards updated every 30s are fresh
enough for capacity planning and tuning; operators debugging a
specific incident can tune `[proxy] zygote_metrics_interval`
down to 10s or 5s temporarily. Tuning below that risks
contention; tuning above 60s starts hiding short spikes.

The tick is shared between the `Manager.metricsLoop` (per-worker
polls) and the host-global scraper (`/sys/kernel/mm/ksm/pages_sharing`).
Both advance on the same ticker so correlation across the two
scopes is stable at the tick boundary.

### D11. Children pin themselves at `oom_score_adj=1000`; zygote stays at default

When KSM-broken pages spike RSS during a coordinated GC burst
(D7's "RSS spike during recovery"), the kernel's OOM killer
picks the process with the highest `oom_score`. Without
intervention that's the zygote — it has the largest RSS and
the same default `oom_score_adj=0` as everything else in the
cgroup. Killing the zygote loses the entire family: the
preload investment, every active session, and forces a full
cold start on the next request. Killing a single child loses
one session, which the existing 307-redirect fallback recovers
transparently.

Linux exposes `/proc/<pid>/oom_score_adj` precisely for biasing
this decision. The value is added to the badness score (max
effective score: 1000), and crucially **raising your own
`oom_score_adj` is unprivileged** — `CAP_SYS_RESOURCE` is only
required to *lower* it. So each forked child writes `1000` to
`/proc/self/oom_score_adj` as the very first thing it does
post-fork (step 1 of `apply_post_fork_sandbox`, before the
unshare), before any other sandbox setup or user code runs.
The zygote stays at the default `0`. Under memory pressure the
kernel will always reap a child first, regardless of the RSS
ratio between zygote and children.

Three properties of this approach matter:

- **No privilege coupling.** Phase 4-6 sandboxing drops all
  capabilities; if we relied on `CAP_SYS_RESOURCE` to lower
  the zygote's adj, we'd have to coordinate with the
  cap-dropping story or special-case the zygote. Raising the
  child's adj sidesteps the issue entirely — works in any
  environment, including the most locked-down sandbox.
- **Bounded blast radius.** In both backends the only OOM
  candidates within the affected cgroup or namespace are the
  zygote and its children. Setting children to the maximum
  `1000` doesn't risk reaping unrelated host processes.
- **Failure mode is observable.** If the `write()` to
  `/proc/self/oom_score_adj` fails (extremely unlikely — this
  is a self-write to a procfs file always present on Linux),
  the helper returns `ERR_OOM_ADJ` and the child exits cleanly
  via `mcexit(1L)`. Operators see the failure as a CHILDEXIT
  with exit code 1 and a `blockyard_zygote event=sandbox
  status=failed step=1` log line.

Alternatives considered:

- **Lower the zygote to `-800` or `-1000` instead.** Cleaner
  symbolically (it's "the system process") but requires
  `CAP_SYS_RESOURCE`, which phase 4-6 would need to grant
  explicitly before dropping capabilities. Also risks edge
  cases where `-1000` makes the zygote OOM-immune and the
  kernel panics instead of killing anything. Rejected.
- **Use cgroup memory limits to bound children individually.**
  Solves a related problem (per-session memory cap) but
  doesn't change the OOM-kill order when the *group* is under
  pressure. Also requires rootless cgroup delegation which
  isn't available in all supported deployments. Orthogonal,
  not a substitute.
- **Have the zygote `madvise(MADV_DONTNEED)` package pages on
  the children's behalf when memory pressure is high.**
  Theoretically attractive (proactive recovery) but R has no
  idea which pages are "package pages" vs. session pages, and
  getting this wrong corrupts the heap. Way out of scope.

### D12. Apparmor unconfined is opt-in per host, not blanket

Ubuntu 23.10+ ships a default apparmor profile for Docker
containers that blocks unprivileged user-namespace creation —
exactly the operation the post-fork sandbox hook needs. The
fix is `--security-opt apparmor=unconfined` on the container
create call, which disables apparmor for the container while
leaving the seccomp filter and user-namespace isolation intact.

**But this should only apply on hosts where apparmor is
actually active.** Adding `apparmor=unconfined` unconditionally
has two side effects on hosts without apparmor:

- Docker daemon logs a warning on every container create
  (apparmor not enabled, security-opt has no effect).
- Any future security tooling that parses container labels
  looking for "unconfined" would see a false positive.

Detection happens at server startup:
`/sys/module/apparmor/parameters/enabled` exists and contains
`Y` → apparmor is active on the host; `N` or missing → it isn't.
The result is stored on the server state; the Docker `Forking`
spawn reads it and conditionally adds the security-opt.

The process backend doesn't care about apparmor because it
runs bwrap directly, not inside a Docker container. If bwrap
is prevented from creating user namespaces by the host's
apparmor profile, operators need to adjust the host profile —
that's a deployment prerequisite documented in phase 3-8's
operator guide. Phase 4-6 doesn't attempt to work around
host-level apparmor restrictions on the process backend.

---

## Deferred

1. **KSM side-channel documentation and future hardening.** KSM
   has a well-documented side-channel history — Suzaki et al.,
   *"Memory deduplication as a threat to the guest OS"* (EuroSec
   2011) first described the vector, and Bosman et al., *"Dedup
   Est Machina"* (IEEE S&P 2016) weaponized it. When two
   processes have bit-identical pages merged into one physical
   frame, either process can detect access patterns on the
   other via cache-timing measurement (Flush+Reload and
   variants). In the phase 4-6 model this matters in two
   places:

   - **Cross-session within a zygote family.** All children
     inherit `MMF_VM_MERGE_ANY` from the zygote, so they
     participate in the same merge domain. A malicious session
     on app X can measure timing patterns against other
     sessions on app X and leak data held in bit-identical
     pages — code, constants, cached auth tokens, shared
     lookup tables with sensitive values.
   - **Cross-zygote across apps on the same host.** Worse:
     `PR_SET_MEMORY_MERGE` puts a process into a *global*
     kernel-level merge pool, not a per-cgroup or per-namespace
     one. If app A's zygote and app B's zygote both opt in,
     any bit-identical pages between them get merged by the
     same ksmd. A malicious session on app A can Flush+Reload
     against app B's memory via the shared physical frames.
     Mount namespaces, seccomp, capability dropping, and
     cgroups do not address this — KSM operates below all of
     those isolation primitives.

   **Phase 4-6's sandboxing does not fix this.** The
   sandboxing track addresses `/tmp` leakage, capability
   excess, and syscall surface — all above the physical-frame
   layer where KSM operates. Treat the two concerns as
   independent prerequisites for multi-tenant deployment.

   **Phase 4-6 already ships the primary mitigation: the
   per-app `ksm` flag and the server-wide `experimental.ksm`
   flag from D3.** Operators who are concerned about side
   channels leave `experimental.ksm = false` (the default)
   and/or leave `apps.ksm = false` for affected apps. Those
   apps get the zygote model's unconditional benefits (startup
   latency, per-session isolation) without any KSM exposure —
   `prctl(PR_SET_MEMORY_MERGE)` is never called, the process
   never joins the kernel merge pool, and the memory model
   degrades to the PSOCK-equivalent steady state that phase
   4-5's fallback path already supports. This is the "kill
   switch" path that D3 describes.

   **What remains deferred for multi-tenant hardening beyond
   the opt-in:**

   - **Threat-model documentation.** A dedicated
     operator-facing doc explaining when KSM is safe to
     enable, what data a malicious session can plausibly
     leak, and how to audit a bundle for sensitive
     singletons. Phase 4-6 ships only the inline warnings in
     the UI and a reference to Suzaki/Bosman in this design
     doc.
   - **Per-app fine-grained controls beyond the boolean.** For
     example, disabling KSM only for apps tagged as
     `access_type=public` (already a column in the apps
     table), or auto-disabling when OIDC user count exceeds a
     threshold, or a host-level deny list. These are all
     ergonomic refinements on top of the opt-in that phase
     4-6 ships.
   - **Active side-channel mitigations** — scrubbing sensitive
     memory regions, using `MADV_UNMERGEABLE` on specific
     allocations, or kernel patches that scope the merge pool
     by cgroup. All of these are substantial R&D work and
     don't belong in a phase focused on landing the mechanism.

   The phase 4-6 opt-in gate is the structural fix; the
   deferred items here are refinements that make the opt-in
   more ergonomic and better documented for operators who
   need to make the call.

2. **Capacity-model guidance for KSM deployments.** Tracked as
   a follow-up alongside the phase-4-5 capacity work
   (cynkra/blockyard#160). The KSM-specific sub-question is
   the worst-case headroom math for the RSS spike during
   recovery — how much transient growth should operators plan
   for, given their bundle size and expected coordinated-GC
   arrival rate? D11's `oom_score_adj` pinning makes the spike
   survivable (the OOM killer reaps a child, not the zygote)
   but operators still need to size their hosts so that the
   child reap isn't too frequent. Phase 4-6 ships the
   observability (D8) to measure the spike; phase 4-6+
   capacity guidance turns those measurements into host-sizing
   recommendations.

3. **Per-session CPU/memory bounds via per-child cgroup
   delegation.** D5 drops `RLIMIT_AS` and `RLIMIT_CPU` because
   they don't work cleanly with R's allocator and Shiny's
   session lifetime. The right mechanism for per-session
   memory/CPU bounds is cgroup delegation — create a per-child
   cgroup, set limits on it, move the child into it. Blocked
   today by cgroup v2 delegation not being universally
   available in supported deployments (rootless Docker,
   non-systemd hosts, some k8s configurations). Revisit once
   cgroup v2 delegation is a reasonable prerequisite across
   the supported deployment surface.

4. **Fork-safe package allowlist / metadata.** Some R packages
   are not safe to load before forking (rJava, arrow, anything
   with open fds or threads at load time). Phase 4-6's
   documentation (deliverable #8) covers the categories. Phase
   4-6 ships without runtime checks — a bundle that loads
   fork-unsafe packages into the zygote will fail at fork time
   with an opaque error. Adding a bundle-build-time check
   (parse the package list, warn on known-unsafe) is a
   follow-up.

5. **Per-app rlimits when measurements exist.** If operators
   eventually need graduated per-session memory/CPU/NPROC
   bounds beyond the `RLIMIT_NPROC=64` default, the natural
   shape is per-app config columns (`session_memory_limit`,
   `session_cpu_limit`, `session_nproc`) plus the
   `ContainerUpdate`-based enforcement path. Phase 4-6 ships
   the `STATS` infrastructure that would feed tuning decisions
   but does not ship the per-app fields themselves — see D5
   for the reasoning.

6. **Per-child logging.** All children currently write to the
   same container stdout, so `Logs(workerID)` returns
   interleaved output from the zygote and all children. For
   debugging this is annoying but not blocking. A follow-up
   could prefix each line with `[childID]` from inside the
   zygote or per-child.
