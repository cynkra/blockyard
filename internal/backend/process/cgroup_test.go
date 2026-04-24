package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEnrollNoOpWhenUnavailable. The cgroup manager must be safe to
// call with an empty workersPath and on a nil receiver — the spawn
// path invokes Enroll unconditionally and the cgroup-unavailable
// case is the common one on many production hosts. A panic here
// would break worker spawn; a log-and-continue keeps layer 6 as a
// pure enhancement.
func TestEnrollNoOpWhenUnavailable(t *testing.T) {
	(*cgroupManager)(nil).Enroll(1)
	(&cgroupManager{}).Enroll(1)
	(&cgroupManager{workersPath: ""}).Enroll(1)
}

// TestEnsureWorkersSubcgroupIdempotent. Spawn paths and restarts
// hit ensureWorkersSubcgroup repeatedly; a double-mkdir must not
// fail. Uses a temp dir as a stand-in for the delegated cgroup
// root — ensureWorkersSubcgroup does nothing cgroup-specific, it
// just mkdir-p's the `workers/` subdirectory.
func TestEnsureWorkersSubcgroupIdempotent(t *testing.T) {
	root := t.TempDir()
	w1, err := ensureWorkersSubcgroup(root)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	w2, err := ensureWorkersSubcgroup(root)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if w1 != w2 {
		t.Errorf("inconsistent path: %q vs %q", w1, w2)
	}
	if filepath.Base(w1) != "workers" {
		t.Errorf("expected path ending in /workers, got %q", w1)
	}
	info, err := os.Stat(w1)
	if err != nil {
		t.Fatalf("stat %q: %v", w1, err)
	}
	if !info.IsDir() {
		t.Errorf("%q is not a directory", w1)
	}
}

// TestEnrollWritesPIDToProcsFile. The real kernel cgroup.procs file
// ignores mode bits and magically moves PIDs; a regular file in a
// temp directory accepts arbitrary writes. Good enough to confirm
// we write to <workers>/cgroup.procs with the pid as ASCII. A
// deeper test would need a real delegated cgroup and root, both
// out of scope for unit tests.
func TestEnrollWritesPIDToProcsFile(t *testing.T) {
	workers := t.TempDir()
	procs := filepath.Join(workers, "cgroup.procs")
	if err := os.WriteFile(procs, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &cgroupManager{workersPath: workers}
	m.Enroll(4242)
	got, err := os.ReadFile(procs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strconv.Itoa(4242) {
		t.Errorf("cgroup.procs content = %q, want %q", got, "4242")
	}
}

// TestEnrollSurvivesMissingProcsFile. The write can fail in many
// real-world ways — read-only mount, cgroup removed under us,
// permissions. Enroll is best-effort: a failure logs a warning but
// must not panic or cause the caller to fail.
func TestEnrollSurvivesMissingProcsFile(t *testing.T) {
	m := &cgroupManager{workersPath: "/nonexistent/cgroup/workers"}
	m.Enroll(1234)
}

// TestDetectCgroupDelegationV1Hybrid. On cgroup-v1 hybrid hosts
// /proc/self/cgroup has multiple lines (one per controller). The
// detector must reject this and return "" so blockyard falls back
// to flat-cgroup behaviour — v1 doesn't have a usable delegated
// subtree for our purposes. Uses a writable file in place of
// /proc/self/cgroup by stubbing out the reader? No — the function
// reads /proc/self/cgroup directly. On a cgroup-v2-only test host
// this test is trivially a no-op; we still want to exercise the
// function end-to-end at least once.
func TestDetectCgroupDelegationDoesNotPanic(t *testing.T) {
	// Run on whatever the test host is. The function has three
	// return paths: ("", nil) for "not delegated", (path, nil) for
	// "delegated and writable", ("", err) for IO errors. Any of the
	// three is acceptable; we're guarding against panics / type
	// confusion / uncaught errors.
	_, _ = detectCgroupDelegation()
}

// TestNewCgroupManagerSafe. Startup calls newCgroupManager()
// unconditionally. A nil return would break the Spawn path (the
// receiver would be nil and methods would be called), so the
// contract is: always returns a non-nil *cgroupManager, even on
// detection error.
func TestNewCgroupManagerSafe(t *testing.T) {
	m, _ := newCgroupManager()
	if m == nil {
		t.Error("newCgroupManager returned nil; must always return non-nil")
	}
}
