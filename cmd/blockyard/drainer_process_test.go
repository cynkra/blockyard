//go:build !minimal || process_backend

package main

import (
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend/process"
	"github.com/cynkra/blockyard/internal/config"
)

// Process-backend branch of finishIdleWaitForBackend: default is 5
// minutes when cfg.Update is nil or DrainIdleWait is zero, otherwise
// the configured value. Guards the rolling-update correctness rule
// that workers get a chance to finish local sessions before
// Pdeathsig tears them down.
func TestFinishIdleWaitForBackend_Process(t *testing.T) {
	be := &process.ProcessBackend{}

	t.Run("no cfg.Update section — defaults to 5m", func(t *testing.T) {
		cfg := &config.Config{}
		if got := finishIdleWaitForBackend(be, cfg); got != 5*time.Minute {
			t.Errorf("finishIdleWaitForBackend(process, no-update) = %v, want 5m", got)
		}
	})

	t.Run("zero DrainIdleWait — defaults to 5m", func(t *testing.T) {
		cfg := &config.Config{Update: &config.UpdateConfig{}}
		if got := finishIdleWaitForBackend(be, cfg); got != 5*time.Minute {
			t.Errorf("finishIdleWaitForBackend(process, zero) = %v, want 5m", got)
		}
	})

	t.Run("explicit DrainIdleWait — honoured verbatim", func(t *testing.T) {
		cfg := &config.Config{Update: &config.UpdateConfig{
			DrainIdleWait: config.Duration{Duration: 90 * time.Second},
		}}
		if got := finishIdleWaitForBackend(be, cfg); got != 90*time.Second {
			t.Errorf("finishIdleWaitForBackend(process, 90s) = %v, want 90s", got)
		}
	})
}
