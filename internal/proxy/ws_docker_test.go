//go:build docker_test

package proxy_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cynkra/blockyard/internal/api"
	"github.com/cynkra/blockyard/internal/backend"
	dockerbe "github.com/cynkra/blockyard/internal/backend/docker"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/testutil"
)

// wsEchoServerSource is a minimal Go WebSocket echo server.
// It accepts WS connections on the port specified by $SHINY_PORT (default 8080),
// reads messages, and echoes them back. A special "close-me" text message
// causes the server to close the connection (used by the backend-close test).
const wsEchoServerSource = `package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/coder/websocket"
)

func main() {
	port := os.Getenv("SHINY_PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(200)
			w.Write([]byte("ok"))
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageText && string(data) == "close-me" {
				c.Close(websocket.StatusNormalClosure, "server closing")
				return
			}
			c.Write(context.Background(), typ, data)
		}
	})
	fmt.Fprintf(os.Stderr, "ws-echo listening on :%s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}
`

// sharedWSBinary caches the compiled ws-echo binary across all tests in
// the package so we only pay the go-mod-tidy + go-build cost once.
var sharedWSBinary struct {
	once sync.Once
	path string
	err  error
}

// buildWSEchoBinary compiles the WS echo server as a static Linux binary
// and returns its path. The binary is compiled once and shared across tests.
func buildWSEchoBinary(t *testing.T) string {
	t.Helper()

	sharedWSBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "wsecho-build-*")
		if err != nil {
			sharedWSBinary.err = err
			return
		}

		// Write the source file and go.mod.
		srcDir := filepath.Join(dir, "wsecho")
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			sharedWSBinary.err = err
			return
		}
		if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(wsEchoServerSource), 0o644); err != nil {
			sharedWSBinary.err = err
			return
		}
		goMod := "module wsecho\n\ngo 1.24.1\n\nrequire github.com/coder/websocket v1.8.14\n"
		if err := os.WriteFile(filepath.Join(srcDir, "go.mod"), []byte(goMod), 0o644); err != nil {
			sharedWSBinary.err = err
			return
		}
		if err := os.WriteFile(filepath.Join(srcDir, "go.sum"), []byte(""), 0o644); err != nil {
			sharedWSBinary.err = err
			return
		}

		// Run go mod tidy to populate go.sum.
		tidy := exec.Command("go", "mod", "tidy")
		tidy.Dir = srcDir
		tidy.Env = append(os.Environ(), "GOFLAGS=")
		if out, err := tidy.CombinedOutput(); err != nil {
			sharedWSBinary.err = fmt.Errorf("go mod tidy failed: %v\n%s", err, out)
			return
		}

		// Cross-compile as a static binary for linux.
		binPath := filepath.Join(dir, "wsecho-bin")
		build := exec.Command("go", "build", "-o", binPath, ".")
		build.Dir = srcDir
		build.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS=linux",
			"GOARCH=amd64",
			"GOFLAGS=",
		)
		if out, err := build.CombinedOutput(); err != nil {
			sharedWSBinary.err = fmt.Errorf("go build ws-echo failed: %v\n%s", err, out)
			return
		}

		sharedWSBinary.path = binPath
	})

	if sharedWSBinary.err != nil {
		t.Fatal(sharedWSBinary.err)
	}
	return sharedWSBinary.path
}

// dockerTestConfig returns a DockerConfig suitable for integration tests.
func dockerTestConfig(t *testing.T) *config.DockerConfig {
	t.Helper()
	return &config.DockerConfig{
		Socket:    "/var/run/docker.sock",
		Image:     testutil.AlpineImage(t),
		ShinyPort: 8080,
		PakVersion: "stable",
	}
}

// dockerWSTestSetup creates a full proxy server backed by a real Docker
// container running the WS echo server. It returns the server, the
// httptest server, and a cleanup function that stops the container.
func dockerWSTestSetup(t *testing.T, wsBinary string) (*server.Server, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	// Create DockerBackend.
	be, err := dockerbe.New(ctx, dockerTestConfig(t), tmp)
	if err != nil {
		t.Fatalf("docker backend: %v", err)
	}

	// Create the server with the Docker backend.
	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{
			Image:     testutil.AlpineImage(t),
			ShinyPort: 8080,
		},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			WsCacheTTL:         config.Duration{Duration: 5 * time.Second},
			WorkerStartTimeout: config.Duration{Duration: 30 * time.Second},
			MaxWorkers:         10,
		},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	srv := server.NewServer(cfg, be, database)

	// Create an app in the DB with an active bundle so the proxy
	// recognizes it without going through the full upload flow.
	app, err := database.CreateApp("ws-docker-app", "")
	if err != nil {
		t.Fatal(err)
	}
	bundleID := uuid.New().String()[:8]
	if _, err := database.CreateBundle(bundleID, app.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateBundleStatus(bundleID, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := database.SetActiveBundle(app.ID, bundleID); err != nil {
		t.Fatal(err)
	}

	// Spawn a real Docker container running the WS echo server.
	workerID := "wstest-" + uuid.New().String()[:8]
	spec := backend.WorkerSpec{
		AppID:       app.ID,
		WorkerID:    workerID,
		Image:       testutil.AlpineImage(t),
		Cmd:         []string{"/wsecho-bin"},
		BundlePath:  tmp,
		LibraryPath: "",
		WorkerMount: "/app",
		ShinyPort:   8080,
		Labels:      map[string]string{},
	}

	// We need the binary accessible inside the container. The Docker
	// backend mounts BundlePath at WorkerMount (/app). Place the
	// binary in the bundle dir so it's accessible inside the container
	// at /app/wsecho-bin. But the Cmd path must be absolute inside the
	// container. We mount tmp -> /app, and the binary is at tmp/wsecho-bin.
	destBin := filepath.Join(tmp, "wsecho-bin")
	data, err := os.ReadFile(wsBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destBin, data, 0o755); err != nil {
		t.Fatal(err)
	}

	// Update command to use in-container path.
	spec.Cmd = []string{"/app/wsecho-bin"}

	if err := be.Spawn(ctx, spec); err != nil {
		t.Fatalf("spawn ws-echo container: %v", err)
	}
	t.Cleanup(func() { be.Stop(context.Background(), workerID) })

	// Get the container's address and wait for it to become healthy.
	addr, err := be.Addr(ctx, workerID)
	if err != nil {
		t.Fatalf("get container addr: %v", err)
	}
	t.Logf("ws-echo container at %s", addr)

	// Wait for the WS echo server to start accepting connections.
	// Use 30s to match the WorkerStartTimeout in the test config and
	// give slow CI runners enough headroom.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if be.HealthCheck(ctx, workerID) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !be.HealthCheck(ctx, workerID) {
		// Log diagnostics so we know why next time.
		if _, addrErr := be.Addr(ctx, workerID); addrErr != nil {
			t.Logf("container addr lookup failed (container may have crashed): %v", addrErr)
		}
		if ls, logErr := be.Logs(ctx, workerID); logErr == nil {
			var lines []string
			for i := 0; i < 20; i++ {
				select {
				case l, ok := <-ls.Lines:
					if !ok {
						break
					}
					lines = append(lines, l)
				case <-time.After(500 * time.Millisecond):
				}
			}
			ls.Close()
			if len(lines) > 0 {
				t.Logf("container logs:\n%s", strings.Join(lines, "\n"))
			}
		}
		t.Fatal("ws-echo container did not become healthy within 30s")
	}

	// Register the worker in the server's registry and worker map
	// so the proxy can route to it.
	srv.Registry.Set(workerID, addr)
	srv.Workers.Set(workerID, server.ActiveWorker{AppID: app.ID})

	// Create the HTTP test server with the full router.
	handler := api.NewRouter(srv, func() {}, nil, context.Background())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

// TestDockerWSEcho deploys a real WebSocket echo server in a Docker
// container, connects through the proxy, sends messages, and verifies
// they are echoed back.
func TestDockerWSEcho(t *testing.T) {
	wsBin := buildWSEchoBinary(t)

	_, ts := dockerWSTestSetup(t, wsBin)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/ws-docker-app/"

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial through proxy: %v", err)
	}
	defer conn.CloseNow()

	// Send several messages and verify echo.
	messages := []string{"hello", "world", "blockyard"}
	for _, msg := range messages {
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read echo for %q: %v", msg, err)
		}
		if typ != websocket.MessageText {
			t.Errorf("expected text message, got %v", typ)
		}
		if string(data) != msg {
			t.Errorf("expected echo %q, got %q", msg, data)
		}
	}

	conn.Close(websocket.StatusNormalClosure, "done")
}

// TestDockerWSLargeMessage sends a 100KB binary message through the
// proxy to a real Docker WS echo container and verifies the full
// payload is returned intact.
func TestDockerWSLargeMessage(t *testing.T) {
	wsBin := buildWSEchoBinary(t)

	_, ts := dockerWSTestSetup(t, wsBin)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/ws-docker-app/"

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1)

	// 100KB message simulating a Shiny renderPlot base64 PNG response.
	bigMsg := make([]byte, 100*1024)
	for i := range bigMsg {
		bigMsg[i] = byte('A' + i%26)
	}

	if err := conn.Write(ctx, websocket.MessageBinary, bigMsg); err != nil {
		t.Fatalf("write large message: %v", err)
	}

	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read large echo: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("expected binary message, got %v", typ)
	}
	if len(data) != len(bigMsg) {
		t.Fatalf("expected %d bytes, got %d", len(bigMsg), len(data))
	}
	// Verify content integrity.
	for i := range data {
		if data[i] != bigMsg[i] {
			t.Fatalf("byte mismatch at offset %d: expected %d, got %d", i, bigMsg[i], data[i])
		}
	}
}

// TestDockerWSBackendClose verifies that when the backend container's WS
// server closes the connection, the disconnect propagates through the
// proxy to the client.
func TestDockerWSBackendClose(t *testing.T) {
	wsBin := buildWSEchoBinary(t)

	_, ts := dockerWSTestSetup(t, wsBin)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/ws-docker-app/"

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.CloseNow()

	// Send the magic "close-me" message that tells the echo server
	// to close the WebSocket connection from the backend side.
	if err := conn.Write(ctx, websocket.MessageText, []byte("close-me")); err != nil {
		t.Fatalf("write close-me: %v", err)
	}

	// The backend closes after reading "close-me" -- the client
	// should see an error on the next read.
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Error("expected error from Read after backend close, got nil")
	}
}

// TestDockerWSReconnectCache verifies the full WS cache/reconnect lifecycle
// with a real Docker container: connect, disconnect, reconnect with the same
// session cookie, and confirm the backend connection was reused.
func TestDockerWSReconnectCache(t *testing.T) {
	wsBin := buildWSEchoBinary(t)

	_, ts := dockerWSTestSetup(t, wsBin)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/ws-docker-app/"

	// First connection -- get a session cookie.
	conn1, resp1, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial 1: %v", err)
	}

	var sessCookie *http.Cookie
	for _, c := range resp1.Cookies() {
		if c.Name == "blockyard_route" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie on first WS response")
	}

	// Verify echo works on first connection.
	if err := conn1.Write(ctx, websocket.MessageText, []byte("first")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn1.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" {
		t.Fatalf("expected 'first', got %q", data)
	}

	// Close the client side -- backend should be cached by the proxy.
	conn1.Close(websocket.StatusNormalClosure, "bye")
	time.Sleep(200 * time.Millisecond)

	// Reconnect with the same session cookie.
	hdr := http.Header{}
	hdr.Set("Cookie", fmt.Sprintf("%s=%s", sessCookie.Name, sessCookie.Value))
	conn2, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: hdr,
	})
	if err != nil {
		t.Fatalf("ws dial 2 (reconnect): %v", err)
	}
	defer conn2.CloseNow()

	// Verify echo works on the reconnected connection.
	if err := conn2.Write(ctx, websocket.MessageText, []byte("second")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn2.Read(ctx)
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("expected 'second', got %q", data)
	}
}
