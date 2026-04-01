package testutil

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ComposeServiceImage extracts the image for a named service from a
// docker-compose file, relative to the repo root. Panics on error so
// it can be used in TestMain and package-level vars.
func ComposeServiceImage(composeFile, service string) string {
	root := mustRepoRoot()
	data, err := os.ReadFile(filepath.Join(root, composeFile))
	if err != nil {
		panic(fmt.Sprintf("read compose file %s: %v", composeFile, err))
	}

	// Simple YAML scan: find "  <service>:" then the next "image:" line.
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	inService := false
	serviceIndent := ""
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if inService {
			// Detect end of service block: a line at the same or lower indent.
			if len(line) > 0 && !strings.HasPrefix(line, serviceIndent+"  ") && trimmed != "" {
				break
			}
			if strings.HasPrefix(trimmed, "image:") {
				img := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
				return img
			}
		}

		// Match "  <service>:" at the service level (2-space indent under services:).
		if trimmed == service+":" {
			inService = true
			serviceIndent = line[:len(line)-len(strings.TrimLeft(line, " "))]
		}
	}

	panic(fmt.Sprintf("no image found for service %q in %s", service, composeFile))
}

var tomlImageRe = regexp.MustCompile(`(?m)^image\s*=\s*"([^"]+)"`)

// TOMLDockerImage reads the docker.image field from a blockyard.toml
// file, relative to the repo root.
func TOMLDockerImage(t *testing.T) string {
	t.Helper()
	root := mustRepoRoot()
	data, err := os.ReadFile(filepath.Join(root, "blockyard.toml"))
	if err != nil {
		t.Fatalf("read blockyard.toml: %v", err)
	}
	m := tomlImageRe.FindSubmatch(data)
	if m == nil {
		t.Fatal("no image field in blockyard.toml")
	}
	return string(m[1])
}

func mustRepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("getwd: %v", err))
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not find repo root (no go.mod)")
		}
		dir = parent
	}
}
