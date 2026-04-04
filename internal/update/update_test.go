package update

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestInferChannel(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"main+abc123", "main"},
		{"main+2026-03-26-abc", "main"},
		{"1.0.0", "stable"},
		{"0.9.0-rc1", "stable"},
		{"", "stable"},
	}
	for _, tt := range tests {
		if got := InferChannel(tt.version); got != tt.want {
			t.Errorf("InferChannel(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestFetchRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("expected Accept header, got %q", r.Header.Get("Accept"))
		}
		json.NewEncoder(w).Encode(GitHubRelease{
			TagName: "v1.2.3",
			Name:    "Release 1.2.3",
			Assets: []GitHubAsset{
				{Name: "blockyard-linux-amd64.tar.gz", URL: "https://example.com/asset"},
			},
		})
	}))
	defer srv.Close()

	rel, err := FetchRelease(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName = %q, want %q", rel.TagName, "v1.2.3")
	}
	if rel.Name != "Release 1.2.3" {
		t.Errorf("Name = %q, want %q", rel.Name, "Release 1.2.3")
	}
	if len(rel.Assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(rel.Assets))
	}
	if rel.Assets[0].Name != "blockyard-linux-amd64.tar.gz" {
		t.Errorf("Asset name = %q", rel.Assets[0].Name)
	}
}

func TestFetchRelease_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := FetchRelease(srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestFetchRelease_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := FetchRelease(srv.URL)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFetchLatestStableRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v2.0.0"})
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	rel, err := FetchLatestStableRelease()
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v2.0.0" {
		t.Errorf("TagName = %q, want %q", rel.TagName, "v2.0.0")
	}
}

func TestFetchReleaseByTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/tags/main" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "main", Name: "main+abc123"})
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	rel, err := FetchReleaseByTag("main")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Name != "main+abc123" {
		t.Errorf("Name = %q, want %q", rel.Name, "main+abc123")
	}
}

func TestCheckLatest_Stable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.5.0"})
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	result, err := CheckLatest("stable", "1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if result.Channel != "stable" {
		t.Errorf("Channel = %q", result.Channel)
	}
	if result.CurrentVersion != "1.4.0" {
		t.Errorf("CurrentVersion = %q", result.CurrentVersion)
	}
	if result.LatestVersion != "1.5.0" {
		t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, "1.5.0")
	}
	if !result.UpdateAvailable {
		t.Error("expected UpdateAvailable=true")
	}
}

func TestCheckLatest_StableUpToDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.4.0"})
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	result, err := CheckLatest("stable", "1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if result.UpdateAvailable {
		t.Error("expected UpdateAvailable=false when versions match")
	}
}

func TestCheckLatest_Main(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{
			TagName: "main",
			Name:    "main+newbuild",
		})
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	result, err := CheckLatest("main", "main+oldbuild")
	if err != nil {
		t.Fatal(err)
	}
	if result.LatestVersion != "main+newbuild" {
		t.Errorf("LatestVersion = %q", result.LatestVersion)
	}
	if !result.UpdateAvailable {
		t.Error("expected UpdateAvailable=true for different main builds")
	}
}

func TestCheckLatest_FetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	_, err := CheckLatest("stable", "1.0.0")
	if err == nil {
		t.Fatal("expected error when fetch fails")
	}

	_, err = CheckLatest("main", "main+abc")
	if err == nil {
		t.Fatal("expected error when fetch fails for main channel")
	}
}

func TestAddGitHubAuth_WithToken(t *testing.T) {
	old := os.Getenv("GITHUB_TOKEN")
	os.Setenv("GITHUB_TOKEN", "test-token-123")
	defer func() {
		if old == "" {
			os.Unsetenv("GITHUB_TOKEN")
		} else {
			os.Setenv("GITHUB_TOKEN", old)
		}
	}()

	req, _ := http.NewRequest("GET", "https://example.com", nil)
	AddGitHubAuth(req)

	if got := req.Header.Get("Authorization"); got != "Bearer test-token-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token-123")
	}
}

func TestAddGitHubAuth_NoToken(t *testing.T) {
	old := os.Getenv("GITHUB_TOKEN")
	os.Unsetenv("GITHUB_TOKEN")
	defer func() {
		if old != "" {
			os.Setenv("GITHUB_TOKEN", old)
		}
	}()

	req, _ := http.NewRequest("GET", "https://example.com", nil)
	AddGitHubAuth(req)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("expected no Authorization header, got %q", got)
	}
}
