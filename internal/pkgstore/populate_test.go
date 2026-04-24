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
// PopulateRuntime
// ---------------------------------------------------------------------------

// makeEntry is a terse helper for standard lockfile entries used by the
// PopulateRuntime tests.
func makeEntry(pkg, version, sha string) LockfileEntry {
	return LockfileEntry{
		Package:  pkg,
		Version:  version,
		Type:     "standard",
		SHA256:   sha,
		RVersion: "4.5",
		Platform: "x86_64-pc-linux-gnu",
	}
}

// seedStore plants a package tree + configs.json + sidecar into the
// store so ResolveConfig finds it. linkingTo names the declared
// LinkingTo deps; linkingToKeys is the expected source-key map for the
// config hash; emit==true writes the corresponding config entry. When
// emit==false, the tree exists but no config matches — a store miss.
func seedStore(
	t *testing.T, s *Store, pkg, sourceHash string,
	linkingTo []string, linkingToKeys map[string]string, emit bool,
) string {
	t.Helper()
	configHash := ConfigHash(linkingToKeys)
	pkgPath := s.Path(pkg, sourceHash, configHash)
	os.MkdirAll(pkgPath, 0o755)
	os.WriteFile(filepath.Join(pkgPath, "DESCRIPTION"),
		[]byte("Package: "+pkg+"\n"), 0o644)

	configs := StoreConfigs{
		LinkingTo: linkingTo,
		Configs:   make(map[string]map[string]string),
	}
	if emit {
		if linkingToKeys == nil {
			linkingToKeys = map[string]string{}
		}
		configs.Configs[configHash] = linkingToKeys
	}
	if err := WriteStoreConfigs(s.ConfigsPath(pkg, sourceHash), configs); err != nil {
		t.Fatal(err)
	}
	WriteConfigMeta(s.ConfigMetaPath(pkg, sourceHash, configHash), ConfigMeta{})
	return configHash
}

// seedWorkerLib creates a package dir under refLib so hardlinkDir can
// copy it into staging.
func seedWorkerLib(t *testing.T, refLib, pkg string) {
	t.Helper()
	p := filepath.Join(refLib, pkg)
	os.MkdirAll(p, 0o755)
	os.WriteFile(filepath.Join(p, "DESCRIPTION"),
		[]byte("Package: "+pkg+"\n"), 0o644)
}

// TestPopulateRuntime_NoRefManifest_AllNew exercises the fourth phase
// (new/changed packages) with an empty refManifest: every package is
// "new", just like the build-time path. Covers both store-hit and
// store-miss arms.
func TestPopulateRuntime_NoRefManifest_AllNew(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	hit := makeEntry("rlang", "1.1.0", "rlang-sha")
	miss := makeEntry("purrr", "1.0.0", "purrr-sha")
	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{hit, miss}}

	hitKey, _ := StoreKey(hit)
	seedStore(t, s, "rlang", hitKey, nil, nil, true)

	stats, err := s.PopulateRuntime(lf, lib, refLib, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 1 {
		t.Errorf("Hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("Misses = %d, want 1", stats.Misses)
	}
	if _, err := os.Stat(filepath.Join(lib, "rlang", "DESCRIPTION")); err != nil {
		t.Error("hit package not populated into lib")
	}
}

// TestPopulateRuntime_UnchangedNoLinkingTo verifies the "no LinkingTo"
// short-circuit: a package whose source key matches refManifest and
// has no LinkingTo deps is hardlinked directly from refLib.
func TestPopulateRuntime_UnchangedNoLinkingTo(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := makeEntry("rlang", "1.1.0", "rlang-sha")
	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{entry}}

	sourceHash, _ := StoreKey(entry)
	configHash := seedStore(t, s, "rlang", sourceHash, nil, nil, true)
	seedWorkerLib(t, refLib, "rlang")

	refManifest := map[string]string{"rlang": StoreRef(sourceHash, configHash)}

	stats, err := s.PopulateRuntime(lf, lib, refLib, refManifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 0 || stats.Misses != 0 || stats.ABIHits != 0 || stats.ABIRebuilds != 0 {
		t.Errorf("unchanged no-linkingto: stats = %+v, want all zero", stats)
	}
	if _, err := os.Stat(filepath.Join(lib, "rlang", "DESCRIPTION")); err != nil {
		t.Error("package not hardlinked from refLib")
	}
}

// TestPopulateRuntime_UnchangedLinkingToUnchanged covers the branch
// where the package has LinkingTo deps but none of them changed —
// should still hardlink from refLib (no recompile needed).
func TestPopulateRuntime_UnchangedLinkingToUnchanged(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	rcpp := makeEntry("Rcpp", "1.0.12", "rcpp-sha")
	rcppK, _ := StoreKey(rcpp)
	seedStore(t, s, "Rcpp", rcppK, nil, nil, true)

	consumer := makeEntry("s2", "1.1.0", "s2-sha")
	consumerK, _ := StoreKey(consumer)
	// s2 declares LinkingTo Rcpp; the stored config used Rcpp's key.
	seedStore(t, s, "s2", consumerK,
		[]string{"Rcpp"}, map[string]string{"Rcpp": rcppK}, true)
	seedWorkerLib(t, refLib, "Rcpp")
	seedWorkerLib(t, refLib, "s2")

	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{rcpp, consumer}}
	refManifest := map[string]string{
		"Rcpp": StoreRef(rcppK, ConfigHash(nil)),
		"s2":   StoreRef(consumerK, ConfigHash(map[string]string{"Rcpp": rcppK})),
	}

	stats, err := s.PopulateRuntime(lf, lib, refLib, refManifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ABIHits != 0 || stats.ABIRebuilds != 0 {
		t.Errorf("expected no ABI action, got %+v", stats)
	}
}

// TestPopulateRuntime_LinkingToChanged_ABIHit covers: the consumer is
// unchanged but its LinkingTo dep (Rcpp) has a new source key. The
// store has a matching config for the new key combo, so the consumer
// is hardlinked from the store (ABIHit).
func TestPopulateRuntime_LinkingToChanged_ABIHit(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// New Rcpp in the lockfile.
	rcppNew := makeEntry("Rcpp", "1.1.0", "rcpp-new-sha")
	rcppNewK, _ := StoreKey(rcppNew)
	seedStore(t, s, "Rcpp", rcppNewK, nil, nil, true)

	// Consumer's source key is unchanged, but refManifest carries the
	// OLD Rcpp key — so changed[Rcpp]==true.
	rcppOld := makeEntry("Rcpp", "1.0.0", "rcpp-old-sha")
	rcppOldK, _ := StoreKey(rcppOld)

	consumer := makeEntry("s2", "1.1.0", "s2-sha")
	consumerK, _ := StoreKey(consumer)
	// Emit a store config for s2 compiled against the NEW Rcpp key.
	seedStore(t, s, "s2", consumerK,
		[]string{"Rcpp"}, map[string]string{"Rcpp": rcppNewK}, true)

	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{rcppNew, consumer}}
	refManifest := map[string]string{
		"Rcpp": StoreRef(rcppOldK, ConfigHash(nil)),
		"s2":   StoreRef(consumerK, ConfigHash(map[string]string{"Rcpp": rcppOldK})),
	}

	stats, err := s.PopulateRuntime(lf, lib, refLib, refManifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ABIHits != 1 {
		t.Errorf("ABIHits = %d, want 1", stats.ABIHits)
	}
	if stats.ABIRebuilds != 0 {
		t.Errorf("ABIRebuilds = %d, want 0", stats.ABIRebuilds)
	}
	if _, err := os.Stat(filepath.Join(lib, "s2", "DESCRIPTION")); err != nil {
		t.Error("ABI-updated consumer not linked into lib")
	}
}

// TestPopulateRuntime_LinkingToChanged_ABIRebuild covers: consumer's
// LinkingTo dep changed AND the store has no config for the new
// key combo. Consumer is excluded (ABIRebuild) so pak can reinstall.
func TestPopulateRuntime_LinkingToChanged_ABIRebuild(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	rcppNew := makeEntry("Rcpp", "1.1.0", "rcpp-new-sha")
	rcppNewK, _ := StoreKey(rcppNew)
	seedStore(t, s, "Rcpp", rcppNewK, nil, nil, true)

	rcppOld := makeEntry("Rcpp", "1.0.0", "rcpp-old-sha")
	rcppOldK, _ := StoreKey(rcppOld)

	consumer := makeEntry("s2", "1.1.0", "s2-sha")
	consumerK, _ := StoreKey(consumer)
	// Store has a configs.json referencing the OLD Rcpp key only, so
	// the new key combo misses.
	seedStore(t, s, "s2", consumerK,
		[]string{"Rcpp"}, map[string]string{"Rcpp": rcppOldK}, true)

	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{rcppNew, consumer}}
	refManifest := map[string]string{
		"Rcpp": StoreRef(rcppOldK, ConfigHash(nil)),
		"s2":   StoreRef(consumerK, ConfigHash(map[string]string{"Rcpp": rcppOldK})),
	}

	stats, err := s.PopulateRuntime(lf, lib, refLib, refManifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ABIRebuilds != 1 {
		t.Errorf("ABIRebuilds = %d, want 1", stats.ABIRebuilds)
	}
	if stats.ABIHits != 0 {
		t.Errorf("ABIHits = %d, want 0", stats.ABIHits)
	}
	if _, err := os.Stat(filepath.Join(lib, "s2")); err == nil {
		t.Error("consumer should be excluded from staging")
	}
}

// TestPopulateRuntime_ChangedPackage_CacheHit covers the fourth phase
// "changed" arm: the consumer's own source key changed and a matching
// config exists in the store.
func TestPopulateRuntime_ChangedPackage_CacheHit(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// New version of rlang in the lockfile.
	newEntry := makeEntry("rlang", "1.1.0", "new-sha")
	newK, _ := StoreKey(newEntry)
	seedStore(t, s, "rlang", newK, nil, nil, true)

	// refManifest holds the OLD key → changed[rlang]==true.
	oldEntry := makeEntry("rlang", "1.0.0", "old-sha")
	oldK, _ := StoreKey(oldEntry)

	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{newEntry}}
	refManifest := map[string]string{
		"rlang": StoreRef(oldK, ConfigHash(nil)),
	}

	stats, err := s.PopulateRuntime(lf, lib, refLib, refManifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 1 {
		t.Errorf("Hits = %d, want 1", stats.Hits)
	}
}

// TestPopulateRuntime_AlreadyInStaging skips packages that are already
// present in the staging lib (idempotency safeguard).
func TestPopulateRuntime_AlreadyInStaging(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := makeEntry("rlang", "1.1.0", "rlang-sha")
	lf := &Lockfile{LockfileVersion: 1, Packages: []LockfileEntry{entry}}

	// Both unchanged-in-refManifest and pre-staged — should be skipped.
	os.MkdirAll(filepath.Join(lib, "rlang"), 0o755)
	sourceHash, _ := StoreKey(entry)
	refManifest := map[string]string{
		"rlang": StoreRef(sourceHash, ConfigHash(nil)),
	}

	stats, err := s.PopulateRuntime(lf, lib, refLib, refManifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 0 || stats.Misses != 0 || stats.ABIHits != 0 || stats.ABIRebuilds != 0 {
		t.Errorf("expected zero stats for pre-staged package, got %+v", stats)
	}
}

// TestPopulateRuntime_SkipsMetaEntries ensures pak pseudo-packages
// (deps::/app, local::/lib) are ignored by the runtime populate path.
func TestPopulateRuntime_SkipsMetaEntries(t *testing.T) {
	root := t.TempDir()
	lib, refLib := t.TempDir(), t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	lf := &Lockfile{
		LockfileVersion: 1,
		Packages: []LockfileEntry{
			{Package: "deps::/app", Type: "deps"},
			{Package: "local::/lib", Type: "local"},
		},
	}

	stats, err := s.PopulateRuntime(lf, lib, refLib, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Hits != 0 || stats.Misses != 0 || stats.ABIHits != 0 || stats.ABIRebuilds != 0 {
		t.Errorf("expected zero stats for meta-only lockfile, got %+v", stats)
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
