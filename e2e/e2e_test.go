//go:build e2e

package e2e_test

import (
	"os"
	"os/exec"
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

	os.Exit(m.Run())
}
