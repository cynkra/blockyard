package errorlog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreRingBuffer(t *testing.T) {
	s := NewStore(3)
	for i := 0; i < 5; i++ {
		s.Append(Entry{Message: string(rune('a' + i))})
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}
	snap := s.Snapshot()
	// Newest-first: we appended a, b, c, d, e → store keeps c, d, e;
	// snapshot order should be e, d, c.
	want := []string{"e", "d", "c"}
	if len(snap) != len(want) {
		t.Fatalf("snapshot len = %d, want %d", len(snap), len(want))
	}
	for i, w := range want {
		if snap[i].Message != w {
			t.Errorf("snap[%d] = %q, want %q", i, snap[i].Message, w)
		}
	}
}

func TestStoreEmpty(t *testing.T) {
	s := NewStore(4)
	if got := s.Len(); got != 0 {
		t.Errorf("len = %d, want 0", got)
	}
	if snap := s.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot = %v, want empty", snap)
	}
}

func TestStoreDefaultCapacity(t *testing.T) {
	s := NewStore(0)
	if s.Cap() != DefaultCapacity {
		t.Errorf("cap = %d, want %d", s.Cap(), DefaultCapacity)
	}
}

func TestStoreNilSafe(t *testing.T) {
	var s *Store
	s.Append(Entry{Message: "x"}) // must not panic
	if got := s.Len(); got != 0 {
		t.Errorf("nil len = %d, want 0", got)
	}
	if snap := s.Snapshot(); snap != nil {
		t.Errorf("nil snapshot = %v, want nil", snap)
	}
}

func TestStoreConcurrent(t *testing.T) {
	s := NewStore(128)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Append(Entry{Message: "x"})
			}
		}()
	}
	wg.Wait()
	if got := s.Len(); got != 128 {
		t.Errorf("len after concurrent writes = %d, want 128", got)
	}
}

func TestHandlerCapturesWarnAndAbove(t *testing.T) {
	store := NewStore(10)
	var buf bytes.Buffer
	delegate := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := NewHandler(delegate, store, slog.LevelWarn)
	logger := slog.New(h)

	logger.Info("info msg")
	logger.Warn("warn msg", "key", "val")
	logger.Error("error msg", "err", "boom")

	snap := store.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (info should be skipped)", len(snap))
	}
	// Newest-first: error then warn.
	if snap[0].Level != slog.LevelError || snap[0].Message != "error msg" {
		t.Errorf("snap[0] = %+v, want error msg", snap[0])
	}
	if snap[1].Level != slog.LevelWarn || snap[1].Message != "warn msg" {
		t.Errorf("snap[1] = %+v, want warn msg", snap[1])
	}
	if !strings.Contains(buf.String(), "info msg") {
		t.Error("delegate did not receive info record")
	}
}

func TestHandlerCapturesWhenDelegateDisabled(t *testing.T) {
	// Delegate is configured at ERROR; the handler should still capture
	// WARN into the ring buffer.
	store := NewStore(10)
	var buf bytes.Buffer
	delegate := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	h := NewHandler(delegate, store, slog.LevelWarn)
	logger := slog.New(h)

	logger.Warn("hidden from stderr")

	if got := store.Len(); got != 1 {
		t.Fatalf("store len = %d, want 1", got)
	}
	if strings.Contains(buf.String(), "hidden from stderr") {
		t.Error("delegate should not have written the WARN record")
	}
}

func TestHandlerFlattensAttrsAndGroups(t *testing.T) {
	store := NewStore(10)
	h := NewHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil), store, slog.LevelWarn)
	logger := slog.New(h).With("scope", "test").WithGroup("req").With("id", "abc")

	logger.Warn("boom", "code", 42)

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	got := attrMap(snap[0].Attrs)
	// "scope" was added at top level before the group, so it stays flat.
	if got["scope"] != "test" {
		t.Errorf("scope = %q, want \"test\"", got["scope"])
	}
	// "id" was added inside the req group; it should be prefixed.
	if got["req.id"] != "abc" {
		t.Errorf("req.id = %q, want \"abc\"", got["req.id"])
	}
	// Record-time attr should also be prefixed since the handler is in a group.
	if got["req.code"] != "42" {
		t.Errorf("req.code = %q, want \"42\"", got["req.code"])
	}
}

func TestHandlerPreservesTime(t *testing.T) {
	store := NewStore(4)
	h := NewHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil), store, slog.LevelWarn)

	before := time.Now()
	slog.New(h).Warn("t")
	after := time.Now()

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("len = %d", len(snap))
	}
	if snap[0].Time.Before(before) || snap[0].Time.After(after) {
		t.Errorf("entry time %v not within [%v, %v]", snap[0].Time, before, after)
	}
}

func TestHandlerEnabled(t *testing.T) {
	delegate := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})
	h := NewHandler(delegate, NewStore(4), slog.LevelWarn)
	ctx := context.Background()
	if !h.Enabled(ctx, slog.LevelWarn) {
		t.Error("Enabled(WARN) should be true because capture level is WARN")
	}
	if !h.Enabled(ctx, slog.LevelError) {
		t.Error("Enabled(ERROR) should be true")
	}
	if h.Enabled(ctx, slog.LevelInfo) {
		t.Error("Enabled(INFO) should be false when both delegate and capture reject it")
	}
}

func attrMap(attrs []Attr) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a.Value
	}
	return m
}
