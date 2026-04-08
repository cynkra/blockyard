package process

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/cynkra/blockyard/internal/backend"
)

// waitLogBufferClosed polls the buffer's closed flag until ingest
// signals EOF or the deadline elapses.
func waitLogBufferClosed(t *testing.T, lb *logBuffer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lb.mu.Lock()
		closed := lb.closed
		lb.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for logBuffer.closed")
}

func TestLogBuffer(t *testing.T) {
	lb := newLogBuffer(100)
	r, w := io.Pipe()

	go lb.ingest(r)
	fmt.Fprintln(w, "line 1")
	fmt.Fprintln(w, "line 2")
	w.Close()
	waitLogBufferClosed(t, lb)

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
	waitLogBufferClosed(t, lb)

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

// TestNewLogBufferClampsNonPositive guards against a zero-sized
// ring: ingest's `seq % size` would divide by zero on the first
// line. A zero would not panic the construction itself but would
// crash the first ingest call, so we spawn an ingest to confirm.
func TestNewLogBufferClampsNonPositive(t *testing.T) {
	for _, n := range []int{-1, 0} {
		lb := newLogBuffer(n)
		if lb.size == 0 || len(lb.buf) == 0 {
			t.Errorf("newLogBuffer(%d): size=%d len(buf)=%d, want >=1", n, lb.size, len(lb.buf))
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ingest panicked for newLogBuffer(%d): %v", n, r)
				}
			}()
			r, w := io.Pipe()
			done := make(chan struct{})
			go func() { lb.ingest(r); close(done) }()
			fmt.Fprintln(w, "canary")
			w.Close()
			<-done
		}()
	}
}

// TestLogBufferDoubleCloseIsSafe — stream Close() uses defer recover
// to swallow the double-close panic; exercise that path.
func TestLogBufferDoubleCloseIsSafe(t *testing.T) {
	lb := newLogBuffer(8)
	stream := lb.stream()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("double Close panicked: %v", r)
		}
	}()
	stream.Close()
	stream.Close()
}

// TestLogBufferMultipleSubscribers — a single shared notify channel
// would drop wakeups for all but one reader, so broadcast must
// signal each subscriber's own channel.
func TestLogBufferMultipleSubscribers(t *testing.T) {
	lb := newLogBuffer(100)
	r, w := io.Pipe()
	go lb.ingest(r)

	s1 := lb.stream()
	defer s1.Close()
	s2 := lb.stream()
	defer s2.Close()

	go func() {
		fmt.Fprintln(w, "alpha")
		fmt.Fprintln(w, "beta")
		w.Close()
	}()

	collect := func(s backend.LogStream) []string {
		var got []string
		deadline := time.After(2 * time.Second)
		for len(got) < 2 {
			select {
			case line, ok := <-s.Lines:
				if !ok {
					return got
				}
				got = append(got, line)
			case <-deadline:
				return got
			}
		}
		return got
	}
	g1 := collect(s1)
	g2 := collect(s2)
	if len(g1) != 2 || g1[0] != "alpha" || g1[1] != "beta" {
		t.Errorf("subscriber 1 saw %v", g1)
	}
	if len(g2) != 2 || g2[0] != "alpha" || g2[1] != "beta" {
		t.Errorf("subscriber 2 saw %v", g2)
	}
}
