package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string: got %q", got)
	}
	if got := truncate("hello world", 8); got != "hello..." {
		t.Errorf("long string: got %q", got)
	}
}

func TestDerefStr(t *testing.T) {
	s := "hello"
	if got := derefStr(&s, "default"); got != "hello" {
		t.Errorf("non-nil: got %q", got)
	}
	if got := derefStr(nil, "default"); got != "default" {
		t.Errorf("nil: got %q", got)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{0, "0 B"},
		{1024 * 1024, "1 MiB"},
		{256 * 1024 * 1024, "256 MiB"},
		{2 * 1024 * 1024 * 1024, "2.0 GiB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildQuery(t *testing.T) {
	got := buildQuery("/api/v1/apps", map[string]string{
		"search": "hello",
		"empty":  "",
	})
	if got != "/api/v1/apps?search=hello" {
		t.Errorf("got %q", got)
	}

	got = buildQuery("/api/v1/apps", map[string]string{})
	if got != "/api/v1/apps" {
		t.Errorf("empty params: got %q", got)
	}
}

func TestJoinOr(t *testing.T) {
	if got := joinOr(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := joinOr([]string{"a"}); got != "a" {
		t.Errorf("single: got %q", got)
	}
	if got := joinOr([]string{"a", "b", "c"}); got != "a, b or c" {
		t.Errorf("multi: got %q", got)
	}
}

func TestDerefFloat(t *testing.T) {
	v := 3.14
	if got := derefFloat(&v, 0); got != "3.1" {
		t.Errorf("non-nil: got %q", got)
	}
	if got := derefFloat(nil, 1.5); got != "1.5" {
		t.Errorf("nil: got %q", got)
	}
}

// captureStdout redirects os.Stdout, runs fn, and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestPrintJSON(t *testing.T) {
	got := captureStdout(t, func() {
		printJSON(map[string]int{"count": 42})
	})
	if !strings.Contains(got, `"count": 42`) {
		t.Errorf("expected indented JSON, got %q", got)
	}
}

func TestPrintRawJSON_ValidJSON(t *testing.T) {
	got := captureStdout(t, func() {
		printRawJSON([]byte(`{"ok":true}`))
	})
	// Should be pretty-printed (indented).
	if !strings.Contains(got, "  ") {
		t.Errorf("expected pretty-printed JSON, got %q", got)
	}
}

func TestPrintRawJSON_InvalidData(t *testing.T) {
	got := captureStdout(t, func() {
		printRawJSON([]byte("not json at all"))
	})
	if !strings.Contains(got, "not json at all") {
		t.Errorf("expected raw passthrough, got %q", got)
	}
}

func TestPrintKeyValue(t *testing.T) {
	got := captureStdout(t, func() {
		printKeyValue([][2]string{
			{"Name", "myapp"},
			{"Mode", "shiny"},
		})
	})
	if !strings.Contains(got, "Name:") || !strings.Contains(got, "myapp") {
		t.Errorf("expected key-value output, got %q", got)
	}
	if !strings.Contains(got, "Mode:") || !strings.Contains(got, "shiny") {
		t.Errorf("expected key-value output, got %q", got)
	}
}

func TestStreamResponse(t *testing.T) {
	input := io.NopCloser(strings.NewReader("line1\nline2\n"))
	var buf bytes.Buffer
	if err := streamResponse(input, &buf); err != nil {
		t.Fatalf("streamResponse: %v", err)
	}
	if buf.String() != "line1\nline2\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestStreamResponse_Error(t *testing.T) {
	// errReader returns an error after the first read.
	r := &errAfterNReader{data: []byte("partial"), n: 1}
	var buf bytes.Buffer
	err := streamResponse(io.NopCloser(r), &buf)
	if err == nil {
		t.Fatal("expected error")
	}
}

// errAfterNReader returns data on the first read, then errors.
type errAfterNReader struct {
	data []byte
	n    int
	call int
}

func (r *errAfterNReader) Read(p []byte) (int, error) {
	r.call++
	if r.call <= r.n {
		n := copy(p, r.data)
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}
