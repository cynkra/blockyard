//go:build !minimal || docker_backend

package main

import (
	"os"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/units"
)

func init() {
	orchestratorFactoryFns = append(orchestratorFactoryFns,
		func(srv *server.Server, cfg *config.Config, be backend.Backend) orchestrator.ServerFactory {
			// Containerized process-backend mode runs as PID 1;
			// orchestrator has no home there even when the Docker
			// factory would otherwise match. The Docker branch is
			// unaffected because it requires ServerID() != "".
			if os.Getpid() == 1 && cfg.Server.Backend != "docker" {
				return nil
			}
			dbe, ok := be.(*docker.DockerBackend)
			if !ok || dbe.ServerID() == "" {
				return nil
			}
			return orchestrator.NewDockerFactory(
				dbe.Client(),
				dbe.ServerID(),
				srv.Version,
				func() string { return units.ListenPort(cfg.Server.Bind) },
			)
		})
}
