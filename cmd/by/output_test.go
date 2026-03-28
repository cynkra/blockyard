package main

import "testing"

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
