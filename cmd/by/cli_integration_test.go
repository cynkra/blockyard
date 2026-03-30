//go:build cli_test

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// byBin is the path to the compiled CLI binary, set once in TestMain.
var byBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "by-cli-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}

	byBin = filepath.Join(dir, "by")
	args := []string{"build"}
	if os.Getenv("GOCOVERDIR") != "" {
		args = append(args, "-cover", "-coverpkg=github.com/cynkra/blockyard/...")
	}
	args = append(args, "-o", byBin, ".")
	build := exec.Command("go", args...)
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ── Mock API server ─────────────────────────────────────────────────

type recorded struct {
	Method      string
	Path        string
	RawQuery    string
	ContentType string
	Body        []byte
	Auth        string
}

type mockAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	routes   map[string]http.HandlerFunc
}

func newMock(t *testing.T) *mockAPI {
	t.Helper()
	m := &mockAPI{routes: make(map[string]http.HandlerFunc)}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.requests = append(m.requests, recorded{
			Method:      r.Method,
			Path:        r.URL.Path,
			RawQuery:    r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
			Auth:        r.Header.Get("Authorization"),
		})
		m.mu.Unlock()

		key := r.Method + " " + r.URL.Path
		if h, ok := m.routes[key]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not_found", "message": "no mock route"}) //nolint:errcheck
	}))
	t.Cleanup(m.Close)
	return m
}

// on registers a JSON response for method+path.
func (m *mockAPI) on(method, path string, status int, body any) {
	m.routes[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}
}

// onEmpty registers a response with no body.
func (m *mockAPI) onEmpty(method, path string, status int) {
	m.routes[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}
}

// onText registers a plain-text response (used for log streaming).
func (m *mockAPI) onText(method, path string, text string) {
	m.routes[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, text) //nolint:errcheck
	}
}

// reqs returns a snapshot of all recorded requests.
func (m *mockAPI) reqs() []recorded {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recorded(nil), m.requests...)
}

// reqTo returns the first recorded request matching method and path.
func (m *mockAPI) reqTo(method, path string) *recorded {
	reqs := m.reqs()
	for i := range reqs {
		if reqs[i].Method == method && reqs[i].Path == path {
			return &reqs[i]
		}
	}
	return nil
}

// ── CLI runner ──────────────────────────────────────────────────────

type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r result) jsonMap() map[string]any {
	var v map[string]any
	_ = json.Unmarshal([]byte(r.Stdout), &v)
	return v
}

func run(t *testing.T, mock *mockAPI, args ...string) result {
	t.Helper()
	cmd := exec.Command(byBin, args...)
	cmd.Env = append(os.Environ(),
		"BLOCKYARD_URL="+mock.URL,
		"BLOCKYARD_TOKEN=test-tok-123",
		"XDG_CONFIG_HOME="+t.TempDir(),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// runNoEnv runs the CLI without BLOCKYARD_URL/TOKEN set.
func runNoEnv(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(byBin, args...)
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"XDG_CONFIG_HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
	}
	if d := os.Getenv("GOCOVERDIR"); d != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+d)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec: %v", err)
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// ── Assertion helpers ───────────────────────────────────────────────

func assertExit(t *testing.T, r result, want int) {
	t.Helper()
	if r.ExitCode != want {
		t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s",
			r.ExitCode, want, r.Stdout, r.Stderr)
	}
}

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("output missing %q in:\n%s", substr, s)
	}
}

func bodyJSON(t *testing.T, req *recorded) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(req.Body, &v); err != nil {
		t.Fatalf("unmarshal request body: %v\nbody: %s", err, req.Body)
	}
	return v
}

// ── Tests ───────────────────────────────────────────────────────────

// TestCLI_AuthHeader verifies that every request carries the Bearer token.
func TestCLI_AuthHeader(t *testing.T) {
	m := newMock(t)
	m.on("GET", "/api/v1/apps", 200, map[string]any{"apps": []any{}, "total": 0})

	r := run(t, m, "list")
	assertExit(t, r, 0)

	req := m.reqTo("GET", "/api/v1/apps")
	if req == nil {
		t.Fatal("no request recorded")
	}
	if req.Auth != "Bearer test-tok-123" {
		t.Errorf("auth = %q, want %q", req.Auth, "Bearer test-tok-123")
	}
}

// TestCLI_NoCredentials verifies failure when env vars are missing.
func TestCLI_NoCredentials(t *testing.T) {
	r := runNoEnv(t, "list")
	assertExit(t, r, 1)
	assertContains(t, r.Stderr, "no server configured")
}

// TestCLI_APIError verifies that HTTP error responses are reported.
func TestCLI_APIError(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps", 403, map[string]string{
			"error": "forbidden", "message": "not allowed",
		})
		r := run(t, m, "list")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "not allowed")
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps", 403, map[string]string{
			"error": "forbidden", "message": "not allowed",
		})
		r := run(t, m, "list", "--json")
		assertExit(t, r, 1)
		j := r.jsonMap()
		if j["message"] != "not allowed" {
			t.Errorf("json message = %v, want %q", j["message"], "not allowed")
		}
	})
}

// ── App listing ─────────────────────────────────────────────────────

func TestCLI_List(t *testing.T) {
	apps := map[string]any{
		"apps": []map[string]any{
			{"name": "app1", "title": nil, "owner": "alice", "status": "running", "enabled": true},
			{"name": "app2", "title": "My App", "owner": "bob", "status": "stopped", "enabled": false},
		},
		"total": 2,
	}

	t.Run("text", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps", 200, apps)
		r := run(t, m, "list")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "app1")
		assertContains(t, r.Stdout, "app2")
		assertContains(t, r.Stdout, "alice")
		assertContains(t, r.Stdout, "running")
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps", 200, apps)
		r := run(t, m, "list", "--json")
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["total"].(float64) != 2 {
			t.Errorf("total = %v, want 2", j["total"])
		}
	})

	t.Run("empty", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps", 200, map[string]any{"apps": []any{}, "total": 0})
		r := run(t, m, "list")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "No apps found")
	})

	t.Run("deleted_flag", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps", 200, map[string]any{"apps": []any{}, "total": 0})
		r := run(t, m, "list", "--deleted")
		assertExit(t, r, 0)
		req := m.reqTo("GET", "/api/v1/apps")
		if !strings.Contains(req.RawQuery, "deleted=true") {
			t.Errorf("query = %q, want deleted=true", req.RawQuery)
		}
	})

	t.Run("alias_ls", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps", 200, map[string]any{"apps": []any{}, "total": 0})
		r := run(t, m, "ls")
		assertExit(t, r, 0)
	})
}

// ── App details ─────────────────────────────────────────────────────

func TestCLI_Get(t *testing.T) {
	app := map[string]any{
		"id": "app-123", "name": "myapp", "owner": "alice",
		"access_type": "public", "active_bundle": "bun-1",
		"memory_limit": "2g", "cpu_limit": 2.0,
		"title": "My App", "description": "A test app",
		"enabled": true, "status": "running",
		"tags": []string{"prod"}, "created_at": "2025-01-01T00:00:00Z",
	}

	t.Run("text", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp", 200, app)
		r := run(t, m, "get", "myapp")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "myapp")
		assertContains(t, r.Stdout, "alice")
		assertContains(t, r.Stdout, "running")
		assertContains(t, r.Stdout, "public")
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp", 200, app)
		r := run(t, m, "get", "myapp", "--json")
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["name"] != "myapp" {
			t.Errorf("name = %v, want myapp", j["name"])
		}
	})

	t.Run("runtime", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp", 200, app)
		m.on("GET", "/api/v1/apps/myapp/runtime", 200, map[string]any{
			"workers": []any{}, "active_sessions": 5,
			"total_views": 100, "recent_views": 42,
			"unique_visitors": 20, "last_deployed_at": "2025-01-01T12:00:00Z",
		})
		r := run(t, m, "get", "myapp", "--runtime")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Runtime:")
		assertContains(t, r.Stdout, "5") // active sessions
	})
}

// ── Simple app actions (enable, disable, restore) ───────────────────

func TestCLI_AppActions(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		method string
		path   string
		output string
	}{
		{"enable", []string{"enable", "myapp"}, "POST", "/api/v1/apps/myapp/enable", "Enabled myapp"},
		{"disable", []string{"disable", "myapp"}, "POST", "/api/v1/apps/myapp/disable", "Disabled myapp"},
		{"restore", []string{"restore", "myapp"}, "POST", "/api/v1/apps/myapp/restore", "Restored myapp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMock(t)
			m.on(tt.method, tt.path, 200, map[string]string{"status": "ok"})
			r := run(t, m, tt.args...)
			assertExit(t, r, 0)
			assertContains(t, r.Stdout, tt.output)
			if req := m.reqTo(tt.method, tt.path); req == nil {
				t.Fatalf("no %s %s request", tt.method, tt.path)
			}
		})
	}
}

// ── Delete ──────────────────────────────────────────────────────────

func TestCLI_Delete(t *testing.T) {
	t.Run("soft", func(t *testing.T) {
		m := newMock(t)
		m.onEmpty("DELETE", "/api/v1/apps/myapp", 204)
		r := run(t, m, "delete", "myapp")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Deleted myapp")
	})

	t.Run("purge", func(t *testing.T) {
		m := newMock(t)
		m.onEmpty("DELETE", "/api/v1/apps/myapp", 204)
		r := run(t, m, "delete", "myapp", "--purge")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Purged myapp")
		req := m.reqTo("DELETE", "/api/v1/apps/myapp")
		if !strings.Contains(req.RawQuery, "purge=true") {
			t.Errorf("query = %q, want purge=true", req.RawQuery)
		}
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.onEmpty("DELETE", "/api/v1/apps/myapp", 204)
		r := run(t, m, "delete", "myapp", "--json")
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["status"] != "deleted" {
			t.Errorf("status = %v, want deleted", j["status"])
		}
	})

	t.Run("alias_rm", func(t *testing.T) {
		m := newMock(t)
		m.onEmpty("DELETE", "/api/v1/apps/myapp", 204)
		r := run(t, m, "rm", "myapp")
		assertExit(t, r, 0)
	})
}

// ── Bundles & rollback ──────────────────────────────────────────────

func TestCLI_Bundles(t *testing.T) {
	bundles := map[string]any{
		"bundles": []map[string]any{
			{"id": "bun-1", "status": "ready", "uploaded_at": "2025-01-01", "deployed_by": "alice", "pinned": true},
			{"id": "bun-2", "status": "building", "uploaded_at": "2025-01-02", "deployed_by": nil, "pinned": false},
		},
	}

	t.Run("text", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp/bundles", 200, bundles)
		r := run(t, m, "bundles", "myapp")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "bun-1")
		assertContains(t, r.Stdout, "ready")
		assertContains(t, r.Stdout, "bun-2")
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp/bundles", 200, bundles)
		r := run(t, m, "bundles", "myapp", "--json")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "bun-1")
	})
}

func TestCLI_Rollback(t *testing.T) {
	m := newMock(t)
	m.on("POST", "/api/v1/apps/myapp/rollback", 200, map[string]string{"status": "ok"})
	r := run(t, m, "rollback", "myapp", "bun-old")
	assertExit(t, r, 0)
	assertContains(t, r.Stdout, "Rolled back myapp to bundle bun-old")

	req := m.reqTo("POST", "/api/v1/apps/myapp/rollback")
	if req == nil {
		t.Fatal("no rollback request")
	}
	body := bodyJSON(t, req)
	if body["bundle_id"] != "bun-old" {
		t.Errorf("bundle_id = %v, want bun-old", body["bundle_id"])
	}
}

// ── Scale ───────────────────────────────────────────────────────────

func TestCLI_Scale(t *testing.T) {
	t.Run("memory_and_cpu", func(t *testing.T) {
		m := newMock(t)
		m.on("PATCH", "/api/v1/apps/myapp", 200, map[string]string{"status": "ok"})
		r := run(t, m, "scale", "myapp", "--memory", "4g", "--cpu", "2.5")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Updated scaling for myapp")

		body := bodyJSON(t, m.reqTo("PATCH", "/api/v1/apps/myapp"))
		if body["memory_limit"] != "4g" {
			t.Errorf("memory_limit = %v, want 4g", body["memory_limit"])
		}
		if body["cpu_limit"] != 2.5 {
			t.Errorf("cpu_limit = %v, want 2.5", body["cpu_limit"])
		}
	})

	t.Run("workers_and_sessions", func(t *testing.T) {
		m := newMock(t)
		m.on("PATCH", "/api/v1/apps/myapp", 200, map[string]string{"status": "ok"})
		r := run(t, m, "scale", "myapp", "--max-workers", "5", "--max-sessions", "10", "--pre-warm", "2")
		assertExit(t, r, 0)

		body := bodyJSON(t, m.reqTo("PATCH", "/api/v1/apps/myapp"))
		if body["max_workers_per_app"] != float64(5) {
			t.Errorf("max_workers = %v, want 5", body["max_workers_per_app"])
		}
		if body["max_sessions_per_worker"] != float64(10) {
			t.Errorf("max_sessions = %v, want 10", body["max_sessions_per_worker"])
		}
		if body["pre_warmed_seats"] != float64(2) {
			t.Errorf("pre_warm = %v, want 2", body["pre_warmed_seats"])
		}
	})

	t.Run("no_flags_error", func(t *testing.T) {
		m := newMock(t)
		r := run(t, m, "scale", "myapp")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "no flags specified")
	})
}

// ── Update ──────────────────────────────────────────────────────────

func TestCLI_Update(t *testing.T) {
	t.Run("title_and_description", func(t *testing.T) {
		m := newMock(t)
		m.on("PATCH", "/api/v1/apps/myapp", 200, map[string]string{"status": "ok"})
		r := run(t, m, "update", "myapp", "--title", "New Title", "--description", "New Desc")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Updated myapp")

		body := bodyJSON(t, m.reqTo("PATCH", "/api/v1/apps/myapp"))
		if body["title"] != "New Title" {
			t.Errorf("title = %v, want New Title", body["title"])
		}
		if body["description"] != "New Desc" {
			t.Errorf("description = %v, want New Desc", body["description"])
		}
	})

	t.Run("no_flags_error", func(t *testing.T) {
		m := newMock(t)
		r := run(t, m, "update", "myapp")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "no flags specified")
	})
}

// ── Access control ──────────────────────────────────────────────────

func TestCLI_Access(t *testing.T) {
	t.Run("show", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp", 200, map[string]any{"access_type": "acl"})
		m.on("GET", "/api/v1/apps/myapp/access", 200, []map[string]string{
			{"principal": "bob", "kind": "user", "role": "viewer", "granted_by": "alice"},
		})
		r := run(t, m, "access", "show", "myapp")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "acl")
		assertContains(t, r.Stdout, "bob")
		assertContains(t, r.Stdout, "viewer")
	})

	t.Run("set_type", func(t *testing.T) {
		m := newMock(t)
		m.on("PATCH", "/api/v1/apps/myapp", 200, map[string]string{"status": "ok"})
		r := run(t, m, "access", "set-type", "myapp", "public")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Set access type for myapp to public")

		body := bodyJSON(t, m.reqTo("PATCH", "/api/v1/apps/myapp"))
		if body["access_type"] != "public" {
			t.Errorf("access_type = %v, want public", body["access_type"])
		}
	})

	t.Run("set_type_invalid", func(t *testing.T) {
		m := newMock(t)
		r := run(t, m, "access", "set-type", "myapp", "invalid")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "invalid access type")
	})

	t.Run("grant", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/apps/myapp/access", 200, map[string]string{"status": "granted"})
		r := run(t, m, "access", "grant", "myapp", "bob", "--role", "collaborator")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Granted bob access to myapp as collaborator")

		body := bodyJSON(t, m.reqTo("POST", "/api/v1/apps/myapp/access"))
		if body["principal"] != "bob" {
			t.Errorf("principal = %v, want bob", body["principal"])
		}
		if body["role"] != "collaborator" {
			t.Errorf("role = %v, want collaborator", body["role"])
		}
		if body["kind"] != "user" {
			t.Errorf("kind = %v, want user", body["kind"])
		}
	})

	t.Run("grant_default_role", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/apps/myapp/access", 200, map[string]string{"status": "granted"})
		run(t, m, "access", "grant", "myapp", "bob")

		body := bodyJSON(t, m.reqTo("POST", "/api/v1/apps/myapp/access"))
		if body["role"] != "viewer" {
			t.Errorf("default role = %v, want viewer", body["role"])
		}
	})

	t.Run("revoke", func(t *testing.T) {
		m := newMock(t)
		m.onEmpty("DELETE", "/api/v1/apps/myapp/access/user/bob", 204)
		r := run(t, m, "access", "revoke", "myapp", "bob")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Revoked access for bob on myapp")
	})
}

// ── Tags ────────────────────────────────────────────────────────────

func TestCLI_Tags(t *testing.T) {
	t.Run("list_global", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/tags", 200, map[string]any{
			"tags": []map[string]string{
				{"id": "t1", "name": "production"},
				{"id": "t2", "name": "staging"},
			},
		})
		r := run(t, m, "tags", "list")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "production")
		assertContains(t, r.Stdout, "staging")
	})

	t.Run("create", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/tags", 200, map[string]string{"id": "t3", "name": "test"})
		r := run(t, m, "tags", "create", "test")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Created tag test")

		body := bodyJSON(t, m.reqTo("POST", "/api/v1/tags"))
		if body["name"] != "test" {
			t.Errorf("name = %v, want test", body["name"])
		}
	})

	t.Run("delete", func(t *testing.T) {
		m := newMock(t)
		m.onEmpty("DELETE", "/api/v1/tags/t1", 204)
		r := run(t, m, "tags", "delete", "t1")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Deleted tag t1")
	})

	t.Run("app_list", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp/tags", 200, map[string]any{
			"tags": []map[string]string{{"id": "t1", "name": "production"}},
		})
		r := run(t, m, "tags", "app-list", "myapp")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "production")
	})

	t.Run("app_add", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/apps/myapp/tags", 200, map[string]string{"status": "added"})
		r := run(t, m, "tags", "app-add", "myapp", "t1")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Added tag t1 to myapp")
	})

	t.Run("app_remove", func(t *testing.T) {
		m := newMock(t)
		m.onEmpty("DELETE", "/api/v1/apps/myapp/tags/t1", 204)
		r := run(t, m, "tags", "app-remove", "myapp", "t1")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Removed tag t1 from myapp")
	})
}

// ── Deploy ──────────────────────────────────────────────────────────

func TestCLI_Deploy(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/testapp", 200, map[string]any{"id": "app-123"})
		m.on("POST", "/api/v1/apps/app-123/bundles", 202, map[string]string{
			"bundle_id": "bun-1", "task_id": "task-1",
		})

		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("shinyApp(ui, server)"), 0o644)

		r := run(t, m, "deploy", "--name", "testapp", "--yes", dir)
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Uploading bundle")
		assertContains(t, r.Stdout, "done")
		assertContains(t, r.Stdout, "bun-1")

		req := m.reqTo("POST", "/api/v1/apps/app-123/bundles")
		if req == nil {
			t.Fatal("no upload request")
		}
		if req.ContentType != "application/gzip" {
			t.Errorf("content-type = %q, want application/gzip", req.ContentType)
		}
		if len(req.Body) == 0 {
			t.Error("upload body is empty")
		}
	})

	t.Run("wait_success", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/testapp", 200, map[string]any{"id": "app-123"})
		m.on("POST", "/api/v1/apps/app-123/bundles", 202, map[string]string{
			"bundle_id": "bun-1", "task_id": "task-1",
		})
		m.onText("GET", "/api/v1/tasks/task-1/logs", "Installing packages...\nBuild complete.\n")
		m.on("GET", "/api/v1/tasks/task-1", 200, map[string]string{"status": "completed"})

		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("shinyApp(ui, server)"), 0o644)

		r := run(t, m, "deploy", "--name", "testapp", "--yes", "--wait", dir)
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Building")
		assertContains(t, r.Stdout, "Installing packages")
		assertContains(t, r.Stdout, "Deployed testapp")
	})

	t.Run("wait_failed", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/testapp", 200, map[string]any{"id": "app-123"})
		m.on("POST", "/api/v1/apps/app-123/bundles", 202, map[string]string{
			"bundle_id": "bun-1", "task_id": "task-1",
		})
		m.onText("GET", "/api/v1/tasks/task-1/logs", "Error: compilation failed\n")
		m.on("GET", "/api/v1/tasks/task-1", 200, map[string]string{"status": "failed"})

		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("shinyApp(ui, server)"), 0o644)

		r := run(t, m, "deploy", "--name", "testapp", "--yes", "--wait", dir)
		assertExit(t, r, 1)
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/testapp", 200, map[string]any{"id": "app-123"})
		m.on("POST", "/api/v1/apps/app-123/bundles", 202, map[string]string{
			"bundle_id": "bun-1", "task_id": "task-1",
		})

		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("shinyApp(ui, server)"), 0o644)

		r := run(t, m, "deploy", "--name", "testapp", "--yes", "--json", dir)
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["bundle_id"] != "bun-1" {
			t.Errorf("bundle_id = %v, want bun-1", j["bundle_id"])
		}
		if j["status"] != "building" {
			t.Errorf("status = %v, want building", j["status"])
		}
	})

	t.Run("creates_app_if_not_found", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/newapp", 404, map[string]string{"error": "not_found"})
		m.on("POST", "/api/v1/apps", 201, map[string]any{"id": "new-123"})
		m.on("POST", "/api/v1/apps/new-123/bundles", 202, map[string]string{
			"bundle_id": "bun-1", "task_id": "task-1",
		})

		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("shinyApp(ui, server)"), 0o644)

		r := run(t, m, "deploy", "--name", "newapp", "--yes", dir)
		assertExit(t, r, 0)

		req := m.reqTo("POST", "/api/v1/apps")
		if req == nil {
			t.Fatal("no create-app request")
		}
		body := bodyJSON(t, req)
		if body["name"] != "newapp" {
			t.Errorf("app name = %v, want newapp", body["name"])
		}
	})
}

// ── Users ───────────────────────────────────────────────────────────

func TestCLI_Users(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/users", 200, []map[string]any{
			{"sub": "u1", "name": "Alice", "email": "alice@example.com", "role": "admin", "active": true},
			{"sub": "u2", "name": "Bob", "email": "bob@example.com", "role": "publisher", "active": false},
		})
		r := run(t, m, "users", "list")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Alice")
		assertContains(t, r.Stdout, "Bob")
		assertContains(t, r.Stdout, "admin")
	})

	t.Run("update_role", func(t *testing.T) {
		m := newMock(t)
		m.on("PATCH", "/api/v1/users/u1", 200, map[string]string{"status": "ok"})
		r := run(t, m, "users", "update", "u1", "--role", "publisher")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Updated user u1")

		body := bodyJSON(t, m.reqTo("PATCH", "/api/v1/users/u1"))
		if body["role"] != "publisher" {
			t.Errorf("role = %v, want publisher", body["role"])
		}
	})

	t.Run("update_active", func(t *testing.T) {
		m := newMock(t)
		m.on("PATCH", "/api/v1/users/u1", 200, map[string]string{"status": "ok"})
		r := run(t, m, "users", "update", "u1", "--active=false")
		assertExit(t, r, 0)

		body := bodyJSON(t, m.reqTo("PATCH", "/api/v1/users/u1"))
		if body["active"] != false {
			t.Errorf("active = %v, want false", body["active"])
		}
	})

	t.Run("no_flags_error", func(t *testing.T) {
		m := newMock(t)
		r := run(t, m, "users", "update", "u1")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "no flags specified")
	})
}

// ── Logs ────────────────────────────────────────────────────────────

func TestCLI_Logs(t *testing.T) {
	t.Run("with_worker", func(t *testing.T) {
		m := newMock(t)
		m.onText("GET", "/api/v1/apps/myapp/logs", "line1\nline2\nline3\n")
		r := run(t, m, "logs", "myapp", "--worker", "w-1")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "line1")
		assertContains(t, r.Stdout, "line3")

		req := m.reqTo("GET", "/api/v1/apps/myapp/logs")
		if !strings.Contains(req.RawQuery, "worker_id=w-1") {
			t.Errorf("query = %q, want worker_id=w-1", req.RawQuery)
		}
		if !strings.Contains(req.RawQuery, "stream=false") {
			t.Errorf("query = %q, want stream=false (no --follow)", req.RawQuery)
		}
	})

	t.Run("auto_select_worker", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/apps/myapp/runtime", 200, map[string]any{
			"workers": []map[string]any{
				{"id": "w-auto", "status": "active", "started_at": "2025-01-01T00:00:00Z"},
			},
		})
		m.onText("GET", "/api/v1/apps/myapp/logs", "auto-log\n")
		r := run(t, m, "logs", "myapp")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "auto-log")

		req := m.reqTo("GET", "/api/v1/apps/myapp/logs")
		if !strings.Contains(req.RawQuery, "worker_id=w-auto") {
			t.Errorf("query = %q, want worker_id=w-auto", req.RawQuery)
		}
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.onText("GET", "/api/v1/apps/myapp/logs", "json-log\n")
		r := run(t, m, "logs", "myapp", "--worker", "w-1", "--json")
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["worker"] != "w-1" {
			t.Errorf("worker = %v, want w-1", j["worker"])
		}
		logs, _ := j["logs"].(string)
		assertContains(t, logs, "json-log")
	})
}

// ── Refresh ─────────────────────────────────────────────────────────

func TestCLI_Refresh(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/apps/myapp/refresh", 200, map[string]string{
			"task_id": "task-r1", "message": "refreshing",
		})
		m.onText("GET", "/api/v1/tasks/task-r1/logs", "Refreshing deps...\nDone.\n")
		m.on("GET", "/api/v1/tasks/task-r1", 200, map[string]string{"status": "completed"})

		r := run(t, m, "refresh", "myapp")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Refreshing dependencies for myapp")
		assertContains(t, r.Stdout, "Refreshing deps")
	})

	t.Run("rollback", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/apps/myapp/refresh/rollback", 200, map[string]string{
			"task_id": "task-r2", "message": "rolling back",
		})
		m.onText("GET", "/api/v1/tasks/task-r2/logs", "Rolling back...\n")
		m.on("GET", "/api/v1/tasks/task-r2", 200, map[string]string{"status": "completed"})

		r := run(t, m, "refresh", "myapp", "--rollback")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Rolling back dependencies for myapp")

		if req := m.reqTo("POST", "/api/v1/apps/myapp/refresh/rollback"); req == nil {
			t.Fatal("no rollback request")
		}
	})

	t.Run("json", func(t *testing.T) {
		m := newMock(t)
		m.on("POST", "/api/v1/apps/myapp/refresh", 200, map[string]string{
			"task_id": "task-r3", "message": "refreshing",
		})
		m.onText("GET", "/api/v1/tasks/task-r3/logs", "log line\n")
		m.on("GET", "/api/v1/tasks/task-r3", 200, map[string]string{"status": "completed"})

		r := run(t, m, "refresh", "myapp", "--json")
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["status"] != "completed" {
			t.Errorf("status = %v, want completed", j["status"])
		}
		if j["task_id"] != "task-r3" {
			t.Errorf("task_id = %v, want task-r3", j["task_id"])
		}
	})
}

// ── Init command ───────────────────────────────────────────────────

func TestInit(t *testing.T) {
	t.Run("new manifest from DESCRIPTION", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte(
			"Package: myapp\nTitle: Test\nType: ShinyApp\nDepends: shiny\n"), 0o644)

		r := runNoEnv(t, "init", dir)
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Wrote manifest.json")

		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
			t.Error("manifest.json not created")
		}
	})

	t.Run("existing manifest", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(
			`{"version":1,"metadata":{"appmode":"shiny","entrypoint":"app.R"},"files":{}}`+"\n"), 0o644)

		r := runNoEnv(t, "init", dir)
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "already exists")
	})

	t.Run("nonexistent directory", func(t *testing.T) {
		r := runNoEnv(t, "init", "/no/such/path")
		if r.ExitCode == 0 {
			t.Error("expected non-zero exit for nonexistent dir")
		}
	})

	t.Run("json output", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "DESCRIPTION"), []byte(
			"Package: myapp\nTitle: Test\nType: ShinyApp\nDepends: shiny\n"), 0o644)

		r := runNoEnv(t, "init", dir, "--json")
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["status"] != "created" {
			t.Errorf("status = %v, want created", j["status"])
		}
	})

	t.Run("json output existing", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(
			`{"version":1,"metadata":{"appmode":"shiny","entrypoint":"app.R"},"files":{}}`+"\n"), 0o644)

		r := runNoEnv(t, "init", dir, "--json")
		assertExit(t, r, 0)
		j := r.jsonMap()
		if j["status"] != "exists" {
			t.Errorf("status = %v, want exists", j["status"])
		}
	})

	t.Run("bare scripts error", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.R"), []byte("library(shiny)\n"), 0o644)

		r := runNoEnv(t, "init", dir)
		if r.ExitCode == 0 {
			t.Error("expected non-zero exit for bare scripts")
		}
		assertContains(t, r.Stderr, "cannot generate manifest from bare scripts")
	})
}
