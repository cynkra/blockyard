package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEnsureRProfile(t *testing.T) {
	// Reset the sync.Once so this test is self-contained.
	rProfileOnce = sync.Once{}
	rProfilePath = ""
	rProfileErr = nil

	dir := t.TempDir()
	path, err := EnsureRProfile(dir)
	if err != nil {
		t.Fatalf("EnsureRProfile: %v", err)
	}

	if filepath.Dir(path) != dir {
		t.Errorf("profile written to %q, expected dir %q", path, dir)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	content := string(data)

	// Must bridge SHINY_HOST and SHINY_PORT to R options.
	if !strings.Contains(content, "SHINY_HOST") {
		t.Error("profile does not reference SHINY_HOST")
	}
	if !strings.Contains(content, "SHINY_PORT") {
		t.Error("profile does not reference SHINY_PORT")
	}
	if !strings.Contains(content, "shiny.host") {
		t.Error("profile does not set shiny.host option")
	}
	if !strings.Contains(content, "shiny.port") {
		t.Error("profile does not set shiny.port option")
	}
}

func TestEnsureRProfile_Idempotent(t *testing.T) {
	rProfileOnce = sync.Once{}
	rProfilePath = ""
	rProfileErr = nil

	dir := t.TempDir()
	p1, err1 := EnsureRProfile(dir)
	if err1 != nil {
		t.Fatal(err1)
	}

	// Second call returns the same path without error.
	p2, err2 := EnsureRProfile(dir)
	if err2 != nil {
		t.Fatal(err2)
	}
	if p1 != p2 {
		t.Errorf("second call returned %q, want %q", p2, p1)
	}
}
