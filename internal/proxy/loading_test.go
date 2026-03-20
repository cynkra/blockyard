package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cynkra/blockyard/internal/db"
)

func TestIsBrowserRequest(t *testing.T) {
	tests := []struct {
		accept string
		want   bool
	}{
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"text/html", true},
		{"application/json", false},
		{"", false},
		{"*/*", false},
		{"application/xml, text/html", true},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		if tt.accept != "" {
			r.Header.Set("Accept", tt.accept)
		}
		got := isBrowserRequest(r)
		if got != tt.want {
			t.Errorf("isBrowserRequest(%q) = %v, want %v", tt.accept, got, tt.want)
		}
	}
}

func TestServeLoadingPage(t *testing.T) {
	srv := testColdstartServer(t)
	app := &db.AppRow{Name: "my-app"}

	rec := httptest.NewRecorder()
	serveLoadingPage(rec, app, "my-app", srv)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected no-store, got %q", cc)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "my-app") {
		t.Error("expected app name in body")
	}
	if !strings.Contains(body, "__blockyard/ready") {
		t.Error("expected ready URL in body")
	}
	if !strings.Contains(body, "spinner") {
		t.Error("expected spinner in body")
	}
}

func TestServeLoadingPageWithTitle(t *testing.T) {
	srv := testColdstartServer(t)
	title := "My Dashboard"
	app := &db.AppRow{Name: "my-app", Title: &title}

	rec := httptest.NewRecorder()
	serveLoadingPage(rec, app, "my-app", srv)

	body := rec.Body.String()
	if !strings.Contains(body, "My Dashboard") {
		t.Error("expected title in body")
	}
}

func TestDisplayName(t *testing.T) {
	// No title — use name.
	app := &db.AppRow{Name: "my-app"}
	if got := displayName(app); got != "my-app" {
		t.Errorf("expected my-app, got %q", got)
	}

	// Empty title — use name.
	empty := ""
	app.Title = &empty
	if got := displayName(app); got != "my-app" {
		t.Errorf("expected my-app, got %q", got)
	}

	// Non-empty title — use title.
	title := "Dashboard"
	app.Title = &title
	if got := displayName(app); got != "Dashboard" {
		t.Errorf("expected Dashboard, got %q", got)
	}
}
