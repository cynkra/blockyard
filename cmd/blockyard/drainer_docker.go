//go:build minimal && docker_backend && !process_backend

package main

import (
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
)

// finishIdleWaitForProcess returns (0, false) in the docker-only
// variant — there is no process backend compiled in, so no backend
// type assertion can succeed.
func finishIdleWaitForProcess(_ backend.Backend, _ *config.Config) (time.Duration, bool) {
	return 0, false
}
