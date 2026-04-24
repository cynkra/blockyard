package main

import (
	"testing"
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
