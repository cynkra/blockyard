package orchestrator

import (
	"context"

	"github.com/cynkra/blockyard/internal/task"
)

// ServerFactory abstracts "create a new server instance" so the
// orchestrator's cutover/watchdog/scheduled core stays backend-agnostic.
// Two implementations exist: Docker container clone (clone_docker.go)
// and process fork+exec (clone_process.go).
type ServerFactory interface {
	// CreateInstance starts the new server instance and blocks until
	// its address is resolvable. On success, the returned instance's
	// Addr() is immediately usable for polling and activation — no
	// async resolution required by the caller. The ctx's deadline
	// (set by the orchestrator from cfg.Proxy.WorkerStartTimeout)
	// bounds address resolution; the remaining budget flows through
	// to waitReady for /readyz polling.
	//
	// ref is the image reference for the Docker variant; the process
	// variant ignores it (it always execs the current binary).
	//
	// extraEnv is a list of KEY=VALUE strings injected into the new
	// instance's environment (activation token, etc.).
	CreateInstance(ctx context.Context, ref string, extraEnv []string, sender task.Sender) (newServerInstance, error)

	// PreUpdate runs variant-specific preparation before the instance
	// is created. Docker pulls the new image; process is a no-op (the
	// binary is already on disk).
	PreUpdate(ctx context.Context, version string, sender task.Sender) error

	// CurrentImageBase returns the image repository (without tag) for
	// the running server. Docker reads it from container inspect;
	// process returns a stable placeholder (the process variant has
	// no equivalent concept).
	CurrentImageBase(ctx context.Context) string

	// CurrentImageTag returns the image tag for the running server.
	// Docker reads it from container inspect; process returns the
	// current version string.
	CurrentImageTag(ctx context.Context) string

	// SupportsRollback indicates whether this factory can restart a
	// previous version. Docker can (pull old image); process cannot
	// (previous binary typically overwritten by upgrade).
	SupportsRollback() bool

	// IsAlreadyCurrent reports whether ref resolves to the same
	// image as the running server. Called after PreUpdate so the
	// just-pulled image is in the local registry cache. Used by the
	// main channel to short-circuit when the rolling :main tag
	// resolves to the digest already in use.
	//
	// Docker: compares the pulled ref's image ID against the
	// running container's image ID via ContainerInspect/ImageInspect.
	// Process: returns false — the variant has no registry semantics
	// and Update is effectively a fork+exec restart.
	IsAlreadyCurrent(ctx context.Context, ref string) (bool, error)
}

// newServerInstance is the handle returned by CreateInstance. It
// exposes just enough to poll, activate, and tear down the new
// server without the orchestrator knowing which backend produced it.
type newServerInstance interface {
	// ID returns a stable identifier for logging (Docker container
	// ID, process PID as string).
	ID() string

	// Addr returns host:port usable for polling /readyz and calling
	// /admin/activate. Cached at CreateInstance time — cheap and
	// synchronous, never errors.
	Addr() string

	// Kill tears down the instance on failure or watchdog rollback.
	// Best-effort; logs errors internally.
	Kill(ctx context.Context)
}
