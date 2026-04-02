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
	MgmtServer      *http.Server       // may be nil
	BGCancel        context.CancelFunc
	BGWait          *sync.WaitGroup
	TracingShutdown func(context.Context) error // may be nil
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
// Called after a successful drain in the rolling update path.
// In phase 3-4 (without the phase 3-5 watchdog), SIGUSR1 calls
// Drain() followed by Finish().
func (d *Drainer) Finish(timeout time.Duration) {
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
