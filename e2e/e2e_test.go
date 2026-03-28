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

func TestMain(m *testing.M) {
	// Find repo root.
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		panic("git rev-parse --show-toplevel: " + err.Error())
	}
	repoRoot := string(out[:len(out)-1]) // trim newline

	// Build the blockyard image from source so tests exercise current code.
	cmd := exec.Command("docker", "build",
		"-t", "ghcr.io/cynkra/blockyard:latest",
		"-f", "docker/server.Dockerfile", ".")
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
	build := exec.Command("go", "build", "-o", byBin, "./cmd/by")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		panic(fmt.Sprintf("build CLI: %v\n%s", err, out))
	}

	code := m.Run()
	os.RemoveAll(tmpDir)
	os.Exit(code)
}
