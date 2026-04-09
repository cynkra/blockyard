package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEmitWritesEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	l := New(path, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx, path)
		close(done)
	}()

	l.Emit(Entry{
		Action: ActionAppCreate,
		Actor:  "user1",
		Target: "app-123",
		Detail: map[string]any{"name": "my-app"},
	})

	// Give the background writer time to flush
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		t.Fatal("expected at least one line in audit log")
	}

	var entry Entry
	if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if entry.Action != ActionAppCreate {
		t.Errorf("action = %q, want %q", entry.Action, ActionAppCreate)
	}
	if entry.Actor != "user1" {
		t.Errorf("actor = %q, want %q", entry.Actor, "user1")
	}
	if entry.Target != "app-123" {
		t.Errorf("target = %q, want %q", entry.Target, "app-123")
	}
	if entry.Timestamp == "" {
		t.Error("expected timestamp to be populated")
	}
}

func TestEmitNilLogger(t *testing.T) {
	var l *Log
	// Should not panic
	l.Emit(Entry{Action: ActionAppCreate, Actor: "test"})
}

func TestRunDrainsOnCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	l := New(path, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx, path)
		close(done)
	}()

	// Write multiple entries
	for i := 0; i < 5; i++ {
		l.Emit(Entry{Action: ActionAppCreate, Actor: "user1"})
	}

	// Cancel immediately — Run should drain
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", count+1, err)
		}
		count++
	}
	if count != 5 {
		t.Errorf("expected 5 entries, got %d", count)
	}
}

func TestBufferFullDropsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	l := New(path, nil)
	// Don't start Run — buffer will fill up

	for i := 0; i < bufferSize+10; i++ {
		l.Emit(Entry{Action: ActionAppCreate, Actor: "user1"})
	}

	// The last 10 should have been dropped (buffer is full)
	if len(l.entries) != bufferSize {
		t.Errorf("expected buffer size %d, got %d", bufferSize, len(l.entries))
	}
}

func TestNewEmptyPathReturnsNil(t *testing.T) {
	l := New("", nil)
	if l != nil {
		t.Error("expected nil for empty path")
	}
}

func TestRunNilLogBlocksUntilCancel(t *testing.T) {
	var l *Log
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx, "ignored")
		close(done)
	}()
	cancel()
	<-done // should return promptly
}

func TestRunInvalidPath(t *testing.T) {
	l := New("/tmp/audit-test-path", nil)
	// Use a path under a non-existent directory to trigger OpenFile error.
	done := make(chan struct{})
	go func() {
		l.Run(context.Background(), "/nonexistent-dir/sub/audit.jsonl")
		close(done)
	}()
	// Run should return quickly due to the open error.
	<-done
}

func TestJSONLinesFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	l := New(path, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx, path)
		close(done)
	}()

	l.Emit(Entry{Action: ActionAppCreate, Actor: "a"})
	l.Emit(Entry{Action: ActionAppDelete, Actor: "b", SourceIP: "127.0.0.1"})

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	lineCount := 0
	f, _ := os.Open(path)
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if !json.Valid(s.Bytes()) {
			t.Errorf("line %d is not valid JSON: %s", lineCount+1, s.Text())
		}
		lineCount++
	}
	if lineCount != 2 {
		t.Errorf("expected 2 lines, got %d", lineCount)
	}
}
