package backend

import "context"

// Backend is the pluggable container runtime abstraction.
// Docker/Podman for v0, Kubernetes for v2.
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
}

type WorkerSpec struct {
	AppID       string
	WorkerID    string
	Image       string
	Cmd         []string          // container command; nil = use image entrypoint
	BundlePath  string            // server-side path to unpacked bundle
	LibraryPath string            // server-side path to restored R library
	WorkerMount string            // in-container mount point (BundleWorkerPath)
	ShinyPort   int
	MemoryLimit string            // e.g. "512m", "" if unset
	CPULimit    float64           // fractional vCPUs, 0 if unset
	Labels      map[string]string
}

type BuildSpec struct {
	AppID        string
	BundleID     string
	Image        string
	RvBinaryPath string            // server-side path to cached rv binary
	BundlePath   string            // server-side path to unpacked bundle
	LibraryPath  string            // server-side output path for restored library
	Labels       map[string]string
}

type BuildResult struct {
	Success  bool
	ExitCode int
	Logs     string // combined stdout+stderr from the build container
}

type ManagedResource struct {
	ID   string
	Kind ResourceKind
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
