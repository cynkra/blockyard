package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/cynkra/blockyard/internal/api"
	"github.com/cynkra/blockyard/internal/auth"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/proxy"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/testutil"
)

const testPAT = "by_testtoken000000000000000000000000000000000"

func seedTestAdmin(t *testing.T, database *db.DB) {
	t.Helper()
	_, err := database.UpsertUserWithRole("admin", "admin@test", "Admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	hash := auth.HashPAT(testPAT)
	_, err = database.CreatePAT("test-pat-id", hash, "admin", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
}

func testProxyServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{
			Image:      "test-image",
			ShinyPort:  3838,
			PakVersion: "stable",
		},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			WsCacheTTL:         config.Duration{Duration: 5 * time.Second},
			WorkerStartTimeout: config.Duration{Duration: 5 * time.Second},
			MaxWorkers:         10,
			HTTPForwardTimeout: config.Duration{Duration: 5 * time.Minute},
		},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	// Track background restore goroutines so cleanup waits for them.
	var wg sync.WaitGroup
	srv.RestoreWG = &wg
	handler := api.NewRouter(srv, func() {}, nil, context.Background())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	// Wait for restore goroutines before DB/TempDir cleanup (LIFO order).
	t.Cleanup(wg.Wait)

	return srv, ts
}

// createAndStartApp creates an app, uploads a bundle, waits for the
// mock restore, and starts the app via the API.
func createAndStartApp(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/start", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
}

func TestProxyHTTPForward(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	resp, err := http.Get(ts.URL + "/app/my-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestProxySetsSessionCookie(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	resp, err := http.Get(ts.URL + "/app/my-app/")
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == "blockyard_route" && c.Value != "" {
			found = true
			if c.Path != "/app/my-app/" {
				t.Errorf("expected path /app/my-app/, got %s", c.Path)
			}
			if !c.HttpOnly {
				t.Error("expected HttpOnly cookie")
			}
		}
	}
	if !found {
		t.Error("expected blockyard_route cookie")
	}
}

func TestProxySessionReuse(t *testing.T) {
	srv, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	resp, _ := http.Get(ts.URL + "/app/my-app/")
	var sessCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "blockyard_route" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie")
		return
	}

	initialWorkerCount := srv.Workers.Count()

	req, _ := http.NewRequest("GET", ts.URL+"/app/my-app/", nil)
	req.AddCookie(sessCookie)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if srv.Workers.Count() != initialWorkerCount {
		t.Errorf("expected %d workers (reuse), got %d",
			initialWorkerCount, srv.Workers.Count())
	}
}

func TestProxyTrailingSlashRedirect(t *testing.T) {
	_, ts := testProxyServer(t)

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/app/my-app")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("expected 301, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/app/my-app/" {
		t.Errorf("expected redirect to /app/my-app/, got %s", loc)
	}
}

func TestProxyNonexistentApp(t *testing.T) {
	_, ts := testProxyServer(t)
	resp, err := http.Get(ts.URL + "/app/nonexistent/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestProxyAppWithoutBundleReturns503(t *testing.T) {
	_, ts := testProxyServer(t)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"no-bundle"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	http.DefaultClient.Do(req)

	resp, err := http.Get(ts.URL + "/app/no-bundle/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestProxyAtCapacityReturns503(t *testing.T) {
	srv, ts := testProxyServer(t)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"cap-app"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	srv.DB.CreateBundle("b-1", id, "", false)
	srv.DB.UpdateBundleStatus("b-1", "ready")
	srv.DB.SetActiveBundle(id, "b-1")

	for i := range srv.Config.Proxy.MaxWorkers {
		srv.Workers.Set(
			fmt.Sprintf("fake-%d", i),
			server.ActiveWorker{AppID: "fake"},
		)
	}

	resp, err := http.Get(ts.URL + "/app/cap-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", resp.StatusCode)
	}
}

func TestProxySubPathForwarded(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	resp, err := http.Get(ts.URL + "/app/my-app/sub/path")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestProxyRoutesUnauthenticated(t *testing.T) {
	_, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	resp, err := http.Get(ts.URL + "/app/my-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("proxy: expected 200 without auth, got %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/api/v1/apps")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("api: expected 401 without auth, got %d", resp.StatusCode)
	}
}

func TestProxyColdStartSpawnsWorker(t *testing.T) {
	srv, ts := testProxyServer(t)

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"cold-app"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	if srv.Workers.Count() != 0 {
		t.Fatalf("expected 0 workers before proxy hit, got %d", srv.Workers.Count())
	}

	resp, err := http.Get(ts.URL + "/app/cold-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if srv.Workers.Count() != 1 {
		t.Errorf("expected 1 worker after cold start, got %d", srv.Workers.Count())
	}
}

// wsHeaderCapture returns a WebSocket handler that captures the
// request headers, then echoes messages. The captured headers are
// written to *captured after the upgrade succeeds.
func wsHeaderCapture(captured *http.Header) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*captured = r.Header.Clone()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			c.Write(context.Background(), typ, data)
		}
	}
}

// wsEchoHandler returns a WebSocket handler that echoes messages back.
// The backend also removes its own read limit so large messages work.
func wsEchoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			c.Write(context.Background(), typ, data)
		}
	}
}

func TestProxyWebSocketEcho(t *testing.T) {
	srv, ts := testProxyServer(t)
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsEchoHandler())
	createAndStartApp(t, ts, "echo-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/echo-app/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText || string(data) != "hello" {
		t.Errorf("expected text 'hello', got %v %q", typ, data)
	}
}

// TestProxyWebSocketLargeMessage verifies that messages larger than the
// default 32KB read limit are forwarded correctly (Bug 1 fix).
func TestProxyWebSocketLargeMessage(t *testing.T) {
	srv, ts := testProxyServer(t)
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsEchoHandler())
	createAndStartApp(t, ts, "large-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/large-app/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(-1)

	// 100KB message — well over the default 32KB limit.
	// This simulates a Shiny renderPlot base64 PNG response.
	bigMsg := make([]byte, 100*1024)
	for i := range bigMsg {
		bigMsg[i] = byte('A' + i%26)
	}

	if err := conn.Write(ctx, websocket.MessageBinary, bigMsg); err != nil {
		t.Fatal(err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read failed (message too big?): %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("expected binary message, got %v", typ)
	}
	if len(data) != len(bigMsg) {
		t.Errorf("expected %d bytes, got %d", len(bigMsg), len(data))
	}
}

// TestProxyWebSocketCacheReconnect verifies that when a client
// disconnects abnormally (network error) and reconnects within the TTL,
// the backend WebSocket connection is reused.
func TestProxyWebSocketCacheReconnect(t *testing.T) {
	srv, ts := testProxyServer(t)

	// Track how many times the backend accepts a WebSocket connection
	var backendAccepts int32
	srv.Backend.(*mock.MockBackend).SetWSHandler(func(w http.ResponseWriter, r *http.Request) {
		backendAccepts++
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			c.Write(context.Background(), typ, data)
		}
	})

	createAndStartApp(t, ts, "reconn-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/reconn-app/"

	// First connection — get session cookie
	conn1, resp1, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Extract session cookie
	var sessCookie *http.Cookie
	for _, c := range resp1.Cookies() {
		if c.Name == "blockyard_route" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie on WebSocket response")
		return
	}

	// Send and receive a message to confirm it works
	if err := conn1.Write(ctx, websocket.MessageText, []byte("msg1")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn1.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "msg1" {
		t.Fatalf("expected 'msg1', got %q", data)
	}

	if backendAccepts != 1 {
		t.Fatalf("expected 1 backend accept, got %d", backendAccepts)
	}

	// Close client connection abruptly (simulates network error) —
	// backend should be cached for reconnect.
	conn1.CloseNow()
	// Give the proxy a moment to detect the close and cache the backend
	time.Sleep(100 * time.Millisecond)

	// Reconnect with same session cookie — should reuse cached backend
	hdr := http.Header{}
	hdr.Set("Cookie", sessCookie.String())
	conn2, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: hdr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.CloseNow()

	// Send and receive to confirm the reconnected path works
	if err := conn2.Write(ctx, websocket.MessageText, []byte("msg2")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn2.Read(ctx)
	if err != nil {
		t.Fatalf("read after reconnect failed: %v", err)
	}
	if string(data) != "msg2" {
		t.Fatalf("expected 'msg2', got %q", data)
	}

	// The backend should NOT have accepted a second connection —
	// the cached one should have been reused.
	if backendAccepts != 1 {
		t.Errorf("expected 1 backend accept (cached reuse), got %d", backendAccepts)
	}
}

// TestProxyWebSocketCleanCloseSkipsCache verifies that a clean WebSocket
// close (1000 Normal, 1001 Going Away) does NOT cache the backend reader.
// A clean close corresponds to page reload or navigation, where the Shiny
// client state is destroyed and a stale backend would deliver messages
// that the freshly-initialized client cannot handle.
func TestProxyWebSocketCleanCloseSkipsCache(t *testing.T) {
	srv, ts := testProxyServer(t)

	var backendAccepts int32
	srv.Backend.(*mock.MockBackend).SetWSHandler(func(w http.ResponseWriter, r *http.Request) {
		backendAccepts++
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			c.Write(context.Background(), typ, data)
		}
	})

	createAndStartApp(t, ts, "clean-close-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/clean-close-app/"

	conn1, resp1, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	var sessCookie *http.Cookie
	for _, c := range resp1.Cookies() {
		if c.Name == "blockyard_route" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie on WebSocket response")
		return
	}

	// Confirm first connection works
	if err := conn1.Write(ctx, websocket.MessageText, []byte("msg1")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn1.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "msg1" {
		t.Fatalf("expected 'msg1', got %q", data)
	}

	// Close cleanly (simulates page reload / browser Going Away)
	conn1.Close(websocket.StatusGoingAway, "page reload")
	time.Sleep(100 * time.Millisecond)

	// Reconnect with same session cookie — backend should NOT be cached,
	// so the proxy must dial a fresh backend connection.
	hdr := http.Header{}
	hdr.Set("Cookie", sessCookie.String())
	conn2, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: hdr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.CloseNow()

	if err := conn2.Write(ctx, websocket.MessageText, []byte("msg2")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn2.Read(ctx)
	if err != nil {
		t.Fatalf("read after reconnect failed: %v", err)
	}
	if string(data) != "msg2" {
		t.Fatalf("expected 'msg2', got %q", data)
	}

	// Backend should have accepted TWO connections — no cache reuse.
	if backendAccepts != 2 {
		t.Errorf("expected 2 backend accepts (no cache), got %d", backendAccepts)
	}
}

// TestProxyWebSocketQueryStringForwarded verifies that query parameters
// on the WebSocket upgrade URL are forwarded to the backend.
func TestProxyWebSocketQueryStringForwarded(t *testing.T) {
	srv, ts := testProxyServer(t)

	var backendRequestURL string
	srv.Backend.(*mock.MockBackend).SetWSHandler(func(w http.ResponseWriter, r *http.Request) {
		backendRequestURL = r.URL.String()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			c.Write(context.Background(), typ, data)
		}
	})

	createAndStartApp(t, ts, "qs-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/qs-app/websocket/?w=abc&s=def"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// Exchange a message so the connection is fully established.
	if err := conn.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	conn.Read(ctx)

	// The backend should see the stripped path with query string preserved.
	if backendRequestURL == "" {
		t.Fatal("backend never received the upgrade request")
	}
	// Path should be stripped of /app/qs-app prefix, query preserved.
	if !strings.Contains(backendRequestURL, "w=abc") ||
		!strings.Contains(backendRequestURL, "s=def") {
		t.Errorf("expected query params w=abc&s=def in backend URL, got %q", backendRequestURL)
	}
	if !strings.HasPrefix(backendRequestURL, "/websocket/") {
		t.Errorf("expected path /websocket/..., got %q", backendRequestURL)
	}
}

// TestProxyWebSocketBinaryMessage verifies that binary WebSocket frames
// are forwarded with their message type preserved. Shiny uses a custom
// binary protocol (0x01020202 magic header) for file uploads.
func TestProxyWebSocketBinaryMessage(t *testing.T) {
	srv, ts := testProxyServer(t)
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsEchoHandler())
	createAndStartApp(t, ts, "bin-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/bin-app/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// Simulate a Shiny binary upload frame: 0x01020202 magic + payload.
	payload := []byte{0x01, 0x02, 0x02, 0x02}
	// Append a 4-byte length (little-endian) and a small JSON blob.
	jsonBlob := []byte(`{"method":"upload","args":["file1"]}`)
	length := uint32(len(jsonBlob))
	payload = append(payload,
		byte(length), byte(length>>8), byte(length>>16), byte(length>>24))
	payload = append(payload, jsonBlob...)

	if err := conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("expected MessageBinary, got %v", typ)
	}
	if len(data) != len(payload) {
		t.Errorf("expected %d bytes, got %d", len(payload), len(data))
	}
	// Verify magic header preserved byte-for-byte.
	if data[0] != 0x01 || data[1] != 0x02 || data[2] != 0x02 || data[3] != 0x02 {
		t.Errorf("binary magic header corrupted: %x", data[:4])
	}
}

// TestProxyWebSocketCrossOriginRejected verifies that cross-origin
// WebSocket upgrades are rejected when external_url is not configured.
// This prevents cross-site WebSocket hijacking in misconfigured deployments.
func TestProxyWebSocketCrossOriginRejected(t *testing.T) {
	srv, ts := testProxyServer(t)
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsEchoHandler())
	createAndStartApp(t, ts, "origin-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/origin-app/"

	// Dial with a cross-origin Origin header — should be rejected
	// because external_url is not configured.
	_, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin": []string{"http://different-host.example.com"},
		},
	})
	if err == nil {
		t.Fatal("expected cross-origin WebSocket to be rejected, but it succeeded")
	}
}

// TestProxyWebSocketCrossOriginAllowedByExternalURL verifies that
// cross-origin WebSocket upgrades are accepted when the Origin matches
// the configured external_url host.
func TestProxyWebSocketCrossOriginAllowedByExternalURL(t *testing.T) {
	srv, ts := testProxyServer(t)
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsEchoHandler())

	// Configure external_url to match the cross-origin header we'll send.
	srv.Config.Server.ExternalURL = "http://my-app.example.com"

	createAndStartApp(t, ts, "origin-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/origin-app/"

	// Dial with Origin matching the configured external_url host.
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin": []string{"http://my-app.example.com"},
		},
	})
	if err != nil {
		t.Fatalf("same-origin WebSocket dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Confirm the connection works end-to-end.
	if err := conn.Write(ctx, websocket.MessageText, []byte("xorigin")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read after cross-origin upgrade failed: %v", err)
	}
	if string(data) != "xorigin" {
		t.Errorf("expected 'xorigin', got %q", data)
	}
}

// TestProxyWebSocketBackendDisconnect verifies that when the backend
// WebSocket closes after one message, the disconnect is propagated to
// the client (the client's next Read returns an error).
func TestProxyWebSocketBackendDisconnect(t *testing.T) {
	srv, ts := testProxyServer(t)

	// Backend handler: accept one message, then close.
	srv.Backend.(*mock.MockBackend).SetWSHandler(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		// Read one message, then close.
		c.Read(context.Background())
		c.Close(websocket.StatusNormalClosure, "done")
	})

	createAndStartApp(t, ts, "disc-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/disc-app/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	// Send a message to the backend.
	if err := conn.Write(ctx, websocket.MessageText, []byte("trigger")); err != nil {
		t.Fatal(err)
	}

	// The backend closes after reading — client should see an error.
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Error("expected error from Read after backend disconnect, got nil")
	}
}

// TestProxyWebSocketClientWriteError verifies that abruptly closing the
// client side does not cause a panic in the proxy.
func TestProxyWebSocketClientWriteError(t *testing.T) {
	srv, ts := testProxyServer(t)
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsEchoHandler())
	createAndStartApp(t, ts, "cwerr-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/cwerr-app/"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Abruptly close the client connection.
	conn.CloseNow()

	// Give the proxy time to notice and clean up.
	time.Sleep(200 * time.Millisecond)
	// If we reach here without a panic, the test passes.
}

// TestProxyAppLookupByUUID verifies that requesting /app/{uuid}/ resolves
// correctly, not just /app/{name}/.
func TestProxyAppLookupByUUID(t *testing.T) {
	_, ts := testProxyServer(t)

	// Create app and extract its UUID.
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"uuid-app"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	appID := created["id"].(string)

	// Upload bundle and start.
	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+appID+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+appID+"/start", nil)
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)

	// Request via UUID instead of name.
	resp, err = http.Get(ts.URL + "/app/" + appID + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 for UUID lookup, got %d", resp.StatusCode)
	}
}

// TestProxyStaleSessionCreatesNew verifies that presenting a session
// cookie whose worker has been removed from the registry results in a
// new session (and a new worker being assigned).
func TestProxyStaleSessionCreatesNew(t *testing.T) {
	srv, ts := testProxyServer(t)
	createAndStartApp(t, ts, "stale-app")

	// First request — get a session cookie.
	resp, err := http.Get(ts.URL + "/app/stale-app/")
	if err != nil {
		t.Fatal(err)
	}
	var sessCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "blockyard_route" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie")
		return
	}

	// Delete the worker from the registry so the session is stale.
	workerIDs := srv.Workers.All()
	for _, wid := range workerIDs {
		srv.Registry.Delete(wid)
	}

	// Second request with the stale cookie.
	req, _ := http.NewRequest("GET", ts.URL+"/app/stale-app/", nil)
	req.AddCookie(sessCookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 after stale session, got %d", resp.StatusCode)
	}

	// Should have received a new session cookie.
	var newCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "blockyard_route" {
			newCookie = c
		}
	}
	if newCookie == nil {
		t.Error("expected a new session cookie after stale session")
	} else if newCookie.Value == sessCookie.Value {
		t.Error("expected new session ID, got same as stale session")
	}
}

// TestProxyWebSocketForwardsHeaders verifies that the proxy forwards
// relevant client headers to the backend (Bug 4 fix — lost headers).
func TestProxyWebSocketForwardsHeaders(t *testing.T) {
	srv, ts := testProxyServer(t)

	var backendHeaders http.Header
	srv.Backend.(*mock.MockBackend).SetWSHandler(wsHeaderCapture(&backendHeaders))
	createAndStartApp(t, ts, "hdr-app")

	ctx := context.Background()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) +
		"/app/hdr-app/"

	// Use same-origin (matching the test server's host) so the
	// proxy's origin check accepts the connection.
	sameOrigin := ts.URL // e.g. "http://127.0.0.1:<port>"

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Origin":     []string{sameOrigin},
			"Cookie":     []string{"foo=bar"},
			"User-Agent": []string{"TestAgent/1.0"},
		},
	})
	if err != nil {
		t.Fatalf("WebSocket dial failed: %v", err)
	}
	defer conn.CloseNow()

	// Exchange a message so the connection is fully established.
	if err := conn.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	conn.Read(ctx)

	if backendHeaders == nil {
		t.Fatal("backend never received the upgrade request")
		return
	}
	if got := backendHeaders.Get("Origin"); got != sameOrigin {
		t.Errorf("Origin: expected %q, got %q", sameOrigin, got)
	}
	if got := backendHeaders.Get("Cookie"); got != "foo=bar" {
		t.Errorf("Cookie: expected 'foo=bar', got %q", got)
	}
	if got := backendHeaders.Get("User-Agent"); got != "TestAgent/1.0" {
		t.Errorf("User-Agent: expected 'TestAgent/1.0', got %q", got)
	}
}

// TestProxyInjectShinyHeaders verifies that the proxy sets X-Shiny-User
// and X-Shiny-Access headers on forwarded requests when the caller is
// authenticated via OIDC.
func TestProxyInjectShinyHeaders(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testProxyServerWithOIDC(t, idp)

	// Set up a handler on the mock backend that echoes X-Shiny-User and
	// X-Shiny-Access back as response headers so we can inspect them.
	be := srv.Backend.(*mock.MockBackend)
	be.SetHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-User", r.Header.Get("X-Shiny-User"))
		w.Header().Set("X-Echo-Access", r.Header.Get("X-Shiny-Access"))
		w.WriteHeader(http.StatusOK)
	}))

	// Publisher creates and starts a public app.
	srv.DB.UpsertUserWithRole("publisher-1", "publisher-1@example.com", "Publisher", "publisher")
	pubToken := createTestPAT(t, srv.DB, "publisher-1")
	appID := createAndStartAppWithPAT(t, ts, "header-app", pubToken)

	// Set access_type to "public" so any authenticated user can access it.
	req, _ := http.NewRequest("PATCH", ts.URL+"/api/v1/apps/"+appID,
		bytes.NewReader([]byte(`{"access_type":"public"}`)))
	req.Header.Set("Authorization", "Bearer "+pubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set access_type: expected 200, got %d", resp.StatusCode)
	}

	// Create a viewer user and send a request through the proxy.
	srv.DB.UpsertUserWithRole("viewer-1", "viewer-1@example.com", "Viewer", "viewer")
	cookie := makeSessionCookie(t, srv, "viewer-1")

	req, _ = http.NewRequest("GET", ts.URL+"/app/header-app/", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify the backend received the correct identity headers.
	if got := resp.Header.Get("X-Echo-User"); got != "Viewer" {
		t.Errorf("X-Shiny-User: expected %q, got %q", "Viewer", got)
	}
	if got := resp.Header.Get("X-Echo-Access"); got != "viewer" {
		t.Errorf("X-Shiny-Access: expected %q, got %q", "viewer", got)
	}
}

// TestProxyCollaboratorAccess verifies that a user with a collaborator
// ACL grant receives X-Shiny-Access: collaborator.
func TestProxyCollaboratorAccess(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testProxyServerWithOIDC(t, idp)

	be := srv.Backend.(*mock.MockBackend)
	be.SetHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-User", r.Header.Get("X-Shiny-User"))
		w.Header().Set("X-Echo-Access", r.Header.Get("X-Shiny-Access"))
		w.WriteHeader(http.StatusOK)
	}))

	// Publisher creates and starts an ACL app (default access_type).
	srv.DB.UpsertUserWithRole("publisher-2", "publisher-2@example.com", "Publisher", "publisher")
	pubToken := createTestPAT(t, srv.DB, "publisher-2")
	appID := createAndStartAppWithPAT(t, ts, "collab-app", pubToken)

	// Create a viewer-role user and grant them collaborator access on the app.
	srv.DB.UpsertUserWithRole("collab-user", "collab@example.com", "Collab", "viewer")
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps/"+appID+"/access",
		bytes.NewReader([]byte(`{"kind":"user","principal":"collab-user","role":"collaborator"}`)))
	req.Header.Set("Authorization", "Bearer "+pubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("grant access: expected 204, got %d", resp.StatusCode)
	}

	// Send a proxy request as the collaborator user.
	cookie := makeSessionCookie(t, srv, "collab-user")
	req, _ = http.NewRequest("GET", ts.URL+"/app/collab-app/", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if got := resp.Header.Get("X-Echo-User"); got != "Collab" {
		t.Errorf("X-Shiny-User: expected %q, got %q", "Collab", got)
	}
	if got := resp.Header.Get("X-Echo-Access"); got != "collaborator" {
		t.Errorf("X-Shiny-Access: expected %q, got %q", "collaborator", got)
	}
}

// TestProxyAdminAccess verifies that an admin user receives
// X-Shiny-Access: owner (admin maps to RelationAdmin -> "owner").
func TestProxyAdminAccess(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testProxyServerWithOIDC(t, idp)

	be := srv.Backend.(*mock.MockBackend)
	be.SetHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-User", r.Header.Get("X-Shiny-User"))
		w.Header().Set("X-Echo-Access", r.Header.Get("X-Shiny-Access"))
		w.WriteHeader(http.StatusOK)
	}))

	// Publisher creates and starts an app.
	srv.DB.UpsertUserWithRole("publisher-3", "publisher-3@example.com", "Publisher", "publisher")
	pubToken := createTestPAT(t, srv.DB, "publisher-3")
	createAndStartAppWithPAT(t, ts, "admin-app", pubToken)

	// The "admin" user was already seeded by seedTestAdmin. Send a proxy
	// request as admin.
	cookie := makeSessionCookie(t, srv, "admin")
	req, _ := http.NewRequest("GET", ts.URL+"/app/admin-app/", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	if got := resp.Header.Get("X-Echo-User"); got != "Admin" {
		t.Errorf("X-Shiny-User: expected %q, got %q", "Admin", got)
	}
	if got := resp.Header.Get("X-Echo-Access"); got != "owner" {
		t.Errorf("X-Shiny-Access: expected %q, got %q", "owner", got)
	}
}

// TestProxyInjectCredentialsShared verifies that shared container
// credential injection sets X-Blockyard-Session-Token. Since
// injectCredentials is unexported and already covered by vault_test.go
// (TestInjectCredentials_SharedContainer_InjectsSessionToken), this
// integration-level test is skipped to avoid duplicating OIDC + vault
// setup complexity.
func TestProxyInjectCredentialsShared(t *testing.T) {
	t.Skip("requires OIDC + vault integration; covered by vault_test.go")
}

// TestRedirectTrailingSlash tests the RedirectTrailingSlash handler
// directly with httptest.NewRecorder.
func TestRedirectTrailingSlash(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/app/{name}", proxy.RedirectTrailingSlash)

	tests := []struct {
		path     string
		wantLoc  string
		wantCode int
	}{
		{"/app/my-app", "/app/my-app/", http.StatusMovedPermanently},
		{"/app/other", "/app/other/", http.StatusMovedPermanently},
		{"/app/INVALID", "", http.StatusNotFound},
		{"/app/has%20spaces", "", http.StatusNotFound},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", tt.path, nil)
		r.ServeHTTP(rec, req)

		if rec.Code != tt.wantCode {
			t.Errorf("%s: expected %d, got %d", tt.path, tt.wantCode, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != tt.wantLoc {
			t.Errorf("%s: expected Location %q, got %q", tt.path, tt.wantLoc, loc)
		}
	}
}

// testProxyServerWithOIDC creates a test proxy server with OIDC configured.
// It returns the server, httptest.Server, and the MockIdP so callers can
// create sessions.
func testProxyServerWithOIDC(t *testing.T, idp *testutil.MockIdP) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	cfg := &config.Config{
		Server: config.ServerConfig{},
		Docker: config.DockerConfig{
			Image:      "test-image",
			ShinyPort:  3838,
			PakVersion: "stable",
		},
		Storage: config.StorageConfig{
			BundleServerPath: tmp,
			BundleWorkerPath: "/app",
			BundleRetention:  50,
			MaxBundleSize:    10 * 1024 * 1024,
		},
		Proxy: config.ProxyConfig{
			WsCacheTTL:         config.Duration{Duration: 5 * time.Second},
			WorkerStartTimeout: config.Duration{Duration: 5 * time.Second},
			MaxWorkers:         10,
			HTTPForwardTimeout: config.Duration{Duration: 5 * time.Minute},
		},
		OIDC: &config.OidcConfig{
			IssuerURL:    idp.IssuerURL(),
			ClientID:     "blockyard",
			ClientSecret: config.NewSecret("test-secret"),
			CookieMaxAge: config.Duration{Duration: 24 * time.Hour},
		},
	}

	database, err := db.Open(config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	seedTestAdmin(t, database)

	be := mock.New()
	srv := server.NewServer(cfg, be, database)

	// Set up signing key and user session store so AppAuthMiddleware works.
	srv.SigningKey = auth.DeriveSigningKey("test-session-secret")
	srv.UserSessions = auth.NewUserSessionStore()

	handler := api.NewRouter(srv, func() {}, nil, context.Background())
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

// createTestPAT creates a Personal Access Token for the given user subject
// and returns the plaintext token string.
func createTestPAT(t *testing.T, database *db.DB, sub string) string {
	t.Helper()
	plaintext, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreatePAT(plaintext[3:9], hash, sub, "test", nil); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

// createAndStartAppWithPAT creates an app using a PAT bearer token, uploads
// a bundle, and starts the app. Returns the app ID.
func createAndStartAppWithPAT(t *testing.T, ts *httptest.Server, name, token string) string {
	t.Helper()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id := created["id"].(string)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+token)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/start", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	http.DefaultClient.Do(req)

	return id
}

// makeSessionCookie creates a signed session cookie for the given sub
// and registers a server-side session.
func makeSessionCookie(t *testing.T, srv *server.Server, sub string) *http.Cookie {
	t.Helper()

	// Register the server-side session.
	srv.UserSessions.Set(sub, &auth.UserSession{
		AccessToken: "mock-access-token",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})

	// Create signed cookie payload.
	payload := &auth.CookiePayload{
		Sub:      sub,
		IssuedAt: time.Now().Unix(),
	}
	cookieValue, err := payload.Encode(srv.SigningKey)
	if err != nil {
		t.Fatal(err)
	}

	return &http.Cookie{
		Name:  "blockyard_session",
		Value: cookieValue,
	}
}

func TestProxyOIDCRedirectsUnauthenticated(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testProxyServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("publisher-1", "publisher-1@example.com", "Publisher", "publisher")
	token := createTestPAT(t, srv.DB, "publisher-1")

	createAndStartAppWithPAT(t, ts, "oidc-app", token)

	// Make an unauthenticated request (no cookies, no auth header).
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/app/oidc-app/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/login?return_url=") {
		t.Errorf("expected redirect to /login?return_url=..., got Location: %s", loc)
	}
}

func TestProxyOIDCDeniesUnauthorizedUser(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testProxyServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("publisher-1", "publisher-1@example.com", "Publisher", "publisher")
	srv.DB.UpsertUserWithRole("viewer-1", "viewer-1@example.com", "Viewer", "viewer")

	// Publisher creates and starts app (access_type defaults to "acl").
	pubToken := createTestPAT(t, srv.DB, "publisher-1")
	createAndStartAppWithPAT(t, ts, "acl-app", pubToken)

	// Another user with a mapped role but NO grant on this app.
	cookie := makeSessionCookie(t, srv, "viewer-1")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("GET", ts.URL+"/app/acl-app/", nil)
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unauthorized user, got %d", resp.StatusCode)
	}
}

func TestProxyOIDCAllowsPublicApp(t *testing.T) {
	idp := testutil.NewMockIdP()
	defer idp.Close()
	srv, ts := testProxyServerWithOIDC(t, idp)

	srv.DB.UpsertUserWithRole("publisher-1", "publisher-1@example.com", "Publisher", "publisher")

	// Publisher creates and starts app.
	pubToken := createTestPAT(t, srv.DB, "publisher-1")
	appID := createAndStartAppWithPAT(t, ts, "public-app", pubToken)

	// Set access_type to "public".
	req, _ := http.NewRequest("PATCH", ts.URL+"/api/v1/apps/"+appID,
		bytes.NewReader([]byte(`{"access_type":"public"}`)))
	req.Header.Set("Authorization", "Bearer "+pubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set access_type: expected 200, got %d", resp.StatusCode)
	}

	// Any authenticated user (even without a grant) should get 200.
	srv.DB.UpsertUserWithRole("random-user", "random@example.com", "Random", "viewer")
	cookie := makeSessionCookie(t, srv, "random-user")
	req, _ = http.NewRequest("GET", ts.URL+"/app/public-app/", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for public app, got %d", resp.StatusCode)
	}
}

// TestProxyMultiplexSessions verifies that with max_sessions_per_worker > 1,
// two WebSocket clients are routed to the same worker and each gets
// independent per-connection state. This is the core property that makes
// Posit-style session multiplexing work: httpuv (Shiny's HTTP server)
// creates a separate ShinySession for each WebSocket connection, so two
// connections to the same R process get independent reactive graphs.
//
// The mock backend's WS handler mimics this by keeping a per-connection
// counter (local variable in the handler closure). If the proxy correctly
// routes both sessions to the same worker, incrementing one client's
// counter must not affect the other's.
func TestProxyMultiplexSessions(t *testing.T) {
	srv, ts := testProxyServer(t)

	// Track backend WS accept count to confirm both sessions hit the
	// same httptest.Server (= same worker process).
	var backendAccepts int32
	var mu sync.Mutex

	srv.Backend.(*mock.MockBackend).SetWSHandler(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backendAccepts++
		mu.Unlock()

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()

		// Per-connection counter — mirrors Shiny's per-session state.
		counter := 0
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			switch string(data) {
			case "inc":
				counter++
				c.Write(context.Background(), websocket.MessageText,
					[]byte(fmt.Sprintf("%d", counter)))
			case "get":
				c.Write(context.Background(), websocket.MessageText,
					[]byte(fmt.Sprintf("%d", counter)))
			}
		}
	})

	// Create app and upload bundle.
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"mux-app"}`)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer "+testPAT)
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	// Allow two sessions per worker.
	maxSessions := 2
	srv.DB.UpdateApp(id, db.AppUpdate{
		MaxSessionsPerWorker: &maxSessions,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/app/mux-app/"

	// --- Client 1: triggers cold start, spawns a worker ---
	conn1, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial 1: %v", err)
	}
	defer conn1.CloseNow()

	// --- Client 2: should reuse the same worker (capacity = 2) ---
	conn2, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial 2: %v", err)
	}
	defer conn2.CloseNow()

	// Both sessions must share one worker.
	if srv.Workers.Count() != 1 {
		t.Fatalf("expected 1 worker (multiplexed), got %d", srv.Workers.Count())
	}

	// --- Verify independent per-session state ---
	// Each round-trip also proves the backend WS is connected, so we
	// check the accept count after these exchanges (the proxy's client
	// Accept completes before the backend Dial, so checking immediately
	// after websocket.Dial races).

	// Increment client 1 twice.
	if err := conn1.Write(ctx, websocket.MessageText, []byte("inc")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn1.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1" {
		t.Errorf("client 1 first inc: expected '1', got %q", data)
	}

	if err := conn1.Write(ctx, websocket.MessageText, []byte("inc")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn1.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "2" {
		t.Errorf("client 1 second inc: expected '2', got %q", data)
	}

	// Client 2's counter must still be 0 — independent state.
	if err := conn2.Write(ctx, websocket.MessageText, []byte("get")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn2.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0" {
		t.Errorf("client 2 get: expected '0' (independent state), got %q", data)
	}

	// Increment client 2 — should start from its own zero.
	if err := conn2.Write(ctx, websocket.MessageText, []byte("inc")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn2.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1" {
		t.Errorf("client 2 inc: expected '1', got %q", data)
	}

	// Client 1 unaffected by client 2's increment.
	if err := conn1.Write(ctx, websocket.MessageText, []byte("get")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn1.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "2" {
		t.Errorf("client 1 get after client 2 inc: expected '2', got %q", data)
	}

	// Now that both connections have proven round-trip connectivity,
	// verify the backend saw exactly two WS connections (one per session,
	// same worker).
	mu.Lock()
	accepts := backendAccepts
	mu.Unlock()
	if accepts != 2 {
		t.Errorf("expected 2 backend WS accepts on same worker, got %d", accepts)
	}
}
