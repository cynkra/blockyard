package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeProfilePrependsOverlay runs the full merge on a minimal
// synthetic upstream+overlay pair and verifies the overlay rules
// appear at the head of the merged syscalls list. This is the
// load-bearing ordering guarantee: unconditional allows must
// precede any cap-gated rules referencing the same syscalls.
func TestMergeProfilePrependsOverlay(t *testing.T) {
	dir := t.TempDir()
	upstream := `{
		"defaultAction": "SCMP_ACT_ERRNO",
		"syscalls": [
			{"names": ["read"], "action": "SCMP_ACT_ALLOW"},
			{"names": ["clone"], "action": "SCMP_ACT_ALLOW", "includes": {"caps": ["CAP_SYS_ADMIN"]}}
		]
	}`
	overlay := `{
		"description": "test",
		"prependRules": [
			{"names": ["clone", "unshare"], "action": "SCMP_ACT_ALLOW"}
		]
	}`
	writeFile(t, filepath.Join(dir, "up.json"), upstream)
	writeFile(t, filepath.Join(dir, "ov.json"), overlay)

	merged, err := mergeProfile(filepath.Join(dir, "up.json"), filepath.Join(dir, "ov.json"))
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names    []string `json:"names"`
			Action   string   `json:"action"`
			Includes struct {
				Caps []string `json:"caps"`
			} `json:"includes"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(merged, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction not preserved: %q", parsed.DefaultAction)
	}
	if len(parsed.Syscalls) != 3 {
		t.Fatalf("expected 3 syscall rules, got %d", len(parsed.Syscalls))
	}
	// First rule: overlay's prepend — clone+unshare, no caps.
	first := parsed.Syscalls[0]
	if len(first.Includes.Caps) != 0 {
		t.Errorf("prepended rule has caps gating: %v", first.Includes.Caps)
	}
	if !contains(first.Names, "clone") || !contains(first.Names, "unshare") {
		t.Errorf("prepended rule missing expected syscalls: %v", first.Names)
	}
	// Second rule: upstream's first — read.
	if parsed.Syscalls[1].Names[0] != "read" {
		t.Errorf("upstream ordering lost: %v", parsed.Syscalls[1])
	}
	// Third rule: upstream's cap-gated clone.
	if len(parsed.Syscalls[2].Includes.Caps) == 0 {
		t.Errorf("cap-gated rule lost its gating: %v", parsed.Syscalls[2])
	}
}

func TestMergeProfileRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "up.json"), "not json")
	writeFile(t, filepath.Join(dir, "ov.json"), `{"prependRules":[]}`)
	_, err := mergeProfile(filepath.Join(dir, "up.json"), filepath.Join(dir, "ov.json"))
	if err == nil {
		t.Error("expected error on malformed upstream")
	}
	if !strings.Contains(err.Error(), "upstream") {
		t.Errorf("unexpected error: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
