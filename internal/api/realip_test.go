package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealIPMiddleware_NoCIDRs(t *testing.T) {
	mw := realIPMiddleware(nil)
	var got string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "1.2.3.4:5678" {
		t.Fatalf("expected original RemoteAddr, got %q", got)
	}
}

func TestRealIPMiddleware_UntrustedPeer(t *testing.T) {
	mw := realIPMiddleware([]string{"10.0.0.0/8"})
	var got string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	// Peer is not in trusted range — header ignored.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "1.2.3.4:5678" {
		t.Fatalf("expected original RemoteAddr, got %q", got)
	}
}

func TestRealIPMiddleware_TrustedPeerXFF(t *testing.T) {
	mw := realIPMiddleware([]string{"10.0.0.0/8"})
	var got string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 10.0.0.2")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "203.0.113.50" {
		t.Fatalf("expected real client IP, got %q", got)
	}
}

func TestRealIPMiddleware_TrustedPeerXRealIP(t *testing.T) {
	mw := realIPMiddleware([]string{"10.0.0.0/8"})
	var got string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5678"
	req.Header.Set("X-Real-IP", "203.0.113.50")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "203.0.113.50" {
		t.Fatalf("expected real client IP, got %q", got)
	}
}

func TestRealIPMiddleware_AllTrusted(t *testing.T) {
	mw := realIPMiddleware([]string{"10.0.0.0/8"})
	var got string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	// All IPs in XFF are trusted — keep original.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5678"
	req.Header.Set("X-Forwarded-For", "10.0.0.5, 10.0.0.3")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "10.0.0.1:5678" {
		t.Fatalf("expected original RemoteAddr when all XFF trusted, got %q", got)
	}
}

func TestRealIPMiddleware_MultipleCIDRs(t *testing.T) {
	mw := realIPMiddleware([]string{"10.0.0.0/8", "172.16.0.0/12"})
	var got string
	handler := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.RemoteAddr
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.16.0.1:5678"
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 10.0.0.5")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got != "198.51.100.1" {
		t.Fatalf("expected real client IP with multiple CIDRs, got %q", got)
	}
}
