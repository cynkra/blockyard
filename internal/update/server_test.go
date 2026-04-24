package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type mockStore struct {
	version string
	last    *Result
}

func (m *mockStore) SetUpdateStatus(r *Result) { m.last = r }
func (m *mockStore) GetVersion() string        { return m.version }

func TestPerformCheck_Semver_UpdateAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v2.0.0"})
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	store := &mockStore{version: "1.0.0"}
	res, err := PerformCheck(store, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateUpdateAvailable {
		t.Errorf("State = %q", res.State)
	}
	if store.last == nil || store.last.State != StateUpdateAvailable {
		t.Errorf("store not updated, got %+v", store.last)
	}
	if store.last.Channel != "stable" {
		t.Errorf("Channel = %q, want %q", store.last.Channel, "stable")
	}
}

func TestPerformCheck_Semver_UpToDateStillRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	store := &mockStore{version: "1.0.0"}
	_, _ = PerformCheck(store, "stable")
	if store.last == nil || store.last.State != StateUpToDate {
		t.Errorf("expected up_to_date recorded, got %+v", store.last)
	}
}

// Regression: a previous "update available" record must be replaced
// by a fresh "up to date" record once the user upgrades, otherwise
// the post-upgrade banner would linger forever.
func TestPerformCheck_OverwritesPriorAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	store := &mockStore{
		version: "1.0.0",
		last:    &Result{State: StateUpdateAvailable, LatestVersion: "2.0.0"},
	}
	_, _ = PerformCheck(store, "stable")
	if store.last.State != StateUpToDate {
		t.Errorf("expected stale available record cleared, got %+v", store.last)
	}
}

func TestPerformCheck_DevBuild(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v0.0.2"})
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	store := &mockStore{version: "dev"}
	_, _ = PerformCheck(store, "stable")
	if store.last == nil || store.last.State != StateDevBuild {
		t.Errorf("expected dev_build state, got %+v", store.last)
	}
}

func TestSpawnChecker_CancelledImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &mockStore{version: "1.0.0"}
	done := make(chan struct{})
	go func() {
		SpawnChecker(ctx, "1.0.0", "stable", store)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SpawnChecker did not return after context cancel")
	}
}
