package main

import (
	"context"
	"sort"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/redisstate"
)

// backendFactory constructs a Backend from the already-parsed config
// and a (possibly nil) shared Redis client. The process backend uses
// rc for Redis-backed allocators when non-nil and falls back to
// in-memory allocators otherwise. The Docker backend ignores rc —
// its Redis awareness is limited to reading the URL string from
// cfg.Redis.URL for its preflight check.
//
// version is threaded through because docker.New needs it for the
// orchestrator's version-comparison path.
type backendFactory func(ctx context.Context, cfg *config.Config, rc *redisstate.Client, version string) (backend.Backend, error)

// backendFactories maps [server] backend = "..." values to factories.
// Populated by init() in the tag-gated backend_docker.go /
// backend_process.go files. Each variant build registers only the
// factories it has tags for; a binary built with
// `-tags 'minimal,process_backend'` has no docker entry and refuses
// to start if the operator sets backend = "docker".
var backendFactories = map[string]backendFactory{}

// availableBackends returns the sorted list of backends compiled into
// this binary. Used for error messages when an operator names a
// backend that isn't available in the current variant.
func availableBackends() []string {
	names := make([]string, 0, len(backendFactories))
	for k := range backendFactories {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
