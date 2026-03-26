package pkgstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadStoreConfigs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configs.json")

	sc := StoreConfigs{
		SourceCompiled: true,
		LinkingTo:      []string{"Rcpp"},
		Configs: map[string]map[string]string{
			"cfg1": {"Rcpp": "key1"},
		},
	}

	if err := WriteStoreConfigs(path, sc); err != nil {
		t.Fatal(err)
	}

	got, err := ReadStoreConfigs(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SourceCompiled {
		t.Error("source_compiled should be true")
	}
	if len(got.LinkingTo) != 1 || got.LinkingTo[0] != "Rcpp" {
		t.Errorf("linkingto = %v", got.LinkingTo)
	}
	if got.Configs["cfg1"]["Rcpp"] != "key1" {
		t.Error("config entry mismatch")
	}
}

func TestStoreConfigsWithMultipleConfigs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configs.json")

	sc := StoreConfigs{
		Configs: map[string]map[string]string{
			"cfg1": {"Rcpp": "key1"},
			"cfg2": {"Rcpp": "key2"},
		},
	}

	if err := WriteStoreConfigs(path, sc); err != nil {
		t.Fatal(err)
	}

	got, err := ReadStoreConfigs(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Configs) != 2 {
		t.Errorf("expected 2 configs, got %d", len(got.Configs))
	}
}

func TestWriteReadConfigMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")

	meta := ConfigMeta{}
	if err := WriteConfigMeta(path, meta); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Error("config meta file not created")
	}
}

func TestResolveConfig_NoConfigsFile(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	lf := &Lockfile{Packages: []LockfileEntry{}}

	_, ok := s.ResolveConfig("shiny", "abc123", lf)
	if ok {
		t.Error("expected miss when no configs.json exists")
	}
}

func TestResolveConfig_MatchingConfig(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Create a package that LinkingTo Rcpp.
	rcppEntry := LockfileEntry{
		Package:  "Rcpp",
		Version:  "1.0.0",
		Type:     "standard",
		SHA256:   "rcpp-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	rcppKey, _ := StoreKey(rcppEntry)

	// Compute the expected config hash.
	expectedConfig := ConfigHash(map[string]string{"Rcpp": rcppKey})

	// Write configs.json with this config.
	configsDir := filepath.Join(root, "4.5-x86_64-pc-linux-gnu", "sf", "sfhash")
	os.MkdirAll(configsDir, 0o755)
	sc := StoreConfigs{
		LinkingTo: []string{"Rcpp"},
		Configs: map[string]map[string]string{
			expectedConfig: {"Rcpp": rcppKey},
		},
	}
	WriteStoreConfigs(filepath.Join(configsDir, "configs.json"), sc)

	lf := &Lockfile{Packages: []LockfileEntry{rcppEntry}}

	configHash, ok := s.ResolveConfig("sf", "sfhash", lf)
	if !ok {
		t.Fatal("expected match")
	}
	if configHash != expectedConfig {
		t.Errorf("got %q, want %q", configHash, expectedConfig)
	}
}

func TestResolveConfig_NoMatchingConfig(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	// Write configs.json with a config that won't match.
	configsDir := filepath.Join(root, "4.5-x86_64-pc-linux-gnu", "sf", "sfhash")
	os.MkdirAll(configsDir, 0o755)
	sc := StoreConfigs{
		LinkingTo: []string{"Rcpp"},
		Configs: map[string]map[string]string{
			"stale-config": {"Rcpp": "old-key"},
		},
	}
	WriteStoreConfigs(filepath.Join(configsDir, "configs.json"), sc)

	// Lockfile has a different Rcpp version.
	lf := &Lockfile{Packages: []LockfileEntry{{
		Package:  "Rcpp",
		Version:  "2.0.0",
		Type:     "standard",
		SHA256:   "new-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}}}

	_, ok := s.ResolveConfig("sf", "sfhash", lf)
	if ok {
		t.Error("expected miss when config doesn't match")
	}
}

func TestResolveConfig_EmptyLinkingTo(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	emptyConfig := ConfigHash(nil)

	configsDir := filepath.Join(root, "4.5-x86_64-pc-linux-gnu", "ggplot2", "gghash")
	os.MkdirAll(configsDir, 0o755)
	sc := StoreConfigs{
		LinkingTo: nil,
		Configs: map[string]map[string]string{
			emptyConfig: {},
		},
	}
	WriteStoreConfigs(filepath.Join(configsDir, "configs.json"), sc)

	lf := &Lockfile{Packages: []LockfileEntry{}}

	configHash, ok := s.ResolveConfig("ggplot2", "gghash", lf)
	if !ok {
		t.Fatal("expected match for empty LinkingTo")
	}
	if configHash != emptyConfig {
		t.Errorf("got %q, want %q", configHash, emptyConfig)
	}
}

func TestIngestContext_NoLinkingTo(t *testing.T) {
	dir := t.TempDir()
	descPath := filepath.Join(dir, "DESCRIPTION")
	os.WriteFile(descPath, []byte("Package: shiny\nVersion: 1.9.1\nNeedsCompilation: no\n"), 0o644)

	lf := &Lockfile{Packages: []LockfileEntry{}}

	configHash, keys, compiled, names, err := IngestContext(descPath, lf)
	if err != nil {
		t.Fatal(err)
	}
	if compiled {
		t.Error("NeedsCompilation: no should set compiled=false")
	}
	if len(keys) != 0 {
		t.Error("expected empty linkingToKeys")
	}
	if len(names) != 0 {
		t.Error("expected empty linkingToNames")
	}
	if configHash != ConfigHash(nil) {
		t.Error("expected canonical empty config hash")
	}
}

func TestIngestContext_WithLinkingTo(t *testing.T) {
	dir := t.TempDir()
	descPath := filepath.Join(dir, "DESCRIPTION")
	os.WriteFile(descPath, []byte("Package: sf\nVersion: 1.0\nNeedsCompilation: yes\nLinkingTo: Rcpp, s2\n"), 0o644)

	rcppEntry := LockfileEntry{
		Package:  "Rcpp",
		Version:  "1.0",
		Type:     "standard",
		SHA256:   "rcpp-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	s2Entry := LockfileEntry{
		Package:  "s2",
		Version:  "2.0",
		Type:     "standard",
		SHA256:   "s2-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	lf := &Lockfile{Packages: []LockfileEntry{rcppEntry, s2Entry}}

	configHash, keys, compiled, names, err := IngestContext(descPath, lf)
	if err != nil {
		t.Fatal(err)
	}
	if !compiled {
		t.Error("NeedsCompilation: yes should set compiled=true")
	}
	if len(keys) != 2 {
		t.Errorf("expected 2 linkingToKeys, got %d", len(keys))
	}
	if len(names) != 2 {
		t.Errorf("expected 2 linkingToNames, got %d", len(names))
	}
	// Names should be sorted.
	if names[0] != "Rcpp" || names[1] != "s2" {
		t.Errorf("names = %v (expected sorted)", names)
	}
	if configHash == ConfigHash(nil) {
		t.Error("should not be canonical empty hash")
	}
}

func TestWriteIngestMeta(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := LockfileEntry{
		Package:  "sf",
		Version:  "1.0",
		Type:     "standard",
		SHA256:   "sf-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	rcppEntry := LockfileEntry{
		Package:  "Rcpp",
		Version:  "1.0",
		Type:     "standard",
		SHA256:   "rcpp-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	lf := &Lockfile{Packages: []LockfileEntry{entry, rcppEntry}}

	rcppKey, _ := StoreKey(rcppEntry)
	linkingToKeys := map[string]string{"Rcpp": rcppKey}
	configHash := ConfigHash(linkingToKeys)
	sourceHash := "sfhash"

	// Create the source hash directory.
	os.MkdirAll(s.SourceDir(entry.Package, sourceHash), 0o755)

	err := s.WriteIngestMeta(entry, lf, sourceHash, configHash, linkingToKeys, true, []string{"Rcpp"})
	if err != nil {
		t.Fatal(err)
	}

	// configs.json should exist with the config.
	sc, err := ReadStoreConfigs(s.ConfigsPath(entry.Package, sourceHash))
	if err != nil {
		t.Fatal(err)
	}
	if !sc.SourceCompiled {
		t.Error("source_compiled should be true")
	}
	if _, ok := sc.Configs[configHash]; !ok {
		t.Error("config hash missing from configs.json")
	}

	// Config sidecar should exist.
	metaPath := s.ConfigMetaPath(entry.Package, sourceHash, configHash)
	if _, err := os.Stat(metaPath); err != nil {
		t.Error("config sidecar not written")
	}
}

func TestWriteIngestMeta_AppendsConfig(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	s.SetPlatform("4.5-x86_64-pc-linux-gnu")

	entry := LockfileEntry{
		Package:  "sf",
		Version:  "1.0",
		Type:     "standard",
		SHA256:   "sf-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	lf := &Lockfile{Packages: []LockfileEntry{entry}}
	sourceHash := "sfhash"

	// Create directory and write initial config.
	os.MkdirAll(s.SourceDir(entry.Package, sourceHash), 0o755)
	sc := StoreConfigs{
		SourceCompiled: true,
		LinkingTo:      []string{"Rcpp"},
		Configs:        map[string]map[string]string{"existing-cfg": {}},
	}
	WriteStoreConfigs(s.ConfigsPath(entry.Package, sourceHash), sc)

	// Add a second config.
	err := s.WriteIngestMeta(entry, lf, sourceHash, "new-cfg", map[string]string{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := ReadStoreConfigs(s.ConfigsPath(entry.Package, sourceHash))
	if len(got.Configs) != 2 {
		t.Errorf("expected 2 configs, got %d", len(got.Configs))
	}
}

func TestIngestContext_MissingDESCRIPTION(t *testing.T) {
	lf := &Lockfile{Packages: []LockfileEntry{}}

	// Non-existent path — should return empty config hash, no error.
	configHash, keys, compiled, names, err := IngestContext("/nonexistent/DESCRIPTION", lf)
	if err != nil {
		t.Fatalf("expected nil error for missing DESCRIPTION, got %v", err)
	}
	if compiled {
		t.Error("compiled should be false")
	}
	if len(keys) != 0 || len(names) != 0 {
		t.Error("expected empty linkingTo")
	}
	if configHash != ConfigHash(nil) {
		t.Error("expected canonical empty config hash")
	}
}

func TestIngestContext_LinkingToNotInLockfile(t *testing.T) {
	dir := t.TempDir()
	descPath := filepath.Join(dir, "DESCRIPTION")
	os.WriteFile(descPath, []byte("Package: sf\nLinkingTo: Rcpp, NonExistent\nNeedsCompilation: yes\n"), 0o644)

	rcppEntry := LockfileEntry{
		Package:  "Rcpp",
		Version:  "1.0",
		Type:     "standard",
		SHA256:   "rcpp-sha",
		Platform: "x86_64-pc-linux-gnu",
		RVersion: "4.5",
		Metadata: LockfileMetadata{RemoteType: "standard"},
	}
	lf := &Lockfile{Packages: []LockfileEntry{rcppEntry}}

	_, keys, _, names, err := IngestContext(descPath, lf)
	if err != nil {
		t.Fatal(err)
	}
	// Both names should be listed, but only Rcpp has a store key.
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %d", len(names))
	}
	if len(keys) != 1 {
		t.Errorf("expected 1 key (NonExistent not in lockfile), got %d", len(keys))
	}
}

func TestParsePkgList_WithVersionConstraints(t *testing.T) {
	got := parsePkgList("Rcpp (>= 1.0.0), s2, BH (>= 1.72)")
	if len(got) != 3 {
		t.Fatalf("expected 3 packages, got %d: %v", len(got), got)
	}
	if got[0] != "Rcpp" || got[1] != "s2" || got[2] != "BH" {
		t.Errorf("got %v", got)
	}
}

func TestParsePkgList_Empty(t *testing.T) {
	got := parsePkgList("")
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestLockfileStoreKey_NotFound(t *testing.T) {
	lf := &Lockfile{Packages: []LockfileEntry{
		{Package: "shiny", Version: "1.0", Type: "standard", SHA256: "abc",
			Platform: "x", RVersion: "4.5", Metadata: LockfileMetadata{RemoteType: "standard"}},
	}}
	// Package not in lockfile.
	key := lockfileStoreKey(lf, "nonexistent")
	if key != "" {
		t.Errorf("expected empty key for missing package, got %q", key)
	}
}

func TestLockfileStoreKey_UnsupportedType(t *testing.T) {
	lf := &Lockfile{Packages: []LockfileEntry{
		{Package: "bad", Version: "1.0", Type: "url", Platform: "x", RVersion: "4.5",
			Metadata: LockfileMetadata{RemoteType: "url"}},
	}}
	// StoreKey returns error for unsupported type -> lockfileStoreKey returns "".
	key := lockfileStoreKey(lf, "bad")
	if key != "" {
		t.Errorf("expected empty key for unsupported type, got %q", key)
	}
}

func TestParseDCF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "DESCRIPTION")
	os.WriteFile(path, []byte("Package: test\nVersion: 1.0\nTitle: A test\n  package\nLinkingTo: Rcpp (>= 1.0),\n  s2\n"), 0o644)

	fields, err := ParseDCF(path)
	if err != nil {
		t.Fatal(err)
	}
	if fields["Package"] != "test" {
		t.Errorf("Package = %q", fields["Package"])
	}
	if fields["Title"] != "A test package" {
		t.Errorf("Title = %q", fields["Title"])
	}
}
