package mock

import (
	"context"
	"testing"

	"github.com/cynkra/blockyard/internal/backend"
)

func testWorkerSpec(appID, workerID string) backend.WorkerSpec {
	return backend.WorkerSpec{
		AppID:       appID,
		WorkerID:    workerID,
		Image:       "test:latest",
		BundlePath:  "/tmp/bundle",
		LibraryPath: "/tmp/lib",
		WorkerMount: "/app",
		ShinyPort:   3838,
	}
}

func TestSpawnAndStop(t *testing.T) {
	b := New()
	ctx := context.Background()

	spec := testWorkerSpec("app-1", "worker-1")
	if err := b.Spawn(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if b.WorkerCount() != 1 {
		t.Errorf("expected 1 worker, got %d", b.WorkerCount())
	}
	if !b.HasWorker("worker-1") {
		t.Error("expected HasWorker to return true")
	}

	if err := b.Stop(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if b.WorkerCount() != 0 {
		t.Errorf("expected 0 workers, got %d", b.WorkerCount())
	}
}

func TestHealthCheckConfigurable(t *testing.T) {
	b := New()
	ctx := context.Background()

	spec := testWorkerSpec("app-1", "worker-1")
	b.Spawn(ctx, spec)

	if !b.HealthCheck(ctx, "worker-1") {
		t.Error("expected healthy")
	}

	b.HealthOK.Store(false)
	if b.HealthCheck(ctx, "worker-1") {
		t.Error("expected unhealthy")
	}
}

func TestAddr(t *testing.T) {
	b := New()
	ctx := context.Background()

	spec := testWorkerSpec("app-1", "worker-1")
	b.Spawn(ctx, spec)

	addr, err := b.Addr(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" {
		t.Error("expected non-empty address")
	}
}

func TestBuildConfigurable(t *testing.T) {
	b := New()
	ctx := context.Background()

	result, _ := b.Build(ctx, backend.BuildSpec{})
	if !result.Success {
		t.Error("expected success")
	}

	b.BuildSuccess.Store(false)
	result, _ = b.Build(ctx, backend.BuildSpec{})
	if result.Success {
		t.Error("expected failure")
	}
}

func TestStopNonexistent(t *testing.T) {
	b := New()
	err := b.Stop(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error stopping nonexistent worker")
	}
}
