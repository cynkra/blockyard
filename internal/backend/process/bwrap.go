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
// bwrap invocation (Spawn, build, preflight probes). It combines:
//
//   - Pdeathsig: SIGKILL — so bwrap (and its sandboxed descendants) die
//     when the blockyard server exits. bwrap's own --die-with-parent
//     only kills the R child when bwrap exits; it does NOT kill bwrap
//     when blockyard exits.
//   - Credential{Uid, Gid} when blockyard runs as root — the forked
//     child calls setgid(gid), setuid(uid) before exec(bwrap), so
//     bwrap sees caller_uid == sandbox_uid (its --uid flag) and writes
//     an identity uid_map `uid uid 1`. The sandboxed child's kuid in
//     init_userns is therefore `uid`, and the operator's iptables
//     `--uid-owner $uid` / `--gid-owner $gid` rules match worker
//     traffic.
//
// Non-root blockyard: Credential is not set — the kernel would reject
// setuid to a foreign uid. bwrap still writes uid_map `uid caller_uid 1`,
// and the sandboxed child appears as blockyard's own kuid in
// init_userns. The iptables owner-match mechanism does not work in
// this mode; checkBwrapHostUIDMapping surfaces this as an explicit
// preflight error that points operators at phase 3-9's --userns +
// newuidmap path (or the Docker backend).
func bwrapSysProcAttr(uid, gid int) *syscall.SysProcAttr {
	spa := &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if os.Getuid() == 0 {
		spa.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)} //nolint:gosec // G115: uid/gid are validated config
	}
	return spa
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
// "--" separator. cmd.Args[0] is the program name (set by exec.Command).
// Falls back to appending if no separator is found, which shouldn't
// happen with well-formed bwrap args.
func spliceBeforeSeparator(cmd *exec.Cmd, extra []string) {
	for i, arg := range cmd.Args {
		if arg == "--" {
			result := make([]string, 0, len(cmd.Args)+len(extra))
			result = append(result, cmd.Args[:i]...)
			result = append(result, extra...)
			result = append(result, cmd.Args[i:]...)
			cmd.Args = result
			return
		}
	}
	cmd.Args = append(cmd.Args, extra...)
}
