package pkgstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// RecoverPlatform
// ---------------------------------------------------------------------------

func TestRecoverPlatform(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "4.5-x86_64-pc-linux-gnu"), 0o755)
	// Add a hidden dir and a regular file that should be skipped.
	os.MkdirAll(filepath.Join(root, ".locks"), 0o755)
	os.WriteFile(filepath.Join(root, "README"), []byte("hi"), 0o644)

	got := RecoverPlatform(root)
	if got != "4.5-x86_64-pc-linux-gnu" {
		t.Errorf("RecoverPlatform = %q, want 4.5-x86_64-pc-linux-gnu", got)
	}
}

func TestRecoverPlatform_Empty(t *testing.T) {
	root := t.TempDir()
	if got := RecoverPlatform(root); got != "" {
		t.Errorf("RecoverPlatform on empty dir = %q, want empty", got)
	}
}

func TestRecoverPlatform_Nonexistent(t *testing.T) {
	if got := RecoverPlatform("/no/such/path"); got != "" {
		t.Errorf("RecoverPlatform on nonexistent path = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// hardlinkDir
// ---------------------------------------------------------------------------

func TestHardlinkDir(t *testing.T) {
	srcLib := t.TempDir()
	destLib := t.TempDir()

	// Create a package directory with a file.
	pkgDir := filepath.Join(srcLib, "shiny")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "DESCRIPTION"), []byte("Package: shiny\n"), 0o644)
	os.MkdirAll(filepath.Join(pkgDir, "R"), 0o755)
	os.WriteFile(filepath.Join(pkgDir, "R", "app.R"), []byte("# code"), 0o644)

	if err := hardlinkDir(srcLib, "shiny", destLib); err != nil {
		t.Fatal(err)
	}

	// Verify files exist in destination.
	desc := filepath.Join(destLib, "shiny", "DESCRIPTION")
	if _, err := os.Stat(desc); err != nil {
		t.Errorf("DESCRIPTION not found in dest: %v", err)
	}
	appR := filepath.Join(destLib, "shiny", "R", "app.R")
	if _, err := os.Stat(appR); err != nil {
		t.Errorf("R/app.R not found in dest: %v", err)
	}

	// Verify it's a hardlink (same inode).
	srcInfo, _ := os.Stat(filepath.Join(pkgDir, "DESCRIPTION"))
	destInfo, _ := os.Stat(desc)
	if !os.SameFile(srcInfo, destInfo) {
		t.Error("destination file is not a hardlink to source")
	}
}

// ---------------------------------------------------------------------------
// PopulateBuild
// ---------------------------------------------------------------------------

func TestPopulateBuild_CacheHit(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	lf := &Lockfile{
		LockfileVersion: 1,
		Packages: []LockfileEntry{{
			Package:  "rlang",
			Version:  "1.1.0",
			Type:     "standard",
			RVersion: "4.5",
			Platform: "x86_64-pc-linux-gnu",
		}},
	}

	// Compute the store key for this entry.
	sourceHash, err := StoreKey(lf.Packages[0])
	if err != nil {
		t.Fatal(err)
	}
	// The config hash for no LinkingTo is ConfigHash(nil).
	configHash := ConfigHash(nil)

	// Seed the store: create the package dir and a configs.json.
	pkgPath := s.Path("rlang", sourceHash, configHash)
	os.MkdirAll(pkgPath, 0o755)
	os.WriteFile(filepath.Join(pkgPath, "DESCRIPTION"),
		[]byte("Package: rlang\nVersion: 1.1.0\n"), 0o644)

	// Write configs.json so ResolveConfig can find it.
	sc := StoreConfigs{
		Configs: map[string]map[string]string{
			configHash: {},
		},
	}
	if err := WriteStoreConfigs(s.ConfigsPath("rlang", sourceHash), sc); err != nil {
		t.Fatal(err)
	}

	// Write config sidecar so Touch doesn't fail.
	WriteConfigMeta(s.ConfigMetaPath("rlang", sourceHash, configHash),
		ConfigMeta{})

	stats, err := s.PopulateBuild(lf, lib, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 1 {
		t.Errorf("Hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 0 {
		t.Errorf("Misses = %d, want 0", stats.Misses)
	}

	// Verify the package was hardlinked into lib.
	if _, err := os.Stat(filepath.Join(lib, "rlang", "DESCRIPTION")); err != nil {
		t.Error("package not populated into lib")
	}
}

func TestPopulateBuild_CacheMiss(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	lf := &Lockfile{
		LockfileVersion: 1,
		Packages: []LockfileEntry{{
			Package:  "rlang",
			Version:  "1.1.0",
			Type:     "standard",
			RVersion: "4.5",
			Platform: "x86_64-pc-linux-gnu",
		}},
	}

	stats, err := s.PopulateBuild(lf, lib, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Misses != 1 {
		t.Errorf("Misses = %d, want 1", stats.Misses)
	}
	if stats.Hits != 0 {
		t.Errorf("Hits = %d, want 0", stats.Hits)
	}
}

func TestPopulateBuild_SkipsExisting(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	lf := &Lockfile{
		LockfileVersion: 1,
		Packages: []LockfileEntry{{
			Package:  "rlang",
			Version:  "1.1.0",
			Type:     "standard",
			RVersion: "4.5",
			Platform: "x86_64-pc-linux-gnu",
		}},
	}

	// Pre-create the package in lib — PopulateBuild should skip it.
	os.MkdirAll(filepath.Join(lib, "rlang"), 0o755)

	stats, err := s.PopulateBuild(lf, lib, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Errorf("expected 0 hits/misses for pre-existing package, got %d/%d",
			stats.Hits, stats.Misses)
	}
}

func TestPopulateBuild_SkipsMetaEntries(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	lf := &Lockfile{
		LockfileVersion: 1,
		Packages: []LockfileEntry{
			{Package: "deps::/app", Type: "deps"},
			{Package: "local::/lib", Type: "local"},
		},
	}

	stats, err := s.PopulateBuild(lf, lib, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 0 || stats.Misses != 0 {
		t.Errorf("expected no hits/misses for meta entries, got %d/%d",
			stats.Hits, stats.Misses)
	}
}

func TestPopulateBuild_SkipsRefManifestMatch(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := LockfileEntry{
		Package:  "rlang",
		Version:  "1.1.0",
		Type:     "standard",
		RVersion: "4.5",
		Platform: "x86_64-pc-linux-gnu",
	}
	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{entry}}

	sourceHash, _ := StoreKey(entry)
	configHash := ConfigHash(nil)

	// Seed the store and configs.json.
	pkgPath := s.Path("rlang", sourceHash, configHash)
	os.MkdirAll(pkgPath, 0o755)
	os.WriteFile(filepath.Join(pkgPath, "DESCRIPTION"),
		[]byte("Package: rlang\n"), 0o644)
	WriteStoreConfigs(s.ConfigsPath("rlang", sourceHash), StoreConfigs{
		Configs: map[string]map[string]string{configHash: {}},
	})

	// Provide a refManifest that already matches — should skip.
	refManifest := map[string]string{
		"rlang": StoreRef(sourceHash, configHash),
	}

	stats, err := s.PopulateBuild(lf, lib, refManifest)
	if err != nil {
		t.Fatal(err)
	}
	// Should skip because refManifest matches. No hits, no misses.
	if stats.Hits != 0 {
		t.Errorf("Hits = %d, want 0 (refManifest should skip)", stats.Hits)
	}
}

// ---------------------------------------------------------------------------
// IngestPackages
// ---------------------------------------------------------------------------

func TestIngestPackages_Basic(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := LockfileEntry{
		Package:  "shiny",
		Version:  "1.9.0",
		Type:     "standard",
		RVersion: "4.5",
		Platform: "x86_64-pc-linux-gnu",
	}
	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{entry}}

	// Create the installed package in lib with a DESCRIPTION.
	pkgDir := filepath.Join(lib, "shiny")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "DESCRIPTION"),
		[]byte("Package: shiny\nVersion: 1.9.0\n"), 0o644)

	manifest, err := s.IngestPackages(context.Background(), lf, lib, nil)
	if err != nil {
		t.Fatal(err)
	}

	if ref, ok := manifest["shiny"]; !ok || ref == "" {
		t.Error("expected shiny in manifest with a non-empty ref")
	}

	// The package should now be in the store (moved from lib).
	sourceHash, _ := StoreKey(entry)
	configHash := ConfigHash(nil)
	if !s.Has("shiny", sourceHash, configHash) {
		t.Error("package not in store after ingest")
	}

	// The original lib dir should have been renamed away.
	if dirExists(pkgDir) {
		t.Error("lib dir should have been renamed into store")
	}
}

func TestIngestPackages_SkipsMetaEntries(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	lf := &Lockfile{
		LockfileVersion: 1,
		Packages: []LockfileEntry{
			{Package: "deps::/app", Type: "deps"},
		},
	}

	manifest, err := s.IngestPackages(context.Background(), lf, lib, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 0 {
		t.Errorf("expected empty manifest for meta-only lockfile, got %v", manifest)
	}
}

func TestIngestPackages_CarryForwardRefManifest(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := LockfileEntry{
		Package:  "shiny",
		Version:  "1.9.0",
		Type:     "standard",
		RVersion: "4.5",
		Platform: "x86_64-pc-linux-gnu",
	}
	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{entry}}

	// shiny is in the lockfile but NOT in lib → should carry from refManifest.
	refManifest := map[string]string{
		"shiny": "abc/def",
		"extra": "111/222", // not in lockfile, should be carried forward
	}

	manifest, err := s.IngestPackages(context.Background(), lf, lib, refManifest)
	if err != nil {
		t.Fatal(err)
	}

	// shiny not in lib → ref carried forward.
	if manifest["shiny"] != "abc/def" {
		t.Errorf("shiny ref = %q, want abc/def", manifest["shiny"])
	}
	// extra not in lockfile → carried forward from refManifest.
	if manifest["extra"] != "111/222" {
		t.Errorf("extra ref = %q, want 111/222", manifest["extra"])
	}
}

func TestIngestPackages_SkipsAlreadyIngested(t *testing.T) {
	root := t.TempDir()
	lib := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := LockfileEntry{
		Package:  "shiny",
		Version:  "1.9.0",
		Type:     "standard",
		RVersion: "4.5",
		Platform: "x86_64-pc-linux-gnu",
	}
	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{entry}}

	sourceHash, _ := StoreKey(entry)
	configHash := ConfigHash(nil)

	// Pre-seed the store with the package.
	pkgPath := s.Path("shiny", sourceHash, configHash)
	os.MkdirAll(pkgPath, 0o755)
	os.WriteFile(filepath.Join(pkgPath, "DESCRIPTION"),
		[]byte("Package: shiny\nVersion: 1.9.0\n"), 0o644)
	WriteStoreConfigs(s.ConfigsPath("shiny", sourceHash), StoreConfigs{
		Configs: map[string]map[string]string{configHash: {}},
	})
	WriteConfigMeta(s.ConfigMetaPath("shiny", sourceHash, configHash),
		ConfigMeta{})

	// Also create the package in lib (with DESCRIPTION for IngestContext).
	libPkg := filepath.Join(lib, "shiny")
	os.MkdirAll(libPkg, 0o755)
	os.WriteFile(filepath.Join(libPkg, "DESCRIPTION"),
		[]byte("Package: shiny\nVersion: 1.9.0\n"), 0o644)

	manifest, err := s.IngestPackages(context.Background(), lf, lib, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := manifest["shiny"]; !ok {
		t.Error("expected shiny in manifest")
	}

	// lib dir should still exist (not moved) since store already had it.
	if !dirExists(libPkg) {
		t.Error("lib dir should remain when store already has the package")
	}
}
