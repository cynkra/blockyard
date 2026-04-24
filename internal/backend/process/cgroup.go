package process

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cgroupManager coordinates cgroup-v2 delegation for the process
// backend. When the host delegates a v2 subtree to blockyard, the
// manager creates `<delegated>/workers/` and exposes Enroll(pid) for
// the spawn path to move each worker into it. When delegation is
// unavailable, manager.workersPath is empty and Enroll is a no-op.
type cgroupManager struct {
	workersPath string // "" when delegation unavailable
}

// newCgroupManager probes for cgroup-v2 delegation and, on success,
// creates the workers subcgroup. A detection error is non-fatal:
// blockyard starts without delegation and the layer-6 preflight
// reports the gap.
func newCgroupManager() (*cgroupManager, error) {
	root, err := detectCgroupDelegation()
	if err != nil {
		return &cgroupManager{}, err
	}
	if root == "" {
		return &cgroupManager{}, nil
	}
	workers, err := ensureWorkersSubcgroup(root)
	if err != nil {
		return &cgroupManager{}, err
	}
	return &cgroupManager{workersPath: workers}, nil
}

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
	text := strings.TrimRight(string(data), "\n")
	// cgroup-v2 unified: single line "0::/<path>".
	// cgroup-v1 hybrid: multiple lines with controllers; skip.
	if strings.Contains(text, "\n") {
		return "", nil
	}
	if !strings.HasPrefix(text, "0::") {
		return "", nil
	}
	cgPath := strings.TrimPrefix(text, "0::")
	fullPath := filepath.Join("/sys/fs/cgroup", cgPath)

	probe := filepath.Join(fullPath, ".blockyard-delegation-probe")
	if err := os.Mkdir(probe, 0o755); err != nil { //nolint:gosec // G301: transient test dir, cleaned below
		if errors.Is(err, os.ErrPermission) {
			return "", nil
		}
		return "", fmt.Errorf("probe subcgroup: %w", err)
	}
	_ = os.Remove(probe) //nolint:gosec // G703: path rooted at /sys/fs/cgroup + cgroup from /proc/self/cgroup
	return fullPath, nil
}

// ensureWorkersSubcgroup creates <cgRoot>/workers/. Idempotent.
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
	if err := os.MkdirAll(workers, 0o755); err != nil { //nolint:gosec // G301: delegated cgroup dir
		return "", fmt.Errorf("mkdir workers subcgroup: %w", err)
	}
	return workers, nil
}

// Enroll moves pid into the workers subcgroup. Best-effort: a write
// failure logs a warning and continues. The spawn path must tolerate
// cgroup move failures because the worker is functionally correct
// without the move — only the cgroup-based iptables rule fails to
// match, which is already the non-root layer-6 gap.
//
// Single-PID: descendants forked before the write stay in their
// original cgroup (cgroup-v2 cgroup.procs only moves the thread
// group whose PID is written). Callers that need the whole worker
// tree — which is every caller spawning bwrap — should use
// EnrollTree instead.
//
// Safe on a nil receiver and on a manager with no delegated subtree.
func (m *cgroupManager) Enroll(pid int) {
	if m == nil || m.workersPath == "" {
		return
	}
	m.enroll(pid)
}

// EnrollTree enrolls pid and any descendants into the workers
// subcgroup. Required for the bwrap spawn path because cgroup-v2
// cgroup.procs only moves the single tgid written — bwrap's inner
// fork(s) produce descendant tgids that stay in their fork-time
// cgroup unless we catch them explicitly. The walk polls
// /proc/<pid>/task/<tid>/children briefly since bwrap's first fork
// typically lands 10–50 ms after exec and may not be visible at
// our first look.
//
// Polling stops early after enrollTreeStableRounds iterations
// without new descendants (typical fast path: ~20 ms) and always
// stops at enrollTreeMaxPoll (worst case: ~100 ms). Best-effort:
// see Enroll for the severity rationale.
//
// Safe on a nil receiver and on a manager with no delegated subtree.
func (m *cgroupManager) EnrollTree(pid int) {
	if m == nil || m.workersPath == "" {
		return
	}
	m.enroll(pid)

	seen := map[int]bool{pid: true}
	deadline := time.Now().Add(enrollTreeMaxPoll)
	stable := 0
	for time.Now().Before(deadline) {
		added := 0
		for _, child := range collectDescendants(pid) {
			if !seen[child] {
				seen[child] = true
				m.enroll(child)
				added++
			}
		}
		if added == 0 {
			stable++
			if stable >= enrollTreeStableRounds {
				return
			}
		} else {
			stable = 0
		}
		time.Sleep(enrollTreePollInterval)
	}
}

// enroll writes one PID to cgroup.procs. Shared primitive used by
// both Enroll and EnrollTree.
func (m *cgroupManager) enroll(pid int) {
	procsFile := filepath.Join(m.workersPath, "cgroup.procs")
	if err := os.WriteFile(procsFile, []byte(strconv.Itoa(pid)), 0); err != nil {
		slog.Warn("process backend: cgroup enroll failed",
			"pid", pid, "path", m.workersPath, "err", err)
	}
}

// EnrollTree tuning. Exposed as package-level consts so tests can
// temporarily shorten them.
const (
	enrollTreePollInterval  = 5 * time.Millisecond
	enrollTreeMaxPoll       = 100 * time.Millisecond
	enrollTreeStableRounds  = 3
)
