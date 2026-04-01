package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AlpineImage returns the Alpine image reference from docker/server.Dockerfile.
// This is the version we ship with, so integration tests should use it.
func AlpineImage(t *testing.T) string {
	t.Helper()
	root := mustRepoRoot()
	data, err := os.ReadFile(filepath.Join(root, "docker", "server.Dockerfile"))
	if err != nil {
		t.Fatalf("read server.Dockerfile: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "FROM alpine:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "FROM "))
		}
	}
	t.Fatal("no FROM alpine: line in docker/server.Dockerfile")
	return ""
}
