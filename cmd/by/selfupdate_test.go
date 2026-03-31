package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cynkra/blockyard/internal/update"
)

func TestInferChannel(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"dev", "stable"},
		{"0.0.3", "stable"},
		{"1.2.3", "stable"},
		{"main+abc1234", "main"},
		{"main+0000000", "main"},
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
