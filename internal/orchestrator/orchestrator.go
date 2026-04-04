// Package orchestrator manages rolling updates from inside the running server.
// It uses the Docker socket the backend already holds — no sidecar container,
// no CLI-side Docker access.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/moby/moby/client"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/update"
)

// Orchestrator manages rolling updates from inside the running server.
type Orchestrator struct {
	docker    dockerClient
	serverID  string // own container ID from DockerBackend.ServerID()
	db        *db.DB
	cfg       *config.Config
	version   string // current server version
	tasks     *task.Store
	update    updateAPI
	log       *slog.Logger
	drainFn         func()
	undrainFn       func()
	exitFn          func()
	activationToken string       // set during startClone, used by activate
	state           atomic.Value // "idle"/"updating"/"watching"/"rolling_back"
}

// dockerClient is the subset of the Docker API needed by the orchestrator.
// The concrete *client.Client satisfies this interface.
type dockerClient interface {
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerWait(ctx context.Context, containerID string, options client.ContainerWaitOptions) client.ContainerWaitResult
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

// updateAPI abstracts the GitHub release check so tests can mock it.
type updateAPI interface {
	CheckLatest(channel, currentVersion string) (*update.Result, error)
}

// DefaultChecker wraps the existing update.CheckLatest function.
type DefaultChecker struct{}

func (DefaultChecker) CheckLatest(channel, currentVersion string) (*update.Result, error) {
	return update.CheckLatest(channel, currentVersion)
}

// New creates an Orchestrator wired to the running server's Docker backend.
func New(
	docker *client.Client,
	serverID string,
	database *db.DB,
	cfg *config.Config,
	version string,
	tasks *task.Store,
	checker updateAPI,
	log *slog.Logger,
	drainFn, undrainFn, exitFn func(),
) *Orchestrator {
	o := &Orchestrator{
		docker:    docker,
		serverID:  serverID,
		db:        database,
		cfg:       cfg,
		version:   version,
		tasks:     tasks,
		update:    checker,
		log:       log,
		drainFn:   drainFn,
		undrainFn: undrainFn,
		exitFn:    exitFn,
	}
	o.state.Store("idle")
	return o
}

// NewForTest creates a minimal Orchestrator for API tests that only need
// state management (no Docker client, no DB).
// NewForTest creates a minimal Orchestrator for API tests that only need
// state management. The update checker returns "already up to date" so
// background goroutines spawned by handlers exit quickly without panics.
func NewForTest() *Orchestrator {
	o := &Orchestrator{
		exitFn: func() {},
		update: noopChecker{},
		tasks:  task.NewStore(),
		log:    slog.Default(),
		cfg:    &config.Config{},
	}
	o.state.Store("idle")
	return o
}

type noopChecker struct{}

func (noopChecker) CheckLatest(_, _ string) (*update.Result, error) {
	return &update.Result{UpdateAvailable: false}, nil
}

// State returns the current orchestrator phase.
func (o *Orchestrator) State() string {
	return o.state.Load().(string)
}

// CASState performs a compare-and-swap on the state. Returns true on success.
func (o *Orchestrator) CASState(old, new string) bool {
	return o.state.CompareAndSwap(old, new)
}

// SetState sets the orchestrator state directly.
func (o *Orchestrator) SetState(s string) {
	o.state.Store(s)
}

// Exit signals the main goroutine to call Finish and exit.
func (o *Orchestrator) Exit() {
	o.exitFn()
}

// UpdateResult holds the new container's identity so the caller can
// pass it to Watchdog. Nil when the server is already up to date.
type UpdateResult struct {
	ContainerID string // Docker container ID of the new instance
	Addr        string // internal IP:port for health checks
}

// Update executes the rolling update. It reports progress to the
// provided sender and returns the new container's identity on success
// (nil result when already up to date).
//
// The caller (API handler or cron trigger) runs this in a goroutine.
// The context should be the server's background context, not a request context.
func (o *Orchestrator) Update(
	ctx context.Context,
	channel string,
	sender task.Sender,
) (*UpdateResult, error) {
	// 1. Check for newer version.
	result, err := o.update.CheckLatest(channel, o.version)
	if err != nil {
		return nil, fmt.Errorf("check latest: %w", err)
	}
	if !result.UpdateAvailable {
		sender.Write("Already up to date (" + result.CurrentVersion + ").")
		return nil, nil
	}
	newImage := imageRef(o.serverID, result, o.currentImageBase(ctx))
	sender.Write(fmt.Sprintf("Update available: %s → %s",
		result.CurrentVersion, result.LatestVersion))

	// 2. Pull new image.
	sender.Write("Pulling " + newImage + " ...")
	if err := o.pullImage(ctx, newImage); err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}

	// 3. Back up database.
	sender.Write("Backing up database ...")
	meta, err := o.db.BackupWithMeta(ctx, o.currentImageTag(ctx))
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	sender.Write("Backup: " + meta.BackupPath)

	// 4. Start new container (passive mode).
	sender.Write("Starting new container ...")
	newID, err := o.startClone(ctx, newImage)
	if err != nil {
		return nil, fmt.Errorf("start new container: %w", err)
	}

	// 5. Poll /readyz on new container until 200.
	sender.Write("Waiting for new container to become ready ...")
	newAddr, err := o.waitReady(ctx, newID)
	if err != nil {
		o.killAndRemove(ctx, newID)
		return nil, fmt.Errorf("new container never became ready: %w", err)
	}

	// 6. Drain self.
	sender.Write("Draining current server ...")
	o.drainFn()

	// 7. Activate new server (start background goroutines).
	sender.Write("Activating new server ...")
	if err := o.activate(ctx, newAddr); err != nil {
		o.killAndRemove(ctx, newID)
		o.undrainFn()
		return nil, fmt.Errorf("activate new server: %w", err)
	}

	// 8. Return new container identity for watchdog.
	sender.Write("Update complete. Entering watchdog mode ...")
	return &UpdateResult{ContainerID: newID, Addr: newAddr}, nil
}

// imageRef constructs a Docker image reference for the new version.
// It reads the base image name from the current container's config and
// replaces the tag with the new version.
func imageRef(serverID string, result *update.Result, base string) string {
	return imageWithTag(base, result.LatestVersion)
}

// imageWithTag constructs a Docker image reference with the given tag.
func imageWithTag(base, tag string) string {
	return base + ":" + tag
}

// currentImageBase returns the image repository (without tag) from the
// running container's config.
func (o *Orchestrator) currentImageBase(ctx context.Context) string {
	result, err := o.docker.ContainerInspect(ctx, o.serverID, client.ContainerInspectOptions{})
	if err != nil {
		o.log.Warn("inspect self for image base", "error", err)
		return "blockyard"
	}
	img := result.Container.Config.Image
	if idx := strings.LastIndex(img, ":"); idx != -1 {
		return img[:idx]
	}
	return img
}

// currentImageTag reads the image tag from the running container's
// inspect result.
func (o *Orchestrator) currentImageTag(ctx context.Context) string {
	result, err := o.docker.ContainerInspect(ctx, o.serverID, client.ContainerInspectOptions{})
	if err != nil {
		o.log.Warn("inspect self for image tag", "error", err)
		return o.version
	}
	img := result.Container.Config.Image
	if idx := strings.LastIndex(img, ":"); idx != -1 {
		return img[idx+1:]
	}
	return o.version
}
