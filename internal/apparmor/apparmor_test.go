package apparmor

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestEmbedMatchesFile guards against the embed drifting from the
// on-disk profile source. Anyone editing blockyard can rebuild and
// ship the new bytes; anyone editing this Go package without touching
// the profile would break the shipped behaviour silently without this.
func TestEmbedMatchesFile(t *testing.T) {
	onDisk, err := os.ReadFile("blockyard")
	if err != nil {
		t.Fatalf("read profile source: %v", err)
	}
	if !bytes.Equal(onDisk, Profile) {
		t.Error("embedded Profile does not match on-disk blockyard")
	}
}

// TestProfileHasUsernsRule is the single load-bearing property of the
// profile — without the `userns,` rule there's no point shipping it.
func TestProfileHasUsernsRule(t *testing.T) {
	if !strings.Contains(string(Profile), "userns,") {
		t.Error("profile missing 'userns,' rule — the whole point of shipping this profile")
	}
}

// TestDefaultInstallPath pins the conventional AppArmor profile
// directory; changing it would break `sudo apparmor_parser -r <path>`
// for every operator following the docs.
func TestDefaultInstallPath(t *testing.T) {
	if DefaultInstallPath != "/etc/apparmor.d/blockyard" {
		t.Errorf("DefaultInstallPath = %q, want /etc/apparmor.d/blockyard", DefaultInstallPath)
	}
}
