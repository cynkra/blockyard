package apiclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_Get(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %s", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/apps" {
			t.Errorf("expected /api/v1/apps, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	resp, err := c.Get("/api/v1/apps")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestClient_PostJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "myapp" {
			t.Errorf("expected name=myapp, got %s", body["name"])
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	resp, err := c.PostJSON("/api/v1/apps", map[string]string{"name": "myapp"})
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
}

func TestCheckResponse_Success(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Body:       http.NoBody,
	}
	if err := CheckResponse(resp); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestCheckResponse_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   "not_found",
			"message": "app not found",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	resp, err := c.Get("/api/v1/apps/missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	err = CheckResponse(resp)
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Status != 404 {
		t.Errorf("expected status 404, got %d", apiErr.Status)
	}
	if apiErr.Message != "app not found" {
		t.Errorf("expected message 'app not found', got %q", apiErr.Message)
	}
}

func TestClient_PatchJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["role"] != "admin" {
			t.Errorf("expected role=admin, got %s", body["role"])
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	resp, err := c.PatchJSON("/api/v1/users/u1", map[string]string{"role": "admin"})
	if err != nil {
		t.Fatalf("patchJSON: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestClient_Delete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %s", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	resp, err := c.Delete("/api/v1/apps/myapp")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204, got %d", resp.StatusCode)
	}
}

func TestClient_Post(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/gzip" {
			t.Errorf("expected application/gzip, got %s", r.Header.Get("Content-Type"))
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != "binary-data" {
			t.Errorf("unexpected body: %q", data)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"bundle_id": "b1"})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	resp, err := c.Post("/api/v1/apps/a1/bundles", strings.NewReader("binary-data"), "application/gzip")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
}

func TestClient_DoWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("expected no Authorization header, got %s", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	resp, err := c.Get("/api/v1/health")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestNewStreaming(t *testing.T) {
	c := NewStreaming("https://example.com/", "tok")
	if c.BaseURL != "https://example.com" {
		t.Errorf("expected trailing slash stripped, got %q", c.BaseURL)
	}
	if c.HTTPClient.Timeout != 0 {
		t.Errorf("expected zero timeout for streaming client, got %v", c.HTTPClient.Timeout)
	}
}

func TestAPIError_Error(t *testing.T) {
	// With message.
	e := &APIError{Status: 404, Code: "not_found", Message: "app not found"}
	if got := e.Error(); got != "app not found" {
		t.Errorf("with message: got %q", got)
	}
	// Without message — falls back to status+code.
	e2 := &APIError{Status: 500, Code: "internal"}
	if got := e2.Error(); got != "HTTP 500: internal" {
		t.Errorf("without message: got %q", got)
	}
}

func TestDecodeJSON_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"name": "myapp"})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	resp, err := c.Get("/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var result map[string]string
	if err := DecodeJSON(resp, &result); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if result["name"] != "myapp" {
		t.Errorf("expected name=myapp, got %q", result["name"])
	}
}

func TestDecodeJSON_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "forbidden", "message": "access denied"})
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	resp, err := c.Get("/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var result map[string]string
	err = DecodeJSON(resp, &result)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Status != 403 {
		t.Errorf("expected status 403, got %d", apiErr.Status)
	}
}

func TestReadBodyRaw_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("raw content here"))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	resp, err := c.Get("/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	data, err := ReadBodyRaw(resp)
	if err != nil {
		t.Fatalf("ReadBodyRaw: %v", err)
	}
	if string(data) != "raw content here" {
		t.Errorf("got %q", string(data))
	}
}

func TestReadBodyRaw_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"server_error","message":"boom"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	resp, err := c.Get("/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	_, err = ReadBodyRaw(resp)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestCheckResponse_ErrorWithoutMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("plain text error"))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	resp, err := c.Get("/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	err = CheckResponse(resp)
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr := err.(*APIError)
	// When body is not valid JSON, message should be the raw body text.
	if apiErr.Message != "plain text error" {
		t.Errorf("expected raw body as message, got %q", apiErr.Message)
	}
}

func TestBuildQuery(t *testing.T) {
	got := BuildQuery("/api/v1/apps", map[string]string{
		"search": "hello",
		"empty":  "",
	})
	if got != "/api/v1/apps?search=hello" {
		t.Errorf("got %q", got)
	}

	got = BuildQuery("/api/v1/apps", map[string]string{})
	if got != "/api/v1/apps" {
		t.Errorf("empty params: got %q", got)
	}
}
