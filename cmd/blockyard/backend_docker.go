//go:build !minimal || docker_backend

package main

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/redisstate"
)

func init() {
	backendFactories["docker"] = func(ctx context.Context, cfg *config.Config, _ *redisstate.Client, _ *sqlx.DB, version string) (backend.Backend, error) {
		return docker.New(ctx, cfg, cfg.Storage.BundleServerPath, version)
	}
}
