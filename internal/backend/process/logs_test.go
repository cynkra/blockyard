package process

import (
	"fmt"
	"io"
	"testing"
	"time"
)

func TestLogBuffer(t *testing.T) {
	lb := newLogBuffer(100)
	r, w := io.Pipe()

	go lb.ingest(r)
	fmt.Fprintln(w, "line 1")
	fmt.Fprintln(w, "line 2")
	w.Close()

	// Wait for ingest goroutine to mark the buffer closed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lb.mu.Lock()
		closed := lb.closed
		lb.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	stream := lb.stream()
	defer stream.Close()

	var lines []string
	for line := range stream.Lines {
		lines = append(lines, line)
	}
	if len(lines) != 2 || lines[0] != "line 1" || lines[1] != "line 2" {
		t.Errorf("unexpected lines: %v", lines)
	}
}

func TestLogBufferRingOverflow(t *testing.T) {
	lb := newLogBuffer(3)
	r, w := io.Pipe()

	go lb.ingest(r)
	for i := range 10 {
		fmt.Fprintf(w, "line %d\n", i)
	}
	w.Close()

	// Wait for ingest to finish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lb.mu.Lock()
		closed := lb.closed
		lb.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	stream := lb.stream()
	defer stream.Close()

	var lines []string
	for line := range stream.Lines {
		lines = append(lines, line)
	}
	// Only the last 3 lines should be in the buffer.
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d: %v", len(lines), lines)
	}
}

func TestLogBufferLiveStream(t *testing.T) {
	lb := newLogBuffer(100)
	r, w := io.Pipe()

	go lb.ingest(r)

	stream := lb.stream()
	defer stream.Close()

	// Write before any data exists.
	go func() {
		fmt.Fprintln(w, "first")
		fmt.Fprintln(w, "second")
		w.Close()
	}()

	// Read with a short deadline to avoid hanging on bug.
	got := make([]string, 0, 2)
	timeout := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case line, ok := <-stream.Lines:
			if !ok {
				t.Fatalf("stream closed early; got %v", got)
			}
			got = append(got, line)
		case <-timeout:
			t.Fatalf("timeout waiting for lines, got %v", got)
		}
	}
	if got[0] != "first" || got[1] != "second" {
		t.Errorf("unexpected lines: %v", got)
	}
}
