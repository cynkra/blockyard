package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cynkra/blockyard/internal/api"
	"github.com/cynkra/blockyard/internal/backend/mock"
	"github.com/cynkra/blockyard/internal/config"
	"github.com/cynkra/blockyard/internal/db"
	"github.com/cynkra/blockyard/internal/server"
	"github.com/cynkra/blockyard/internal/testutil"
)

func testProxyServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	tmp := t.TempDir()

	rvBin := testutil.FakeRvBinary(t)
	cfg := &config.Config{
		Server: config.ServerConfig{Token: config.NewSecret("test-token")},
		Docker: config.DockerConfig{
			Image:        "test-image",
			ShinyPort:    3838,
			RvBinaryPath: rvBin,
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
		},
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	be := mock.New()
	srv := server.NewServer(cfg, be, database)
	handler := api.NewRouter(srv)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return srv, ts
}

// createAndStartApp creates an app, uploads a bundle, waits for the
// mock restore, and starts the app via the API.
func createAndStartApp(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/apps",
		bytes.NewReader([]byte(`{"name":"`+name+`"}`)))
	req.Header.Set("Authorization", "Bearer test-token")
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
	req.Header.Set("Authorization", "Bearer test-token")
	http.DefaultClient.Do(req)
	time.Sleep(200 * time.Millisecond)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/start", nil)
	req.Header.Set("Authorization", "Bearer test-token")
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
		if c.Name == "blockyard_session" && c.Value != "" {
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
		t.Error("expected blockyard_session cookie")
	}
}

func TestProxySessionReuse(t *testing.T) {
	srv, ts := testProxyServer(t)
	createAndStartApp(t, ts, "my-app")

	resp, _ := http.Get(ts.URL + "/app/my-app/")
	var sessCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "blockyard_session" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie")
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
	req.Header.Set("Authorization", "Bearer test-token")
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
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	srv.DB.CreateBundle("b-1", id)
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
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var created map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&created)
	id := created["id"].(string)

	req, _ = http.NewRequest("POST",
		ts.URL+"/api/v1/apps/"+id+"/bundles",
		bytes.NewReader(testutil.MakeBundle(t)))
	req.Header.Set("Authorization", "Bearer test-token")
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

// wsEchoHandler returns a WebSocket handler that echoes messages back.
// The backend also removes its own read limit so large messages work.
func wsEchoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
// disconnects and reconnects within the TTL, the backend WebSocket
// connection is reused (Bug 3 fix — context lifecycle).
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
		if c.Name == "blockyard_session" {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("no session cookie on WebSocket response")
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

	// Close client connection — backend should be cached
	conn1.Close(websocket.StatusNormalClosure, "bye")
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
