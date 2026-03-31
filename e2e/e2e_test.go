//go:build e2e

package e2e_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const (
	dexEmail1      = "demo@example.com"
	dexEmail2      = "demo2@example.com"
	dexPassword    = "password"
	demoSub1       = "Cg1kZW1vLXVzZXItMDAxEgVsb2NhbA"
	demoSub2       = "Cg1kZW1vLXVzZXItMDAyEgVsb2NhbA"
	vaultRootToken = "root-dev-token"
)

// byBin is the path to the compiled CLI binary, set once in TestMain.
var byBin string

// covDir holds the path to the e2e coverage directory (set when
// E2E_GOCOVERDIR is non-empty). CLI invocations write coverage data
// here; the server container writes to a subdirectory via bind mount.
var covDir string

func TestMain(m *testing.M) {
	// Find repo root.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		panic("git rev-parse --show-toplevel: " + err.Error())
	}
	repoRoot := string(out[:len(out)-1]) // trim newline

	// Coverage mode: when E2E_GOCOVERDIR is set, build with -cover so
	// the server and CLI binaries write coverage data on exit.
	covDir = os.Getenv("E2E_GOCOVERDIR")

	// Build the blockyard image from source so tests exercise current code.
	buildArgs := []string{"build",
		"-t", "ghcr.io/cynkra/blockyard:latest",
		"-t", "ghcr.io/cynkra/blockyard:main",
		"-f", "docker/server.Dockerfile"}
	if covDir != "" {
		buildArgs = append(buildArgs, "--build-arg", "COVER=1")
	}
	buildArgs = append(buildArgs, ".")
	cmd := exec.Command("docker", buildArgs...)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("docker build failed: " + err.Error())
	}

	// Build the CLI binary so e2e tests exercise it end-to-end.
	tmpDir, err := os.MkdirTemp("", "by-e2e-*")
	if err != nil {
		panic("temp dir: " + err.Error())
	}

	byBin = filepath.Join(tmpDir, "by")
	cliBuildArgs := []string{"build"}
	if covDir != "" {
		cliBuildArgs = append(cliBuildArgs, "-cover")
	}
	cliBuildArgs = append(cliBuildArgs, "-o", byBin, "./cmd/by")
	build := exec.Command("go", cliBuildArgs...)
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		panic(fmt.Sprintf("build CLI: %v\n%s", err, out))
	}

	// Ensure coverage subdirectories exist.
	if covDir != "" {
		os.MkdirAll(filepath.Join(covDir, "cli"), 0o755)
		os.MkdirAll(filepath.Join(covDir, "server"), 0o755)
	}

	code := m.Run()
	os.RemoveAll(tmpDir)
	os.Exit(code)
}
