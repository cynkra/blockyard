//go:build minimal && docker_backend && !process_backend

package main

import "testing"

// TestDockerOnlyVariantFactories verifies the docker-only minimal
// variant registers only the docker factory. Built and run as part
// of the CI matrix against the `-tags "minimal,docker_backend"`
// configuration.
func TestDockerOnlyVariantFactories(t *testing.T) {
	got := availableBackends()
	if len(got) != 1 || got[0] != "docker" {
		t.Errorf("availableBackends() = %v, want [docker]", got)
	}
}
