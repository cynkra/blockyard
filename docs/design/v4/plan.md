# blockyard v4 Implementation Plan

v4 adds per-session process isolation, cross-process memory sharing
via KSM, and the instrumentation to measure whether fork-based
optimization is worth its complexity. The approach is
measurement-first: build independent-process isolation and KSM,
gather real-world data on memory sharing effectiveness, then add the
experimental zygote (fork-based) model only if measurements justify
the additional complexity.

v4 depends on v3's process backend (phases 3-7, 3-8) and per-app
configuration (phase 3-6). v5 (Kubernetes) is independent and can be
developed in parallel with v4's later phases.

## Motivation

v3's process backend runs one R process per session inside a bwrap
sandbox. The Docker backend multiplexes sessions onto shared workers
(Posit Connect model). Neither offers per-session isolation with
cross-session memory sharing.

The zygote (fork-based) worker model claims two advantages over
independent processes: startup latency elimination and a KSM head
start. Both rest on unvalidated assumptions:

1. **Startup latency.** For typical Shiny apps (shiny + dplyr +
   ggplot2), cold start is ~1-3s with lazy loading. Fork saves
   ~1-2s per session. Real but modest, and already mitigated by
   `pre_warmed_sessions`. The heavy-package cases where fork
   saves 10-30s (rstan, arrow, torch) overlap heavily with
   packages that fail fork-safety checks.

2. **KSM effectiveness.** Fork gives KSM a head start (all pages
   shared at fork time), but KSM works on any identical pages
   across any processes. Whether independently started R processes
   produce enough byte-identical pages for KSM to be effective
   is an empirical question we haven't answered.

v4 answers these questions with measurements before committing to
the fork-based architecture.

## Build Phases

### Cross-cutting concern: runtime × KSM interaction

KSM's behaviour depends on the runtime context in ways that a flat
per-app toggle cannot express. Key interactions to work out during
the design of phases 4-1 through 4-3:

- **runc containers** share the host kernel. KSM merges pages
  across all runc workers on the same host. Multi-process
  containers (phase 4-2) are optional for sharing — independent
  containers already benefit.
- **Kata / VM-isolated containers** each have their own kernel.
  KSM only works *within* a single Kata VM. Multi-process
  containers become the only way to get cross-session sharing
  for Kata apps.
- **Process backend** workers share the host kernel. Same story
  as runc.

The config surface must let operators express combinations like
"KSM across runc workers, KSM within Kata containers, no KSM for
public-facing apps." Phase 4-1's per-app backend/runtime selection
and phase 4-3's KSM opt-in need to be designed together so the
resulting config model handles these interactions cleanly rather
than treating KSM as a context-free boolean.

### Phase 4-1: Hybrid Backend Dispatch

Per-app backend selection. Currently `server.backend` is a global
choice (Docker or process). Phase 4-1 allows mixing: some apps run
in containers, others as processes on the host.

**Deliverables:**

1. **Backend dispatcher** — a new type implementing `backend.Backend`
   that routes operations to the correct underlying backend based
   on worker ID -> app -> backend config. The proxy, coldstart, and
   ops layers continue to call `srv.Backend` unchanged.
2. **`backend` column on `apps`** — per-app override. Values:
   `""` (use server default), `"docker"`, `"process"`. Migration
   follows phase 3-1 expand-only rules.
3. **Co-existing backend instances** — `cmd/blockyard/main.go`
   constructs both a `DockerBackend` and a `ProcessBackend` when
   both config sections are present. The dispatcher holds
   references to both. When only one section is present, the
   dispatcher degrades to a pass-through.
4. **Fan-out operations** — `ListManaged`, `CleanupOrphanResources`,
   and `Preflight` fan out to all configured backends.
5. **Config changes** — both `[docker]` and `[process]` sections
   can coexist. `server.backend` becomes the default, not the
   exclusive choice.
6. **API/CLI/UI** — `backend` field on app update, validated
   against configured backends.
7. **Tests** — dispatcher routing, fan-out operations, per-app
   override, fallback to server default.

### Phase 4-2: Multi-Process Containers

Per-session isolation via independent R processes inside a single
container. A lightweight supervisor inside the container manages
child R processes — one per session. No forking: each child starts
R fresh and loads the bundle independently.

For runc containers this is one way to get per-session isolation
(the other being one container per session). For Kata containers
it is additionally the only way to get KSM sharing — processes
inside one Kata VM share a kernel, but separate Kata VMs do not.
The config/enablement design should account for this (see the
runtime × KSM note above).

**Deliverables:**

1. **In-container supervisor** — a small process (could be a Go
   binary or an R script) that listens on a control port, starts
   independent R child processes on demand, reports exits. Simpler
   than the zygote control protocol — no fork-safety constraints,
   no app.R shape requirements.
2. **`session.Entry.Addr`** — per-session routing target (reuses
   the design from `phase-4-5.md` Step 3, but for independently
   started children rather than forked ones).
3. **Child port allocation** — reuses the port allocator from
   phase 3-8 (process backend) or per-container ranges (Docker
   backend).
4. **Proxy integration** — session-addressed routing, unreachable-
   child fallback (reuses `phase-4-5.md` Step 10 design).
5. **Cleanup paths** — child exit handling, sweep loop for orphaned
   children (reuses `phase-4-5.md` Step 11 design).
6. **Tests** — supervisor start, child spawn, independent health,
   child crash detection, cleanup convergence.

### Phase 4-3: KSM on Independent Processes

Kernel same-page merging across independently started R processes.
No forking prerequisite — KSM finds and merges identical pages
across any registered processes.

Where KSM merges depends on the runtime: across all runc/process
workers on a host (shared kernel), but only within a single Kata
VM. The enablement mechanism (`prctl` inside the worker) is the
same in all cases, but preflight, observability, and the config
model need to account for the different scopes. See the
runtime × KSM note above.

**Deliverables:**

1. **KSM enablement** — C helper calling
   `prctl(PR_SET_MEMORY_MERGE)` per R process. Graceful fallback
   on older kernels or seccomp-restricted environments.
2. **KSM preflight** — check `/sys/kernel/mm/ksm/run` and
   `pages_to_scan`, warn when ksmd is off or under-configured.
3. **KSM observability** — Prometheus gauges for per-worker and
   host-global KSM merge counts, scraped from
   `/proc/<pid>/ksm_stat` and `/sys/kernel/mm/ksm/`.
4. **Per-app opt-in** — `ksm` column + `experimental.ksm` server
   flag (two-level gating from `phase-4-6.md`).
5. **Seccomp profile extension** — permit `prctl(PR_SET_MEMORY_MERGE)`
   for KSM-enabled workers.
6. **Tests** — KSM helper fallback, preflight checks, metrics
   scraping, skip on unsupported kernels.

### Phase 4-4: Memory Sharing Instrumentation

Tooling to measure real-world KSM effectiveness and answer the
question: does forking meaningfully improve page sharing over
independently started processes?

**Deliverables:**

1. **RSS and KSM dashboard** — Grafana dashboard (or equivalent)
   showing per-worker RSS, KSM merged pages, convergence time,
   and sharing ratio over session lifecycle.
2. **Comparison tooling** — scripts or test harness to run the
   same Shiny app in two modes (independent processes vs. forked
   children) and compare page-level sharing via
   `/proc/<pid>/pagemap` or KSM stats.
3. **Benchmark bundles** — representative Shiny apps (lightweight:
   shiny+dplyr+ggplot2; medium: shiny+DT+plotly+dbplyr; heavy:
   large data loading) for reproducible measurements.
4. **Documentation** — measurement methodology, results, and
   decision framework for when to use forking vs. independent
   processes.

### Phase 4-5: Experimental Zygote Model

**Informed by phase 4-4 measurements.** Phase 4-4 will show how
well KSM works on independently started processes. If sharing is
poor, the key question becomes whether forking recovers it — that
would be the primary justification for the zygote model's
complexity. Lands as experimental; promotion to production depends
on the measured delta.

**See `phase-4-5.md` for the full design and wire protocol.** Key
deliverables:

1. **`backend.Forking` capability interface**
2. **`internal/zygotectl/` + `internal/zygote/` packages**
3. **Docker and process backend `Forking` implementations**
4. **Zygote R script with fork-safety checks**
5. **Two-level opt-in** (`experimental.zygote` + per-app `zygote`)

### Phase 4-6: Zygote Hardening

**Depends on phase 4-5.** Post-fork sandboxing, byte-compilation,
OOM score pinning.

**See `phase-4-6.md` for the full design.**

## Build Order

```
Phase 4-1: Hybrid Backend Dispatch
  prerequisite for: phase 4-2 (multi-process needs per-app routing)

Phase 4-2: Multi-Process Containers
  depends on: phase 4-1
  prerequisite for: phase 4-3 (KSM needs multiple processes)

Phase 4-3: KSM on Independent Processes
  depends on: phase 4-2

Phase 4-4: Memory Sharing Instrumentation
  depends on: phase 4-3
  prerequisite for: phase 4-5 (data-driven go/no-go)

Phase 4-5: Experimental Zygote Model
  depends on: phase 4-4 (measurements inform scope)

Phase 4-6: Zygote Hardening
  depends on: phase 4-5
```

Phase 4-4 measurements inform how phases 4-5 and 4-6 proceed. If
KSM works well on independent processes, forking may not be needed.
If KSM sharing is poor without fork's identical starting state,
that's the case for promoting the zygote model from experimental
to production.
