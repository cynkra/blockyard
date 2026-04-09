package main

import (
	"log/slog"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/server"
)

// orchestratorFactoryFns is populated by init() in the tag-gated
// orchestrator_docker.go / orchestrator_process.go files. Each entry
// inspects the resolved backend and returns a ServerFactory if it
// matches, otherwise nil. The dispatcher below picks the first
// non-nil result.
//
// Containerized deployments (PID 1) skip orchestrator creation
// entirely — the candidate functions return nil when os.Getpid() == 1
// because fork+exec-ing a new blockyard inside a container is
// pointless (killing PID 1 stops the container regardless).
var orchestratorFactoryFns []func(srv *server.Server, cfg *config.Config, be backend.Backend) orchestrator.ServerFactory

// newServerFactory iterates the registered candidates and returns
// the first non-nil ServerFactory. Returns nil when no candidate
// matches (e.g. containerized process backend, or the built variant
// doesn't include the active backend's orchestrator).
func newServerFactory(srv *server.Server, cfg *config.Config, be backend.Backend) orchestrator.ServerFactory {
	for _, fn := range orchestratorFactoryFns {
		if f := fn(srv, cfg, be); f != nil {
			slog.Debug("orchestrator factory selected")
			return f
		}
	}
	return nil
}
