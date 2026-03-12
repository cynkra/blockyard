package mock

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"

	"github.com/cynkra/blockyard/internal/backend"
)

type MockBackend struct {
	mu           sync.RWMutex
	workers      map[string]*mockWorker
	HealthOK     atomic.Bool         // configurable: default true
	BuildSuccess atomic.Bool         // configurable: default true
	wsHandler    http.HandlerFunc    // optional WS handler for mock workers
}

type mockWorker struct {
	id     string
	spec   backend.WorkerSpec
	server *httptest.Server
}

func New() *MockBackend {
	b := &MockBackend{
		workers: make(map[string]*mockWorker),
	}
	b.HealthOK.Store(true)
	b.BuildSuccess.Store(true)
	return b
}

func (b *MockBackend) WorkerCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.workers)
}

func (b *MockBackend) HasWorker(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.workers[id]
	return ok
}

// SetWSHandler configures a WebSocket handler for new mock workers.
// When set, the handler is registered on each mock worker's httptest
// server, allowing WebSocket integration tests.
func (b *MockBackend) SetWSHandler(h http.HandlerFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wsHandler = h
}

func (b *MockBackend) Spawn(_ context.Context, spec backend.WorkerSpec) error {
	b.mu.Lock()
	handler := b.wsHandler
	b.mu.Unlock()

	var srv *httptest.Server
	if handler != nil {
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Upgrade") == "websocket" {
				handler(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	} else {
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.workers[spec.WorkerID] = &mockWorker{
		id:     spec.WorkerID,
		spec:   spec,
		server: srv,
	}
	return nil
}

// GetWorkerURL returns the httptest server URL for a worker (for testing).
func (b *MockBackend) GetWorkerURL(id string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	w, ok := b.workers[id]
	if !ok {
		return ""
	}
	return w.server.URL
}

func (b *MockBackend) Stop(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.workers[id]
	if !ok {
		return fmt.Errorf("worker %q not found", id)
	}
	w.server.Close()
	delete(b.workers, id)
	return nil
}

func (b *MockBackend) HealthCheck(_ context.Context, id string) bool {
	b.mu.RLock()
	_, ok := b.workers[id]
	b.mu.RUnlock()
	if !ok {
		return false
	}
	return b.HealthOK.Load()
}

func (b *MockBackend) Logs(_ context.Context, _ string) (backend.LogStream, error) {
	ch := make(chan string)
	close(ch)
	return backend.LogStream{Lines: ch, Close: func() {}}, nil
}

func (b *MockBackend) Addr(_ context.Context, id string) (string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	w, ok := b.workers[id]
	if !ok {
		return "", fmt.Errorf("worker %q not found", id)
	}
	return w.server.Listener.Addr().String(), nil
}

func (b *MockBackend) Build(_ context.Context, _ backend.BuildSpec) (backend.BuildResult, error) {
	if b.BuildSuccess.Load() {
		return backend.BuildResult{Success: true, ExitCode: 0}, nil
	}
	return backend.BuildResult{Success: false, ExitCode: 1}, nil
}

func (b *MockBackend) ListManaged(_ context.Context) ([]backend.ManagedResource, error) {
	return nil, nil
}

func (b *MockBackend) RemoveResource(_ context.Context, _ backend.ManagedResource) error {
	return nil
}
