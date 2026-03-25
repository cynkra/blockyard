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
