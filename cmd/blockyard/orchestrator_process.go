//go:build !minimal || process_backend

package main

import (
	"os"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/process"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/server"
)

func init() {
	orchestratorFactoryFns = append(orchestratorFactoryFns,
		func(srv *server.Server, cfg *config.Config, be backend.Backend) orchestrator.ServerFactory {
			// Containerized blockyard runs as PID 1; fork+exec-ing a
			// new blockyard inside the container is pointless (killing
			// PID 1 stops the container regardless). Operators use
			// their container runtime's update mechanism instead.
			if os.Getpid() == 1 {
				return nil
			}
			if _, ok := be.(*process.ProcessBackend); !ok {
				return nil
			}
			return orchestrator.NewProcessFactory(cfg, srv.Version)
		})
}
