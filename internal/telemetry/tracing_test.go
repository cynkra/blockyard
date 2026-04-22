package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestInitTracingEmpty(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
		return
	}
	// No-op shutdown should succeed
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestInitTracingWithEndpoint(t *testing.T) {
	// Use a local listener that accepts gRPC connections.
	// We just need it to not refuse the connection; the exporter
	// creation is async and won't fail on an unreachable endpoint.
	// Using localhost:0 won't actually bind, but the OTLP exporter
	// initializes lazily, so NewTracerProvider succeeds even if
	// the endpoint is not reachable.
	shutdown, err := InitTracing(context.Background(), "localhost:4317")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
		return
	}
	// Shutdown should succeed (flushes empty batch).
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestInitTracingSetsPropagator(t *testing.T) {
	// Propagator must be set even with no endpoint, so the proxy can
	// still forward inbound traceparent to workers in deployments
	// where blockyard itself is not exporting traces.
	shutdown, err := InitTracing(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer shutdown(context.Background())

	fields := otel.GetTextMapPropagator().Fields()
	var sawTraceparent bool
	for _, f := range fields {
		if f == "traceparent" {
			sawTraceparent = true
		}
	}
	if !sawTraceparent {
		t.Errorf("expected propagator to handle traceparent, fields=%v", fields)
	}
	// Ensure the installed propagator is actually a composite including
	// the W3C trace-context propagator (not just a no-op).
	if _, ok := otel.GetTextMapPropagator().(propagation.TextMapPropagator); !ok {
		t.Error("expected a TextMapPropagator to be installed")
	}
}

func TestTracingMiddleware(t *testing.T) {
	mw := TracingMiddleware()
	if mw == nil {
		t.Fatal("expected non-nil middleware")
	}
}

func TestTracingMiddlewareHandlesRequest(t *testing.T) {
	// Initialize a no-op tracer (empty endpoint) to ensure the global
	// tracer provider is set.
	shutdown, _ := InitTracing(context.Background(), "")
	defer shutdown(context.Background())

	mw := TracingMiddleware()

	// Create a chi router so RouteContext is available.
	r := chi.NewRouter()
	r.Use(mw)
	r.Get("/test/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	req := httptest.NewRequest("GET", "/test/123", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rec.Body.String())
	}
}

func TestTracingMiddlewareRecordsStatusCode(t *testing.T) {
	shutdown, _ := InitTracing(context.Background(), "")
	defer shutdown(context.Background())

	mw := TracingMiddleware()

	r := chi.NewRouter()
	r.Use(mw)
	r.Get("/error", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	})

	req := httptest.NewRequest("GET", "/error", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestStatusWriterWriteHeaderIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: 200}

	sw.WriteHeader(http.StatusNotFound)
	if sw.status != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", sw.status)
	}
	if !sw.wroteHeader {
		t.Error("expected wroteHeader to be true")
	}

	// Second call should not change the stored status.
	sw.WriteHeader(http.StatusOK)
	if sw.status != http.StatusNotFound {
		t.Errorf("expected status to remain 404, got %d", sw.status)
	}
}

func TestTracingMiddlewarePreservesWebSocketUpgrade(t *testing.T) {
	// Regression test for #246: the tracing middleware must not hide
	// the http.Hijacker interface, otherwise WebSocket upgrades fail.
	shutdown, _ := InitTracing(context.Background(), "")
	defer shutdown(context.Background())

	mw := TracingMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("websocket.Accept: %v", err)
			return
		}
		c.Close(websocket.StatusNormalClosure, "")
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")
}

func TestTracingMiddlewareWithoutRouteContext(t *testing.T) {
	// Test that the middleware works even without chi's RouteContext
	// (i.e., when used with a plain http.ServeMux).
	shutdown, _ := InitTracing(context.Background(), "")
	defer shutdown(context.Background())

	mw := TracingMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/plain", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
