package backend

import (
	"context"
	"errors"

	"github.com/cynkra/blockyard/internal/preflight"
)

// Backend is the pluggable worker runtime abstraction. The Docker
// implementation runs each worker in its own container; the process
// implementation spawns sandboxed child processes via bubblewrap. A
// future Kubernetes implementation would manage Pods.
type Backend interface {
	// Spawn starts a long-lived worker. The caller provides the worker ID
	// in spec.WorkerID; the backend uses it as its internal key.
	Spawn(ctx context.Context, spec WorkerSpec) error

	// Stop stops and removes a worker by ID.
	Stop(ctx context.Context, id string) error

	// HealthCheck probes whether a worker is responsive.
	HealthCheck(ctx context.Context, id string) bool

	// Logs streams stdout/stderr from a worker.
	Logs(ctx context.Context, id string) (LogStream, error)

	// Addr resolves the worker's network address (host:port).
	Addr(ctx context.Context, id string) (string, error)

	// Build runs a build task to completion (dependency restore).
	Build(ctx context.Context, spec BuildSpec) (BuildResult, error)

	// ListManaged lists all resources carrying blockyard labels.
	ListManaged(ctx context.Context) ([]ManagedResource, error)

	// RemoveResource removes an orphaned resource.
	RemoveResource(ctx context.Context, r ManagedResource) error

	// WorkerResourceUsage returns a point-in-time resource usage
	// snapshot for a worker. Returns nil stats if the worker is not
	// found. Backends may implement this differently — Docker reads
	// container cgroup counters, the process backend walks /proc.
	WorkerResourceUsage(ctx context.Context, workerID string) (*WorkerResourceUsageResult, error)

	// UpdateResources live-updates memory and CPU limits for a running
	// worker. Returns ErrNotSupported if the backend does not support
	// live resource updates.
	UpdateResources(ctx context.Context, id string, mem int64, nanoCPUs int64) error

	// CleanupOrphanResources removes backend-specific stale state left
	// over from previous runs (Docker: orphaned iptables rules).
	// Process backend is a no-op. Called once at startup.
	CleanupOrphanResources(ctx context.Context) error

	// Preflight runs backend-specific startup checks and returns the
	// resulting report. Each backend checks its own prerequisites.
	// main.go calls this through the interface so it does not have to
	// branch on the backend type.
	Preflight(ctx context.Context) (*preflight.Report, error)
}

// ErrNotSupported is returned by backend methods that are not
// available for the current backend type.
var ErrNotSupported = errors.New("operation not supported by this backend")

// WorkerResourceUsageResult holds point-in-time resource usage for a
// worker. MemoryLimitBytes is 0 when the backend does not enforce a
// per-worker limit (e.g. process backend, or Docker without a limit).
type WorkerResourceUsageResult struct {
	CPUPercent       float64
	MemoryUsageBytes uint64
	MemoryLimitBytes uint64
}

type WorkerSpec struct {
	AppID       string
	WorkerID    string
	Image       string
	Cmd         []string          // container command; nil = use image entrypoint
	BundlePath  string            // server-side path to unpacked bundle
	LibraryPath string            // server-side path to restored R library (legacy, phase 2-5)
	LibDir      string            // server-side path to per-worker lib dir from store; empty if no store
	TransferDir string            // server-side path to per-worker transfer dir (phase 2-7)
	TokenDir    string            // server-side path to worker token dir; mounted ro at /var/run/blockyard
	WorkerMount string            // in-container mount point (BundleWorkerPath)
	ShinyPort   int
	RVersion    string            // pinned R version from bundle manifest (e.g. "4.5.0"); empty = default
	MemoryLimit string            // e.g. "512m", "" if unset
	CPULimit    float64           // fractional vCPUs, 0 if unset
	Labels      map[string]string
	Env         map[string]string // additional env vars (e.g. VAULT_ADDR)
	DataMounts  []MountEntry      // data mounts from app config; resolved host paths
	Runtime     string            // OCI runtime override; empty = default
}

type BuildSpec struct {
	AppID    string
	BundleID string
	Image    string
	Labels   map[string]string
	LogWriter func(string)   // called with each log line during the build; may be nil
	Cmd      []string        // container command (e.g. R script invocation)
	Mounts   []MountEntry    // bind/volume mounts for the build container
	Env      []string        // environment variables (KEY=VALUE)
	RVersion string          // pinned R version from bundle manifest; empty = default
}

// MountEntry describes a single bind/volume mount for a build container.
type MountEntry struct {
	Source   string
	Target   string
	ReadOnly bool
}

type BuildResult struct {
	Success  bool
	ExitCode int
	Logs     string // combined stdout+stderr from the build container
}

type ManagedResource struct {
	ID     string
	Kind   ResourceKind
	Labels map[string]string
}

type ResourceKind int

const (
	ResourceContainer ResourceKind = iota
	ResourceNetwork
)

// LogStream delivers log lines as they arrive.
// Read from Lines until the channel is closed (container exited).
type LogStream struct {
	Lines <-chan string
	// Close cancels the underlying log follow.
	Close func()
}
