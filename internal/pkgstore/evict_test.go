package pkgstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEvictStale_RemovesExpired(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Create a config entry with old mtime.
	pkg, sh, ch := "shiny", "src1", "cfg1"
	os.MkdirAll(s.Path(pkg, sh, ch), 0o755)
	sidecar := s.ConfigMetaPath(pkg, sh, ch)
	os.WriteFile(sidecar, []byte(`{"created_at":"2020-01-01T00:00:00Z"}`), 0o644)
	// Set mtime to 2 hours ago.
	oldTime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(sidecar, oldTime, oldTime)

	// Write configs.json.
	sc := StoreConfigs{
		Configs: map[string]map[string]string{ch: {}},
	}
	WriteStoreConfigs(s.ConfigsPath(pkg, sh), sc)

	n, err := s.EvictStale(context.Background(), 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 eviction, got %d", n)
	}

	// Config dir and sidecar should be gone.
	if dirExists(s.Path(pkg, sh, ch)) {
		t.Error("config dir still exists")
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Error("sidecar still exists")
	}
}

func TestEvictStale_PreservesFresh(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	pkg, sh, ch := "shiny", "src1", "cfg1"
	os.MkdirAll(s.Path(pkg, sh, ch), 0o755)
	sidecar := s.ConfigMetaPath(pkg, sh, ch)
	os.WriteFile(sidecar, []byte(`{}`), 0o644)
	// mtime is now (fresh).

	sc := StoreConfigs{Configs: map[string]map[string]string{ch: {}}}
	WriteStoreConfigs(s.ConfigsPath(pkg, sh), sc)

	n, _ := s.EvictStale(context.Background(), 1*time.Hour)
	if n != 0 {
		t.Errorf("expected 0 evictions, got %d", n)
	}
	if !dirExists(s.Path(pkg, sh, ch)) {
		t.Error("fresh config should still exist")
	}
}

func TestEvictStale_MixedConfigs(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	pkg, sh := "shiny", "src1"
	ch1, ch2 := "old-cfg", "new-cfg"

	os.MkdirAll(s.Path(pkg, sh, ch1), 0o755)
	os.MkdirAll(s.Path(pkg, sh, ch2), 0o755)

	// Old sidecar.
	sc1 := s.ConfigMetaPath(pkg, sh, ch1)
	os.WriteFile(sc1, []byte(`{}`), 0o644)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(sc1, old, old)

	// Fresh sidecar.
	sc2 := s.ConfigMetaPath(pkg, sh, ch2)
	os.WriteFile(sc2, []byte(`{}`), 0o644)

	// configs.json with both.
	sc := StoreConfigs{
		Configs: map[string]map[string]string{
			ch1: {},
			ch2: {},
		},
	}
	WriteStoreConfigs(s.ConfigsPath(pkg, sh), sc)

	n, err := s.EvictStale(context.Background(), 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 eviction, got %d", n)
	}

	// Old config gone, new config preserved.
	if dirExists(s.Path(pkg, sh, ch1)) {
		t.Error("old config should be evicted")
	}
	if !dirExists(s.Path(pkg, sh, ch2)) {
		t.Error("new config should still exist")
	}

	// configs.json should only have ch2.
	got, _ := ReadStoreConfigs(s.ConfigsPath(pkg, sh))
	if len(got.Configs) != 1 {
		t.Errorf("expected 1 config in configs.json, got %d", len(got.Configs))
	}
	if _, ok := got.Configs[ch2]; !ok {
		t.Error("new config missing from configs.json")
	}
}

func TestEvictStale_CleansEmptyDirs(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	pkg, sh, ch := "shiny", "src1", "cfg1"
	os.MkdirAll(s.Path(pkg, sh, ch), 0o755)
	sidecar := s.ConfigMetaPath(pkg, sh, ch)
	os.WriteFile(sidecar, []byte(`{}`), 0o644)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(sidecar, old, old)

	sc := StoreConfigs{Configs: map[string]map[string]string{ch: {}}}
	WriteStoreConfigs(s.ConfigsPath(pkg, sh), sc)

	s.EvictStale(context.Background(), 1*time.Hour)

	// Source hash dir should be removed (empty after eviction).
	if dirExists(filepath.Join(root, "4.5-x86_64-pc-linux-gnu", pkg, sh)) {
		t.Error("source hash dir should be removed when empty")
	}
	// Package dir should be removed (empty after source hash removal).
	if dirExists(filepath.Join(root, "4.5-x86_64-pc-linux-gnu", pkg)) {
		t.Error("package dir should be removed when empty")
	}
}

func TestEvictStale_DisabledWhenZero(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	n, err := s.EvictStale(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestEvictStale_EmptyStore(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	n, err := s.EvictStale(context.Background(), 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestRemoveIfEmpty(t *testing.T) {
	dir := t.TempDir()
	emptyDir := filepath.Join(dir, "empty")
	os.MkdirAll(emptyDir, 0o755)

	removeIfEmpty(emptyDir)
	if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
		t.Error("empty dir should be removed")
	}

	// Non-empty dir should remain.
	nonEmpty := filepath.Join(dir, "nonempty")
	os.MkdirAll(nonEmpty, 0o755)
	os.WriteFile(filepath.Join(nonEmpty, "file"), []byte("x"), 0o644)

	removeIfEmpty(nonEmpty)
	if _, err := os.Stat(nonEmpty); err != nil {
		t.Error("non-empty dir should remain")
	}
}

func TestSpawnEvictionSweeper_RunsSweep(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Create an expired entry so the sweep has something to evict.
	pkg, sh, ch := "shiny", "src1", "cfg1"
	os.MkdirAll(s.Path(pkg, sh, ch), 0o755)
	sidecar := s.ConfigMetaPath(pkg, sh, ch)
	os.WriteFile(sidecar, []byte(`{}`), 0o644)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(sidecar, old, old)

	sc := StoreConfigs{Configs: map[string]map[string]string{ch: {}}}
	WriteStoreConfigs(s.ConfigsPath(pkg, sh), sc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a very short retention so the sweep fires quickly.
	// retention/2 = interval; retention must be short enough for the
	// ticker to fire before our deadline.
	SpawnEvictionSweeper(ctx, s, 200*time.Millisecond)

	// Wait for the sweep to fire.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for sweeper to evict")
		default:
		}
		if !dirExists(s.Path(pkg, sh, ch)) {
			return // success - entry was evicted
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSpawnEvictionSweeper_DisabledWhenZero(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Should return immediately (no goroutine started).
	SpawnEvictionSweeper(ctx, s, 0)
}
