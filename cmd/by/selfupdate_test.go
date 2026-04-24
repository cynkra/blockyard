package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/update"
)

func TestInferChannel(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		// Clean semver tags map to the stable release stream.
		{"0.0.3", "stable"},
		{"1.2.3", "stable"},
		{"v1.2.3", "stable"},
		// SHA-shaped builds (legacy main+, git describe, bare hash) and
		// the unidentified "dev" placeholder all default to main.
		{"main+abc1234", "main"},
		{"v0.0.3-3-gabc1234", "main"},
		{"abc1234", "main"},
		{"dev", "main"},
	}
	for _, tt := range tests {
		got := update.InferChannel(tt.version)
		if got != tt.want {
			t.Errorf("InferChannel(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestSelfUpdateBinaryName(t *testing.T) {
	name := selfUpdateBinaryName()
	want := "by-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if name != want {
		t.Errorf("selfUpdateBinaryName() = %q, want %q", name, want)
	}
}

func TestFetchRelease(t *testing.T) {
	rel := update.GitHubRelease{
		TagName: "v0.0.3",
		Name:    "0.0.3",
		Assets: []update.GitHubAsset{
			{Name: "by-linux-amd64", URL: "https://example.com/by-linux-amd64"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			json.NewEncoder(w).Encode(rel)
		case "/releases/tags/main":
			json.NewEncoder(w).Encode(update.GitHubRelease{
				TagName: "main",
				Name:    "main+abc1234",
				Assets:  rel.Assets,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	old := update.APIBase
	update.APIBase = srv.URL
	defer func() { update.APIBase = old }()

	t.Run("stable", func(t *testing.T) {
		got, err := update.FetchLatestStableRelease()
		if err != nil {
			t.Fatal(err)
		}
		if got.TagName != "v0.0.3" {
			t.Errorf("TagName = %q, want v0.0.3", got.TagName)
		}
	})

	t.Run("main", func(t *testing.T) {
		got, err := update.FetchReleaseByTag("main")
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "main+abc1234" {
			t.Errorf("Name = %q, want main+abc1234", got.Name)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		_, err := update.FetchReleaseByTag("nonexistent")
		if err == nil {
			t.Fatal("expected error for missing release")
		}
	})
}

func TestDownloadAsset(t *testing.T) {
	content := []byte("#!/bin/sh\necho hello\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "by-test")
	if err := downloadAsset(srv.URL+"/asset", dst); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("downloaded content = %q, want %q", got, content)
	}

	info, _ := os.Stat(dst)
	if info.Mode()&0o111 == 0 {
		t.Error("downloaded file is not executable")
	}
}

func TestSelfUpdateCmd_AlreadyUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(update.GitHubRelease{
			TagName: "v0.0.3",
			Name:    "0.0.3",
		})
	}))
	defer srv.Close()

	oldAPI := update.APIBase
	update.APIBase = srv.URL
	defer func() { update.APIBase = oldAPI }()

	oldVersion := version
	version = "0.0.3"
	defer func() { version = oldVersion }()

	cmd := selfUpdateCmd()
	cmd.SetArgs([]string{})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	if got := out; got != "Already up to date (0.0.3).\n" {
		t.Errorf("output = %q", got)
	}
}

// selfUpdateReleaseHandler routes /releases/latest and /releases/tags/main
// to a pre-baked release payload. Returns the server; caller must Close.
func selfUpdateReleaseHandler(t *testing.T, stable, main update.GitHubRelease) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			_ = json.NewEncoder(w).Encode(stable)
		case "/releases/tags/main":
			_ = json.NewEncoder(w).Encode(main)
		default:
			http.NotFound(w, r)
		}
	}))
}

// withVersionAndAPI swaps `version` and `update.APIBase` for the test
// and restores them on cleanup.
func withVersionAndAPI(t *testing.T, ver, api string) {
	t.Helper()
	oldV, oldA := version, update.APIBase
	version = ver
	update.APIBase = api
	t.Cleanup(func() {
		version = oldV
		update.APIBase = oldA
	})
}

func TestSelfUpdateCmd_AlreadyUpToDate_JSON(t *testing.T) {
	srv := selfUpdateReleaseHandler(t,
		update.GitHubRelease{TagName: "v0.0.3", Name: "0.0.3"},
		update.GitHubRelease{})
	defer srv.Close()
	withVersionAndAPI(t, "0.0.3", srv.URL)

	cmd := selfUpdateCmd()
	cmd.Flags().Bool("json", true, "")
	_ = cmd.Flags().Set("json", "true")
	cmd.SetArgs([]string{})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal JSON: %v\n%s", err, out)
	}
	if got["status"] != "up_to_date" {
		t.Errorf("status = %v, want up_to_date", got["status"])
	}
	if got["current_version"] != "0.0.3" {
		t.Errorf("current_version = %v, want 0.0.3", got["current_version"])
	}
	if got["channel"] != "stable" {
		t.Errorf("channel = %v, want stable", got["channel"])
	}
}

// TestSelfUpdateCmd_MainChannel_UpToDate exercises the "main" channel
// path (the other arm of the channel switch) without touching the
// binary-replace logic. The main-tag release carries a fresh SHA each
// build, so "up to date" requires version == release.Name verbatim.
func TestSelfUpdateCmd_MainChannel_UpToDate(t *testing.T) {
	srv := selfUpdateReleaseHandler(t,
		update.GitHubRelease{},
		update.GitHubRelease{TagName: "main", Name: "main+abc1234"})
	defer srv.Close()
	withVersionAndAPI(t, "main+abc1234", srv.URL)

	cmd := selfUpdateCmd()
	cmd.SetArgs([]string{"--channel", "main"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if got := out; got != "Already up to date (main+abc1234).\n" {
		t.Errorf("output = %q", got)
	}
}

// TestSelfUpdateCmd_InferChannelFromVersion verifies the auto-inferred
// channel when --channel is not passed: a SHA-shaped version picks
// "main" and fetches /releases/tags/main.
func TestSelfUpdateCmd_InferChannelFromVersion(t *testing.T) {
	srv := selfUpdateReleaseHandler(t,
		update.GitHubRelease{},
		update.GitHubRelease{TagName: "main", Name: "main+deadbee"})
	defer srv.Close()
	withVersionAndAPI(t, "main+deadbee", srv.URL)

	cmd := selfUpdateCmd()
	cmd.SetArgs([]string{})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if got := out; got != "Already up to date (main+deadbee).\n" {
		t.Errorf("output = %q", got)
	}
}

// TestSelfUpdateBinaryName_Windows sanity-checks the `.exe` suffix
// branch of selfUpdateBinaryName. The runtime GOOS can't be switched
// at test time, so we only assert the suffix invariant; the other leg
// is exercised by TestSelfUpdateBinaryName on Linux/macOS CI runners.
func TestSelfUpdateBinaryName_Suffix(t *testing.T) {
	name := selfUpdateBinaryName()
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		t.Errorf("expected .exe suffix on Windows, got %q", name)
	}
	if runtime.GOOS != "windows" && strings.HasSuffix(name, ".exe") {
		t.Errorf("unexpected .exe suffix on %s, got %q", runtime.GOOS, name)
	}
}

// TestDownloadAsset_HTTPError covers the non-200 response path.
func TestDownloadAsset_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "by-test")
	err := downloadAsset(srv.URL+"/asset", dst)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want 500 in message", err)
	}
}

// TestDownloadAsset_BadURL covers the http.NewRequest error path.
func TestDownloadAsset_BadURL(t *testing.T) {
	err := downloadAsset("://malformed", filepath.Join(t.TempDir(), "by-test"))
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

// TestDownloadAsset_UnreachableHost covers the http.DefaultClient.Do
// failure path (connection refused).
func TestDownloadAsset_UnreachableHost(t *testing.T) {
	err := downloadAsset("http://127.0.0.1:1/asset", filepath.Join(t.TempDir(), "by-test"))
	if err == nil {
		t.Fatal("expected error for unreachable host")
	}
}

// TestDownloadAsset_OpenFails covers the os.OpenFile error path (dst
// inside a nonexistent dir).
func TestDownloadAsset_OpenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("content")) //nolint:errcheck
	}))
	defer srv.Close()

	err := downloadAsset(srv.URL+"/asset", filepath.Join(t.TempDir(), "nonexistent-dir", "by"))
	if err == nil {
		t.Fatal("expected error for unwritable dst")
	}
}
