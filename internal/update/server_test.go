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
	version   string
	available string
}

func (m *mockStore) SetUpdateAvailable(v string) { m.available = v }
func (m *mockStore) GetVersion() string           { return m.version }

func TestCheck_UpdateAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v2.0.0"})
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	store := &mockStore{version: "1.0.0"}
	check("1.0.0", store)

	if store.available != "2.0.0" {
		t.Errorf("expected available=2.0.0, got %q", store.available)
	}
}

func TestCheck_NoUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	store := &mockStore{version: "1.0.0"}
	check("1.0.0", store)

	if store.available != "" {
		t.Errorf("expected no update, got %q", store.available)
	}
}

func TestCheck_FetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()

	store := &mockStore{version: "1.0.0"}
	// Should not panic or set update.
	check("1.0.0", store)

	if store.available != "" {
		t.Errorf("expected no update on error, got %q", store.available)
	}
}

func TestSpawnChecker_CancelledImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &mockStore{version: "1.0.0"}
	done := make(chan struct{})
	go func() {
		SpawnChecker(ctx, "1.0.0", store)
		close(done)
	}()

	select {
	case <-done:
		// Returned as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("SpawnChecker did not return after context cancel")
	}
}
