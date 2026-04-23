# Phase 3-9: Native Non-Root Egress Isolation (`--userns` + newuidmap)

Phase 3-7 introduced the process backend's worker-vs-internal-services
egress isolation via per-worker host UIDs and operator-installed
iptables `--uid-owner` / `--gid-owner` rules. Phase 3-7 + #305 made
that story actually work — but only for containerized deployments
where blockyard runs as root and the spawn path can fork+setuid into
the worker UID/GID before `exec(bwrap)`, producing an identity
uid_map.

Native non-root deployments (blockyard running as an ordinary user on
Debian 12+/Ubuntu 24.04+, or on Fedora/RHEL where the operator
declines to run blockyard as root) cannot use fork+setuid — the
kernel rejects `setuid(W)` without CAP_SETUID — and therefore get no
iptables-owner-match isolation. `checkBwrapHostUIDMapping` surfaces
this explicitly: non-root blockyard returns an `Error` severity
whose message points operators at this phase, at the Docker backend,
or at `server.skip_preflight=true` for deployments willing to run
with zero egress isolation.

This phase closes the gap. Blockyard, running as a non-root user
`B`, pre-creates a user namespace, writes uid_map/gid_map entries
mapping sandbox UIDs/GIDs into a `setuid(0)`-less subuid range
delegated to `B` via `/etc/subuid` / `/etc/subgid`, and passes that
namespace to bwrap via `--userns`. Workers' kuid in init_userns is
then a subuid — not blockyard's own UID, not the sandbox's
namespace-local UID — and operator iptables rules against those
subuids fire on worker traffic without requiring blockyard to be
root.

Depends on phases 3-7 and 3-8. Does not block either; containerized
deployments are unaffected.

---

## Background: why fork+setuid doesn't generalize

The fork+setuid shim from #305 gives bwrap `caller_uid == sandbox_uid`,
so bwrap writes an identity uid_map `W W 1`. That works because
`setuid(W)` from a root caller (blockyard running as UID 0 inside a
container) is a no-op-permissioned kernel call that just rewrites the
task's creds. From a non-root caller it is rejected with EPERM —
`setuid` can only drop to IDs in the caller's saved-uid set, which for
an unprivileged process is just its own UID.

The kernel does offer non-root processes a way to assign sandboxed
children foreign UIDs: delegate a subuid range via `/etc/subuid`,
then use `newuidmap`/`newgidmap` (setuid-root helpers shipped with
shadow-utils) to write a uid_map that spans that range. Inside the
new user namespace, subuids behave like ordinary UIDs; from init
userns they appear as the actual subuid values (e.g., 231072 +
offset). The iptables owner-match rule then targets those subuids
rather than `worker_uid`.

This phase wires that mechanism into the spawn path and the
preflight check, and extends the operator-facing documentation.

---

## Prerequisites from Earlier Phases

- **Phase 3-7** — process backend core: `ProcessBackend`, `bwrapArgs`,
  `bwrapSysProcAttr`, `uidAllocator`, `checkBwrapHostUIDMapping`, the
  `--uid`/`--gid` plumbing this phase changes. #305's Credential-based
  fork+setuid path remains for root-blockyard deployments; this phase
  adds a parallel non-root path.
- **Phase 3-8** — packaging: native deployment documentation this
  phase extends. The phase-3-8 `blockyard-process` image already
  ships as root inside the container and takes the fork+setuid path,
  so the image itself needs no changes.

---

## Deliverables

1. **`internal/backend/process/userns.go`** — new file. Exposes
   `prepareWorkerUserns(cfg, uid, gid) (nsFile *os.File, cleanup
   func(), err error)` which:
   - Calls `unshare(CLONE_NEWUSER)` in a forked helper.
   - Invokes `newuidmap <pid> <sandbox_uid> <subuid_start> 1` and
     `newgidmap <pid> <sandbox_gid> <subgid_start> 1` with
     per-worker offsets inside the configured subuid/subgid range.
   - Returns an open file descriptor on `/proc/<pid>/ns/user` that
     the spawn path passes to bwrap via `--userns N`.
   - Cleanup closes the fd and reaps the helper; the namespace is
     freed once bwrap exits and the fd is closed.

2. **Spawn-path wiring** — extend `bwrapArgs` and the Spawn/build
   call sites so that when `os.Getuid() != 0` and a subuid range is
   configured, they call `prepareWorkerUserns`, splice `--userns N`
   into the args, and pass the fd via `cmd.ExtraFiles`. When
   blockyard is root the existing fork+setuid Credential path stays.
   When blockyard is non-root and no subuid range is configured, the
   spawn proceeds without isolation (matching today's behavior) and
   the preflight check has already warned the operator at startup.

3. **`[process] worker_subuid_range_start` / `worker_subuid_range_end`
   config** — optional. When unset, blockyard calls `getsubids(3)`
   (or parses `/etc/subuid` directly) to derive the range from the
   invoking user's allocation. Explicit config wins so operators
   running multi-tenant boxes can carve up a shared range
   deterministically. The range must be ≥ the width of
   `worker_uid_range_start..end`; preflight rejects mismatches.

4. **`checkBwrapHostUIDMapping` non-root branch** — when
   `os.Getuid() != 0`:
   - If `newuidmap` is missing → `Error`, install instructions.
   - If no subuid range is configured or derivable → `Error`,
     `/etc/subuid` setup instructions.
   - If subuid range is narrower than worker UID range → `Error`,
     resize instructions.
   - Otherwise → spawn a bwrap probe through `prepareWorkerUserns`,
     read `/proc/<bwrap-pid>/status`, confirm Uid/Gid line reports
     the expected subuid values. `OK` on success. The existing
     root-blockyard branch from #305 is unchanged.

5. **`checkWorkerEgress` message** — remediation text gains a second
   example using subuid values:

   ```
   iptables -A OUTPUT -m owner --uid-owner <subuid_start..end> \
            -d <service-ip> -j REJECT
   ```

   The range syntax (not a single UID) reflects that each worker's
   subuid is a distinct value inside the range; operators typically
   install a rule per known-bad destination against the whole range.

6. **Tests** — `package process` internal test for
   `prepareWorkerUserns` (skipped unless `newuidmap` + a subuid range
   are available, same skip shape as the other process_test helpers);
   `package process_test` integration test that spawns a worker via
   the native non-root path and verifies the sandboxed child's kuid
   in init_userns sits inside the configured subuid range.

7. **CI matrix addition** — add `native-nonroot` to the existing
   `mode: [root, setuid, unprivileged]` matrix. It provisions a
   subuid range for the non-root test user in `/etc/subuid` during
   setup and runs the integration tests. Expected outcome: preflight
   OK, spawn produces kuid inside the subuid range. The existing
   `unprivileged` mode becomes the "no subuid provisioned"
   counterpart — preflight Error, lifecycle tests skip.

8. **Operator documentation** — extend `docs/backends.md` and the
   native-deployment guide in phase-3-8 with the subuid
   provisioning story: how to add an entry to `/etc/subuid` /
   `/etc/subgid`, how to size the range, what iptables rules look
   like against subuid values instead of `worker_uid`.

---

## Open questions for implementation

These don't need to be resolved before starting; list them here so the
implementer has a checklist of decisions to make.

1. **Namespace lifecycle.** bwrap `--userns N` joins an existing
   namespace. If the blockyard-side helper that unshared the
   namespace exits before bwrap joins, the namespace is freed and
   the join fails with ESRCH. Keeping the helper alive for the
   worker's lifetime is simple but multiplies the blockyard process
   count. An alternative is `setns`+`pidfd_open` trickery; pick one
   and document why.

2. **Subuid range allocation.** Do we allocate contiguously
   (`subuid_start + worker_uid_offset`) or randomly? Contiguous is
   simpler; random may be marginally friendlier to concurrent-server
   deployments sharing a subuid range. With Redis-backed UID
   allocation already in phase 3-8, contiguous inside the
   allocator's existing reservation is probably fine.

3. **`newuidmap` permission failures.** If `/etc/subuid` is present
   but the entries mismatch the running user, `newuidmap` fails with
   a generic error. The preflight check should surface a clearer
   diagnostic than "newuidmap exit status 1" — probably a followup
   helper that reads `/etc/subuid` itself and cross-references.

4. **setgroups.** bwrap's current flow writes `/proc/self/setgroups
   = deny` before writing gid_map, which is required in unprivileged
   userns. With `newgidmap` writing the map from outside,
   setgroups handling changes. Verify against shadow-utils behavior.

5. **Rollback to the pre-#305 silent-failure mode.** If an operator
   has `skip_preflight=true` and no subuid provisioning, they
   currently get workers at blockyard's own UID. Should phase 3-9
   preserve that fallback or make the spawn path hard-fail when
   isolation can't be set up? Probably preserve — this phase is
   additive — but call it out in the docs.
