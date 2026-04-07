package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

// newFakeBackend constructs a ProcessBackend without going through New()
// — the real New verifies bwrap is on PATH, which is not guaranteed in
// every environment. Tests that only exercise pure helpers or the
// methods that don't fork bwrap use this.
func newFakeBackend(t *testing.T) *ProcessBackend {
	t.Helper()
	cfg := &config.ProcessConfig{
		BwrapPath:      "/nonexistent/bwrap",
		RPath:          "/bin/sh",
		PortRangeStart: 20000,
		PortRangeEnd:   20099,
		WorkerUIDStart: 70000,
		WorkerUIDEnd:   70099,
		WorkerGID:      65534,
	}
	full := &config.Config{Process: cfg}
	return &ProcessBackend{
		cfg:     cfg,
		fullCfg: full,
		ports:   newPortAllocator(cfg.PortRangeStart, cfg.PortRangeEnd),
		uids:    newUIDAllocator(cfg.WorkerUIDStart, cfg.WorkerUIDEnd),
		workers: make(map[string]*workerProc),
	}
}

func TestNewRequiresProcessSection(t *testing.T) {
	_, err := New(&config.Config{})
	if err == nil {
		t.Fatal("expected error when Process config is nil")
	}
	if !strings.Contains(err.Error(), "config section is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewRejectsMissingBwrap(t *testing.T) {
	cfg := &config.Config{
		Process: &config.ProcessConfig{
			BwrapPath:      "/definitely/does/not/exist/bwrap",
			RPath:          "/bin/sh",
			PortRangeStart: 10000,
			PortRangeEnd:   10099,
			WorkerUIDStart: 60000,
			WorkerUIDEnd:   60099,
			WorkerGID:      65534,
		},
	}
	_, err := New(cfg)
	if err == nil {
		t.Fatal("expected error when bwrap path does not exist")
	}
	if !strings.Contains(err.Error(), "bwrap not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUpdateResourcesReturnsErrNotSupported(t *testing.T) {
	b := newFakeBackend(t)
	err := b.UpdateResources(context.Background(), "irrelevant", 1<<30, 1e9)
	if !errors.Is(err, backend.ErrNotSupported) {
		t.Errorf("expected ErrNotSupported, got %v", err)
	}
}

func TestCleanupOrphanResourcesIsNoop(t *testing.T) {
	b := newFakeBackend(t)
	if err := b.CleanupOrphanResources(context.Background()); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestListManagedEmpty(t *testing.T) {
	b := newFakeBackend(t)
	resources, err := b.ListManaged(context.Background())
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(resources) != 0 {
		t.Errorf("expected empty list, got %d entries", len(resources))
	}
}

func TestListManagedReturnsRegisteredWorker(t *testing.T) {
	b := newFakeBackend(t)
	// Inject a synthetic worker directly into the map. This avoids the
	// bwrap dependency of Spawn while still exercising the list path.
	b.workers["w-1"] = &workerProc{
		port: 20000,
		uid:  70000,
		spec: backend.WorkerSpec{
			WorkerID: "w-1",
			AppID:    "app-1",
			Labels:   map[string]string{"k": "v"},
		},
		done: make(chan struct{}),
	}
	resources, err := b.ListManaged(context.Background())
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(resources) != 1 || resources[0].ID != "w-1" || resources[0].Kind != backend.ResourceContainer {
		t.Errorf("unexpected resources: %+v", resources)
	}
	if resources[0].Labels["k"] != "v" {
		t.Errorf("labels not propagated: %+v", resources[0].Labels)
	}
}

func TestLookupMissingWorker(t *testing.T) {
	b := newFakeBackend(t)
	ctx := context.Background()

	if ok := b.HealthCheck(ctx, "missing"); ok {
		t.Error("HealthCheck should return false for unknown worker")
	}
	if _, err := b.Addr(ctx, "missing"); err == nil {
		t.Error("Addr should return error for unknown worker")
	}
	if _, err := b.Logs(ctx, "missing"); err == nil {
		t.Error("Logs should return error for unknown worker")
	}
	if err := b.Stop(ctx, "missing"); err == nil {
		t.Error("Stop should return error for unknown worker")
	}
	if err := b.RemoveResource(ctx, backend.ManagedResource{ID: "missing"}); err != nil {
		t.Errorf("RemoveResource should tolerate missing: %v", err)
	}
	stats, err := b.WorkerResourceUsage(ctx, "missing")
	if err != nil {
		t.Errorf("WorkerResourceUsage: %v", err)
	}
	if stats != nil {
		t.Errorf("expected nil stats, got %+v", stats)
	}
}

// TestExitedWorkerLogsAreRetained verifies that an exited worker
// entry persists in the map until explicit Stop/RemoveResource so
// callers can still retrieve buffered stderr/stdout for diagnosis.
// Matches Docker's "stopped containers remain listable until rm"
// semantic.
func TestExitedWorkerLogsAreRetained(t *testing.T) {
	b := newFakeBackend(t)

	// Inject an exited worker entry directly. Done channel is
	// closed and the log buffer has some content.
	logs := newLogBuffer(10)
	r, w := io.Pipe()
	go logs.ingest(r)
	fmt.Fprintln(w, "hello")
	fmt.Fprintln(w, "goodbye")
	w.Close()

	// Wait for ingest to mark the buffer closed.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		logs.mu.Lock()
		closed := logs.closed
		logs.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	done := make(chan struct{})
	close(done)
	b.workers["exited-worker"] = &workerProc{
		port: 20000,
		uid:  70000,
		spec: backend.WorkerSpec{WorkerID: "exited-worker"},
		logs: logs,
		done: done,
	}

	ctx := context.Background()

	// Logs() must succeed and return the buffered content.
	stream, err := b.Logs(ctx, "exited-worker")
	if err != nil {
		t.Fatalf("Logs after exit: %v", err)
	}
	var lines []string
	for line := range stream.Lines {
		lines = append(lines, line)
	}
	if len(lines) != 2 || lines[0] != "hello" || lines[1] != "goodbye" {
		t.Errorf("unexpected log lines: %v", lines)
	}

	// HealthCheck must report unhealthy (done channel is closed).
	if b.HealthCheck(ctx, "exited-worker") {
		t.Error("HealthCheck should return false for an exited worker")
	}

	// Addr should still return the last-known address (matches the
	// Docker semantic where you can inspect a stopped container).
	addr, err := b.Addr(ctx, "exited-worker")
	if err != nil {
		t.Errorf("Addr: %v", err)
	}
	if addr == "" {
		t.Error("Addr returned empty string")
	}

	// ListManaged includes the exited worker.
	resources, err := b.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range resources {
		if r.ID == "exited-worker" {
			seen = true
		}
	}
	if !seen {
		t.Error("ListManaged should include exited worker")
	}

	// Stop on an exited worker is a no-op + delete.
	if err := b.Stop(ctx, "exited-worker"); err != nil {
		t.Errorf("Stop on exited worker: %v", err)
	}

	// After Stop, the entry is gone.
	if _, err := b.Logs(ctx, "exited-worker"); err == nil {
		t.Error("Logs should fail after Stop deletes the entry")
	}
}

// TestSpawnRefusesDuplicateLiveWorker verifies that calling Spawn
// with a worker ID that's already running returns an error rather
// than clobbering the live entry.
func TestSpawnRefusesDuplicateLiveWorker(t *testing.T) {
	b := newFakeBackend(t)

	// Inject a "live" entry (done channel still open).
	b.workers["live-worker"] = &workerProc{
		port: 20000,
		uid:  70000,
		spec: backend.WorkerSpec{WorkerID: "live-worker"},
		done: make(chan struct{}),
	}

	// Spawn would normally fail at the bwrap LookPath step (the
	// fake backend uses /nonexistent/bwrap), but the duplicate
	// check runs first so we should see the duplicate error.
	err := b.Spawn(context.Background(), backend.WorkerSpec{WorkerID: "live-worker"})
	if err == nil {
		t.Fatal("expected error spawning duplicate live worker")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadOneProcStatsSelf(t *testing.T) {
	// Reading our own PID should always succeed and return non-zero
	// values for a running test binary.
	pid := os.Getpid()
	rssKB, utime, stime := readOneProcStats(pid)
	if rssKB == 0 {
		t.Error("expected non-zero RSS for self")
	}
	// utime+stime may legitimately be 0 for a just-started goroutine,
	// but on a test runner something has already accounted. Be lenient.
	_ = utime
	_ = stime
}

func TestReadOneProcStatsMissing(t *testing.T) {
	// PID 0 never exists; expect all-zero result.
	rssKB, utime, stime := readOneProcStats(0)
	if rssKB != 0 || utime != 0 || stime != 0 {
		t.Errorf("expected zeros for missing pid, got %d %d %d", rssKB, utime, stime)
	}
}

func TestReadProcTreeStatsSelf(t *testing.T) {
	// Tree stats for self should at least return non-zero RSS.
	rssBytes, _ := readProcTreeStats(os.Getpid())
	if rssBytes == 0 {
		t.Error("expected non-zero tree RSS for self")
	}
}

func TestCollectDescendantsSelf(t *testing.T) {
	// The test process may or may not have children; either case is
	// fine, we just verify the call doesn't error or panic.
	_ = collectDescendants(os.Getpid())
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\nb\n", []string{"a", "b"}},
		{"\n\n", nil},
		{"a\n\nb", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := splitLines(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitLines(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
