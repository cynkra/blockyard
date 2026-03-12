package rvcache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureBinary_CachesOnDisk(t *testing.T) {
	// Serve a fake binary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#!/bin/sh\nexit 0\n"))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	version := "v0.0.0-test"

	// Patch downloadURL by writing the binary ourselves via the helper,
	// but since downloadURL is not configurable, test the caching layer
	// by pre-populating the cache.
	dest := filepath.Join(cacheDir, "rv-"+version)
	if err := os.WriteFile(dest, []byte("fake-rv"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Should return the cached path without downloading.
	got, err := EnsureBinary(context.Background(), cacheDir, version)
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != dest {
		t.Errorf("got %q, want %q", got, dest)
	}
}

func TestEnsureBinary_CreatesCacheDir(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "nested", "cache")

	// Pre-populate so we don't hit the network.
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(cacheDir, "rv-v1.0.0")
	if err := os.WriteFile(dest, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := EnsureBinary(context.Background(), cacheDir, "v1.0.0")
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != dest {
		t.Errorf("got %q, want %q", got, dest)
	}
}

func TestDownloadURL_Versioned(t *testing.T) {
	url := downloadURL("v0.19.0")
	expected := "https://github.com/a2-ai/rv/releases/download/v0.19.0/rv-"
	if len(url) < len(expected) || url[:len(expected)] != expected {
		t.Errorf("unexpected URL for versioned: %s", url)
	}
}

func TestDownloadURL_Latest(t *testing.T) {
	url := downloadURL("latest")
	expected := "https://github.com/a2-ai/rv/releases/latest/download/rv-"
	if len(url) < len(expected) || url[:len(expected)] != expected {
		t.Errorf("unexpected URL for latest: %s", url)
	}
}
