package process

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// RunBwrapExec is the `blockyard bwrap-exec --uid W --gid G -- <bwrap>
// <bwrap-args>` subcommand the process backend invokes instead of
// calling bwrap directly. It drops into the worker's (uid, gid),
// restores the dumpable flag that the kernel clears on suid
// transitions, and execs bwrap.
//
// Why this isn't `SysProcAttr.Credential`: Go's fork+exec does
// setgroups+setgid+setuid but never calls
// `prctl(PR_SET_DUMPABLE, 1)` afterward. The kernel marks the
// process non-dumpable after any credential transition (even
// root→user), and a non-dumpable process sees /proc/self/uid_map as
// owned by root (not the current ruid). bwrap's unprivileged
// uid_map write then fails with EPERM — "bwrap: setting up uid map:
// Permission denied". Restoring dumpable between setuid and exec
// makes /proc/self/uid_map owned by the worker UID again, so bwrap
// can write the identity uid_map that makes worker traffic
// host-identifiable for iptables owner-match rules.
//
// Exported because both cmd/blockyard/main.go and the process
// package's TestMain dispatch to it: in tests, os.Executable()
// returns the test binary rather than the blockyard binary, so the
// test binary must also recognise the "bwrap-exec" first arg and
// execute the shim itself — otherwise exec.Command(testbin,
// "bwrap-exec", ...) re-enters the test runner and the whole suite
// recurses.
//
// Returns an error for any misuse; on success it never returns
// (execve replaces the process).
func RunBwrapExec(args []string) error {
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

	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("bwrap-exec: setgroups: %w", err)
	}
	if err := syscall.Setgid(*gid); err != nil {
		return fmt.Errorf("bwrap-exec: setgid(%d): %w", *gid, err)
	}
	if err := syscall.Setuid(*uid); err != nil {
		return fmt.Errorf("bwrap-exec: setuid(%d): %w", *uid, err)
	}
	// PR_SET_DUMPABLE = 4 (linux/prctl.h), value 1 = SUID_DUMP_USER.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, 4, 1, 0); errno != 0 {
		return fmt.Errorf("bwrap-exec: prctl(PR_SET_DUMPABLE, 1): %w", errno)
	}

	// Diagnostic dump — helps debug "bwrap: setting up uid map:
	// Permission denied" when the shim appears to complete but bwrap
	// still fails. Dumps the relevant /proc/self state right before
	// execve.
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(l, "Uid:") || strings.HasPrefix(l, "Gid:") || strings.HasPrefix(l, "Groups:") {
				fmt.Fprintf(os.Stderr, "bwrap-exec DEBUG: %s\n", l)
			}
		}
	}
	if d, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, 3, 0, 0); e == 0 {
		fmt.Fprintf(os.Stderr, "bwrap-exec DEBUG: dumpable=%d\n", d)
	}
	if fi, err := os.Stat("/proc/self/uid_map"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			fmt.Fprintf(os.Stderr, "bwrap-exec DEBUG: /proc/self/uid_map owner=%d:%d mode=%o\n", st.Uid, st.Gid, fi.Mode().Perm())
		}
	}

	argv := append([]string{bwrapPath}, bwrapArgs...)
	return syscall.Exec(bwrapPath, argv, os.Environ()) //nolint:gosec // G204: bwrapPath is from the validated process config
}
