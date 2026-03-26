package server

import (
	"testing"
)

func TestLastLines_FewerLines(t *testing.T) {
	input := "line1\nline2"
	got := lastLines(input, 5)
	if got != "line1\nline2" {
		t.Errorf("lastLines(%q, 5) = %q, want %q", input, got, "line1\nline2")
	}
}

func TestLastLines_ExactN(t *testing.T) {
	input := "a\nb\nc"
	got := lastLines(input, 3)
	if got != "a\nb\nc" {
		t.Errorf("lastLines(%q, 3) = %q, want %q", input, got, "a\nb\nc")
	}
}

func TestLastLines_MoreThanN(t *testing.T) {
	input := "a\nb\nc\nd\ne"
	got := lastLines(input, 2)
	if got != "d\ne" {
		t.Errorf("lastLines(%q, 2) = %q, want %q", input, got, "d\ne")
	}
}

func TestLastLines_Empty(t *testing.T) {
	got := lastLines("", 3)
	if got != "" {
		t.Errorf("lastLines(%q, 3) = %q, want %q", "", got, "")
	}
}

func TestLastLines_SingleLine(t *testing.T) {
	got := lastLines("hello", 1)
	if got != "hello" {
		t.Errorf("lastLines(%q, 1) = %q, want %q", "hello", got, "hello")
	}
}

func TestLastLines_ZeroN(t *testing.T) {
	got := lastLines("a\nb\nc", 0)
	if got != "" {
		t.Errorf("lastLines(%q, 0) = %q, want %q", "a\nb\nc", got, "")
	}
}

func TestLastLines_TrailingNewline(t *testing.T) {
	input := "a\nb\nc\n"
	got := lastLines(input, 2)
	// The trailing newline creates an empty final "line"
	if got == "" {
		t.Error("expected non-empty result for trailing newline input")
	}
}
