//go:build !minimal || docker_backend

package main

import (
	"os"
	"strings"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/orchestrator"
	"github.com/cynkra/blockyard/internal/server"
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
				func() string { return listenPortFromBind(cfg.Server.Bind) },
			)
		})
}

// listenPortFromBind extracts the port from a "host:port" bind address.
// Returns "8080" when the bind has no colon (edge case — validate
// elsewhere). Duplicated from orchestrator.listenPort rather than
// exporting it, since the orchestrator package intentionally does not
// depend on config.
func listenPortFromBind(bind string) string {
	if idx := strings.LastIndex(bind, ":"); idx != -1 {
		return bind[idx+1:]
	}
	return "8080"
}
