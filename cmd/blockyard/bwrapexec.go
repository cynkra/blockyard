package main

import (
	"flag"
	"fmt"
	"os"
	"syscall"
)

// runBwrapExec is the `blockyard bwrap-exec --uid W --gid G -- <bwrap>
// <bwrap-args>` subcommand used by the process backend when blockyard
// runs as root. It drops into the worker's (uid, gid), restores the
// dumpable flag that the kernel clears on suid transitions, and execs
// bwrap.
//
// Why this isn't `SysProcAttr.Credential`: Go's fork+exec does
// setgroups+setgid+setuid but never calls
// `prctl(PR_SET_DUMPABLE, 1)` afterward. The kernel marks the process
// non-dumpable after any credential transition (even root→user), and a
// non-dumpable process sees `/proc/self/uid_map` as owned by root
// (not the current ruid). bwrap's unprivileged uid_map write then
// fails with EPERM — `bwrap: setting up uid map: Permission denied`.
// Restoring dumpable between setuid and exec makes /proc/self/uid_map
// owned by the worker UID again, so bwrap can write the single-line
// identity map that makes worker traffic host-identifiable for
// iptables owner-match rules.
//
// Exits nonzero with a stderr error on any misuse; on success it
// never returns (execve replaces the process).
func runBwrapExec(args []string) error {
	fs := flag.NewFlagSet("bwrap-exec", flag.ContinueOnError)
	uid := fs.Int("uid", -1, "setuid target (required)")
	gid := fs.Int("gid", -1, "setgid target (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *uid < 0 || *gid < 0 {
		return fmt.Errorf("bwrap-exec: --uid and --gid are required")
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("bwrap-exec: missing bwrap path after --")
	}
	bwrapPath := rest[0]
	bwrapArgs := rest[1:]

	// Clear supplementary groups, setgid, setuid. Order matters:
	// setgid/setgroups require CAP_SETGID which a setuid'd non-root
	// process no longer has.
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("bwrap-exec: setgroups: %w", err)
	}
	if err := syscall.Setgid(*gid); err != nil {
		return fmt.Errorf("bwrap-exec: setgid(%d): %w", *gid, err)
	}
	if err := syscall.Setuid(*uid); err != nil {
		return fmt.Errorf("bwrap-exec: setuid(%d): %w", *uid, err)
	}
	// Restore dumpable so /proc/self/uid_map is owned by the new
	// ruid, not root. PR_SET_DUMPABLE = 4 (linux/prctl.h), value 1 =
	// SUID_DUMP_USER.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 4, 1, 0); errno != 0 {
		return fmt.Errorf("bwrap-exec: prctl(PR_SET_DUMPABLE, 1): %w", errno)
	}

	// execve replaces the process; args[0] is conventionally the
	// program name for downstream ps/logging. bwrapPath comes in
	// over our own argv from the process backend, which only calls
	// this shim with the validated cfg.BwrapPath — same trust
	// boundary as the other bwrap exec.Command sites.
	argv := append([]string{bwrapPath}, bwrapArgs...)
	return syscall.Exec(bwrapPath, argv, os.Environ()) //nolint:gosec // G204: bwrapPath is from the validated process config
}

