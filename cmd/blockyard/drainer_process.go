//go:build !minimal || process_backend

package main

import (
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/backend/process"
	"github.com/cynkra/blockyard/internal/config"
)

// finishIdleWaitForProcess is the tag-gated companion to
// finishIdleWaitForBackend. Returns (duration, true) when the
// resolved backend is the process backend, otherwise (0, false).
//
// Default is 5 minutes when cfg.Update is nil — operators who
// don't declare [update] still get a reasonable idle-wait for
// same-binary rolling updates.
func finishIdleWaitForProcess(be backend.Backend, cfg *config.Config) (time.Duration, bool) {
	if _, ok := be.(*process.ProcessBackend); !ok {
		return 0, false
	}
	if cfg.Update != nil && cfg.Update.DrainIdleWait.Duration > 0 {
		return cfg.Update.DrainIdleWait.Duration, true
	}
	return 5 * time.Minute, true
}
