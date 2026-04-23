//go:build !minimal || process_backend

package main

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/process"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/redisstate"
)

func init() {
	backendFactories["process"] = func(_ context.Context, cfg *config.Config, rc *redisstate.Client, db *sqlx.DB, _ string) (backend.Backend, error) {
		return process.New(cfg, rc, db)
	}
	runBwrapExecFn = process.RunBwrapExec
}
