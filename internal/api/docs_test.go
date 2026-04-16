package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocsRedirect(t *testing.T) {
	srv := testServerForReadyz(t)
	router := NewRouter(srv, func() {}, nil, context.Background())

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
	router := NewRouter(srv, func() {}, nil, context.Background())

	req := httptest.NewRequest(http.MethodGet, "/docs/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html content type, got %q", ct)
	}

	body := rec.Body.String()
	// Guard against the "docs not included" stub at
	// internal/docs/dist/index.html being served because the Hugo
	// build step was skipped. The stub is valid HTML, so status +
	// content-type alone passes silently — that gap is exactly how
	// broken docs reached production before.
	if strings.Contains(body, "Documentation was not included in this build") {
		t.Fatal("served the docs stub placeholder — run `hugo --minify --baseURL /docs/` from docs/ and copy public/ to internal/docs/dist/ before testing (CI unit job does this automatically)")
	}
	// Marker from docs/layouts/index.html; only present when Hugo
	// produced real output with the hugo-book theme submodule. Also
	// catches the case where the submodule is missing and Hugo
	// silently produces a body without the baseof-wrapped main
	// block.
	if !strings.Contains(body, "bk-hero") {
		preview := body
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Errorf("expected real docs home page (bk-hero class from docs/layouts/index.html) but got %d bytes; first 200: %q", len(body), preview)
	}
}
