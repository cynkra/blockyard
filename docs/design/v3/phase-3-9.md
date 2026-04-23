# Phase 3-9: Close the Egress-Isolation Gap

Phase 3-7's process backend promised worker-vs-internal-services
egress isolation via per-worker host UIDs and operator-installed
`iptables -m owner --uid-owner` / `--gid-owner` rules. #305 diagnosed
why the original implementation never actually delivered on that
promise and landed a fork+setuid shim that makes the iptables story
work in the "recommended" root-blockyard / containerized deployment
— **but only on hosts where
`kernel.apparmor_restrict_unprivileged_userns=0`**. On Ubuntu 23.10+
(the current default) the post-exec shim's
`unshare(CLONE_NEWUSER)` inside bwrap fails with EPERM because the
setuid'd process is no longer in init_userns's CAP_SYS_ADMIN
short-circuit for the restriction. #305 ships with an ops-doc
advising the sysctl override as an interim measure; phase 3-9 is
the permanent fix.

Phase 3-9 closes the egress-isolation gap for **both** deployment
modes without operator-facing sysctl friction:

1. **Root-blockyard (containerized).** Restructure the spawn path to
   `unshare(CLONE_NEWUSER)` while the caller is still root, so the
   kernel's `CAP_SYS_ADMIN in init_userns` short-circuit bypasses
   the AppArmor check. setuid happens *inside* the pre-created
   namespace, where the worker UID is already mapped. bwrap joins
   the pre-created namespace via `--userns <fd>` and drops its own
   `--unshare-user`.

2. **Native non-root.** No root step available. Blockyard delegates
   the uid_map write to shadow-utils'
   `newuidmap`/`newgidmap` (setuid-root binaries) against a
   subuid/subgid range admin-provisioned for blockyard's user via
   `/etc/subuid`/`/etc/subgid`. The same "pre-create namespace, pass
   fd via `--userns`" plumbing as path 1.

The paths share a single mechanism: a caller-side userns setup that
bwrap joins instead of creating. Bwrap never calls `unshare` itself
in this phase, which means the AppArmor check never fires on a
non-root process and the `PR_SET_DUMPABLE` workaround from #305 is
no longer needed — setuid happens while we're the userns creator,
the subsequent uid_map write is handled by our (privileged or
newuidmap-helper) code before bwrap exists, and dumpable state is
irrelevant to the spawn path after that.

The shared mechanism also retires the #305 `bwrap-exec` shim and
the `TestMain` dispatch, the `chmod`-all-ancestors
`workerAccessibleTempDir` helper stays (it's an unrelated
production constraint — see "Worker-accessible source paths" in
Background), and the CI workflow's `kernel.apparmor_restrict_*=0`
sysctl override can be removed from the root matrix once phase 3-9
lands.

Depends on phases 3-7 and 3-8. #305 is the escape hatch for
AppArmor-unrestricted hosts in the interim.

---

## Background

Before listing deliverables, four kernel/bwrap interactions have to
be understood together — they're what made #305's implementation
take so long to pin down. Future phase-3-9 implementers need all
four.

### 1. The identity-kuid constraint

iptables `--uid-owner $W` matches processes whose kuid in init_userns
equals `W`. bwrap's default `--uid W --gid G --unshare-user` creates
a new user namespace and writes a uid_map of the form
`<sandbox_uid> <caller_uid> 1` — single-line, single-length, because
that's the only shape an unprivileged writer is allowed. Result: an
inner process running with in-ns uid=W has kuid=`caller_uid` in
init_userns, not W. Operator rules target the wrong kuid.

Three ways to force `kuid == W` (i.e., produce an identity-shaped
effective map):

- **`caller_uid == sandbox_uid` at bwrap time.** The `<sandbox_uid>
  <caller_uid> 1` map becomes an identity map by construction.
  Achieved by fork+setuid(W) before `exec(bwrap)`. Needs CAP_SETUID
  at fork time. This is the #305 shim path.
- **Multi-line map covering both 0 and W.** A privileged writer
  (root in init_userns writing a child userns's uid_map) can include
  multiple extents. If the map covers `0 0 1` and `W W 1`, a process
  inside the ns can setuid(W) and its kuid becomes W. Requires being
  the userns creator and writing before the setuid. This is the
  phase-3-9 **root** path.
- **newuidmap against a subuid range.** The setuid-root
  `newuidmap`/`newgidmap` helpers let a non-root caller write a
  multi-line map whose outside IDs are drawn from the caller's
  subuid range in `/etc/subuid`. Worker kuid becomes a subuid
  value, not W per se. This is the phase-3-9 **non-root** path.

Setuid-bwrap mode (the Fedora/RHEL default, `chmod u+s /usr/bin/bwrap`)
was documented in phase 3-7 as a valid isolation mode. **It isn't**:
bwrap's `drop_privs` sets the monitor UID to `opt_sandbox_uid` but
the uid_map write format still uses `real_uid_of_caller` as the
outside ID, and bwrap never calls `setgid()` outside the
`opt_userns_fd != -1` branch. Workers appear with `kuid = caller_uid`
and `kgid = caller_gid` from iptables's perspective regardless.
Phase-3-9's CI matrix rework retires the `setuid` leg accordingly.

### 2. The AppArmor unprivileged-userns restriction

Ubuntu 23.10+ ships
`kernel.apparmor_restrict_unprivileged_userns=1` as a host sysctl.
The check intercepts `unshare(CLONE_NEWUSER)` when:

- `current_cred()->euid != 0` (or more precisely, the process lacks
  `CAP_SYS_ADMIN` in init_userns), AND
- the process's AppArmor profile does not grant the `userns`
  permission.

Three facts make this surprisingly hard to escape:

- **The check is host-level.** `docker run --privileged` sets the
  container's AppArmor profile to `unconfined`, which **does** grant
  `userns`, but the host sysctl runs before AppArmor mediation
  reaches the container. #305's CI evidence: inside a `--privileged`
  container on an Ubuntu 24.04 GHA runner, the setuid'd process
  still hit EPERM on `unshare`, and the CI workflow had to
  explicitly `sysctl -w
  kernel.apparmor_restrict_unprivileged_userns=0` for all three
  matrices to pass.
- **Root short-circuits.** The kernel skips the check entirely when
  the caller has `CAP_SYS_ADMIN` in init_userns. A root-blockyard
  process can always create userns regardless of the sysctl. This
  is what the phase-3-9 root path exploits — do the unshare *before*
  the setuid, while we're still in the short-circuit, and the check
  never runs.
- **Nested userns doesn't help.** If the caller pre-unshares as
  root, then setuid(W), then execs bwrap with `--unshare-user`,
  bwrap creates a nested userns from an unprivileged process. The
  AppArmor check runs again on the nested unshare. Fix: tell bwrap
  `--userns <fd>` so it joins the caller-created ns instead of
  creating its own.

The orbstack kernel does not ship this sysctl, which is why #305's
local reproductions on the sandbox VM passed while CI failed;
expect the same discrepancy when debugging phase 3-9.

### 3. Bwrap's DAC check on bind-mount sources

bwrap's `--ro-bind <source> <target>` and `--bind <source> <target>`
open `<source>` for the bind with the caller's credentials. If the
caller is uid W (post-setuid) and `<source>` is 0700 root-owned,
bwrap fails with `Can't find source path <path>: Permission denied`
— despite the fact that bwrap has CAP_SYS_ADMIN inside the user
namespace for the *mount* operation itself. DAC is checked on the
path-resolution walk, not on the mount syscall.

#305 surfaced this in the integration tests: `t.TempDir()` creates
0700 root-owned dirs which workers (uid W) can't traverse. Fix was
the `workerAccessibleTempDir` helper that `chmod 0755`s every
tmpdir-prefixed ancestor. **This is not a test-only concern** —
production deployments need bundle storage world-readable (or
group-readable by the worker GID) for the same reason. Phase 3-8's
ops docs already assume this because the Docker backend has the
same constraint; phase 3-9 carries the constraint unchanged.

### 4. Map-write privilege and `setgroups=deny`

An unprivileged writer to `/proc/<pid>/uid_map` is limited to a
single-extent single-length map whose outside ID matches the
caller's own euid. For gid_map, the kernel also requires the caller
to first write `deny` to `/proc/<pid>/setgroups` (mitigates a
historical privilege-escalation path through group membership).

A privileged writer — root in init_userns writing a child userns's
map — has neither restriction: multi-line maps are allowed, and
`setgroups=deny` is not required.

The phase-3-9 root path writes maps as root from the parent side
(multi-line, no setgroups restriction). The non-root path delegates
to `newuidmap`/`newgidmap`, which handle setgroups internally per
shadow-utils' logic.

### 5. Go runtime constraints on unshare

`unshare(CLONE_NEWUSER)` from a multi-threaded process returns
EINVAL (kernel's `check_unshare_flags` requires `thread_group_empty`
when the implicit `CLONE_THREAD|CLONE_FS` is added to the
unshare flags). Go's runtime is multi-threaded by default.

Workarounds:

- Fork a helper child (single-threaded immediately after fork, by
  definition) and do the unshare+map-write there. Go exposes this
  via `syscall.ForkExec` or `exec.Cmd` + `SysProcAttr.Unshareflags`
  + `SysProcAttr.UidMappings`.
- Use `cmd.SysProcAttr.Unshareflags = CLONE_NEWUSER` +
  `UidMappings`/`GidMappings` on the `exec.Cmd` for bwrap — Go's
  `forkAndExecInChild1` does the unshare+map write in the forked
  child before exec. Simplest if we can make the bwrap command
  itself work with this setup.

`syscall.Setuid` in Go 1.16+ uses `AllThreadsSyscall` and
propagates across all runtime threads, so post-setuid all threads
are consistent — but that's irrelevant to the unshare
multi-threaded-process limitation.

### 6. PR_SET_DUMPABLE (for context — not needed by phase-3-9)

#305's post-exec shim restores `PR_SET_DUMPABLE` after setuid
because the kernel clears dumpable on cred transitions and a
non-dumpable process sees `/proc/self/*` as owned by root, which
makes bwrap's unprivileged uid_map write fail with EACCES. Phase 3-9
avoids this entirely: setuid happens inside a ns we already
control, the uid_map write happens on our side of the fork (or
through newuidmap), and bwrap never does an unprivileged
`/proc/self/uid_map` write post-exec. The shim's prctl step can be
deleted along with the shim.

---

## Prerequisites from Earlier Phases

- **Phase 3-7** — process backend core: `ProcessBackend`,
  `bwrapArgs`, `uidAllocator`, `checkBwrapHostUIDMapping`, the
  `--uid`/`--gid`/`--unshare-user` plumbing this phase replaces.
- **Phase 3-8** — packaging and the operator-facing
  `docs/backends.md` / native-deployment guide that phase 3-9
  extends with subuid setup.
- **#305** — the post-exec fork+setuid shim, `bwrap-exec`
  subcommand, and `TestMain` dispatch. Phase 3-9 **deletes** the
  shim and its wiring; integration tests that were added to exercise
  it (`requireHostUIDMapping`, `workerAccessibleTempDir`) carry
  over because they describe real production constraints, not shim
  quirks.

---

## Deliverables

### Core infrastructure

1. **`internal/backend/process/userns.go`** — new file exposing:
   ```go
   // PrepareWorkerUserns creates a user namespace, writes the
   // appropriate uid_map/gid_map, and returns a fd referring to
   // /proc/<helper-pid>/ns/user that bwrap can `--userns` into.
   // The helper process parks until cleanup() reaps it; closing
   // the fd before the helper exits keeps the ns alive for
   // bwrap's lifetime.
   func PrepareWorkerUserns(cfg *ProcessConfig, uid, gid int) (nsFile *os.File, cleanup func(), err error)
   ```

   Implementation branches on `os.Getuid() == 0`:
   - **Root path.** Fork a helper. Helper unshares CLONE_NEWUSER as
     root (short-circuits AppArmor). Parent writes multi-line
     `/proc/<helper-pid>/uid_map` with entries for 0 (so the helper
     can still exec), W (the worker uid), and any other uids bwrap
     needs to see mapped. Same for gid_map. No setgroups=deny
     needed. Helper opens `/proc/self/ns/user` and holds it; parent
     dups the fd over a pipe, then signals the helper to park. On
     cleanup the parent closes the fd and SIGKILLs the helper.
   - **Non-root path.** Fork a helper. Helper unshares CLONE_NEWUSER
     as non-root — this is where AppArmor fires, so the helper
     needs to have a profile granting `userns` (see deliverable 7)
     or blockyard needs a sysctl override documented. Parent invokes
     `newuidmap <helper-pid> <sandbox_uid> <subuid_start_plus_offset> 1 ...`
     and `newgidmap <helper-pid> <sandbox_gid>
     <subgid_start_plus_offset> 1 ...`. Rest is like the root path.

2. **Spawn-path restructure (`process.go`, `preflight.go`).** Each
   bwrap invocation site:
   - Calls `PrepareWorkerUserns` for the (uid, gid) it needs.
   - Strips `--unshare-user`, `--uid`, `--gid` from the bwrap args.
   - Adds `--userns <N>` where N is the inherited fd slot.
   - Passes the ns fd via `cmd.ExtraFiles`.
   - Drops `cmd.SysProcAttr.Credential` (or the post-exec shim the
     pre-#305 code paths used). No setuid at exec time; the worker
     runs as whatever uid the caller is inside the pre-created ns.

   `bwrapArgs` keeps the `--unshare-pid --unshare-uts` flags and
   its bind/tmpfs/proc/dev mounts. Those namespaces don't trip
   AppArmor because the userns is already open and the process has
   CAP_SYS_ADMIN inside it.

3. **Delete the #305 shim.** Remove
   `internal/backend/process/bwrap_exec.go`, the
   `postgres_test.go` TestMain dispatch for `bwrap-exec`, the
   `cmd/blockyard/main.go` dispatch + `runBwrapExecFn` indirection,
   and the `backend_process.go` init hook that wires it. `bwrapSysProcAttr`
   reduces to `{Pdeathsig: SIGKILL}` only.

### Configuration

4. **`[process] worker_subuid_range_start` / `worker_subuid_range_end`
   config.** Non-root path only. Optional — when unset, blockyard
   reads `/etc/subuid` for the invoking user's range directly (or
   calls `getsubids(3)` if the binding is available). Explicit
   config wins on multi-tenant hosts where subuids are shared.
   Preflight rejects a subuid range narrower than
   `worker_uid_range_start..end`, and also rejects overlap with
   `worker_uid_range_*` (operator mistake: trying to run root and
   non-root paths simultaneously).

### Preflight

5. **`checkBwrapHostUIDMapping` rewrite.** Becomes a per-path check:
   - Root blockyard: spawn a probe through `PrepareWorkerUserns` +
     bwrap and verify the sandboxed child's kuid via
     `/proc/<pid>/status`. Should be OK unconditionally (no
     AppArmor in play). Error with diagnostic if not, because that
     means something fundamental is broken.
   - Non-root blockyard: check `newuidmap` is installed, a subuid
     range is configured or derivable, the range is wide enough,
     and a probe spawn succeeds with kuid inside the range. Error
     with path-specific remediation (install `uidmap`, add
     `/etc/subuid` entry, widen range, etc.) otherwise.

6. **New `checkUnprivilegedUserns`.** Proactive probe of the
   AppArmor restriction. Reads
   `/proc/sys/kernel/apparmor_restrict_unprivileged_userns` and,
   when set to `1`, forks a helper that attempts
   `setuid(probe_uid)` + `unshare(CLONE_NEWUSER)`. Maps the helper's
   exit status to:
   - Helper succeeded → Info: "restriction present but an AppArmor
     profile allowing `userns` is in effect; no action needed".
   - Helper failed with EPERM → Warning (for root-blockyard: won't
     trigger in practice because the root path doesn't use
     unprivileged userns; for non-root: blocks the phase-3-9 spawn
     path).
   Error severity is reserved for `checkBwrapHostUIDMapping`'s
   end-to-end probe because this check is a diagnostic-layer warning
   only.

### Operator story

7. **Shipped AppArmor profile** (`packaging/apparmor/blockyard`).
   A profile granting `userns` for blockyard's binary, so operators
   on Ubuntu 23.10+ can load it instead of disabling the sysctl
   globally. Ship in the `blockyard-process` deb/rpm (phase 3-8)
   and document `apparmor_parser -r` in the install guide. Pin
   `abi <abi/4.0>` — the `userns` profile language became stable
   in AppArmor 4.0 (Ubuntu 24.04's version); earlier releases need
   a different syntax. Non-goal: supporting AppArmor < 4.0.

8. **`checkWorkerEgress` remediation text split by mode.** Root
   path keeps `--uid-owner $W` / `--gid-owner $G`. Non-root path
   targets a subuid/subgid range:
   ```
   iptables -A OUTPUT -m owner \
            --uid-owner $subuid_start-$subuid_end \
            -d <service-ip> -j REJECT
   ```
   Range syntax because each worker gets a distinct subuid inside
   the admin-provisioned range.

9. **`docs/backends.md` and phase-3-8 native-deployment guide
   rewrite** with three sections:
   - **Containerized root-blockyard** (the default). No host
     sysctl tweaks needed post-phase-3-9. Bundle storage must be
     readable by the worker UID (same as Docker backend, which
     phase 3-8 already documents).
   - **Native non-root.** subuid/subgid provisioning walkthrough:
     `/etc/subuid` entry shape, range sizing (≥ worker UID range
     width, recommend 2×), `newuidmap` install (`uidmap`
     package on Debian/Ubuntu, `shadow-utils` elsewhere). iptables
     rule shape with the range syntax. AppArmor profile load step
     for Ubuntu 23.10+ (deliverable 7).
   - **Managed K8s / PaaS matrix.** Document what works and what
     doesn't. Hardened clusters rejecting privileged pods and
     unable to load AppArmor profiles need the Docker backend;
     the process backend is not a fit.

### Testing

10. **Unit and integration tests.**
    - `package process` internal test for `PrepareWorkerUserns`
      root path (skipped unless running as root + unprivileged
      userns available).
    - `package process` internal test for non-root path (skipped
      unless `newuidmap` present + subuid range provisioned).
    - `package process_test` integration test asserting that
      worker kuid in init_userns matches expected (identity for
      root, inside subuid range for non-root).
    - Retain `workerAccessibleTempDir` from #305 — still needed
      for bundle-path DAC.

11. **CI matrix rework.** `.github/workflows/ci.yml` process job
    changes:
    - `root`: remove the
      `sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`
      override (the root path doesn't trigger the restriction).
      This becomes the regression test that phase-3-9's
      pre-unshare approach actually bypasses AppArmor.
    - `native-nonroot`: new matrix leg. Provisions
      `/etc/subuid` and `/etc/subgid` entries for
      `blockyard-runner`, loads the shipped AppArmor profile,
      runs the non-root spawn tests.
    - `native-nonroot-unconfigured`: non-root user, no subuid.
      Asserts preflight Error with the
      `/etc/subuid`-setup remediation text.
    - `setuid-bwrap`: **retire**. Setuid-bwrap was never a valid
      isolation mode and phase 3-9 doesn't use it.
    - `unprivileged` (current): effectively subsumed by the two
      `native-nonroot-*` legs. Remove.

### Interim (kept from #305 until phase 3-9 lands)

- The CI workflow's `sysctl -w
  kernel.apparmor_restrict_unprivileged_userns=0` in all three
  process matrix legs stays in place.
- `phase-3-7.md` and `docs/backends.md` document the sysctl as the
  interim remediation for Ubuntu 23.10+ root-blockyard operators,
  with a pointer to phase-3-9 for the permanent fix.

---

## Open questions for implementation

Not blockers — pick any reasonable path, document the choice.

1. **Helper lifecycle.** `PrepareWorkerUserns`'s helper holds the
   userns open for bwrap to `--userns` into. Keeping the helper
   alive for the worker's lifetime adds one process per worker.
   Alternatives:
   - `pidfd_open` on the helper + `setns` dance, so the helper can
     exit once bwrap has attached. More complex, harder to reason
     about lifetimes.
   - Use Go's `SysProcAttr.Unshareflags` + `UidMappings` directly
     on the bwrap `exec.Cmd` (no separate helper), and arrange for
     the uid_map write + setuid to happen inside the forked child
     before exec. Requires bwrap to work without `--unshare-user`,
     which it should if we pass `--userns` — but needs
     verification.

   Pick the simplest viable option; CI will catch lifetime bugs.

2. **Subuid range allocation strategy.** Contiguous
   (`subuid_start + worker_uid_offset`) or random inside the range?
   Contiguous simpler; random marginally friendlier for
   concurrent-server deployments sharing a subuid range. With
   phase-3-8's Redis-backed UID allocator already in place,
   contiguous allocation inside the allocator's reservation is
   probably fine.

3. **`newuidmap` diagnostic helpers.** `newuidmap` exit status 1
   with no stderr detail is common when `/etc/subuid` entries are
   subtly wrong. Consider a helper in `checkBwrapHostUIDMapping`'s
   non-root path that parses `/etc/subuid` itself and cross-checks
   against the runtime UID, surfacing the specific mismatch
   ("`/etc/subuid` has `blockyard:100000:65536` but blockyard is
   running as `blockyard-worker`") before invoking `newuidmap`.

4. **Preserving the `skip_preflight=true` escape hatch.** An
   operator who sets `skip_preflight=true` currently gets workers
   running at blockyard's own UID (pre-#305 silent-failure mode).
   Should phase 3-9 preserve that fallback, or hard-fail the spawn
   path when the preconditions aren't met? Recommendation:
   preserve (phase 3-9 is additive), but surface it in docs so
   operators know they're trading isolation for continuity.

5. **AppArmor profile packaging channel.** Ship the profile in the
   deb/rpm directly, or in a separate
   `blockyard-apparmor` package? Separate package is cleaner for
   container images that don't run AppArmor tooling, but adds an
   install step for operators who expect the deb to "just work".
   Probably inline in `blockyard-process` with a conditional
   postinst that runs `apparmor_parser -r` only when AppArmor is
   active on the host.

---

## What not to carry over from #305

A few things #305 introduced as escape hatches that phase 3-9
deletes:

- The `blockyard bwrap-exec` subcommand and its dispatch in
  `cmd/blockyard/main.go`. Phase 3-9 doesn't exec a shim — bwrap is
  invoked directly.
- The `TestMain` `bwrap-exec` dispatch in
  `internal/backend/process/postgres_test.go`. Gone with the shim.
- `bwrapSysProcAttr` complexity. Reduces to Pdeathsig-only.
- The `runBwrapExecFn` indirection in `backend_process.go`.
- The CI workflow's apparmor sysctl override in the root matrix
  (keeps it in the non-root legs until the phase-3-9 AppArmor
  profile is the expected solution).

What to carry over:

- `workerAccessibleTempDir` test helper — describes the
  bundle-path DAC constraint, not a shim artefact.
- `requireHostUIDMapping` — classifier is still useful, just
  updated to reflect phase-3-9's actual capability detection.
- The phase-3-7.md corrections to the iptables claim and the
  `checkBwrapHostUIDMapping` doc comment.
- The phase-3-9 entry in `plan.md`.
