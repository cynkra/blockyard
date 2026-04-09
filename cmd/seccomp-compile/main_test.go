package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompileMinimalProfile runs the compile path against a
// hand-written minimal profile. Success is the BPF file existing and
// being non-empty — deeper correctness lives in the integration test
// that loads the blob into a real bwrap child (process_test tag).
func TestCompileMinimalProfile(t *testing.T) {
	dir := t.TempDir()
	input := `{
		"defaultAction": "SCMP_ACT_ERRNO",
		"syscalls": [
			{"names": ["read", "write", "exit", "exit_group"], "action": "SCMP_ACT_ALLOW"}
		]
	}`
	inPath := filepath.Join(dir, "profile.json")
	outPath := filepath.Join(dir, "profile.bpf")
	if err := os.WriteFile(inPath, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := compile(inPath, outPath); err != nil {
		t.Fatalf("compile: %v", err)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Error("output BPF file is empty")
	}
}

func TestCompileCommittedOuterProfile(t *testing.T) {
	// Compile the actual committed profile to catch regressions
	// where a merge produces a JSON file libseccomp can't read.
	dir := t.TempDir()
	outPath := filepath.Join(dir, "blockyard-bwrap.bpf")
	if err := compile("../../internal/seccomp/blockyard-bwrap.json", outPath); err != nil {
		t.Fatalf("compile committed profile: %v", err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 100 {
		t.Errorf("committed profile compiled to implausibly small BPF (%d bytes)", info.Size())
	}
}

func TestCompileRejectsUnknownAction(t *testing.T) {
	dir := t.TempDir()
	input := `{
		"defaultAction": "SCMP_ACT_ERRNO",
		"syscalls": [
			{"names": ["read"], "action": "NOPE"}
		]
	}`
	inPath := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(inPath, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	err := compile(inPath, filepath.Join(dir, "out.bpf"))
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("expected unknown action error, got %v", err)
	}
}

func TestCompileSkipsUnknownSyscalls(t *testing.T) {
	dir := t.TempDir()
	// Mix of a real and a fake syscall name. The fake one should
	// be silently skipped, not error the whole compile.
	input := `{
		"defaultAction": "SCMP_ACT_ERRNO",
		"syscalls": [
			{"names": ["read", "definitely_not_a_real_syscall_12345"], "action": "SCMP_ACT_ALLOW"}
		]
	}`
	inPath := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(inPath, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compile(inPath, filepath.Join(dir, "out.bpf")); err != nil {
		t.Errorf("compile failed on unknown syscall: %v", err)
	}
}
