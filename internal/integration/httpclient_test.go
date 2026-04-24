package integration

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewHTTPClient_NoPath(t *testing.T) {
	c, err := NewHTTPClient("")
	if err != nil {
		t.Fatalf("NewHTTPClient(\"\"): %v", err)
	}
	if c.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", c.Timeout)
	}
	if c.Transport != nil {
		t.Errorf("Transport = %v, want nil (default)", c.Transport)
	}
}

func TestNewHTTPClient_ValidCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	caPath := writeCert(t, srv.Certificate())

	c, err := NewHTTPClient(caPath)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET with custom CA: %v", err)
	}
	resp.Body.Close()

	// Default client (system CAs only) must reject the self-signed cert.
	if _, err := DefaultHTTPClient().Get(srv.URL); err == nil {
		t.Error("default client unexpectedly trusted self-signed cert")
	}
}

func TestNewHTTPClient_MissingFile(t *testing.T) {
	_, err := NewHTTPClient("/does/not/exist.pem")
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("err = %v, want 'read' error", err)
	}
}

func TestNewHTTPClient_NotPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(path, []byte("not a pem file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewHTTPClient(path)
	if err == nil || !strings.Contains(err.Error(), "no valid PEM") {
		t.Fatalf("err = %v, want 'no valid PEM' error", err)
	}
}

func writeCert(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
		t.Fatal(err)
	}
	return path
}
