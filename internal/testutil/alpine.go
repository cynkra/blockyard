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
	root := repoRoot(t)
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

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod)")
		}
		dir = parent
	}
}
