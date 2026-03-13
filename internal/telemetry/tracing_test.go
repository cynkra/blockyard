package telemetry

import (
	"context"
	"testing"
)

func TestInitTracingEmpty(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function")
	}
	// No-op shutdown should succeed
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown error: %v", err)
	}
}

func TestTracingMiddleware(t *testing.T) {
	mw := TracingMiddleware()
	if mw == nil {
		t.Fatal("expected non-nil middleware")
	}
}
