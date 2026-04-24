package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cynkra/blockyard/internal/apparmor"
)

func TestAdminCmdStructure(t *testing.T) {
	cmd := adminCmd()
	if cmd.Use != "admin" {
		t.Errorf("Use = %q, want admin", cmd.Use)
	}
	if !cmd.HasSubCommands() {
		t.Error("expected subcommands")
	}

	subs := map[string]bool{}
	for _, sub := range cmd.Commands() {
		subs[sub.Use] = true
	}
	for _, want := range []string{"update", "rollback", "status", "install-seccomp", "install-apparmor"} {
		if !subs[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestAdminUpdateCmdFlags(t *testing.T) {
	cmd := adminUpdateCmd()
	if f := cmd.Flags().Lookup("channel"); f == nil {
		t.Error("missing --channel flag")
	}
	if f := cmd.Flags().Lookup("yes"); f == nil {
		t.Error("missing --yes flag")
	}
}

func TestAdminRollbackCmdFlags(t *testing.T) {
	cmd := adminRollbackCmd()
	if f := cmd.Flags().Lookup("yes"); f == nil {
		t.Error("missing --yes flag")
	}
}

func TestAdminStatusCmdFlags(t *testing.T) {
	cmd := adminStatusCmd()
	if f := cmd.Flags().Lookup("json"); f == nil {
		t.Error("missing --json flag")
	}
}

// TestInstallApparmorProfileWritesEmbed guards the core install path:
// the target file must contain exactly apparmor.Profile. A wrong-bytes
// regression here would ship operators a profile that silently disagrees
// with what got shipped in the release asset and the Docker image.
// The helper also mkdir-p's the parent — missing parent is the most
// likely real failure and kept here so the test covers it.
func TestInstallApparmorProfileWritesEmbed(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "dir", "blockyard")
	if err := installApparmorProfile(target); err != nil {
		t.Fatalf("installApparmorProfile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, apparmor.Profile) {
		t.Errorf("installed %d bytes, want %d (embed mismatch)",
			len(got), len(apparmor.Profile))
	}
}

// TestValidateApparmorProfileMissingParserIsNoop — hosts without
// apparmor_parser (Fedora, Arch, RHEL, minikube COS, etc.) must not
// surface a failure from `install-apparmor`. The command still writes
// the file; the validation step is a best-effort courtesy. Probe by
// overriding PATH to a dir that doesn't contain apparmor_parser.
func TestValidateApparmorProfileMissingParserIsNoop(t *testing.T) {
	if _, err := exec.LookPath("apparmor_parser"); err == nil {
		// On a host with the parser, LookPath will find it via PATH.
		// Override PATH so our LookPath call misses.
		t.Setenv("PATH", t.TempDir())
	}
	if err := validateApparmorProfile("/nonexistent/does-not-matter"); err != nil {
		t.Errorf("expected nil on missing apparmor_parser, got %v", err)
	}
}
