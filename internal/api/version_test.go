package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersionHeader(t *testing.T) {
	handler := versionHeader("v1.2.3")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	got := rec.Header().Get("X-Blockyard-Version")
	if got != "v1.2.3" {
		t.Errorf("expected v1.2.3, got %q", got)
	}
}

func TestVersionHeader_Empty(t *testing.T) {
	handler := versionHeader("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if got := rec.Header().Get("X-Blockyard-Version"); got != "" {
		t.Errorf("expected empty header, got %q", got)
	}
}
