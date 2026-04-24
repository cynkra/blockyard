# Phase 3-9: Rootless Enablement and Layer 6 Alternatives

Phase 3-7's process backend delivered six isolation properties for
workers: private filesystem view (1), PID namespace (2), no caps in
sandbox (3), seccomp syscall filtering (4), distinct in-sandbox UIDs
(5), and distinct *host* kuids visible to `init_userns` (6).
Post-#305 layer 6 works in the recommended containerized-root
deployment via fork+setuid before `exec(bwrap)`, provided the host
kernel either doesn't ship `kernel.apparmor_restrict_unprivileged_userns`
(pre-Ubuntu-23.10, Debian, Fedora, RHEL, Arch, minikube's default node
OS, GKE's COS, etc.) or has it disabled.

Phase 3-9 closes two gaps that #305 and its CI fallout surfaced:

1. **Ubuntu 23.10+ hosts block rootless unshare by default.** The host
   sysctl `kernel.apparmor_restrict_unprivileged_userns=1` intercepts
   any non-root `unshare(CLONE_NEWUSER)` unless the caller is under an
   AppArmor profile granting the `userns` permission. This affects
   *layers 1–5 too*, not just layer 6 — non-root bwrap can't create
   its own sandbox namespace at all. Today's operator remediation is
   `sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`, which
   relaxes the restriction host-wide for every unprivileged process.
   Phase 3-9 ships a narrow AppArmor profile that grants `userns` to
   blockyard and its subprocesses only.

2. **Non-root deployments get no layer 6.** The iptables
   `-m owner --uid-owner`/`--gid-owner` mechanism requires distinct
   host kuids per worker, which #305's fork+setuid shim provides only
   when blockyard runs as root. A #305 follow-up using
   `--userns <fd>` + newuidmap was investigated during phase 3-9 drafting
   and rejected: bwrap's `--userns` + `--uid` + `--gid` code path has
   a setuid-before-setgid ordering that drops CAP_SETGID before the
   `setgid(G)` call, EPERM'ing any attempt to land a worker at a
   non-host UID/GID in the pre-created namespace. Workarounds require
   either a bwrap upstream fix (slow) or a vendored bwrap fork
   (maintenance burden we don't want). Instead, phase 3-9 adds an
   orthogonal layer-6 mechanism via cgroup-v2 delegation: when
   blockyard's cgroup is delegated (the standard systemd pattern),
   workers are moved into a `workers/` subcgroup and operators filter
   egress with `iptables -m cgroup --path <path>/workers`. This works
   for both root and non-root deployments, without any userns
   gymnastics.

Phase 3-9 also hardens preflight to catch the misconfigurations that
layer 6 was implicitly defending against — a publicly-reachable cloud
metadata endpoint from blockyard's own process, or an unauthenticated
Redis — turning "kernel-level defence via owner-match iptables" into
"kernel-level defence by cgroup path when available + preflight
warnings for the footguns always".

Depends on phases 3-7 (process backend core), 3-8 (packaging patterns
and `internal/seccomp/` embed model that this phase mirrors for
AppArmor), and issue #305 (the fork+setuid shim, which stays). No
changes to the #305 shim, `bwrapExecSpec`, or `checkBwrapHostUIDMapping`
mechanics; phase 3-9 only adds around them.

---

## Prerequisites from Earlier Phases

- **Phase 3-7** — `ProcessBackend`, `bwrapArgs`, `uidAllocator`, the
  iptables egress story in `checkWorkerEgress`, the `blockyard probe`
  subcommand. Phase 3-9 extends `Spawn` with an optional cgroup move
  after `cmd.Start`, and adds three new preflight checks alongside
  the existing ones.
- **Phase 3-8** — the `internal/seccomp/` package's embed + CLI install
  pattern (`by admin install-seccomp`, release-asset upload, image
  `COPY` location). Phase 3-9 applies exactly the same pattern to the
  AppArmor profile.
- **#305 (merged)** — the `blockyard bwrap-exec` shim in
  `internal/backend/process/bwrap_exec.go`, `bwrapExecSpec` in
  `bwrap.go`, `checkBwrapHostUIDMapping` in `preflight.go`, and the
  `bwrapSysProcAttr` Pdeathsig wiring. All remain in place. Phase 3-9
  updates the preflight's non-root error message to frame the situation
  as "layer 6 unavailable without cgroup delegation" rather than
  "upgrade to phase 3-9", but the probe mechanism and root-path logic
  are unchanged.
- **Bubblewrap ≥ 0.6** — already required by phase 3-7 for
  `--unshare-user` semantics. No new bwrap version requirement.

---

## Deliverables

1. **`internal/apparmor/blockyard`** — shipped AppArmor profile granting
   `userns` to blockyard and inheriting the profile through its
   subprocesses (`bwrap`, `blockyard bwrap-exec`, the worker R process,
   etc.). Uses only core AppArmor rule syntax (no `abi <abi/N.N>,`
   pragma) so the profile parses on any AppArmor release that knows
   the `userns` rule, from Ubuntu 23.10's 3.x backport through
   AppArmor 4.x on 24.04+. The `by admin install-apparmor` CLI runs
   a post-write syntax check via `apparmor_parser -QT` (skip-kernel-
   load + skip-cache) so version-specific parse failures surface at
   install time with the parser's error message, not at load time as
   a cryptic `apparmor_parser -r` failure.

2. **`internal/apparmor/` Go package** — `//go:embed blockyard` into
   `var Profile []byte`. Exported `DefaultInstallPath` constant
   (`/etc/apparmor.d/blockyard`). Mirrors `internal/seccomp/` from
   phase 3-8.

3. **`by admin install-apparmor [--target <path>]`** — CLI subcommand
   that writes `apparmor.Profile` to the target path. Defaults to
   `apparmor.DefaultInstallPath`. Follows `by admin install-seccomp`
   shape exactly; the command prints a short follow-up instruction
   (`sudo apparmor_parser -r <target>` to load). Part of the same
   subcommand group added in phase 3-5.

3b. **`blockyard bwrap-smoke`** — a narrow subcommand on the server
   binary that exec's `bwrap --unshare-user --ro-bind / / -- /bin/true`
   and exits 0 on success, non-zero on failure. Used by the standalone
   `apparmor-smoke` CI job (Step 7) and by operators who want to
   verify a production host's AppArmor profile actually unblocks
   rootless bwrap. Tiny: no config parsing, no logging setup, just
   `exec.Command(bwrap, …).Run()`. The existing `#305 bwrap-exec`
   shim is orthogonal and stays unchanged.

4. **Release-asset upload** — `release.yml`'s publishing jobs upload
   `internal/apparmor/blockyard` as a GitHub release asset named
   `blockyard-apparmor` alongside the existing seccomp profiles. No
   new workflow; just an additional file in the existing
   `seccomp-blob` job's upload list.

5. **Docker image bundling** — the bwrap-capable variant Dockerfiles
   (`docker/server-process.Dockerfile` for `blockyard-process` and
   `docker/server-everything.Dockerfile` for `blockyard`) `COPY
   internal/apparmor/blockyard` to `/etc/blockyard/apparmor/blockyard`.
   The Docker-backend-only variant (`docker/server.Dockerfile` →
   `blockyard-docker`) does not ship bwrap and omits the profile.
   Operators extract the profile via `docker run --rm --entrypoint cat
   IMAGE /etc/blockyard/apparmor/blockyard`, matching the existing
   seccomp extraction pattern.

6. **Cgroup-v2 delegation detection and worker subgroup**
   (`internal/backend/process/cgroup.go`, new). Startup:
   detects whether blockyard's own cgroup is in a writable v2 subtree
   by attempting to create a sentinel subdirectory; if so, creates
   `<cgroup>/workers/`. Controllers (cpu/memory/io) are intentionally
   *not* enabled on `subtree_control`: process grouping via
   `cgroup.procs` is all the `iptables -m cgroup --path` match needs,
   and enabling controllers at a level where blockyard itself resides
   violates cgroup-v2's "no internal processes" rule. Per-worker
   resource limits stay out of scope (phase 3-7 decision #6). Spawn:
   after `cmd.Start`, writes `cmd.Process.Pid` to
   `<workers>/cgroup.procs` (best-effort — a failure logs a warning
   but does not abort the spawn). Fallback when delegation is
   unavailable: no cgroup move, phase-3-7 behaviour preserved.

7. **`checkCloudMetadataReachable`** (`internal/backend/process/preflight.go`)
   — new preflight check. Attempts a TCP connect to `169.254.169.254:80`
   from blockyard's own process (not from inside a bwrap sandbox). If
   reachable, `SeverityError`: "cloud metadata endpoint is reachable
   from blockyard; a compromised worker can steal the VM's IAM
   credentials. Block it host-wide (`iptables -A OUTPUT -d 169.254.169.254
   -j REJECT`) or use IMDSv2 / IRSA / run on a VM without an instance
   role." Skipped when the operator sets
   `[process] skip_metadata_check = true` (only useful when blockyard
   legitimately needs metadata access — rare).

8. **`checkRedisAuth`** (`internal/preflight/redis_auth.go`) — new
   preflight check called from the existing `checkRedisOnServiceNetwork`
   infrastructure (phase 3-3). Dials the configured Redis URL *without
   AUTH*, writes a `PING`, observes the reply. `+PONG` → Error:
   "Redis accepts commands without authentication. Workers (or any
   process on the host network) can read/modify session state, flush
   the registry, or DoS the service. Configure `requirepass` or
   ACLs." `-NOAUTH` → OK: "Redis requires authentication." Any
   other reply (including generic `-ERR` like MAXCLIENTS, a truncated
   response, or an empty reply from a TLS-only server rejecting our
   plain-TCP bytes) → Info with the raw reply, so the operator
   investigates rather than getting a false OK. `rediss://` URLs
   short-circuit to Info ("TLS Redis; plain-TCP auth probe skipped")
   without dialing. Connection failure → Info: "Redis not reachable
   from blockyard" (the reachability concern itself is surfaced by
   `checkRedisOnServiceNetwork`, not this check). Applies to all
   deployment modes regardless of backend.

9. **`checkCgroupDelegation`**
   (`internal/backend/process/preflight.go`) — new preflight check.
   When delegation is available: Info with the detected path and the
   example `iptables -m cgroup --path <path>/workers ...` rule
   operators can use. When delegation is unavailable: Info noting
   that per-worker egress via cgroup match is not possible and
   steering root deployments to the existing `--uid-owner` path.

10. **`checkBwrapHostUIDMapping` messaging refresh**
    (`internal/backend/process/preflight.go`). No mechanism changes.
    The non-root error message is rewritten to frame the gap in
    layer-6 terms: "non-root blockyard does not produce per-worker
    host kuids. Workers have filesystem, PID, caps, seccomp, and
    in-sandbox UID isolation regardless. For per-worker egress
    isolation on this deployment mode: (a) switch to containerized
    root, (b) enable cgroup-v2 delegation and use
    `iptables -m cgroup --path` rules — see `checkCgroupDelegation`,
    or (c) use the Docker backend." The placeholder "wait for phase
    3-9" text is removed.

11. **`checkWorkerEgress` probe enrolls into the workers cgroup**
    (`internal/backend/process/preflight.go`). Phase 3-7's egress
    probe was written against `-m owner --uid-owner/--gid-owner`
    rules, so it spawns bwrap with the worker UID/GID and relies on
    the kernel's owner match to mirror a real worker. That fails
    under cgroup-path layer 6: the probe is in blockyard's cgroup,
    not `workers/`, so `iptables -m cgroup --path workers` never
    matches and the probe reaches targets that real workers cannot.
    The fix threads `*cgroupManager` through `RunPreflight` and
    `probeReachable`, and after `cmd.Start()` the probe path calls
    `cgroups.Enroll(cmd.Process.Pid)` before `cmd.Wait()`. The
    race between `Start` and `Enroll` is bounded by bwrap's
    namespace/mount setup (~10–50 ms) vs. the enroll write (~1 ms),
    so the probe's first `connect()` lands after enrollment. When
    delegation is unavailable the probe behaves identically to the
    phase-3-7 code path (no-op enroll).

12. **CI coverage update** (`.github/workflows/ci.yml`).
    Retire the `setuid` leg from the `process` matrix (setuid-bwrap was
    incorrectly documented as a valid isolation mode in phase 3-7 and
    never delivered per-worker host kuids; this is a correctness
    retirement). Keep `root` and `unprivileged` — both still exercise
    distinct production code paths inside the `--privileged` CI
    container (root spawn with fork+setuid shim vs. non-root spawn).
    Add a new standalone `apparmor-smoke` job that runs directly on
    the Ubuntu 24.04 VM (no `container:`) because `--privileged`
    bypasses AppArmor enforcement inside the container and cannot
    faithfully test the profile. The standalone job installs the
    profile, keeps `apparmor_restrict_unprivileged_userns=1`, and
    invokes `blockyard bwrap-smoke` as a non-root user to validate
    that the profile actually unblocks rootless `unshare(CLONE_NEWUSER)`.
    Also asserts the negative: with the profile unloaded, the same
    invocation fails.

13. **Documentation** — `docs/design/backends.md` gains a
    deployment-mode × isolation-layer matrix (see Step 7). The
    phase-3-8 `process-backend.md` native guide gains a section on
    cgroup-v2 delegation + `systemd` unit configuration + the
    `iptables -m cgroup` recipe. A short section on "what k8s users
    should expect" points non-trivial egress-isolation requirements
    at the Docker backend.

### What phase 3-9 explicitly does *not* do

- **Does not restructure the root path.** The investigated
  alternative ("pre-unshare while still root, pass `--userns <fd>` to
  bwrap") is blocked by the bwrap `--userns` + `--uid` + `--gid`
  setuid-before-setgid bug and offers no benefit over #305's
  fork+setuid-before-exec mechanism. The #305 shim, `bwrapExecSpec`,
  and the `PR_SET_DUMPABLE` restoration stay exactly as they are.
- **Does not delete `bwrap_exec.go` or the `TestMain` dispatch.**
  They're the mechanism layer 6 relies on for root deployments.
- **Does not add newuidmap, subuid config, or any
  `PrepareWorkerUserns` helper.** Deferred indefinitely; see Design
  decisions.
- **Does not ship deb/rpm packages for the AppArmor profile.** Same
  channel story as the seccomp profile (CLI install, release asset,
  Docker image).

---

## Step-by-step

### Step 1: Ship the AppArmor profile

Profile source: `internal/apparmor/blockyard`. Mirrors the posture of
phase 3-8's `internal/seccomp/blockyard-outer.json` — source-of-truth
committed to the repo, embedded into a Go package for CLI install,
uploaded as a release asset, and `COPY`ed into the variant Docker
images.

Profile shape. The profile uses only core rule syntax — no
`abi <abi/N.N>,` pragma — so it parses on any AppArmor release that
knows the `userns` rule. That covers Ubuntu 23.10's 3.x backport and
Ubuntu 24.04's 4.x baseline; older AppArmor (pre-23.10) that lacks
the `userns` rule entirely also lacks the restriction this profile
exists to lift, so it doesn't need one.

```
include <tunables/global>

# Purpose: grant the `userns` permission to blockyard and its
# subprocesses, narrowly, so rootless bwrap can create its sandbox
# user namespace on hosts where kernel.apparmor_restrict_unprivileged_userns=1
# (Ubuntu 23.10+ default). This profile does NOT confine blockyard
# itself — the rules below are deliberately broad (all capabilities,
# all paths, network, mount, ptrace) because blockyard is the trusted
# component in this architecture; the workers it spawns are the
# untrusted ones, and they are confined by bwrap's capability drop,
# seccomp, and bind-mount restrictions, not by AppArmor. Site-specific
# profiles wanting tighter confinement of blockyard should layer on
# top of this one.

profile blockyard /usr/{bin,local/bin}/blockyard
         flags=(attach_disconnected, mediate_deleted) {

    include <abstractions/base>

    # The load-bearing grant this profile exists for.
    userns,

    # Core filesystem access — blockyard reads its config, writes
    # bundle storage, opens /proc and /sys for preflight checks, etc.
    # We intentionally grant broad filesystem access rather than
    # enumerate paths: tightening is an operator-hardening exercise
    # and belongs in a site-specific profile, not the default we
    # ship.
    / r,
    /** mrwklix,

    capability,
    network,
    signal,
    dbus,
    mount,
    umount,
    pivot_root,
    ptrace,

    # Subprocess transitions — blockyard exec's itself (bwrap-exec
    # shim, probe subcommand), bwrap, and the worker R interpreter.
    # `ix` inherits the profile; the bwrap child's internal
    # `unshare(CLONE_NEWUSER)` then runs under this profile and sees
    # the `userns` grant.
    /usr/{bin,local/bin}/blockyard ix,
    /usr/bin/bwrap ix,
    /usr/{bin,local/bin}/R* ix,  # matches R, Rscript, Rdevel, …
    /opt/R/*/bin/R* ix,          # rig-managed R installations
}
```

Why `ix` (inherit) rather than `Ux` (unconfined) for bwrap and R:
with inheritance, the nested `unshare(CLONE_NEWUSER)` that bwrap
performs in its creator-path fork runs under this profile and
receives the `userns` grant. Without it the nested unshare would hit
the restriction again. The trade-off is that the R interpreter inside
the sandbox runs under the same profile — but bwrap's
capability-dropping + seccomp are the primary controls on the worker,
and the profile doesn't confine anything the in-sandbox environment
cares about.

The profile covers both `/usr/bin/blockyard` and
`/usr/local/bin/blockyard` because distribution packages typically
install to the former while `go install` lands in the latter.
`/opt/R/*/bin/R*` covers rig-managed R installations that phase 3-7
supports.

### Step 2: `internal/apparmor` Go package

```go
// internal/apparmor/apparmor.go
package apparmor

import _ "embed"

//go:embed blockyard
var Profile []byte

// DefaultInstallPath is where `apparmor_parser -r` expects the
// profile on Ubuntu/Debian systems.
const DefaultInstallPath = "/etc/apparmor.d/blockyard"
```

Same shape as `internal/seccomp/seccomp.go` from phase 3-8.
Deliberately no loading logic in the Go side — profile loading
requires root and changes host state; the operator runs
`apparmor_parser -r` themselves after `by admin install-apparmor`
writes the file.

### Step 3: `by admin install-apparmor`

New subcommand in `cmd/by/admin.go` (the `by admin` group landed in
phase 3-5). Mirrors `by admin install-seccomp` — same
`installApparmorProfile(target)` helper pattern, same `MkdirAll` on
the parent directory, same `0o644` file mode, same default-when-empty
handling.

```go
func adminInstallApparmorCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "install-apparmor",
        Short: "Write the blockyard AppArmor profile to disk",
        Long: `Write the embedded AppArmor profile to a target path so
operators on AppArmor-enforcing hosts (Ubuntu 23.10+ by default) can
load it with 'sudo apparmor_parser -r <target>'. The profile grants
the 'userns' permission narrowly to blockyard and its subprocesses,
enabling rootless bwrap to create its sandbox user namespace without
disabling kernel.apparmor_restrict_unprivileged_userns host-wide.`,
        Args: cobra.NoArgs,
        RunE: func(cmd *cobra.Command, _ []string) error {
            target, _ := cmd.Flags().GetString("target")
            if target == "" {
                target = apparmor.DefaultInstallPath
            }
            if err := installApparmorProfile(target); err != nil {
                return err
            }
            fmt.Printf("Wrote AppArmor profile to %s\n", target)
            if err := validateApparmorProfile(target); err != nil {
                // Non-fatal: surface the parse error and guidance, but
                // the file is already written — operators can inspect
                // it or try a different AppArmor version.
                fmt.Fprintf(os.Stderr,
                    "Warning: apparmor_parser rejected the profile: %v\n"+
                        "On AppArmor versions without the 'userns' rule, use "+
                        "sysctl kernel.apparmor_restrict_unprivileged_userns=0 "+
                        "as a host-wide fallback instead.\n", err)
                return nil
            }
            fmt.Println("Load with: sudo apparmor_parser -r " + target)
            return nil
        },
    }
    cmd.Flags().String("target", "",
        `output path (default: /etc/apparmor.d/blockyard)`)
    return cmd
}

// validateApparmorProfile runs apparmor_parser in syntax-check mode
// (-Q skip-kernel-load, -T skip-cache) to catch version-specific
// parse failures at install time. This fully exercises the grammar
// and binary-policy generation without touching the kernel or the
// on-disk parser cache. Missing apparmor_parser is not an error;
// the host simply isn't configured for AppArmor and the load step
// is a no-op anyway.
func validateApparmorProfile(target string) error {
    parser, err := exec.LookPath("apparmor_parser")
    if err != nil {
        return nil
    }
    out, err := exec.Command(parser, "-QT", target).CombinedOutput()
    if err != nil {
        return fmt.Errorf("%s: %w (output: %s)",
            parser, err, strings.TrimSpace(string(out)))
    }
    return nil
}
```

### Step 4: Release asset + Docker image COPY

Release workflow. The existing `seccomp-blob` job in `release.yml`
(which already uploads `blockyard-bwrap-seccomp.bpf` and
`blockyard-outer.json` artifacts) gains a third upload step for
the AppArmor profile, and the downstream `github-release` job adds
the artifact to its download+release list:

```yaml
# In seccomp-blob job:
- uses: actions/upload-artifact@v7
  with:
    name: apparmor-profile
    path: internal/apparmor/blockyard
    retention-days: 1

# In github-release job:
- uses: actions/download-artifact@v8
  with:
    name: apparmor-profile
    path: .
# …and add `blockyard-apparmor` (the renamed artifact file) to the
# softprops/action-gh-release `files:` list.
```

The published release asset filename is `blockyard-apparmor` (no
extension — matches `blockyard-bwrap-seccomp.bpf` /
`blockyard-outer.json` style). Renaming the job from `seccomp-blob`
to `security-artifacts` is cosmetic and can be a follow-up.

Dockerfile COPY. Add to the two bwrap-capable variants only —
`docker/server-process.Dockerfile` and
`docker/server-everything.Dockerfile`:

```dockerfile
COPY internal/apparmor/blockyard /etc/blockyard/apparmor/blockyard
```

`docker/server.Dockerfile` (the Docker-backend-only variant) does
not ship bwrap, so the profile is irrelevant there and is not
copied.

Operators extract via:

```sh
docker run --rm --entrypoint cat \
  ghcr.io/cynkra/blockyard-process:<v> \
  /etc/blockyard/apparmor/blockyard | sudo tee /etc/apparmor.d/blockyard
sudo apparmor_parser -r /etc/apparmor.d/blockyard
```

### Step 5: Cgroup-v2 delegation detection and worker subgroup

New file `internal/backend/process/cgroup.go`.

```go
package process

// cgroupManager coordinates cgroup-v2 delegation for the process
// backend. When the host delegates a v2 subtree to blockyard, the
// manager creates `<delegated>/workers/` and exposes Enroll(pid)
// for the spawn path to move each worker into it. When delegation
// is unavailable, manager.workersPath is empty and Enroll is a
// no-op.
type cgroupManager struct {
    workersPath string // "" when delegation unavailable
}
```

Detection logic:

```go
// detectCgroupDelegation reads /proc/self/cgroup, verifies the
// unified hierarchy, and tests write access on blockyard's own
// cgroup by creating and removing a sentinel subdirectory. Returns
// the absolute path to blockyard's cgroup on success, "" on any
// detection or permission failure.
//
// The probe is deliberately conservative: any error (missing
// cgroup-v2, cgroup namespaced away, read-only mount, permission
// denied on mkdir) yields "" and the fallback path. Misreporting
// delegation-available when it isn't would surface as noisy cgroup
// write errors on every spawn, so we err on the side of reporting
// unavailable.
func detectCgroupDelegation() (string, error) {
    data, err := os.ReadFile("/proc/self/cgroup")
    if err != nil {
        return "", fmt.Errorf("read /proc/self/cgroup: %w", err)
    }
    line := strings.TrimSpace(string(data))
    // cgroup-v2 unified: single line "0::/<path>".
    // cgroup-v1 hybrid: multiple lines with controllers; skip.
    if !strings.HasPrefix(line, "0::") || strings.Count(line, "\n") > 0 {
        return "", nil
    }
    cgPath := strings.TrimPrefix(line, "0::")
    fullPath := filepath.Join("/sys/fs/cgroup", cgPath)

    probe := filepath.Join(fullPath, ".blockyard-delegation-probe")
    if err := os.Mkdir(probe, 0o755); err != nil {
        if errors.Is(err, os.ErrPermission) {
            return "", nil
        }
        return "", fmt.Errorf("probe subcgroup: %w", err)
    }
    _ = os.Remove(probe)
    return fullPath, nil
}
```

Workers-subcgroup setup:

```go
// ensureWorkersSubcgroup creates <cgroot>/workers/. Idempotent.
//
// Resource controllers (cpu/memory/io) are deliberately not enabled
// on cgRoot's subtree_control. The iptables `-m cgroup --path` match
// only reads cgroup.procs membership, and enabling controllers at
// cgRoot would violate cgroup-v2's "no internal processes" rule
// because blockyard itself is a process at cgRoot (both blockyard
// and workers/ sit at the same level). Per-worker resource limits
// stay out of scope — see phase 3-7 decision #6.
func ensureWorkersSubcgroup(cgRoot string) (string, error) {
    workers := filepath.Join(cgRoot, "workers")
    if err := os.MkdirAll(workers, 0o755); err != nil {
        return "", fmt.Errorf("mkdir workers subcgroup: %w", err)
    }
    return workers, nil
}
```

Enrollment:

```go
// Enroll moves pid into the workers subcgroup. Best-effort: a
// write failure logs a warning and continues. The spawn path must
// tolerate cgroup move failures because the worker is functionally
// correct without the move — only the cgroup-based iptables rule
// fails to match, which is already the non-root layer-6 gap.
func (m *cgroupManager) Enroll(pid int) {
    if m.workersPath == "" {
        return
    }
    procsFile := filepath.Join(m.workersPath, "cgroup.procs")
    if err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0); err != nil {
        slog.Warn("process backend: cgroup enroll failed",
            "pid", pid, "path", m.workersPath, "err", err)
    }
}
```

Spawn-path integration in `process.go`:

```go
// Inside (*ProcessBackend).Spawn, just after cmd.Start succeeds
// and before the wait goroutine is unblocked via proceed.
b.cgroups.Enroll(cmd.Process.Pid)
```

The cgroup manager is a field on `ProcessBackend`, initialised in
`New`:

```go
func New(fullCfg *config.Config, rc *redisstate.Client, db *sqlx.DB) (*ProcessBackend, error) {
    // ...existing bwrap/bundle checks...
    cgMgr, err := newCgroupManager()
    if err != nil {
        // Detection error is informational only.
        slog.Info("process backend: cgroup delegation probe failed, falling back to flat cgroup",
            "err", err)
    }
    return &ProcessBackend{
        // ...existing fields...
        cgroups: cgMgr,
    }, nil
}
```

`newCgroupManager` runs `detectCgroupDelegation` and, on success,
`ensureWorkersSubcgroup`. A detection error is non-fatal.

Why best-effort rather than strict: the cgroup path is a
layer-6 *enhancement* — functionally, workers work either way. A
strict failure would surface detection quirks (containers without
cgroup namespaces, cgroup-v1 hybrid hosts) as backend startup errors
and force operators to debug before anything else runs. Best-effort
with clear preflight reporting via `checkCgroupDelegation` matches
the layer-6 severity everywhere else in this phase.

### Step 6: Preflight check additions

All three new checks register in `RunPreflight` alongside the
existing phase-3-7 checks. Order matters only where one check
depends on another's environment; none of the three new checks
depend on each other.

`RunPreflight`'s signature grows a `*cgroupManager` parameter so
`checkCgroupDelegation` can read `workersPath` and `checkWorkerEgress`
can enroll its probe into the workers cgroup (deliverable #11):

```go
func RunPreflight(
    cfg *config.ProcessConfig,
    fullCfg *config.Config,
    cgroups *cgroupManager,
) *preflight.Report
```

`ProcessBackend.Preflight` passes `b.cgroups`. Tests that invoke
`RunPreflight` directly pass `nil`, which is handled throughout
(Enroll no-ops on nil or on empty `workersPath`).

`checkCloudMetadataReachable`:

```go
func checkCloudMetadataReachable(cfg *config.ProcessConfig) preflight.Result {
    const name = "cloud_metadata"
    if cfg.SkipMetadataCheck {
        return preflight.Result{
            Name: name, Severity: preflight.SeverityInfo,
            Message: "cloud metadata check skipped by [process] skip_metadata_check",
            Category: "process",
        }
    }
    d := net.Dialer{Timeout: 2 * time.Second}
    conn, err := d.Dial("tcp", "169.254.169.254:80")
    if err != nil {
        return preflight.Result{
            Name: name, Severity: preflight.SeverityOK,
            Message: "cloud metadata endpoint not reachable from blockyard",
            Category: "process",
        }
    }
    _ = conn.Close()
    return preflight.Result{
        Name: name, Severity: preflight.SeverityError,
        Message: "cloud metadata endpoint (169.254.169.254) is reachable from blockyard. " +
            "A compromised worker can steal this VM's IAM credentials. " +
            "Block it with `iptables -A OUTPUT -d 169.254.169.254 -j REJECT`, " +
            "enable IMDSv2 (EC2) or Workload Identity (GCP/AKS), " +
            "or run on a VM without an attached instance role. " +
            "Set [process] skip_metadata_check = true to suppress this check.",
        Category: "process",
    }
}
```

The new config field `[process] skip_metadata_check` is a single
bool, default false. Added to `ProcessConfig` alongside the existing
fields. The escape hatch is for the rare deployment where blockyard
legitimately needs metadata (e.g. running on a VM whose IAM role is
used by blockyard itself for S3 storage) — operators who opt into
this also accept the worker-compromise implication.

`checkRedisAuth` lives in `internal/preflight/redis_auth.go` because
it applies across backends and its existing Redis inspection code
(`checkRedisOnServiceNetwork` from phase 3-3) already imports the
Redis config:

```go
func CheckRedisAuth(cfg *config.RedisConfig) Result {
    const name = "redis_auth"
    if cfg == nil || cfg.URL == "" {
        return Result{Name: name, Severity: SeverityOK,
            Message: "Redis not configured", Category: "redis"}
    }
    if strings.HasPrefix(strings.ToLower(cfg.URL), "rediss://") {
        // Plain-TCP probe against a TLS-only server would write
        // garbage at the TLS handshake layer and get no useful reply.
        // Skip rather than report spurious "unexpected reply".
        return Result{Name: name, Severity: SeverityInfo,
            Message: "TLS Redis (rediss://); plain-TCP auth probe skipped",
            Category: "redis"}
    }
    hp := TCPAddrFromRedisURL(cfg.URL) // existing helper
    if hp == "" {
        return Result{Name: name, Severity: SeverityInfo,
            Message: "Redis URL not parseable for auth probe", Category: "redis"}
    }
    d := net.Dialer{Timeout: 2 * time.Second}
    conn, err := d.Dial("tcp", hp)
    if err != nil {
        return Result{Name: name, Severity: SeverityInfo,
            Message: "Redis not reachable from blockyard for auth probe", Category: "redis"}
    }
    defer conn.Close()
    // Send PING without AUTH.
    if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
        return Result{Name: name, Severity: SeverityInfo,
            Message: fmt.Sprintf("Redis PING failed: %v", err), Category: "redis"}
    }
    conn.SetReadDeadline(time.Now().Add(1 * time.Second))
    buf := make([]byte, 128)
    n, _ := conn.Read(buf)
    reply := string(buf[:n])
    switch {
    case strings.HasPrefix(reply, "+PONG"):
        return Result{Name: name, Severity: SeverityError,
            Message: "Redis accepts commands without authentication. " +
                "Any host-network process (including compromised workers) can " +
                "read/modify session state, flush the registry, or DoS the service. " +
                "Configure `requirepass` in redis.conf or enable ACLs.",
            Category: "redis"}
    case strings.HasPrefix(reply, "-NOAUTH"):
        return Result{Name: name, Severity: SeverityOK,
            Message: "Redis requires authentication", Category: "redis"}
    default:
        // Includes generic `-ERR ...` (protocol errors, MAXCLIENTS),
        // and surprise ACL replies like `-WRONGPASS` / `-NOPERM`
        // that shouldn't fire for an unauthenticated PING but would
        // indicate the probe is hitting a weirdly-configured server.
        // Surface as Info so the operator investigates rather than a
        // false OK.
        return Result{Name: name, Severity: SeverityInfo,
            Message: fmt.Sprintf("Redis responded with unexpected reply to unauthenticated PING: %q", reply),
            Category: "redis"}
    }
}
```

Wiring: `preflight.CheckRedisAuth(fullCfg.Redis)` is called
explicitly from both backends' `RunPreflight` — 
`internal/backend/process/preflight.go:RunPreflight` and
`internal/backend/docker/preflight.go:RunPreflight` — alongside
their respective backend-specific checks. There is no existing
cross-backend Redis entry point to plug into (phase 3-3's
`checkRedisOnServiceNetwork` is docker-specific, uses Docker's
NetworkInspect). Two call sites is small enough not to warrant a
registry abstraction; if a future phase adds a third backend, the
call moves to a shared helper at that point. The check itself is
identical across backends — unauth'd Redis is a footgun either way,
even though the Docker backend's per-worker bridge network mitigates
worker-to-Redis reachability.

`checkCgroupDelegation` lives in
`internal/backend/process/preflight.go` since it's process-backend
specific. It also probes for `xt_cgroup` availability, because a
kernel without that module will fail the operator's
`iptables -m cgroup --path` rule at rule-install time with a cryptic
"No chain/target/match by that name" error:

```go
func checkCgroupDelegation(b *ProcessBackend) preflight.Result {
    const name = "cgroup_delegation"
    if b.cgroups.workersPath == "" {
        return preflight.Result{
            Name: name, Severity: preflight.SeverityInfo,
            Message: "cgroup-v2 delegation unavailable. Per-worker egress " +
                "isolation via `iptables -m cgroup --path` is not available " +
                "on this host. Root deployments can use `iptables -m owner " +
                "--gid-owner` rules on the per-worker host kuids instead. " +
                "For non-root deployments wanting per-worker egress: enable " +
                "cgroup delegation (systemd: Delegate=yes on the unit) or " +
                "use the Docker backend.",
            Category: "process",
        }
    }
    xtCgroup := xtCgroupAvailable()
    msg := fmt.Sprintf(
        "cgroup-v2 delegation available at %q; workers moved into %q. "+
            "Install a rule like `iptables -A OUTPUT -m cgroup --path %s -d <service-ip> -j REJECT` "+
            "to block worker access to internal services.",
        filepath.Dir(b.cgroups.workersPath),
        b.cgroups.workersPath,
        strings.TrimPrefix(b.cgroups.workersPath, "/sys/fs/cgroup/"))
    if !xtCgroup {
        return preflight.Result{
            Name: name, Severity: preflight.SeverityWarning,
            Message: msg + " WARNING: the xt_cgroup netfilter module does " +
                "not appear to be loaded (no match in /proc/net/ip_tables_matches); " +
                "`iptables -m cgroup` rules will fail to install. Run " +
                "`sudo modprobe xt_cgroup` or add it to /etc/modules-load.d/.",
            Category: "process",
        }
    }
    return preflight.Result{
        Name: name, Severity: preflight.SeverityOK,
        Message: msg, Category: "process",
    }
}

// xtCgroupAvailable reports whether the xt_cgroup netfilter match is
// loaded. /proc/net/ip_tables_matches lists builtin+loaded matches,
// one per line. Returns true on any read error so we don't emit a
// false warning on hosts where the file isn't accessible (rootless
// containers, odd /proc mounts).
func xtCgroupAvailable() bool {
    data, err := os.ReadFile("/proc/net/ip_tables_matches")
    if err != nil {
        return true
    }
    for _, line := range strings.Split(string(data), "\n") {
        if strings.TrimSpace(line) == "cgroup" {
            return true
        }
    }
    return false
}
```

`checkWorkerEgress` — existing phase-3-7 function. Signature gains
`cgroups *cgroupManager`; the probe path enrolls after `Start` and
before `Wait` so the probe PID is in the `workers/` cgroup before
its first `connect()`:

```go
func probeReachable(
    cfg *config.ProcessConfig,
    cgroups *cgroupManager,
    uid, gid int,
    target string,
) bool {
    // ...existing bwrapExecSpec + exec.Command setup...
    if err := cmd.Start(); err != nil {
        return false
    }
    cgroups.Enroll(cmd.Process.Pid) // no-op when delegation unavailable
    return cmd.Wait() == nil
}
```

The `probeReachableFn` test seam at the top of `preflight.go` keeps
the same shape; tests that replace it adopt the new signature. Tests
that stub `probeReachable` to return a fixed bool already ignore the
extra argument.

`checkBwrapHostUIDMapping` — the existing function's non-root branch
is rewritten. Message text changes, and severity drops from Error to
Info: the `-m owner` mechanism is inherently inapplicable in non-root
mode (not broken), so blocking startup was wrong. Operators reach
layer-6 in non-root deployments through cgroup delegation instead;
`checkCgroupDelegation` reports that mechanism. Root-branch probe
logic is unchanged — it still errors if the fork+setuid wiring is
broken:

```go
// In checkBwrapHostUIDMapping, the existing os.Getuid() != 0 branch:
return preflight.Result{
    Name:     name,
    Severity: preflight.SeverityInfo,
    Message: "non-root blockyard cannot produce per-worker host kuids " +
        "via fork+setuid, so `iptables -m owner --uid-owner` rules do " +
        "not match worker traffic. This is inherent to non-root mode, " +
        "not a failure: workers still have filesystem, PID, capability, " +
        "seccomp, and in-sandbox UID isolation (layers 1-5), and " +
        "per-worker egress (layer 6) is available via cgroup-v2 " +
        "delegation — see cgroup_delegation. Alternatives: run as root " +
        "(containerized deployment) for the `-m owner` path, or use the " +
        "Docker backend for per-worker network namespaces.",
    Category: "process",
}
```

This removes the previous situation where an operator who had done
everything right (`Delegate=yes` systemd unit, cgroup delegation
preflight reports OK, `-m cgroup --path` rules installed) still had
to set `server.skip_preflight=true` to start the service.

### Step 7: CI coverage update

Two changes. First, `.github/workflows/ci.yml`'s `process` job matrix
drops the `setuid` leg (correctness — never delivered per-worker host
kuids, was mis-documented as a valid isolation mode in phase 3-7).
`root` and `unprivileged` remain, unchanged: both exercise distinct
production spawn paths inside the `--privileged` CI container, and
the existing `sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`
preamble is still needed because bwrap's post-setuid (root leg) and
direct non-root (unprivileged leg) userns unshare calls fire the
restriction regardless of AppArmor enforcement state.

Why the matrix can't host an "apparmor-enforced" leg: GitHub
Actions `container:` jobs with `options: --privileged` mount the
container with `--security-opt apparmor=unconfined` effectively.
An AppArmor profile loaded on the host VM doesn't attach to
processes inside the privileged container, so the rootless-with-
profile behaviour can't be observed there.

Second, add a standalone `apparmor-smoke` job on the VM directly.
Also add `apparmor-smoke` to the `workflow_dispatch.inputs.job`
choice enum at the top of `ci.yml` so the job is dispatchable by
name (`gh workflow run ci.yml -f job=apparmor-smoke`):

```yaml
apparmor-smoke:
  runs-on: ubuntu-24.04
  timeout-minutes: 10
  needs: [unit]
  if: github.event_name != 'merge_group' && github.event_name != 'push' && (inputs.job == '' || inputs.job == 'apparmor-smoke')
  # Runs directly on the Ubuntu 24.04 VM — not inside a container —
  # so the host AppArmor profile actually attaches to processes that
  # match its path. The CI runner's VM image ships AppArmor 4.x and
  # has kernel.apparmor_restrict_unprivileged_userns=1 by default.
  steps:
    - uses: actions/checkout@v6
    - uses: actions/setup-go@v6
      with:
        go-version-file: go.mod
    - name: Install bubblewrap + apparmor utils
      run: |
        sudo apt-get update
        sudo apt-get install -y --no-install-recommends bubblewrap apparmor apparmor-utils
    - name: Build and install blockyard to /usr/local/bin
      run: |
        go build -o /tmp/blockyard ./cmd/blockyard
        sudo install -m 755 /tmp/blockyard /usr/local/bin/blockyard
    - name: Assert baseline — profile unloaded, rootless bwrap blocked
      run: |
        sudo useradd -m -u 2000 blockyard-runner
        [ "$(sysctl -n kernel.apparmor_restrict_unprivileged_userns)" = 1 ] \
          || { echo "sysctl expected to be 1 on the runner"; exit 1; }
        # Expected to fail: no profile grants userns, sysctl=1 blocks.
        if sudo -u blockyard-runner /usr/local/bin/blockyard bwrap-smoke; then
          echo "FAIL: rootless bwrap succeeded without the profile — test is not exercising the restriction"
          exit 1
        fi
    - name: Load profile, assert rootless bwrap unblocked
      run: |
        sudo install -m 644 internal/apparmor/blockyard /etc/apparmor.d/blockyard
        sudo apparmor_parser -r /etc/apparmor.d/blockyard
        sudo aa-status | grep -q 'blockyard' \
          || { echo "profile did not load"; exit 1; }
        sudo -u blockyard-runner /usr/local/bin/blockyard bwrap-smoke
```

The two-step assertion is the point: step 3 proves the restriction
is active (rootless bwrap fails unprofiled), step 4 proves the
profile lifts it. Without both, a green result means nothing — the
environment might be quietly permissive.

Net: `process` matrix goes from 3 legs to 2 (drop `setuid`). The
standalone `apparmor-smoke` job adds ~2 minutes of CI time for the
only coverage that can meaningfully validate the shipped profile.
The existing `requireHostUIDMapping` test helper classification is
unchanged.

### Step 8: Documentation

`docs/design/backends.md` adds an isolation-layer matrix:

```
| Layer                         | Mechanism                             | Root | Rootless | k8s pod |
|-------------------------------|---------------------------------------|------|----------|---------|
| 1 Filesystem view             | bwrap --ro-bind + --tmpfs             | ✓    | ✓        | ✓       |
| 2 PID namespace               | bwrap --unshare-pid                   | ✓    | ✓        | ✓       |
| 3 Capabilities                | bwrap --cap-drop ALL                  | ✓    | ✓        | ✓       |
| 4 Seccomp                     | bwrap --seccomp                       | ✓    | ✓        | ✓       |
| 5 In-sandbox UIDs             | bwrap --uid                           | ✓    | ✓        | ✓       |
| 6 Per-worker host kuid        | fork+setuid+exec(bwrap), --uid W      | ✓    | ✗        | n/a     |
| 6' Per-worker cgroup (v2)     | cgroup subtree + iptables -m cgroup   | ✓    | ✓¹       | n/a²    |
| 7 Per-worker network namespace| (not used; Docker backend instead)    | —    | —        | —       |

¹ Requires cgroup-v2 delegation (systemd: Delegate=yes).
² Restricted k8s pods lack CAP_NET_ADMIN for host-iptables-in-pod
  rules; use Docker backend's per-worker-netns for per-worker egress.
```

`docs/content/docs/guides/process-backend.md` (phase 3-8's native
guide) gains a "Per-worker egress on non-root hosts" section:

- systemd unit template with `Delegate=yes`:
  ```
  [Service]
  Delegate=yes
  DelegateSubgroup=workers
  User=blockyard
  ExecStart=/usr/bin/blockyard --config /etc/blockyard/blockyard.toml
  ```
- Preflight verifies delegation via `checkCgroupDelegation`.
- iptables recipe:
  ```
  CGPATH=system.slice/blockyard.service/workers
  iptables -A OUTPUT -m cgroup --path $CGPATH -d 169.254.169.254 -j REJECT
  iptables -A OUTPUT -m cgroup --path $CGPATH -d <redis-ip>       -j REJECT
  ```
- Caveats: cgroup-v2 unified hierarchy required (`grep cgroup2 /proc/mounts`);
  `xt_cgroup` module required (`modprobe xt_cgroup`).

`docs/content/docs/guides/process-backend-container.md` (phase 3-8's
containerized guide) gains a "Rootless containers" subsection:

- How to load the shipped AppArmor profile on the host
  (`docker run --entrypoint cat ... | apparmor_parser -r -`).
- Pod-running-as-non-root configuration example.
- Note that layer 6 is unavailable in non-root containers; steer
  to root-blockyard-in-container, cgroup delegation, or Docker
  backend.

### Step 9: Tests

**`internal/apparmor` tests.** Embed integrity: the Go package's
`Profile` matches the on-disk source. A small `TestEmbedMatchesFile`
reads `blockyard` and compares to `Profile`.

**`internal/backend/process/cgroup_test.go`.** Unit tests for
`detectCgroupDelegation` against fixture `/proc/self/cgroup` contents
(v1 hybrid, v2 unified without write access, v2 unified with write
access). `ensureWorkersSubcgroup` idempotency test against a
temporary cgroup-v2 mount. `Enroll` failure path logs a warning but
doesn't panic.

**Integration test** (tagged `process_test`):
`TestCgroupEnrollment` — when `detectCgroupDelegation` succeeds on
the test host, spawn a worker and verify its PID appears in
`<workers>/cgroup.procs`. Skipped when delegation is unavailable
(CI `root` leg inside `--privileged` container may not have a
writable unified hierarchy).

**Preflight integration tests** — extend the existing
`preflight_internal_test.go` and `preflight_unit_test.go` with fixture
cases for the three new checks. `checkCloudMetadataReachable` uses an
injectable dialer. `CheckRedisAuth` uses miniredis with/without AUTH
configured. `checkCgroupDelegation` tests both outcomes.

**`blockyard bwrap-smoke` subcommand.** Unit test that mocks the
bwrap binary (or points at a stub) and asserts exit-code propagation.
The real production-path validation is the standalone
`apparmor-smoke` CI job (Step 7), which exercises the subcommand
end-to-end against a loaded profile on an Ubuntu 24.04 VM.

**CI standalone `apparmor-smoke` job.** The negative/positive pair
described in Step 7 is itself the regression test: profile-unloaded
run fails, profile-loaded run succeeds. No additional Go tests —
the job's two assertion steps are the test.

---

## Files changed

New:
- `internal/apparmor/blockyard` (profile source)
- `internal/apparmor/apparmor.go` (embed + constants)
- `internal/apparmor/apparmor_test.go` (embed integrity)
- `internal/backend/process/cgroup.go` (delegation manager)
- `internal/backend/process/cgroup_test.go`
- `internal/preflight/redis_auth.go` (cross-backend Redis AUTH probe)
- `internal/preflight/redis_auth_test.go`

Modified:
- `cmd/by/admin.go` (new `install-apparmor` subcommand)
- `cmd/blockyard/main.go` (new `bwrap-smoke` subcommand)
- `internal/backend/process/process.go` (cgroup manager field,
  Enroll call in Spawn)
- `internal/backend/process/preflight.go` (three new checks,
  `checkBwrapHostUIDMapping` message refresh, `RunPreflight` +
  `checkWorkerEgress` + `probeReachable` signatures gain
  `*cgroupManager` for probe enrollment into `workers/`)
- `internal/backend/process/process_integration_test.go` (new
  TestCgroupEnrollment; refresh `requireHostUIDMapping` skip message
  at line 102 — currently promises "phase 3-9 ships --userns+newuidmap"
  which this phase explicitly does not do)
- `internal/config/config.go` (new `skip_metadata_check` field on
  `ProcessConfig`)
- `docker/server-process.Dockerfile`, `docker/server-everything.Dockerfile`
  (COPY profile into the bwrap-capable variants; `docker/server.Dockerfile`
  is the Docker-backend-only variant and does not need the profile)
- `.github/workflows/ci.yml` (drop `setuid` leg from the process
  matrix; add standalone `apparmor-smoke` job on the VM)
- `.github/workflows/release.yml` (extend the `seccomp-blob` job's
  uploads to include the AppArmor profile; rename to reflect the
  broader scope if desired)
- `docs/design/backends.md` (isolation-layer matrix)
- `docs/content/docs/guides/process-backend.md` (cgroup section)
- `docs/content/docs/guides/process-backend-container.md` (rootless
  subsection)
- `docs/design/v3/phase-3-7.md` (corrections: remove the setuid-bwrap
  mode description — mis-documented as a valid mode; rewrite the
  several "wait for phase 3-9's `--userns`+`newuidmap`" forward-
  references (lines ~101, 423-432, 3014-3015) to point at the
  cgroup-delegation mechanism this phase actually delivers; retitle
  the stale "Step 9: Phase 3-9 (zygote workers) forward compatibility"
  section (line 2377) to "Step 9: v4 zygote-worker forward
  compatibility" — phase 3-9 is no longer the zygote-workers phase,
  but the three contracts that section documents are still v4
  prerequisites)

Unchanged (explicit):
- `internal/backend/process/bwrap_exec.go` (#305 shim stays)
- `internal/backend/process/bwrap.go` `bwrapExecSpec` (routing stays)
- `internal/backend/process/preflight.go`
  `checkBwrapHostUIDMapping`'s root-path probe logic (only the
  non-root message text changes)

---

## Design decisions

### Root path stays on #305's mechanism

An earlier draft proposed rebuilding the root path: pre-unshare the
userns while still root (short-circuiting AppArmor via CAP_SYS_ADMIN
in init_userns), pass the ns fd to bwrap via `--userns`, and let
bwrap's `--uid W --gid G` complete the worker identity. Empirical
testing on Ubuntu 24.04 rejected this: bwrap's `--userns` code path
does `setuid(W)` before `setgid(G)`, and the `setuid` call drops
CAP_SETGID before `setgid(G)` can run, EPERM'ing the call with
"unable to switch to gid G: Operation not permitted".

#305's existing mechanism — fork+setuid(W,G) in a shim, then exec
bwrap with `--uid W --gid G --unshare-user` — sidesteps the bug
because bwrap sees `real_uid == opt_sandbox_uid` and skips its own
(broken) setuid/setgid block. The trade-off is that bwrap's internal
post-setuid `unshare(CLONE_NEWUSER)` runs from a non-root process,
which fires the Ubuntu 23.10+ AppArmor restriction. The operator
remediation — sysctl override or AppArmor profile — is what this
phase's AppArmor profile addresses.

The two alternatives compared:

| Approach | Root path works? | Ubuntu 23.10+ requires sysctl/profile? | Layer 6 works? |
|---|---|---|---|
| #305 as-is (kept) | ✓ | Yes | ✓ |
| Draft's pre-unshare + `--userns` | ✗ (bwrap bug) | No (if it worked) | ✓ (if it worked) |

Keeping #305 and shipping the AppArmor profile gives us the Ubuntu
23.10+ remedy without inheriting an upstream bwrap bug.

### Non-root layer 6 via cgroup-v2 delegation, not newuidmap

The newuidmap path (pre-create userns as non-root, write multi-extent
map via setuid-root `newuidmap`, pass fd to bwrap via `--userns`)
hits the same bwrap `--userns + --uid + --gid` bug as the root pre-
unshare variant. Workarounds require either upstream bwrap changes
(not on a timeline we control) or vendoring a patched bwrap (ongoing
maintenance burden this project shouldn't take on).

Cgroup-v2 delegation is orthogonal: it doesn't touch UIDs or user
namespaces. Blockyard moves worker PIDs into a `workers/` subcgroup
and operators write iptables `-m cgroup --path` rules matching that
subtree. Works for both root and non-root blockyard. The cost is
cgroup-v2 delegation setup, which on systemd-managed hosts is a
single `Delegate=yes` directive. On container runtimes without
delegation support, the check reports unavailable and the deployment
falls back to layers 1–5.

This decision re-scopes phase 3-9 materially. The original scope
("native non-root egress isolation via `--userns` + newuidmap") is
replaced by "an AppArmor profile that enables rootless layers 1–5,
plus cgroup-based layer 6 for hosts where delegation is available".
Operators who need per-worker egress isolation on deployment modes
where neither applies (rootless containers without delegation,
restricted k8s pods) are directed to the Docker backend.

### k8s steers to Docker backend for per-worker egress

Layer-6 via iptables inside a k8s pod requires CAP_NET_ADMIN — not
available in restricted pods. Layer-6 via cgroup delegation inside a
pod requires both CAP_NET_ADMIN (for the iptables rule) and a
delegated cgroup subtree inside the pod (not default). Restricted
k8s pods have neither.

For k8s deployments requiring per-worker egress isolation, the
Docker backend's pod-per-worker model plus NetworkPolicy is the
supported path. Phase 3-9 documents this explicitly rather than
papering over it with a process-backend mechanism that only works
on privileged pods.

### AppArmor profile over sysctl override

The sysctl `kernel.apparmor_restrict_unprivileged_userns=0` disables
the restriction for every unprivileged process on the host.
Operators applying it lose AppArmor's control over every other
unprivileged service and workload. The shipped profile is the
narrow equivalent: grants `userns` to blockyard's binary and its
subprocess inheritance chain, leaves the restriction in place for
everything else.

For CI specifically, the sysctl override remains in the `root`
matrix leg because GHA container jobs can't load AppArmor profiles
in a way that covers the test binary's path (the loaded profile
attaches by path; `/github/workspace/...` isn't the profile's
expected path). This is a CI-environment artefact, not a production
recommendation.

### Profile inherits through `ix`, not `Ux`

The profile's subprocess transitions use `ix` (inherit, keep the
current profile) rather than `Ux` (unconfined). This matters for
bwrap specifically: bwrap's own `--unshare-user` internally calls
`unshare(CLONE_NEWUSER)` from its forked sandbox-setup child, which
would fire the restriction again if that child were unconfined. With
`ix`, the child runs under the blockyard profile and inherits the
`userns` grant.

The side effect is that the worker R interpreter runs under the
blockyard profile too. This is acceptable because the profile's
rules are broad (capability, network, mount, etc.) and the worker's
actual confinement comes from bwrap's capability dropping, seccomp
filter, and filesystem bind-mount restrictions — not from AppArmor.
Operators wanting tighter AppArmor control over workers can layer
a stricter site-specific profile on top.

### Preflight as the layer-6 fallback

Much of layer 6's historical value was implicit defence against
operator misconfiguration — blocking workers from reaching a
public cloud metadata endpoint, or an unauth'd Redis. Phase 3-9
makes those defences explicit:

- `checkCloudMetadataReachable` probes from blockyard's own
  process. If blockyard can reach metadata, workers can too; the
  check produces an Error prompting the operator to install the
  host-wide block rule.
- `checkRedisAuth` probes for unauth'd access. Tests with a raw
  `PING`, which Redis accepts without AUTH when `requirepass` is
  unset.

Neither defends against every possible misconfiguration, but they
target the two most-cited layer-6 attack vectors from the phase-3-9
draft discussion. Operators running root with layer 6 still benefit
— "kernel-level + preflight" is strictly stronger than "kernel-level
only" — and non-root deployments without layer 6 gain a defence that
would otherwise rely on operator vigilance.

### Cgroup enrollment is best-effort

Spawn doesn't fail if `Enroll(pid)` errors. The worker is
functionally correct without the cgroup move — only the iptables
rule fails to match, which is already the non-root-without-
delegation situation the deployment mode accepts. Strict enforcement
would surface cgroup quirks (delegated-but-read-only subtree,
cgroup namespaces, transient write races on `cgroup.procs`) as
spawn errors, which is worse than a best-effort warning.

The check `checkCgroupDelegation` reports the chosen mode at
startup, so operators see "cgroup delegation unavailable" once
rather than as a spawn-time surprise. If future work tightens this
to "fail spawn when delegation was expected but the write failed",
the plumbing point is the `Enroll` function — easy to make
strict later without restructuring.

---

## Deferred

- **Non-root layer-6 via `--userns` + newuidmap.** Blocked on bwrap's
  `--userns + --uid + --gid` setuid-before-setgid bug. Revisit if
  (a) upstream bwrap lands the fix, or (b) operator demand justifies
  the ambient-caps + inner-wrapper workaround complexity. Cgroup-v2
  delegation covers most of the motivating use case for now.
- **Deb/rpm packaging for the AppArmor profile.** Following phase-3-8's
  decision to ship via Docker images + CLI install + release assets.
  Deb/rpm adds packaging infrastructure this track doesn't otherwise
  need.
- **Per-worker cgroup (one cgroup per worker rather than one shared
  `workers/` cgroup).** Would enable per-worker cgroup memory/CPU
  limits, matching the Docker backend's per-container limits. Today
  the process backend intentionally does not enforce per-worker
  resource limits (phase 3-7 decision #6). Revisit together if the
  decision is ever revisited.
- **AppArmor profile tightening.** The shipped profile is permissive;
  it grants blockyard broad filesystem and capability access because
  confining blockyard itself isn't the goal (blockyard is the trusted
  component). Operators wanting a tighter profile can layer a
  site-specific one on top; future work could ship an optional
  `blockyard-strict` profile variant.
