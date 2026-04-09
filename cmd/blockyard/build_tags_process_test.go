//go:build minimal && process_backend && !docker_backend

package main

import "testing"

// TestProcessOnlyVariantFactories verifies the process-only minimal
// variant registers only the process factory. Built and run as part
// of the CI matrix against the `-tags "minimal,process_backend"`
// configuration.
func TestProcessOnlyVariantFactories(t *testing.T) {
	got := availableBackends()
	if len(got) != 1 || got[0] != "process" {
		t.Errorf("availableBackends() = %v, want [process]", got)
	}
}
