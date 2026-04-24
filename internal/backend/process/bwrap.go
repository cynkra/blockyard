package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

// bwrapSysProcAttr returns the SysProcAttr that must be set on every
// bwrap invocation (Spawn, build, preflight probes). The only field
// set is Pdeathsig — the setuid into the worker (uid, gid) happens in
// the `blockyard bwrap-exec` shim reached via bwrapExecSpec, not via
// Go's SysProcAttr.Credential. Go's fork+exec does
// setgroups+setgid+setuid but does not restore dumpable afterward, so
// the child is non-dumpable; /proc/self/uid_map is then owned by
// root (not the worker uid), and bwrap's unprivileged map write
// fails with EPERM ("bwrap: setting up uid map: Permission denied").
// The shim does the setuid + prctl(PR_SET_DUMPABLE, 1) sequence
// itself, so bwrap runs dumpable and the identity uid_map write
// succeeds.
//
// Pdeathsig: SIGKILL so bwrap (and its sandboxed descendants) die
// when the blockyard server exits. bwrap's own --die-with-parent only
// kills the R child when bwrap exits; it does NOT kill bwrap when
// blockyard exits.
func bwrapSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// bwrapExecSpec returns the program and argv to invoke bwrap with the
// given worker (uid, gid). When blockyard runs as root the command is
// routed through the `blockyard bwrap-exec` shim (same binary) so the
// forked child drops into (uid, gid), restores dumpable via
// prctl(PR_SET_DUMPABLE, 1), and execs bwrap — giving bwrap
// `caller_uid == sandbox_uid` and therefore the identity uid_map
// `uid uid 1` that makes `iptables --uid-owner $uid` rules match
// worker traffic.
//
// Non-root blockyard: bwrap runs directly. Setuid to a foreign UID
// fails without CAP_SETUID, so the `-m owner` path is inherently
// inapplicable in this deployment mode. Phase 3-9 added cgroup-v2
// delegation as an orthogonal layer-6 mechanism (see cgroup.go and
// checkCgroupDelegation); a `--userns + newuidmap` path was
// investigated and rejected on an upstream bwrap bug, see
// docs/design/v3/phase-3-9.md.
//
// The bwrap path is always resolved to an absolute path via $PATH
// before being passed on: the shim path uses syscall.Exec which does
// not do PATH lookup, and callers routinely pass a bare "bwrap" from
// the default config. The non-shim path would also accept a bare
// name (exec.Command handles PATH lookup itself), but resolving here
// keeps the two branches symmetric.
func bwrapExecSpec(bwrapPath string, uid, gid int, bwrapArgs []string) (prog string, argv []string, err error) {
	resolvedBwrap, err := exec.LookPath(bwrapPath)
	if err != nil {
		return "", nil, fmt.Errorf("resolve bwrap binary %q: %w", bwrapPath, err)
	}
	if os.Getuid() != 0 {
		return resolvedBwrap, bwrapArgs, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("resolve blockyard binary for bwrap-exec shim: %w", err)
	}
	argv = make([]string, 0, 6+len(bwrapArgs))
	argv = append(argv, "bwrap-exec",
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--", resolvedBwrap,
	)
	argv = append(argv, bwrapArgs...)
	return self, argv, nil
}

// bwrapArgs constructs the bwrap command-line arguments for a worker.
// uid is the host UID this worker runs as (allocated from the worker
// UID pool); gid is the shared host GID for all workers (used by the
// operator's destination-scoped egress firewall rules). Together they
// let operators install rules like
// `iptables -m owner --gid-owner $worker_gid -d <internal-ip> -j REJECT`
// to block worker access to specific internal services without
// affecting blockyard or blocking the open internet.
//
// For the host UID/GID to actually take effect (so iptables owner
// match works), blockyard must run as root — the spawn path then
// fork+setuid's the child to uid before exec(bwrap), producing an
// identity uid_map. See bwrapSysProcAttr and checkBwrapHostUIDMapping.
func bwrapArgs(_ *config.ProcessConfig, spec backend.WorkerSpec, port, uid, gid int) []string {
	args := []string{
		// Namespace isolation
		"--unshare-pid",
		"--unshare-user",
		"--unshare-uts",

		// Host identity — workers run as a per-worker UID and a shared
		// GID. The UID gives per-worker filesystem isolation; the GID
		// is the target of the operator's egress firewall rule.
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),

		// Process lifecycle
		"--die-with-parent",
		"--new-session",

		// Filesystem: read-only bind of the entire host root.
		// In containerized mode this is the outer container's rootfs.
		// In native mode this is the host filesystem (read-only).
		"--ro-bind", "/", "/",

		// Writable scratch space (shadows the read-only /tmp).
		"--tmpfs", "/tmp",

		// Virtual filesystems (shadow the read-only copies).
		"--proc", "/proc",
		"--dev", "/dev",

		// Working directory — /tmp is always writable (tmpfs above).
		// Without --chdir the inherited cwd may not be accessible
		// after --unshare-user remaps the UID.
		"--chdir", "/tmp",

		// App bundle — shadow with the specific bundle path.
		"--ro-bind", spec.BundlePath, spec.WorkerMount,
	}

	// R library (read-only) — mount target must match the Docker
	// backend's convention so the same R_LIBS env var resolves
	// correctly on either backend. Store-assembled library (phase 2-6)
	// mounts at /blockyard-lib-store; legacy per-bundle library
	// (phase 2-5) mounts at /blockyard-lib. Must not use /lib, which
	// shadows the system shared library directory.
	if spec.LibDir != "" {
		args = append(args, "--ro-bind", spec.LibDir, "/blockyard-lib-store")
	} else if spec.LibraryPath != "" {
		args = append(args, "--ro-bind", spec.LibraryPath, "/blockyard-lib")
	}

	// Worker token directory (read-only, optional) — mount target
	// /var/run/blockyard matches the Docker backend's convention.
	// Workers read /var/run/blockyard/token to authenticate to the
	// packages endpoint.
	if spec.TokenDir != "" {
		args = append(args, "--ro-bind", spec.TokenDir, "/var/run/blockyard")
	}

	// Transfer directory (read-write, optional) — mount target /transfer
	// matches the Docker backend's convention. Workers read the handoff
	// file via the BLOCKYARD_TRANSFER_PATH env var.
	if spec.TransferDir != "" {
		args = append(args, "--bind", spec.TransferDir, "/transfer")
	}

	// Per-app data mounts (resolved host paths from app config).
	for _, dm := range spec.DataMounts {
		if dm.ReadOnly {
			args = append(args, "--ro-bind", dm.Source, dm.Target)
		} else {
			args = append(args, "--bind", dm.Source, dm.Target)
		}
	}

	// Capability dropping — bwrap drops all by default with --unshare-user,
	// but we explicitly drop to be defensive in case of flag changes.
	args = append(args, "--cap-drop", "ALL")

	// Separator and command
	args = append(args, "--")
	if len(spec.Cmd) > 0 {
		args = append(args, spec.Cmd...)
	} else {
		args = append(args,
			"Rscript", filepath.Join(spec.WorkerMount, "app.R"),
		)
	}

	return args
}

// bwrapBuildArgs constructs the bwrap arguments for a build task.
// Same root strategy as workers but with additional writable mounts
// for build output. uid/gid follow the same convention as bwrapArgs;
// builds use the next free worker UID and the same shared GID, so
// build egress is also covered by the operator's firewall rule.
func bwrapBuildArgs(_ *config.ProcessConfig, spec backend.BuildSpec, uid, gid int) []string {
	args := []string{
		"--unshare-pid",
		"--unshare-user",
		"--unshare-uts",

		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),

		"--die-with-parent",
		"--new-session",

		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--chdir", "/tmp",
	}

	// Build mounts — shadow specific paths with read-only or read-write
	// binds as needed. Read-write mounts (e.g., library output dir)
	// shadow the read-only root at that path.
	for _, m := range spec.Mounts {
		if m.ReadOnly {
			args = append(args, "--ro-bind", m.Source, m.Target)
		} else {
			args = append(args, "--bind", m.Source, m.Target)
		}
	}

	args = append(args, "--cap-drop", "ALL")
	args = append(args, "--")
	args = append(args, spec.Cmd...)

	return args
}

// applySeccomp opens the seccomp BPF profile and configures cmd to pass
// it via an inherited fd. bwrap's --seccomp flag expects a file descriptor
// number, not a path. The profile must be pre-compiled to BPF binary
// format (not the Docker/OCI JSON format). Phase 3-8 ships the compiled
// profile; this phase accepts it as a pre-compiled file.
//
// Returns:
//   - the bwrap args to splice before "--" in the command line
//   - a cleanup func that closes the parent-side fd; the caller must
//     defer it so the fd is released regardless of whether cmd.Start
//     succeeds or fails. The child gets its own duplicated fd from
//     fork(), so closing the parent's copy after Start does not
//     affect the sandboxed process.
//
// When profilePath is empty, returns (nil, no-op cleanup, nil) —
// seccomp is optional until phase 3-8 ships the compiled profile.
func applySeccomp(cmd *exec.Cmd, profilePath string) ([]string, func(), error) {
	noop := func() {}
	if profilePath == "" {
		return nil, noop, nil
	}
	f, err := os.Open(profilePath) //nolint:gosec // G304: path comes from validated config
	if err != nil {
		return nil, noop, fmt.Errorf("open seccomp profile: %w", err)
	}
	// cmd.ExtraFiles[i] is exposed to the child as fd 3+i.
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	fd := 3 + len(cmd.ExtraFiles) - 1
	return []string{"--seccomp", strconv.Itoa(fd)}, func() { f.Close() }, nil
}

// spliceBeforeSeparator inserts extra into cmd.Args just before the
// bwrap-level "--" separator (the one between bwrap flags and the
// inner command bwrap executes inside the sandbox). cmd.Args[0] is
// the program name set by exec.Command.
//
// We splice before the LAST "--" in cmd.Args: when blockyard routes
// bwrap through the bwrap-exec shim there are two "--" tokens (the
// shim's, separating shim flags from the bwrap invocation, and
// bwrap's own, separating bwrap flags from the inner command).
// seccomp args are bwrap flags, so they must go before bwrap's,
// which is always the last "--" in the combined argv as long as the
// inner command does not contain a literal "--" of its own.
//
// Falls back to appending if no separator is found, which shouldn't
// happen with well-formed bwrap args.
func spliceBeforeSeparator(cmd *exec.Cmd, extra []string) {
	lastSep := -1
	for i, arg := range cmd.Args {
		if arg == "--" {
			lastSep = i
		}
	}
	if lastSep < 0 {
		cmd.Args = append(cmd.Args, extra...)
		return
	}
	result := make([]string, 0, len(cmd.Args)+len(extra))
	result = append(result, cmd.Args[:lastSep]...)
	result = append(result, extra...)
	result = append(result, cmd.Args[lastSep:]...)
	cmd.Args = result
}
