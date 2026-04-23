package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
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
	// Stub at internal/docs/dist/index.html indicates the Hugo build
	// step was skipped. In CI this is a contract violation — the unit
	// job's Hugo prep must run so TestDocsServesHTML exercises the
	// real embed (the gap that let broken docs reach prod before).
	// Locally, skip so `go test ./...` stays green without requiring
	// every developer to run Hugo first.
	if strings.Contains(body, "Documentation was not included in this build") {
		const msg = "docs stub served — run `hugo --minify --baseURL /docs/` from docs/ and copy public/ to internal/docs/dist/ to exercise this test (CI unit job does this automatically)"
		if os.Getenv("CI") != "" {
			t.Fatal(msg)
		}
		t.Skip(msg)
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
