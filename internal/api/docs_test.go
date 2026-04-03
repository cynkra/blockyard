package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocsRedirect(t *testing.T) {
	srv := testServerForReadyz(t)
	router := NewRouter(srv, func() {}, nil)

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("expected 301, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/docs/" {
		t.Errorf("expected redirect to /docs/, got %q", loc)
	}
}

func TestDocsServesHTML(t *testing.T) {
	srv := testServerForReadyz(t)
	router := NewRouter(srv, func() {}, nil)

	req := httptest.NewRequest(http.MethodGet, "/docs/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html content type, got %q", ct)
	}
}
