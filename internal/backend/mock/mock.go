package mock

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
	"github.com/cynkra/blockyard/internal/preflight"
)

type MockBackend struct {
	mu               sync.Mutex
	workers          map[string]*mockWorker
	managedResources []backend.ManagedResource
	logLines         []string
	HealthOK         atomic.Bool      // configurable: default true
	BuildSuccess     atomic.Bool      // configurable: default true
	SpawnFails       atomic.Bool      // configurable: default false
	AddrFails        atomic.Bool      // configurable: default false
	wsHandler        http.HandlerFunc // optional WS handler for mock workers
	httpHandler      http.HandlerFunc // optional HTTP handler for mock workers
	BuildFn          func(context.Context, backend.BuildSpec) (backend.BuildResult, error) // optional build callback
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
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.workers)
}

func (b *MockBackend) HasWorker(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
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

// SetHTTPHandler configures an HTTP handler for new mock workers.
// When set, non-WebSocket requests are routed to this handler instead
// of the default 200 OK response.
func (b *MockBackend) SetHTTPHandler(h http.HandlerFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.httpHandler = h
}

// SetManagedResources configures what ListManaged returns.
// RemoveResource removes individual entries from this list.
func (b *MockBackend) SetManagedResources(resources []backend.ManagedResource) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.managedResources = make([]backend.ManagedResource, len(resources))
	copy(b.managedResources, resources)
}

// SetLogLines configures what Logs() emits for any worker.
func (b *MockBackend) SetLogLines(lines []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logLines = make([]string, len(lines))
	copy(b.logLines, lines)
}

func (b *MockBackend) Spawn(_ context.Context, spec backend.WorkerSpec) error {
	if b.SpawnFails.Load() {
		return fmt.Errorf("mock: spawn failed for worker %q", spec.WorkerID)
	}

	b.mu.Lock()
	wsH := b.wsHandler
	httpH := b.httpHandler
	b.mu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" && wsH != nil {
			wsH(w, r)
			return
		}
		if httpH != nil {
			httpH(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

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
	b.mu.Lock()
	defer b.mu.Unlock()
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
	b.mu.Lock()
	_, ok := b.workers[id]
	b.mu.Unlock()
	if !ok {
		return false
	}
	return b.HealthOK.Load()
}

func (b *MockBackend) Logs(_ context.Context, _ string) (backend.LogStream, error) {
	b.mu.Lock()
	lines := make([]string, len(b.logLines))
	copy(lines, b.logLines)
	b.mu.Unlock()

	ch := make(chan string, len(lines))
	for _, line := range lines {
		ch <- line
	}
	close(ch)
	return backend.LogStream{Lines: ch, Close: func() {}}, nil
}

func (b *MockBackend) Addr(_ context.Context, id string) (string, error) {
	if b.AddrFails.Load() {
		return "", fmt.Errorf("mock: addr resolution failed for worker %q", id)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.workers[id]
	if !ok {
		return "", fmt.Errorf("worker %q not found", id)
	}
	return w.server.Listener.Addr().String(), nil
}

func (b *MockBackend) Build(ctx context.Context, spec backend.BuildSpec) (backend.BuildResult, error) {
	if b.BuildFn != nil {
		return b.BuildFn(ctx, spec)
	}
	if b.BuildSuccess.Load() {
		return backend.BuildResult{Success: true, ExitCode: 0}, nil
	}
	return backend.BuildResult{Success: false, ExitCode: 1, Logs: "mock build failure"}, nil
}

func (b *MockBackend) ListManaged(_ context.Context) ([]backend.ManagedResource, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]backend.ManagedResource, len(b.managedResources))
	copy(result, b.managedResources)
	return result, nil
}

func (b *MockBackend) WorkerResourceUsage(_ context.Context, _ string) (*backend.WorkerResourceUsageResult, error) {
	return &backend.WorkerResourceUsageResult{}, nil
}

// CleanupOrphanResources is a no-op for the mock backend.
func (b *MockBackend) CleanupOrphanResources(_ context.Context) error {
	return nil
}

// Preflight returns an empty success report for the mock backend.
func (b *MockBackend) Preflight(_ context.Context) (*preflight.Report, error) {
	return &preflight.Report{RanAt: time.Now().UTC()}, nil
}

func (b *MockBackend) UpdateResources(_ context.Context, id string, mem int64, nanoCPUs int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.workers[id]
	if !ok {
		return fmt.Errorf("worker %s not found", id)
	}
	if mem > 0 {
		w.spec.MemoryLimit = fmt.Sprintf("%dm", mem/1024/1024)
	}
	if nanoCPUs > 0 {
		w.spec.CPULimit = float64(nanoCPUs) / 1e9
	}
	return nil
}

func (b *MockBackend) RemoveResource(_ context.Context, r backend.ManagedResource) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, res := range b.managedResources {
		if res.ID == r.ID && res.Kind == r.Kind {
			b.managedResources = append(b.managedResources[:i], b.managedResources[i+1:]...)
			return nil
		}
	}
	return nil
}
