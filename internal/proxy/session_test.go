package proxy

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestExtractSessionID(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "abc-123"})
	if got := extractSessionID(req); got != "abc-123" {
		t.Errorf("expected abc-123, got %q", got)
	}
}

func TestExtractSessionIDMissing(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	if got := extractSessionID(req); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractSessionIDEmpty(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: ""})
	if got := extractSessionID(req); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestSessionCookie(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	c := sessionCookie("sess-123", "my-app", req)
	if c.Name != cookieName {
		t.Errorf("expected name %q, got %q", cookieName, c.Name)
	}
	if c.Value != "sess-123" {
		t.Errorf("expected value sess-123, got %q", c.Value)
	}
	if c.Path != "/app/my-app/" {
		t.Errorf("expected path /app/my-app/, got %q", c.Path)
	}
	if !c.HttpOnly {
		t.Error("expected HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("expected SameSiteLax, got %v", c.SameSite)
	}
	if c.Secure {
		t.Error("expected Secure=false for plain HTTP request")
	}
}

func TestSessionCookieSecureWhenTLS(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	req.TLS = &tls.ConnectionState{}
	c := sessionCookie("sess-123", "my-app", req)
	if !c.Secure {
		t.Error("expected Secure=true for TLS request")
	}
}

func TestSessionCookieSecureWhenForwardedProto(t *testing.T) {
	req, _ := http.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	c := sessionCookie("sess-123", "my-app", req)
	if !c.Secure {
		t.Error("expected Secure=true when X-Forwarded-Proto=https")
	}
}
