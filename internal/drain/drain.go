package drain

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cynkra/blockyard/internal/ops"
	"github.com/cynkra/blockyard/internal/server"
)

// Drainer manages server lifecycle for drain mode (SIGUSR1) and
// shutdown (SIGTERM). Four methods cover the full rolling update
// lifecycle:
//
//   - Drain / Undrain toggle health endpoint responses (503 / 200).
//   - Finish tears down the process without evicting workers.
//   - Shutdown tears down the process and evicts all workers.
type Drainer struct {
	Srv             *server.Server
	MainServer      *http.Server
	MgmtServer      *http.Server               // may be nil
	BGCancel        context.CancelFunc
	BGWait          *sync.WaitGroup
	TracingShutdown func(context.Context) error // may be nil

	// FinishIdleWait, if non-zero, makes Finish wait up to this
	// duration for the local server's session count to reach zero
	// before tearing down. Set by main.go for the process backend;
	// zero for docker (which cuts over hard and relies on the
	// reverse proxy to drain the last requests).
	FinishIdleWait time.Duration

	// ServerID identifies this server process uniquely among peers
	// sharing a Redis. Used by waitForIdle to filter the workermap
	// so the old server waits for its *own* sessions to finish, not
	// the new server's fresh sessions. Set by main.go to the same
	// value passed to NewRedisWorkerMap. Safe to leave empty in the
	// memory-store path (single-node = all workers are ours).
	ServerID string
}

// Drain sets the draining flag. Health endpoints start returning 503,
// causing the proxy/LB to stop routing new traffic. HTTP listeners
// stay alive so Undrain() can reverse this without recreating servers.
func (d *Drainer) Drain() {
	slog.Info("drain mode: health endpoints returning 503")
	d.Srv.Draining.Store(true)
}

// Undrain clears the draining flag. Health endpoints resume returning
// 200 and the proxy/LB routes traffic again. Used when a rolling
// update fails and the old server must resume serving.
func (d *Drainer) Undrain() {
	slog.Info("undrain: health endpoints returning 200")
	d.Srv.Draining.Store(false)
}

// Finish performs non-destructive teardown: shuts down HTTP servers,
// cancels background goroutines, closes the database, and flushes
// tracing. Workers survive — the new server manages them via Redis.
//
// Called after a successful drain in the rolling update path. On the
// process backend, Finish preludes with an idle-wait that polls the
// local server's session count until it reaches zero or
// FinishIdleWait elapses. The prelude is skipped when
// FinishIdleWait is zero (Docker variant) so Finish's behavior is
// backwards-compatible with phase 3-4.
//
// In phase 3-4 (without the phase 3-5 watchdog), SIGUSR1 calls
// Drain() followed by Finish().
func (d *Drainer) Finish(timeout time.Duration) {
	if d.FinishIdleWait > 0 {
		d.waitForIdle(d.FinishIdleWait)
	}
	slog.Info("finish: shutting down (workers survive)")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 1. Shut down HTTP servers (finish in-flight requests).
	// Note: Shutdown does NOT wait for hijacked connections (WebSockets).
	// Active terminal/log sessions are severed immediately — clients
	// reconnect through the new server. Workers survive, so the
	// interruption is brief.
	if d.MgmtServer != nil {
		if err := d.MgmtServer.Shutdown(ctx); err != nil {
			slog.Error("finish: management server shutdown error", "error", err)
		}
	}
	if err := d.MainServer.Shutdown(ctx); err != nil {
		slog.Error("finish: main server shutdown error", "error", err)
	}

	// 2. Stop background goroutines.
	d.BGCancel()
	d.BGWait.Wait()

	// 3. Close database.
	if err := d.Srv.DB.Close(); err != nil {
		slog.Error("finish: database close error", "error", err)
	}

	// 4. Flush tracing.
	if d.TracingShutdown != nil {
		d.TracingShutdown(context.Background()) //nolint:errcheck
	}

	slog.Info("finish: complete, exiting")
}

// idleWaitPollInterval is the poll cadence waitForIdle uses between
// WorkersForServer/CountForWorkers checks. 5 seconds is short enough
// for a snappy cutover and long enough that the Redis SCAN runs at
// most 12 times per minute, which is negligible compared to the
// sustained traffic the server already pushes through Redis.
// A package-level var so internal tests can shorten it without
// stretching the test runtime.
var idleWaitPollInterval = 5 * time.Second

// waitForIdle polls the local server's session count until it
// reaches zero or maxWait elapses. Only workers owned by this server
// (matched via d.ServerID against the workermap) contribute — a new
// peer's workers would otherwise keep the count above zero
// indefinitely during a same-host rolling update.
//
// Unexported — only Finish calls it.
func (d *Drainer) waitForIdle(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(idleWaitPollInterval)
	defer ticker.Stop()

	for {
		own := d.Srv.Workers.WorkersForServer(d.ServerID)
		sessions := d.Srv.WsConns.CountForWorkers(own)
		if sessions == 0 {
			slog.Info("finish: session count reached zero",
				"server_id", d.ServerID)
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("finish: idle wait elapsed, proceeding with teardown",
				"remaining_sessions", sessions,
				"server_id", d.ServerID)
			return
		}
		slog.Info("finish: waiting for sessions to end",
			"remaining_sessions", sessions,
			"server_id", d.ServerID)
		<-ticker.C
	}
}

// Shutdown performs full teardown including worker eviction. Called
// on SIGTERM/SIGINT.
func (d *Drainer) Shutdown(timeout time.Duration) {
	slog.Info("shutdown: entering (SIGTERM/SIGINT)")

	// 1. Health endpoints start returning 503.
	d.Drain()

	// 2. Shut down HTTP servers (finish in-flight requests;
	// hijacked WebSocket connections are severed immediately).
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if d.MgmtServer != nil {
		if err := d.MgmtServer.Shutdown(ctx); err != nil {
			slog.Error("shutdown: management server error", "error", err)
		}
	}
	if err := d.MainServer.Shutdown(ctx); err != nil {
		slog.Error("shutdown: main server error", "error", err)
	}

	// 3. Stop background goroutines.
	d.BGCancel()
	d.BGWait.Wait()

	// 4. Stop all workers and clean up. Reuses the timeout context —
	// whatever budget remains after HTTP shutdown goes to worker
	// eviction. drainAndEvictAll also uses ShutdownTimeout/2 internally,
	// so this ctx is a ceiling, not the only guard.
	ops.GracefulShutdown(ctx, d.Srv)

	// 5. Close database.
	if err := d.Srv.DB.Close(); err != nil {
		slog.Error("shutdown: database close error", "error", err)
	}

	// 6. Flush tracing.
	if d.TracingShutdown != nil {
		d.TracingShutdown(context.Background()) //nolint:errcheck
	}

	slog.Info("shutdown complete")
}
