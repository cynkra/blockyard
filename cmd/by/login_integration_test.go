//go:build cli_test

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStdin runs the CLI with piped stdin and without default env vars
// (login writes its own config). xdgHome overrides XDG_CONFIG_HOME so
// each test can inspect the resulting config.json in isolation.
func runStdin(t *testing.T, xdgHome, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(byBin, args...)
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"XDG_CONFIG_HOME=" + xdgHome,
		"PATH=" + os.Getenv("PATH"),
	}
	if d := os.Getenv("GOCOVERDIR"); d != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+d)
	}
	cmd.Stdin = strings.NewReader(stdin)
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

// extractJSON returns the JSON object embedded in mixed output.
// login writes prompt text ("Paste your token:", "Opening browser...")
// to stdout before emitting the JSON blob, so r.jsonMap() on the raw
// buffer fails. The JSON object is the only '{' in the stream.
func extractJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	i := strings.Index(s, "{")
	if i < 0 {
		t.Fatalf("no JSON object in output:\n%s", s)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(s[i:]), &v); err != nil {
		t.Fatalf("unmarshal JSON tail: %v\n%s", err, s[i:])
	}
	return v
}

func readConfig(t *testing.T, xdgHome string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(xdgHome, "by", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal config: %v\n%s", err, data)
	}
	return v
}

func TestCLI_Login(t *testing.T) {
	t.Run("server_flag_and_token", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/users/me", 200, map[string]string{
			"sub": "demo@example.com", "name": "Demo User",
		})

		xdg := t.TempDir()
		r := runStdin(t, xdg, "tok-abc\n", "login", "--server", m.URL)
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Logged in to")
		assertContains(t, r.Stdout, "Demo User")

		req := m.reqTo("GET", "/api/v1/users/me")
		if req == nil {
			t.Fatal("no /users/me request")
			return
		}
		if req.Auth != "Bearer tok-abc" {
			t.Errorf("auth = %q, want Bearer tok-abc", req.Auth)
		}

		cfg := readConfig(t, xdg)
		if cfg["server"] != m.URL {
			t.Errorf("config server = %v, want %v", cfg["server"], m.URL)
		}
		if cfg["token"] != "tok-abc" {
			t.Errorf("config token = %v, want tok-abc", cfg["token"])
		}
	})

	t.Run("interactive_server_prompt", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/users/me", 200, map[string]string{
			"sub": "demo@example.com", "name": "Demo User",
		})

		// Both prompts via stdin: server URL (with trailing slash to
		// exercise the TrimRight), then token.
		xdg := t.TempDir()
		r := runStdin(t, xdg, m.URL+"/\ntok-xyz\n", "login")
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "Server URL:")
		assertContains(t, r.Stdout, "Paste your token:")

		cfg := readConfig(t, xdg)
		if cfg["server"] != m.URL {
			t.Errorf("config server = %v, want %v (trailing slash trimmed)", cfg["server"], m.URL)
		}
	})

	t.Run("falls_back_to_sub_when_name_empty", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/users/me", 200, map[string]string{
			"sub": "sub-only@example.com",
		})

		xdg := t.TempDir()
		r := runStdin(t, xdg, "tok\n", "login", "--server", m.URL)
		assertExit(t, r, 0)
		assertContains(t, r.Stdout, "sub-only@example.com")
	})

	t.Run("json_output", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/users/me", 200, map[string]string{
			"sub": "demo@example.com", "name": "Demo User",
		})

		xdg := t.TempDir()
		r := runStdin(t, xdg, "tok\n", "login", "--server", m.URL, "--json")
		assertExit(t, r, 0)
		j := extractJSON(t, r.Stdout)
		if j["user"] != "Demo User" {
			t.Errorf("json user = %v, want Demo User", j["user"])
		}
		if j["server"] != m.URL {
			t.Errorf("json server = %v, want %v", j["server"], m.URL)
		}
	})

	t.Run("empty_server_url", func(t *testing.T) {
		xdg := t.TempDir()
		// Empty line for server prompt -> error.
		r := runStdin(t, xdg, "\n", "login")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "server URL is required")
	})

	t.Run("empty_token", func(t *testing.T) {
		m := newMock(t)
		xdg := t.TempDir()
		r := runStdin(t, xdg, "\n", "login", "--server", m.URL)
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "token is required")
	})

	t.Run("auth_failure_text", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/users/me", 401, map[string]string{
			"error": "unauthorized", "message": "bad token",
		})

		xdg := t.TempDir()
		r := runStdin(t, xdg, "bad-tok\n", "login", "--server", m.URL)
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "authentication failed")
	})

	t.Run("auth_failure_json", func(t *testing.T) {
		m := newMock(t)
		m.on("GET", "/api/v1/users/me", 401, map[string]string{
			"error": "unauthorized", "message": "bad token",
		})

		xdg := t.TempDir()
		r := runStdin(t, xdg, "bad-tok\n", "login", "--server", m.URL, "--json")
		assertExit(t, r, 1)
		j := extractJSON(t, r.Stdout)
		if j["error"] != "error" {
			t.Errorf("json error = %v, want error", j["error"])
		}
	})

	t.Run("connection_failure", func(t *testing.T) {
		// Point at a closed port — apiclient.Get should fail.
		xdg := t.TempDir()
		r := runStdin(t, xdg, "tok\n", "login", "--server", "http://127.0.0.1:1")
		assertExit(t, r, 1)
		assertContains(t, r.Stderr, "failed to connect to server")
	})
}
