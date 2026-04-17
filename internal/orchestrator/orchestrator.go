// Package orchestrator manages rolling updates from inside the running
// server. The core flow (Update → Watchdog → Rollback → scheduled) is
// backend-agnostic. Variant-specific code (Docker container clone,
// process fork+exec) lives behind the ServerFactory interface in
// clone_docker.go and clone_process.go.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/task"
	"github.com/cynkra/blockyard/internal/update"
)

// Orchestrator manages rolling updates from inside the running server.
// The factory field abstracts the variant-specific "create a new server
// instance" step. The activeInstance field carries the new instance
// between Update and Watchdog without threading it through the admin
// handler goroutine — the state machine already serializes those phases
// so no additional locking is needed.
type Orchestrator struct {
	factory   ServerFactory
	db        *db.DB
	cfg       *config.Config
	version   string // current server version
	tasks     *task.Store
	update    updateAPI
	log       *slog.Logger
	drainFn   func()
	undrainFn func()
	exitFn    func()

	activationToken string            // set during CreateInstance, used by activate
	activeInstance  newServerInstance // set by Update, read by Watchdog/Rollback

	state atomic.Value // "idle"/"updating"/"watching"/"rolling_back"
}

// updateAPI abstracts the GitHub release lookup so tests can mock
// it. FetchInstallTarget returns the version string to install on
// the given channel, or "" when current already matches.
type updateAPI interface {
	FetchInstallTarget(channel, currentVersion string) (string, error)
}

// DefaultChecker wraps the update package's install-target lookup.
type DefaultChecker struct{}

func (DefaultChecker) FetchInstallTarget(channel, currentVersion string) (string, error) {
	return update.FetchInstallTarget(channel, currentVersion)
}

// New creates an Orchestrator wired to a ServerFactory. The factory
// encapsulates variant-specific server-creation logic; the core flow
// uses only the interface.
func New(
	factory ServerFactory,
	database *db.DB,
	cfg *config.Config,
	version string,
	tasks *task.Store,
	checker updateAPI,
	log *slog.Logger,
	drainFn, undrainFn, exitFn func(),
) *Orchestrator {
	o := &Orchestrator{
		factory:   factory,
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

// NewForTest creates a minimal Orchestrator for API tests that only
// need state management. The update checker returns "already up to
// date" so background goroutines spawned by handlers exit quickly
// without panics. The factory is a stub that reports
// SupportsRollback=true so the admin rollback handler's
// pre-dispatch check passes; the factory's CreateInstance is never
// reached because the test exits before the goroutine runs.
func NewForTest() *Orchestrator {
	return newForTestWithFactory(stubFactory{supportsRollback: true})
}

// NewForTestNoRollback is NewForTest but with a factory that reports
// SupportsRollback=false — simulates the process backend at the
// orchestrator layer. The admin rollback handler uses this to cover
// the phase 3-8 "backend cannot rollback" 501 branch without
// plugging the real processServerFactory (which lives behind a
// build tag).
func NewForTestNoRollback() *Orchestrator {
	return newForTestWithFactory(stubFactory{supportsRollback: false})
}

func newForTestWithFactory(f ServerFactory) *Orchestrator {
	o := &Orchestrator{
		exitFn:  func() {},
		update:  noopChecker{},
		factory: f,
		tasks:   task.NewStore(),
		log:     slog.Default(),
		cfg:     &config.Config{},
	}
	o.state.Store("idle")
	return o
}

type noopChecker struct{}

func (noopChecker) FetchInstallTarget(_, _ string) (string, error) {
	return "", nil
}

// stubFactory is a minimal ServerFactory used by NewForTest. All
// methods return zero values or errors; tests that exercise the
// Update/Rollback flow use a real factory instead.
type stubFactory struct {
	supportsRollback bool
}

func (stubFactory) CreateInstance(_ context.Context, _ string, _ []string, _ task.Sender) (newServerInstance, error) {
	return nil, fmt.Errorf("stub factory: not implemented")
}
func (stubFactory) PreUpdate(_ context.Context, _ string, _ task.Sender) error { return nil }
func (stubFactory) CurrentImageBase(_ context.Context) string                  { return "blockyard" }
func (stubFactory) CurrentImageTag(_ context.Context) string                   { return "test" }
func (f stubFactory) SupportsRollback() bool                                   { return f.supportsRollback }

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

// SupportsRollback reports whether the active factory can restart a
// previous version. The admin handler returns 501 for factories that
// cannot (process backend).
func (o *Orchestrator) SupportsRollback() bool {
	if o.factory == nil {
		return false
	}
	return o.factory.SupportsRollback()
}

// Update executes the rolling update. It reports progress to the
// provided sender and returns true on success (false when already up
// to date).
//
// On success, the new instance is stashed on o.activeInstance so
// Watchdog can poll it without the caller threading any opaque handle
// through the API layer. The state machine serializes Update →
// Watchdog so the field is only ever read between those phases by one
// caller.
//
// The caller (API handler or cron trigger) runs this in a goroutine.
// The context should be the server's background context, not a
// request context.
func (o *Orchestrator) Update(
	ctx context.Context,
	channel string,
	sender task.Sender,
) (bool, error) {
	// 1. Resolve install target for the configured channel.
	target, err := o.update.FetchInstallTarget(channel, o.version)
	if err != nil {
		return false, fmt.Errorf("fetch install target: %w", err)
	}
	if target == "" {
		sender.Write("Already up to date (" + o.version + ").")
		return false, nil
	}
	newRef := imageWithTag(o.factory.CurrentImageBase(ctx), target)
	sender.Write(fmt.Sprintf("Update target: %s → %s", o.version, target))

	// 2. Variant-specific prep (Docker: pull image; process: no-op).
	if err := o.factory.PreUpdate(ctx, target, sender); err != nil {
		return false, fmt.Errorf("pre-update: %w", err)
	}

	// 3. Back up database.
	sender.Write("Backing up database ...")
	meta, err := o.db.BackupWithMeta(ctx, o.factory.CurrentImageTag(ctx))
	if err != nil {
		return false, fmt.Errorf("backup: %w", err)
	}
	sender.Write("Backup: " + meta.BackupPath)

	// 4. Create new instance (passive mode).
	o.activationToken = generateActivationToken()
	sender.Write("Starting new instance ...")
	startCtx, cancel := context.WithTimeout(ctx, o.cfg.Proxy.WorkerStartTimeout.Duration)
	defer cancel()
	inst, err := o.factory.CreateInstance(startCtx, newRef, []string{
		"BLOCKYARD_ACTIVATION_TOKEN=" + o.activationToken,
	}, sender)
	if err != nil {
		return false, fmt.Errorf("create instance: %w", err)
	}

	// 5. Poll /readyz on new instance until 200.
	sender.Write("Waiting for new instance to become ready ...")
	if err := o.waitReady(startCtx, inst.Addr()); err != nil {
		inst.Kill(ctx)
		return false, fmt.Errorf("new instance never became ready: %w", err)
	}

	// 6. Drain self.
	sender.Write("Draining current server ...")
	o.drainFn()

	// 7. Activate new server (start background goroutines).
	sender.Write("Activating new server ...")
	if err := o.activate(ctx, inst.Addr()); err != nil {
		inst.Kill(ctx)
		o.undrainFn()
		return false, fmt.Errorf("activate new server: %w", err)
	}

	// 8. Stash the instance for Watchdog.
	o.activeInstance = inst
	sender.Write("Update complete. Entering watchdog mode ...")
	return true, nil
}

// imageWithTag constructs a Docker image reference with the given tag.
// Used by the Docker variant for logging and the Rollback flow. The
// process variant ignores the reference.
func imageWithTag(base, tag string) string {
	return base + ":" + tag
}
