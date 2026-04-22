package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestForwardHTTPInjectsTraceparent verifies that forwardHTTP injects
// W3C trace context into upstream requests so instrumented Shiny apps
// can continue blockyard's trace.
func TestForwardHTTPInjectsTraceparent(t *testing.T) {
	// Install the W3C propagator and a real tracer provider so spans
	// have a valid trace/span ID to inject.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	tp := sdktrace.NewTracerProvider()
	defer tp.Shutdown(context.Background())
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	var gotTraceparent string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	addr := strings.TrimPrefix(backend.URL, "http://")

	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()
	req := httptest.NewRequest("GET", "/app/myapp/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	forwardHTTP(rec, req, addr, "myapp", "", http.DefaultTransport, 5*time.Second)

	if gotTraceparent == "" {
		t.Fatal("backend did not receive a Traceparent header")
	}
	// A valid W3C traceparent starts with version "00-".
	if !strings.HasPrefix(gotTraceparent, "00-") {
		t.Errorf("unexpected traceparent format: %q", gotTraceparent)
	}
}
