package rvcache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rvTarGz returns a tar.gz archive containing a single "rv" entry with the
// given content. This mirrors the real release asset format.
func rvTarGz(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "rv", Size: int64(len(content)), Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

// setBaseURL overrides the package-level baseURL for the duration of the test.
func setBaseURL(t *testing.T, url string) {
	t.Helper()
	orig := baseURL
	baseURL = url
	t.Cleanup(func() { baseURL = orig })
}

// ---------- EnsureBinary (full download path) ----------

func TestEnsureBinary_DownloadsAndExtractsTarGz(t *testing.T) {
	binary := []byte("#!/bin/sh\necho rv\n")
	tarball := rvTarGz(t, binary)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			t.Errorf("expected .tar.gz request, got %s", r.URL.Path)
		}
		w.Write(tarball)
	}))
	defer srv.Close()
	setBaseURL(t, srv.URL)

	cacheDir := t.TempDir()
	got, err := EnsureBinary(context.Background(), cacheDir, "v0.0.1-test")
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}

	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read cached binary: %v", err)
	}
	if !bytes.Equal(data, binary) {
		t.Errorf("cached binary = %q, want %q", data, binary)
	}

	info, _ := os.Stat(got)
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("cached binary is not executable")
	}
}

func TestEnsureBinary_404ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	setBaseURL(t, srv.URL)

	_, err := EnsureBinary(context.Background(), t.TempDir(), "v99.99.99")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got: %v", err)
	}
}

func TestEnsureBinary_BadTarballReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not a tarball"))
	}))
	defer srv.Close()
	setBaseURL(t, srv.URL)

	_, err := EnsureBinary(context.Background(), t.TempDir(), "v0.0.1-test")
	if err == nil {
		t.Fatal("expected error for corrupt tarball")
	}
}

// ---------- EnsureBinary (cache layer) ----------

func TestEnsureBinary_CachesOnDisk(t *testing.T) {
	cacheDir := t.TempDir()
	version := "v0.0.0-test"

	// Pre-populate the cache.
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

// ---------- extractRvFromTarGz ----------

func TestExtractRvFromTarGz(t *testing.T) {
	content := []byte("#!/bin/sh\nexit 0\n")
	tarball := rvTarGz(t, content)

	var out bytes.Buffer
	if err := extractRvFromTarGz(bytes.NewReader(tarball), &out); err != nil {
		t.Fatalf("extractRvFromTarGz: %v", err)
	}
	if !bytes.Equal(out.Bytes(), content) {
		t.Errorf("got %q, want %q", out.Bytes(), content)
	}
}

func TestExtractRvFromTarGz_MissingEntry(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	tw.WriteHeader(&tar.Header{Name: "not-rv", Size: 3, Mode: 0o644})
	tw.Write([]byte("foo"))
	tw.Close()
	gw.Close()

	var out bytes.Buffer
	if err := extractRvFromTarGz(&buf, &out); err == nil {
		t.Fatal("expected error for missing rv entry")
	}
}

// ---------- downloadURL ----------

func TestDownloadURL_Versioned(t *testing.T) {
	url := downloadURL("v0.19.0")
	if !strings.Contains(url, "/download/v0.19.0/rv-v0.19.0-") {
		t.Errorf("unexpected URL for versioned: %s", url)
	}
	if !strings.HasSuffix(url, ".tar.gz") {
		t.Errorf("expected .tar.gz suffix, got: %s", url)
	}
}

func TestDownloadURL_Latest(t *testing.T) {
	url := downloadURL("latest")
	if !strings.Contains(url, "/latest/download/rv-") {
		t.Errorf("unexpected URL for latest: %s", url)
	}
	if !strings.HasSuffix(url, ".tar.gz") {
		t.Errorf("expected .tar.gz suffix, got: %s", url)
	}
}

func TestEnsureBinary_LatestRedownloadsWhenStale(t *testing.T) {
	oldContent := []byte("#!/bin/sh\necho old\n")
	newContent := []byte("#!/bin/sh\necho new\n")
	newTarball := rvTarGz(t, newContent)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(newTarball)
	}))
	defer srv.Close()
	setBaseURL(t, srv.URL)

	cacheDir := t.TempDir()
	dest := filepath.Join(cacheDir, "rv-latest")

	// Pre-populate with old content and set mtime to 2 hours ago.
	if err := os.WriteFile(dest, oldContent, 0o755); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dest, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	got, err := EnsureBinary(context.Background(), cacheDir, "latest")
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}

	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read cached binary: %v", err)
	}
	if !bytes.Equal(data, newContent) {
		t.Errorf("expected new content after stale refresh, got %q", data)
	}
}

func TestEnsureBinary_LatestSkipsWhenFresh(t *testing.T) {
	// Server that fails if called — we expect no download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called for fresh latest binary")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	setBaseURL(t, srv.URL)

	cacheDir := t.TempDir()
	dest := filepath.Join(cacheDir, "rv-latest")

	// Pre-populate with fresh mtime (now).
	if err := os.WriteFile(dest, []byte("fresh-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := EnsureBinary(context.Background(), cacheDir, "latest")
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != dest {
		t.Errorf("got %q, want %q", got, dest)
	}
}

func TestDownload_ServerError500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	setBaseURL(t, srv.URL)

	_, err := EnsureBinary(context.Background(), t.TempDir(), "v0.0.1-test")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500, got: %v", err)
	}
}
